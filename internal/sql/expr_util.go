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
	switch e.(type) {
	case *NumericLit, *StringLit, *NullLit, *BlobLit:
		return exprStringLiteral(e)
	case *ParameterExpr, *ColumnRef:
		return exprStringReference(e)
	case *BinaryOp, *UnaryOp, *ParenExpr, *RowValue, *FuncCall, *Between, *InList:
		return exprStringCompound(e)
	case *IsNull, *IsNotNull, *IsDistinctFrom, *IsNotDistinctFrom, *IsTrue, *IsFalse:
		return exprStringNullTest(e)
	case *Subquery, *ExistsExpr:
		return exprStringSubquery(e)
	}
	return "?"
}

// exprStringLiteral renders literal-valued expression nodes.
func exprStringLiteral(e Expr) string {
	switch v := e.(type) {
	case *NumericLit:
		return v.Value
	case *StringLit:
		return "'" + strings.ReplaceAll(v.Value, "'", "''") + "'"
	case *NullLit:
		return "NULL"
	case *BlobLit:
		return "x'" + hex.EncodeToString(v.Value) + "'"
	}
	return "?"
}

// exprStringReference renders parameter and column-reference nodes.
func exprStringReference(e Expr) string {
	switch v := e.(type) {
	case *ParameterExpr:
		if v.Name != "" {
			return v.Name
		}
		return "?"
	case *ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Name
		}
		return v.Name
	}
	return "?"
}

// exprStringCompound renders operator and structure expression nodes.
func exprStringCompound(e Expr) string {
	switch v := e.(type) {
	case *BinaryOp:
		return formatBinaryOp(v)
	case *UnaryOp:
		return formatUnaryOp(v)
	case *ParenExpr:
		return "(" + ExprString(v.Expr) + ")"
	case *RowValue:
		return "(" + formatExprList(v.Values) + ")"
	case *FuncCall:
		return formatFuncCall(v)
	case *Between:
		return formatBetween(v)
	case *InList:
		return formatInList(v)
	}
	return "?"
}

// formatBinaryOp renders a binary operation, normalizing <> to != as SQLite
// does in schema text.
func formatBinaryOp(v *BinaryOp) string {
	op := v.Operator
	if strings.TrimSpace(op) == "<>" {
		return ExprString(v.Left) + "!=" + ExprString(v.Right)
	}
	return ExprString(v.Left) + " " + op + " " + ExprString(v.Right)
}

// formatUnaryOp renders a unary operation. A nested unary or negative
// numeric operand is parenthesized so "-(−9223372036854775808)" renders as
// "-(-9223372036854775808)" (not "- -9223372036854775808", which the parser
// rejects).
func formatUnaryOp(v *UnaryOp) string {
	operand := ExprString(v.Operand)
	if _, isUnary := v.Operand.(*UnaryOp); isUnary {
		return v.Operator + "(" + operand + ")"
	}
	if _, isNum := v.Operand.(*NumericLit); isNum && strings.HasPrefix(strings.TrimSpace(operand), "-") {
		return v.Operator + "(" + operand + ")"
	}
	return v.Operator + " " + operand
}

// exprStringNullTest renders IS NULL / IS TRUE family nodes.
func exprStringNullTest(e Expr) string {
	switch v := e.(type) {
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
	}
	return "?"
}

// exprStringSubquery renders subquery and EXISTS nodes.
func exprStringSubquery(e Expr) string {
	switch v := e.(type) {
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
	}
	return "?"
}

// selectStmtToString converts a SelectStmt back to SQL text for use in
// ExprString (see original comment).
func selectStmtToString(s *SelectStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder

	writeSelectCTEs(&b, s.CTEs)
	b.WriteString("SELECT ")
	if s.Distinct {
		b.WriteString("DISTINCT ")
	}
	writeSelectColumns(&b, s.Columns)
	writeSelectFrom(&b, s.From)
	writeSelectJoins(&b, s.Joins)
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(ExprString(s.Where))
	}
	writeSelectGrouping(&b, s.GroupBy, s.Having)
	writeSelectOrdering(&b, s.OrderBy, s.Limit, s.Offset)
	writeSelectUnion(&b, s)
	return b.String()
}

// writeSelectCTEs writes the WITH clause prefix.
func writeSelectCTEs(b *strings.Builder, ctes []CTEDef) {
	if len(ctes) == 0 {
		return
	}
	b.WriteString("WITH ")
	for i, cte := range ctes {
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

// writeSelectColumns writes the SELECT column list.
func writeSelectColumns(b *strings.Builder, cols []SelectColumn) {
	for i, col := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(ExprString(col.Expr))
		if col.As != "" {
			b.WriteString(" AS ")
			b.WriteString(col.As)
		}
	}
}

// writeSelectFrom writes the FROM clause for a plain table reference.
func writeSelectFrom(b *strings.Builder, from TableRef) {
	if from.Name == "" {
		return
	}
	b.WriteString(" FROM ")
	b.WriteString(from.Name)
	if from.As != "" {
		b.WriteString(" AS ")
		b.WriteString(from.As)
	}
}

// writeSelectJoins writes JOIN clauses.
func writeSelectJoins(b *strings.Builder, joins []JoinClause) {
	for _, join := range joins {
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
}

// writeSelectGrouping writes GROUP BY and HAVING clauses.
func writeSelectGrouping(b *strings.Builder, groupBy []Expr, having Expr) {
	if len(groupBy) > 0 {
		b.WriteString(" GROUP BY ")
		b.WriteString(strings.Join(formatExprListParts(groupBy), ", "))
	}
	if having != nil {
		b.WriteString(" HAVING ")
		b.WriteString(ExprString(having))
	}
}

// writeSelectOrdering writes ORDER BY, LIMIT and OFFSET clauses.
func writeSelectOrdering(b *strings.Builder, orderBy []OrderByTerm, limit, offset Expr) {
	if len(orderBy) > 0 {
		b.WriteString(" ORDER BY ")
		b.WriteString(strings.Join(formatOrderByParts(orderBy), ", "))
	}
	if limit != nil {
		b.WriteString(" LIMIT ")
		b.WriteString(ExprString(limit))
	}
	if offset != nil {
		b.WriteString(" OFFSET ")
		b.WriteString(ExprString(offset))
	}
}

// writeSelectUnion writes the compound UNION / INTERSECT / EXCEPT tail.
func writeSelectUnion(b *strings.Builder, s *SelectStmt) {
	if s.Union == nil {
		return
	}
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

// formatExprList joins a list of expressions with ", ".
func formatExprList(exprs []Expr) string {
	return strings.Join(formatExprListParts(exprs), ", ")
}

// formatExprListParts renders each expression in a list.
func formatExprListParts(exprs []Expr) []string {
	parts := make([]string, len(exprs))
	for i, e := range exprs {
		parts[i] = ExprString(e)
	}
	return parts
}

// formatOrderByParts renders ORDER BY terms (with DESC suffix where set).
func formatOrderByParts(terms []OrderByTerm) []string {
	parts := make([]string, len(terms))
	for i, ob := range terms {
		s := ExprString(ob.Expr)
		if ob.Desc {
			s += " DESC"
		}
		parts[i] = s
	}
	return parts
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
