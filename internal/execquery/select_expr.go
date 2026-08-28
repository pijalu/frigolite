// Package exec implements query execution.
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns compile-time expression analysis helpers used during SELECT
// planning: collation resolution, aggregate/subquery detection, structural
// equality, min/max source-row selection, rowid-reference detection, and the
// generic expression-tree walker. Extracted from select.go for file-level SRP
// (CX-07). Future home: execquery/expr.go.

// lastMinMaxInExpr walks an expression tree depth-first, left-to-right, and
// returns the last single-argument MIN/MAX aggregate function call found.
// FuncCall nodes use lastMinMaxInFuncCall (which scans all args of a
// non-aggregate call and keeps the last match); all other nodes iterate their
// children in traversal order, returning the first match.
func lastMinMaxInExpr(expr sql.Expr, funcs *function.Registry) *minMaxAggregate {
	if fc, ok := expr.(*sql.FuncCall); ok {
		return lastMinMaxInFuncCall(fc, funcs)
	}
	for _, c := range lastMinMaxChildren(expr) {
		if mm := lastMinMaxInExpr(c, funcs); mm != nil {
			return mm
		}
	}
	return nil
}

// lastMinMaxInFuncCall handles the FuncCall case of lastMinMaxInExpr. A
// single-argument MIN/MAX aggregate returns the last min/max nested in its
// argument (earlier in traversal order), else itself; any other call scans all
// arguments and keeps the last min/max found.
func lastMinMaxInFuncCall(fc *sql.FuncCall, funcs *function.Registry) *minMaxAggregate {
	if mm, isAgg := minMaxAggregateMatch(fc, funcs); isAgg {
		// Nested occurrences inside the argument come earlier in traversal
		// order, so check them first.
		if inner := lastMinMaxInExpr(fc.Args[0], funcs); inner != nil {
			return inner
		}
		return mm
	}
	// Not a single-arg min/max aggregate: scan the arguments, keep the last.
	var last *minMaxAggregate
	for _, arg := range fc.Args {
		if mm := lastMinMaxInExpr(arg, funcs); mm != nil {
			last = mm
		}
	}
	return last
}

// minMaxAggregateMatch reports whether fc is a single-argument MIN/MAX that the
// registry classifies as an aggregate, returning the resolved aggregate.
func minMaxAggregateMatch(fc *sql.FuncCall, funcs *function.Registry) (*minMaxAggregate, bool) {
	if len(fc.Args) != 1 || !isMinMaxName(fc.Name) {
		return nil, false
	}
	fn, ok := funcs.Find(fc.Name)
	if !ok || fn.Type != function.TypeAggregate {
		return nil, false
	}
	return &minMaxAggregate{name: strings.ToUpper(fc.Name), arg: fc.Args[0]}, true
}

// isMinMaxName reports whether name is MIN or MAX (case-insensitive).
func isMinMaxName(name string) bool {
	return strings.EqualFold(name, "MIN") || strings.EqualFold(name, "MAX")
}

// lastMinMaxChildren returns the child expressions that lastMinMaxInExpr
// descends into for non-FuncCall nodes. It reuses exprChildren but adds
// RowValue (scanned for min/max) and excludes RaiseExpr (not descended into by
// the original traversal), preserving historical behavior.
func lastMinMaxChildren(e sql.Expr) []sql.Expr {
	if rv, ok := e.(*sql.RowValue); ok {
		return rv.Values
	}
	if _, ok := e.(*sql.RaiseExpr); ok {
		return nil
	}
	return exprChildren(e)
}

// isSubqueryNode reports whether expr is a scalar Subquery or EXISTS node.
func isSubqueryNode(expr sql.Expr) bool {
	switch expr.(type) {
	case *sql.Subquery, *sql.ExistsExpr:
		return true
	}
	return false
}

// exprContainsSubquery checks if an expression tree contains a Subquery node.
func exprContainsSubquery(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	if isSubqueryNode(expr) {
		return true
	}
	for _, c := range exprSubqueryChildren(expr) {
		if exprContainsSubquery(c) {
			return true
		}
	}
	return false
}

