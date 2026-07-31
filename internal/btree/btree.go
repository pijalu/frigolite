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
	tx          *BTree
	pageNum     uint32 // current leaf page
	cellIdx     int
	endOfBTree  bool

	// Path stack for multi-level tree traversal. Each entry records an
	// interior page and the child index within it that we descended through.
	// The stack grows as we descend and shrinks as we ascend.
	path []cursorPathEntry

	// Cache for current page to avoid repeated ParsePage calls
	currentPg    *pager.Page
	currentPage  *storage.BTreePage
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
	isTable    bool // true for table b-trees, false for index b-trees
}

// NewBTree creates a new BTree instance.
func NewBTree(pg *pager.Pager, rootPage uint32, isTable bool) *BTree {
	return &BTree{
		pager:    pg,
		rootPage: rootPage,
		pageSize: pg.PageSize(),
		isTable:  isTable,
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
			cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, 0))
			c.pageNum = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		} else {
			c.pageNum = page.RightmostPtr
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
			cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, top.childIdx))
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
			cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, 0))
			c.pageNum = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		} else {
			c.pageNum = page.RightmostPtr
		}
	}
}

// RootPage returns the current root page number (may change after splits).
func (t *BTree) RootPage() uint32 {
	return t.rootPage
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
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid))
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
	// Binary search on row IDs in interior page
	lo, hi := 0, int(page.CellCount)-1
	childPage := page.RightmostPtr // default to rightmost child

	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid))
		// Interior table cells: 4-byte left child + rowID varint
		midRowID, _ := util.GetVarint(pg.Data[cellOff+4:])
		if int64(midRowID) < rowID {
			lo = mid + 1
		} else {
			childPage = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			hi = mid - 1
		}
	}
	if lo < int(page.CellCount) {
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), lo))
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
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid)), storage.CellIndexLeaf, int(c.tx.pageSize))
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
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid)), storage.CellIndexInterior, int(c.tx.pageSize))
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
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), lo)), storage.CellIndexInterior, int(c.tx.pageSize))
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

	cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), c.cellIdx))
	return storage.DecodeCell(pg.Data, cellOff, cellType, int(c.tx.pageSize))
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

	cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), c.cellIdx))

	data := pg.Data[cellOff:]

	// Skip payload length varint
	plen, n := util.GetVarint(data)
	pos := cellOff + n

	// Read rowID varint
	rowid, n := util.GetVarint(pg.Data[pos:])
	pos += n
	rowID = int64(rowid)

	// Slice the payload from the page data (no copy)
	payloadLen := int(plen)
	if pos+payloadLen > len(pg.Data) {
		payloadLen = len(pg.Data) - pos
	}
	payload = pg.Data[pos : pos+payloadLen]

	return payload, rowID, nil
}

// leafHasRoom checks if a leaf page has enough room for the given cell data.
func leafHasRoom(pg *pager.Page, page *storage.BTreePage, cellData []byte, coff int, pageSize uint32) bool {
	cellPtrEnd := coff + storage.CellPointerOffset + int(page.CellCount)*2 + 2
	cellContentEnd := int(page.CellContent)
	var cellStart int
	if cellContentEnd == 0 {
		cellStart = int(pageSize) - len(cellData) - int(page.FragFree)
	} else {
		cellStart = cellContentEnd - len(cellData)
	}
	return cellStart >= cellPtrEnd
}

// InsertCell inserts a cell into the b-tree.
// Uses a recursive insert with proper split propagation for multi-level trees.
// When a page splits, the split key and new sibling propagate up to the parent.
func (t *BTree) InsertCell(newCell *storage.Cell) error {
	splitKey, newSibling, err := t.insertPage(t.rootPage, newCell)
	if err != nil {
		return err
	}
	if splitKey > 0 {
		// Root page split — create a new root one level up
		rootPg, err := t.createInteriorRoot(t.rootPage, splitKey, newSibling)
		if err != nil {
			return err
		}
		t.rootPage = rootPg.PageNum
	}
	return nil
}

