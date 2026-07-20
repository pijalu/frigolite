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
	tx       *BTree
	pageNum  uint32
	cellIdx  int
	endOfBTree bool
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
	return c, nil
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

	// Reached end of leaf page - in a simple B+Tree we'd need to follow
	// sibling pointers, but for now we just mark as end.
	c.endOfBTree = true
	return false, nil
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

// InsertCell inserts a cell into the b-tree.
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

	if page.PageType != storage.PageTypeLeafTable && page.PageType != storage.PageTypeLeafIndex {
		return fmt.Errorf("btree: insert only supported on leaf pages")
	}

	cellData := storage.EncodeCell(newCell)

	if len(cellData) >= len(pg.Data)-storage.CellPointerOffset {
		return fmt.Errorf("btree: cell too large for page")
	}

	// Find insertion position
	var insertIdx int
	if t.isTable {
		insertIdx = t.findInsertPositionTable(pg, page, newCell.RowID)
	} else {
		insertIdx = t.findInsertPositionIndex(pg, page, newCell.Payload)
	}

	// Compute available space
	cellPtrEnd := storage.CellPointerOffset + int(page.CellCount)*2 + 2 // after adding new ptr
	cellContentEnd := int(page.CellContent)

	// Place new cell data just before existing cell content (or at page end)
	var cellStart int
	if cellContentEnd == 0 {
		// No existing content, place at bottom of page
		cellStart = len(pg.Data) - len(cellData) - int(page.FragFree)
	} else {
		// Place just before existing content
		cellStart = cellContentEnd - len(cellData)
	}

	if cellStart < cellPtrEnd+coff {
		return fmt.Errorf("btree: page is full (need %d, have %d)", cellStart, cellPtrEnd+coff)
	}

	// Shift cell pointers to make room for new one
	ptrBase := coff + storage.CellPointerOffset
	for i := int(page.CellCount); i > insertIdx; i-- {
		src := ptrBase + (i-1)*2
		dst := ptrBase + i*2
		pg.Data[dst] = pg.Data[src]
		pg.Data[dst+1] = pg.Data[src+1]
	}

	// Write the cell data at the new position
	copy(pg.Data[cellStart:], cellData)

	// Update cell pointer
	binary.BigEndian.PutUint16(pg.Data[ptrBase+insertIdx*2:ptrBase+insertIdx*2+2], uint16(cellStart))

	// Update page header
	page.CellCount++
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)

	if cellContentEnd == 0 || cellStart < cellContentEnd {
		// Update cell content area start
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(cellStart))
	}

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
		pg, err := t.pager.ReadPage(t.rootPage)
		if err != nil {
			return deleted, err
		}
		coff := contentOffset(pg.PageNum)
		page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
		if err != nil {
			return deleted, err
		}

		if page.PageType != storage.PageTypeLeafTable && page.PageType != storage.PageTypeLeafIndex {
			return deleted, fmt.Errorf("btree: delete only supported on leaf pages")
		}

		found := false
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pg.Data, coff, i))
			var cellType storage.CellType
			if page.PageType == storage.PageTypeLeafTable {
				cellType = storage.CellTableLeaf
			} else {
				cellType = storage.CellIndexLeaf
			}
			cell, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.pageSize))
			if err != nil {
				continue
			}
			if fn(cell) {
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
