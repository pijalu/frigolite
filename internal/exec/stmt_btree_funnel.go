// SPDX-License-Identifier: GPL-3.0-or-later

package exec

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
)

// Statement b-tree funnel: every statement-execution tree the engine builds
// goes through the tableBTree* constructors, is tracked as an ownership
// lease for the innermost active statement, and is closed + recycled at
// statement teardown (see Engine.stmtBtrees for the segment contract).
//
// Reuse discipline (btree.TreeFreeList, PERF.BTREEUSE): the wrapper the
// funnel closes goes onto the ENGINE-SCOPED free list; the next acquire
// re-arms it via btree.Reinit (full reset + arm-generation bump) instead of
// allocating a fresh one — the one small allocation per statement the point
// SELECT/DDL profiles showed. A release whose lease generation no longer
// matches the wrapper's current generation is STALE (ownership moved) and
// no-ops: this is what keeps the recycling structurally safe against the
// late-Close/re-arm hazard that removed the first (global-pool) wrapper
// pooling.

// newStatementBTree builds a b-tree wrapper for a statement, reusing a
// closed wrapper from the engine's free list when one is available
// (btree.TreeFreeList + btree.Reinit: full reset + arm-generation bump).
// Only the funnel-tracked constructors call this — the wrapper enters the
// statement's lease segment and comes back at teardown.
func (e *Engine) newStatementBTree(pg *pager.Pager, rootPage uint32, isTable bool) *btree.BTree {
	if t := e.treeFree.Get(); t != nil {
		return t.Reinit(pg, rootPage, isTable)
	}
	return btree.NewBTree(pg, rootPage, isTable)
}

// tablePager returns the pager that owns the given table: the attached
// database's pager for tables in ATTACHed databases, else the main pager.
func (e *Engine) tablePager(tableName string) *pager.Pager {
	pg := e.pager
	if _, ctx, err := e.findTable(tableName); err == nil && ctx != nil && ctx.Pager != nil {
		pg = ctx.Pager
	}
	return pg
}

// tableBTree creates a BTree for a table, using the engine's tracked root page.
func (e *Engine) tableBTree(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	t := e.newStatementBTree(e.tablePager(tableName), e.rootPage(tableName, schemaRoot), isTable)
	e.trackStatementBTree(t)
	return t
}

// tableBTreeForName resolves the table's owning database context and builds a
// BTree over that context's pager (a table in an ATTACHed database lives on
// the attached pager, not the main pager).
func (e *Engine) tableBTreeForName(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	t := e.newStatementBTree(e.tablePager(tableName), e.rootPage(tableName, schemaRoot), isTable)
	e.trackStatementBTree(t)
	return t
}

// tableBTreePg creates a BTree for a table using a specific pager.
func (e *Engine) tableBTreePg(pg *pager.Pager, tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	t := e.newStatementBTree(pg, e.rootPage(tableName, schemaRoot), isTable)
	e.trackStatementBTree(t)
	return t
}

// trackStatementBTree records a b-tree wrapper for release at the end of the
// innermost active statement (see Engine.stmtBtrees). Trees built outside any
// Engine.Exec frame are not tracked; they keep the finalizer-only lifecycle.
// The record is the ownership lease (wrapper + arm generation) the release
// path re-checks before closing and recycling.
func (e *Engine) trackStatementBTree(t *btree.BTree) {
	if e.stmtBtreeDepth == 0 || t == nil {
		return
	}
	e.stmtBtrees = append(e.stmtBtrees, btree.TreeLease{Tree: t, Gen: t.Generation()})
}

// releaseStatementTrees closes the b-tree wrappers created since the given
// statement-entry mark and truncates the tracking slice. BTree.Close is
// idempotent, so trees already released through a tighter scope are no-ops.
//
// Each release re-checks its lease generation first (btree.TreeLease): a
// wrapper whose current generation differs from the one its statement
// acquired under has been re-armed for another owner meanwhile — the close
// would be STALE, so it no-ops (no Close, no free-list Put). For a live
// wrapper the close runs and the closed wrapper returns to the engine's
// free list for the next statement's acquire.
func (e *Engine) releaseStatementTrees(mark int) {
	if mark >= len(e.stmtBtrees) {
		return
	}
	trees := e.stmtBtrees[mark:]
	e.stmtBtrees = e.stmtBtrees[:mark]
	for i := len(trees) - 1; i >= 0; i-- {
		lease := trees[i]
		t := lease.Tree
		if t.Closed() || t.Generation() != lease.Gen {
			continue
		}
		t.Close()
		e.treeFree.Put(t)
	}
}