// insertPage inserts a cell into the page at pageNum.
// Returns (splitKey, newSiblingPageNum, error). If splitKey > 0, the page
// was split: the original page contains keys < splitKey, the new sibling
// contains keys >= splitKey. The caller must add (pageNum, splitKey, newSibling)
// to the parent interior page.
func (t *BTree) insertPage(pageNum uint32, newCell *storage.Cell) (uint64, uint32, error) {
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return 0, 0, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return 0, 0, err
	}

	switch page.PageType {
	case storage.PageTypeLeafTable, storage.PageTypeLeafIndex:
		return t.insertLeafPage(pg, page, newCell)
	case storage.PageTypeInteriorTable, storage.PageTypeInteriorIndex:
		return t.insertInteriorPage(pg, page, newCell)
	default:
		return 0, 0, fmt.Errorf("btree: unknown page type 0x%02x", page.PageType)
	}
}

// insertLeafPage inserts a cell into a leaf page. If the leaf is full, it splits.
func (t *BTree) insertLeafPage(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell) (uint64, uint32, error) {
	coff := contentOffset(pg.PageNum)
	cellData := storage.EncodeCell(newCell)

	if leafHasRoom(pg, page, cellData, coff, t.pageSize) {
		// There is room — insert directly.
		if err := t.writeLeafCell(pg, page, newCell, cellData, coff); err != nil {
			return 0, 0, err
		}
		return 0, 0, nil
	}

	// Leaf is full. If empty, the cell is too large.
	if page.CellCount == 0 {
		return 0, 0, fmt.Errorf("btree: cell too large for page (size=%d, pageSize=%d)", len(cellData), t.pageSize)
	}

	// Split the leaf
	newLeafNum, medianKey, err := t.splitLeaf(pg, page)
	if err != nil {
		return 0, 0, err
	}
	if err := t.pager.WritePage(pg); err != nil {
		return 0, 0, err
	}
	newPg, err := t.pager.ReadPage(newLeafNum)
	if err != nil {
		return 0, 0, err
	}
	if err := t.pager.WritePage(newPg); err != nil {
		return 0, 0, err
	}

	// Insert the new cell into the correct half
	if t.isTable && newCell.RowID >= int64(medianKey) {
		if err := t.writeLeafCell(newPg, nil, newCell, cellData, contentOffset(newPg.PageNum)); err != nil {
			return 0, 0, err
		}
	} else if !t.isTable && util.CompareValues(newCell.Payload, cellData) >= 0 {
		if err := t.writeLeafCell(newPg, nil, newCell, cellData, contentOffset(newPg.PageNum)); err != nil {
			return 0, 0, err
		}
	} else {
		if err := t.writeLeafCell(pg, nil, newCell, cellData, coff); err != nil {
			return 0, 0, err
		}
	}

	return medianKey, newLeafNum, nil
}

// insertInteriorPage inserts a cell into an interior page by routing to the
// correct child. If the child splits, adds a new pointer. If this interior
// page is then full, splits it too.
func (t *BTree) insertInteriorPage(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell) (uint64, uint32, error) {
	// Find the child page that should receive the new cell
	childPageNum := t.findChildPageForInsert(pg, page, newCell)

	// Recursively insert into the child
	childSplitKey, childNewSibling, err := t.insertPage(childPageNum, newCell)
	if err != nil {
		return 0, 0, err
	}

	if childSplitKey == 0 {
		// No split occurred — done
		return 0, 0, nil
	}

	// Child split occurred. Add new child pointer to this interior page.
	err = t.addInteriorCell(pg, page, childPageNum, childSplitKey, childNewSibling)
	if err == nil {
		return 0, 0, nil
	}

	// addInteriorCell failed — check if it's because this page is full
	if err.Error() != "btree: interior page full, cannot add child pointer" {
		return 0, 0, err
	}

	// This interior page is full. Split it.
	newInteriorNum, splitKey, err := t.splitInteriorPage(pg, page)
	if err != nil {
		return 0, 0, err
	}

	// Now add the child pointer to the correct half.
	// After the split, the original page has keys < splitKey,
	// the new sibling has keys >= splitKey.
	if t.isTable {
		if int64(childSplitKey) >= int64(splitKey) {
			// Goes to the new sibling
			newPg, err := t.pager.ReadPage(newInteriorNum)
			if err != nil {
				return 0, 0, err
			}
			newPage, err := storage.ParsePage(newPg.Data, int(t.pageSize), contentOffset(newPg.PageNum))
			if err != nil {
				return 0, 0, err
			}
			if err := t.addInteriorCell(newPg, newPage, childPageNum, childSplitKey, childNewSibling); err != nil {
				return 0, 0, err
			}
		} else {
			// Goes to the original page
			origPg, err := t.pager.ReadPage(pg.PageNum)
			if err != nil {
				return 0, 0, err
			}
			origPage, err := storage.ParsePage(origPg.Data, int(t.pageSize), contentOffset(origPg.PageNum))
			if err != nil {
				return 0, 0, err
			}
			if err := t.addInteriorCell(origPg, origPage, childPageNum, childSplitKey, childNewSibling); err != nil {
				return 0, 0, err
			}
		}
	}

	return splitKey, newInteriorNum, nil
}

