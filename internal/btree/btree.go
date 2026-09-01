// Package btree implements a B+Tree on top of the pager.
// It provides cursor-based access for both table and index b-trees.
package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// contentOffset returns the b-tree page header offset for a page number.
// Page 1 has a 100-byte database header before the b-tree content.
func contentOffset(pageNum uint32) int {
	if pageNum == 1 {
		return pager.HeaderSize
	}
	return 0
}

// Cursor provides sequential access to b-tree entries.
type Cursor struct {
	tx         *BTree
	pageNum    uint32 // current leaf page
	cellIdx    int
	endOfBTree bool

	// Path stack for multi-level tree traversal. Each entry records an
	// interior page and the child index within it that we descended through.
	// The stack grows as we descend and shrinks as we ascend.
	path []cursorPathEntry

	// Cache for current page to avoid repeated ParsePage calls
	currentPg   *pager.Page
	currentPage *storage.BTreePage
}

// cursorPathEntry records one level of the traversal path.
type cursorPathEntry struct {
	pageNum  uint32 // interior page number
	childIdx int    // which child we are currently visiting
}

// cachePage caches the parsed page for the current pageNum.
// If the pageNum has changed since last call, re-reads and re-parses.
func (c *Cursor) cachePage() error {
	if c.currentPg != nil && c.currentPg.PageNum == c.pageNum {
		return nil // cache hit
	}
	pg, err := c.tx.pager.ReadPage(c.pageNum)
	if err != nil {
		return err
	}
	page, err := storage.ParsePage(pg.Data, int(c.tx.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return err
	}
	c.currentPg = pg
	c.currentPage = page
	return nil
}

// clearPageCache invalidates the page cache. Used when pageNum changes.
func (c *Cursor) clearPageCache() {
	c.currentPg = nil
	c.currentPage = nil
}

// BTree represents a single b-tree (table or index).
type BTree struct {
	pager      *pager.Pager
	rootPage   uint32
	pageSize   uint32
	usableSize uint32 // pageSize - reserved bytes (payload math, SQLite usable-size formulas)
	isTable    bool   // true for table b-trees, false for index b-trees
}

// NewBTree creates a new BTree instance.
func NewBTree(pg *pager.Pager, rootPage uint32, isTable bool) *BTree {
	return &BTree{
		pager:      pg,
		rootPage:   rootPage,
		pageSize:   pg.PageSize(),
		usableSize: pg.UsableSize(),
		isTable:    isTable,
	}
}

// OpenCursor creates a new cursor positioned at the beginning.
func (t *BTree) OpenCursor() (*Cursor, error) {
	c := &Cursor{
		tx:      t,
		pageNum: t.rootPage,
		cellIdx: 0,
	}
	// Descend from root to the leftmost leaf, building the path stack
	if err := c.descendToFirstLeaf(); err != nil {
		return nil, err
	}
	return c, nil
}

// descendToFirstLeaf navigates from the current page to the leftmost leaf,
// pushing interior pages onto the path stack. Used during OpenCursor.
func (c *Cursor) descendToFirstLeaf() error {
	for {
		pg, err := c.tx.pager.ReadPage(c.pageNum)
		if err != nil {
			c.endOfBTree = true
			return err
		}
		coff := contentOffset(pg.PageNum)
		page, err := storage.ParsePage(pg.Data, int(c.tx.pageSize), coff)
		if err != nil {
			c.endOfBTree = true
			return err
		}
		if page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeInteriorIndex {
			// Leaf page — done
			c.cellIdx = 0
			c.endOfBTree = false
			return nil
		}
		// Interior page — descend to first child (child index 0)
		c.path = append(c.path, cursorPathEntry{pageNum: pg.PageNum, childIdx: 0})
		if page.CellCount > 0 {
			// CellPointer adds 8 to the offset parameter. For interior pages
			// (cellPtrOffset=12), pass coff+4 to get coff+12.
			cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, 0, int(c.tx.pageSize)))
			c.pageNum = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		} else if page.RightmostPtr != 0 {
			// Empty interior page with a rightmost-child: descend to it.
			// An empty interior page with RightmostPtr == 0 is a fully
			// empty btree (root collapse after DELETE all freed every
			// leaf); the cursor should report EOF rather than try to
			// read page 0, which pager.ReadPage rejects with
			// "database disk image is malformed" (see
			// clearEmptyRootRightmost + DELETE-all reproducer in
			// internal/exec/btree_vacuum_corruption_test.go).
			c.pageNum = page.RightmostPtr
		} else {
			// Empty interior page with no children: EOF.
			c.endOfBTree = true
			return nil
		}
	}
}

