package execquery

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

// validateExprOrderBy validates that ORDER BY is only used inside aggregate
// functions, and that nested aggregates are not misused. It recurses through
// all sub-expressions.
func (e *SelectEngine) validateExprOrderBy(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return e.validateOrderByFuncExpr(v)
	case *sql.BinaryOp:
		return e.validateBinaryOrderBy(v)
	case *sql.UnaryOp:
		return e.validateExprOrderBy(v.Operand)
	case *sql.CaseExpr:
		return e.validateCaseOrderBy(v)
	case *sql.Subquery:
		return e.validateSubqueryInnerSelect(v)
	case *sql.ExistsExpr:
		return e.validateSubqueryInnerSelect(&sql.Subquery{Select: v.Select})
	}
	return nil
}

// validateOrderByFuncExpr validates a function call's ORDER BY usage: only
// aggregate functions may have ORDER BY, and nested aggregates are checked.
func (e *SelectEngine) validateOrderByFuncExpr(v *sql.FuncCall) error {
	if len(v.OrderBy) > 0 {
		fn, ok := e.ctx.Functions().Find(v.Name)
		if ok && fn.Type != function.TypeAggregate {
			return fmt.Errorf("ORDER BY may not be used with non-aggregate %s()", v.Name)
		}
		for _, ob := range v.OrderBy {
			if nested := findNestedAggregate(ob.Expr, e.ctx.Functions()); nested != "" {
				return fmt.Errorf("misuse of aggregate function %s()", nested)
			}
		}
	}
	for _, arg := range v.Args {
		if err := e.validateExprOrderBy(arg); err != nil {
			return err
		}
	}
	return nil
}

// validateBinaryOrderBy validates both sides of a binary operation for ORDER BY
// usage.
func (e *SelectEngine) validateBinaryOrderBy(v *sql.BinaryOp) error {
	if err := e.validateExprOrderBy(v.Left); err != nil {
		return err
	}
	return e.validateExprOrderBy(v.Right)
}

// validateCaseOrderBy validates a CASE expression's sub-expressions for ORDER BY
// usage.
