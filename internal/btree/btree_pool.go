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
// WRAPPERS are deliberately NOT pooled: a pooled wrapper is re-armed for
// whichever statement Gets it next, so any Close that races a statement
// still holding the wrapper (the engine's funnel assumes statements nest
// strictly; concurrent Exec frames or a mis-attributed segment mark) turns
// "a closed wrapper" into "a live statement's pager pointer vanished" —
// a nil-pager SIGSEGV deep in the pager. A closed wrapper that is NOT
// recycled degrades to the pre-pooling behavior (reads through it report
// errors; the object is simply garbage). The per-wrapper cursorFree list is
// therefore dead weight; the global cursorPool replaces it.

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

// resetForPool clears the wrapper's per-tree state before it becomes
// garbage. The wrapper is NOT returned to a pool (see the file header): a
// recycled wrapper re-armed under a racing Close is the crash this package
// guards against. It stays closed until a NewBTree reset re-arms a FRESH
// wrapper. Kept name for the call sites' wording.
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
