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
// `defer tree.Close()` sites) closes every wrapper deterministically. That
// deterministic lifecycle makes the objects poolable: Close is terminal — a
// closed wrapper's cursors are released (unregistered from the cross-statement
// invalidation registry, marked released) and no code path uses a wrapper or
// cursor after its Close — so Close can hand both back to a pool and NewBTree
// can hand out a Reset one.
//
// The reset contract (resetFor / Cursor.resetFor) reinitializes EVERY field;
// buffers with capacity (the wrapper's cursor list, the cursor free list, the
// cursor's path stack, the landing-page scratch) are kept and truncated so a
// reused wrapper costs zero allocations.

// btreePool recycles BTree wrappers across statements.
var btreePool = sync.Pool{
	New: func() interface{} { return new(BTree) },
}

// initFrom initializes a (fresh or recycled) wrapper over the given tree.
func (t *BTree) initFrom(pg *pager.Pager, rootPage uint32, isTable, isSchema bool) *BTree {
	probeInit(t, pg)
	t.pager = pg
	t.rootPage = rootPage
	t.pageSize = pg.PageSize()
	t.usableSize = pg.UsableSize()
	t.isTable = isTable
	// SetKeyCompare installs a per-tree comparator right after construction;
	// a recycled wrapper must not inherit the previous tenant's.
	t.keyCompare = nil
	t.isSchema = isSchema
	t.closed = false
	if t.cursors == nil {
		// A statement's wrapper opens a handful of cursors at most; the
		// pre-sized slice absorbs them without per-OpenCursor growth.
		// Recycled wrappers keep the capacity they grew to.
		t.cursors = make([]*Cursor, 0, 4)
	}
	return t
}

// releaseCursors returns the wrapper's cursors to its free list without
// killing the wrapper (the registry unregistration happens in Close before
// this runs). Every released cursor is reset: a recycled cursor must not
// carry page references, save/restore state, or a registry key.
func (t *BTree) releaseCursors(owned []*Cursor) {
	for _, c := range owned {
		c.resetFor(t)
		t.cursorFree = append(t.cursorFree, c)
	}
}

// resetForPool clears the wrapper's per-tree state before it re-enters the
// pool (the pooled object must not retain the last tenant's tree identity or
// pager references; cursorFree keeps only reset cursors).
func (t *BTree) resetForPool() {
	probePool(t)
	t.pager = nil
	t.rootPage = 0
	t.pageSize = 0
	t.usableSize = 0
	t.isTable = false
	t.keyCompare = nil
	t.isSchema = false
	t.closed = true // stays closed until a NewBTree reset re-arms it
	btreePool.Put(t)
}

// acquireCursor returns a reset cursor for a fresh OpenCursor, reusing one
// from the free list when available.
func (t *BTree) acquireCursor() *Cursor {
	if n := len(t.cursorFree); n > 0 {
		c := t.cursorFree[n-1]
		t.cursorFree = t.cursorFree[:n-1]
		c.resetFor(t)
		return c
	}
	return t.newCursor()
}

// newCursor builds a bare cursor over t (fresh allocation path).
//
// The registry safety-net finalizer is installed EXACTLY ONCE here, at
// allocation, and never re-set or cleared (btree_cursor_save.go): the cursor
// is recycled across statements, and per-registration SetFinalizer calls on
// a recycled object race the GC sweep cycle (a special can outlive its
// object through the pool drop at poolCleanup — the fatal "runtime.
// SetFinalizer: finalizer already set" on the next registration). The
// finalizer reads c.regKey at run time; resetFor and Close zero the key, so
// it unregisters nothing once the cursor left its registered life.
func (t *BTree) newCursor() *Cursor {
	c := &Cursor{
		tx:      t,
		pageNum: t.rootPage,
		cellIdx: 0,
		// B-trees are shallower than 4 levels in practice; pre-sizing the
		// path stack keeps every descent's appends allocation-free (SeekToRowID
		// clears the slice, not the capacity, so seeks reuse it too).
		path: make([]cursorPathEntry, 0, 4),
	}
	runtime.SetFinalizer(c, cursorRegistryFinalizer)
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
