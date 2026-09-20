// Package execddl: FTS flush-side shadow-table writers (segment flush at
// COMMIT, %_segdir/%_segments row writers, idx allocation + crisis merge,
// %_stat rows). Split from export.go; behavior unchanged.
package execddl

import (
	"fmt"

	"sort"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// FlushFTSSegments writes the pending FTS3 segment for every FTS table that
// accumulated inserts since the last flush (called at COMMIT; SQLite's FTS3
// flushes the pending-terms hash into one segment per transaction — fts3.c
// fts3PendingTermsFlush). Each pending batch becomes one %_segdir row whose
// columns mirror SQLite's fts3SegWriterFlush / fts3WriteSegdir:
//
//	root-only segment: (level, idx, 0, 0, "0 <rootLen>", root)
//	multi-block:       (level, idx, <firstBlock>, <lastLeafBlock>,
//	                    "<lastBlock> <leafDataSize>", root)
//
// When many segments have accumulated, SQLite's auto-incr-merge reads the
// existing roots — a corrupt one then surfaces as "database disk image is
// malformed" (fts3corrupt 1.3: the 17th insert after the root was modified
// fails). Returns a Result whose Error is set when a corrupt root must abort
// the flush.
func (e *DDLExecutor) FlushFTSSegments() *Result {
	for tableName, ftsTable := range e.ctx.FTSTables() {
		if res := e.flushFTSTable(tableName, ftsTable); res != nil {
			return res
		}
	}
	return nil
}

// FlushFTSPendingTable flushes ONLY tableName's pending batch (SQLite's
// sqlite3Fts3PendingTermsFlush(p) for one Fts3Table). It backs the
// fts3PendingTermsDocid docid-restart flush inside xUpdate: a mid-statement
// flush must not touch OTHER FTS tables' pending batches — inside an open
// transaction C keeps those pending until their own next operation or the
// COMMIT's xSync (fts4onepass-4.0 + the fts4merge4 2.2 grid).
func (e *DDLExecutor) FlushFTSPendingTable(tableName string) *Result {
	ftsTable, ok := e.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return nil
	}
	return e.flushFTSTable(tableName, ftsTable)
}

// flushFTSPendingFlagged is FlushFTSPendingTable entered under the engine's
// FTS-flush flag (the flush's shadow writes belong to the enclosing
// statement's rollback scope, so they skip per-write pager snapshots).
func (e *DDLExecutor) flushFTSPendingFlagged(tableName string) *Result {
	was := e.ctx.InFTSFlush()
	e.ctx.SetFTSFlush(true)
	res := e.FlushFTSPendingTable(tableName)
	e.ctx.SetFTSFlush(was)
	return res
}

// flushFTSTable writes one FTS table's pending batch (the body of
// FlushFTSSegments; SQLite's xSync per table).
func (e *DDLExecutor) flushFTSTable(tableName string, ftsTable *fts.FTS3Table) *Result {
	ids := ftsTable.PendingFlush()
	deleted := ftsTable.DeletedFlush()
	if len(ids) == 0 && len(deleted) == 0 {
		return nil
	}
	// A pending batch whose documents produce no terms at all (a commit
	// containing only empty-content rows) writes NO segment — SQLite's
	// pending-terms hash stays empty and fts3SegmentMerge(PENDING) bails
	// before the level-0 idx allocation. Skipping here keeps the level-0
	// segment count aligned with the oracle (fts4merge 3.2: the 30040-doc
	// build's single empty document must not add a level-0 row).
	if len(ids) > 0 && !ftsTable.BatchHasTerms(ids) {
		ids = nil
	}
	// INSERT OR REPLACE of a flushed row inside one transaction puts the
	// docid in BOTH lists; SQLite's single pending batch flushes ONE
	// segment per index carrying the delete entry AND the new postings,
	// so exclude it from the separate marker pass and let the segment
	// builders inject its marker entries (fts4opt 2.x per-ROW parity).
	replaced, deleted := splitReplacedDeletes(ftsTable, ids, deleted)
	// The flush allocates a new segment at level 0 (or 1024*iIndex for a
	// prefix index). SQLite's fts3AllocateSegdirIdx crisis-merges a level
	// that has reached MergeCount (16) segments into the next level, so
	// every level stays below the threshold. The crisis merge reads the
	// existing roots — a corrupt one then surfaces as "database disk image
	// is malformed" (fts3corrupt 1.2 inserts succeed, the 17th fails).
	nodeSize := e.ftsNodeSize(ftsTable)
	nLeafAdd := 0
	if len(deleted) > 0 {
		n, res := e.flushDeleteMarkers(tableName, ftsTable, deleted, nodeSize)
		if res != nil {
			return res
		}
		nLeafAdd += n
	}
	if len(ids) > 0 {
		n, res := e.flushPendingSegments(tableName, ftsTable, ids, nodeSize)
		if res != nil {
			return res
		}
		nLeafAdd += n
	}
	if len(replaced) > 0 {
		// The merged marker entries are persisted with the pending
		// segments; drop the replace markers and the term snapshots.
		ftsTable.ClearReplaceDocs()
		ftsTable.ConsumeDeleteMarkers(replaced)
	}
	e.writeFTSStat(tableName, ftsTable)
	e.autoIncrMergeAfterFlush(tableName, ftsTable, nLeafAdd)
	return nil
}

