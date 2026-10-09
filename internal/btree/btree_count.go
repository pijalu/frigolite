// Entry counting over a b-tree's page headers — the port of
// sqlite3BtreeCount (btree.c:10465) that backs OP_Count's "SELECT count(*)
// FROM t" shortcut (select.c:8854 isSimpleCount, vdbe.c:3795 OP_Count).
//
// The count never decodes a cell: every page contributes its header's cell
// count, and only the left-child pointers of the interior pages are followed,
// so the cost is one page-header parse per page instead of one decode per row.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// countMaxDepth caps the interior descent (corrupt child cycles).
const countMaxDepth = 64

// CountEntries returns the number of entries in the b-tree: the exact port of
// sqlite3BtreeCount's rule (btree.c:10487) — every LEAF page contributes its
// cell count, and so does every INTERIOR INDEX page, because an index b-tree's
// interior cells are real entries (balance_nonroot moves one cell per split
// boundary up into the parent; see plan/R13_L7_INDEX_INTERIOR.md). Interior
// TABLE cells are rowid separators in both layouts and are not entries.
//
// Malformed pages are reported rather than counted: callers fall back to the
// scan, which surfaces the same corruption.
func (t *BTree) CountEntries() (int64, error) {
	var n int64
	if err := t.countPageEntries(t.rootPage, 0, &n); err != nil {
		return 0, err
	}
	return n, nil
}

func (t *BTree) countPageEntries(pageNum uint32, depth int, n *int64) error {
	if depth > countMaxDepth {
		return fmt.Errorf("btree: interior page chain too deep")
	}
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	page, err := pg.ParsedBTree(int(t.pageSize), coff)
	if err != nil {
		return err
	}
	switch page.PageType {
	case storage.PageTypeLeafTable, storage.PageTypeLeafIndex, storage.PageTypeInteriorIndex:
		*n += int64(page.CellCount)
	case storage.PageTypeInteriorTable:
		// Rowid separators: not entries.
	default:
		return fmt.Errorf("btree: unexpected page type 0x%02x for count", page.PageType)
	}
	if page.PageType == storage.PageTypeLeafTable || page.PageType == storage.PageTypeLeafIndex {
		return nil
	}
	for i := 0; i < int(page.CellCount); i++ {
		child, cerr := interiorChildPtr(pg, page, coff, i, int(t.pageSize))
		if cerr != nil {
			return cerr
		}
		if err := t.countPageEntries(child, depth+1, n); err != nil {
			return err
		}
	}
	if page.RightmostPtr == 0 {
		return nil
	}
	return t.countPageEntries(page.RightmostPtr, depth+1, n)
}

// interiorChildPtr returns the left-child page number stored in an interior
// page's idx-th cell (the cell-pointer array is shifted by the child-pointer
// prefix: the 4-byte child precedes the cell body).
func interiorChildPtr(pg *pager.Page, page *storage.BTreePage, coff, idx, pageSize int) (uint32, error) {
	cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, idx, pageSize))
	if cellOff < 0 || cellOff+4 > len(pg.Data) {
		return 0, fmt.Errorf("database disk image is malformed")
	}
	return binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4]), nil
}
