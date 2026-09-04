// Root-page allocation for auto-vacuum databases (P8.INCRVACUUM BUG D).
// Port of btree.c::btreeCreateTable's autovacuum branch (src/btree.c
// ~10150): the new root page is meta[3]+1 — the largest root page created
// so far, plus one — skipping pointer-map and pending-byte pages. If a
// live page already occupies that position, it is relocated to a freshly
// extended page first (the "pgnoMove" dance), exactly as the C code does.
//
// The guarantee this buys: every root page lives in the contiguous block
// [3..meta[3]] and every non-root page lives above it. incrVacuumStep
// therefore never meets a root page as the relocation tail while the
// freelist is non-empty (a root there is corruption: src/btree.c:4030).
package btree

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// AllocateRootPage allocates the root page for a new table or index in an
// auto-vacuum database and returns it uninitialized (the caller sets the
// page type and header, as with pager.AllocateRootPage before it).
//
// The returned page number is LargestRootPage()+1 (skipping pointer-map
// and pending-byte pages); the pager's meta[3] is updated. When the
// position is already occupied by a live page, that page is relocated to
// a fresh page at the end of the file (btreeCreateTable's pgnoMove dance,
// src/btree.c:10177-10217). On a non-auto-vacuum pager the allocation
// falls back to pager.AllocateRootPage's generic behavior.
func (t *BTree) AllocateRootPage() (*pager.Page, error) {
	if !t.pager.AutoVacuum() {
		return t.pager.AllocateRootPage()
	}
	// meta[3]+1, skipping pointer-map and pending-byte pages
	// (src/btree.c:10168-10172).
	pgnoRoot := t.pager.LargestRootPage() + 1
	if pgnoRoot < 3 {
		pgnoRoot = 3
	}
	for storage.IsPtrmapPageNo(pgnoRoot, t.pageSize) || pgnoRoot == t.pager.PendingBytePage() {
		pgnoRoot++
	}
	if err := t.prepareRootSlot(pgnoRoot); err != nil {
		return nil, err
	}
	pg, err := t.pager.ReadPage(pgnoRoot)
	if err != nil {
		return nil, fmt.Errorf("btree: AllocateRootPage: read %d: %w", pgnoRoot, err)
	}
	if t.pager.AutoVacuum() {
		if err := t.pager.WritePtrmap(pgnoRoot, storage.PtrmapRootpage, 0); err != nil {
			return nil, fmt.Errorf("btree: AllocateRootPage: ptrmap for %d: %w", pgnoRoot, err)
		}
	}
	t.pager.SetLargestRootPage(pgnoRoot)
	return pg, nil
}

// prepareRootSlot makes pgnoRoot usable for a new root page, mirroring
// allocateBtreePage's BTALLOC_EXACT outcomes (src/btree.c:10177-10217):
// beyond EOF the file is extended page by page (the pages allocated along
// the way are the skipped pointer-map pages — phase 15's allocation wiring
// leaves them untracked); a free slot is popped from the freelist; a live
// occupant is relocated to a fresh page at the end of the file. The
// occupant must not itself be a root or a free page (btree.c:10194-10197
// treats both as corruption). A type-0 entry is fine: RelocatePage
// resolves the parent by tree walk for those.
func (t *BTree) prepareRootSlot(pgnoRoot uint32) error {
	if pgnoRoot > t.pager.NumPages() {
		for t.pager.NumPages() < pgnoRoot {
			_ = t.pager.AllocatePageSkipFreelist()
		}
		return nil
	}
	if pager.IsPageOnFreelist(t.pager, pgnoRoot) {
		t.pager.TakePageFromFreelist(pgnoRoot)
		return nil
	}
	parentType, _, err := t.pager.ReadPtrmap(pgnoRoot)
	if err != nil {
		return fmt.Errorf("btree: AllocateRootPage: read ptrmap for %d: %w", pgnoRoot, err)
	}
	if parentType == storage.PtrmapRootpage || parentType == storage.PtrmapFreelist {
		return fmt.Errorf("btree: AllocateRootPage: page %d has ptrmap type %d (corrupt)", pgnoRoot, parentType)
	}
	moveTo := t.pager.AllocatePageSkipFreelist()
	if _, err := t.RelocatePage(moveTo.PageNum, pgnoRoot); err != nil {
		return fmt.Errorf("btree: AllocateRootPage: relocate occupant %d -> %d: %w", pgnoRoot, moveTo.PageNum, err)
	}
	return nil
}

// MoveRoot moves a root b-tree page from `from` to `to` (btree.c
// relocatePage's PTRMAP_ROOTPAGE branch as driven by btreeDropTable:
// the largest root page is moved into a vacated lower-numbered slot so
// the root block stays dense — src/btree.c:10341-10352). `to` must be
// an allocated page (pop it from the freelist first when it was just
// freed — the engine's FreeTable, unlike C's clearTable, frees the root
// page too). The page content is copied, `to`'s pointer-map entry
// becomes PTRMAP_ROOTPAGE, and every child page of the moved root is
// re-parented in the pointer-map (relocatePage's setChildPtrmaps step,
// src/btree.c:6605). A root has no parent pointer to update. `from` is
// freed last (btreeDropTable's freePage(pMove), src/btree.c:10353-10359);
// the caller persists the schema rootpage change and updates meta[3].
func (t *BTree) MoveRoot(from, to uint32) error {
	if from == to {
		return fmt.Errorf("btree: MoveRoot: from == to == %d", from)
	}
	fromPg, err := t.pager.ReadPage(from)
	if err != nil {
		return fmt.Errorf("btree: MoveRoot: read page %d: %w", from, err)
	}
	toPg, err := t.pager.ReadPage(to)
	if err != nil {
		return fmt.Errorf("btree: MoveRoot: read page %d: %w", to, err)
	}
	copy(toPg.Data, fromPg.Data)
	pager.MarkPageDirtyForVacuum(t.pager, to)
	if err := t.pager.WritePtrmap(to, storage.PtrmapRootpage, 0); err != nil {
		return fmt.Errorf("btree: MoveRoot: ptrmap for %d: %w", to, err)
	}
	// btreeDropTable relocates whatever page sits at meta[3] — usually a
	// root, but the meta watermark can also point at a free or content
	// page (src/btree.c keeps no stronger invariant; the schema UPDATE in
	// destroyRootPage matches zero rows in that case). A page with no
	// parseable content has no children to re-parent, so skip the
	// re-parenting step for unparseable content.
	if _, perr := storage.ParsePage(toPg.Data, int(t.pageSize), contentOffset(to)); perr == nil {
		if err := t.setChildPtrmaps(toPg, to); err != nil {
			return fmt.Errorf("btree: MoveRoot: setChildPtrmaps for %d: %w", to, err)
		}
	}
	if err := t.freePageWithPtrmap(from); err != nil {
		return fmt.Errorf("btree: MoveRoot: free page %d: %w", from, err)
	}
	return nil
}