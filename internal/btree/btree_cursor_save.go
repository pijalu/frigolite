package btree

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// Cross-statement cursor invalidation (btree.c saveAllCursors).
//
// A nested statement (an eval() UDF body, a trigger, or any DML invoked from
// expression evaluation) may modify a table b-tree while an enclosing
// statement's cursor is still positioned inside it: cells move on page
// defragmentation, leaves are freed on a full DELETE, and interior routing
// changes on splits. Reading the enclosing cursor's cached (page, cellIdx)
// afterwards reports corruption on what SQLite treats as a normal mid-scan
// write (misc8-1.6: SELECT ... eval('DELETE FROM t1') must succeed).
//
// btree.c solves this by walking BtShared's cursor list before every write
// (saveAllCursors): each other cursor's position is saved as a KEY
// (saveCursorPosition, state CURSOR_REQUIRESEEK), and the next use of the
// cursor re-seeks to that key (restoreCursorPosition). When the exact key no
// longer exists, the seek leaves the cursor at the next-larger entry and
// records skipNext>0 so the pending BtreeNext returns that entry instead of
// stepping past it (btree.c btreeNext's CURSOR_SKIPNEXT branch).
//
// frigolite creates a fresh BTree wrapper per statement over the same
// (pager, rootPage) pair — that pair is the BtShared identity — so the cursor
// list lives in a registry keyed on it. SQLite closes a statement's cursors
// when its program halts, so its saveAllCursors only ever sees live cursors;
// frigolite mirrors that with BTree.Close at statement teardown, which
// unregisters every cursor the closed wrapper owns. Without it the registry
// only ever shrank through the (lazy) runtime finalizer, and each mutation's
// saveAllCursors walked every cursor ever opened on the tree.

type cursorTreeKey struct {
	pg   *pager.Pager
	root uint32
}

// cursorState mirrors the btree.c eState values this port needs.
type cursorState int8

const (
	cursorValid       cursorState = iota // positioned; page references trusted
	cursorRequireSeek                    // position saved as a key; pages released
)

var (
	cursorRegMu    sync.Mutex
	cursorRegistry = map[cursorTreeKey][]*Cursor{}
)

// registerTreeCursor adds a cursor to its tree's invalidation list. The list
// entry is removed deterministically by BTree.Close (statement teardown,
// btree.c sqlite3VdbeFrameDelete/closeCursorsInFrame); a cursor finalizer
// (set once at allocation, btree_pool.go newCursor) stays as the safety net
// for wrappers that are never closed (out-of-statement use).
//
// Registration itself must NOT call SetFinalizer: cursors are pooled and
// recycled across statements (btree_pool.go), and setting/clearing a
// finalizer per registration on a recycled object races the GC sweep cycle
// (a special can outlive its object through the pool drop at poolCleanup,
// surfacing as the fatal "runtime.SetFinalizer: finalizer already set" on
// the NEXT registration — 2 of 4 full-suite runs crashed there). The
// finalizer reads c.regKey AT RUN TIME instead: registration just records
// the key, Close/resetFor zero it once the registration is gone, so a late
// (queued) finalizer unregisters nothing.
//
// The key rides on the cursor so the finalizer can be a static function —
// a per-registration closure would allocate on every OpenCursor.
func registerTreeCursor(key cursorTreeKey, c *Cursor) {
	c.regKey = key
	cursorRegMu.Lock()
	defer cursorRegMu.Unlock()
	cursorRegistry[key] = append(cursorRegistry[key], c)
}

// cursorRegistryFinalizer is the cursor's allocation-time finalizer (set once
// in newCursor, never re-set or cleared). It unregisters the cursor from the
// invalidation registry under the key of its CURRENT registration; a zero
// key (the cursor was recycled, closed, or never registered since reset)
// unregisters nothing, so a finalizer queued while the cursor sat in a
// dropped pool batch cannot disturb a later registration.
func cursorRegistryFinalizer(cc *Cursor) {
	if cc.regKey == (cursorTreeKey{}) {
		return
	}
	unregisterTreeCursor(cc.regKey, cc)
}

// unregisterTreeCursor removes a cursor from its tree's invalidation list.
func unregisterTreeCursor(key cursorTreeKey, c *Cursor) {
	cursorRegMu.Lock()
	defer cursorRegMu.Unlock()
	removeRegisteredCursor(key, c)
}

