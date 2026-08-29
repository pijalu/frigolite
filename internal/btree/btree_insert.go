// Package btree implements a B+Tree on top of the pager.
// This file holds the insert/split machinery for both table and index
// b-trees (cell insertion, page splitting, interior cell management).
package btree

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// InsertCell inserts a cell into the b-tree.
// Uses a recursive insert with proper split propagation for multi-level trees.
// When a page splits, the split key and new sibling propagate up to the parent.
func (t *BTree) InsertCell(newCell *storage.Cell) error {
	splits, err := t.insertPage(t.rootPage, newCell)
	if err != nil {
		return err
	}
	if len(splits) > 0 {
		// Root page split.
		if t.rootPage != 1 {
			// btree.c balance_deeper keeps the root page as the root: its
			// page number never changes (schema entries stay valid) and the
			// two halves move to freshly allocated pages in ascending order.
			return t.relocateRootSplit(splits)
		}
		// The schema b-tree (sqlite_schema) is permanently rooted at page 1:
		// page 1 is the database file header page and cannot be demoted to a
		// child. When its root splits, page 1 becomes an interior page and
		// the split halves are moved to newly allocated pages.
		rootPg, err := t.createInteriorRoot(t.rootPage, splits[0].medianKey, splits[0].pageNum)
		if err != nil {
			return err
		}
		for i := 1; i < len(splits); i++ {
			if err := t.addInteriorCellToPage(rootPg.PageNum, splits[i-1].pageNum, splits[i].medianKey, splits[i].pageNum); err != nil {
				return err
			}
		}
		t.rootPage = rootPg.PageNum
	}

	return nil
}

// relocateRootSplit keeps the splitting root's page number stable. The split
// has already rewritten the root as its left segment and allocated the right
// siblings S1..Sk; one more page is allocated as the final child slot so the
// segment contents rotate down one slot (S1←left, Si←R(i-1), new←Rk), and the
// root page is rewritten as an interior node over the relocated children.
func (t *BTree) relocateRootSplit(splits []leafSplitResult) error {
	rootPg, err := t.pager.ReadPage(t.rootPage)
	if err != nil {
		return err
	}
	left := make([]byte, len(rootPg.Data))
	copy(left, rootPg.Data)

	// Final child slot: allocated last so it carries the highest page
	// number, matching btree.c's up-front child allocation order.
	tail := t.pager.AllocatePage()

	prev := left
	for _, s := range splits {
		dst, err := t.pager.ReadPage(s.pageNum)
		if err != nil {
			return err
		}
		cur := make([]byte, len(dst.Data))
		copy(cur, dst.Data)
		copy(dst.Data, prev)
		if err := t.pager.WritePage(dst); err != nil {
			return err
		}
		prev = cur
	}
	copy(tail.Data, prev)
	if err := t.pager.WritePage(tail); err != nil {
		return err
	}

	// Children in key order after the rotation; seps[i] separates child i
	// from child i+1 (splits[i].medianKey semantics are unchanged).
	children := make([]uint32, 0, len(splits)+1)
	seps := make([]uint64, 0, len(splits))
	for _, s := range splits {
		children = append(children, s.pageNum)
		seps = append(seps, s.medianKey)
	}
	children = append(children, tail.PageNum)
	return t.writeInteriorRootAt(t.rootPage, children, seps)
}

// writeInteriorRootAt rewrites page dst as a fresh interior node over the
// ordered children: cell j points at children[j] with separator seps[j], and
// children[len-1] is the rightmost pointer.
func (t *BTree) writeInteriorRootAt(dst uint32, children []uint32, seps []uint64) error {
	pg, err := t.pager.ReadPage(dst)
	if err != nil {
		return err
	}
	coff := contentOffset(dst)
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	if t.isTable {
		pg.Data[coff] = storage.PageTypeInteriorTable
	} else {
		pg.Data[coff] = storage.PageTypeInteriorIndex
	}
	cellData := t.encodeInteriorCell(children[0], seps[0])
	cellStart := int(t.pageSize) - len(cellData)
	copy(pg.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(pg.Data[coff+cellPtrOffset(pg.Data[coff]):], uint16(cellStart))
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 1)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(cellStart))
	binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], children[len(children)-1]) // rightmostPtr
	if err := t.pager.WritePage(pg); err != nil {
		return err
	}
	for i := 1; i < len(seps); i++ {
		if err := t.addInteriorCellToPage(dst, children[i], seps[i], children[i+1]); err != nil {
			return err
		}
	}
	return nil
}

