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
	rightPg, _ = pg.ReadPage(3)

	// Run balanceNonroot with iParentIdx = -1 (rightmost-child). The
	// parent IS the btree root here, matching maybeRebalanceAfterDelete's
	// isRoot plumbing.
	ctx := &balanceNonrootContext{
		parent:     parentPg,
		iParentIdx: -1,
		page:       rightPg,
		isRoot:     true,
	}
	_, err := bt.balanceNonroot(ctx)
	if err != nil {
		t.Fatalf("balanceNonroot: %v", err)
	}

	// After balance: the redistribution packs all 3 cells onto the
	// leftmost gathered page (apNew[0] = apOld[0], src/btree.c:8617) and
	// frees the surplus highest-numbered page (freePage(apOld[nNew..nOld)),
	// src/btree.c:8960). The parent (the btree ROOT) is then left with 0
	// dividers over that single child, which trips the balance_shallower
	// absorption (isRoot && pParent->nCell==0, src/btree.c:8918-8943): the
	// child's content is copied INTO the root — the root keeps its page
	// number — and the child is freed. Page 1 therefore ends up the leaf
	// holding rows 6/7/8 and BOTH children (2 and 3) return to the
	// freelist. Freeing the highest-numbered pages is what keeps the file
	// truncatable from the right during auto/incremental vacuum.
	rootPg2, _ := pg.ReadPage(1)
	rp, err := storage.ParsePage(rootPg2.Data, 1024, pcoff)
	if err != nil {
		t.Fatalf("ParsePage absorbed root: %v", err)
	}
	if rp.PageType != storage.PageTypeLeafTable {
		t.Errorf("absorbed root type: got %#02x, want leaf table", rp.PageType)
	}
	if rp.CellCount != 3 {
		t.Errorf("absorbed root cell count: got %d, want 3", rp.CellCount)
	}
	// Verify the rowids are present and in ascending order (walk order).
	var got []int64
	for i := uint16(0); i < rp.CellCount; i++ {
		cp := int(storage.CellPointer(rootPg2.Data, pcoff, int(i), 1024))
		c, err := storage.DecodeCell(rootPg2.Data, cp, storage.CellTableLeaf, 1024)
		if err != nil {
			t.Errorf("absorbed root cell %d: decode: %v", i, err)
			continue
		}
		got = append(got, c.RowID)
	}
	if len(got) != 3 || got[0] != 6 || got[1] != 7 || got[2] != 8 {
		t.Errorf("absorbed root rowids: got %v, want [6 7 8]", got)
	}
	// The emptied children return to the freelist.
	if !pager.IsPageOnFreelist(pg, 2) {
		t.Errorf("page 2 not on freelist after absorption")
	}
	if !pager.IsPageOnFreelist(pg, 3) {
		t.Errorf("page 3 not on freelist after balance (surplus page must be freed)")
	}
}
