// Statement-journal before-image buffer pool: lifetime and rollback-correctness
// pins for the recycled images (pagerstmt.go stmtImagePool).
package pager

import (
	"encoding/binary"
	"testing"
)

// poolPager opens a memory pager (every statement-journal image is a memory
// copy there — the pooling path).
func poolPager(t *testing.T) *Pager {
	t.Helper()
	p := OpenInMemory(512)
	// Materialize 4 pages so statements mutate existing (begin-dirty) pages.
	// Memory pages are always memory-captured by the statement journal, so
	// every statement below exercises the pooled-image path.
	for i := 0; i < 5; i++ {
		p.AllocatePage()
	}
	return p
}

// pageStamp writes a recognizable pattern into a page's bytes.
func pageStamp(pg *Page, gen uint32) {
	for off := 0; off+4 <= len(pg.Data); off += 4 {
		binary.BigEndian.PutUint32(pg.Data[off:], gen)
	}
}

// pagePattern reports the generation stamped into the page (0 when the bytes
// do not match any single generation).
func pagePattern(pg *Page) uint32 {
	gen := binary.BigEndian.Uint32(pg.Data[0:])
	for off := 0; off+4 <= len(pg.Data); off += 4 {
		if binary.BigEndian.Uint32(pg.Data[off:]) != gen {
			return 0
		}
	}
	return gen
}

// TestStmtJournalPoolRollbackExactness drives many statements over the same
// pages with recycling in play: every failed statement must restore its
// pages byte-exactly, and every committed statement's writes must survive
// later rollbacks (the spliced parent image must stay the OLDEST one).
func TestStmtJournalPoolRollbackExactness(t *testing.T) {
	p := poolPager(t)
	defer p.Close()

	// Statement 1 (committed): stamp pages 1..4 with generation 1.
	j := p.BeginStatement()
	for pgno := uint32(2); pgno <= 5; pgno++ {
		pg, err := p.ReadPage(pgno)
		if err != nil {
			t.Fatalf("read %d: %v", pgno, err)
		}
		pageStamp(pg, 1)
		p.WritePage(pg)
	}
	p.EndStatement(j)

	// Statements 2..N: stamp further generations, roll each back. The
	// restored image must be generation 1 every time — including after the
	// pool has started recycling buffers across statements.
	for gen := uint32(2); gen <= 40; gen++ {
		j := p.BeginStatement()
		for pgno := uint32(2); pgno <= 5; pgno++ {
			pg, err := p.ReadPage(pgno)
			if err != nil {
				t.Fatalf("read %d: %v", pgno, err)
			}
			pageStamp(pg, gen)
			p.WritePage(pg)
		}
		p.RollbackStatement(j)
		for pgno := uint32(2); pgno <= 5; pgno++ {
			pg, err := p.ReadPage(pgno)
			if err != nil {
				t.Fatalf("read %d: %v", pgno, err)
			}
			if got := pagePattern(pg); got != 1 {
				t.Fatalf("after rollback gen %d: page %d holds pattern %d, want 1", gen, pgno, got)
			}
		}
	}

	// Committed statements 41..80 recycle the same buffers through the
	// EndStatement discard path (outermost scope, no parent); then a
	// rollback of generation 100 must restore generation 80 — the spliced
	// parent (outermost) image.
	for gen := uint32(41); gen <= 80; gen++ {
		j := p.BeginStatement()
		pg, err := p.ReadPage(2)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		pageStamp(pg, gen)
		p.WritePage(pg)
		p.EndStatement(j)
	}
	j = p.BeginStatement()
	pg, _ := p.ReadPage(2)
	pageStamp(pg, 100)
	p.WritePage(pg)
	p.RollbackStatement(j)
	pg, _ = p.ReadPage(2)
	if got := pagePattern(pg); got != 80 {
		t.Fatalf("post-commit rollback: page 2 holds %d, want 80", got)
	}
}