// insertPage inserts a cell into the page at pageNum.
// Returns the new pages produced by a split (each with the median key
// separating it from the previous page), or nil when no split occurred. The
// caller must add each (pageNum, medianKey, newPage) pointer to the parent
// interior page.
func (t *BTree) insertPage(pageNum uint32, newCell *storage.Cell) ([]leafSplitResult, error) {
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return nil, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return nil, err
	}

	switch page.PageType {
	case storage.PageTypeLeafTable, storage.PageTypeLeafIndex:
		return t.insertLeafPage(pg, page, newCell)
	case storage.PageTypeInteriorTable, storage.PageTypeInteriorIndex:
		return t.insertInteriorPage(pg, page, newCell)
	default:
		return nil, fmt.Errorf("btree: unknown page type 0x%02x", page.PageType)
	}
}

// prepareCell writes overflow pages for a cell whose payload exceeds what
// fits in the cell itself, mutating the cell to reference the overflow chain
// (PayloadLen = full length, LocalLen = local portion, Overflow = first
// overflow page). The Payload slice is left intact so callers can still use
// the full key for ordering comparisons. For cells that fit, it leaves the
// cell unchanged.
func (t *BTree) prepareCell(c *storage.Cell) error {
	cellType := storage.CellTableLeaf
	if !t.isTable {
		cellType = storage.CellIndexLeaf
	}
	plen := len(c.Payload)
	local := storage.LocalPayloadSize(plen, int(t.usableSize), cellType)
	if local >= plen {
		return nil
	}
	first, err := t.writeOverflowPages(c.Payload[local:])
	if err != nil {
		return err
	}
	c.Overflow = first
	c.PayloadLen = plen
	c.LocalLen = local
	return nil
}

// writeOverflowPages stores payload on a chain of overflow pages and returns
// the first page number. Each overflow page holds up to pageSize-4 payload
// bytes, prefixed with a 4-byte big-endian next-page pointer (0 = last).
func (t *BTree) writeOverflowPages(payload []byte) (uint32, error) {
	chunk := int(t.usableSize) - 4
	var first uint32
	var prev *pager.Page
	pos := 0
	for pos < len(payload) {
		pg := t.pager.AllocatePage()
		end := pos + chunk
		if end > len(payload) {
			end = len(payload)
		}
		copy(pg.Data[4:], payload[pos:end])
		if prev != nil {
			binary.BigEndian.PutUint32(prev.Data[0:4], pg.PageNum)
			if err := t.pager.WritePage(prev); err != nil {
				return 0, err
			}
		} else {
			first = pg.PageNum
		}
		prev = pg
		pos = end
	}
	if prev != nil {
		binary.BigEndian.PutUint32(prev.Data[0:4], 0) // last page
		if err := t.pager.WritePage(prev); err != nil {
			return 0, err
		}
	}
	return first, nil
}

// readOverflow expands a decoded cell's local payload to the full payload by
// following the overflow chain. The returned cell is a copy when expansion is
// needed; the input cell is never mutated so it can still be re-encoded.
// Cells without overflow are returned unchanged.
func (t *BTree) readOverflow(c *storage.Cell) (*storage.Cell, error) {
	if c.Overflow == 0 {
		return c, nil
	}
	full := make([]byte, 0, c.PayloadLen)
	full = append(full, c.Payload...)
	pageNum := c.Overflow
	remaining := c.PayloadLen - len(c.Payload)
	for remaining > 0 {
		pg, err := t.pager.ReadPage(pageNum)
		if err != nil {
			return nil, err
		}
		next := binary.BigEndian.Uint32(pg.Data[0:4])
		chunk := int(t.usableSize) - 4
		n := chunk
		if n > remaining {
			n = remaining
		}
		full = append(full, pg.Data[4:4+n]...)
		remaining -= n
		pageNum = next
		if pageNum == 0 && remaining > 0 {
			return nil, fmt.Errorf("btree: overflow chain ended early")
		}
	}
	c2 := *c
	c2.Payload = full
	c2.Overflow = 0
	return &c2, nil
}