// splitReplacedDeletes separates the docids that appear in BOTH the pending
// and deleted lists (INSERT OR REPLACE of an already-flushed row inside one
// transaction) from the plain deletes, registering the replaced docs on the
// FTS table; see flushFTSTable.
func splitReplacedDeletes(ftsTable *fts.FTS3Table, ids, deleted []int64) ([]int64, []int64) {
	var replaced []int64
	if len(deleted) == 0 || len(ids) == 0 {
		return nil, deleted
	}
	idset := make(map[int64]bool, len(ids))
	for _, id := range ids {
		idset[id] = true
	}
	rest := deleted[:0]
	for _, id := range deleted {
		if idset[id] {
			replaced = append(replaced, id)
		} else {
			rest = append(rest, id)
		}
	}
	if len(replaced) > 0 {
		ftsTable.SetReplaceDocs(replaced)
	}
	return replaced, rest
}

// flushDeleteMarkers writes the delete-marker segments for one flush's
// deleted documents (fts3.c fts3DeleteTerms: doclists carry only docids, no
// positions) so a segment reload does not resurrect the deleted documents.
// fts3DeleteTerms feeds EVERY index's pending-terms hash (main + prefix
// indexes), so ONE marker segment is flushed PER INDEX at its absolute level
// 1024*iIndex (fts4opt 2.x: without prefix-index markers the level structure
// diverges from the oracle after delete churn). Returns the leaf-block count
// added.
func (e *DDLExecutor) flushDeleteMarkers(tableName string, ftsTable *fts.FTS3Table, deleted []int64, nodeSize int) (int, *Result) {
	nLeafAdd := 0
	nIndexes := 1 + len(ftsTable.PrefixLengths())
	for iIndex := 0; iIndex < nIndexes; iIndex++ {
		dmRoot, dmBlocks := ftsTable.DeleteMarkerRootIndex(deleted, nodeSize, iIndex)
		if dmRoot == nil {
			// No term maps into this index — SQLite's pending-terms
			// merge bails before allocating a segdir idx.
			continue
		}
		level := 1024 * iIndex
		nLeafAdd += len(dmBlocks)
		idx, res := e.allocFTSIdx(tableName, level, ftsTable)
		if res != nil {
			return 0, res
		}
		dmStart := e.writeFTSShadowRow(tableName, level, idx, dmBlocks, dmRoot)
		ftsTable.SetSegdirNextIdx(level, idx+1)
		e.writeSegmentBlocks(tableName, ftsTable, dmBlocks, e.resumeSegmentBlockID(tableName, ftsTable, dmStart), false)
	}
	// The markers are persisted; drop the term snapshots so the next
	// flush does not rebuild them.
	ftsTable.ConsumeDeleteMarkers(deleted)
	return nLeafAdd, nil
}

// flushPendingSegments flushes one pending batch's documents into segments
// (grouped per language for a languageid=<col> table); returns the
// leaf-block count added.
func (e *DDLExecutor) flushPendingSegments(tableName string, ftsTable *fts.FTS3Table, ids []int64, nodeSize int) (int, *Result) {
	nLeafAdd := 0
	for _, g := range langFlushGroups(ftsTable, ids) {
		n, res := e.flushOneSegmentGroup(tableName, ftsTable, g, nodeSize)
		if res != nil {
			return 0, res
		}
		nLeafAdd += n
	}
	return nLeafAdd, nil
}

// ftsFlushGroup is one flush's (absolute level, docid set) group.
type ftsFlushGroup struct {
	level int
	ids   []int64
}

// langFlushGroups groups a pending batch per language for a languageid=<col>
// table: the pending-terms hash is keyed by language, and each language's
// segment lands at its base absolute level
// ((iLangid*nIndex+iIndex)*1024 = iLangid*1024 without prefix indexes —
// fts3_write.c getAbsoluteLevel; fts4langid 5.1.1: levels 0 1024 2048 2^40
// for languages 0,1,2,1<<30).
func langFlushGroups(ftsTable *fts.FTS3Table, ids []int64) []ftsFlushGroup {
	if ftsTable.LangIDColName() == "" {
		return []ftsFlushGroup{{level: 0, ids: ids}}
	}
	byLang := map[int64][]int64{}
	for _, id := range ids {
		l := ftsTable.DocLangID(id)
		byLang[l] = append(byLang[l], id)
	}
	langs := make([]int64, 0, len(byLang))
	for l := range byLang {
		langs = append(langs, l)
	}
	sort.Slice(langs, func(i, j int) bool { return langs[i] < langs[j] })
	groups := make([]ftsFlushGroup, 0, len(langs))
	for _, l := range langs {
		groups = append(groups, ftsFlushGroup{level: int(l) * 1024, ids: byLang[l]})
	}
	return groups
}

