// Recursive parent-boundary cascade for index balance. When the generalized
// parent plan cannot fit because the full parent has no room for one more
// divider, this file rebalances that parent one level up with the same
// balance_nonroot-style gather + cntNew pack + parent-plan commit, repeating
// upward until a level fits (SQLite balance()'s do-loop, src/btree.c:9097).
// Planning is allocation- and write-free: no page is written and no overflow
// chain is allocated until every level has passed its room check, so an
// atomic bail leaves the tree untouched.

package btree

import (
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// indexBalanceCascadeLevel is one interior level of a recursive parent
// cascade. win redistributes a full parent across its siblings under the next
// level up; probe is the (room-checked, unencoded) edit to that upper parent.
type indexBalanceCascadeLevel struct {
	win   *indexInteriorWindow
	probe *indexBalanceParentPlan
	// carried maps a post-sort position in win.entries to the index of the
	// previous level's boundary it represents. Its leftChild is filled with
	// the real output page number at commit time.
	carried map[int]int
	// rightPlaceholder is true when the lower edit rewrote its rightmost
	// header field (rather than a trailing cell), leaving a sentinel that
	// prepare replaces with the lower level's last output page.
	rightPlaceholder bool
}

// indexBalanceParentEntryAt decodes parent cell idx as an interior-index
// entry with its full payload. When idx is the plan's trailing cell its left
// child is rewritten to rightmost; a -1 trailing leaves the header field.
func (t *BTree) indexBalanceParentEntryAt(plan *indexBalanceParentPlan, idx int) (interiorEntry, error) {
	off := int(binary.BigEndian.Uint16(plan.parentPg.Data[plan.ptrBase+idx*2 : plan.ptrBase+idx*2+2]))
	cell, err := storage.DecodeCell(plan.parentPg.Data, off, storage.CellIndexInterior, int(t.usableSize))
	if err != nil {
		return interiorEntry{}, err
	}
	full, err := t.readOverflow(cell)
	if err != nil {
		return interiorEntry{}, err
	}
	// readOverflow returns the decoded cell unchanged when the payload is
	// wholly local, so Payload aliases the parent page's cell content. The
	// cascade rewrites that page, so the entry must own a copy.
	payload := append([]byte(nil), full.Payload...)
	left := cell.LeftPtr
	if idx == plan.trailing {
		left = plan.rightmost
	}
	return interiorEntry{leftChild: left, payload: payload}, nil
}

// indexBalanceParentEditedEntries returns the parent page's cells after the
// plan is applied, decoded as interior entries, plus its post-edit rightmost
// pointer. Survivors keep their exact child pointer and payload (with the
// trailing child rewritten); new boundaries are inserted at insertAt with a
// sentinel left child that commit replaces by the real output page number.
// Nothing is written or allocated.
func (t *BTree) indexBalanceParentEditedEntries(plan *indexBalanceParentPlan) ([]interiorEntry, map[int]int, int, uint32, bool, error) {
	oldCount := int(plan.parent.CellCount)
	removed := make(map[int]bool, len(plan.remove))
	for _, idx := range plan.remove {
		removed[idx] = true
	}
	entries := make([]interiorEntry, 0, oldCount+len(plan.boundaries))
	carried := make(map[int]int)
	trailingPos := -1
	emit := func(idx int) error {
		e, err := t.indexBalanceParentEntryAt(plan, idx)
		if err != nil {
			return err
		}
		if idx == plan.trailing {
			trailingPos = len(entries)
		}
		entries = append(entries, e)
		return nil
	}
	for i := 0; i < plan.insertAt; i++ {
		if !removed[i] {
			if err := emit(i); err != nil {
				return nil, nil, 0, 0, false, err
			}
		}
	}
	for i, b := range plan.boundaries {
		carried[len(entries)] = i
		entries = append(entries, interiorEntry{leftChild: 0, payload: b.payload})
	}
	for i := plan.insertAt; i < oldCount; i++ {
		if !removed[i] {
			if err := emit(i); err != nil {
				return nil, nil, 0, 0, false, err
			}
		}
	}
	right := binary.BigEndian.Uint32(plan.parentPg.Data[plan.parentCo+8 : plan.parentCo+12])
	placeholder := plan.trailing == -1
	if placeholder {
		right = plan.rightmost // placeholder; resolved at commit
	}
	return entries, carried, trailingPos, right, placeholder, nil
}

// planIndexBalanceCascade plans upward cascades until plan fits. It returns
// (levels, true) when every level fits — levels is empty when plan already
// fits — and (nil, false) for an atomic bail: no page written, no chain
// allocated. Each cascade level re-gathers the full parent with up to two
// siblings and folds the parent edit into the anchor's cell array.
func (t *BTree) planIndexBalanceCascade(plan *indexBalanceParentPlan) ([]*indexBalanceCascadeLevel, bool, error) {
	if t.indexBalanceRoom(plan) {
		return nil, true, nil
	}
	var levels []*indexBalanceCascadeLevel
	cur := plan
	for depth := 0; depth < 32; depth++ {
		entries, carried, trailingPos, right, placeholder, err := t.indexBalanceParentEditedEntries(cur)
		if err != nil {
			return nil, false, err
		}
		pgno := cur.parentPg.PageNum
		gpgno, _, ferr := t.findParentByWalk(pgno)
		if ferr != nil {
			return nil, false, nil // atomic bail: parent is the root
		}
		gpg, err := t.pager.ReadPage(gpgno)
		if err != nil {
			return nil, false, err
		}
		gco := contentOffset(gpg.PageNum)
		gpage, err := storage.ParsePage(gpg.Data, int(t.pageSize), gco)
		if err != nil || gpage.PageType != storage.PageTypeInteriorIndex {
			return nil, false, nil
		}
		gbase := gco + cellPtrOffset(gpage.PageType)
		g, ok, err := t.gatherIndexBalancePages(gpg, gpage, gco, gbase, pgno, false)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		w, ok, err := t.gatherIndexInteriorOverride(gpg, gpage, gco, gbase, g, pgno, entries, right, carried, trailingPos)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		if err := t.planIndexInteriorWindow(w); err != nil {
			return nil, false, err
		}
		if w.parentPlan == nil {
			return nil, false, nil
		}
		levels = append(levels, &indexBalanceCascadeLevel{win: w, probe: w.parentPlan, carried: w.carried, rightPlaceholder: placeholder})
		if t.indexBalanceRoom(w.parentPlan) {
			return levels, true, nil
		}
		cur = w.parentPlan
	}
	return nil, false, nil
}

// gatherIndexInteriorOverride builds an interior window in which the anchor
// page's cells are supplied by a parent edit (the full parent one level down)
// instead of read from its page image. Used by the recursive cascade: the
// parent edit's new dividers are already in entries and re-keyed into the
// rightmost field, so no child-split fold is needed. carried positions are
// re-mapped after the merge sort by payload identity.
func (t *BTree) gatherIndexInteriorOverride(parentPg *pager.Page, parent *storage.BTreePage, parentCo, ptrBase int, g *indexBalanceGather, anchorPgno uint32, anchorEntries []interiorEntry, anchorRight uint32, carriedIn map[int]int, trailingIn int) (*indexInteriorWindow, bool, error) {
	w := &indexInteriorWindow{
		parentPg: parentPg, parent: parent, parentCo: parentCo, ptrBase: ptrBase,
		pages: g.pages, outPages: g.pages, gather: g, coff: make([]int, len(g.pages)),
		cntOld: make([]int, len(g.pages)), used: make([]int, len(g.pages)),
		rightmost: make([]uint32, len(g.pages)), carried: make(map[int]int),
		trailingPos: -1, anchorDivPos: -1,
	}
	w.outCo = w.coff
	anchorIdx := -1
	anchorStart := 0
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
		var entries []interiorEntry
		w.rightmost[i] = ipPage.RightmostPtr
		if ip.PageNum == anchorPgno {
			anchorIdx = i
			anchorStart = len(w.entries)
			entries = append([]interiorEntry(nil), anchorEntries...)
			w.rightmost[i] = anchorRight
		} else {
			entries = t.collectInteriorEntries(ip, ipCo+cellPtrOffset(ipPage.PageType)-8, ipPage)
			// collectInteriorEntries returns payload views into the sibling
			// page, which the cascade rewrites; the merged array must own them.
			for i := range entries {
				entries[i].payload = append([]byte(nil), entries[i].payload...)
			}
		}
		start := len(w.entries)
		for _, e := range entries {
			w.entries = append(w.entries, e)
			w.sizes = append(w.sizes, t.dividerCellLen(e.splitResult(t.isTable)))
		}
		divPos := -1
		if i < len(g.boundaries) {
			payload, perr := t.indexBalancePayload(parentPg, ptrBase, g.boundaries[i])
			if perr != nil {
				return nil, false, perr
			}
			divider := interiorEntry{leftChild: w.rightmost[i], payload: payload}
			w.entries = append(w.entries, divider)
			w.sizes = append(w.sizes, t.dividerCellLen(divider.splitResult(t.isTable)))
			divPos = len(w.entries) - 1
		}
		w.cntOld[i] = len(w.entries)
		w.used[i] = indexBalanceUsed(w.sizes, start, len(w.entries))
		if i == anchorIdx {
			w.anchorDivPos = divPos
		}
	}
	if anchorIdx < 0 {
		return nil, false, nil
	}
	// Remap the carried edit, the trailing survivor and the divider appended
	// after the anchor (which inherited its placeholder rightmost) by payload
	// identity after the merge sort.
	type carryMark struct {
		payload   []byte
		bi        int
		trail     bool
		anchorDiv bool
	}
	var marks []carryMark
	for pos, bi := range carriedIn {
		abs := anchorStart + pos
		if abs >= len(w.entries) {
			return nil, false, nil
		}
		marks = append(marks, carryMark{payload: w.entries[abs].payload, bi: bi})
	}
	if trailingIn >= 0 {
		abs := anchorStart + trailingIn
		if abs >= len(w.entries) {
			return nil, false, nil
		}
		marks = append(marks, carryMark{payload: w.entries[abs].payload, bi: -1, trail: true})
	}
	if w.anchorDivPos >= 0 {
		marks = append(marks, carryMark{payload: w.entries[w.anchorDivPos].payload, bi: -1, anchorDiv: true})
	}
	sort.SliceStable(w.entries, func(i, j int) bool {
		return t.compareKey(w.entries[i].payload, w.entries[j].payload) < 0
	})
	// sizes is indexed positionally by the pack; realign it after the sort.
	for i := range w.entries {
		w.sizes[i] = t.dividerCellLen(w.entries[i].splitResult(t.isTable))
	}
	for _, m := range marks {
		found := false
		for pos := range w.entries {
			if samePayload(w.entries[pos].payload, m.payload) {
				switch {
				case m.trail:
					w.trailingPos = pos
				case m.anchorDiv:
					w.anchorDivPos = pos
				default:
					w.carried[pos] = m.bi
				}
				found = true
				break
			}
		}
		if !found {
			return nil, false, nil
		}
	}
	return w, true, nil
}

