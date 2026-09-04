// Port of btree.c::balance_nonroot (line 8206). The main btree
// rebalance routine: gathers up to 5 sibling pages + the divider
// cells in their parent, redistributes all cells size-balanced
// across the new page set, and rewrites the parent's divider cells
// to match the new layout.
//
// This is a focused port. The full C algorithm handles:
//   - intkey + non-intkey btrees
//   - leaf + interior pages (interior balancing also redistributes
//     child pointers)
//   - overflow cells (kept in apOvfl[], separate from cell pointer
//     array)
//   - autovacuum pointer-map updates for every moved cell's
//     children/overflow chains
//   - "balance_shallower" (root collapse) and root right-child
//     propagation
//
// Our port covers the cases the testgen packages need:
//   - intkey (table leaf) btrees only
//   - no overflow cells in the balanced set
//   - no interior-page balancing (parent's child pointers are
//     updated by the divider-cell rewrite)
//   - the shallower path (root collapse) is handled by a separate
//     helper
//
// Reference: src/btree.c::balance_nonroot (line 8206).

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// balanceNonrootContext bundles the arguments to balance_nonroot
// for clarity. Most fields are pointers to the parent and the
// "page being balanced" (the one with too many/few cells).
type balanceNonrootContext struct {
	parent     *pager.Page // parent interior page
	iParentIdx int         // index of the page being balanced in parent's cell pointer array (-1 == rightmost-child)
	page       *pager.Page // the page being balanced
	aOvflSpace []byte      // page-size bytes of overflow scratch (unused in our simplified port)
	isRoot     bool        // true if parent is the btree root
}

