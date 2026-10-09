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
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
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
	if c.onInteriorCell {
		// The target entry lives on an INTERIOR page: sqlite3BtreeDelete's rule
		// (src/btree.c:9877-9944) replaces it with its predecessor — the largest
		// entry of the deleted cell's left child subtree, which is always on a
		// leaf — and deletes that leaf cell.
		ok, derr := t.deleteInteriorIndexEntry(c)
		return ok, ok, derr
	}
	ok, err := t.dropIndexLeafCell(c.pageNum, c.cellIdx)
	if err != nil || !ok {
		return false, false, err
	}
	// No rebalance follow-up: an emptied INDEX leaf stays in place
	// (maybeRebalanceAfterDelete's leaf-index branch).
	return true, true, nil
}

// deleteInteriorIndexEntry removes the index entry the cursor sits on when that
// entry is an INTERIOR page's cell, following sqlite3BtreeDelete
// (src/btree.c:9877-9944). The deleted cell's left child C is the boundary of the
// subtree that precedes the entry, so:
//
//   - when C's subtree still holds entries, the entry that takes the deleted
//     cell's place is its in-order predecessor — the LAST entry of C's subtree
//     (sqlite moves the cursor there with sqlite3BtreePrevious because "the
//     previous entry is always a part of the sub-tree headed by the child page
//     of the cell being deleted") — which keeps the child pointer (and so the
//     subtree) reachable while the tree loses exactly one entry;
//   - when C's subtree holds NO entries at all, there is nothing to move in.
//     sqlite never meets this case (balance keeps every page non-empty); this
//     engine keeps emptied index leaves in place (maybeRebalanceAfterDelete), so
//     the cell is dropped outright and C's empty pages are released with it.
//
// handled=false declines the operation (the caller's byte-exact walk owns it).
func (t *BTree) deleteInteriorIndexEntry(c *Cursor) (bool, error) {
	pg, page, _, err := c.indexPageAt()
	if err != nil {
		return false, err
	}
	if page.PageType != storage.PageTypeInteriorIndex {
		return false, nil
	}
	k := c.cellIdx
	if k < 0 || k >= int(page.CellCount) {
		return false, nil
	}
	cellOff := indexCellOffset(pg, page, storage.CellIndexInterior, k, int(t.pageSize))
	if cellOff < 0 || cellOff+4 > len(pg.Data) {
		return false, nil
	}
	leftChild := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])

	// Every mutation below runs on a by-value copy of the parsed header: the
	// pager's memo hands out a SHARED read-only struct, so in-place edits must
	// be published back with RefreshParsedBTree (dropIndexLeafCell's pattern).
	pageOwn := *page
	predecessor, hasPred, err := t.takeLastEntryOfSubtree(leftChild, 0)
	if err != nil {
		return false, err
	}
	if hasPred {
		return t.replaceInteriorCell(pg, &pageOwn, k, leftChild, predecessor)
	}
	if err := t.freeEmptySubtree(leftChild, 0); err != nil {
		return false, err
	}
	if err := t.removeInteriorCellRange(pg, &pageOwn, k, 1); err != nil {
		return false, err
	}
	pg.RefreshParsedBTree(int(t.pageSize), contentOffset(pg.PageNum), &pageOwn)
	if werr := t.pager.WritePage(pg); werr != nil {
		return false, werr
	}
	return true, nil
}

// replaceInteriorCell rewrites interior cell idx in place as a CellIndexInterior
// carrying `payload` and leaving `leftChild` in place: both encodings derive from
// one record length, so the wire sizes match and the write fits the slot exactly.
func (t *BTree) replaceInteriorCell(pg *pager.Page, page *storage.BTreePage, idx int, leftChild uint32, payload []byte) (bool, error) {
	repl, eerr := t.encodeDividerCell(leftChild, leafSplitResult{medianPayload: payload}, pg.PageNum)
	if eerr != nil {
		return false, eerr
	}
	cellOff := indexCellOffset(pg, page, storage.CellIndexInterior, idx, int(t.pageSize))
	old, oerr := storage.DecodeCell(pg.Data, cellOff, storage.CellIndexInterior, int(t.usableSize))
	if oerr != nil {
		return false, oerr
	}
	t.pager.PrepareWrite(pg)
	if old.Overflow != 0 {
		if ferr := t.freeOverflowChain(old.Overflow); ferr != nil {
			return false, ferr
		}
	}
	if len(repl) != storage.CellWireLen(old) {
		// Different wire size (an interior local-payload boundary fall):
		// relocate through the re-key machinery's pattern instead of an
		// in-place copy, which would overrun the neighbouring cell. Both
		// encodings derive from the same payload length, so this is a guard.
		return t.replaceInteriorCellRelocated(pg, page, idx, repl)
	}
	copy(pg.Data[cellOff:cellOff+len(repl)], repl)
	pg.RefreshParsedBTree(int(t.pageSize), contentOffset(pg.PageNum), page)
	if werr := t.pager.WritePage(pg); werr != nil {
		return false, werr
	}
	return true, nil
}