// removeRegisteredCursor is unregisterTreeCursor without the lock (the caller
// already holds cursorRegMu). An emptied list stays in the map (with spare
// capacity): statements rebuild their cursors on the same (pager, rootPage)
// key over and over, and re-growing the slice from nil per statement was a
// measurable per-OpenCursor allocation.
func removeRegisteredCursor(key cursorTreeKey, c *Cursor) {
	list := cursorRegistry[key]
	for i, cc := range list {
		if cc == c {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	cursorRegistry[key] = list
}

// Close releases the wrapper's cursors deterministically at statement
// teardown (btree.c closes a statement's cursors when its VDBE program
// halts — vdbeaux.c closeCursorsInFrame). Every cursor opened from this
// wrapper is removed from the cross-statement invalidation registry and
// marked released: writers no longer save its position, and any later use
// reports an error instead of silently reading pages that may hold other
// rows. Close is idempotent. The registry itself is untouched for OTHER
// wrappers — an enclosing statement's positioned cursor must still be
// saved/restored by a nested statement's write (misc8-1.6 contract).
//
// Close is terminal: after it runs no wrapper or cursor reference is used
// again (the statement-tracking funnel truncates its slice; every
// function-local site drops the variable), so the wrapper and its cursors
// are recycled through the btree pool (btree_pool.go).
func (t *BTree) Close() {
	if t == nil || t.closed {
		return
	}
	t.closed = true
	owned := t.cursors
	t.cursors = t.cursors[:0]
	if len(owned) > 0 {
		cursorRegMu.Lock()
		for _, c := range owned {
			// Unregister under the key the cursor was REGISTERED under,
			// not the wrapper's current (pager, rootPage): schemaCursor
			// opens a schema-keyed cursor on a user-tree wrapper while its
			// rootPage is temporarily 1, so the wrapper's rootPage at Close
			// time can differ from a cursor's registration key.
			removeRegisteredCursor(c.regKey, c)
			// Zero the key BEFORE the cursor is recycled: the allocation-time
			// finalizer (newCursor) reads regKey at run time, so a finalizer
			// queued against this cursor must find no live registration once
			// Close has run.
			c.regKey = cursorTreeKey{}
		}
		cursorRegMu.Unlock()
	}
	t.releaseCursors(owned)
	// releaseCursors reset each cursor into the free list (clearing page
	// references and the registration key); re-mark them released AFTER that
	// so the marker survives until the cursor's NEXT acquisition — every use
	// of a closed cursor reports an error (checkOpen / restoreIfNeeded /
	// cachePage), and the wrapper re-entry in resetForPool must not leave a
	// readable cursor behind.
	for _, c := range owned {
		c.released = true
	}
	t.resetForPool()
}

// saveAllCursors saves the positions of every positioned cursor open on this
// tree except the one being used to perform the write (btree.c
// saveAllCursors). Called at the top of every public mutation entry point.
// With statement-scoped teardown the registry holds only live cursors, so
// the walk is short; the fast path returns before allocating when no cursor
// needs saving.
func (t *BTree) saveAllCursors() {
	key := cursorTreeKey{pg: t.pager, root: t.rootPage}
	cursorRegMu.Lock()
	list := cursorRegistry[key]
	n := 0
	for _, c := range list {
		if c.state == cursorValid && !c.endOfBTree {
			n++
		}
	}
	if n == 0 {
		cursorRegMu.Unlock()
		return
	}
	targets := make([]*Cursor, 0, n)
	for _, c := range list {
		if c.state == cursorValid && !c.endOfBTree {
			targets = append(targets, c)
		}
	}
	cursorRegMu.Unlock()
	for _, c := range targets {
		c.saveCursorPosition()
	}
}

// saveCursorPosition records the cursor's current key and releases its page
// references (btree.c saveCursorPosition): the position becomes
// cursorRequireSeek and the next use re-seeks.
func (c *Cursor) saveCursorPosition() {
	if c.state != cursorValid || c.endOfBTree {
		return
	}
	rowID, key, err := c.currentKey()
	if err != nil {
		// The position cannot be captured (a previous statement already
		// damaged the page): leave the cursor valid; its next read reports
		// the corruption as before.
		return
	}
	c.savedRowID = rowID
	c.savedKey = key
	c.skipNext = 0
	c.state = cursorRequireSeek
	c.clearPageCache()
	c.path = nil
}

// currentKey extracts the seek key at the cursor position: the rowid for
// table b-trees, a private copy of the full key payload for index b-trees.
func (c *Cursor) currentKey() (int64, []byte, error) {
	if err := c.cachePage(); err != nil {
		return 0, nil, err
	}
	pg := c.currentPg
	page := c.currentPage
	if c.cellIdx < 0 || c.cellIdx >= int(page.CellCount) {
		return 0, nil, fmt.Errorf("btree: cursor at end")
	}
	if page.PageType == storage.PageTypeLeafTable {
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), c.cellIdx, int(c.tx.pageSize)))
		if cellOff < 0 || cellOff >= len(pg.Data) {
			return 0, nil, fmt.Errorf("btree: cursor at end")
		}
		// Skip the payload-length varint, then read the rowid varint
		// (tableLeafCellHeader without the payload work).
		data := pg.Data[cellOff:]
		_, n := util.GetVarint(data)
		pos := cellOff + n
		if pos >= len(pg.Data) {
			return 0, nil, fmt.Errorf("btree: cursor at end")
		}
		rowID, _ := util.GetVarint(pg.Data[pos:])
		return int64(rowID), nil, nil
	}
	cell, err := c.ReadCell()
	if err != nil {
		return 0, nil, err
	}
	if c.tx.isTable {
		return cell.RowID, nil, nil
	}
	key := make([]byte, len(cell.Payload))
	copy(key, cell.Payload)
	return cell.RowID, key, nil
}

