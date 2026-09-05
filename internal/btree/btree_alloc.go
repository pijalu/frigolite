// P8.INCRVACUUM phase 5/15: helper wrappers around pager.AllocatePage
// that also write the pointer-map entry for the new page. Every
// btree-node and overflow-page allocation must do this; without
// it, btree.c::relocatePage can't find a page's parent during
// incremental vacuum and bails with "no parent recorded".
//
// Phase15: all allocation helpers are guarded by pager.AutoVacuum()
// (btree.c ptrmapPut is a no-op when !autoVacuum — src/btree.c:1151),
// and freePageWithPtrmap centralizes the PTRMAP_FREEPAGE write that
// btree.c::freePage performs (src/btree.c:6841).
//
// Reference: src/btree.c::allocateBtreePage (the BTALLOC_*
// branches set the parent type via setChildPtrmaps or
// ptrmapPutOvflPtr for each newly allocated page).

package btree

import (
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// ptrmapEnabled reports whether pointer-map entries must be maintained.
// Mirrors btree.c's compile-time/auto-vacuum guard around every ptrmapPut.
func (t *BTree) ptrmapEnabled() bool {
	return t.pager.AutoVacuum()
}

// allocBtreeNode allocates a btree-node page and writes a ptrmap
// entry (PtrmapBtree, parent=parentPgno). The returned page's
// caller is responsible for initializing its content (page type,
// cell count, etc.). Pass parentPgno=0 to skip the ptrmap write
// (used for the schema root page, which is its own root).
func (t *BTree) allocBtreeNode(parentPgno uint32) (*pager.Page, error) {
	pg := t.allocPage()
	if parentPgno != 0 && t.ptrmapEnabled() {
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
	pg := t.allocPage()
	if t.ptrmapEnabled() {
		if err := t.pager.WritePtrmap(pg.PageNum, storage.PtrmapRootpage, 0); err != nil {
			return nil, err
		}
	}
	return pg, nil
}

// allocOverflow allocates an overflow page and writes a ptrmap
// entry (PtrmapOverflow1, parent=parentPgno). Used by the btree
// when a cell payload doesn't fit on the btree page. The first
// overflow's parent is the btree page; subsequent overflows in the
// same chain set their parent to the previous overflow (use
// allocOverflowNext for that case).
func (t *BTree) allocOverflow(parentPgno uint32) (*pager.Page, error) {
	pg := t.allocPage()
	if t.ptrmapEnabled() {
		if err := t.pager.WritePtrmap(pg.PageNum, storage.PtrmapOverflow1, parentPgno); err != nil {
			return nil, err
		}
	}
	return pg, nil
}

// allocOverflowNext allocates a continuation overflow page whose ptrmap
// entry is PtrmapOverflow2 with parent=prevPgno (the previous overflow
// page in the chain). Mirrors btree.c fillInCell's second and later
// ptrmapPutOvfl calls.
func (t *BTree) allocOverflowNext(prevPgno uint32) (*pager.Page, error) {
	pg := t.allocPage()
	if t.ptrmapEnabled() {
		if err := t.pager.WritePtrmap(pg.PageNum, storage.PtrmapOverflow2, prevPgno); err != nil {
			return nil, err
		}
	}
	return pg, nil
}

// freePageWithPtrmap returns a page to the freelist and records
// PtrmapFreelist for it (btree.c freePage → ptrmapPut(PTRMAP_FREEPAGE)
// at src/btree.c:6841), so incremental vacuum can distinguish freed
// pages from live btree/overflow pages and relocatePage never chases a
// stale parent. Guarded like every other ptrmap write.
func (t *BTree) freePageWithPtrmap(pgno uint32) error {
	if err := t.pager.FreePage(pgno); err != nil {
		return err
	}
	if t.ptrmapEnabled() {
		return t.pager.WritePtrmap(pgno, storage.PtrmapFreelist, 0)
	}
	return nil
}

// reparentPageOverflowChains rewrites the PtrmapOverflow1 entry of every
// cell's overflow chain on the leaf page pgno to point at pgno (btree.c
// ptrmapPutOvflPtr semantics: a cell that moved to a different page takes
// its chain with it, src/btree.c:1582-1596). Used by relocateRootSplit,
// whose segment rotation moves cell content between pages wholesale.
func (t *BTree) reparentPageOverflowChains(pgno uint32) error {
	if !t.ptrmapEnabled() {
		return nil
	}
	pg, err := t.pager.ReadPage(pgno)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	// Interior pages have no overflow chains (the page-level Overflow field is
	// only meaningful on leaf cells). Skip them: decoding interior cells as
	// leaf cells would read garbage into c.Overflow and corrupt the ptrmap.
	var cellType storage.CellType = storage.CellTableLeaf
	switch page.PageType {
	case storage.PageTypeLeafTable:
		// cellType already CellTableLeaf
	case storage.PageTypeLeafIndex:
		cellType = storage.CellIndexLeaf
	default:
		return nil
	}
	for i := 0; i < int(page.CellCount); i++ {
		p := storage.CellPointer(pg.Data, coff, i, int(t.pageSize))
		c, err := storage.DecodeCell(pg.Data, int(p), cellType, int(t.usableSize))
		if err != nil || c.Overflow == 0 {
			continue
		}
		if err := t.pager.WritePtrmap(c.Overflow, storage.PtrmapOverflow1, pgno); err != nil {
			return err
		}
	}
	return nil
}
