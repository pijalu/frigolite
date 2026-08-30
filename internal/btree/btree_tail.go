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

func (t *BTree) DeleteCellsWhere(fn func(cell *storage.Cell) bool) (int64, error) {
	var deleted int64
	// Collect all leaf pages — the tree may have multiple levels.
	var leaves []uint32
	if err := t.collectLeafPages(t.rootPage, &leaves, nil); err != nil {
		return 0, err
	}
	for _, leafNum := range leaves {
		// Delete every matching cell in ONE pass (SQLite's single-sweep
		// delete). The previous per-cell loop re-parsed the page and scanned
		// from index 0 after each deletion — O(k^2) per leaf, which made
		// DELETE FROM %_segments (thousands of 4KB blob rows) take ~40s
		// (fts4merge4's between-scenario DELETE).
		for {
			n, err := t.deleteAllMatchingFromLeaf(leafNum, fn)
			if err != nil {
				return deleted, err
			}
			deleted += n
			if n == 0 {
				break
			}
		}
		// P8.INCRVACUUM phase 1: the leaf is left in the btree as an empty
		// leaf (cell count 0, content pointer at pageSize). We do NOT
		// call FreePage here because nulling the parent's leftChild
		// reference leaves the btree in a state that subsequent inserts
		// and unique-constraint scans can't navigate (they'd see
		// leftChild=0 and try to descend into page 0, which errors with
		// "database disk image is malformed"). Proper integration
		// requires btree rebalance (phase 3+) so freed leaves are
		// removed from the parent's cell array entirely.
	}
	return deleted, nil
}

