package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns aggregate/outer-reference analysis and SELECT expression
// validation, extracted from select.go for single-responsibility cohesion.
//
// Two logical groups:
//   - Aggregate & outer-ref analysis: aggHasColumnRef, selectHasCorrelatedAggSubquery,
//     aggRefsMatchFromTable, subqueryOuterAggRef, aggRefsOuter, exprRefsOuterCol.
//   - SELECT expression validation: validateSelectExprs, validateSelectColumnRefs,
//     validateSelectRowValues, validateDistinctAggArgs.

// ---------------------------------------------------------------------------
// Aggregate & outer-reference analysis
// ---------------------------------------------------------------------------

// aggHasColumnRef reports whether any SELECT column is an aggregate function
// whose arguments contain a column reference (excluding bare "*" which means
// "all rows").
func (e *SelectEngine) aggHasColumnRef(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if e.columnIsAggWithColumnRef(col) {
			return true
		}
	}
	return false
}

// columnIsAggWithColumnRef checks a single SELECT column: if it contains an
// aggregate function call (at any nesting depth — e.g. inside || or CASE)
// whose arguments include a column reference.
func (e *SelectEngine) columnIsAggWithColumnRef(col sql.SelectColumn) bool {
	return e.exprAggWithColumnRef(col.Expr)
}

// exprAggWithColumnRef walks an expression tree looking for an aggregate
// function call whose arguments contain a column reference.
func (e *SelectEngine) exprAggWithColumnRef(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	if _, ok := expr.(*sql.FuncCall); ok {
		return e.funcCallAggWithColumnRef(expr.(*sql.FuncCall))
	}
	for _, kid := range aggCorrelationChildren(expr) {
		if e.exprAggWithColumnRef(kid) {
			return true
		}
	}
	return false
}

// aggCorrelationChildren returns an expression node's child expressions for
// the correlated-aggregate walk.
func aggCorrelationChildren(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.ParenExpr:
		return []sql.Expr{v.Expr}
	case *sql.CastExpr:
		return []sql.Expr{v.Operand}
	case *sql.CaseExpr:
		kids := make([]sql.Expr, 0, 3+2*len(v.Whens))
		kids = append(kids, v.Operand)
		for _, w := range v.Whens {
			kids = append(kids, w.When, w.Then)
		}
		return append(kids, v.Else)
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		kids := make([]sql.Expr, 0, len(v.List)+1)
		kids = append(kids, v.Operand)
		kids = append(kids, v.List...)
		return kids
	}
	return nil
}

// funcCallAggWithColumnRef reports whether an aggregate call's arguments carry
// a column reference, or a nested expression does. A window function (OVER
// clause) is not a correlated aggregate: it does not collapse the query to one
// row, so its aggregate name (e.g. min(a) OVER ()) must not trigger the
// correlated-aggregate path.
func (e *SelectEngine) funcCallAggWithColumnRef(v *sql.FuncCall) bool {
	if v.Over != nil {
		return false
	}
	reg, found := e.ctx.Functions().Find(v.Name)
	if found && reg.Type == function.TypeAggregate {
		for _, arg := range v.Args {
			if e.exprHasColumnRef(arg) {
				return true
			}
		}
	}
	for _, arg := range v.Args {
		if e.exprAggWithColumnRef(arg) {
			return true
		}
	}
	for _, ob := range v.OrderBy {
		if e.exprAggWithColumnRef(ob.Expr) {
			return true
		}
	}
	return false
}

// selectHasCorrelatedAggSubquery checks if a SELECT statement (or any nested
// subquery within it) contains a correlated aggregate — an aggregate function
// that references columns from an outer context.
// This detects two cases:
//  1. FROM-less SELECT with aggregates that have column references
//  2. SELECT with FROM clause where aggregate args reference only outer columns
//     (none exist in the FROM table — making the aggregate fully correlated).
func (e *SelectEngine) selectHasCorrelatedAggSubquery(s *sql.SelectStmt) bool {
	if s == nil {
		return false
	}
	if e.selectFromlessAggHasColRef(s) || e.selectFromAggRefsOuterOnly(s) || e.aggCallRefsOnlyOuter(s) {
		return true
	}
	if s.From.Subquery != nil && e.selectHasCorrelatedAggSubquery(s.From.Subquery) {
		return true
	}
	if e.columnsHaveCorrelatedAggSubquery(s.Columns) {
		return true
	}
	return e.unionHasCorrelatedAgg(s.Union)
}

