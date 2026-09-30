package pager

// Adversarial review probes for the P5 statement journal + before-image pool
// (fleet/review-perf-audit). Complements pagerstmt_test.go with the hostile
// lifecycle orders: double rollback, rollback of a parent after a child
// splice, splice-drops-newer-image ordering, out-of-order scope teardown
// (interrupted statement), and buffer-pool ownership under repeated
// rollback/adoption cycles.

import (
	"path/filepath"
	"testing"
)

func probeStmtPager(t *testing.T) *Pager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.db")
	p, err := Open(path, 512)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	// Materialize pages 1-3 so raw page writes have real targets.
	allocPagesLocked(p, 3)
	return p
}

// Rollback of a parent AFTER a child EndStatement splice: the parent must
// keep its OLDER before-image for page 2 (child's newer image dies at splice)
// and gain page 3. Parent rollback restores both pages to the pre-parent
// state.
func TestProbeJournalSpliceThenParentRollback(t *testing.T) {
	p := probeStmtPager(t)
	// Baseline writes outside any scope (dirty, unjournaled).
	writePage(t, p, 2, 0xAA)
	writePage(t, p, 3, 0xBB)

	outer := p.BeginStatement()
	writePage(t, p, 2, 0x11) // outer journals page 2 (older image AA)

	inner := p.BeginStatement()
	writePage(t, p, 2, 0x22) // inner journals page 2 (image 11)
	writePage(t, p, 3, 0x33) // inner journals page 3 (image BB)
	p.EndStatement(inner)    // splice: parent keeps 2=AA, gains 3=BB

	// Parent rollback undoes BOTH scopes' writes.
	p.RollbackStatement(outer)
	if got := readPageFill(t, p, 2); got != 0xAA {
		t.Fatalf("page 2 after parent rollback = %#x, want 0xAA (pre-outer image)", got)
	}
	if got := readPageFill(t, p, 3); got != 0xBB {
		t.Fatalf("page 3 after parent rollback = %#x, want 0xBB", got)
	}
}

// Double rollback on the same scope must be a no-op (the done flag), and a
// double End must not splice twice or pool buffers twice.
func TestProbeJournalDoubleClose(t *testing.T) {
	p := probeStmtPager(t)
	writePage(t, p, 2, 0x01)
	j := p.BeginStatement()
	writePage(t, p, 2, 0x02)
	p.RollbackStatement(j)
	p.RollbackStatement(j) // must be a silent no-op
	p.EndStatement(j)      // ditto
	if got := readPageFill(t, p, 2); got != 0x01 {
		t.Fatalf("page 2 after double rollback = %#x, want 0x01", got)
	}
	// The scope must be unlinked: a fresh scope journals from the current state.
	j2 := p.BeginStatement()
	writePage(t, p, 2, 0x03)
	p.EndStatement(j2)
	if got := readPageFill(t, p, 2); got != 0x03 {
		t.Fatalf("page 2 after end = %#x, want 0x03", got)
	}
}

// Interrupted statement: a scope closed while a NEWER scope is already open
// (engine abort paths can close out of order — unlinkStmtLocked handles the
// middle-of-chain removal). The newer scope's entries must stay functional.
func TestProbeJournalOutOfOrderClose(t *testing.T) {
	p := probeStmtPager(t)
	writePage(t, p, 2, 0x01)
	writePage(t, p, 3, 0xBB)

	outer := p.BeginStatement()
	writePage(t, p, 2, 0x02)
	inner := p.BeginStatement() // becomes stmtTop
	writePage(t, p, 3, 0x0A)    // inner journals 3 (image BB)

	// Close the OUTER (non-top) scope first — an out-of-order teardown.
	p.RollbackStatement(outer)
	if got := readPageFill(t, p, 2); got != 0x01 {
		t.Fatalf("outer rollback: page 2 = %#x, want 0x01", got)
	}
	// The inner scope is still open; its rollback must still work even though
	// its parent chain was rewired.
	writePage(t, p, 3, 0x0B)
	p.RollbackStatement(inner)
	if got := readPageFill(t, p, 3); got != 0xBB {
		t.Fatalf("inner rollback: page 3 = %#x, want 0xBB (its begin image)", got)
	}
}

// Buffer-pool ownership under repeated adopt-on-rollback: a buffer adopted
// as a page's data must never be Put back or reused while live. Every cycle
// rolls back to the baseline image (0x00), so contents must return to 0x00
// each time; 64 cycles exercise the acquire/adopt paths hard.
func TestProbeJournalPoolAdoptionCycles(t *testing.T) {
	p := probeStmtPager(t)
	writePage(t, p, 2, 0x00)
	for cycle := 0; cycle < 64; cycle++ {
		fill := byte(0x10 + cycle)
		j := p.BeginStatement()
		// Page 2 is dirty at scope begin (the restore of the previous cycle
		// re-dirtied it): capture is a memory image; rollback adopts it.
		writePage(t, p, 2, fill)
		p.RollbackStatement(j)
		if got := readPageFill(t, p, 2); got != 0x00 {
			t.Fatalf("cycle %d: page 2 = %#x, want 0x00 (baseline image)", cycle, got)
		}
	}
}

// Allocated-page rollback: pages created inside a statement (pgno above the
// scope's starting count) must vanish on rollback, and the page counter must
// return so a later statement can reallocate the same numbers.
func TestProbeJournalAllocatedPageRollback(t *testing.T) {
	p := probeStmtPager(t)
	base := p.NumPages()
	j := p.BeginStatement()
	pg := p.AllocatePage()
	newPg := pg.PageNum
	// Dirty it so the journal records the allocation.
	pg.Data[0] = 0x7E
	if err := p.WritePage(pg); err != nil {
		t.Fatal(err)
	}
	if got := p.NumPages(); got <= base {
		t.Fatalf("allocation did not raise page count: %d -> %d", base, got)
	}
	p.RollbackStatement(j)
	if got := p.NumPages(); got != base {
		t.Fatalf("page count after rollback = %d, want %d", got, base)
	}
	// The freed number must be reusable.
	re := p.AllocatePage()
	if re.PageNum != newPg {
		t.Logf("realloc gave %d (was %d) — freelist policy, informational", re.PageNum, newPg)
	}
}
