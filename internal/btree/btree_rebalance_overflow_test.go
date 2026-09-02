// Copyright 2026 Frigolite Authors.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// btree_rebalance_test.go: btree rebalance correctness tests.
//
// These tests verify that the btree rebalance (balanceNonroot)
// correctly preserves all cells when:
//   - a cell's first byte is 0x80 (varint encoding of payload
//     sizes 128, 256, 384, ..., 896 on a 1024-byte page).
//   - the cell pointer array is NOT in monotonic address order
//     (e.g. cells are in freeblocks after a defragment).
//
// Regression test for the P8.INCRVACUUM phase10 cell-loss bug:
//   `balanceNonroot`'s Phase 2 used the previous cell's pointer
//   as the cell-end boundary, which is wrong when cells are in
//   freeblock regions.

package btree

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// makePayload returns a payload of the given size filled with a
// deterministic pattern derived from id.
func makePayload(id int64, size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(id + int64(i))
	}
	return payload
}

// TestRebalancePreservesOverflowBoundaryCell verifies that cells
// whose payload size results in a first byte of 0x80 are preserved
// across a rebalance. Payload sizes 128, 256, 384, 512, 640, 768,
// 896 all encode as 2-byte varints starting with 0x80. The previous
// bca.addCell filter dropped these cells (a misguided attempt to
// filter "corrupt" cells with 0x80 first bytes).
func TestRebalancePreservesOverflowBoundaryCell(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	tr := NewBTree(pg, 1, true)

	// Insert 5 rows with payload sizes that all encode varints
	// starting with 0x80. The first byte 0x80 was previously
	// filtered out by bca.addCell, losing these cells.
	for _, id := range []int64{1, 2, 3, 4, 5} {
		size := 128 * int(id) // 128, 256, 384, 512, 640
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: makePayload(id, size)}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}
	// Delete id 1 to make room for rebalance.
	if _, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool {
		return c.RowID == 1
	}); err != nil {
		t.Fatalf("delete 1: %v", err)
	}
	// All remaining rows must be reachable.
	for _, id := range []int64{2, 3, 4, 5} {
		cur, _ := tr.OpenCursor()
		found, serr := cur.SeekToRowID(id)
		if serr != nil || !found {
			t.Errorf("after delete 1: key %d not found (found=%v err=%v)", id, found, serr)
		}
	}
}

// TestRebalancePreservesFreeblockOrderCells verifies that cells
// in a page whose cell pointer array is not in monotonic address
// order (because they live in freeblock regions after partial
// defragments) are preserved across a rebalance. The previous
// implementation read cell bytes as [cp[i], cp[i-1]) assuming
// decreasing address order, which corrupted cells when cp[] was
// out of order.
func TestRebalancePreservesFreeblockOrderCells(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	tr := NewBTree(pg, 1, true)

	// Insert 20 rows; payload ~100 bytes (fits in 1024-page local,
	// no overflow).
	live := map[int64]bool{}
	for id := int64(1); id <= 20; id++ {
		payload := makePayload(id, 100)
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: payload}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
		live[id] = true
	}
	// Delete in interleaved order to fragment the cell pointer
	// array / freeblocks.
	for _, id := range []int64{3, 7, 11, 15, 19, 1, 5, 9, 13, 17} {
		if _, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool {
			return c.RowID == id
		}); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
		delete(live, id)
	}
	// Insert a few more to force defragment/rebalance.
	for id := int64(21); id <= 30; id++ {
		payload := makePayload(id, 100)
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: payload}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
		live[id] = true
	}
	// Delete again to force another rebalance.
	for _, id := range []int64{21, 23, 25} {
		if _, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool {
			return c.RowID == id
		}); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
		delete(live, id)
	}
	// Verify all live keys are reachable.
	for id := range live {
		cur, _ := tr.OpenCursor()
		found, serr := cur.SeekToRowID(id)
		
		if serr != nil || !found {
			t.Errorf("key %d missing after fragment+rebalance (found=%v err=%v)", id, found, serr)
		}
	}
}

// TestRebalancePreservesMixedPayloadSizes verifies that cells
// with different payload sizes (some in page, some overflow) are
// preserved across a rebalance. The previous cell-size-from-next-
// pointer trick would corrupt cells whose neighbors had different
// sizes.
func TestRebalancePreservesMixedPayloadSizes(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	tr := NewBTree(pg, 1, true)

	// Mix of small, medium, and large (overflow) payloads.
	specs := []struct {
		id   int64
		size int
	}{
		{1, 50}, {2, 200}, {3, 500}, {4, 800}, {5, 1500}, // 5 overflows
		{6, 100}, {7, 300}, {8, 700}, {9, 1200}, {10, 200},
	}
	live := map[int64]bool{}
	for _, s := range specs {
		payload := makePayload(s.id, s.size)
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: s.id, Payload: payload}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("insert %d: %v", s.id, err)
		}
		live[s.id] = true
	}
	// Force rebalance via delete+reinsert.
	if _, err := tr.DeleteCellsWhere(func(c *storage.Cell) bool {
		return c.RowID == 3 || c.RowID == 7
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	delete(live, 3)
	delete(live, 7)
	for id := int64(11); id <= 15; id++ {
		payload := makePayload(id, 250)
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: payload}
		if err := tr.InsertCell(cell); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
		live[id] = true
	}
	// Verify.
	for id := range live {
		cur, _ := tr.OpenCursor()
		found, serr := cur.SeekToRowID(id)
		
		if serr != nil || !found {
			t.Errorf("key %d missing (found=%v err=%v)", id, found, serr)
		}
	}
	_ = fmt.Sprintf
}