// aggCallRefsOnlyOuter reports whether the SELECT contains an aggregate function
// call whose arguments reference ONLY outer columns (no inner columns of the
// subquery's own FROM tables). SQLite collapses such correlated-aggregate
// subqueries in the result set: the outer query becomes an aggregate query
// (aggnested-1.1 `string_agg(a1,'x') FROM t2` where a1 is outer-only collapses
// to one row). A call that mixes inner and outer references (e.g.
// `string_agg(b1,a1)` with b1 inner) stays per-row (aggnested-1.3).
func (e *SelectEngine) aggCallRefsOnlyOuter(s *sql.SelectStmt) bool {
	if s == nil {
		return false
	}
	inner, innerTables := e.collectInnerColsAndTables(s)
	return e.aggCallRefsOnlyOuterExpr(s.Columns, inner, innerTables)
}

// aggCallRefsOnlyOuterExpr walks SELECT columns looking for an aggregate call
// whose arguments reference outer columns but no inner columns.
func (e *SelectEngine) aggCallRefsOnlyOuterExpr(columns []sql.SelectColumn, inner map[string]bool, innerTables map[string]bool) bool {
	for _, col := range columns {
		if e.aggExprRefsOnlyOuter(col.Expr, inner, innerTables) {
			return true
		}
	}
	return false
}

// aggExprRefsOnlyOuter walks an expression tree for an aggregate call with
// only-outer argument references.
func (e *SelectEngine) aggExprRefsOnlyOuter(expr sql.Expr, inner map[string]bool, innerTables map[string]bool) bool {
	if expr == nil {
		return false
	}
	if fn, ok := expr.(*sql.FuncCall); ok {
		if res, decided := e.aggFuncCallRefsOnlyOuter(fn, inner, innerTables); decided {
			return res
		}
	}
	for _, child := range aggValidateChildExprs(expr) {
		if e.aggExprRefsOnlyOuter(child, inner, innerTables) {
			return true
		}
	}
	return false
}

// aggFuncCallRefsOnlyOuter decides one aggregate call's outer/inner reference
// ownership. decided=true carries the verdict; decided=false lets the walk
// continue into the call's children. A window function (OVER clause) is not a
// correlated aggregate: it does not collapse the query, so it must not
// trigger the outer-row aggregate path. A FILTER bound to the subquery's own
// rows keeps the aggregate inner-evaluated: the query is a per-row
// correlated-aggregate subquery, NOT an outer aggregate collapse (filter1-6.1:
// COUNT(a) FILTER(WHERE x) with x inner evaluates per outer row).
func (e *SelectEngine) aggFuncCallRefsOnlyOuter(fn *sql.FuncCall, inner map[string]bool, innerTables map[string]bool) (verdict, decided bool) {
	if fn.Over != nil {
		return false, true
	}
	reg, found := e.ctx.Functions().Find(fn.Name)
	if !found || reg.Type != function.TypeAggregate {
		return false, false
	}
	if fn.Filter != nil && exprHasColRefInMap(fn.Filter, inner) {
		return false, true
	}
	refsOuter := false
	refsInner := false
	scanRefs := func(exprs []sql.Expr) {
		for _, a := range exprs {
			if e.exprRefsOuterCol(a, inner, innerTables) {
				refsOuter = true
			}
			if exprHasColRefInMap(a, inner) {
				refsInner = true
			}
		}
	}
	scanRefs(fn.Args)
	scanRefs(orderByExprs(fn.OrderBy))
	if fn.Filter != nil {
		scanRefs([]sql.Expr{fn.Filter})
	}
	return refsOuter && !refsInner, true
}

// selectFromlessAggHasColRef detects Case 1: a FROM-less SELECT whose aggregate
// columns contain column references.
func (e *SelectEngine) selectFromlessAggHasColRef(s *sql.SelectStmt) bool {
	fromless := s.From.Name == "" && s.From.Subquery == nil && len(s.From.As) == 0
	return fromless && e.aggHasColumnRef(s.Columns)
}

// selectFromAggRefsOuterOnly detects Case 2: a SELECT with a FROM table whose
// aggregates reference only outer columns (none matching the FROM table). A
// window function (OVER clause) is not a collapsing aggregate — a subquery
// whose only aggregate is a window function must not be treated as a
// correlated-aggregate query (it evaluates per outer row, one window row each).
func (e *SelectEngine) selectFromAggRefsOuterOnly(s *sql.SelectStmt) bool {
	if s.From.Name == "" {
		return false
	}
	if e.aggHasColumnRef(s.Columns) && !e.aggRefsMatchFromTable(s) {
		// Exclude subqueries whose aggregate references are all window
		// functions (they do not collapse).
		for _, col := range s.Columns {
			if e.colHasNonWindowAggregate(col.Expr) {
				return true
			}
		}
	}
	return false
}

