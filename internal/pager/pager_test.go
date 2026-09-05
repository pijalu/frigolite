// P8.INCRVACUUM.S6 chain behavior tests.
//
// Verifies the on-disk freelist chain invariants (btree.c freePage2 +
// allocateBtreePage) through the public pager API:
//   - trunk pop advances header.trunk to the popped trunk's next pointer
//   - leaf pop decrements header.count and zeroes the slot
//   - free → alloc → free is idempotent (no duplicates in chain)
//   - 300-page multi-trunk walk: chain count == header.count, leaves
//     per trunk ≤ maxTrunkLeaves, every chain entry reachable from
//     header.trunk walks without cycles
//
// The tests use OpenInMemory (no file) so they don't touch disk and
// don't need cleanup. We first grow numPages via AllocatePage (which
// extends the file by one page each call), then exercise FreePage /
// AllocatePageANY / AllocatePageLE on the allocated pages.
package pager

import (
	"encoding/binary"
	"testing"
)

// chainCountsLocked walks the chain from header[32:36] and returns the
// (trunkCount, leafCount) it sees. Caller holds p.mu.
func chainCountsLocked(p *Pager) (int, int) {
	if p.header == nil || len(p.header) < 40 {
		return 0, 0
	}
	trunk := 0
	leaf := 0
	cur := binary.BigEndian.Uint32(p.header[32:36])
	seen := map[uint32]bool{}
	for cur != 0 {
		if seen[cur] {
			break
		}
		seen[cur] = true
		trunk++
		tr, ok := p.readFreelistTrunkLocked(cur)
		if !ok {
			break
		}
		leaf += len(tr.leaves)
		cur = tr.next
	}
	return trunk, leaf
}

// allocPagesLocked extends the pager by n pages, returning the new
// page numbers. Uses AllocatePageMode(true) (skip freelist) so the
// allocations are deterministic and don't interact with the freelist
// under test.
func allocPagesLocked(p *Pager, n int) []uint32 {
	out := make([]uint32, 0, n)
	for i := 0; i < n; i++ {
		pg := p.AllocatePageMode(true)
		out = append(out, pg.PageNum)
	}
	return out
}

// TestChainTrunkPopAdvancesHeader: allocate page 2 (grows numPages
// past 1), free it (becomes a trunk), allocate it again (trunk pop),
// header.trunk should advance to 0 (the popped trunk's next pointer).
func TestChainTrunkPopAdvancesHeader(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()

	allocPagesLocked(p, 1) // grows numPages to 1, returns page 1
	allocPagesLocked(p, 1) // grows numPages to 2, returns page 2
	if err := p.FreePage(2); err != nil {
		t.Fatalf("FreePage(2): %v", err)
	}
	before := binary.BigEndian.Uint32(p.header[32:36])
	countBefore := binary.BigEndian.Uint32(p.header[36:40])
	if before != 2 {
		t.Fatalf("header.trunk before pop = %d, want 2", before)
	}
	if countBefore != 1 {
		t.Fatalf("header.count before pop = %d, want 1", countBefore)
	}

	pg, err := p.AllocatePageANY()
	if err != nil {
		t.Fatalf("AllocatePageANY: %v", err)
	}
	if pg.PageNum != 2 {
		t.Fatalf("AllocatePageANY returned page %d, want 2", pg.PageNum)
	}

	after := binary.BigEndian.Uint32(p.header[32:36])
	countAfter := binary.BigEndian.Uint32(p.header[36:40])
	if after != 0 {
		t.Errorf("header.trunk after pop = %d, want 0", after)
	}
	if countAfter != 0 {
		t.Errorf("header.count after pop = %d, want 0", countAfter)
	}
}

// TestChainLeafPopDecrements: allocate pages 2-3, free page 4 first
// (becomes a trunk above 3), then free page 3 (becomes a leaf of trunk 4).
// AllocatePageLE(3) walks past trunk 4 (4 > 3, so not a candidate) and
// returns leaf 3. The leaf slot on trunk 4 is zeroed and count drops to 1.
func TestChainLeafPopDecrements(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()

	allocPagesLocked(p, 4) // pages 1, 2, 3, 4
	if err := p.FreePage(4); err != nil {
		t.Fatalf("FreePage(4): %v", err)
	}
	if err := p.FreePage(3); err != nil {
		t.Fatalf("FreePage(3): %v", err)
	}
	countBefore := binary.BigEndian.Uint32(p.header[36:40])
	if countBefore != 2 {
		t.Fatalf("header.count after 2 frees = %d, want 2", countBefore)
	}

	pg, err := p.AllocatePageLE(3)
	if err != nil {
		t.Fatalf("AllocatePageLE(3): %v", err)
	}
	if pg.PageNum != 3 {
		t.Fatalf("AllocatePageLE(3) returned page %d, want 3", pg.PageNum)
	}

	trunk := binary.BigEndian.Uint32(p.header[32:36])
	count := binary.BigEndian.Uint32(p.header[36:40])
	if trunk != 4 {
		t.Errorf("header.trunk after leaf pop = %d, want 4", trunk)
	}
	if count != 1 {
		t.Errorf("header.count after leaf pop = %d, want 1", count)
	}
	if pg, ok := p.pages[4]; ok {
		leaf0 := binary.BigEndian.Uint32(pg.Data[8:12])
		if leaf0 != 0 {
			t.Errorf("trunk 4 leaf slot 0 = %d, want 0", leaf0)
		}
	}
}

