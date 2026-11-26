// Index leaf balance: the balance_nonroot gather/pack path for a full index
// leaf. The old two-page left spill is replaced by one window operation that
// accepts 2 or 3 adjacent leaves, folds every parent divider into the cell
// array and uses cntNew packing before any page write. A bail (no room in the
// gathered pages or in the parent) leaves the tree untouched and lets
// splitLeafMulti do its normal new-page split.

package btree

import (
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// balanceIndexLeafSplit redistributes an overflowing index leaf across its
// parent-child window. It returns placed=true when the pending cell was
// absorbed. A parent-boundary rebuild covers any output page the window adds,
// so no sibling split result is handed up to the caller.
func (t *BTree) balanceIndexLeafSplit(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell, newCellData []byte, bulk bool) (bool, []leafSplitResult, error) {
	if t.isTable || parentPgno == 0 {
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
	ptrBase := parentCo + cellPtrOffset(parent.PageType)
	g, ok, err := t.gatherIndexBalancePages(parentPg, parent, parentCo, ptrBase, pg.PageNum, bulk)
	if !ok || err != nil {
		return false, nil, err
	}
	w, ok, err := t.gatherIndexLeafWindow(parentPg, parent, ptrBase, g, pg, page, newCell, newCellData)
	if !ok || err != nil {
		return false, nil, err
	}
	defer w.release()
	w.bulk = bulk
	t.planIndexLeafWindow(w)
	if w.parentPlan == nil {
		return false, nil, nil
	}
	levels, fits, err := t.planIndexBalanceCascade(w.parentPlan)
	if err != nil || !fits {
		return false, nil, err
	}
	// Planning copied every boundary payload. Release the consumed cells'
	// overflow chains BEFORE replacement parent dividers are encoded so their
	// allocation reclaims those pages (btree.c insertCell inherits the chain
	// in place; free-then-realloc has the same net effect).
	if err := t.freeConsumedLeafDividerChains(w); err != nil {
		return false, nil, err
	}
	// All room checks passed. Allocate and encode only now so an atomic bail
	// can never strand a page or an overflow chain.
	if err := prepareLeafWindowPages(t, w); err != nil {
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
	if err := t.writeIndexLeafPages(w); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

// indexLeafWindow is the index leaf counterpart of the shared gather: sorted
// entries, their encoded sizes, the old parent dividers (identified by their
// cell struct pointer), and the pack result.
type indexLeafWindow struct {
	parentPg   *pager.Page
	parent     *storage.BTreePage
	parentCo   int
	ptrBase    int
	pages      []*pager.Page
	outPages   []*pager.Page
	coff       []int
	outCo      []int
	gather     *indexBalanceGather
	all        []splitEntry
	sizes      []int
	cntOld     []int
	used       []int
	cntNew     []int
	oldDiv     map[*storage.Cell]bool
	oldPay     [][]byte
	staging    []*splitStaging
	parentPlan *indexBalanceParentPlan
	bulk       bool
	needsExtra bool
}

// gatherIndexLeafWindow reads every gathered leaf's cells plus the pending
// cell, appends each parent divider after its left page and sorts the result.
func (t *BTree) gatherIndexLeafWindow(parentPg *pager.Page, parent *storage.BTreePage, ptrBase int, g *indexBalanceGather, pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell, newCellData []byte) (*indexLeafWindow, bool, error) {
	w := &indexLeafWindow{
		parentPg: parentPg, parent: parent, parentCo: contentOffset(parentPg.PageNum),
		ptrBase: ptrBase, pages: g.pages, outPages: g.pages, gather: g,
		coff: make([]int, len(g.pages)), cntOld: make([]int, len(g.pages)),
		used: make([]int, len(g.pages)), oldDiv: make(map[*storage.Cell]bool),
	}
	w.outCo = w.coff
	for i, lp := range g.pages {
		lpCo := contentOffset(lp.PageNum)
		w.coff[i] = lpCo
		lpPage, err := storage.ParsePage(lp.Data, int(t.pageSize), lpCo)
		if err != nil {
			w.release()
			return nil, false, err
		}
		if lpPage.PageType != storage.PageTypeLeafIndex {
			w.release()
			return nil, false, nil
		}
		st := acquireSplitStaging()
		w.staging = append(w.staging, st)
		var cells []splitEntry
		if lp.PageNum == pg.PageNum {
			cells, err = t.readCellsForSplit(st, lp, page, lpCo, storage.CellIndexLeaf, newCell, newCellData)
		} else {
			cells, err = t.readCellsForSplit(st, lp, lpPage, lpCo, storage.CellIndexLeaf, nil, nil)
		}
		if err != nil {
			w.release()
			return nil, false, err
		}
		start := len(w.all)
		for _, e := range cells {
			w.all = append(w.all, e)
			w.sizes = append(w.sizes, t.leafEntryLen(e))
		}
		if i < len(g.boundaries) {
			payload, perr := t.indexBalancePayload(parentPg, ptrBase, g.boundaries[i])
			if perr != nil {
				w.release()
				return nil, false, perr
			}
			leafCell := &storage.Cell{Type: storage.CellIndexLeaf, PayloadLen: len(payload), Payload: payload}
			entry := splitEntry{leafCell, storage.AppendEncodedCell(nil, leafCell), payload}
			w.all = append(w.all, entry)
			w.sizes = append(w.sizes, t.leafPayloadLen(len(payload)))
			w.oldDiv[leafCell] = true
			w.oldPay = append(w.oldPay, payload)
		}
		w.cntOld[i] = len(w.all)
		w.used[i] = indexBalanceUsed(w.sizes, start, len(w.all))
	}
	sortSplitCells(w.all, false, t.compareKey)
	// sizes is a parallel array of the encoded entry lengths. The sort
	// permutes w.all, so rebuild sizes to keep each length with its entry;
	// otherwise the pack can undercount a page by a byte and write an
	// overfull leaf ("split failed: leaf half full").
	for i := range w.all {
		w.sizes[i] = t.leafEntryLen(w.all[i])
	}
	return w, true, nil
}

// leafEntryLen reports an existing leaf cell's encoded length. Cells carrying
// an overflow chain already have it; parent dividers are measured by
// leafPayloadLen so the pack sees the chain pointer they will need if they
// land on a page.
func (t *BTree) leafEntryLen(e splitEntry) int {
	if e.cell.Overflow != 0 {
		return len(e.cellData)
	}
	return t.leafPayloadLen(len(e.key))
}

// leafPayloadLen is an index leaf cell's encoded size for a payload of plen
// bytes (including the 4-byte overflow pointer when the payload spills).
func (t *BTree) leafPayloadLen(plen int) int {
	local := storage.LocalPayloadSize(plen, int(t.usableSize), storage.CellIndexLeaf)
	n := util.VarintLen(uint64(plen)) + local
	if local < plen {
		n += 4
	}
	return n
}

// release returns the gather's pooled cell buffers.
func (w *indexLeafWindow) release() {
	for _, st := range w.staging {
		releaseSplitStaging(st)
	}
	w.staging = nil
}

// planIndexLeafWindow computes cntNew and the room-checked parent probe
// without allocating a page or encoding a divider. A bail leaves
// w.parentPlan nil and every page untouched.
func (t *BTree) planIndexLeafWindow(w *indexLeafWindow) {
	oldN := len(w.pages)
	cntNew, ok := packIndexBalance(w.sizes, w.cntOld, w.used, int(t.usableSize)-8, oldN+1, false)
	if !ok || len(cntNew) < oldN {
		return
	}
	k := len(cntNew)
	n := len(w.all)
	for p := 0; p < k; p++ {
		if _, _, boundary, valid := indexBalanceRun(cntNew, p, n); !valid || (p < k-1 && boundary < 0) {
			return
		}
	}
	anchor := w.gather.anchorIdx
	if anchor < 0 || anchor >= oldN {
		return
	}
	boundaries := make([]indexBalanceParentBoundary, k-1)
	for i := range boundaries {
		boundaries[i].payload = w.all[cntNew[i]].key
	}
	probe := t.newIndexBalanceParentPlan(w.parentPg, w.parent, w.parentCo, w.ptrBase, w.gather, boundaries, 0)
	if probe == nil {
		return
	}
	// Do not bail on a full parent here: the cascade planner owns the room
	// check and rebalances this parent one level up when it does not fit.
	w.parentPlan = probe
	w.cntNew = cntNew
	w.needsExtra = k > oldN
}

// prepareLeafWindowPages allocates the extra output page (when needed) and
// resolves every new boundary's left child and the trailing rightmost. Runs
// only after the whole cascade passed its room checks.
func prepareLeafWindowPages(t *BTree, w *indexLeafWindow) error {
	if w.needsExtra {
		anchor := w.gather.anchorIdx
		pages := append([]*pager.Page(nil), w.pages...)
		cos := append([]int(nil), w.coff...)
		for e := 0; e < len(w.cntNew)-len(w.pages); e++ {
			np, err := t.allocBtreeNode(w.parentPg.PageNum)
			if err != nil {
				return err
			}
			npCo := contentOffset(np.PageNum)
			np.Data[npCo] = w.pages[anchor].Data[w.coff[anchor]]
			at := anchor + 1 + e
			pages = append(pages[:at], append([]*pager.Page{np}, pages[at:]...)...)
			cos = append(cos[:at], append([]int{npCo}, cos[at:]...)...)
		}
		w.outPages, w.outCo = pages, cos
	}
	for i := range w.parentPlan.boundaries {
		w.parentPlan.boundaries[i].leftChild = w.outPages[i].PageNum
	}
	w.parentPlan.rightmost = w.outPages[len(w.cntNew)-1].PageNum
	return nil
}

// freeConsumedLeafDividerChains releases the overflow pages of cells that
// become parent boundaries, after planning copied their payloads and before
// the replacement dividers allocate new chains. The freed pages are then
// reclaimed by those allocations, matching SQLite's in-place chain inheritance
// without leaving a residue on the freelist.
func (t *BTree) freeConsumedLeafDividerChains(w *indexLeafWindow) error {
	for p := 0; p < len(w.cntNew)-1; p++ {
		e := &w.all[w.cntNew[p]]
		if e.cell.Overflow == 0 {
			continue
		}
		if err := t.freeOverflowChain(e.cell.Overflow); err != nil {
			return err
		}
		e.cell.Overflow = 0
	}
	return nil
}

// writeIndexLeafPages writes every output leaf page with its cntNew run. The
// consumed dividers' chains were already released before the parent was
// rebuilt. It does not touch the parent — the caller commits the topmost
// parent plan.
func (t *BTree) writeIndexLeafPages(w *indexLeafWindow) error {
	if err := t.prepareLeafDividerChains(w); err != nil {
		return err
	}
	for p, lp := range w.outPages {
		start, end, _, _ := indexBalanceRun(w.cntNew, p, len(w.all))
		if err := t.writeSpillLeafHalf(lp, w.outCo[p], w.all[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// prepareLeafDividerChains writes the overflow chain (when needed) for every
// old parent divider that remains a regular cell after the redistribution.
func (t *BTree) prepareLeafDividerChains(w *indexLeafWindow) error {
	consumed := make(map[int]bool, len(w.cntNew))
	for p := 0; p < len(w.cntNew)-1; p++ {
		consumed[w.cntNew[p]] = true
	}
	for idx := range w.all {
		e := &w.all[idx]
		if !w.oldDiv[e.cell] || consumed[idx] {
			continue
		}
		target := -1
		for p := range w.outPages {
			start, end, _, _ := indexBalanceRun(w.cntNew, p, len(w.all))
			if idx >= start && idx < end {
				target = p
				break
			}
		}
		if target < 0 {
			continue
		}
		if err := t.prepareCell(e.cell, w.outPages[target].PageNum); err != nil {
			return err
		}
		e.cellData = storage.AppendEncodedCell(nil, e.cell)
	}
	return nil
}
