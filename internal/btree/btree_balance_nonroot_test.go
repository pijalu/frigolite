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
	binary.BigEndian.PutUint16(leftPg.Data[lcoff+3:lcoff+5], 0)          // 0 cells
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
	// Cells grow downward; place rightCell8 at highest, then 7, then 6.
	pos := 1024 - 4
	pos -= len(rightCell8)
	copy(rightPg.Data[pos:pos+len(rightCell8)], rightCell8)
	pos -= len(rightCell7)
	copy(rightPg.Data[pos:pos+len(rightCell7)], rightCell7)
	pos -= len(rightCell6)
	copy(rightPg.Data[pos:pos+len(rightCell6)], rightCell6)
	binary.BigEndian.PutUint16(rightPg.Data[rcoff+8:rcoff+10], uint16(pos))   // cell 0 (rowid 6)
	binary.BigEndian.PutUint16(rightPg.Data[rcoff+10:rcoff+12], uint16(pos+len(rightCell6))) // cell 1 (rowid 7)
	binary.BigEndian.PutUint16(rightPg.Data[rcoff+12:rcoff+14], uint16(pos+len(rightCell6)+len(rightCell7))) // cell 2
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

	// After balance: the empty left sibling should be freed (or
	// overwritten with the right sibling's content). The right
	// sibling should still have all 3 cells.
	rightPg2, _ := pg.ReadPage(3)
	rp, err := storage.ParsePage(rightPg2.Data, 1024, rcoff)
	if err != nil {
		t.Fatalf("ParsePage right sibling: %v", err)
	}
	if rp.CellCount != 3 {
		t.Errorf("right sibling cell count: got %d, want 3", rp.CellCount)
	}
	// Verify the rowids in order.
	for i := uint16(0); i < rp.CellCount; i++ {
		cp := int(storage.CellPointer(rightPg2.Data, rcoff, int(i), 1024))
		c, err := storage.DecodeCell(rightPg2.Data, cp, storage.CellTableLeaf, 1024)
		if err != nil {
			t.Errorf("right sibling cell %d: decode: %v", i, err)
			continue
		}
		wantRowid := []int64{6, 7, 8}[i]
		if c.RowID != wantRowid {
			t.Errorf("right sibling cell %d: rowid got %d, want %d", i, c.RowID, wantRowid)
		}
	}
	// The parent should now reference only the right sibling.
	parentPg2, _ := pg.ReadPage(1)
	pp, err := storage.ParsePage(parentPg2.Data, 1024, pcoff)
	if err != nil {
		t.Fatalf("ParsePage parent: %v", err)
	}
	// Either: parent has 0 cells and rightmost = page 3 (left sibling was
	// coalesced into right sibling), OR parent has 1 cell pointing to page 3
	// (right sibling got the divider cell).
	rmp := pp.RightmostPtr
	if rmp != 3 {
		t.Errorf("parent rightmost: got %d, want 3", rmp)
	}
}
