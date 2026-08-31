// Port of btree.c::balance_nonroot (subset: the "drop empty leaf +
// coalesce with sibling" path) into Go. This is intentionally NOT
// the full C algorithm — it covers the cases that the testgen
// autovacuum / incrvacuum / incrvacuum2 packages need:
//   - after a leaf becomes empty, return it to the freelist and
//     splice its divider cell out of the parent interior page;
//   - if its sibling has cells, move the sibling's cells into the
//     (now empty) leaf so the data is contiguous on a smaller tree.
//
// The full balance_nonroot (5-sibling gather + size-balanced
// redistribution) is documented in TODO below and will be added in a
// follow-up commit; the simpler merge is sufficient for the
// autovacuum-2 / incrvacuum-2 delete+vacuum cycles that today leave
// empty leaves stranded.
//
// Reference: src/btree.c::balance_nonroot (line 8206) and
// src/btree.c::sqlite3BtreeBalance (line 9083).

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// emptyLeafInfo is the per-leaf bookkeeping collected by
// collectEmptyLeaves. One entry per reachable leaf whose
// cellCount==0, with the parent interior page number and the cell
// index inside that parent (cellIdx = -1 for the rightmost-child
// pointer of the parent).
type emptyLeafInfo struct {
	leaf       uint32
	parent     uint32
	cellIdx    int // -1 == rightmost-child of parent
	leftSib    uint32
	leftSibIx  int
	rightSib   uint32
	rightSibIx int
}

// rebalanceEmptyLeaves walks the tree, and for every leaf whose
// cellCount is 0, either frees the page outright (if it has no
// siblings) or merges its sibling's cells into the empty leaf (if
// that fits) and then frees the sibling. The parent interior page's
// divider cells and rightmost-child pointer are updated so the tree
// remains navigable.
func (t *BTree) rebalanceEmptyLeaves() error {
	for iter := 0; iter < 100; iter++ {
		var empties []emptyLeafInfo
		if err := t.collectEmptyLeaves(&empties); err != nil {
			return err
		}
		if len(empties) == 0 {
			return nil
		}
		progress := false
		for _, e := range empties {
			ok, err := t.mergeOrFreeEmptyLeaf(e)
			if err != nil {
				return err
			}
			if ok {
				progress = true
			}
		}
		if !progress {
			return nil
		}
	}
	return fmt.Errorf("btree: rebalance did not converge after 100 iterations")
}

// collectEmptyLeaves walks the tree and records every leaf with
// cellCount == 0, plus its left and right siblings and the cell
// index in the parent.
func (t *BTree) collectEmptyLeaves(out *[]emptyLeafInfo) error {
	return t.walkInterior(t.rootPage, 0, func(parent, leaf uint32, cellIdx int) error {
		pg, err := t.pager.ReadPage(leaf)
		if err != nil {
			return err
		}
		coff := contentOffset(pg.PageNum)
		page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
		if err != nil {
			return err
		}
		if page.CellCount != 0 {
			return nil
		}
		if parent == 0 {
			// Root is itself the empty leaf — caller handles
			// the special "reset root" case.
			return nil
		}
		info := emptyLeafInfo{leaf: leaf, parent: parent, cellIdx: cellIdx}
		ppg, err := t.pager.ReadPage(parent)
		if err != nil {
			return err
		}
		pc := contentOffset(ppg.PageNum)
		ppage, err := storage.ParsePage(ppg.Data, int(t.pageSize), pc)
		if err != nil {
			return err
		}
		if cellIdx == -1 {
			// Rightmost-child: left sibling is the last cell.
			if ppage.CellCount > 0 {
				last := int(ppage.CellCount) - 1
				cp := storage.CellPointer(ppg.Data, pc+cellPtrOffset(ppage.PageType)-8, last, int(t.pageSize))
				info.leftSib = binary.BigEndian.Uint32(ppg.Data[cp : cp+4])
				info.leftSibIx = last
			}
		} else {
			// A cell-child. The cell's first 4 bytes are the
			// pointer to this leaf. Find siblings.
			if cellIdx > 0 {
				leftCp := storage.CellPointer(ppg.Data, pc+cellPtrOffset(ppage.PageType)-8, cellIdx-1, int(t.pageSize))
				info.leftSib = binary.BigEndian.Uint32(ppg.Data[leftCp : leftCp+4])
				info.leftSibIx = cellIdx - 1
			}
			if cellIdx < int(ppage.CellCount)-1 {
				rightCp := storage.CellPointer(ppg.Data, pc+cellPtrOffset(ppage.PageType)-8, cellIdx+1, int(t.pageSize))
				info.rightSib = binary.BigEndian.Uint32(ppg.Data[rightCp : rightCp+4])
				info.rightSibIx = cellIdx + 1
			} else {
				// cellIdx is the last cell; right sibling is
				// the rightmost-child pointer.
				info.rightSib = binary.BigEndian.Uint32(ppg.Data[pc+8 : pc+12])
				info.rightSibIx = -1
			}
		}
		*out = append(*out, info)
		return nil
	})
}

