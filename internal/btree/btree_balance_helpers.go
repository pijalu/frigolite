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
	// the end of usable size. The cell-content pointer stored in the
	// btree header is a direct offset into the page buffer (relative
	// to aData[0], not aData[hdrOffset]); this matches btree.c which
	// reads it via `get2byte(&aFrom[iFromHdr+5])` and uses it
	// directly as `&aTo[iData]`.
	iData := int(fromPage.CellContent)
	usableEnd := int(t.usableSize)
	if iData > usableEnd {
		// A cell-content pointer past the usable end is corrupt;
		// bail rather than scribble.
		return errBtreeCorrupt("copyNodeContent: cell-content %d > usable %d", iData, usableEnd)
	}
	copy(pTo.Data[iData:], pFrom.Data[iData:usableEnd])

	// Cell pointer array + header: copy the btree header + the
	// cell-pointer array from pFrom to pTo. btree.c uses
	// `memcpy(&aTo[iToHdr], &aFrom[iFromHdr], pFrom->cellOffset + 2*pFrom->nCell)`
	// where iToHdr is 100 for page 1 and 0 otherwise (the btree
	// content starts there), iFromHdr is the source's btree content
	// offset (same convention), and the length is the btree-header
	// + cell-pointer-array bytes.
	//
	// In our Go package, cellPtrOffset(pageType) returns 12 for
	// interior pages, 8 for leaf pages. The cell-pointer array
	// starts at hdrOffset + cellPtrOffset. The length of the
	// header-to-end-of-cell-pointer-array region is
	// cellPtrOffset + 2*nCell.
	cellPtrArrayStart := fromHdr + cellPtrOffset(fromPage.PageType)
	arrayLen := int(fromPage.CellCount) * 2
	headerCopyLen := cellPtrArrayStart - fromHdr // 12 for interior, 8 for leaf
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