// samePayload compares two full index-key payloads byte-for-byte.
func samePayload(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// errIndexBalanceCascadeChild signals a carried boundary that cannot be
// resolved to an output page during cascade preparation.
var errIndexBalanceCascadeChild = fmt.Errorf("btree: index balance cascade: unresolved carried child")

// cascadeTopPlan returns the plan that is actually written to a page: the
// topmost cascade level's probe when levels is non-empty, otherwise plan.
func cascadeTopPlan(plan *indexBalanceParentPlan, levels []*indexBalanceCascadeLevel) *indexBalanceParentPlan {
	if len(levels) > 0 {
		return levels[len(levels)-1].probe
	}
	return plan
}

// cascadePages gathers every output page slice that must be written, ordered
// top-down: the cascade levels from highest to lowest, then the origin pages.
// Each caller appends its own origin pages last.
func cascadeWindows(levels []*indexBalanceCascadeLevel) []*indexInteriorWindow {
	out := make([]*indexInteriorWindow, 0, len(levels))
	for i := len(levels) - 1; i >= 0; i-- {
		out = append(out, levels[i].win)
	}
	return out
}

// prepareCascadeLevels allocates the extra output page of each cascade level
// and resolves the sentinel left children. Levels are prepared bottom-up so a
// lower level's real page numbers are available to the level above. The
// caller supplies the origin's final output pages (indexed by the origin
// plan) for the lowest cascade level's carried entries.
func (t *BTree) prepareCascadeLevels(levels []*indexBalanceCascadeLevel, originPages []*pager.Page) error {
	prev := originPages
	for i := range levels {
		lv := levels[i]
		w := lv.win
		if err := prepareInteriorWindowPages(t, w); err != nil {
			return err
		}
		for pos, bidx := range lv.carried {
			if bidx < 0 || bidx >= len(prev) {
				return errIndexBalanceCascadeChild
			}
			w.entries[pos].leftChild = prev[bidx].PageNum
		}
		// The lower edit re-keyed its trailing survivor and rightmost field to
		// the lower level's last output page; both were placeholders at plan
		// time and must be resolved before any page is written.
		last := prev[len(prev)-1].PageNum
		if w.trailingPos >= 0 {
			w.entries[w.trailingPos].leftChild = last
		}
		if lv.rightPlaceholder {
			if ai := w.gather.anchorIdx; ai >= 0 {
				w.rightmost[ai] = last
			}
			if w.anchorDivPos >= 0 {
				w.entries[w.anchorDivPos].leftChild = last
			}
		}
		// The next level's carried entries point at this level's output pages.
		prev = w.outPages[:len(w.cntNew)]
	}
	return nil
}
