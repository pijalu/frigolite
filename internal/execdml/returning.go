package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
)

// returningScope pairs the qualifier used in expressions (the table's alias,
// if any, otherwise its name) with the real table name for column lookups.
type returningScope struct {
	qualifier string
	table     string
}

// validateReturning checks RETURNING expressions before any rows are
// processed. SQLite resolves RETURNING column references at prepare time, so
// an invalid reference ("no such column: x", or a TABLE.* wildcard) must fail
// even when the statement matches zero rows. RETURNING may only reference
// columns of the table being modified (unqualified, or qualified with the
// table name); NEW/OLD, aliases, and other tables are rejected. Subqueries in
// the RETURNING expression are walked with their own FROM scope.
func (e *DMLExecutor) validateReturning(ret sql.SelectColumn, colDefs []sql.ColumnDef, tableName string) error {
	scope := []returningScope{{qualifier: tableName, table: tableName}}
	return e.validateReturningExpr(ret.Expr, colDefs, tableName, scope, true)
}

// validateReturningExpr walks one expression, validating ColumnRefs against
// the visible scopes. scope lists the visible qualifier/table pairs; topLevel
// is true only for the RETURNING expression itself (where TABLE.* wildcards
// are rejected).
func (e *DMLExecutor) validateReturningExpr(expr sql.Expr, colDefs []sql.ColumnDef, tableName string, scope []returningScope, topLevel bool) error {
	if expr == nil {
		return nil
	}
	switch x := expr.(type) {
	case *sql.ColumnRef:
		return e.validateReturningColumnRef(x, colDefs, tableName, scope, topLevel)
	case *sql.ParenExpr, *sql.UnaryOp, *sql.IsNull, *sql.IsNotNull, *sql.IsTrue, *sql.IsFalse, *sql.CastExpr, *sql.RaiseExpr:
		return e.validateReturningExpr(returningSingleOperand(x), colDefs, tableName, scope, false)
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		left, right := execquery.BinaryExprOperands(x)
		if err := e.validateReturningExpr(left, colDefs, tableName, scope, false); err != nil {
			return err
		}
		return e.validateReturningExpr(right, colDefs, tableName, scope, false)
	case *sql.FuncCall:
		return e.validateReturningExprList(x.Args, colDefs, tableName, scope, x.Filter)
	case *sql.Between:
		return e.validateReturningBetween(x, colDefs, tableName, scope)
	case *sql.InList:
		return e.validateReturningExprList(append([]sql.Expr{x.Operand}, x.List...), colDefs, tableName, scope, nil)
	case *sql.Subquery, *sql.ExistsExpr:
		return e.validateReturningSelect(execquery.SubquerySelect(x), colDefs, tableName, scope)
	case *sql.CaseExpr:
		return e.validateReturningCase(x, colDefs, tableName, scope)
	case *sql.RowValue:
		return e.validateReturningExprList(x.Values, colDefs, tableName, scope, nil)
	default:
		// Literals (NumericLit, StringLit, NullLit, BlobLit) have no columns.
		return nil
	}
}

// returningSingleOperand returns the single operand of a unary-like RETURNING
// expression node, or nil for other node types.
func returningSingleOperand(expr interface{}) sql.Expr {
	switch x := expr.(type) {
	case *sql.ParenExpr:
		return x.Expr
	case *sql.UnaryOp:
		return x.Operand
	case *sql.IsNull:
		return x.Operand
	case *sql.IsNotNull:
		return x.Operand
	case *sql.IsTrue:
		return x.Operand
	case *sql.IsFalse:
		return x.Operand
	case *sql.CastExpr:
		return x.Operand
	case *sql.RaiseExpr:
		return x.Message
	}
	return nil
}

// validateReturningExprList validates every expression in the list (plus an
// optional FILTER expression) against the visible scopes.
func (e *DMLExecutor) validateReturningExprList(exprs []sql.Expr, colDefs []sql.ColumnDef, tableName string, scope []returningScope, filter sql.Expr) error {
	for _, expr := range exprs {
		if err := e.validateReturningExpr(expr, colDefs, tableName, scope, false); err != nil {
			return err
		}
	}
	if filter != nil {
		return e.validateReturningExpr(filter, colDefs, tableName, scope, false)
	}
	return nil
}

