// Shared machinery for the index-only balance_nonroot port. The gathered
// window is a contiguous run of parent children plus the parent dividers
// BETWEEN them; every divider is appended to the cell array after its left
// page (SQLite's balance_nonroot gather, src/btree.c:8268-8485) so it is
// redistributed like any other cell. A boundary cell chosen by cntNew[] is
// copied into the parent and is NOT stored on either sibling page.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// indexBalanceGather is a parent-child window: pages in ascending key order
// and the parent cell indices BETWEEN adjacent pages. The cell immediately
// after boundaries[len-1] is the rightmost boundary of the window; -1 means
// the window ends at the parent's rightmost-child pointer.
type indexBalanceGather struct {
	pages      []*pager.Page
	boundaries []int
	trailing   int // parent cell index after the last page, or -1 for rightmost
	anchorIdx  int // position of the splitting page inside pages
}

// gatherIndexBalancePages collects the splitting page's parent-child window.
// Non-bulk callers gather up to three pages (left, anchor, right); a bulk
// split directly under the root takes the anchor and its right sibling, or
// the left sibling when the anchor is the rightmost child. Every bail returns
// ok=false before any page read beyond the parent.
func (t *BTree) gatherIndexBalancePages(parentPg *pager.Page, parent *storage.BTreePage, parentCo, ptrBase int, anchorPgno uint32, bulk bool) (*indexBalanceGather, bool, error) {
	if parent.PageType != storage.PageTypeInteriorIndex || parent.CellCount == 0 {
		return nil, false, nil
	}
	k := locateSpillAnchorCell(parentPg, parent, parentCo, ptrBase, anchorPgno)
	if k < 0 {
		return nil, false, nil
	}
	canLeft, canRight := k > 0, k < int(parent.CellCount)
	if !canLeft && !canRight {
		return nil, false, nil
	}
	g := &indexBalanceGather{trailing: -1, anchorIdx: -1}
	switch {
	case bulk && canRight:
		g.boundaries = []int{k}
	case bulk:
		g.boundaries = []int{k - 1}
	case canLeft && canRight:
		g.boundaries = []int{k - 1, k}
	case canLeft:
		g.boundaries = []int{k - 1}
	default:
		g.boundaries = []int{k}
	}
	g.trailing = g.boundaries[len(g.boundaries)-1] + 1
	if g.trailing >= int(parent.CellCount) {
		g.trailing = -1
	}
	for i, b := range g.boundaries {
		pgno, err := parentLeftChild(parentPg, ptrBase, b)
		if err != nil {
			return nil, false, err
		}
		pg, err := t.pager.ReadPage(pgno)
		if err != nil {
			return nil, false, err
		}
		if pg.PageNum == anchorPgno {
			g.anchorIdx = i
		}
		g.pages = append(g.pages, pg)
	}
	lastPgno := parentRightmost(parentPg, parentCo)
	if g.trailing >= 0 {
		var err error
		lastPgno, err = parentLeftChild(parentPg, ptrBase, g.trailing)
		if err != nil {
			return nil, false, err
		}
	}
	lastPg, err := t.pager.ReadPage(lastPgno)
	if err != nil {
		return nil, false, err
	}
	if lastPg.PageNum == anchorPgno {
		g.anchorIdx = len(g.pages)
	}
	g.pages = append(g.pages, lastPg)
	if g.anchorIdx < 0 {
		return nil, false, nil
	}
	return g, true, nil
}