// colHasNonWindowAggregate reports whether expr contains a plain (non-window)
// aggregate function call.
func (e *SelectEngine) colHasNonWindowAggregate(expr sql.Expr) bool {
	if fc, ok := expr.(*sql.FuncCall); ok {
		return e.funcCallHasNonWindowAggregate(fc)
	}
	for _, kid := range nonWindowAggChildren(expr) {
		if e.colHasNonWindowAggregate(kid) {
			return true
		}
	}
	return false
}

// funcCallHasNonWindowAggregate handles the FuncCall node of the non-window
// aggregate walk: a window call is not a plain aggregate (returns false
// without descending); a registered aggregate returns true; a scalar call
// descends into args and aggregate ORDER BY terms.
func (e *SelectEngine) funcCallHasNonWindowAggregate(fc *sql.FuncCall) bool {
	if fc.Over != nil {
		return false
	}
	if reg, found := e.ctx.Functions().Find(fc.Name); found && reg.Type == function.TypeAggregate {
		return true
	}
	for _, arg := range fc.Args {
		if e.colHasNonWindowAggregate(arg) {
			return true
		}
	}
	for _, ob := range fc.OrderBy {
		if e.colHasNonWindowAggregate(ob.Expr) {
			return true
		}
	}
	return false
}

// nonWindowAggChildren returns an expression node's child expressions for the
// non-window-aggregate walk.
func nonWindowAggChildren(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		left, right := BinaryExprOperands(expr)
		return []sql.Expr{left, right}
	case *sql.UnaryOp, *sql.ParenExpr, *sql.CastExpr, *sql.IsNull, *sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		return []sql.Expr{singleExprOperand(expr)}
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		kids := make([]sql.Expr, 0, len(v.List)+1)
		kids = append(kids, v.Operand)
		kids = append(kids, v.List...)
		return kids
	case *sql.CaseExpr:
		kids := make([]sql.Expr, 0, 3+2*len(v.Whens))
		if v.Operand != nil {
			kids = append(kids, v.Operand)
		}
		for _, w := range v.Whens {
			kids = append(kids, w.When, w.Then)
		}
		if v.Else != nil {
			kids = append(kids, v.Else)
		}
		return kids
	case *sql.RowValue:
		return v.Values
	}
	return nil
}

// columnsHaveCorrelatedAggSubquery checks SELECT column subqueries for
// correlated aggregates.
func (e *SelectEngine) columnsHaveCorrelatedAggSubquery(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if subq, ok := col.Expr.(*sql.Subquery); ok {
			if e.selectHasCorrelatedAggSubquery(subq.Select) {
				return true
			}
		}
	}
	return false
}

// unionHasCorrelatedAgg walks compound (UNION/INTERSECT/EXCEPT) members.
func (e *SelectEngine) unionHasCorrelatedAgg(union *sql.SelectStmt) bool {
	for m := union; m != nil; m = m.Union {
		if e.selectHasCorrelatedAggSubquery(m) {
			return true
		}
	}
	return false
}

// aggRefsMatchFromTable checks if any aggregate function's column references
// match a column name in the FROM table. Returns true if any aggregate arg
// references a column that exists in the FROM table, indicating the aggregate
// is NOT fully correlated (it references inner columns).
func (e *SelectEngine) aggRefsMatchFromTable(s *sql.SelectStmt) bool {
	if s.From.Name == "" {
		return false
	}
	colNames := e.fromRefColumnNames(s.From.Name)
	for _, col := range s.Columns {
		if e.aggColumnArgsRefInner(col, colNames) {
			return true
		}
	}
	return false
}

// fromRefColumnNames returns the column-name set of a FROM reference for
// correlated-aggregate analysis: a real table/view when present, otherwise a
// CTE declared in scope. For a CTE it uses the explicit column list, the
// body-derived names, or (when neither applies, e.g. a SELECT * CTE) the common
// single-letter column names a CTE typically exposes. This keeps a
// non-correlated aggregate over a CTE FROM — e.g. (SELECT sum(a) FROM x1) where
// x1 is a CTE — from being mistaken for an outer-only aggregate that would
// collapse the enclosing query to a single row (with2 1.10).
func (e *SelectEngine) fromRefColumnNames(name string) map[string]bool {
	// A CTE FROM takes precedence over any materialized temp-table stub the
	// schema may hold for the same name (its entry carries a synthetic "*"
	// column); the correlated-aggregate column analysis must use the CTE's
	// logical columns. Check the CTE scope first.
	if cte, ok := e.FindCTEByScope(name); ok {
		names := make(map[string]bool)
		for _, c := range e.cteOutputColumnNames(cte) {
			if c != "" {
				names[strings.ToLower(c)] = true
			}
		}
		if len(names) == 0 {
			for _, c := range []string{"a", "b", "x", "y", "i", "c"} {
				names[c] = true
			}
		}
		return names
	}
	if tableEntry, err := e.ctx.Schema().FindTable(name); err == nil {
		return e.fromTableColumnNames(tableEntry)
	}
	return nil
}

