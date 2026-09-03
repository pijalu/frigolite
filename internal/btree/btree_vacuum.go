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
	if parentType == storage.PtrmapOverflow1 || parentType == storage.PtrmapOverflow2 {
		return fmt.Errorf("btree: updateParentChildPtr: overflow parent not supported (parent=%d oldChild=%d)", parentPgno, oldChild)
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
			if err := t.pager.Truncate(lastPg - 1); err != nil {
				return steps, fmt.Errorf("btree: IncrVacuumStep: truncate past pending byte: %w", err)
			}
			steps++
			continue
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
			_ = t.pager.FreePage(freePg.PageNum)
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
			if err := t.pager.FreePage(freePg.PageNum); err != nil {
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
				if err := t.pager.WritePtrmap(child, storage.PtrmapBtree, pgNo); err != nil {
					return err
				}
			}
		}
		rmp := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
		if rmp != 0 {
			if err := t.pager.WritePtrmap(rmp, storage.PtrmapBtree, pgNo); err != nil {
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
		if err := t.pager.WritePtrmap(c.Overflow, storage.PtrmapOverflow1, pgNo); err != nil {
			return err
		}
	}
	return nil
}

// findParentInOverflowChain walks the btree rooted at `rootPgno`
// looking for `target` as the overflow next-pointer of any leaf
// cell. Overflow pages are not btree children — they hang off
// leaf cells — so a separate scan is needed. Returns the owning
// cell's page number on success, errNotInBtree if `target` is not
// in the chain.
func (t *BTree) findParentInOverflowChain(rootPgno, target uint32) (uint32, error) {
	if rootPgno == 0 {
		return 0, errNotInBtree
	}
	if pager.IsPageOnFreelist(t.pager, rootPgno) {
		return 0, errNotInBtree
	}
	pg, err := t.pager.ReadPage(rootPgno)
	if err != nil {
		return 0, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return 0, err
	}
	var cellType storage.CellType
	var ptrBase int
	switch page.PageType {
	case storage.PageTypeLeafTable:
		cellType = storage.CellTableLeaf
		ptrBase = coff
	case storage.PageTypeLeafIndex:
		cellType = storage.CellIndexLeaf
		ptrBase = coff
	case storage.PageTypeInteriorTable:
		cellType = storage.CellTableInterior
		ptrBase = coff + cellPtrOffset(page.PageType) - 8
	case storage.PageTypeInteriorIndex:
		cellType = storage.CellIndexInterior
		ptrBase = coff + cellPtrOffset(page.PageType) - 8
	default:
		return 0, errNotInBtree
	}
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(t.pageSize)))
		if cellOff+4 > len(pg.Data) {
			continue
		}
		c, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.usableSize))
		if err != nil {
			continue
		}
		if c.Overflow == target {
			return pg.PageNum, nil
		}
		if page.PageType == storage.PageTypeInteriorTable || page.PageType == storage.PageTypeInteriorIndex {
			if c.LeftPtr != 0 {
				if p, err := t.findParentInOverflowChain(c.LeftPtr, target); err == nil {
					return p, nil
				} else if !errors.Is(err, errNotInBtree) {
					return 0, err
				}
			}
		}
	}
	if page.PageType == storage.PageTypeInteriorTable || page.PageType == storage.PageTypeInteriorIndex {
		rmp := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
		if rmp != 0 {
			if p, err := t.findParentInOverflowChain(rmp, target); err == nil {
				return p, nil
			} else if !errors.Is(err, errNotInBtree) {
				return 0, err
			}
		}
	}
	return 0, errNotInBtree
}

// schemaCursor opens a cursor on the schema btree (rooted at page 1).
// Used by collectSchemaRoots to walk the schema even when the BTree
// handle's rootPage is a user-table root.
func (t *BTree) schemaCursor() (*Cursor, error) {
	savedRoot := t.rootPage
	t.rootPage = 1
	defer func() { t.rootPage = savedRoot }()
	return t.OpenCursor()
}


