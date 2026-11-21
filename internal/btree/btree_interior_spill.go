// The interior spill: an anchor interior page that cannot take a child
// split's divider redistributes the [left sibling, anchor] window — plus a
// freshly allocated page when the window itself overflows — instead of
// tail-splitting (balance_nonroot's redistribution one level up, the
// interior twin of the leaf spill). The tail-split fallback (a fresh
// ~1-cell sibling per firing) is where sparse interiors come from; the
// spill packs the window's pages evenly and re-keys the parent's boundary
// divider, inserting one new divider cell for the created page. Every bail
// path returns before any page write (the parent-side room check predicts
// the encoded sizes with dividerCellLen), so a bail means the tree is
// untouched.

package btree

import (
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// spillInteriorWindow holds the gathered [left, anchor] window: both pages'
// interior entries merged with the pending divider (the anchor child's cell
// re-keyed — or, for a rightmost-anchored divider, appended as a new cell),
// sorted and partitioned into page runs.
type spillInteriorWindow struct {
	parentPg  *pager.Page
	parent    *storage.BTreePage
	parentCo  int
	ptrBase   int
	k         int // the boundary divider's cell index in the parent
	divOff    int // the boundary cell's byte offset in the parent
	leftPg    *pager.Page
	leftPgno  uint32
	leftCo    int
	leftPage  *storage.BTreePage
	pg        *pager.Page // the anchor
	page      *storage.BTreePage
	merged    []interiorEntry
	sizes     []int       // the encoded divider-cell size per merged entry
	runs      [][]int     // half-open index runs: runs[0] → the left page; run pi>0's first entry is the divider consumed into the parent
	rightmost uint32      // the right page's final rightmost pointer (the anchor's own, or the split's new page for a rightmost-anchored divider)
	newPg     *pager.Page // the created middle page (the 3-run window)
	rekeyed   []byte      // the parent's re-keyed boundary divider
	inserted  []byte      // the parent's new divider cell for the middle page (the 3-run window)
}

// spillInteriorPrelude runs the interior spill before the tail-split
// fallback: C's balance_nonroot redistributes the [left, anchor] window
// instead of manufacturing a fresh ~1-cell sibling — the spill is where the
// interior density comes from. INDEX trees only; every bail leaves the tree
// untouched and the tail-split loop runs.
func (t *BTree) spillInteriorPrelude(pg *pager.Page, parentPgno, childPageNum uint32, childSplits []leafSplitResult) (bool, error) {
	if t.isTable {
		return false, nil
	}
	anchorPage, perr := pg.ParsedBTree(int(t.pageSize), contentOffset(pg.PageNum))
	if perr != nil {
		return false, perr
	}
	return t.spillInteriorDividersLeft(pg, anchorPage, parentPgno, childPageNum, childSplits)
}

// spillInteriorDividersLeft offers the anchor page's pending divider (one
// per leg — applyChildSplitChain applies the chain divider-by-divider) to
// the anchor's LEFT sibling through the balance window. INDEX trees only —
// the table trees already match C's interior shape byte-for-byte and keep
// the tail split. Returns placed=true when the divider landed; every bail
// (no left sibling, a non-index window, a window needing more than three
// pages, a parent that cannot take the re-key + insert) leaves the tree
// untouched and the tail-split fallback runs.
func (t *BTree) spillInteriorDividersLeft(pg *pager.Page, page *storage.BTreePage, parentPgno, childPageNum uint32, childSplits []leafSplitResult) (bool, error) {
	if len(childSplits) != 1 {
		return false, nil // the chain applies one divider per leg
	}
	// bulk = the anchor's parent is the tree root (C's bBulk=(iPage==1)):
	// the chain runs inside the anchor's insertPage frame, whose insDepth
	// is the anchor's depth (the child recursion's defers restored it).
	bulk := t.insDepth == 2
	w, ok, err := t.gatherSpillInteriorWindow(pg, page, parentPgno, childPageNum, childSplits[0])
	if !ok || err != nil {
		return false, err
	}
	if ok, err = t.pickSpillInteriorRuns(w, bulk); !ok || err != nil {
		return false, err
	}
	if ok, err = t.precheckSpillInteriorRekey(w); !ok || err != nil {
		return false, err
	}
	return true, t.commitSpillInteriorWindow(w)
}

// gatherSpillInteriorWindow assembles the [left, anchor] window: both pages'
// entries are collected, the pending divider is folded in (the anchor
// child's cell re-keyed to the split's median plus the carrier sibling
// entry carrying the anchor child's original divider — applyChildSplits'
// (C, Kold) → (C, D1), (P1, Kold) transformation; for a rightmost-anchored
// divider C itself becomes the new cell and the split's page takes the
// rightmost pointer), and the merge is sorted. Returns ok=false when the
// window does not apply (a root anchor, non-index parent or sibling, the
// anchor child absent).
func (t *BTree) gatherSpillInteriorWindow(pg *pager.Page, page *storage.BTreePage, parentPgno, childPageNum uint32, cs leafSplitResult) (*spillInteriorWindow, bool, error) {
	parentPg, parent, ptrBase, k, divOff, divFull, ok, err := t.readSpillInteriorParent(parentPgno, pg.PageNum)
	if !ok || err != nil {
		return nil, false, err
	}
	leftPg, leftPage, leftCo, ok, err := t.readSpillInteriorSibling(parentPg, ptrBase, k)
	if !ok || err != nil {
		return nil, false, err
	}

	pgCo := contentOffset(pg.PageNum)
	// CellPointer adds 8 to the given offset (see splitInteriorPage): pass
	// the pointer-array base minus 8 so the collect reads coff+ptroff+i*2.
	anchorEntries := t.collectInteriorEntries(pg, pgCo+cellPtrOffset(page.PageType)-8, page)
	leftEntries := t.collectInteriorEntries(leftPg, leftCo+cellPtrOffset(leftPage.PageType)-8, leftPage)

	// The parent's OLD boundary divider joins the cell array — C's
	// balance_nonroot drops the dividers into the array and re-places them
	// (src/btree.c:8451-8482), with the divider's left pointer REPLACED by
	// the left page's rightmost child (src/btree.c:8464-8469): the entry
	// represents the subtree between the left page's last cell and the
	// divider key, so it sorts between the two siblings' entries and the
	// old divider's key survives as a regular entry wherever the partition
	// puts it. Without it the re-keyed boundary would DELETE the old
	// divider's entry from the tree ("row N missing from index").
	boundary := interiorEntry{
		leftChild: leftPage.RightmostPtr,
		key:       0,
		payload:   append([]byte(nil), divFull.Payload...),
	}
	pending, rightmost, ok := t.foldPendingDivider(anchorEntries, page, childPageNum, cs)
	if !ok {
		return nil, false, nil // the anchor child is not this page's cell
	}

	merged := make([]interiorEntry, 0, len(leftEntries)+1+len(anchorEntries)+len(pending))
	merged = append(merged, leftEntries...)
	merged = append(merged, boundary)
	merged = append(merged, anchorEntries...)
	merged = append(merged, pending...)
	sort.SliceStable(merged, func(i, j int) bool {
		return t.compareKey(merged[i].payload, merged[j].payload) < 0
	})
	sizes := make([]int, len(merged))
	for i := range merged {
		sizes[i] = t.dividerCellLen(merged[i].splitResult(t.isTable))
	}

	w := &spillInteriorWindow{
		parentPg: parentPg, parent: parent, parentCo: contentOffset(parentPg.PageNum), ptrBase: ptrBase,
		k: k, divOff: divOff, leftPg: leftPg, leftPgno: leftPg.PageNum, leftCo: leftCo,
		leftPage: leftPage, pg: pg, page: page, merged: merged, sizes: sizes,
		rightmost: rightmost,
	}
	return w, true, nil
}

// readSpillInteriorParent reads and parses the anchor's parent and locates
// the anchor's boundary: the cell index k whose left child is the anchor
// (parent.CellCount when the anchor is the rightmost pointer; k<=0 when the
// anchor has no LEFT sibling) and the boundary divider's full payload.
// Returns ok=false when the window does not apply (a root anchor, a
// non-index parent, no left sibling).
func (t *BTree) readSpillInteriorParent(parentPgno, anchorPgno uint32) (*pager.Page, *storage.BTreePage, int, int, int, *storage.Cell, bool, error) {
	if parentPgno == 0 {
		return nil, nil, 0, 0, 0, nil, false, nil // the root anchor: balance_deeper's case
	}
	parentPg, err := t.pager.ReadPage(parentPgno)
	if err != nil {
		return nil, nil, 0, 0, 0, nil, false, err
	}
	parentCo := contentOffset(parentPg.PageNum)
	parent, err := storage.ParsePage(parentPg.Data, int(t.pageSize), parentCo)
	if err != nil {
		return nil, nil, 0, 0, 0, nil, false, err
	}
	if parent.PageType != storage.PageTypeInteriorIndex {
		return nil, nil, 0, 0, 0, nil, false, nil
	}
	ptrBase := parentCo + cellPtrOffset(parent.PageType)
	k := locateSpillAnchorCell(parentPg, parent, parentCo, ptrBase, anchorPgno)
	if k <= 0 {
		return nil, nil, 0, 0, 0, nil, false, nil // no left sibling
	}
	divOff := int(binary.BigEndian.Uint16(parentPg.Data[ptrBase+(k-1)*2 : ptrBase+(k-1)*2+2]))
	divCell, err := storage.DecodeCell(parentPg.Data, divOff, storage.CellIndexInterior, int(t.usableSize))
	if err != nil {
		return nil, nil, 0, 0, 0, nil, false, err
	}
	divFull, err := t.readOverflow(divCell)
	if err != nil {
		return nil, nil, 0, 0, 0, nil, false, err
	}
	return parentPg, parent, ptrBase, k, divOff, divFull, true, nil
}

// readSpillInteriorSibling reads the anchor's left sibling (the parent's
// cell at k-1's left child). Returns ok=false when the sibling is not an
// index interior (the spill does not apply across mixed page types).
func (t *BTree) readSpillInteriorSibling(parentPg *pager.Page, ptrBase, k int) (*pager.Page, *storage.BTreePage, int, bool, error) {
	lcp := int(binary.BigEndian.Uint16(parentPg.Data[ptrBase+(k-1)*2 : ptrBase+(k-1)*2+2]))
	leftPgno := binary.BigEndian.Uint32(parentPg.Data[lcp : lcp+4])
	leftPg, err := t.pager.ReadPage(leftPgno)
	if err != nil {
		return nil, nil, 0, false, err
	}
	leftCo := contentOffset(leftPg.PageNum)
	leftPage, err := storage.ParsePage(leftPg.Data, int(t.pageSize), leftCo)
	if err != nil {
		return nil, nil, 0, false, err
	}
	if leftPage.PageType != storage.PageTypeInteriorIndex {
		return nil, nil, 0, false, nil
	}
	return leftPg, leftPage, leftCo, true, nil
}

// foldPendingDivider folds the leg's pending divider into the anchor's
// entries: a cell-anchored divider re-keys the anchor child's cell to the
// split's median and appends the carrier sibling entry carrying the anchor
// child's ORIGINAL divider (the upper bound moves with the last new page,
// applyChildSplits' Kold); a rightmost-anchored divider (C's end-insert)
// turns the anchor child into a regular cell carrying the split's median
// and hands the rightmost pointer to the split's new page. Returns
// ok=false when the anchor child is neither a cell nor the rightmost
// pointer.
func (t *BTree) foldPendingDivider(anchorEntries []interiorEntry, page *storage.BTreePage, childPageNum uint32, cs leafSplitResult) ([]interiorEntry, uint32, bool) {
	rightmost := page.RightmostPtr
	for i := range anchorEntries {
		if anchorEntries[i].leftChild != childPageNum {
			continue
		}
		carrier := anchorEntries[i]
		anchorEntries[i].key = cs.medianKey
		anchorEntries[i].payload = cs.medianPayload
		return []interiorEntry{{leftChild: cs.pageNum, key: carrier.key, payload: carrier.payload}}, rightmost, true
	}
	if page.RightmostPtr == childPageNum {
		return []interiorEntry{{leftChild: childPageNum, key: cs.medianKey, payload: cs.medianPayload}}, cs.pageNum, true
	}
	return nil, rightmost, false
}

// interiorHeaderSize is an interior page's fixed overhead: the 8-byte
// b-tree header plus the 4-byte rightmost pointer.
const interiorHeaderSize = 12

// partitionInteriorRuns greedily partitions the sorted entries into page
// runs: each run takes the longest prefix that fits (the consumed-divider
// convention — run pi>0's first entry moves up into the parent and occupies
// no page space). Index interiors never live on page 1, so the content
// offset is 0.
func partitionInteriorRuns(sizes []int, usable int) [][]int {
	var runs [][]int
	start, n, bytes := 0, 0, 0
	for i, sz := range sizes {
		if n > 0 && usable-bytes-sz < interiorHeaderSize+(n+1)*2+2 {
			runs = append(runs, []int{start, i})
			start, n, bytes = i, 0, 0
		}
		n++
		bytes += sz
	}
	if n > 0 {
		runs = append(runs, []int{start, len(sizes)})
	}
	return runs
}

// interiorRunFullUsed computes a run's FULL page footprint: content + 2
// bytes per cell pointer + the 12-byte header, where run pi>0's first entry
// is the divider consumed into the parent.
func interiorRunFullUsed(sizes []int, runs [][]int, pi int) int {
	s, e := runs[pi][0], runs[pi][1]
	first := 0
	if pi > 0 {
		first = 1
	}
	n, bytes := 0, 0
	for idx := s + first; idx < e; idx++ {
		n++
		bytes += sizes[idx]
	}
	if n == 0 {
		return 0
	}
	return bytes + 2*n + interiorHeaderSize
}

// rotateInteriorRuns moves run pi-1's last entry onto run pi's front: the
// moved entry becomes run pi's divider and the old divider stays with the
// page as its first regular cell. The runs are half-open [start, end) pairs
// over a contiguous partition, so the moved index is always exactly the
// left run's new end (== the right run's start - 1).
func rotateInteriorRuns(runs [][]int, pi int) {
	runs[pi-1][1]--
	runs[pi][0] = runs[pi-1][1]
}

// rotateInteriorRunBoundaries runs balance_nonroot's right-to-left
// adjustment over the partition: for each pair of neighbors (right to
// left), entries slide from the left run into the right while the right
// stays no fuller than the left's slack; bulk — the anchor directly under
// the root — moves regardless while the right page is EMPTY.
func rotateInteriorRunBoundaries(sizes []int, runs [][]int, bulk bool) {
	for i := len(runs) - 1; i >= 1; i-- {
		szRight := interiorRunFullUsed(sizes, runs, i)
		szLeft := interiorRunFullUsed(sizes, runs, i-1)
		reserve := 0
		if i != len(runs)-1 {
			reserve = 2
		}
		for runs[i-1][1] > runs[i-1][0] {
			szR := sizes[runs[i-1][1]-1]
			szD := sizes[runs[i][0]]
			if szRight != 0 && (bulk || szRight+szD+2 > szLeft-szR-reserve) {
				break
			}
			rotateInteriorRuns(runs, i)
			szRight += szD + 2
			szLeft -= szR + 2
		}
	}
}

// pickSpillInteriorRuns partitions the window into 2 or 3 page runs: the
// greedy pack, then balance_nonroot's right-to-left adjustment (entries
// slide leftward into the left page while the right stays no fuller than
// the left's slack; bulk — the anchor directly under the root — moves
// regardless while the right page is EMPTY), then the guards: 2 or 3 runs,
// every page holding at least one regular cell, and the last run's page
// fitting. Returns ok=false (nothing changed) otherwise.
func (t *BTree) pickSpillInteriorRuns(w *spillInteriorWindow, bulk bool) (bool, error) {
	runs := partitionInteriorRuns(w.sizes, int(t.usableSize))
	if len(runs) < 2 {
		return false, nil
	}
	rotateInteriorRunBoundaries(w.sizes, runs, bulk)
	if len(runs) < 2 || len(runs) > 3 {
		return false, nil
	}
	if runs[0][1] == runs[0][0] {
		return false, nil
	}
	for pi := 1; pi < len(runs); pi++ {
		if runs[pi][1]-runs[pi][0] < 2 {
			return false, nil // a page would hold only its consumed divider
		}
	}
	if interiorRunFullUsed(w.sizes, runs, len(runs)-1) > int(t.usableSize) {
		return false, nil // the window needs more than three pages
	}
	w.runs = runs
	return true, nil
}

// precheckSpillInteriorRekey runs the parent-side room check BEFORE any page write:
// the boundary divider (run 1's first entry) re-keys the parent's boundary
// cell, and the 3-run window additionally inserts one new divider cell for
// the middle page. The sizes are predicted with dividerCellLen (encoding
// first would leak the fresh overflow chains on the bail). The middle page
// is allocated here — the encodes need its page number, and the allocation
// rolls back with the statement on any later failure.
func (t *BTree) precheckSpillInteriorRekey(w *spillInteriorWindow) (bool, error) {
	need := t.dividerCellLen(w.merged[w.runs[1][0]].splitResult(t.isTable))
	extraCells := 0
	if len(w.runs) == 3 {
		extraCells = 1
		need += t.dividerCellLen(w.merged[w.runs[2][0]].splitResult(t.isTable))
	}
	rekeyPtrEnd := w.ptrBase + (int(w.parent.CellCount)+extraCells)*2 + 2
	if int(w.parent.CellContent)-need < rekeyPtrEnd {
		return false, nil // the parent cannot take the re-key (+ insert)
	}
	if len(w.runs) == 3 {
		newPg, aerr := t.allocBtreeNode(w.parentPg.PageNum)
		if aerr != nil {
			return false, aerr
		}
		w.newPg = newPg
		newCo := contentOffset(newPg.PageNum)
		for i := newCo; i < int(t.pageSize); i++ {
			newPg.Data[i] = 0
		}
		newPg.Data[newCo] = w.page.PageType
		inserted, eerr := t.encodeDividerCell(newPg.PageNum, w.merged[w.runs[2][0]].splitResult(t.isTable), w.parentPg.PageNum)
		if eerr != nil {
			return false, eerr
		}
		w.inserted = inserted
	}
	rekeyed, eerr := t.encodeDividerCell(w.leftPgno, w.merged[w.runs[1][0]].splitResult(t.isTable), w.parentPg.PageNum)
	if eerr != nil {
		return false, eerr
	}
	w.rekeyed = rekeyed
	return true, nil
}

// reparentInteriorChildren re-points the ptrmap entries of the given
// entries' children (and the extra child, when non-zero) at owner.
func (t *BTree) reparentInteriorChildren(entries []interiorEntry, owner uint32, extra uint32) error {
	if !t.ptrmapEnabled() {
		return nil
	}
	for i := range entries {
		if err := t.pager.WritePtrmap(entries[i].leftChild, storage.PtrmapBtree, owner); err != nil {
			return err
		}
	}
	if extra != 0 {
		return t.pager.WritePtrmap(extra, storage.PtrmapBtree, owner)
	}
	return nil
}

// writeInteriorCells writes an interior page's cells (a half-open slice of
// the merged entries) packing from the usable end downward, and sets the
// header's cell count, content end and rightmost pointer.
func (t *BTree) writeInteriorCells(pg *pager.Page, co, ptroff int, entries []interiorEntry, rightmost uint32) error {
	end := int(t.usableSize)
	count := 0
	for i := range entries {
		cellData, eerr := t.encodeDividerCell(entries[i].leftChild, entries[i].splitResult(t.isTable), pg.PageNum)
		if eerr != nil {
			return eerr
		}
		ptrEnd := co + ptroff + (count+1)*2 + 2
		start := end - len(cellData)
		if start < ptrEnd {
			return fmt.Errorf("btree: interior spill: page overflow")
		}
		copy(pg.Data[start:], cellData)
		binary.BigEndian.PutUint16(pg.Data[co+ptroff+count*2:], uint16(start))
		end = start
		count++
	}
	binary.BigEndian.PutUint16(pg.Data[co+3:co+5], uint16(count))
	binary.BigEndian.PutUint16(pg.Data[co+5:co+7], uint16(end))
	binary.BigEndian.PutUint32(pg.Data[co+8:co+12], rightmost)
	return nil
}

// commitSpillInteriorWindow applies the redistribution: both window pages'
// old divider chains die (the rewrite re-encodes every entry with a fresh
// chain owned by its target page), the window's pages are rewritten with
// their runs, and the parent's boundary divider is re-keyed (plus the new
// divider cell inserted for the 3-run window) — the room check already ran
// in precheckSpillInteriorRekey.
func (t *BTree) commitSpillInteriorWindow(w *spillInteriorWindow) error {
	if err := t.freeInteriorDividerChains(w.leftPg, w.leftPage); err != nil {
		return err
	}
	if err := t.freeInteriorDividerChains(w.pg, w.page); err != nil {
		return err
	}

	ptroff := cellPtrOffset(w.page.PageType)

	// The left page: run 0's entries, rightmost = the consumed divider's
	// left child.
	leftEntries := w.merged[w.runs[0][0]:w.runs[0][1]]
	if err := t.writeSpillInteriorPage(w.leftPg, w.leftCo, ptroff, leftEntries, w.merged[w.runs[1][0]].leftChild); err != nil {
		return err
	}

	// The middle page (the 3-run window): run 1 minus its consumed divider.
	if w.newPg != nil {
		newCo := contentOffset(w.newPg.PageNum)
		midEntries := w.merged[w.runs[1][0]+1 : w.runs[1][1]]
		if err := t.writeSpillInteriorPage(w.newPg, newCo, ptroff, midEntries, w.merged[w.runs[2][0]].leftChild); err != nil {
			return err
		}
	}

	// The anchor page: the last run minus its consumed divider, with the
	// window's final rightmost pointer.
	last := w.runs[len(w.runs)-1]
	rightEntries := w.merged[last[0]+1 : last[1]]
	if err := t.writeSpillInteriorPage(w.pg, contentOffset(w.pg.PageNum), ptroff, rightEntries, w.rightmost); err != nil {
		return err
	}
	return t.finishSpillInteriorRekey(w)
}

// writeSpillInteriorPage rewrites one window page with its entries: the
// write-intent barrier, the content clear, the cell write, the moved
// entries' child re-parenting (extraChild is the page's new rightmost
// pointer) and the persist.
func (t *BTree) writeSpillInteriorPage(pg *pager.Page, co, ptroff int, entries []interiorEntry, extraChild uint32) error {
	t.pager.PrepareWrite(pg)
	for i := co + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}
	if err := t.writeInteriorCells(pg, co, ptroff, entries, extraChild); err != nil {
		return err
	}
	if err := t.reparentInteriorChildren(entries, pg.PageNum, extraChild); err != nil {
		return err
	}
	return t.pager.WritePage(pg)
}

