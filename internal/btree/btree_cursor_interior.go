// Cursor navigation over an index b-tree's interleaved interior cells.
//
// sqlite stores an index key exactly once, on a leaf or on an INTERIOR page:
// balance_nonroot moves one cell per split boundary up into the parent
// (src/btree.c:8791-8849, plan/R13_L7_INDEX_INTERIOR.md). In-order traversal of
// an interior page therefore reads subtree(cell 0's left child), cell 0,
// subtree(cell 1's left child), cell 1, …, subtree(rightmost) — so the cursor
// must yield interior cells between the child subtrees, and an interior
// position (Cursor.onInteriorCell) is a real entry. Table b-trees keep the
// rowid-separator convention: their interior cells are not entries and are
// skipped, exactly as before.

package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/storage"
)

// navigateToNextChild advances the cursor to the next leaf in sequence.
// This handles multi-level trees by walking up the path stack to find the
// next sibling, then descending to its leftmost leaf.
func (c *Cursor) navigateToNextChild() {
	// Walk up the path stack to find the next child to visit
	for len(c.path) > 0 {
		top := &c.path[len(c.path)-1]
		topPage, topChild := top.pageNum, top.childIdx

		pg, err := c.tx.pager.ReadPage(topPage)
		if err != nil {
			c.endOfBTree = true
			return
		}
		coff := contentOffset(pg.PageNum)
		page, err := pg.ParsedBTree(int(c.tx.pageSize), coff)
		if err != nil {
			c.endOfBTree = true
			return
		}

		// The subtree of child `topChild` is exhausted. In an INDEX b-tree
		// that child's divider cell is a REAL entry (balance_nonroot moves one
		// cell per split boundary up into the parent), so in-order traversal
		// yields it here, between the two child subtrees. Table b-trees keep
		// the rowid-separator convention: their interior cells are not entries
		// and are skipped. See plan/R13_L7_INDEX_INTERIOR.md §0.1.
		if !c.tx.isTable && topChild < int(page.CellCount) {
			c.path = c.path[:len(c.path)-1]
			c.pageNum = topPage
			c.cellIdx = topChild
			c.onInteriorCell = true
			c.endOfBTree = false
			c.clearPageCache()
			return
		}
		*top = cursorPathEntry{pageNum: topPage, childIdx: topChild + 1}

		if top.childIdx < int(page.CellCount) {
			// Navigate to cell[top.childIdx].leftChild
			cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, top.childIdx, int(c.tx.pageSize)))
			if cellOff < 0 || cellOff+4 > len(pg.Data) {
				// Crafted cell pointer aimed at the page tail: stop the walk
				// instead of slicing past the buffer; the caller's next read
				// reports the malformed image.
				c.endOfBTree = true
				return
			}
			c.pageNum = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
			c.cellIdx = 0
			c.endOfBTree = false
			// Descend to leftmost leaf from here
			c.descendToFirstLeafFromCurrent()
			return
		} else if top.childIdx == int(page.CellCount) {
			// Navigate to the rightmost pointer
			c.pageNum = page.RightmostPtr
			c.cellIdx = 0
			c.endOfBTree = false
			c.descendToFirstLeafFromCurrent()
			return
		}
		// This interior page is exhausted — pop and try parent
		c.path = c.path[:len(c.path)-1]
	}
	c.endOfBTree = true
}

// stepIntoChildAfterInterior continues an in-order walk positioned on an
// interior index page's cell k (a real entry): the next entries are the ones
// held by the subtree of the FOLLOWING child — cell k+1's left child, or the
// rightmost pointer when k is the page's last cell.

// stepIntoChildAfterInterior continues an in-order walk positioned on an
// interior index page's cell k (a real entry): the next entries are the ones
// held by the subtree of the FOLLOWING child — cell k+1's left child, or the
// rightmost pointer when k is the page's last cell.
func (c *Cursor) stepIntoChildAfterInterior() (bool, error) {
	if err := c.cachePage(); err != nil {
		return false, err
	}
	pg := c.currentPg
	page := c.currentPage
	k := c.cellIdx
	var child uint32
	if k+1 < int(page.CellCount) {
		cellOff := int(storage.CellPointer(pg.Data, contentOffset(pg.PageNum)+cellPtrOffset(page.PageType)-8, k+1, int(c.tx.pageSize)))
		if cellOff < 0 || cellOff+4 > len(pg.Data) {
			// Crafted cell pointer aimed at the page tail: stop the walk
			// instead of slicing past the buffer (navigateToNextChild's rule).
			c.endOfBTree = true
			return false, nil
		}
		child = binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
	} else {
		child = page.RightmostPtr
	}
	if child == 0 {
		c.endOfBTree = true
		return false, nil
	}
	c.path = append(c.path, cursorPathEntry{pageNum: pg.PageNum, childIdx: k + 1})
	c.pageNum = child
	c.cellIdx = 0
	c.onInteriorCell = false
	c.clearPageCache()
	c.descendToFirstLeafFromCurrent()
	return !c.endOfBTree, nil
}

// skipEmptyPosition advances the cursor past empty leaf pages, reporting ok=false
// when the walk reaches the end of the tree. Empty leaves are legal in this
// engine (an emptied index leaf stays in place, maybeRebalanceAfterDelete), and
// a scan that reads a cell at such a position would decode the page's stale
// pointer slot as an entry — so every scan-shaped reader skips first.

// skipEmptyPosition advances the cursor past empty leaf pages, reporting ok=false
// when the walk reaches the end of the tree. Empty leaves are legal in this
// engine (an emptied index leaf stays in place, maybeRebalanceAfterDelete), and
// a scan that reads a cell at such a position would decode the page's stale
// pointer slot as an entry — so every scan-shaped reader skips first.
func (c *Cursor) skipEmptyPosition() (bool, error) {
	for {
		if err := c.cachePage(); err != nil {
			return false, err
		}
		page := c.currentPage
		if page.CellCount != 0 ||
			(page.PageType != storage.PageTypeLeafTable && page.PageType != storage.PageTypeLeafIndex) {
			return true, nil
		}
		// Empty leaf: move to the next child in the tree.
		c.cellIdx = 0
		c.clearPageCache()
		c.navigateToNextChild()
		if c.endOfBTree {
			return false, nil
		}
	}
}

// skipEmptyLeaves advances the cursor past any empty leaf pages at the
// current position. The engine keeps empty leaves in the tree after deletes
// (it does not rebalance), so scans must skip them rather than stop early.

// skipEmptyLeaves advances the cursor past any empty leaf pages at the
// current position. The engine keeps empty leaves in the tree after deletes
// (it does not rebalance), so scans must skip them rather than stop early.
func (c *Cursor) skipEmptyLeaves() error {
	ok, err := c.skipEmptyPosition()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("btree: cursor at end")
	}
	return nil
}
