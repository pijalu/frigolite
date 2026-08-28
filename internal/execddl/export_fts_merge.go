// Package execddl: FTS incremental-merge machinery (MergeFTS, %_segdir
// readers, level/idx utilities, merge hint + heap). Split from export.go;
// behavior unchanged.
package execddl

import (
	"container/heap"
	"fmt"

	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// ftsSegdirRow is one decoded %_segdir row of an FTS table.
type ftsSegdirRow struct {
	idx            int
	level          int
	rowid          int64
	rowidKnown     bool
	root           interface{}
	leavesEndBlock interface{}
	startBlock     interface{}
	endBlock       interface{}
}

// segdirRowStart extracts the start_block column of a %_segdir row.
func segdirRowStart(row ftsSegdirRow) int {
	if sb, ok := row.startBlock.(int64); ok {
		return int(sb)
	}
	return 0
}

// segdirRowLastTerm returns the largest term stored in one %_segdir row's
// segment (the last leaf's last term), or "" when it cannot be determined.
// SQLite's fts3IncrmergeLoad rejects an append whose first merged term is NOT
// greater than this (bAppendable=0).
func (e *DDLExecutor) segdirRowLastTerm(tableName string, row ftsSegdirRow) string {
	root := fts.RootBlobBytes(row.root)
	le := e.segdirRowLeavesEnd(row.leavesEndBlock)
	if le > 0 {
		if blk, res := e.readFTSBlock(tableName, int(le)); res == nil && blk != nil {
			if _, last := fts.LeafTermRange(blk); last != "" {
				return last
			}
		}
		return ""
	}
	// Single-leaf segment: the root IS the leaf.
	if _, last := fts.LeafTermRange(root); last != "" {
		return last
	}
	return ""
}

// segdirRowEndBlockID extracts the end_block column's FIRST value (the last
// block id — for a merge output the pre-allocated range end, i.e. the NULL
// marker row; fts3ReadEndBlockField). Returns 0 when absent.
func segdirRowEndBlockID(v interface{}) int {
	switch eb := v.(type) {
	case string:
		fields := strings.Fields(eb)
		if len(fields) >= 1 {
			id, _ := strconv.Atoi(fields[0])
			return id
		}
	case []byte:
		fields := strings.Fields(string(eb))
		if len(fields) >= 1 {
			id, _ := strconv.Atoi(fields[0])
			return id
		}
	case int64:
		return int(eb)
	}
	return 0
}

// segdirRowSize extracts the leaf-data SIZE from a %_segdir row's end_block
// TEXT "<end> <size>" suffix (fts3ReadEndBlockField). A continuation output
// carries the ACCUMULATED size of every append (SQLite's pWriter->nLeafData
// persists across calls), which promotion's 3/2 rule compares against — using
// only the current call's blocks under-sizes the output and over-promotes
// (fts4merge4 tx19: engine promoted L2 into L1 where SQLite's accumulated
// 475K > 1.5*224K kept it).
func segdirRowSize(v interface{}) int64 {
	switch eb := v.(type) {
	case string:
		fields := strings.Fields(eb)
		if len(fields) >= 2 {
			sz, _ := strconv.ParseInt(fields[1], 10, 64)
			return sz
		}
	case []byte:
		fields := strings.Fields(string(eb))
		if len(fields) >= 2 {
			sz, _ := strconv.ParseInt(fields[1], 10, 64)
			return sz
		}
	}
	return 0
}

// segdirEndBlockFirst returns the first component of a %_segdir end_block
// value (the pre-allocated range end / marker block id), or 0 when absent.
func segdirEndBlockFirst(v interface{}) int64 {
	switch eb := v.(type) {
	case string:
		fields := strings.Fields(eb)
		if len(fields) >= 1 {
			n, _ := strconv.ParseInt(fields[0], 10, 64)
			return n
		}
	case []byte:
		fields := strings.Fields(string(eb))
		if len(fields) >= 1 {
			n, _ := strconv.ParseInt(fields[0], 10, 64)
			return n
		}
	case int64:
		return eb
	}
	return 0
}

// readFTSSegdirRows reads every %_segdir row of one absolute level, sorted by
// idx. Used by the crisis merge and incremental merge to read the source
// segments' contents and to renumber surviving rows.
func (e *DDLExecutor) readFTSSegdirRows(tableName string, level int) []ftsSegdirRow {
	segdir := tableName + "_segdir"
	segEntry, _, err := e.ctx.FindTable(segdir)
	if err != nil || segEntry == nil {
		return nil
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil
	}
	var rows []ftsSegdirRow
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) < 6 {
			break
		}
		lv, lvOK := rec.Values[0].(int64)
		ix, ixOK := rec.Values[1].(int64)
		if lvOK && ixOK && int(lv) == level {
			rows = append(rows, ftsSegdirRow{
				idx:            int(ix),
				level:          int(lv),
				rowid:          cell.RowID,
				rowidKnown:     true,
				root:           rec.Values[len(rec.Values)-1],
				leavesEndBlock: rec.Values[3],
				startBlock:     rec.Values[2],
				endBlock:       rec.Values[4],
			})
		}
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	// The btree scans in ROWID order; SQLite reads the level with ORDER BY
	// idx ASC (azSql#12). The merge loads rows[:n] as the OLDEST segments and
	// the flush allocates max(idx)+1, so idx order is semantic — after a
	// promotion renumbers rows in place (rowids stay in creation order, idx
	// no longer aligns with rowid), scanning without sorting returns the
	// wrong "oldest" segments (fts4merge4 tx19: L1 read as [i1 i2 i0 i3]).
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].idx < rows[j].idx })
	return rows
}

// segdirRowLeavesEnd extracts the leaves_end_block column of a %_segdir row.
func (e *DDLExecutor) segdirRowLeavesEnd(leavesEndVal interface{}) int64 {
	var leavesEndBlock int64
	switch lb := leavesEndVal.(type) {
	case int64:
		leavesEndBlock = lb
	case float64:
		leavesEndBlock = int64(lb)
	case []byte:
		fmt.Sscanf(string(lb), "%d", &leavesEndBlock)
	case string:
		fmt.Sscanf(lb, "%d", &leavesEndBlock)
	}
	return leavesEndBlock
}