// flushOneSegmentGroup flushes one (level, docids) group's segment: allocates
// the segment idx at the group's absolute level (crisis-merging a full level
// first — SQLite's fts3AllocateSegdirIdx), builds the segment, promotes
// smaller higher-level segments down, writes the leaf blocks, then the FTS4
// prefix indexes. Returns the leaf-block count added.
func (e *DDLExecutor) flushOneSegmentGroup(tableName string, ftsTable *fts.FTS3Table, g ftsFlushGroup, nodeSize int) (int, *Result) {
	level := g.level
	// Allocate the segment idx at the group's absolute level
	// (crisis-merging a full level first — SQLite's
	// fts3AllocateSegdirIdx).
	idx, res := e.allocFTSIdx(tableName, level, ftsTable)
	if res != nil {
		return 0, res
	}
	rootBlob, blocks := ftsTable.SegmentRootBlocks(g.ids, nodeSize)
	nLeafAdd := len(blocks)
	// writeFTSShadowRow returns the first block id it recorded in the
	// row AND patched into the root; the leaf writes MUST use the SAME
	// id (re-reading the cache after the row write can diverge when a
	// shadow-table write invalidated it — fts4merge4 am=2 stale root).
	startBlock := e.writeFTSShadowRow(tableName, level, idx, blocks, rootBlob)
	ftsTable.SetSegdirNextIdx(level, idx+1)
	// fts3PromoteSegments: a freshly flushed base-level segment
	// DEMOTES every smaller higher-level segment of its group to
	// this level (relabeled in place) — fts4opt 1.8 folds the
	// lone level-B+1 merge output back down when regrowth lands.
	var mainLeaf int
	for _, blk := range blocks {
		mainLeaf += len(blk.Block)
	}
	e.promoteFTSSegments(tableName, ftsTable, level, mainLeaf)
	// Multi-block segments store their leaf blocks in %_segments
	// (fts3.c fts3WriteSegment); the corruption tests count them
	// (fts3corrupt 8.1: count(*) FROM f_segments). Block IDs are global
	// across segments (the next block is max(existing)+1).
	nextBlock := e.resumeSegmentBlockID(tableName, ftsTable, startBlock)
	if res := e.writeSegmentBlocks(tableName, ftsTable, blocks, nextBlock, true); res != nil {
		return 0, res
	}
	n, res := e.flushPrefixIndexes(tableName, ftsTable, g.ids, nodeSize)
	if res != nil {
		return 0, res
	}
	return nLeafAdd + n, nil
}

// flushPrefixIndexes flushes the FTS4 prefix indexes for one group's docids
// (fts3.c fts3PrefixParameter + fts3SegWriter): each non-empty prefix index
// is a separate segment at absolute level 1024*iIndex. An index whose prefix
// length exceeds every token (e.g. prefix="1,600,2" with short documents)
// contributes no postings and therefore no segdir row, which is why
// fts3prefix.test 6.4.2 (1,600,2 vs 1,2) compares equal.
func (e *DDLExecutor) flushPrefixIndexes(tableName string, ftsTable *fts.FTS3Table, ids []int64, nodeSize int) (int, *Result) {
	nLeafAdd := 0
	for i, prefixLen := range ftsTable.PrefixLengths() {
		iIndex := i + 1
		if !ftsTable.IndexHasPostings(iIndex, prefixLen, ids) {
			continue
		}
		pRoot, pBlocks := ftsTable.SegmentRootBlocksIndex(ids, nodeSize, iIndex)
		nLeafAdd += len(pBlocks)
		level := 1024 * iIndex
		pIdx, res := e.allocFTSIdx(tableName, level, ftsTable)
		if res != nil {
			return 0, res
		}
		pStart := e.writeFTSShadowRow(tableName, level, pIdx, pBlocks, pRoot)
		ftsTable.SetSegdirNextIdx(level, pIdx+1)
		// Promotion for prefix groups too (fts4opt 1.8: 1057/2081/
		// 3105 outputs fold back to their base levels likewise).
		var pLeaf int
		for _, blk := range pBlocks {
			pLeaf += len(blk.Block)
		}
		e.promoteFTSSegments(tableName, ftsTable, level, pLeaf)
		nextBlock := e.resumeSegmentBlockID(tableName, ftsTable, pStart)
		e.writeSegmentBlocks(tableName, ftsTable, pBlocks, nextBlock, false)
	}
	return nLeafAdd, nil
}

// resumeSegmentBlockID derives the first %_segments block id for a segment's
// leaf writes: the row write's recorded start block, or the cache/scan
// fallback for a root-only segment (no leaf blocks; the fallback decides the
// NEXT segment's allocation).
func (e *DDLExecutor) resumeSegmentBlockID(tableName string, ftsTable *fts.FTS3Table, startBlock int) int {
	if startBlock != 0 {
		return startBlock
	}
	nextBlock, cached := ftsTable.NextBlockID()
	if !cached {
		nextBlock = e.ftsNextBlockID(tableName)
	}
	return nextBlock
}

// writeSegmentBlocks persists a segment's leaf blocks at consecutive ids
// from nextBlock (fts3.c fts3WriteSegment) and bumps the table's block
// counter past them. strict=true surfaces a failed write to the caller (the
// pending-batch segment write aborts the flush); the marker/prefix writes
// keep the historical fire-and-forget behavior.
func (e *DDLExecutor) writeSegmentBlocks(tableName string, ftsTable *fts.FTS3Table, blocks []fts.SegmentBlock, nextBlock int, strict bool) *Result {
	for _, blk := range blocks {
		res := e.ctx.Exec(&sql.InsertStmt{
			Table:   tableName + "_segments",
			Columns: []string{"blockid", "block"},
			Values: [][]sql.Expr{
				{
					&sql.NumericLit{Value: fmt.Sprintf("%d", nextBlock)},
					&sql.BlobLit{Value: blk.Block},
				},
			},
		})
		if strict && res != nil && res.Error != nil {
			return &Result{Error: res.Error}
		}
		nextBlock++
	}
	ftsTable.SetNextBlockID(nextBlock)
	return nil
}