// navigateToNextChild advances the cursor to the next leaf in sequence.
// This handles multi-level trees by walking up the path stack to find the
// next sibling, then descending to its leftmost leaf.
func (c *Cursor) navigateToNextChild() {
	// Walk up the path stack to find the next child to visit
	for len(c.path) > 0 {
		top := &c.path[len(c.path)-1]

		pg, err := c.tx.pager.ReadPage(top.pageNum)
		if err != nil {
			c.endOfBTree = true
			return
		}
		coff := contentOffset(pg.PageNum)
		page, err := storage.ParsePage(pg.Data, int(c.tx.pageSize), coff)
		if err != nil {
			c.endOfBTree = true
			return
		}

		top.childIdx++
		if top.childIdx < int(page.CellCount) {
			// Navigate to cell[top.childIdx].leftChild
			cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, top.childIdx, int(c.tx.pageSize)))
			c.pageNum = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			c.cellIdx = 0
			c.endOfBTree = false
			// Descend to leftmost leaf from here
			c.descendToFirstLeafFromCurrent()
			return
		} else if top.childIdx == int(page.CellCount) {
			// Navigate to the rightmost pointer
			c.pageNum = page.RightmostPtr
			c.cellIdx = 0
			c.endOfBTree = false
			c.descendToFirstLeafFromCurrent()
			return
		}
		// This interior page is exhausted — pop and try parent
		c.path = c.path[:len(c.path)-1]
	}
	c.endOfBTree = true
}

// descendToFirstLeafFromCurrent descends from the current page to the leftmost
// leaf, pushing interior pages onto the path stack. The current page may be
// a leaf or interior.
func (c *Cursor) descendToFirstLeafFromCurrent() {
	for {
		pg, err := c.tx.pager.ReadPage(c.pageNum)
		if err != nil {
			c.endOfBTree = true
			return
		}
		coff := contentOffset(pg.PageNum)
		page, err := storage.ParsePage(pg.Data, int(c.tx.pageSize), coff)
		if err != nil {
			c.endOfBTree = true
			return
		}
		if page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeInteriorIndex {
			return // leaf
		}
		// Interior — descend to first child
		c.path = append(c.path, cursorPathEntry{pageNum: pg.PageNum, childIdx: 0})
		if page.CellCount > 0 {
			cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, 0, int(c.tx.pageSize)))
			c.pageNum = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		} else if page.RightmostPtr != 0 {
			c.pageNum = page.RightmostPtr
		} else {
			// Empty interior page with no children: EOF.
			c.endOfBTree = true
			return
		}
	}
}

// RootPage returns the current root page number (may change after splits).
func (t *BTree) RootPage() uint32 {
	return t.rootPage
}

// LastRowID returns the largest rowid in the table b-tree (SQLite's
// sqlite3BtreeLast + sqlite3BtreeKeySize). It walks the rightmost child chain
// to the last leaf and reads its final cell — O(depth), not a full scan (the
// engine's nextFTSBlockID previously scanned every %_segments row per flush,
// O(n) per flush, O(n^2) over the automerge's many flushes).
//
// A per-row DELETE can leave the rightmost leaf EMPTY (the engine's delete
// does not collapse/rebalance interior levels); SQLite's cursor would move
// left in that case, so the walk falls back to the interior page's last cell
// child until a NON-EMPTY leaf is found. Returning 0 for a tree whose last
// leaf is empty made nextFTSBlockID allocate block 1 and overwrite live
// blocks (fts4merge 1.4: the merge=1 continuation deleted the output's
// blocks 27-28, the emptied leaf reported max=0, and the rebuilt output was
// written over blocks 1-3).
func (t *BTree) LastRowID() (int64, error) {
	return t.lastRowIDFrom(t.rootPage, 0)
}

