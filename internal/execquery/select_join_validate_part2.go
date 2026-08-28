// Package exec — JOIN/ON/USING validation functions extracted from select.go
// (file-level SRP). All functions remain methods on *SelectEngine in package
// internal/exec.
package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// addSubqueryFromCols adds the column names of a subquery's FROM tables (and
// nested derived tables) to the given set, so ON clauses referencing a derived
// table's inner columns (ON c=b with (SELECT * FROM t2, t3)) validate.

func validateOnColumnRefs(on sql.Expr, names map[string]bool) error {
	if on == nil {
		return nil
	}
	if hasUnresolvedOnColumn(on, names) {
		return fmt.Errorf("ON clause references tables to its right")
	}
	return nil
}

// hasUnresolvedOnColumn reports whether the ON expression contains an
// unqualified column reference that does not resolve among the given names.
func hasUnresolvedOnColumn(on sql.Expr, names map[string]bool) bool {
	var bad bool
	walkJoinOnExpr(on, func(e2 sql.Expr) {
		cr, ok := e2.(*sql.ColumnRef)
		if !ok || cr.Table != "" {
			return
		}
		if isPseudoColumn(cr.Name) {
			return
		}
		if !names[cr.Name] {
			bad = true
		}
	})
	return bad
}

// isPseudoColumn reports whether a bare identifier is a wildcard, rowid alias,
// or boolean literal (all represented as unqualified ColumnRefs by the parser).
func isPseudoColumn(n string) bool {
	if n == "*" || n == "rowid" || n == "oid" || n == "_rowid_" {
		return true
	}
	return strings.EqualFold(n, "TRUE") || strings.EqualFold(n, "FALSE")
}

// walkJoinOnExpr visits the direct references of a join ON expression,
// descending into function arguments but not into subqueries (which have
// their own table scope).
func walkJoinOnExpr(expr sql.Expr, fn func(sql.Expr)) {
	if expr == nil {
		return
	}
	fn(expr)
	if child, ok := joinOnExprSingleChild(expr); ok {
		walkJoinOnExpr(child, fn)
		return
	}
	walkJoinOnCompound(expr, fn)
}

// joinOnExprSingleChild returns the single sub-expression of expression types
// that have exactly one operand (or false if the type is not one of those).
func joinOnExprSingleChild(expr sql.Expr) (sql.Expr, bool) {
	switch e := expr.(type) {
	case *sql.ParenExpr:
		return e.Expr, true
	case *sql.UnaryOp:
		return e.Operand, true
	case *sql.CastExpr:
		return e.Operand, true
	case *sql.IsNull:
		return e.Operand, true
	case *sql.IsNotNull:
		return e.Operand, true
	case *sql.IsTrue:
		return e.Operand, true
	case *sql.IsFalse:
		return e.Operand, true
	}
	return nil, false
}

// walkJoinOnCompound dispatches multi-child expression types, recursing into
// each sub-expression. Subquery and ExistsExpr nodes are visited but not
// descended into (their inner references are validated separately).
func walkJoinOnCompound(expr sql.Expr, fn func(sql.Expr)) {
	switch e := expr.(type) {
	case *sql.BinaryOp:
		walkJoinOnExpr(e.Left, fn)
		walkJoinOnExpr(e.Right, fn)
	case *sql.Between:
		walkJoinOnExpr(e.Operand, fn)
		walkJoinOnExpr(e.Low, fn)
		walkJoinOnExpr(e.High, fn)
	case *sql.IsDistinctFrom:
		walkJoinOnExpr(e.Left, fn)
		walkJoinOnExpr(e.Right, fn)
	case *sql.IsNotDistinctFrom:
		walkJoinOnExpr(e.Left, fn)
		walkJoinOnExpr(e.Right, fn)
	case *sql.InList:
		walkJoinOnExpr(e.Operand, fn)
		walkJoinOnExprSlice(e.List, fn)
	case *sql.FuncCall:
		walkJoinOnExprSlice(e.Args, fn)
		for _, ob := range e.OrderBy {
			walkJoinOnExpr(ob.Expr, fn)
		}
	case *sql.CaseExpr:
		walkJoinOnCaseExpr(e, fn)
	case *sql.RowValue:
		walkJoinOnExprSlice(e.Values, fn)
	}
}

// walkJoinOnExprSlice walks a slice of expressions.
func walkJoinOnExprSlice(exprs []sql.Expr, fn func(sql.Expr)) {
	for _, e := range exprs {
		walkJoinOnExpr(e, fn)
	}
}

// walkJoinOnCaseExpr walks a CASE expression: its optional operand, each
// WHEN/THEN pair, and the optional ELSE.
func walkJoinOnCaseExpr(e *sql.CaseExpr, fn func(sql.Expr)) {
	if e.Operand != nil {
		walkJoinOnExpr(e.Operand, fn)
	}
	for _, w := range e.Whens {
		walkJoinOnExpr(w.When, fn)
		walkJoinOnExpr(w.Then, fn)
	}
	if e.Else != nil {
		walkJoinOnExpr(e.Else, fn)
	}
}
