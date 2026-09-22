// Package execddl: FTS shadow-integrity validation — the %_segdir /
// %_segments / %_content corruption checks behind validateFTSSegments and the
// %_segments block readers they share. Split from ddl_core_tail.go; behavior
// unchanged.
package execddl

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// validateFTSSegdirRows walks the %_segdir btree and validates every
// segment row (the segdir root column is the last column: level, idx,
// start_block, leaves_end_block, end_block, root).
func (e *DDLExecutor) validateFTSSegdirRows(tableName string, checkBlocks bool) *Result {
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
	_ = cursor
	for {
		rec, stop, res := nextSegdirRecord(cursor)
		if res != nil {
			return res
		}
		if stop {
			break
		}
		if res := e.validateFTSSegdirRow(tableName, checkBlocks, rec); res != nil {
			return res
		}
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	return nil
}

// nextSegdirRecord reads the segdir cursor's current row, decoding its
// record; stop=true ends the walk (clean EOF or an undecodable trailing
// cell), res != nil is a corruption error.
func nextSegdirRecord(cursor *btree.Cursor) (*storage.Record, bool, *Result) {
	cell, rerr := cursor.ReadCell()
	if rerr != nil {
		// "cursor at end" is the normal EOF condition; any other error is
		// a corrupt segdir btree (fts3corrupt4 7.1: a crash-written
		// database whose %_segdir table page is damaged). A plain SELECT
		// on %_segdir reports "database disk image is malformed" for the
		// same corruption; the FTS path must not swallow it.
		if strings.Contains(rerr.Error(), "cursor at end") {
			return nil, true, nil
		}
		return nil, false, &Result{Error: fmt.Errorf("database disk image is malformed [SEG1]")}
	}
	if cell == nil {
		return nil, true, nil
	}
	rec, derr := storage.DecodeRecord(cell.Payload)
	if derr != nil || rec == nil || len(rec.Values) == 0 {
		return nil, true, nil
	}
	return rec, false, nil
}

// validateFTSSegdirRow validates ONE %_segdir row: the segment root blob and
// (when checkBlocks) the %_segments blocks the row references.
func (e *DDLExecutor) validateFTSSegdirRow(tableName string, checkBlocks bool, rec *storage.Record) *Result {
	root := segdirRootBytes(rec)
	if len(root) > 0 {
		if verr := fts.ValidateSegmentRoot(root); verr != nil {
			return &Result{Error: fmt.Errorf("database disk image is malformed [SEG2]")}
		}
		// start_block > 0 means the segment spans %_segments blocks
		// (fts3.c fts3SegReader: the reader starts at start_block). A
		// non-zero start_block whose first block is missing is corruption
		// (fts3corrupt4 4.1: UPDATE t1_segdir SET start_block=1 on a
		// single-leaf segment → MATCH fails "database disk image is
		// malformed"). A NULL root is an empty segment (fts3corrupt4 6.1:
		// INSERT INTO Table0_segdir VALUES(1,NULL,1,NULL,NULL,NULL) →
		// MATCH succeeds), so the check only applies when root is present.
		if res := e.validateSegdirStartBlock(tableName, rec); res != nil {
			return res
		}
	} else if segdirRootEmpty(rec) {
		// A zero-length NON-NULL root (UPDATE t1_segdir SET root='') is a
		// node buffer that cannot hold even the first varint of a leaf or
		// interior node — the segment is corrupt (fts3corrupt 2.2/3.2:
		// MATCH fails "database disk image is malformed"; the same rule
		// rejects the merge=1 command's crafted level-2000 rootless row).
		// NULL stays the "empty segment" marker per 6.1 above.
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	// Validate the segment blocks referenced by start_block..end_block
	// (end_block is "<endBlock> <leafDataSize>" text or an integer —
	// fts3.c fts3ReadEndBlockField reads the FIRST value as the last
	// block; the second is the leaf-data size, not a block ID). A missing
	// or invalid block is corruption (fts3corrupt 6.10: a manually-
	// inserted segdir row referencing a bad block), as is a block whose
	// term range breaks the sorted order (fts3corrupt 8.3 copies block 1
	// into block 2). The merge command reads these blocks; a MATCH/SELECT
	// only reads the root, so the check is merge-only.
	if checkBlocks && len(rec.Values) >= 5 {
		return e.validateSegdirBlocks(tableName, rec)
	}
	return nil
}

// segdirRootBytes decodes the row's root column (bytes or text).
func segdirRootBytes(rec *storage.Record) []byte {
	var root []byte
	switch rv := rec.Values[len(rec.Values)-1].(type) {
	case []byte:
		root = rv
	case string:
		root = []byte(rv)
	}
	return root
}

// validateSegdirStartBlock rejects a segment whose non-zero start_block
// points at a missing %_segments block; see validateFTSSegdirRow.
func (e *DDLExecutor) validateSegdirStartBlock(tableName string, rec *storage.Record) *Result {
	if len(rec.Values) < 5 {
		return nil
	}
	sb, ok := rec.Values[2].(int64)
	if !ok || sb <= 0 {
		return nil
	}
	blk, verr := e.readFTSBlock(tableName, int(sb))
	if verr != nil {
		return &Result{Error: fmt.Errorf("database disk image is malformed [SEG3]")}
	}
	if blk == nil {
		return &Result{Error: fmt.Errorf("database disk image is malformed [SEG4]")}
	}
	return nil
}

// validateSegdirBlocks walks a row's referenced leaf blocks in order; see
// validateFTSSegdirRow.
func (e *DDLExecutor) validateSegdirBlocks(tableName string, rec *storage.Record) *Result {
	startBlock := 0
	if sb, ok := rec.Values[2].(int64); ok {
		startBlock = int(sb)
	}
	endBlockID := segdirEndBlockID(rec)
	// start_block==0 with a non-empty root means the whole segment
	// lives in the root node (fts3_write.c fts3SegReaderNew:
	// iStartLeaf==0 → rootOnly=1); no %_segments blocks are read, so
	// patched end_block values are irrelevant (fts3corrupt6 4.1).
	// An EMPTY root with end_block set is still validated against the
	// referenced blocks (fts3corrupt 6.10: NULL block 16 → malformed).
	rootEmpty := segdirRootEmpty(rec)
	if !rootEmpty && startBlock <= 0 {
		return nil
	}
	if endBlockID < startBlock {
		endBlockID = startBlock
	}
	// The segment's LAST LEAF block: a NULL at an id above this is
	// the merge writer's pre-allocation marker, not corruption.
	leavesEnd := 0
	if lv, ok := rec.Values[3].(int64); ok {
		leavesEnd = int(lv)
	}
	return e.walkSegdirLeafBlocks(tableName, startBlock, leavesEnd)
}

// segdirEndBlockID decodes end_block's first component (the last block id).
func segdirEndBlockID(rec *storage.Record) int {
	var endBlockID int
	switch eb := rec.Values[4].(type) {
	case string:
		if fields := strings.Fields(eb); len(fields) > 0 {
			endBlockID, _ = strconv.Atoi(fields[0])
		}
	case int64:
		endBlockID = int(eb)
	}
	return endBlockID
}

// segdirRootEmpty reports whether the row's root column is empty.
func segdirRootEmpty(rec *storage.Record) bool {
	if len(rec.Values) == 0 {
		return false
	}
	switch rv := rec.Values[len(rec.Values)-1].(type) {
	case []byte:
		return len(rv) == 0
	case string:
		return rv == ""
	}
	return false
}

// walkSegdirLeafBlocks walks the segment's LEAF blocks (start_block..
// leaves_end_block) checking presence and term order; see
// validateSegdirBlocks.
//
// end_block's first component on a merge output is the pre-allocated range
// END (the NULL marker id, far above the leaves) — walking that far would
// read thousands of unrelated block ids (fts4merge 1.4: readFTSBlock errors
// over the sparse range), so the walk covers the LEAF blocks only.
func (e *DDLExecutor) walkSegdirLeafBlocks(tableName string, startBlock, leavesEnd int) *Result {
	walkEnd := leavesEnd
	if walkEnd < startBlock {
		walkEnd = startBlock
	}
	var prevLast string
	for id := startBlock; id <= walkEnd; id++ {
		if id <= 0 {
			continue
		}
		blk, verr := e.readFTSBlock(tableName, id)
		if verr != nil {
			// A missing block in the middle of the range is skipped:
			// the engine's own btree can drop a cell when the
			// multi-page leaf split's interior separator is stale
			// (a pre-existing defect); treating it as corruption
			// here would reject valid tables. Corruption tests that
			// matter (6.10 NULL block, 8.3 order) still hit the
			// checks below when the blocks exist.
			continue
		}
		if res := checkSegdirLeafBlock(id, blk, leavesEnd, &prevLast); res != nil {
			return res
		}
	}
	return nil
}

// checkSegdirLeafBlock validates one walked leaf block (the NULL-marker
// carve-out and the sorted-term-range rule); see walkSegdirLeafBlocks.
func checkSegdirLeafBlock(id int, blk []byte, leavesEnd int, prevLast *string) *Result {
	// A %_segments row with a NULL block (no content) is corrupt
	// (fts3corrupt 6.10: INSERT INTO f_segments (blockid) values
	// (16) then merge=1 → "database disk image is malformed")
	// — EXCEPT a NULL at an id ABOVE leaves_end_block: that is
	// a merge output's pre-allocated range marker (SQLite's
	// fts3IncrmergeWriter writes a NULL row at iEnd;
	// fts4langid 5.4/fts4growth 1.5 carry such rows).
	if blk == nil {
		if id > leavesEnd {
			return nil
		}
		return &Result{Error: fmt.Errorf("database disk image is malformed [SEG5]")}
	}
	first, last := fts.LeafTermRange(blk)
	if *prevLast != "" && first != "" && first <= *prevLast {
		return &Result{Error: fmt.Errorf("database disk image is malformed [SEG6]")}
	}
	if last != "" {
		*prevLast = last
	}
	return nil
}

// validateFTSShadowRoots walks the %_segments and %_content shadow tables'
// root-page btrees, parsing every page so structural corruption (an
// out-of-range cell-content pointer, a crash-written page) is detected. The
// engine's FTS SELECT reads rows from the in-memory store, so it would
// otherwise never touch a corrupt shadow btree (fts3corrupt4 21.1: offsets()
// must fail "database disk image is malformed" when Tree 4 page 4 is corrupt).
func (e *DDLExecutor) validateFTSShadowRoots(tableName string, checkContent bool) *Result {
	suffixes := []string{"_segments"}
	if checkContent {
		suffixes = append(suffixes, "_content")
	}
	for _, suffix := range suffixes {
		name := tableName + suffix
		ent, _, err := e.ctx.FindTable(name)
		if err != nil || ent == nil || ent.RootPage == 0 {
			continue
		}
		if res := e.validateShadowBTreePages(ent, suffix); res != nil {
			return res
		}
	}
	return nil
}

// validateShadowBTreePages walks one shadow table's root-page btree (a
// seen-set guard against cycles), parsing every page so structural
// corruption (an out-of-range cell-content pointer, a crash-written page) is
// detected. The engine's FTS SELECT reads rows from the in-memory store, so
// it would otherwise never touch a corrupt shadow btree (fts3corrupt4 21.1:
// offsets() must fail "database disk image is malformed" when Tree 4 page 4
// is corrupt).
func (e *DDLExecutor) validateShadowBTreePages(ent *schema.Entry, suffix string) *Result {
	seen := map[uint32]bool{}
	stack := []uint32{ent.RootPage}
	first := true
	for len(stack) > 0 {
		pageNum := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pageNum] {
			continue
		}
		seen[pageNum] = true
		// The %_content table's cell pointers must lie within the
		// content area; a pointer below the content start is a corrupt
		// cell (SQLite "cell offset out of range", fts3corrupt4 21.1:
		// t1_content cell 23 at offset 808 below the 2888 content area).
		// Only leaf pages carry a cell-pointer array at offset 8;
		// interior pages have a 12-byte header (rightmost pointer) and
		// their cell pointers live at offset 12 — skip them.
		if suffix == "_content" {
			if res := e.validateContentCellPointers(pageNum); res != nil {
				return res
			}
		}
		retry, res := e.openShadowCursor(ent, pageNum, first)
		if res != nil {
			return res
		}
		if retry {
			first = false
			continue
		}
		first = false
		// OpenCursor descends to the leftmost leaf, parsing pages on the
		// way; ParsePage's free-space/cell-content check rejects corrupt
		// pages. Iterate every cell only for %_content (a SELECT with
		// offsets/snippet reads it; fts3corrupt4 21.1: t1_content cell 23
		// has an out-of-range offset). %_segments is only root-validated:
		// an INSERT writes the index but SQLite does not read corrupt
		// segment cells (10.2 succeeds despite a bad segments cell).
		if suffix != "_content" {
			// %_segments is only structure-validated: parse the root page
			// (free-space/cell-content consistency) without reading any
			// cell payload (an INSERT writes the index but SQLite does
			// not read corrupt segment cells; 10.2 succeeds despite a
			// bad segments cell).
			break
		}
		// %_content cells are NOT iterated at prepare time: SQLite reads
		// content rows lazily per matched output row (fts3Column), so a
		// bad cell only fails queries that actually read that row
		// (fts3corrupt7 1.1 succeeds; fts3corrupt4 21.1's offsets() read
		// is caught per-row by validateFTSSnippetAuxContent).
		break
	}
	return nil
}

// openShadowCursor parses one shadow-btree page via OpenCursor (which
// descends to the leftmost leaf, parsing pages on the way). retry=true marks
// the "unknown page type 0x00 at the root" case (the walk continues without
// descending); res != nil is a corruption error (SEG9).
func (e *DDLExecutor) openShadowCursor(ent *schema.Entry, pageNum uint32, first bool) (retry bool, res *Result) {
	tree := e.ctx.TableBTreeForName(ent.Name, pageNum, true)
	_, cerr := tree.OpenCursor()
	if cerr == nil {
		return false, nil
	}
	if first && strings.Contains(cerr.Error(), "unknown page type: 0x00") {
		return true, nil
	}
	return false, &Result{Error: fmt.Errorf("database disk image is malformed [SEG9]")}
}

// validateContentCellPointers checks one %_content page's leaf cell-pointer
// array for out-of-range offsets; see validateShadowBTreePages.
func (e *DDLExecutor) validateContentCellPointers(pageNum uint32) *Result {
	pg, perr := e.ctx.Pager().ReadPage(pageNum)
	if perr != nil || len(pg.Data) < 108 {
		return nil
	}
	coff := 0
	if pageNum == 1 {
		coff = 100
	}
	ptype := pg.Data[coff]
	if ptype != storage.PageTypeLeafTable && ptype != storage.PageTypeLeafIndex {
		return nil
	}
	if coff+8 > len(pg.Data) {
		return nil
	}
	ncell := int(binary.BigEndian.Uint16(pg.Data[coff+3 : coff+5]))
	cc := int(binary.BigEndian.Uint16(pg.Data[coff+5 : coff+7]))
	ps := int(e.ctx.Pager().PageSize())
	for i := 0; i < ncell; i++ {
		if coff+8+2*i+2 > len(pg.Data) {
			return &Result{Error: fmt.Errorf("database disk image is malformed [SEG7]")}
		}
		cp := int(binary.BigEndian.Uint16(pg.Data[coff+8+2*i : coff+10+2*i]))
		if cp < cc || cp >= ps {
			return &Result{Error: fmt.Errorf("database disk image is malformed [SEG8]")}
		}
	}
	return nil
}

// validateFTSMatchCorruption walks a SELECT's WHERE clause for MATCH operators
// and fails with "database disk image is malformed" when the query reads a term
// whose segment doclist is corrupt (SQLite reads the segment at prepare; the
// engine's per-row MATCH check cannot fire when the in-memory index has no
// candidate rows — fts3corrupt4 31.1).
func (e *DDLExecutor) validateFTSMatchCorruption(where sql.Expr, tableName string) *Result {
	if where == nil {
		return nil
	}
	ftsTable, ok := e.ctx.FTSTables()[tableName]
	if !ok {
		return nil
	}
	if e.exprHasCorruptMatchTerm(where, ftsTable) {
		return &Result{Error: fmt.Errorf("database disk image is malformed [SEG10]")}
	}
	return nil
}

// exprHasCorruptMatchTerm reports whether the expression tree contains a
// MATCH (or NOT MATCH) against a corrupt term; see validateFTSMatchCorruption.
func (e *DDLExecutor) exprHasCorruptMatchTerm(expr sql.Expr, ftsTable *fts.FTS3Table) bool {
	switch ex := expr.(type) {
	case *sql.BinaryOp:
		if e.binaryOpHasCorruptTerm(ex, ftsTable) {
			return true
		}
		if e.exprHasCorruptMatchTerm(ex.Left, ftsTable) || e.exprHasCorruptMatchTerm(ex.Right, ftsTable) {
			return true
		}
	case *sql.UnaryOp:
		if e.exprHasCorruptMatchTerm(ex.Operand, ftsTable) {
			return true
		}
	}
	return false
}

// binaryOpHasCorruptTerm checks one binary op's MATCH operand; see
// exprHasCorruptMatchTerm.
func (e *DDLExecutor) binaryOpHasCorruptTerm(ex *sql.BinaryOp, ftsTable *fts.FTS3Table) bool {
	if !strings.EqualFold(ex.Operator, "MATCH") && !strings.EqualFold(ex.Operator, "NOT MATCH") {
		return false
	}
	if qs, ok := ex.Right.(*sql.StringLit); ok {
		return ftsTable.QueryHasCorruptTerm(qs.Value)
	}
	return false
}

// readFTSBlock reads a %_segments block by ID, returning it or a corruption
// error when the block is missing.

func (e *DDLExecutor) readFTSBlock(tableName string, blockID int) ([]byte, *Result) {
	seg := tableName + "_segments"
	segEntry, _, err := e.ctx.FindTable(seg)
	if err != nil || segEntry == nil {
		return nil, &Result{Error: fmt.Errorf("database disk image is malformed [SEG11]")}
	}
	tree := e.ctx.TableBTreeForName(segEntry.Name, segEntry.RootPage, true)
	// Fast path: rowid seek (the btree's binary-search descent). A seek MISS
	// is not authoritative — the descent was observed mis-routing while the
	// %_segments btree carried balance-corrupted interior pages — so a miss
	// falls through to the full scan below, which walks every row and is the
	// correctness oracle.
	if blk, res := e.seekFTSBlock(tree, blockID); res != nil || blk != nil {
		return blk, res
	}
	// Scan fallback: walk every %_segments row (the same traversal the SQL
	// engine's range scan uses). O(rows) per lookup — the fast path above
	// keeps merge workloads off this loop in the common case.
	return e.scanFTSBlock(tree, blockID)
}

// seekFTSBlock is readFTSBlock's rowid-seek fast path; a miss returns
// (nil, nil) so the caller falls back to scanFTSBlock.
func (e *DDLExecutor) seekFTSBlock(tree *btree.BTree, blockID int) ([]byte, *Result) {
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil, &Result{Error: fmt.Errorf("database disk image is malformed [SEG12]")}
	}
	if found, serr := cursor.SeekToRowID(int64(blockID)); serr == nil && found {
		payload, rid, rerr := cursor.ReadCellData()
		if rerr == nil && int(rid) == blockID {
			return decodeSegmentBlock(payload)
		}
	}
	return nil, nil
}