// lastRowIDFrom returns the largest rowid in the subtree rooted at pageNum.
// depth guards against corrupt cycles.
func (t *BTree) lastRowIDFrom(pageNum uint32, depth int) (int64, error) {
	if depth > 64 {
		return 0, fmt.Errorf("btree: interior page chain too deep")
	}
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return 0, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return 0, err
	}
	switch page.PageType {
	case storage.PageTypeInteriorTable:
		if page.RightmostPtr != 0 {
			if id, err := t.lastRowIDFrom(page.RightmostPtr, depth+1); err == nil && id > 0 {
				return id, nil
			} else if err != nil {
				return 0, err
			}
		}
		// The rightmost subtree is empty (or absent): walk the interior
		// cells high-to-low until a non-empty subtree is found.
		for i := int(page.CellCount) - 1; i >= 0; i-- {
			cellOff := int(storage.CellPointer(pg.Data, coff+4, i, int(t.pageSize)))
			child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			if child == 0 {
				continue
			}
			id, cerr := t.lastRowIDFrom(child, depth+1)
			if cerr != nil {
				return 0, cerr
			}
			if id > 0 {
				return id, nil
			}
		}
		return 0, nil // the whole subtree is empty
	case storage.PageTypeLeafTable:
		if page.CellCount == 0 {
			return 0, nil
		}
		last := int(page.CellCount) - 1
		cellOff := int(storage.CellPointer(pg.Data, coff, last, int(t.pageSize)))
		_, n := util.GetVarint(pg.Data[cellOff:])
		rowID, _ := util.GetVarint(pg.Data[cellOff+n:])
		return int64(rowID), nil
	default:
		return 0, fmt.Errorf("btree: unexpected page type 0x%02x", page.PageType)
	}
}

// Clear empties the b-tree, resetting the root page to a single empty leaf
// (SQLite's sqlite3BtreeClearTable / the btree root becoming an empty leaf).
// The old interior nodes and leaf pages are left allocated (their content is
// overwritten by future inserts); only the root page is rewritten, so the
// schema's rootpage stays valid and the tree is structurally clean — a
// per-row DELETE leaves stale interior boundary keys that make SeekToRowID
// miss rows after the table is repopulated (fts4merge4's between-scenario
// DELETE FROM %_segments: 72 of 187 blocks became unfindable).
func (t *BTree) Clear() error {
	pg, err := t.pager.ReadPage(t.rootPage)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	data := pg.Data
	pageType := storage.PageTypeLeafTable
	if !t.isTable {
		pageType = storage.PageTypeLeafIndex
	}
	data[coff] = pageType
	data[coff+1] = 0 // first freeblock
	data[coff+2] = 0
	data[coff+3] = 0 // cell count
	data[coff+4] = 0
	usable := int(t.pageSize) - coff
	data[coff+5] = byte(usable >> 8) // cell content offset = usable size
	data[coff+6] = byte(usable)
	data[coff+7] = 0 // fragmented free bytes
	// Zero the cell pointer area.
	for i := coff + 8; i < coff+8+2*4096 && i < usable; i += 2 {
		data[i] = 0
		data[i+1] = 0
	}
	return t.pager.WritePage(pg)
}

// SeekToRowID positions the cursor at the entry with the given rowid (table
// b-trees only). Returns true if found.
func (c *Cursor) SeekToRowID(rowID int64) (bool, error) {
	return c.seekInPage(c.tx.rootPage, rowID)
}

func (c *Cursor) seekInPage(pageNum uint32, rowID int64) (bool, error) {
	pg, err := c.tx.pager.ReadPage(pageNum)
	if err != nil {
		return false, err
	}

	page, err := storage.ParsePage(pg.Data, int(c.tx.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return false, err
	}

	switch page.PageType {
	case storage.PageTypeLeafTable:
		return c.seekInLeafTable(pg, page, rowID)
	case storage.PageTypeInteriorTable:
		return c.seekInInteriorTable(pg, page, rowID)
	default:
		return false, fmt.Errorf("btree: unexpected page type 0x%02x", page.PageType)
	}
}

func (c *Cursor) seekInLeafTable(pg *pager.Page, page *storage.BTreePage, rowID int64) (bool, error) {
	// Binary search on row IDs
	// Leaf table cells store rowID after payload length
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid, int(c.tx.pageSize)))
		// Skip payload length varint
		_, n := util.GetVarint(pg.Data[cellOff:])
		cellOff += n
		// Read rowID
		midRowID, _ := util.GetVarint(pg.Data[cellOff:])
		switch {
		case int64(midRowID) < rowID:
			lo = mid + 1
		case int64(midRowID) > rowID:
			hi = mid - 1
		default:
			c.pageNum = pg.PageNum
			c.cellIdx = mid
			c.endOfBTree = false
			return true, nil
		}
	}

	// Not found, position at insertion point
	c.pageNum = pg.PageNum
	c.cellIdx = lo
	c.endOfBTree = lo > int(page.CellCount)-1
	return false, nil
}

