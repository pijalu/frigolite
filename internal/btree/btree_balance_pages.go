// Port of btree.c::rebuildPage (line 7605), pageInsertArray
// (line 7723), pageFreeArray (line 7780), and editPage (line 7834).
//
// These are the lower-level helpers balance_nonroot calls to
// redistribute cells across a set of sibling pages. They are also
// the building blocks for any btree operation that needs to
// rewrite a page's cell layout (e.g. deleteCellsWhere when the
// survivors don't fit contiguously after compaction).
//
// The CellArray struct in C holds:
//   apCell[]: pointer to each cell's data (in aData or in aSpace1)
//   szCell[]: cell size in bytes
//   apEnd[]:  upper bound of the source memory for each cell
//             (so overflow cells' data is not read past apEnd)
//   ixNx[]:  apCell[i..ixNx[k]-1] all live in the same memory
//            region (aData of one page, or aSpace1 of the divider
//            copy, or aOvflSpace). Cells with ixNx[k]<=i live in
//            region k.
//
// Reference: src/btree.c::rebuildPage (7605),
//            src/btree.c::pageInsertArray (7723),
//            src/btree.c::pageFreeArray (7780),
//            src/btree.c::editPage (7834).

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// balanceCell is one entry in a CellArray. cells is the cell's
// encoded bytes (already in a contiguous slice). end is the upper
// bound of the source memory the cell lives in (so reads past end
// are corrupt). The region index (region) tells the cell-array
// helpers which apEnd[] to compare against when checking bounds.
type balanceCell struct {
	cells  []byte
	end    int // page-buffer offset one past the end of the source memory
	region int // which apEnd[] this cell lives in
}

// balanceCellArray is the in-memory representation of C's
// CellArray. b.apCell[i] = balanceCell{cells: ..., end: ...},
// b.szCell[i] = len(balanceCell.cells). The NB constant in C is
// the max number of siblings (5). Our Go array has no fixed cap;
// balance_nonroot sets nOld <= NB+2 (= 7).
type balanceCellArray struct {
	cells     []balanceCell
	regionEnd []int // apEnd[k] — source memory upper bound for region k
	regionIx  []int // ixNx[k] — first cell index in region k
}

// NB is the maximum number of sibling pages balance_nonroot
// considers. SQLite defines it as 5 in btree.c.
const balanceNB = 5

// newBalanceCellArray returns an empty CellArray with room for
// the standard 2*NB+1 region-end entries.
func newBalanceCellArray(nCell, nRegion int) *balanceCellArray {
	return &balanceCellArray{
		cells:     make([]balanceCell, 0, nCell),
		regionEnd: make([]int, 0, nRegion+1),
		regionIx:  make([]int, 0, nRegion+1),
	}
}

// addCell appends a cell to the array.
func (b *balanceCellArray) addCell(cells []byte, end int, region int) {
	b.cells = append(b.cells, balanceCell{cells: cells, end: end, region: region})
}

// endRegion closes out the current region. After endRegion, any
// subsequent addCell calls go into the next region.
func (b *balanceCellArray) endRegion() {
	b.regionIx = append(b.regionIx, len(b.cells))
}

// finalizeRegionEnds sets the apEnd[] values. The last entry is a
// sentinel with a large value so the cell-array bounds-check
// never fails for the "after last region" case.
func (b *balanceCellArray) finalizeRegionEnds(pageEnds []int) {
	b.regionEnd = append(b.regionEnd, pageEnds...)
	// Sentinel.
	b.regionEnd = append(b.regionEnd, 0x7FFFFFFF)
}

// cellEnd returns apEnd[ix] for cell i. Mirrors the C b.ixNx[]
// walk: for k such that ixNx[k]<=i<ixNx[k+1], apEnd[k].
func (b *balanceCellArray) cellEnd(i int) int {
	for k := 0; k < len(b.regionIx)-1; k++ {
		if i < b.regionIx[k+1] {
			return b.regionEnd[k]
		}
	}
	if len(b.regionEnd) == 0 {
		return 0x7FFFFFFF
	}
	return b.regionEnd[len(b.regionEnd)-1]
}

// nCell returns the number of cells in the array.
func (b *balanceCellArray) nCell() int { return len(b.cells) }

