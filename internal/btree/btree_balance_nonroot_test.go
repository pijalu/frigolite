// Focused UTs for the btree.c rebalance port (P8.INCRVACUUM phase 5.5).
// Each test exercises one piece of the btree.c machinery — the same
// pieces that autovacuum/incrvacuum/incrvacuum2 testgen packages need.

package btree

import (
	"encoding/binary"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// TestBalanceNonroot_MergeEmptyLeaf: build a 2-leaf tree with an
// empty leaf on the left and a full leaf on the right, run
// balanceNonroot, and verify the empty leaf is freed and the
// remaining cells are reachable. This is the autovacuum/incrvacuum
// "after-delete-vacuum" scenario: the rightmost child of the
// parent is empty, and balance should coalesce it.
func TestBalanceNonroot_MergeEmptyLeaf(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	// Page 1: parent (interior table, root).
	pg.AllocatePage()
	// Page 2: left sibling (empty leaf after delete).
	pg.AllocatePage()
	// Page 3: right sibling (the page being balanced, full).
	pg.AllocatePage()
	bt := NewBTree(pg, 1, true)

	// Build parent with 1 cell pointing to page 2 (left sibling)
	// and rightmost-child = page 3 (the page being balanced).
	parentPg, _ := pg.ReadPage(1)
	pcoff := contentOffset(1)
	parentPg.Data[pcoff+0] = storage.PageTypeInteriorTable
	binary.BigEndian.PutUint16(parentPg.Data[pcoff+3:pcoff+5], 1)  // 1 cell
	binary.BigEndian.PutUint32(parentPg.Data[pcoff+8:pcoff+12], 3) // rightmost = page 3
	// Cell: 4-byte child=2 + varint rowid=5 (divider for right sibling).
	const dividerStart = 1024 - 5
	binary.BigEndian.PutUint32(parentPg.Data[dividerStart:dividerStart+4], 2)
	parentPg.Data[dividerStart+4] = 5
	binary.BigEndian.PutUint16(parentPg.Data[pcoff+12:pcoff+14], uint16(dividerStart))
	binary.BigEndian.PutUint16(parentPg.Data[pcoff+5:pcoff+7], uint16(dividerStart))
	pg.WritePage(parentPg)

	// Build left sibling: empty leaf.
	leftPg, _ := pg.ReadPage(2)
	lcoff := contentOffset(2)
	leftPg.Data[lcoff+0] = storage.PageTypeLeafTable
	binary.BigEndian.PutUint16(leftPg.Data[lcoff+3:lcoff+5], 0)            // 0 cells
	binary.BigEndian.PutUint16(leftPg.Data[lcoff+5:lcoff+7], uint16(1024)) // content = end
	leftPg.Data[lcoff+7] = 0
	pg.WritePage(leftPg)

	// Build right sibling: leaf with 3 cells (rowids 6, 7, 8).
	rightPg, _ := pg.ReadPage(3)
	rcoff := contentOffset(3)
	rightPg.Data[rcoff+0] = storage.PageTypeLeafTable
	rightCell6 := buildTableLeafCell(t, 6, []byte("a"), 0)
	rightCell7 := buildTableLeafCell(t, 7, []byte("bb"), 0)
	rightCell8 := buildTableLeafCell(t, 8, []byte("ccc"), 0)
	// Engine cell layout: cells grow downward from the end of the usable
	// area and the pointer array is ordered by ASCENDING rowid — cell 0
	// (lowest rowid) at the highest address. Cursor seeks binary-search
	// the pointer array in index order, so the array must be sorted by
	// rowid (the fixture previously stored rowids descending, which no
	// real engine-written page does).
	pos := 1024 - 4
	pos -= len(rightCell6)
	copy(rightPg.Data[pos:pos+len(rightCell6)], rightCell6)
	ptrCell0 := pos
	pos -= len(rightCell7)
	copy(rightPg.Data[pos:pos+len(rightCell7)], rightCell7)
	ptrCell1 := pos
	pos -= len(rightCell8)
	copy(rightPg.Data[pos:pos+len(rightCell8)], rightCell8)
	ptrCell2 := pos
	binary.BigEndian.PutUint16(rightPg.Data[rcoff+8:rcoff+10], uint16(ptrCell0))
	binary.BigEndian.PutUint16(rightPg.Data[rcoff+10:rcoff+12], uint16(ptrCell1))
	binary.BigEndian.PutUint16(rightPg.Data[rcoff+12:rcoff+14], uint16(ptrCell2))
	binary.BigEndian.PutUint16(rightPg.Data[rcoff+3:rcoff+5], 3)
	binary.BigEndian.PutUint16(rightPg.Data[rcoff+5:rcoff+7], uint16(pos))
	rightPg.Data[rcoff+7] = 0
	pg.WritePage(rightPg)

	// Re-read everything (pager may have invalidated buffers).
	parentPg, _ = pg.ReadPage(1)
	leftPg, _ = pg.ReadPage(2)
	rightPg, _ = pg.ReadPage(3)

	// Run balanceNonroot with iParentIdx = -1 (rightmost-child).
	ctx := &balanceNonrootContext{
		parent:     parentPg,
		iParentIdx: -1,
		page:       rightPg,
	}
	_, err := bt.balanceNonroot(ctx)
	if err != nil {
		t.Fatalf("balanceNonroot: %v", err)
	}

	// After balance: SQLite's positional page reuse (apNew[i] = apOld[i],
	// src/btree.c:8617) keeps the LEFTMOST gathered page as the sole
	// survivor and frees the surplus HIGHEST-numbered page
	// (freePage(apOld[nNew..nOld)), src/btree.c:8960). Page 2 therefore
	// holds all 3 cells and page 3 returns to the freelist. Freeing the
	// rightmost page is what keeps the file truncatable from the right
	// during auto/incremental vacuum (src/btree.c:3822-3984).
	leftPg2, _ := pg.ReadPage(2)
	lp, err := storage.ParsePage(leftPg2.Data, 1024, lcoff)
	if err != nil {
		t.Fatalf("ParsePage survivor (left sibling): %v", err)
	}
	if lp.CellCount != 3 {
		t.Errorf("survivor cell count: got %d, want 3", lp.CellCount)
	}
	// Verify the rowids are present and in ascending order (walk order).
	var got []int64
	for i := uint16(0); i < lp.CellCount; i++ {
		cp := int(storage.CellPointer(leftPg2.Data, lcoff, int(i), 1024))
		c, err := storage.DecodeCell(leftPg2.Data, cp, storage.CellTableLeaf, 1024)
		if err != nil {
			t.Errorf("survivor cell %d: decode: %v", i, err)
			continue
		}
		got = append(got, c.RowID)
	}
	if len(got) != 3 || got[0] != 6 || got[1] != 7 || got[2] != 8 {
		t.Errorf("survivor rowids: got %v, want [6 7 8]", got)
	}
	// The freed page's type byte is its freelist chain pointer.
	if !pager.IsPageOnFreelist(pg, 3) {
		t.Errorf("page 3 not on freelist after balance (surplus page must be freed)")
	}
	// The parent should now reference only the survivor (rightmost = 2),
	// with no divider cells (a single child needs no divider).
	parentPg2, _ := pg.ReadPage(1)
	pp, err := storage.ParsePage(parentPg2.Data, 1024, pcoff)
	if err != nil {
		t.Fatalf("ParsePage parent: %v", err)
	}
	rmp := pp.RightmostPtr
	if rmp != 2 {
		t.Errorf("parent rightmost: got %d, want 2", rmp)
	}
	if pp.CellCount != 0 {
		t.Errorf("parent cell count: got %d, want 0", pp.CellCount)
	}
}
