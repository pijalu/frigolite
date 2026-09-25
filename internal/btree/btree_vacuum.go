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
	"errors"
	"fmt"
	"os"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// errRelocateRoot marks the incrVacuumStep position where the tail page
// is a genuine root page (btree.c reports SQLITE_CORRUPT there,
// src/btree.c:4030). The drain stops without truncating the root; the
// file simply stays larger.
var errRelocateRoot = errors.New("btree: cannot relocate a root page")

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
	parentType, parentPgno, orphan, err := t.relocateParentRef(from)
	if err != nil {
		return false, err
	}
	if orphan {
		// Orphaned page: the btree's parent was already dropped/freed,
		// but `from` itself wasn't marked free. The file truncation
		// reclaims the slot; the wasted `to` allocation must be
		// returned to the freelist (signal relocated=false).
		return false, nil
	}
	// 1. Read `from`'s data into a temporary buffer.
	// 2. Read `to`'s page (loads it into cache; its current content
	//    is the free-page content the pager saved when the page
	//    was added to the freelist).
	// 3. Update the parent's child pointer from `from` → `to` in
	//    memory (journaled via MarkPageDirtyForVacuum).
	// 4. If the parent update fails (parent not an interior btree
	//    page, child pointer not found, etc.), return error
	//    WITHOUT touching `toPg.Data` or `fromPg.Data`. The
	//    caller's `IncrVacuumStep` will see `relocated=false` and
	//    skip the file truncation, leaving the btree and file
	//    consistent: parent still points to `from`, `to` still
	//    holds its free-page content (recyclable by the next
	//    AllocatePageLE call), `from` still holds its btree
	//    content.
	// 5. If the parent update succeeds, copy `from` → `to` in
	//    `toPg.Data`, mark `to` dirty (journaled), write the
	//    ptrmap entry for `to` (same parent as `from` had), and
	//    update the child ptrmaps so future vacuum steps can find
	//    the moved page as their parent.
	//
	// Why parent-first, not copy-first: a failed parent update
	// after a copy corrupts `to` with `from`'s content while the
	// btree still references `from` and the caller will truncate
	// past `from`. The result: `to` becomes a phantom copy of a
	// truncated page, and the btree has a stale reference to a
	// non-existent page. integrity_check reports this as
	// "Page N: never used" or "database disk image is
	// malformed" (the freelist chain then references pages with
	// stale btree content). The earlier "copy first, parent
	// second" order (commits up to 2ad222cc) hit this every time
	// the ptrmap was uninitialized for `from` (the btree's
	// allocation sites bypass WritePtrmap, so most pages have
	// ptrmap type 0; findParentByWalk only traverses the schema
	// btree and can't reach user-btree pages; the orphan branch
	// returns relocated=false, BUT the copy has already
	// destroyed `to`'s free-page content). The fix: do the
	// parent update first. If it fails, the buffer is untouched
	// and the caller can safely decide not to truncate.
	fromPg, err := t.pager.ReadPage(from)
	if err != nil {
		return false, fmt.Errorf("btree: RelocatePage: read page %d: %w", from, err)
	}
	if _, err := t.pager.ReadPage(to); err != nil {
		return false, fmt.Errorf("btree: RelocatePage: read page %d: %w", to, err)
	}
	// Update the parent's reference from `from` to `to`. Done
	// BEFORE the copy so a failure leaves both pages' content
	// intact.
	if err := t.updateParentChildPtr(parentPgno, from, to, parentType); err != nil {
		// Parent update failed. The parent does not reference
		// `from` (e.g. the btree's parent is wrong, or the page
		// is genuinely an orphan). The caller (IncrVacuumStep)
		// will see `relocated=false` and skip the truncation;
		// the btree remains consistent. We must NOT corrupt
		// `to` (it's still a free page in cache; the journal
		// BEFORE image has its original free-page content), and
		// we must NOT corrupt `from` (it's still a live btree
		// page). Both buffers are unchanged: the copy hasn't
		// happened yet, no MarkPageDirtyForVacuum was called
		// for either page in this function.
		return false, fmt.Errorf("btree: RelocatePage: update parent %d: %w", parentPgno, err)
	}
	// Copy `from` → `to`. After the parent update succeeded, the
	// btree now references `to` (not `from`); the copy makes
	// `to`'s content match what the btree expects. The caller's
	// Truncate will then remove `from` from the file. On
	// ROLLBACK, the parent's journal BEFORE image restores the
	// `from` reference, and `to`'s journal BEFORE image (saved
	// by FreePage when the page was added to the freelist)
	// restores the free-page content.
	toPg, err := t.pager.ReadPage(to)
	if err != nil {
		return false, fmt.Errorf("btree: RelocatePage: re-read page %d: %w", to, err)
	}
	copy(toPg.Data, fromPg.Data)
	// Mark `to` as dirty so the copy is written back on commit
	// (and journaled for ROLLBACK).
	pager.MarkPageDirtyForVacuum(t.pager, to)
	// Write the pointer-map entry for `to` (same parent as `from` had).
	if err := t.pager.WritePtrmap(to, parentType, parentPgno); err != nil {
		return false, fmt.Errorf("btree: RelocatePage: write ptrmap for %d: %w", to, err)
	}
	// P8.INCRVACUUM phase 5: update the ptrmap entries for every child of
	// the moved page (see reparentMovedPageChildren).
	if err := t.reparentMovedPageChildren(toPg, to, parentType); err != nil {
		return false, err
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

// relocateParentRef resolves the parent reference of `from` for relocation:
// the pointer-map entry, with fallbacks for uninitialized (type 0) and
// stale PTRMAP_ROOTPAGE entries — both re-resolved by a tree walk. Returns
// orphan=true when the page is not in the tree (its parent was already
// dropped/freed but the page itself wasn't marked free; the file truncation
// reclaims the slot, so the caller signals relocated=false and returns the
// wasted `to` allocation to the freelist).
func (t *BTree) relocateParentRef(from uint32) (parentType byte, parentPgno uint32, orphan bool, err error) {
	parentType, parentPgno, err = t.pager.ReadPtrmap(from)
	if err != nil {
		return 0, 0, false, fmt.Errorf("btree: RelocatePage: read ptrmap for %d: %w", from, err)
	}

	if parentType == 0 {
		// Uninitialized ptrmap entry — fall back to a tree walk from
		// the root to find `from`'s parent. The walk is O(n) but
		// happens at most once per vacuum step; the next iteration
		// will have the ptrmap populated.
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
		pp, pt, perr := t.findParentByWalk(from)
		if perr != nil {
			return 0, 0, true, nil
		}
		parentPgno = pp
		parentType = pt
	}
	if parentType == storage.PtrmapRootpage {
		// P8.INCRVACUUM BUG D (btree.c incrVacuumStep, src/btree.c:4030):
		// a PTRMAP_ROOTPAGE entry is only trustworthy if the page is
		// still a root of some schema object. For a genuine root the
		// vacuum must never relocate or truncate it — SQLite reports
		// CORRUPT in this position; the engine stops the drain step
		// (the caller leaves the file above the root). For a STALE
		// entry (the page changed role since the entry was written)
		// the real parent is resolved by tree walk and the relocation
		// proceeds with it.
		if err := t.checkRelocatableRoot(from); err != nil {
			return 0, 0, false, err
		}
		pp, pt, perr := t.findParentByWalk(from)
		if perr != nil {
			return 0, 0, true, nil
		}
		parentPgno = pp
		parentType = pt
	}
	return parentType, parentPgno, false, nil
}

// checkRelocatableRoot reports errRelocateRoot when `from` is a genuine
// root of some schema object; a stale PTRMAP_ROOTPAGE entry falls through
// to the tree walk.
func (t *BTree) checkRelocatableRoot(from uint32) error {
	if roots, rerr := t.collectSchemaRoots(); rerr == nil {
		for _, r := range roots {
			if r == from {
				return fmt.Errorf("btree: RelocatePage: %w: page %d is a root", errRelocateRoot, from)
			}
		}
	}
	return nil
}

// reparentMovedPageChildren updates the ptrmap entries of the moved page's
// children — their "parent" is now `to`, not `from`. Without this, the next
// vacuum step that tries to move a child of `to` will look up `to` in the
// ptrmap, fail (entry says parent=from), and the engine falls back to a
// tree-walk that may pick the wrong ancestor if the parent's own child
// pointer was already updated. (Port of btree.c::relocatePage's child-fixup
// step, ~line 6605: b-tree pages run setChildPtrmaps; overflow pages only
// re-parent their next-in-chain pointer, the first 4 bytes.)
func (t *BTree) reparentMovedPageChildren(toPg *pager.Page, to uint32, parentType byte) error {
	switch parentType {
	case storage.PtrmapBtree, storage.PtrmapRootpage:
		if err := t.setChildPtrmaps(toPg, to); err != nil {
			return fmt.Errorf("btree: RelocatePage: setChildPtrmaps for %d: %w", to, err)
		}
	case storage.PtrmapOverflow1, storage.PtrmapOverflow2:
		if len(toPg.Data) >= 4 {
			if next := binary.BigEndian.Uint32(toPg.Data[0:4]); next != 0 {
				if err := t.pager.WritePtrmap(next, storage.PtrmapOverflow2, to); err != nil {
					return fmt.Errorf("btree: RelocatePage: ptrmap next-ovfl %d: %w", next, err)
				}
			}
		}
	}
	return nil
}

// updateParentChildPtr updates the parent page's child pointer from
// `oldChild` to `newChild`. Port of btree.c::modifyPagePointer (~line
// 3877). The parent is identified by the pointer-map type:
//
//   - PTRMAP_OVERFLOW2: the pointer is always the first 4 bytes of the
//     parent overflow page (the chain's next pointer).
//   - PTRMAP_OVERFLOW1: the parent is a leaf b-tree page; the overflowing
//     cell's last 4 on-page bytes hold the chain head.
//   - PTRMAP_BTREE: `oldChild` appears either as a cell left-child (the
//     first 4 bytes of an interior cell) or as the rightmost-pointer.
//   - PTRMAP_ROOTPAGE: a root page has no page-level parent (the schema
//     owns the root pointer) — btree.c relocatePage skips the
//     modifyPagePointer call entirely for this type.
func (t *BTree) updateParentChildPtr(parentPgno, oldChild, newChild uint32, parentType byte) error {
	switch parentType {
	case storage.PtrmapOverflow2:
		return t.updateOvfl2ParentPtr(parentPgno, oldChild, newChild)
	case storage.PtrmapOverflow1:
		parentPg, err := t.pager.ReadPage(parentPgno)
		if err != nil {
			return err
		}
		return t.updateOvfl1ParentPtr(parentPg, parentPgno, oldChild, newChild)
	case storage.PtrmapRootpage:
		// The schema owns the root pointer; no page-level fixup.
		return nil
	}
	return t.updateBtreeParentPtr(parentPgno, oldChild, newChild)
}

// updateOvfl2ParentPtr rewrites the chain's next pointer on the parent
// overflow page (btree.c modifyPagePointer, PTRMAP_OVERFLOW2 branch: the
// pointer is always the first 4 bytes of the parent overflow page).
func (t *BTree) updateOvfl2ParentPtr(parentPgno, oldChild, newChild uint32) error {
	parentPg, err := t.pager.ReadPage(parentPgno)
	if err != nil {
		return err
	}
	if len(parentPg.Data) < 8 {
		return fmt.Errorf("btree: updateParentChildPtr: overflow parent %d too small", parentPgno)
	}
	if got := binary.BigEndian.Uint32(parentPg.Data[0:4]); got != oldChild {
		return fmt.Errorf("btree: updateParentChildPtr: overflow page %d chains to %d, not %d", parentPgno, got, oldChild)
	}
	binary.BigEndian.PutUint32(parentPg.Data[0:4], newChild)
	pager.MarkPageDirtyForVacuum(t.pager, parentPgno)
	return nil
}

// updateBtreeParentPtr repoints `oldChild` → `newChild` inside a parent
// interior page (btree.c modifyPagePointer, PTRMAP_BTREE branch).
func (t *BTree) updateBtreeParentPtr(parentPgno, oldChild, newChild uint32) error {
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

// ovfl1CellTypeForPage maps a page type to the cell layout used when
// locating a cell's overflow-chain head pointer. Index interior dividers
// carry the separator key with their own overflow chain, so a relocated
// chain head may be owned by an interior page's divider cell.
func ovfl1CellTypeForPage(pgno uint32, pageType byte) (storage.CellType, error) {
	switch pageType {
	case storage.PageTypeLeafTable:
		return storage.CellTableLeaf, nil
	case storage.PageTypeLeafIndex:
		return storage.CellIndexLeaf, nil
	case storage.PageTypeInteriorIndex:
		return storage.CellIndexInterior, nil
	default:
		return 0, fmt.Errorf("btree: updateOvfl1ParentPtr: parent %d is not a leaf (type 0x%02x)", pgno, pageType)
	}
}

// ovfl1CellSize returns the on-page size of the cell at cellOff: the
// payload-length varint (+ rowid varint for table leaves) + local payload +
// 4-byte overflow pointer. Index interior divider cells additionally lead
// with the 4-byte left-child pointer.
func ovfl1CellSize(data []byte, cellOff int, cellType storage.CellType, cell storage.Cell) int {
	_, n1 := util.GetVarint(data[cellOff:])
	sz := n1 + cell.LocalLen + 4
	if cellType == storage.CellTableLeaf {
		_, n2 := util.GetVarint(data[cellOff+n1:])
		sz += n2
	}
	if cellType == storage.CellIndexInterior {
		// The payload-length varint starts AFTER the 4-byte child
		// pointer in an interior cell.
		_, nChild := util.GetVarint(data[cellOff+4:])
		sz = 4 + nChild + cell.LocalLen + 4
	}
	return sz
}

// rewriteOvfl1PtrAt rewrites the overflow-chain head pointer of the cell at
// pointer index idx when it equals oldChild. Reports whether the chain head
// was found (and rewritten).
func (t *BTree) rewriteOvfl1PtrAt(pg *pager.Page, ptrBase, idx int, cellType storage.CellType, pgno, oldChild, newChild uint32) bool {
	cellOff := int(storage.CellPointer(pg.Data, ptrBase, idx, int(t.pageSize)))
	if cellOff+4 > len(pg.Data) {
		return false
	}
	c, cerr := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.usableSize))
	if cerr != nil || c.Overflow == 0 {
		return false
	}
	ovflOff := cellOff + ovfl1CellSize(pg.Data, cellOff, cellType, *c) - 4
	if ovflOff < 0 || ovflOff+4 > len(pg.Data) {
		return false
	}
	if binary.BigEndian.Uint32(pg.Data[ovflOff:ovflOff+4]) != oldChild {
		return false
	}
	binary.BigEndian.PutUint32(pg.Data[ovflOff:ovflOff+4], newChild)
	pager.MarkPageDirtyForVacuum(t.pager, pgno)
	return true
}