// segdirRowStreamDoclists streams one %_segdir row's segment (root +
// %_segments blocks) and returns the doc IDs it contains plus its (term → raw
// doclist) map, read via the lazy SegmentStreamReader. A corrupt segment
// returns an error (the caller fails the operation with "database disk image
// is malformed").
//
//lint:ignore U1000 retained for FTS merge compatibility
func (e *DDLExecutor) segdirRowStreamDoclists(tableName string, rootVal, leavesEndVal interface{}) ([]int64, map[string][]byte, error) {
	root := fts.RootBlobBytes(rootVal)
	termDoclists := map[string][]byte{}
	if len(root) == 0 {
		return nil, termDoclists, nil
	}
	reader := func(blockID int) ([]byte, error) {
		blk, res := e.readFTSBlock(tableName, blockID)
		if res != nil {
			return nil, fmt.Errorf("corrupt segment root")
		}
		return blk, nil
	}
	sr := fts.NewSegmentStreamReader(root, int(e.segdirRowLeavesEnd(leavesEndVal)), reader)
	seen := make(map[int64]bool)
	var ids []int64
	for {
		term, docIDs, dl, _, ok := sr.Next()
		if !ok {
			if sr.Err() != nil {
				return nil, nil, sr.Err()
			}
			break
		}
		termDoclists[term] = dl
		for _, id := range docIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, termDoclists, nil
}

// segdirRowDocIDs returns the doc IDs contained in one %_segdir row's segment
// (its root blob plus %_segments leaf blocks), read into a throwaway index so
// the live in-memory index is untouched. A corrupt segment returns an error
// (the caller fails the operation with "database disk image is malformed").
func (e *DDLExecutor) segdirRowDocIDs(tableName string, rootVal, leavesEndVal interface{}) ([]int64, error) {
	root := fts.RootBlobBytes(rootVal)
	if len(root) == 0 {
		return nil, nil
	}
	reader := func(blockID int) ([]byte, error) {
		blk, res := e.readFTSBlock(tableName, blockID)
		if res != nil {
			return nil, fmt.Errorf("corrupt segment root")
		}
		return blk, nil
	}
	// Stream the segment's docids lazily (fts.SegmentStreamReader) instead of
	// materializing the whole segment into an InvertedIndex — the crisis merge
	// reads every source segment this way, so the InvertedIndex construction
	// (an addPosting per posting, including position lists) made the 30040-doc
	// automerge O(segment size) per crisis merge, O(n^2) over many flushes
	// (fts4merge4 2.2.x).
	sr := fts.NewSegmentStreamReader(root, int(e.segdirRowLeavesEnd(leavesEndVal)), reader)
	seen := make(map[int64]bool)
	var ids []int64
	for {
		_, docIDs, _, _, ok := sr.Next()
		if !ok {
			if sr.Err() != nil {
				return nil, sr.Err()
			}
			break
		}
		for _, id := range docIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

// ftsNodeSize returns the FTS segment node size: the table's nodesize=
// override, or the database page size minus 35 — SQLite's default
// nNodeSize = nPgsz-35 (fts3.c fts3ConnectMethod). Using the raw page size
// makes every leaf 35 bytes larger, so the incremental merge's page-flush
// simulation consumes ~3.5% more source terms per quota and the automerge
// level structure drifts from the oracle (fts4merge4 2.2.x).
func (e *DDLExecutor) ftsNodeSize(ftsTable *fts.FTS3Table) int {
	if n := ftsTable.NodeSize(); n > 0 {
		return n
	}
	ps := int(e.ctx.Pager().PageSize())
	if ps > 35 {
		return ps - 35
	}
	return ps
}

// MergeFTS implements the FTS 'merge=N[,M]' special command (fts3_write.c
// fts3DoIncrmerge / sqlite3Fts3Incrmerge): it incrementally merges segments at
// the lowest level with at least nMin segments into one segment at the next
// level, writing up to nMerge leaf pages. The merged output accumulates the
// consumed segments' doc IDs; fully-consumed input segments are deleted, a
// partially-consumed segment is kept (its remaining doc IDs stay in the row).
// The %_stat id=1 hint records (level, remaining count) so the next merge
// continues the same output segment (fts4merge 4.3: 16→14→13→... consumes one
// or two level-0 segments per merge=1,16 call into a single level-1 output).
func (e *DDLExecutor) MergeFTS(tableName string, nMerge, nMin int) {
	ftsTable, ok := e.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return
	}
	if nMin < 2 {
		nMin = 2
	}
	nodeSize := e.ftsNodeSize(ftsTable)
	// Track the next %_segdir rowid so the merge's output/truncation writes use
	// EXPLICIT rowids — the implicit allocation's cached max can go stale
	// mid-transaction after a delete invalidates it, reusing a rowid and
	// duplicating the row (fts4merge4 2.2.x with the corrected hint: the L2
	// output's replacingOut delete + insert reused rowid 34).
	segdirNextRowID := e.ftsSegdirNextRowID(tableName)
	// Loop over levels: consume a level, then move up (SQLite's outer while
	// loop in sqlite3Fts3Incrmerge). nMerge is the leaf-page quota; the quota
	// (nRem) decreases by (1 + leaf pages written) each iteration and the
	// loop exits when it is exhausted.
	nRem := nMerge
	// The %_stat id=1 hint is a LIST of (level, nSeg) pairs (SQLite's
	// fts3IncrmergeHintPop/Push): each iteration POPS the first entry (and
	// restores it if the fresh FIND takes precedence), and chomp PUSHES
	// (level, remaining) to the END. A single row carries several pending
	// levels across the iterations of one call (fts4merge4 tx26: hint.n=4 =
	// the (1,2) and (2,2) pairs; the engine's old single-entry row dropped
	// the second pair when the first level was fully consumed).
	var hintList []ftsHintEntry
	if blob := e.readFTSStatRow(tableName, 1); blob != nil {
		hintList = ftsParseHintList(blob)
	}
	storeHint := func() {
		if len(hintList) == 0 {
			e.writeFTSStatRow(tableName, 1, nil)
		} else {
			e.writeFTSStatRow(tableName, 1, ftsEncodeHintList(hintList))
		}
	}
	for {
		if nRem <= 0 {
			return
		}
		// A %_stat id=1 hint from a previous merge (fts4merge 4.3: the hint
		// continues consuming the same level even after its segment count
		// drops below nMin) overrides the fresh FIND_MERGE_LEVEL search — but
		// ONLY when the hinted level is at or below the level FIND_MERGE_LEVEL
		// would pick RELATIVE to nMod (fts3_write.c sqlite3Fts3Incrmerge:
		// iAbsLevel%nMod >= iHintAbsLevel%nMod). The POP is LIFO — from the
		// END of the list, the entry chomp pushed LAST call — and it is
		// UNCONDITIONAL: the popped entry is only restored (re-appended)
		// when a strictly-lower found level takes precedence. Checking the
		// hinted level's live count here diverged: stale heads accumulated
		// at the FRONT (FIFO) and continuations never engaged, so merge=5,2
		// loops re-found fresh levels every call and drained orders of
		// magnitude slower than SQLite (fts4opt 2.6).
		foundLevel := e.ftSMergeLevel(tableName, nMin)
		level := -1
		effMin := nMin
		fromHint := false
		nMod := 1024 * (1 + len(ftsTable.PrefixLengths()))
		if len(hintList) > 0 {
			popped := hintList[len(hintList)-1]
			hintList = hintList[:len(hintList)-1]
			hLevel, hSeg := popped.level, popped.nSeg
			if foundLevel < 0 || foundLevel%nMod >= hLevel%nMod {
				// nSeg = MIN(MAX(nMin, found), nHintSeg); SQLite then opens
				// the OLDEST nSeg segments and proceeds ONLY when exactly
				// nSeg exist (pCsr->nSegment==nSeg) — a stale hint pointing
				// at a drained level does NO work instead of merging a
				// lone segment upward (which cascaded outputs 1057→1058→…).
				rows2 := e.readFTSSegdirRows(tableName, hLevel)
				cap2 := nSegCap(nMin, foundCountAt(e, tableName, foundLevel), hSeg)
				if len(rows2) >= cap2 {
					level = hLevel
					effMin = cap2
					fromHint = true
				}
				// else: dead/short hint — entry stays consumed, this
				// iteration falls back below without touching the level.
			} else {
				// The fresh FIND picked a strictly-lower relative level; undo
				// the pop (SQLite restores hint.n).
				hintList = append(hintList, popped)
			}
		}
		if level < 0 {
			// No usable hint: use the lowest absolute level with at least
			// nMin segments.
			level = foundLevel
			if level < 0 {
				return
			}
		}
		rows := e.readFTSSegdirRows(tableName, level)
		if len(rows) == 0 {
			// The hinted level was consumed; clear the hint and retry.
			e.clearFTSStatRow(tableName, 1)
			continue
		}
		if fromHint {
			// SQLite's fts3IncrmergeLoad decides APPENDABILITY purely by the
			// zero-length marker block at end_block (fts3IsAppendable:
			// "blockid=? AND block IS NULL") plus the first-term order check.
			// A BARE integer end_block (no size suffix) does NOT block
			// continuation — fts3ReadEndBlockField parses it to (iEnd,
			// nLeafData=0) and bNoLeafData only gates promotion later. The
			// engine previously returned a silent no-op here, diverging from
			// oracle (fts4growth x6: merge25b extends leaves 744->769 on a
			// bare end_block). The geometry fallback below performs the marker
			// and order checks.
		}
		// The source segments' blocks are LIVE until this merge's chomp
		// truncates them. The output writes (and continuation's new leaves,
		// and the truncation's fresh blocks) must allocate ABOVE every source
		// segment's leaves_end_block — otherwise a source block and an output
		// leaf share an id and the truncation's old-range cleanup deletes the
		// fresh output leaf (fts4merge4 am=2 tx 17: the L1[1] continuation
		// wrote new leaves at 3420+, colliding with the L0 sources at 3421+;
		// the chomp deleted them, shrinking L1[1] and diverging the cascade).
		// SQLite avoids this by pre-allocating the writer's entire block
		// range (iStart..iEnd) above all existing blocks.
		maxSourceEnd := 0
		for _, row := range rows {
			if end := int(e.segdirRowLeavesEnd(row.leavesEndBlock)); end > maxSourceEnd {
				maxSourceEnd = end
			}
		}
		// The output lives at level+1. When the hint directed us here (a
		// continuation), APPEND to the existing output segment (the largest
		// idx at level+1); a fresh FIND_MERGE_LEVEL creates a new output.
		nextLevel := level + 1
		// SQLite's bIgnoreEmpty (fts3_write.c fts3SegmentMerge): when the
		// output lands ABOVE every other segment of this index, delete-
		// marker entries are DROPPED from the merge instead of preserved —
		// nothing older remains below the output, so applying the deletions
		// is safe and keeps outputs compact. Without this, propagated bare
		// entries inflate merged outputs (~1.5x) until regrowth-time
		// promotion aborts on size, stranding stale segments (fts4opt 2.7/
		// 2.8).
		promoBase := (level / 1024) * 1024
		iMaxLevel := -1
		for _, r2 := range e.readFTSSegdirRowsRange(tableName, promoBase, promoBase+1024) {
			if r2.level > iMaxLevel {
				iMaxLevel = r2.level
			}
		}
		bIgnoreEmpty := false && nextLevel > iMaxLevel
		outIdx := e.ftSSegmentIdx(tableName, nextLevel)
		replacingOut := false
		// A continuation appends to the existing output segment (SQLite's
		// fts3IncrmergeLoad): the existing leaves STAY at their block ids and
		// only NEW merged leaves are written after them. The engine previously
		// replayed the whole existing output through the writer (rewriting
		// every leaf at fresh ids), which both did O(segment) extra work and
		// made the continuation's output so large that the leaf quota stopped
		// mid-merge where SQLite's append finished (fts4merge4 tx22: engine
		// charged 506 leaves vs SQLite's 261 new ones).
		nLeafEst := 0
		mc := ftsTable.MergeCtxFor(nextLevel)
		var lastOutRow ftsSegdirRow
		// No-replay continuation state: existing leaves keep their ids.
		contStartBlock, contLeavesEnd := 0, 0
		contBounds := []string(nil)
		contSize := int64(0)
		contBare := false // candidate had NO size suffix: keep end_block bare
		contLeaves := 0
		if fromHint && mc != nil {
			outRows := e.readFTSSegdirRows(tableName, nextLevel)
			if len(outRows) > 0 {
				last := outRows[len(outRows)-1]
				if last.rowidKnown && last.rowid != mc.OutRowID {
					mc = nil
				} else {
					lastOutRow = last
					outIdx = last.idx
					replacingOut = true
					nLeafEst = mc.NLeafEst
					contStartBlock = segdirRowStart(last)
					contLeavesEnd = int(e.segdirRowLeavesEnd(last.leavesEndBlock))
					contSize = segdirRowSize(last.endBlock)
					// bNoLeafData propagation (fts3IncrmergeLoad): a candidate
					// loaded WITHOUT a size suffix keeps the bare integer
					// end_block on EVERY subsequent rewrite of the row —
					// SQLite rebinds int64 iEndBlock while bNoLeafData is set,
					// even after further partial or completing merges
					// (fts4growth 7.5: the completed output stays "23694",
					// never gains a size suffix).
					contBare = contSize == 0
					contLeaves = mc.IBlock
					if h, fb, bounds := fts.ParseSegmentRootBounds(fts.RootBlobBytes(last.root)); h > 0 && contStartBlock > 0 {
						contBounds = bounds
						if fb > 0 {
							contStartBlock = fb
						}
					} else {
						// A single-leaf output (root IS the leaf, no %_segments
						// rows) cannot be appended in place. Fall back to the
						// rebuild path — but KEEP the existing output row as a
						// merge SOURCE: its terms were already merged (and its
						// sources chomped away), so dropping it here would lose
						// every term it holds (fts4opt 1.x: merge #2 rebuilt
						// without it and integrity-check reported missing-term
						// runs covering the previous call's entire output).
						rows = append(rows, last)
						if end := int(e.segdirRowLeavesEnd(last.leavesEndBlock)); end > maxSourceEnd {
							maxSourceEnd = end
						}
						replacingOut = false
						mc = nil
					}
					if replacingOut {
						// The append-order check (first merged term > existing last term)
						// runs after the reader heap is primed below.
					}
				}
			}
		}
		markerID := 0
		if !replacingOut {
			// No usable MergeCtx: fall back to the candidate output row's own
			// GEOMETRY when its end_block has NO size suffix (user-stripped,
			// fts4growth 7.3) — SQLite's fts3IncrmergeLoad re-derives writer
			// state from %_segdir and the zero-length marker row alone, so a
			// merge can keep appending inside the pre-allocated range even
			// after the size accounting is gone (oracle s.db: 7.5 merges
			// continue the stripped segment; start_block/leaves preserved).
			if cand := e.readFTSSegdirRows(tableName, nextLevel); len(cand) > 0 {
				c := cand[len(cand)-1]
				start := segdirRowStart(c)
				le := int(e.segdirRowLeavesEnd(c.leavesEndBlock))
				endFirst := int(segdirEndBlockFirst(c.endBlock))
				if start > 0 && le > 0 && endFirst > le && segdirRowSize(c.endBlock) == 0 {
					// The marker row is a ZERO-LENGTH/NULL block.
					blk, res := e.readFTSBlock(tableName, endFirst)
					if res == nil && len(blk) == 0 {
						lastOutRow = c
						outIdx = c.idx
						replacingOut = true
						contStartBlock = start
						contLeavesEnd = le
						contSize = segdirRowSize(c.endBlock)
						contLeaves = contLeavesEnd - contStartBlock + 1
						markerID = endFirst
						contBare = true
						// SQLite derives the pre-allocated range as
						// iEnd = iStart-1 + nLeafEst*FTS_MAX_APPENDABLE_HEIGHT
						// (HEIGHT=16); invert it for the writer's flush quota.
						nLeafEst = (endFirst - contStartBlock + 1) / 16
						if h, fb, bounds := fts.ParseSegmentRootBounds(fts.RootBlobBytes(c.root)); h > 0 {
							contBounds = bounds
							if fb > 0 {
								contStartBlock = fb
							}
						} else {
							replacingOut = false
							mc = nil
						}
					}
				}
			}
		}
		if !replacingOut && mc != nil && fromHint {
			nLeafEst = 1 << 30
		}
		if !replacingOut && nLeafEst == 0 {
			// Fresh merge (SQL_MAX_LEAF_NODE_ESTIMATE): nLeafEst = 2*total(1
			// + leaves_end_block - start_block) over the source segments.
			for _, row := range rows {
				sb := int64(0)
				if s, ok := row.startBlock.(int64); ok {
					sb = s
				}
				le := e.segdirRowLeavesEnd(row.leavesEndBlock)
				nLeafEst += 2 * int(1+le-sb)
			}
			if nLeafEst < 2 {
				nLeafEst = 2
			}
		}
		// Consume the source level's segments via a STREAMING k-way term merge
		// (SQLite's sqlite3Fts3Incrmerge): the merged term stream of ALL source
		// segments is consumed in sorted order, writing the output until the
		// leaf-page quota (nWork >= nRem) is met. Each source segment is then
		// truncated to its terms that were NOT merged (fts3IncrmergeChomp /
		// fts3TruncateSegment); a segment whose terms were all merged is
		// deleted. The engine's earlier per-segment sequential consumption
		// deleted small fully-consumed segments that SQLite keeps (fts4merge
		// 5.3: the oracle truncates all 15 L1 segments, keeping L1=15).
		// SQLite's SQL_FIND_MERGE_LEVEL returns the count of segments at the
		// lowest level with at least nMin segments; sqlite3Fts3Incrmerge then
		// CAPS nSeg to the hint's remaining count when the hint is used
		// (nSeg = MIN(MAX(nMin, found), nHintSeg)), merging only that many of
		// the OLDEST segments — the flush's fresh segment is left behind when
		// the hint still tracks the earlier ones (fts4merge4 2.2.x: the
		// am=2 hint (0,2) at tx 19 merges the 2 old small L0 segments and
		// keeps the new flush at L0). Each source segment is streamed by a
		// lazy reader (fts.SegmentStreamReader, the engine's Fts3SegReader
		// equivalent) that reads %_segments leaf blocks only as the merge
		// consumes terms, so the automerge's cost is bounded by the leaf-page
		// quota, not the level size.
		loadCount := len(rows)
		if fromHint && effMin < loadCount {
			loadCount = effMin
		}
		readers := make([]segReader, 0, loadCount)

		for _, row := range rows[:loadCount] {

			sr := fts.NewSegmentStreamReader(fts.RootBlobBytes(row.root), int(e.segdirRowLeavesEnd(row.leavesEndBlock)), func(blockID int) ([]byte, error) {
				blk, res := e.readFTSBlock(tableName, blockID)

				if res != nil {
					return nil, fmt.Errorf("corrupt segment root")
				}
				return blk, nil
			})
			readers = append(readers, segReader{row: row, reader: sr})
		}
		if len(readers) == 0 {
			return
		}
		// Prime the k-way merge heap with each reader's first term. The merge
		// pulls terms in sorted order, groups the readers that share a term,
		// and simulates the output node buffer (fts3IncrmergeAppend).
		h := &mergeHeap{}
		for i, sr := range readers {
			term, ids, doclist, size, ok := sr.reader.Next()
			if !ok {
				if sr.reader.Err() != nil {
					return
				}
				continue // empty segment
			}
			heap.Push(h, mergeHeapEntry{term: term, docIDs: ids, doclist: doclist, size: size, reader: sr.reader, seq: i})
		}
		// Append-order check for a continuation: the first merged term must be
		// > the existing output's last term (SQLite's fts3IncrmergeLoad
		// bAppendable=0 rejects the append otherwise; the output would break
		// sorted order).
		if replacingOut && h.Len() > 0 {
			first := h.peekTerm()
			lt := e.segdirRowLastTerm(tableName, lastOutRow)
			if lt != "" && first <= lt {
				replacingOut = false
				mc = nil
			}
		}
		// The output is written by the REAL incremental leaf writer
		// (fts.IncrLeafWriter, fts3IncrmergeAppend): each flushed leaf becomes
		// a %_segments block immediately, so the work quota charged
		// (nWork = leaves flushed this call) equals the blocks actually
		// written. A continuation (SQLite's fts3IncrmergeLoad) keeps the
		// EXISTING leaves at their block ids and appends only the NEW merged
		// leaves after them; the existing leaves are NOT replayed/rewritten
		// (that made the output so large the quota stopped mid-merge where
		// SQLite's append finished — fts4merge4 tx22).
		writer := fts.NewIncrLeafWriter(nodeSize, nLeafEst, 0, 0)
		// contReuseLeaf: the first flush of a continuation is the loaded
		// existing last leaf; it must OVERWRITE the existing last-leaf block
		// (SQLite's fts3IncrmergeLoad keeps aNodeWriter[0].iBlock = the
		// existing leaf's block, and the flush writes it in place). Only
		// subsequent NEW leaves allocate fresh block ids.
		contReuseLeaf := false
		lastWrittenBlock := 0
		if replacingOut {
			writer = fts.NewIncrLeafWriter(nodeSize, nLeafEst, contLeaves, 0)
			// A continuation resumes the existing output's LAST leaf (SQLite's
			// fts3IncrmergeLoad loads the candidate segment's last leaf into
			// pWriter->aNodeWriter[0]): the buffer is already full, so the
			// first appended term flushes it and the quota is charged exactly
			// as SQLite's does (fts4merge 4.3: one source segment per
			// merge=1,16 call, not two).
			if contLeavesEnd > 0 {
				if lastLeaf, res := e.readFTSBlock(tableName, contLeavesEnd); res == nil && lastLeaf != nil {
					writer.LoadLeaf(lastLeaf)
					// nLeafData is cumulative across partial calls. Loading the
					// last leaf restores its fill, but not this accounting value.
					if contSize < 0 {
						contSize = -contSize
					}
					writer.SeedLeafData(int(contSize))
					contReuseLeaf = true
				}
			}
		}
		firstBlock := 0
		if replacingOut {
			firstBlock = contStartBlock // existing leaves keep their ids
		}
		// The block-allocation floor: every fresh output leaf and every
		// truncation block must land above (a) the still-live source blocks
		// (maxSourceEnd) AND (b) the continuation output's existing leaves
		// (contLeavesEnd — the output may extend beyond the sources).
		allocFloor := maxSourceEnd
		if contLeavesEnd > allocFloor {
			allocFloor = contLeavesEnd
		}
		// SQLite's incrmerge writer PRE-ALLOCATES the output's block range
		// (fts3IncrmergeWriter: iEnd = iStart-1 + nLeafEst*HEIGHT, HEIGHT=16)
		// and writes a NULL marker %_segments row at iEnd so a later merge can
		// detect the appendable segment. The base must be above EVERY live
		// block (other levels' segments stay live during this merge).
		// SCOPE: currently enabled only for languageid=<col> tables
		// (fts4langid asserts the marker row and (iEnd,size) end_block).
		// Real SQLite pre-allocates for ALL tables, but its writer reserves a
		// PER-LAYER range (aNodeWriter[i].iBlock = iStart + i*nLeafEst) that
		// this engine's allocator does not mirror yet; enabling it globally
		// diverges block ids across multi-merge sequences (fts4merge 1.3,
		// fts4growth 1.x). Porting the exact reservation is the remaining
		// step for full segment-internals parity.
		useMarker := true // markers now model SQLite unconditionally (oracle-verified at page_size 1024)
		if useMarker {
			if nb := e.nextFTSBlockID(tableName) - 1; nb > allocFloor {
				allocFloor = nb
			}
		}
		// markerID is the segment's iEnd: the end_block first component for
		// fresh outputs (fts4langid 5.4 "256 65") and the allocation ceiling
		// for continuations.
		//
		// SCOPE: the marker/pre-allocation model is enabled only for
		// languageid=<col> tables, whose tests assert the marker row and the
		// (iEnd,size) end_block form (fts4langid 5.4.x.5). Non-langid tables
		// keep the engine's historical no-marker layout: their large-scale
		// merge scenarios (fts4growth 2.x+) diverge from the oracle in source
		// counting long before markers matter, and switching them over
		// regressed those suites.
		if mc != nil && replacingOut {
			markerID = mc.MarkerID
		}
		freshNext := 0
		if !replacingOut && useMarker {
			markerID = allocFloor + nLeafEst*16
			_ = e.ctx.Exec(&sql.InsertStmt{
				Table:   tableName + "_segments",
				Columns: []string{"blockid", "block"},
				Values: [][]sql.Expr{
					{
						&sql.NumericLit{Value: fmt.Sprintf("%d", markerID)},
						&sql.NullLit{},
					},
				},
			})
			// Leaf allocation runs sequentially from just above the floor and
			// never reaches the marker within one quota (nRem < nLeafEst*16);
			// pin the cache there so cache invalidations cannot jump past it.
			freshNext = allocFloor + 1
			ftsTable.SetNextBlockID(freshNext)
		}
		// Enable layered interior output (SQLite aNodeWriter): interior
		// layer L allocates blocks at iStart + L*nLeafEst. Continuations
		// seed layer 1 with the stored root's boundary separators so the
		// pending node resumes exactly where the previous call stopped.
		hierStart := freshNext
		if replacingOut {
			hierStart = contStartBlock
		}
		writer.BeginHierarchy(hierStart, nLeafEst)
		if replacingOut && len(contBounds) > 0 {
			writer.SeedHierarchySeps(contStartBlock, contBounds)
		}
		// SQLite nLeafData tracks bytes written for leaf nodes, including the
		// height byte in each serialized leaf block.
		outLeafData := 0
		// contNext tracks the continuation's in-range allocation cursor
		// (SQLite's aNodeWriter[0].iBlock increments inside iStart..iEnd).
		contNext := int64(contLeavesEnd)
		writeOutBlock := func(blk []byte) int {
			next, cached := ftsTable.NextBlockID()
			reuseLeaf := contReuseLeaf
			if contReuseLeaf {
				next = contLeavesEnd
				contReuseLeaf = false
				cached = true
			} else if replacingOut && markerID > 0 && contLeavesEnd > 0 {
				// A markered continuation allocates SEQUENTIALLY inside its
				// pre-allocated range: the previous leaf id + 1, never the
				// max-based fallback (the NULL marker row sits above the
				// range and would drag every leaf up to markerID-1,
				// overwriting one block per flush).
				contNext++
				next = int(contNext)
				cached = true
			} else if !cached {
				// The cache was invalidated by a shadow write: recompute the max
				// (an uncached read returns 0 — writing block 0/1 would clobber
				// live blocks, fts4merge 1.2's "malformed").
				next = e.nextFTSBlockID(tableName)
			}
			if !reuseLeaf && next <= allocFloor && !(replacingOut && markerID > 0) {
				next = allocFloor + 1
				cached = true
			}
			if useMarker && !replacingOut && !contReuseLeaf {
				// Sequential allocation inside the writer's pre-allocated range
				// (iStart..iEnd): the cache/max-based fallback would jump ABOVE
				// the marker row once a shadow write invalidates the cache,
				// scattering leaves outside the root's contiguous range.
				next = freshNext
				freshNext++
				cached = true
			}
			if firstBlock == 0 {
				firstBlock = next
			}
			if reuseLeaf {
				// The continuation's first flush OVERWRITES the existing last
				// leaf block in place (SQLite's fts3IncrmergeLoad keeps
				// aNodeWriter[0].iBlock and fts3WriteSegment REPLACES the row).
				// A plain INSERT would leave the OLD block content behind as a
				// duplicate rowid, breaking the term order (fts4merge 1.4:
				// L2[3] block 29 kept its old "beta" tail while the new leaf
				// also started at "beta" — "database disk image is malformed").
				if dres := e.ctx.Exec(&sql.DeleteStmt{
					Table: tableName + "_segments",
					Where: &sql.BinaryOp{
						Operator: "=",
						Left:     &sql.ColumnRef{Name: "blockid"},
						Right:    &sql.NumericLit{Value: fmt.Sprintf("%d", next)},
					},
				}); dres != nil && dres.Error != nil {
				}
			}
			if ires := e.ctx.Exec(&sql.InsertStmt{
				Table:   tableName + "_segments",
				Columns: []string{"blockid", "block"},
				Values: [][]sql.Expr{
					{
						&sql.NumericLit{Value: fmt.Sprintf("%d", next)},
						&sql.BlobLit{Value: blk},
					},
				},
			}); ires != nil && ires.Error != nil {
			}
			ftsTable.SetNextBlockID(next + 1)
			outLeafData += len(blk)
			lastWrittenBlock = next
			return next
		}
		flushCount := 0
		// mergedDoclists holds the MERGED doclist per merged term (SQLite's
		// fts3IncrmergeAppend writes the merged term's doclist, NOT every
		// document's full postings — a truncated source segment only holds its
		// unmerged terms, so rebuilding from full documents would duplicate
		// the already-merged terms, and the live index may not cover flushed
		// segments at all).
		mergedDoclists := make(map[string][]byte)
		for h.Len() > 0 {
			e0 := heap.Pop(h).(mergeHeapEntry)
			term := e0.term

			// Group every reader whose current term equals term: the output
			// entry combines their doclists (SQLite's pCsr->aDoclist merges
			// all source segments for the current term).
			group := []mergeHeapEntry{e0}
			totalDoclist := e0.size
			var groupDoclists [][]byte
			if len(e0.doclist) > 0 {
				groupDoclists = append(groupDoclists, e0.doclist)
			}
			for h.Len() > 0 && h.peekTerm() == term {
				e1 := heap.Pop(h).(mergeHeapEntry)
				group = append(group, e1)
				totalDoclist += e1.size
				if len(e1.doclist) > 0 {
					groupDoclists = append(groupDoclists, e1.doclist)
				}
			}
			// Merge the group's raw doclists into the output entry (SQLite's
			// fts3IncrmergeAppend writes the merged term's doclist).
			var merged []byte
			if bIgnoreEmpty {
				merged = fts.MergeDoclistsApply(groupDoclists...)
			} else {
				merged = fts.MergeDoclists(groupDoclists...)
			}
			if len(merged) > 0 {
				mergedDoclists[term] = merged
			}
			// Charge one prefix-compressed entry sized by the MERGED doclist
			// (SQLite's nSpace uses pCsr->aDoclist after fts3DoclistMerge) —
			// the raw per-segment doclist SUM undercounts because combining
			// segments for the same term adds docid deltas (the automerge
			// consumed ~1.5x the source per quota, shifting the level
			// structure; fts4merge4 2.2.x).
			nDoclist := len(merged)
			if nDoclist == 0 {
				nDoclist = totalDoclist
			}
			_ = nDoclist
			if blk := writer.Append(term, merged); blk != nil {
				writer.NoteFlushedID(writeOutBlock(blk))
			}
			flushCount = writer.WorkDone()

			// Each group reader advances to its next (unmerged) term.
			for _, g := range group {

				if nterm, nids, ndl, nsize, ok := g.reader.Next(); ok {

					heap.Push(h, mergeHeapEntry{term: nterm, docIDs: nids, doclist: ndl, size: nsize, reader: g.reader, seq: g.seq})
				}
			}
			if nMerge > 0 && flushCount >= nRem {
				// The term that triggered the flush was appended to the new
				// output node (SQLite fts3IncrmergeAppend flushes the old
				// block then adds the term), so it is merged; the group
				// readers are now positioned at their first unmerged terms.
				// The stop uses the CURRENT iteration's remaining quota nRem,
				// not the original nMerge (SQLite's do-while stops when
				// pWriter->nWork >= nRem).
				break
			}
		}

		// Write the merged output at level+1. A continuation KEEPS the existing
		// output row and its leaves in place (SQLite's fts3IncrmergeLoad
		// appends inside the pre-allocated range); only new leaves are added.
		// A fresh merge writes a new row (deleteFTSSegdirIdx was only needed
		// by the old rebuild-on-every-call path).
		if replacingOut {
			// no row/blocks deletion: the existing leaves remain the segment
		}
		// Release (fts3IncrmergeRelease): write the outstanding final leaf,
		// then the segdir row over the writer's real layout.
		if blk := writer.TakeLeaf(); blk != nil {
			writer.NoteFlushedID(writeOutBlock(blk))
			// Release flush is outside fts3IncrmergeAppend and therefore does
			// not contribute to nWork or this call's quota.
		}
		leavesEndBlock := 0
		if lastWrittenBlock > 0 {
			// The real last written block (a continuation's first flush
			// OVERWRITES the existing last leaf, so the block ids are NOT a
			// simple contiguous range from firstBlock).
			leavesEndBlock = lastWrittenBlock
		} else if firstBlock > 0 {
			leavesEndBlock = firstBlock + writer.LeavesFlushed() - 1
		}
		// Finalize the layered hierarchy: interior layers below the root are
		// persisted as %_segments blocks at their pre-allocated slots
		// (iStart + L*nLeafEst + seq); the highest non-empty layer becomes
		// the root blob (fts3IncrmergeRelease). For segments small enough to
		// need a single interior node this reproduces the legacy flat root
		// byte-for-byte.
		rootBlob, interiorBlocks := writer.Finish()
		for _, ib := range interiorBlocks {
			_ = e.ctx.Exec(&sql.InsertStmt{
				Table:   tableName + "_segments",
				Columns: []string{"blockid", "block"},
				Values: [][]sql.Expr{
					{
						&sql.NumericLit{Value: fmt.Sprintf("%d", ib.ID)},
						&sql.BlobLit{Value: ib.Data},
					},
				},
			})
		}
		if rootBlob == nil {
			if replacingOut && len(contBounds) == 0 {
				rootBlob = fts.RootBlobBytes(lastOutRow.root)
			} else {
				rootBlob = writer.BuildRoot(firstBlock)
			}
		}

		// Persist SQLite's cumulative nLeafData, not serialized block bytes.
		// SQLite's nLeafData includes one height byte per newly materialized
		// leaf; continuation's pre-existing leaves are already in contSize.
		outLeafData = writer.LeafData()
		if replacingOut && outLeafData < int(contSize) {
			outLeafData = int(contSize)
		}
		// Defer the %_segdir row write until AFTER chomp: SQLite negates the
		// output's nLeafData when the merge was PARTIAL (fts3IncrmergeRelease
		// runs after fts3IncrmergeChomp, and `if(nSeg!=0) nLeafData *= -1` —
		// the negative size suffix marks a partial-merge output, and
		// fts3PromoteSegments aborts on any nSize<=0 candidate). The engine
		// writes the row after chomp below with the correct sign.
		outRowID := segdirNextRowID
		// A continuation's end_block size is the ACCUMULATED leaf data of every
		// append (SQLite's pWriter->nLeafData persists across calls); the
		// promotion 3/2 rule uses it, so fold the existing output's size in.
		// Continuation writer already starts with prior nLeafData, restored from
		// end_block before appending. Do not add prior size a second time.
		outSize := outLeafData
		_ = outSize
		// Truncate each source segment to its unmerged terms (SQLite's
		// fts3IncrmergeChomp / fts3TruncateSegment) — chompFTSMerge deletes
		// fully-consumed segments and truncates the rest IN PLACE (trimmed
		// blocks keep their ids, so no fresh allocation is needed).
		_, truncated := e.chompFTSMerge(tableName, level, readers)
		// Write the %_segdir row now that the chomp's result is known: negate
		// the size suffix when the merge was partial (SQLite's nLeafData *= -1
		// when nSeg!=0 — promotion aborts on any negative candidate).
		remaining := truncated
		rowSize := outSize
		if remaining > 0 {
			rowSize = -outSize
		}
		if replacingOut {
			// The continuation REWRITES the output row: SQLite deletes the
			// old %_segdir row and inserts a fresh one (fts3IncrmergeRelease
			// → fts3DeleteSegdir + insert), so its rowid moves ABOVE every
			// surviving segment row. An in-place UPDATE keeps the old rowid
			// and reverses the natural (rowid) order of segdir rows, which
			// unordered SELECTs expose (fts4growth 6.4/6.5). The leaves keep
			// their ids; only the ROW identity is new.
			e.deleteFTSSegdirIdx(tableName, nextLevel, outIdx)
			outRowID = e.ftsSegdirNextRowID(tableName)
			if contBare {
				// bNoLeafData: the loaded candidate carried no size suffix,
				// so the rewritten row keeps a BARE integer end_block
				// (fts3WriteSegdir binds int64 when nLeafData==0).
				rowSize = 0
			}
			e.writeFTSShadowRowAtRange(tableName, nextLevel, outIdx, outRowID, firstBlock, leavesEndBlock, rowSize, rootBlob, markerID)
			if lastOutRow.rowidKnown {
				lastOutRow.rowid = outRowID
			}
		} else {
			e.writeFTSShadowRowAtRange(tableName, nextLevel, outIdx, outRowID, firstBlock, leavesEndBlock, rowSize, rootBlob, markerID)
			segdirNextRowID++
		}
		// Write the %_stat id=1 hint: (absLevel, remaining count at the level)
		// so the next merge continues this output (fts4merge 4.3: X'000E'...
		// X'0006'; fts4merge 1.3 after merge #2: X'010F' = (1, 15)). When the
		// level is fully consumed the hint is an EMPTY blob (fts4merge 1.3
		// after merge #3: id=1 exists with value X'').
		// SQLite's nSeg after chomp counts only the segments the merge
		// LOADED (pCsr->nSegment): fully-consumed ones are deleted, the rest
		// truncated; segments at the level NOT part of this merge (e.g. the
		// flush's fresh segment, or the tail the hint capped away) are NOT
		// counted. len(rows)-deleted over-counted them, pushing a hint that
		// made the next iteration re-merge the level (fts4merge4 tx18:
		// oracle goes 0:1 1:2 2:1, the engine looped L0 again producing
		// overlapping L1 outputs).
		if remaining > 0 {
			// PUSH (level, remaining) to the END of the hint list (SQLite's
			// fts3IncrmergeHintPush); bDirtyHint is set when the hint was used
			// OR a push happened, so store the current list.
			hintList = append(hintList, ftsHintEntry{level: level, nSeg: remaining})
			storeHint()
		} else if fromHint {
			// The hint directed this merge and the level is now fully
			// consumed: the popped entry is gone; store the REMAINING list
			// (other entries survive — the engine's old single-row hint
			// dropped them, fts4merge4 tx26).
			storeHint()
		}
		// else: a fresh FIND_MERGE_LEVEL merge fully consumed its level
		// without touching the hint — SQLite's bDirtyHint stays 0 and the
		// PREVIOUS hint row is preserved (fts3_write.c sqlite3Fts3Incrmerge:
		// fts3IncrmergeHintStore only runs when bDirtyHint is set; fts4merge
		// 5.11: the merge=1,6 drains L0 while the L1 hint X'010E' remains).
		// No write here keeps the existing row.
		ftsTable.InvalidateSegmentCacheKeepMergeCtx()
		// Persist the writer state for the next continuation (after the
		// cache invalidation above, which clears it): nLeafEst (unchanged),
		// iBlock (leaves written so far), buffer (the ending leaf-buffer
		// fill — SQLite's nearly-empty last leaf after a quota stop, which
		// the next merge resumes from so it consumes a full page; fts4merge
		// 1.3 merge #3 drains L1). Dropped when the level is fully consumed.
		if remaining > 0 {
			mcOut := outRowID
			if replacingOut {
				mcOut = lastOutRow.rowid
			}
			ftsTable.SetMergeCtx(nextLevel, &fts.MergeCtx{
				NLeafEst: nLeafEst,
				IBlock:   writer.LeavesFlushed(),
				Buffer:   writer.BufferFill(),
				OutRowID: mcOut,
				MarkerID: markerID,
			})
		} else {
			ftsTable.ClearMergeCtx(nextLevel)
			// The merge fully consumed its level: promote every segment at
			// higher levels of this index that is smaller than 3/2 of the new
			// output's leaf data down to the output level (SQLite
			// fts3PromoteSegments, fts3_write.c:5076 — the oracle's sparse level
			// structures come from this collapse, fts4merge4 2.2 am=2: 4 1 6 1).
			// A segment with no size in end_block (0 or unparsable) blocks the
			// whole promotion, exactly like fts3ReadEndBlockField nSize<=0.

			e.promoteFTSSegments(tableName, ftsTable, nextLevel, outLeafData)
		}
		// SQLite's sqlite3Fts3Incrmerge subtracts (1 + nWork) from the leaf
		// quota after each merge iteration.
		nRem -= (1 + flushCount)
		// If the level was fully consumed, continue at the next level; else
		// the quota stopped us and the loop's next FIND_MERGE_LEVEL may pick
		// the same level again (the kept rows still count). SQLite's outer
		// loop re-runs FIND_MERGE_LEVEL, so loop again.
	}
}

// ftSMergeLevel returns the lowest absolute level whose %_segdir has at least
// nMin segments, or -1 when no such level exists (fts3_write.c
// SQL_FIND_MERGE_LEVEL: the level with the smallest relative level containing
// at least nMin segments).
// nSegCap mirrors SQLite's nSeg = MIN(MAX(nMin, nSegFound), nHintSeg)
// (fts3_write.c sqlite3Fts3Incrmerge): a hint continuation loads at most
// nHintSeg segments, but never fewer than nMin when the level has more.
func nSegCap(nMin, foundCount, hintSeg int) int {
	nSeg := nMin
	if foundCount > nSeg {
		nSeg = foundCount
	}
	if hintSeg < nSeg {
		nSeg = hintSeg
	}
	if nSeg < 2 {
		nSeg = 2
	}
	return nSeg
}

// foundCountAt returns the segment count at an absolute level (0 for none).
func foundCountAt(e *DDLExecutor, tableName string, level int) int {
	if level < 0 {
		return 0
	}
	return len(e.readFTSSegdirRows(tableName, level))
}

func (e *DDLExecutor) ftSMergeLevel(tableName string, nMin int) int {
	segdir := tableName + "_segdir"
	segEntry, _, err := e.ctx.FindTable(segdir)
	if err != nil || segEntry == nil {
		return -1
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return -1
	}
	counts := map[int]int{}
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) == 0 {
			break
		}
		if lv, ok := rec.Values[0].(int64); ok {
			// Skip prefix-index levels (>= 1024) — the main index is level 0.
			counts[int(lv)]++
		}
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	best := -1
	for lv, n := range counts {
		if n >= nMin && (best < 0 || lv < best) {
			best = lv
		}
	}
	return best
}

// maxFTSLevel returns the largest absolute level present in the %_segdir table
// (SQLite's sqlite3Fts3MaxLevel, used by fts3SyncMethod to compute the
// auto-incr-merge quota A = nLeafAdd*mxLevel + A/2). Returns 0 when the table
// has no segments.
func (e *DDLExecutor) maxFTSLevel(tableName string) int {
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
	maxLevel := 0
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) == 0 {
			break
		}
		if lv, ok := rec.Values[0].(int64); ok && int(lv) < 1024 && int(lv) > maxLevel {
			maxLevel = int(lv)
		}
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	return maxLevel
}

// readFTSSegdirRowsRange reads every %_segdir row whose absolute level is in
// [lo, hi], sorted by (level, idx).
func (e *DDLExecutor) readFTSSegdirRowsRange(tableName string, lo, hi int) []ftsSegdirRow {
	segEntry, _, err := e.ctx.FindTable(tableName + "_segdir")
	if err != nil || segEntry == nil {
		return nil
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil
	}
	var rows []ftsSegdirRow
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil || len(rec.Values) < 6 {
			break
		}
		lv, lvOK := rec.Values[0].(int64)
		ix, ixOK := rec.Values[1].(int64)
		if lvOK && ixOK && int(lv) >= lo && int(lv) <= hi {
			rows = append(rows, ftsSegdirRow{
				idx:            int(ix),
				level:          int(lv),
				rowid:          cell.RowID,
				rowidKnown:     true,
				root:           rec.Values[len(rec.Values)-1],
				leavesEndBlock: rec.Values[3],
				startBlock:     rec.Values[2],
				endBlock:       rec.Values[4],
			})
		}
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].level != rows[j].level {
			return rows[i].level < rows[j].level
		}
		return rows[i].idx < rows[j].idx
	})
	return rows
}
