// Port of btree.c::balance_shallower (src/btree.c:8918-8943) and the
// dead-child cascade of balance()'s upward walk: interior pages that lost
// their children are collapsed, spliced, or absorbed so the tree never
// keeps a 0-cell interior page (unreadable by SQLite) or a reference to a
// freed page.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// cascadeChildless collapses interior pages that lost their last child.
// After a balance frees an emptied subtree, an interior page may be left
// with 0 cell-children — SQLite's balance_deeper/balance_shallower collapse
// these upward until a live ancestor remains (src/btree.c:8115-8205). pnum
// is walked upward:
//   - a 0-cell interior with a DEAD (zero or freed) rightmost child has its
//     reference dropped from its own parent, is freed, and the check
//     repeats one level up;
//   - the ROOT with a live single child is absorbed into the root page
//     itself (balance_shallower, btree.c:8918-8943), decreasing the tree
//     height by one;
//   - the root with no live children is rewritten as an empty leaf.
//
// A non-root 0-cell interior with a LIVE single child is left alone: its
// callers keep the parent legal instead (see balanceNonroot's coversParent
// and all-empty branches), because splicing such a husk out would lift its
// subtree a level and break the all-leaves-same-depth invariant.
func (t *BTree) cascadeChildless(pnum uint32) error {
	for {
		next, done, err := t.cascadeChildlessStep(pnum)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		pnum = next
	}
}

// cascadeChildlessStep performs one collapse step for pnum (a 0-cell interior
// page): done=true means the cascade ends at pnum (leaf, still-populated,
// live-single-child, or collapsed root); done=false means pnum was unlinked
// and freed and the walk continues at next (its parent).
func (t *BTree) cascadeChildlessStep(pnum uint32) (next uint32, done bool, err error) {
	pg, err := t.pager.ReadPage(pnum)
	if err != nil {
		return 0, true, err
	}
	coff := contentOffset(pnum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return 0, true, err
	}
	if page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeInteriorIndex {
		return 0, true, nil // leaf: nothing to collapse
	}
	rmp := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
	if page.CellCount != 0 {
		return 0, true, nil // the page still has dividers
	}
	if rmp != 0 && !pager.IsPageOnFreelist(t.pager, rmp) {
		return 0, true, t.cascadeLiveSingleChild(pg, coff, pnum, rmp)
	}
	// No live children.
	if pnum == t.rootPage {
		return 0, true, t.rewriteRootAsEmptyLeaf(pg, coff, page.PageType)
	}
	// Non-root: unlink from the parent, free, and cascade upward.
	parentPgno, err := t.unlinkChildlessFromParent(pnum)
	if err != nil {
		return 0, true, err
	}
	if parentPgno == 0 {
		return 0, true, nil // no parent found; nothing to unlink
	}
	return parentPgno, false, nil
}

// unlinkChildlessFromParent removes a childless non-root interior page from
// its parent, frees the page, and returns the parent's page number for the
// upward cascade. Returns (0, nil) when no parent can be found.
func (t *BTree) unlinkChildlessFromParent(pnum uint32) (uint32, error) {
	parentPgno, _, err := t.findParentByWalk(pnum)
	if err != nil {
		return 0, nil // no parent found; nothing to unlink
	}
	parentPg, err := t.pager.ReadPage(parentPgno)
	if err != nil {
		return 0, err
	}
	pco := contentOffset(parentPg.PageNum)
	parentPage, err := storage.ParsePage(parentPg.Data, int(t.pageSize), pco)
	if err != nil {
		return 0, err
	}
	idx, err := t.findLeafIndexInParent(parentPg, pnum)
	if err != nil {
		return 0, nil
	}
	if idx >= 0 {
		if err := t.removeInteriorCellRange(parentPg, parentPage, idx, 1); err != nil {
			return 0, err
		}
	} else if parentPage.CellCount > 0 {
		// pnum was the parent's RIGHTMOST child. A parent with n cells
		// holds n+1 children; dropping the rightmost pointer alone would
		// leave n cells with n children (an invalid node the cursor walk
		// reads as "descend to page 0"). balance_shallower instead pulls
		// the LAST divider up into the rightmost slot: the last cell's
		// left child becomes the rightmost pointer and the cell count
		// drops by one, keeping children == cells+1.
		last := int(parentPage.CellCount) - 1
		ptroff := cellPtrOffset(parentPage.PageType)
		lastOff := int(binary.BigEndian.Uint16(parentPg.Data[pco+ptroff+last*2 : pco+ptroff+last*2+2]))
		lastChild := binary.BigEndian.Uint32(parentPg.Data[lastOff : lastOff+4])
		if err := t.removeInteriorCellRange(parentPg, parentPage, last, 1); err != nil {
			return 0, err
		}
		binary.BigEndian.PutUint32(parentPg.Data[pco+8:pco+12], lastChild)
	} else {
		binary.BigEndian.PutUint32(parentPg.Data[pco+8:pco+12], 0)
	}
	if err := t.pager.WritePage(parentPg); err != nil {
		return 0, err
	}
	if err := t.freePageWithPtrmap(pnum); err != nil {
		return 0, err
	}
	return parentPgno, nil
}