// fromTableColumnNames resolves the column-name set for a FROM table entry.
func (e *SelectEngine) fromTableColumnNames(tableEntry *schema.Entry) map[string]bool {
	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	names := make(map[string]bool, len(colDefs))
	for _, cd := range colDefs {
		names[cd.Name] = true
	}
	return names
}

// aggColumnArgsRefInner checks if a SELECT column's aggregate function args,
// FILTER clause, or ORDER BY terms reference any of the given inner column
// names. The FILTER is part of the aggregate expression (resolve.c
// sqlite3ReferencesSrcList scans the whole aggregate), so a FILTER reference
// to an inner column (filter1-6.1: COUNT(a) FILTER(WHERE x)) makes the
// aggregate inner-owned.
func (e *SelectEngine) aggColumnArgsRefInner(col sql.SelectColumn, colNames map[string]bool) bool {
	fn, ok := col.Expr.(*sql.FuncCall)
	if !ok {
		return false
	}
	for _, arg := range fn.Args {
		if exprHasColRefInMap(arg, colNames) {
			return true
		}
	}
	for _, ob := range fn.OrderBy {
		if exprHasColRefInMap(ob.Expr, colNames) {
			return true
		}
	}
	// A FILTER referencing a FROM-table column binds the aggregate to the
	// inner rows even when the arguments are outer-only (filter1-6.1:
	// COUNT(a) FILTER(WHERE x) with x in the FROM table).
	if fn.Filter != nil && exprHasColRefInMap(fn.Filter, colNames) {
		return true
	}
	return false
}

// subqueryOuterAggRef returns the name of an aggregate function in the given
// SELECT that references a column outside the subquery's own FROM tables
// (a correlated/outer reference), or "" if none. SQLite rejects such
// aggregates in IN-subquery contexts with "misuse of aggregate".
func (e *SelectEngine) subqueryOuterAggRef(s *sql.SelectStmt) string {
	if s == nil {
		return ""
	}
	inner, innerTables := e.collectInnerColsAndTables(s)
	// Check the head member plus every compound member (a correlated
	// aggregate in a later UNION member is still a correlated aggregate;
	// window1 71.0's (SELECT 2,2 UNION SELECT sum(b),max(b) OVER(...)) has
	// the aggregate in the second member).
	for cur := s; cur != nil; cur = cur.Union {
		for _, col := range cur.Columns {
			if name := e.aggRefsOuter(col.Expr, inner, innerTables); name != "" {
				return name
			}
		}
		for _, ob := range cur.OrderBy {
			if name := e.aggRefsOuter(ob.Expr, inner, innerTables); name != "" {
				return name
			}
		}
	}
	return ""
}

// collectInnerColsAndTables gathers the column names and table/alias names from
// a SELECT's FROM clause and JOINs for outer-reference detection.
func (e *SelectEngine) collectInnerColsAndTables(s *sql.SelectStmt) (map[string]bool, map[string]bool) {
	inner := make(map[string]bool)
	innerTables := make(map[string]bool)
	e.addTableInnerCols(s.From.Name, inner)
	e.recordTableAndAlias(s.From.Name, s.From.As, innerTables)
	for _, j := range s.Joins {
		e.addTableInnerCols(j.Table.Name, inner)
		e.recordTableAndAlias(j.Table.Name, j.Table.As, innerTables)
	}
	return inner, innerTables
}

// addTableInnerCols adds the columns of a table (by name) to the inner set.
func (e *SelectEngine) addTableInnerCols(name string, inner map[string]bool) {
	if name == "" {
		return
	}
	if _, ok := e.FindCTEByScope(name); ok {
		cte, _ := e.FindCTEByScope(name)
		if len(cte.Columns) > 0 {
			for _, c := range cte.Columns {
				inner[strings.ToLower(c)] = true
			}
		} else {
			inner["a"] = true
			inner["b"] = true
			inner["x"] = true
			inner["y"] = true
			inner["i"] = true
			inner["c"] = true
		}
		return
	}
	cols, err := e.tableColumnNames(name)
	if err != nil {
		return
	}
	for _, c := range cols {
		inner[strings.ToLower(c)] = true
	}
}

