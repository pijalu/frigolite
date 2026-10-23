package execddl

import "github.com/pijalu/frigolite/internal/sql"

// validateDetachExprSources resolves subquery FROM sources before DETACH
// evaluates scalar expressions. SQLite prepares subqueries (and reports missing
// tables) before runtime-only function errors such as RAISE outside triggers.
func validateDetachExprSources(ctx DDLContext, expr sql.Expr) error {
	switch e := expr.(type) {
	case *sql.Subquery:
		return validateDetachSelectSources(ctx, e.Select)
	case *sql.BinaryOp:
		if err := validateDetachExprSources(ctx, e.Left); err != nil {
			return err
		}
		return validateDetachExprSources(ctx, e.Right)
	case *sql.UnaryOp:
		return validateDetachExprSources(ctx, e.Operand)
	case *sql.ParenExpr:
		return validateDetachExprSources(ctx, e.Expr)
	case *sql.FuncCall:
		for _, arg := range e.Args {
			if err := validateDetachExprSources(ctx, arg); err != nil {
				return err
			}
		}
	case *sql.InList:
		if err := validateDetachExprSources(ctx, e.Operand); err != nil {
			return err
		}
		for _, item := range e.List {
			if err := validateDetachExprSources(ctx, item); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDetachSelectSources(ctx DDLContext, sel *sql.SelectStmt) error {
	if sel == nil {
		return nil
	}
	if err := validateDetachTableRef(ctx, sel.From); err != nil {
		return err
	}
	for _, column := range sel.Columns {
		if ref, ok := column.Expr.(*sql.ColumnRef); ok && ref.Table != "" && ref.Name == "*" {
			if _, _, err := ctx.FindTable(ref.Table); err != nil {
				return err
			}
		}
	}
	for _, join := range sel.Joins {
		if err := validateDetachTableRef(ctx, join.Table); err != nil {
			return err
		}
		if err := validateDetachExprSources(ctx, join.On); err != nil {
			return err
		}
	}
	return validateDetachSelectSources(ctx, sel.Union)
}

func validateDetachTableRef(ctx DDLContext, ref sql.TableRef) error {
	if ref.Subquery != nil {
		return validateDetachSelectSources(ctx, ref.Subquery)
	}
	if ref.Name != "" {
		if _, _, err := ctx.FindTable(ref.Name); err != nil {
			return err
		}
	}
	return nil
}
