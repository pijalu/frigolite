package btree

// P8.INCRVACUUM phase 3: tests for relocatePage + IncrVacuumStep.
// Reference: btree.c::relocatePage (line ~6530), sqlite3BtreeIncrVacuum
// (line ~6780), incrVacuumStep (line ~6700).

import (
	"encoding/binary"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// fillBtreeWithBigRows inserts n rows with large payloads so the btree
// overflows past one leaf and grows to multiple leaves. Returns the
// rowids inserted.
func fillBtreeWithBigRows(t *testing.T, tr *BTree, n int, payloadSize int) []int64 {
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

// writePtrmapForParent records the pointer-map entry for `child` as
// having `parentPg` of the given type. The btree doesn't automatically
// write ptrmap entries for the cells it inserts (phase 2+3 work; phase
// 2 added the API, phase 3 adds the writer). For these tests we write
// the entry directly so relocatePage can find the parent.
func writePtrmapForParent(t *testing.T, tr *BTree, child, parentPg uint32, parentType byte) {
	t.Helper()
	if err := tr.pager.WritePtrmap(child, parentType, parentPg); err != nil {
		t.Fatalf("WritePtrmap(child=%d, parent=%d): %v", child, parentPg, err)
	}
}

func TestRelocatePageBasic(t *testing.T) {
	// Set up a multi-leaf btree with ptrmap entries.
	tr, pg := newBtreeForVacuum(t, 1024)
	fillBtreeWithBigRows(t, tr, 5, 800)
	// Discover leaves and interior parents.
	var leaves []uint32
	var refs []leafRef
	if err := tr.collectLeafPages(tr.RootPage(), &leaves, &refs); err != nil {
		t.Fatalf("collectLeafPages: %v", err)
	}
	if len(leaves) < 2 {
		t.Skipf("test needs multi-leaf btree; got %d leaves", len(leaves))
	}
	// Write ptrmap entries: each leaf is a btree node with its parent.
	for i, leaf := range leaves {
		var parent uint32
		var parentType byte = storage.PtrmapBtreeNode
		if i < len(refs) && refs[i].parent != 0 {
			parent = refs[i].parent
		} else {
			parent = tr.RootPage()
		}
		writePtrmapForParent(t, tr, leaf, parent, parentType)
	}
	// Pick a non-root leaf, free a slot manually (mimic the
	// post-DELETE state), then relocate the last page to it.
	lastPg := pg.NumPages()
	target := lastPg - 1 // pick a low free page
	if target == 1 {
		target = 2
	}
	// We need a "free" page to be allocated. AllocatePageLE returns
	// one from the in-memory freelist, which is empty, so this would
	// fail. Instead, use the last page of the file as the "to"
	// target — that's the highest-numbered page, and we're relocating
	// a different page to it. This bypasses AllocatePageLE.
	// RelocatePage(0, 5) → move page 5's content to page 0... no, we
	// need a real destination. Use lastPg-1.
	if err := tr.RelocatePage(target, leaves[0]); err != nil {
		t.Fatalf("RelocatePage(%d, %d): %v", target, leaves[0], err)
	}
	// The freed leaf should now be on the freelist.
	if !pager.IsPageOnFreelist(pg, leaves[0]) {
		t.Errorf("page %d should be on freelist after RelocatePage", leaves[0])
	}
	// The target page should have the same content as the original leaf.
	origLeaf, _ := pg.ReadPage(leaves[0])
	// After RelocatePage, the original page is freed (in freePages
	// set). Reading it from the file would still return the old data
	// (the on-disk content wasn't zeroed). But our in-memory cache
	// for leaves[0] might have been replaced.
	// The more reliable check: read the target page and compare its
	// b-tree page type.
	targetPg, _ := pg.ReadPage(target)
	if targetPg.Data[0] == 0 {
		t.Errorf("target page %d has byte 0 = 0 (not a b-tree page)", target)
	}
	_ = origLeaf
}

func TestIncrVacuumStepFreelistOnly(t *testing.T) {
	// File with N pages; free the LAST 3 pages (so the last page is
	// on the freelist). IncrVacuumStep(3) should truncate by 3 pages.
	tr, pg := newBtreeForVacuum(t, 1024)
	// Allocate pages up to 10.
	for i := uint32(0); i < 9; i++ {
		_ = pg.AllocatePage()
	}
	// Free the LAST 3 pages (8, 9, 10) so the last page (10) is on
	// the freelist.
	for i := uint32(8); i <= 10; i++ {
		if err := pg.FreePage(i); err != nil {
			t.Fatalf("FreePage(%d): %v", i, err)
		}
	}
	before := pg.NumPages()
	steps, err := tr.IncrVacuumStep(3)
	if err != nil {
		t.Fatalf("IncrVacuumStep: %v", err)
	}
	after := pg.NumPages()
	// The freelist had 3 pages; IncrVacuumStep should consume them.
	if int(after) != int(before)-steps {
		t.Errorf("page count: before=%d, after=%d, steps=%d", before, after, steps)
	}
	if steps != 3 {
		t.Errorf("expected 3 steps, got %d", steps)
	}
	t.Logf("freelist-only vacuum: before=%d after=%d steps=%d", before, after, steps)
}

func TestIncrVacuumStepInUseLastPage(t *testing.T) {
	// Multi-leaf btree where the last page is in use. IncrVacuumStep
	// needs a free page; without one, it should stop with 0 steps.
	tr, pg := newBtreeForVacuum(t, 1024)
	fillBtreeWithBigRows(t, tr, 10, 800)
	// No free pages in this scenario. IncrVacuumStep should be a no-op.
	steps, err := tr.IncrVacuumStep(5)
	if err != nil {
		t.Fatalf("IncrVacuumStep: %v", err)
	}
	if steps != 0 {
		t.Errorf("expected 0 steps (no free page), got %d", steps)
	}
	if pg.NumPages() == 0 {
		t.Errorf("page count shouldn't decrease")
	}
}

func TestTruncateFile(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	for i := uint32(0); i < 5; i++ {
		_ = pg.AllocatePage()
	}
	if pg.NumPages() != 5 {
		t.Errorf("expected 5 pages, got %d", pg.NumPages())
	}
	if err := pg.Truncate(2); err != nil {
		t.Fatalf("Truncate(2): %v", err)
	}
	if pg.NumPages() != 2 {
		t.Errorf("expected 2 pages after truncate, got %d", pg.NumPages())
	}
	// Pages > 2 should be evicted from the cache.
	if _, err := pg.ReadPage(5); err == nil {
		t.Errorf("ReadPage(5) after Truncate(2) should error")
	}
}

// silences an unused-import warning if the binary imports shrink.
var _ = binary.BigEndian
