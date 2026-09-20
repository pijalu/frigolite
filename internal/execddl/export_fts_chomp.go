// Package execddl: FTS incremental-merge CHOMB + segdir-level helpers
// (fts3IncrmergeChomp's truncate/delete loop, %_segdir renumbering, block
// cleanup, merge-hint LIST encoding/decoding, and the k-way merge heap).
// Split from export_fts_merge.go; behavior unchanged.
package execddl

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/btree"

	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// segReader is one source segment of an incremental merge: its %_segdir row
// and the lazy stream over its terms (fts.SegmentStreamReader, the engine's
// Fts3SegReader equivalent).
type segReader struct {
	row    ftsSegdirRow
	reader *fts.SegmentStreamReader
}

// promoteFTSSegments implements SQLite's fts3PromoteSegments (fts3_write.c
// 3119-3205), run after an incremental merge fully consumed its level and
// wrote a segment of nLeafData leaf bytes at level outLevel: if EVERY segment
// at higher levels of the same index (levels outLevel+1 .. 1023 for the main
// index) carries a size in end_block and is <= (nLeafData*3)/2 bytes, they
// are ALL moved down to outLevel in one batch (oldest first), so the next
// merge can combine them instead of maintaining separate levels — the source
// of the oracle's sparse structures like "1 1 | 2 1 | 4 1 | 6 1".
//
// Like SQLite, the move is two-phase to keep (level, idx) pairs addressable:
// each row is first re-staged at (level=-1, idx=0..N-1) in (level DESC, idx
// ASC) order, then all staged rows become level=outLevel. Rows are rewritten
// in place under their own rowid (btree replace).
// ftsPromotionRow mirrors one %_segdir row during segment promotion.
type ftsPromotionRow struct {
	rowid  int64
	level  int
	idx    int
	size   int64
	values []interface{}
}

func (e *DDLExecutor) promoteFTSSegments(tableName string, ftsTable *fts.FTS3Table, outLevel, nLeafData int) {
	if nLeafData <= 0 {
		return
	}
	segdir := tableName + "_segdir"
	segEntry, _, err := e.ctx.FindTable(segdir)
	if err != nil || segEntry == nil {
		return
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return
	}
	candidates := e.collectPromotionCandidates(cursor, outLevel)
	if len(candidates) == 0 {
		return
	}
	limit := (int64(nLeafData) * 3) / 2
	if !promotionAllowed(candidates, outLevel, limit) {
		return
	}
	// (level DESC, idx ASC) — SQL_SELECT_LEVEL_RANGE2's order; the oldest
	// segment becomes idx 0 at the promoted level.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].level != candidates[j].level {
			return candidates[i].level > candidates[j].level
		}
		return candidates[i].idx < candidates[j].idx
	})
	rewritePromotedRows(tree, candidates, outLevel)
	// Promoted levels' writer state is stale: drop it so the next merge at
	// outLevel starts fresh outputs instead of appending to moved segments.
	ftsTable.InvalidateSegmentCacheKeepMergeCtx()
	for _, c := range candidates {
		ftsTable.ClearMergeCtx(c.level)
	}
}

// collectPromotionCandidates scans the %_segdir rows eligible for promotion
// (the outLevel band, same index only); see promoteFTSSegments.
func (e *DDLExecutor) collectPromotionCandidates(cursor *btree.Cursor, outLevel int) []ftsPromotionRow {
	var candidates []ftsPromotionRow
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) < 5 {
			break
		}
		lv, lvOK := rec.Values[0].(int64)
		ix, ixOK := rec.Values[1].(int64)
		if !lvOK || !ixOK {
			continue
		}
		appendPromotionCandidate(&candidates, rec, cell.RowID, int(lv), int(ix), outLevel)
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	return candidates
}

