// Port of btree.c::copyNodeContent (line 8124). Copies the content of
// a b-tree node (pFrom) onto a freshly-allocated page (pTo) and updates
// the pointer-map entries for any child/overflow pages so they record
// pTo as their new parent. Used by balance_shallower (when the root
// collapses into its only child) and by balance_deeper (when the root
// is split and its content moves to a new child page).
//
// In SQLite this is invoked only on interior nodes (the root's content
// is moved onto a new page; or vice-versa). Leaf pages do not need
// copyNodeContent because the leaf is the source/destination of cell
// insertion and the page's bytes are written directly. We mirror
// that: copyNodeContent is interior-only.
//
// Reference: src/btree.c::copyNodeContent (line 8124).

package btree

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// copyNodeContent copies the b-tree node content from page pFrom to
// page pTo, then refreshes pTo's parsed view and rewrites the
// pointer-map entries of every child / overflow page so they point to
// pTo's new page number. Both pages must be the same kind (interior
// table or interior index). The caller's reference to pTo after this
// call must be re-parsed (it now carries pFrom's content).
//
// In SQLite, this routine is only invoked from balance_shallower and
// balance_deeper, both of which deal with the root page. Our callers
// in the port of balance_nonroot and balance_deeper will likewise use
// this for interior-page relocations.
//
// The pointer-map rewrite is delegated to setChildPtrmaps, which
// already handles both interior (cell left-children + rightmost-child)
// and leaf (cell overflow chains) cases — even though copyNodeContent
// is interior-only in practice, the helper does the right thing if
// invoked on a leaf.
func (t *BTree) copyNodeContent(pFrom, pTo *pager.Page) error {
	// Determine the btree content offset for each page: page 1 has a
	// 100-byte database header in front of the btree content; all
	// other pages start at 0.
	fromHdr := contentOffset(pFrom.PageNum)
	toHdr := contentOffset(pTo.PageNum)

	// Parse pFrom so we can read the cell-content pointer and the
	// cell-pointer-array length.
	fromPage, err := storage.ParsePage(pFrom.Data, int(t.pageSize), fromHdr)
	if err != nil {
		return err
	}

	// SQLite asserts both pages are interior (pFrom) and (pTo) and
	// that pTo is freshly allocated. We enforce interior-page only
	// here so a misuse on a leaf is caught with a clear error rather
	// than a silent corruption.
	if fromPage.PageType != storage.PageTypeInteriorTable &&
		fromPage.PageType != storage.PageTypeInteriorIndex {
		return errBtreeNotInterior("copyNodeContent", pFrom.PageNum, fromPage.PageType)
	}

	// Cell content area: copy the bytes from cell-content pointer to
	// the end of usable size. SQLite reads the cell-content pointer
	// as a 2-byte big-endian value at fromHdr+5; that value is a
	// PAGE-BUFFER absolute offset (NOT a btree-content-relative
	// offset). The cell content area extends from iData to
	// usableSize (which is `pageSize - reserved` and is a page-buffer
	// offset too).
	//
	// For page 1, the btree content lives in [fromHdr..pageSize]; the
	// cell-content pointer is some iData >= fromHdr, and the cell
	// content area is [iData..usableSize]. For non-page-1, the btree
	// content lives in [0..usableSize].
	usableSize := int(t.usableSize)
	iData := int(fromPage.CellContent)
	if iData < 0 {
		return errBtreeCorrupt("copyNodeContent: cell-content %d < 0", iData)
	}
	if iData > usableSize {
		// A cell-content pointer past the usable end is corrupt;
		// bail rather than scribble.
		return errBtreeCorrupt("copyNodeContent: cell-content %d > usable %d", iData, usableSize)
	}
	length := usableSize - iData
	if length < 0 {
		length = 0
	}
	// iData is page-buffer absolute; no fromHdr/toHdr offset.
	if iData+length > len(pFrom.Data) {
		length = len(pFrom.Data) - iData
		if length < 0 {
			length = 0
		}
	}
	if iData+length > len(pTo.Data) {
		length = len(pTo.Data) - iData
		if length < 0 {
			length = 0
		}
	}
	copy(pTo.Data[iData:iData+length], pFrom.Data[iData:iData+length])

	// Cell pointer array + header: copy from pFrom's hdrOffset to
	// cellOffset + 2*nCell. SQLite's "cellOffset" is the byte offset
	// of the first cell pointer; for interior pages this is
	// hdrOffset+12 (after page-type, first-free, cell-count,
	// cell-content, frag-free, right-child). We compute it as
	// fromHdr + cellPtrOffset(pFrom->aData[0]) (CellPointer in our
	// Go storage package adds 8 to its offset argument, so the raw
	// array starts at fromHdr + 12 for interior pages).
	//
	// In btree.c the memcpy length is `pFrom->cellOffset + 2*pFrom->nCell`.
	// For interior pages, cellOffset = hdrOffset + 12 (rightmost-child is
	// at hdrOffset+8, then the cell-pointer array starts at hdrOffset+12).
	// For leaf pages, cellOffset = hdrOffset + 8.
	cellPtrArrayStart := fromHdr + cellPtrOffset(fromPage.PageType)
	arrayLen := int(fromPage.CellCount) * 2
	headerCopyLen := cellPtrArrayStart - fromHdr
	totalCopyLen := headerCopyLen + arrayLen
	if totalCopyLen <= 0 || fromHdr+totalCopyLen > len(pFrom.Data) {
		return errBtreeCorrupt("copyNodeContent: header+cellArray length %d out of bounds", totalCopyLen)
	}
	copy(pTo.Data[toHdr:toHdr+totalCopyLen], pFrom.Data[fromHdr:fromHdr+totalCopyLen])

	// The rightmost-child pointer lives at fromHdr+8..fromHdr+12
	// (interior pages) — already covered by the header copy above.
	// The cell-content pointer lives at fromHdr+5..fromHdr+7 — also
	// covered.

	// Sanity: the cell-content pointer on pTo should match pFrom's.
	toCellContent := binary.BigEndian.Uint16(pTo.Data[toHdr+5 : toHdr+7])
	if int(toCellContent) != iData {
		// If the cell-content pointer didn't copy (e.g. if the
		// interior's cell-content lives in the btree-content area
		// but pTo has a different header layout), force it.
		binary.BigEndian.PutUint16(pTo.Data[toHdr+5:toHdr+7], uint16(iData))
	}

	// SQLite's btreeComputeFreeSpace is no-op in our model: we don't
	// track per-page free bytes beyond the cell-content pointer, and
	// pTo is fresh so any "free space" leftover from allocation is
	// implicit. The cell-content pointer is the authoritative
	// free-space boundary for our writes.

	// Auto-vacuum: update the pointer-map entries for every child /
	// overflow page that pTo now contains pointers to. The new
	// parent is pTo.PageNum.
	if t.pager.AutoVacuum() {
		if err := t.setChildPtrmaps(pTo, pTo.PageNum); err != nil {
			return err
		}
	}
	return nil
}