// cascadeLiveSingleChild handles a 0-cell interior page whose rightmost child
// is LIVE. Splicing it out (repointing the parent's reference at the child)
// would lift the child's subtree one level, breaking the all-leaves-same-
// depth invariant ("Child page depth differs" in sqlite3 integrity_check); C
// never faces this because its balance() walk rebalances interior pages among
// their siblings before they can empty. The balance paths that feed
// cascadeChildless keep the parent legal instead (see the coversParent and
// all-empty branches in balanceNonroot), so the cascade simply ends here —
// except at the ROOT, whose single live child is absorbed (balance_shallower).
// C skips the absorption when the child's content cannot fit the root page
// (pParent->hdrOffset<=apNew[0]->nFree, src/btree.c:8918): the root stays an
// interior page over its single live child. Propagating errRootAbsorbNoFit as
// a hard error would fail a perfectly legal DELETE.
func (t *BTree) cascadeLiveSingleChild(pg *pager.Page, coff int, pnum, rmp uint32) error {
	if pnum != t.rootPage {
		return nil
	}
	if err := t.absorbSingleChildRoot(pg, coff, rmp); err != nil && err != errRootAbsorbNoFit {
		return err
	}
	return nil
}

// rewriteRootAsEmptyLeaf converts an interior root whose subtree is fully
// dead into an empty leaf of the matching kind (zeroPage header: content
// area at the usable end, no freeblock, no fragmentation).
func (t *BTree) rewriteRootAsEmptyLeaf(rootPg *pager.Page, coff int, pageType byte) error {
	if pageType == storage.PageTypeInteriorTable {
		rootPg.Data[coff] = storage.PageTypeLeafTable
	} else {
		rootPg.Data[coff] = storage.PageTypeLeafIndex
	}
	binary.BigEndian.PutUint16(rootPg.Data[coff+1:coff+3], 0)
	binary.BigEndian.PutUint16(rootPg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(rootPg.Data[coff+5:coff+7], uint16(t.usableSize))
	rootPg.Data[coff+7] = 0
	binary.BigEndian.PutUint32(rootPg.Data[coff+8:coff+12], 0)
	pager.MarkPageDirtyForVacuum(t.pager, rootPg.PageNum)
	return t.pager.WritePage(rootPg)
}

// errRootAbsorbNoFit reports that the root's single child cannot be
// absorbed because its cells exceed the root page's usable area (page 1
// loses 100 bytes to the file header). The caller must leave the tree
// untouched — C skips the absorption under the same condition
// (pParent->hdrOffset<=apNew[0]->nFree, src/btree.c:8918).
var errRootAbsorbNoFit = fmt.Errorf("btree: child content does not fit the root page")

// absorbSingleChildRoot copies the content of the root's single live child
// into the root page and frees the child — btree.c:8918-8943
// (balance_shallower): "The root page of the b-tree now contains no cells.
// The only sibling page is the right-child of the parent. Copy the contents
// of the child page into the parent, decreasing the overall height of the
// b-tree structure by one." The root keeps its page number (schema entries
// and cursors reference it); the child's cells are repacked at the root's
// content offset (page 1 carries the 100-byte file header) and any overflow
// chains the copied cells own are re-parented to the root.
func (t *BTree) absorbSingleChildRoot(rootPg *pager.Page, rootCoff int, childPgno uint32) error {
	childPg, err := t.pager.ReadPage(childPgno)
	if err != nil {
		return err
	}
	childCoff := contentOffset(childPgno)
	childPage, err := storage.ParsePage(childPg.Data, int(t.pageSize), childCoff)
	if err != nil {
		return err
	}
	isInterior := childPage.PageType == storage.PageTypeInteriorTable || childPage.PageType == storage.PageTypeInteriorIndex

	// Size every child cell BEFORE touching the root: if the content does
	// not fit the root's smaller usable area (page 1's file header), the
	// caller must leave the tree untouched — C skips the absorption under
	// the same condition rather than truncating the tree.
	sizes := make([]int, int(childPage.CellCount))
	total := 0
	limit := rootCoff + cellPtrOffset(childPage.PageType) + int(childPage.CellCount)*2 + 2
	// The child's cell-pointer array starts AFTER ITS OWN header: interior
	// pages carry a 12-byte header (rightmost pointer at bytes 8-11), leaf
	// pages an 8-byte header, and storage.CellPointer's base argument must
	// be arrayStart-8 (the findLeafIndexInParent convention). Reading the
	// array at the leaf offset for an INTERIOR child served the
	// rightmost-pointer bytes as cell 0's pointer and shifted every
	// subsequent pointer one slot, so garbage divider cells (leftChild
	// 0x05000000 in tkt-6bfb98dfc0) were copied into the root and the next
	// insert descended into page 0 ("database disk image is malformed").
	childPtrBase := childCoff + cellPtrOffset(childPage.PageType) - 8
	for i := 0; i < int(childPage.CellCount); i++ {
		src := int(storage.CellPointer(childPg.Data, childPtrBase, i, int(t.pageSize)))
		sz, err := t.absorbChildCellSize(childPg, src, isInterior, childPage.PageType)
		if err != nil {
			return err
		}
		sizes[i] = sz
		total += sz
	}
	if total > int(t.usableSize)-limit {
		return errRootAbsorbNoFit
	}

	// Clear the root's b-tree area (keep the file header on page 1).
	for i := rootCoff + 1; i < int(t.pageSize); i++ {
		rootPg.Data[i] = 0
	}
	rootPg.Data[rootCoff] = childPage.PageType

	// Repack the child's cells into the root, from the usable end down.
	// Cell bytes are copied RAW (b-tree cells are position-independent);
	// the on-page size is derived from the child's OWN cell kind — table
	// leaf, index leaf, or interior. Decoding every child as a table-leaf
	// cell mis-sizes index cells (the av1_idx root absorbs an index leaf
	// in autovacuum-1) and writes garbled cells into the root.
	end := t.repackAbsorbedChildCells(rootPg, childPg, rootCoff, childPtrBase, childPage, sizes)
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+1:rootCoff+3], 0)
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+3:rootCoff+5], childPage.CellCount)
	if childPage.CellCount > 0 {
		binary.BigEndian.PutUint16(rootPg.Data[rootCoff+5:rootCoff+7], uint16(end))
	} else {
		binary.BigEndian.PutUint16(rootPg.Data[rootCoff+5:rootCoff+7], uint16(t.usableSize))
	}
	rootPg.Data[rootCoff+7] = 0
	// Interior roots carry the rightmost pointer at bytes 8-12. A leaf
	// child must NOT touch bytes 8-11: on a leaf page that range is the
	// FIRST CELL-POINTER SLOTS (the previous unconditional zero-write
	// clobbered cell 0's pointer, leaving the root leaf with a cell at
	// offset 0 — "Offset 0 out of range" in sqlite3 integrity_check —
	// and the absorbed cell's overflow chain orphaned, "Page N: never
	// used").
	if isInterior {
		binary.BigEndian.PutUint32(rootPg.Data[rootCoff+8:rootCoff+12], childPage.RightmostPtr)
	}
	pager.MarkPageDirtyForVacuum(t.pager, rootPg.PageNum)
	if err := t.pager.WritePage(rootPg); err != nil {
		return err
	}
	// Cells copied onto the root take their overflow chains with them:
	// re-parent each chain's first page to the root (ptrmapPutOvflPtr).
	if err := t.reparentPageOverflowChains(rootPg.PageNum); err != nil {
		return err
	}
	return t.freePageWithPtrmap(childPgno)
}