// writeLeafCell inserts a cell at the correct position in a leaf page.
// Assumes the page has room (call leafHasRoom first).
func (t *BTree) writeLeafCell(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell, cellData []byte, coff int) error {
	if page == nil {
		var err error
		page, err = storage.ParsePage(pg.Data, int(t.pageSize), coff)
		if err != nil {
			return err
		}
	}

	// Find insertion position
	var insertIdx int
	if t.isTable {
		insertIdx = t.findInsertPositionTable(pg, page, newCell.RowID)
	} else {
		insertIdx = t.findInsertPositionIndex(pg, page, newCell.Payload)
	}

	// Compute cell placement
	cellPtrEnd := coff + storage.CellPointerOffset + int(page.CellCount)*2 + 2
	cellContentEnd := int(page.CellContent)
	var cellStart int
	if cellContentEnd == 0 {
		// Reserve 4 bytes at page end for chain pointer
		cellStart = int(t.pageSize) - 4 - len(cellData) - int(page.FragFree)
	} else {
		cellStart = cellContentEnd - len(cellData)
	}


	if cellStart < cellPtrEnd {
		return fmt.Errorf("btree: page is full")
	}

	// Shift cell pointers
	ptrBase := coff + storage.CellPointerOffset
	for i := int(page.CellCount); i > insertIdx; i-- {
		src := ptrBase + (i-1)*2
		dst := ptrBase + i*2
		pg.Data[dst] = pg.Data[src]
		pg.Data[dst+1] = pg.Data[src+1]
	}

	// Write cell data and pointer
	copy(pg.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(pg.Data[ptrBase+insertIdx*2:ptrBase+insertIdx*2+2], uint16(cellStart))

	// Update header
	page.CellCount++
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if cellContentEnd == 0 || cellStart < cellContentEnd {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(cellStart))
	}

	return t.pager.WritePage(pg)
}

