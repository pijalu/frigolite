// Package btree implements a B+Tree on top of the pager.
// This file holds the insert path's orchestration: routing a new cell to the
// leaf/interior handler, dropping same-key table cells before insert, and
// the retry loop that re-balances a full parent interior page.
package btree

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// InsertCell inserts a cell into the b-tree.
// Uses a recursive insert with proper split propagation for multi-level trees.
// When a page splits, the split key and new sibling propagate up to the parent.
func (t *BTree) InsertCell(newCell *storage.Cell) error {
	// parentPgno=0: the root has no parent; split allocations on the root
	// path are parented to the root page itself (btree.c balance_deeper
	// ptrmapPut(pBt, pgnoChild, PTRMAP_BTREE, pRoot->pgno), src/btree.c:9028).
	splits, err := t.insertPage(t.rootPage, 0, newCell)
	if err != nil {
		return err
	}
	if len(splits) > 0 {
		// Root page split.
		if t.rootPage != 1 {
			// btree.c balance_deeper keeps the root page as the root: its
			// page number never changes (schema entries stay valid) and the
			// two halves move to freshly allocated pages in ascending order.
			return t.relocateRootSplit(splits)
		}
		// The schema b-tree (sqlite_schema) is permanently rooted at page 1:
		// page 1 is the database file header page and cannot be demoted to a
		// child. When its root splits, page 1 becomes an interior page and
		// the split halves are moved to newly allocated pages.
		rootPg, err := t.createInteriorRoot(t.rootPage, splits[0].medianKey, splits[0].pageNum)
		if err != nil {
			return err
		}
		for i := 1; i < len(splits); i++ {
			if err := t.addInteriorCellToPage(rootPg.PageNum, splits[i-1].pageNum, splits[i].medianKey, splits[i].pageNum); err != nil {
				return err
			}
		}
		t.rootPage = rootPg.PageNum
	}

	return nil
}

// insertPage inserts a cell into the page at pageNum.
// Returns the new pages produced by a split (each with the median key
// separating it from the previous page), or nil when no split occurred. The
// caller must add each (pageNum, medianKey, newPage) pointer to the parent
// interior page.
func (t *BTree) insertPage(pageNum uint32, parentPgno uint32, newCell *storage.Cell) ([]leafSplitResult, error) {
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return nil, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return nil, err
	}

	switch page.PageType {
	case storage.PageTypeLeafTable, storage.PageTypeLeafIndex:
		return t.insertLeafPage(pg, page, parentPgno, newCell)
	case storage.PageTypeInteriorTable, storage.PageTypeInteriorIndex:
		return t.insertInteriorPage(pg, page, parentPgno, newCell)
	default:
		return nil, fmt.Errorf("btree: unknown page type 0x%02x", page.PageType)
	}
}

// insertLeafPage inserts a cell into a leaf page. If the leaf is full, it
// splits (possibly into multiple pages). Returns the new pages produced by
// the split (empty when the cell was inserted in place).
func (t *BTree) insertLeafPage(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell) ([]leafSplitResult, error) {
	coff := contentOffset(pg.PageNum)
	// Optimistically parent the overflow chain to this page — SQLite's
	// fillInCell runs on the page the cell lands on; if the split below
	// moves the cell to a new page, splitLeafMulti re-parents the chain
	// (ptrmapPutOvflPtr, src/btree.c:8025).
	if err := t.prepareCell(newCell, pg.PageNum); err != nil {
		return nil, err
	}
	cellData := storage.EncodeCell(newCell)

	// Table b-trees REPLACE a cell with the same rowid (SQLite btree.c
	// sqlite3BtreeInsert, loc==0: dropCell runs BEFORE insertCellFast, so the
	// later balance()/split never sees two equal keys). Drop the old cell
	// FIRST, on every path — including the full-page split path: leaving it
	// for writeLeafCell only meant a full page redistributed the old cell
	// AND the new cell via splitLeafMulti, duplicating the rowid
	// (fts4merge4 2.2.x: duplicate %_segdir rows).
	if t.isTable {
		if err := t.dropTableLeafDuplicateRowid(pg, page, newCell.RowID); err != nil {
			return nil, err
		}
	}

	if leafHasRoom(pg, page, cellData, coff, t.usableSize) {
		// There is room — insert directly.
		if err := t.writeLeafCell(pg, page, newCell, cellData, coff); err != nil {
			return nil, err
		}
		return nil, nil
	}

	return t.splitFullLeaf(pg, page, parentPgno, newCell, cellData)
}