// finishSpillInteriorRekey re-keys the parent's boundary divider with the window's
// boundary entry and — for the 3-run window — inserts the middle page's
// divider cell after it, then repacks the abandoned divider bytes and
// persists the parent.
func (t *BTree) finishSpillInteriorRekey(w *spillInteriorWindow) error {
	t.pager.PrepareWrite(w.parentPg)
	if _, aerr := t.abandonDividerCell(w.parentPg, w.divOff); aerr != nil {
		return aerr
	}
	extraCells := 0
	if len(w.runs) == 3 {
		extraCells = 1
	}
	rekeyPtrEnd := w.ptrBase + (int(w.parent.CellCount)+extraCells)*2 + 2
	rkStart := int(w.parent.CellContent) - len(w.rekeyed)
	if rkStart < rekeyPtrEnd {
		return fmt.Errorf("btree: interior spill re-key: parent %d full", w.parentPg.PageNum)
	}
	copy(w.parentPg.Data[rkStart:], w.rekeyed)
	binary.BigEndian.PutUint16(w.parentPg.Data[w.ptrBase+(w.k-1)*2:], uint16(rkStart))
	binary.BigEndian.PutUint16(w.parentPg.Data[w.parentCo+5:w.parentCo+7], uint16(rkStart))
	w.parent.CellContent = rkStart
	if w.inserted != nil {
		ncStart := int(w.parent.CellContent) - len(w.inserted)
		nCount := int(w.parent.CellCount) + 1
		copy(w.parentPg.Data[ncStart:], w.inserted)
		shiftCellPtrsRight(w.parentPg.Data, w.ptrBase, w.k, int(w.parent.CellCount))
		binary.BigEndian.PutUint16(w.parentPg.Data[w.ptrBase+w.k*2:], uint16(ncStart))
		w.parent.CellCount = uint16(nCount)
		binary.BigEndian.PutUint16(w.parentPg.Data[w.parentCo+3:w.parentCo+5], uint16(nCount))
		binary.BigEndian.PutUint16(w.parentPg.Data[w.parentCo+5:w.parentCo+7], uint16(ncStart))
		w.parent.CellContent = ncStart
	}
	if err := t.defragmentInterior(w.parentPg, w.parent); err != nil {
		return err
	}
	pp2, err := storage.ParsePageInto(w.parentPg.Data, int(t.pageSize), w.parentCo, &storage.BTreePage{})
	if err != nil {
		return err
	}
	w.parentPg.RefreshParsedBTree(int(t.pageSize), w.parentCo, pp2)
	return t.pager.WritePage(w.parentPg)
}