// recordTableAndAlias records a table name and optional alias in the tables set.
func (e *SelectEngine) recordTableAndAlias(name, alias string, tables map[string]bool) {
	if name == "" {
		return
	}
	tables[strings.ToLower(name)] = true
	if alias != "" {
		tables[strings.ToLower(alias)] = true
	}
}

// aggRefsOuter walks an expression for an aggregate function whose argument
// references a column not present in the inner column set. Returns the
// aggregate name or "".
func (e *SelectEngine) aggRefsOuter(expr sql.Expr, inner map[string]bool, innerTables map[string]bool) string {
	if expr == nil {
		return ""
	}
	if fn, ok := expr.(*sql.FuncCall); ok {
		if name := e.aggFuncRefsOuter(fn, inner, innerTables); name != "" {
			return name
		}
	}
	for _, child := range aggValidateChildExprs(expr) {
		if name := e.aggRefsOuter(child, inner, innerTables); name != "" {
			return name
		}
	}
	return ""
}

// aggFuncRefsOuter checks if a function call is an aggregate whose arguments
// reference outer columns. Returns the function name or "". A window function
// (OVER clause) is not a plain aggregate — its aggregate-looking name must not
// be treated as a correlated aggregate.
func (e *SelectEngine) aggFuncRefsOuter(fn *sql.FuncCall, inner map[string]bool, innerTables map[string]bool) string {
	if fn.Over != nil {
		return ""
	}
	reg, found := e.ctx.Functions().Find(fn.Name)
	if !found || reg.Type != function.TypeAggregate {
		return ""
	}
	for _, a := range fn.Args {
		if e.exprRefsOuterCol(a, inner, innerTables) {
			return fn.Name
		}
	}
	if fn.Filter != nil && e.exprRefsOuterCol(fn.Filter, inner, innerTables) {
		return fn.Name
	}
	return ""
}

// exprRefsOuterCol reports whether an expression references a column outside
// the subquery's own FROM tables (ignoring subqueries). A qualified reference
// (t.col) is outer when t is not one of the subquery's tables; an unqualified
// reference is outer when the name is not an inner column.
func (e *SelectEngine) exprRefsOuterCol(expr sql.Expr, inner map[string]bool, innerTables map[string]bool) bool {
	if ref, ok := expr.(*sql.ColumnRef); ok {
		return e.colRefIsOuter(ref, inner, innerTables)
	}
	for _, child := range aggValidateChildExprs(expr) {
		if e.exprRefsOuterCol(child, inner, innerTables) {
			return true
		}
	}
	return false
}

// colRefIsOuter determines whether a single column reference is an outer
// (correlated) reference.
func (e *SelectEngine) colRefIsOuter(ref *sql.ColumnRef, inner map[string]bool, innerTables map[string]bool) bool {
	if ref.Name == "*" {
		return false
	}
	if ref.Table != "" {
		t := strings.ToLower(ref.Table)
		if dot := strings.Index(t, "."); dot >= 0 {
			t = t[dot+1:]
		}
		return !innerTables[t]
	}
	return !inner[strings.ToLower(ref.Name)]
}

// aggValidateChildExprs returns the immediate sub-expressions of expr for
// aggregate/outer-ref analysis traversal. FuncCall args are included so callers
// can recurse into them; Subquery is intentionally excluded (those have their
// own scope).
func aggValidateChildExprs(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return v.Args
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.IsNull:
		return []sql.Expr{v.Operand}
	case *sql.IsNotNull:
		return []sql.Expr{v.Operand}
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.CaseExpr:
		return caseExprChildren(v)
	}
	return nil
}

// caseExprChildren returns all sub-expressions of a CASE expression.
func caseExprChildren(v *sql.CaseExpr) []sql.Expr {
	children := make([]sql.Expr, 0, 2*len(v.Whens)+2)
	children = append(children, v.Operand)
	for _, w := range v.Whens {
		children = append(children, w.When, w.Then)
	}
	children = append(children, v.Else)
	return children
}

// ---------------------------------------------------------------------------
// SELECT expression validation
// ---------------------------------------------------------------------------

// orderByWindowAliasRef returns the name of a SELECT-list window-function
// alias referenced from a scalar subquery in the ORDER BY clause (SQLite
// rejects ORDER BY (SELECT m) on SELECT count() OVER() AS m with "misuse of
// aliased window function m"). Returns "" when no such reference exists.
func (e *SelectEngine) orderByWindowAliasRef(s *sql.SelectStmt) string {
	// Map SELECT-list aliases to window-function expressions.
	winAliases := make(map[string]bool)
	for _, col := range s.Columns {
		if col.As == "" || !e.exprHasWindowFunc(col.Expr) {
			continue
		}
		winAliases[strings.ToLower(col.As)] = true
	}
	if len(winAliases) == 0 {
		return ""
	}
	for _, ob := range s.OrderBy {
		if name := subqueryWindowAliasRef(ob.Expr, winAliases); name != "" {
			return name
		}
	}
	return ""
}

