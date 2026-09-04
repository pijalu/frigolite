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
		isRoot := false
		if roots, rerr := t.collectSchemaRoots(); rerr == nil {
			for _, r := range roots {
				if r == from {
					isRoot = true
					break
				}
			}
		}
		if isRoot {
			return false, fmt.Errorf("btree: RelocatePage: %w: page %d is a root", errRelocateRoot, from)
		}
		if pp, pt, perr := t.findParentByWalk(from); perr == nil {
			parentPgno = pp
			parentType = pt
		} else {
			return false, nil
		}
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
	// P8.INCRVACUUM phase 5: update the ptrmap entries for every
	// child of the moved page. The children's "parent" is now `to`,
	// not `from`. Without this, the next vacuum step that tries to
	// move a child of `to` will look up `to` in the ptrmap, fail
	// (entry says parent=from), and the engine falls back to a
	// tree-walk that may pick the wrong ancestor if the parent's
	// own child pointer was already updated. (Port of
	// btree.c::relocatePage's child-fixup step, ~line 6605: b-tree
	// pages run setChildPtrmaps; overflow pages only re-parent their
	// next-in-chain pointer, the first 4 bytes.)
	switch parentType {
	case storage.PtrmapBtree, storage.PtrmapRootpage:
		if err := t.setChildPtrmaps(toPg, to); err != nil {
			return false, fmt.Errorf("btree: RelocatePage: setChildPtrmaps for %d: %w", to, err)
		}
	case storage.PtrmapOverflow1, storage.PtrmapOverflow2:
		if len(toPg.Data) >= 4 {
			if next := binary.BigEndian.Uint32(toPg.Data[0:4]); next != 0 {
				if err := t.pager.WritePtrmap(next, storage.PtrmapOverflow2, to); err != nil {
					return false, fmt.Errorf("btree: RelocatePage: ptrmap next-ovfl %d: %w", next, err)
				}
			}
		}
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
	var cellType storage.CellType
	switch page.PageType {
	case storage.PageTypeLeafTable:
		cellType = storage.CellTableLeaf
	case storage.PageTypeLeafIndex:
		cellType = storage.CellIndexLeaf
	default:
		return fmt.Errorf("btree: updateOvfl1ParentPtr: parent %d is not a leaf (type 0x%02x)", parentPgno, page.PageType)
	}
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(parentPg.Data, ptrBase, i, int(t.pageSize)))
		if cellOff+4 > len(parentPg.Data) {
			continue
		}
		c, cerr := storage.DecodeCell(parentPg.Data, cellOff, cellType, int(t.usableSize))
		if cerr != nil || c.Overflow == 0 {
			continue
		}
		// Cell size on the page: payload-length varint (+ rowid varint
		// for table leaves) + local payload + 4-byte overflow pointer.
		_, n1 := util.GetVarint(parentPg.Data[cellOff:])
		sz := n1 + c.LocalLen + 4
		if cellType == storage.CellTableLeaf {
			_, n2 := util.GetVarint(parentPg.Data[cellOff+n1:])
			sz += n2
		}
		ovflOff := cellOff + sz - 4
		if ovflOff < 0 || ovflOff+4 > len(parentPg.Data) {
			continue
		}
		if binary.BigEndian.Uint32(parentPg.Data[ovflOff:ovflOff+4]) == oldChild {
			binary.BigEndian.PutUint32(parentPg.Data[ovflOff:ovflOff+4], newChild)
			pager.MarkPageDirtyForVacuum(t.pager, parentPgno)
			return nil
		}
	}
	return fmt.Errorf("btree: updateOvfl1ParentPtr: leaf %d does not chain to overflow %d", parentPgno, oldChild)
}

