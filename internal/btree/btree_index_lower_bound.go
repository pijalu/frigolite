// Index lower-bound descent: the shared seek behind index-entry deletion
// (OP_IdxDelete) and index-key candidate lookup.
//
// SQLite positions an index cursor with sqlite3BtreeMovetoUnpacked's index
// branch (src/btree.c:6863): a binary search over the interior divider cells —
// descend into the LEFT child of the first divider that does not sort before
// the probe, and into the rightmost child when every divider sorts before it —
// followed by a binary search over that leaf's cells. The resulting position is
// the first entry that is not less than the probe; a caller needing
// greater-or-equal semantics advances from there with Next(), exactly as
// sqlite3BtreeMovetoUnpacked's callers step with btreeNext when the landing
// leaf holds no such cell (lwr == nCell).
//
// The descent is driven by a caller-supplied comparator, so the same code
// serves a full-record delete target (the tree's installed KeyInfo comparator)
// and a prefix probe (IndexRecordCompare). Going LEFT on an equal divider is
// what makes the prefix form correct: a divider that equals the probe's probed
// fields does not prove the lower bound is in the right subtree, because
// entries that compare equal on the probed prefix may still sort before it on
// the remaining key fields.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// indexCellCompare orders the index cell at data[cellOff:] against a
// caller-held probe (negative: the cell sorts before the probe). cellType
// names the cell's on-page kind — an interior cell's body starts after its
// 4-byte left-child pointer, a leaf cell's body starts at cellOff.
type indexCellCompare func(data []byte, cellOff int, cellType storage.CellType) (int, error)

// indexLowerBoundScan positions the cursor at the first index entry that is not
// less than cmp's probe, reporting ok=false when every entry in the tree sorts
// before it (the cursor is then past the last entry). The cursor is left with a
// usable path stack, so the caller can continue with Next().
func (c *Cursor) indexLowerBoundScan(cmp indexCellCompare) (bool, error) {
	if err := c.checkOpen(); err != nil {
		return false, err
	}
	c.clearSavedSeek()
	c.clearPageCache()
	c.endOfBTree = false
	if err := c.descendIndexLowerBound(cmp); err != nil {
		return false, err
	}
	return c.advanceIndexLowerBound(cmp)
}

// advanceIndexLowerBound walks forward from the descent's landing position to
// the first entry whose comparison is >= 0: within a leaf one cell at a time,
// across leaves through the path stack (_indexLowerBoundScan's contract allows
// the landing leaf to hold no such cell).
func (c *Cursor) advanceIndexLowerBound(cmp indexCellCompare) (bool, error) {
	for !c.endOfBTree {
		rc, has, err := c.currentIndexCellCompare(cmp)
		if err != nil {
			return false, err
		}
		if has {
			if rc >= 0 {
				return true, nil
			}
			c.cellIdx++
			continue
		}
		ok, err := c.Next()
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return false, nil
}

// currentIndexCellCompare compares the index-leaf cell at the cursor's
// position; has=false reports the position is past the leaf's last cell.
func (c *Cursor) currentIndexCellCompare(cmp indexCellCompare) (rc int, has bool, err error) {
	pg, page, err := c.indexLeaf()
	if err != nil {
		return 0, false, err
	}
	if c.cellIdx >= int(page.CellCount) {
		return 0, false, nil
	}
	cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum), c.cellIdx, int(c.tx.pageSize)))
	rc, err = cmp(pg.Data, cellOff, storage.CellIndexLeaf)
	return rc, err == nil, err
}

// indexLeaf caches and validates the cursor's current page as an index leaf.
func (c *Cursor) indexLeaf() (*pager.Page, *storage.BTreePage, error) {
	if err := c.cachePage(); err != nil {
		return nil, nil, err
	}
	page := c.currentPage
	if page.PageType != storage.PageTypeLeafIndex {
		return nil, nil, fmt.Errorf("btree: unexpected page type 0x%02x for index seek", page.PageType)
	}
	return c.currentPg, page, nil
}

// descendIndexLowerBound walks from the root to the leaf that holds the lower
// bound, recording the descent path (childIdx = the divider descended through,
// CellCount = the rightmost child) so a later Next() can cross leaf boundaries.
func (c *Cursor) descendIndexLowerBound(cmp indexCellCompare) error {
	c.path = c.path[:0]
	pageNum := c.tx.rootPage
	for depth := 0; ; depth++ {
		child, done, err := c.indexDescentStep(pageNum, depth, cmp)
		if err != nil || done {
			return err
		}
		pageNum = child
	}
}

