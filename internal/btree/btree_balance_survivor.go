// The degenerate balance_nonroot outcomes: the all-empty window (every
// gathered sibling lost its cells) and the single-survivor window (every
// surviving cell fits one page after a mass DELETE), including the
// divider-borrow fallback that keeps a non-root parent legal.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// balanceAllEmptyWindow handles the all-empty balance branch: every
// gathered sibling held no cells. Returns handled=true when the caller is
// done (it returns immediately).
func (t *BTree) balanceAllEmptyWindow(ctx *balanceNonrootContext, parent *storage.BTreePage, siblings []*pager.Page, c0, c1, parentCo int) (bool, error) {
	// All gathered cells vanished: every window sibling is empty.
	// C-faithful all-empty balance (balance_nonroot with nCell==0): the
	// window's children and their dividers all go away — every emptied
	// child's key range is empty, so dropping its divider loses nothing —
	// and only pages that keep a reference may survive. The previous port
	// freed every window sibling while dropping only dividers [c0..c1)
	// and zeroing the rightmost pointer based on the post-removal cell
	// count, so the surviving divider d_c1 was left pointing at a FREED
	// page. The next allocation re-issues that page number to a fresh
	// leaf and the tree then holds one child under two parents ("2nd
	// reference to page N" in sqlite3 integrity_check): seeks route
	// through the stale divider into the reused page, REPLACE deletes
	// miss the row it lands beside, and interior splits walk into the
	// duplicate (fts4merge4 wipe-churn: "UNIQUE constraint failed:
	// t2_segments.blockid", "interior rebalance did not converge",
	// "database disk image is malformed"). The over-eager rmp-clear
	// (c1 compared against the REDUCED cell count) additionally orphaned
	// a live rightmost subtree OUTSIDE the window (98 "Page N: never
	// used").
	if c0 < 0 {
		c0 = 0
	}
	if c1 > int(parent.CellCount) {
		c1 = int(parent.CellCount)
	}
	nOrig := int(parent.CellCount)
	fullWindow := nOrig-(c1-c0) == 0
	if ctx.isRoot && fullWindow {
		return true, t.emptyWindowAbsorbRoot(ctx, parent, parentCo, siblings)
	}
	if fullWindow {
		// Non-root parent whose whole child set is in the window: the
		// parent's subtree is dead — free every child, drop every divider
		// and clear the rightmost pointer, and let the dead-child cascade
		// unlink the parent itself (balance()'s upward walk). Keeping any
		// emptied child alive under a divider is NOT an option:
		// moveToChild (btree.c:77872) rejects every descended page with
		// nCell<1, so an empty leaf may never sit below an interior page.
		return true, t.emptyWindowFreeAllChildren(ctx, parentCo, siblings)
	}
	// Partial window: the parent keeps dividers OUTSIDE [c0..c1], so after
	// dropping the window's dividers (INCLUDING d_c1 — child c1 is empty,
	// and a surviving d_c1 was the stale-reference bug) and the rightmost
	// pointer when the window holds it, the parent still holds at least
	// one divider. Free every emptied window child.
	dropCount := c1 - c0 + 1
	if c1 >= nOrig {
		dropCount = c1 - c0
		binary.BigEndian.PutUint32(ctx.parent.Data[parentCo+8:parentCo+12], 0)
	}
	if dropCount > 0 {
		if err := t.removeInteriorCellRange(ctx.parent, parent, c0, dropCount); err != nil {
			return true, err
		}
	}
	for _, sp := range siblings {
		if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
			return true, err
		}
	}
	if err := t.pager.WritePage(ctx.parent); err != nil {
		return true, fmt.Errorf("balanceNonroot: write parent: %w", err)
	}
	return true, nil
}