// IncrVacuumStep performs up to n steps of the incremental-vacuum
// algorithm (port of btree.c::incrVacuumStep, src/btree.c:4010).
// Each step:
//  1. If the last page of the file is on the freelist, just truncate
//     the file (decrement numPages by 1).
//  2. Otherwise, the last page is in use. Find the lowest free page
//     (pager.AllocatePageLE) and relocate the last page to that free
//     page (RelocatePage). Truncate the file.
//
// bCommit mirrors the C parameter (btree.c:4245 passes nVac==nFree):
// false for PRAGMA incremental_vacuum (sqlite3BtreeIncrVacuum) AND for
// partial auto-vacuum drains (callback capped nVac < nFree) — each step
// keeps the freelist chain consistent below the truncation point by
// popping the trailing FREE page via TakePageFromFreelist (the
// BTALLOC_EXACT path). True only for a FULL auto-vacuum drain
// (nVac==nFree) — where a trailing FREE page is left alone ("it doesn't
// matter if it still contains some garbage entries", btree.c:4022-4024)
// because the commit end zeroes the whole chain header
// (btree.c:4247-4252).
//
// Returns the number of steps actually performed. Stops early if the
// freelist is empty (no more free pages to swap) or if `n` is exhausted.
func (t *BTree) IncrVacuumStep(n int, bCommit bool) (int, error) {
	steps := 0
	// P8.INCRVACUUM safety net: if the in-memory page count is ahead of
	// the on-disk file (e.g. pages were allocated in memory but never
	// flushed), the "tail" lives in cache only. Trusting it as a vacuum
	// target would relocate phantom pages onto real free pages and
	// corrupt the tree. SQLite C never sees this state because it grows
	// the file at allocation time. Best-effort resync: flush any dirty
	// extends, then clamp numPages to the actual file size when smaller.
	if info, ok := t.pager.FileInfo(); ok && info != nil {
		if fp := uint32(info.Size() / int64(t.pager.PageSize())); fp > 0 && t.pager.NumPages() > fp {
			_ = t.pager.Sync()
			if info2, ok2 := t.pager.FileInfo(); ok2 && info2 != nil {
				if fp2 := uint32(info2.Size() / int64(t.pager.PageSize())); fp2 > 0 && fp2 < t.pager.NumPages() {
					t.pager.SetNumPagesForTesting(fp2)
				}
			}
		}
	}
	for i := 0; i < n; i++ {
		lastPg := t.pager.NumPages()
		if lastPg <= 1 {
			// Page 1 is the schema page; can't truncate below it.
			return steps, nil
		}
		// PENDING_BYTE page: the lock-byte reservation (btree.c
		// PENDING_BYTE_PAGE; src/btree.c:4017 skips this page in
		// incrVacuumStep). The test harness may lower the byte to
		// 0x10000 via sqlite3_test_control_pending_byte; with
		// pageSize=1024, the byte lives in page 65. SQLite's
		// autovacuum truncates the file PAST the pending byte
		// page: the PENDING_BYTE is just a byte offset, and the
		// file can be smaller than the byte position. Mirror that
		// here: simply truncate the file and continue.
		if lastPg == t.pager.PendingBytePage() {
			// The pending-byte page is never on the freelist and the C
			// code never touches the freelist for it (the bCommit==0 tail
			// block just decrements iLastPg past it) — truncate without
			// any freelist count adjustment.
			if err := t.pager.TruncateNoFreelistAdjust(lastPg - 1); err != nil {
				return steps, fmt.Errorf("btree: IncrVacuumStep: truncate past pending byte: %w", err)
			}
			steps++
			continue
		}
		// Check if `lastPg` is free. btree.c:4019 decides this from
		// the pointer-map entry (ptrmapGet): PTRMAP_FREEPAGE means the
		// page can be dropped straight off the tail. The in-memory
		// freelist set is only a fast path for session-local frees —
		// it is empty after a reopen, but a real auto-vacuum file
		// persists PtrmapFreelist entries for freed pages, so the
		// ptrmap read is authoritative when the set misses.
		isFree := pager.IsPageOnFreelist(t.pager, lastPg)
		if !isFree {
			if ptype, _, err := t.pager.ReadPtrmap(lastPg); err == nil && ptype == storage.PtrmapFreelist {
				isFree = true
			}
		}
		if isFree {
			// bCommit==0: pop the page from the freelist chain properly
			// (allocateBtreePage BTALLOC_EXACT equivalent) before the file
			// shrinks, so the on-disk chain never references a truncated
			// page. The pop owns the count decrement. bCommit==1: leave
			// it; the commit end zeroes the chain header (btree.c:4249-4252).
			if !bCommit {
				t.pager.TakePageFromFreelist(lastPg)
			}
			// The truncate itself NEVER adjusts the freelist count on
			// this path: for bCommit==0 the pop above already decremented
			// it, and for bCommit==1 the chain entry stays behind as
			// intentional garbage (btree.c:4026) with the count in
			// lockstep. btree.c only changes the count inside
			// allocateBtreePage/freePage2 — never during truncation.
			if err := t.pager.TruncateNoFreelistAdjust(lastPg - 1); err != nil {
				return steps, fmt.Errorf("btree: IncrVacuumStep: truncate to %d: %w", lastPg-1, err)
			}
			steps++
			continue
		}
		// Pointer-map pages: the C incrVacuumStep checks
		// PTRMAP_ISPAGE early and just skips the body — the bCommit==0
		// tail block decrements iLastPg past the ptrmap page and sets
		// bDoTruncate; no relocation (a ptrmap page has no child
		// pointer to update) and no freelist interaction either way.
		// We just truncate the file past it, freelist count untouched.
		if storage.IsPtrmapPageNo(lastPg, t.pageSize) {
			if err := t.pager.TruncateNoFreelistAdjust(lastPg - 1); err != nil {
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
		if errors.Is(err, errRelocateRoot) {
			// The tail page is a genuine root page: the vacuum must
			// never relocate or truncate it (btree.c:4030 reports
			// CORRUPT). Return the wasted `to` allocation to the
			// freelist and stop the drain cleanly — the file stays
			// above the root. (With root pages allocated in the
			// [3..meta[3]] root block this position is unreachable,
			// matching SQLite's structural guarantee.)
			_ = t.freePageWithPtrmap(freePg.PageNum)
			return steps, nil
		}
		if err != nil {
			// P8.INCRVACUUM.phase8: even on a relocation ERROR (not
			// just the orphan branch), put the wasted `to` page
			// back on the freelist. The relocator's parent-update
			// may have succeeded for `to` (in which case `to` is
			// now a real btree page) — but in that case `to` was
			// copied from `from` and the btree parent now points
			// at `to`, so we must NOT free `to` (the parent would
			// dangle). The error path here is reserved for genuine
			// failures where `to` was not adopted by any btree
			// (e.g. RelocatePage read `from` then `to` then failed
			// the parent update before copying, so `toPg.Data` is
			// unchanged but already loaded in cache). The relocator
			// returns (false, nil) for the orphan branch, so the
			// `err != nil` case is the failure branch where the
			// relocator decided it cannot proceed safely.
			_ = t.freePageWithPtrmap(freePg.PageNum)
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
			// or "database disk image is malformed".
			//
			// P8.INCRVACUUM.phase8: keep the FreePage so the chain
			// count stays accurate.
			if err := t.freePageWithPtrmap(freePg.PageNum); err != nil {
				return steps, fmt.Errorf("btree: IncrVacuumStep: free wasted %d: %w", freePg.PageNum, err)
			}
			//
			// P8.INCRVACUUM note: the SQLite C version never hits
			// this branch because it always writes the ptrmap at
			// allocation time. The proper long-term fix is to wire
			// WritePtrmap into every t.pager.AllocatePage() call
			// site in the btree. Until that's done, this
			// conservative branch keeps the btree intact at the
			// cost of leaving some pages un-relocated.
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
