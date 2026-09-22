package exec

import (
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// echoOrBranch is one OR branch of a MULTI-INDEX OR plan: an equality
// between a source-table column and a constant.
type echoOrBranch struct {
	col   int         // index into the source table's column defs
	value interface{} // the constant side (int64, float64, or string)
}

// orderRowsByOrBranches reproduces the row ORDER of SQLite's MULTI-INDEX OR
// plan (where.c whereLoopAddOr + RowSet dedup) for an echo vtab scan whose
// WHERE is a top-level OR of indexable equality terms: each branch is probed
// through its own index, so rows emerge branch by branch — every row matching
// the FIRST branch (in source scan order), then rows matching only the second
// branch, and so on, with each row emitted at its first matching branch only.
// The reorder applies only when every branch is such an equality on a column
// that LEADS an index on the source table (test8.c getIndexArray's aIndex —
// exactly the constraints echo claims in xBestIndex); any other WHERE keeps
// scan order. Row order is all the reordering does: no row is added, dropped,
// or changed.
func (e *Engine) orderRowsByOrBranches(vtabName string, srcEntry *schema.Entry, defs []sql.ColumnDef, where sql.Expr, rows [][]interface{}, rowids []int64) ([][]interface{}, []int64) {
	if where == nil || len(rows) < 2 {
		return rows, rowids
	}
	branches := echoOrEqualityBranches(vtabName, where, defs)
	if branches == nil || !e.orBranchesOnIndexedColumns(srcEntry, defs, branches) {
		return rows, rowids
	}
	return reorderRowsByBranchRank(orBranchRanks(defs, branches, rows), rows, rowids)
}

// orBranchesOnIndexedColumns reports whether every branch's column leads an
// index on the source table (the columns echo claims in xBestIndex).
func (e *Engine) orBranchesOnIndexedColumns(srcEntry *schema.Entry, defs []sql.ColumnDef, branches []echoOrBranch) bool {
	for _, b := range branches {
		if !e.echoSourceLeadsIndex(srcEntry.Name, defs[b.col].Name) {
			return false
		}
	}
	return true
}

// orBranchRanks assigns each row the index of its first matching OR branch
// (rows matching no branch rank len(branches) and trail last).
func orBranchRanks(defs []sql.ColumnDef, branches []echoOrBranch, rows [][]interface{}) []int {
	rank := make([]int, len(rows))
	for i := range rank {
		rank[i] = len(branches)
	}
	for i, row := range rows {
		for bi, b := range branches {
			if rowMatchesBranch(row[b.col], b, defs) {
				rank[i] = bi
				break
			}
		}
	}
	return rank
}

// rowMatchesBranch evaluates one equality branch against a raw row value
// with the column's affinity applied (NULL never matches).
func rowMatchesBranch(v interface{}, b echoOrBranch, defs []sql.ColumnDef) bool {
	if v == nil {
		return false
	}
	aff := defs[b.col].Type
	rv := util.ApplyColumnAffinity(v, aff)
	lv := util.ApplyColumnAffinity(b.value, aff)
	return util.CompareValues(rv, lv) == 0
}

// reorderRowsByBranchRank stably reorders rows (and their rowids) by rank.
func reorderRowsByBranchRank(rank []int, rows [][]interface{}, rowids []int64) ([][]interface{}, []int64) {
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool { return rank[order[x]] < rank[order[y]] })
	sortedRows := make([][]interface{}, len(rows))
	sortedRowids := make([]int64, len(rowids))
	for i, src := range order {
		sortedRows[i] = rows[src]
		if src < len(rowids) {
			sortedRowids[i] = rowids[src]
		}
	}
	return sortedRows, sortedRowids
}

// echoOrEqualityBranches splits a top-level OR chain into constant-equality
// branches. Every branch must be "<column> = <constant>" (either operand
// order), the column must resolve in defs (unqualified, or qualified by the
// vtab's own name), and the constant must be a non-NULL numeric or string
// literal. Any other shape returns nil: the WHERE is not eligible for the
// MULTI-INDEX OR row order.
func echoOrEqualityBranches(vtabName string, where sql.Expr, defs []sql.ColumnDef) []echoOrBranch {
	var branches []echoOrBranch
	var walk func(expr sql.Expr) bool
	walk = func(expr sql.Expr) bool {
		bo, ok := expr.(*sql.BinaryOp)
		if !ok {
			return false
		}
		if strings.EqualFold(bo.Operator, "OR") {
			return walk(bo.Left) && walk(bo.Right)
		}
		if !strings.EqualFold(bo.Operator, "=") {
			return false
		}
		b, ok := echoEqualityBranch(vtabName, bo.Left, bo.Right, defs)
		if !ok {
			return false
		}
		branches = append(branches, b)
		return true
	}
	if !walk(where) || len(branches) < 2 {
		return nil
	}
	return branches
}

// echoEqualityBranch resolves one equality's column side against defs and its
// constant side (a non-NULL numeric or string literal). ok is false when
// either side does not fit (x = NULL yields no rows, so it is not a plan
// branch either).
func echoEqualityBranch(vtabName string, left, right sql.Expr, defs []sql.ColumnDef) (echoOrBranch, bool) {
	ref, ok := left.(*sql.ColumnRef)
	if !ok {
		ref, ok = right.(*sql.ColumnRef)
		if !ok {
			return echoOrBranch{}, false
		}
		left, right = right, left
	}
	if ref.Table != "" && !strings.EqualFold(ref.Table, vtabName) {
		return echoOrBranch{}, false
	}
	col := -1
	for i := range defs {
		if strings.EqualFold(defs[i].Name, ref.Name) {
			col = i
			break
		}
	}
	if col < 0 {
		return echoOrBranch{}, false
	}
	switch lit := right.(type) {
	case *sql.NumericLit:
		if n, err := strconv.ParseInt(lit.Value, 10, 64); err == nil {
			return echoOrBranch{col: col, value: n}, true
		}
		if f, err := strconv.ParseFloat(lit.Value, 64); err == nil {
			return echoOrBranch{col: col, value: f}, true
		}
	case *sql.StringLit:
		return echoOrBranch{col: col, value: lit.Value}, true
	}
	return echoOrBranch{}, false
}

// echoSourceLeadsIndex reports whether an index on srcTable leads with the
// named column (test8.c getIndexArray: every index contributes its left-most
// column to echo_vtab.aIndex, and echo's xBestIndex claims constraints only
// on such columns).
func (e *Engine) echoSourceLeadsIndex(srcTable, colName string) bool {
	entry, _, err := e.findTable(srcTable)
	if err != nil || entry == nil {
		return false
	}
	indexes, ierr := e.schema.FindIndexesForTable(entry.Name)
	if ierr != nil {
		return false
	}
	for _, idx := range indexes {
		if strings.EqualFold(firstIndexColumn(idx.SQL, entry.Name), colName) {
			return true
		}
	}
	return false
}
