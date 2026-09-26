package fts5

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// MULTI-INDEX OR branch evaluation (where.c whereLoopAddOr). When a WHERE is
// a top-level OR chain whose every branch is a conjunction of MATCH
// constraints on this table, C runs one sub-plan per branch: rows emerge
// branch by branch, each branch's rows in rowid order, deduplicated at first
// emission — not as a globally rowid-sorted union (fts5misc 20.3/20.4/20.5).

// MatchOrBranchRowids evaluates the WHERE's top-level OR branches and returns
// the emission-ordered rowids: branch by branch in written order, each
// branch's matches ascending, a rowid emitted at its first matching branch
// only. evalExpr resolves the MATCH right-hand sides (the same resolver the
// conjunct path uses). A nil result means the WHERE is not an all-MATCH-branch
// OR chain (fewer than two branches, or any branch holding a leaf that is not
// a usable MATCH constraint on this table) and the caller keeps its fallback.
func (t *Table) MatchOrBranchRowids(where sql.Expr, evalExpr func(sql.Expr) (interface{}, error)) ([]int64, error) {
	branches := topOrBranches(where)
	if len(branches) < 2 {
		return nil, nil
	}
	sets := make([]map[int64]bool, 0, len(branches))
	for _, branch := range branches {
		set, usable, err := t.matchBranchSet(branch, evalExpr)
		if err != nil {
			return nil, err
		}
		if !usable {
			return nil, nil
		}
		sets = append(sets, set)
	}
	return emitOrBranchRowids(t, sets), nil
}

// topOrBranches splits a top-level OR chain into its branches in written
// order; nil when the WHERE is not an OR at the top level.
func topOrBranches(where sql.Expr) []sql.Expr {
	if bop, ok := where.(*sql.BinaryOp); ok && bop.Operator == "OR" {
		return append(topOrBranches(bop.Left), topOrBranches(bop.Right)...)
	}
	return []sql.Expr{where}
}

// matchBranchSet evaluates one OR branch: a conjunction of MATCH constraints
// on this table (nested ANDs allowed, C's single sub-plan per branch). usable
// is false when the branch holds any leaf that is not a usable MATCH on this
// table — the MULTI-INDEX OR plan does not exist for such a WHERE.
func (t *Table) matchBranchSet(expr sql.Expr, evalExpr func(sql.Expr) (interface{}, error)) (set map[int64]bool, usable bool, err error) {
	if bop, ok := expr.(*sql.BinaryOp); ok && bop.Operator == "AND" {
		return t.matchBranchConjunction(bop.Left, bop.Right, evalExpr)
	}
	bop, ok := expr.(*sql.BinaryOp)
	if !ok || bop.Operator != "MATCH" {
		return nil, false, nil
	}
	col, applies := matchConstraintColumn(bop.Left, t)
	if !applies {
		return nil, false, nil
	}
	set, err = t.matchConjunctSet(bop.Right, col, evalExpr)
	if err != nil {
		return nil, false, err
	}
	return set, true, nil
}

// matchBranchConjunction intersects the two halves of an AND inside an OR
// branch (each half may itself nest further ANDs).
func (t *Table) matchBranchConjunction(left, right sql.Expr, evalExpr func(sql.Expr) (interface{}, error)) (map[int64]bool, bool, error) {
	lset, lok, err := t.matchBranchSet(left, evalExpr)
	if err != nil || !lok {
		return nil, lok, err
	}
	rset, rok, err := t.matchBranchSet(right, evalExpr)
	if err != nil || !rok {
		return nil, rok, err
	}
	return intersectTwoSets(lset, rset), true, nil
}

// intersectTwoSets returns the intersection of two rowid sets.
func intersectTwoSets(a, b map[int64]bool) map[int64]bool {
	out := make(map[int64]bool)
	for rowid := range a {
		if b[rowid] {
			out[rowid] = true
		}
	}
	return out
}

// emitOrBranchRowids orders the branch sets for emission: every branch's
// rowids ascending, a rowid emitted at its first matching branch only
// (where.c's RowSet dedup).
func emitOrBranchRowids(t *Table, sets []map[int64]bool) []int64 {
	seen := make(map[int64]bool)
	var out []int64
	for _, set := range sets {
		for _, rowid := range t.SortedMatchRowids(set) {
			if !seen[rowid] {
				seen[rowid] = true
				out = append(out, rowid)
			}
		}
	}
	return out
}