// balanceNonroot is the simplified port of btree.c::balance_nonroot.
// It assumes the page being balanced has too many or too few cells
// (insert/delete) and that the parent has 1 or more siblings that
// can absorb or donate cells.
//
// The simplified algorithm:
//  1. Gather parent + up to 4 siblings (5 total including `page`).
//  2. Build a CellArray with all cells (in-memory).
//  3. Distribute the cells size-balanced into 1..k new pages,
//     reusing old pages where possible.
//  4. Write the new cells to the new pages, write new divider
//     cells in the parent, free any pages we no longer need.
//
// Returns the updated parent page (the caller is responsible for
// WritePage). The function does NOT recurse up the tree — the
// caller is responsible for calling balanceNonroot again if the
// parent's cell count changes.
func (t *BTree) balanceNonroot(ctx *balanceNonrootContext) (*pager.Page, error) {
	if ctx == nil || ctx.parent == nil || ctx.page == nil {
		return nil, fmt.Errorf("btree: balanceNonroot: nil context")
	}
	// Parse the parent.
	parentCo := contentOffset(ctx.parent.PageNum)
	parent, err := storage.ParsePage(ctx.parent.Data, int(t.pageSize), parentCo)
	if err != nil {
		return nil, fmt.Errorf("balanceNonroot: parse parent %d: %w", ctx.parent.PageNum, err)
	}
	if parent.PageType != storage.PageTypeInteriorTable && parent.PageType != storage.PageTypeInteriorIndex {
		return nil, fmt.Errorf("balanceNonroot: parent %d is not interior (type 0x%02x)", ctx.parent.PageNum, parent.PageType)
	}

	// Phase 1: gather siblings.
	// We collect up to NB=5 siblings: the page being balanced plus
	// up to 2 on each side. SQLite's gather walks the parent
	// interior page's cell pointer array outward from iParentIdx.
	// For our port we gather the page being balanced and read the
	// adjacent cells in the parent.
	siblings := make([]*pager.Page, 0, balanceNB)
	siblings = append(siblings, ctx.page)
	// For simplicity in this first port, only handle the case
	// where the page being balanced is the rightmost child (no
	// left siblings). The general case (page in the middle) is
	// deferred — the autovacuum/incrvacuum testgen scenarios all
	// have the page being balanced as the rightmost or only
	// child of the parent, since DELETE leaves empty leaves at
	// the right end of the tree.
	// For our supported configurations, the page being balanced
	// is either:
	//   - the rightmost-child (iParentIdx == -1): gather 0 or 1
	//     left sibling(s)
	//   - the rightmost cell-child (iParentIdx == parent.CellCount-1):
	//     gather the rightmost-child as the right sibling
	//   - the leftmost cell-child (iParentIdx == 0): gather cell[1]'s
	//     left-child (the next sibling) as the right sibling
	if ctx.iParentIdx >= 0 && ctx.iParentIdx < int(parent.CellCount) {
		// The page being balanced is a cell-child of the parent.
		// Determine the right sibling.
		var rmp uint32
		if ctx.iParentIdx < int(parent.CellCount)-1 {
			// Right sibling is the left-child of cell[iParentIdx+1].
			ptrBase := parentCo + cellPtrOffset(parent.PageType) - 8
			cp := storage.CellPointer(ctx.parent.Data, ptrBase, ctx.iParentIdx+1, int(t.pageSize))
			if int(cp)+4 > len(ctx.parent.Data) {
				return nil, fmt.Errorf("balanceNonroot: cell pointer for right sibling out of bounds")
			}
			rmp = binary.BigEndian.Uint32(ctx.parent.Data[cp : cp+4])
		} else {
			// iParentIdx is the last cell-child; right sibling
			// is the rightmost-child pointer.
			rmp = binary.BigEndian.Uint32(ctx.parent.Data[parentCo+8 : parentCo+12])
		}
		if rmp == 0 {
			return nil, fmt.Errorf("balanceNonroot: parent %d has no right sibling for cell-child %d", ctx.parent.PageNum, ctx.iParentIdx)
		}
		rpg, err := t.pager.ReadPage(rmp)
		if err != nil {
			return nil, fmt.Errorf("balanceNonroot: read right sibling %d: %w", rmp, err)
		}
		siblings = append(siblings, rpg)
		// Extend the gather LEFTWARD through the cells below iParentIdx
		// while capacity allows (btree.c balance_nonroot fills apSibling
		// from the window's leftmost sibling; src/btree.c ~8500). Without
		// the left sibling a 3-leaf parent redistributes into 2 pages but
		// the excess freed page is chosen by position, not by SQLite's
		// left-to-right packing.
		ptrBaseL := parentCo + cellPtrOffset(parent.PageType) - 8
		for left := ctx.iParentIdx - 1; left >= 0 && len(siblings) < balanceNB; left-- {
			cpL := storage.CellPointer(ctx.parent.Data, ptrBaseL, left, int(t.pageSize))
			if int(cpL)+4 > len(ctx.parent.Data) {
				break
			}
			ls := binary.BigEndian.Uint32(ctx.parent.Data[cpL : cpL+4])
			if ls == 0 {
				break
			}
			lpg, rerr := t.pager.ReadPage(ls)
			if rerr != nil {
				break
			}
			siblings = append([]*pager.Page{lpg}, siblings...)
		}
	} else {
		// The page being balanced is the rightmost-child of the
		// parent. Gather up to 1 left sibling.
		if int(parent.CellCount) >= 1 {
			ptrBase := parentCo + cellPtrOffset(parent.PageType) - 8
			cp := storage.CellPointer(ctx.parent.Data, ptrBase, int(parent.CellCount)-1, int(t.pageSize))
			if int(cp)+4 <= len(ctx.parent.Data) {
				ls := binary.BigEndian.Uint32(ctx.parent.Data[cp : cp+4])
				if ls != 0 {
					lpg, err := t.pager.ReadPage(ls)
					if err == nil {
						siblings = append([]*pager.Page{lpg}, siblings...)
					}
				}
			}
		}
	}
	if len(siblings) < 1 || len(siblings) > balanceNB {
		return nil, fmt.Errorf("balanceNonroot: gathered %d siblings (must be 1..%d)", len(siblings), balanceNB)
	}

	// Phase 2: collect all cells from all siblings into a CellArray.
	bca := newBalanceCellArray(0, len(siblings))
	for _, sp := range siblings {
		spCo := contentOffset(sp.PageNum)
		spPage, err := storage.ParsePage(sp.Data, int(t.pageSize), spCo)
		if err != nil {
			return nil, fmt.Errorf("balanceNonroot: parse sibling %d: %w", sp.PageNum, err)
		}
		// For each cell in the sibling, copy its bytes into the
		// CellArray. We require that the sibling is a table leaf
		// (no overflow cells, no index btree).
		if spPage.PageType != storage.PageTypeLeafTable {
			return nil, fmt.Errorf("balanceNonroot: sibling %d is not a table leaf (type 0x%02x)", sp.PageNum, spPage.PageType)
		}
		for i := 0; i < int(spPage.CellCount); i++ {
			cp := storage.CellPointer(sp.Data, spCo, i, int(t.usableSize))
			// Cell bytes: cell i starts at cp[i] and is sz bytes long,
			// where sz is decoded from the cell's header (varint
			// payload length + varint rowid + local payload + optional
			// 4-byte overflow pointer) — matching SQLite's
			// btree.c::computeCellSize / cachedCellSize. The cell
			// pointer array is sorted by KEY (rowid), but the cell
			// addresses on the page grow downward and may live in
			// freeblock regions after a defragment, so the cp[] values
			// are NOT in monotonic address order. The previous code
			// read [cp[i], cp[i-1]) assuming the cp[] array was in
			// decreasing-address order; that mis-read cells whenever
			// the page was defragmented.
			cellSize, err := storage.TableLeafCellSizeAt(sp.Data, int(cp), int(t.usableSize))
			if err != nil || cellSize <= 0 {
				// Corrupt cell: skip it. Matches SQLite's
				// "best effort" behavior on corrupt pages.
				continue
			}
			cellEnd := int(cp) + cellSize
			if cellEnd > int(t.usableSize) {
				continue
			}
			cellBytes := make([]byte, cellSize)
			copy(cellBytes, sp.Data[cp:cellEnd])
			bca.addCell(cellBytes, int(t.usableSize), 0)
		}
		bca.endRegion()
	}
	bca.finalizeRegionEnds([]int{int(t.usableSize)})

	// Phase 3: distribution. All gathered siblings — INCLUDING
	// currently-empty ones — participate (btree.c balance_nonroot keeps
	// empty pages in apSibling: cells are redistributed into them and the
	// surplus HIGHEST-numbered pages are freed). Freeing rightmost pages
	// is what keeps the database file truncatable from the right during
	// auto/incremental vacuum — freeing a mid-file page would leave live
	// pages above it and stall incrVacuumStep (src/btree.c:3822-3984).
	//
	// nNew is the minimum page count: greedily pack cells left-to-right
	// up to usable capacity, at least one cell per page. cntNewFull[k] is
	// the first cell index placed on survivor k.
	nOldFull := len(siblings)
	// Window geometry (needed by every parent-edit path below, including
	// the all-empty branch): the child-index range [c0..c1] of the
	// gathered siblings inside the parent.
	c0, c1 := 0, int(parent.CellCount)
	if ctx.iParentIdx >= 0 {
		// Window: [child(iParentIdx-nOldFull+2) .. child(iParentIdx+1)].
		c0 = ctx.iParentIdx - (nOldFull - 2)
		c1 = ctx.iParentIdx + 1
	} else {
		// Page being balanced is the rightmost child: window is
		// [child(CellCount-nOldFull+1) .. child(CellCount)].
		c0 = int(parent.CellCount) - (nOldFull - 1)
	}
	if bca.nCell() == 0 {
		// All gathered cells vanished: every window sibling is empty.
		// The window's children must be UNLINKED from the parent, not
		// just freed — freeing them while their dividers remain makes
		// every later walk read a freed page ("database disk image is
		// malformed"). Drop the window's dividers (and the rightmost
		// pointer when the window includes child CellCount), free the
		// empty pages, then let the parent itself be collapsed upward
		// if it lost its last child (cascadeChildless).
		if c0 < 0 {
			c0 = 0
		}
		if c1 > int(parent.CellCount) {
			c1 = int(parent.CellCount)
		}
		if c1-c0 > 0 {
			if err := t.removeInteriorCellRange(ctx.parent, parent, c0, c1-c0); err != nil {
				return nil, err
			}
		}
		if c1 >= int(parent.CellCount) {
			// The window includes the rightmost child: it is empty too —
			// drop the reference.
			binary.BigEndian.PutUint32(ctx.parent.Data[parentCo+8:parentCo+12], 0)
		}
		for _, sp := range siblings {
			if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
				return nil, err
			}
		}
		if err := t.pager.WritePage(ctx.parent); err != nil {
			return nil, fmt.Errorf("balanceNonroot: write parent: %w", err)
		}
		if err := t.cascadeChildless(ctx.parent.PageNum); err != nil {
			return nil, err
		}
		return ctx.parent, nil
	}
	// Greedy packing: boundaries only advance when the current page is
	// full, so every survivor receives at least one cell and every
	// surplus (nNew..nOld) page is empty by construction. The per-page
	// budget must match rebuildPage's constraint exactly: total cell
	// bytes + the cell pointer array (2 bytes per cell) + the page
	// header (8 + contentOffset — 100 for page 1) must fit in usable;
	// otherwise rebuildPage rejects the packed page ("cell too large").
	cntNewFull := make([]int, 1, nOldFull+1)
	fill := 0
	nOnPage := 0
	for i := 0; i < bca.nCell(); i++ {
		sz := len(bca.cells[i].cells)
		pgIdx := len(cntNewFull) - 1
		capacity := int(t.usableSize) - contentOffset(siblings[pgIdx].PageNum) - 8 - 2*(nOnPage+1)
		if nOnPage > 0 && fill+sz > capacity {
			cntNewFull = append(cntNewFull, i)
			fill = 0
			nOnPage = 0
		}
		fill += sz
		nOnPage++
	}
	if len(cntNewFull) > nOldFull {
		cntNewFull = cntNewFull[:nOldFull]
	}
	cntNewFull = append(cntNewFull, bca.nCell())
	nNewFull := len(cntNewFull) - 1
	// btree.c balance_nonroot: when the redistributed cell set no longer
	// needs every gathered sibling, the survivors are siblings[0..nNew)
	// and each excess page returns to the freelist. The parent update is
	// window-local in SQLite: dividers WITHIN the gathered window are
	// replaced (insertCell at nxDiv+i, src/btree.c:8813-8852) and the
	// child pointer following the window is repointed
	// (put4byte(pRight, apNew[nNew-1]), src/btree.c:8699); dividers
	// outside the window are never touched. A wholesale parent rewrite
	// is therefore only valid when the gathered window spans ALL of the
	// parent's children: child-index range [c0..c1] with c0==0 (window
	// starts at the parent's first child) AND c1==CellCount (window ends
	// at the parent's rightmost-child pointer).
	coversParent := c0 <= 0 && c1 >= int(parent.CellCount)
	if coversParent {
		for _, sp := range siblings[nNewFull:] {
			if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
				return nil, err
			}
		}
		children := make([]uint32, 0, nNewFull)
		seps := make([]uint64, 0, nNewFull)
		for i := 0; i < nNewFull; i++ {
			sp := siblings[i]
			sub := newBalanceCellArray(cntNewFull[i+1]-cntNewFull[i], 1)
			for j := cntNewFull[i]; j < cntNewFull[i+1]; j++ {
				sub.addCell(bca.cells[j].cells, int(t.usableSize), 0)
			}
			sub.endRegion()
			sub.finalizeRegionEnds([]int{int(t.usableSize)})
			if err := t.rebuildPage(sp, sub, 0, sub.nCell()); err != nil {
				return nil, fmt.Errorf("balanceNonroot: rebuildPage sibling %d: %w", sp.PageNum, err)
			}
			if err := t.pager.WritePage(sp); err != nil {
				return nil, fmt.Errorf("balanceNonroot: write sibling %d: %w", sp.PageNum, err)
			}
			// Moved cells take their overflow chains: re-parent each
			// chain's first page to the surviving owner
			// (ptrmapPutOvflPtr, src/btree.c:8025/8783).
			if err := t.reparentPageOverflowChains(sp.PageNum); err != nil {
				return nil, err
			}
			children = append(children, sp.PageNum)
			if i < nNewFull-1 {
				// Separator convention of this engine's splits: the
				// divider key is the FIRST rowid of the RIGHT subtree —
				// seekInInteriorTable routes keys < K to the divider's
				// left child and keys >= K to the following subtree.
				// (SQLite's leafData branch uses the LEFT page's last
				// rowid because its OP_Seek/MoveToChild convention is
				// key <= K routes left; the boundary must match the
				// seek code it serves, here the right-page minimum.)
				seps = append(seps, uint64(readFirstRowID(siblings[i+1].Data, contentOffset(siblings[i+1].PageNum), storage.CellTableLeaf, int(t.usableSize), int(t.pageSize))))
			}
		}
		// Uniform parent rebuild (balance_nonroot tail): one divider
		// per survivor boundary, last survivor is the rightmost child.
		if err := t.writeInteriorRootAt(ctx.parent.PageNum, children, seps); err != nil {
			return nil, err
		}
		return ctx.parent, nil
	}

	// Partial window (the parent has children outside the gathered
	// range): SQLite's window-local parent edit, the tail of
	// balance_nonroot (src/btree.c:8699-8980):
	//   1. pages beyond nNew return to the freelist
	//      (freePage(apOld[nNew..nOld)), src/btree.c:8960);
	//   2. survivors are rewritten in place (apNew[i] == apOld[i],
	//      src/btree.c:8617);
	//   3. the window's dividers [c0, c1) are replaced by nNew-1 new
	//      dividers at the same array positions (the dividers were
	//      dropped during gather, src/btree.c:8336-8345, and re-inserted
	//      by insertCell(pParent, nxDiv+i, ...) at 8852). A table-leaf
	//      boundary divider carries the last rowid of the LEFT survivor
	//      (the leafData branch, src/btree.c:8837-8845);
	//   4. the child pointer immediately following the window — divider
	//      c1 (now at array index c0+nNew-1) or, if the window bordered
	//      the end of the cell array, the parent's rightmost-child
	//      pointer — is repointed at the last survivor
	//      (put4byte(pRight, apNew[nNew-1]->pgno), src/btree.c:8699).
	// Dividers outside the window are never touched.
	for _, sp := range siblings[nNewFull:] {
		if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
			return nil, err
		}
	}
	for i := 0; i < nNewFull; i++ {
		sp := siblings[i]
		sub := newBalanceCellArray(cntNewFull[i+1]-cntNewFull[i], 1)
		for j := cntNewFull[i]; j < cntNewFull[i+1]; j++ {
			sub.addCell(bca.cells[j].cells, int(t.usableSize), 0)
		}
		sub.endRegion()
		sub.finalizeRegionEnds([]int{int(t.usableSize)})
		if err := t.rebuildPage(sp, sub, 0, sub.nCell()); err != nil {
			return nil, fmt.Errorf("balanceNonroot: rebuildPage sibling %d: %w", sp.PageNum, err)
		}
		if err := t.pager.WritePage(sp); err != nil {
			return nil, fmt.Errorf("balanceNonroot: write sibling %d: %w", sp.PageNum, err)
		}
		// Moved cells take their overflow chains: re-parent each
		// chain's first page to the surviving owner
		// (ptrmapPutOvflPtr, src/btree.c:8025/8783).
		if err := t.reparentPageOverflowChains(sp.PageNum); err != nil {
			return nil, err
		}
	}
	// (3) Replace the window's dividers.
	if err := t.removeInteriorCellRange(ctx.parent, parent, c0, c1-c0); err != nil {
		return nil, err
	}
	for i := 0; i < nNewFull-1; i++ {
		// Separator = first rowid of the RIGHT survivor (the engine's
		// strict-< seek convention — see the coversParent branch above).
		right := siblings[i+1]
		key := uint64(readFirstRowID(right.Data, contentOffset(right.PageNum), storage.CellTableLeaf, int(t.usableSize), int(t.pageSize)))
		if err := t.insertInteriorDividerAt(ctx.parent, parent, c0+i, siblings[i].PageNum, key); err != nil {
			return nil, err
		}
	}
	// (4) Repoint the child pointer that followed the window.
	last := siblings[nNewFull-1].PageNum
	followIdx := c0 + nNewFull - 1
	if followIdx < int(parent.CellCount) {
		ptrBase := parentCo + cellPtrOffset(parent.PageType)
		cp := int(binary.BigEndian.Uint16(ctx.parent.Data[ptrBase+followIdx*2 : ptrBase+followIdx*2+2]))
		binary.BigEndian.PutUint32(ctx.parent.Data[cp:cp+4], last)
	} else {
		binary.BigEndian.PutUint32(ctx.parent.Data[parentCo+8:parentCo+12], last)
	}
	if err := t.pager.WritePage(ctx.parent); err != nil {
		return nil, fmt.Errorf("balanceNonroot: write parent: %w", err)
	}
	return ctx.parent, nil
}

