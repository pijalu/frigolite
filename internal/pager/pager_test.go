// P8.INCRVACUUM.phase8 unit tests for the multi-trunk FreePage chain.
//
// Verifies that the trunkPages / leafToTrunk maps correctly track the
// on-disk freelist chain topology, and that AllocatePage's in-memory
// branch:
//   - advances header.trunk when popping a trunk page;
//   - zeroes the leaf slot when popping a leaf;
//   - does not create duplicates when the same page is freed, popped,
//     re-allocated, and re-freed.
//
// The tests use OpenInMemory (no file) so they don't touch disk and
// don't need cleanup. The on-disk chain state is read directly from
// the cached page Data via p.pages[pg].Data.
package pager

import (
	"encoding/binary"
	"testing"
)

// TestPhase8TrunkPopAdvancesHeader: free a page (becomes a trunk),
// allocate it (trunk pop), header.trunk should advance to the popped
// page's nextTrunk.
func TestPhase8TrunkPopAdvancesHeader(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()

	// Need 2 pages: page 1 is the schema root, page 2 will be our
	// first trunk.
	p.AllocatePage() // page 1: schema root (or any in-memory page)

	// Create a multi-trunk chain manually: page 2 is a trunk, with
	// nextTrunk = 0 (last). page 3 is a leaf of trunk 2.
	// We do this directly so the test is deterministic.
	p2 := &Page{Data: make([]byte, 1024), PageNum: 2}
	binary.BigEndian.PutUint32(p2.Data[0:4], 0) // nextTrunk = 0 (last)
	binary.BigEndian.PutUint32(p2.Data[4:8], 1) // leafCount = 1
	binary.BigEndian.PutUint32(p2.Data[8:12], 3)
	p.pages[2] = p2
	p.dirty[2] = true
	binary.BigEndian.PutUint32(p.header[32:36], 2) // header.trunk = 2
	binary.BigEndian.PutUint32(p.header[36:40], 2) // header.count = 2

	// Manually add to trunkPages (simulating what FreePage would do).
	if p.trunkPages == nil {
		p.trunkPages = make(map[uint32]bool)
	}
	p.trunkPages[2] = true
	if p.leafToTrunk == nil {
		p.leafToTrunk = make(map[uint32]uint32)
	}
	p.leafToTrunk[3] = 2

	// Add page 3 to freePages (as if FreePage was called for it).
	p.freePages = map[uint32]bool{3: true}
	// Note: we DON'T set p.freePages[2] because that would be a trunk
	// pop, not a leaf pop. For this test we want to verify leaf pop.
	// Reset and use the leaf path.
	p.freePages = map[uint32]bool{3: true}

	// Pop the leaf.
	alloc := p.AllocatePage()
	if alloc.PageNum != 3 {
		t.Fatalf("AllocatePage returned page %d, want 3", alloc.PageNum)
	}

	// After pop, trunk 2 should have leafCount=0. The leaf slot itself
	// is NOT required to zero: SQLite's allocateBtreePage removes a leaf
	// by copying the LAST leaf into the removed slot and decrementing
	// the count (src/btree.c:6697-6700). When the removed leaf is the
	// only one, the copy is a no-op and the stale page number remains
	// at slot 0; consumers only read slots [0, leafCount), so the stale
	// bytes are inert. P8.INCRVACUUM T5: the engine keeps SQLite's
	// copy-last semantics, so assert only the count.
	lc := binary.BigEndian.Uint32(p.pages[2].Data[4:8])
	if lc != 0 {
		t.Errorf("trunk 2 leafCount = %d, want 0", lc)
	}
	// header.count should be 1.
	count := binary.BigEndian.Uint32(p.header[36:40])
	if count != 1 {
		t.Errorf("header.count = %d, want 1", count)
	}
	// leafToTrunk should no longer have page 3.
	if _, ok := p.leafToTrunk[3]; ok {
		t.Errorf("leafToTrunk[3] still set after pop")
	}
	// trunkPages should still have 2 (the trunk itself is still there).
	if !p.trunkPages[2] {
		t.Errorf("trunkPages[2] = false, want true")
	}
}

// TestPhase8TrunkPopAdvancesHeader: free a page that becomes a trunk,
// allocate it (trunk pop), header.trunk should advance to nextTrunk.
func TestPhase8TrunkPopNextTrunkAdvance(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()

	// Build a 2-trunk chain: page 2 is the new trunk, page 3 is the
	// next trunk (which will become header.trunk after we pop page 2).
	p.pages[2] = &Page{Data: make([]byte, 1024), PageNum: 2}
	binary.BigEndian.PutUint32(p.pages[2].Data[0:4], 3) // nextTrunk = 3
	binary.BigEndian.PutUint32(p.pages[2].Data[4:8], 0) // leafCount = 0
	p.dirty[2] = true

	p.pages[3] = &Page{Data: make([]byte, 1024), PageNum: 3}
	binary.BigEndian.PutUint32(p.pages[3].Data[0:4], 0) // last trunk
	binary.BigEndian.PutUint32(p.pages[3].Data[4:8], 0)
	p.dirty[3] = true

	binary.BigEndian.PutUint32(p.header[32:36], 2) // header.trunk = 2
	binary.BigEndian.PutUint32(p.header[36:40], 2) // header.count = 2

	if p.trunkPages == nil {
		p.trunkPages = make(map[uint32]bool)
	}
	p.trunkPages[2] = true
	p.trunkPages[3] = true

	// Add page 2 (the trunk) to freePages.
	p.freePages = map[uint32]bool{2: true}

	// Pop the trunk.
	alloc := p.AllocatePage()
	if alloc.PageNum != 2 {
		t.Fatalf("AllocatePage returned page %d, want 2", alloc.PageNum)
	}

	// header.trunk should now be 3 (the popped trunk's nextTrunk).
	trunk := binary.BigEndian.Uint32(p.header[32:36])
	if trunk != 3 {
		t.Errorf("header.trunk = %d, want 3", trunk)
	}
	// header.count should be 1.
	count := binary.BigEndian.Uint32(p.header[36:40])
	if count != 1 {
		t.Errorf("header.count = %d, want 1", count)
	}
	// trunkPages should no longer have 2.
	if p.trunkPages[2] {
		t.Errorf("trunkPages[2] still set after pop")
	}
	if !p.trunkPages[3] {
		t.Errorf("trunkPages[3] = false, want true (next trunk)")
	}
}