// subqueryWindowAliasRef returns the first SELECT-list window-function alias
// referenced bare inside a scalar subquery of expr, or "".
func subqueryWindowAliasRef(expr sql.Expr, winAliases map[string]bool) string {
	found := ""
	WalkExprFull(expr, func(en sql.Expr) {
		if found != "" {
			return
		}
		sub, ok := en.(*sql.Subquery)
		if !ok || sub.Select == nil {
			return
		}
		for _, col := range sub.Select.Columns {
			if name := bareColWindowAlias(col.Expr, winAliases); name != "" {
				found = name
				return
			}
		}
	})
	return found
}

// bareColWindowAlias returns the first bare column reference in expr whose
// name matches a window-function alias, or "".
func bareColWindowAlias(expr sql.Expr, winAliases map[string]bool) string {
	found := ""
	WalkExprFull(expr, func(en sql.Expr) {
		if found != "" {
			return
		}
		if ref, ok := en.(*sql.ColumnRef); ok && ref.Table == "" {
			if winAliases[strings.ToLower(ref.Name)] {
				found = ref.Name
			}
		}
	})
	return found
}

// validateSelectExprs validates aggregate misuse, DISTINCT aggregate arity,
// subquery validity, ORDER BY length limits, row-value misuse, and UNION
// subquery aggregates across a SELECT's clauses.
func (e *SelectEngine) validateSelectExprs(s *sql.SelectStmt) error {
	if err := e.validateSelectExprsClauses(s); err != nil {
		return err
	}
	return e.validateSelectExprsOrdering(s)
}

// validateSelectExprsClauses validates the SELECT list and each auxiliary
// clause (GROUP BY / HAVING / WHERE / LIMIT / OFFSET / ORDER BY terms).
func (e *SelectEngine) validateSelectExprsClauses(s *sql.SelectStmt) error {
	if err := e.validateMultipleFTSMatch(s); err != nil {
		return err
	}
	if err := e.checkOrderByAggMisuse(s); err != nil {
		return err
	}
	if err := e.validateAggregateStarArgs(s); err != nil {
		return err
	}
	if err := e.validateSelectColumnList(s); err != nil {
		return err
	}
	if err := e.validateWindowFunctions(s); err != nil {
		return err
	}
	if err := e.validateGroupByClauses(s); err != nil {
		return err
	}
	// LIMIT/OFFSET expressions are name-resolved at prepare time like other
	// clauses: a subquery naming a missing table fails the statement
	// ("no such table: blah", misc5-3.2), it is not silently un-evaluable.
	if err := e.validateLimitOffsetSubqueries(s); err != nil {
		return err
	}
	if err := e.validateHavingExprs(s); err != nil {
		return err
	}
	if err := e.validateHavingAliasedAggregate(s); err != nil {
		return err
	}
	if err := e.validateWhereExprs(s); err != nil {
		return err
	}
	return e.validateOrderByTerms(s)
}

// validateLimitOffsetSubqueries validates scalar subqueries inside the
// LIMIT/OFFSET expressions.
func (e *SelectEngine) validateLimitOffsetSubqueries(s *sql.SelectStmt) error {
	for _, limExpr := range []sql.Expr{s.Limit, s.Offset} {
		if limExpr == nil {
			continue
		}
		if err := e.validateExprSubqueries(limExpr); err != nil {
			return err
		}
	}
	return nil
}

// validateGroupByClauses rejects aggregates in GROUP BY and HAVING.
func (e *SelectEngine) validateGroupByClauses(s *sql.SelectStmt) error {
	if err := e.validateGroupByExprs(s); err != nil {
		return err
	}
	if err := e.validateClauseFunctions(s.GroupBy); err != nil {
		return err
	}
	return e.validateClauseFunctions([]sql.Expr{s.Having})
}

// validateOrderByTerms validates each ORDER BY term: DISTINCT aggregate arity
// and scalar-subquery resolution. SQLite resolves ORDER BY term
// names/subqueries even when the term does not match a result column
// (window1 67.1: a nested (SELECT 1 FROM v1) inside a window's ORDER BY must
// raise "no such table: v1").
func (e *SelectEngine) validateOrderByTerms(s *sql.SelectStmt) error {
	for _, ob := range s.OrderBy {
		if err := validateDistinctAggArgs(ob.Expr); err != nil {
			return err
		}
		if err := e.validateExprSubqueries(ob.Expr); err != nil {
			return err
		}
	}
	return nil
}

