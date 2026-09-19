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
		handled, err := t.balanceAllEmptyWindow(ctx, parent, siblings, c0, c1, parentCo)
		if err != nil || handled {
			return ctx.parent, err
		}
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
	if coversParent && nNewFull == 1 {
		handled, err := t.balanceCoversSingleSurvivor(ctx, parent, siblings, c0, c1, parentCo)
		if err != nil || handled {
			return ctx.parent, err
		}
	}
	if coversParent {
		for _, sp := range siblings[nNewFull:] {
			if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
				return nil, err
			}
		}
		children := make([]uint32, 0, nNewFull)
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
		}
		// Separator convention of this engine's splits: the divider key is the
		// LAST rowid of the LEFT subtree (sqlite3BtreeTableMoveto at
		// btree.c:5877 + leafData splitter at btree.c:8813). seekInInteriorTable
		// routes keys <= K to the divider's left child and keys > K to the
		// following subtree, which is exactly the boundary the seek code
		// expects. (Earlier the engine used the RIGHT subtree's first rowid
		// and routed keys < K left, which kept the engine self-consistent
		// but produced files that sqlite3 integrity_check rejected with
		// "right child Rowid N out of order" on every table btree split;
		// see incrvacuum2 4.1.) The keys are read in a SECOND loop, after
		// every survivor has been rebuilt: reading siblings[i+1] during
		// the rebuild loop picked up the PRE-rebuild page state (an emptied
		// leaf yielded separator 0), producing out-of-order dividers that
		// then scrambled the parent rewrite (rows dropped from the tree —
		// BUG C).
		seps := make([]uint64, 0, nNewFull)
		for i := 0; i < nNewFull-1; i++ {
			seps = append(seps, uint64(readLastRowID(siblings[i].Data, contentOffset(siblings[i].PageNum), storage.CellTableLeaf, int(t.usableSize), int(t.pageSize))))
		}
		// Uniform parent rebuild (balance_nonroot tail): one divider
		// per survivor boundary, last survivor is the rightmost child.
		// A single survivor leaves the parent with 0 dividers over one
		// child — a passthrough husk — which cascadeChildless splices
		// (balance_shallower); with more survivors the parent keeps
		// nNew-1 >= 1 dividers and the cascade is a no-op.
		if err := t.writeInteriorRootAt(ctx.parent.PageNum, children, seps); err != nil {
			return nil, err
		}
		if err := t.cascadeChildless(ctx.parent.PageNum); err != nil {
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
		// Separator = LAST rowid of the LEFT survivor (SQLite's leafData
		// boundary convention — paired with seekInInteriorTable's <= K
		// routes-left rule). The coversParent branch above uses the same
		// convention for the multi-sibling write.
		left := siblings[i]
		key := uint64(readLastRowID(left.Data, contentOffset(left.PageNum), storage.CellTableLeaf, int(t.usableSize), int(t.pageSize)))
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
// Equivalent to SQLite's dropCell in a loop (src/btree.c dropCell). The
// dropped cells' bytes cannot simply stay in place: any untracked byte
// region inside the cell content area (between live cells) is flagged by
// SQLite's integrity_check as fragmentation ("Fragmentation of N bytes
// reported as M", the coverage walk at src/btree.c:11004-11064) — so the
// page is defragmented after the pointer shift (defragmentPage parity,
// the same end state dropCell→freeSpace + a later allocateSpace
// defragment reaches). Interior cells in this engine are 4-byte
// left-child + varint key with no overflow chain, so nothing is leaked
// to the freelist.
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
	// Reclaim the dropped cells' bytes: repack the surviving dividers
	// contiguously from the usable end so the content area holds no
	// untracked holes (integrity_check coverage parity).
	return t.defragmentInterior(pg, page)
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
	// length determines the on-page cell size (uniform for table and index
	// interiors in this engine; dividers carry no payload/overflow).
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
	start := int(t.usableSize)
	for i := 0; i < cnt; i++ {
		start -= sizes[i]
		copy(pg.Data[start:start+sizes[i]], data[i])
		binary.BigEndian.PutUint16(pg.Data[ptrBase+i*2:ptrBase+i*2+2], uint16(start))
	}
	// Full header reset (zeroPage parity): a page handed back by the
	// freelist as a cached buffer from an earlier incarnation can carry
	// a stale freeblock pointer that would now overlap the packed cells
	// ("Multiple uses for byte N"); the engine never maintains a
	// freeblock chain, so after a defragment there is none.
	binary.BigEndian.PutUint16(pg.Data[coff+1:coff+3], 0) // first freeblock
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

// readLastRowID returns the rowid of the LAST cell of a table-leaf page.
// Used by balanceNonroot's divider convention: each separator between
// sibling i and sibling i+1 is the LAST rowid of sibling i (sqlite3
// BTreeTableMoveto routes keys <= K to the left subtree, so K must be
// the inclusive upper bound of that left subtree).
func readLastRowID(data []byte, coff int, cellType storage.CellType, usableSize, pageSize int) int64 {
	page, err := storage.ParsePage(data, pageSize, coff)
	if err != nil || page.CellCount == 0 {
		return 0
	}
	cp := storage.CellPointer(data, coff, int(page.CellCount)-1, usableSize)
	c, err := storage.DecodeCell(data, int(cp), cellType, usableSize)
	if err != nil {
		return 0
	}
	return c.RowID
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
		// Root absorption (balance-shallower, btree.c:8918-8943): the
		// root's whole subtree is dead — rewrite the root (which keeps
		// its page number; schema entries reference it) as an empty leaf
		// and free every child.
		for _, sp := range siblings {
			if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
				return true, err
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
		return true, nil
	}
	if fullWindow {
		// Non-root parent whose whole child set is in the window: the
		// parent's subtree is dead — free every child, drop every divider
		// and clear the rightmost pointer, and let the dead-child cascade
		// unlink the parent itself (balance()'s upward walk). Keeping any
		// emptied child alive under a divider is NOT an option:
		// moveToChild (btree.c:77872) rejects every descended page with
		// nCell<1, so an empty leaf may never sit below an interior page.
		for _, sp := range siblings {
			if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
				return true, err
			}
		}
		binary.BigEndian.PutUint32(ctx.parent.Data[parentCo+8:parentCo+12], 0)
		binary.BigEndian.PutUint16(ctx.parent.Data[parentCo+3:parentCo+5], 0)
		if err := t.pager.WritePage(ctx.parent); err != nil {
			return true, fmt.Errorf("balanceNonroot: write parent: %w", err)
		}
		if err := t.cascadeChildless(ctx.parent.PageNum); err != nil {
			return true, err
		}
		return true, nil
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
	cellCounts := make([]int, len(siblings))
	survivor := -1 // first child with cells, -1 when none
	for i, sp := range siblings {
		spPage, perr := storage.ParsePage(sp.Data, int(t.pageSize), contentOffset(sp.PageNum))
		if perr != nil {
			return true, perr
		}
		cellCounts[i] = int(spPage.CellCount)
		if spPage.CellCount > 0 && survivor < 0 {
			survivor = i
		}
	}
	if survivor < 0 {
		return true, fmt.Errorf("balanceNonroot: coversParent with no surviving cells")
	}
	kept := make([]int, 0, 3)
	for i := range siblings {
		if cellCounts[i] > 0 {
			kept = append(kept, i)
		}
	}
	// Free every window child that is not kept. Kept children hold cells,
	// so every child reference that survives points at a page with
	// nCell>=1 (moveToChild, btree.c:77872, rejects descended pages with
	// nCell<1 — empty leaves may never sit below an interior page).
	keptSet := make(map[int]bool, len(kept))
	for _, i := range kept {
		keptSet[i] = true
	}
	for i, sp := range siblings {
		if !keptSet[i] {
			if err := t.freePageWithPtrmap(sp.PageNum); err != nil {
				return true, err
			}
		}
	}
	if len(kept) >= 2 {
		// Rewrite the parent over the kept (non-empty) children under
		// their ORIGINAL dividers: cells do not move, keys stay
		// monotonic, all children keep their depth, and the parent keeps
		// kept-1 >= 1 dividers. This case must run BEFORE the root
		// absorption below — absorbing only the FIRST non-empty child
		// while freeing the rest destroyed every other non-empty sibling
		// (18 schema rows lost in autovacuum-2.4.7's DROP loop).
		children := make([]uint32, 0, len(kept))
		seps := make([]uint64, 0, len(kept))
		for _, i := range kept {
			children = append(children, siblings[i].PageNum)
			if i != kept[len(kept)-1] {
				seps = append(seps, t.cellKeyAt(ctx.parent, ptrBase, c0+i))
			}
		}
		if err := t.writeInteriorRootAt(ctx.parent.PageNum, children, seps); err != nil {
			return true, err
		}
		return true, nil
	}
	if ctx.isRoot {
		// Single non-empty child of the ROOT: balance_shallower
		// (btree.c:8918-8943) absorbs it into the root page — the root
		// keeps its page number and the emptied child is freed. When the
		// child's content exceeds the root's (smaller) usable area the
		// absorption is skipped, exactly like C's hdrOffset<=nFree guard,
		// and the root stays an interior page over its single child.
		if err := t.absorbSingleChildRoot(ctx.parent, parentCo, siblings[survivor].PageNum); err != nil {
			if err == errRootAbsorbNoFit {
				return true, nil
			}
			return true, err
		}
		return true, nil
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
	borrowed := false
	if gpgno, _, gerr := t.findParentByWalk(ctx.parent.PageNum); gerr == nil {
		gpg, rerr := t.pager.ReadPage(gpgno)
		if rerr == nil {
			gcoff := contentOffset(gpg.PageNum)
			gpage, perr := storage.ParsePage(gpg.Data, int(t.pageSize), gcoff)
			if perr == nil {
				gn := int(gpage.CellCount)
				gbase := gcoff + cellPtrOffset(gpage.PageType)
				pidx, ferr := t.findLeafIndexInParent(gpg, ctx.parent.PageNum)
				if ferr == nil {
					// The parent's OWN bound at the grandparent: the divider
					// key routing keys <= K_P to the parent (absent when the
					// parent is the grandparent's rightmost child).
					var kPOld uint64
					hasKPOld := false
					if pidx >= 0 {
						kPOld = t.cellKeyAt(gpg, gbase, pidx)
						hasKPOld = true
					}
					// candidate donor: the interior sibling IMMEDIATELY to
					// the right of the parent (a LEFT donor is impossible:
					// the left sibling's outermost child holds keys ABOVE
					// the right sibling's remaining rows — moving it into
					// the parent's low side breaks divider/leaf monotonic
					// order, "Rowid N out of order" in sqlite3
					// integrity_check — C's redistribution instead repacks
					// the whole window's boundaries monotonically).
					type cand struct {
						pgno uint32
						left bool
					}
					var cands []cand
					if pidx >= 0 {
						if pidx+1 <= gn-1 {
							cp := storage.CellPointer(gpg.Data, gbase-8, pidx+1, int(t.pageSize))
							cands = append(cands, cand{binary.BigEndian.Uint32(gpg.Data[cp : cp+4]), false})
						} else {
							r := binary.BigEndian.Uint32(gpg.Data[gcoff+8 : gcoff+12])
							if r != 0 {
								cands = append(cands, cand{r, false})
							}
						}
					}
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
						// The donor's outermost divider (child + key).
						var xi int
						if cd.left {
							xi = int(dpage.CellCount) - 1
						}
						xoff := int(binary.BigEndian.Uint16(dpg.Data[dcoff+cellPtrOffset(dpage.PageType)+xi*2:]))
						x := binary.BigEndian.Uint32(dpg.Data[xoff : xoff+4])
						k, _ := util.GetVarint(dpg.Data[xoff+4:])
						if cd.left {
							// Borrowed child holds the donor's LOWEST keys,
							// below every surviving key: it becomes the
							// parent's FIRST child, bounded by its own key.
							// The grandparent's divider for the parent (K_P,
							// which still bounds the surviving child and now
							// X too) is unchanged.
							if err := t.removeInteriorCellRange(dpg, dpage, xi, 1); err != nil {
								return true, err
							}
							if err := t.writeInteriorRootAt(ctx.parent.PageNum, []uint32{x, siblings[survivor].PageNum}, []uint64{k}); err != nil {
								return true, err
							}
						} else {
							// Borrowed child holds keys just ABOVE the
							// parent's old bound K_P: it becomes the parent's
							// LAST child. The parent's internal divider stays
							// K_P (bounding the surviving child), and the
							// GRANDPARENT'S divider for the parent is re-keyed
							// from K_P to K so the borrowed keys route into
							// the parent.
							if err := t.removeInteriorCellRange(dpg, dpage, 0, 1); err != nil {
								return true, err
							}
							if err := t.writeInteriorRootAt(ctx.parent.PageNum, []uint32{siblings[survivor].PageNum, x}, []uint64{kPOld}); err != nil {
								return true, err
							}
							if hasKPOld {
								if err := t.removeInteriorCellRange(gpg, gpage, pidx, 1); err != nil {
									return true, err
								}
								if err := t.insertInteriorDividerAt(gpg, gpage, pidx, ctx.parent.PageNum, k); err != nil {
									return true, err
								}
								if err := t.pager.WritePage(gpg); err != nil {
									return true, err
								}
							}
						}
						if err := t.pager.WritePage(dpg); err != nil {
							return true, err
						}
						borrowed = true
						break
					}
				}
			}
		}
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
	return true, nil
}