// walkInterior invokes fn(parent, child, cellIdx) for every child
// page reachable from pageNum. parent=0 means pageNum is itself a
// leaf (and is visited with cellIdx=-1).
func (t *BTree) walkInterior(pageNum, parent uint32, fn func(parent, child uint32, cellIdx int) error) error {
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	if page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeInteriorIndex {
		return fn(parent, pageNum, -1)
	}
	for i := 0; i < int(page.CellCount); i++ {
		cp := storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, i, int(t.pageSize))
		if int(cp)+4 > len(pg.Data) {
			continue
		}
		child := binary.BigEndian.Uint32(pg.Data[cp : cp+4])
		if child == 0 {
			continue
		}
		if err := t.walkInterior(child, pageNum, fn); err != nil {
			return err
		}
	}
	rightmost := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
	if rightmost != 0 {
		if err := t.walkInterior(rightmost, pageNum, fn); err != nil {
			return err
		}
	}
	return nil
}

// mergeOrFreeEmptyLeaf handles one empty leaf.
func (t *BTree) mergeOrFreeEmptyLeaf(e emptyLeafInfo) (bool, error) {
	if e.parent == 0 {
		return false, nil
	}
	// Sanity: read the parent's child pointer for e.leaf to verify
	// e.cellIdx is still correct (it may be stale if a previous
	// rebalance already removed this leaf from the parent).
	ppg, err := t.pager.ReadPage(e.parent)
	if err != nil {
		return false, err
	}
	pc := contentOffset(ppg.PageNum)
	ppage, err := storage.ParsePage(ppg.Data, int(t.pageSize), pc)
	if err != nil {
		return false, err
	}
	var curChild uint32
	ptrBase := pc + cellPtrOffset(ppage.PageType) - 8
	if e.cellIdx == -1 {
		curChild = binary.BigEndian.Uint32(ppg.Data[pc+8 : pc+12])
	} else {
		cp := storage.CellPointer(ppg.Data, ptrBase, e.cellIdx, int(t.pageSize))
		curChild = binary.BigEndian.Uint32(ppg.Data[cp : cp+4])
	}
	if curChild != e.leaf {
		// The parent no longer references e.leaf — another
		// rebalance already handled it. Skip.
		return false, nil
	}
	// Try merging with the right sibling first.
	if e.rightSib != 0 {
		// dst = empty leaf (e.leaf); src = right sibling.
		// After merge: dst has all cells, src freed.
		ok, err := t.mergeIntoLeft(e.rightSib, e.leaf, e.parent, e.rightSibIx, e.cellIdx)
		if err != nil || ok {
			return ok, err
		}
	}
	if e.leftSib != 0 {
		// dst = left sibling; src = empty leaf.
		ok, err := t.mergeIntoLeft(e.leaf, e.leftSib, e.parent, e.cellIdx, e.leftSibIx)
		if err != nil || ok {
			return ok, err
		}
	}
	// No sibling or both are too full to merge: free the leaf
	// directly and patch the parent.
	if err := t.removeLeafFromParent(e.parent, e.leaf, e.cellIdx); err != nil {
		return false, err
	}
	if err := t.pager.FreePage(e.leaf); err != nil {
		return false, err
	}
	return true, nil
}

