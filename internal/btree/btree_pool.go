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
// WRAPPERS are pooled too (wrapperPool below), with a strict
// ownership-transfer contract — the shape that history (the P1
// use-after-pool) showed to be the only safe one:
//
//   - NO finalizer ever touches a wrapper. The P1 was a SetFinalizer on a
//     recycled object (double-set fatal) plus a finalizer closing a wrapper
//     that a later tenant already owned. Wrappers carry no finalizer and no
//     registry entry of their own; their cursors' allocation-time finalizers
//     read the cursor's key at run time and unregister nothing once Close
//     has recycled them.
//   - A wrapper enters the pool EXACTLY ONCE, from Close, under the
//     t.closed transition guard. Double Close is a no-op and never re-Puts.
//   - A wrapper leaves the pool ONLY through NewBTree/NewSchemaBTree, which
//     re-arm every field in initFrom — including the state a previous
//     tenant could have customized (keyCompare) or buffered (cellScratch,
//     delArena) — so a recycled wrapper is byte-for-byte indistinguishable
//     from a fresh one.
//   - Ownership is linear per wrapper: NewBTree (one owner: the statement)
//     -> Close (pool return). The engine's statement funnel guarantees the
//     closing frame is the creating frame; nothing holds a wrapper across
//     statements. A stale reference used after its owner closed would
//     alias a later tenant — the same discipline the cursor pool (and every
//     per-engine cache) already relies on.

// wrapperPool recycles statement-scoped BTree wrappers. A recycled wrapper
// is fully re-initialized by initFrom before it becomes visible to its next
// owner; no finalizer is ever set on a wrapper (see the ownership contract
// above).
var wrapperPool = sync.Pool{
	New: func() interface{} { return new(BTree) },
}

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

// initFrom initializes a fresh wrapper over the given tree. It re-arms EVERY
// field a wrapper carries — including the previous tenant's customizations
// (keyCompare) and reusable buffers (cellScratch, delArena) — so a recycled
// wrapper is indistinguishable from a fresh allocation.
func (t *BTree) initFrom(pg *pager.Pager, rootPage uint32, isTable, isSchema bool) *BTree {
	t.pager = pg
	t.rootPage = rootPage
	t.pageSize = pg.PageSize()
	t.usableSize = pg.UsableSize()
	t.isTable = isTable
	t.isSchema = isSchema
	t.keyCompare = nil
	t.closed = false
	// A statement's wrapper opens a handful of cursors at most; the pre-sized
	// slice absorbs them without per-OpenCursor growth. A recycled wrapper
	// keeps its backing array (Close truncated it to :0).
	if cap(t.cursors) >= 4 {
		t.cursors = t.cursors[:0]
	} else {
		t.cursors = make([]*Cursor, 0, 4)
	}
	// Stale encode/delete staging from a previous tenant must never leak:
	// both are per-statement buffers dropped on Close, and re-arming clears
	// them (kept capacity would be safe only for cellScratch's reset-in-place
	// uses; delArena holds raw page spans that must never alias).
	t.cellScratch = nil
	t.delArena = nil
	return t
}

// releaseCursors returns the wrapper's cursors to the global cursor pool.
// Every released cursor is reset: a recycled cursor must not carry page
// references, save/restore state, or a registry key.
func (t *BTree) releaseCursors(owned []*Cursor) {
	for _, c := range owned {
		c.resetFor(t)
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

// resetForPool clears the wrapper's per-tree state and returns it to the
// wrapper pool. Called EXACTLY ONCE per wrapper generation, from Close under
// the t.closed transition guard (see the ownership contract in the file
// header): a double Close is a no-op and never re-Puts, and the next owner
// receives the wrapper only through NewBTree/NewSchemaBTree's initFrom, which
// re-arms every field. Kept name for the call sites' wording.
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
	t.delArena = nil
	wrapperPool.Put(t)
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
