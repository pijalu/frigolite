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
	if pgnoRoot > t.pager.NumPages() {
		// Beyond the current file: extend to exactly pgnoRoot. Pages
		// allocated along the way are the skipped pointer-map pages —
		// AllocatePage extends by one page at a time and phase 15's
		// allocation wiring leaves them untracked.
		for t.pager.NumPages() < pgnoRoot {
			_ = t.pager.AllocatePageSkipFreelist()
		}
	} else if pager.IsPageOnFreelist(t.pager, pgnoRoot) {
		// Free slot at pgnoRoot: pop it from the freelist exactly
		// (allocateBtreePage BTALLOC_EXACT finding iNear on the chain).
		t.pager.TakePageFromFreelist(pgnoRoot)
	} else {
		// A live page occupies pgnoRoot: relocate it to a fresh page at
		// the end of the file (src/btree.c:10177-10217). The occupant
		// must not itself be a root or a free page (btree.c:10194-10197
		// treats both as corruption). A type-0 entry is fine here:
		// RelocatePage resolves the parent by tree walk for those.
		parentType, _, err := t.pager.ReadPtrmap(pgnoRoot)
		if err != nil {
			return nil, fmt.Errorf("btree: AllocateRootPage: read ptrmap for %d: %w", pgnoRoot, err)
		}
		if parentType == storage.PtrmapRootpage || parentType == storage.PtrmapFreelist {
			return nil, fmt.Errorf("btree: AllocateRootPage: page %d has ptrmap type %d (corrupt)", pgnoRoot, parentType)
		}
		moveTo := t.pager.AllocatePageSkipFreelist()
		if _, err := t.RelocatePage(moveTo.PageNum, pgnoRoot); err != nil {
			return nil, fmt.Errorf("btree: AllocateRootPage: relocate occupant %d -> %d: %w", pgnoRoot, moveTo.PageNum, err)
		}
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
