package exec

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// evalExpr is the engine's expression evaluation entry point. The actual
// evaluation tree lives in the execexpr.Evaluator; this method delegates to
// it so call sites throughout the engine keep a single entry point.
func (e *Engine) evalExpr(expr sql.Expr, row Row) (interface{}, error) {
	return e.expr.Eval(expr, row)
}

// evalFuncCall evaluates a function call expression.
func (e *Engine) evalFuncCall(f *sql.FuncCall, row Row) (interface{}, error) {
	return e.expr.EvalFuncCall(f, row)
}

// evalBool evaluates an expression as a boolean predicate (WHERE, ON, CHECK).
func (e *Engine) evalBool(expr sql.Expr, row Row) (bool, error) {
	return e.expr.EvalBool(expr, row)
}

// evalSubquery evaluates a scalar subquery expression.
func (e *Engine) evalSubquery(v *sql.Subquery, row Row) (interface{}, error) {
	return e.expr.EvalSubquery(v, row)
}