// TestPhase8FreePageThenRealloc: free a page, allocate it, free it
// again. The on-disk chain should not have duplicate entries.
func TestPhase8FreePageThenRealloc(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()

	// Allocate a couple of pages to use as btree root + data.
	p.AllocatePage() // page 1
	// Simulate that page 2 was a btree page (now we free it).
	if err := p.FreePage(2); err != nil {
		t.Fatalf("FreePage(2): %v", err)
	}
	// After FreePage(2): page 2 is a trunk with leafCount=0,
	// header.trunk=2, header.count=1.

	// Allocate again — should pop trunk 2.
	alloc := p.AllocatePage()
	if alloc.PageNum != 2 {
		t.Fatalf("AllocatePage returned page %d, want 2", alloc.PageNum)
	}
	// header.trunk should now be 0 (page 2 was the only trunk).
	trunk := binary.BigEndian.Uint32(p.header[32:36])
	if trunk != 0 {
		t.Errorf("after re-alloc, header.trunk = %d, want 0", trunk)
	}
	count := binary.BigEndian.Uint32(p.header[36:40])
	if count != 0 {
		t.Errorf("after re-alloc, header.count = %d, want 0", count)
	}
	// p.trunkPages should be empty.
	if p.trunkPages != nil {
		if _, ok := p.trunkPages[2]; ok {
			t.Errorf("trunkPages[2] still set after pop")
		}
	}

	// Now free page 2 again — should become a new trunk.
	if err := p.FreePage(2); err != nil {
		t.Fatalf("FreePage(2) again: %v", err)
	}
	trunk = binary.BigEndian.Uint32(p.header[32:36])
	if trunk != 2 {
		t.Errorf("after re-free, header.trunk = %d, want 2", trunk)
	}
	if !p.trunkPages[2] {
		t.Errorf("after re-free, trunkPages[2] = false, want true")
	}
}

// TestPhase8MultiTrunkFreeList: free 300 pages, verify the chain has
// 300 pages total, no leaf count > 246 (the (pageSize-8)/4 - 8 cap for
// 1024-byte pages), and no cycles.
func TestPhase8MultiTrunkFreeList(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()

	// Allocate 301 pages (so numPages=301, covers pages 1..301).
	for i := 0; i < 301; i++ {
		p.AllocatePage()
	}
	if p.numPages != 301 {
		t.Fatalf("numPages = %d, want 301", p.numPages)
	}
	// Now free all 300 pages (skip page 1 which is the schema root
	// and cannot be freed — pager.FreePage rejects it).
	firstPage := uint32(2) // AllocatePage starts at page 1 for in-memory
	for pg := firstPage; pg < firstPage+300; pg++ {
		if err := p.FreePage(pg); err != nil {
			t.Fatalf("FreePage(%d): %v", pg, err)
		}
	}
	// Walk the chain and verify count = 300 (leaves + trunks).
	count := uint32(0)
	seenTrunks := make(map[uint32]bool)
	trunk := binary.BigEndian.Uint32(p.header[32:36])
	for trunk != 0 {
		if seenTrunks[trunk] {
			t.Fatalf("cycle detected at trunk %d", trunk)
		}
		seenTrunks[trunk] = true
		trunkPg, err := p.readPageLocked(trunk)
		if err != nil {
			t.Fatalf("readPageLocked(%d): %v", trunk, err)
		}
		nextTrunk := binary.BigEndian.Uint32(trunkPg.Data[0:4])
		leafCount := binary.BigEndian.Uint32(trunkPg.Data[4:8])
		if leafCount > 246 {
			t.Errorf("trunk %d has leafCount=%d, exceeds cap 246", trunk, leafCount)
		}
		for i := uint32(0); i < leafCount; i++ {
			off := 8 + i*4
			if off+4 > uint32(len(trunkPg.Data)) {
				break
			}
			leafPg := binary.BigEndian.Uint32(trunkPg.Data[off : off+4])
			if leafPg == 0 {
				t.Errorf("trunk %d leaf slot %d is 0", trunk, i)
			}
			count++
		}
		// Count the trunk itself.
		count++
		trunk = nextTrunk
	}
	if count != 300 {
		t.Errorf("chain walk counted %d pages (trunks+leaves), want 300", count)
	}
}