// autoIncrMergeAfterFlush runs the flush-time auto-incr-merge (fts3.c
// fts3SyncMethod): when the automerge setting is enabled, estimate the work A
// = nLeafAdd*mxLevel + A/2 and run an incremental merge ONLY when A
// exceeds the minimum useful amount (nMinMerge=64 leaf blocks) AND the
// flush itself added more than nMinMerge/16 = 4 leaf blocks (SQLite's
// p->nLeafAdd>(nMinMerge/16) pre-gate — small flushes never trigger a
// merge no matter how tall the tree is; fts4merge4 2.2 am=8/am=1: the
// oracle keeps 0 4 | 1 3 | 2 1 because the small per-tx flushes stay
// below the gate and only the occasional larger flush merges).
// nLeafAdd counts EVERY leaf block the transaction wrote: the main
// index's flush blocks plus prefix-index and delete-marker segments
// (fts3_write.c fts3SegWriterAddBlock/flush increment per leaf).
func (e *DDLExecutor) autoIncrMergeAfterFlush(tableName string, ftsTable *fts.FTS3Table, nLeafAdd int) {
	am, known := ftsTable.Automerge()
	if !known && nLeafAdd > 0 {
		// The setting is UNKNOWN (SQLite's 0xff sentinel after a (re)open —
		// fts4merge4 2.2 tn2=2: a freshly reopened connection must keep
		// automerging). fts3_write.c sqlite3Fts3PendingTermsFlush restores it
		// from the %_stat id=2 row written by fts3DoAutoincrmerge: a present
		// row enables it (1 maps to 8); an absent row disables it. The result
		// is remembered so the row is read once per connection.
		if v, ok := e.readFTSAutomergeStat(tableName); ok {
			am = ftsTable.SetAutomerge(v)
		} else {
			am = ftsTable.SetAutomerge(0)
		}
		known = true
	}
	if known && am > 0 && am <= 16 && nLeafAdd > 4 {
		mxLevel := e.maxFTSLevel(tableName)
		A := nLeafAdd * mxLevel
		A += A / 2

		if A > 64 {
			e.MergeFTS(tableName, A, am)
		}
	}
}

// WriteFTSShadowRow inserts one %_segdir row (exported for the DML layer's
// optimize command).
func (e *DDLExecutor) WriteFTSShadowRow(tableName string, level, idx int, blocks []fts.SegmentBlock, root []byte) {
	e.writeFTSShadowRow(tableName, level, idx, blocks, root)
}

// NextFTSBlockID returns the next %_segments block ID (exported for the DML
// layer's optimize command), using the per-table cache when valid.
func (e *DDLExecutor) NextFTSBlockID(tableName string) int {
	return e.ftsNextBlockID(tableName)
}

// writeFTSShadowRow inserts one %_segdir row with SQLite's column layout and
// values (fts3_write.c fts3WriteSegdir): level, idx, start_block,
// leaves_end_block, end_block, root. blocks are the segment's leaf blocks
// (empty for a root-only segment). The end_block column stores the TEXT
// "start size" (nLeafData != 0) or INTEGER 0 (no leaf data); start_block and
// leaves_end_block are 0 for a root-only segment.
//
// It returns the first %_segments block id the caller must write the leaf
// blocks at (0 for a root-only segment). The row's start_block, the root's
// interior first-block varint, and the caller's block writes MUST agree — the
// caller writes the blocks AFTER this row write, and a shadow-table write can
// invalidate the block-id cache, so re-reading it would diverge (fts4merge4
// am=2: the L0 flush's root firstBlock=157 vs the row's start_block=3485 left
// an unreadable segment; the merge aborted and L0 accumulated).
func (e *DDLExecutor) writeFTSShadowRow(tableName string, level, idx int, blocks []fts.SegmentBlock, root []byte) int {
	segdir := tableName + "_segdir"
	startBlock := 0
	leavesEndBlock := 0
	var endBlockExpr sql.Expr
	if len(blocks) > 0 {
		// The engine serializes a segment as leaf blocks in %_segments plus
		// the interior/root node in %_segdir.root (serializeSegmentBlocks), so
		// the first block is also the last leaf block and nLeafData is the
		// sum of the leaf payload sizes.
		nextBlock := e.ftsNextBlockID(tableName)
		startBlock = nextBlock
		leavesEndBlock = nextBlock + len(blocks) - 1
		// The segment root's interior node stores the first leaf block id;
		// serializeSegmentBlocks used internal ids 1..N, but the leaf blocks
		// are stored in %_segments at the table-global block counter. Patch
		// the root so a reload reads THIS segment's blocks, not an earlier
		// segment's (fts4check t2: the prefix="3,1" segments at levels 1024
		// and 2048 follow the main segment's blocks).
		root = fts.RewriteSegmentRootFirstBlock(root, nextBlock)
		var nLeafData int
		for _, blk := range blocks {
			nLeafData += len(blk.Block)
		}
		endBlockExpr = &sql.StringLit{Value: fmt.Sprintf("%d %d", leavesEndBlock, nLeafData)}
	} else {
		// Root-only segment: end_block = "0 <rootLen>" (nLeafData = root blob
		// length, fts3SegWriterFlush root-only branch).
		endBlockExpr = &sql.StringLit{Value: fmt.Sprintf("0 %d", len(root))}
	}
	e.writeFTSShadowRowRaw(tableName, segdir, level, idx, startBlock, leavesEndBlock, endBlockExpr, root)
	return startBlock
}

