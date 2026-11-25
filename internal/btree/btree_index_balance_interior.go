// Index interior balance: the balance_nonroot gather/pack path for a parent
// interior page that cannot accept a child split's divider. It folds the
// pending `(C,Kold) → (C,D1),(P1,Kold)` transformation into the anchor's
// entries before sorting, then redistributes the whole parent-child window.
// A bail leaves every page untouched and the tail split remains the fallback.

package btree

import (
	"sort"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// balanceIndexInteriorSplit redistributes a full index interior page and its
// pending child divider across the gathered window. It returns placed=true
// when the parent-boundary rebuild fits; the extra output page is included in
// that rebuild, so no carrier split result is handed to the upper parent.
func (t *BTree) balanceIndexInteriorSplit(pg *pager.Page, parentPgno, childPageNum uint32, childSplits []leafSplitResult) (bool, []leafSplitResult, error) {
	if t.isTable || parentPgno == 0 || len(childSplits) != 1 {
		return false, nil, nil
	}
	parentPg, err := t.pager.ReadPage(parentPgno)
	if err != nil {
		return false, nil, err
	}
	parentCo := contentOffset(parentPg.PageNum)
	parent, err := storage.ParsePage(parentPg.Data, int(t.pageSize), parentCo)
	if err != nil {
		return false, nil, err
	}
	if parent.PageType != storage.PageTypeInteriorIndex {
		return false, nil, nil
	}
	ptrBase := parentCo + cellPtrOffset(parent.PageType)
	// C's bBulk is true when the page being balanced is directly under the
	// root; the chain's insDepth is the anchor's depth at this point.
	bulk := t.insDepth == 2
	g, ok, err := t.gatherIndexBalancePages(parentPg, parent, parentCo, ptrBase, pg.PageNum, bulk)
	if !ok || err != nil {
		return false, nil, err
	}
	w, ok, err := t.gatherIndexInteriorWindow(parentPg, parent, parentCo, ptrBase, g, pg, childPageNum, childSplits[0])
	if !ok || err != nil {
		return false, nil, err
	}
	w.bulk = bulk
	if err := t.planIndexInteriorWindow(w); err != nil {
		return false, nil, err
	}
	if w.parentPlan == nil {
		return false, nil, nil
	}
	levels, fits, err := t.planIndexBalanceCascade(w.parentPlan)
	if err != nil || !fits {
		return false, nil, err
	}
	// All room checks passed. Allocate and encode only now so an atomic bail
	// can never strand a page or an overflow chain.
	if err := prepareInteriorWindowPages(t, w); err != nil {
		return false, nil, err
	}
	if err := t.prepareCascadeLevels(levels, w.outPages[:len(w.cntNew)]); err != nil {
		return false, nil, err
	}
	top := cascadeTopPlan(w.parentPlan, levels)
	if err := t.prepareIndexBalanceParent(top); err != nil {
		return false, nil, err
	}
	if err := t.commitIndexBalanceParent(top); err != nil {
		return false, nil, err
	}
	for _, cw := range cascadeWindows(levels) {
		if err := t.writeIndexInteriorPages(cw); err != nil {
			return false, nil, err
		}
	}
	if err := t.writeIndexInteriorPages(w); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

// indexInteriorWindow holds the merged interior entries and their per-page
// gather state. rightmosts is updated by the pending-divider fold before the
// parent dividers are appended.
type indexInteriorWindow struct {
	parentPg     *pager.Page
	parent       *storage.BTreePage
	parentCo     int
	ptrBase      int
	pages        []*pager.Page
	outPages     []*pager.Page
	coff         []int
	outCo        []int
	gather       *indexBalanceGather
	entries      []interiorEntry
	sizes        []int
	cntOld       []int
	used         []int
	cntNew       []int
	oldPay       [][]byte
	rightmost    []uint32
	parentPlan   *indexBalanceParentPlan
	lastOriginal int
	fresh        []bool
	bulk         bool
	needsExtra   bool
	carried      map[int]int
	trailingPos  int
	anchorDivPos int
}

// gatherIndexInteriorWindow reads every gathered interior page, folds the
// pending divider into the anchor, appends each parent divider after its left
// page and sorts the merged entry array by full key.
func (t *BTree) gatherIndexInteriorWindow(parentPg *pager.Page, parent *storage.BTreePage, parentCo, ptrBase int, g *indexBalanceGather, anchor *pager.Page, childPageNum uint32, cs leafSplitResult) (*indexInteriorWindow, bool, error) {
	w := &indexInteriorWindow{
		parentPg: parentPg, parent: parent, parentCo: parentCo, ptrBase: ptrBase,
		pages: g.pages, outPages: g.pages, gather: g, coff: make([]int, len(g.pages)),
		cntOld: make([]int, len(g.pages)), used: make([]int, len(g.pages)),
		rightmost: make([]uint32, len(g.pages)),
	}
	w.outCo = w.coff
	anchorIdx := -1
	for i, ip := range g.pages {
		ipCo := contentOffset(ip.PageNum)
		w.coff[i] = ipCo
		ipPage, err := storage.ParsePage(ip.Data, int(t.pageSize), ipCo)
		if err != nil {
			return nil, false, err
		}
		if ipPage.PageType != storage.PageTypeInteriorIndex {
			return nil, false, nil
		}
		entries := t.collectInteriorEntries(ip, ipCo+cellPtrOffset(ipPage.PageType)-8, ipPage)
		w.rightmost[i] = ipPage.RightmostPtr
		if ip.PageNum == anchor.PageNum {
			anchorIdx = i
			pending, rightmost, ok := t.foldPendingDivider(entries, ipPage, childPageNum, cs)
			if !ok {
				return nil, false, nil
			}
			entries = append(entries, pending...)
			w.rightmost[i] = rightmost
		}
		start := len(w.entries)
		for _, e := range entries {
			w.entries = append(w.entries, e)
			w.sizes = append(w.sizes, t.dividerCellLen(e.splitResult(t.isTable)))
		}
		if i < len(g.boundaries) {
			payload, perr := t.indexBalancePayload(parentPg, ptrBase, g.boundaries[i])
			if perr != nil {
				return nil, false, perr
			}
			divider := interiorEntry{leftChild: w.rightmost[i], payload: payload}
			w.entries = append(w.entries, divider)
			w.sizes = append(w.sizes, t.dividerCellLen(divider.splitResult(t.isTable)))
			w.oldPay = append(w.oldPay, payload)
		}
		w.cntOld[i] = len(w.entries)
		w.used[i] = indexBalanceUsed(w.sizes, start, len(w.entries))
	}
	if anchorIdx < 0 {
		return nil, false, nil
	}
	sort.SliceStable(w.entries, func(i, j int) bool {
		return t.compareKey(w.entries[i].payload, w.entries[j].payload) < 0
	})
	// Keep sizes aligned with the sorted entries: cntNew packing indexes
	// both arrays positionally.
	for i := range w.entries {
		w.sizes[i] = t.dividerCellLen(w.entries[i].splitResult(t.isTable))
	}
	return w, true, nil
}

// planIndexInteriorWindow computes cntNew with one extra page and builds the
// room-checked parent probe. It allocates no page and encodes no divider: the
// extra page and the encoded boundaries are prepared at commit time, once the
// whole cascade (if any) has passed its room checks. A bail sets
// w.parentPlan=nil and leaves every page untouched.
func (t *BTree) planIndexInteriorWindow(w *indexInteriorWindow) error {
	oldN := len(w.pages)
	cntNew, ok := packIndexBalance(w.sizes, w.cntOld, w.used, int(t.usableSize)-12, oldN+1, false)
	if !ok || len(cntNew) < oldN {
		return nil
	}
	k := len(cntNew)
	anchor := w.gather.anchorIdx
	if anchor < 0 || anchor >= oldN {
		return nil
	}
	n := len(w.entries)
	for p := 0; p < k; p++ {
		start, end, boundary, valid := indexBalanceRun(cntNew, p, n)
		if !valid || (p < k-1 && boundary < 0) {
			return nil
		}
		used := indexBalanceUsed(w.sizes, start, end)
		if used+interiorHeaderSize+2*(end-start+1) > int(t.usableSize) {
			return nil
		}
	}
	boundaries := make([]indexBalanceParentBoundary, k-1)
	for i := range boundaries {
		boundaries[i].payload = w.entries[cntNew[i]].payload
	}
	probe := t.newIndexBalanceParentPlan(w.parentPg, w.parent, w.parentCo, w.ptrBase, w.gather, boundaries, 0)
	if probe == nil {
		return nil
	}
	// Do not bail on a full parent here: the cascade planner owns the room
	// check and rebalances this parent one level up when it does not fit.
	w.parentPlan = probe
	w.cntNew = cntNew
	w.lastOriginal = len(w.pages) - 1
	w.needsExtra = k > oldN
	return nil
}

// prepareInteriorWindowPages allocates the extra output page (when needed),
// resolves every new boundary's left child to its real output page, and sets
// the plan's trailing rightmost pointer. It runs only after all room checks
// have passed, so a bail can never strand an allocated page.
func prepareInteriorWindowPages(t *BTree, w *indexInteriorWindow) error {
	if w.needsExtra {
		anchor := w.gather.anchorIdx
		np, err := t.allocBtreeNode(w.parentPg.PageNum)
		if err != nil {
			return err
		}
		npCo := contentOffset(np.PageNum)
		np.Data[npCo] = w.pages[anchor].Data[w.coff[anchor]]
		at := anchor + 1
		w.outPages = append([]*pager.Page(nil), w.pages...)
		w.outCo = append([]int(nil), w.coff...)
		w.fresh = make([]bool, len(w.pages)+1)
		w.fresh[at] = true
		w.outPages = append(w.outPages[:at], append([]*pager.Page{np}, w.outPages[at:]...)...)
		w.outCo = append(w.outCo[:at], append([]int{npCo}, w.outCo[at:]...)...)
	}
	for i := range w.parentPlan.boundaries {
		w.parentPlan.boundaries[i].leftChild = w.outPages[i].PageNum
	}
	w.parentPlan.rightmost = w.outPages[len(w.cntNew)-1].PageNum
	return nil
}

// writeIndexInteriorPages rewrites each output page with its cntNew run. A
// page's rightmost pointer is the child pointer carried by the boundary
// consumed into the parent; the last page keeps the final rightmost of the
// original rightmost sibling (updated by a rightmost child split). It does not
// touch the parent — cascade levels use this so their consumed parent edit
// stays folded into the level above instead of being written twice.
func (t *BTree) writeIndexInteriorPages(w *indexInteriorWindow) error {
	for pi, ip := range w.outPages {
		if pi < len(w.fresh) && w.fresh[pi] {
			continue // freshly allocated: no old divider chains to release
		}
		page, err := storage.ParsePage(ip.Data, int(t.pageSize), contentOffset(ip.PageNum))
		if err != nil {
			return err
		}
		if err := t.freeInteriorDividerChains(ip, page); err != nil {
			return err
		}
	}
	ptroff := cellPtrOffset(storage.PageTypeInteriorIndex)
	for p, ip := range w.outPages {
		start, end, _, _ := indexBalanceRun(w.cntNew, p, len(w.entries))
		rightmost := w.rightmost[w.lastOriginal]
		if p < len(w.outPages)-1 {
			rightmost = w.entries[w.cntNew[p]].leftChild
		}
		if err := t.writeSpillInteriorPage(ip, w.outCo[p], ptroff, w.entries[start:end], rightmost); err != nil {
			return err
		}
	}
	return nil
}
