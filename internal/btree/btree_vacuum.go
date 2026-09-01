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
// Returns relocated=true when the page content was actually moved to
// `to`; relocated=false when the page was treated as an orphan (no
// parent found and the tree-walk fallback also failed) and `to`'s
// content was left untouched — in that case the caller (IncrVacuumStep)
// must put the wasted `to` allocation back on the freelist so the
// chain count stays accurate. The `from` page is NOT added to the
// on-disk freelist chain; the caller (IncrVacuumStep / AutoVacuumCommit)
// is responsible for truncating the file past `from` to reclaim the
// slot. This mirrors btree.c::relocatePage which uses PagerMovepage
// (a low-level move, not copy+free) and lets the file truncation in
// sqlite3BtreeCommitPhaseOne reclaim the source.
//
// Reference: btree.c::relocatePage (line ~6530).
func (t *BTree) RelocatePage(to, from uint32) (relocated bool, err error) {
	if to == from {
		return false, fmt.Errorf("btree: RelocatePage: to == from == %d", to)
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
		return false, fmt.Errorf("btree: RelocatePage: read ptrmap for %d: %w", from, err)
	}
	if parentType == 0 {
		// Uninitialized ptrmap entry — fall back to a tree walk from
		// the root to find `from`'s parent. The walk is O(n) but
		// happens at most once per vacuum step; the next iteration
		// will have the ptrmap populated.
		if pp, pt, perr := t.findParentByWalk(from); perr == nil {
			parentPgno = pp
			parentType = pt
		} else {
			// Page is not in the tree (orphaned — the btree's parent
			// was already dropped/freed, but `from` itself wasn't
			// marked free). The file truncation reclaims the slot.
			// The wasted `to` allocation must be returned to the
			// freelist (signal relocated=false).
			//
			// P8.INCRVACUUM fix: do NOT FreePage(from) here. The
			// orphan branch is hit when the ptrmap for `from` is
			// uninitialized and the tree-walk fallback also failed
			// (e.g. the btree allocated the page without writing a
			// ptrmap entry, so the autovacuum has no way to find
			// the parent). Adding `from` to the freelist would
			// inflate p.freePages with a page that the next
			// AllocatePageLE call would then return, causing a
			// cascade of overwrites at the same target. The file
			// truncation reclaims the slot; we just leave `from`
			// to the truncation.
			return false, nil
		}
	}
	// Read both pages and copy `from` → `to`.
	fromPg, err := t.pager.ReadPage(from)
	if err != nil {
		return false, fmt.Errorf("btree: RelocatePage: read page %d: %w", from, err)
	}
	toPg, err := t.pager.ReadPage(to)
	if err != nil {
		return false, fmt.Errorf("btree: RelocatePage: read page %d: %w", to, err)
	}
	// Copy the content (whole page is pageSize bytes; the cell-content
	// pointer, cell-count, etc. are all in the first ~12 bytes).
	copy(toPg.Data, fromPg.Data)
	// Mark `to` as dirty so it gets written back.
	pager.MarkPageDirtyForVacuum(t.pager, to)
	// Update the parent's reference from `from` to `to`.
	if err := t.updateParentChildPtr(parentPgno, from, to, parentType); err != nil {
		return false, fmt.Errorf("btree: RelocatePage: update parent %d: %w", parentPgno, err)
	}
	// Write the pointer-map entry for `to` (same parent as `from` had).
	if err := t.pager.WritePtrmap(to, parentType, parentPgno); err != nil {
		return false, fmt.Errorf("btree: RelocatePage: write ptrmap for %d: %w", to, err)
	}
	// P8.INCRVACUUM phase 5: update the ptrmap entries for every
	// child of the moved page. The children's "parent" is now `to`,
	// not `from`. Without this, the next vacuum step that tries to
	// move a child of `to` will look up `to` in the ptrmap, fail
	// (entry says parent=from), and the engine falls back to a
	// tree-walk that may pick the wrong ancestor if the parent's
	// own child pointer was already updated. (Port of
	// btree.c::setChildPtrmaps, ~line 6490.)
	if err := t.setChildPtrmaps(toPg, to); err != nil {
		return false, fmt.Errorf("btree: RelocatePage: setChildPtrmaps for %d: %w", to, err)
	}
	// P8.INCRVACUUM fix: do NOT call FreePage(from) here. The
	// caller (IncrVacuumStep / AutoVacuumCommit) will truncate the
	// file past `from`; the source slot is reclaimed by the
	// truncation, not by the freelist. Adding `from` to the
	// freelist would:
	//   1. Bloat the on-disk chain with entries for pages that
	//      are about to be truncated (pruneFreelistChain would
	//      remove them later, but at the cost of extra work).
	//   2. Cause AllocatePageLE to return the same target page on
	//      subsequent vac steps (because FreePage(from) re-adds
	//      a high page that is then removed by Truncate, but the
	//      chain topology causes the next pop to land on a page
	//      the btree has already overwritten).
	//   3. Most critically: create a cascade where the same
	//      target page is reused for multiple source pages,
	//      causing the btree to lose content (each successive
	//      overwrite of `to` destroys the previous source's
	//      btree state, leaving parent cells pointing to `to`
	//      with mismatched content).
	//
	// SQLite's btree.c::relocatePage does NOT call freePage2
	// either. The source page is "moved" via PagerMovepage
	// (a low-level cache/file move, not a copy+free), and the
	// file truncation that follows (in
	// sqlite3BtreeCommitPhaseOne) reclaims the source slot. The
	// btree's parent was already updated above to point to `to`.
	// For ROLLBACK, the target page's journal BEFORE image
	// restores the free-page content, and the parent's journal
	// BEFORE image restores the original `from` reference — so
	// the rollback is consistent.
	return true, nil
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
	// Interior pages have the cell pointer array at coff+12, which
	// translates to a CellPointer offset of coff+4 (CellPointer
	// adds 8 internally).
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(parentPg.Data, ptrBase, i, int(t.pageSize)))
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
	return fmt.Errorf("btree: updateParentChildPtr: parent %d does not reference child %d (cells=%d, rmp=%d)", parentPgno, oldChild, page.CellCount, rmp)
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
		// Pointer-map pages: the C incrVacuumStep checks
		// PTRMAP_ISPAGE early and just decrements the file size
		// (no relocation — the ptrmap page has no child pointer
		// to update). For bCommit=1, the page's ptrmap entry
		// (eType==PTRMAP_FREEPAGE) means it's already on the
		// freelist; the C code does nothing for that case in
		// bCommit mode. We just truncate the file past it.
		if storage.IsPtrmapPageNo(lastPg, t.pageSize) {
			if err := t.pager.Truncate(lastPg - 1); err != nil {
				return steps, fmt.Errorf("btree: IncrVacuumStep: truncate past ptrmap %d: %w", lastPg, err)
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
		// Relocate lastPg → freePg. RelocatePage returns relocated=false
		// when the page was treated as an orphan (no parent found and
		// the tree-walk fallback also failed) and the wasted `to`
		// allocation must be put back on the freelist.
		// In the normal case, the page content was moved to freePg and
		// lastPg is now on the freelist via the RelocatePage's FreePage;
		// the caller's Truncate will remove lastPg from the file
		// entirely, and pruneFreelistChain will clean the dangling
		// chain entry.
		relocated, err := t.RelocatePage(freePg.PageNum, lastPg)
		if err != nil {
			return steps, fmt.Errorf("btree: IncrVacuumStep: relocate %d -> %d: %w", lastPg, freePg.PageNum, err)
		}
		if !relocated {
			// Orphan branch: the page's parent could not be located
			// (ptrmap uninitialized AND tree-walk failed — typical
			// when the btree allocated a page without writing a
			// ptrmap entry, leaving the autovacuum with no way to
			// find the parent). Do NOT truncate the file: the btree
			// has a (stale) parent pointer to `lastPg` that the
			// relocator was supposed to update. Truncating would
			// leave the btree referencing a non-existent page,
			// which integrity_check reports as "Page N: never used"
			// or "database disk image is malformed". The wasted
			// `to` allocation also leaks (stays allocated but
			// never reused); the cost is bounded by nVac per
			// autovacuum pass.
			//
			// P8.INCRVACUUM note: the SQLite C version never hits
			// this branch because it always writes the ptrmap at
			// allocation time. The proper long-term fix is to wire
			// WritePtrmap into every t.pager.AllocatePage() call
			// site in the btree. Until that's done, this
			// conservative branch keeps the btree intact at the
			// cost of leaving some pages un-relocated. The
			// AutoVacuumCommit caller will notice no progress
			// (NumPages unchanged) and break out of its loop.
			return steps, nil
		}
		// Truncate the file to remove the (now-relocated) last page.
		if err := t.pager.Truncate(lastPg - 1); err != nil {
			return steps, fmt.Errorf("btree: IncrVacuumStep: truncate to %d: %w", lastPg-1, err)
		}
		steps++
	}
	return steps, nil
}