// sqlite_schema. The schema btree's cells are (type, name, tblname,
// rootpage, sql) records. We open a cursor on the schema btree
// (rooted at page 1) and walk every cell, decoding just enough of
// the record header to extract the rootpage int64.
func (t *BTree) collectSchemaRoots() ([]uint32, error) {
	// Open the schema btree directly (it's always at page 1), regardless
	// of what this BTree handle's rootPage is. The previous guard
	// `if t.rootPage != 1 { return nil }` made the function a no-op for
	// user-table BTree handles, which broke findParentByWalk in
	// maybeRebalanceAfterDelete (autovacuum-9.5: no roots enumerated
	// → user-table leaves reported as orphans → balanceNonroot never
	// called → FreePage never called → autovacuum never shrinks the
	// file).
	cur, err := t.schemaCursor()
	if err != nil {
		return nil, err
	}
	var roots []uint32
	for {
		payload, _, err := cur.ReadCellData()
		if err != nil {
			break
		}
		root, ok := decodeSchemaRootpage(payload)
		if ok && root > 1 {
			roots = append(roots, root)
		}
		ok2, err := cur.Next()
		if err != nil || !ok2 {
			break
		}
	}
	return roots, nil
}

// decodeSchemaRootpage extracts the 4th field of a sqlite_schema
// record (the rootpage int64). The header is: 1+ varint headerSize
// followed by hdrSize-1 varint serial types; the data follows at
// byte hdrSize.
func decodeSchemaRootpage(payload []byte) (uint32, bool) {
	if len(payload) < 2 {
		return 0, false
	}
	hdrSize, n := binary.Uvarint(payload)
	if n <= 0 || hdrSize == 0 || int(hdrSize) > len(payload) {
		return 0, false
	}
	headerEnd := int(hdrSize)
	dataPos := headerEnd
	// Field 1: type (string).
	typeCode, n := binary.Uvarint(payload[n:headerEnd])
	if n <= 0 {
		return 0, false
	}
	typeBytes, err := storage.SerialTypeLength(typeCode)
	if err != nil {
		return 0, false
	}
	dataPos += int(typeBytes)
	// Field 2: name.
	nameCode, n := binary.Uvarint(payload[n+1 : headerEnd])
	if n <= 0 {
		return 0, false
	}
	nameBytes, err := storage.SerialTypeLength(nameCode)
	if err != nil {
		return 0, false
	}
	dataPos += int(nameBytes)
	// Field 3: tblname.
	tblCode, n := binary.Uvarint(payload[n+2 : headerEnd])
	if n <= 0 {
		return 0, false
	}
	tblBytes, err := storage.SerialTypeLength(tblCode)
	if err != nil {
		return 0, false
	}
	dataPos += int(tblBytes)
	// Field 4: rootpage (int).
	rootCode, n := binary.Uvarint(payload[n+3 : headerEnd])
	if n <= 0 {
		return 0, false
	}
	rootLen, err := storage.SerialTypeLength(rootCode)
	if err != nil {
		return 0, false
	}
	if rootLen == 0 {
		return 0, false
	}
	if dataPos+int(rootLen) > len(payload) {
		return 0, false
	}
	var root int64
	switch rootLen {
	case 1:
		root = int64(int8(payload[dataPos]))
	case 2:
		root = int64(int16(binary.BigEndian.Uint16(payload[dataPos:])))
	case 3:
		v := uint32(payload[dataPos])<<16 | uint32(payload[dataPos+1])<<8 | uint32(payload[dataPos+2])
		if v&0x800000 != 0 {
			v |= 0xFF000000
		}
		root = int64(int32(v))
	case 4:
		root = int64(int32(binary.BigEndian.Uint32(payload[dataPos:])))
	case 6:
		v := uint64(payload[dataPos])<<40 | uint64(payload[dataPos+1])<<32 | uint64(payload[dataPos+2])<<24 |
			uint64(payload[dataPos+3])<<16 | uint64(payload[dataPos+4])<<8 | uint64(payload[dataPos+5])
		if v&0x800000000000 != 0 {
			v |= 0xFF00000000000000
		}
		root = int64(v)
	case 8:
		root = int64(binary.BigEndian.Uint64(payload[dataPos:]))
	default:
		return 0, false
	}
	if root < 0 || root > 0xFFFFFFFF {
		return 0, false
	}
	return uint32(root), true
}