// splitLeaf splits a leaf page's existing cells between the original page
// and a new page. Does NOT include the new cell. Returns (newLeafPageNum, medianKey, error).
func (t *BTree) splitLeaf(pg *pager.Page, page *storage.BTreePage) (uint32, uint64, error) {
	coff := contentOffset(pg.PageNum)

	// Read existing cells
	type entry struct {
		cell     *storage.Cell
		cellData []byte
	}
	var cells []entry

	cellType := storage.CellTableLeaf
	if !t.isTable {
		cellType = storage.CellIndexLeaf
	}

	for i := uint16(0); i < page.CellCount; i++ {
		cellOff := int(storage.CellPointer(pg.Data, coff, int(i)))
		c, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.pageSize))
		if err != nil {
			return 0, 0, err
		}
		cells = append(cells, entry{c, storage.EncodeCell(c)})
	}

	// Sort by rowid/key
	if t.isTable {
		for i := 0; i < len(cells); i++ {
			for j := i + 1; j < len(cells); j++ {
				if cells[i].cell.RowID > cells[j].cell.RowID {
					cells[i], cells[j] = cells[j], cells[i]
				}
			}
		}
	}

	splitIdx := len(cells) / 2

	// Allocate new leaf
	newPg := t.pager.AllocatePage()
	newCoff := contentOffset(newPg.PageNum)
	newPg.Data[newCoff] = pg.Data[coff] // same page type

	// Clear original leaf content (except page type)
	for i := coff + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}

	// Write first half to original leaf
	var leftCount uint16
	leftEnd := int(t.pageSize) - 4 // reserve last 4 bytes for chain pointer
	for i := 0; i < splitIdx; i++ {
		d := cells[i].cellData
		start := leftEnd - len(d)
		ptrOff := coff + storage.CellPointerOffset + int(leftCount)*2
		if start < ptrOff+2 {
			return 0, 0, fmt.Errorf("btree: split failed: left leaf full")
		}
		copy(pg.Data[start:], d)
		binary.BigEndian.PutUint16(pg.Data[ptrOff:], uint16(start))
		leftCount++
		leftEnd = start
	}
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], leftCount)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(leftEnd))

	// Write second half to new leaf
	var rightCount uint16
	rightEnd := int(t.pageSize) - 4 // reserve last 4 bytes for chain pointer
	for i := splitIdx; i < len(cells); i++ {
		d := cells[i].cellData
		start := rightEnd - len(d)
		ptrOff := newCoff + storage.CellPointerOffset + int(rightCount)*2
		if start < ptrOff+2 {
			return 0, 0, fmt.Errorf("btree: split failed: right leaf full")
		}
		copy(newPg.Data[start:], d)
		binary.BigEndian.PutUint16(newPg.Data[ptrOff:], uint16(start))
		rightCount++
		rightEnd = start
	}
	binary.BigEndian.PutUint16(newPg.Data[newCoff+3:newCoff+5], rightCount)
	binary.BigEndian.PutUint16(newPg.Data[newCoff+5:newCoff+7], uint16(rightEnd))

	// Median key = first rowid of the right leaf
	var medianKey uint64
	if t.isTable {
		medianKey = uint64(cells[splitIdx].cell.RowID)
	} else if len(cells) > splitIdx {
		medianKey = uint64(len(cells[splitIdx].cellData))
	}

	// Set chain pointer: original leaf → new leaf (last 4 bytes)
	binary.BigEndian.PutUint32(pg.Data[int(t.pageSize)-4:int(t.pageSize)], newPg.PageNum)

	return newPg.PageNum, medianKey, nil
}

// createInteriorRoot creates an interior page pointing to two children.
func (t *BTree) createInteriorRoot(leftChild uint32, medianKey uint64, rightChild uint32) (*pager.Page, error) {
	rootPg := t.pager.AllocatePage()
	rootCoff := contentOffset(rootPg.PageNum)

	if t.isTable {
		rootPg.Data[rootCoff] = storage.PageTypeInteriorTable
	} else {
		rootPg.Data[rootCoff] = storage.PageTypeInteriorIndex
	}

	// One cell: {leftChild, medianKey}
	cellData := t.encodeInteriorCell(leftChild, medianKey)
	cellStart := int(t.pageSize) - len(cellData)
	copy(rootPg.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+cellPtrOffset(rootPg.Data[rootCoff]):], uint16(cellStart))
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+3:rootCoff+5], 1)
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+5:rootCoff+7], uint16(cellStart))
	binary.BigEndian.PutUint32(rootPg.Data[rootCoff+8:rootCoff+12], rightChild) // rightmostPtr

	if err := t.pager.WritePage(rootPg); err != nil {
		return nil, err
	}
	return rootPg, nil
}

// cellPtrOffset returns the cell pointer array offset for a given page type.
// Interior pages have a 12-byte header (rightmost pointer at bytes 8-11),
// so cell pointers start at byte 12. Leaf pages have an 8-byte header.
func cellPtrOffset(pageType byte) int {
	if pageType == storage.PageTypeInteriorTable || pageType == storage.PageTypeInteriorIndex {
		return 12
	}
	return 8
}

// encodeInteriorCell creates an interior cell: 4-byte leftChild + rowID varint.
func (t *BTree) encodeInteriorCell(leftChild uint32, rowID uint64) []byte {
	ridLen := util.VarintLen(rowID)
	buf := make([]byte, 4+ridLen)
	binary.BigEndian.PutUint32(buf[:4], leftChild)
	util.PutVarint(buf[4:], rowID)
	return buf
}

