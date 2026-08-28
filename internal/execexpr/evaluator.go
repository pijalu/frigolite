package execexpr

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// Eval evaluates an expression against a row. It is the public entry point
// the execution engine delegates to.
func (ev *Evaluator) Eval(expr sql.Expr, row Row) (interface{}, error) {
	return ev.evalExpr(expr, row)
}

// EvalExprWithCollation evaluates an expression and applies the compile-time
// collation propagation (SQLite sqlite3ExprCollSeq): when the expression's
// subtree contains an explicit COLLATE that propagates up through a function
// call, CASE, or ||, the result is wrapped in an explicit CollatedValue so an
// enclosing comparison uses that collation (collate8 semantics).
func (ev *Evaluator) EvalExprWithCollation(expr sql.Expr, row Row) (interface{}, error) {
	return ev.evalExprWithCollation(expr, row)
}

// EvalRowValueExpr evaluates each element of a row value into a slice.
func (ev *Evaluator) EvalRowValueExpr(v *sql.RowValue, row Row) (interface{}, error) {
	return ev.evalRowValueExpr(v, row)
}

// EvalFuncCall evaluates a function call expression.
func (ev *Evaluator) EvalFuncCall(f *sql.FuncCall, row Row) (interface{}, error) {
	return ev.evalFuncCall(f, row)
}

// EvalBool evaluates an expression as a boolean predicate (WHERE, ON, CHECK).
func (ev *Evaluator) EvalBool(expr sql.Expr, row Row) (bool, error) {
	return ev.evalBool(expr, row)
}

// EvalSubquery evaluates a scalar subquery expression.
func (ev *Evaluator) EvalSubquery(v *sql.Subquery, row Row) (interface{}, error) {
	return ev.evalSubquery(v, row)
}

// EvalSubqueryRows executes a subquery and returns all result rows.
func (ev *Evaluator) EvalSubqueryRows(subq *sql.Subquery, row Row) ([][]interface{}, error) {
	return ev.evalSubqueryRows(subq, row)
}

// EvalBinaryOpValues evaluates a binary operator over two pre-evaluated
// scalar values (used by HAVING evaluation over aggregate rows).
func (ev *Evaluator) EvalBinaryOpValues(op string, left, right interface{}) (interface{}, error) {
	return ev.evalBinaryOpValues(op, left, right)
}
