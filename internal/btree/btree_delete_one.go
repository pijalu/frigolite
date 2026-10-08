package btree

// Single-cell table-leaf delete: DeleteCellByRowID's fast path. The generic
// deleteAllMatchingFromLeaf rewrites a leaf by decoding AND re-encoding every
// cell of the page (defensible for predicate sweeps that remove many cells,
// wasteful by ~N decodes when exactly one known cell goes away). The fast
// path performs btree.c dropCell's O(1) removal instead (src/btree.c:7228,
// see btree_page_space.go): the cell's bytes return to the page's freeblock
// chain / fragmented-byte count and the pointer slot is unlinked — no
// survivor byte moves. Any anomaly (parse failure, malformed cell, a
// duplicate rowid on the same leaf) declines the fast path and the caller
// re-runs the generic predicate delete, so observable behavior is identical
// in every case, including corrupt images.

import (
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// deleteSingleTableRowID removes the cell at idx (the row the cursor seeked
// to) from the table leaf at leafNum without touching the surviving cells.
// handled=false declines the fast path (the caller falls back to
// deleteAllMatchingFromLeaf); a non-nil error is real and propagates.
func (t *BTree) deleteSingleTableRowID(leafNum uint32, idx int, rowID int64) (handled bool, n int64, err error) {
	pg, page, coff, delCell, delOff, sz, ok := t.pointDeleteTarget(leafNum, idx, rowID)
	if !ok {
		return false, 0, nil
	}
	// Overflow-chain release happens before any page mutation; on failure the
	// generic path owns the same situation (finishLeafDelete frees chains
	// before the rewrite and propagates the error), so decline rather than
	// diverge.
	if delCell.Overflow != 0 {
		if ferr := t.freeOverflowChain(delCell.Overflow); ferr != nil {
			return false, 0, nil
		}
	}
	if derr := dropCellFromLeafPage(t.pager, pg, page, coff, idx, delOff, sz, t.usableSize); derr != nil {
		// The page image rejected the O(1) removal (corrupt free space):
		// the generic path rebuilds the page wholesale and owns the error.
		return false, 0, nil
	}
	// The write path kept `page` in step with its byte writes: re-arm the
	// pager's parse memo so the next statement's seek hits instead of
	// re-parsing the just-mutated leaf (btree.c's live-MemPage discipline).
	pg.RefreshParsedBTree(int(t.pageSize), coff, page)
	if werr := t.pager.WritePage(pg); werr != nil {
		return false, 0, werr
	}
	return true, 1, nil
}

// pointDeleteTarget resolves the fast path's preconditions on the target
// leaf: a parseable table leaf holding a well-formed cell at idx, with no
// duplicate of the same rowid after it (the generic predicate deletes ALL
// matches on the leaf; the cursor seek lands on the FIRST match, so checking
// the next pointer suffices). The cell and its on-page size come out of ONE
// header parse (C's BTREE_CLEAR_CELL xParseCell fills CellInfo once,
// btree.c:9908 — the fast path used to re-walk the same header varints
// through TableLeafCellSizeAt). ok=false declines the fast path.
func (t *BTree) pointDeleteTarget(leafNum uint32, idx int, rowID int64) (*pager.Page, *storage.BTreePage, int, storage.Cell, int, int, bool) {
	var delCell storage.Cell
	pg, err := t.pager.ReadPage(leafNum)
	if err != nil {
		return nil, nil, 0, delCell, 0, 0, false // the generic path re-reads and surfaces the error
	}
	coff := contentOffset(pg.PageNum)
	// The seek that positioned this statement already parsed this leaf (the
	// statement writes nothing before its delete), so the memo serves the
	// header here. dropCellFromLeafPage keeps its parsed copy in step with
	// its byte writes, and the memo's page is shared read-only — hand it a
	// by-value copy to mutate instead.
	memoPage, err := pg.ParsedBTree(int(t.pageSize), coff)
	if err != nil {
		return nil, nil, 0, delCell, 0, 0, false
	}
	pageOwn := *memoPage
	page := &pageOwn
	if page.PageType != storage.PageTypeLeafTable {
		return nil, nil, 0, delCell, 0, 0, false
	}
	if idx < 0 || idx >= int(page.CellCount) {
		return nil, nil, 0, delCell, 0, 0, false
	}
	if idx+1 < int(page.CellCount) && t.tableLeafRowidAt(pg, coff, idx+1) == rowID {
		return nil, nil, 0, delCell, 0, 0, false
	}
	delOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	sz, derr := storage.DecodeTableLeafCellAndSize(pg.Data, delOff, int(t.usableSize), &delCell)
	if derr != nil {
		// A malformed target cell is kept by the generic path (decode-failed
		// cells never match the predicate) — let it run.
		return nil, nil, 0, delCell, 0, 0, false
	}
	if delCell.RowID != rowID {
		// The generic predicate deletes only exact rowid matches; a cell
		// whose stored rowid differs (stale hinted position, corrupt image)
		// must not be removed by the fast path.
		return nil, nil, 0, delCell, 0, 0, false
	}
	return pg, page, coff, delCell, delOff, sz, true
}
