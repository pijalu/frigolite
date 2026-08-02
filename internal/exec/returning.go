package exec

import (
	"fmt"
	"strings"

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
func (e *Engine) validateReturning(ret sql.SelectColumn, colDefs []sql.ColumnDef, tableName string) error {
	scope := []returningScope{{qualifier: tableName, table: tableName}}
	return e.validateReturningExpr(ret.Expr, colDefs, tableName, scope, true)
}

// validateReturningExpr walks one expression, validating ColumnRefs against
// the visible scopes. scope lists the visible qualifier/table pairs; topLevel
// is true only for the RETURNING expression itself (where TABLE.* wildcards
// are rejected).
func (e *Engine) validateReturningExpr(expr sql.Expr, colDefs []sql.ColumnDef, tableName string, scope []returningScope, topLevel bool) error {
	if expr == nil {
		return nil
	}
	switch x := expr.(type) {
	case *sql.ColumnRef:
		return e.validateReturningColumnRef(x, colDefs, tableName, scope, topLevel)
	case *sql.ParenExpr:
		return e.validateReturningExpr(x.Expr, colDefs, tableName, scope, false)
	case *sql.BinaryOp:
		if err := e.validateReturningExpr(x.Left, colDefs, tableName, scope, false); err != nil {
			return err
		}
		return e.validateReturningExpr(x.Right, colDefs, tableName, scope, false)
	case *sql.UnaryOp:
		return e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false)
	case *sql.FuncCall:
		for _, arg := range x.Args {
			if err := e.validateReturningExpr(arg, colDefs, tableName, scope, false); err != nil {
				return err
			}
		}
		if x.Filter != nil {
			return e.validateReturningExpr(x.Filter, colDefs, tableName, scope, false)
		}
		return nil
	case *sql.IsNull:
		return e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false)
	case *sql.IsNotNull:
		return e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false)
	case *sql.IsDistinctFrom:
		if err := e.validateReturningExpr(x.Left, colDefs, tableName, scope, false); err != nil {
			return err
		}
		return e.validateReturningExpr(x.Right, colDefs, tableName, scope, false)
	case *sql.IsNotDistinctFrom:
		if err := e.validateReturningExpr(x.Left, colDefs, tableName, scope, false); err != nil {
			return err
		}
		return e.validateReturningExpr(x.Right, colDefs, tableName, scope, false)
	case *sql.IsTrue:
		return e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false)
	case *sql.IsFalse:
		return e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false)
	case *sql.Between:
		if err := e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false); err != nil {
			return err
		}
		if err := e.validateReturningExpr(x.Low, colDefs, tableName, scope, false); err != nil {
			return err
		}
		return e.validateReturningExpr(x.High, colDefs, tableName, scope, false)
	case *sql.InList:
		if err := e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false); err != nil {
			return err
		}
		for _, item := range x.List {
			if err := e.validateReturningExpr(item, colDefs, tableName, scope, false); err != nil {
				return err
			}
		}
		return nil
	case *sql.Subquery:
		return e.validateReturningSelect(x.Select, colDefs, tableName, scope)
	case *sql.ExistsExpr:
		return e.validateReturningSelect(x.Select, colDefs, tableName, scope)
	case *sql.CaseExpr:
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
	case *sql.CastExpr:
		return e.validateReturningExpr(x.Operand, colDefs, tableName, scope, false)
	case *sql.RaiseExpr:
		return e.validateReturningExpr(x.Message, colDefs, tableName, scope, false)
	case *sql.RowValue:
		for _, item := range x.Values {
			if err := e.validateReturningExpr(item, colDefs, tableName, scope, false); err != nil {
				return err
			}
		}
		return nil
	default:
		// Literals (NumericLit, StringLit, NullLit, BlobLit) have no columns.
		return nil
	}
}

// validateReturningSelect walks a subquery's expressions. The subquery's FROM
// tables are added to the visible scope (a qualifier may name any of them,
// and an unqualified reference may resolve to any of their columns or to an
// outer column of the modified table).
func (e *Engine) validateReturningSelect(s *sql.SelectStmt, colDefs []sql.ColumnDef, tableName string, outer []returningScope) error {
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
	if err := e.validateReturningExpr(s.Where, colDefs, tableName, scope, false); err != nil {
		return err
	}
	for _, g := range s.GroupBy {
		if err := e.validateReturningExpr(g, colDefs, tableName, scope, false); err != nil {
			return err
		}
	}
	if err := e.validateReturningExpr(s.Having, colDefs, tableName, scope, false); err != nil {
		return err
	}
	for _, o := range s.OrderBy {
		if err := e.validateReturningExpr(o.Expr, colDefs, tableName, scope, false); err != nil {
			return err
		}
	}
	if err := e.validateReturningExpr(s.Limit, colDefs, tableName, scope, false); err != nil {
		return err
	}
	if err := e.validateReturningExpr(s.Offset, colDefs, tableName, scope, false); err != nil {
		return err
	}
	if s.Union != nil {
		if err := e.validateReturningSelect(s.Union, colDefs, tableName, outer); err != nil {
			return err
		}
	}
	return nil
}

// appendReturningFromScope adds the qualifier names of a FROM table reference
// (alias if present, otherwise the table name) to the visible scope, mapping
// the qualifier to the real table for column lookups.
func (e *Engine) appendReturningFromScope(scope []returningScope, ref *sql.TableRef) []returningScope {
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
func (e *Engine) validateReturningColumnRef(v *sql.ColumnRef, colDefs []sql.ColumnDef, tableName string, scope []returningScope, topLevel bool) error {
	if v.Name == "*" {
		if topLevel && v.Table != "" {
			return fmt.Errorf("RETURNING may not use \"TABLE.*\" wildcards")
		}
		return nil
	}
	if v.Table == "" {
		// Unqualified: TRUE/FALSE, rowid aliases, a column of the modified
		// table, or (inside a subquery) a column of an in-scope table.
		if strings.EqualFold(v.Name, "TRUE") || strings.EqualFold(v.Name, "FALSE") {
			return nil
		}
		if isRowIDName(v.Name) {
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
	// Qualified reference.
	if strings.EqualFold(v.Table, tableName) {
		if isRowIDName(v.Name) {
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
func (e *Engine) returningTableHasColumn(tableName, name string) bool {
	entry, err := e.schema.FindTable(tableName)
	if err != nil {
		return true
	}
	cols := e.parseColumnDefs(entry.Name, entry.SQL)
	for _, cd := range cols {
		if strings.EqualFold(cd.Name, name) {
			return true
		}
	}
	return false
}
