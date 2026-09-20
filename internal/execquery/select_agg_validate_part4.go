package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

// whereReferencesBareName reports whether the WHERE tree contains a
// table-unqualified ColumnRef with exactly the given name.
func whereReferencesBareName(expr sql.Expr, name string) bool {
	if expr == nil {
		return false
	}
	found := false
	WalkExprFull(expr, func(n sql.Expr) {
		if ref, ok := n.(*sql.ColumnRef); ok && ref.Table == "" && strings.EqualFold(ref.Name, name) {
			found = true
		}
	})
	return found
}

// whereDirectAggregateScoped returns the name of the first inner-owned scalar
// aggregate found directly in s's WHERE tree, or "" when every WHERE
// aggregate is either absent, valid-by-arity, or a pure-outer aggregate of an
// aggregate query (resolve.c:1960 + resolve.c:1332 — aggnested-3.11 allows
// WHERE value2=max(value1) inside SELECT count(*) FROM t2 because count(*)
// keeps NC_AllowAgg set and max(value1) references no t2 column, so it is
// attributed to the enclosing aggregate context).
func (e *SelectEngine) whereDirectAggregateScoped(s *sql.SelectStmt) string {
	name := whereDirectAggregate(s.Where, e.ctx.Functions())
	if name == "" {
		return ""
	}
	// resolve.c:1960 — without result-set aggregates or GROUP BY, NC_AllowAgg
	// is cleared and any WHERE aggregate is a misuse.
	if !e.hasAggregates(s.Columns) && len(s.GroupBy) == 0 {
		return name
	}
	// resolve.c:1332 ownership walk — an aggregate referencing no column of
	// this SELECT's own FROM scope belongs to an outer context. count(*)
	// (no column references) stays inner-owned and remains a misuse.
	inner, innerTables := e.collectInnerColsAndTables(s)
	var bad string
	var stop bool
	WalkExprFull(s.Where, func(en sql.Expr) {
		if bad != "" || stop {
			return
		}
		switch en.(type) {
		case *sql.Subquery, *sql.ExistsExpr:
			stop = true
			return
		}
		fn, ok := en.(*sql.FuncCall)
		if !ok || !e.isAggregateFuncCallName(fn.Name) {
			return
		}
		if name := e.innerOwnedAggName(fn, inner, innerTables); name != "" {
			bad = name
		}
	})
	return bad
}

// innerOwnedAggName reports the (lowercased) aggregate name when the call's
// args/ORDER BY/FILTER reference an inner column or a qualified inner-table
// column (inner-owned, therefore a WHERE misuse). "" when outer-owned.
func (e *SelectEngine) innerOwnedAggName(fn *sql.FuncCall, inner map[string]bool, innerTables map[string]bool) string {
	refs := make([]sql.Expr, 0, len(fn.Args)+len(fn.OrderBy)+1)
	refs = append(refs, fn.Args...)
	for _, ob := range fn.OrderBy {
		refs = append(refs, ob.Expr)
	}
	if fn.Filter != nil {
		refs = append(refs, fn.Filter)
	}
	for _, r := range refs {
		if exprHasColRefInMap(r, inner) {
			return strings.ToLower(fn.Name)
		}
	}
	for _, r := range refs {
		if e.exprRefsInnerTable(r, innerTables) {
			return strings.ToLower(fn.Name)
		}
	}
	return ""
}

// exprRefsInnerTable reports whether expr contains a column reference
// qualified by one of the given (inner) table names or aliases.
func (e *SelectEngine) exprRefsInnerTable(expr sql.Expr, innerTables map[string]bool) bool {
	if expr == nil {
		return false
	}
	if ref, ok := expr.(*sql.ColumnRef); ok && ref.Table != "" {
		t := strings.ToLower(ref.Table)
		if dot := strings.IndexByte(t, '.'); dot >= 0 {
			t = t[dot+1:]
		}
		return innerTables[t]
	}
	for _, child := range aggValidateChildExprs(expr) {
		if e.exprRefsInnerTable(child, innerTables) {
			return true
		}
	}
	return false
}

// whereDirectAggregate returns the (lowercased) name of the first scalar
// aggregate function found directly in the expression tree, ignoring any
// nested inside Subquery / EXISTS nodes.
func whereDirectAggregate(expr sql.Expr, fns *function.Registry) string {
	if expr == nil {
		return ""
	}
	found := ""
	var stop bool
	WalkExprFull(expr, func(n sql.Expr) {
		if found != "" || stop {
			return
		}
		switch n.(type) {
		case *sql.Subquery, *sql.ExistsExpr:
			stop = true
			return
		}
		fn, ok := n.(*sql.FuncCall)
		if !ok {
			return
		}
		found = directAggregateName(fn, fns)
	})
	return found
}

// directAggregateName returns the lowercased name when fn is an in-scope
// aggregate for WHERE-misuse purposes. min/max are dual-natured
// (builtins.c: 2+ arguments select the scalar implementation): "WHERE
// max(a,b)!=1" is valid. And an invalid argument count is reported first, as
// an arity error, not a misuse (select1-3.9 count(f1,f2)).
func directAggregateName(fn *sql.FuncCall, fns *function.Registry) string {
	reg, found := fns.Find(fn.Name)
	if !found || reg.Type != function.TypeAggregate {
		return ""
	}
	name := strings.ToUpper(fn.Name)
	if (name == "MAX" || name == "MIN") && len(fn.Args) > 1 {
		return ""
	}
	if len(fn.Args) < reg.MinArgs || (reg.MaxArgs >= 0 && len(fn.Args) > reg.MaxArgs) {
		return ""
	}
	return strings.ToLower(fn.Name)
}

// whereRowValueCorrelatedAggCollapse reports the row-value width N when the
// WHERE clause contains a row-value comparison (=, <, >, <=, >=, <>) whose
// subquery operand is a compound or aggregate subquery with a correlated
// aggregate (SQLite raises "N columns assigned 1 values" at prepare). Returns
// 0 when no such pattern is present.
func (e *SelectEngine) whereRowValueCorrelatedAggCollapse(expr sql.Expr) int {
	if expr == nil {
		return 0
	}
	if bop, ok := expr.(*sql.BinaryOp); ok && isComparisonOp(bop.Operator) {
		if n := e.rowValueSubqCorrelatedAggWidth(bop.Left, bop.Right); n > 0 {
			return n
		}
		if n := e.rowValueSubqCorrelatedAggWidth(bop.Right, bop.Left); n > 0 {
			return n
		}
	}
	for _, child := range aggValidateChildExprs(expr) {
		if n := e.whereRowValueCorrelatedAggCollapse(child); n > 0 {
			return n
		}
	}
	return 0
}

// rowValueSubqCorrelatedAggWidth returns the LHS row-value width when one side
// of a comparison is a row value and the other side is a subquery containing a
// correlated aggregate (in the FROM-less compound/aggregate case SQLite
// collapses the subquery's vector to 1). Returns 0 when the pattern does not
// apply.