// takeLastEntryOfSubtree removes the LAST entry (in key order) of the subtree
// rooted at pageNum and returns its full payload. ok=false reports a subtree
// that holds no entries at all — possible in this engine, where an emptied index
// leaf stays in place (sqlite's balance keeps every page non-empty, so the case
// does not exist there). An interior page's own cells are entries too, so the
// walk first tries the rightmost subtree, then falls back to the page's cells
// from the end, replacing each cell with the last entry of its left subtree (the
// same rule one level down) or dropping it when that subtree is empty.
func (t *BTree) takeLastEntryOfSubtree(pageNum uint32, depth int) ([]byte, bool, error) {
	if pageNum == 0 {
		return nil, false, nil
	}
	if depth > indexSeekMaxDepth {
		return nil, false, fmt.Errorf("btree: interior page chain too deep")
	}
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return nil, false, err
	}
	page, err := pg.ParsedBTree(int(t.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return nil, false, err
	}
	switch page.PageType {
	case storage.PageTypeLeafIndex:
		return t.takeLastLeafEntry(pageNum, pg, page)
	case storage.PageTypeInteriorIndex:
		return t.takeLastInteriorEntry(pageNum, pg, page, depth)
	default:
		return nil, false, fmt.Errorf("btree: unexpected page type 0x%02x for index delete", page.PageType)
	}
}

// takeLastLeafEntry removes the last cell of an index leaf and returns its full
// payload; ok=false reports an empty leaf (no entries).
func (t *BTree) takeLastLeafEntry(pageNum uint32, pg *pager.Page, page *storage.BTreePage) ([]byte, bool, error) {
	if page.CellCount == 0 {
		return nil, false, nil
	}
	last := int(page.CellCount) - 1
	payload, perr := t.indexCellPayloadAt(pg, page, storage.CellIndexLeaf, last)
	if perr != nil {
		return nil, false, perr
	}
	dropped, derr := t.dropIndexLeafCell(pageNum, last)
	if derr != nil {
		return nil, false, derr
	}
	if !dropped {
		return nil, false, fmt.Errorf("btree: cannot drop index leaf cell %d/%d", pageNum, last)
	}
	return payload, true, nil
}

// takeLastInteriorEntry removes the last entry of the subtree of an INTERIOR
// index page. Its own cells are entries, and in-order the page reads
// subtree(L_0), c_0, subtree(L_1), …, c_{n-1}, subtree(R) — so the rightmost
// subtree is tried first and the page's own last cell is the fallback.
func (t *BTree) takeLastInteriorEntry(pageNum uint32, pg *pager.Page, page *storage.BTreePage, depth int) ([]byte, bool, error) {
	if page.RightmostPtr != 0 {
		payload, ok, rerr := t.takeLastEntryOfSubtree(page.RightmostPtr, depth+1)
		if rerr != nil || ok {
			return payload, ok, rerr
		}
	}
	if page.CellCount == 0 {
		// No cells and no entries below: this subtree holds nothing.
		return nil, false, nil
	}
	// With subtree(R) empty the LAST entry is the page's own last cell. Its slot
	// must keep its left-child pointer, so the slot is refilled with the last
	// entry of that child's subtree (the same rule one level down) and the cell's
	// OWN payload is what leaves the tree.
	j := int(page.CellCount) - 1
	cellOff := indexCellOffset(pg, page, storage.CellIndexInterior, j, int(t.pageSize))
	if cellOff < 0 || cellOff+4 > len(pg.Data) {
		return nil, false, fmt.Errorf("database disk image is malformed")
	}
	left := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
	own, perr := t.indexCellPayloadAt(pg, page, storage.CellIndexInterior, j)
	if perr != nil {
		return nil, false, perr
	}
	if err := t.refillOrDropInteriorSlot(pg, page, j, left, depth); err != nil {
		return nil, false, err
	}
	return own, true, nil
}

// refillOrDropInteriorSlot keeps interior slot j's left-child pointer reachable
// while its payload leaves the tree: the slot is rewritten with the last entry
// of that child's subtree (replaceInteriorCell, the predecessor rule of
// sqlite3BtreeDelete), or — when the child's subtree holds no entries at all
// (this engine keeps emptied leaves) — the slot is dropped and the empty subtree
// released with it.
func (t *BTree) refillOrDropInteriorSlot(pg *pager.Page, page *storage.BTreePage, j int, left uint32, depth int) error {
	filler, ok, rerr := t.takeLastEntryOfSubtree(left, depth+1)
	if rerr != nil {
		return rerr
	}
	// Mutations run on a by-value copy of the parsed header: the pager's memo
	// hands out shared read-only state (dropIndexLeafCell's pattern).
	pageOwn := *page
	if ok {
		_, serr := t.replaceInteriorCell(pg, &pageOwn, j, left, filler)
		return serr
	}
	if ferr := t.freeEmptySubtree(left, depth+1); ferr != nil {
		return ferr
	}
	if rerr := t.removeInteriorCellRange(pg, &pageOwn, j, 1); rerr != nil {
		return rerr
	}
	return t.pager.WritePage(pg)
}