// insertLeafPage inserts a cell into a leaf page. If the leaf is full, it
// splits (possibly into multiple pages). Returns the new pages produced by
// the split (empty when the cell was inserted in place).
func (t *BTree) insertLeafPage(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell) ([]leafSplitResult, error) {
	coff := contentOffset(pg.PageNum)
	if err := t.prepareCell(newCell); err != nil {
		return nil, err
	}
	cellData := storage.EncodeCell(newCell)

	// Table b-trees REPLACE a cell with the same rowid (SQLite btree.c
	// sqlite3BtreeInsert, loc==0: dropCell runs BEFORE insertCellFast, so the
	// later balance()/split never sees two equal keys). Drop the old cell
	// FIRST, on every path — including the full-page split path: leaving it
	// for writeLeafCell only meant a full page redistributed the old cell
	// AND the new cell via splitLeafMulti, duplicating the rowid
	// (fts4merge4 2.2.x: duplicate %_segdir rows).
	if t.isTable {
		if idx := t.findInsertPositionTable(pg, page, newCell.RowID); idx < int(page.CellCount) {
			cellOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
			_, n := util.GetVarint(pg.Data[cellOff:])
			if midRowID, _ := util.GetVarint(pg.Data[cellOff+n:]); int64(midRowID) == newCell.RowID {
				if err := t.deleteCellOnPage(pg, page, idx); err != nil {
					return nil, err
				}
			}
		}
	}

	if leafHasRoom(pg, page, cellData, coff, t.pageSize) {
		// There is room — insert directly.
		if err := t.writeLeafCell(pg, page, newCell, cellData, coff); err != nil {
			return nil, err
		}
		return nil, nil
	}

	// Leaf is full. If empty, the cell is too large for this page (e.g.
		// corrupt.test corrupt-5.2: page_size=1024 + ~100 cincr columns on the
		// sqlite_master root page 1 whose usable local area after the 100-byte
		// database header is too small for the cell's full local form). Retry
		// with a smaller local payload (force some bytes onto overflow pages) so
		// the cell fits in the available local area. SQLite's btree.c
		// btreeParseCellPtr + balance_nonroot route the cell through overflow
		// slots when sz+2 > nFree; we approximate by reducing LocalLen to
		// minLocal and spilling the remainder.
		if page.CellCount == 0 {
			if newCell.Overflow != 0 || len(newCell.Payload) <= storage.MinLocalPayload(int(t.usableSize), storage.CellTableLeaf) {
				return nil, fmt.Errorf("btree: cell too large for page (size=%d, pageSize=%d)", len(cellData), t.pageSize)
			}
			// Re-prepare with reduced local payload.
			cellType := storage.CellTableLeaf
			if !t.isTable {
				cellType = storage.CellIndexLeaf
			}
			reduced := *newCell
			reduced.PayloadLen = len(reduced.Payload)
			reduced.LocalLen = storage.MinLocalPayload(int(t.usableSize), cellType)
			for {
				probe := storage.EncodeCell(&reduced)
				if leafHasRoom(pg, page, probe, coff, t.pageSize) {
					break
				}
				if reduced.LocalLen <= 4 {
					return nil, fmt.Errorf("btree: cell too large for page (size=%d, pageSize=%d)", len(cellData), t.pageSize)
				}
				reduced.LocalLen -= 4
			}
			first, err := t.writeOverflowPages(reduced.Payload[reduced.LocalLen:])
			if err != nil {
				return nil, err
			}
			newCell.Overflow = first
			newCell.PayloadLen = reduced.PayloadLen
			newCell.LocalLen = reduced.LocalLen
			cellData = storage.EncodeCell(newCell)
			if leafHasRoom(pg, page, cellData, coff, t.pageSize) {
				if err := t.writeLeafCell(pg, page, newCell, cellData, coff); err != nil {
					return nil, err
				}
				return nil, nil
			}
			return nil, fmt.Errorf("btree: cell too large for page (size=%d, pageSize=%d)", len(cellData), t.pageSize)
		}

	// Split the leaf, distributing existing cells plus the new cell across
	// as many pages as needed (the new cell is already written by the split).
	results, err := t.splitLeafMulti(pg, page, newCell, cellData)
	if err != nil {
		return nil, err
	}
	if err := t.pager.WritePage(pg); err != nil {
		return nil, err
	}
	for _, r := range results {
		newPg, rerr := t.pager.ReadPage(r.pageNum)
		if rerr != nil {
			return nil, rerr
		}
		if err := t.pager.WritePage(newPg); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// insertInteriorPage inserts a cell into an interior page by routing to the
// correct child. If the child splits, adds the new pointers. If this interior
// page is then full, splits it too.
func (t *BTree) insertInteriorPage(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell) ([]leafSplitResult, error) {
	// Find the child page that should receive the new cell
	childPageNum := t.findChildPageForInsert(pg, page, newCell)

	// Recursively insert into the child
	childSplits, err := t.insertPage(childPageNum, newCell)
	if err != nil {
		return nil, err
	}

	if len(childSplits) == 0 {
		// No split occurred — done
		return nil, nil
	}

	// Child split occurred. Apply the separator chain to this interior page.
		if err := t.applyChildSplits(pg, page, childPageNum, childSplits); err != nil {
			if err != errInteriorFull {
				return nil, err
			}
			// This interior page is full. Split it, then re-locate the original
			// child in the new structure: the child may have moved to the new
			// right half (entries[splitIdx+1..]) when its position in the original
			// page was above splitIdx, or it may now be the rightmost pointer of
			// the left half (entries[splitIdx].leftChild). The splitKey comparison
			// alone is insufficient for index b-trees (findChildPageForInsert
			// returns the rightmost unconditionally) and for table b-trees when
			// the split boundary lands exactly on the child's first key.
			newInteriorNum, splitKey, serr := t.splitInteriorPage(pg, page)
			if serr != nil {
				return nil, serr
			}
			target, ferr := t.findParentOfChild(pg.PageNum, newInteriorNum, childPageNum)
			if ferr != nil {
				return nil, ferr
			}
			if target == 0 {
				return nil, fmt.Errorf("btree: parent split lost child %d", childPageNum)
			}
			tp, rerr := t.pager.ReadPage(target)
			if rerr != nil {
				return nil, rerr
			}
			tpage, perr := storage.ParsePage(tp.Data, int(t.pageSize), contentOffset(tp.PageNum))
			if perr != nil {
				return nil, perr
			}
			if aerr := t.applyChildSplits(tp, tpage, childPageNum, childSplits); aerr != nil {
				return nil, aerr
			}
			return []leafSplitResult{{pageNum: newInteriorNum, medianKey: splitKey}}, nil
		}

	return nil, nil
}

// errInteriorFull signals that an interior page has no room for the
// separator cells of a child split.
var errInteriorFull = fmt.Errorf("btree: interior page full, cannot add child pointer")

// applyChildSplits updates this interior page after a child page split into
// len(splits)+1 pages. SQLite's balance_nonroot semantics for a table b-tree:
// the parent cell that pointed at the splitting child keeps its LEFT child
// but gets its divider KEY replaced by the first split's median; each new
// sibling page is inserted right after it carrying the PREVIOUS upper bound,
// so the last new cell ends up with the original divider as its key and no
// unrelated subtree pointer is ever touched.
//
//	cell_j = (C, Kold)            →  (C, D1), (P1, D2), …, (Pn-1, Kold)
func (t *BTree) applyChildSplits(pg *pager.Page, page *storage.BTreePage, origChild uint32, splits []leafSplitResult) error {
	coff := contentOffset(pg.PageNum)
	ptroff := cellPtrOffset(page.PageType)
	ptrBase := coff + ptroff

	// Locate the cell pointing at the original child.
	idx := -1
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		if binary.BigEndian.Uint32(pg.Data[cellOff:cellOff+4]) == origChild {
			idx = i
			break
		}
	}
	if idx < 0 && page.RightmostPtr == origChild {
		// The split child is the rightmost pointer: it has no divider cell.
		// Append one divider cell per split page and move the rightmost
		// pointer to the LAST new page:
		//   rightmost=C, splits=[(P1,D1)..(Pn,Dn)]  →
		//   cells += (C,D1),(P1,D2)…(Pn-1,Dn);  rightmost = Pn
		for si := 0; si < len(splits); si++ {
			leftOfCell := origChild
			if si > 0 {
				leftOfCell = splits[si-1].pageNum
			}
			newData := t.encodeInteriorCell(leftOfCell, splits[si].medianKey)
			ncStart := int(page.CellContent) - len(newData)
			nCount := int(page.CellCount) + 1
			ncPtrEnd := coff + ptroff + nCount*2 + 2
			if ncStart < ncPtrEnd || page.CellContent == 0 {
				return errInteriorFull
			}
			copy(pg.Data[ncStart:], newData)
			binary.BigEndian.PutUint16(pg.Data[ptrBase+int(page.CellCount)*2:], uint16(ncStart))
			page.CellCount = uint16(nCount)
			// Advance the content pointer: the next appended cell must land
			// BELOW this one. Without this, iteration si+1 recomputes ncStart
			// from the stale offset and overwrites cell si's bytes (both cell
			// pointers then read identical child/key data — duplicate adjacent
			// separators, orphaned keys).
			page.CellContent = uint16(ncStart)
			binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], uint16(nCount))
			binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(ncStart))
		}
		binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], splits[len(splits)-1].pageNum)
		return t.pager.WritePage(pg)
	}
	if idx < 0 {
		return fmt.Errorf("btree: parent %d has no cell for split child %d", pg.PageNum, origChild)
	}

	// EXACT room precheck — applyChildSplits must be ATOMIC: a mid-chain
	// errInteriorFull would leave partially-mutated page data visible through
	// the pager cache, and the retry-after-parent-split then desyncs the
	// structure and orphans whole subtrees (fts4opt churn: keys stranded on
	// unreachable pages).
	//
	// Per split i: the current cell is RELOCATED (4 + varint(D_i) bytes — a
	// wider divider varint must never be written in place, it would overrun
	// the neighbor) and one sibling cell (4 + varint(Kold)) is appended.
	n := len(splits)
	carrierKey := t.cellKeyAt(pg, ptrBase, idx)
	dataNeed := 0
	for _, cs := range splits {
		dataNeed += 4 + util.VarintLen(cs.medianKey)
	}
	dataNeed += n * (4 + util.VarintLen(carrierKey))
	cellContentEnd := int(page.CellContent)
	ptrNeed := coff + ptroff + (int(page.CellCount)+n)*2 + 2
	if cellContentEnd == 0 {
		return errInteriorFull
	}
	if cellContentEnd-dataNeed < ptrNeed {
		return errInteriorFull
	}

	// Carry the upper bound through the chain: the ORIGINAL cell's key.
	for si, cs := range splits {
		// Re-key the current cell: it now bounds curLeft by cs.medianKey.
		curLeft := origChild
		if si > 0 {
			curLeft = splits[si-1].pageNum
		}
		_ = curLeft // the located cell's left child already equals curLeft
		// Re-key by RELOCATING the cell: writing a wider varint in place
		// would overrun the cell's bytes into its neighbor (the source of
		// duplicate/garbled separators after this fix landed).
		curChildOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
		curChild := binary.BigEndian.Uint32(pg.Data[curChildOff : curChildOff+4])
		rekeyed := t.encodeInteriorCell(curChild, cs.medianKey)
		rkStart := int(page.CellContent) - len(rekeyed)
		if rkStart < coff+ptroff+(int(page.CellCount)+1)*2+2 {
			return errInteriorFull
		}
		copy(pg.Data[rkStart:], rekeyed)
		binary.BigEndian.PutUint16(pg.Data[ptrBase+idx*2:], uint16(rkStart))
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(rkStart))
		page.CellContent = uint16(rkStart)
		// Insert the new sibling cell AFTER it, carrying carrierKey.
		newData := t.encodeInteriorCell(cs.pageNum, carrierKey)
		ncStart := int(page.CellContent) - len(newData)
		nCount := int(page.CellCount) + 1
		ncPtrEnd := coff + ptroff + nCount*2 + 2
		if ncStart < ncPtrEnd {
			return errInteriorFull // unreachable: exact precheck above
		}
		copy(pg.Data[ncStart:], newData)
		// Advance the content pointer past the sibling cell: the next
		// iteration's relocated cell must land BELOW it, otherwise the two
		// writes overlap and both cell pointers read identical bytes.
		page.CellContent = uint16(ncStart)
		// Shift pointers [idx+1..CellCount) right by one.
		for i := int(page.CellCount); i > idx+1; i-- {
			src := ptrBase + (i-1)*2
			dst := ptrBase + i*2
			pg.Data[dst] = pg.Data[src]
			pg.Data[dst+1] = pg.Data[src+1]
		}
		binary.BigEndian.PutUint16(pg.Data[ptrBase+(idx+1)*2:], uint16(ncStart))
		page.CellCount = uint16(nCount)
		binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], uint16(nCount))
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(ncStart))
		idx++
	}
	return t.pager.WritePage(pg)
}

