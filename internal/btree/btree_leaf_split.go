// Leaf-page cell writing and the multi-page leaf split (splitLeafMulti):
// cells are re-sorted, partitioned by size across the original page plus as
// many newly allocated pages as needed, and the divider keys between the
// resulting siblings are computed.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

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

	// SQLite's table b-tree REPLACES a cell with the same rowid rather than
	// inserting a duplicate (sqlite3BtreeInsert with the same key overwrites).
	// The engine's position search returns the first cell with rowid >= the
	// target; if that cell has the SAME rowid, remove it first so the normal
	// insert below writes a single cell (a second cell with the same rowid
	// makes DELETE/UPDATE/seek hit the wrong row and duplicates appear in
	// scans — fts4merge4 2.2.x: the L0 flush and L2 output re-used rowids
	// 33/34, creating duplicate %_segdir rows).
	if t.isTable {
		if err := t.dropTableLeafDuplicateRowid(pg, page, newCell.RowID); err != nil {
			return err
		}
		// Recompute the insertion position after a possible deletion.
		insertIdx = t.findInsertPositionTable(pg, page, newCell.RowID)
	}

	// Compute cell placement
	cellPtrEnd := coff + storage.CellPointerOffset + int(page.CellCount)*2 + 2
	cellContentEnd := int(page.CellContent)
	cellStart := cellContentEnd - len(cellData)
	if cellContentEnd == 0 {
		// Fresh (zero-initialized) page: the first cell ends at the usable
		// end (zeroPage sets the content pointer to usableSize; btree.c
		// packs cells from cbrk=usableSize with no page-end reservation).
		cellStart = int(t.usableSize) - len(cellData) - int(page.FragFree)
	}

	if cellStart < cellPtrEnd {
		return fmt.Errorf("btree: page is full")
	}

	// Shift cell pointers
	shiftCellPtrsRight(pg.Data, coff+storage.CellPointerOffset, insertIdx, int(page.CellCount))

	// Write cell data and pointer
	copy(pg.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(pg.Data[coff+storage.CellPointerOffset+insertIdx*2:coff+storage.CellPointerOffset+insertIdx*2+2], uint16(cellStart))

	// Update header
	page.CellCount++
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if cellContentEnd == 0 || cellStart < cellContentEnd {
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(cellStart))
	}

	return t.pager.WritePage(pg)
}

// tableLeafRowidAt returns the rowid stored in the table-leaf cell at cell
// pointer index idx.
func (t *BTree) tableLeafRowidAt(pg *pager.Page, coff, idx int) int64 {
	cellOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	_, n := util.GetVarint(pg.Data[cellOff:])
	rowID, _ := util.GetVarint(pg.Data[cellOff+n:])
	return int64(rowID)
}

// shiftCellPtrsRight shifts the cell pointer slots [to..from) (index order)
// one slot to the right within the pointer array at ptrBase. Callers insert
// at `to`; every pointer from `to` up to (exclusive) `from` moves one slot up.
func shiftCellPtrsRight(data []byte, ptrBase, to, from int) {
	for i := from; i > to; i-- {
		src := ptrBase + (i-1)*2
		dst := ptrBase + i*2
		data[dst] = data[src]
		data[dst+1] = data[src+1]
	}
}

// splitEntry is a cell plus its encoded byte form and (for index b-trees)
// its full sort key, used during leaf splitting.
type splitEntry struct {
	cell     *storage.Cell
	cellData []byte
	key      []byte // sort key for index b-trees (full payload)
}