// writeFTSShadowRowAtRange inserts a new %_segdir row with an EXPLICIT rowid
// AND block range (the merge output writer allocated and wrote the blocks
// once). nLeafData is the segment's leaf-data size — SQLite's fts3WriteSegdir
// stores end_block as TEXT "<endBlock> <nLeafData>" so fts3PromoteSegments
// can compare segment sizes (a 0 size disables promotion for that row).
func (e *DDLExecutor) writeFTSShadowRowAtRange(tableName string, level, idx int, rowID int64, startBlock, leavesEndBlock int, nLeafData int, root []byte, endBlockID ...int) {
	segdir := tableName + "_segdir"
	var endBlockExpr sql.Expr
	if nLeafData == 0 {
		// SQLite stores a BARE integer end_block whenever the segment's
		// leaf-data size is unknown/zero (fts3WriteSegdir: nLeafData==0 →
		// bind_int64(iEndBlock); the "blockid size" text form only appears
		// once real size accounting exists).
		if len(endBlockID) > 0 && endBlockID[0] > leavesEndBlock {
			endBlockExpr = &sql.NumericLit{Value: fmt.Sprintf("%d", endBlockID[0])}
		} else if leavesEndBlock > 0 {
			endBlockExpr = &sql.NumericLit{Value: fmt.Sprintf("%d", leavesEndBlock)}
		} else {
			endBlockExpr = &sql.NumericLit{Value: "0"}
		}
	} else if len(endBlockID) > 0 && endBlockID[0] > leavesEndBlock {
		// A merge output's end_block first component is the PRE-ALLOCATED
		// range end (the NULL marker block id, fts3_write.c iEnd), which sits
		// above the last leaf (fts4langid 5.4: end_block "256 65" with
		// leaves_end_block 1).
		endBlockExpr = &sql.StringLit{Value: fmt.Sprintf("%d %d", endBlockID[0], nLeafData)}
	} else if leavesEndBlock > 0 {
		endBlockExpr = &sql.StringLit{Value: fmt.Sprintf("%d %d", leavesEndBlock, nLeafData)}
	} else {
		endBlockExpr = &sql.StringLit{Value: fmt.Sprintf("0 %d", len(root))}
	}
	if err := e.ctx.Exec(&sql.InsertStmt{
		Table:   segdir,
		Columns: []string{"rowid", "level", "idx", "start_block", "leaves_end_block", "end_block", "root"},
		Values: [][]sql.Expr{
			{
				&sql.NumericLit{Value: fmt.Sprintf("%d", rowID)},
				&sql.NumericLit{Value: fmt.Sprintf("%d", level)},
				&sql.NumericLit{Value: fmt.Sprintf("%d", idx)},
				&sql.NumericLit{Value: fmt.Sprintf("%d", startBlock)},
				&sql.NumericLit{Value: fmt.Sprintf("%d", leavesEndBlock)},
				endBlockExpr,
				&sql.BlobLit{Value: root},
			},
		},
	}); err != nil && err.Error != nil {
	}
}

// writeFTSShadowRowRaw inserts a new %_segdir row with precomputed fields.
func (e *DDLExecutor) writeFTSShadowRowRaw(tableName, segdir string, level, idx, startBlock, leavesEndBlock int, endBlockExpr sql.Expr, root []byte) {
	_ = e.ctx.Exec(&sql.InsertStmt{
		Table:   segdir,
		Columns: []string{"level", "idx", "start_block", "leaves_end_block", "end_block", "root"},
		Values: [][]sql.Expr{
			{
				&sql.NumericLit{Value: fmt.Sprintf("%d", level)},
				&sql.NumericLit{Value: fmt.Sprintf("%d", idx)},
				&sql.NumericLit{Value: fmt.Sprintf("%d", startBlock)},
				&sql.NumericLit{Value: fmt.Sprintf("%d", leavesEndBlock)},
				endBlockExpr,
				&sql.BlobLit{Value: root},
			},
		},
	})
}

// updateFTSShadowRow updates an existing %_segdir row's segment fields
// (start_block, leaves_end_block, end_block, root) after a truncation, so the
// row is replaced in place rather than delete+insert (which would create a
// duplicate when the segdir has no unique index).
//
//lint:ignore U1000 retained for FTS shadow-row compatibility
func (e *DDLExecutor) updateFTSShadowRow(tableName string, level, idx int, blocks []fts.SegmentBlock, root []byte) {
	segdir := tableName + "_segdir"
	startBlock := 0
	leavesEndBlock := 0
	var endBlockExpr sql.Expr
	if len(blocks) > 0 {
		nextBlock := e.ftsNextBlockID(tableName)
		startBlock = nextBlock
		leavesEndBlock = nextBlock + len(blocks) - 1
		root = fts.RewriteSegmentRootFirstBlock(root, nextBlock)
		var nLeafData int
		for _, blk := range blocks {
			nLeafData += len(blk.Block)
		}
		endBlockExpr = &sql.StringLit{Value: fmt.Sprintf("%d %d", leavesEndBlock, nLeafData)}
	} else {
		endBlockExpr = &sql.StringLit{Value: fmt.Sprintf("0 %d", len(root))}
	}
	_ = e.ctx.Exec(&sql.UpdateStmt{
		Table: segdir,
		Assignments: []sql.Assignment{
			{Column: "start_block", Value: &sql.NumericLit{Value: fmt.Sprintf("%d", startBlock)}},
			{Column: "leaves_end_block", Value: &sql.NumericLit{Value: fmt.Sprintf("%d", leavesEndBlock)}},
			{Column: "end_block", Value: endBlockExpr},
			{Column: "root", Value: &sql.BlobLit{Value: root}},
		},
		Where: &sql.BinaryOp{
			Operator: "AND",
			Left:     &sql.BinaryOp{Operator: "=", Left: &sql.ColumnRef{Name: "level"}, Right: &sql.NumericLit{Value: fmt.Sprintf("%d", level)}},
			Right:    &sql.BinaryOp{Operator: "=", Left: &sql.ColumnRef{Name: "idx"}, Right: &sql.NumericLit{Value: fmt.Sprintf("%d", idx)}},
		},
	})
}

