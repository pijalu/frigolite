// Package exec implements query execution.
//
// This file holds SQL-text serialization: converting SELECT statements and
// expressions back to SQL text (used for stored views, triggers, and
// CREATE TABLE ... AS SELECT schema round-tripping). It is split out of
// ddl_core.go so that file stays within the repository's size budget.
package execddl

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// exprToString converts an expression to its string representation.
func exprToString(expr sql.Expr) string {
	if expr == nil {
		return ""
	}
	switch v := expr.(type) {
	case *sql.ColumnRef, *sql.NumericLit, *sql.StringLit, *sql.NullLit:
		return leafToString(v)
	case *sql.BinaryOp, *sql.UnaryOp:
		return opToString(v)
	case *sql.FuncCall:
		return funcCallToString(v)
	case *sql.IsNull, *sql.IsNotNull:
		return isNullCheckToString(v)
	case *sql.ParenExpr, *sql.CastExpr:
		return wrapToString(v)
	case *sql.Between, *sql.InList, *sql.CaseExpr, *sql.RowValue:
		return complexExprToString(v)
	case *sql.Subquery, *sql.ExistsExpr:
		return subqueryToString(v)
	default:
		return "?"
	}
}

// leafToString serializes a leaf node: column reference or literal atom.
func leafToString(v sql.Expr) string {
	switch lit := v.(type) {
	case *sql.ColumnRef:
		if lit.Table != "" {
			return lit.Table + "." + lit.Name
		}
		return lit.Name
	case *sql.NumericLit:
		return lit.Value
	case *sql.StringLit:
		return "'" + lit.Value + "'"
	default:
		return "NULL"
	}
}

// opToString serializes a binary or unary operation.
func opToString(v sql.Expr) string {
	switch op := v.(type) {
	case *sql.BinaryOp:
		return binaryOpToString(op)
	default:
		return unaryOpToString(op.(*sql.UnaryOp))
	}
}

// wrapToString serializes a parenthesized expression or CAST.
func wrapToString(v sql.Expr) string {
	switch w := v.(type) {
	case *sql.ParenExpr:
		return "(" + exprToString(w.Expr) + ")"
	default:
		c := w.(*sql.CastExpr)
		return "CAST(" + exprToString(c.Operand) + " AS " + c.AsType + ")"
	}
}

// complexExprToString serializes a BETWEEN, IN, CASE, or row-value
// expression.
func complexExprToString(v sql.Expr) string {
	switch c := v.(type) {
	case *sql.Between:
		return betweenToString(c)
	case *sql.InList:
		return inListToString(c)
	case *sql.CaseExpr:
		return caseExprToString(c)
	default:
		return rowValueToString(c.(*sql.RowValue))
	}
}

// binaryOpToString serializes a binary operation.
func binaryOpToString(v *sql.BinaryOp) string {
	if v.Operator == "OR" || v.Operator == "AND" {
		return exprToString(v.Left) + " " + v.Operator + " " + exprToString(v.Right)
	}
	return exprToString(v.Left) + v.Operator + exprToString(v.Right)
}

// unaryOpToString serializes a unary operation.
func unaryOpToString(v *sql.UnaryOp) string {
	if v.Operator == "NOT" {
		return v.Operator + " " + exprToString(v.Operand)
	}
	return v.Operator + exprToString(v.Operand)
}

// isNullCheckToString serializes an IS NULL / IS NOT NULL check.
func isNullCheckToString(v sql.Expr) string {
	if _, ok := v.(*sql.IsNotNull); ok {
		return exprToString(v.(*sql.IsNotNull).Operand) + " IS NOT NULL"
	}
	return exprToString(v.(*sql.IsNull).Operand) + " IS NULL"
}

// subqueryToString serializes a subquery or EXISTS expression.
func subqueryToString(v sql.Expr) string {
	if ex, ok := v.(*sql.ExistsExpr); ok {
		result := ""
		if ex.Negated {
			result += "NOT "
		}
		return result + "EXISTS(" + selectStmtToString(ex.Select) + ")"
	}
	return "(" + selectStmtToString(v.(*sql.Subquery).Select) + ")"
}

// rowValueToString serializes a row-value expression.
func rowValueToString(v *sql.RowValue) string {
	result := "("
	for i, val := range v.Values {
		if i > 0 {
			result += ", "
		}
		result += exprToString(val)
	}
	return result + ")"
}

// selectStmtToString converts a SELECT statement to SQL text (used for
// views).
func selectStmtToString(s *sql.SelectStmt) string {
	if s == nil {
		return ""
	}
	result := ""
	// CTEs must be output before SELECT: WITH name AS (...) SELECT ...
	if len(s.CTEs) > 0 {
		result += ctesToString(s.CTEs)
	}
	result += "SELECT "
	if s.Distinct {
		result += "DISTINCT "
	}
	result += selectColumnsToString(s.Columns)
	result += selectFromToString(s)
	result += joinClausesToString(s.Joins)
	if s.Where != nil {
		result += " WHERE " + exprToString(s.Where)
	}
	result += groupByToString(s)
	if s.Having != nil {
		result += " HAVING " + exprToString(s.Having)
	}
	result += orderByToString(s.OrderBy)
	result += limitOffsetToString(s)
	result += windowsToString(s.Windows)
	result += compoundToString(s)
	return result
}

