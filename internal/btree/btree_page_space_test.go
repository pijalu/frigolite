package btree

// Pins for the btree.c page-space port (btree_page_space.go) and the
// in-place overwrite (btree_update_inplace.go): dropCell must leave a
// well-formed freeblock chain / fragmented-byte count, allocateSpace must
// reuse or fold that space, and the same-size overwrite must keep the cell
// at its exact offset. Every scenario ends with a full row-order walk so a
// silent routing or layout corruption fails loudly.

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// newTestTableBTree builds a table btree over a fresh in-memory pager with
// the given page size (the harness pattern of btree_test.go).
func newTestTableBTree(t *testing.T, pageSize int) *BTree {
	t.Helper()
	pg := pager.OpenInMemory(uint32(pageSize))
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		t.Fatal(err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatal(err)
	}
	return NewBTree(pg, 1, true)
}

func testCell(t *testing.T, rid int64, body string) *storage.Cell {
	t.Helper()
	rec, err := storage.EncodeRecord([]interface{}{rid, body})
	if err != nil {
		t.Fatal(err)
	}
	return &storage.Cell{Type: storage.CellTableLeaf, RowID: rid, Payload: rec}
}

// insertRows fills the tree with rowids 1..n, body "row-<rid>-<pad>".
func insertRows(t *testing.T, bt *BTree, n int, pad int) {
	t.Helper()
	for rid := 1; rid <= n; rid++ {
		body := fmt.Sprintf("row-%d-%s", rid, makePad(pad))
		if err := bt.InsertCell(testCell(t, int64(rid), body)); err != nil {
			t.Fatalf("InsertCell(%d): %v", rid, err)
		}
	}
}

func makePad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// walkRows returns every (rowid, payload-string-column) in rowid order.
func walkRows(t *testing.T, bt *BTree) [][2]interface{} {
	t.Helper()
	cur, err := bt.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]interface{}
	for {
		cell, rerr := cur.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			t.Fatalf("decode rowid %d: %v", cell.RowID, derr)
		}
		var body string
		switch v := rec.Values[1].(type) {
		case string:
			body = v
		case []byte:
			body = string(v)
		default:
			t.Fatalf("rowid %d: unexpected body type %T", cell.RowID, rec.Values[1])
		}
		out = append(out, [2]interface{}{cell.RowID, body})
		if ok, nerr := cur.Next(); nerr != nil || !ok {
			break
		}
	}
	return out
}

func requireRows(t *testing.T, got [][2]interface{}, wantIDs []int64, pad int) {
	t.Helper()
	if len(got) != len(wantIDs) {
		t.Fatalf("row count = %d, want %d", len(got), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i][0] != id {
			t.Fatalf("row %d rowid = %v, want %d", i, got[i][0], id)
		}
		wantBody := fmt.Sprintf("row-%d-%s", id, makePad(pad))
		if got[i][1] != wantBody {
			t.Fatalf("row %d body = %v, want %q", i, got[i][1], wantBody)
		}
	}
}

