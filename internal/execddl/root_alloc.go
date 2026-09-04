// Root-page allocation routing for DDL (P8.INCRVACUUM BUG D). All
// CREATE TABLE / CREATE INDEX root allocations go through the btree
// layer so auto-vacuum databases place every root page in the
// [3..meta[3]] root block (btreeCreateTable's pgnoMove dance,
// src/btree.c ~10150). A root outside that block interleaves with data
// pages and the commit-time vacuum drain truncates through it.
package execddl

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
)

// allocateRootPage allocates the next root page for a table or index.
func allocateRootPage(p *pager.Pager) (*pager.Page, error) {
	bt := btree.NewBTree(p, 1, true)
	return bt.AllocateRootPage()
}
