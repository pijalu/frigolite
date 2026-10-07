package btree

// Parse-memo refresh pins (R11.BTREEMEMO): every mutating write path that
// keeps its parsed header in step re-arms the pager's parse memo
// (Page.RefreshParsedBTree), so the FIRST access after a page mutation is a
// memo HIT instead of a full re-parse. These tests pin, per write shape:
//
//  1. the post-mutation memo is byte-for-byte the fresh validating parse
//     (field-for-field — the parse is a pure function of the header span),
//  2. the next access is SERVED from the memo (hit counter, zero misses),
//  3. a stale/unsynced struct is declined (canary), leaving the miss path,
//  4. snapshot/rollback restores revert the memo (bytes restored => re-parse),
//  5. the 64KiB u16 content-offset wrap survives refresh (empty-page reset),
//  6. corrupt images still error exactly on the load path (disk-loaded pages
//     keep the validating parse; refresh never bypasses it).

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

func memoTestCell(rowID int64, payloadByte int) *storage.Cell {
	rec, _ := storage.EncodeRecord([]interface{}{rowID, string(make([]byte, payloadByte))})
	return &storage.Cell{Type: storage.CellTableLeaf, RowID: rowID, Payload: rec}
}

// memoTestPage returns page 1 (the test trees' only leaf/root).
func memoTestPage(t *testing.T, pg *pager.Pager) *pager.Page {
	t.Helper()
	root, err := pg.ReadPage(1)
	if err != nil {
		t.Fatalf("ReadPage(1): %v", err)
	}
	return root
}

