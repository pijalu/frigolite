// Targeted tests for the btree rebalance gap. Each test fails on
// the current engine and pins down one rebalance requirement; the
// port of btree.c::balance_nonroot (and balance_deeper /
// copyNodeContent / pageAllocate) must make these pass.
//
// These mirror SQLite's test_btree.c btree_balance() scenarios at a
// smaller scale: a few pages, deliberate splits, then deletes that
// empty leaves, and check that the live row set is preserved
// (walkable + seekable) and the empty leaves are coalesced/freed.

package btree

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// makeReBalanceHarness is a small wrapper around the full
// btreeHarness: it inserts N rows then deletes all but 5 to force
// the leaf pages to become mostly empty.
func rebalanceHarness(t *testing.T, n int) *btreeHarness {
	t.Helper()
	h := newBtreeHarness(t)
	for i := 0; i < n; i++ {
		h.insert()
	}
	return h
}

// leafCount returns the number of leaf pages reachable from the
// current root.
func leafCount(h *btreeHarness) int {
	var leaves []uint32
	_ = h.tr.collectLeafPages(h.tr.RootPage(), &leaves, nil)
	return len(leaves)
}

// TestRebalanceDeleteMost: after inserting 200 rows on 1024-byte
// pages (forces multiple leaves and several levels), delete all but
// the last 5. The engine must:
//  1. still walk all 5 surviving rowids in order;
//  2. have no orphaned cells on unreachable leaves;
//  3. (rebalance) have removed the now-empty leaves from the tree.
//
// In SQLite, this exercises sqlite3BtreeDelete -> balance_nonroot
// which moves cells from a mostly-empty leaf into a sibling then
// drops the empty page from the parent. Without it, the empty
// leaves stay in the tree (cellCount=0) and the parent interior
// pages still reference them.
func TestRebalanceDeleteMost(t *testing.T) {
	h := rebalanceHarness(t, 200)
	leavesBefore := leafCount(h)
	for id := int64(1); id <= 195; id++ {
		if _, err := h.tr.DeleteCellsWhere(func(c *storage.Cell) bool {
			return c.RowID == id
		}); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
		delete(h.live, id)
	}
	h.verify("after delete 1..195 of 200")
	leavesAfter := leafCount(h)
	// Surviving 5 rows should fit on a single leaf (or two if
	// payloads are large). Without rebalance, every original leaf
	// remains in the tree, so leavesAfter ~= leavesBefore.
	if leavesAfter >= leavesBefore {
		t.Errorf("rebalance gap: %d leaves before, %d after deleting 195/200; engine did not coalesce empty leaves", leavesBefore, leavesAfter)
	}
}

// TestRebalanceDeleteRange: insert 200 rows then delete a contiguous
// range in the middle (10..190). The remaining must be 1..9 and
// 191..200, in order, all seekable. This stresses the case where a
// leaf becomes empty in the middle of the tree and balance_nonroot
// must walk past it to find live siblings.
func TestRebalanceDeleteRange(t *testing.T) {
	h := rebalanceHarness(t, 200)
	leavesBefore := leafCount(h)
	for id := int64(10); id <= 190; id++ {
		if _, err := h.tr.DeleteCellsWhere(func(c *storage.Cell) bool {
			return c.RowID == id
		}); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
		delete(h.live, id)
	}
	h.verify("after delete 10..190 of 200")
	leavesAfter := leafCount(h)
	if leavesAfter >= leavesBefore {
		t.Errorf("rebalance gap: %d leaves before, %d after deleting 181/200; engine did not coalesce empty middle leaves", leavesBefore, leavesAfter)
	}
}

// TestRebalanceAlternating: insert 200 rows, then delete every other
// one (1,3,5,...,199). Exercises the "cells need to compact within
// the leaf" path; if the engine merely decrements cellCount without
// repacking, the leftover space cannot be reused and a follow-up
// insert should spill unnecessarily. With rebalance, leaves that
// fall below half-full should be coalesced with siblings.
func TestRebalanceAlternating(t *testing.T) {
	h := rebalanceHarness(t, 200)
	leavesBefore := leafCount(h)
	for id := int64(1); id <= 199; id += 2 {
		if _, err := h.tr.DeleteCellsWhere(func(c *storage.Cell) bool {
			return c.RowID == id
		}); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
		delete(h.live, id)
	}
	h.verify("after alternating delete of 200")
	leavesAfter := leafCount(h)
	if leavesAfter >= leavesBefore {
		t.Errorf("rebalance gap: %d leaves before, %d after alternating delete of 199; engine did not coalesce half-empty leaves", leavesBefore, leavesAfter)
	}
}