// absorbChildCellSize computes the on-page size of one child cell at src for
// the root-absorption repack. Cell bytes are copied RAW (b-tree cells are
// position-independent), so the size derives from the child's OWN cell kind —
// table leaf, index leaf, or interior. Decoding every child as a table-leaf
// cell mis-sizes index cells (the av1_idx root absorbs an index leaf in
// autovacuum-1) and writes garbled cells into the root.
func (t *BTree) absorbChildCellSize(childPg *pager.Page, src int, isInterior bool, pageType byte) (int, error) {
	switch {
	case isInterior && !t.isTable:
		// Index divider: child pointer + payload-length varint + LOCAL
		// payload + 4-byte overflow head when the separator spills. The
		// chain itself is position-independent and moves with the cell.
		c, err := storage.DecodeCell(childPg.Data, src, storage.CellIndexInterior, int(t.usableSize))
		if err != nil {
			return 0, err
		}
		_, n := util.GetVarint(childPg.Data[src+4:])
		sz := 4 + n + c.LocalLen
		if c.Overflow != 0 {
			sz += 4
		}
		return sz, nil
	case isInterior:
		_, n := util.GetVarint(childPg.Data[src+4:])
		return 4 + n, nil
	case pageType == storage.PageTypeLeafIndex:
		plen, n1 := util.GetVarint(childPg.Data[src:])
		local := storage.LocalPayloadSize(int(plen), int(t.usableSize), storage.CellIndexLeaf)
		sz := n1 + local
		if local < int(plen) {
			sz += 4
		}
		return sz, nil
	default:
		return storage.TableLeafCellSizeAt(childPg.Data, src, int(t.usableSize))
	}
}

// repackAbsorbedChildCells copies the child's cells (already sized) into the
// root page from the usable end down, writing each cell pointer, and returns
// the new lowest cell offset.
func (t *BTree) repackAbsorbedChildCells(rootPg, childPg *pager.Page, rootCoff, childPtrBase int, childPage *storage.BTreePage, sizes []int) int {
	end := int(t.usableSize)
	for i := int(childPage.CellCount) - 1; i >= 0; i-- {
		src := int(storage.CellPointer(childPg.Data, childPtrBase, i, int(t.pageSize)))
		sz := sizes[i]
		start := end - sz
		copy(rootPg.Data[start:start+sz], childPg.Data[src:src+sz])
		binary.BigEndian.PutUint16(rootPg.Data[rootCoff+cellPtrOffset(childPage.PageType)+i*2:], uint16(start))
		end = start
	}
	return end
}