// removeInteriorCellRange removes count divider cells starting at index
// start from an interior page, shifting subsequent cell pointers down.
// Equivalent to SQLite's dropCell in a loop (src/btree.c dropCell).
func (t *BTree) removeInteriorCellRange(pg *pager.Page, page *storage.BTreePage, start, count int) error {
	if count <= 0 {
		return nil
	}
	if start < 0 || start+count > int(page.CellCount) {
		return fmt.Errorf("btree: removeInteriorCellRange: range [%d,%d) outside 0..%d", start, start+count, page.CellCount)
	}
	coff := contentOffset(pg.PageNum)
	ptrBase := coff + cellPtrOffset(page.PageType)
	cnt := int(page.CellCount)
	for k := start + count; k < cnt; k++ {
		src := ptrBase + k*2
		dst := ptrBase + (k-count)*2
		copy(pg.Data[dst:dst+2], pg.Data[src:src+2])
	}
	for k := cnt - count; k < cnt; k++ {
		zp := ptrBase + k*2
		pg.Data[zp] = 0
		pg.Data[zp+1] = 0
	}
	page.CellCount = uint16(cnt - count)
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	return nil
}

// insertInteriorDividerAt inserts a table-interior divider cell (4-byte
// left-child + varint rowid key) at cell index idx of an interior page.
// The cell body is allocated from the bottom of the content area and the
// cell pointer array is shifted up — the insertCell layout
// (src/btree.c insertCell, 7406+).
func (t *BTree) insertInteriorDividerAt(pg *pager.Page, page *storage.BTreePage, idx int, child uint32, key uint64) error {
	if idx < 0 || idx > int(page.CellCount) {
		return fmt.Errorf("btree: insertInteriorDividerAt: index %d outside 0..%d", idx, page.CellCount)
	}
	coff := contentOffset(pg.PageNum)
	ptrBase := coff + cellPtrOffset(page.PageType)
	// Build the divider cell: 4-byte child + varint key. The varint must
	// be SQLite's big-endian base-128 encoding (util.PutVarint) — Go's
	// binary.PutUvarint is LEB128 and decodes as garbage for keys >= 128
	// (rowid 210 read back as 10497), breaking every seek past the page
	// split point.
	var cell [13]byte
	binary.BigEndian.PutUint32(cell[0:4], child)
	n := util.PutVarint(cell[4:13], key)
	sz := 4 + n
	// Allocate from the bottom of the content area. The divider cell
	// (sz bytes) and one extra pointer slot (2 bytes) must both fit
	// between the end of the (growing) cell pointer array and the
	// (shrinking) content area — without the check the new cell lands
	// ON TOP of the pointer array (page 1 is worst: its array starts
	// at coff+12 after the 100-byte header), corrupting both. SQLite's
	// insertCell refuses via nFree accounting and the caller rebalances;
	// here the stale bytes of dividers dropped by removeInteriorCellRange
	// are reclaimed first (defragmentInterior, balance_nonroot's compacted
	// parent), then a genuine overflow is an error.
	ptrEnd := ptrBase + (int(page.CellCount)+1)*2
	cs := int(binary.BigEndian.Uint16(pg.Data[coff+5 : coff+7]))
	if cs == 0 {
		cs = 65536
	}
	if cs-sz < ptrEnd {
		if err := t.defragmentInterior(pg, page); err != nil {
			return err
		}
		cs = int(binary.BigEndian.Uint16(pg.Data[coff+5 : coff+7]))
		if cs == 0 {
			cs = 65536
		}
	}
	ns := cs - sz
	if ns < ptrEnd {
		return fmt.Errorf("btree: insertInteriorDividerAt: interior page %d has no room for a divider (cs=%d ptrEnd=%d)", pg.PageNum, cs, ptrEnd)
	}
	copy(pg.Data[ns:ns+sz], cell[:sz])
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(ns))
	// Shift the cell pointer array up by one at idx and store the new
	// pointer.
	cnt := int(page.CellCount)
	for k := cnt; k > idx; k-- {
		src := ptrBase + (k-1)*2
		dst := ptrBase + k*2
		copy(pg.Data[dst:dst+2], pg.Data[src:src+2])
	}
	binary.BigEndian.PutUint16(pg.Data[ptrBase+idx*2:ptrBase+idx*2+2], uint16(ns))
	page.CellCount = uint16(cnt + 1)
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	return nil
}

