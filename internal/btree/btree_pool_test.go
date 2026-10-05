package btree

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// buildPoolTestTree creates a table btree at a fresh root with n rowid rows.
func buildPoolTestTree(t *testing.T, n int) (*pager.Pager, uint32) {
	t.Helper()
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	rootPg := pg.AllocatePage()
	root := rootPg.PageNum
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	if err := pg.WritePage(rootPg); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	bt := NewBTree(pg, root, true)
	for i := int64(1); i <= int64(n); i++ {
		rec, _ := storage.EncodeRecord([]interface{}{i, fmt.Sprintf("x%d", i)})
		cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: i, Payload: rec}
		if err := bt.InsertCell(cell); err != nil {
			t.Fatalf("InsertCell %d: %v", i, err)
		}
	}
	bt.Close()
	return pg, root
}

// TestPooledSeekLifecycle replays the statement lifecycle — fresh wrapper,
// open cursor, seek, read, close — over and over, and asserts every seek
// finds its row. A pooling reset bug (a stale cursor position, a retained
// page cache, a wrapper re-armed with the previous tenant's state) shows up
// as a missed seek.
func TestPooledSeekLifecycle(t *testing.T) {
	pg, root := buildPoolTestTree(t, 1000)
	for round := 0; round < 50; round++ {
		tree := NewBTree(pg, root, true)
		cur, err := tree.OpenCursor()
		if err != nil {
			t.Fatalf("round %d: OpenCursor: %v", round, err)
		}
		// Full scan sees every row exactly once.
		seen := make(map[int64]bool)
		for {
			payload, rowID, err := cur.ReadCellData()
			if err != nil {
				break
			}
			if seen[rowID] {
				t.Fatalf("round %d: row %d seen twice", round, rowID)
			}
			seen[rowID] = true
			if len(payload) == 0 {
				t.Fatalf("round %d: row %d empty payload", round, rowID)
			}
			ok, err := cur.Next()
			if err != nil || !ok {
				break
			}
		}
		if len(seen) != 1000 {
			t.Fatalf("round %d: scan saw %d rows, want 1000", round, len(seen))
		}
		tree.Close()

		// Point seeks on a fresh wrapper per round (the per-statement shape).
		for _, want := range []int64{1, 2, 500, 999, 1000} {
			tree2 := NewBTree(pg, root, true)
			cur2, err := tree2.OpenCursor()
			if err != nil {
				t.Fatalf("round %d seek %d: OpenCursor: %v", round, want, err)
			}
			found, err := cur2.SeekToRowID(want)
			if err != nil {
				t.Fatalf("round %d seek %d: %v", round, want, err)
			}
			if !found {
				t.Errorf("round %d: SeekToRowID(%d) not found", round, want)
			} else {
				payload, gotRowID, err := cur2.ReadCellData()
				if err != nil {
					t.Errorf("round %d seek %d: ReadCellData: %v", round, want, err)
				} else if gotRowID != want {
					t.Errorf("round %d: seek %d landed on %d", round, want, gotRowID)
				} else if len(payload) == 0 {
					t.Errorf("round %d: seek %d empty payload", round, want)
				}
			}
			tree2.Close()
		}
	}
}

// TestPooledScanAcrossWrappers mixes wrapper identities within one
// statement-shaped unit: scan with wrapper A while probing seeks with
// wrappers B and C, then close all — the registry-based save/restore must
// stay keyed on (pager, root), not wrapper identity.
func TestPooledScanAcrossWrappers(t *testing.T) {
	pg, root := buildPoolTestTree(t, 500)
	for round := 0; round < 20; round++ {
		a := NewBTree(pg, root, true)
		b := NewBTree(pg, root, true)
		curA, err := a.OpenCursor()
		if err != nil {
			t.Fatalf("round %d: OpenCursor A: %v", round, err)
		}
		n := 0
		for {
			if _, _, err := curA.ReadCellData(); err != nil {
				break
			}
			n++
			// A nested statement's write pattern: probe via another wrapper.
			curB, err := b.OpenCursor()
			if err != nil {
				t.Fatalf("round %d: OpenCursor B: %v", round, err)
			}
			if _, err := curB.SeekToRowID(int64(n)); err != nil {
				t.Fatalf("round %d: seek B: %v", round, err)
			}
			ok, err := curA.Next()
			if err != nil || !ok {
				break
			}
		}
		if n != 500 {
			t.Fatalf("round %d: scan saw %d rows, want 500", round, n)
		}
		a.Close()
		b.Close()
	}
}