// emptyWindowAbsorbRoot handles the ROOT whose whole subtree died
// (balance-shallower, btree.c:8918-8943): the root keeps its page number
// (schema entries reference it) and is rewritten as an empty leaf while
// every child is freed.
func (t *BTree) emptyWindowAbsorbRoot(ctx *balanceNonrootContext, parent *storage.BTreePage, parentCo int, siblings []*pager.Page) error {
	for _, sp := range siblings {
		if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
			return err
		}
	}
	coff := parentCo
	if parent.PageType == storage.PageTypeInteriorTable {
		ctx.parent.Data[coff] = storage.PageTypeLeafTable
	} else {
		ctx.parent.Data[coff] = storage.PageTypeLeafIndex
	}
	binary.BigEndian.PutUint16(ctx.parent.Data[coff+1:coff+3], 0)
	binary.BigEndian.PutUint16(ctx.parent.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(ctx.parent.Data[coff+5:coff+7], uint16(t.usableSize))
	ctx.parent.Data[coff+7] = 0
	binary.BigEndian.PutUint32(ctx.parent.Data[coff+8:coff+12], 0)
	pager.MarkPageDirtyForVacuum(t.pager, ctx.parent.PageNum)
	return nil
}

// emptyWindowFreeAllChildren handles a NON-root parent whose whole child set
// is in the window: every child is freed, every divider dropped, the
// rightmost pointer cleared, and the dead-child cascade unlinks the parent
// itself (balance()'s upward walk).
func (t *BTree) emptyWindowFreeAllChildren(ctx *balanceNonrootContext, parentCo int, siblings []*pager.Page) error {
	for _, sp := range siblings {
		if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
			return err
		}
	}
	binary.BigEndian.PutUint32(ctx.parent.Data[parentCo+8:parentCo+12], 0)
	binary.BigEndian.PutUint16(ctx.parent.Data[parentCo+3:parentCo+5], 0)
	if err := t.pager.WritePage(ctx.parent); err != nil {
		return fmt.Errorf("balanceNonroot: write parent: %w", err)
	}
	return t.cascadeChildless(ctx.parent.PageNum)
}

// balanceCoversSingleSurvivor handles coversParent with nNew==1: every
// surviving cell fits one page, so most window children emptied. Keeps the
// children that still hold cells under their original dividers, borrows a
// divider for a single-survivor non-root parent, and absorbs a single
// surviving child into the ROOT (balance_shallower, btree.c:8918-8943).
// Returns handled=true.
func (t *BTree) balanceCoversSingleSurvivor(ctx *balanceNonrootContext, parent *storage.BTreePage, siblings []*pager.Page, c0, c1, parentCo int) (bool, error) {
	// Degenerate redistribution: every surviving cell fits on one page (a
	// mass DELETE emptied most of the window's children).
	ptrBase := parentCo + cellPtrOffset(parent.PageType)
	cellCounts, survivor, err := t.singleSurvivorCounts(siblings)
	if err != nil {
		return true, err
	}
	kept := make([]int, 0, 3)
	for i := range siblings {
		if cellCounts[i] > 0 {
			kept = append(kept, i)
		}
	}
	// Kept children hold cells, so every child reference that survives
	// points at a page with nCell>=1 (moveToChild, btree.c:77872, rejects
	// descended pages with nCell<1 — empty leaves may never sit below an
	// interior page). The SURPLUS (emptied) children are freed only AFTER
	// the parent's new shape is established below — btree.c balance_nonroot
	// frees apOld[i] for i>=nNew at the very end (src/btree.c:8952), after
	// editPage/put4byte dropped every reference to them. Freeing first left
	// the parent's dividers pointing at freed pages whenever the root
	// absorption below was skipped (errRootAbsorbNoFit): the stale divider
	// made the next balance re-gather the freed sibling, free it AGAIN, and
	// the double freelist entry handed one page number out twice
	// (TestShallowerRootAbsorbInteriorChild: row loss + duplicate child).
	keptSet := make(map[int]bool, len(kept))
	for _, i := range kept {
		keptSet[i] = true
	}
	freeSurplus := func() error {
		return t.freeSurplusPages(siblings, keptSet)
	}
	if len(kept) >= 2 {
		return true, t.rebuildParentOverKept(ctx, siblings, kept, c0, ptrBase, freeSurplus)
	}
	if ctx.isRoot {
		return true, t.absorbSurvivorIntoRoot(ctx, parentCo, siblings, survivor, freeSurplus)
	}
	// Exactly ONE child with cells: the parent cannot hold a divider over
	// a single child (0 dividers = a husk), and keeping an emptied child
	// under a divider is illegal. C's balance() walks up to the
	// grandparent here and redistributes the dividers of the parent and
	// its sibling INTERIOR pages, which hands this parent a divider from
	// an adjacent sibling at the same level. Outcome-equivalent local
	// transform: BORROW the outermost divider of an adjacent interior
	// sibling that can spare one (>=2 dividers). The borrowed divider's
	// child sits at the same depth as the surviving child and on the
	// correct side of the borrowed key, so all invariants hold and the
	// grandparent is untouched.
	borrowed, err := t.borrowDividerForSingleSurvivor(ctx, siblings, survivor)
	if err != nil {
		return true, err
	}
	if !borrowed {
		// No donor available (every adjacent sibling is a 1-divider
		// interior): leave the parent with the single survivor only — the
		// transient 0-divider state C's balance() also produces before its
		// upward cascade — rather than failing the delete.
		if err := t.writeInteriorRootAt(ctx.parent.PageNum, []uint32{siblings[survivor].PageNum}, nil); err != nil {
			return true, err
		}
	}
	return true, freeSurplus()
}