// defragmentInterior compacts an interior page's divider cells contiguously
// from the top of the content area, removing the dead bytes left by
// removeInteriorCellRange and any fragmentation — the interior analogue of
// the leaf compaction in deleteCellOnPage (btree.c defragmentPage,
// src/btree.c:2205). Cell order and pointer-array order are preserved;
// CellContent is reset to the new lowest cell offset.
func (t *BTree) defragmentInterior(pg *pager.Page, page *storage.BTreePage) error {
	coff := contentOffset(pg.PageNum)
	ptrBase := coff + cellPtrOffset(page.PageType)
	cnt := int(page.CellCount)
	// Interior cell layout: 4-byte left-child + varint key — the varint
	// length determines the on-page cell size.
	sizes := make([]int, cnt)
	data := make([][]byte, cnt)
	for i := 0; i < cnt; i++ {
		off := int(binary.BigEndian.Uint16(pg.Data[ptrBase+i*2 : ptrBase+i*2+2]))
		_, n := util.GetVarint(pg.Data[off+4:])
		sz := 4 + n
		if off+sz > len(pg.Data) {
			return fmt.Errorf("btree: defragmentInterior: cell %d out of bounds on page %d", i, pg.PageNum)
		}
		sizes[i] = sz
		data[i] = append([]byte(nil), pg.Data[off:off+sz]...)
	}
	start := int(t.pageSize)
	for i := 0; i < cnt; i++ {
		start -= sizes[i]
		copy(pg.Data[start:start+sizes[i]], data[i])
		binary.BigEndian.PutUint16(pg.Data[ptrBase+i*2:ptrBase+i*2+2], uint16(start))
	}
	page.CellContent = uint16(start)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(start))
	pg.Data[coff+7] = 0 // fragmented free bytes
	return nil
}