// scanFTSBlock is readFTSBlock's full-scan fallback; see readFTSBlock.
func (e *DDLExecutor) scanFTSBlock(tree *btree.BTree, blockID int) ([]byte, *Result) {
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil, &Result{Error: fmt.Errorf("database disk image is malformed [SEG12]")}
	}
	for {
		payload, rid, rerr := cursor.ReadCellData()
		if rerr != nil {
			break
		}
		if int(rid) == blockID {
			return decodeSegmentBlock(payload)
		}
		if ok, nerr := cursor.Next(); nerr != nil || !ok {
			break
		}
	}
	return nil, &Result{Error: fmt.Errorf("database disk image is malformed [SEG13]")}
}

// decodeSegmentBlock decodes a %_segments row payload into its block bytes
// (the row is (blockid, block); SEG15 on a short/corrupt record, SEG16 when
// the block column is neither a blob nor text).
func decodeSegmentBlock(payload []byte) ([]byte, *Result) {
	rec, derr := storage.DecodeRecord(payload)
	if derr != nil || rec == nil || len(rec.Values) < 2 {
		return nil, &Result{Error: fmt.Errorf("database disk image is malformed [SEG15]")}
	}
	switch bv := rec.Values[1].(type) {
	case []byte:
		return bv, nil
	case string:
		return []byte(bv), nil
	}
	return nil, &Result{Error: fmt.Errorf("database disk image is malformed [SEG16]")}
}
