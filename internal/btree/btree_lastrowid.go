package btree

// LastRowID / MaxRowID — the largest-rowid walk (SQLite's sqlite3BtreeLast +
// sqlite3BtreeKeySize), split out of btree.go to keep the file under the
// 1000-line hard gate.

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// LastRowID returns the largest rowid in the table b-tree (SQLite's
// sqlite3BtreeLast + sqlite3BtreeKeySize). It walks the rightmost child chain
// to the last leaf and reads its final cell — O(depth), not a full scan (the
// engine's nextFTSBlockID previously scanned every %_segments row per flush,
// O(n) per flush, O(n^2) over the automerge's many flushes). An empty tree
// reports 0.
func (t *BTree) LastRowID() (int64, error) {
	id, found, err := t.maxRowIDFrom(t.rootPage, 0)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	return id, nil
}

// MaxRowID returns the largest rowid in the table b-tree and whether any row
// exists — the (id, found) form of sqlite3BtreeLast + sqlite3BtreeIntegerKey
// the engine's rowid allocator needs: a found=false answer (empty tree) and a
// found=true answer with id <= 0 (negative rowids are legal through explicit
// INSERT ... ROWID) must be distinguishable, and the found flag keeps the
// walk byte-equal to a full max scan for every input.
func (t *BTree) MaxRowID() (int64, bool) {
	id, found, err := t.maxRowIDFrom(t.rootPage, 0)
	if err != nil || !found {
		return 0, false
	}
	return id, true
}

// maxRowIDFrom returns the largest rowid in the subtree rooted at pageNum and
// whether the subtree holds any row. depth guards against corrupt cycles.
//
// A per-row DELETE can leave the rightmost leaf EMPTY (the engine's delete
// does not collapse/rebalance interior levels); SQLite's cursor would move
// left in that case, so the walk falls back to the interior page's last cell
// child until a NON-EMPTY leaf is found. Returning found=false for a tree
// whose last leaf is empty made nextFTSBlockID allocate block 1 and overwrite
// live blocks (fts4merge 1.4: the merge=1 continuation deleted the output's
// blocks 27-28, the emptied leaf reported max=0, and the rebuilt output was
// written over blocks 1-3).
func (t *BTree) maxRowIDFrom(pageNum uint32, depth int) (int64, bool, error) {
	if depth > 64 {
		return 0, false, fmt.Errorf("btree: interior page chain too deep")
	}
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return 0, false, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := pg.ParsedBTree(int(t.pageSize), coff)
	if err != nil {
		return 0, false, err
	}
	switch page.PageType {
	case storage.PageTypeInteriorTable:
		return t.maxRowIDFromInterior(pg, coff, page, depth)
	case storage.PageTypeLeafTable:
		if page.CellCount == 0 {
			return 0, false, nil
		}
		last := int(page.CellCount) - 1
		cellOff := int(storage.CellPointer(pg.Data, coff, last, int(t.pageSize)))
		if cellOff < 0 || cellOff >= len(pg.Data) {
			return 0, false, fmt.Errorf("database disk image is malformed")
		}
		_, n := util.GetVarint(pg.Data[cellOff:])
		if cellOff+n >= len(pg.Data) {
			return 0, false, fmt.Errorf("database disk image is malformed")
		}
		rowID, _ := util.GetVarint(pg.Data[cellOff+n:])
		return int64(rowID), true, nil
	default:
		return 0, false, fmt.Errorf("btree: unexpected page type 0x%02x", page.PageType)
	}
}

// maxRowIDFromInterior finds the largest rowid under an interior page: the
// rightmost subtree first, then the cell children high-to-low, until a
// non-empty subtree is found (an empty subtree reports found=false — the
// found flag, not a non-positive id, is the emptiness signal, so trees whose
// every rowid is negative still answer correctly).
func (t *BTree) maxRowIDFromInterior(pg *pager.Page, coff int, page *storage.BTreePage, depth int) (int64, bool, error) {
	if page.RightmostPtr != 0 {
		id, found, err := t.maxRowIDFrom(page.RightmostPtr, depth+1)
		if err != nil {
			return 0, false, err
		}
		if found {
			return id, true, nil
		}
	}
	// The rightmost subtree is empty (or absent): walk the interior
	// cells high-to-low until a non-empty subtree is found.
	for i := int(page.CellCount) - 1; i >= 0; i-- {
		cellOff := int(storage.CellPointer(pg.Data, coff+4, i, int(t.pageSize)))
		if cellOff < 0 || cellOff+4 > len(pg.Data) {
			return 0, false, fmt.Errorf("database disk image is malformed")
		}
		child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		if child == 0 {
			continue
		}
		id, found, cerr := t.maxRowIDFrom(child, depth+1)
		if cerr != nil {
			return 0, false, cerr
		}
		if found {
			return id, true, nil
		}
	}
	return 0, false, nil // the whole subtree is empty
}