// readFirstRowID returns the rowid of the first cell of a table-leaf
// page (its decoded header is parsed from the raw page bytes).
func readFirstRowID(data []byte, coff int, cellType storage.CellType, usableSize, pageSize int) int64 {
	page, err := storage.ParsePage(data, pageSize, coff)
	if err != nil || page.CellCount == 0 {
		return 0
	}
	cp := storage.CellPointer(data, coff, 0, usableSize)
	c, err := storage.DecodeCell(data, int(cp), cellType, usableSize)
	if err != nil {
		return 0
	}
	return c.RowID
}

// cascadeChildless collapses interior pages that lost their last child.
// After a balance frees an emptied subtree, an interior page may be left
// with 0 cell-children and a dead rightmost pointer — SQLite's
// balance_deeper/balance_shallower collapse these upward until a live
// ancestor remains (src/btree.c:8115-8205). pnum is walked upward: a
// non-root interior page with no live children has its reference dropped
// from its own parent, is freed, and the check repeats one level up.
// The root is rewritten as an empty leaf of the matching type.
// A 0-cell interior page whose rightmost child is still live is left
// alone (the single-child collapse is the documented shallower gap).
func (t *BTree) cascadeChildless(pnum uint32) error {
	for {
		pg, err := t.pager.ReadPage(pnum)
		if err != nil {
			return err
		}
		coff := contentOffset(pnum)
		page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
		if err != nil {
			return err
		}
		if page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeInteriorIndex {
			return nil // leaf: nothing to collapse
		}
		rmp := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
		if page.CellCount != 0 || (rmp != 0 && !pager.IsPageOnFreelist(t.pager, rmp)) {
			return nil // the page still has live children
		}
		// No live children. Root: rewrite as an empty leaf and stop.
		if pnum == t.rootPage {
			if page.PageType == storage.PageTypeInteriorTable {
				pg.Data[coff] = storage.PageTypeLeafTable
			} else {
				pg.Data[coff] = storage.PageTypeLeafIndex
			}
			binary.BigEndian.PutUint16(pg.Data[coff+1:coff+3], 0)
			binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(t.pageSize))
			pg.Data[coff+7] = 0
			binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], 0)
			pager.MarkPageDirtyForVacuum(t.pager, pnum)
			return nil
		}
		// Non-root: unlink from the parent, free, and cascade upward.
		parentPgno, _, err := t.findParentByWalk(pnum)
		if err != nil {
			return nil // no parent found; nothing to unlink
		}
		parentPg, err := t.pager.ReadPage(parentPgno)
		if err != nil {
			return err
		}
		pco := contentOffset(parentPg.PageNum)
		parentPage, err := storage.ParsePage(parentPg.Data, int(t.pageSize), pco)
		if err != nil {
			return err
		}
		idx, err := t.findLeafIndexInParent(parentPg, pnum)
		if err != nil {
			return nil
		}
		if idx >= 0 {
			if err := t.removeInteriorCellRange(parentPg, parentPage, idx, 1); err != nil {
				return err
			}
		} else {
			binary.BigEndian.PutUint32(parentPg.Data[pco+8:pco+12], 0)
		}
		if err := t.pager.WritePage(parentPg); err != nil {
			return err
		}
		if err := t.freePageWithPtrmap(pnum); err != nil {
			return err
		}
		pnum = parentPgno
	}
}

// removeInteriorCell removes cell i from an interior page. The
// divider cell at index i is dropped; subsequent cells are
// shifted down.
func (t *BTree) removeInteriorCell(pg *pager.Page, page *storage.BTreePage, i int) error {
	coff := contentOffset(pg.PageNum)
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	for k := i; k < int(page.CellCount)-1; k++ {
		src := ptrBase + (k+1)*2 + 8
		dst := ptrBase + k*2 + 8
		copy(pg.Data[dst:dst+2], pg.Data[src:src+2])
	}
	// Zero the last pointer slot.
	zp := ptrBase + (int(page.CellCount)-1)*2 + 8
	pg.Data[zp] = 0
	pg.Data[zp+1] = 0
	page.CellCount--
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], page.CellCount)
	return nil
}