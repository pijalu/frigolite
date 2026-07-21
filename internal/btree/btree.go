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
	pageNum     uint32
	cellIdx     int
	endOfBTree  bool
	interiorRoot uint32 // root page number if interior, 0 if leaf root
	childIdx     int    // current child index in the interior root
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
	// If root is interior, navigate to the first leaf
	pg, err := t.pager.ReadPage(t.rootPage)
	if err != nil {
		return nil, err
	}
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return nil, err
	}
	if page.PageType == storage.PageTypeInteriorTable || page.PageType == storage.PageTypeInteriorIndex {
		c.interiorRoot = t.rootPage
		c.childIdx = -1 // will be incremented to 0 by navigateToNextChild
		c.navigateToNextChild()
	}
	return c, nil
}

// navigateToNextChild advances the cursor to the next child of the interior root.
// Used when the current leaf is exhausted (leaf tree) or for initial positioning.
func (c *Cursor) navigateToNextChild() {
	if c.interiorRoot == 0 {
		c.endOfBTree = true
		return
	}
	c.childIdx++

	pg, err := c.tx.pager.ReadPage(c.interiorRoot)
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

	if c.childIdx < int(page.CellCount) {
		// Navigate to cell[c.childIdx].leftChild
		cellOff := int(storage.CellPointer(pg.Data, coff+4, c.childIdx))
		c.pageNum = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		c.cellIdx = 0
		c.endOfBTree = false
	} else if c.childIdx == int(page.CellCount) {
		// Navigate to the rightmost pointer (last child)
		c.pageNum = page.RightmostPtr
		c.cellIdx = 0
		c.endOfBTree = false
	} else {
		c.endOfBTree = true
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

	pg, err := c.tx.pager.ReadPage(c.pageNum)
	if err != nil {
		return false, err
	}
	page, err := storage.ParsePage(pg.Data, int(c.tx.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return false, err
	}

	c.cellIdx++
	if c.cellIdx < int(page.CellCount) {
		return true, nil
	}

	// Reached end of current leaf — move to next child of interior root.
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

	pg, err := c.tx.pager.ReadPage(c.pageNum)
	if err != nil {
		return nil, err
	}
	page, err := storage.ParsePage(pg.Data, int(c.tx.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return nil, err
	}

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
// If the root is a leaf and is full, it splits into an interior root with two leaves.
// If the root is an interior page, it descends recursively, splitting children as needed.
func (t *BTree) InsertCell(newCell *storage.Cell) error {
	pg, err := t.pager.ReadPage(t.rootPage)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}

	switch page.PageType {
	case storage.PageTypeLeafTable, storage.PageTypeLeafIndex:
		return t.insertIntoLeafRoot(pg, page, newCell)
	case storage.PageTypeInteriorTable, storage.PageTypeInteriorIndex:
		return t.insertIntoInterior(pg, page, newCell)
	default:
		return fmt.Errorf("btree: unknown page type 0x%02x", page.PageType)
	}
}

// insertIntoLeafRoot handles insert when the root is a leaf.
func (t *BTree) insertIntoLeafRoot(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell) error {
	coff := contentOffset(pg.PageNum)
	cellData := storage.EncodeCell(newCell)

	if leafHasRoom(pg, page, cellData, coff, t.pageSize) {
		// There is room — insert directly.
		return t.writeLeafCell(pg, page, newCell, cellData, coff)
	}

	// Root leaf is full. Split it into an interior root with two leaves.
	// First, create a new leaf with half the cells (original keeps the other half).
	newLeafNum, medianKey, err := t.splitLeaf(pg, page)
	if err != nil {
		return err
	}

	// Write both leaves
	if err := t.pager.WritePage(pg); err != nil {
		return err
	}
	newPg, err := t.pager.ReadPage(newLeafNum)
	if err != nil {
		return err
	}
	if err := t.pager.WritePage(newPg); err != nil {
		return err
	}

	// Create interior root pointing to both leaves
	rootPg, err := t.createInteriorRoot(pg.PageNum, medianKey, newLeafNum)
	if err != nil {
		return err
	}
	t.rootPage = rootPg.PageNum

	// Insert the new cell into the correct child
	if t.isTable && newCell.RowID >= int64(medianKey) {
		return t.writeLeafCell(newPg, nil, newCell, cellData, contentOffset(newPg.PageNum))
	} else if !t.isTable && util.CompareValues(newCell.Payload, cellData) >= 0 {
		return t.writeLeafCell(newPg, nil, newCell, cellData, contentOffset(newPg.PageNum))
	}
	return t.writeLeafCell(pg, nil, newCell, cellData, coff)
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

// insertIntoInterior navigates an interior page to find the correct leaf,
// splitting children as needed.
func (t *BTree) insertIntoInterior(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell) error {
	cellData := storage.EncodeCell(newCell)

	// Find the child page
	childPage := t.findChildPageForInsert(pg, page, newCell)

	// Read the child
	childPg, err := t.pager.ReadPage(childPage)
	if err != nil {
		return err
	}
	childCoff := contentOffset(childPg.PageNum)
	childPageType, err := storage.ParsePage(childPg.Data, int(t.pageSize), childCoff)
	if err != nil {
		return err
	}

	switch childPageType.PageType {
	case storage.PageTypeLeafTable, storage.PageTypeLeafIndex:
		// Check if child has room
		if leafHasRoom(childPg, childPageType, cellData, childCoff, t.pageSize) {
			// Just insert into the child
			return t.writeLeafCell(childPg, childPageType, newCell, cellData, childCoff)
		}
		// Child is full — split it (without the new cell)
		newLeafNum, medianKey, err := t.splitLeaf(childPg, childPageType)
		if err != nil {
			return err
		}
		// Write the original leaf
		if err := t.pager.WritePage(childPg); err != nil {
			return err
		}
		// Write the new leaf
		newLeafPg, err := t.pager.ReadPage(newLeafNum)
		if err != nil {
			return err
		}
		if err := t.pager.WritePage(newLeafPg); err != nil {
			return err
		}
		// Add new child pointer to this interior page
		if err := t.addInteriorCell(pg, page, childPage, medianKey, newLeafNum); err != nil {
			return err
		}
		// Insert the new cell into the correct half
		if t.isTable && int64(medianKey) <= newCell.RowID {
			return t.writeLeafCell(newLeafPg, nil, newCell, cellData, contentOffset(newLeafPg.PageNum))
		}
		return t.writeLeafCell(childPg, nil, newCell, cellData, childCoff)
	default:
		return fmt.Errorf("btree: unexpected child page type 0x%02x", childPageType.PageType)
	}
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