// dropTableLeafDuplicateRowid deletes an existing table-leaf cell with the
// same rowid as the incoming cell (a no-op when the cell at the insert
// position has a different rowid).
func (t *BTree) dropTableLeafDuplicateRowid(pg *pager.Page, page *storage.BTreePage, rowID int64) error {
	coff := contentOffset(pg.PageNum)
	idx := t.findInsertPositionTable(pg, page, rowID)
	if idx >= int(page.CellCount) {
		return nil
	}
	if t.tableLeafRowidAt(pg, coff, idx) != rowID {
		return nil
	}
	return t.deleteCellOnPage(pg, page, idx)
}

// splitFullLeaf handles a full leaf: a ROOT whose usable area cannot hold the
// cell even when empty reconciles through balance_deeper; everything else
// splits across multiple pages (splitLeafMulti).
func (t *BTree) splitFullLeaf(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell, cellData []byte) ([]leafSplitResult, error) {
	coff := contentOffset(pg.PageNum)
	// Leaf is full. If the cell's local form cannot fit this page even when
	// the page is EMPTY, no redistribution can ever place it here: the page
	// is a ROOT whose usable area is smaller than the largest legal cell.
	// The live case is page 1 (sqlite_schema's permanent root): its content
	// area loses 100 bytes to the database file header, so a fully-local
	// cell of up to maxLocal bytes satisfies the file-format formula yet
	// exceeds page 1's area (corrupt-5.2/misc1 manycol: ~100 columns at
	// page_size=1024). SQLite keeps the formula-mandated local size and
	// reconciles through balance_deeper (src/btree.c:9010) — the root's
	// content moves to a fresh child leaf and the root becomes interior.
	if parentPgno == 0 && !leafCellsFit([][]byte{cellData}, coff, int(t.usableSize)) {
		return t.balanceDeeperRootLeaf(pg, page, newCell, cellData, coff)
	}
	if page.CellCount == 0 {
		// Unreachable by geometry: a fresh non-root leaf's content area
		// (pageSize-8-2) exceeds the largest legal cell (pageSize-20).
		// Kept as a guard against non-root oversize cells.
		return nil, fmt.Errorf("btree: cell too large for page (size=%d, pageSize=%d)", len(cellData), t.pageSize)
	}

	// Split the leaf, distributing existing cells plus the new cell across
	// as many pages as needed (the new cell is already written by the split).
	results, err := t.splitLeafMulti(pg, page, parentPgno, newCell, cellData)
	if err != nil {
		return nil, err
	}
	if err := t.pager.WritePage(pg); err != nil {
		return nil, err
	}
	for _, r := range results {
		newPg, rerr := t.pager.ReadPage(r.pageNum)
		if rerr != nil {
			return nil, rerr
		}
		if err := t.pager.WritePage(newPg); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// insertInteriorPage inserts a cell into an interior page by routing to the
// correct child. If the child splits, adds the new pointers. If this interior
// page is then full, splits it too.
func (t *BTree) insertInteriorPage(pg *pager.Page, page *storage.BTreePage, parentPgno uint32, newCell *storage.Cell) ([]leafSplitResult, error) {
	// Find the child page that should receive the new cell
	childPageNum := t.findChildPageForInsert(pg, page, newCell)

	// Recursively insert into the child; the child's parent is this page.
	childSplits, err := t.insertPage(childPageNum, pg.PageNum, newCell)
	if err != nil {
		return nil, err
	}

	if len(childSplits) == 0 {
		// No split occurred — done
		return nil, nil
	}

	// Child split occurred. Apply the separator chain to this interior page.
	if err := t.applyChildSplits(pg, page, childPageNum, childSplits); err != nil {
		if err != errInteriorFull {
			return nil, err
		}
		// This interior page is full. SQLite's balance_nonroot keeps creating
		// sibling pages until the overfull page has absorbed the pending
		// divider cells (btree.c: the do-while over apCell redistributes cells
		// across as many siblings as needed); a single tail split frees only
		// one cell slot, which is not always enough for the exact room the
		// separator chain requires (fts4opt churn: the retried apply hit
		// precheck-noroom again and the insert failed outright). Mirror the
		// loop: split the left page repeatedly, each split moving its last
		// cell to a fresh sibling, until the half that owns the split child
		// can take the chain.
		return t.retryChildSplitApply(pg, parentPgno, childPageNum, childSplits)
	}

	return nil, nil
}

// retryChildSplitApply repeatedly splits the left interior page, each split
// moving its last cell to a fresh sibling, until the half that owns the split
// child can take the separator chain. Successive left-page splits nest
// rightward (split #2's page sorts between the left page and split #1's
// page), so the dividers returned to the parent are in reverse accumulation
// order and are handed up left-to-right.
func (t *BTree) retryChildSplitApply(pg *pager.Page, parentPgno, childPageNum uint32, childSplits []leafSplitResult) ([]leafSplitResult, error) {
	var outs []leafSplitResult
	for tries := 0; tries < 4096; tries++ {
		// Re-parse the left page: the previous iteration's split rewrote
		// pg.Data in place, so the caller's parsed `page` header (cell
		// count/content start) is stale.
		coff := contentOffset(pg.PageNum)
		page, perr := storage.ParsePage(pg.Data, int(t.pageSize), coff)
		if perr != nil {
			return nil, perr
		}
		newInteriorNum, splitKey, serr := t.splitInteriorPage(pg, page, parentPgno)
		if serr != nil {
			return nil, serr
		}
		outs = append(outs, leafSplitResult{pageNum: newInteriorNum, medianKey: splitKey})
		order := childOrderAmongSplits(pg, outs)
		done, aerr := t.applyChildSplitsToFirstFit(order, childPageNum, childSplits)
		if aerr != nil {
			return nil, aerr
		}
		if done {
			// The chain found a home; hand every divider up in
			// left-to-right order.
			rev := make([]leafSplitResult, len(outs))
			for i := range outs {
				rev[i] = outs[len(outs)-1-i]
			}
			return rev, nil
		}
		// Still full: balance further and try again. The apply is atomic
		// (exact precheck before any mutation), so the retry sees a clean
		// page.
	}
	return nil, fmt.Errorf("btree: interior rebalance did not converge (page %d)", pg.PageNum)
}

// childOrderAmongSplits returns the original left page followed by every
// split sibling, left to right.
func childOrderAmongSplits(pg *pager.Page, outs []leafSplitResult) []uint32 {
	order := make([]uint32, 0, len(outs)+1)
	order = append(order, pg.PageNum)
	for i := len(outs) - 1; i >= 0; i-- {
		order = append(order, outs[i].pageNum)
	}
	return order
}

// applyChildSplitsToFirstFit locates the original child among the ordered
// pages and applies its split chain to the page that holds it. Returns
// done=true once the chain finds a home; a false return with nil error means
// every candidate page was still errInteriorFull and the caller must balance
// further.
func (t *BTree) applyChildSplitsToFirstFit(order []uint32, childPageNum uint32, childSplits []leafSplitResult) (bool, error) {
	target, ferr := t.locateChildAmong(order, childPageNum)
	if ferr != nil {
		return false, ferr
	}
	if target == 0 {
		return false, fmt.Errorf("btree: parent split lost child %d", childPageNum)
	}
	tp, rerr := t.pager.ReadPage(target)
	if rerr != nil {
		return false, rerr
	}
	tpage, perr := storage.ParsePage(tp.Data, int(t.pageSize), contentOffset(tp.PageNum))
	if perr != nil {
		return false, perr
	}
	if aerr := t.applyChildSplits(tp, tpage, childPageNum, childSplits); aerr == nil {
		return true, nil
	} else if aerr != errInteriorFull {
		return false, aerr
	}
	return false, nil
}