// TestRebalanceInsertAfterBulkDelete: insert 200, delete 180, then
// insert 100 more. The engine must reuse the freed space (cells and
// possibly pages) rather than always allocating new pages. This
// stresses the boundary between DELETE's rebalance and INSERT's
// split.
func TestRebalanceInsertAfterBulkDelete(t *testing.T) {
	h := rebalanceHarness(t, 200)
	leavesBefore := leafCount(h)
	for id := int64(1); id <= 180; id++ {
		if _, err := h.tr.DeleteCellsWhere(func(c *storage.Cell) bool {
			return c.RowID == id
		}); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
		delete(h.live, id)
	}
	h.verify("after delete 1..180 of 200")
	leavesMid := leafCount(h)
	for i := 0; i < 100; i++ {
		h.insert()
	}
	h.verify("after re-insert 100 more")
	leavesAfter := leafCount(h)
	if leavesMid >= leavesBefore {
		t.Errorf("rebalance gap after bulk delete: %d leaves before, %d after deleting 180/200; engine did not coalesce", leavesBefore, leavesMid)
	}
	// P8.INCRVACUUM T5b: the original "leavesAfter <= leavesMid*2" bound
	// was arithmetically impossible AND un-SQLite. Oracle measurement
	// (sqlite3 3.51.0, same deterministic workload: page_size=1024,
	// 200 rows of 100+(id*7)%1200 bytes, DELETE rowid 1..180, INSERT
	// 201..300): SQLite itself produces exactly **91 leaf pages** —
	// identical to this engine — and its layout has the same local
	// slack this test's old invariants condemned: page 3 holds 2 cells
	// with 657 unused bytes while the next leaf's first cell (192B)
	// would fit, plus dozens of 1-cell pages with 300-500B spare
	// (dbstat('main')). SQLite's balance_nonroot is window-local and
	// deliberately does NOT globally greedy-pack after bulk deletes, and
	// its appends even allocate past the old high-water mark (max
	// pageno 153). Therefore pin the emergent density metric to the
	// oracle count (10% slack for legitimate internal divergence) and
	// the no-regression guard; do NOT pin per-leaf fill or page-number
	// reuse — the oracle violates both.
	if leavesAfter > leavesBefore {
		t.Errorf("rebalance gap on re-insert: %d leaves after re-insert vs %d for the original 200-row layout; split packing regressed (degenerate ~1-row pages)", leavesAfter, leavesBefore)
	}
	oracleLeaves := 91 // sqlite3 3.51.0 dbstat measurement, see above
	if leavesAfter > oracleLeaves+oracleLeaves/10 {
		t.Errorf("density gap on re-insert: %d leaves vs oracle %d (+10%%); split packing is sparser than SQLite's", leavesAfter, oracleLeaves)
	}
	_ = fmt.Sprint // keep fmt import
}

// TestRebalanceFreePage: a sanity test for the post-delete free
// page state. After deleting all rows from a table that spanned
// many pages, the engine should have returned those pages to the
// on-disk freelist. Without rebalance, the empty leaves are never
// freed and the freelist count stays at 0 even though many pages
// are unused.
func TestRebalanceFreePage(t *testing.T) {
	h := rebalanceHarness(t, 200) // forces many leaves
	beforeFree := freelistCount(t, h)
	for id := int64(1); id <= 200; id++ {
		if _, err := h.tr.DeleteCellsWhere(func(c *storage.Cell) bool {
			return c.RowID == id
		}); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
		delete(h.live, id)
	}
	afterFree := freelistCount(t, h)
	h.verify("after deleting all 200")
	if afterFree <= beforeFree {
		t.Errorf("rebalance gap: freelist count before=%d after=%d; engine did not return empty leaves to the freelist", beforeFree, afterFree)
	}
	_ = pager.OpenInMemory // keep pager import
}

// freelistCount reads the freelist count from the on-disk header
// (offset 36-40). Works for any pager state.
func freelistCount(t *testing.T, h *btreeHarness) uint32 {
	t.Helper()
	hdr := h.tr.pager.Header()
	return uint32(hdr[36])<<24 | uint32(hdr[37])<<16 | uint32(hdr[38])<<8 | uint32(hdr[39])
}