// splitInteriorPage splits a full interior page into two pages.
// Returns (newPageNum, splitKey, error) where splitKey is the median key
// that goes into the parent.
func (t *BTree) splitInteriorPage(pg *pager.Page, page *storage.BTreePage) (uint32, uint64, error) {
	coff := contentOffset(pg.PageNum)
	ptroff := cellPtrOffset(page.PageType)
	// CellPointer adds 8 to the given offset. For interior pages (ptroff=12),
	// we pass coff+4 so it computes coff+4+8+i*2 = coff+12+i*2.
	ptrBase := coff + ptroff - 8

	// Collect all interior cells (leftChild, key pairs) and the rightmost pointer
	type interiorEntry struct {
		leftChild uint32
		key       uint64
	}
	var entries []interiorEntry
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, i))
		leftChild := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		key, _ := util.GetVarint(pg.Data[cellOff+4:])
		entries = append(entries, interiorEntry{leftChild, key})
	}
	rightmostChild := page.RightmostPtr

	// Split at midpoint
	splitIdx := len(entries) / 2

	// The key at splitIdx goes up to the parent (it's the separator between the two halves)
	splitKey := entries[splitIdx].key

	// Left page keeps entries[0..splitIdx) and its rightmost child becomes entries[splitIdx].leftChild
	// Right page keeps entries[splitIdx+1..) and the original rightmostChild

	// Allocate new interior page
	newPg := t.pager.AllocatePage()
	newCoff := contentOffset(newPg.PageNum)
	newPg.Data[newCoff] = page.PageType // same interior type

	// Clear original interior page content (except page type)
	for i := coff + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}

	// Rewrite left page: entries[0..splitIdx), rightmost = entries[splitIdx].leftChild
	leftRightmost := entries[splitIdx].leftChild
	leftCellContentEnd := int(t.pageSize) // track content end in local var
	for i := 0; i < splitIdx; i++ {
		cellData := t.encodeInteriorCell(entries[i].leftChild, entries[i].key)
		cellPtrEnd := coff + ptroff + i*2 + 2
		cellStart := leftCellContentEnd - len(cellData)
		if cellStart < cellPtrEnd {
			return 0, 0, fmt.Errorf("btree: interior split failed: left page overflow")
		}
		copy(pg.Data[cellStart:], cellData)
		binary.BigEndian.PutUint16(pg.Data[coff+ptroff+i*2:], uint16(cellStart))
		leftCellContentEnd = cellStart
	}
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], uint16(splitIdx))
	if splitIdx > 0 {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(leftCellContentEnd))
	} else {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], 0)
	}
	binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], leftRightmost)

	// Write right page: entries[splitIdx+1..), rightmost = original rightmostChild
	rightCount := 0
	rightCellContentEnd := int(t.pageSize)
	for i := splitIdx + 1; i < len(entries); i++ {
		cellData := t.encodeInteriorCell(entries[i].leftChild, entries[i].key)
		cellPtrEnd := newCoff + ptroff + rightCount*2 + 2
		cellStart := rightCellContentEnd - len(cellData)
		if cellStart < cellPtrEnd {
			return 0, 0, fmt.Errorf("btree: interior split failed: right page overflow")
		}
		copy(newPg.Data[cellStart:], cellData)
		binary.BigEndian.PutUint16(newPg.Data[newCoff+ptroff+rightCount*2:], uint16(cellStart))
		rightCellContentEnd = cellStart
		rightCount++
	}
	binary.BigEndian.PutUint16(newPg.Data[newCoff+3:newCoff+5], uint16(rightCount))
	if rightCount > 0 {
		binary.BigEndian.PutUint16(newPg.Data[newCoff+5:newCoff+7], uint16(rightCellContentEnd))
	} else {
		binary.BigEndian.PutUint16(newPg.Data[newCoff+5:newCoff+7], 0)
	}
	binary.BigEndian.PutUint32(newPg.Data[newCoff+8:newCoff+12], rightmostChild)

	if err := t.pager.WritePage(pg); err != nil {
		return 0, 0, err
	}
	if err := t.pager.WritePage(newPg); err != nil {
		return 0, 0, err
	}

	return newPg.PageNum, splitKey, nil
}

// findChildPageForInsert returns the child page that should receive the new cell.
func (t *BTree) findChildPageForInsert(pg *pager.Page, page *storage.BTreePage, cell *storage.Cell) uint32 {
	if !t.isTable {
		return page.RightmostPtr // for index b-trees, always append to rightmost
	}
	coff := contentOffset(pg.PageNum)
	// Binary search on row IDs in interior page
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, coff+4, mid))
		midRowID, _ := util.GetVarint(pg.Data[cellOff+4:])
		if int64(midRowID) < cell.RowID {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if lo < int(page.CellCount) {
		cellOff := int(storage.CellPointer(pg.Data, coff+4, lo))
		return binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
	}
	return page.RightmostPtr
}