// TestStmtJournalPoolNestedSpliceKeepsOldest pins the nested-scope rule under
// recycling: when a child scope's image is discarded because the parent
// already holds one, the parent's OLDER image is what a parent rollback
// restores.
func TestStmtJournalPoolNestedSpliceKeepsOldest(t *testing.T) {
	p := poolPager(t)
	defer p.Close()

	// Committed base: generation 1.
	j := p.BeginStatement()
	pg, _ := p.ReadPage(2)
	pageStamp(pg, 1)
	p.WritePage(pg)
	p.EndStatement(j)

	// Outer statement: generation 2 (splices into the outermost scope on
	// EndStatement — the outermost scope IS the parent here).
	outer := p.BeginStatement()
	pg, _ = p.ReadPage(2)
	pageStamp(pg, 2)
	p.WritePage(pg)

	// Inner statement: generation 3, commits — its image is discarded
	// because the parent (outer) already journalled page 1. A pooled buffer
	// must not end up shared between the discarded and kept entries.
	inner := p.BeginStatement()
	pg, _ = p.ReadPage(2)
	pageStamp(pg, 3)
	p.WritePage(pg)
	p.EndStatement(inner)

	// Inner rollback #2 (generation 4) — replay adopts a pooled buffer as
	// the restored page; the outer image must be untouched.
	inner2 := p.BeginStatement()
	pg, _ = p.ReadPage(2)
	pageStamp(pg, 4)
	p.WritePage(pg)
	p.RollbackStatement(inner2)
	pg, _ = p.ReadPage(2)
	// The inner commit (generation 3) is the statement-start state of the
	// rolled-back inner scope, so 3 — not the outer statement's 2 — is
	// restored here.
	if got := pagePattern(pg); got != 3 {
		t.Fatalf("after inner rollback: page holds %d, want 3 (inner commit's state)", got)
	}

	// Outer rollback: back to generation 1 (the OLDEST image — the inner
	// commit's discarded generation-3 image must not have overwritten it).
	p.RollbackStatement(outer)
	pg, _ = p.ReadPage(2)
	if got := pagePattern(pg); got != 1 {
		t.Fatalf("after outer rollback: page holds %d, want 1 (oldest image)", got)
	}
}

// TestStmtJournalPoolBufferNotShared pins that a recycled buffer never ends
// up referenced by two live entries: capture two distinct pages in one
// scope, roll back, and verify both pages restore independently (a shared
// buffer would make the second restore clobber the first).
func TestStmtJournalPoolBufferNotShared(t *testing.T) {
	p := poolPager(t)
	defer p.Close()

	j := p.BeginStatement()
	pg1, _ := p.ReadPage(2)
	pageStamp(pg1, 1)
	p.WritePage(pg1)
	pg2, _ := p.ReadPage(3)
	pageStamp(pg2, 2)
	p.WritePage(pg2)
	p.EndStatement(j)

	for round := 0; round < 50; round++ {
		j := p.BeginStatement()
		pg1, _ := p.ReadPage(2)
		pageStamp(pg1, uint32(100+round))
		p.WritePage(pg1)
		pg2, _ := p.ReadPage(2)
		pageStamp(pg2, uint32(200+round))
		p.WritePage(pg2)
		p.RollbackStatement(j)
		pg1, _ = p.ReadPage(2)
		pg2, _ = p.ReadPage(3)
		if got := pagePattern(pg1); got != 1 {
			t.Fatalf("round %d: page 2 = %d, want 1", round, got)
		}
		if got := pagePattern(pg2); got != 2 {
			t.Fatalf("round %d: page 3 = %d, want 2", round, got)
		}
	}
}

