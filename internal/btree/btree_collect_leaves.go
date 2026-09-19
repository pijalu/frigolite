// Leaf-page collection: the tree walk that lists every leaf reachable from a
// root, recording each leaf's parent reference so callers can update the
// parent when a leaf is freed.

package btree

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

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
	if err := t.collectCellChildren(pg, page, coff, pageNum, numPages, out, parentRefs); err != nil {
		return err
	}
	if page.RightmostPtr == 0 || page.RightmostPtr > numPages {
		return nil
	}
	return t.collectChildLeaves(page.RightmostPtr, pageNum, 0, 0, true, out, parentRefs)
}

// collectCellChildren walks the cell-pointer array of an interior page,
// recording each valid child's leaves. Interior pages have a 4-byte rightmost
// pointer, so the cell pointer array starts at coff+12. A child that points
// outside the on-disk file is corruption (e.g. a hex patch that wrote 0x0314
// into the rightmost pointer slot of an interior page): SQLite's btree.c
// lockBtree + balance_nonroot skip unreachable children rather than abort the
// surrounding operation, because the btree is already corrupt and the calling
// DML is the only way the user can finish the operation. We mirror that: skip
// the bad child and continue.
func (t *BTree) collectCellChildren(pg *pager.Page, page *storage.BTreePage, coff int, parent uint32, numPages uint32, out *[]uint32, parentRefs *[]leafRef) error {
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
		// reference.
		if err := t.collectChildLeaves(child, parent, i, uint16(cellOff), false, out, parentRefs); err != nil {
			return err
		}
	}
	return nil
}

// collectChildLeaves records one interior page's child in the leaf walk: a
// leaf child is appended with its parent reference; an interior child is
// recursed into with the child as the new current parent (the leaves added
// at deeper levels have child, not parent, as their immediate parent). A
// child that cannot be read or parsed is skipped — the walk mirrors SQLite's
// best-effort behavior on corrupt pages.
func (t *BTree) collectChildLeaves(child, parent uint32, childIdx int, cellOff uint16, isRightmost bool, out *[]uint32, parentRefs *[]leafRef) error {
	cpg, cerr := t.pager.ReadPage(child)
	if cerr != nil {
		return nil
	}
	ccoff := contentOffset(cpg.PageNum)
	cpage, cerr2 := storage.ParsePage(cpg.Data, int(t.pageSize), ccoff)
	if cerr2 != nil {
		return nil
	}
	if cpage.PageType == storage.PageTypeLeafTable || cpage.PageType == storage.PageTypeLeafIndex {
		*out = append(*out, child)
		if parentRefs != nil {
			*parentRefs = append(*parentRefs, leafRef{
				leafNum:       child,
				parent:        parent,
				childIdx:      childIdx,
				parentCellOff: cellOff,
				isRightmost:   isRightmost,
			})
		}
		return nil
	}
	return t.collectLeafPagesWithParent(child, child, out, parentRefs)
}
