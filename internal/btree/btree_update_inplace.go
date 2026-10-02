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
	page, err = storage.ParsePage(pg.Data, int(t.pageSize), contentOffset(pg.PageNum))
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
	old, err := storage.DecodeCell(pg.Data, oldOff, storage.CellTableLeaf, int(t.usableSize))
	if err != nil {
		return false, err
	}
	if old.RowID != rowID {
		return false, nil
	}
	oldSize, err := storage.TableLeafCellSizeAt(pg.Data, oldOff, int(t.usableSize))
	if err != nil {
		return false, err
	}
	if declineInPlaceOverwrite(old, oldSize, len(cellData), newPayloadLen(cellData), int(t.usableSize), t.ptrmapEnabled()) {
		return false, nil
	}
	if err := overwriteBoundsError(oldOff, coff, len(cellData), int(t.pageSize)); err != nil {
		return false, err
	}
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