// singleSurvivorCounts counts the cells of every gathered sibling and
// returns the index of the FIRST sibling holding cells (-1 when none does).
func (t *BTree) singleSurvivorCounts(siblings []*pager.Page) ([]int, int, error) {
	cellCounts := make([]int, len(siblings))
	survivor := -1 // first child with cells, -1 when none
	for i, sp := range siblings {
		spPage, perr := storage.ParsePage(sp.Data, int(t.pageSize), contentOffset(sp.PageNum))
		if perr != nil {
			return nil, 0, perr
		}
		cellCounts[i] = int(spPage.CellCount)
		if spPage.CellCount > 0 && survivor < 0 {
			survivor = i
		}
	}
	if survivor < 0 {
		return nil, 0, fmt.Errorf("balanceNonroot: coversParent with no surviving cells")
	}
	return cellCounts, survivor, nil
}

// freeSurplusPages frees every gathered sibling NOT in keptSet — the surplus
// (emptied) children — after the parent's new shape has been established
// (btree.c balance_nonroot frees apOld[i] for i>=nNew at the very end,
// src/btree.c:8952).
func (t *BTree) freeSurplusPages(siblings []*pager.Page, keptSet map[int]bool) error {
	for i, sp := range siblings {
		if !keptSet[i] {
			if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
				return err
			}
		}
	}
	return nil
}

// rebuildParentOverKept rewrites the parent over the kept (non-empty)
// children under their ORIGINAL dividers: cells do not move, keys stay
// monotonic, all children keep their depth, and the parent keeps
// kept-1 >= 1 dividers. This case must run BEFORE the root absorption —
// absorbing only the FIRST non-empty child while freeing the rest destroyed
// every other non-empty sibling (18 schema rows lost in
// autovacuum-2.4.7's DROP loop).
func (t *BTree) rebuildParentOverKept(ctx *balanceNonrootContext, siblings []*pager.Page, kept []int, c0, ptrBase int, freeSurplus func() error) error {
	children := make([]uint32, 0, len(kept))
	seps := make([]uint64, 0, len(kept))
	for _, i := range kept {
		children = append(children, siblings[i].PageNum)
		if i != kept[len(kept)-1] {
			seps = append(seps, t.cellKeyAt(ctx.parent, ptrBase, c0+i))
		}
	}
	if err := t.writeInteriorRootAt(ctx.parent.PageNum, children, seps); err != nil {
		return err
	}
	return freeSurplus()
}

// absorbSurvivorIntoRoot absorbs the single non-empty child of the ROOT
// (balance_shallower, btree.c:8918-8943) — the root keeps its page number
// and the emptied child is freed. When the child's content exceeds the
// root's (smaller) usable area the absorption is skipped, exactly like C's
// hdrOffset<=nFree guard: C's parent update has by then replaced every
// window divider and repointed the rightmost pointer at apNew[0]
// (put4byte(pRight, apNew[nNew-1]), src/btree.c:8699), leaving the root a
// 0-cell interior page over the single survivor. Replicate that end state
// BEFORE freeing the surplus children — the earlier free-then-keep order
// left the freed pages referenced by the root's dividers.
func (t *BTree) absorbSurvivorIntoRoot(ctx *balanceNonrootContext, parentCo int, siblings []*pager.Page, survivor int, freeSurplus func() error) error {
	if err := t.absorbSingleChildRoot(ctx.parent, parentCo, siblings[survivor].PageNum); err != nil {
		if err == errRootAbsorbNoFit {
			if err := t.writeInteriorRootAt(ctx.parent.PageNum, []uint32{siblings[survivor].PageNum}, nil); err != nil {
				return err
			}
			return freeSurplus()
		}
		return err
	}
	return freeSurplus()
}