// splitLeafMulti splits a full leaf page's cells — plus the incoming new cell
// — across the original page and as many newly allocated pages as needed,
// distributing by size so every page fits (SQLite's balance_nonroot
// rebalances across multiple pages when no two-way split can hold the cells).
// Returns the new pages in order, each with the median key separating it from
// the previous page (the first cell's key of that page).
func (t *BTree) splitLeafMulti(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell, newCellData []byte) ([]leafSplitResult, error) {
	coff := contentOffset(pg.PageNum)
	// Split children hang off the splitting page's parent (btree.c
	// balance_nonroot ptrmapPut PTRMAP_BTREE pParent->pgno, src/btree.c:8023);
	// when the splitting page IS the root, they hang off the root itself
	// (balance_deeper, src/btree.c:9028) — parentPgno==0 marks that case.
	ptrParent := parentPgno
	if ptrParent == 0 {
		ptrParent = pg.PageNum
	}

	cellType := storage.CellTableLeaf
	if !t.isTable {
		cellType = storage.CellIndexLeaf
	}

	cells, err := t.readCellsForSplit(pg, page, coff, cellType, newCell, newCellData)
	if err != nil {
		return nil, err
	}
	sortSplitCells(cells, t.isTable, t.compareKey)

	partitions, err := partitionSplitCells(cells, coff, int(t.usableSize))
	if err != nil {
		return nil, err
	}

	// Clear original leaf content (except page type)
	for i := coff + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}

	// Write the first partition to the original leaf.
	if err := writeLeafHalf(pg, coff, partitions[0], int(t.usableSize)); err != nil {
		return nil, err
	}
	// Pre-allocate every new page, then write each partition. There is no
	// right-sibling chain pointer: btree pages carry no page-end trailer
	// (cells pack from usableSize) and traversal follows the parent's
	// child pointers.
	return t.writeSplitPartitions(pg, coff, partitions, ptrParent)
}

// partitionSplitCells partitions the sorted cells into pages. The greedy
// pass ("longest prefix that fits", btree.c balance_nonroot's page-fill
// loop) determines the MINIMUM page count; the boundaries are then
// re-balanced to even cell counts — SQLite distributes cells across the new
// pages rather than packing the left page maximally, and a maximally-packed
// left page leaves 1-2 cell right siblings that mass deletes then empty one
// by one (each emptied leaf triggers an O(pages) parent walk).
func partitionSplitCells(cells []splitEntry, coff, usableSize int) ([][]splitEntry, error) {
	var partitions [][]splitEntry
	cur := []splitEntry{}
	flush := func() {
		if len(cur) > 0 {
			partitions = append(partitions, cur)
			cur = nil
		}
	}
	for _, c := range cells {
		if len(cur) > 0 {
			probe := append(append([]splitEntry{}, cur...), c)
			if !leafCellsFit(cellDatas(probe), coff, usableSize) {
				flush()
			}
		}
		cur = append(cur, c)
	}
	flush()
	if len(partitions) == 0 {
		return nil, fmt.Errorf("btree: split failed: cannot balance leaf pages")
	}
	if len(partitions) < 2 || true {
		return partitions, nil
	}
	// Even redistribution over the greedy page count.
	total := len(cells)
	even := make([][]splitEntry, 0, len(partitions))
	idx := 0
	fits := true
	for p := 0; p < len(partitions); p++ {
		remainingParts := len(partitions) - p
		remainingCells := total - idx
		take := (remainingCells + remainingParts - 1) / remainingParts
		part := cells[idx : idx+take]
		if !leafCellsFit(cellDatas(part), coff, usableSize) {
			fits = false
			break
		}
		even = append(even, part)
		idx += take
	}
	if fits {
		return even, nil
	}
	return partitions, nil
}

// writeSplitPartitions pre-allocates len(partitions)-1 new leaf pages of the
// same type as the original, persists the rewritten original page (pg), then
// writes each remaining partition to its new page. Returns the new pages in
// order with the median key separating each from its left neighbor.
func (t *BTree) writeSplitPartitions(pg *pager.Page, coff int, partitions [][]splitEntry, ptrParent uint32) ([]leafSplitResult, error) {
	nNew := len(partitions) - 1
	newPages := make([]*pager.Page, 0, nNew)
	for i := 0; i < nNew; i++ {
		np, aerr := t.allocBtreeNode(ptrParent)
		if aerr != nil {
			return nil, aerr
		}
		newPages = append(newPages, np)
	}
	if err := t.pager.WritePage(pg); err != nil {
		return nil, err
	}
	results := make([]leafSplitResult, 0, nNew)
	for pi := 1; pi < len(partitions); pi++ {
		newPg := newPages[pi-1]
		newCoff := contentOffset(newPg.PageNum)
		newPg.Data[newCoff] = pg.Data[coff] // same page type
		if err := writeLeafHalf(newPg, newCoff, partitions[pi], int(t.usableSize)); err != nil {
			return nil, err
		}
		if err := t.reparentSplitOverflowChains(partitions[pi], newPg); err != nil {
			return nil, err
		}
		// No page-end chain pointer (btree.c pages carry no trailer): the
		// cell content area runs to usableSize.
		if err := t.pager.WritePage(newPg); err != nil {
			return nil, err
		}
		key, payload := t.splitMedianKey(partitions, pi)
		results = append(results, leafSplitResult{pageNum: newPg.PageNum, medianKey: key, medianPayload: payload})
	}
	return results, nil
}

