// Bulk cell deletion over the whole btree: the multi-pass DeleteCellsWhere
// sweep and the per-leaf rebalance hooks that keep the tree legal while
// leaves empty out.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

func (t *BTree) DeleteCellsWhere(fn func(cell *storage.Cell) bool) (int64, error) {
	t.saveAllCursors() // btree.c saveAllCursors on the delete path
	cursorRegMu.Lock()
	t.invalidateAppendCursorLocked() // a delete may remove the maximum key
	cursorRegMu.Unlock()
	var deleted int64
	// The sweep runs in passes: balanceNonroot (invoked when a leaf
	// empties) can redistribute surviving cells into a leaf that was
	// already swept earlier in this pass — and its root-absorption path
	// can demote the interior root INTO a leaf holding surviving rows —
	// so the leaf set is RE-COLLECTED at the start of every pass. The
	// previous single collection never visited rows that moved onto the
	// root page mid-sweep (DELETE FROM leaving 8 of 500 rows behind).
	// SQLite's row-by-row OP_Delete keeps its cursor position across
	// balances; this bulk sweep instead re-runs the leaf list until a
	// full pass deletes nothing, so no migrated cell is stranded.
	for {
		passDeleted, err := t.deletePass(fn)
		deleted += passDeleted
		if err != nil {
			return deleted, err
		}
		if passDeleted == 0 {
			break
		}
	}
	// P8.INCRVACUUM phase 5.5 fix: after deleting all rows, the root
	// (if interior) may have 0 cells but still carry a stale
	// rightmost-child pointer to a freed leaf. balanceNonroot's
	// "all cells vanished" branch frees the empty leaves but does NOT
	// clear the root's rightmost-child (the root collapse /
	// balance_shallower path is not implemented in this port). Without
	// this fix, a subsequent SELECT walks the stale rightmost-child,
	// hits a freed page whose first 4 bytes are a freelist chain
	// pointer (interpreted as a cell pointer), and fails with
	// "database disk image is malformed". Clear the rightmost-child
	// when the root has 0 cells; the btree is then an empty subtree
	// the cursor handles correctly (collectLeafPages returns []uint32
	// for a 0-cell interior root with rmp=0).
	if err := t.clearEmptyRootRightmost(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// deletePass sweeps every live leaf of the btree once, rebalancing emptied
// leaves as it goes, and returns the number of cells deleted during the pass.
func (t *BTree) deletePass(fn func(cell *storage.Cell) bool) (int64, error) {
	var passDeleted int64
	var leaves []uint32
	if err := t.collectLeafPages(t.rootPage, &leaves, nil); err != nil {
		return passDeleted, err
	}
	for _, leafNum := range leaves {
		// balanceNonroot may have freed this page as a surplus empty
		// sibling during an earlier iteration of this loop; a freed
		// page's first bytes are its freelist chain pointer (type byte
		// 0x00), so it must be skipped, not parsed.
		if pager.IsPageOnFreelist(t.pager, leafNum) {
			continue
		}
		n, err := t.sweepLeafCells(leafNum, fn)
		passDeleted += n
		if err != nil {
			return passDeleted, err
		}
		// P8.INCRVACUUM phase 5.5: after a leaf becomes empty,
		// rebalance it. The leaf is the rightmost child of its
		// parent (typical case for DELETE which leaves the
		// rightmost leaf empty). We invoke balanceNonroot with
		// iParentIdx = -1 (rightmost-child) and the empty leaf as
		// the "page being balanced". balanceNonroot's Phase 3
		// filter drops the empty leaf, Phase 5 frees it, and the
		// parent is rewritten to point to the next non-empty
		// sibling.
		if err := t.maybeRebalanceAfterDelete(leafNum); err != nil {
			return passDeleted, err
		}
	}
	return passDeleted, nil
}

// sweepLeafCells deletes every matching cell from one leaf, in repeated
// single-compaction passes until nothing matches. The caller has already
// skipped leaves freed as surplus empty siblings (a freed page's first bytes
// are its freelist chain pointer, not a page type).
func (t *BTree) sweepLeafCells(leafNum uint32, fn func(cell *storage.Cell) bool) (int64, error) {
	// Delete every matching cell in ONE pass (SQLite's single-sweep
	// delete). The previous per-cell loop re-parsed the page and
	// scanned from index 0 after each deletion — O(k^2) per leaf,
	// which made DELETE FROM %_segments (thousands of 4KB blob
	// rows) take ~40s (fts4merge4's between-scenario DELETE).
	var passDeleted int64
	for {
		n, err := t.deleteAllMatchingFromLeaf(leafNum, fn)
		if err != nil {
			return passDeleted, err
		}
		passDeleted += n
		if n == 0 {
			return passDeleted, nil
		}
	}
}

// clearEmptyRootRightmost collapses the btree root when it is an interior
// page with 0 cells and no live children. SQLite's btree.c::balance_shallower
// collapses the root into its only child (or converts it to a leaf when the
// last child is freed); our simplified port does not implement
// balance_shallower, so without this the root stays an interior page whose
// rightmost-child is stale. A 0-cell interior root breaks WRITES: the next
// INSERT's schema/btree path cannot insert into an interior root with no
// cells (autovacuum-2.5.1: after dropping 528 tables the sqlite_schema root
// is a 0-cell interior page and the next CREATE TABLE fails with "database
// disk image is malformed"). With 0 cells there are no cell-children, so
// when the rightmost-child is dead (zero or on the freelist) the subtree is
// empty and the root is rewritten as an empty leaf — balance_shallower's end
// state for an emptied table. When the rightmost-child is still live the
// single-child collapse (copy child content into root) is required; that is
// the unimplemented shallower path, so the root is left untouched.
func (t *BTree) clearEmptyRootRightmost() error {
	rootPg, err := t.pager.ReadPage(t.rootPage)
	if err != nil {
		return err
	}
	coff := contentOffset(rootPg.PageNum)
	page, err := storage.ParsePage(rootPg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	if page.CellCount != 0 {
		return nil
	}
	if page.PageType != storage.PageTypeInteriorTable && page.PageType != storage.PageTypeInteriorIndex {
		return nil
	}
	// Root is an interior page with 0 cells: the only possible child is
	// the rightmost-child. A live child means the subtree still holds data
	// (single-child collapse needed — the unimplemented shallower path).
	rmp := binary.BigEndian.Uint32(rootPg.Data[coff+8 : coff+12])
	if rmp != 0 && !pager.IsPageOnFreelist(t.pager, rmp) {
		return nil
	}
	// No live children: rewrite the root as an empty leaf of the matching
	// kind (interior table -> leaf table, interior index -> leaf index),
	// with the cell-content pointer at the usable end and no fragmentation.
	if page.PageType == storage.PageTypeInteriorTable {
		rootPg.Data[coff] = storage.PageTypeLeafTable
	} else {
		rootPg.Data[coff] = storage.PageTypeLeafIndex
	}
	binary.BigEndian.PutUint16(rootPg.Data[coff+1:coff+3], 0)
	// zeroPage parity: the empty page's content pointer is the USABLE end
	// (src/btree.c:2189), not the page end — with a per-page reserve the
	// page-end anchor pushes the first re-inserted cell into the reserved
	// tail (reservebytes-1.3.2).
	binary.BigEndian.PutUint16(rootPg.Data[coff+5:coff+7], uint16(t.usableSize))
	rootPg.Data[coff+7] = 0
	binary.BigEndian.PutUint32(rootPg.Data[coff+8:coff+12], 0)
	pager.MarkPageDirtyForVacuum(t.pager, t.rootPage)
	return nil
}

// maybeRebalanceAfterDelete runs balanceNonroot on a leaf that may
// have become empty after a delete. The leaf must be a child of
// an interior page (i.e. not the root of the btree); the root
// being a leaf is handled by the caller (Clear/Clear-like paths).
// Index leaves are never routed through balanceNonroot (which only
// handles table leaves): an emptied index leaf stays in place —
// walkable and format-valid — rather than being unlinked from its
// parent (see the leaf-index branch below).
func (t *BTree) maybeRebalanceAfterDelete(leafNum uint32) error {
	// Only rebalance if the leaf became empty.
	leafPg, err := t.pager.ReadPage(leafNum)
	if err != nil {
		return err
	}
	leafCo := contentOffset(leafNum)
	leafPage, err := storage.ParsePage(leafPg.Data, int(t.pageSize), leafCo)
	if err != nil {
		return err
	}
	if leafPage.CellCount != 0 {
		return nil
	}
	if leafPage.PageType == storage.PageTypeLeafIndex {
		// An emptied INDEX leaf stays in place (walkable, format-valid):
		// reclaiming it needs findParentByWalk, an O(pages) BFS through a
		// cold page cache — per emptied leaf that is quadratic for mass
		// deletes on value-ordered trees (temptable2 3.2.1: 2947 emptied
		// leaves x a 60k-page walk through cache_size=10 = minutes). The
		// root-level empty case is still handled by
		// DeleteIndexEntries' clearEmptyRootRightmost.
		return nil
	}
	// The leaf is a child of some parent. We need to find it.
	// The btree.c approach uses the pointer map; we use the
	// tree walk (findParentByWalk) for now.
	parentPgno, _, err := t.findParentByWalk(leafNum)
	if err != nil {
		return nil
	}
	parentPg, err := t.pager.ReadPage(parentPgno)
	if err != nil {
		return err
	}
	// Determine iParentIdx: the index of the leaf in the parent's
	// cell pointer array, or -1 if it's the rightmost-child.
	iParentIdx, err := t.findLeafIndexInParent(parentPg, leafNum)
	if err != nil {
		return err
	}
	ctx := &balanceNonrootContext{
		parent:     parentPg,
		iParentIdx: iParentIdx,
		page:       leafPg,
		isRoot:     parentPgno == t.rootPage,
	}
	_, err = t.balanceNonroot(ctx)
	return err
}

// PageNum returns the cursor's current leaf page number (valid after a
// successful seek; the point-statement paths use it to re-address the
// seeked row without a second descent).
func (c *Cursor) PageNum() uint32 { return c.pageNum }

// CellIdx returns the cursor's current cell index within its page (valid
// after a successful seek).
func (c *Cursor) CellIdx() int { return c.cellIdx }

// PathParent returns the deepest interior page on the cursor's seek path —
// the leaf's parent when the seek landed on a leaf below an interior level,
// 0 otherwise (root leaf). The point-delete rebalance uses it as a verified
// parent hint.
func (c *Cursor) PathParent() uint32 {
	if len(c.path) == 0 {
		return 0
	}
	return c.path[len(c.path)-1].pageNum
}

// DeleteCellByRowIDAt is DeleteCellByRowID for a row whose leaf position a
// caller-seeked cursor already established: the same single-cell fast path,
// the same rebalance hint, the same generic-predicate fallback on a declined
// fast path — but no second root-to-leaf descent. positionOK must come from
// a seek on THIS tree (the target cell is re-validated against rowID; a
// stale position falls back to the full DeleteCellByRowID seek).
func (t *BTree) DeleteCellByRowIDAt(rowID int64, leaf uint32, idx int, hintParent uint32) (int64, error) {
	t.saveAllCursors() // btree.c saveAllCursors on the delete path
	cursorRegMu.Lock()
	t.invalidateAppendCursorLocked() // a delete may remove the maximum key
	cursorRegMu.Unlock()
	handled, n, ferr := t.deleteSingleTableRowID(leaf, idx, rowID)
	if ferr != nil {
		return n, ferr
	}
	if handled {
		if n == 0 {
			return n, nil
		}
		if err := t.maybeRebalanceAfterDeleteHinted(leaf, hintParent); err != nil {
			return n, err
		}
		return n, nil
	}
	// Fast path declined (anomaly on the leaf): the same generic predicate
	// delete DeleteCellByRowID would run, on the same leaf.
	n, err := t.deleteAllMatchingFromLeaf(leaf, func(cell *storage.Cell) bool {
		return cell.RowID == rowID
	})
	if err != nil || n == 0 {
		return n, err
	}
	if err := t.maybeRebalanceAfterDeleteHinted(leaf, hintParent); err != nil {
		return n, err
	}
	return n, nil
}

// DeleteCellByRowID deletes the single table-leaf cell with the given rowid
// using a direct O(log n) cursor seek instead of DeleteCellsWhere's full
// sweep (a per-row sweep made UPDATE loops O(rows x tree): sqllimits1-7.5's
// trigger cascade under rollback protection). Returns the number of cells
// deleted (0 when the rowid is absent).
func (t *BTree) DeleteCellByRowID(rowID int64) (int64, error) {
	t.saveAllCursors() // btree.c saveAllCursors on the delete path
	cursorRegMu.Lock()
	t.invalidateAppendCursorLocked() // a delete may remove the maximum key
	cursorRegMu.Unlock()
	c, err := t.OpenCursor()
	if err != nil {
		return 0, err
	}
	found, err := c.SeekToRowID(rowID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	leaf := c.pageNum
	// The seek's path stack names the leaf's parent (its last interior
	// level); after the delete it feeds the rebalance lookup, sparing the
	// O(pages) whole-database parent walk per emptied leaf. The hint is
	// verified against the live tree before use and falls back to the walk.
	hintParent := uint32(0)
	if len(c.path) > 0 {
		hintParent = c.path[len(c.path)-1].pageNum
	}
	// Single-cell fast path: one known cell on a table leaf is removed with a
	// raw-span repack instead of a decode/re-encode of every cell on the page
	// (btree_delete_one.go). Any anomaly declines and the generic predicate
	// delete below runs, so behavior — including on corrupt images and
	// duplicate-rowid leaves — stays identical to it.
	handled, n, ferr := t.deleteSingleTableRowID(leaf, c.cellIdx, rowID)
	if ferr != nil {
		return n, ferr
	}
	if handled {
		if n == 0 {
			return n, nil
		}
		if err := t.maybeRebalanceAfterDeleteHinted(leaf, hintParent); err != nil {
			return n, err
		}
		return n, nil
	}
	n, err = t.deleteAllMatchingFromLeaf(leaf, func(cell *storage.Cell) bool {
		return cell.RowID == rowID
	})
	if err != nil || n == 0 {
		return n, err
	}
	if err := t.maybeRebalanceAfterDeleteHinted(leaf, hintParent); err != nil {
		return n, err
	}
	return n, nil
}

// maybeRebalanceAfterDeleteHinted is maybeRebalanceAfterDelete with a
// candidate parent page for the emptied leaf (the seek path's last interior
// level). The hint is only a shortcut to the same lookup: it is accepted
// only when the leaf is still reachable from that exact parent right now
// (findLeafIndexInParent verifies), and every miss — stale hint, freed
// parent, root-level leaf — falls through to the unchanged
// findParentByWalk logic.
func (t *BTree) maybeRebalanceAfterDeleteHinted(leafNum, hintParent uint32) error {
	if hintParent == 0 || hintParent == leafNum {
		return t.maybeRebalanceAfterDelete(leafNum)
	}
	leafPg, err := t.pager.ReadPage(leafNum)
	if err != nil {
		return err
	}
	leafCo := contentOffset(leafNum)
	leafPage, err := storage.ParsePage(leafPg.Data, int(t.pageSize), leafCo)
	if err != nil {
		return err
	}
	if leafPage.CellCount != 0 {
		return nil
	}
	if leafPage.PageType == storage.PageTypeLeafIndex {
		// An emptied INDEX leaf stays in place — see
		// maybeRebalanceAfterDelete.
		return nil
	}
	// Match findParentByWalk's refusal exactly: a page that is the ROOT of
	// any tree is never rebalanced through a parent (the walk answers
	// "page is a root" and maybeRebalanceAfterDelete no-ops). Routing back
	// through the walk on that case — and when the schema roots cannot be
	// enumerated, which the walk treats as "not found" — keeps the hinted
	// path byte-for-byte aligned with it.
	roots, rerr := t.collectSchemaRoots()
	if rerr != nil {
		return t.maybeRebalanceAfterDelete(leafNum)
	}
	for _, r := range roots {
		if r == leafNum {
			return t.maybeRebalanceAfterDelete(leafNum)
		}
	}
	parentPg, err := t.pager.ReadPage(hintParent)
	if err != nil {
		// The hinted parent is gone: fall back to the walk.
		return t.maybeRebalanceAfterDelete(leafNum)
	}
	iParentIdx, err := t.findLeafIndexInParent(parentPg, leafNum)
	if err != nil {
		// The leaf is not reachable from the hint (stale path): fall back.
		return t.maybeRebalanceAfterDelete(leafNum)
	}
	ctx := &balanceNonrootContext{
		parent:     parentPg,
		iParentIdx: iParentIdx,
		page:       leafPg,
		isRoot:     hintParent == t.rootPage,
	}
	_, err = t.balanceNonroot(ctx)
	return err
}

// findLeafIndexInParent returns the cell-pointer index of leaf in
// parentPg's cell array, or -1 if leaf is the rightmost-child.
// Returns 0 for the leftmost cell-child.
func (t *BTree) findLeafIndexInParent(parentPg *pager.Page, leafNum uint32) (int, error) {
	coff := contentOffset(parentPg.PageNum)
	page, err := storage.ParsePage(parentPg.Data, int(t.pageSize), coff)
	if err != nil {
		return 0, err
	}
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	for i := 0; i < int(page.CellCount); i++ {
		cp := storage.CellPointer(parentPg.Data, ptrBase, i, int(t.pageSize))
		if int(cp)+4 > len(parentPg.Data) {
			continue
		}
		child := binary.BigEndian.Uint32(parentPg.Data[cp : cp+4])
		if child == leafNum {
			return i, nil
		}
	}
	// Not a cell-child; check the rightmost-child.
	rmp := binary.BigEndian.Uint32(parentPg.Data[coff+8 : coff+12])
	if rmp == leafNum {
		return -1, nil
	}
	return 0, fmt.Errorf("leaf %d not found in parent %d", leafNum, parentPg.PageNum)
}