func (c *Cursor) seekInInteriorTable(pg *pager.Page, page *storage.BTreePage, rowID int64) (bool, error) {
	// Binary search on row IDs in interior page. Each interior table cell is
	// (left child, key); the engine's split (addInteriorCell) routes keys < key
	// to that cell's left child and keys >= key to the NEXT cell's left child
	// (or the rightmost pointer for the last cell). So the child holding rowID
	// is the first cell whose key is STRICTLY greater than rowID — when
	// rowID == an interior key, the row lives in the following cell's range
	// (the previous code descended into the equal-key cell's left child, which
	// only holds keys strictly less than it, missing the boundary row;
	// fts4merge/seek: rowid 113 — an interior split key — was unfindable).
	lo, hi := 0, int(page.CellCount)-1
	childPage := page.RightmostPtr // default: rowID >= all keys -> rightmost
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum)+cellPtrOffset(page.PageType)-8, mid, int(c.tx.pageSize)))
		// Interior table cells: 4-byte left child + rowID varint
		midRowID, _ := util.GetVarint(pg.Data[cellOff+4:])
		if int64(midRowID) <= rowID {
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
	return c.seekInPage(childPage, rowID)
}

// SeekToKey positions the cursor at the entry with the given key (index
// b-trees only). Returns true if found.
func (c *Cursor) SeekToKey(key []byte) (bool, error) {
	return c.seekKeyInPage(c.tx.rootPage, key)
}

func (c *Cursor) seekKeyInPage(pageNum uint32, key []byte) (bool, error) {
	pg, err := c.tx.pager.ReadPage(pageNum)
	if err != nil {
		return false, err
	}
	page, err := storage.ParsePage(pg.Data, int(c.tx.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return false, err
	}

	switch page.PageType {
	case storage.PageTypeLeafIndex:
		return c.seekInLeafIndex(pg, page, key)
	case storage.PageTypeInteriorIndex:
		return c.seekInInteriorIndex(pg, page, key)
	default:
		return false, fmt.Errorf("btree: unexpected page type 0x%02x for index seek", page.PageType)
	}
}

func (c *Cursor) seekInLeafIndex(pg *pager.Page, page *storage.BTreePage, key []byte) (bool, error) {
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid, int(c.tx.pageSize))), storage.CellIndexLeaf, int(c.tx.usableSize))
		if err != nil {
			return false, err
		}
		cmp := util.CompareValues(cell.Payload, key)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp > 0:
			hi = mid - 1
		default:
			c.pageNum = pg.PageNum
			c.cellIdx = mid
			c.endOfBTree = false
			return true, nil
		}
	}
	c.pageNum = pg.PageNum
	c.cellIdx = lo
	c.endOfBTree = lo > int(page.CellCount)-1
	return false, nil
}

func (c *Cursor) seekInInteriorIndex(pg *pager.Page, page *storage.BTreePage, key []byte) (bool, error) {
	lo, hi := 0, int(page.CellCount)-1
	childPage := page.RightmostPtr

	for lo <= hi {
		mid := (lo + hi) / 2
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid, int(c.tx.pageSize))), storage.CellIndexInterior, int(c.tx.usableSize))
		if err != nil {
			return false, err
		}
		cmp := util.CompareValues(cell.Payload, key)
		if cmp < 0 {
			lo = mid + 1
		} else {
			childPage = cell.LeftPtr
			hi = mid - 1
		}
	}
	if lo < int(page.CellCount) {
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), lo, int(c.tx.pageSize))), storage.CellIndexInterior, int(c.tx.usableSize))
		if err != nil {
			return false, err
		}
		childPage = cell.LeftPtr
	}
	return c.seekKeyInPage(childPage, key)
}

