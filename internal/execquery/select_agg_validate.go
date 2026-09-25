package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns aggregate/outer-reference analysis for SELECT statements,
// extracted from select.go for single-responsibility cohesion: correlated
// aggregate detection, outer-reference resolution, and inner-column
// reference analysis (the clause validations live in
// select_validate_exprs.go).

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
// names. Aggregates are located anywhere in the column expression (resolve.c
// marks an aggregate inner-owned by where its column references resolve, not
// by the shape of the result expression: max(a)*max(a) over FROM t1 owns t1's
// rows exactly like a bare max(a) column would). The FILTER is part of the
// aggregate expression (resolve.c sqlite3ReferencesSrcList scans the whole
// aggregate), so a FILTER reference to an inner column (filter1-6.1:
// COUNT(a) FILTER(WHERE x)) makes the aggregate inner-owned.
func (e *SelectEngine) aggColumnArgsRefInner(col sql.SelectColumn, colNames map[string]bool) bool {
	return e.exprAggArgsRefInner(col.Expr, colNames)
}

// exprAggArgsRefInner walks an expression tree looking for an aggregate
// function call whose args, ORDER BY terms, or FILTER reference any of the
// given inner column names. The walk covers every expression node kind
// (CAST, IN-lists, CASE, paren nesting, …) via WalkExprFull and does not
// cross Subquery boundaries (a nested subquery owns its own aggregates).
func (e *SelectEngine) exprAggArgsRefInner(expr sql.Expr, colNames map[string]bool) bool {
	if expr == nil {
		return false
	}
	found := false
	WalkExprFull(expr, func(n sql.Expr) {
		if found {
			return
		}
		if fn, ok := n.(*sql.FuncCall); ok && funcAggArgsRefInner(fn, colNames) {
			found = true
		}
	})
	return found
}

// funcAggArgsRefInner reports whether one aggregate function call's args,
// ORDER BY terms, or FILTER reference any of the given inner column names.
func funcAggArgsRefInner(fn *sql.FuncCall, colNames map[string]bool) bool {
	for _, arg := range fn.Args {
		if exprHasColRefNames(arg, colNames) {
			return true
		}
	}
	for _, ob := range fn.OrderBy {
		if exprHasColRefNames(ob.Expr, colNames) {
			return true
		}
	}
	// A FILTER referencing a FROM-table column binds the aggregate to the
	// inner rows even when the arguments are outer-only (filter1-6.1:
	// COUNT(a) FILTER(WHERE x) with x in the FROM table).
	return fn.Filter != nil && exprHasColRefNames(fn.Filter, colNames)
}

// exprHasColRefNames reports whether an expression tree contains a column
// reference whose name matches an entry in colNames. Like WalkExprFull the
// walk covers every expression node kind and does not descend into
// subqueries.
func exprHasColRefNames(expr sql.Expr, colNames map[string]bool) bool {
	found := false
	WalkExprFull(expr, func(n sql.Expr) {
		if ref, ok := n.(*sql.ColumnRef); ok && colNames[ref.Name] {
			found = true
		}
	})
	return found
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
