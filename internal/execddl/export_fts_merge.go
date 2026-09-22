// Package execddl: FTS incremental-merge machinery (MergeFTS, %_segdir
// readers, level/idx utilities, merge hint + heap). Split from export.go;
// behavior unchanged.
package execddl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
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

// segdirCursor opens a cursor over a table's %_segdir shadow btree; ok is
// false when the shadow table is absent or unreadable.
func (e *DDLExecutor) segdirCursor(tableName string) (*btree.Cursor, bool) {
	return e.shadowTableCursor(tableName, "_segdir")
}

// nextSegdirScanRecord reads and decodes the cursor's next %_segdir record
// for a plain (non-validating) scan: ok is false at end/error or when the
// record has fewer than minValues columns. Unlike the integrity walk's
// nextSegdirRecord, a mid-scan read error is a plain stop, not a corruption
// report (the pre-existing merge scanners' tolerance).
func nextSegdirScanRecord(cursor *btree.Cursor, minValues int) (*storage.Cell, *storage.Record, bool) {
	return nextShadowRecord(cursor, minValues)
}

// segdirRowFromRecord builds an ftsSegdirRow from a decoded %_segdir record
// (level, idx, start_block, leaves_end_block, end_block, ..., root).
func segdirRowFromRecord(cell *storage.Cell, rec *storage.Record) ftsSegdirRow {
	lv, _ := rec.Values[0].(int64)
	ix, _ := rec.Values[1].(int64)
	return ftsSegdirRow{
		idx:            int(ix),
		level:          int(lv),
		rowid:          cell.RowID,
		rowidKnown:     true,
		root:           rec.Values[len(rec.Values)-1],
		leavesEndBlock: rec.Values[3],
		startBlock:     rec.Values[2],
		endBlock:       rec.Values[4],
	}
}

// scanSegdirRows walks a %_segdir cursor and collects the rows whose absolute
// level passes match, in btree (rowid) order.
func scanSegdirRows(cursor *btree.Cursor, match func(level int) bool) []ftsSegdirRow {
	var rows []ftsSegdirRow
	for {
		cell, rec, ok := nextSegdirScanRecord(cursor, 6)
		if !ok {
			break
		}
		lv, lvOK := rec.Values[0].(int64)
		_, ixOK := rec.Values[1].(int64)
		if lvOK && ixOK && match(int(lv)) {
			rows = append(rows, segdirRowFromRecord(cell, rec))
		}
		if !advanceSequenceCursor(cursor) {
			break
		}
	}
	return rows
}

// readFTSSegdirRows reads every %_segdir row of one absolute level, sorted by
// idx. Used by the crisis merge and incremental merge to read the source
// segments' contents and to renumber surviving rows.
func (e *DDLExecutor) readFTSSegdirRows(tableName string, level int) []ftsSegdirRow {
	cursor, ok := e.segdirCursor(tableName)
	if !ok {
		return nil
	}
	rows := scanSegdirRows(cursor, func(lv int) bool { return lv == level })
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

// syncSegdirRowID raises the merge's explicit-rowid cursor above a row
// allocated by a fresh scan. The cont-rewrite branch rewrites the output row
// with EXPLICIT rowid outRowID; a later fresh-branch write in the SAME
// MergeFTS call must not reuse the scanned rowid — an explicit-rowid INSERT
// is a btree put, so a colliding rowid silently REPLACES the live segment
// row. fts4merge4 2.2.x: the L2 output row destroyed the truncated L1 idx1
// row, so the next merge loaded a full segment instead of the truncated
// remainder and the appendability check rejected the grind continuation.
func syncSegdirRowID(cursor, allocated int64) int64 {
	if allocated >= cursor {
		return allocated + 1
	}
	return cursor
}

// MergeFTS implements the FTS 'merge=N[,M]' special command (fts3_write.c
// fts3DoIncrmerge / sqlite3Fts3Incrmerge): it incrementally merges segments at
// the lowest level with at least nMin segments into one segment at the next
// level, writing up to nMerge leaf pages. The merged output accumulates the
// consumed segments' doc IDs; fully-consumed input segments are deleted, a
func nSegCap(nMin, foundCount, hintSeg int) int {
	// SQLite: nSeg = MIN(MAX(nMin, found), nHintSeg) — no lower bound. The
	// FIND_MERGE_LEVEL path's HAVING cnt>=MAX(2,nMin) guarantees >=2 there;
	// a hint entry may legitimately cap the merge to a single segment
	// (nHintSeg=1), which must not be re-raised (fts3corrupt 6.10: the
	// hint x'cf0f01' = (level 1999, nSeg 1) engages the merge).
	nSeg := nMin
	if foundCount > nSeg {
		nSeg = foundCount
	}
	if hintSeg < nSeg {
		nSeg = hintSeg
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
	cursor, ok := e.segdirCursor(tableName)
	if !ok {
		return -1
	}
	counts := map[int]int{}
	for {
		_, rec, ok := nextSegdirScanRecord(cursor, 1)
		if !ok {
			break
		}
		if lv, ok := rec.Values[0].(int64); ok {
			// Skip prefix-index levels (>= 1024) — the main index is level 0.
			counts[int(lv)]++
		}
		if !advanceSequenceCursor(cursor) {
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
	cursor, ok := e.segdirCursor(tableName)
	if !ok {
		return 0
	}
	maxLevel := 0
	for {
		_, rec, ok := nextSegdirScanRecord(cursor, 1)
		if !ok {
			break
		}
		if lv, ok := rec.Values[0].(int64); ok && int(lv) < 1024 && int(lv) > maxLevel {
			maxLevel = int(lv)
		}
		if !advanceSequenceCursor(cursor) {
			break
		}
	}
	return maxLevel
}

// readFTSSegdirRowsRange reads every %_segdir row whose absolute level is in
// [lo, hi], sorted by (level, idx).
func (e *DDLExecutor) readFTSSegdirRowsRange(tableName string, lo, hi int) []ftsSegdirRow {
	cursor, ok := e.segdirCursor(tableName)
	if !ok {
		return nil
	}
	rows := scanSegdirRows(cursor, func(lv int) bool { return lv >= lo && lv <= hi })
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].level != rows[j].level {
			return rows[i].level < rows[j].level
		}
		return rows[i].idx < rows[j].idx
	})
	return rows
}