// reparentSplitOverflowChains re-parents the overflow chains of the cells
// that moved to this new page: each chain's FIRST page follows its cell's new
// owner (btree.c balance_nonroot ptrmapPutOvflPtr, src/btree.c:8025/8783).
// Later chain pages keep their OVFL2 parent (the previous overflow page).
func (t *BTree) reparentSplitOverflowChains(partition []splitEntry, newPg *pager.Page) error {
	if !t.ptrmapEnabled() {
		return nil
	}
	for _, e := range partition {
		if e.cell.Overflow != 0 {
			if werr := t.pager.WritePtrmap(e.cell.Overflow, storage.PtrmapOverflow1, newPg.PageNum); werr != nil {
				return werr
			}
		}
	}
	return nil
}

// splitMedianKey computes the divider key between partition pi-1 and
// partition pi, together with the index divider payload (empty for tables).
func (t *BTree) splitMedianKey(partitions [][]splitEntry, pi int) (uint64, []byte) {
	if t.isTable {
		// SQLite's leafData separator convention (btree.c:8813): the
		// divider cell between two sibling leaves carries the LAST
		// rowid of the LEFT sibling (left subtree holds keys <= key,
		// right subtree holds keys > key — sqlite3BtreeTableMoveto
		// descends into the separator's left child on key==rowid).
		// Using MIN(right) instead (the engine's pre-fix convention)
		// made the right subtree overlap the boundary row, breaking
		// sqlite3 integrity_check on every table btree split
		// (incrvacuum2 4.1: doubling leaves produced "right child
		// Rowid N out of order" starting at iter 1).
		left := partitions[pi-1]
		return uint64(left[len(left)-1].cell.RowID), nil
	}
	// Index btrees (non-leafData): the divider is the FIRST cell of
	// the RIGHT sibling (btree.c:8820, pCell -= 4 branch) — the left
	// subtree holds keys < medianKey and the right subtree holds
	// keys >= medianKey (sqlite3BtreeIndexMoveto: equal keys go
	// right). The divider cell carries that cell's full record payload
	// (balance_nonroot copies the cell into the interior page), so
	// interior descent and sqlite3 integrity_check see value-ordered
	// separators.
	return 0, partitions[pi][0].key
}

// leafSplitResult is one new page produced by a split: the page number and
// the divider separating it from the previous page — medianKey (the last
// rowid of the left sibling) for table b-trees, medianPayload (the right
// sibling's first cell's full record payload, balance_nonroot's copied
// separator cell) for index b-trees.
type leafSplitResult struct {
	pageNum       uint32
	medianKey     uint64
	medianPayload []byte
}

// readCellsForSplit decodes the existing cells on a leaf page plus the new
// cell into a unified split-entry list, ready for redistribution.
func (t *BTree) readCellsForSplit(pg *pager.Page, page *storage.BTreePage, coff int, cellType storage.CellType, newCell *storage.Cell, newCellData []byte) ([]splitEntry, error) {
	var cells []splitEntry
	for i := uint16(0); i < page.CellCount; i++ {
		cellOff := int(storage.CellPointer(pg.Data, coff, int(i), int(t.pageSize)))
		c, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.usableSize))
		if err != nil {
			return nil, err
		}

		e := splitEntry{c, storage.EncodeCell(c), nil}
		if !t.isTable {
			full, err := t.readOverflow(c)
			if err != nil {
				return nil, err
			}
			// Clone the sort key: it aliases pg.Data, and splitLeafMulti
			// zeroes this page's cell area while redistributing — the
			// divider payload handed to the parent must stay valid.
			e.key = append([]byte(nil), full.Payload...)
		}
		cells = append(cells, e)
	}
	// Include the new cell in the redistribution — REPLACING any existing
	// cell with the same key (SQLite's sqlite3BtreeInsert overwrites in
	// place; the non-split path dedupes in writeLeafCell, and the split
	// path must too, otherwise overwriting a full page's boundary rowid
	// writes the rowid twice — duplicate rowids across/within partitions,
	// fts4opt churn: rowid 229 duplicated on leaf 171).
	// (The new cell's payload is engine-owned, not a page view.)
	for i := 0; i < len(cells); i++ {
		if t.isTable && cells[i].cell.RowID == newCell.RowID {
			cells = append(cells[:i], cells[i+1:]...)
			i--
		}
	}
	cells = append(cells, splitEntry{newCell, newCellData, newCell.Payload})
	return cells, nil
}