// TestChainFreeAllocFreeIdempotent: free page 2, pop it, free it
// again. The chain must have exactly one entry for page 2 (a single
// trunk, no leaves, no duplicates).
func TestChainFreeAllocFreeIdempotent(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()
	allocPagesLocked(p, 1) // page 1
	allocPagesLocked(p, 1) // page 2

	steps := []func() error{
		func() error { return p.FreePage(2) },
		func() error {
			pg, err := p.AllocatePageANY()
			if err != nil {
				return err
			}
			if pg.PageNum != 2 {
				t.Errorf("expected page 2, got %d", pg.PageNum)
			}
			return nil
		},
		func() error { return p.FreePage(2) },
	}
	for i, s := range steps {
		if err := s(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	trunk, leaf := chainCountsLocked(p)
	if trunk != 1 || leaf != 0 {
		t.Errorf("after free/alloc/free: trunk=%d leaf=%d, want 1+0", trunk, leaf)
	}
	count := binary.BigEndian.Uint32(p.header[36:40])
	if count != 1 {
		t.Errorf("header.count = %d, want 1", count)
	}
}

// TestChain300PageMultiTrunk: allocate 301 pages so we have 300
// non-root pages to free. The chain must have:
//   - header.count == 300
//   - chain-walked trunk + leaf == 300
//   - each trunk has leafCount <= maxTrunkLeaves (pageSize/4 - 8)
//   - chain walk terminates without cycles
func TestChain300PageMultiTrunk(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()

	const want = 300
	pages := allocPagesLocked(p, want+1) // pages 1..301; first is schema root
	freed := 0
	for _, pgno := range pages {
		if pgno == 1 {
			continue // never free the schema root
		}
		if err := p.FreePage(pgno); err != nil {
			t.Fatalf("FreePage(%d): %v", pgno, err)
		}
		freed++
	}
	count := binary.BigEndian.Uint32(p.header[36:40])
	if int(count) != want {
		t.Fatalf("header.count = %d, want %d (freed %d)", count, want, freed)
	}

	maxLeaves := int(p.pageSize/4 - 8)
	trunk, leaf := chainCountsLocked(p)
	if trunk+leaf != want {
		t.Errorf("chain-walked trunk(%d) + leaf(%d) = %d, want %d", trunk, leaf, trunk+leaf, want)
	}

	// Walk and verify each trunk obeys the leaf cap and there are
	// no cycles.
	cur := binary.BigEndian.Uint32(p.header[32:36])
	seen := map[uint32]bool{}
	for cur != 0 {
		if seen[cur] {
			t.Fatalf("cycle detected at trunk %d", cur)
		}
		seen[cur] = true
		tr, ok := p.readFreelistTrunkLocked(cur)
		if !ok {
			t.Fatalf("readFreelistTrunkLocked(%d): !ok", cur)
		}
		if len(tr.leaves) > maxLeaves {
			t.Errorf("trunk %d has %d leaves, max %d", cur, len(tr.leaves), maxLeaves)
		}
		cur = tr.next
	}
}

// TestChainAllocatePageLEPrefersLowPage: allocate pages 5, 6, 7,
// free them, then AllocatePageLE(4) should return the lowest ≤ 4.
// With pages 5/6/7 in the chain, LE(4) is empty → disk-full error.
func TestChainAllocatePageLEPrefersLowPage(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()
	pages := allocPagesLocked(p, 3) // pages 2, 3, 4
	if len(pages) != 3 {
		t.Fatalf("allocPagesLocked returned %d pages, want 3", len(pages))
	}
	// Re-allocate to grow to 5, 6, 7.
	for i := 0; i < 3; i++ {
		allocPagesLocked(p, 1)
	}
	// Free pages 5, 6, 7. Pages 2, 3, 4 stay allocated.
	for _, pg := range []uint32{5, 6, 7} {
		if err := p.FreePage(pg); err != nil {
			t.Fatalf("FreePage(%d): %v", pg, err)
		}
	}
	// LE(4) must fail (no free page ≤ 4).
	if _, err := p.AllocatePageLE(4); err == nil {
		t.Errorf("AllocatePageLE(4) succeeded, expected disk-full")
	}
	// LE(7) should return a page ≤ 7 (one of 5/6/7).
	pg, err := p.AllocatePageLE(7)
	if err != nil {
		t.Fatalf("AllocatePageLE(7): %v", err)
	}
	if pg.PageNum < 5 || pg.PageNum > 7 {
		t.Errorf("AllocatePageLE(7) returned %d, want page in [5,7]", pg.PageNum)
	}
}