// validateReturningBetween validates the operand and both bounds of a
// BETWEEN expression in RETURNING.
func (e *DMLExecutor) validateReturningBetween(x *sql.Between, colDefs []sql.ColumnDef, tableName string, scope []returningScope) error {
	if err := e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false); err != nil {
		return err
	}
	if err := e.validateReturningExpr(x.Low, colDefs, tableName, scope, false); err != nil {
		return err
	}
	return e.validateReturningExpr(x.High, colDefs, tableName, scope, false)
}

// validateReturningCase validates the operand, WHEN branches, and ELSE of a
// CASE expression in RETURNING.
func (e *DMLExecutor) validateReturningCase(x *sql.CaseExpr, colDefs []sql.ColumnDef, tableName string, scope []returningScope) error {
	if err := e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false); err != nil {
		return err
	}
	for _, when := range x.Whens {
		if err := e.validateReturningExpr(when.When, colDefs, tableName, scope, false); err != nil {
			return err
		}
		if err := e.validateReturningExpr(when.Then, colDefs, tableName, scope, false); err != nil {
			return err
		}
	}
	return e.validateReturningExpr(x.Else, colDefs, tableName, scope, false)
}

// validateReturningSelect walks a subquery's expressions. The subquery's FROM
// tables are added to the visible scope (a qualifier may name any of them,
// and an unqualified reference may resolve to any of their columns or to an
// outer column of the modified table).
func (e *DMLExecutor) validateReturningSelect(s *sql.SelectStmt, colDefs []sql.ColumnDef, tableName string, outer []returningScope) error {
	if s == nil {
		return nil
	}
	scope := make([]returningScope, len(outer))
	copy(scope, outer)
	scope = e.appendReturningFromScope(scope, &s.From)
	for _, j := range s.Joins {
		scope = e.appendReturningFromScope(scope, &j.Table)
	}
	for _, col := range s.Columns {
		if err := e.validateReturningExpr(col.Expr, colDefs, tableName, scope, false); err != nil {
			return err
		}
	}
	return e.validateReturningSelectClauses(s, colDefs, tableName, scope, outer)
}

// validateReturningSelectClauses validates WHERE / GROUP BY / HAVING /
// ORDER BY / LIMIT / OFFSET / UNION of a subquery in RETURNING.
func (e *DMLExecutor) validateReturningSelectClauses(s *sql.SelectStmt, colDefs []sql.ColumnDef, tableName string, scope []returningScope, outer []returningScope) error {
	if err := e.validateReturningExpr(s.Where, colDefs, tableName, scope, false); err != nil {
		return err
	}
	if err := e.validateReturningExprs(s.GroupBy, colDefs, tableName, scope); err != nil {
		return err
	}
	if err := e.validateReturningClauses(colDefs, tableName, scope, s.Having, s.Limit, s.Offset); err != nil {
		return err
	}
	orderExprs := make([]sql.Expr, len(s.OrderBy))
	for i, o := range s.OrderBy {
		orderExprs[i] = o.Expr
	}
	if err := e.validateReturningExprs(orderExprs, colDefs, tableName, scope); err != nil {
		return err
	}
	if s.Union != nil {
		if err := e.validateReturningSelect(s.Union, colDefs, tableName, outer); err != nil {
			return err
		}
	}
	return nil
}

// validateReturningExprs validates a list of RETURNING expressions.
func (e *DMLExecutor) validateReturningExprs(exprs []sql.Expr, colDefs []sql.ColumnDef, tableName string, scope []returningScope) error {
	for _, expr := range exprs {
		if err := e.validateReturningExpr(expr, colDefs, tableName, scope, false); err != nil {
			return err
		}
	}
	return nil
}