// addInteriorCellToPage reads the page at pageNum and adds a child pointer
// cell for the given split key and new sibling page.
func (t *BTree) addInteriorCellToPage(pageNum, childPageNum uint32, childSplitKey uint64, childNewSibling uint32) error {
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return err
	}
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return err
	}
	return t.addInteriorCell(pg, page, childPageNum, childSplitKey, childNewSibling)
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

	// SQLite's table b-tree REPLACES a cell with the same rowid rather than
	// inserting a duplicate (sqlite3BtreeInsert with the same key overwrites).
	// The engine's position search returns the first cell with rowid >= the
	// target; if that cell has the SAME rowid, remove it first so the normal
	// insert below writes a single cell (a second cell with the same rowid
	// makes DELETE/UPDATE/seek hit the wrong row and duplicates appear in
	// scans — fts4merge4 2.2.x: the L0 flush and L2 output re-used rowids
	// 33/34, creating duplicate %_segdir rows).
	if t.isTable && insertIdx < int(page.CellCount) {
		cellOff := int(storage.CellPointer(pg.Data, coff, insertIdx, int(t.pageSize)))
		_, n := util.GetVarint(pg.Data[cellOff:])
		midRowID, _ := util.GetVarint(pg.Data[cellOff+n:])
		if int64(midRowID) == newCell.RowID {
			if err := t.deleteCellOnPage(pg, page, insertIdx); err != nil {
				return err
			}
			// Recompute the insertion position after the deletion.
			insertIdx = t.findInsertPositionTable(pg, page, newCell.RowID)
		}
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
func (t *BTree) splitLeafMulti(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell, newCellData []byte) ([]leafSplitResult, error) {
	coff := contentOffset(pg.PageNum)

	cellType := storage.CellTableLeaf
	if !t.isTable {
		cellType = storage.CellIndexLeaf
	}

	cells, err := t.readCellsForSplit(pg, page, coff, cellType, newCell, newCellData)
	if err != nil {
		return nil, err
	}
	sortSplitCells(cells, t.isTable)

	// Greedily partition the sorted cells into pages: each page takes the
	// longest prefix that fits. Every page holds at least one cell (a single
	// cell whose local payload is oversized goes alone — prepareCell caps the
	// local payload so this is defensive).
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
			if !leafCellsFit(cellDatas(probe), coff, int(t.pageSize)) {
				flush()
			}
		}
		cur = append(cur, c)
	}
	flush()
	if len(partitions) == 0 {
		return nil, fmt.Errorf("btree: split failed: cannot balance leaf pages")
	}

	// Clear original leaf content (except page type)
	for i := coff + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}

	// Write the first partition to the original leaf.
	if err := writeLeafHalf(pg, coff, partitions[0], int(t.pageSize)); err != nil {
		return nil, err
	}
	// Pre-allocate every new page, then write each partition and set the
	// right-sibling chain pointers between consecutive pages (a chain pointer
	// must reference the page that actually holds the next partition; a
	// leaked allocation would make the cursor follow an unwritten page).
	nNew := len(partitions) - 1
	newPages := make([]*pager.Page, 0, nNew)
	for i := 0; i < nNew; i++ {
		newPages = append(newPages, t.pager.AllocatePage())
	}
	// The original leaf's right-sibling chain must point to the first new
	// page (a full scan follows the chain from the leftmost leaf).
	if nNew > 0 {
		binary.BigEndian.PutUint32(pg.Data[int(t.pageSize)-4:int(t.pageSize)], newPages[0].PageNum)
	}
	if err := t.pager.WritePage(pg); err != nil {
		return nil, err
	}
	results := make([]leafSplitResult, 0, nNew)
	for pi := 1; pi < len(partitions); pi++ {
		newPg := newPages[pi-1]
		newCoff := contentOffset(newPg.PageNum)
		newPg.Data[newCoff] = pg.Data[coff] // same page type
		if err := writeLeafHalf(newPg, newCoff, partitions[pi], int(t.pageSize)); err != nil {
			return nil, err
		}
		// Right-sibling chain pointer (last 4 bytes): the next partition's
		// page, or 0 for the last new page.
		chainOff := int(t.pageSize) - 4
		if pi < len(partitions)-1 {
			binary.BigEndian.PutUint32(newPg.Data[chainOff:chainOff+4], newPages[pi].PageNum)
		} else {
			binary.BigEndian.PutUint32(newPg.Data[chainOff:chainOff+4], 0)
		}
		if err := t.pager.WritePage(newPg); err != nil {
			return nil, err
		}
		var medianKey uint64
		if t.isTable {
			medianKey = uint64(partitions[pi][0].cell.RowID)
		} else {
			medianKey = uint64(len(partitions[pi][0].cellData))
		}
		results = append(results, leafSplitResult{pageNum: newPg.PageNum, medianKey: medianKey})
	}
	return results, nil
}