// exprSubqueryChildren returns the sub-expressions that exprContainsSubquery
// descends into, preserving its historical traversal coverage (including
// FuncCall ORDER BY terms and CAST operands).
func exprSubqueryChildren(e sql.Expr) []sql.Expr {
	switch v := e.(type) {
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.FuncCall:
		kids := append([]sql.Expr{}, v.Args...)
		for _, ob := range v.OrderBy {
			kids = append(kids, ob.Expr)
		}
		return kids
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		return append([]sql.Expr{v.Operand}, v.List...)
	case *sql.CaseExpr:
		kids := []sql.Expr{v.Operand}
		for _, w := range v.Whens {
			kids = append(kids, w.When, w.Then)
		}
		return append(kids, v.Else)
	case *sql.CastExpr:
		return []sql.Expr{v.Operand}
	}
	return nil
}

// exprStructurallyEqual reports whether two expressions have identical
// structure (operator, operand kinds, column names, literals). Pointer
// fields (e.g. subquery ASTs) are compared by structural equality only for
// the leaf kinds used in GROUP BY columns.
func exprStructurallyEqual(a, b sql.Expr) bool {
	if a == nil || b == nil {
		return a == b
	}
	if eq, handled := leafExprEqual(a, b); handled {
		return eq
	}
	switch x := a.(type) {
	case *sql.BinaryOp:
		return binaryOpStructEqual(x, b)
	case *sql.UnaryOp:
		return unaryOpStructEqual(x, b)
	case *sql.FuncCall:
		return funcCallStructEqual(x, b)
	}
	return false
}

// leafExprEqual compares leaf expressions (column references and literals).
// It returns (result, true) when a is a recognized leaf kind, else
// (false, false) so the caller can try compound kinds.
func leafExprEqual(a, b sql.Expr) (bool, bool) {
	switch x := a.(type) {
	case *sql.ColumnRef:
		y, ok := b.(*sql.ColumnRef)
		return ok && x.Name == y.Name && x.Table == y.Table, true
	case *sql.NumericLit:
		y, ok := b.(*sql.NumericLit)
		return ok && x.Value == y.Value, true
	case *sql.StringLit:
		y, ok := b.(*sql.StringLit)
		return ok && x.Value == y.Value, true
	case *sql.BlobLit:
		y, ok := b.(*sql.BlobLit)
		return ok && string(x.Value) == string(y.Value), true
	}
	return false, false
}

// binaryOpStructEqual compares two BinaryOp expressions for structural
// equality (same operator and recursively-equal operands).
func binaryOpStructEqual(x *sql.BinaryOp, b sql.Expr) bool {
	y, ok := b.(*sql.BinaryOp)
	if !ok || x.Operator != y.Operator {
		return false
	}
	return exprStructurallyEqual(x.Left, y.Left) && exprStructurallyEqual(x.Right, y.Right)
}

// unaryOpStructEqual compares two UnaryOp expressions for structural equality.
func unaryOpStructEqual(x *sql.UnaryOp, b sql.Expr) bool {
	y, ok := b.(*sql.UnaryOp)
	if !ok || x.Operator != y.Operator {
		return false
	}
	return exprStructurallyEqual(x.Operand, y.Operand)
}

// funcCallStructEqual compares two FuncCall expressions for structural
// equality (same name ignoring case, same arity, recursively-equal arguments).
func funcCallStructEqual(x *sql.FuncCall, b sql.Expr) bool {
	y, ok := b.(*sql.FuncCall)
	if !ok || !strings.EqualFold(x.Name, y.Name) || len(x.Args) != len(y.Args) {
		return false
	}
	for i := range x.Args {
		if !exprStructurallyEqual(x.Args[i], y.Args[i]) {
			return false
		}
	}
	return true
}

// findAggregateInExpr walks an expression looking for aggregate function calls.
// It returns the name of the first aggregate found (depth-first), or "".
func FindAggregateInExpr(expr sql.Expr) string {
	if fc, ok := expr.(*sql.FuncCall); ok {
		if name := aggregateFuncName(fc); name != "" {
			return name
		}
	}
	for _, c := range exprAggregateChildren(expr) {
		if nested := FindAggregateInExpr(c); nested != "" {
			return nested
		}
	}
	return ""
}

// aggregateFuncName returns the aggregate name when fc is a recognized
// aggregate call, else "". MIN/MAX are aggregates only in single-argument form
// (with two+ args they are scalar functions, so SELECT min(x,5) must not
// collapse to one row). A call with an OVER clause is a window function, not a
// plain aggregate, so it is not reported here.
func aggregateFuncName(fc *sql.FuncCall) string {
	if fc.Over != nil {
		return ""
	}
	switch strings.ToUpper(fc.Name) {
	case "COUNT", "SUM", "AVG", "TOTAL", "GROUP_CONCAT", "STRING_AGG":
		return fc.Name
	case "MIN", "MAX":
		if len(fc.Args) == 1 {
			return fc.Name
		}
	}
	return ""
}