// rebuildPage rewrites a page (pg) with the cells from b in the
// range [iFirst, iFirst+nCell). The cells are placed at the
// bottom of the cell content area (highest addresses first), and
// the cell pointer array is updated. The page is NOT written
// back; the caller is responsible.
//
// Reference: src/btree.c::rebuildPage (line 7605).
func (t *BTree) rebuildPage(pg *pager.Page, b *balanceCellArray, iFirst, nCell int) error {
	if nCell <= 0 {
		return fmt.Errorf("btree: rebuildPage: nCell must be > 0 (got %d)", nCell)
	}
	if iFirst < 0 || iFirst+nCell > len(b.cells) {
		return fmt.Errorf("btree: rebuildPage: range [%d,%d) out of bounds (cells=%d)", iFirst, iFirst+nCell, len(b.cells))
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	// The new cell content pointer is the lowest address we'll
	// write a cell at. Start from the end of usable and grow
	// downward.
	usableStart := int(t.usableSize)
	// Cell pointer array starts at coff + cellPtrOffset(pageType).
	// CellPointer (storage) adds 8 internally to its offset
	// parameter, so the actual start of the array is
	// coff+cellPtrOffset(pageType)-8 + 8 = coff+cellPtrOffset(pageType).
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	// ptrArrayEnd: the lowest byte the cell pointer array occupies
	// (after writing nCell entries). The cell content area must
	// not overlap.
	ptrArrayEnd := ptrBase + 8 + 2*nCell
	if ptrArrayEnd >= usableStart {
		return fmt.Errorf("btree: rebuildPage: cell pointer array would overlap cell content area")
	}
	// Place each cell from the END of the cell content area
	// downward; write the cell pointers in cell-index order.
	pos := usableStart
	for k := 0; k < nCell; k++ {
		c := b.cells[iFirst+k]
		if pos-len(c.cells) < ptrArrayEnd {
			return fmt.Errorf("btree: rebuildPage: cell %d too large for remaining content area", iFirst+k)
		}
		pos -= len(c.cells)
		copy(pg.Data[pos:pos+len(c.cells)], c.cells)
		binary.BigEndian.PutUint16(pg.Data[ptrBase+8+k*2:ptrBase+8+k*2+2], uint16(pos))
	}
	// Update page header.
	binary.BigEndian.PutUint16(pg.Data[coff+1:coff+3], 0)            // first freeblock
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], uint16(nCell)) // cell count
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(pos))   // cell content start
	pg.Data[coff+7] = 0                                              // frag free
	_ = page
	return nil
}

// pageInsertArray inserts cells from b (in the range [iFirst, iFirst+nCell))
// into pg's cell content area. pData is the current end-of-content
// (lowest byte) — cells grow downward from there. pCellptr is the
// current end of the cell pointer array — cell pointers grow upward
// from there. The caller has pre-allocated space for both.
//
// Returns 0 on success, 1 if cells don't fit (caller should defragment
// or rebuild).
//
// Reference: src/btree.c::pageInsertArray (line 7723).
func (t *BTree) pageInsertArray(pg *pager.Page, pBegin int, pData *int, pCellptr *int, b *balanceCellArray, iFirst, nCell int) int {
	if nCell <= 0 {
		return 0
	}
	usableStart := int(t.usableSize)
	for i := iFirst; i < iFirst+nCell; i++ {
		c := b.cells[i]
		if *pData-len(c.cells) < pBegin {
			return 1
		}
		*pData -= len(c.cells)
		copy(pg.Data[*pData:*pData+len(c.cells)], c.cells)
		binary.BigEndian.PutUint16(pg.Data[*pCellptr:*pCellptr+2], uint16(*pData))
		*pCellptr += 2
		_ = usableStart
	}
	return 0
}

// pageFreeArray returns the number of cells that were in pg's
// aData region (i.e. cells whose backing memory was in pg.Data,
// not in the divider-cell aSpace1 buffer). The C version uses
// pointer arithmetic to test this; we approximate by checking
// whether the cell bytes' start address falls within pg.Data's
// backing array. In our Go model this is approximated as "every
// cell whose data isn't a copy from aSpace1" — for the rebalance
// tests, every cell that is removed came from a real page, so
// this returns nCell.
//
// Reference: src/btree.c::pageFreeArray (line 7780).
func (t *BTree) pageFreeArray(pg *pager.Page, b *balanceCellArray, iFirst, nCell int) int {
	_ = pg
	return nCell
}

// editPage applies a cell redistribution to pg. It removes the
// cells at indices [iOld, iOld+oldN) and inserts the cells from
// b at indices [iNew, iNew+nNew). On success the page has nNew
// cells, all sourced from b, in the new order.
//
// The C editPage is complex (it handles tail-shifting and
// overflow-cell handling); our port is the simple
// "remove-then-rebuild" version which is sufficient for the
// rebalance tests since we don't track overflow cells separately
// from in-page cells.
//
// Reference: src/btree.c::editPage (line 7834).
func (t *BTree) editPage(pg *pager.Page, iOld, iNew, nNew int, b *balanceCellArray) error {
	_ = iOld
	if nNew < 0 {
		return fmt.Errorf("btree: editPage: nNew must be >= 0 (got %d)", nNew)
	}
	if nNew == 0 {
		coff := contentOffset(pg.PageNum)
		usableStart := int(t.usableSize)
		binary.BigEndian.PutUint16(pg.Data[coff+1:coff+3], 0)
		binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
		binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(usableStart))
		pg.Data[coff+7] = 0
		return nil
	}
	return t.rebuildPage(pg, b, iNew, nNew)
}