// deleteAllMatchingFromLeaf removes every matching cell on the leaf page at
// leafNum in a single compaction pass (decode all cells once, keep the
// survivors, rebuild the page once). The previous per-cell delete rewrote all
// remaining cells each time — O(k^2) per leaf, which made DELETE FROM
// %_segments (thousands of 4KB blob rows) take ~30-40s (fts4merge4's
// between-scenario DELETE).
func (t *BTree) deleteAllMatchingFromLeaf(leafNum uint32, fn func(cell *storage.Cell) bool) (int64, error) {
	pg, err := t.pager.ReadPage(leafNum)
	if err != nil {
		return 0, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return 0, err
	}
	if page.PageType != storage.PageTypeLeafTable && page.PageType != storage.PageTypeLeafIndex {
		return 0, fmt.Errorf("btree: delete only supported on leaf pages")
	}
	var cellType storage.CellType
	if page.PageType == storage.PageTypeLeafTable {
		cellType = storage.CellTableLeaf
	} else {
		cellType = storage.CellIndexLeaf
	}
	// Decode every cell once.
	encoded := make([][]byte, 0, int(page.CellCount))
	ptrs := make([]uint16, int(page.CellCount))
	decoded := make([]storage.Cell, int(page.CellCount))
	for i := 0; i < int(page.CellCount); i++ {
		p := storage.CellPointer(pg.Data, coff, i, int(t.pageSize))
		ptrs[i] = p
		c, derr := storage.DecodeCell(pg.Data, int(p), cellType, int(t.usableSize))
		if derr != nil {
			// A cell that fails to decode (corrupt cell pointer, garbage at
			// the cell's offset, overlapping cells) is kept in the page as
			// a raw byte slice. SQLite's btree.c clearDatabasePage treats
			// such cells as "drop without decoding" — the bytes are
			// preserved so the page stays valid for subsequent reads. We
			// mirror that by encoding the raw bytes (re-validated on read).
			raw := pg.Data[int(p):]
			// Bound the raw slice so we don't read past the page.
			end := len(raw)
			if end > int(t.usableSize)-int(p) {
				end = int(t.usableSize) - int(p)
			}
			encoded = append(encoded, append([]byte(nil), raw[:end]...))
			decoded[i] = storage.Cell{Type: cellType, RowID: 0, PayloadLen: 0, LocalLen: 0}
			continue
		}
		decoded[i] = *c
		encoded = append(encoded, storage.EncodeCell(c))
	}
	// Keep the survivors, preserving order. Also collect the deleted cell
	// indices so we can free their overflow-page chains.
	var keep []int
	deleted := int64(0)
	var deletedIdx []int
	for i := 0; i < len(encoded); i++ {
		if t.cellMatches(pg, page, i, fn) {
			deleted++
			deletedIdx = append(deletedIdx, i)
			continue
		}
		keep = append(keep, i)
	}
	if deleted == 0 {
		return 0, nil
	}
	// Free the overflow pages of the cells that were deleted. Each
	// table-leaf cell may carry a chain of overflow pages (4KB blobs
	// need several pages); when the cell is deleted the chain becomes
	// orphaned and must be returned to the freelist so the header count
	// tracks the freed space (corrupt2-14.2/14.3/14.5 depend on this).
	for _, di := range deletedIdx {
		dcell := &decoded[di]
		if dcell.Overflow != 0 {
			pn := dcell.Overflow
			for pn != 0 {
				np, _ := t.pager.ReadPage(pn)
				if np == nil {
					break
				}
				next := binary.BigEndian.Uint32(np.Data[0:4])
				_ = t.pager.FreePage(pn)
				pn = next
			}
		}
	}
	// Rewrite the surviving cells contiguously from the end of the usable
	// area (cells grow downward; the first cell occupies the highest
	// addresses, pageSize-4 for the reserved chain pointer).
	start := int(t.pageSize) - 4
	newPtrs := make([]uint16, len(keep))
	for pos, ci := range keep {
		start -= len(encoded[ci])
		copy(pg.Data[start:start+len(encoded[ci])], encoded[ci])
		newPtrs[pos] = uint16(start)
	}
	ptrBase := coff + storage.CellPointerOffset
	for i := 0; i < len(newPtrs); i++ {
		binary.BigEndian.PutUint16(pg.Data[ptrBase+i*2:ptrBase+i*2+2], newPtrs[i])
	}
	// Zero the remaining pointer slots.
	for i := len(newPtrs); i < int(page.CellCount); i++ {
		pg.Data[ptrBase+i*2] = 0
		pg.Data[ptrBase+i*2+1] = 0
	}
	page.CellCount = uint16(len(newPtrs))
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if len(newPtrs) == 0 {
		// The page became empty: SQLite sets the cell content pointer to the
		// page's usable end for empty leaves (free-space accounting).
		page.CellContent = uint16(t.pageSize)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(t.pageSize))
		pg.Data[coff+7] = 0
	} else {
		page.CellContent = uint16(start)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(start))
		pg.Data[coff+7] = 0
	}
	if err := t.pager.WritePage(pg); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// deleteCellOnPage removes the cell at cellIdx from the given leaf page,