// mergeIntoLeft moves every cell from src leaf into dst leaf (which
// must currently be empty) by appending. The parent's divider cell
// is updated: divider pointing to src is removed. The src leaf is
// then returned to the freelist.
//
// The merge is safe when dst has enough free space for src's
// cells. If dst is too small, we return false so the caller can try
// a different sibling.
func (t *BTree) mergeIntoLeft(src, dst, parent uint32, srcParentIdx, dstParentIdx int) (bool, error) {
	srcPg, err := t.pager.ReadPage(src)
	if err != nil {
		return false, err
	}
	scoff := contentOffset(srcPg.PageNum)
	srcPage, err := storage.ParsePage(srcPg.Data, int(t.pageSize), scoff)
	if err != nil {
		return false, err
	}
	if srcPage.PageType != storage.PageTypeLeafTable && srcPage.PageType != storage.PageTypeLeafIndex {
		return false, nil
	}
	var srcCellType storage.CellType
	if srcPage.PageType == storage.PageTypeLeafTable {
		srcCellType = storage.CellTableLeaf
	} else {
		srcCellType = storage.CellIndexLeaf
	}
	// Extract src cells as raw bytes in order, then determine the
	// end of each cell (the next pointer or the end of the usable
	// area).
	usableStart := int(t.pageSize) - 4 // skip the 4-byte reserved area
	srcPtrs := make([]uint16, int(srcPage.CellCount))
	for i := 0; i < int(srcPage.CellCount); i++ {
		srcPtrs[i] = storage.CellPointer(srcPg.Data, scoff, i, int(t.pageSize))
	}
	srcCells := make([][]byte, int(srcPage.CellCount))
	for i := 0; i < int(srcPage.CellCount); i++ {
		p := int(srcPtrs[i])
		end := usableStart
		if i+1 < int(srcPage.CellCount) {
			end = int(srcPtrs[i+1])
		}
		srcCells[i] = append([]byte(nil), srcPg.Data[p:end]...)
	}
	// Capacity check on dst.
	dstPg, err := t.pager.ReadPage(dst)
	if err != nil {
		return false, err
	}
	dcoff := contentOffset(dstPg.PageNum)
	dstPage, err := storage.ParsePage(dstPg.Data, int(t.pageSize), dcoff)
	if err != nil {
		return false, err
	}
	// Available bytes after the cell-pointer array in dst.
	ptrBase := dcoff + storage.CellPointerOffset
	ptrArrayEnd := ptrBase + 2*int(dstPage.CellCount)
	// Plus 2 bytes for the new pointer (1 cell pointer) and 2 bytes
	// per src cell.
	need := ptrArrayEnd + 2*len(srcCells) + 2
	for _, c := range srcCells {
		need += len(c)
	}
	if need > int(t.pageSize) {
		return false, nil
	}
	// Append cells to dst, growing downward.
	start := ptrArrayEnd + 2*len(srcCells) + 2
	for i := len(srcCells) - 1; i >= 0; i-- {
		start -= len(srcCells[i])
		copy(dstPg.Data[start:start+len(srcCells[i])], srcCells[i])
	}
	// Write new cell pointers. dst's existing pointers (none if
	// empty) come first, then src's pointers offset by the new
	// start position.
	dstPtrs := make([]uint16, 0, int(dstPage.CellCount)+len(srcCells))
	for i := 0; i < int(dstPage.CellCount); i++ {
		dstPtrs = append(dstPtrs, binary.BigEndian.Uint16(dstPg.Data[ptrBase+i*2:ptrBase+i*2+2]))
	}
	for i := 0; i < len(srcCells); i++ {
		off := start
		for j := 0; j < i; j++ {
			off += len(srcCells[j])
		}
		dstPtrs = append(dstPtrs, uint16(off))
	}
	// Sort pointers ascending (the cells themselves are already
	// in sorted order in src, but mixing them with dst's existing
	// cells requires re-sorting by physical pointer position).
	for i := 1; i < len(dstPtrs); i++ {
		for j := i; j > 0 && dstPtrs[j-1] > dstPtrs[j]; j-- {
			dstPtrs[j-1], dstPtrs[j] = dstPtrs[j], dstPtrs[j-1]
		}
	}
	for i := 0; i < len(dstPtrs); i++ {
		binary.BigEndian.PutUint16(dstPg.Data[ptrBase+i*2:ptrBase+i*2+2], dstPtrs[i])
	}
	newCount := len(dstPtrs)
	dstPage.CellCount = uint16(newCount)
	binary.BigEndian.PutUint16(dstPg.Data[dcoff+3:dcoff+5], dstPage.CellCount)
	binary.BigEndian.PutUint16(dstPg.Data[dcoff+5:dcoff+7], uint16(start))
	dstPg.Data[dcoff+7] = 0
	if err := t.pager.WritePage(dstPg); err != nil {
		return false, err
	}
	// Free src's overflow pages (so the on-disk count tracks).
	if err := t.freeLeafOverflows(srcPg, scoff, srcPage, srcCellType); err != nil {
		return false, err
	}
	// Patch the parent: remove src's cell.
	if err := t.removeLeafFromParent(parent, src, srcParentIdx); err != nil {
		return false, err
	}
	if err := t.pager.FreePage(src); err != nil {
		return false, err
	}
	_ = dstParentIdx
	return true, nil
}