// parentLeftChild reads the 4-byte child pointer at the head of the parent's
// cell idx.
func parentLeftChild(parentPg *pager.Page, ptrBase, idx int) (uint32, error) {
	off := int(binary.BigEndian.Uint16(parentPg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
	if off+4 > len(parentPg.Data) {
		return 0, fmt.Errorf("btree: balance parent cell %d out of bounds", idx)
	}
	return binary.BigEndian.Uint32(parentPg.Data[off : off+4]), nil
}

// parentRightmost reads the parent's rightmost-child pointer.
func parentRightmost(parentPg *pager.Page, parentCo int) uint32 {
	return binary.BigEndian.Uint32(parentPg.Data[parentCo+8 : parentCo+12])
}

// indexBalancePayload reads a parent divider cell's full payload. It clones
// the payload: the parent's cell content is rewritten after the redistribution.
func (t *BTree) indexBalancePayload(parentPg *pager.Page, ptrBase, idx int) ([]byte, error) {
	off := int(binary.BigEndian.Uint16(parentPg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
	cell, err := storage.DecodeCell(parentPg.Data, off, storage.CellIndexInterior, int(t.usableSize))
	if err != nil {
		return nil, err
	}
	full, err := t.readOverflow(cell)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), full.Payload...), nil
}

// indexBalanceParentBoundary is one final divider of the rebuilt parent. The
// encoded image is prepared before any child page is written; payload is kept
// for the room precheck (which must not allocate overflow pages).
type indexBalanceParentBoundary struct {
	leftChild uint32
	payload   []byte
	encoded   []byte
}

// indexBalanceParentPlan is C's parent-side balance_nonroot boundary rewrite:
// drop the gathered divider slots, insert one divider after every output page
// except the last, and point the slot (or header field) after the gathered
// window at the last output page. Cells outside the window keep their bytes.
type indexBalanceParentPlan struct {
	parentPg   *pager.Page
	parent     *storage.BTreePage
	parentCo   int
	ptrBase    int
	remove     []int
	insertAt   int
	trailing   int // parent cell after the window, or -1 for the rightmost field
	rightmost  uint32
	boundaries []indexBalanceParentBoundary
}

// indexBalanceParentCell copies one on-page parent cell image, retaining its
// overflow pointer when the payload spills. The child pointer is the first
// four bytes of every interior-index cell.
func (t *BTree) indexBalanceParentCell(pg *pager.Page, ptrBase, idx int) ([]byte, error) {
	off := int(binary.BigEndian.Uint16(pg.Data[ptrBase+idx*2 : ptrBase+idx*2+2]))
	cell, err := storage.DecodeCell(pg.Data, off, storage.CellIndexInterior, int(t.usableSize))
	if err != nil {
		return nil, err
	}
	sz := 4 + util.VarintLen(uint64(cell.PayloadLen)) + cell.LocalLen
	if cell.Overflow != 0 {
		sz += 4
	}
	return append([]byte(nil), pg.Data[off:off+sz]...), nil
}

// newIndexBalanceParentPlan maps a gathered window to its parent edit. The
// gathered boundaries are exactly the slots C's balance_nonroot drops before
// inserting the new dividers at nxDiv (boundaries[0]).
func (t *BTree) newIndexBalanceParentPlan(parentPg *pager.Page, parent *storage.BTreePage, parentCo, ptrBase int, g *indexBalanceGather, boundaries []indexBalanceParentBoundary, rightmost uint32) *indexBalanceParentPlan {
	if g == nil || len(g.boundaries) == 0 || len(boundaries) == 0 {
		return nil
	}
	return &indexBalanceParentPlan{
		parentPg: parentPg, parent: parent, parentCo: parentCo, ptrBase: ptrBase,
		remove: append([]int(nil), g.boundaries...), insertAt: g.boundaries[0],
		trailing: g.trailing, rightmost: rightmost, boundaries: boundaries,
	}
}

// indexBalanceRoom reports whether the rebuilt parent fits. It runs before the
// extra page allocation and before any divider is encoded, so a bail leaves
// the tree untouched. New divider sizes are measured from their payloads.
func (t *BTree) indexBalanceRoom(plan *indexBalanceParentPlan) bool {
	if plan == nil || plan.parent == nil || plan.insertAt < 0 || plan.trailing < -1 {
		return false
	}
	oldCount := int(plan.parent.CellCount)
	if plan.trailing >= oldCount || plan.insertAt > oldCount {
		return false
	}
	removed := make(map[int]bool, len(plan.remove))
	for _, idx := range plan.remove {
		if idx < 0 || idx >= oldCount || removed[idx] || idx == plan.trailing {
			return false
		}
		removed[idx] = true
	}
	cellBytes := 0
	for i := 0; i < oldCount; i++ {
		if removed[i] {
			continue
		}
		image, err := t.indexBalanceParentCell(plan.parentPg, plan.ptrBase, i)
		if err != nil {
			return false
		}
		cellBytes += len(image)
	}
	for _, b := range plan.boundaries {
		cellBytes += t.dividerCellLen(leafSplitResult{medianPayload: b.payload})
	}
	finalCount := oldCount - len(removed) + len(plan.boundaries)
	ptrEnd := plan.ptrBase + finalCount*2 + 2
	return int(t.usableSize)-cellBytes-ptrEnd >= 0
}

// prepareIndexBalanceParent encodes every new boundary cell before any child
// page write. A payload that spills gets a fresh chain owned by the parent.
func (t *BTree) prepareIndexBalanceParent(plan *indexBalanceParentPlan) error {
	for i := range plan.boundaries {
		b := &plan.boundaries[i]
		cell, err := t.encodeDividerCell(b.leftChild, leafSplitResult{medianPayload: b.payload}, plan.parentPg.PageNum)
		if err != nil {
			return err
		}
		b.encoded = cell
	}
	return nil
}

// commitIndexBalanceParent rebuilds the parent from exact survivor images and
// the prepared boundary cells, frees only the replaced dividers' chains, and
// rewrites the right pointer that follows the gathered window.
func (t *BTree) commitIndexBalanceParent(plan *indexBalanceParentPlan) error {
	for _, b := range plan.boundaries {
		if len(b.encoded) == 0 {
			return fmt.Errorf("btree: index balance: parent boundary not prepared")
		}
	}
	oldCount := int(plan.parent.CellCount)
	removed := make(map[int]bool, len(plan.remove))
	for _, idx := range plan.remove {
		removed[idx] = true
	}
	survivors := make([][]byte, 0, oldCount-len(removed))
	for i := 0; i < oldCount; i++ {
		if removed[i] {
			continue
		}
		image, err := t.indexBalanceParentCell(plan.parentPg, plan.ptrBase, i)
		if err != nil {
			return err
		}
		if i == plan.trailing {
			if len(image) < 4 {
				return fmt.Errorf("btree: index balance: short parent cell")
			}
			binary.BigEndian.PutUint32(image[:4], plan.rightmost)
		}
		survivors = append(survivors, image)
	}
	if plan.insertAt < 0 || plan.insertAt > len(survivors) {
		return fmt.Errorf("btree: index balance: invalid parent insert slot")
	}
	final := make([][]byte, 0, len(survivors)+len(plan.boundaries))
	final = append(final, survivors[:plan.insertAt]...)
	for _, b := range plan.boundaries {
		final = append(final, b.encoded)
	}
	final = append(final, survivors[plan.insertAt:]...)

	rightmost := binary.BigEndian.Uint32(plan.parentPg.Data[plan.parentCo+8 : plan.parentCo+12])
	if plan.trailing == -1 {
		rightmost = plan.rightmost
	}
	for _, idx := range plan.remove {
		off := int(binary.BigEndian.Uint16(plan.parentPg.Data[plan.ptrBase+idx*2 : plan.ptrBase+idx*2+2]))
		if _, err := t.abandonDividerCell(plan.parentPg, off); err != nil {
			return err
		}
	}
	t.pager.PrepareWrite(plan.parentPg)
	for i := plan.parentCo + 1; i < int(t.pageSize); i++ {
		plan.parentPg.Data[i] = 0
	}
	end := int(t.usableSize)
	for i, image := range final {
		start := end - len(image)
		if start < plan.ptrBase+(len(final)+1)*2 {
			return fmt.Errorf("btree: index balance: parent %d full", plan.parentPg.PageNum)
		}
		copy(plan.parentPg.Data[start:], image)
		binary.BigEndian.PutUint16(plan.parentPg.Data[plan.ptrBase+i*2:], uint16(start))
		end = start
	}
	binary.BigEndian.PutUint16(plan.parentPg.Data[plan.parentCo+3:plan.parentCo+5], uint16(len(final)))
	binary.BigEndian.PutUint16(plan.parentPg.Data[plan.parentCo+5:plan.parentCo+7], uint16(end))
	binary.BigEndian.PutUint32(plan.parentPg.Data[plan.parentCo+8:plan.parentCo+12], rightmost)
	pp, err := storage.ParsePageInto(plan.parentPg.Data, int(t.pageSize), plan.parentCo, &storage.BTreePage{})
	if err != nil {
		return err
	}
	plan.parentPg.RefreshParsedBTree(int(t.pageSize), plan.parentCo, pp)
	return t.pager.WritePage(plan.parentPg)
}