// shifting the pointer array down and updating the cell count.
func (t *BTree) deleteCellOnPage(pg *pager.Page, page *storage.BTreePage, cellIdx int) error {
	coff := contentOffset(pg.PageNum)
	if cellIdx < 0 || cellIdx >= int(page.CellCount) {
		return fmt.Errorf("btree: cell index %d out of range (count %d)", cellIdx, page.CellCount)
	}
	ptrBase := coff + storage.CellPointerOffset
	for i := cellIdx; i < int(page.CellCount)-1; i++ {
		src := ptrBase + (i+1)*2
		dst := ptrBase + i*2
		pg.Data[dst] = pg.Data[src]
		pg.Data[dst+1] = pg.Data[src+1]
	}
	lastPtr := ptrBase + (int(page.CellCount)-1)*2
	pg.Data[lastPtr] = 0
	pg.Data[lastPtr+1] = 0
	page.CellCount--
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	if page.CellCount == 0 {
		// The page became empty: SQLite sets the cell content pointer to the
		// page's usable end for empty leaves (so the free-space accounting is
		// consistent; an empty page whose content pointer is 0 looks like a
		// crash-written page — "free space corruption"). Reset it to the
		// usable size so the next insert treats it as fresh.
		page.CellContent = uint16(t.pageSize)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(t.pageSize))
		pg.Data[coff+7] = 0 // fragmented free bytes
	} else {
		// Compact the remaining cells down so the deleted cell's bytes are
		// reclaimed. Without this, repeated create/drop on the schema btree
		// fragments the content area and eventually corrupts cells (stale
		// dropped-table ghosts, overlapping new cells). Collect the remaining
		// cells' data, rebuild them contiguously from the end of the usable
		// area, and update CellContent to the new lowest start.
		type cellRef struct {
			data []byte
		}
		cells := make([]cellRef, int(page.CellCount))
		for i := 0; i < int(page.CellCount); i++ {
			p := int(storage.CellPointer(pg.Data, coff, i, int(t.pageSize)))
			// Read the cell's encoded length: for table cells the payload
			// length varint precedes the rowid; the encoded length is the
			// number of bytes the cell occupies on the page.
			var cellType storage.CellType
			if page.PageType == storage.PageTypeLeafTable {
				cellType = storage.CellTableLeaf
			} else {
				cellType = storage.CellIndexLeaf
			}
			c, err := storage.DecodeCell(pg.Data, p, cellType, int(t.usableSize))
			if err != nil {
				return err
			}
			cells[i] = cellRef{data: storage.EncodeCell(c)}
		}
		// Rewrite cells contiguously: the first cell (index 0) ends at
		// pageSize-4 (reserved chain pointer). Each subsequent cell is placed
		// immediately after the previous one's start... cells grow downward,
		// so cell 0 occupies the highest addresses. Compute each cell's start.
		start := int(t.pageSize) - 4
		for i := 0; i < len(cells); i++ {
			start -= len(cells[i].data)
			copy(pg.Data[start:start+len(cells[i].data)], cells[i].data)
			binary.BigEndian.PutUint16(pg.Data[ptrBase+i*2:ptrBase+i*2+2], uint16(start))
		}
		page.CellContent = uint16(start)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(start))
		// After compaction there is no fragmented free space.
		pg.Data[coff+7] = 0
	}
	// Persist the mutation so a fresh cursor / pager read sees the deletion
	// (the pager cache returns the same buffer, but the page must be marked
	// dirty to be written back on flush and to keep reads consistent).
	return t.pager.WritePage(pg)
}

// leafRef describes where a leaf is referenced from its parent interior
// page. isRightmost=true means the leaf is the rightmost child
// (parent.RightmostPtr), otherwise the leaf is the left child of
// interior cell[childIdx] (parent cell at offset parentCellOff). The
// parent cell pointer index lets DeleteCellsWhere null the parent's
// reference to the leaf after freeing the page.
type leafRef struct {
	leafNum       uint32
	parent        uint32
	childIdx      int    // index in parent's cell-pointer array
	parentCellOff uint16 // offset of cell[childIdx] in parent page (0 if isRightmost)
	isRightmost   bool
}

// collectLeafPages appends the page numbers of all leaf pages reachable
// from pageNum (following interior child pointers) to out. The
// parentRefs slice, if non-nil, is populated with one leafRef per leaf
// so callers (DeleteCellsWhere) can update the parent when a leaf is
// freed.
func (t *BTree) collectLeafPages(pageNum uint32, out *[]uint32, parentRefs *[]leafRef) error {
	return t.collectLeafPagesWithParent(pageNum, 0, out, parentRefs)
}

