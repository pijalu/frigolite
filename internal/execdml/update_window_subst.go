// This file implements the UPDATE ... SET window-function substitution:
// when an INSTEAD OF trigger on a view assigns window function results to
// columns, each window function call in the SET expression is replaced by
// its pre-materialized value for the current row before evaluation.
package execdml

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/sql"
)

func replaceWindowFuncs(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	// Clone the expression, substituting window FuncCalls with literals.
	clone, err := cloneExprWithWindowSubst(expr, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	return clone, nil
}

// cloneExprWithWindowSubst deep-copies an expression, replacing each window
// FuncCall with a NumericLit/StringLit/NullLit carrying its precomputed value.
func cloneExprWithWindowSubst(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	if expr == nil {
		return nil, nil
	}
	switch v := expr.(type) {
	case *sql.FuncCall:
		return cloneWindowSubstFuncCall(v, winVals, rowIdx)
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		return cloneWindowSubstPair(expr, winVals, rowIdx)
	case *sql.UnaryOp, *sql.ParenExpr, *sql.CastExpr, *sql.IsNull,
		*sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		return cloneWindowSubstSingle(expr, winVals, rowIdx)
	case *sql.Between:
		return cloneWindowSubstBetween(v, winVals, rowIdx)
	case *sql.InList:
		return cloneWindowSubstInList(v, winVals, rowIdx)
	case *sql.CaseExpr:
		return cloneWindowSubstCase(v, winVals, rowIdx)
	default:
		// Leaf nodes (ColumnRef, literals, Subquery, etc.) are returned as-is.
		return expr, nil
	}
}

// cloneWindowSubstFuncCall clones a FuncCall, substituting a window function
// with its precomputed value and recursing into a plain function's arguments.
func cloneWindowSubstFuncCall(v *sql.FuncCall, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	if v.Over != nil {
		vals, ok := winVals[v]
		if !ok {
			return nil, fmt.Errorf("window function %s has no precomputed value", v.Name)
		}
		if rowIdx >= len(vals) {
			return nil, fmt.Errorf("window function %s row index out of range", v.Name)
		}
		return literalForValue(vals[rowIdx]), nil
	}
	args := make([]sql.Expr, len(v.Args))
	for i, a := range v.Args {
		c, err := cloneExprWithWindowSubst(a, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
		args[i] = c
	}
	cp := *v
	cp.Args = args
	return &cp, nil
}

// cloneWindowSubstPair clones a two-operand expression node (BinaryOp /
// IsDistinctFrom / IsNotDistinctFrom), recursing into both operands.
func cloneWindowSubstPair(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	l, r, err := cloneWindowSubstOperands(expr, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return &sql.BinaryOp{Operator: v.Operator, Left: l, Right: r}, nil
	case *sql.IsDistinctFrom:
		return &sql.IsDistinctFrom{Left: l, Right: r}, nil
	case *sql.IsNotDistinctFrom:
		return &sql.IsNotDistinctFrom{Left: l, Right: r}, nil
	}
	return expr, nil
}

// cloneWindowSubstOperands clones the two operands of a pair expression.
func cloneWindowSubstOperands(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, sql.Expr, error) {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return cloneWindowSubstPairChildren(v.Left, v.Right, winVals, rowIdx)
	case *sql.IsDistinctFrom:
		return cloneWindowSubstPairChildren(v.Left, v.Right, winVals, rowIdx)
	case *sql.IsNotDistinctFrom:
		return cloneWindowSubstPairChildren(v.Left, v.Right, winVals, rowIdx)
	}
	return nil, nil, nil
}

// cloneWindowSubstPairChildren clones a left/right child pair.
func cloneWindowSubstPairChildren(left, right sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, sql.Expr, error) {
	l, err := cloneExprWithWindowSubst(left, winVals, rowIdx)
	if err != nil {
		return nil, nil, err
	}
	r, err := cloneExprWithWindowSubst(right, winVals, rowIdx)
	if err != nil {
		return nil, nil, err
	}
	return l, r, nil
}

// cloneWindowSubstSingle clones a single-operand expression node (UnaryOp,
// ParenExpr, CastExpr, IsNull, IsNotNull, IsTrue, IsFalse).
func cloneWindowSubstSingle(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	o, err := cloneExprWithWindowSubst(winSingleOperand(expr), winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	switch v := expr.(type) {
	case *sql.UnaryOp:
		return &sql.UnaryOp{Operator: v.Operator, Operand: o}, nil
	case *sql.ParenExpr:
		return &sql.ParenExpr{Expr: o}, nil
	case *sql.CastExpr:
		return &sql.CastExpr{Operand: o, AsType: v.AsType}, nil
	case *sql.IsNull:
		return &sql.IsNull{Operand: o}, nil
	case *sql.IsNotNull:
		return &sql.IsNotNull{Operand: o}, nil
	case *sql.IsTrue:
		return &sql.IsTrue{Operand: o}, nil
	case *sql.IsFalse:
		return &sql.IsFalse{Operand: o}, nil
	}
	return expr, nil
}

// winSingleOperand returns the single operand of a unary-like expression node.
func winSingleOperand(expr sql.Expr) sql.Expr {
	switch v := expr.(type) {
	case *sql.UnaryOp:
		return v.Operand
	case *sql.ParenExpr:
		return v.Expr
	case *sql.CastExpr:
		return v.Operand
	case *sql.IsNull:
		return v.Operand
	case *sql.IsNotNull:
		return v.Operand
	case *sql.IsTrue:
		return v.Operand
	case *sql.IsFalse:
		return v.Operand
	}
	return nil
}

// cloneWindowSubstBetween clones a BETWEEN expression, recursing into the
// operand, low, and high bounds.
func cloneWindowSubstBetween(v *sql.Between, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	o, err := cloneExprWithWindowSubst(v.Operand, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	lo, err := cloneExprWithWindowSubst(v.Low, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	hi, err := cloneExprWithWindowSubst(v.High, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	return &sql.Between{Negated: v.Negated, Operand: o, Low: lo, High: hi}, nil
}

// cloneWindowSubstInList clones an IN-list expression, recursing into the
// operand and every list item.
func cloneWindowSubstInList(v *sql.InList, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	o, err := cloneExprWithWindowSubst(v.Operand, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	list := make([]sql.Expr, len(v.List))
	for i, item := range v.List {
		c, err := cloneExprWithWindowSubst(item, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
		list[i] = c
	}
	return &sql.InList{Negated: v.Negated, Operand: o, List: list}, nil
}

// cloneWindowSubstCase clones a CASE expression, recursing into the operand,
// WHEN/THEN pairs, and ELSE.
func cloneWindowSubstCase(v *sql.CaseExpr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	var operand sql.Expr
	var err error
	if v.Operand != nil {
		operand, err = cloneExprWithWindowSubst(v.Operand, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
	}
	whens := make([]sql.WhenClause, len(v.Whens))
	for i, w := range v.Whens {
		wc, err := cloneExprWithWindowSubst(w.When, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
		tc, err := cloneExprWithWindowSubst(w.Then, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
		whens[i] = sql.WhenClause{When: wc, Then: tc}
	}
	var els sql.Expr
	if v.Else != nil {
		els, err = cloneExprWithWindowSubst(v.Else, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
	}
	return &sql.CaseExpr{Operand: operand, Whens: whens, Else: els}, nil
}

// literalForValue wraps a window value as a literal expression node.
