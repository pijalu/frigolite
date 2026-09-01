// P8.INCRVACUUM phase 5: helper wrappers around pager.AllocatePage
// that also write the pointer-map entry for the new page. Every
// btree-node and overflow-page allocation must do this; without
// it, btree.c::relocatePage can't find a page's parent during
// incremental vacuum and bails with "no parent recorded".
//
// Reference: src/btree.c::allocateBtreePage (the BTALLOC_*
// branches set the parent type via setChildPtrmaps or
// ptrmapPutOvflPtr for each newly allocated page).

package btree

import (
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// allocBtreeNode allocates a btree-node page and writes a ptrmap
// entry (PtrmapBtree, parent=parentPgno). The returned page's
// caller is responsible for initializing its content (page type,
// cell count, etc.). Pass parentPgno=0 to skip the ptrmap write
// (used for the schema root page, which is its own root).
func (t *BTree) allocBtreeNode(parentPgno uint32) (*pager.Page, error) {
	pg := t.pager.AllocatePage()
	if parentPgno != 0 {
		if err := t.pager.WritePtrmap(pg.PageNum, storage.PtrmapBtree, parentPgno); err != nil {
			return nil, err
		}
	}
	return pg, nil
}

// allocRootpage allocates a btree root page and writes a ptrmap
// entry (PtrmapRootpage, parent=0). Root pages have no parent in
// the btree; the ptrmap entry exists so relocatePage can identify
// them as roots (and refuse to relocate them).
func (t *BTree) allocRootpage() (*pager.Page, error) {
	pg := t.pager.AllocatePage()
	if err := t.pager.WritePtrmap(pg.PageNum, storage.PtrmapRootpage, 0); err != nil {
		return nil, err
	}
	return pg, nil
}

// allocOverflow allocates an overflow page and writes a ptrmap
// entry (PtrmapOverflow1, parent=parentPgno). Used by the btree
// when a cell payload doesn't fit on the btree page. The first
// overflow's parent is the btree page; subsequent overflows in the
// same chain should set their parent to the previous overflow (use
// allocOverflowNext for that case).
func (t *BTree) allocOverflow(parentPgno uint32) (*pager.Page, error) {
	pg := t.pager.AllocatePage()
	if err := t.pager.WritePtrmap(pg.PageNum, storage.PtrmapOverflow1, parentPgno); err != nil {
		return nil, err
	}
	return pg, nil
}