// registeredCursorCount returns how many cursors are registered for a key.
func registeredCursorCount(key cursorTreeKey) int {
	cursorRegMu.Lock()
	defer cursorRegMu.Unlock()
	return len(cursorRegistry[key])
}

// TestCursorRecycleRegistrationContract pins the pooling + registration
// contract that the fleet/perf-parity-wrappers tranche broke (fleet/
// perf-parity-poolfix): cursors are pooled and recycled across statements —
// a wrapper is closed at the end of every statement-shaped unit and the next
// one recycles its CURSORS (wrappers themselves are not recycled — see
// btree_pool.go) — and every recycled cursor is REGISTERED AGAIN on the next
// OpenCursor. Registration must never touch runtime.SetFinalizer:
// setting/clearing a finalizer per registration on a recycled object races
// the GC sweep cycle (a special can outlive its object through the pool drop
// at poolCleanup), which crashed 2 of 4 full-suite runs with the fatal
// "runtime.SetFinalizer: finalizer already set" and — through a late-queued
// finalizer unregistering a live cursor — a SIGSEGV in a concurrent scan.
//
// The loop interleaves GC with cursor recycling to maximize exposure of any
// finalizer/special desync; a violation either fatals the process (the old
// bug) or corrupts the registry (a stale entry keeps a dead cursor in
// saveAllCursors' walk). Both are caught: the process survives, and the
// registry must hold exactly the live cursors at every checkpoint.
func TestCursorRecycleRegistrationContract(t *testing.T) {
	pg, root := buildPoolTestTree(t, 200)
	key := cursorTreeKey{pg: pg, root: root}
	for round := 0; round < 200; round++ {
		// Statement-shaped unit: acquire a (likely recycled) wrapper, open a
		// (likely recycled) cursor, scan, close. Close must unregister every
		// cursor it owned — the registry holds only live cursors.
		tree := NewBTree(pg, root, true)
		cur, err := tree.OpenCursor()
		if err != nil {
			t.Fatalf("round %d: OpenCursor: %v", round, err)
		}
		if got := registeredCursorCount(key); got != 1 {
			t.Fatalf("round %d: registry holds %d cursors while scanning, want 1", round, got)
		}
		n := 0
		for {
			if _, _, err := cur.ReadCellData(); err != nil {
				break
			}
			n++
			if ok, err := cur.Next(); err != nil || !ok {
				break
			}
		}
		if n != 200 {
			t.Fatalf("round %d: scan saw %d rows, want 200", round, n)
		}
		tree.Close()
		if got := registeredCursorCount(key); got != 0 {
			t.Fatalf("round %d: registry holds %d cursors after Close, want 0", round, got)
		}
		// GC churn between statements: pools are dropped and finalizers queue
		// at exactly these boundaries. The old per-registration SetFinalizer
		// desynced from the runtime here; the allocation-time finalizer must
		// be a no-op for any recycled (unregistered) cursor.
		if round%10 == 0 {
			runtime.GC()
		}
	}
}

