package btree

import (
	"runtime"
	"sync"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// BTree wrapper and cursor pooling.
//
// Every statement builds fresh BTree wrappers over the same (pager, rootPage)
// pairs and opens short-lived cursors on them; the statement teardown path
// (Engine.releaseStatementTrees -> BTree.Close, and the function-local
// `defer tree.Close()` sites) closes every wrapper deterministically.
//
// CURSORS are pooled (cursorPool below): Close recycles every owned cursor
// and OpenCursor hands out a Reset one, so a busy connection reuses cursor
// objects instead of allocating them per statement.
//
// WRAPPERS are reused through OWNERSHIP-TOKEN free lists (TreeFreeList
// below), never through a global pool. The first wrapper-pooling attempt
// (a global sync.Pool) was removed: a pooled wrapper is re-armed for
// whichever statement Gets it next, so any Close that fires after the
// re-arm — a late second Close from the previous owner — proceeded against
// the NEW owner's live wrapper (the old `closed bool` idempotency cannot
// tell "my close" from "the new owner's state"). The free-list discipline
// closes that hazard structurally:
//
//   - A free list is scoped to ONE owner (an engine's statement funnel, a
//     DML executor's write-tree cache) and is touched only by that owner's
//     single-goroutine statement path. Wrappers never migrate across
//     engines or goroutines.
//   - A wrapper is Put ONLY after Close, at a point where its ownership is
//     provably dead (statement teardown / cache invalidation).
//   - Every acquire bumps the wrapper's arm generation (gen); the tracker
//     records the generation it acquired under (TreeLease). A release whose
//     lease generation no longer matches the wrapper's current generation
//     is STALE — ownership moved — and no-ops: no Close, no Put. This is
//     the belt-and-suspenders gate: even a mis-attributed segment mark or a
//     future double-release cannot close or pool a live successor.
//
// A closed wrapper that is never recycled degrades to the pre-pooling
// behavior (reads through it report errors; the object is simply garbage).

// cursorPool recycles cursors across statements and wrappers. A recycled
// cursor's registry finalizer is installed once at allocation (acquireCursor)
// and never re-set or cleared: re-registration only records regKey, and a
// finalizer queued against a dropped pool batch finds regKey == 0 and
// unregisters nothing. SetFinalizer on a recycled object is exactly what is
// forbidden — see btree_cursor_save.go.
var cursorPool = sync.Pool{
	New: func() interface{} {
		c := &Cursor{
			path: make([]cursorPathEntry, 0, 4),
		}
		runtime.SetFinalizer(c, cursorRegistryFinalizer)
		return c
	},
}

// initFrom initializes a fresh wrapper over the given tree.
func (t *BTree) initFrom(pg *pager.Pager, rootPage uint32, isTable, isSchema bool) *BTree {
	t.pager = pg
	t.rootPage = rootPage
	t.pageSize = pg.PageSize()
	t.usableSize = pg.UsableSize()
	t.isTable = isTable
	t.isSchema = isSchema
	// A statement's wrapper opens a handful of cursors at most; the inline
	// array absorbs them without per-OpenCursor growth AND without the
	// per-wrapper heap slice allocation (NewBTree runs once per statement —
	// one INSERT — so the make showed up verbatim in the insert profile).
	t.cursors = t.cursorsArr[:0]
	return t
}

// TreeFreeList is a single-owner free list of CLOSED BTree wrappers. See the
// file header for the ownership-token discipline; in short: one owner, one
// goroutine (the statement funnel), Put only after Close at a provably-dead
// ownership point, and every Get re-arms through BTree.Reinit (full state
// reset + generation bump). Not synchronized — like the engine's other
// per-statement scratches (cloneScratches, snapBufs), it relies on the
// strictly sequential statement funnel of its owning engine.
type TreeFreeList struct {
	free []*BTree
}

// maxTreeFreeList caps the free list: a statement touches a bounded number
// of trees (its tables + indexes); 64 absorbs every realistic shape while
// bounding idle memory.
const maxTreeFreeList = 64

// Get pops the most recently closed wrapper, or returns nil when the list
// is empty (the caller builds a fresh one via NewBTree).
func (p *TreeFreeList) Get() *BTree {
	if n := len(p.free); n > 0 {
		t := p.free[n-1]
		p.free[n-1] = nil
		p.free = p.free[:n-1]
		return t
	}
	return nil
}

// Put returns a CLOSED wrapper to the free list. Callers must have closed t
// at a point where its ownership is provably dead; Put tolerates an open
// wrapper only by ignoring it (defensive: never pool a live wrapper).
func (p *TreeFreeList) Put(t *BTree) {
	if t == nil || !t.closed || len(p.free) >= maxTreeFreeList {
		return
	}
	p.free = append(p.free, t)
}

// Purge drops every pooled wrapper (without closing them — they are already
// closed), leaving the list empty. Fired on pager layout replacement: a
// pooled wrapper's snapshot geometry (pageSize/usableSize) is stale after an
// in-place layout change, and dropping the cargo is cheaper than reasoning
// about which pagers the change touched. The next Get builds fresh.
func (p *TreeFreeList) Purge() {
	for i := range p.free {
		p.free[i] = nil
	}
	p.free = p.free[:0]
}

// TreeLease is the ownership token for a statement-tracked wrapper: the
// wrapper plus the arm generation its acquirer saw. releaseStatementTrees
// compares the lease against the wrapper's CURRENT generation; a mismatch
// means ownership moved since tracking (a recycled wrapper re-armed for
// someone else) and the release no-ops instead of closing the live owner's
// wrapper. See the file header.
type TreeLease struct {
	Tree *BTree
	Gen  uint64
}

// Generation returns the wrapper's arm generation: bumped by every Reinit
// (and unique per NewBTree lease by construction — 0 for a never-re-armed
// wrapper). Callers that track wrappers across an ownership window record
// it at acquire and re-check at release.
func (t *BTree) Generation() uint64 {
	if t == nil {
		return 0
	}
	return t.gen
}

// Reinit re-arms a CLOSED wrapper for a new (pager, rootPage) ownership —
// the acquire half of the free-list discipline. The reset is FULL: every
// per-ownership field is re-initialized exactly as a fresh NewBTree would
// leave it (geometry re-snapshotted from the CURRENT pager layout, cursor
// registry emptied onto the inline array, key comparator cleared, insert
// scratch depth reset, closed flag cleared); only stateless capacity is
// kept (cursorsArr, insScratch parse slots, quickPageScratch — all
// rewritten before use, mirroring the cursor pool's kept path/buffer
// capacity; cellScratch arrives nil because Close's resetForPool drops it —
// its lifetime is a single insert). No append-cursor invalidation here on
// purpose: the slot is keyed by (pager, root) identity and its trust
// re-derives from page truth on every engagement, so a re-armed wrapper
// must behave exactly like a fresh NewBTree — and initFrom never touches
// the slot. The arm generation bump is what makes a stale lease detectable.
func (t *BTree) Reinit(pg *pager.Pager, rootPage uint32, isTable bool) *BTree {
	t.gen++
	t.initFrom(pg, rootPage, isTable, false)
	t.keyCompare = nil
	t.insDepth = 0
	t.closed = false
	return t
}

// releaseCursors returns the wrapper's cursors to the global cursor pool.
// Every released cursor is reset: a recycled cursor must not carry page
// references, save/restore state, or a registry key. The released marker is
// set BEFORE the Put: a cursor is live pool cargo the moment Put runs, and a
// write after Put races the next acquirer's resetFor (concurrent
// connections' -race stress: the closer's flag write interleaved with the
// acquirer's reset). resetFor clears the flag on acquisition, so the marker
// still survives exactly until the cursor's NEXT acquisition — every use of
// a closed cursor reports an error.
func (t *BTree) releaseCursors(owned []*Cursor) {
	for _, c := range owned {
		c.resetFor(t)
		c.released = true
		cursorPool.Put(c)
	}
}

// acquireCursor returns a reset cursor for a fresh OpenCursor, reusing one
// from the global cursor pool when available.
//
// The pooled cursor's registry safety-net finalizer was installed EXACTLY
// ONCE at allocation and is never re-set or cleared (btree_cursor_save.go):
// re-registration only records regKey, and a finalizer queued against a
// dropped pool batch finds regKey == 0 and unregisters nothing. Per-call
// SetFinalizer on a recycled object is exactly what is forbidden — it races
// the GC sweep cycle and fatally throws "runtime.SetFinalizer: finalizer
// already set" when a stale special survives object reuse.
func (t *BTree) acquireCursor() *Cursor {
	c, _ := cursorPool.Get().(*Cursor)
	if c == nil {
		c = &Cursor{
			// B-trees are shallower than 4 levels in practice; pre-sizing the
			// path stack keeps every descent's appends allocation-free
			// (SeekToRowID clears the slice, not the capacity, so seeks reuse
			// it too).
			path: make([]cursorPathEntry, 0, 4),
		}
		runtime.SetFinalizer(c, cursorRegistryFinalizer)
	}
	c.resetFor(t)
	return c
}

// resetFor returns a cursor to a pristine just-opened state over t. Buffers
// with capacity (path stack, landing-page scratch) are kept; every position,
// cache, and save/restore field is reinitialized so a recycled cursor cannot
// observe a previous tenant's state.
func (c *Cursor) resetFor(t *BTree) {
	c.tx = t
	c.pageNum = t.rootPage
	c.cellIdx = 0
	c.endOfBTree = false
	if c.path != nil {
		c.path = c.path[:0]
	} else {
		c.path = make([]cursorPathEntry, 0, 4)
	}
	c.currentPg = nil
	c.currentPage = nil // pageScratch (buffer) is kept
	c.state = cursorValid
	c.savedRowID = 0
	c.savedKey = nil
	c.skipNext = 0
	c.released = false
	c.regKey = cursorTreeKey{}
}

// resetForPool clears the wrapper's per-tree state at Close. A funnel-owned
// wrapper then goes to its owner's TreeFreeList, whose next Get re-arms it
// through Reinit (generation bump + full reset); any other wrapper stays
// closed until GC. The wrapper itself never re-arms in place at Close: only
// an explicit acquire (Reinit) may revive it, so every Close stays terminal.
// Kept name for the call sites' wording.
func (t *BTree) resetForPool() {
	t.pager = nil
	t.rootPage = 0
	t.pageSize = 0
	t.usableSize = 0
	t.isTable = false
	t.keyCompare = nil
	t.isSchema = false
	t.closed = true
	t.cursors = nil
	t.cellScratch = nil
}

// ReleaseIdleCursors releases every cursor the wrapper owns WITHOUT closing
// the wrapper — the per-statement sweep for a PERSISTENT wrapper
// (execdml's cached write trees). The btree write primitives' internal seek
// cursors (seekLeafRow, DeleteCellByRowID's open) stay owned by the wrapper
// after they return; a statement-local wrapper's Close sweeps them at
// statement teardown, so a cached wrapper needs the explicit sweep after
// each statement — otherwise the leaked cursors accumulate across
// statements and every saveAllCursors pays for the list. Cursor release
// semantics match Close exactly (registry unregistration under the
// registry lock, pool reset with the released marker); the wrapper itself
// stays open and serves the next statement.
func (t *BTree) ReleaseIdleCursors() {
	if t == nil || t.closed || len(t.cursors) == 0 {
		return
	}
	owned := t.cursors
	t.cursors = t.cursors[:0]
	cursorRegMu.Lock()
	for _, c := range owned {
		if c.regKey != (cursorTreeKey{}) {
			removeRegisteredCursor(c.regKey, c)
			c.regKey = cursorTreeKey{}
		}
	}
	cursorRegMu.Unlock()
	t.releaseCursors(owned)
}

// landingScratch returns the cursor's reusable parsed-header scratch,
// allocating it on first use. cachePage and cacheLandingLeaf decode into it
// so positioning a cursor allocates once per cursor lifetime instead of once
// per page visit. Callers never retain a *storage.BTreePage across a cursor
// reposition (every use reads the header fields immediately after
// cachePage/cacheLandingLeaf), so reusing one buffer is observably identical
// to a fresh copy per call.
func (c *Cursor) landingScratch() *storage.BTreePage {
	if c.pageScratch == nil {
		c.pageScratch = new(storage.BTreePage)
	}
	return c.pageScratch
}
