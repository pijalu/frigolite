// Shared interior-page helpers for the coherent index balance window: the
// pending-divider fold into the anchor's entry array and the page writer
// used by the recursive cascade. The old two-page interior spill was
// superseded by the coherent balance_nonroot-style path in
// btree_index_balance_interior.go; see portplan/HANDOFF_2026-11-27_S66_INDEX_BALANCE.md.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// foldPendingDivider folds the leg's pending divider into the anchor's
// entries: a cell-anchored divider re-keys the anchor child's cell to the
// split's median and appends the carrier sibling entry carrying the anchor
// child's ORIGINAL divider (the upper bound moves with the last new page,
// applyChildSplits' Kold); a rightmost-anchored divider (C's end-insert)
// turns the anchor child into a regular cell carrying the split's median
// and hands the rightmost pointer to the split's new page. Returns
// ok=false when the anchor child is neither a cell nor the rightmost
// pointer.
func (t *BTree) foldPendingDivider(anchorEntries []interiorEntry, page *storage.BTreePage, childPageNum uint32, cs leafSplitResult) ([]interiorEntry, uint32, bool) {
	rightmost := page.RightmostPtr
	for i := range anchorEntries {
		if anchorEntries[i].leftChild != childPageNum {
			continue
		}
		carrier := anchorEntries[i]
		anchorEntries[i].key = cs.medianKey
		anchorEntries[i].payload = cs.medianPayload
		return []interiorEntry{{leftChild: cs.pageNum, key: carrier.key, payload: carrier.payload}}, rightmost, true
	}
	if page.RightmostPtr == childPageNum {
		return []interiorEntry{{leftChild: childPageNum, key: cs.medianKey, payload: cs.medianPayload}}, cs.pageNum, true
	}
	return nil, rightmost, false
}

// interiorHeaderSize is an interior page's fixed overhead: the 8-byte
// b-tree header plus the 4-byte rightmost pointer.
const interiorHeaderSize = 12

// reparentInteriorChildren re-points the ptrmap entries of the given
// entries' children (and the extra child, when non-zero) at owner.
func (t *BTree) reparentInteriorChildren(entries []interiorEntry, owner uint32, extra uint32) error {
	if !t.ptrmapEnabled() {
		return nil
	}
	for i := range entries {
		if err := t.pager.WritePtrmap(entries[i].leftChild, storage.PtrmapBtree, owner); err != nil {
			return err
		}
	}
	if extra != 0 {
		return t.pager.WritePtrmap(extra, storage.PtrmapBtree, owner)
	}
	return nil
}

// writeInteriorCells writes an interior page's cells (a half-open slice of
// the merged entries) packing from the usable end downward, and sets the
// header's cell count, content end and rightmost pointer.
func (t *BTree) writeInteriorCells(pg *pager.Page, co, ptroff int, entries []interiorEntry, rightmost uint32) error {
	end := int(t.usableSize)
	count := 0
	for i := range entries {
		cellData, eerr := t.encodeDividerCell(entries[i].leftChild, entries[i].splitResult(t.isTable), pg.PageNum)
		if eerr != nil {
			return eerr
		}
		ptrEnd := co + ptroff + (count+1)*2
		start := end - len(cellData)
		if start < ptrEnd {
			return fmt.Errorf("btree: interior spill: page overflow")
		}
		copy(pg.Data[start:], cellData)
		binary.BigEndian.PutUint16(pg.Data[co+ptroff+count*2:], uint16(start))
		end = start
		count++
	}
	binary.BigEndian.PutUint16(pg.Data[co+3:co+5], uint16(count))
	binary.BigEndian.PutUint16(pg.Data[co+5:co+7], uint16(end))
	binary.BigEndian.PutUint32(pg.Data[co+8:co+12], rightmost)
	return nil
}

// writeSpillInteriorPage rewrites one window page with its entries: the
// write-intent barrier, the content clear, the cell write, the moved
// entries' child re-parenting (extraChild is the page's new rightmost
// pointer) and the persist.
func (t *BTree) writeSpillInteriorPage(pg *pager.Page, co, ptroff int, entries []interiorEntry, extraChild uint32) error {
	t.pager.PrepareWrite(pg)
	for i := co + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}
	if err := t.writeInteriorCells(pg, co, ptroff, entries, extraChild); err != nil {
		return err
	}
	if err := t.reparentInteriorChildren(entries, pg.PageNum, extraChild); err != nil {
		return err
	}
	return t.pager.WritePage(pg)
}
