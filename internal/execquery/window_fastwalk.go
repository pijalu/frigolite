package execquery

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns the allocation-free window-function existence check: the
// several-per-statement "does this statement use window functions" scans
// (needMaps decision, post-scan dispatch, per-group output building) walk the
// expression tree directly instead of building the child slices the real
// window collector materializes per node.

// exprHasWindowFunc reports whether an expression tree contains a window
// function call. It mirrors collectWindowChildren's traversal without
// building child slices.
func (e *SelectEngine) exprHasWindowFunc(expr sql.Expr) bool {
	return exprHasWindowFuncFast(expr)
}

// exprHasWindowFuncFast reports whether expr contains a FuncCall node with an
// OVER clause, descending exactly the node families collectWindowChildren
// visits (it does NOT descend into subquery bodies — a window function inside
// a scalar subquery belongs to that subquery's window pass, not the
// enclosing statement's).
func exprHasWindowFuncFast(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return funcCallHasWindow(v)
	case *sql.BinaryOp:
		return exprHasWindowFuncFast(v.Left) || exprHasWindowFuncFast(v.Right)
	case *sql.IsDistinctFrom:
		return exprHasWindowFuncFast(v.Left) || exprHasWindowFuncFast(v.Right)
	case *sql.IsNotDistinctFrom:
		return exprHasWindowFuncFast(v.Left) || exprHasWindowFuncFast(v.Right)
	default:
		return compositeExprHasWindow(expr)
	}
}

// funcCallHasWindow reports whether the call itself is a window function or
// one of its args / ORDER BY terms / FILTER condition contains one.
func funcCallHasWindow(v *sql.FuncCall) bool {
	if v.Over != nil {
		return true
	}
	for _, a := range v.Args {
		if exprHasWindowFuncFast(a) {
			return true
		}
	}
	for _, ob := range v.OrderBy {
		if exprHasWindowFuncFast(ob.Expr) {
			return true
		}
	}
	return v.Filter != nil && exprHasWindowFuncFast(v.Filter)
}

// compositeExprHasWindow walks the composite node families, delegating to the
// per-family helpers. Leaf nodes (literals, column refs) fall through as
// false.
func compositeExprHasWindow(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.Between:
		return betweenExprHasWindow(v)
	case *sql.InList:
		if exprHasWindowFuncFast(v.Operand) {
			return true
		}
		return listHasWindow(v.List)
	case *sql.CaseExpr:
		return caseExprHasWindow(v)
	case *sql.RowValue:
		return listHasWindow(v.Values)
	default:
		return unaryExprHasWindow(expr)
	}
}

// unaryExprHasWindow walks the single-operand wrapper family (unary ops,
// parens, CAST, IS NULL/NOT NULL, IS TRUE/FALSE); other nodes are leaves.
func unaryExprHasWindow(expr sql.Expr) bool {
	if operand := singleExprOperand(expr); operand != nil {
		return exprHasWindowFuncFast(operand)
	}
	return false
}

// betweenExprHasWindow checks a BETWEEN's operand and bounds.
func betweenExprHasWindow(v *sql.Between) bool {
	return exprHasWindowFuncFast(v.Operand) || exprHasWindowFuncFast(v.Low) ||
		exprHasWindowFuncFast(v.High)
}

// listHasWindow reports whether any list element contains a window function.
func listHasWindow(list []sql.Expr) bool {
	for _, item := range list {
		if exprHasWindowFuncFast(item) {
			return true
		}
	}
	return false
}

// caseExprHasWindow checks a CASE's operand, WHEN/THEN arms, and ELSE.
func caseExprHasWindow(v *sql.CaseExpr) bool {
	if exprHasWindowFuncFast(v.Operand) || exprHasWindowFuncFast(v.Else) {
		return true
	}
	for _, w := range v.Whens {
		if exprHasWindowFuncFast(w.When) || exprHasWindowFuncFast(w.Then) {
			return true
		}
	}
	return false
}