// Next moves the cursor to the next entry. Returns false at end.
func (c *Cursor) Next() (bool, error) {
	if c.endOfBTree {
		return false, nil
	}

	if err := c.cachePage(); err != nil {
		return false, err
	}
	page := c.currentPage

	c.cellIdx++
	if c.cellIdx < int(page.CellCount) {
		return true, nil
	}

	// Reached end of current leaf — move to next child of interior root.
	c.clearPageCache()
	c.navigateToNextChild()
	return !c.endOfBTree, nil
}

// Prev moves the cursor to the previous entry.
func (c *Cursor) Prev() (bool, error) {
	if c.cellIdx > 0 {
		c.cellIdx--
		return true, nil
	}
	return false, nil
}

// ReadCell reads the cell at the current cursor position.
func (c *Cursor) ReadCell() (*storage.Cell, error) {
	if c.endOfBTree {
		return nil, fmt.Errorf("btree: cursor at end")
	}

	if err := c.cachePage(); err != nil {
		return nil, err
	}

	// A leaf page may be empty (all its cells deleted). Skip forward past
	// empty leaves so a scan does not stop early.
	if err := c.skipEmptyLeaves(); err != nil {
		return nil, err
	}
	pg := c.currentPg
	page := c.currentPage

	if c.cellIdx < 0 || c.cellIdx >= int(page.CellCount) {
		return nil, fmt.Errorf("btree: cell index %d out of range (count %d)", c.cellIdx, page.CellCount)
	}

	var cellType storage.CellType
	switch page.PageType {
	case storage.PageTypeLeafTable:
		cellType = storage.CellTableLeaf
	case storage.PageTypeLeafIndex:
		cellType = storage.CellIndexLeaf
	case storage.PageTypeInteriorTable:
		cellType = storage.CellTableInterior
	case storage.PageTypeInteriorIndex:
		cellType = storage.CellIndexInterior
	default:
		return nil, fmt.Errorf("btree: unknown page type 0x%02x", page.PageType)
	}

	cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), c.cellIdx, int(c.tx.pageSize)))

	cell, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(c.tx.usableSize))
	if err != nil {
		return nil, err
	}
	return c.tx.readOverflow(cell)
}

// skipEmptyLeaves advances the cursor past any empty leaf pages at the
// current position. The engine keeps empty leaves in the tree after deletes
// (it does not rebalance), so scans must skip them rather than stop early.
func (c *Cursor) skipEmptyLeaves() error {
	for {
		if err := c.cachePage(); err != nil {
			return err
		}
		page := c.currentPage
		if page.CellCount != 0 ||
			(page.PageType != storage.PageTypeLeafTable && page.PageType != storage.PageTypeLeafIndex) {
			return nil
		}
		// Empty leaf: move to the next child in the tree.
		c.cellIdx = 0
		c.clearPageCache()
		c.navigateToNextChild()
		if c.endOfBTree {
			return fmt.Errorf("btree: cursor at end")
		}
	}
}

// ReadCellData reads the current cell's payload data and rowID for table leaf
// cells without allocating a Cell struct. This is the fast path for table scans.
// For non-table-leaf pages, it falls back to ReadCell.
func (c *Cursor) ReadCellData() (payload []byte, rowID int64, err error) {
	if c.endOfBTree {
		return nil, 0, fmt.Errorf("btree: cursor at end")
	}

	if err := c.cachePage(); err != nil {
		return nil, 0, err
	}

	// Skip forward past empty leaf pages.
	if err := c.skipEmptyLeaves(); err != nil {
		return nil, 0, err
	}
	pg := c.currentPg
	page := c.currentPage

	if c.cellIdx < 0 || c.cellIdx >= int(page.CellCount) {
		return nil, 0, fmt.Errorf("btree: cell index %d out of range (count %d)", c.cellIdx, page.CellCount)
	}

	if page.PageType != storage.PageTypeLeafTable {
		// Fall back to full cell decode for other page types
		cell, err := c.ReadCell()
		if err != nil {
			return nil, 0, err
		}
		return cell.Payload, cell.RowID, nil
	}

	cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), c.cellIdx, int(c.tx.pageSize)))

	// A corrupt cell pointer (outside the page buffer) must error, not panic
	// (SQLite reports "database disk image is malformed").
	if cellOff < 0 || cellOff >= len(pg.Data) {
		return nil, 0, fmt.Errorf("database disk image is malformed")
	}

	data := pg.Data[cellOff:]

	// Skip payload length varint
	plen, n := util.GetVarint(data)
	pos := cellOff + n

	// Read rowID varint
	if pos >= len(pg.Data) {
		return nil, 0, fmt.Errorf("database disk image is malformed")
	}
	rowid, n := util.GetVarint(pg.Data[pos:])
	pos += n
	rowID = int64(rowid)

	// Slice the local payload from the page data (no copy)
	payloadLen := storage.LocalPayloadSize(int(plen), int(c.tx.usableSize), storage.CellTableLeaf)
	if payloadLen > len(pg.Data)-pos {
		payloadLen = len(pg.Data) - pos
	}
	payload = pg.Data[pos : pos+payloadLen]
	pos += payloadLen

	// If the payload spills to overflow pages, follow the chain.
	if payloadLen < int(plen) {
		if pos+4 > len(pg.Data) {
			return nil, 0, fmt.Errorf("database disk image is malformed")
		}
		cell := &storage.Cell{
			Type:       storage.CellTableLeaf,
			RowID:      rowID,
			Payload:    payload,
			PayloadLen: int(plen),
			Overflow:   binary.BigEndian.Uint32(pg.Data[pos : pos+4]),
		}
		full, err := c.tx.readOverflow(cell)
		if err != nil {
			return nil, 0, err
		}
		return full.Payload, rowID, nil
	}

	return payload, rowID, nil
}