// validateReturningClauses validates a set of optional RETURNING expressions
// (HAVING, LIMIT, OFFSET, ...) against the visible scopes.
func (e *DMLExecutor) validateReturningClauses(colDefs []sql.ColumnDef, tableName string, scope []returningScope, exprs ...sql.Expr) error {
	for _, expr := range exprs {
		if err := e.validateReturningExpr(expr, colDefs, tableName, scope, false); err != nil {
			return err
		}
	}
	return nil
}

// appendReturningFromScope adds the qualifier names of a FROM table reference
// (alias if present, otherwise the table name) to the visible scope, mapping
// the qualifier to the real table for column lookups.
func (e *DMLExecutor) appendReturningFromScope(scope []returningScope, ref *sql.TableRef) []returningScope {
	if ref == nil || ref.Subquery != nil {
		// Anonymous subquery in FROM: no qualifier name.
		return scope
	}
	qualifier := ref.Name
	if ref.As != "" {
		qualifier = ref.As
	}
	for _, s := range scope {
		if strings.EqualFold(s.qualifier, qualifier) {
			return scope
		}
	}
	return append(scope, returningScope{qualifier: qualifier, table: ref.Name})
}

// validateReturningColumnRef checks a single column reference in a RETURNING
// expression against the visible scopes.
func (e *DMLExecutor) validateReturningColumnRef(v *sql.ColumnRef, colDefs []sql.ColumnDef, tableName string, scope []returningScope, topLevel bool) error {
	if v.Name == "*" {
		if topLevel && v.Table != "" {
			return fmt.Errorf("RETURNING may not use \"TABLE.*\" wildcards")
		}
		return nil
	}
	if v.Table == "" {
		return e.returningUnqualifiedCol(v, colDefs, tableName, scope)
	}
	return e.returningQualifiedCol(v, colDefs, tableName, scope)
}

// returningUnqualifiedCol validates an unqualified column reference in
// RETURNING: TRUE/FALSE literals, rowid aliases, a column of the modified
// table, or (inside a subquery) a column of an in-scope table.
func (e *DMLExecutor) returningUnqualifiedCol(v *sql.ColumnRef, colDefs []sql.ColumnDef, tableName string, scope []returningScope) error {
	if strings.EqualFold(v.Name, "TRUE") || strings.EqualFold(v.Name, "FALSE") {
		return nil
	}
	if execquery.IsRowIDName(v.Name) {
		return nil
	}
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, v.Name) {
			return nil
		}
	}
	for _, qual := range scope {
		if strings.EqualFold(qual.qualifier, tableName) {
			continue
		}
		if e.returningTableHasColumn(qual.table, v.Name) {
			return nil
		}
	}
	return fmt.Errorf("no such column: %s", v.Name)
}

// returningQualifiedCol validates a qualified column reference against the
// modified table or an in-scope table.
func (e *DMLExecutor) returningQualifiedCol(v *sql.ColumnRef, colDefs []sql.ColumnDef, tableName string, scope []returningScope) error {
	if strings.EqualFold(v.Table, tableName) {
		if execquery.IsRowIDName(v.Name) {
			return nil
		}
		for _, cd := range colDefs {
			if strings.EqualFold(cd.Name, v.Name) {
				return nil
			}
		}
		return fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
	}
	for _, qual := range scope {
		if strings.EqualFold(qual.qualifier, v.Table) {
			if e.returningTableHasColumn(qual.table, v.Name) {
				return nil
			}
			return fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
		}
	}
	return fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
}

// returningTableHasColumn reports whether a table in the visible scope has a
// column with the given name. Tables that cannot be resolved (virtual tables,
// views, missing entries) are treated leniently: the reference is accepted
// rather than producing a false "no such column" error.
func (e *DMLExecutor) returningTableHasColumn(tableName, name string) bool {
	entry, err := e.ctx.Schema().FindTable(tableName)
	if err != nil {
		return true
	}
	cols := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, cd := range cols {
		if strings.EqualFold(cd.Name, name) {
			return true
		}
	}
	return false
}