// parseTestLeaf reads the page header fields of the tree's root.
func parseTestLeaf(t *testing.T, bt *BTree) *storage.BTreePage {
	t.Helper()
	pg, err := bt.pager.ReadPage(bt.rootPage)
	if err != nil {
		t.Fatal(err)
	}
	page, err := storage.ParsePage(pg.Data, int(bt.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// leafOf seeks rowID and returns its leaf page, parsed header and cell index.
func leafOf(t *testing.T, bt *BTree, rowID int64) (*pager.Page, *storage.BTreePage, int) {
	t.Helper()
	pg, page, idx, ok, err := bt.seekLeafRow(rowID)
	if err != nil || !ok {
		t.Fatalf("rowid %d not on a leaf (ok=%v err=%v)", rowID, ok, err)
	}
	return pg, page, idx
}

// TestDropCellLeavesReusableFreeblock pins the O(1) point-delete: the
// deleted cell's bytes join the page's free space (not a compaction), the
// freed space is reusable by the next insert of the same size, and the
// surviving rows keep their order.
func TestDropCellLeavesReusableFreeblock(t *testing.T) {
	bt := newTestTableBTree(t, 1024)
	defer bt.Close()
	insertRows(t, bt, 40, 20) // single leaf page

	insertRows(t, bt, 40, 20) // rows span several leaves at 1024 bytes
	leafPg, _, _ := leafOf(t, bt, 10)
	leafNum := leafPg.PageNum
	if n, err := bt.DeleteCellByRowID(10); err != nil || n != 1 {
		t.Fatalf("delete rowid 10: n=%d err=%v", n, err)
	}
	leafPg, _ = bt.pager.ReadPage(leafNum)
	coff := contentOffset(leafNum)
	page, err := storage.ParsePage(leafPg.Data, int(bt.pageSize), coff)
	if err != nil {
		t.Fatal(err)
	}
	if page.FirstFree == 0 && page.FragFree == 0 {
		t.Fatalf("dropCell left no free-space accounting (compaction happened)")
	}
	// The freed block must be reachable and its size must cover the deleted
	// cell: walk the chain and total the sizes.
	pg := leafPg
	total := 0
	blocks := 0
	for off := int(page.FirstFree); off != 0; {
		if off < coff+8+2*int(page.CellCount) || off+4 > int(bt.usableSize) {
			t.Fatalf("freeblock %d out of bounds", off)
		}
		sz := int(binary.BigEndian.Uint16(pg.Data[off+2 : off+4]))
		if sz < 4 {
			t.Fatalf("freeblock %d size %d < 4", off, sz)
		}
		total += sz
		blocks++
		off = int(binary.BigEndian.Uint16(pg.Data[off : off+2]))
	}
	if blocks != 1 || total <= 0 {
		t.Fatalf("freeblock chain: blocks=%d total=%d", blocks, total)
	}
	blocksBefore := 0
	for off := int(page.FirstFree); off != 0; {
		blocksBefore++
		off = int(binary.BigEndian.Uint16(pg.Data[off : off+2]))
	}
	countBefore := page.CellCount
	// Reinsert the same rowid at the same size: the freed block must be
	// consumed (no new freeblock chained, cell count restored).
	if err := bt.InsertCell(testCell(t, 10, fmt.Sprintf("row-%d-%s", 10, makePad(20)))); err != nil {
		t.Fatalf("reinsert: %v", err)
	}
	leafPg, _ = bt.pager.ReadPage(leafNum)
	page, err = storage.ParsePage(leafPg.Data, int(bt.pageSize), coff)
	if err != nil {
		t.Fatal(err)
	}
	if page.CellCount != countBefore+1 {
		t.Fatalf("cell count after reinsert = %d, want %d", page.CellCount, countBefore+1)
	}
	blocksAfter := 0
	for off := int(page.FirstFree); off != 0; {
		blocksAfter++
		off = int(binary.BigEndian.Uint16(leafPg.Data[off : off+2]))
	}
	if blocksAfter > blocksBefore {
		t.Fatalf("reinsert chained a new freeblock (%d -> %d)", blocksBefore, blocksAfter)
	}
	rows := walkRows(t, bt)
	ids := make([]int64, 40)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	requireRows(t, rows, ids, 20)
}

// TestDropCellAdjacentContentArea pins the content-area extension case: the
// lowest-addressed cell (here the last-inserted rowid on a packed page)
// returns straight to the content area — no freeblock is chained.
func TestDropCellAdjacentContentArea(t *testing.T) {
	bt := newTestTableBTree(t, 1024)
	defer bt.Close()
	insertRows(t, bt, 12, 8)
	page := parseTestLeaf(t, bt)
	if page.CellCount != 12 {
		t.Fatalf("setup: cell count %d", page.CellCount)
	}
	if _, err := bt.DeleteCellByRowID(12); err != nil {
		t.Fatal(err)
	}
	page = parseTestLeaf(t, bt)
	if page.FirstFree != 0 {
		t.Fatalf("content-adjacent removal chained a freeblock at %d", page.FirstFree)
	}
	if page.CellCount != 11 {
		t.Fatalf("cell count = %d, want 11", page.CellCount)
	}
	rows := walkRows(t, bt)
	ids := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	requireRows(t, rows, ids, 8)
}

// TestFreeSpaceCoalesce pins freeSpace's coalescing: deleting adjacent cells
// leaves ONE freeblock spanning both, not two.
func TestFreeSpaceCoalesce(t *testing.T) {
	bt := newTestTableBTree(t, 1024)
	defer bt.Close()
	// Ascending inserts pack cells downward in rowid order, so rowids 3 and
	// 4 occupy adjacent byte ranges in the middle of the page (away from
	// both the pointer array and the content area).
	insertRows(t, bt, 12, 8)
	for _, rid := range []int64{4, 3} {
		if n, err := bt.DeleteCellByRowID(rid); err != nil || n != 1 {
			t.Fatalf("delete %d: n=%d err=%v", rid, n, err)
		}
	}
	page := parseTestLeaf(t, bt)
	blocks := 0
	pg, _ := bt.pager.ReadPage(bt.rootPage)
	for off := int(page.FirstFree); off != 0; {
		blocks++
		off = int(binary.BigEndian.Uint16(pg.Data[off : off+2]))
	}
	if blocks != 1 {
		t.Fatalf("adjacent frees produced %d freeblocks, want 1 (coalesce)", blocks)
	}
	rows := walkRows(t, bt)
	requireRows(t, rows, []int64{1, 2, 5, 6, 7, 8, 9, 10, 11, 12}, 8)
}

// TestDeleteInsertCycleStablePageCount pins allocateSpace over a long
// delete/insert cycle: freed slots are reused (or folded), the tree never
// grows, and every rowid remains seekable.
func TestDeleteInsertCycleStablePageCount(t *testing.T) {
	bt := newTestTableBTree(t, 1024)
	defer bt.Close()
	insertRows(t, bt, 400, 30)
	pagesBefore := bt.pager.NumPages()
	for round := 0; round < 3; round++ {
		for rid := round*50 + 1; rid <= round*50+100; rid += 2 {
			if n, err := bt.DeleteCellByRowID(int64(rid)); err != nil || n != 1 {
				t.Fatalf("round %d delete %d: n=%d err=%v", round, rid, n, err)
			}
		}
		for rid := round*50 + 1; rid <= round*50+100; rid += 2 {
			if err := bt.InsertCell(testCell(t, int64(rid), fmt.Sprintf("row-%d-%s", rid, makePad(30)))); err != nil {
				t.Fatalf("round %d insert %d: %v", round, rid, err)
			}
		}
	}
	if got := bt.pager.NumPages(); got > pagesBefore {
		t.Fatalf("page count grew %d -> %d over reuse cycle", pagesBefore, got)
	}
	rows := walkRows(t, bt)
	if len(rows) != 400 {
		t.Fatalf("row count = %d, want 400", len(rows))
	}
	for i, r := range rows {
		if r[0] != int64(i+1) {
			t.Fatalf("row %d rowid = %v, want %d", i, r[0], i+1)
		}
	}
}

// TestInPlaceOverwriteKeepsCellOffset pins the sqlite3BtreeInsert loc==0
// fast path: a same-size overwrite moves NO bytes but the cell payload —
// the cell pointer, content start, freeblock head and fragmented count stay
// byte-identical — and a size change or overflow involvement declines.
func TestInPlaceOverwriteKeepsCellOffset(t *testing.T) {
	bt := newTestTableBTree(t, 1024)
	defer bt.Close()
	insertRows(t, bt, 30, 40)

	pg, _, _ := leafOf(t, bt, 7)
	leafNum := pg.PageNum
	before := append([]byte(nil), pg.Data...)
	oldOff, oldErr := bt.overwriteTargetOffset(7)
	if oldErr != nil {
		t.Fatal(oldErr)
	}

	// Same-size overwrite (same body length): must take the fast path.
	same := storage.EncodeCell(testCell(t, 7, fmt.Sprintf("row-%d-%s", 7, replaceX(makePad(40)))))
	done, err := bt.OverwriteCellByRowID(7, same)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatalf("same-size overwrite declined")
	}
	after, _ := bt.pager.ReadPage(leafNum)
	if string(before) == string(after.Data) {
		t.Fatalf("overwrite wrote nothing")
	}
	// Everything outside the target cell's byte range must be untouched.
	for i := 0; i < int(bt.usableSize); i++ {
		if i >= oldOff && i < oldOff+len(same) {
			continue
		}
		if before[i] != after.Data[i] {
			t.Fatalf("byte %d changed outside the overwritten cell (%x -> %x)", i, before[i], after.Data[i])
		}
	}
	rows := walkRows(t, bt)
	if len(rows) != 30 {
		t.Fatalf("row count = %d", len(rows))
	}

	// Size-differing overwrite must decline (fall back to delete+insert).
	big := storage.EncodeCell(testCell(t, 7, fmt.Sprintf("row-%d-%s", 7, makePad(400))))
	done, err = bt.OverwriteCellByRowID(7, big)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatalf("size-differing overwrite took the in-place path")
	}

	// The declined path's fallback (delete + insert) keeps the tree sound.
	if err := bt.InsertCell(testCell(t, 7, fmt.Sprintf("row-%d-%s", 7, makePad(400)))); err != nil {
		t.Fatal(err)
	}
	rows = walkRows(t, bt)
	if len(rows) != 30 {
		t.Fatalf("row count after fallback = %d", len(rows))
	}
	if rows[6][0] != int64(7) || rows[6][1] != fmt.Sprintf("row-%d-%s", 7, makePad(400)) {
		t.Fatalf("row 7 = %v/%v after fallback", rows[6][0], rows[6][1])
	}
}

// overwriteTargetOffset returns the byte offset of rowid's cell on its leaf
// (test helper: seek through the cursor).
func (bt *BTree) overwriteTargetOffset(rowID int64) (int, error) {
	pg, page, idx, ok, err := bt.seekLeafRow(rowID)
	if err != nil || !ok {
		return 0, fmt.Errorf("rowid %d not found (ok=%v err=%v)", rowID, ok, err)
	}
	_ = page
	coff := contentOffset(pg.PageNum)
	return int(storage.CellPointer(pg.Data, coff, idx, int(bt.pageSize))), nil
}

func replaceX(s string) string {
	b := []byte(s)
	if len(b) > 0 {
		b[0] = 'y'
	}
	return string(b)
}