// findParentByWalk scans the btree starting from the root looking
// for `target` as a child. Returns the parent page number and the
// ptrmap type (always PtrmapBtreeNode; we don't distinguish
// interior-table from interior-index from leaf). Used as a
// fallback when the pointer-map entry for `target` is uninitialized
// (which happens for pages allocated before ptrmap writes were
// wired into the AllocatePage call sites).
func (t *BTree) findParentByWalk(target uint32) (uint32, byte, error) {
	if t.rootPage == target {
		return 0, 0, fmt.Errorf("page %d is the btree root", target)
	}
	var queue []struct {
		parent uint32
		child  uint32
	}
	if err := t.walkChildren(t.rootPage, &queue); err != nil {
		return 0, 0, err
	}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e.child == target {
			return e.parent, storage.PtrmapBtreeNode, nil
		}
		// Don't descend into a child that the pager has freed —
		// its content is now junk. The ptrmap (or walk) will say
		// "this page is on the freelist" if so, but the simpler
		// check is the in-memory freePages set maintained by
		// pager.FreePage.
		if pager.IsPageOnFreelist(t.pager, e.child) {
			continue
		}
		if err := t.walkChildren(e.child, &queue); err != nil {
			return 0, 0, err
		}
	}
	return 0, 0, fmt.Errorf("page %d not found in btree", target)
}