// appendPromotionCandidate collects a %_segdir row into the promotion set
// when it is inside the outLevel band. Same index only: absolute levels
// within this group's 1024-level band (fts3PromoteSegments binds
// SQL_SELECT_LEVEL_RANGE2 with iLast = next index's base - 1). Levels >=
// outLevel are all renumbered — SQLite's fts3PromoteSegments renumbers EVERY
// row from iAbsLevel up (SQL_SELECT_LEVEL_RANGE2 WHERE level BETWEEN
// iAbsLevel AND iLast), not just the higher ones, so the promoted rows and
// the output level's existing rows share one sequential idx sequence
// (otherwise the staged idx 0.. collides with the output level's own rows —
// duplicate (level, idx), fts4merge4 tx19: L1 = [i0 i1 i0 i2]).
func appendPromotionCandidate(candidates *[]ftsPromotionRow, rec *storage.Record, rowid int64, lv, ix, outLevel int) {
	promoBase := (outLevel / 1024) * 1024
	if lv < outLevel || lv >= promoBase+1024 {
		return
	}
	// Segment size from end_block's "<end> <size>" text suffix
	// (fts3ReadEndBlockField). Missing/zero blocks the whole promotion.
	// Only HIGHER levels are size-checked (the output level's rows stay).
	var size int64
	if lv > outLevel {
		size = segdirEndBlockSize(rec)
	}
	*candidates = append(*candidates, ftsPromotionRow{rowid: rowid, level: lv, idx: ix, size: size, values: rec.Values})
}

// segdirEndBlockSize decodes end_block's "<end> <size>" size suffix.
func segdirEndBlockSize(rec *storage.Record) int64 {
	var size int64
	switch eb := rec.Values[4].(type) {
	case string:
		fields := strings.Fields(eb)
		if len(fields) >= 2 {
			size, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	case []byte:
		fields := strings.Fields(string(eb))
		if len(fields) >= 2 {
			size, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return size
}

// promotionAllowed applies the 3/2 rule: any higher-level segment that is
// oversized or has an unknown size blocks the promotion entirely.
func promotionAllowed(candidates []ftsPromotionRow, outLevel int, limit int64) bool {
	for _, c := range candidates {
		if c.level > outLevel && (c.size <= 0 || c.size > limit) {
			return false
		}
	}
	return true
}

// rewritePromotedRows re-stages the candidates at level=-1 with sequential
// idx (phase 1), then moves them all to outLevel (phase 2); rows are
// rewritten in place under their own rowid (btree replace).
func rewritePromotedRows(tree *btree.BTree, candidates []ftsPromotionRow, outLevel int) {
	for i := range candidates {
		candidates[i].values[0] = int64(-1)
		candidates[i].values[1] = int64(i)
		if payload, perr := storage.EncodeRecord(candidates[i].values); perr == nil {
			_ = tree.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: candidates[i].rowid, Payload: payload})
		}
	}
	for i := range candidates {
		candidates[i].values[0] = int64(outLevel)
		if payload, perr := storage.EncodeRecord(candidates[i].values); perr == nil {
			_ = tree.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: candidates[i].rowid, Payload: payload})
		}
	}
}

// deleteFTSBlocksRangeWithMarker deletes a consumed segment's %_segments
// blocks from start_block through the end_block first component INCLUSIVE:
// for a merge output the range end is the pre-allocated reservation's NULL
// marker row, which dies with its segment (fts3DeleteSegment deletes
// "blockid BETWEEN iStart AND iEnd" where iEnd is the marker id). Without
// this the orphaned markers accumulate (fts4growth 1.5: the oracle shows the
// marker only while the output is live).
func (e *DDLExecutor) deleteFTSBlocksRangeWithMarker(tableName string, row ftsSegdirRow, leavesEnd int) {
	// The chomp deletes only the segment's OWN leaf blocks [start_block,
	// leaves_end_block] — matching C's fts3DeleteSegment which uses the
	// segment's iStartBlock..iEndBlock range exclusively. Extending the
	// deletion to the end_block marker id would sweep in blocks belonging
	// to OTHER segments whose ranges numerically interleave with this one
	// (the cascade's chomp ranges and the re-leveled rows' preserved
	// start_blocks interleave after prepare_for_optimize).
	end := leavesEnd
	start := segdirRowStart(row)
	if start <= 0 {
		// Root-only segment: no %_segments blocks of its own; still remove a
		// marker if one was recorded at an id above every leaf.
		if end > 0 && end > leavesEnd {
			e.deleteFTSBlocks(tableName, end, end)
		}
		return
	}
	e.deleteFTSBlocks(tableName, start, end)
}