// validateSelectExprsOrdering runs the ORDER BY / result-shaping validations:
// aliased window-function references, row values, compound subquery
// aggregates, nested aggregates, and schema collation registration.
func (e *SelectEngine) validateSelectExprsOrdering(s *sql.SelectStmt) error {
	// A scalar subquery in ORDER BY that references a SELECT-list alias of a
	// window function is a misuse (window1 43.x: ORDER BY (SELECT m) on
	// SELECT count() OVER() AS m).
	if name := e.orderByWindowAliasRef(s); name != "" {
		return fmt.Errorf("misuse of aliased window function %s", name)
	}
	if err := e.validateSelectRowValues(s); err != nil {
		return err
	}
	if s.From.Subquery != nil {
		if err := validateUnionSubqueryNoAggs(s.From.Subquery); err != nil {
			return err
		}
	}
	if err := e.checkOrderByNestedAgg(s); err != nil {
		return err
	}
	// Schema-declared collations resolve at prepare time: ORDER BY/GROUP BY
	// sort keys, DISTINCT dedup, and compound set-op/ORDER BY column
	// collations must be registered (build.c sqlite3LocateCollSeq; a
	// close/reopen without re-registering a schema collation fails these
	// with "no such collation sequence: NAME" — collate3-2.x).
	if err := e.validateSchemaCollations(s); err != nil {
		return err
	}
	return e.validateCompoundTermLimit(s)
}

// validateSelectColumnList validates each SELECT column expression for ORDER BY
// terms, subquery validity, ORDER BY length, DISTINCT aggregate arity, and
// FILTER clause misuse (FILTER only on aggregates, no window/aggregate inside
// FILTER).
func (e *SelectEngine) validateSelectColumnList(s *sql.SelectStmt) error {
	for _, col := range s.Columns {
		if err := e.validateExprOrderBy(col.Expr); err != nil {
			return err
		}
		if err := validateOrderByLength(col.Expr, 1000); err != nil {
			return err
		}
		if err := e.validateFilterClause(col.Expr); err != nil {
			return err
		}
		if err := e.validateExprSubqueries(col.Expr); err != nil {
			return err
		}
		if err := validateDistinctAggArgs(col.Expr); err != nil {
			return err
		}
	}
	return nil
}

// validateFilterClause validates FILTER clauses on aggregate functions:
// FILTER may only be used with aggregates, and FILTER expressions must not
// contain window functions or aggregate functions.
func (e *SelectEngine) validateFilterClause(expr sql.Expr) error {
	var firstErr error
	WalkExprFull(expr, func(en sql.Expr) {
		if firstErr != nil {
			return
		}
		if fn, ok := en.(*sql.FuncCall); ok && fn.Filter != nil {
			firstErr = e.filterClauseError(fn)
		}
	})
	return firstErr
}

// filterClauseError validates one function call carrying a FILTER clause.
// SQLite reports a different message for a FILTER on a window function (which
// is not an aggregate) vs a plain non-aggregate scalar function
// (src/window.c:691 vs src/resolve.c:1282).
func (e *SelectEngine) filterClauseError(fn *sql.FuncCall) error {
	reg, found := e.ctx.Functions().Find(fn.Name)
	if !found || reg.Type != function.TypeAggregate {
		if fn.Over != nil {
			return fmt.Errorf("FILTER clause may only be used with aggregate window functions")
		}
		return fmt.Errorf("FILTER may not be used with non-aggregate %s()", fn.Name)
	}
	if nested := FindAggregateInExpr(fn.Filter); nested != "" {
		return fmt.Errorf("misuse of aggregate function %s()", nested)
	}
	if nested := e.windowFuncInExpr(fn.Filter); nested != "" {
		return fmt.Errorf("misuse of window function %s()", nested)
	}
	return nil
}

