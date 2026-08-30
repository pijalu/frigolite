// Package btree: auto-vacuum and incremental-vacuum page-swap machinery
// (P8.INCRVACUUM phase 3). Ported from btree.c::relocatePage (~line 6530),
// sqlite3BtreeIncrVacuum (~line 6780), and incrVacuumStep (~line 6700).
//
// The page-swap step moves the content of a page to a free page near the
// front of the file. This is what makes auto-vacuum actually shrink the
// file: instead of leaving holes in the freelist, we relocate the
// highest-numbered page to a lower-numbered free page, then truncate the
// file. Done enough times, the file ends up with no free pages and no
// trailing garbage.
package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// RelocatePage moves the content of `from` to the page `to`. The `to`
// page must be a free page (allocated via pager.AllocatePageLE). The
// parent of `from` is located via the pointer-map (P8.INCRVACUUM phase 2);
// the parent cell or rightmost-pointer that referenced `from` is
// updated to reference `to` instead. The pointer-map entry for `to` is
// written to record its new parent.
//
// Reference: btree.c::relocatePage (line ~6530).
func (t *BTree) RelocatePage(to, from uint32) error {
	if to == from {
		return fmt.Errorf("btree: RelocatePage: to == from == %d", to)
	}
	// Read the pointer-map entry for `from` to find its parent. The
	// parent type tells us how to update the parent's reference:
	//   - PTRMAP_BTREE_NODE / PTRMAP_HAS_ROWID: parent is an interior
	//     b-tree page; the cell that points to `from` has `from` as
	//     its left-child (first 4 bytes of the cell data).
	//   - PTRMAP_OVERFLOW: not used in btree-only relocation; overflow
	//     pages have a single chain, no parent in the b-tree.
	//   - PTRMAP_FREELIST: not used; freelist pages are already free.
	parentType, parentPgno, err := t.pager.ReadPtrmap(from)
	if err != nil {
		return fmt.Errorf("btree: RelocatePage: read ptrmap for %d: %w", from, err)
	}
	if parentType == 0 {
		// Uninitialized entry — `from` has no recorded parent. This
		// can happen if a page is on the freelist (PTRMAP_FREELIST not
		// set yet) or if the pointer-map was never written for `from`.
		// In either case we can't safely update the parent. Bail.
		return fmt.Errorf("btree: RelocatePage: no parent recorded for page %d (uninitialized ptrmap entry)", from)
	}
	// Read both pages and copy `from` → `to`.
	fromPg, err := t.pager.ReadPage(from)
	if err != nil {
		return fmt.Errorf("btree: RelocatePage: read page %d: %w", from, err)
	}
	toPg, err := t.pager.ReadPage(to)
	if err != nil {
		return fmt.Errorf("btree: RelocatePage: read page %d: %w", to, err)
	}
	// Copy the content (whole page is pageSize bytes; the cell-content
	// pointer, cell-count, etc. are all in the first ~12 bytes).
	copy(toPg.Data, fromPg.Data)
	// Mark `to` as dirty so it gets written back.
	pager.MarkPageDirtyForVacuum(t.pager, to)
	// Update the parent's reference from `from` to `to`.
	if err := t.updateParentChildPtr(parentPgno, from, to, parentType); err != nil {
		return fmt.Errorf("btree: RelocatePage: update parent %d: %w", parentPgno, err)
	}
	// Write the pointer-map entry for `to` (same parent as `from` had).
	if err := t.pager.WritePtrmap(to, parentType, parentPgno); err != nil {
		return fmt.Errorf("btree: RelocatePage: write ptrmap for %d: %w", to, err)
	}
	// Free `from`. The freed page is now the trunk of the freelist
	// (or one of the leaves; SQLite's btree.c freePage2 handles the
	// chain details). For phase 3 we just FreePage.
	if err := t.pager.FreePage(from); err != nil {
		return fmt.Errorf("btree: RelocatePage: free %d: %w", from, err)
	}
	return nil
}