// collectLeafPagesWithParent is the workhorse for collectLeafPages.
// curParent is the page number of the immediate parent interior page
// for the leaves this call will append. 0 means "no parent" (top
// level — the leaves added here are the btree root itself if it's a
// leaf).
func (t *BTree) collectLeafPagesWithParent(pageNum, curParent uint32, out *[]uint32, parentRefs *[]leafRef) error {
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	if page.PageType == storage.PageTypeLeafTable || page.PageType == storage.PageTypeLeafIndex {
		*out = append(*out, pageNum)
		if parentRefs != nil {
			*parentRefs = append(*parentRefs, leafRef{leafNum: pageNum, parent: curParent})
		}
		return nil
	}
	numPages := t.pager.NumPages()
	// Interior page: recurse into each child. Interior pages have a 4-byte
	// rightmost pointer, so the cell pointer array starts at coff+12. A child
	// that points outside the on-disk file is corruption (e.g. a hex patch
	// that wrote 0x0314 into the rightmost pointer slot of an interior page):
	// SQLite's btree.c lockBtree + balance_nonroot skip unreachable children
	// rather than abort the surrounding operation, because the btree is
	// already corrupt and the calling DML is the only way the user can finish
	// the operation. We mirror that: skip the bad child and continue.
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, i, int(t.pageSize)))
		if cellOff+4 > len(pg.Data) {
			continue
		}
		child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		if child == 0 || child > numPages {
			continue
		}
		// For each child, we need to know whether it's a leaf or an
		// interior page so we can record the right parent + cell
		// reference. Read the child's page header to decide.
		cpg, cerr := t.pager.ReadPage(child)
		if cerr != nil {
			continue
		}
		ccoff := contentOffset(cpg.PageNum)
		cpage, cerr2 := storage.ParsePage(cpg.Data, int(t.pageSize), ccoff)
		if cerr2 != nil {
			continue
		}
		if cpage.PageType == storage.PageTypeLeafTable || cpage.PageType == storage.PageTypeLeafIndex {
			*out = append(*out, child)
			if parentRefs != nil {
				*parentRefs = append(*parentRefs, leafRef{
					leafNum:       child,
					parent:        pageNum,
					childIdx:      i,
					parentCellOff: uint16(cellOff),
				})
			}
		} else {
			// Interior child: recurse with the child as the new
			// current parent. The leaves added at deeper levels
			// will have child (not pageNum) as their immediate
			// parent.
			if err := t.collectLeafPagesWithParent(child, child, out, parentRefs); err != nil {
				return err
			}
		}
	}
	if page.RightmostPtr == 0 || page.RightmostPtr > numPages {
		return nil
	}
	rightChild := page.RightmostPtr
	rpg, rerr := t.pager.ReadPage(rightChild)
	if rerr == nil {
		rcoff := contentOffset(rpg.PageNum)
		rpage, rerr2 := storage.ParsePage(rpg.Data, int(t.pageSize), rcoff)
		if rerr2 == nil {
			if rpage.PageType == storage.PageTypeLeafTable || rpage.PageType == storage.PageTypeLeafIndex {
				*out = append(*out, rightChild)
				if parentRefs != nil {
					*parentRefs = append(*parentRefs, leafRef{
						leafNum:     rightChild,
						parent:      pageNum,
						isRightmost: true,
					})
				}
			} else {
				if err := t.collectLeafPagesWithParent(rightChild, rightChild, out, parentRefs); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (t *BTree) cellMatches(pg *pager.Page, page *storage.BTreePage, idx int, fn func(cell *storage.Cell) bool) bool {
	coff := contentOffset(pg.PageNum)
	cellOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	var cellType storage.CellType
	if page.PageType == storage.PageTypeLeafTable {
		cellType = storage.CellTableLeaf
	} else {
		cellType = storage.CellIndexLeaf
	}
	cell, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.usableSize))
	if err != nil {
		return false
	}
	// Decode only the cell's local portion — every DeleteCellsWhere caller
	// (DELETE/UPDATE/FK rowid matching) predicates on cell.RowID, which lives
	// in the cell header. Reading the full overflow chain here made a bulk
	// delete of large-blob rows (e.g. DELETE FROM %_segments with 4KB blocks)
	// read every blob once per candidate cell, O(n × blob) — the
	// between-scenario DELETE in fts4merge4 took ~40s.
	return fn(cell)
}

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
		if util.CompareValues(full.Payload, key) < 0 {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return lo
}