// borrowCandidate is one candidate donor divider: the interior sibling
// immediately to the right of the parent (a LEFT donor is impossible: the
// left sibling's outermost child holds keys ABOVE the right sibling's
// remaining rows — moving it into the parent's low side breaks divider/leaf
// monotonic order, "Rowid N out of order" in sqlite3 integrity_check — C's
// redistribution instead repacks the whole window's boundaries
// monotonically). left records which side the donor sits on.
type borrowCandidate struct {
	pgno uint32
	left bool
}

// borrowDividerForSingleSurvivor walks to the grandparent and tries to borrow
// the outermost divider of an adjacent interior sibling that can spare one
// (>=2 dividers). The borrowed divider's child sits at the same depth as the
// surviving child and on the correct side of the borrowed key, so all
// invariants hold and the grandparent is untouched. Every walk/parse failure
// leaves the parent unchanged (borrowed=false), mirroring the original
// error-tolerant nesting.
func (t *BTree) borrowDividerForSingleSurvivor(ctx *balanceNonrootContext, siblings []*pager.Page, survivor int) (bool, error) {
	gpgno, _, gerr := t.findParentByWalk(ctx.parent.PageNum)
	if gerr != nil {
		return false, nil
	}
	gpg, rerr := t.pager.ReadPage(gpgno)
	if rerr != nil {
		return false, nil
	}
	gcoff := contentOffset(gpg.PageNum)
	gpage, perr := storage.ParsePage(gpg.Data, int(t.pageSize), gcoff)
	if perr != nil {
		return false, nil
	}
	gbase := gcoff + cellPtrOffset(gpage.PageType)
	pidx, ferr := t.findLeafIndexInParent(gpg, ctx.parent.PageNum)
	if ferr != nil {
		return false, nil
	}
	// The parent's OWN bound at the grandparent: the divider
	// key routing keys <= K_P to the parent (absent when the
	// parent is the grandparent's rightmost child).
	var kPOld uint64
	hasKPOld := false
	if pidx >= 0 {
		kPOld = t.cellKeyAt(gpg, gbase, pidx)
		hasKPOld = true
	}
	cands := t.donorCandidates(gpg, gpage, gcoff, gbase, pidx)
	return t.borrowFromDonor(ctx, siblings, survivor, gpg, gpage, gcoff, pidx, kPOld, hasKPOld, cands)
}

// donorCandidates lists the donor interior siblings of the parent on the
// grandparent: the cell-child immediately after the parent, or the
// grandparent's rightmost child when the parent is the last cell-child.
func (t *BTree) donorCandidates(gpg *pager.Page, gpage *storage.BTreePage, gcoff, gbase, pidx int) []borrowCandidate {
	gn := int(gpage.CellCount)
	var cands []borrowCandidate
	if pidx >= 0 {
		if pidx+1 <= gn-1 {
			cp := storage.CellPointer(gpg.Data, gbase-8, pidx+1, int(t.pageSize))
			cands = append(cands, borrowCandidate{binary.BigEndian.Uint32(gpg.Data[cp : cp+4]), false})
		} else {
			r := binary.BigEndian.Uint32(gpg.Data[gcoff+8 : gcoff+12])
			if r != 0 {
				cands = append(cands, borrowCandidate{r, false})
			}
		}
	}
	return cands
}

