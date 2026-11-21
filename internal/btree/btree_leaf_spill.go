// The leaf spill: the overflowing leaf page's cell set -- plus the new
// cell -- offered to the LEFT sibling through the 2-page balance window
// (balance_nonroot's redistribution for the [left, page] gather). This is
// where sqlite's index density comes from: the excess spills into the
// sibling's free space and a new page is only needed when the window itself
// overflows. Every bail path returns before any page write (the re-key
// precheck is hoisted), so a bail means the tree is untouched.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// spillIndexLeafPrelude runs the leaf spill for INDEX btrees before the
// fall-through split: C's balance window — for INDEX btrees the excess
// cells spill into the LEFT sibling's free space before any new page is
// allocated (the density mechanism). The spill absorbs the cell set when
// the window fits two pages. Every bail path returns before any page write
// (the re-key precheck is hoisted), so a placed=false return means the tree
// is untouched and the fall-through split re-reads the page with the new
// cell as-is.
func (t *BTree) spillIndexLeafPrelude(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell, newCellData []byte, bulk bool) (bool, error) {
	if t.isTable || parentPgno == 0 {
		return false, nil
	}
	placed, _, _, err := t.spillIndexLeafLeft(pg, page, parentPgno, newCell, newCellData, bulk)
	return placed, err
}

// spillLeafWindow holds the gathered 2-page balance window: the left
// sibling's entries plus the parent's boundary divider (re-encoded in leaf
// form) plus the overflowing page's entries, sorted; with the parse state
// needed to commit the redistribution.
type spillLeafWindow struct {
	parentPg *pager.Page
	parent   *storage.BTreePage
	parentCo int
	ptrBase  int
	k        int // the boundary divider's cell index in the parent
	divOff   int // the boundary cell's byte offset in the parent
	leftPg   *pager.Page
	leftPgno uint32
	leftCo   int
	pg       *pager.Page
	all      []splitEntry // sorted: left entries + divider entry + right entries
	newIdx   int          // the new cell's index in all (-1 when absent)
	divIdx   int          // the divider entry's index in all
	boundary int          // the split point: all[:boundary] | all[boundary] | all[boundary+1:]
	rekeyed  []byte       // the parent's re-keyed divider cell (hoisted precheck)
}

// spillIndexLeafLeft offers the overflowing page's cell set -- plus the new
// cell -- to the LEFT sibling through the 2-page balance window
// (balance_nonroot's redistribution for the [left, page] gather). The spill
// is where sqlite's index density comes from: the excess spills into the
// sibling's free space and a new page is only needed when the window itself
// overflows. Returns (placed, movedNew, moved, err).
//
// The driver runs the phases in order; every bail before the commit phase
// leaves the tree untouched.
func (t *BTree) spillIndexLeafLeft(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell, newCellData []byte, bulk bool) (placed, movedNew, moved bool, err error) {
	w, lst, rst, ok, err := t.gatherSpillLeafWindow(pg, page, parentPgno, newCell, newCellData)
	if !ok || err != nil {
		return false, false, false, err
	}
	defer releaseSplitStaging(lst)
	defer releaseSplitStaging(rst)
	if ok, err = t.pickSpillLeafBoundary(w, bulk); !ok || err != nil {
		return false, false, false, err
	}
	if ok, err = t.precheckSpillRekey(w); !ok || err != nil {
		return false, false, false, err
	}
	if err := t.commitSpillLeafWindow(w); err != nil {
		return false, false, false, err
	}
	if w.newIdx >= 0 {
		movedNew = w.newIdx < w.boundary
		moved = movedNew || w.newIdx > w.boundary
	} else {
		moved = true
	}
	return true, movedNew, moved, nil
}