// restoreIfNeeded re-seeks a saved cursor to its recorded key before the
// cursor's pages are used again (btree.c restoreCursorPosition). A cursor
// released by its owner's Close is rejected here: this is the one checkpoint
// every cursor use (Next/Prev/ReadCell/ReadCellData) already passes through.
// After the restore, skipNext carries the moveto bias: +1 when the exact key
// is gone and the cursor sits on the next-larger entry (the pending Next
// returns it without advancing), -1 for the Previous mirror.
func (c *Cursor) restoreIfNeeded() error {
	if c.released {
		return fmt.Errorf("btree: cursor used after close")
	}
	if c.state != cursorRequireSeek {
		return nil
	}
	rowID := c.savedRowID
	key := c.savedKey
	c.savedKey = nil
	c.savedRowID = 0
	c.clearPageCache()
	c.path = nil
	c.state = cursorValid
	c.skipNext = 0
	var found bool
	var err error
	if c.tx.isTable {
		found, err = c.seekTableLeafWithPath(c.tx.rootPage, rowID)
	} else {
		found, err = c.seekIndexLeafWithPath(c.tx.rootPage, key)
	}
	if err != nil {
		// The tree is gone or unreadable (every row deleted freed the
		// root): report EOF through the same state OpenCursor uses for a
		// failed descent.
		c.endOfBTree = true
		return nil
	}
	if !found && !c.endOfBTree {
		c.skipNext = 1
	}
	return nil
}

// seekTableLeafWithPath is SeekToRowID with the cursor path stack maintained
// (btree.c sqlite3BtreeTableMoveto): interior levels push
// {pageNum, childIdx} entries so navigateToNextChild can continue the scan
// into the following leaves after the restore. Page headers parse into a
// stack scratch — the parsed page only feeds the read-only
// seekInLeafTable/routeInteriorTable helpers and never outlives the level.
func (c *Cursor) seekTableLeafWithPath(pageNum uint32, rowID int64) (bool, error) {
	var sp storage.BTreePage
	for {
		pg, err := c.tx.pager.ReadPage(pageNum)
		if err != nil {
			c.endOfBTree = true
			return false, err
		}
		page, err := storage.ParsePageInto(pg.Data, int(c.tx.pageSize), contentOffset(pg.PageNum), &sp)
		if err != nil {
			c.endOfBTree = true
			return false, err
		}
		if page.PageType == storage.PageTypeLeafTable {
			return c.seekInLeafTable(pg, page, rowID)
		}
		if page.PageType != storage.PageTypeInteriorTable {
			c.endOfBTree = true
			return false, nil
		}
		// Route to the child holding rowID: the first cell whose separator
		// key is >= rowID (its left child), else the rightmost pointer.
		lo, childPage := c.routeInteriorTable(pg, page, rowID)
		c.path = append(c.path, cursorPathEntry{pageNum: pg.PageNum, childIdx: lo})
		pageNum = childPage
	}
}