// updateParentChildPtr updates the parent page's child pointer from
// `oldChild` to `newChild`. The parent is identified by the pointer-map
// type: for interior b-tree pages, `oldChild` appears either as a cell
// left-child (the first 4 bytes of an interior cell) or as the
// rightmost-pointer. We scan the parent for the matching pointer and
// replace it.
func (t *BTree) updateParentChildPtr(parentPgno, oldChild, newChild uint32, parentType byte) error {
	parentPg, err := t.pager.ReadPage(parentPgno)
	if err != nil {
		return err
	}
	coff := contentOffset(parentPg.PageNum)
	page, err := storage.ParsePage(parentPg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	if page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeInteriorIndex {
		// The parent pointer-map says it's a b-tree node, but the page
		// doesn't parse as one. Bail rather than corrupt the page.
		return fmt.Errorf("btree: updateParentChildPtr: page %d is not an interior page (type 0x%02x)", parentPgno, page.PageType)
	}
	// Walk the cell-pointer array; for each cell, check if its
	// left-child (first 4 bytes of the cell data) matches `oldChild`.
	ptrBase := coff + storage.CellPointerOffset
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(binary.BigEndian.Uint16(parentPg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		if cellOff+4 > len(parentPg.Data) {
			continue
		}
		leftChild := binary.BigEndian.Uint32(parentPg.Data[cellOff : cellOff+4])
		if leftChild == oldChild {
			binary.BigEndian.PutUint32(parentPg.Data[cellOff:cellOff+4], newChild)
			pager.MarkPageDirtyForVacuum(t.pager, parentPgno)
			return nil
		}
	}
	// Not found in cells: check the rightmost-pointer (interior pages
	// have a 4-byte rightmost-pointer at offset coff+8).
	rmp := binary.BigEndian.Uint32(parentPg.Data[coff+8 : coff+12])
	if rmp == oldChild {
		binary.BigEndian.PutUint32(parentPg.Data[coff+8:coff+12], newChild)
		pager.MarkPageDirtyForVacuum(t.pager, parentPgno)
		return nil
	}
	return fmt.Errorf("btree: updateParentChildPtr: parent %d does not reference child %d", parentPgno, oldChild)
}

// IncrVacuumStep performs up to n steps of the incremental-vacuum
// algorithm (P8.INCRVACUUM phase 3). Each step:
//  1. If the last page of the file is on the freelist, just truncate
//     the file (decrement numPages by 1).
//  2. Otherwise, the last page is in use. Find the lowest free page
//     (pager.AllocatePageLE) and relocate the last page to that free
//     page (RelocatePage). Truncate the file.
//
// Returns the number of steps actually performed. Stops early if the
// freelist is empty (no more free pages to swap) or if `n` is exhausted.
//
// Reference: btree.c::sqlite3BtreeIncrVacuum (line ~6780).
func (t *BTree) IncrVacuumStep(n int) (int, error) {
	steps := 0
	for i := 0; i < n; i++ {
		lastPg := t.pager.NumPages()
		if lastPg <= 1 {
			// Page 1 is the schema page; can't truncate below it.
			return steps, nil
		}
		// Check if `lastPg` is on the freelist. We don't have a direct
		// IsOnFreelist query; instead, check if `lastPg` is in
		// p.freePages. This is the fast path for the common case
		// (Delete freed pages near the end of the file).
		if pager.IsPageOnFreelist(t.pager, lastPg) {
			// Truncate by 1 page.
			if err := t.pager.Truncate(lastPg - 1); err != nil {
				return steps, fmt.Errorf("btree: IncrVacuumStep: truncate to %d: %w", lastPg-1, err)
			}
			steps++
			continue
		}
		// The last page is in use. Try to allocate a free page.
		freePg, err := t.pager.AllocatePageLE()
		if err != nil {
			// No free page available. We're done.
			return steps, nil
		}
		// Relocate lastPg → freePg.
		if err := t.RelocatePage(freePg.PageNum, lastPg); err != nil {
			return steps, fmt.Errorf("btree: IncrVacuumStep: relocate %d -> %d: %w", lastPg, freePg.PageNum, err)
		}
		// Truncate the file to remove the (now-relocated) last page.
		if err := t.pager.Truncate(lastPg - 1); err != nil {
			return steps, fmt.Errorf("btree: IncrVacuumStep: truncate to %d: %w", lastPg-1, err)
		}
		steps++
	}
	return steps, nil
}