// freeEmptySubtree returns every page of a subtree that holds no entries to the
// freelist. Every page under such a subtree has zero cells (an interior page's
// cells are entries themselves), so the subtree is a chain of childless pages
// hanging off rightmost pointers.
func (t *BTree) freeEmptySubtree(pageNum uint32, depth int) error {
	if pageNum == 0 {
		return nil
	}
	if depth > indexSeekMaxDepth {
		return fmt.Errorf("btree: interior page chain too deep")
	}
	if pager.IsPageOnFreelist(t.pager, pageNum) {
		return nil
	}
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return err
	}
	page, err := pg.ParsedBTree(int(t.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return err
	}
	if page.CellCount != 0 {
		return fmt.Errorf("btree: subtree page %d holds %d entries; not empty", pageNum, page.CellCount)
	}
	switch page.PageType {
	case storage.PageTypeLeafIndex:
	case storage.PageTypeInteriorIndex:
		if err := t.freeEmptySubtree(page.RightmostPtr, depth+1); err != nil {
			return err
		}
	default:
		return fmt.Errorf("btree: unexpected page type 0x%02x for index delete", page.PageType)
	}
	return t.freePageWithPtrmap(pageNum)
}

// indexCellPayloadAt returns the FULL payload (overflow chain reassembled) of
// the index cell at idx of the given page, for either cell kind.
func (t *BTree) indexCellPayloadAt(pg *pager.Page, page *storage.BTreePage, cellType storage.CellType, idx int) ([]byte, error) {
	cellOff := indexCellOffset(pg, page, cellType, idx, int(t.pageSize))
	body := cellOff
	if cellType == storage.CellIndexInterior {
		body += 4 // the cell body follows its 4-byte left-child pointer
	}
	local, fullLen, ovfl, err := t.indexCellLocalPayload(pg.Data, body, cellType)
	if err != nil {
		return nil, err
	}
	full, err := t.fullIndexCellPayload(local, fullLen, ovfl, cellType)
	if err != nil {
		return nil, err
	}
	// The page's bytes move (dropCell/compaction) before the caller writes the
	// payload elsewhere: return an owned copy.
	return append([]byte(nil), full...), nil
}

// replaceInteriorCellRelocated rewrites interior cell idx with encoded bytes of a
// different size: the new cell is written at the content-area start, the old
// bytes are abandoned (its overflow chain is already freed by the caller) and
// the page is defragmented so no untracked hole is left (the
// rekeyCarrierChainIndex + finishChildSplits pattern).
func (t *BTree) replaceInteriorCellRelocated(pg *pager.Page, page *storage.BTreePage, idx int, encoded []byte) (bool, error) {
	coff := contentOffset(pg.PageNum)
	ptrBase := coff + cellPtrOffset(page.PageType)
	start := int(page.CellContent) - len(encoded)
	if start < coff+cellPtrOffset(page.PageType)+(int(page.CellCount)+1)*2+2 {
		return false, nil
	}
	copy(pg.Data[start:], encoded)
	binary.BigEndian.PutUint16(pg.Data[ptrBase+idx*2:], uint16(start))
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(start))
	page.CellContent = start
	if err := t.defragmentInterior(pg, page); err != nil {
		return false, err
	}
	pg.RefreshParsedBTree(int(t.pageSize), coff, page)
	if err := t.pager.WritePage(pg); err != nil {
		return false, err
	}
	return true, nil
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

// indexCursorCellPayload returns the FULL payload of the index cell at the
// cursor's position (local bytes reassembled with their overflow chain). The
// position may be an interior page's cell: sqlite's interior index cells are
// real entries. Empty leaf pages are skipped first (the engine keeps them after
// deletes) and reaching the end of the tree reports errIndexPosPastEnd.
func (t *BTree) indexCursorCellPayload(c *Cursor) ([]byte, error) {
	ok, err := c.skipEmptyPosition()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errIndexPosPastEnd
	}
	pg, page, cellType, err := c.indexPageAt()
	if err != nil {
		return nil, err
	}
	cellOff := indexCellOffset(pg, page, cellType, c.cellIdx, int(t.pageSize))
	body := cellOff
	if cellType == storage.CellIndexInterior {
		body += 4 // the cell body follows its 4-byte left-child pointer
	}
	local, fullLen, ovfl, err := t.indexCellLocalPayload(pg.Data, body, cellType)
	if err != nil {
		return nil, err
	}
	return t.fullIndexCellPayload(local, fullLen, ovfl, cellType)
}

// errIndexPosPastEnd reports that a cursor position ran past the tree's last
// entry while skipping empty leaf pages (an empty run walker's end condition).
var errIndexPosPastEnd = errors.New("btree: cursor past the last index entry")

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
