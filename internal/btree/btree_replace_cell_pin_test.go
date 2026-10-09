package btree

// R13-L6 pin: ReplaceCellByRowIDAt is sqlite3BtreeInsert's whole loc==0 branch
// (src/btree.c:9576-9677) — the same-size memcpy, or dropCell(idx) +
// insertCellFast(idx) on the SAME page with no second root-to-leaf descent.
// The tests pin the three outcomes, the logical equivalence of the new
// same-page branch to the seek-delete + re-insert fallback, and the decline
// that keeps a page which cannot hold the cell on that fallback.

import (
	"bytes"
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
)

// replaceTargetOffset returns the leaf page and on-page offset of the cell
// holding rowID.
func replaceTargetOffset(t *testing.T, bt *BTree, rowID int64) (uint32, int, int) {
	t.Helper()
	pg, _, idx := leafOf(t, bt, rowID)
	coff := contentOffset(pg.PageNum)
	off := int(storage.CellPointer(pg.Data, coff, idx, int(bt.pageSize)))
	return pg.PageNum, idx, off
}

// replaceRecord encodes the (rowid, body) record testCell builds, for the
// fallback path's InsertCell (which takes the payload, not the cell image).
func replaceRecord(t *testing.T, rid int64, body string) []byte {
	t.Helper()
	rec, err := storage.EncodeRecord([]interface{}{rid, body})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestReplaceCellByRowIDAtSameSizeMemcpy pins the loc==0 memcpy branch: a
// same-size replacement moves only the target cell's bytes.
func TestReplaceCellByRowIDAtSameSizeMemcpy(t *testing.T) {
	bt := newTestTableBTree(t, 1024)
	defer bt.Close()
	insertRows(t, bt, 40, 30)
	leafNum, idx, off := replaceTargetOffset(t, bt, 7)
	before := append([]byte(nil), mustReadPage(t, bt, leafNum)...)

	body := "row-7-" + makePad(30)
	cell := storage.EncodeCell(testCell(t, 7, body))
	m0, d0, x0 := replaceLaneHitsForTest()
	done, err := bt.ReplaceCellByRowIDAt(7, cell, leafNum, idx)
	if err != nil || !done {
		t.Fatalf("same-size replace: done=%v err=%v", done, err)
	}
	m1, d1, x1 := replaceLaneHitsForTest()
	if m1-m0 != 1 || d1-d0 != 0 || x1-x0 != 0 {
		t.Fatalf("outcome counters: memcpy=%d dropInsert=%d declined=%d, want 1/0/0",
			m1-m0, d1-d0, x1-x0)
	}
	after := mustReadPage(t, bt, leafNum)
	// Only the cell's bytes may move (the cell pointer array, content start,
	// freeblock chain and fragmented count stay byte-identical).
	for i := range after {
		if i >= off && i < off+len(cell) {
			continue
		}
		if before[i] != after[i] {
			t.Fatalf("byte %d changed outside the replaced cell (0x%02x -> 0x%02x)", i, before[i], after[i])
		}
	}
	assertRowBody(t, bt, 7, body)
}

// TestReplaceCellByRowIDAtSizeChangeSamePage pins the new branch: a
// size-changing replacement stays on the SAME leaf page (no page-count growth,
// no second descent) through dropCell + insertCellFast, and the tree ends up
// with exactly the rows the seek-delete + insert fallback produces.
func TestReplaceCellByRowIDAtSizeChangeSamePage(t *testing.T) {
	// Two identically built trees: one written through the fast path, one
	// through DeleteCellByRowID + InsertCell (the caller's fallback).
	fast := newTestTableBTree(t, 4096)
	defer fast.Close()
	insertRows(t, fast, 60, 30)
	slow := newTestTableBTree(t, 4096)
	defer slow.Close()
	insertRows(t, slow, 60, 30)

	body := "row-11-" + makePad(200) // 207 bytes, ~170 more than the old cell
	big := storage.EncodeCell(testCell(t, 11, body))
	leafNum, idx, _ := replaceTargetOffset(t, fast, 11)
	pagesBefore := fast.pager.NumPages()

	m0, d0, x0 := replaceLaneHitsForTest()
	done, err := fast.ReplaceCellByRowIDAt(11, big, leafNum, idx)
	if err != nil || !done {
		t.Fatalf("size-changing replace: done=%v err=%v", done, err)
	}
	m1, d1, x1 := replaceLaneHitsForTest()
	if m1-m0 != 0 || d1-d0 != 1 || x1-x0 != 0 {
		t.Fatalf("outcome counters: memcpy=%d dropInsert=%d declined=%d, want 0/1/0",
			m1-m0, d1-d0, x1-x0)
	}
	if got := fast.pager.NumPages(); got != pagesBefore {
		t.Fatalf("fast path grew the tree: %d -> %d pages", pagesBefore, got)
	}

	// The fallback reference: seek-delete, then insert the same record.
	if _, derr := slow.DeleteCellByRowID(11); derr != nil {
		t.Fatal(derr)
	}
	if ierr := slow.InsertCell(&storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   11,
		Payload: replaceRecord(t, 11, body),
	}); ierr != nil {
		t.Fatal(ierr)
	}

	fastRows, slowRows := walkRows(t, fast), walkRows(t, slow)
	if len(fastRows) != 60 || len(slowRows) != 60 {
		t.Fatalf("row counts: fast=%d slow=%d want 60", len(fastRows), len(slowRows))
	}
	for i := range fastRows {
		if fastRows[i] != slowRows[i] {
			t.Fatalf("row %d: fast=%v slow=%v", i, fastRows[i], slowRows[i])
		}
	}
	assertRowBody(t, fast, 11, body)
}