// TestStmtJournalScopeRecycleClean pins that a scope object returned to the
// pager's free list carries none of its previous life into the next
// statement: recycled entries must not resurrect (a page the previous
// statement journalled must NOT be journalled in the next statement unless
// that statement touches it), and a recycled scope's rollback restores ITS
// OWN begin state.
func TestStmtJournalScopeRecycleClean(t *testing.T) {
	p := poolPager(t)
	defer p.Close()

	// Baseline content: pages 2 and 3 stamped with generation 1.
	j := p.BeginStatement()
	pg2, _ := p.ReadPage(2)
	pageStamp(pg2, 1)
	p.WritePage(pg2)
	pg3, _ := p.ReadPage(3)
	pageStamp(pg3, 1)
	p.WritePage(pg3)
	p.EndStatement(j)

	// Statement 2 journals page 2 (and only page 2), then commits — the
	// scope (with its entries map) recycles into the free list.
	j = p.BeginStatement()
	pg2, _ = p.ReadPage(2)
	pageStamp(pg2, 2)
	p.WritePage(pg2)
	p.EndStatement(j)

	// Statement 3 touches ONLY page 3 and rolls back. A stale recycled
	// entry for page 2 would replay page 2's statement-2-captured image
	// (generation 1) even though statement 3 never wrote it — page 2 must
	// stay at generation 2 while page 3 returns to generation 1.
	j = p.BeginStatement()
	pg3, _ = p.ReadPage(3)
	pageStamp(pg3, 3)
	p.WritePage(pg3)
	p.RollbackStatement(j)
	pg2, _ = p.ReadPage(2)
	if got := pagePattern(pg2); got != 2 {
		t.Fatalf("page 2 = %d, want 2 (stale recycled entry replayed)", got)
	}
	pg3, _ = p.ReadPage(3)
	if got := pagePattern(pg3); got != 1 {
		t.Fatalf("page 3 = %d, want 1", got)
	}

	// The recycled object must be fully usable as the NEXT statement's
	// scope: a fresh begin on the recycled token journals and rolls back
	// exactly its own writes.
	j = p.BeginStatement()
	pg2, _ = p.ReadPage(2)
	pageStamp(pg2, 9)
	p.WritePage(pg2)
	p.RollbackStatement(j)
	pg2, _ = p.ReadPage(2)
	if got := pagePattern(pg2); got != 2 {
		t.Fatalf("page 2 = %d after recycled-scope rollback, want 2", got)
	}
}

// TestStmtJournalImageFreeListRecycle pins the before-image buffer free list:
// buffers dropped at EndStatement are reused by later captures without
// aliasing — each capture writes its full page image before the entry is
// observable, and a splice-kept parent image survives later statements
// reusing the dropped buffers.
func TestStmtJournalImageFreeListRecycle(t *testing.T) {
	p := poolPager(t)
	defer p.Close()

	// Baseline: page 2 = 1, page 3 = 1.
	j := p.BeginStatement()
	for _, pgno := range []uint32{2, 3} {
		pg, _ := p.ReadPage(pgno)
		pageStamp(pg, 1)
		p.WritePage(pg)
	}
	p.EndStatement(j)

	// Outer scope captures page 2's image, then an inner scope captures
	// pages 2 and 3. The inner splice keeps the outer's OLDER page-2 image
	// and moves its page-3 image up; the dropped inner page-2 buffer
	// recycles. A later capture must overwrite a recycled buffer in full
	// (the outer's kept page-2 image must restore exactly).
	outer := p.BeginStatement()
	pg2, _ := p.ReadPage(2)
	pageStamp(pg2, 2) // outer's first write captures page 2 at generation 1→2 boundary
	p.WritePage(pg2)
	inner := p.BeginStatement()
	pg2, _ = p.ReadPage(2)
	pageStamp(pg2, 3)
	p.WritePage(pg2)
	pg3, _ := p.ReadPage(3)
	pageStamp(pg3, 3)
	p.WritePage(pg3)
	p.EndStatement(inner)
	// More commits: their captures recycle the dropped buffer.
	for gen := uint32(4); gen <= 20; gen++ {
		jc := p.BeginStatement()
		pg3, _ := p.ReadPage(3)
		pageStamp(pg3, gen)
		p.WritePage(pg3)
		p.EndStatement(jc)
	}
	// Roll the outer scope back: page 2 must return to generation 1 (the
	// image kept at splice, untouched by the recycled buffer), page 3 to
	// generation 1.
	p.RollbackStatement(outer)
	pg2, _ = p.ReadPage(2)
	if got := pagePattern(pg2); got != 1 {
		t.Fatalf("page 2 = %d, want 1 (kept oldest image)", got)
	}
	pg3, _ = p.ReadPage(3)
	if got := pagePattern(pg3); got != 1 {
		t.Fatalf("page 3 = %d, want 1", got)
	}
}