// locateSpillAnchorCell returns the parent cell index whose left child is
// the overflowing page (parent.CellCount when the page is the rightmost
// pointer). Returns k<=0 when the page has no LEFT sibling — the spill
// does not apply.
func locateSpillAnchorCell(parentPg *pager.Page, parent *storage.BTreePage, parentCo, ptrBase int, childPgno uint32) int {
	k := -1
	for i := 0; i < int(parent.CellCount); i++ {
		cp := int(binary.BigEndian.Uint16(parentPg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		if binary.BigEndian.Uint32(parentPg.Data[cp:cp+4]) == childPgno {
			k = i
			break
		}
	}
	if k < 0 && binary.BigEndian.Uint32(parentPg.Data[parentCo+8:parentCo+12]) == childPgno {
		k = int(parent.CellCount)
	}
	return k
}

// readSpillLeftSibling reads the anchor's left sibling and the boundary
// divider cell between the two (the parent's cell at k-1). Returns
// ok=false when the sibling is not an index leaf (the spill does not
// apply across mixed page types).
func (t *BTree) readSpillLeftSibling(parentPg *pager.Page, ptrBase, k int) (leftPg *pager.Page, leftPage *storage.BTreePage, leftCo int, divCell *storage.Cell, divOff int, ok bool, err error) {
	lcp := int(binary.BigEndian.Uint16(parentPg.Data[ptrBase+(k-1)*2 : ptrBase+(k-1)*2+2]))
	leftPgno := binary.BigEndian.Uint32(parentPg.Data[lcp : lcp+4])
	leftPg, err = t.pager.ReadPage(leftPgno)
	if err != nil {
		return nil, nil, 0, nil, 0, false, err
	}
	leftCo = contentOffset(leftPg.PageNum)
	leftPage, err = storage.ParsePage(leftPg.Data, int(t.pageSize), leftCo)
	if err != nil {
		return nil, nil, 0, nil, 0, false, err
	}
	if leftPage.PageType != storage.PageTypeLeafIndex {
		return nil, nil, 0, nil, 0, false, nil
	}
	divOff = int(binary.BigEndian.Uint16(parentPg.Data[ptrBase+(k-1)*2 : ptrBase+(k-1)*2+2]))
	divCell, err = storage.DecodeCell(parentPg.Data, divOff, storage.CellIndexInterior, int(t.usableSize))
	if err != nil {
		return nil, nil, 0, nil, 0, false, err
	}
	return leftPg, leftPage, leftCo, divCell, divOff, true, nil
}

// gatherSpillEntries reads the window's cell sets into their stagings: the
// left sibling's entries, the boundary divider re-encoded in leaf form (NO
// chain — its payload needs the chain written for whichever page it finally
// lands on), and the overflowing page's entries with the new cell. The
// merged, sorted entry list is returned with both stagings (their buffers
// back the entries' byte views).
func (t *BTree) gatherSpillEntries(leftPg *pager.Page, leftPage *storage.BTreePage, leftCo int, divCell *storage.Cell, pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell, newCellData []byte) (all []splitEntry, leafForm *storage.Cell, lst, rst *splitStaging, err error) {
	lst = acquireSplitStaging()
	leftCells, err := t.readCellsForSplit(lst, leftPg, leftPage, leftCo, storage.CellIndexLeaf, nil, nil)
	if err != nil {
		releaseSplitStaging(lst)
		return nil, nil, nil, nil, err
	}
	divFull, oerr := t.readOverflow(divCell)
	if oerr != nil {
		releaseSplitStaging(lst)
		return nil, nil, nil, nil, oerr
	}
	leafForm = &storage.Cell{Type: storage.CellIndexLeaf, PayloadLen: divFull.PayloadLen, Payload: divFull.Payload}
	divEntry := splitEntry{leafForm, storage.AppendEncodedCell(nil, leafForm), divFull.Payload}

	rst = acquireSplitStaging()
	rightCells, err := t.readCellsForSplit(rst, pg, page, contentOffset(pg.PageNum), storage.CellIndexLeaf, newCell, newCellData)
	if err != nil {
		releaseSplitStaging(lst)
		releaseSplitStaging(rst)
		return nil, nil, nil, nil, err
	}
	all = make([]splitEntry, 0, len(leftCells)+1+len(rightCells))
	all = append(all, leftCells...)
	all = append(all, divEntry)
	all = append(all, rightCells...)
	sortSplitCells(all, false, t.compareKey)
	return all, leafForm, lst, rst, nil
}

// gatherSpillLeafWindow assembles the [left, page] window: the parent's
// boundary divider at k-1 is re-encoded in leaf form, the sibling entries
// and the overflowing page's entries (with the new cell) are gathered and
// sorted. Returns ok=false when the window does not apply (non-index
// parent, no left sibling, non-index-leaf sibling).
func (t *BTree) gatherSpillLeafWindow(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell, newCellData []byte) (*spillLeafWindow, *splitStaging, *splitStaging, bool, error) {
	parentPg, err := t.pager.ReadPage(parentPgno)
	if err != nil {
		return nil, nil, nil, false, err
	}
	parentCo := contentOffset(parentPg.PageNum)
	parent, err := storage.ParsePage(parentPg.Data, int(t.pageSize), parentCo)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if parent.PageType != storage.PageTypeInteriorIndex {
		return nil, nil, nil, false, nil
	}
	ptrBase := parentCo + cellPtrOffset(parent.PageType)
	k := locateSpillAnchorCell(parentPg, parent, parentCo, ptrBase, pg.PageNum)
	if k <= 0 {
		return nil, nil, nil, false, nil // no left sibling
	}

	leftPg, leftPage, leftCo, divCell, divOff, ok, err := t.readSpillLeftSibling(parentPg, ptrBase, k)
	if !ok || err != nil {
		return nil, nil, nil, false, err
	}

	all, leafForm, lst, rst, err := t.gatherSpillEntries(leftPg, leftPage, leftCo, divCell, pg, page, newCell, newCellData)
	if err != nil {
		return nil, nil, nil, false, err
	}

	w := &spillLeafWindow{
		parentPg: parentPg, parent: parent, parentCo: parentCo, ptrBase: ptrBase,
		k: k, divOff: divOff, leftPg: leftPg, leftPgno: leftPg.PageNum, leftCo: leftCo,
		pg: pg, all: all, newIdx: -1, divIdx: -1,
	}
	w.newIdx, w.divIdx = indexSpillAnchorEntries(all, newCell, leafForm)
	return w, lst, rst, true, nil
}

// indexSpillAnchorEntries records the new cell's and the divider entry's
// positions in the sorted window (first match; -1 when absent).
func indexSpillAnchorEntries(all []splitEntry, newCell, leafForm *storage.Cell) (newIdx, divIdx int) {
	newIdx, divIdx = -1, -1
	for i := range all {
		if newIdx < 0 && all[i].cell == newCell {
			newIdx = i
		}
		if divIdx < 0 && all[i].cell == leafForm {
			divIdx = i
		}
	}
	return newIdx, divIdx
}

// pickSpillLeafBoundary computes the window's split point: the greedy pack
// over the sorted entries, then balance_nonroot's right-to-left adjustment
// (balanceIndexDividerPartitions' rotation), then the right-page fit guard.
// Returns ok=false (nothing changed) when the window cannot split legally.
func (t *BTree) pickSpillLeafBoundary(w *spillLeafWindow, bulk bool) (bool, error) {
	boundary := 0
	fill, n := 0, 0
	for i := 0; i < len(w.all); i++ {
		sz := len(w.all[i].cellData)
		if n > 0 && fill+sz > int(t.usableSize)-8-2*(n+1) {
			break
		}
		fill += sz
		n++
		boundary = i + 1
	}
	if boundary == 0 || boundary >= len(w.all) {
		return false, nil
	}
	szLeft, szRight := spillSideBytes(w.all, boundary)
	if !bulk {
		boundary = spillRotateBoundary(w.all, boundary, szLeft, szRight)
	}
	rightBytes := 0
	for i := boundary + 1; i < len(w.all); i++ {
		rightBytes += len(w.all[i].cellData)
	}
	if rightBytes+2*(len(w.all)-boundary-1)+8 > int(t.usableSize) {
		return false, nil
	}
	w.boundary = boundary
	return true, nil
}

// spillSideBytes computes the two pages' FULL footprints at the given split
// point: content bytes + 2 bytes per cell pointer + the 8-byte page header.
func spillSideBytes(all []splitEntry, boundary int) (szLeft, szRight int) {
	for i := 0; i < boundary; i++ {
		szLeft += len(all[i].cellData)
	}
	szLeft += 2*boundary + 8
	for i := boundary + 1; i < len(all); i++ {
		szRight += len(all[i].cellData)
	}
	return szLeft, szRight + 2*(len(all)-boundary-1) + 8
}

// spillRotateBoundary runs C's right-to-left adjustment pass (btree.c
// balance_nonroot's tail): entries slide leftward into the left page while
// the right page is EMPTY (C's szRight!=0 guard) or while the slide keeps
// the right page no fuller than the left page's slack.
func spillRotateBoundary(all []splitEntry, boundary, szLeft, szRight int) int {
	for boundary > 0 {
		szR := len(all[boundary-1].cellData)
		szD := len(all[boundary].cellData)
		if szRight != 0 && szRight+szD+2 > szLeft-szR {
			break
		}
		szLeft -= szR + 2
		szRight += szD + 2
		boundary--
	}
	return boundary
}

// precheckSpillRekey runs the parent re-key's room check BEFORE any page
// write: the boundary cell moves UP into the parent as the new divider, and
// a parent that cannot take the wider divider must bail with NOTHING
// changed -- a post-write bail would leave the pages redistributed but the
// parent's divider un-rekeyed, and the old divider's key would overlap a
// page's range (the re-read's duplicate-entry corruption).
//
// The check uses the encoded size predicted by dividerCellLen (the
// applyChildSplitsRightmost parity) instead of encoding first: encoding
// first would leak the fresh overflow chain on the bail, and freeing the
// boundary's leaf-form chain first would strand the still-live divider on
// the bail path — both only become safe once the room check has passed.
func (t *BTree) precheckSpillRekey(w *spillLeafWindow) (bool, error) {
	need := t.dividerCellLen(leafSplitResult{medianPayload: w.all[w.boundary].key})
	rekeyPtrEnd := w.ptrBase + int(w.parent.CellCount)*2 + 2
	if int(w.parent.CellContent)-need < rekeyPtrEnd {
		return false, nil // the parent cannot take the wider divider
	}
	rekeyed, eerr := t.encodeDividerCell(w.leftPgno, leafSplitResult{medianPayload: w.all[w.boundary].key}, w.parentPg.PageNum)
	if eerr != nil {
		return false, eerr
	}
	// The boundary entry moves UP as the parent's divider: its leaf-form
	// cell dies with the page rewrite and its chain dies with it (clearCell
	// parity) — freed here, on the commit path only.
	if w.all[w.boundary].cell.Overflow != 0 {
		if ferr := t.freeOverflowChain(w.all[w.boundary].cell.Overflow); ferr != nil {
			return false, ferr
		}
	}
	w.rekeyed = rekeyed
	return true, nil
}

// commitSpillLeafWindow applies the redistribution: the divider entry's
// chain is prepared for whichever page it lands on, the two leaf pages are
// rewritten with their halves, and the parent's boundary divider is
// re-keyed (the encode + room check already ran in precheckSpillRekey).
func (t *BTree) commitSpillLeafWindow(w *spillLeafWindow) error {
	// The divider entry (the parent's old boundary, re-encoded leaf-form
	// with NO chain): when it lands ON a page as a regular cell, its
	// payload needs the chain written for the target page.
	divLandsLeft := w.divIdx < w.boundary
	target := w.pg.PageNum
	if divLandsLeft {
		target = w.leftPgno
	}
	if perr := t.prepareCell(w.all[w.divIdx].cell, target); perr != nil {
		return perr
	}
	w.all[w.divIdx].cellData = storage.AppendEncodedCell(nil, w.all[w.divIdx].cell)

	leftEntries := w.all[:w.boundary]
	rightEntries := w.all[w.boundary+1:]
	if err := t.writeSpillLeafHalf(w.leftPg, w.leftCo, leftEntries); err != nil {
		return err
	}
	if err := t.writeSpillLeafHalf(w.pg, contentOffset(w.pg.PageNum), rightEntries); err != nil {
		return err
	}
	return t.finishSpillRekey(w)
}

// writeSpillLeafHalf rewrites one spill window page with its entries: the
// write-intent barrier, the content clear, the cell write, the parse-memo
// refresh, the moved entries' overflow-chain re-parenting and the persist.
func (t *BTree) writeSpillLeafHalf(pg *pager.Page, co int, entries []splitEntry) error {
	t.pager.PrepareWrite(pg)
	for i := co + 1; i < int(t.pageSize); i++ {
		pg.Data[i] = 0
	}
	if err := writeLeafHalf(pg, co, entries, int(t.usableSize)); err != nil {
		return err
	}
	p2, err := storage.ParsePageInto(pg.Data, int(t.pageSize), co, &storage.BTreePage{})
	if err != nil {
		return err
	}
	pg.RefreshParsedBTree(int(t.pageSize), co, p2)
	if err := t.reparentSplitOverflowChains(entries, pg); err != nil {
		return err
	}
	return t.pager.WritePage(pg)
}

// finishSpillRekey re-keys the parent's boundary divider with the spill's
// boundary key (the precheck-encoded cell), repacks the abandoned divider
// bytes and persists the parent.
func (t *BTree) finishSpillRekey(w *spillLeafWindow) error {
	// The parent re-key's encode + room check ran BEFORE the page writes
	// (the hoisted precheck); the old divider's chain frees with the abandon
	// here (clearCell parity).
	t.pager.PrepareWrite(w.parentPg)
	if _, aerr := t.abandonDividerCell(w.parentPg, w.divOff); aerr != nil {
		return aerr
	}
	rekeyPtrEnd := w.ptrBase + int(w.parent.CellCount)*2 + 2
	rkStart := int(w.parent.CellContent) - len(w.rekeyed)
	if rkStart < rekeyPtrEnd {
		return fmt.Errorf("btree: spill re-key: parent %d full", w.parentPg.PageNum)
	}
	copy(w.parentPg.Data[rkStart:], w.rekeyed)
	binary.BigEndian.PutUint16(w.parentPg.Data[w.ptrBase+(w.k-1)*2:], uint16(rkStart))
	binary.BigEndian.PutUint16(w.parentPg.Data[w.parentCo+5:w.parentCo+7], uint16(rkStart))
	w.parent.CellContent = rkStart
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