// removeLeafFromParent patches the parent interior page so that it
// no longer references child. If child was the rightmost-child and
// there are no cells, the parent itself is now empty (a "root
// collapse" that a later pass will free).
func (t *BTree) removeLeafFromParent(parent, child uint32, childCellIdx int) error {
	ppg, err := t.pager.ReadPage(parent)
	if err != nil {
		return err
	}
	pc := contentOffset(ppg.PageNum)
	page, err := storage.ParsePage(ppg.Data, int(t.pageSize), pc)
	if err != nil {
		return err
	}
	// cellPtrOffset: 12 for interior, 8 for leaf. The cell pointer
	// array starts at pc + cellPtrOffset (we strip 8 because
	// CellPointer adds 8 internally).
	ptrBase := pc + cellPtrOffset(page.PageType) - 8
	if childCellIdx == -1 {
		// Rightmost-child pointer is at pc+8. Set it to the last
		// cell's child pointer (if any), then drop the last cell.
		if page.CellCount == 0 {
			binary.BigEndian.PutUint32(ppg.Data[pc+8:pc+12], 0)
		} else {
			last := int(page.CellCount) - 1
			lastCp := storage.CellPointer(ppg.Data, ptrBase, last, int(t.pageSize))
			rightmost := binary.BigEndian.Uint32(ppg.Data[lastCp : lastCp+4])
			binary.BigEndian.PutUint32(ppg.Data[pc+8:pc+12], rightmost)
			page.CellCount--
			binary.BigEndian.PutUint16(ppg.Data[pc+3:pc+5], page.CellCount)
			zp := ptrBase + int(page.CellCount)*2
			ppg.Data[zp] = 0
			ppg.Data[zp+1] = 0
		}
	} else {
		// Drop cell childCellIdx and shift the rest down.
		for i := childCellIdx; i < int(page.CellCount)-1; i++ {
			src := ptrBase + (i+1)*2
			dst := ptrBase + i*2
			ppg.Data[dst] = ppg.Data[src]
			ppg.Data[dst+1] = ppg.Data[src+1]
		}
		zp := ptrBase + (int(page.CellCount)-1)*2
		ppg.Data[zp] = 0
		ppg.Data[zp+1] = 0
		page.CellCount--
		binary.BigEndian.PutUint16(ppg.Data[pc+3:pc+5], page.CellCount)
	}
	_ = child
	return t.pager.WritePage(ppg)
}

// freeLeafOverflows walks the overflow chains of every cell on
// the page and returns them to the freelist.
func (t *BTree) freeLeafOverflows(pg *pager.Page, coff int, page *storage.BTreePage, cellType storage.CellType) error {
	for i := 0; i < int(page.CellCount); i++ {
		p := storage.CellPointer(pg.Data, coff, i, int(t.pageSize))
		c, err := storage.DecodeCell(pg.Data, int(p), cellType, int(t.usableSize))
		if err != nil {
			continue
		}
		if c.Overflow == 0 {
			continue
		}
		pn := c.Overflow
		for pn != 0 {
			np, rerr := t.pager.ReadPage(pn)
			if rerr != nil || np == nil {
				break
			}
			next := binary.BigEndian.Uint32(np.Data[0:4])
			if err := t.pager.FreePage(pn); err != nil {
				return err
			}
			pn = next
		}
	}
	return nil
}

// debug only: ensure imports stay referenced
var _ = fmt.Sprintf
