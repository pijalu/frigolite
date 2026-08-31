// FreeTable walks the btree rooted at rootPage and returns every
// reachable page (root + all children + overflow pages) to the
// pager freelist. Used by DROP TABLE to reclaim the table's
// storage without needing btree rebalance.
//
// This is a partial port of btree.c::sqlite3BtreeDestroy (which
// also nulls out the cell-content area and updates the pointer
// map for autovacuum databases). We only do the page-freeing
// half because DROP TABLE here is the "user requested destruction"
// path: the schema entry is being removed by the caller, so the
// cells do not need to be nulled; we just need the pages back on
// the freelist so the next VACUUM (or AutoVacuumCommit in FULL
// mode) can truncate the file.
//
// Reference: src/btree.c::sqlite3BtreeDropTable and
// sqlite3BtreeDestroy (~line 3062).

package btree

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// FreeTable returns every page reachable from rootPage to the
// pager freelist. rootPage itself is freed last. After FreeTable
// returns, the schema entry for this rootpage is the only thing
// referring to the (now-stale) btree; the caller must remove the
// schema entry, the autoincrement counter, and any FTS / shadow
// tables for the table to be fully gone.
func (t *BTree) FreeTable(rootPage uint32) error {
	if rootPage == 0 {
		return nil
	}
	var pages []uint32
	if err := t.collectAllPages(rootPage, &pages); err != nil {
		return err
	}
	// Free overflow pages first, then leaves/interior, then the
	// root last. The order doesn't actually matter for correctness
	// (the pager just appends to the freelist), but free in
	// reverse-walk order so the most-root pages are freed last
	// (which is what a human reading the freelist would expect).
	for i := len(pages) - 1; i >= 0; i-- {
		if err := t.pager.FreePage(pages[i]); err != nil {
			return err
		}
	}
	return nil
}

// collectAllPages appends rootPage and every page reachable from
// it (children, overflow pages) to out. Deduplicates by skipping
// pages already in out — defensive against cycles that shouldn't
// exist but might in a corrupted database.
func (t *BTree) collectAllPages(rootPage uint32, out *[]uint32) error {
	visited := make(map[uint32]bool)
	return t.walkAllPages(rootPage, visited, out)
}

func (t *BTree) walkAllPages(pn uint32, visited map[uint32]bool, out *[]uint32) error {
	if pn == 0 || visited[pn] {
		return nil
	}
	visited[pn] = true
	pg, err := t.pager.ReadPage(pn)
	if err != nil {
		return err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		// Corrupt page: include it in the to-free list so the
		// freelist count tracks the leaked page, then stop.
		*out = append(*out, pn)
		return nil
	}
	*out = append(*out, pn)
	if page.PageType == storage.PageTypeInteriorTable || page.PageType == storage.PageTypeInteriorIndex {
		ptrBase := coff + cellPtrOffset(page.PageType) - 8
		for i := 0; i < int(page.CellCount); i++ {
			p := storage.CellPointer(pg.Data, ptrBase, i, int(t.pageSize))
			if int(p)+4 > len(pg.Data) {
				continue
			}
			child := binary.BigEndian.Uint32(pg.Data[p : p+4])
			if child == 0 {
				continue
			}
			if err := t.walkAllPages(child, visited, out); err != nil {
				return err
			}
		}
		if page.RightmostPtr != 0 {
			if err := t.walkAllPages(page.RightmostPtr, visited, out); err != nil {
				return err
			}
		}
		return nil
	}
	// Leaf: walk overflow chains.
	var cellType storage.CellType
	if page.PageType == storage.PageTypeLeafTable {
		cellType = storage.CellTableLeaf
	} else {
		cellType = storage.CellIndexLeaf
	}
	for i := 0; i < int(page.CellCount); i++ {
		p := storage.CellPointer(pg.Data, coff, i, int(t.pageSize))
		c, err := storage.DecodeCell(pg.Data, int(p), cellType, int(t.usableSize))
		if err != nil {
			continue
		}
		if c.Overflow != 0 {
			if err := t.walkAllPages(c.Overflow, visited, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// compile-time guard: keep pager import live for future use
var _ = pager.HeaderSize