// addInteriorCell adds a new cell to an interior page.
func (t *BTree) addInteriorCell(pg *pager.Page, page *storage.BTreePage, leftChild uint32, key uint64, rightChild uint32) error {
	coff := contentOffset(pg.PageNum)
	cellData := t.encodeInteriorCell(leftChild, key)
	ptroff := cellPtrOffset(page.PageType)

	// Compute space
	cellPtrEnd := coff + ptroff + int(page.CellCount)*2 + 2
	cellContentEnd := int(page.CellContent)
	var cellStart int
	if cellContentEnd == 0 {
		cellStart = int(t.pageSize) - len(cellData) - int(page.FragFree)
	} else {
		cellStart = cellContentEnd - len(cellData)
	}
	if cellStart < cellPtrEnd {
		return fmt.Errorf("btree: interior page full, cannot add child pointer")
	}

	// Insert at the end (keys are monotonically increasing)
	insertIdx := int(page.CellCount)
	ptrBase := coff + ptroff
	for i := int(page.CellCount); i > insertIdx; i-- {
		src := ptrBase + (i-1)*2
		dst := ptrBase + i*2
		pg.Data[dst] = pg.Data[src]
		pg.Data[dst+1] = pg.Data[src+1]
	}

	copy(pg.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(pg.Data[ptrBase+insertIdx*2:], uint16(cellStart))

	page.CellCount++
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if cellContentEnd == 0 || cellStart < cellContentEnd {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(cellStart))
	}

	// Update rightmost pointer to point to the new child
	binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], rightChild)

	return t.pager.WritePage(pg)
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

	// For simplicity, we don't reclaim the cell data space immediately.
	// The cell data becomes part of the free space and will be overwritten
	// by subsequent inserts. This is a valid approach for a simple implementation.

	return t.pager.WritePage(pg)
}

// DeleteCellsWhere deletes all cells matching a predicate.
// fn returns true for cells that should be deleted.
func (t *BTree) DeleteCellsWhere(fn func(cell *storage.Cell) bool) (int64, error) {
	var deleted int64
	for {
		pg, page, err := t.readPageForDelete()
		if err != nil {
			return deleted, err
		}
		_ = pg

		found := false
		for i := 0; i < int(page.CellCount); i++ {
			if t.cellMatches(pg, page, i, fn) {
				if err := t.DeleteCell(i); err != nil {
					return deleted, err
				}
				deleted++
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return deleted, nil
}

func (t *BTree) readPageForDelete() (*pager.Page, *storage.BTreePage, error) {
	pg, err := t.pager.ReadPage(t.rootPage)
	if err != nil {
		return nil, nil, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return nil, nil, err
	}
	if page.PageType != storage.PageTypeLeafTable && page.PageType != storage.PageTypeLeafIndex {
		return nil, nil, fmt.Errorf("btree: delete only supported on leaf pages")
	}
	return pg, page, nil
}

func (t *BTree) cellMatches(pg *pager.Page, page *storage.BTreePage, idx int, fn func(cell *storage.Cell) bool) bool {
	coff := contentOffset(pg.PageNum)
	cellOff := int(storage.CellPointer(pg.Data, coff, idx))
	var cellType storage.CellType
	if page.PageType == storage.PageTypeLeafTable {
		cellType = storage.CellTableLeaf
	} else {
		cellType = storage.CellIndexLeaf
	}
	cell, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.pageSize))
	if err != nil {
		return false
	}
	return fn(cell)
}

func (t *BTree) findInsertPositionTable(pg *pager.Page, page *storage.BTreePage, rowID int64) int {
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid))
		_, n := util.GetVarint(pg.Data[cellOff:])
		cellOff += n
		midRowID, _ := util.GetVarint(pg.Data[cellOff:])
		if int64(midRowID) < rowID {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func (t *BTree) findInsertPositionIndex(pg *pager.Page, page *storage.BTreePage, key []byte) int {
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid)), storage.CellIndexLeaf, int(t.pageSize))
		if err != nil {
			return lo
		}
		if util.CompareValues(cell.Payload, key) < 0 {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return lo
}
