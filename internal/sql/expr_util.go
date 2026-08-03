// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//

package sql

import (
	"encoding/hex"
	"strconv"
	"strings"
)

// UnwrapParenExpr recursively unwraps *ParenExpr nodes to return the
// underlying expression. This allows ParenExpr to be a transparent
// wrapper that does not require explicit case handling in every
// type switch — callers can preprocess with UnwrapParenExpr at
// entry points.
//
// This function is idempotent: calling it on an expression with no
// ParenExpr nodes returns the expression unchanged.
func UnwrapParenExpr(expr Expr) Expr {
	for {
		if p, ok := expr.(*ParenExpr); ok {
			expr = p.Expr
		} else {
			return expr
		}
	}
}

// EvalNumber evaluates an expression that is statically known to be a number
// literal (possibly negated), returning its integer value.
func EvalNumber(e Expr) (int64, bool) {
	switch v := e.(type) {
	case *NumericLit:
		n, err := strconv.ParseInt(v.Value, 10, 64)
		if err != nil {
			f, err := strconv.ParseFloat(v.Value, 64)
			if err != nil {
				return 0, false
			}
			return int64(f), true
		}
		return n, true
	case *UnaryOp:
		if v.Operator == "-" {
			inner, ok := EvalNumber(v.Operand)
			return -inner, ok
		}
		return EvalNumber(v.Operand)
	default:
		return 0, false
	}
}

// ExprString renders an expression back to SQL text. Used for trigger body
// serialization and ALTER TABLE RENAME column updates.
func ExprString(e Expr) string {
	if e == nil {
		return ""
	}
	switch v := e.(type) {
	case *NumericLit:
		return v.Value
	case *StringLit:
		return "'" + strings.ReplaceAll(v.Value, "'", "''") + "'"
	case *NullLit:
		return "NULL"
	case *ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Name
		}
		return v.Name
	case *BinaryOp:
		return ExprString(v.Left) + " " + v.Operator + " " + ExprString(v.Right)
	case *UnaryOp:
		return v.Operator + " " + ExprString(v.Operand)
	case *IsNull:
		return ExprString(v.Operand) + " IS NULL"
	case *IsNotNull:
		return ExprString(v.Operand) + " NOT NULL"
	case *IsDistinctFrom:
		return ExprString(v.Left) + " IS DISTINCT FROM " + ExprString(v.Right)
	case *IsNotDistinctFrom:
		return ExprString(v.Left) + " IS NOT DISTINCT FROM " + ExprString(v.Right)
	case *IsTrue:
		if v.Negated {
			return ExprString(v.Operand) + " IS NOT TRUE"
		}
		return ExprString(v.Operand) + " IS TRUE"
	case *IsFalse:
		if v.Negated {
			return ExprString(v.Operand) + " IS NOT FALSE"
		}
		return ExprString(v.Operand) + " IS FALSE"
	case *BlobLit:
		return "x'" + hex.EncodeToString(v.Value) + "'"
	case *Subquery:
		if v.Select != nil {
			return "(" + selectStmtToString(v.Select) + ")"
		}
		return "(?)"
	case *ExistsExpr:
		s := "EXISTS "
		if v.Negated {
			s = "NOT EXISTS "
		}
		if v.Select != nil {
			return s + "(" + selectStmtToString(v.Select) + ")"
		}
		return s + "(?)"
	case *Between:
		return formatBetween(v)
	case *InList:
		return formatInList(v)
	case *FuncCall:
		return formatFuncCall(v)
	case *RowValue:
		result := "("
		for i, val := range v.Values {
			if i > 0 {
				result += ", "
			}
			result += ExprString(val)
		}
		return result + ")"
	case *ParenExpr:
		return "(" + ExprString(v.Expr) + ")"
	default:
		return "?"
	}
}

// selectStmtToString converts a SelectStmt back to SQL text for use in
// ExprString (see original comment).
func selectStmtToString(s *SelectStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder

	// CTEs
	if len(s.CTEs) > 0 {
		b.WriteString("WITH ")
		for i, cte := range s.CTEs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(cte.Name)
			if len(cte.Columns) > 0 {
				b.WriteString("(")
				for j, col := range cte.Columns {
					if j > 0 {
						b.WriteString(", ")
					}
					b.WriteString(col)
				}
				b.WriteString(")")
			}
			b.WriteString(" AS (")
			if cte.Select != nil {
				b.WriteString(selectStmtToString(cte.Select))
			}
			b.WriteString(")")
		}
		b.WriteString(" ")
	}

	b.WriteString("SELECT ")
	if s.Distinct {
		b.WriteString("DISTINCT ")
	}
	for i, col := range s.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(ExprString(col.Expr))
		if col.As != "" {
			b.WriteString(" AS ")
			b.WriteString(col.As)
		}
	}
	if s.From.Name != "" {
		b.WriteString(" FROM ")
		b.WriteString(s.From.Name)
		if s.From.As != "" {
			b.WriteString(" AS ")
			b.WriteString(s.From.As)
		}
	}
	// JOINs
	for _, join := range s.Joins {
		b.WriteString(" ")
		b.WriteString(join.JoinType)
		b.WriteString(" JOIN ")
		b.WriteString(join.Table.Name)
		if join.Table.As != "" {
			b.WriteString(" AS ")
			b.WriteString(join.Table.As)
		}
		if join.On != nil {
			b.WriteString(" ON ")
			b.WriteString(ExprString(join.On))
		}
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(ExprString(s.Where))
	}
	if len(s.GroupBy) > 0 {
		b.WriteString(" GROUP BY ")
		for i, gb := range s.GroupBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(ExprString(gb))
		}
	}
	if s.Having != nil {
		b.WriteString(" HAVING ")
		b.WriteString(ExprString(s.Having))
	}
	if len(s.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, ob := range s.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(ExprString(ob.Expr))
			if ob.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT ")
		b.WriteString(ExprString(s.Limit))
	}
	if s.Offset != nil {
		b.WriteString(" OFFSET ")
		b.WriteString(ExprString(s.Offset))
	}
	// UNION / INTERSECT / EXCEPT
	if s.Union != nil {
		switch s.SetOp {
		case SetUnion:
			b.WriteString(" UNION ")
			if s.UnionAll {
				b.WriteString("ALL ")
			}
		case SetIntersect:
			b.WriteString(" INTERSECT ")
		case SetExcept:
			b.WriteString(" EXCEPT ")
		}
		b.WriteString(selectStmtToString(s.Union))
	}
	return b.String()
}

// formatBetween renders a BETWEEN expression.
func formatBetween(v *Between) string {
	s := ExprString(v.Operand) + " BETWEEN " + ExprString(v.Low) + " AND " + ExprString(v.High)
	if v.Negated {
		s = "NOT (" + s + ")"
	}
	return s
}

// formatInList renders an IN (list) expression.
func formatInList(v *InList) string {
	var items []string
	for _, item := range v.List {
		items = append(items, ExprString(item))
	}
	s := ExprString(v.Operand)
	if v.Negated {
		s += " NOT IN ("
	} else {
		s += " IN ("
	}
	s += strings.Join(items, ", ") + ")"
	return s
}

// formatFuncCall renders a function call expression.
func formatFuncCall(v *FuncCall) string {
	var args []string
	for _, arg := range v.Args {
		args = append(args, ExprString(arg))
	}
	result := v.Name + "(" + strings.Join(args, ", ") + ")"
	if v.Filter != nil {
		result += " FILTER (WHERE " + ExprString(v.Filter) + ")"
	}
	if v.Over != nil {
		result += " OVER " + v.Over.String()
	}
	return result
}