// indexDescentStep resolves one level of the descent: done=true means the
// cursor is positioned on the landing leaf (or the tree is empty), false means
// the returned page is the child to visit next.
func (c *Cursor) indexDescentStep(pageNum uint32, depth int, cmp indexCellCompare) (child uint32, done bool, err error) {
	if depth > indexSeekMaxDepth {
		c.endOfBTree = true
		return 0, false, fmt.Errorf("btree: interior page chain too deep")
	}
	pg, err := c.tx.pager.ReadPage(pageNum)
	if err != nil {
		c.endOfBTree = true
		return 0, false, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := pg.ParsedBTree(int(c.tx.pageSize), coff)
	if err != nil {
		c.endOfBTree = true
		return 0, false, err
	}
	switch page.PageType {
	case storage.PageTypeLeafIndex:
		return 0, true, c.landIndexLowerBoundLeaf(pg, page, cmp)
	case storage.PageTypeInteriorIndex:
		return c.descendIndexLowerBoundInterior(pg, page, cmp)
	default:
		c.endOfBTree = true
		return 0, false, fmt.Errorf("btree: unexpected page type 0x%02x for index seek", page.PageType)
	}
}

// landIndexLowerBoundLeaf parks the cursor on the leaf's lower-bound position
// (the leaf's cell count when the whole leaf sorts before the probe).
func (c *Cursor) landIndexLowerBoundLeaf(pg *pager.Page, page *storage.BTreePage, cmp indexCellCompare) error {
	idx, err := c.leafIndexLowerBound(pg, page, cmp)
	if err != nil {
		c.endOfBTree = true
		return err
	}
	c.pageNum = pg.PageNum
	c.cellIdx = idx
	return nil
}

// descendIndexLowerBoundInterior pushes the chosen divider onto the path and
// returns the child to visit. An interior page with no child to descend into is
// an empty tree: report end-of-btree instead of reading page 0 (the
// stepDownLeftmost convention).
func (c *Cursor) descendIndexLowerBoundInterior(pg *pager.Page, page *storage.BTreePage, cmp indexCellCompare) (uint32, bool, error) {
	childIdx, child, err := c.interiorIndexLowerBoundChild(pg, page, cmp)
	if err != nil {
		c.endOfBTree = true
		return 0, false, err
	}
	if child == 0 {
		c.endOfBTree = true
		return 0, true, nil
	}
	c.path = append(c.path, cursorPathEntry{pageNum: pg.PageNum, childIdx: childIdx})
	return child, false, nil
}

// leafIndexLowerBound returns the index of the first cell of the index leaf
// that is not less than the probe, or the leaf's cell count when every cell
// sorts before it.
func (c *Cursor) leafIndexLowerBound(pg *pager.Page, page *storage.BTreePage, cmp indexCellCompare) (int, error) {
	coff := contentOffset(pg.PageNum)
	lo, hi := 0, int(page.CellCount)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, coff, mid, int(c.tx.pageSize)))
		rc, err := cmp(pg.Data, cellOff, storage.CellIndexLeaf)
		if err != nil {
			return 0, err
		}
		if rc < 0 {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

// interiorIndexLowerBoundChild returns the child to descend into for the lower
// bound plus the divider index that identifies it in the cursor path (the
// page's cell count for the rightmost child — navigateToNextChild's
// convention).
func (c *Cursor) interiorIndexLowerBoundChild(pg *pager.Page, page *storage.BTreePage, cmp indexCellCompare) (int, uint32, error) {
	coff := contentOffset(pg.PageNum)
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	lo, hi := 0, int(page.CellCount)-1
	childIdx := int(page.CellCount)
	for lo <= hi {
		mid := (lo + hi) / 2
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, mid, int(c.tx.pageSize)))
		if cellOff < 0 || cellOff+4 > len(pg.Data) {
			return 0, 0, fmt.Errorf("database disk image is malformed")
		}
		rc, err := cmp(pg.Data, cellOff, storage.CellIndexInterior)
		if err != nil {
			return 0, 0, err
		}
		if rc < 0 {
			lo = mid + 1
		} else {
			childIdx = mid
			hi = mid - 1
		}
	}
	if childIdx >= int(page.CellCount) {
		return childIdx, page.RightmostPtr, nil
	}
	cellOff := int(storage.CellPointer(pg.Data, ptrBase, childIdx, int(c.tx.pageSize)))
	if cellOff < 0 || cellOff+4 > len(pg.Data) {
		return 0, 0, fmt.Errorf("database disk image is malformed")
	}
	return childIdx, binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4]), nil
}