// ftsSegdirNextRowID returns the next %_segdir rowid (max rowid + 1, or 1 when
// empty). The merge uses it to write output/truncation rows with EXPLICIT
// rowids, avoiding the implicit allocation's stale cached max.
func (e *DDLExecutor) ftsSegdirNextRowID(tableName string) int64 {
	segdir := tableName + "_segdir"
	segEntry, _, err := e.ctx.FindTable(segdir)
	if err != nil || segEntry == nil {
		return 1
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	maxID, lerr := tree.LastRowID()
	if lerr != nil {
		return 1
	}
	return maxID + 1
}

// ftSSegmentIdx returns the next idx value for a %_segdir level (the largest
// existing idx + 1, or 0 when the level has no rows). SQLite numbers segments
// within a level 0..n-1 in creation order (fts3.c fts3AllocateSegdirIdx).
func (e *DDLExecutor) ftSSegmentIdx(tableName string, level int) int {
	segdir := tableName + "_segdir"
	segEntry, _, err := e.ctx.FindTable(segdir)
	if err != nil || segEntry == nil {
		return 0
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return 0
	}
	maxIdx := -1
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) < 2 {
			break
		}
		segdirTrackMaxIdx(rec, level, &maxIdx)
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	return maxIdx + 1
}

// segdirTrackMaxIdx raises maxIdx to the row's idx when the row belongs to
// the given level; see ftSSegmentIdx.
func segdirTrackMaxIdx(rec *storage.Record, level int, maxIdx *int) {
	lv, ok := rec.Values[0].(int64)
	if !ok || int(lv) != level {
		return
	}
	if ix, ok := rec.Values[1].(int64); ok && int(ix) > *maxIdx {
		*maxIdx = int(ix)
	}
}

// ftsNextBlockID returns the next %_segments block ID for an FTS table, using
// the table's cached counter when valid (set by the flush/merge writer) and
// falling back to a full scan of %_segments otherwise (SQLite's max(blockid)
// — indexed in real SQLite, the engine's segdir/segments shadow btrees have no
// secondary index, so a scan is O(n) and the cache avoids it per flush).
func (e *DDLExecutor) ftsNextBlockID(tableName string) int {
	if t, ok := e.ctx.FTSTables()[tableName]; ok && t != nil {
		if id, cached := t.NextBlockID(); cached {
			return id
		}
		id := e.nextFTSBlockID(tableName)
		// Strictly monotonic within the session (C's p->iNextBlock): a
		// re-derivation after chomp deletions must not reuse ids that
		// surviving segments above the deleted range still hold.
		if hw := t.BlockIDHighWater(); id < hw {
			id = hw
		}
		t.SetNextBlockID(id)
		return id
	}
	return e.nextFTSBlockID(tableName)
}