// TestReplaceCellByRowIDAtDeclinesWhenCellCannotFit pins the space proof's
// decline: a cell the page cannot hold once the old one is dropped reports
// done=false with the page untouched, so the caller's split-capable fallback
// owns it.
func TestReplaceCellByRowIDAtDeclinesWhenCellCannotFit(t *testing.T) {
	bt := newTestTableBTree(t, 1024)
	defer bt.Close()
	insertRows(t, bt, 40, 20) // many cells: little free space per leaf
	leafNum, idx, _ := replaceTargetOffset(t, bt, 20)
	before := append([]byte(nil), mustReadPage(t, bt, leafNum)...)

	// A body far bigger than any 1024-byte leaf can hold: the fast path must
	// decline rather than let writeLeafCell fail after the drop.
	huge := storage.EncodeCell(testCell(t, 20, "row-20-"+makePad(4000)))
	m0, d0, x0 := replaceLaneHitsForTest()
	done, err := bt.ReplaceCellByRowIDAt(20, huge, leafNum, idx)
	if err != nil {
		t.Fatalf("declined replace returned an error: %v", err)
	}
	if done {
		t.Fatalf("oversized replacement took the fast path")
	}
	m1, d1, x1 := replaceLaneHitsForTest()
	if m1-m0 != 0 || d1-d0 != 0 || x1-x0 != 1 {
		t.Fatalf("outcome counters: memcpy=%d dropInsert=%d declined=%d, want 0/0/1",
			m1-m0, d1-d0, x1-x0)
	}
	if after := mustReadPage(t, bt, leafNum); !bytes.Equal(before, after) {
		t.Fatalf("declined replace mutated the leaf page")
	}
	assertRowBody(t, bt, 20, "row-20-"+makePad(20))
}

// mustReadPage returns a copy of the page's bytes.
func mustReadPage(t *testing.T, bt *BTree, pageNum uint32) []byte {
	t.Helper()
	pg, err := bt.pager.ReadPage(pageNum)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), pg.Data...)
}

// assertRowBody fails unless the row's body column holds want.
func assertRowBody(t *testing.T, bt *BTree, rowID int64, want string) {
	t.Helper()
	for _, r := range walkRows(t, bt) {
		if r[0] == rowID {
			if r[1] != want {
				t.Fatalf("rowid %d body = %q, want %q", rowID, r[1], want)
			}
			return
		}
	}
	t.Fatalf("rowid %d not found", rowID)
}
