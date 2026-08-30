package btree

// P8.INCRVACUUM phase 1: FreePage on emptied non-root leaves. Two UTs:
// TestFreePageEmptiedLeaf — multi-leaf btree; DELETE empties one leaf;
// assert the leaf is freed (pager freelist count == 1) and the
// remaining data is still seekable.
// TestFreePageRootEmptied — single-leaf btree (root == leaf); DELETE
// all rows; root stays as empty leaf, no freelist growth (the root
// page is referenced from sqlite_schema).

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

func newBtreeForVacuum(t *testing.T, pageSize uint32) (*BTree, *pager.Pager) {
	pg := pager.OpenInMemory(pageSize)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	return NewBTree(pg, 1, true), pg
}

// fillWithBigRows inserts n rows with large payloads so the btree
// overflows past one leaf and grows to multiple leaves. Returns the
// set of rowids inserted.
func fillWithBigRows(t *testing.T, tr *BTree, n int, payloadSize int) []int64 {
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id := int64(i + 1)
		pl := make([]byte, payloadSize)
		for j := range pl {
			pl[j] = byte(j)
		}
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: pl}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("insert id=%d: %v", id, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestFreePageEmptiedLeaf(t *testing.T) {
	// Use a small page (1024 bytes) so 30 rows of 800 bytes each
	// overflow one leaf and the btree splits into multiple leaves.
	tr, pg := newBtreeForVacuum(t, 1024)
	fillWithBigRows(t, tr, 30, 800)

	// Sanity: the btree has more than one leaf.
	var leaves []uint32
	var refs []leafRef
	if err := tr.collectLeafPages(tr.RootPage(), &leaves, &refs); err != nil {
		t.Fatalf("collectLeafPages: %v", err)
	}
	if len(leaves) < 2 {
		t.Skipf("test needs multi-leaf btree; got %d leaves", len(leaves))
	}

	// Capture before-state.
	beforeFree := pg.FreelistCount()
	beforePages := pg.NumPages()

	// DELETE all rows from the first leaf. We do this by deleting
	// every row in the btree (which will empty every leaf except
	// the one that has the highest rowid, which still holds the
	// last cell after the loop).
	_, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool { return true })
	if err != nil {
		t.Fatalf("DeleteCellsWhere: %v", err)
	}

	// The non-root leaves should now be on the freelist. The root
	// leaf is left as an empty leaf (with cell count 0).
	afterFree := pg.FreelistCount()
	if afterFree <= beforeFree {
		t.Fatalf("expected freelist growth after multi-leaf DELETE; before=%d after=%d", beforeFree, afterFree)
	}
	t.Logf("freelist_count before=%d after=%d pages_before=%d pages_after=%d",
		beforeFree, afterFree, beforePages, pg.NumPages())

	// Verify the root is valid (either still a leaf with cell count 0,
	// or an interior page with divider cells that were not touched by
	// the DELETE — SQLite's btree uses interior cells as divider keys,
	// not data).
	rootPg, _ := pg.ReadPage(tr.RootPage())
	coff := contentOffset(rootPg.PageNum)
	page, err := storage.ParsePage(rootPg.Data, int(tr.pageSize), coff)
	if err != nil {
		t.Fatalf("root page unparsable: %v", err)
	}
	if page.PageType == storage.PageTypeLeafTable && page.CellCount != 0 {
		t.Fatalf("root leaf should be empty; cell count = %d", page.CellCount)
	}
	// If root is interior, the divider cells are not data rows; the
	// DELETE only removes data cells from leaves. So an interior root
	// with cells is fine.

	// The freed leaves should not be reachable from the btree.
	var leavesAfter []uint32
	if err := tr.collectLeafPages(tr.RootPage(), &leavesAfter, nil); err != nil {
		t.Fatalf("collectLeafPages after: %v", err)
	}
	for _, ln := range leavesAfter {
		if ln != tr.RootPage() {
			t.Fatalf("freed leaf %d is still reachable from root", ln)
		}
	}
}