// leafSplitResult is one new page produced by splitLeafMulti: the page number
// and the median key separating it from the previous page.
type leafSplitResult struct {
	pageNum   uint32
	medianKey uint64
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
			e.key = full.Payload
		}
		cells = append(cells, e)
	}
	// Include the new cell in the redistribution — REPLACING any existing
	// cell with the same key (SQLite's sqlite3BtreeInsert overwrites in
	// place; the non-split path dedupes in writeLeafCell, and the split
	// path must too, otherwise overwriting a full page's boundary rowid
	// writes the rowid twice — duplicate rowids across/within partitions,
	// fts4opt churn: rowid 229 duplicated on leaf 171).
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
func sortSplitCells(cells []splitEntry, isTable bool) {
	if isTable {
		bubbleSortSplitCells(cells, func(a, b splitEntry) bool {
			return a.cell.RowID > b.cell.RowID
		})
		return
	}
	bubbleSortSplitCells(cells, func(a, b splitEntry) bool {
		return util.CompareValues(a.key, b.key) > 0
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
// downward, and updates the page header's cell count and content end. The
// last 4 bytes are reserved for the right-sibling chain pointer (the caller
// writes it after this returns).
func writeLeafHalf(pg *pager.Page, coff int, half []splitEntry, pageSize int) error {
	var count uint16
	end := pageSize - 4 // leaves reserve the 4-byte chain pointer at the end
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
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], count)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(end))
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
// with the given content offset, leaving room for the cell pointer array and
// the 4-byte right-sibling chain pointer at the page end.
func leafCellsFit(cells [][]byte, coff, pageSize int) bool {
	total := 0
	for _, d := range cells {
		total += len(d)
	}
	ptrEnd := coff + storage.CellPointerOffset + len(cells)*2 + 2
	contentEnd := pageSize - 4 // reserve the 4-byte right-sibling chain pointer
	return contentEnd-total >= ptrEnd
}

// createInteriorRoot creates an interior page pointing to two children.
func (t *BTree) createInteriorRoot(leftChild uint32, medianKey uint64, rightChild uint32) (*pager.Page, error) {
	// The schema b-tree (sqlite_schema) is permanently rooted at page 1:
	// page 1 is the database file header page and cannot be demoted to a
	// child. When its root splits, page 1 becomes an interior page and the
	// split halves are moved to newly allocated pages (SQLite semantics).
	if t.rootPage == 1 {
		return t.createInteriorRootAtPage1(medianKey, rightChild)
	}
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

// createInteriorRootAtPage1 converts page 1 (the schema b-tree root, which
// must remain the root because it is the file header page) into an interior
// page after a split. The split's lower half currently stored in page 1 is
// moved to a newly allocated leaf so page 1 becomes a pure interior page
// pointing to both halves.
func (t *BTree) createInteriorRootAtPage1(medianKey uint64, rightChild uint32) (*pager.Page, error) {
	pg1, err := t.pager.ReadPage(1)
	if err != nil {
		return nil, err
	}

	// Move page 1's current content to a fresh page. Page 1's b-tree
	// content lives at offset 100 (after the file header) while a normal
	// page's content starts at offset 0, so the b-tree header and cell
	// pointer array are relocated; the cell data offsets are absolute
	// positions within the page and copy verbatim. The relocated page
	// keeps its ORIGINAL page type: this path runs both for the classic
	// first leaf split (page 1 holds leaf content) AND when a split
	// bubbles up from an interior child (page 1 then holds INTERIOR
	// entries plus its rightmost pointer). Relocating an interior page 1
	// as a leaf orphaned that entire subtree — every row under it vanished
	// from scans and seeks while its cells stayed on the now-unreferenced
	// pages (fts4opt 2.x churn: blockids went "missing" after DELETE FROM
	// + regrowth crossed the second root split).
	oldType := pg1.Data[contentOffset(1)]
	interior := oldType == storage.PageTypeInteriorTable || oldType == storage.PageTypeInteriorIndex
	hdrLen := 8
	if interior {
		hdrLen = 12 // interior pages carry the rightmost pointer at 8..12
	}
	newLeft := t.pager.AllocatePage()

	copy(newLeft.Data, pg1.Data)
	copy(newLeft.Data[0:hdrLen], pg1.Data[100:100+hdrLen])
	n := int(binary.BigEndian.Uint16(pg1.Data[103:105]))
	copy(newLeft.Data[hdrLen:hdrLen+2*n], pg1.Data[100+hdrLen:100+hdrLen+2*n])
	newLeft.Data[0] = oldType
	if err := t.pager.WritePage(newLeft); err != nil {
		return nil, err
	}

	// Convert page 1 into an interior page: one cell {newLeft, medianKey}
	// and rightmostChild = rightChild. Keep the 100-byte file header.
	rootCoff := contentOffset(1)
	pg1.Data[rootCoff] = storage.PageTypeInteriorTable
	for i := rootCoff + 1; i < int(t.pageSize); i++ {
		pg1.Data[i] = 0
	}
	cellData := t.encodeInteriorCell(newLeft.PageNum, medianKey)
	cellStart := int(t.pageSize) - len(cellData)
	copy(pg1.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(pg1.Data[rootCoff+cellPtrOffset(pg1.Data[rootCoff]):], uint16(cellStart))
	binary.BigEndian.PutUint16(pg1.Data[rootCoff+3:rootCoff+5], 1)
	binary.BigEndian.PutUint16(pg1.Data[rootCoff+5:rootCoff+7], uint16(cellStart))
	binary.BigEndian.PutUint32(pg1.Data[rootCoff+8:rootCoff+12], rightChild) // rightmostPtr

	if err := t.pager.WritePage(pg1); err != nil {
		return nil, err
	}
	return pg1, nil
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
// cellKeyAt reads the divider key of the interior cell at pointer index idx.
func (t *BTree) cellKeyAt(pg *pager.Page, ptrBase, idx int) uint64 {
	off := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
	k, _ := util.GetVarint(pg.Data[off+4:])
	return k
}

// setCellKeyAt rewrites the divider key of the interior cell at idx. The
// varint is written in place — divider keys are rowids whose width can grow,
// so the caller must reserve room via the same 14-byte slack used by
// applyChildSplits' room check.
func (t *BTree) setCellKeyAt(pg *pager.Page, ptrBase, idx int, key uint64) {
	off := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
	util.PutVarint(pg.Data[off+4:], key)
}

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
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(t.pageSize)))
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
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(t.pageSize))
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

	// childInPage returns true if `child` is one of the cells' leftChild or the rightmost pointer.
	func (t *BTree) childInPage(pg *pager.Page, page *storage.BTreePage, child uint32, coff int) bool {
		ptroff := cellPtrOffset(page.PageType)
		ptrBase := coff + ptroff
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
			if binary.BigEndian.Uint32(pg.Data[cellOff:cellOff+4]) == child {
				return true
			}
		}
		return page.RightmostPtr == child
	}

	// findParentOfChild scans both halves of a just-split parent and returns the
	// page number that contains the given child (either as a cell's leftChild or
	// the rightmost pointer). Returns 0 if neither half holds it.
	func (t *BTree) findParentOfChild(leftNum, rightNum uint32, child uint32) (uint32, error) {
		for _, pn := range []uint32{leftNum, rightNum} {
			pg, err := t.pager.ReadPage(pn)
			if err != nil {
				return 0, err
			}
			coff := contentOffset(pn)
			page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
			if err != nil {
				return 0, err
			}
			if t.childInPage(pg, page, child, coff) {
				return pn, nil
			}
		}
		return 0, nil
	}

	// findChildPageForInsert returns the child page that should receive the new cell.