// TestCursorFinalizerSafetyNet pins the allocation-time finalizer's contract:
// a live registration must SURVIVE GC churn and finalizer draining (a
// finalizer queued against a recycled cursor — its special outliving the
// object through a pool drop — must not unregister a later registration),
// and a closed or recycled cursor (zero regKey) must unregister nothing.
// Registered cursors are pinned by the registry itself: a wrapper that is
// never closed keeps its cursors registered (and reachable) forever — the
// finalizer is a no-op guard, not a reaper.
func TestCursorFinalizerSafetyNet(t *testing.T) {
	pg, root := buildPoolTestTree(t, 50)
	key := cursorTreeKey{pg: pg, root: root}

	// A properly closed + recycled cursor must never be unregistered by a
	// late-queued finalizer: close it (key zeroed), let GC churn the pool and
	// any queued specials, re-register the recycled cursor, and verify the
	// registration survives further GC + finalizer draining.
	tree := NewBTree(pg, root, true)
	cur, err := tree.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	tree.Close()
	if got := registeredCursorCount(key); got != 0 {
		t.Fatalf("registry holds %d cursors after Close, want 0", got)
	}
	// Re-acquire BEFORE any GC so the pool hands back the same wrapper (and
	// its cursor free list); then churn the GC over the live registration.
	tree2 := NewBTree(pg, root, true)
	cur2, err := tree2.OpenCursor()
	if err != nil {
		t.Fatalf("re-OpenCursor: %v", err)
	}
	if cur2 != cur {
		t.Fatalf("expected the recycled cursor to be reused (fresh=%p recycled=%p)", cur2, cur)
	}
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond) // let any queued finalizer run
	}
	if got := registeredCursorCount(key); got != 1 {
		t.Fatalf("live registration lost after GC (registry holds %d), want 1", got)
	}
	tree2.Close()
	if got := registeredCursorCount(key); got != 0 {
		t.Fatalf("registry holds %d cursors after Close, want 0", got)
	}
}

// TestClosedOwnerReadContract pins the dead-owner guard: a cursor whose
// wrapper was closed and pooled must report an error on reads, never
// dereference the reset wrapper (whose pager is nil after resetForPool).
func TestClosedOwnerReadContract(t *testing.T) {
	pg, root := buildPoolTestTree(t, 10)
	tree := NewBTree(pg, root, true)
	cur, err := tree.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor: %v", err)
	}
	tree.Close()
	// The wrapper re-enters the pool and may already be re-armed by the next
	// NewBTree; the cursor's OWNER pointer must fail reads either way. Grab a
	// second wrapper to exercise the recycled-object path, then read. The
	// released marker (checked by restoreIfNeeded before cachePage) or the
	// dead-owner guard (cachePage) rejects the read — either message upholds
	// the "use of a closed cursor reports an error" contract.
	next := NewBTree(pg, root, true)
	defer next.Close()
	if _, _, err := cur.ReadCellData(); err == nil {
		t.Fatal("ReadCellData on a closed owner succeeded, want an error")
	} else if err.Error() != "btree: cursor used after close" && err.Error() != "btree: cursor's owning tree is closed" {
		t.Fatalf("ReadCellData error = %v, want a closed-cursor contract error", err)
	}
}