// ctesToString serializes a WITH clause.
func ctesToString(ctes []sql.CTEDef) string {
	result := "WITH "
	for i, cte := range ctes {
		if i > 0 {
			result += ", "
		}
		result += cte.Name
		if len(cte.Columns) > 0 {
			result += "(" + strings.Join(cte.Columns, ",") + ")"
		}
		result += " AS (" + selectStmtToString(cte.Select) + ")"
	}
	return result + " "
}

// selectColumnsToString serializes the SELECT column list.
func selectColumnsToString(cols []sql.SelectColumn) string {
	result := ""
	for i, col := range cols {
		if i > 0 {
			result += ", "
		}
		result += selectColumnToString(col)
	}
	return result
}

// selectFromToString serializes the FROM clause (table name and alias).
func selectFromToString(s *sql.SelectStmt) string {
	if s.From.Name == "" {
		return ""
	}
	result := " FROM " + s.From.Name
	if s.From.As != "" {
		result += " AS " + s.From.As
	}
	return result
}

// groupByToString serializes the GROUP BY clause.
func groupByToString(s *sql.SelectStmt) string {
	if len(s.GroupBy) == 0 {
		return ""
	}
	return " GROUP BY " + exprListToString(s.GroupBy)
}

// limitOffsetToString serializes LIMIT and OFFSET clauses.
func limitOffsetToString(s *sql.SelectStmt) string {
	if s.Limit == nil {
		return ""
	}
	result := " LIMIT " + exprToString(s.Limit)
	if s.Offset != nil {
		result += " OFFSET " + exprToString(s.Offset)
	}
	return result
}

// windowsToString serializes the WINDOW clause.
func windowsToString(windows []sql.WindowDef) string {
	if len(windows) == 0 {
		return ""
	}
	result := " WINDOW "
	for i, w := range windows {
		if i > 0 {
			result += ", "
		}
		result += w.Name + " AS (" + windowBodyToString(&w) + ")"
	}
	return result
}

// windowBodyToString serializes the PARTITION BY / ORDER BY body of a WINDOW
// clause definition (without the enclosing parentheses).
func windowBodyToString(w *sql.WindowDef) string {
	result := ""
	if len(w.Partitions) > 0 {
		result += "PARTITION BY " + exprListToString(w.Partitions)
	}
	if len(w.OrderBy) > 0 {
		if len(w.Partitions) > 0 {
			result += " "
		}
		result += "ORDER BY " + orderByToString(w.OrderBy)
	}
	return result
}

// compoundToString serializes the compound operator (UNION, INTERSECT,
// EXCEPT) and the right-hand SELECT.
func compoundToString(s *sql.SelectStmt) string {
	if s.SetOp == sql.SetNone || s.Union == nil {
		return ""
	}
	result := ""
	switch s.SetOp {
	case sql.SetUnion:
		result += "\n    UNION"
		if s.UnionAll {
			result += " ALL"
		}
	case sql.SetIntersect:
		result += "\n    INTERSECT"
	case sql.SetExcept:
		result += "\n    EXCEPT"
	}
	return result + "\n    " + selectStmtToString(s.Union)
}

// selectColumnToString serializes one SELECT column.
func selectColumnToString(col sql.SelectColumn) string {
	if ref, ok := col.Expr.(*sql.ColumnRef); ok {
		if ref.Table != "" {
			return ref.Table + "." + ref.Name + aliasClause(col.As)
		}
		return ref.Name + aliasClause(col.As)
	}
	if fn, ok := col.Expr.(*sql.FuncCall); ok {
		return funcCallToString(fn) + aliasClause(col.As)
	}
	return exprToString(col.Expr) + aliasClause(col.As)
}

// aliasClause serializes an AS alias.
func aliasClause(as string) string {
	if as != "" {
		return " " + as
	}
	return ""
}

// funcCallToString serializes a function call.
func funcCallToString(v *sql.FuncCall) string {
	result := v.Name + "("
	for i, arg := range v.Args {
		if i > 0 {
			result += ", "
		}
		result += exprToString(arg)
	}
	result += ")"
	if v.Filter != nil {
		result += " FILTER (WHERE " + exprToString(v.Filter) + ")"
	}
	if v.Over != nil {
		result += " OVER " + windowDefToString(v.Over)
	}
	return result
}

// betweenToString serializes a BETWEEN expression.
func betweenToString(v *sql.Between) string {
	result := exprToString(v.Operand)
	if v.Negated {
		result += " NOT BETWEEN "
	} else {
		result += " BETWEEN "
	}
	return result + exprToString(v.Low) + " AND " + exprToString(v.High)
}

// inListToString serializes an IN (...) expression.
func inListToString(v *sql.InList) string {
	result := exprToString(v.Operand)
	if v.Negated {
		result += " NOT IN ("
	} else {
		result += " IN ("
	}
	for i, item := range v.List {
		if i > 0 {
			result += ", "
		}
		result += exprToString(item)
	}
	return result + ")"
}

// caseExprToString serializes a CASE expression.
func caseExprToString(v *sql.CaseExpr) string {
	result := "CASE"
	if v.Operand != nil {
		result += " " + exprToString(v.Operand)
	}
	for _, w := range v.Whens {
		result += " WHEN " + exprToString(w.When) + " THEN " + exprToString(w.Then)
	}
	if v.Else != nil {
		result += " ELSE " + exprToString(v.Else)
	}
	return result + " END"
}