// memoTestTree builds a single-leaf table btree rooted at page 1 with rows
// 1..n (payload of payloadByte bytes each).
func memoTestTree(t *testing.T, n int, payloadByte int) (*BTree, *pager.Pager) {
	t.Helper()
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		t.Fatalf("ReadPage(1): %v", err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	bt := NewBTree(pg, 1, true)
	for i := 1; i <= n; i++ {
		if err := bt.InsertCell(memoTestCell(int64(i), payloadByte)); err != nil {
			t.Fatalf("InsertCell(%d): %v", i, err)
		}
	}
	return bt, pg
}

// assertMemoServedFresh resets the memo counters, accesses pg's parsed header,
// and requires the access to be a SERVED hit whose fields equal a fresh
// validating parse of the current bytes.
func assertMemoServedFresh(t *testing.T, pg *pager.Page, pageSize, coff int, label string) *storage.BTreePage {
	t.Helper()
	pager.ResetParseMemoCountersForTest()
	got, err := pg.ParsedBTree(pageSize, coff)
	if err != nil {
		t.Fatalf("%s: ParsedBTree: %v", label, err)
	}
	hits, misses := pager.ParseMemoCountersForTest()
	if hits != 1 || misses != 0 {
		t.Fatalf("%s: expected memo HIT (1 hit, 0 misses), got hits=%d misses=%d", label, hits, misses)
	}
	fresh, err := storage.ParsePage(pg.Data, pageSize, coff)
	if err != nil {
		t.Fatalf("%s: fresh parse: %v", label, err)
	}
	if *got != *fresh {
		t.Fatalf("%s: memo %+v != fresh parse %+v", label, *got, *fresh)
	}
	return got
}

// assertMemoReparse resets the memo counters, accesses pg's parsed header, and
// requires a full re-parse (miss) whose served generation equals the fresh
// validating parse — the never-poisoned contract for un-refreshed shapes
// (splits, rewrites, restores, declines).
func assertMemoReparse(t *testing.T, pg *pager.Page, pageSize, coff int, label string) *storage.BTreePage {
	t.Helper()
	pager.ResetParseMemoCountersForTest()
	got, err := pg.ParsedBTree(pageSize, coff)
	if err != nil {
		t.Fatalf("%s: ParsedBTree: %v", label, err)
	}
	hits, misses := pager.ParseMemoCountersForTest()
	if hits != 0 || misses != 1 {
		t.Fatalf("%s: expected re-parse (0 hits, 1 miss), got hits=%d misses=%d", label, hits, misses)
	}
	fresh, err := storage.ParsePage(pg.Data, pageSize, coff)
	if err != nil {
		t.Fatalf("%s: fresh parse: %v", label, err)
	}
	if *got != *fresh {
		t.Fatalf("%s: memo %+v != fresh parse %+v", label, *got, *fresh)
	}
	return got
}

// armMemo ensures the parse memo on page 1 exists (a miss arms it; a hit
// means an earlier refresh already armed it — both are fine).
func armMemo(t *testing.T, pg *pager.Page, pageSize, coff int) {
	t.Helper()
	pager.ResetParseMemoCountersForTest()
	if _, err := pg.ParsedBTree(pageSize, coff); err != nil {
		t.Fatalf("arm ParsedBTree: %v", err)
	}
	hits, misses := pager.ParseMemoCountersForTest()
	if hits+misses != 1 {
		t.Fatalf("arm: expected 1 access, got hits=%d misses=%d", hits, misses)
	}
}

func TestMemoRefresh_PointDeleteServesRefreshedMemo(t *testing.T) {
	bt, pg := memoTestTree(t, 5, 8)
	coff := pager.HeaderSize
	armMemo(t, memoTestPage(t, pg), pager.DefaultPageSize, coff)
	if n, err := bt.DeleteCellByRowID(3); err != nil || n != 1 {
		t.Fatalf("DeleteCellByRowID(3): n=%d err=%v", n, err)
	}
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after point delete")
	// The tree must still walk correctly (row 3 gone, others intact).
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	var got []int64
	for {
		cell, rerr := c.ReadCell()
		if rerr != nil {
			break
		}
		got = append(got, cell.RowID)
		if ok, nerr := c.Next(); nerr != nil || !ok {
			break
		}
	}
	if len(got) != 4 || got[0] != 1 || got[1] != 2 || got[2] != 4 || got[3] != 5 {
		t.Fatalf("post-delete walk = %v", got)
	}
}

func TestMemoRefresh_InsertServesRefreshedMemo(t *testing.T) {
	bt, pg := memoTestTree(t, 3, 8)
	coff := pager.HeaderSize
	armMemo(t, memoTestPage(t, pg), pager.DefaultPageSize, coff)
	if err := bt.InsertCell(memoTestCell(9, 8)); err != nil {
		t.Fatalf("InsertCell(9): %v", err)
	}
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after insert")
}

func TestMemoRefresh_DupDropPathServesRefreshedMemo(t *testing.T) {
	// Re-inserting an existing rowid runs dropTableLeafDuplicateRowid ->
	// deleteCellOnPage (refresh) then writeLeafCell (refresh): the final memo
	// must describe the page AFTER both.
	bt, pg := memoTestTree(t, 3, 8)
	coff := pager.HeaderSize
	armMemo(t, memoTestPage(t, pg), pager.DefaultPageSize, coff)
	if err := bt.InsertCell(memoTestCell(2, 20)); err != nil {
		t.Fatalf("InsertCell(dup 2): %v", err)
	}
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after dup-drop reinsert")
}

func TestMemoRefresh_QuickAppendServesRefreshedMemo(t *testing.T) {
	bt, pg := memoTestTree(t, 3, 8)
	coff := pager.HeaderSize
	armMemo(t, memoTestPage(t, pg), pager.DefaultPageSize, coff)
	// Ascending append: the quick-append path writes through writeLeafCell.
	if err := bt.InsertCell(memoTestCell(4, 8)); err != nil {
		t.Fatalf("InsertCell(4): %v", err)
	}
	if hits := quickAppendHitsForTest(); hits == 0 {
		t.Logf("quick-append path not engaged (generic path also refreshes)")
	}
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after append")
}

func TestMemoRefresh_GenericRewriteServesRefreshedMemo(t *testing.T) {
	// finishLeafDelete (the predicate/batch rewrite) re-arms the memo too.
	bt, pg := memoTestTree(t, 5, 8)
	coff := pager.HeaderSize
	armMemo(t, memoTestPage(t, pg), pager.DefaultPageSize, coff)
	n, err := bt.deleteAllMatchingFromLeaf(1, func(cell *storage.Cell) bool {
		return cell.RowID == 2 || cell.RowID == 4
	})
	if err != nil || n != 2 {
		t.Fatalf("deleteAllMatchingFromLeaf: n=%d err=%v", n, err)
	}
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after generic rewrite")
}

func TestMemoRefresh_DefragInsertServesRefreshedMemo(t *testing.T) {
	// Delete alternating rows (freeblock chain + fragments), then insert a
	// cell bigger than any single freeblock but smaller than the total free
	// space: allocateSpaceOnPage folds the page (defragmentLeafPage) and
	// writeLeafCell re-arms the memo over the packed layout.
	bt, pg := memoTestTree(t, 8, 8)
	coff := pager.HeaderSize
	for _, row := range []int64{1, 3, 5, 7} {
		if n, err := bt.DeleteCellByRowID(row); err != nil || n != 1 {
			t.Fatalf("DeleteCellByRowID(%d): n=%d err=%v", row, n, err)
		}
	}
	armMemo(t, memoTestPage(t, pg), pager.DefaultPageSize, coff)
	if err := bt.InsertCell(memoTestCell(99, 40)); err != nil {
		t.Fatalf("InsertCell(99): %v", err)
	}
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after defrag insert")
}

func TestMemoRefresh_64KiBEmptyLeafWrap(t *testing.T) {
	const pageSize = 65536
	pg := pager.OpenInMemory(pageSize)
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		t.Fatalf("ReadPage(1): %v", err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	bt := NewBTree(pg, 1, true)
	if err := bt.InsertCell(memoTestCell(1, 8)); err != nil {
		t.Fatalf("InsertCell(1): %v", err)
	}
	coff := pager.HeaderSize
	armMemo(t, memoTestPage(t, pg), pageSize, coff)
	// Deleting the only row empties the page: dropCellResetEmptyPage writes
	// content start = usableSize = 65536, which wraps to 0 in the u16 header
	// field while the parsed struct keeps the unwrapped 65536. The refresh
	// canary must normalize the same way and the memo must serve.
	if n, err := bt.DeleteCellByRowID(1); err != nil || n != 1 {
		t.Fatalf("DeleteCellByRowID(1): n=%d err=%v", n, err)
	}
	got := assertMemoServedFresh(t, memoTestPage(t, pg), pageSize, coff, "after emptying 64KiB leaf")
	if got.CellContent != 65536 {
		t.Fatalf("empty 64KiB leaf CellContent = %d, want unwrapped 65536", got.CellContent)
	}
}

func TestMemoRefresh_DeclinesStaleStruct(t *testing.T) {
	// A struct that does not describe the current bytes (an unsynced write
	// path, hypothetically) must be declined by the live-image canary: the
	// next access takes the normal miss path and re-parses.
	bt, pg := memoTestTree(t, 3, 8)
	coff := pager.HeaderSize
	root := memoTestPage(t, pg)
	armMemo(t, root, pager.DefaultPageSize, coff)
	stale, err := storage.ParsePage(root.Data, pager.DefaultPageSize, coff)
	if err != nil {
		t.Fatalf("stale parse: %v", err)
	}
	if err := bt.InsertCell(memoTestCell(50, 8)); err != nil {
		t.Fatalf("InsertCell(50): %v", err)
	}
	// The write path already refreshed the memo with the CURRENT struct; feed
	// it the STALE one — the canary must decline and the served memo must not
	// regress to the stale generation.
	root.RefreshParsedBTree(pager.DefaultPageSize, coff, stale)
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after declined stale refresh")
}

func TestMemoRefresh_SplitLeavesUnPoisonedMemo(t *testing.T) {
	// The split path rewrites pages without a synced parsed struct: no
	// refresh runs there, so the next access re-parses (validated) — pinned
	// as never-poisoned rather than served.
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	pg.AllocatePage()
	rootPg, err := pg.ReadPage(1)
	if err != nil {
		t.Fatalf("ReadPage(1): %v", err)
	}
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	bt := NewBTree(pg, 1, true)
	for i := 1; i <= 400; i++ {
		if err := bt.InsertCell(memoTestCell(int64(i), 40)); err != nil {
			t.Fatalf("InsertCell(%d): %v", i, err)
		}
	}
	coff := pager.HeaderSize
	assertMemoReparse(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "root after split")
	// Walk the whole tree: every page must parse and every row must surface.
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	count := 0
	for {
		if _, rerr := c.ReadCell(); rerr != nil {
			break
		}
		count++
		ok, nerr := c.Next()
		if nerr != nil || !ok {
			break
		}
	}
	if count != 400 {
		t.Fatalf("post-split walk = %d rows, want 400", count)
	}
}

func TestMemoRefresh_SnapshotRestoreRevertsMemo(t *testing.T) {
	bt, pg := memoTestTree(t, 4, 8)
	coff := pager.HeaderSize
	armMemo(t, memoTestPage(t, pg), pager.DefaultPageSize, coff)
	snap := pg.Snapshot()
	if n, err := bt.DeleteCellByRowID(2); err != nil || n != 1 {
		t.Fatalf("DeleteCellByRowID(2): n=%d err=%v", n, err)
	}
	// Post-delete the memo serves the mutated page (refresh).
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "post-delete")
	// Restore reinstates FRESH page objects: the memo is gone, the next
	// access re-parses the restored bytes (never serves the deleted shape).
	pg.Restore(snap)
	assertMemoReparse(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after restore")
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	count := 0
	for {
		if _, rerr := c.ReadCell(); rerr != nil {
			break
		}
		count++
		ok, nerr := c.Next()
		if nerr != nil || !ok {
			break
		}
	}
	if count != 4 {
		t.Fatalf("restored walk = %d rows, want 4", count)
	}
}

func TestMemoRefresh_StatementRollbackRevertsMemo(t *testing.T) {
	// The per-statement journal restores before-images into the SAME page
	// objects: the memo's fingerprint stops matching, so the next access
	// re-parses the restored bytes.
	bt, pg := memoTestTree(t, 4, 8)
	coff := pager.HeaderSize
	armMemo(t, memoTestPage(t, pg), pager.DefaultPageSize, coff)
	j := pg.BeginStatement()
	if n, err := bt.DeleteCellByRowID(2); err != nil || n != 1 {
		t.Fatalf("DeleteCellByRowID(2): n=%d err=%v", n, err)
	}
	assertMemoServedFresh(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "post-delete in statement")
	pg.RollbackStatement(j)
	assertMemoReparse(t, memoTestPage(t, pg), pager.DefaultPageSize, coff, "after statement rollback")
	c, err := bt.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	count := 0
	for {
		if _, rerr := c.ReadCell(); rerr != nil {
			break
		}
		count++
		ok, nerr := c.Next()
		if nerr != nil || !ok {
			break
		}
	}
	if count != 4 {
		t.Fatalf("rolled-back walk = %d rows, want 4", count)
	}
}

func TestMemoRefresh_CorruptPageStillValidatedOnLoad(t *testing.T) {
	// A corrupt image loaded from the "disk" (the pager cache) must error
	// exactly as a fresh parse would — the refresh path never bypasses
	// validation, and disk-loaded pages never go through refresh.
	_, pg := memoTestTree(t, 3, 8)
	coff := pager.HeaderSize
	root := memoTestPage(t, pg)
	root.Data[coff] = 0x99 // not a page type
	if _, err := root.ParsedBTree(pager.DefaultPageSize, coff); err == nil {
		t.Fatalf("corrupt page type accepted")
	}
	if _, err := storage.ParsePage(root.Data, pager.DefaultPageSize, coff); err == nil {
		t.Fatalf("fresh parse accepted corrupt page type")
	}
}