// routeInteriorTable computes the descent for an interior table page: the
// (child index, child page) pair for rowID, matching seekInInteriorTable's
// separator convention.
func (c *Cursor) routeInteriorTable(pg *pager.Page, page *storage.BTreePage, rowID int64) (int, uint32) {
	lo, hi := 0, int(page.CellCount)-1
	childPage := page.RightmostPtr
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum)+cellPtrOffset(page.PageType)-8, mid, int(c.tx.pageSize)))
		midRowID, _ := util.GetVarint(pg.Data[cellOff+4:])
		if int64(midRowID) < rowID {
			lo = mid + 1
		} else {
			childPage = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			hi = mid - 1
		}
	}
	if lo < int(page.CellCount) {
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum)+cellPtrOffset(page.PageType)-8, lo, int(c.tx.pageSize)))
		childPage = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
	}
	if childPage == 0 {
		childPage = page.RightmostPtr
	}
	return lo, childPage
}

// seekIndexLeafWithPath is the index-b-tree mirror of seekTableLeafWithPath
// (routing on full key comparisons, dividers reassembled from overflow).
func (c *Cursor) seekIndexLeafWithPath(pageNum uint32, key []byte) (bool, error) {
	for {
		pg, page, err := c.readTreePage(pageNum)
		if err != nil {
			return false, err
		}
		if page.PageType == storage.PageTypeLeafIndex {
			return c.seekInLeafIndex(pg, page, key)
		}
		if page.PageType != storage.PageTypeInteriorIndex {
			c.endOfBTree = true
			return false, nil
		}
		lo, childPage, err := c.routeInteriorIndex(pg, page, key)
		if err != nil {
			return false, err
		}
		c.path = append(c.path, cursorPathEntry{pageNum: pg.PageNum, childIdx: lo})
		pageNum = childPage
	}
}

// readTreePage reads and parses one b-tree page for the seek-with-path walk.
func (c *Cursor) readTreePage(pageNum uint32) (*pager.Page, *storage.BTreePage, error) {
	pg, err := c.tx.pager.ReadPage(pageNum)
	if err != nil {
		c.endOfBTree = true
		return nil, nil, err
	}
	page, err := pg.ParsedBTree(int(c.tx.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		c.endOfBTree = true
		return nil, nil, err
	}
	return pg, page, nil
}

// routeInteriorIndex computes the descent for an interior index page: the
// (child index, child page) pair for key, matching seekInInteriorIndex's
// routing on full (overflow-reassembled) divider comparisons. Divider
// convention (splitMedianKey): keys < D live left of D, keys >= D right —
// equal keys go right, because the divider is a COPY of the right sibling's
// first key and the equal entry lives in that sibling.
func (c *Cursor) routeInteriorIndex(pg *pager.Page, page *storage.BTreePage, key []byte) (int, uint32, error) {
	lo, hi := 0, int(page.CellCount)-1
	childPage := page.RightmostPtr
	for lo <= hi {
		mid := (lo + hi) / 2
		cell, err := c.interiorIndexCell(pg, mid)
		if err != nil {
			return 0, 0, err
		}
		if c.tx.compareKey(cell.key, key) <= 0 {
			lo = mid + 1
		} else {
			childPage = cell.leftPtr
			hi = mid - 1
		}
	}
	return lo, childPage, nil
}

// interiorIndexCell decodes one interior index cell (divider) and reassembles
// its spilled payload, returning the comparison key and left-child pointer.
func (c *Cursor) interiorIndexCell(pg *pager.Page, idx int) (struct {
	leftPtr uint32
	key     []byte
}, error) {
	var out struct {
		leftPtr uint32
		key     []byte
	}
	// Interior pages keep their cell-pointer array at coff+12 (CellPointer's
	// base is arrayStart-8); the page type is read from the header byte.
	pageType := pg.Data[contentOffset(pg.PageNum)]
	cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum)+cellPtrOffset(pageType)-8, idx, int(c.tx.pageSize)))
	cell, err := storage.DecodeCell(pg.Data, cellOff, storage.CellIndexInterior, int(c.tx.usableSize))
	if err != nil {
		c.endOfBTree = true
		return out, err
	}
	full, oerr := c.tx.readOverflow(cell)
	if oerr != nil {
		c.endOfBTree = true
		return out, oerr
	}
	out.leftPtr = cell.LeftPtr
	out.key = full.Payload
	return out, nil
}