// chompFTSMerge truncates each source segment of a finished incremental merge
// to its UNMERGED terms (SQLite's fts3IncrmergeChomp / fts3TruncateSegment):
// a segment whose terms were all merged is deleted (the reader is at EOF); a
// partially-consumed segment keeps its terms from the reader's current
// position onward — fts.TruncateNode trims each node on the root-to-leaf
// path and every trimmed block is rewritten IN PLACE under its own block id,
// so untouched segments' id relationships stay stable. Returns the number of
// segments deleted and the number truncated. An error (corrupt segment,
// unreadable block, failed write) propagates to the caller, which ABORTS the
// merge before writing the output %_segdir row — SQLite's chomp rc flows
// back into sqlite3Fts3Incrmerge's rc and gates the release/hint writes.
func (e *DDLExecutor) chompFTSMerge(tableName string, level int, readers []segReader) (deleted, truncated int, err error) {
	for _, sr := range readers {
		// Use the row's OWN level: a merge's source list can span levels
		// (a single-leaf continuation output from level+1 re-merged as a
		// source), so the segdir delete must target sr.row.level, not the
		// iteration's base level.
		if sr.reader.AtEOF() {
			if sr.reader.Err() != nil {
				// AtEOF is also true when the reader FAILED (corrupt
				// block mid-segment). SQLite's chomp sees pSeg->aNode!=0
				// only after a clean step; a failed reader means rc
				// propagates and the whole merge aborts. Deleting the
				// segment here would silently DROP its unmerged terms.
				return deleted, truncated, fmt.Errorf("corrupt segment (chomp reader: %v)", sr.reader.Err())
			}
			// Delete the consumed segment's %_segments blocks before removing
			// its %_segdir row (fts3DeleteSegment: blockid BETWEEN start AND
			// leaves_end; guarded by iStartBlock != 0).
			e.deleteFTSBlocksRangeWithMarker(tableName, sr.row, int(e.segdirRowLeavesEnd(sr.row.leavesEndBlock)))
			e.deleteFTSSegdirIdx(tableName, sr.row.level, sr.row.idx)
			deleted++
			continue
		}
		n, cerr := e.chompPartialSegment(tableName, sr)
		if cerr != nil {
			return deleted, truncated, cerr
		}
		truncated += n
	}
	e.repackFTSSegdirLevel(tableName, level)
	return deleted, truncated, nil
}

// chompPartialSegment truncates one partially-consumed source segment to its
// unmerged terms (SQLite's fts3TruncateSegment): trim the root, then descend
// into the child reported by each truncation, rewriting every trimmed block
// IN PLACE under its own block id. iNewStart ends up holding the first valid
// leaf (0 for a root-only segment).
func (e *DDLExecutor) chompPartialSegment(tableName string, sr segReader) (int, error) {
	// The reader's current term is SQLite's pSeg->zTerm chomp bound.
	zTerm, _, _ := sr.reader.Current()

	oldStart := segdirRowStart(sr.row)
	oldLeavesEnd := int(e.segdirRowLeavesEnd(sr.row.leavesEndBlock))
	rootBlob := fts.RootBlobBytes(sr.row.root)

	newRoot, iBlock := fts.TruncateNode(rootBlob, zTerm)
	if newRoot == nil {
		// Corrupt root: SQLite's fts3TruncateNode returns
		// FTS_CORRUPT_VTAB and the merge aborts.
		return 0, fmt.Errorf("database disk image is malformed (chomp root)")
	}
	iNewStart := int64(0)
	for iBlock != 0 {
		iNewStart = iBlock
		blk, res := e.readFTSBlock(tableName, int(iBlock))
		if res != nil || blk == nil {
			return 0, fmt.Errorf("database disk image is malformed (chomp block %d)", iBlock)
		}
		nb, next := fts.TruncateNode(blk, zTerm)
		if nb == nil {
			return 0, fmt.Errorf("database disk image is malformed (chomp node %d)", iBlock)
		}
		if ures := e.ctx.Exec(&sql.UpdateStmt{
			Table: tableName + "_segments",
			Assignments: []sql.Assignment{{
				Column: "block",
				Value:  &sql.BlobLit{Value: nb},
			}},
			Where: &sql.BinaryOp{
				Operator: "=",
				Left:     &sql.ColumnRef{Name: "blockid"},
				Right:    &sql.NumericLit{Value: fmt.Sprintf("%d", iNewStart)},
			},
		}); ures != nil && ures.Error != nil {
			return 0, ures.Error
		}
		iBlock = next
	}
	// Delete the leading dead run; SQL_CHOMP_SEGDIR keeps leaves_end_block.
	if iNewStart > 0 {
		e.deleteFTSBlocks(tableName, oldStart, int(iNewStart)-1)
	}
	e.updateFTSShadowRowRangeKeepEndBlock(tableName, sr.row.level, sr.row.idx, int(iNewStart), oldLeavesEnd, newRoot)
	return 1, nil
}