// borrowFromDonor tries each candidate donor in order; the first donor that
// can spare a divider (>=2 cells) lends it. Every walk/parse failure on a
// candidate just skips to the next.
func (t *BTree) borrowFromDonor(ctx *balanceNonrootContext, siblings []*pager.Page, survivor int, gpg *pager.Page, gpage *storage.BTreePage, gcoff, pidx int, kPOld uint64, hasKPOld bool, cands []borrowCandidate) (bool, error) {
	for _, cd := range cands {
		dpg, derr := t.pager.ReadPage(cd.pgno)
		if derr != nil {
			continue
		}
		dcoff := contentOffset(dpg.PageNum)
		dpage, dperr := storage.ParsePage(dpg.Data, int(t.pageSize), dcoff)
		if dperr != nil || dpage.PageType != storage.PageTypeInteriorTable || int(dpage.CellCount) < 2 {
			continue
		}
		xi, x, k := donorOutermostDivider(dpg, dpage, dcoff, cd)
		var borrowed bool
		var err error
		if cd.left {
			// Borrowed child holds the donor's LOWEST keys,
			// below every surviving key: it becomes the
			// parent's FIRST child, bounded by its own key.
			// The grandparent's divider for the parent (K_P,
			// which still bounds the surviving child and now
			// X too) is unchanged.
			borrowed, err = t.borrowLeftDivider(ctx, siblings[survivor], dpg, dpage, xi, x, k)
		} else {
			// Borrowed child holds keys just ABOVE the
			// parent's old bound K_P: it becomes the parent's
			// LAST child. The parent's internal divider stays
			// K_P (bounding the surviving child), and the
			// GRANDPARENT'S divider for the parent is re-keyed
			// from K_P to K so the borrowed keys route into
			// the parent.
			borrowed, err = t.borrowRightDivider(ctx, siblings[survivor], dpg, dpage, gpg, gpage, pidx, kPOld, hasKPOld, x, k)
		}
		if err != nil {
			return false, err
		}
		if !borrowed {
			continue
		}
		if err := t.pager.WritePage(dpg); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// donorOutermostDivider reads the donor's outermost divider (child + key):
// the LAST cell for a left donor, cell 0 for a right donor.
func donorOutermostDivider(dpg *pager.Page, dpage *storage.BTreePage, dcoff int, cd borrowCandidate) (int, uint32, uint64) {
	var xi int
	if cd.left {
		xi = int(dpage.CellCount) - 1
	}
	xoff := int(binary.BigEndian.Uint16(dpg.Data[dcoff+cellPtrOffset(dpage.PageType)+xi*2:]))
	x := binary.BigEndian.Uint32(dpg.Data[xoff : xoff+4])
	k, _ := util.GetVarint(dpg.Data[xoff+4:])
	return xi, x, k
}

// borrowLeftDivider moves the right donor's outermost divider into the
// parent's low side and rewrites the parent over the borrowed child plus the
// survivor.
func (t *BTree) borrowLeftDivider(ctx *balanceNonrootContext, survivorPg *pager.Page, dpg *pager.Page, dpage *storage.BTreePage, xi int, x uint32, k uint64) (bool, error) {
	if err := t.removeInteriorCellRange(dpg, dpage, xi, 1); err != nil {
		return false, err
	}
	if err := t.writeInteriorRootAt(ctx.parent.PageNum, []uint32{x, survivorPg.PageNum}, []uint64{k}); err != nil {
		return false, err
	}
	return true, nil
}

// borrowRightDivider moves the left donor's outermost divider into the
// parent's high side and re-keys the grandparent's divider for the parent
// from K_P to K so the borrowed keys route into the parent.
func (t *BTree) borrowRightDivider(ctx *balanceNonrootContext, survivorPg *pager.Page, dpg *pager.Page, dpage *storage.BTreePage, gpg *pager.Page, gpage *storage.BTreePage, pidx int, kPOld uint64, hasKPOld bool, x uint32, k uint64) (bool, error) {
	if err := t.removeInteriorCellRange(dpg, dpage, 0, 1); err != nil {
		return false, err
	}
	if err := t.writeInteriorRootAt(ctx.parent.PageNum, []uint32{survivorPg.PageNum, x}, []uint64{kPOld}); err != nil {
		return false, err
	}
	if hasKPOld {
		if err := t.rekeyGrandparentDivider(gpg, gpage, pidx, ctx.parent.PageNum, k); err != nil {
			return false, err
		}
	}
	return true, nil
}

// rekeyGrandparentDivider replaces the grandparent's divider for the parent
// with the borrowed key and persists the grandparent.
func (t *BTree) rekeyGrandparentDivider(gpg *pager.Page, gpage *storage.BTreePage, pidx int, parentPgno uint32, k uint64) error {
	if err := t.removeInteriorCellRange(gpg, gpage, pidx, 1); err != nil {
		return err
	}
	if err := t.insertInteriorDividerAt(gpg, gpage, pidx, parentPgno, k); err != nil {
		return err
	}
	return t.pager.WritePage(gpg)
}