func (t *BTree) findChildPageForInsert(pg *pager.Page, page *storage.BTreePage, cell *storage.Cell) uint32 {
	if !t.isTable {
		return page.RightmostPtr // for index b-trees, always append to rightmost
	}
	coff := contentOffset(pg.PageNum)
	// Binary search on row IDs in interior page. CellPointer adds 8 internally,
	// so passing coff+4 yields coff+12, the interior cell-pointer array offset
	// (header 8 + 4-byte rightmost pointer).
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, coff+4, mid, int(t.pageSize)))
		midRowID, _ := util.GetVarint(pg.Data[cellOff+4:])
		if int64(midRowID) <= cell.RowID {
			// SQLite routes a key equal to an interior separator to the
			// RIGHT (the separator's left child holds keys < key; the next
			// cell's left child, or the rightmost pointer, holds keys >= key).
			// Using < here sent a same-rowid re-insert (repack rewrite, merge
			// output at an existing rowid) into the WRONG leaf, creating a
			// duplicate rowid (fts4merge4: %_segdir rowid 21 and 26 each
			// appeared twice).
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if lo < int(page.CellCount) {
		cellOff := int(storage.CellPointer(pg.Data, coff+4, lo, int(t.pageSize)))
		return binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
	}
	return page.RightmostPtr
}

