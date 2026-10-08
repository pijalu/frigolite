// Index-entry deletion by seek: OP_IdxDelete's shape. vdbe.c's OP_IdxDelete
// positions the statement's index cursor with sqlite3BtreeMovetoUnpacked and
// then drops that cell (sqlite3BtreeDelete, BTREE_AUXDELETE) — one O(log n)
// descent per deleted entry. The all-leaf walk this file's fast path replaces
// paid O(index entries) per target, which made index maintenance the dominant
// cost of every DELETE/UPDATE on an indexed table (a point DELETE with one
// index: 2.1 ms per row at 50k rows, linear in the index size).

package btree

import (
	"bytes"

	"github.com/pijalu/frigolite/internal/storage"
)

// deleteIndexEntryBySeek removes the single index entry whose FULL payload
// byte-equals target through one lower-bound descent plus dropCell. handled
// reports that the fast path owned the operation: deleted is then its verdict
// (false = the tree has no such entry), and a non-nil error is real. A
// handled=false result means the fast path declined and the caller must run the
// byte-exact leaf walk (the tree carries no value comparator, so the stored
// order is unknown).
func (t *BTree) deleteIndexEntryBySeek(target []byte) (deleted, handled bool, err error) {
	if t.keyCompare == nil || len(target) == 0 {
		return false, false, nil
	}
	t.saveAllCursors()         // btree.c saveAllCursors on the delete path
	t.invalidateAppendCursor() // a delete may remove the maximum key
	c, err := t.OpenCursorAtRoot()
	if err != nil {
		return false, false, err
	}
	found, err := c.indexLowerBoundScan(t.indexTargetCompare(target))
	if err != nil {
		return false, false, err
	}
	if !found {
		// The target sorts after every entry: index entries are unique per row,
		// so a missing entry is the "index predates maintenance" case the walk
		// tolerates — nothing to remove, no page touched.
		return false, true, nil
	}
	full, err := t.indexCursorCellPayload(c)
	if err != nil {
		return false, false, err
	}
	if !bytes.Equal(full, target) {
		// Value equality is not byte equality: an INTEGER 1 and a REAL 1.0
		// compare equal under the KeyInfo yet encode differently. The
		// maintenance payload must byte-match the entry the insert side wrote,
		// so hand the decision to the byte-exact walk.
		return false, false, nil
	}
	ok, err := t.dropIndexLeafCell(c.pageNum, c.cellIdx)
	if err != nil || !ok {
		return false, false, err
	}
	// No rebalance follow-up: an emptied INDEX leaf stays in place
	// (maybeRebalanceAfterDelete's leaf-index branch), and the descent never
	// removes an interior divider cell, so the root-collapse tail
	// (clearEmptyRootRightmost) the batch walk needs cannot trigger here.
	return true, true, nil
}

// indexTargetCompare compares an index cell against a full-record target under
// the tree's installed value comparator — the very order the insert path wrote
// the tree in, so the descent and the stored order always agree. A spilling
// cell's overflow chain is reassembled first: its local bytes are a prefix of
// the record and would order as a shorter record.
func (t *BTree) indexTargetCompare(target []byte) indexCellCompare {
	return func(data []byte, cellOff int, cellType storage.CellType) (int, error) {
		var cell storage.Cell
		if err := storage.DecodeCellInto(data, cellOff, cellType, int(t.usableSize), &cell); err != nil {
			return 0, err
		}
		full, err := t.readOverflow(&cell)
		if err != nil {
			return 0, err
		}
		return t.compareKey(full.Payload, target), nil
	}
}

// indexCursorCellPayload returns the FULL payload of the index-leaf cell at the
// cursor's position (local bytes reassembled with their overflow chain).
func (t *BTree) indexCursorCellPayload(c *Cursor) ([]byte, error) {
	pg, _, err := c.indexLeaf()
	if err != nil {
		return nil, err
	}
	cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), c.cellIdx, int(t.pageSize)))
	local, fullLen, ovfl, err := t.indexCellLocalPayload(pg.Data, cellOff)
	if err != nil {
		return nil, err
	}
	return t.fullIndexCellPayload(local, fullLen, ovfl)
}

// dropIndexLeafCell removes the index-leaf cell at idx with btree.c dropCell's
// O(1) removal (dropCellFromLeafPage: the cell's bytes return to the page's
// freeblock chain, the pointer slot unlinks, no survivor byte moves) after
// returning the cell's overflow chain to the freelist (dropCell →
// sqlite3BtreeClearCell → clearCell → freePageChain). handled=false declines the
// fast path — the caller's leaf walk rebuilds the page wholesale and owns the
// outcome, so corrupt images keep the walk's behavior; a non-nil error is real.
func (t *BTree) dropIndexLeafCell(leaf uint32, idx int) (bool, error) {
	pg, err := t.pager.ReadPage(leaf)
	if err != nil {
		return false, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return false, err
	}
	if page.PageType != storage.PageTypeLeafIndex {
		return false, nil
	}
	if idx < 0 || idx >= int(page.CellCount) {
		return false, nil
	}
	cellOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	var cell storage.Cell
	if derr := storage.DecodeCellInto(pg.Data, cellOff, storage.CellIndexLeaf, int(t.usableSize), &cell); derr != nil {
		// A cell that does not decode is kept by the walk (decode-failed
		// cells never match): decline rather than remove it.
		return false, nil
	}
	sz := storage.CellWireLen(&cell)
	if cell.Overflow != 0 {
		if ferr := t.freeOverflowChain(cell.Overflow); ferr != nil {
			return false, ferr
		}
	}
	// The page is mutated through a by-value copy of its parsed header: the
	// pager's memo hands out a shared read-only struct (the
	// deleteSingleTableRowID pattern).
	pageOwn := *page
	if derr := dropCellFromLeafPage(t.pager, pg, &pageOwn, coff, idx, cellOff, sz, t.usableSize); derr != nil {
		// The page image rejected the O(1) removal (free-space corruption):
		// the walk rebuilds the page and owns the error.
		return false, nil
	}
	pg.RefreshParsedBTree(int(t.pageSize), coff, &pageOwn)
	if werr := t.pager.WritePage(pg); werr != nil {
		return false, werr
	}
	return true, nil
}