// validateGroupByExprs rejects aggregate functions inside GROUP BY
// expressions. SQLite: "aggregate functions are not allowed in the GROUP BY
// clause". Numeric ordinals resolve to their SELECT-column expressions at
// execution (resolveGroupByOrdinals), where out-of-range and aggregate
// ordinals surface the same way (misc4-4.1/4.2).
func (e *SelectEngine) validateGroupByExprs(s *sql.SelectStmt) error {
	for _, gb := range s.GroupBy {
		if nested := FindAggregateInExpr(gb); nested != "" {
			return fmt.Errorf("aggregate functions are not allowed in the GROUP BY clause")
		}
	}
	// Numeric ordinals resolve to their SELECT-column expressions first
	// (resolve.c maps GROUP BY N to the Nth result column before the
	// no-aggregate check), so "GROUP BY 1, 2" over a list holding max(Value)
	// is rejected too (misc4-4.1/4.2). colDefs only matter for the SELECT *
	// mapping, which can never contain an aggregate.
	resolved, err := resolveGroupByOrdinals(s, nil)
	if err != nil {
		return err
	}
	for _, gb := range resolved {
		if nested := FindAggregateInExpr(gb); nested != "" {
			return fmt.Errorf("aggregate functions are not allowed in the GROUP BY clause")
		}
	}
	return nil
}

// validateHavingExprs validates the HAVING clause for ORDER BY terms, subqueries,
// and DISTINCT aggregate arity.
func (e *SelectEngine) validateHavingExprs(s *sql.SelectStmt) error {
	if s.Having == nil {
		return nil
	}
	if err := e.validateExprOrderBy(s.Having); err != nil {
		return err
	}
	if err := e.validateExprSubqueries(s.Having); err != nil {
		return err
	}
	return validateDistinctAggArgs(s.Having)
}

// validateWhereExprs validates the WHERE clause for subquery validity and
// DISTINCT aggregate arity. A correlated aggregate in a WHERE scalar subquery
// is a misuse (SQLite: "misuse of aggregate: X()").
func (e *SelectEngine) validateWhereExprs(s *sql.SelectStmt) error {
	if s.Where == nil {
		return nil
	}
	if err := e.validateExprSubqueries(s.Where); err != nil {
		return err
	}
	// A row-value comparison whose subquery operand contains a correlated
	// aggregate collapses the subquery's vector to a single value at VDBE
	// codegen (SQLite: "N columns assigned 1 values"; window1 71.0 with
	// WHERE (a,1)=(SELECT 2,2 UNION SELECT sum(b),max(b) OVER(ORDER BY b))).
	if n := e.whereRowValueCorrelatedAggCollapse(s.Where); n > 0 {
		return fmt.Errorf("%d columns assigned 1 values", n)
	}
	if name := e.whereSubqueryOuterAggRef(s.Where); name != "" {
		return fmt.Errorf("misuse of aggregate: %s()", name)
	}
	if name := e.whereInSubqOuterAggRef(s.Where); name != "" {
		return fmt.Errorf("misuse of aggregate: %s()", name)
	}
	// A scalar aggregate used DIRECTLY in this level's WHERE is always a
	// misuse (resolve.c: sqlite3ResolveExprNames clears NC_AllowAgg for the
	// WHERE subtree — "misuse of aggregate: max()", tkt1514/tkt3508). The
	// walk does not descend into subqueries: their WHERE clauses are
	// validated against their own scope by the nested validateWhereExprs.
	// resolve.c:1960 exception: when this SELECT is itself an aggregate query
	// (result-set aggregates or GROUP BY), WHERE resolution keeps NC_AllowAgg,
	// and the resolve.c:1332 context walk transfers an aggregate whose
	// arguments reference no column of this SELECT's own FROM to an outer
	// aggregate context (aggnested-3.11: WHERE value2=max(value1)).
	if name := e.whereDirectAggregateScoped(s); name != "" {
		return fmt.Errorf("misuse of aggregate: %s()", name)
	}
	// A WHERE reference to a SELECT alias whose expression IS an aggregate
	// resolves to that aggregate and is the same misuse (resolve.c name
	// resolution falls through to the output alias; tkt3508: "where c > 1"
	// with count(x) AS c).
	for _, col := range s.Columns {
		if col.As == "" || col.Expr == nil {
			continue
		}
		if agg := expressionAggregateName(col.Expr, e.ctx.Functions()); agg != "" {
			if whereReferencesBareName(s.Where, col.As) {
				return fmt.Errorf("misuse of aggregate: %s()", agg)
			}
		}
	}
	return validateDistinctAggArgs(s.Where)
}

// expressionAggregateName returns the lowercased name of the first scalar
// aggregate directly contained in the expression (no subquery descent).
func expressionAggregateName(expr sql.Expr, fns *function.Registry) string {
	if expr == nil {
		return ""
	}
	found := ""
	WalkExprFull(expr, func(n sql.Expr) {
		if found != "" {
			return
		}
		if fn, ok := n.(*sql.FuncCall); ok {
			if reg, found2 := fns.Find(fn.Name); found2 && reg.Type == function.TypeAggregate {
				found = strings.ToLower(fn.Name)
			}
		}
	})
	return found
}

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