// nextFTSBlockID returns the next block ID for an FTS table's %_segments
// table (max existing blockid + 1, or 1 when empty). SQLite allocates block
// IDs globally across all segments of a table. The blockid column is the
// table's INTEGER PRIMARY KEY, so the cell's rowid IS the block id; the max
// is found by walking to the rightmost leaf (O(depth)) — a full scan per
// flush was O(n) each, O(n^2) over the automerge's many flushes
// (fts4merge4 2.2.x).
func (e *DDLExecutor) nextFTSBlockID(tableName string) int {
	seg := tableName + "_segments"
	segEntry, _, err := e.ctx.FindTable(seg)
	if err != nil || segEntry == nil {
		return 1
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	maxID, lerr := tree.LastRowID()
	if lerr != nil {
		return 1
	}
	return int(maxID) + 1
}

// crisisReallocIdx re-allocates a level's segment idx after a crisis merge:
// the crisis may have been a no-op (a stale cache said the level was full but
// it has fewer rows — e.g. after a cascade wrote to this level and
// invalidated the cache mid-flight), so re-scan to get the real count instead
// of assuming the level was cleared.
func (e *DDLExecutor) crisisReallocIdx(tableName string, level int, ftsTable *fts.FTS3Table) (int, *Result) {
	if res := e.crisisMergeFTSLevel(tableName, level, ftsTable); res != nil {
		return 0, res
	}
	rows := e.readFTSSegdirRows(tableName, level)
	if len(rows) >= ftsMergeThreshold {
		return 0, &Result{Error: fmt.Errorf("fts merge: level %d still full after crisis", level)}
	}
	if len(rows) == 0 {
		return 0, nil // the level was cleared; idx restarts at 0
	}
	idx := e.ftSSegmentIdx(tableName, level)
	ftsTable.SetSegdirNextIdx(level, idx)
	return idx, nil
}

// allocFTSIdx implements SQLite's fts3AllocateSegdirIdx: it returns the next
// %_segdir idx to use at an absolute level, crisis-merging the level first
// when it has reached ftsMergeThreshold (16) segments. The merge is recursive:
// writing the merged output into level+1 itself allocates an idx there, which
// can trigger a higher-level merge (fts3SegmentMerge → fts3SegWriterFlush →
// fts3AllocateSegdirIdx). A corrupt source root fails the operation
// ("database disk image is malformed", fts3corrupt 1.3).
func (e *DDLExecutor) allocFTSIdx(tableName string, level int, ftsTable *fts.FTS3Table) (int, *Result) {
	// Use the cached next-idx when valid: the level's rows are numbered
	// 0..idx-1 contiguously (the flush/merge writer maintains the cache), so
	// idx IS the row count — avoiding an O(segdir) scan per flush that would
	// make per-row FTS builds O(n^2) (fts3_build_db_2 30040: 2000+ segdir
	// rows scanned per flush). Only when the cache is stale (a direct SQL
	// write, a table reload, or a just-crisis-merged level) do we scan.
	if idx, ok := ftsTable.SegdirNextIdx(level); ok {
		if idx < ftsMergeThreshold {
			return idx, nil
		}
		return e.crisisReallocIdx(tableName, level, ftsTable)
	}
	rows := e.readFTSSegdirRows(tableName, level)
	if len(rows) >= ftsMergeThreshold {
		if res := e.crisisMergeFTSLevel(tableName, level, ftsTable); res != nil {
			return 0, res
		}
		return 0, nil // the level was cleared; idx restarts at 0
	}
	idx, idxCached := ftsTable.SegdirNextIdx(level)
	if !idxCached {
		idx = e.ftSSegmentIdx(tableName, level)
		ftsTable.SetSegdirNextIdx(level, idx)
	}
	return idx, nil
}

// crisisMergeFTSLevel merges ALL segments at one absolute level into a single
// segment at level+1 (SQLite's fts3SegmentMerge, called by fts3AllocateSegdirIdx
// when a level reaches MergeCount=16). The merged output covers the union of
// the source segments' doc IDs. On success the level's rows are deleted and the
// output row written; the segdir-idx cache is invalidated (levels renumbered).
// A corrupt source segment fails the operation ("database disk image is
// malformed"), matching SQLite (fts3corrupt 1.3: the 17th insert reads the
// corrupted root and fails).
func (e *DDLExecutor) crisisMergeFTSLevel(tableName string, level int, ftsTable *fts.FTS3Table) *Result {
	rows := e.readFTSSegdirRows(tableName, level)
	if len(rows) < ftsMergeThreshold {
		return nil
	}
	// Stream every source segment's (term → doclist) entries and merge them
	// per term, oldest row first. Unlike the previous live-index rebuild this
	// PRESERVES delete-marker tombstones ([docid][0]): a tombstone dropped
	// here lets OLDER segments at higher levels (which still hold the deleted
	// document's postings) resurrect it on reload — integrity-check then
	// fails with extra terms (fts4opt 2.x churn). SQLite's fts3SegWriter
	// keeps empty doclists in merge outputs for the same reason.
	mergedDoclists := make(map[string][][]byte)
	for _, row := range rows {
		_, termDoclists, err := e.segdirRowStreamDoclists(tableName, row.root, row.leavesEndBlock)
		if err != nil {
			return &Result{Error: fmt.Errorf("database disk image is malformed [FLU1]")}
		}
		for term, dl := range termDoclists {
			mergedDoclists[term] = append(mergedDoclists[term], dl)
		}
	}
	terms := make([]string, 0, len(mergedDoclists))
	for term := range mergedDoclists {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	nodeSize := e.ftsNodeSize(ftsTable)
	rootBlob, blocks := fts.BuildSegmentBlocks(terms, func(term string) []byte {
		return fts.MergeDoclists(mergedDoclists[term]...)
	}, nodeSize)
	nextLevel := level + 1
	// The output keeps each source segment's stored term forms verbatim, so
	// it serves whichever index (main or prefix i) owns this absolute level
	// (fts3_write.c fts3SegWriter merges each index separately; the previous
	// live-index rebuild re-tokenized FULL terms into prefix outputs and
	// corrupted the prefix index — fts4opt 1.x integrity-check).
	// Allocate the output idx at level+1 (recursively crisis-merging it if
	// it too is full — SQLite's fts3SegWriterFlush → fts3AllocateSegdirIdx).
	outIdx, res := e.allocFTSIdx(tableName, nextLevel, ftsTable)
	if res != nil {
		return res
	}
	// outStart is the FIRST block id writeFTSShadowRow recorded in the row
	// AND patched into the root; the leaf writes below MUST reuse it. A
	// separate cache read can diverge from the row's range (the cache is
	// invalidated by the segdir write itself), leaving the row pointing at
	// blocks that were never written ("database disk image is malformed" on
	// the next reader — fts4opt 2.x churn).
	outStart := e.writeFTSShadowRow(tableName, nextLevel, outIdx, blocks, rootBlob)
	ftsTable.SetSegdirNextIdx(nextLevel, outIdx+1)
	// Write the merged segment's leaf blocks to %_segments.
	nextBlock := outStart
	if nextBlock == 0 {
		// Root-only output: no leaf blocks; fall back to the cache for the
		// next segment's allocation.
		var blockCached bool
		nextBlock, blockCached = ftsTable.NextBlockID()
		if !blockCached {
			nextBlock = e.ftsNextBlockID(tableName)
		}
	}
	for _, blk := range blocks {
		_ = e.ctx.Exec(&sql.InsertStmt{
			Table:   tableName + "_segments",
			Columns: []string{"blockid", "block"},
			Values: [][]sql.Expr{
				{
					&sql.NumericLit{Value: fmt.Sprintf("%d", nextBlock)},
					&sql.BlobLit{Value: blk.Block},
				},
			},
		})
		nextBlock++
	}
	ftsTable.SetNextBlockID(nextBlock)
	// Delete the consumed source rows AND their %_segments blocks
	// (fts3DeleteSegment for each source segment of fts3SegmentMerge).
	// The deletion range extends to each segment's end_block first component:
	// for a merge output that is the NULL pre-allocation marker, which dies
	// with its segment (an orphaned marker below a later segment's leaves_end
	// reads as corruption in the block-range walk).
	for _, row := range rows {
		e.deleteFTSBlocksRangeWithMarker(tableName, row, int(e.segdirRowLeavesEnd(row.leavesEndBlock)))
	}
	e.deleteFTSSegdirLevel(tableName, level)
	ftsTable.InvalidateSegmentCache()
	return nil
}

// writeFTSStat writes the FTS4 %_stat doctotal row (id=0,
// FTS_STAT_DOCTOTAL — fts3_write.c fts3UpdateDocTotals) after a segment
// flush: the blob is [nDoc varint][per-column total token varints][total text
// bytes varint] (nCol+2 FTS3 varints: varint 0 = row count, varints 1..nCol =
// per-column token totals, varint 1+nCol = total bytes of all text values).
// The corruption tests read/corrupt this row (fts3corrupt 5.2/5.3), so it
// must exist for FTS4 tables.
func (e *DDLExecutor) writeFTSStat(tableName string, ftsTable *fts.FTS3Table) {
	stat := tableName + "_stat"
	if _, _, err := e.ctx.FindTable(stat); err != nil {
		return // FTS3 has no _stat table
	}
	nDoc, totals, totalBytes := ftsTable.StatTotals()
	var blob []byte
	blob = fts.AppendFTS3Varint(blob, uint64(nDoc))
	for _, total := range totals {
		blob = fts.AppendFTS3Varint(blob, uint64(total))
	}
	// The final varint is the total size in bytes of all text values in all
	// columns (fts3.c fts3UpdateDocTotals: aSz[p->nColumn] accumulates
	// sqlite3_column_bytes over the content columns).
	blob = fts.AppendFTS3Varint(blob, uint64(totalBytes))
	// Upsert the id=0 doctotal row (a single REPLACE — SQLite's
	// SQL_REPLACE_STAT; DELETE+INSERT would cost two statement executions per
	// flush and dominate per-row FTS builds).
	e.writeFTSStatRow(tableName, 0, blob)
}

// writeFTSStatRow upserts one %_stat row (id, value BLOB) with a single
// REPLACE (SQLite's SQL_REPLACE_STAT). Used for the doctotal (id=0) and the
// incr-merge hint (id=1, fts4merge 4.4.1). The %_stat table is created on
// demand (SQLite's fts3DoIncrmerge calls sqlite3Fts3CreateStatTable for an
// fts3 table — fts4merge 1.3 runs merge=1 on an fts3 table and reads the
// hint).
func (e *DDLExecutor) writeFTSStatRow(tableName string, id int, blob []byte) {
	stat := tableName + "_stat"
	if _, _, err := e.ctx.FindTable(stat); err != nil {
		if res := e.createShadowTableSQL(stat, []sql.ColumnDef{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "value", Type: "BLOB"},
		}); res.Error != nil {
			return
		}
	}
	_ = e.ctx.Exec(&sql.InsertStmt{
		Table:     stat,
		Columns:   []string{"id", "value"},
		IsReplace: true,
		Values: [][]sql.Expr{
			{
				&sql.NumericLit{Value: fmt.Sprintf("%d", id)},
				&sql.BlobLit{Value: blob},
			},
		},
	})
}

// clearFTSStatRow deletes one %_stat row (used to clear the incr-merge hint
// after its level is fully consumed).
func (e *DDLExecutor) clearFTSStatRow(tableName string, id int) {
	stat := tableName + "_stat"
	_ = e.ctx.Exec(&sql.DeleteStmt{Table: stat, Where: &sql.BinaryOp{
		Operator: "=",
		Left:     &sql.ColumnRef{Name: "id"},
		Right:    &sql.NumericLit{Value: fmt.Sprintf("%d", id)},
	}})
}

// readFTSStatRow reads one %_stat row's value blob (nil when the row or table
// is absent). Used for the incr-merge hint (id=1).
func (e *DDLExecutor) readFTSStatRow(tableName string, id int) []byte {
	cursor, ok := e.shadowTableCursor(tableName, "_stat")
	if !ok {
		return nil
	}
	for {
		cell, rec, ok := nextShadowRecord(cursor, 2)
		if !ok {
			break
		}
		if v, ok := statRowValue(rec, cell.RowID, int64(id)); ok {
			return v
		}
		if !advanceSequenceCursor(cursor) {
			break
		}
	}
	return nil
}

// statRowValue decodes one %_stat row's value blob when the row matches the
// wanted id. id is the %_stat table's INTEGER PRIMARY KEY: the value IS the
// rowid and the record slot decodes as NULL (SQLite's rowid-alias storage
// convention), so the match must use the cell's rowid.
func statRowValue(rec *storage.Record, rowID, id int64) ([]byte, bool) {
	if rowID != id {
		return nil, false
	}
	switch v := rec.Values[1].(type) {
	case []byte:
		return v, true
	case string:
		return []byte(v), true
	}
	return nil, true
}

// updateFTSShadowRowRangeKeepEndBlock updates a %_segdir row's start_block,
// leaves_end_block and root while PRESERVING the end_block column (SQLite's
// SQL_CHOMP_SEGDIR only sets start_block and root — the end_block size and
// marker id survive truncation untouched).
func (e *DDLExecutor) updateFTSShadowRowRangeKeepEndBlock(tableName string, level, idx, startBlock, leavesEndBlock int, root []byte) {
	segdir := tableName + "_segdir"
	_ = e.ctx.Exec(&sql.UpdateStmt{
		Table: segdir,
		Assignments: []sql.Assignment{
			{Column: "start_block", Value: &sql.NumericLit{Value: fmt.Sprintf("%d", startBlock)}},
			{Column: "leaves_end_block", Value: &sql.NumericLit{Value: fmt.Sprintf("%d", leavesEndBlock)}},
			{Column: "root", Value: &sql.BlobLit{Value: root}},
		},
		Where: &sql.BinaryOp{
			Operator: "AND",
			Left:     &sql.BinaryOp{Operator: "=", Left: &sql.ColumnRef{Name: "level"}, Right: &sql.NumericLit{Value: fmt.Sprintf("%d", level)}},
			Right:    &sql.BinaryOp{Operator: "=", Left: &sql.ColumnRef{Name: "idx"}, Right: &sql.NumericLit{Value: fmt.Sprintf("%d", idx)}},
		},
	})
}