// exprAggregateChildren returns the sub-expressions findAggregateInExpr
// descends into (FuncCall args, BinaryOp operands, UnaryOp operand, CASE
// branches), preserving its historical traversal coverage.
func exprAggregateChildren(e sql.Expr) []sql.Expr {
	switch v := e.(type) {
	case *sql.FuncCall:
		return append([]sql.Expr{}, v.Args...)
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.CaseExpr:
		kids := []sql.Expr{v.Operand}
		for _, w := range v.Whens {
			kids = append(kids, w.When, w.Then)
		}
		return append(kids, v.Else)
	}
	return nil
}

// exprHasSubquery checks if an expression tree contains a Subquery or ExistsExpr.
func exprHasSubquery(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	if isSubqueryNode(expr) {
		return true
	}
	for _, c := range exprHasSubqueryChildren(expr) {
		if exprHasSubquery(c) {
			return true
		}
	}
	return false
}

// exprHasSubqueryChildren returns the sub-expressions that exprHasSubquery
// descends into, preserving its historical traversal coverage (ParenExpr but
// not CastExpr/CaseExpr.Operand/FuncCall.OrderBy).
func exprHasSubqueryChildren(e sql.Expr) []sql.Expr {
	switch v := e.(type) {
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.ParenExpr:
		return []sql.Expr{v.Expr}
	case *sql.InList:
		return append([]sql.Expr{v.Operand}, v.List...)
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.FuncCall:
		return append([]sql.Expr{}, v.Args...)
	case *sql.CaseExpr:
		kids := []sql.Expr{}
		for _, w := range v.Whens {
			kids = append(kids, w.When, w.Then)
		}
		return append(kids, v.Else)
	}
	return nil
}

// checkWhereCollations walks a WHERE expression and raises "no such collation
// sequence: X" when a comparison operand is a column reference whose declared
// collation (from the scanned table) is unknown. Mirrors SQLite's compile-time
// collation resolution for comparison operators.
func (e *SelectEngine) checkWhereCollations(where sql.Expr, colDefs []sql.ColumnDef, from sql.TableRef) error {
	refName := from.Name
	if from.As != "" {
		refName = from.As
	}
	colByName := collationMap(colDefs)
	var checkErr error
	WalkExprFull(where, func(e2 sql.Expr) {
		if checkErr != nil {
			return
		}
		bop, ok := e2.(*sql.BinaryOp)
		if !ok || !isComparisonOp(bop.Operator) {
			return
		}
		checkErr = e.checkBopCollationSides(bop, colByName, refName, from.Name)
	})
	return checkErr
}

// collationMap builds a lowercased column-name → collation map for the columns
// of colDefs that declare a collation.
func collationMap(colDefs []sql.ColumnDef) map[string]string {
	m := make(map[string]string, len(colDefs))
	for _, c := range colDefs {
		if c.Collate != "" {
			m[strings.ToLower(c.Name)] = c.Collate
		}
	}
	return m
}

// isComparisonOp reports whether op is one of the comparison operators whose
// operands have their collation resolved at compile time.
func isComparisonOp(op string) bool {
	switch strings.ToUpper(op) {
	case "=", "<>", "!=", "<", ">", "<=", ">=":
		return true
	}
	return false
}

// checkBopCollationSides checks both operands of a comparison for column
// references whose declared collation is unknown, returning the first error.
func (e *SelectEngine) checkBopCollationSides(bop *sql.BinaryOp, colByName map[string]string, refName, tableName string) error {
	for _, side := range []sql.Expr{bop.Left, bop.Right} {
		cr, ok := side.(*sql.ColumnRef)
		if !ok || !columnRefMatchesTable(cr, refName, tableName) {
			continue
		}
		if coll, ok := colByName[strings.ToLower(cr.Name)]; ok {
			if err := e.ctx.CheckCollationString(coll); err != nil {
				return err
			}
		}
	}
	return nil
}

// columnRefMatchesTable reports whether a column reference qualifies the scanned
// table (or its alias) or is unqualified (resolves by name in single-table scope).
func columnRefMatchesTable(cr *sql.ColumnRef, refName, tableName string) bool {
	if cr.Table == "" {
		return true
	}
	return strings.EqualFold(cr.Table, refName) || strings.EqualFold(cr.Table, tableName)
}

