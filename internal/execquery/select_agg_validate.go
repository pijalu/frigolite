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
	switch v := expr.(type) {
	case *sql.FuncCall:
		// A window function (OVER clause) is not a correlated aggregate: it
		// does not collapse the query to one row. Its aggregate name (e.g.
		// min(a) OVER ()) must not trigger the correlated-aggregate path.
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
	case *sql.BinaryOp:
		return e.exprAggWithColumnRef(v.Left) || e.exprAggWithColumnRef(v.Right)
	case *sql.UnaryOp:
		return e.exprAggWithColumnRef(v.Operand)
	case *sql.ParenExpr:
		return e.exprAggWithColumnRef(v.Expr)
	case *sql.CastExpr:
		return e.exprAggWithColumnRef(v.Operand)
	case *sql.CaseExpr:
		if e.exprAggWithColumnRef(v.Operand) {
			return true
		}
		for _, w := range v.Whens {
			if e.exprAggWithColumnRef(w.When) || e.exprAggWithColumnRef(w.Then) {
				return true
			}
		}
		return e.exprAggWithColumnRef(v.Else)
	case *sql.Between:
		return e.exprAggWithColumnRef(v.Operand) || e.exprAggWithColumnRef(v.Low) || e.exprAggWithColumnRef(v.High)
	case *sql.InList:
		if e.exprAggWithColumnRef(v.Operand) {
			return true
		}
		for _, item := range v.List {
			if e.exprAggWithColumnRef(item) {
				return true
			}
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
		// A window function (OVER clause) is not a correlated aggregate: it
		// does not collapse the query, so it must not trigger the outer-row
		// aggregate path.
		if fn.Over != nil {
			return false
		}
		reg, found := e.ctx.Functions().Find(fn.Name)
		if found && reg.Type == function.TypeAggregate {
			refsOuter := false
			refsInner := false
			for _, a := range fn.Args {
				if e.exprRefsOuterCol(a, inner, innerTables) {
					refsOuter = true
				}
				if exprHasColRefInMap(a, inner) {
					refsInner = true
				}
			}
			if refsOuter && !refsInner {
				return true
			}
		}
	}
	for _, child := range aggValidateChildExprs(expr) {
		if e.aggExprRefsOnlyOuter(child, inner, innerTables) {
			return true
		}
	}
	return false
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
	switch v := expr.(type) {
	case *sql.FuncCall:
		if v.Over != nil {
			return false
		}
		reg, found := e.ctx.Functions().Find(v.Name)
		if found && reg.Type == function.TypeAggregate {
			return true
		}
		for _, arg := range v.Args {
			if e.colHasNonWindowAggregate(arg) {
				return true
			}
		}
		for _, ob := range v.OrderBy {
			if e.colHasNonWindowAggregate(ob.Expr) {
				return true
			}
		}
		return false
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		left, right := BinaryExprOperands(v)
		return e.colHasNonWindowAggregate(left) || e.colHasNonWindowAggregate(right)
	case *sql.UnaryOp, *sql.ParenExpr, *sql.CastExpr, *sql.IsNull, *sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		return e.colHasNonWindowAggregate(singleExprOperand(v))
	case *sql.Between:
		return e.colHasNonWindowAggregate(v.Operand) || e.colHasNonWindowAggregate(v.Low) || e.colHasNonWindowAggregate(v.High)
	case *sql.InList:
		if e.colHasNonWindowAggregate(v.Operand) {
			return true
		}
		for _, item := range v.List {
			if e.colHasNonWindowAggregate(item) {
				return true
			}
		}
		return false
	case *sql.CaseExpr:
		if e.colHasNonWindowAggregate(v.Operand) {
			return true
		}
		for _, w := range v.Whens {
			if e.colHasNonWindowAggregate(w.When) || e.colHasNonWindowAggregate(w.Then) {
				return true
			}
		}
		if v.Else != nil {
			return e.colHasNonWindowAggregate(v.Else)
		}
		return false
	case *sql.RowValue:
		for _, item := range v.Values {
			if e.colHasNonWindowAggregate(item) {
				return true
			}
		}
		return false
	default:
		return false
	}
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

// aggColumnArgsRefInner checks if a SELECT column's aggregate function args or
// ORDER BY terms reference any of the given inner column names.
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
	found := ""
	checkSub := func(sub *sql.Subquery) {
		if sub == nil || sub.Select == nil || found != "" {
			return
		}
		for _, col := range sub.Select.Columns {
			WalkExprFull(col.Expr, func(en sql.Expr) {
				if found != "" {
					return
				}
				if ref, ok := en.(*sql.ColumnRef); ok && ref.Table == "" {
					if winAliases[strings.ToLower(ref.Name)] {
						found = ref.Name
					}
				}
			})
			if found != "" {
				return
			}
		}
	}
	for _, ob := range s.OrderBy {
		WalkExprFull(ob.Expr, func(en sql.Expr) {
			if found != "" {
				return
			}
			if sub, ok := en.(*sql.Subquery); ok {
				checkSub(sub)
			}
		})
		if found != "" {
			return found
		}
	}
	return found
}