// leafHasRoom checks if a leaf page has enough room for the given cell data.
func leafHasRoom(pg *pager.Page, page *storage.BTreePage, cellData []byte, coff int, pageSize uint32) bool {
	cellPtrEnd := coff + storage.CellPointerOffset + int(page.CellCount)*2 + 2
	cellContentEnd := int(page.CellContent)
	var cellStart int
	if cellContentEnd == 0 {
		// Reserve 4 bytes at page end for the chain pointer (matches
		// writeLeafCell).
		cellStart = int(pageSize) - 4 - len(cellData) - int(page.FragFree)
	} else {
		cellStart = cellContentEnd - len(cellData)
	}
	return cellStart >= cellPtrEnd
}

// DeleteCell removes a cell from the b-tree by its index position.
// This is a simple implementation that removes the cell from a leaf page
// by shifting remaining cells and updating the page header.
func (t *BTree) DeleteCell(cellIdx int) error {
	pg, err := t.pager.ReadPage(t.rootPage)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}

	if page.PageType != storage.PageTypeLeafTable && page.PageType != storage.PageTypeLeafIndex {
		return fmt.Errorf("btree: delete only supported on leaf pages")
	}

	if cellIdx < 0 || cellIdx >= int(page.CellCount) {
		return fmt.Errorf("btree: cell index %d out of range (count %d)", cellIdx, page.CellCount)
	}

	// Get the cell offset for the cell being deleted
	ptrBase := coff + storage.CellPointerOffset
	_ = int(binary.BigEndian.Uint16(pg.Data[ptrBase+cellIdx*2 : ptrBase+cellIdx*2+2]))

	// Shift remaining cell pointers down
	for i := cellIdx; i < int(page.CellCount)-1; i++ {
		src := ptrBase + (i+1)*2
		dst := ptrBase + i*2
		pg.Data[dst] = pg.Data[src]
		pg.Data[dst+1] = pg.Data[src+1]
	}

	// Clear the last (now unused) cell pointer
	lastPtr := ptrBase + (int(page.CellCount)-1)*2
	pg.Data[lastPtr] = 0
	pg.Data[lastPtr+1] = 0

	// Decrease cell count
	page.CellCount--
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)

	// If the page became empty, reset the content pointer so the next
	// insert treats it as a fresh page (otherwise the stale content end
	// makes leafHasRoom think the empty page is full). SQLite sets the
	// content pointer to the page's usable end for empty leaves.
	if page.CellCount == 0 {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(t.pageSize)) // cell content
		pg.Data[coff+7] = 0                                                    // frag free
	}

	// For simplicity, we don't reclaim the cell data space immediately.
	// The cell data becomes part of the free space and will be overwritten
	// by subsequent inserts. This is a valid approach for a simple implementation.

	return t.pager.WritePage(pg)
}

// DeleteCellsWhere deletes all cells matching a predicate.
// fn returns true for cells that should be deleted.