func (e *SelectEngine) findRowIDRef(s *sql.SelectStmt, tableName, alias string, hasJoins bool) string {
	for _, expr := range selectRowIDExprClauses(s) {
		if ref := e.firstRowIDRefIn(expr, tableName, alias, hasJoins); ref != "" {
			return ref
		}
	}
	return ""
}

// firstRowIDRefIn returns the first rowid/_rowid_/oid column reference in expr
// that resolves to the named table, or "". A qualified reference to another
// table, or an unqualified reference when the query has joins, does not match.
func (e *SelectEngine) firstRowIDRefIn(expr sql.Expr, tableName, alias string, hasJoins bool) string {
	var found string
	WalkExprFull(expr, func(e2 sql.Expr) {
		if found != "" {
			return
		}
		cr, ok := e2.(*sql.ColumnRef)
		if !ok || !isRowIDName(cr.Name) {
			return
		}
		// Qualified reference to another table: that table's rowid.
		if cr.Table != "" && !rowIDRefMatchesTable(cr.Table, tableName, alias) {
			return
		}
		// Unqualified reference with joins may resolve to another table.
		if cr.Table == "" && hasJoins {
			return
		}
		found = cr.Name
	})
	return found
}

// rowIDRefMatchesTable reports whether a table qualifier matches the scanned
// table name or its alias (case-insensitive).
func rowIDRefMatchesTable(table, tableName, alias string) bool {
	return strings.EqualFold(table, tableName) || strings.EqualFold(table, alias)
}

// selectRowIDExprClauses gathers the expression positions of s whose rowid
// references are checked by findRowIDRef: SELECT columns, WHERE, ORDER BY,
// GROUP BY, and HAVING, in traversal order.
func selectRowIDExprClauses(s *sql.SelectStmt) []sql.Expr {
	var exprs []sql.Expr
	for _, col := range s.Columns {
		exprs = append(exprs, col.Expr)
	}
	exprs = append(exprs, s.Where)
	for _, ob := range s.OrderBy {
		exprs = append(exprs, ob.Expr)
	}
	exprs = append(exprs, s.GroupBy...)
	if s.Having != nil {
		exprs = append(exprs, s.Having)
	}
	return exprs
}

// walkExprFull visits every node in an expression tree, descending into all
// expression node types. The traversal is pre-order: fn is invoked on a node
// before its children.
func WalkExprFull(expr sql.Expr, fn func(sql.Expr)) {
	if expr == nil {
		return
	}
	fn(expr)
	for _, child := range exprChildren(expr) {
		WalkExprFull(child, fn)
	}
}

// exprChildren returns the direct child expressions of a node, in traversal
// order. Returns nil for leaf nodes (literals, column refs, subqueries) and
// for unhandled kinds. Compound/multi-child nodes are handled here; unary
// nodes (single operand) fall through to exprUnaryChildren.
func exprChildren(e sql.Expr) []sql.Expr {
	switch v := e.(type) {
	case *sql.ParenExpr:
		return []sql.Expr{v.Expr}
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.IsDistinctFrom:
		return []sql.Expr{v.Left, v.Right}
	case *sql.IsNotDistinctFrom:
		return []sql.Expr{v.Left, v.Right}
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.FuncCall:
		return v.Args
	case *sql.InList:
		return append([]sql.Expr{v.Operand}, v.List...)
	case *sql.RowValue:
		return v.Values
	case *sql.RaiseExpr:
		return []sql.Expr{v.Message}
	}
	return exprUnaryChildren(e)
}

// exprUnaryChildren returns the child expressions of the unary/operand nodes
// (those exposing a single Operand field) and the variadic CASE expression
// (operand + WHEN/THEN pairs + ELSE).
func exprUnaryChildren(e sql.Expr) []sql.Expr {
	switch v := e.(type) {
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.CastExpr:
		return []sql.Expr{v.Operand}
	case *sql.IsNull:
		return []sql.Expr{v.Operand}
	case *sql.IsNotNull:
		return []sql.Expr{v.Operand}
	case *sql.IsTrue:
		return []sql.Expr{v.Operand}
	case *sql.IsFalse:
		return []sql.Expr{v.Operand}
	case *sql.CaseExpr:
		kids := []sql.Expr{v.Operand}
		for _, w := range v.Whens {
			kids = append(kids, w.When, w.Then)
		}
		return append(kids, v.Else)
	}
	return nil
}