// deleteFTSSegdirIdx deletes one %_segdir row at (level, idx).
func (e *DDLExecutor) deleteFTSSegdirIdx(tableName string, level, idx int) {
	segdir := tableName + "_segdir"
	_ = e.ctx.Exec(&sql.DeleteStmt{Table: segdir, Where: &sql.BinaryOp{
		Operator: "AND",
		Left:     &sql.BinaryOp{Operator: "=", Left: &sql.ColumnRef{Name: "level"}, Right: &sql.NumericLit{Value: fmt.Sprintf("%d", level)}},
		Right:    &sql.BinaryOp{Operator: "=", Left: &sql.ColumnRef{Name: "idx"}, Right: &sql.NumericLit{Value: fmt.Sprintf("%d", idx)}},
	}})
}

// repackFTSSegdirLevel renumbers the %_segdir rows at one level to 0..n-1
// in idx order (SQLite's fts3RepackSegdirLevel: SQL_SELECT_INDEXES reads
// the surviving idx values ASC, then SQL_SHIFT_SEGDIR_ENTRY runs
// UPDATE ... SET idx=:new WHERE level=? AND idx=:old for each shifted row —
// in place, never re-inserting). Processing ascending is collision-free:
// survivor idx values are strictly increasing and every target i is <= the
// old value it replaces, so no UPDATE can hit a live (level, idx) pair.
// fts4merge 4.3 expects the surviving level-0 rows to be 0..13, 0..12, ...
//
// The engine performs the same in-place renumber by rewriting each shifted
// row's record under the SAME rowid (btree replace) — rowids never change,
// so the merge's captured next-rowid cannot collide with repacked rows
// (fts4merge4 2.2.x duplicate-row trigger).
func (e *DDLExecutor) repackFTSSegdirLevel(tableName string, level int) {
	segdir := tableName + "_segdir"
	segEntry, _, err := e.ctx.FindTable(segdir)
	if err != nil || segEntry == nil {
		return
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return
	}
	rows := collectSegdirLevelRows(cursor, level)
	sort.Slice(rows, func(i, j int) bool { return rows[i].idx < rows[j].idx })
	needRepack := false
	for i := range rows {
		if rows[i].idx != i {
			needRepack = true
			break
		}
	}
	if !needRepack {
		// Even when no idx changed, the merge's chomp may have DELETED rows
		// (raising the implicit allocator's stale max out of sync); the next
		// flush INSERT with an implicit rowid would reuse a deleted rowid and
		// REPLACE a live row (fts4merge4 tx22: the flush's L0 row replaced the
		// survivor, L0 stayed 1 instead of 2).
		e.ctx.InvalidateRowIDCache(e.ctx.TablePager(segdir), segEntry.RootPage)
		return
	}
	rewriteRepackedRows(tree, rows)
	// The in-place rewrites go through the btree directly, bypassing the DML
	// rowid-allocator bookkeeping; a subsequent flush INSERT with an implicit
	// rowid would reuse the stale cached max and REPLACE a live row (a
	// duplicate rowid reappearing as i0/r26 twice). Drop the cache so the
	// next allocation rescans the true max (SQLite recomputes max(rowid)
	// after every write too).
	e.ctx.InvalidateRowIDCache(e.ctx.TablePager(segdir), segEntry.RootPage)
}

// ftsSegdirLevelRow is one same-level %_segdir row collected for a repack.
type ftsSegdirLevelRow struct {
	rowid  int64
	idx    int
	values []interface{}
}

// collectSegdirLevelRows reads all %_segdir rows at one level; see
// repackFTSSegdirLevel.
func collectSegdirLevelRows(cursor *btree.Cursor, level int) []ftsSegdirLevelRow {
	var rows []ftsSegdirLevelRow
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) < 2 {
			break
		}
		lv, lvOK := rec.Values[0].(int64)
		ix, ixOK := rec.Values[1].(int64)
		if lvOK && ixOK && int(lv) == level {
			rows = append(rows, ftsSegdirLevelRow{rowid: cell.RowID, idx: int(ix), values: rec.Values})
		}
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	return rows
}

// rewriteRepackedRows renumbers the shifted rows 0..n-1 in place under their
// own rowid; see repackFTSSegdirLevel.
func rewriteRepackedRows(tree *btree.BTree, rows []ftsSegdirLevelRow) {
	for i := range rows {
		if rows[i].idx == i {
			continue
		}
		rows[i].values[1] = int64(i)
		payload, perr := storage.EncodeRecord(rows[i].values)
		if perr != nil {
			return
		}
		if ierr := tree.InsertCell(&storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   rows[i].rowid,
			Payload: payload,
		}); ierr != nil {
			return
		}
	}
}