// walkChildren appends (parent, child) edges for every child of
// `parentPgno` to `out`. If `parentPgno` is a leaf, nothing is
// appended.
func (t *BTree) walkChildren(parentPgno uint32, out *[]struct {
	parent uint32
	child  uint32
}) error {
	if pager.IsPageOnFreelist(t.pager, parentPgno) {
		return nil
	}
	pg, err := t.pager.ReadPage(parentPgno)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		// A page with stale header bytes (e.g. an empty leaf
		// whose CellContent was never reset) is treated as a
		// leaf (no children). The rebalance code in
		// maybeRebalanceAfterDelete will free it.
		return nil
	}
	if page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeInteriorIndex {
		return nil
	}
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	numPages := t.pager.NumPages()
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(t.pageSize)))
		if cellOff+4 > len(pg.Data) {
			continue
		}
		child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		if child == 0 || child > numPages {
			continue
		}
		*out = append(*out, struct {
			parent uint32
			child  uint32
		}{parent: parentPgno, child: child})
	}
	rmp := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
	if rmp != 0 && rmp <= numPages {
		*out = append(*out, struct {
			parent uint32
			child  uint32
		}{parent: parentPgno, child: rmp})
	}
	return nil
}

// setChildPtrmaps rewrites the pointer-map entries for every child
// and every overflow page of `pgNo`, setting parent=pgNo. Called by
// RelocatePage after a page has been moved to a new slot so that
// future vacuum steps can find the moved page as their parent.
//
// For interior pages: the first 4 bytes of each cell is the
// left-child page number; the rightmost-child is the 4-byte value
// at pc+8. For leaf pages: each cell's overflow page (if any) is a
// chain — write ptrmap for the first overflow page in the chain.
// Interior pages don't have overflow chains (the divider key is
// inlined).
//
// Reference: src/btree.c::setChildPtrmaps (~line 6490).
func (t *BTree) setChildPtrmaps(pg *pager.Page, pgNo uint32) error {
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	if page.PageType == storage.PageTypeInteriorTable || page.PageType == storage.PageTypeInteriorIndex {
		ptrBase := coff + cellPtrOffset(page.PageType) - 8
		for i := 0; i < int(page.CellCount); i++ {
			cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(t.pageSize)))
			if cellOff+4 > len(pg.Data) {
				continue
			}
			child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			if child != 0 {
				if err := t.pager.WritePtrmap(child, storage.PtrmapBtreeNode, pgNo); err != nil {
					return err
				}
			}
		}
		rmp := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
		if rmp != 0 {
			if err := t.pager.WritePtrmap(rmp, storage.PtrmapBtreeNode, pgNo); err != nil {
				return err
			}
		}
		return nil
	}
	// Leaf page: walk each cell's overflow chain.
	var cellType storage.CellType
	if page.PageType == storage.PageTypeLeafTable {
		cellType = storage.CellTableLeaf
	} else if page.PageType == storage.PageTypeLeafIndex {
		cellType = storage.CellIndexLeaf
	} else {
		return nil
	}
	for i := 0; i < int(page.CellCount); i++ {
		ptrBase := coff + cellPtrOffset(page.PageType) - 8
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(t.usableSize)))
		if cellOff+4 > len(pg.Data) {
			continue
		}
		c, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.usableSize))
		if err != nil || c.Overflow == 0 {
			continue
		}
		if err := t.pager.WritePtrmap(c.Overflow, storage.PtrmapOverflow, pgNo); err != nil {
			return err
		}
	}
	return nil
}