// TestTreeFreeListGenerationLease pins the ownership-token discipline that
// makes wrapper reuse safe (btree_pool.go file header): every Reinit bumps
// the arm generation, a release whose lease generation no longer matches the
// wrapper's current generation is STALE and must no-op — no Close, no Put —
// leaving the live successor untouched and functional. This is the
// structural fix for the A-late-Close/B-reuse hazard that removed the first
// wrapper pooling.
func TestTreeFreeListGenerationLease(t *testing.T) {
	pg, root := buildPoolTestTree(t, 10)

	var fl TreeFreeList
	tree := NewBTree(pg, root, true)
	firstGen := tree.Generation()
	lease := TreeLease{Tree: tree, Gen: firstGen}

	tree.Close()
	fl.Put(tree)

	// The next owner re-arms the SAME wrapper object.
	var got *BTree
	for {
		candidate := fl.Get()
		if candidate == nil {
			t.Fatal("free list empty, want the just-closed wrapper")
		}
		if candidate == tree {
			got = candidate
			break
		}
	}
	if got.Generation() != firstGen {
		t.Fatalf("closed wrapper gen = %d, want %d (bump happens at Reinit)", got.Generation(), firstGen)
	}
	got.Reinit(pg, root, true)
	if got.Closed() {
		t.Fatal("Reinit left the wrapper closed")
	}
	if got.Generation() != firstGen+1 {
		t.Fatalf("Reinit gen = %d, want %d", got.Generation(), firstGen+1)
	}

	// The FIRST owner's stale release (lease gen no longer matches) must
	// no-op: the live successor keeps working.
	if lease.Gen == lease.Tree.Generation() {
		t.Fatal("stale lease unexpectedly matches the re-armed generation")
	}
	if lease.Tree.Closed() {
		t.Fatal("re-armed wrapper reported closed before the stale release")
	}
	// (The stale release itself is the caller's gen check skipping Close+Put
	// — the same comparison releaseStatementTrees runs. Pin the comparison.)
	staleReleaseNoOp := lease.Tree.Closed() || lease.Tree.Generation() != lease.Gen
	if !staleReleaseNoOp {
		t.Fatal("stale lease release would close the live successor")
	}
	cur, err := got.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor after stale release: %v", err)
	}
	found, err := cur.SeekToRowID(7)
	if err != nil || !found {
		t.Fatalf("SeekToRowID(7) after stale release: found=%v err=%v", found, err)
	}
	cur.Close()
	got.Close()
	// The LIVE owner's release still recycles: Put accepts the closed
	// wrapper and a later Get hands it back.
	fl.Put(got)
	if again := fl.Get(); again != got {
		t.Fatal("live owner's release did not return the wrapper to the free list")
	}
	// Put refuses open wrappers (defensive: never pool a live wrapper).
	live := NewBTree(pg, root, true)
	defer live.Close()
	fl.Put(live)
	if n := len(fl.free); n != 0 {
		t.Fatalf("free list holds %d wrappers after Put of an open wrapper, want 0", n)
	}
}

// TestTreeFreeListPurge pins the layout-hook contract: Purge drops every
// pooled wrapper without closing them (they are already closed) and the
// next Get builds nothing.
func TestTreeFreeListPurge(t *testing.T) {
	pg, root := buildPoolTestTree(t, 4)
	var fl TreeFreeList
	for i := 0; i < 3; i++ {
		tree := NewBTree(pg, root, true)
		tree.Close()
		fl.Put(tree)
	}
	if n := len(fl.free); n != 3 {
		t.Fatalf("free list holds %d wrappers, want 3", n)
	}
	fl.Purge()
	if n := len(fl.free); n != 0 {
		t.Fatalf("free list holds %d wrappers after Purge, want 0", n)
	}
	if fl.Get() != nil {
		t.Fatal("Get after Purge returned a wrapper, want nil")
	}
}

// TestReinitFullReset pins the acquire-side reset contract: a re-armed
// wrapper is observably a fresh NewBTree over the new identity — no
// inherited key comparator (WITHOUT ROWID tenant -> rowid tenant), no
// inherited cursors, geometry from the CURRENT pager.
func TestReinitFullReset(t *testing.T) {
	pg, root := buildPoolTestTree(t, 4)
	tree := NewBTree(pg, root, true)
	tree.SetKeyCompare(func(a, b []byte) int { return 1 })
	tree.Close()

	var fl TreeFreeList
	fl.Put(tree)
	re := fl.Get()
	if re == nil {
		t.Fatal("free list empty, want the closed wrapper")
	}
	re.Reinit(pg, root, true)
	defer re.Close()
	if re.keyCompare != nil {
		t.Fatal("Reinit inherited the previous tenant's key comparator")
	}
	if len(re.cursors) != 0 {
		t.Fatalf("Reinit inherited %d cursors", len(re.cursors))
	}
	if re.pageSize != pg.PageSize() || re.usableSize != pg.UsableSize() || re.rootPage != root || re.pager != pg {
		t.Fatal("Reinit did not re-snapshot the geometry from the given pager")
	}
	cur, err := re.OpenCursor()
	if err != nil {
		t.Fatalf("OpenCursor on re-armed wrapper: %v", err)
	}
	found, err := cur.SeekToRowID(3)
	if err != nil || !found {
		t.Fatalf("SeekToRowID(3) on re-armed wrapper: found=%v err=%v", found, err)
	}
	cur.Close()
}
