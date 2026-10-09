package btree

// In-place same-size cell overwrite: btree.c sqlite3BtreeInsert's loc==0
// fast path (src/btree.c:9596-9614). When an INSERT/UPDATE targets a rowid
// that already exists and the new cell is the same byte size as the old one
// (and fully local on both sides), SQLite overwrites the old cell's bytes at
// the SAME offset with a plain memcpy: the cell pointer array, the content
// start and the free-space accounting are all untouched, and no balance()
// runs. dropCell+insertCell (and their page restructuring) only happen when
// the size differs. This distinction is observable on a page whose cell
// pointer array was crafted to point at record bodies (test/corrupt.test
// corrupt-7.x): the in-place UPDATE rewrites record bytes without touching
// the crafted pointer, so the next page initialization — balance_deeper's
// btreeInitPage on the copied child — still detects it.

import (
	"sync/atomic"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// OverwriteCellByRowID replaces the leaf cell stored under rowID with
// cellData when the C in-place fast path applies (see declineInPlaceOverwrite
// for the guard list). It reports done=false when the row does not exist or
// any guard declines — the caller then falls back to the dropCell +
// insertCell path (delete + InsertCell), which mirrors sqlite3BtreeInsert's
// non-fast loc==0 branch.
func (t *BTree) OverwriteCellByRowID(rowID int64, cellData []byte) (done bool, err error) {
	t.saveAllCursors() // btree.c saveAllCursors at the sqlite3BtreeInsert entry
	pg, page, idx, ok, err := t.seekLeafRow(rowID)
	if err != nil || !ok {
		return false, err
	}
	return t.overwriteLeafCellAt(pg, page, idx, rowID, cellData)
}

// ReplaceCellByRowIDAt is OverwriteCellByRowIDAt extended to the whole
// loc==0 branch of sqlite3BtreeInsert (src/btree.c:9576-9677): when the new
// cell is the same size as the old one the bare memcpy runs, and otherwise
// the SAME page takes dropCell(idx) + insertCellFast(idx) — no second
// root-to-leaf descent, no position search, no balance() (the cursor is
// already on the row, so btree.c knows the slot). Only a page that cannot
// hold the new cell after the drop (or a shape this fast path cannot prove
// safe, see replaceInLeafFast) reports done=false, and the caller then runs
// the seek-delete + InsertCell form of the same two operations. The leaf
// repeat-UPDATE workload is the reason this exists: half of its statements
// change the record's byte size (a growing/decreasing text or integer column
// re-encodes to a different length), so the memcpy-only fast path declined
// on them and paid two descents, a binary search and a rebalance check per
// statement.
func (t *BTree) ReplaceCellByRowIDAt(rowID int64, cellData []byte, leaf uint32, idx int) (done bool, err error) {
	t.saveAllCursors() // btree.c saveAllCursors at the sqlite3BtreeInsert entry
	var old storage.Cell
	pg, page, oldOff, ok, err := t.locateLeafCell(leaf, idx, rowID, &old)
	if err != nil || !ok {
		if err != nil {
			return false, err
		}
		// Stale hinted position: the authoritative seek owns the write.
		return t.OverwriteCellByRowID(rowID, cellData)
	}
	oldSize := tableLeafCellSizeDecoded(pg.Data, oldOff, &old)
	if !declineInPlaceOverwrite(&old, oldSize, len(cellData), newPayloadLen(cellData), int(t.usableSize), t.ptrmapEnabled()) {
		replaceLaneMemcpy.Add(1)
		return t.overwriteLeafCellAtDecoded(pg, page, idx, &old, oldOff, cellData)
	}
	if !t.replaceInLeafFast(pg, page, idx, oldOff, oldSize, &old, cellData) {
		replaceLaneDeclined.Add(1)
		// The fast path declined before mutating the page (a shape it cannot
		// prove, or a page that cannot hold the cell after the drop): the
		// caller runs the seek-delete + insert form of the same loc==0
		// branch.
		return false, nil
	}
	replaceLaneDropInsert.Add(1)
	return true, nil
}

var (
	replaceLaneMemcpy     atomic.Int64
	replaceLaneDropInsert atomic.Int64
	replaceLaneDeclined   atomic.Int64
)

// replaceLaneHitsForTest reports the ReplaceCellByRowIDAt outcome counters
// (white-box observability: the replace lane's three branches, mirroring
// quickAppendHitsForTest).
func replaceLaneHitsForTest() (memcpy, dropInsert, declined int64) {
	return replaceLaneMemcpy.Load(), replaceLaneDropInsert.Load(), replaceLaneDeclined.Load()
}

// locateLeafCell re-reads the caller-hinted leaf position through the pager's
// parse memo and decodes the cell stored there into old. ok=false reports
// either a position that no longer holds rowID (the caller re-seeks) or an
// image whose page/pointer cannot be trusted for a write (err non-nil).
func (t *BTree) locateLeafCell(leaf uint32, idx int, rowID int64, old *storage.Cell) (pg *pager.Page, page *storage.BTreePage, oldOff int, ok bool, err error) {
	pg, err = t.pager.ReadPage(leaf)
	if err != nil {
		return nil, nil, 0, false, err
	}
	coff := contentOffset(pg.PageNum)
	page, err = pg.ParsedBTree(int(t.pageSize), coff)
	if err != nil {
		return nil, nil, 0, false, err
	}
	if page.PageType != storage.PageTypeLeafTable || idx < 0 || idx >= int(page.CellCount) {
		return nil, nil, 0, false, nil
	}
	oldOff = int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	if derr := storage.DecodeCellInto(pg.Data, oldOff, storage.CellTableLeaf, int(t.usableSize), old); derr != nil {
		return nil, nil, 0, false, derr
	}
	if old.RowID != rowID {
		return nil, nil, 0, false, nil
	}
	return pg, page, oldOff, true, nil
}

// replaceInLeafFast runs btree.c's non-memcpy loc==0 branch: free the old
// cell's overflow chain (clearCell), dropCell(idx), then insertCellFast at
// the SAME index on the same page. It reports false — with the page untouched
// — when the fast path cannot prove the write safe:
//
//   - the new cell image needs an overflow chain. The caller's image is built
//     by the record encoder and carries no chain (fillInCell/prepareCell run
//     only on the seeking path), so it cannot be written truthfully here.
//   - the page cannot hold the new cell once the old one's bytes are back in
//     the free space. btree.c needs no such proof because insertCell can
//     always fall back to balance(); this engine's split machinery lives
//     above btree, so the fast path proves the fit up front and lets the
//     caller's split-capable path handle the rare miss.
func (t *BTree) replaceInLeafFast(pg *pager.Page, memo *storage.BTreePage, idx, oldOff, oldSize int, old *storage.Cell, cellData []byte) bool {
	coff := contentOffset(pg.PageNum)
	newPayload := newPayloadLen(cellData)
	if storage.LocalPayloadSize(newPayload, int(t.usableSize), storage.CellTableLeaf) != newPayload {
		return false
	}
	if !leafHoldsAfterDrop(memo, coff, int(t.usableSize), oldSize, len(cellData)) {
		return false
	}
	// clearCell frees the replaced cell's overflow chain before dropCell
	// (src/btree.c:9586); a failure declines before any page mutation.
	if old.Overflow != 0 {
		if ferr := t.freeOverflowChain(old.Overflow); ferr != nil {
			return false
		}
	}
	// The memo struct is shared read-only: dropCell and insertCell mutate the
	// parsed header, so hand them a by-value copy (the same discipline the
	// point-delete target follows).
	page := *memo
	if derr := dropCellFromLeafPage(t.pager, pg, &page, coff, idx, oldOff, oldSize, t.usableSize); derr != nil {
		// Corrupt free space: the caller's fallback re-seeks and rebuilds
		// the page wholesale, exactly as it does for the delete path.
		return false
	}
	if werr := t.writeLeafCell(pg, &page, nil, cellData, coff, idx); werr != nil {
		return false
	}
	return true
}

// leafHoldsAfterDrop reports whether a leaf page holds nByte cell bytes once
// the cell occupying oldSize bytes has been dropped. Only the header fields
// are read: the freed bytes plus the gap between the cell-pointer array and
// the content area are a lower bound of the page's free space, and
// allocateSpaceOnPage defragments the rest of the free space (freeblocks,
// fragments) into that gap when the direct fit fails — so this condition
// guarantees the following writeLeafCell cannot report errLeafFull.
func leafHoldsAfterDrop(page *storage.BTreePage, coff, usableSize, oldSize, nByte int) bool {
	top := page.CellContent
	if top == 0 {
		top = usableSize
	}
	gap := top - (coff + storage.CellPointerOffset + 2*int(page.CellCount))
	return gap+oldSize >= nByte
}

// seekLeafRow positions a fresh cursor on the row with the given rowid and
// returns its leaf page, parsed page and cell index. ok is false when the
// rowid is not stored on a table leaf (missing row, interior page, or index
// out of range).
func (t *BTree) seekLeafRow(rowID int64) (pg *pager.Page, page *storage.BTreePage, idx int, ok bool, err error) {
	c, err := t.OpenCursorAtRoot() // the seek re-descends from the root
	if err != nil {
		return nil, nil, 0, false, err
	}
	found, err := c.SeekToRowID(rowID)
	if err != nil || !found {
		return nil, nil, 0, false, err
	}
	pg, err = t.pager.ReadPage(c.pageNum)
	if err != nil {
		return nil, nil, 0, false, err
	}
	page, err = pg.ParsedBTree(int(t.pageSize), contentOffset(pg.PageNum))
	if err != nil {
		return nil, nil, 0, false, err
	}
	ok = page.PageType == storage.PageTypeLeafTable && c.cellIdx >= 0 && c.cellIdx < int(page.CellCount)
	return pg, page, c.cellIdx, ok, nil
}

// overwriteLeafCellAt runs the in-place fast path on the cell at idx: the
// guard list of src/btree.c:9596-9614 (declineInPlaceOverwrite), the bounds
// checks of src/btree.c:9605-9610, then the memcpy over the old cell bytes.
func (t *BTree) overwriteLeafCellAt(pg *pager.Page, page *storage.BTreePage, idx int, rowID int64, cellData []byte) (bool, error) {
	coff := contentOffset(pg.PageNum)
	oldOff := int(storage.CellPointer(pg.Data, coff, idx, int(t.pageSize)))
	var old storage.Cell
	if err := storage.DecodeCellInto(pg.Data, oldOff, storage.CellTableLeaf, int(t.usableSize), &old); err != nil {
		return false, err
	}
	if old.RowID != rowID {
		return false, nil
	}
	return t.overwriteLeafCellAtDecoded(pg, page, idx, &old, oldOff, cellData)
}

// overwriteLeafCellAtDecoded is overwriteLeafCellAt with the target cell
// already decoded at oldOff (one decode per overwrite instead of two).
func (t *BTree) overwriteLeafCellAtDecoded(pg *pager.Page, page *storage.BTreePage, idx int, old *storage.Cell, oldOff int, cellData []byte) (bool, error) {
	coff := contentOffset(pg.PageNum)
	// The decode already walked this cell's header (btree.c xParseCell fills
	// CellInfo ONCE); the on-page size is the two header varints plus the
	// local payload the decode measured (+4 overflow pointer, floored at 4 —
	// cellSizePtrTableLeaf), so no second header walk is needed.
	oldSize := tableLeafCellSizeDecoded(pg.Data, oldOff, old)
	if declineInPlaceOverwrite(old, oldSize, len(cellData), newPayloadLen(cellData), int(t.usableSize), t.ptrmapEnabled()) {
		return false, nil
	}
	if err := overwriteBoundsError(oldOff, coff, len(cellData), int(t.pageSize)); err != nil {
		return false, err
	}
	// Write-intent barrier (sqlite3PagerWrite parity): capture the leaf's
	// statement-journal before-image before the in-place memcpy.
	t.pager.PrepareWrite(pg)
	// memcpy(oldCell, newCell, szNew) — the cell pointer array, content
	// start and free-space accounting stay byte-identical.
	copy(pg.Data[oldOff:oldOff+len(cellData)], cellData)
	return true, t.pager.WritePage(pg)
}

// declineInPlaceOverwrite reports whether the sqlite3BtreeInsert loc==0
// in-place fast path must be skipped (false = proceed with the memcpy).
// src/btree.c:9596-9614 guards:
//   - info.nLocal==info.nPayload: the old cell carries no overflow chain, so
//     skipping clearCell frees nothing (clearCell never fails then).
//   - info.nSize==szNew: same byte size on the page.
//   - !ISAUTOVACUUM || szNew<minLocal: on autovacuum databases the insertCell
//     path is required to maintain PTRMAP_OVERFLOW1 entries, so the fast path
//     only applies to sub-minLocal (never-overflowing) cells.
//
// The new payload being fully local by the file-format formula is the
// engine-side strengthening of the autovacuum rationale: no overflow chain
// exists yet at this point (fillInCell/prepareCell have not run), so an
// overflow-bearing cell image could not be written truthfully and must take
// the slow path.
func declineInPlaceOverwrite(old *storage.Cell, oldSize, szNew, newPayload, usableSize int, autovacuum bool) bool {
	switch {
	case old.LocalLen != old.PayloadLen:
		return true
	case oldSize != szNew:
		return true
	case autovacuum && szNew >= storage.MinLocalPayload(usableSize, storage.CellTableLeaf):
		return true
	case storage.LocalPayloadSize(newPayload, usableSize, storage.CellTableLeaf) != newPayload:
		return true
	}
	return false
}

// overwriteBoundsError mirrors src/btree.c:9605-9610's pre-memcpy checks:
// oldCell < aData+hdrOffset+10 or oldCell+szNew > aDataEnd is corruption.
func overwriteBoundsError(oldOff, coff, szNew, pageSize int) error {
	if oldOff < coff+10 || oldOff+szNew > pageSize {
		return storage.ErrMalformedImage
	}
	return nil
}

// newPayloadLen extracts the payload length from an encoded table-leaf cell
// image (leading payload-size varint).
func newPayloadLen(cellData []byte) int {
	plen, n := util.GetVarint(cellData)
	if n < 1 {
		return 0
	}
	return int(plen)
}

// tableLeafCellSizeDecoded computes the on-page size of the already-decoded
// table-leaf cell old at data[oldOff:] without re-decoding it — the size is
// the payload-length varint + rowid varint + old.LocalLen (+4 when the cell
// spills, for the overflow pointer), floored at 4 (storage's
// cellSizePtrTableLeaf pads undersized cells so a pointer slot is never
// smaller than the pointer). The offsets decoded successfully once, so the
// two varint reads stay in bounds; this mirrors storage.TableLeafCellSizeAt's
// arithmetic exactly (btree.c xCellSize reads the size straight from the
// bytes xParseCell just walked).
func tableLeafCellSizeDecoded(data []byte, oldOff int, old *storage.Cell) int {
	_, n1 := util.GetVarint(data[oldOff:])
	_, n2 := util.GetVarint(data[oldOff+n1:])
	sz := n1 + n2 + old.LocalLen
	if old.LocalLen < old.PayloadLen {
		sz += 4
	}
	if sz < 4 {
		sz = 4
	}
	return sz
}