// sortSplitCells orders cells by key (rowid for tables, full payload for
// indexes).
func sortSplitCells(cells []splitEntry, isTable bool, cmp func(a, b []byte) int) {
	if isTable {
		bubbleSortSplitCells(cells, func(a, b splitEntry) bool {
			return a.cell.RowID > b.cell.RowID
		})
		return
	}
	bubbleSortSplitCells(cells, func(a, b splitEntry) bool {
		return cmp(a.key, b.key) > 0
	})
}

// bubbleSortSplitCells sorts cells in place using a "greater than"
// comparison, keeping entries with equal keys in their original order.
func bubbleSortSplitCells(cells []splitEntry, greater func(a, b splitEntry) bool) {
	for i := 0; i < len(cells); i++ {
		for j := i + 1; j < len(cells); j++ {
			if greater(cells[i], cells[j]) {
				cells[i], cells[j] = cells[j], cells[i]
			}
		}
	}
}

// writeLeafHalf writes a slice of split entries to a leaf page starting at
// the given content offset, appending cells from the end of the usable area
// downward, and updates the page header's cell count and content end.
func writeLeafHalf(pg *pager.Page, coff int, half []splitEntry, usableSize int) error {
	var count uint16
	end := usableSize // cells pack from the usable end (zeroPage convention)
	for i := range half {
		d := half[i].cellData
		start := end - len(d)
		ptrOff := coff + storage.CellPointerOffset + int(count)*2
		if start < ptrOff+2 {
			return fmt.Errorf("btree: split failed: leaf half full")
		}
		copy(pg.Data[start:], d)
		binary.BigEndian.PutUint16(pg.Data[ptrOff:], uint16(start))
		count++
		end = start
	}
	// Full header rewrite (zeroPage parity): the page may be a cached
	// buffer from an earlier incarnation (freed leaf/overflow/trunk page
	// handed out again by the freelist pop), so freeblock and fragmentation
	// must be reset explicitly — stale bytes there read as free-space
	// corruption ("Fragmentation of N bytes reported as M").
	binary.BigEndian.PutUint16(pg.Data[coff+1:coff+3], 0) // first freeblock
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], count)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(end))
	pg.Data[coff+7] = 0 // fragmented free bytes
	return nil
}

// cellDatas extracts the encoded cell byte slices from a split entry list.
func cellDatas(cells []splitEntry) [][]byte {
	out := make([][]byte, len(cells))
	for i, c := range cells {
		out[i] = c.cellData
	}
	return out
}

// leafCellsFit reports whether the given cell byte slices fit in a leaf page
// with the given content offset, leaving room for the cell pointer array.
func leafCellsFit(cells [][]byte, coff, usableSize int) bool {
	total := 0
	for _, d := range cells {
		total += len(d)
	}
	ptrEnd := coff + storage.CellPointerOffset + len(cells)*2 + 2
	contentEnd := usableSize // cells pack from the usable end (no page-end trailer)
	return contentEnd-total >= ptrEnd
}

// findInsertPositionTable returns the index of the first table-leaf cell with
// rowid >= rowID (binary search over the cell pointer array).
func (t *BTree) findInsertPositionTable(pg *pager.Page, page *storage.BTreePage, rowID int64) int {
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid, int(t.pageSize)))
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

// findInsertPositionIndex returns the index of the first index-leaf cell whose
// full payload is >= key (binary search over the cell pointer array).
func (t *BTree) findInsertPositionIndex(pg *pager.Page, page *storage.BTreePage, key []byte) int {
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cell, err := storage.DecodeCell(pg.Data, int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), mid, int(t.pageSize))), storage.CellIndexLeaf, int(t.usableSize))
		if err != nil {
			return lo
		}
		full, err := t.readOverflow(cell)
		if err != nil {
			return lo
		}
		if t.compareKey(full.Payload, key) < 0 {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return lo
}