// updateOvfl1ParentPtr rewrites the overflow-chain head pointer stored in
// a leaf page's cell (btree.c modifyPagePointer, PTRMAP_OVERFLOW1 branch:
// the cell's last 4 on-page bytes hold the chain head when the payload
// overflows). Scans every cell; the one whose chain head is `oldChild`
// is rewritten to `newChild`.
func (t *BTree) updateOvfl1ParentPtr(parentPg *pager.Page, parentPgno, oldChild, newChild uint32) error {
	coff := contentOffset(parentPg.PageNum)
	page, err := storage.ParsePage(parentPg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	cellType, err := ovfl1CellTypeForPage(parentPgno, page.PageType)
	if err != nil {
		return err
	}
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	for i := 0; i < int(page.CellCount); i++ {
		if t.rewriteOvfl1PtrAt(parentPg, ptrBase, i, cellType, parentPgno, oldChild, newChild) {
			return nil
		}
	}
	return fmt.Errorf("btree: updateOvfl1ParentPtr: leaf %d does not chain to overflow %d", parentPgno, oldChild)
}

// IncrVacuumStep performs up to n steps of the incremental-vacuum
// algorithm (port of btree.c::incrVacuumStep, src/btree.c:4010-4104).
// Each step:
//  1. If the last page of the file is on the freelist, just truncate
//     the file (decrement numPages by 1).
//  2. Otherwise, the last page is in use. Find a free page and relocate
//     the last page to it, then truncate the file.
//
// bCommit and nFin mirror the C parameters (btree.c:4010): bCommit=true
// only for a FULL auto-vacuum drain (nVac==nFree). In that case, the
// caller will keep calling IncrVacuumStep until the file reaches nFin;
// trailing FREE pages are left in the chain as intentional garbage
// (btree.c:4022-4024, btree.c:4249-4252 zero the chain header at the
// commit end). nFin is the target file size; the bCommit=1 relocate
// branch uses a do-while loop that pops ANY free page (not just <= nFin)
// and discards any pop that lands above nFin — exactly the
// btree.c:4072-4085 do-while.
//
// bCommit=false for PRAGMA incremental_vacuum (sqlite3BtreeIncrVacuum)
// and for partial auto-vacuum drains (callback capped nVac < nFree) —
// each step pops the trailing FREE page via TakePageFromFreelist
// (BTALLOC_EXACT path) and uses AllocatePageLE(nFin) for the live
// relocation target.
//
// Returns the number of steps actually performed. Stops early if the
// freelist is empty (no more free pages to swap) or if `n` is exhausted.
// iLastPg is the page to inspect (the "tail" in C's incrVacuumStep).
// For bCommit=0 (PRAGMA incremental_vacuum) the caller passes the
// current pager.NumPages() — the step itself decrements it after
// the truncate. For bCommit=1 (FULL auto-vacuum drain) the caller
// drives the loop and passes the next iFree value to inspect,
// matching btree.c autoVacuumCommit's `for(iFree=nOrig; iFree>nFin; iFree--)`.
// Passing 0 means "use the current NumPages" (legacy bCommit=0 path).
// vacuumSkipPages decrements n past any pointer-map or pending-byte
// pages, mirroring the C btree.c:4098-4102 do-while at the end of
// incrVacuumStep (bCommit==0). The caller passes the post-work
// iLastPg-1 value; the helper walks it down to the highest
// non-skip page so the next call to IncrVacuumStep (or the
// drainAutoVacuum loop iteration) starts on a real btree page.
//
// Reference: btree.c incrVacuumStep lines 4096-4102:
//
//	if( bCommit==0 ){
//	  do {
//	    iLastPg--;
//	  }while( iLastPg==PENDING_BYTE_PAGE(pBt) || PTRMAP_ISPAGE(pBt, iLastPg) );
//	  pBt->bDoTruncate = 1;
//	  pBt->nPage = iLastPg;
//	}
func (t *BTree) vacuumSkipPages(n uint32) uint32 {
	for n > 1 {
		if n == t.pager.PendingBytePage() {
			n--
			continue
		}
		if storage.IsPtrmapPageNo(n, t.pageSize) {
			n--
			continue
		}
		break
	}
	return n
}

func (t *BTree) IncrVacuumStep(n int, bCommit bool, nFin uint32, iLastPg uint32) (int, error) {
	steps := 0
	// P8.INCRVACUUM safety net (REMOVED in S6): the historical "clamp
	// numPages to file size" guard misfires in healthy code paths. The
	// pre-S6 autovacuum traced a `iFreePg 8 > dbSize 7` error back to
	// this safety net clamping numPages mid-iteration: AutoVacuumCommit
	// had truncated the file (nOrig=8 → 7), the next IncrVacuumStep
	// entered with numPages=8 (the in-memory value used by the
	// pre-truncate copy), the safety net saw fileSize=7 < numPages=8
	// and clamped to 7, but the chain still referenced page 8 (the
	// pre-truncate relocator's "above nFin" entry). The do-while then
	// popped page 8 (iFreePg=8) and the `iFreePg > dbSize` guard fired
	// SQLITE_CORRUPT_BKPT, masking a real C-parity pop. The correct
	// behavior is to let the engine's own truncation machinery keep
	// numPages authoritative — the file is always at or above numPages
	// after a successful TruncateNoFreelistAdjust, and an explicit
	// truncate mid-loop is a feature, not a bug.
	for i := 0; i < n; i++ {
		done, stepped, err := t.incrVacuumOneStep(bCommit, nFin, iLastPg)
		if err != nil {
			return steps, err
		}
		if done {
			return steps, nil
		}
		if stepped {
			steps++
		}
	}
	return steps, nil
}

// incrVacuumOneStep performs one incremental-vacuum step on the tail page
// and reports: done=true — the drain stops cleanly at this point (tail is
// the schema page, a skip page under bCommit=0, the freelist chain is
// empty, or the tail was an orphan/root); stepped=true — the step consumed
// one unit (the caller increments its counter and keeps draining).
func (t *BTree) incrVacuumOneStep(bCommit bool, nFin uint32, iLastPg uint32) (done, stepped bool, err error) {
	lastPg := t.vacuumTailPage(iLastPg)
	if lastPg <= 1 {
		// Page 1 is the schema page; can't truncate below it.
		return true, false, nil
	}
	skipped, err := t.skipVacuumTailPage(lastPg, bCommit)
	if err != nil {
		return true, false, err
	}
	if skipped {
		// bCommit==1: one no-op step; the caller's loop decrement walks
		// past the skip page. bCommit==0: the file was truncated past
		// the skip pages in one step; the drain stops here.
		return bCommit, bCommit, nil
	}
	isFree := t.vacuumTailIsFree(lastPg)
	if os.Getenv("FRIGOLITE_VACUUM_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "VACDBG step bCommit=%v nFin=%d lastPg=%d isFree=%v\n", bCommit, nFin, lastPg, isFree)
	}
	if isFree {
		if err := t.freeVacuumTail(lastPg, bCommit); err != nil {
			return true, false, err
		}
		return false, true, nil
	}
	done, err = t.acquireAndRelocate(bCommit, nFin, lastPg)
	if err != nil {
		return false, false, err
	}
	if done {
		return true, false, nil
	}
	// Truncate the file to remove the (now-relocated) last page.
	// btree.c:4100 — bCommit=0 sets pBt->bDoTruncate=1; the actual
	// file shrink happens in sqlite3BtreeCommitPhaseOne. We mirror
	// that by truncating immediately (the engine's Truncate does
	// both the file shrink and the chain-skip in one step).
	// bCommit=1: the C bCommit=1 path does NOT truncate during
	// the step. The post-loop block in autoVacuumCommit
	// truncates the file to nFin in one shot — otherwise the
	// chain entries above the new file size become dangling
	// references and the next iteration's dbSize check fires
	// SQLITE_CORRUPT_BKPT (the visible bug for
	// autovacuum-1.1.16, autovacuum-2.x, incrvacuum-6).
	if bCommit {
		return false, true, nil
	}
	if err := t.truncateRelocatedTail(lastPg); err != nil {
		return true, false, err
	}
	return false, true, nil
}

// vacuumTailPage returns the page to inspect this step: the caller-provided
// iLastPg when set, else the current file tail.
func (t *BTree) vacuumTailPage(iLastPg uint32) uint32 {
	if iLastPg > 0 {
		return iLastPg
	}
	return t.pager.NumPages()
}

// skipVacuumTailPage handles a tail page that is skipped rather than
// relocated. btree.c:4017 — the PENDING_BYTE page is skipped: the bCommit==0
// tail block (lines 4096-4102) decrements iLastPg past it and sets nPage,
// while the bCommit==1 caller (autoVacuumCommit) decrements iFree past it.
// btree.c:4017 PTRMAP_ISPAGE — the ptrmap page itself is skipped the same
// way (page 2 is a ptrmap covering page 1, and the C post-block decrements
// iLastPg past it). For bCommit==0 we mirror C by skipping the work AND
// truncating past the skip page in one step (no work was done, so the file
// shrinks by the skip count). Returns skipped=true when the page was a skip
// page (the caller moves on or stops, per bCommit).
func (t *BTree) skipVacuumTailPage(lastPg uint32, bCommit bool) (bool, error) {
	if lastPg != t.pager.PendingBytePage() && !storage.IsPtrmapPageNo(lastPg, t.pageSize) {
		return false, nil
	}
	if bCommit {
		return true, nil
	}
	newLastPg := t.vacuumSkipPages(lastPg - 1)
	if err := t.pager.TruncateNoFreelistAdjust(newLastPg); err != nil {
		return true, fmt.Errorf("btree: IncrVacuumStep: truncate to %d: %w", newLastPg, err)
	}
	return true, nil
}

// vacuumTailIsFree reports whether the tail page is free (btree.c:4019):
// freelist membership, or a PTRMAP_FREELIST pointer-map entry.
func (t *BTree) vacuumTailIsFree(lastPg uint32) bool {
	if pager.IsPageOnFreelist(t.pager, lastPg) {
		return true
	}
	ptype, _, err := t.pager.ReadPtrmap(lastPg)
	return err == nil && ptype == storage.PtrmapFreelist
}

// freeVacuumTail handles a tail page already on the freelist
// (btree.c:4034-4049). bCommit==0: pop the page from the chain
// (BTALLOC_EXACT) before the file shrinks; the bCommit==0 tail block in C
// then sets bDoTruncate=1 and nPage=iLastPg-1, and the actual file truncate
// happens at commit. We mirror that by truncating immediately (no commit
// hook between PRAGMA incremental_vacuum steps). bCommit==1: leave the
// chain alone. The post-loop block in autoVacuumCommit zeroes the chain
// header and truncates the file to nFin in one shot. Truncating here would
// shrink the file below the chain's reach (the chain still has the popped
// page) and turn the remaining chain entries into dangling references. The
// C bCommit==1 path returns SQLITE_OK without touching nPage or
// bDoTruncate when the tail is free; the caller decrements iFree and loops.
func (t *BTree) freeVacuumTail(lastPg uint32, bCommit bool) error {
	if bCommit {
		return nil
	}
	t.pager.TakePageFromFreelist(lastPg)
	return t.truncateVacuumTail(lastPg)
}

// allocRelocationTarget allocates the relocation target for the LIVE-tail
// branch (btree.c:4050-4093). bCommit=0 runs the allocator exactly once
// (BTALLOC_LE, nearby=nFin). bCommit=1 runs a do-while: pop ANY free page
// (BTALLOC_ANY), and if it lands above nFin the pop is discarded (the page
// is just gone — the file truncation reclaims the slot). The do-while is
// bounded by the chain count; an empty chain reports freePg==nil with no
// error — the SQLITE_DONE equivalent (the drain can't shrink the file any
// further).
func (t *BTree) allocRelocationTarget(bCommit bool, nFin uint32) (*pager.Page, error) {
	if !bCommit {
		// bCommit=0: BTALLOC_LE(nFin) once.
		freePg, err := t.pager.AllocatePageLE(nFin)
		if err != nil {
			// btree.c:4076 CORRUPT / SQLITE_FULL — chain empty or
			// no free page ≤ nFin. The PRAGMA path treats this as
			// DONE; the C code returns SQLITE_DONE at this point.
			return nil, nil
		}
		if err := t.checkRelocationTargetPage(freePg.PageNum); err != nil {
			return nil, err
		}
		return freePg, nil
	}
	// bCommit=1 do-while: BTALLOC_ANY, discard pops above nFin.
	for {
		freePg, err := t.pager.AllocatePageANY()
		if err != nil {
			return nil, nil
		}
		if err := t.checkRelocationTargetPage(freePg.PageNum); err != nil {
			return nil, err
		}
		if freePg.PageNum <= nFin {
			return freePg, nil
		}
		// Pop landed above nFin — discard. The pop already
		// decremented the count, so the chain stays
		// consistent. The page will be truncated away at the
		// commit end (autoVacuumCommit's bDoTruncate / nPage).
		// CRUCIAL: do NOT call FreePage here — the C btree.c
		// equivalent is `releasePage(pFreePg)` which only drops
		// the in-memory reference. Putting the page BACK on the
		// chain via freePageWithPtrmap would re-increment the
		// count and create an infinite loop (pop-decrement, free-
		// increment, pop-decrement, ...). The page's data is
		// also already overwritten with the on-disk free-page
		// content, so it's not btree data.
		_ = freePg // discard the page reference; do NOT free it
	}
}

// checkRelocationTargetPage validates a popped free page against the file
// size — btree.c:4081 CORRUPT_BKPT guard. Refusing the relocation is the
// safe move: a page number beyond the file can't host a btree node. The
// wasted allocation is returned to the chain (it'll be zeroed by the commit
// end if nVac==nFree).
func (t *BTree) checkRelocationTargetPage(iFreePg uint32) error {
	dbSize := t.pager.NumPages()
	if iFreePg > dbSize {
		_ = t.freePageWithPtrmap(iFreePg)
		return fmt.Errorf("btree: IncrVacuumStep: iFreePg %d > dbSize %d (corrupt freelist)", iFreePg, dbSize)
	}
	return nil
}

// acquireAndRelocate allocates a relocation target (allocRelocationTarget)
// and moves lastPg's content into it (relocateVacuumTail). done=true means
// the drain stops cleanly here: the chain is empty (SQLITE_DONE equivalent —
// the drain can't shrink the file any further), the tail is a genuine root,
// or the tail page was an orphan; the wasted target allocation, when any,
// has already been returned to the freelist.
func (t *BTree) acquireAndRelocate(bCommit bool, nFin, lastPg uint32) (bool, error) {
	freePg, err := t.allocRelocationTarget(bCommit, nFin)
	if err != nil {
		return false, err
	}
	if freePg == nil {
		return true, nil
	}
	return t.relocateVacuumTail(freePg, lastPg)
}

// relocateVacuumTail relocates lastPg's content into freePg (RelocatePage)
// and handles the stop-cleanly outcomes: a genuine root tail
// (errRelocateRoot, btree.c:4030 reports CORRUPT there — the vacuum must
// never relocate or truncate it) and the orphan branch (the page's parent
// could not be located; do NOT truncate the file — the btree has a (stale)
// parent pointer to `lastPg` that the relocator was supposed to update, and
// truncating would leave the btree referencing a non-existent page). Both
// return the wasted `to` allocation to the freelist and report done=true.
// A relocation ERROR (not the orphan branch) frees the wasted `to` page and
// is reported.
func (t *BTree) relocateVacuumTail(freePg *pager.Page, lastPg uint32) (bool, error) {
	if os.Getenv("FRIGOLITE_VACUUM_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "VACDBG step relocating lastPg=%d -> freePg=%d\n", lastPg, freePg.PageNum)
	}
	relocated, err := t.RelocatePage(freePg.PageNum, lastPg)
	if errors.Is(err, errRelocateRoot) {
		_ = t.freePageWithPtrmap(freePg.PageNum)
		return true, nil
	}
	if err != nil {
		_ = t.freePageWithPtrmap(freePg.PageNum)
		return false, fmt.Errorf("btree: IncrVacuumStep: relocate %d -> %d: %w", lastPg, freePg.PageNum, err)
	}
	if !relocated {
		if err := t.freePageWithPtrmap(freePg.PageNum); err != nil {
			return true, fmt.Errorf("btree: IncrVacuumStep: free wasted %d: %w", freePg.PageNum, err)
		}
		return true, nil
	}
	return false, nil
}

// truncateVacuumTail truncates the file past lastPg without adjusting the
// freelist (the bCommit=0 skip path).
func (t *BTree) truncateVacuumTail(lastPg uint32) error {
	newLastPg := t.vacuumSkipPages(lastPg - 1)
	if err := t.pager.TruncateNoFreelistAdjust(newLastPg); err != nil {
		return fmt.Errorf("btree: IncrVacuumStep: truncate to %d: %w", newLastPg, err)
	}
	return nil
}

// truncateRelocatedTail truncates past the relocated tail page (the
// engine's Truncate does both the file shrink and the chain-skip in one
// step). The skip-decrement mirrors btree.c:4096-4102: any ptrmap/pending-
// byte page between lastPg-1 and the new file end is also removed. Without
// this, a step that relocates lastPg=3 (where page 2 is a ptrmap page
// covering page 1) would leave the file at 2 pages, and the next call's
// iLastPg=2 hits the ptrmap-page branch with no progress — an infinite
// loop (the visible incrvacuum-5.2.5 hang). vacuumSkipPages(2) = 1, so the
// file ends at 1 page and the caller's `iLastPg <= nFin` guard breaks the
// loop.
func (t *BTree) truncateRelocatedTail(lastPg uint32) error {
	newLastPg := t.vacuumSkipPages(lastPg - 1)
	if err := t.pager.Truncate(newLastPg); err != nil {
		return fmt.Errorf("btree: IncrVacuumStep: truncate to %d: %w", newLastPg, err)
	}
	return nil
}

// findParentByWalk scans the database looking for `target` as a
// child of some btree node. Returns the parent page number and
// the ptrmap type. The walk must descend into every user-table
// btree (not just `t.rootPage`) because:
//
//  1. The pointer-map is uninitialized for pages allocated by
//     btree_insert.go (which bypasses the allocBtreeNode /
//     allocOverflow helpers in btree_alloc.go and uses
//     t.pager.AllocatePage() directly — leaving the ptrmap with
//     type=0 for those pages).
//  2. runIncrVacuumStep invokes this with t.rootPage=1, which
//     is the schema (sqlite_schema) btree. The schema btree
//     only references rootpages of user tables/indexes, never
//     the btree pages of those tables. The target is almost
//     always a user-table btree page, so walking only the schema
//     finds nothing.
//
// The walk reads sqlite_schema (the schema btree at page 1) to
// enumerate every (rootpage, type) for tables and indexes, then
// walks each btree. This is O(N) where N is the btree size, the
// same cost as the ptrmap would have been.
//
// Reference: src/btree.c::relocatePage (the C code uses ptrmap
// for the same lookup; the ptrmap is populated at every
// allocateBTreePage call site. Until those call sites are
// uniformly wired in Go, this walk is the only way to find the
// parent.)
func (t *BTree) findParentByWalk(target uint32) (uint32, byte, error) {
	if t.rootPage == target {
		return 0, 0, fmt.Errorf("page %d is the btree root", target)
	}
	// 1. Walk the schema btree to enumerate every user-table / index root.
	if pp, err := t.findParentInBtree(1, target); err == nil {
		return pp.parent, storage.PtrmapBtree, nil
	} else if !errors.Is(err, errNotInBtree) {
		// Schema btree walk itself failed; report the underlying error.
		return 0, 0, err
	}
	// 2. The schema walk itself does not visit user btree pages (the
	//    schema records point at root pages, but their interior/leaf
	//    subtrees are not reached). Enumerate the rootpages recorded
	//    in sqlite_schema and walk each subtree for the target.
	//    Done unconditionally so user-btree interior/leaf pages can
	//    be located even when the BTree handle's rootPage points
	//    at a user-table root (autovacuum-9.5: maybeRebalanceAfterDelete
	//    must find the parent of an empty user-table leaf to call
	//    balanceNonroot + FreePage; the schema walk covers only page 1,
	//    not the user-table subtrees).
	{
		_ = t.rootPage // walked regardless of whether this handle is the schema btree
		roots, rerr := t.collectSchemaRoots()
		if rerr == nil {
			for _, r := range roots {
				if r == target {
					return 0, 0, fmt.Errorf("page %d is a root", target)
				}
				if pp, err := t.findParentInBtree(r, target); err == nil {
					return pp.parent, storage.PtrmapBtree, nil
				} else if !errors.Is(err, errNotInBtree) {
					return 0, 0, err
				}
			}
		}
	}
	return 0, 0, fmt.Errorf("page %d not found in btree", target)
}

// errNotInBtree is returned by findParentInBtree when `target` is
// not a child of any node in the btree rooted at `rootPgno`.
var errNotInBtree = fmt.Errorf("not in btree")

// findParentInBtree walks the btree rooted at `rootPgno` and
// returns the (parent, target) edge if `target` is found as a
// child. Returns errNotInBtree if `target` is not in the btree
// (caller should try another root or fail).
func (t *BTree) findParentInBtree(rootPgno, target uint32) (struct {
	parent uint32
	child  uint32
}, error) {
	var queue []struct {
		parent uint32
		child  uint32
	}
	if err := t.walkChildren(rootPgno, &queue); err != nil {
		return struct {
			parent uint32
			child  uint32
		}{}, err
	}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e.child == target {
			return e, nil
		}
		if pager.IsPageOnFreelist(t.pager, e.child) {
			continue
		}
		if err := t.walkChildren(e.child, &queue); err != nil {
			return struct {
				parent uint32
				child  uint32
			}{}, err
		}
	}
	return struct {
		parent uint32
		child  uint32
	}{0, 0}, errNotInBtree
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