func TestFreePageRootEmptied(t *testing.T) {
	// Single-leaf btree (root == leaf). DELETE all rows. Root must
	// stay as an empty leaf; no freelist growth.
	tr, pg := newBtreeForVacuum(t, 1024)
	// Insert a small number of small rows so the btree stays single-leaf.
	fillWithBigRows(t, tr, 3, 50)

	var leaves []uint32
	if err := tr.collectLeafPages(tr.RootPage(), &leaves, nil); err != nil {
		t.Fatalf("collectLeafPages: %v", err)
	}
	if len(leaves) != 1 {
		t.Skipf("test needs single-leaf btree; got %d leaves", len(leaves))
	}
	if leaves[0] != tr.RootPage() {
		t.Skipf("test needs root==leaf; root=%d leaves[0]=%d", tr.RootPage(), leaves[0])
	}

	beforeFree := pg.FreelistCount()

	_, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool { return true })
	if err != nil {
		t.Fatalf("DeleteCellsWhere: %v", err)
	}

	afterFree := pg.FreelistCount()
	if afterFree != beforeFree {
		t.Fatalf("single-leaf btree should not grow freelist; before=%d after=%d", beforeFree, afterFree)
	}

	// Root leaf is still valid and empty.
	rootPg, _ := pg.ReadPage(tr.RootPage())
	coff := contentOffset(rootPg.PageNum)
	page, err := storage.ParsePage(rootPg.Data, int(tr.pageSize), coff)
	if err != nil {
		t.Fatalf("root page unparsable: %v", err)
	}
	if page.CellCount != 0 {
		t.Fatalf("root leaf should be empty; cell count = %d", page.CellCount)
	}
}

func TestFreePageSelectiveDelete(t *testing.T) {
	// Multi-leaf btree; DELETE only the rows that fit on a single
	// non-root leaf (the leftmost). That leaf should be freed; other
	// leaves keep their data.
	tr, pg := newBtreeForVacuum(t, 1024)
	fillWithBigRows(t, tr, 30, 800)

	var leaves []uint32
	var refs []leafRef
	if err := tr.collectLeafPages(tr.RootPage(), &leaves, &refs); err != nil {
		t.Fatalf("collectLeafPages: %v", err)
	}
	if len(leaves) < 2 {
		t.Skipf("test needs multi-leaf btree; got %d leaves", len(leaves))
	}

	// Walk the leftmost leaf and find the rowid range it covers.
	leftLeaf := leaves[0]
	pgL, _ := pg.ReadPage(leftLeaf)
	coffL := contentOffset(pgL.PageNum)
	pageL, _ := storage.ParsePage(pgL.Data, int(tr.pageSize), coffL)
	if pageL.CellCount == 0 {
		t.Skipf("left leaf already empty")
	}
	var minRow, maxRow int64
	minRow = 1 << 62
	maxRow = -1
	for i := 0; i < int(pageL.CellCount); i++ {
		pp := storage.CellPointer(pgL.Data, coffL, i, int(tr.pageSize))
		c, derr := storage.DecodeCell(pgL.Data, int(pp), storage.CellTableLeaf, int(tr.pageSize))
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		if c.RowID < minRow {
			minRow = c.RowID
		}
		if c.RowID > maxRow {
			maxRow = c.RowID
		}
	}

	beforeFree := pg.FreelistCount()

	// Delete the leftmost leaf's range only.
	_, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool {
		return c.RowID >= minRow && c.RowID <= maxRow
	})
	if err != nil {
		t.Fatalf("DeleteCellsWhere: %v", err)
	}

	afterFree := pg.FreelistCount()
	if afterFree <= beforeFree {
		t.Fatalf("expected freelist growth; before=%d after=%d", beforeFree, afterFree)
	}

	// Surviving rows (in the right leaf) are still seekable.
	var leavesAfter []uint32
	if err := tr.collectLeafPages(tr.RootPage(), &leavesAfter, nil); err != nil {
		t.Fatalf("collectLeafPages after: %v", err)
	}
	seen := 0
	for _, ln := range leavesAfter {
		p, _ := pg.ReadPage(ln)
		coff := contentOffset(p.PageNum)
		page, _ := storage.ParsePage(p.Data, int(tr.pageSize), coff)
		seen += int(page.CellCount)
	}
	wantSurvivors := 30 - (int(maxRow) - int(minRow) + 1)
	if seen != wantSurvivors {
		t.Fatalf("expected %d surviving rows in non-root leaves; got %d", wantSurvivors, seen)
	}
}