// deleteFTSBlocks deletes one %_segments block range (SQLite fts3DeleteSegment
// via SQL_DELETE_SEGMENTS_RANGE: DELETE FROM %_segments WHERE blockid BETWEEN
// :start AND :end). A consumed segment's blocks must not linger: they bloat
// the %_segments btree, slow every scan, and inflate the next merge's nLeafEst
// source estimate (fts4merge4 2.2.x).
func (e *DDLExecutor) deleteFTSBlocks(tableName string, startBlock, endBlock int) {
	if startBlock <= 0 || endBlock < startBlock {
		return
	}
	_ = e.ctx.Exec(&sql.DeleteStmt{
		Table: tableName + "_segments",
		Where: &sql.Between{
			Operand: &sql.ColumnRef{Name: "blockid"},
			Low:     &sql.NumericLit{Value: fmt.Sprintf("%d", startBlock)},
			High:    &sql.NumericLit{Value: fmt.Sprintf("%d", endBlock)},
		},
	})
}

// deleteFTSSegdirLevel deletes all %_segdir rows at one absolute level
// (sqlite3Fts3Incrmerge removes the input segments after the merge).
func (e *DDLExecutor) deleteFTSSegdirLevel(tableName string, level int) {
	segdir := tableName + "_segdir"
	_ = e.ctx.Exec(&sql.DeleteStmt{Table: segdir, Where: &sql.BinaryOp{
		Operator: "=",
		Left:     &sql.ColumnRef{Name: "level"},
		Right:    &sql.NumericLit{Value: fmt.Sprintf("%d", level)},
	}})
}

// ftsHintEntry is one (level, nSeg) incr-merge hint list entry.
type ftsHintEntry struct {
	level int
	nSeg  int
}

// ftsParseHintList decodes every (level, nSeg) pair of a hint blob.
// ftsParseHintList decodes every (level, nSeg) pair of a hint blob using the
// FTS3 little-endian varint codec (the same one ftsEncodeHintList writes
// with) — NOT util.GetVarint, whose big-endian record format would misread
// multi-byte values.
func ftsParseHintList(blob []byte) []ftsHintEntry {
	var out []ftsHintEntry
	pos := 0
	for pos < len(blob) {
		lv, n1 := fts.GetFTS3Varint(blob[pos:])
		if n1 == 0 {
			break
		}
		pos += n1
		ns, n2 := util.GetVarint(blob[pos:])
		if n2 == 0 {
			break
		}
		pos += n2
		out = append(out, ftsHintEntry{level: int(lv), nSeg: int(ns)})
	}
	return out
}

// ftsEncodeHintList serializes a hint list to the %_stat blob.
func ftsEncodeHintList(entries []ftsHintEntry) []byte {
	var out []byte
	for _, e := range entries {
		out = fts.AppendFTS3Varint(out, uint64(e.level))
		out = fts.AppendFTS3Varint(out, uint64(e.nSeg))
	}
	return out
}

// mergeHeap is a min-heap of the incremental merge's per-segment stream
// readers, keyed by their current term — the k-way merge order of SQLite's
// Fts3MultiSegReader (sqlite3Fts3SegReaderStep picks the segment with the
// smallest current term).
type mergeHeap []mergeHeapEntry

// mergeHeapEntry is one reader's current term in the merge heap.
type mergeHeapEntry struct {
	term    string
	docIDs  []int64
	doclist []byte
	size    int
	reader  *fts.SegmentStreamReader
	// seq is the creating reader's age ordinal (rows are ordered oldest
	// first). The heap breaks term ties on seq so same-term doclists merge
	// oldest→newest: SQLite's "newer data knocks out older data" rule makes
	// the LAST processed doclist win, so a delete marker in a newer segment
	// must be applied AFTER the older segment's position entries
	// (fts3.c "Handling of deletions and updates").
	seq int
}

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if h[i].term != h[j].term {
		return h[i].term < h[j].term
	}
	return h[i].seq < h[j].seq
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *mergeHeap) Push(x interface{}) { *h = append(*h, x.(mergeHeapEntry)) }
func (h *mergeHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// peekTerm returns the smallest term in the heap (the heap's top), or "" when
// empty.
func (h mergeHeap) peekTerm() string {
	if len(h) == 0 {
		return ""
	}
	return h[0].term
}