// validateSelectExprs validates aggregate misuse, DISTINCT aggregate arity,
// subquery validity, ORDER BY length limits, row-value misuse, and UNION
// subquery aggregates across a SELECT's clauses.
func (e *SelectEngine) validateSelectExprs(s *sql.SelectStmt) error {
	if err := e.validateMultipleFTSMatch(s); err != nil {
		return err
	}
	if err := e.checkOrderByAggMisuse(s); err != nil {
		return err
	}
	if err := e.validateSelectColumnList(s); err != nil {
		return err
	}
	if err := e.validateWindowFunctions(s); err != nil {
		return err
	}
	if err := e.validateGroupByExprs(s); err != nil {
		return err
	}
	if err := e.validateHavingExprs(s); err != nil {
		return err
	}
	if err := e.validateWhereExprs(s); err != nil {
		return err
	}
	for _, ob := range s.OrderBy {
		if err := validateDistinctAggArgs(ob.Expr); err != nil {
			return err
		}
		// Validate scalar subqueries inside ORDER BY terms (table
		// resolution etc.) so their errors surface here — SQLite resolves
		// ORDER BY term names/subqueries even when the term does not match
		// a result column (window1 67.1: a nested (SELECT 1 FROM v1) inside
		// a window's ORDER BY must raise "no such table: v1").
		if err := e.validateExprSubqueries(ob.Expr); err != nil {
			return err
		}
	}
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
	return e.checkOrderByNestedAgg(s)
}

// checkOrderByAggMisuse rejects aggregate functions in ORDER BY when the SELECT
// is not an aggregate query (no GROUP BY, no aggregate in SELECT list).
// Compound queries skip this: a trailing ORDER BY on a compound member is the
// compound-level ORDER BY, where aggregates are permitted.
func (e *SelectEngine) checkOrderByAggMisuse(s *sql.SelectStmt) error {
	if len(s.OrderBy) == 0 || s.GroupBy != nil || e.inCompoundMember || s.Union != nil {
		return nil
	}
	isAgg := e.hasAggregates(s.Columns)
	for _, ob := range s.OrderBy {
		if e.exprHasAggregate(ob.Expr) && !isAgg {
			return fmt.Errorf("misuse of aggregate: %s()", e.aggregateName(ob.Expr))
		}
		// An aggregate inside a scalar subquery in ORDER BY that references
		// outer columns is a misuse (window1 61.4.3: ORDER BY (SELECT sum(a)
		// FROM t2) where a is t1's column). A subquery aggregate over its own
		// FROM is fine (61.4.4).
		if !isAgg {
			if name := e.orderBySubqueryOuterAgg(ob.Expr); name != "" {
				return fmt.Errorf("misuse of aggregate: %s()", name)
			}
		}
	}
	return nil
}

// orderBySubqueryOuterAgg returns the name of the first correlated aggregate
// (aggregate referencing columns outside the subquery's own FROM) inside a
// scalar subquery in an ORDER BY expression, or "".
func (e *SelectEngine) orderBySubqueryOuterAgg(expr sql.Expr) string {
	found := ""
	WalkExprFull(expr, func(en sql.Expr) {
		if found != "" {
			return
		}
		if sub, ok := en.(*sql.Subquery); ok && sub.Select != nil {
			if n := e.subqueryOuterAggRef(sub.Select); n != "" {
				found = n
			}
		}
	})
	return found
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
		fn, ok := en.(*sql.FuncCall)
		if !ok || fn.Filter == nil {
			return
		}
		reg, found := e.ctx.Functions().Find(fn.Name)
		if !found || reg.Type != function.TypeAggregate {
			// SQLite reports a different message for a FILTER on a window
			// function (which is not an aggregate) vs a plain non-aggregate
			// scalar function (src/window.c:691 vs src/resolve.c:1282).
			if fn.Over != nil {
				firstErr = fmt.Errorf("FILTER clause may only be used with aggregate window functions")
			} else {
				firstErr = fmt.Errorf("FILTER may not be used with non-aggregate %s()", fn.Name)
			}
			return
		}
		if nested := FindAggregateInExpr(fn.Filter); nested != "" {
			firstErr = fmt.Errorf("misuse of aggregate function %s()", nested)
			return
		}
		if nested := e.windowFuncInExpr(fn.Filter); nested != "" {
			firstErr = fmt.Errorf("misuse of window function %s()", nested)
			return
		}
	})
	return firstErr
}

// validateGroupByExprs rejects aggregate functions inside GROUP BY
// expressions. SQLite: "aggregate functions are not allowed in the GROUP BY
// clause".
func (e *SelectEngine) validateGroupByExprs(s *sql.SelectStmt) error {
	for _, gb := range s.GroupBy {
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
	return validateDistinctAggArgs(s.Where)
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