// addInteriorCell adds a new cell to an interior page.
func (t *BTree) addInteriorCell(pg *pager.Page, page *storage.BTreePage, leftChild uint32, key uint64, rightChild uint32) error {
	coff := contentOffset(pg.PageNum)
	if os.Getenv("BT_DBG") != "" {
		fmt.Printf("[ADDI] parent=%d count=%d left=%d key=%d right=%d cc0=%d fragfree=%d\n", pg.PageNum, page.CellCount, leftChild, key, rightChild, int(page.CellContent), page.FragFree)
		defer func() {
			if pg.PageNum == 781 {
				co := coff
				cc := int(binary.BigEndian.Uint16(pg.Data[co+5 : co+7]))
				n := int(binary.BigEndian.Uint16(pg.Data[co+3 : co+5]))
				line := fmt.Sprintf("[ADDI] done parent=%d ccNow=%d(count=%d) tail:", pg.PageNum, cc, n)
				for j := n - 4; j < n; j++ {
					if j < 0 {
						continue
					}
					o := int(binary.BigEndian.Uint16(pg.Data[co+12+2*j : co+12+2*j+2]))
					lc := binary.BigEndian.Uint32(pg.Data[o : o+4])
					k, _ := util.GetVarint(pg.Data[o+4:])
					line += fmt.Sprintf(" [%d]off=%d{L=%d,K=%d}", j, o, lc, k)
				}
				fmt.Println(line)
			}
		}()
	}
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

	// Find the insert position by key so interior cells stay sorted.
	// The new cell is the separator "leftChild ... key ... rightChild"; it
	// belongs after every existing cell whose key is < key.
	ptrBase := coff + ptroff
	insertIdx := int(page.CellCount)
	for i := int(page.CellCount) - 1; i >= 0; i-- {
		cellOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		ekey, _ := util.GetVarint(pg.Data[cellOff+4:])
		if ekey <= key {
			insertIdx = i + 1
			break
		}
		insertIdx = i
	}

	// Shift cells at [insertIdx..CellCount) right by one slot.
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

	// If the new key is the largest, the new child becomes the rightmost;
	// otherwise the cell that follows the new separator (the old pointer to
	// the split leaf) must be repointed to the new sibling. The separator
	// {leftChild, key} routes keys < key to leftChild and keys >= key to
	// the NEXT cell's left child (or the rightmost pointer for the last
	// cell), so the sibling becomes the next cell's left child.
	if insertIdx == int(page.CellCount)-1 {
		binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], rightChild)
	} else {
		nextOff := int(binary.BigEndian.Uint16(pg.Data[ptrBase+(insertIdx+1)*2 : ptrBase+(insertIdx+1)*2+2]))
		binary.BigEndian.PutUint32(pg.Data[nextOff:nextOff+4], rightChild)
	}

	return t.pager.WritePage(pg)
}
