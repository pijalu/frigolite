package exec

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Template-cache value substitution, copy-on-write: only the statement root
// and the expression chain leading to each substituted literal are cloned;
// every other node is shared with the template. Sharing is safe because
// statement execution treats AST nodes as immutable — the exact-text
// statement cache has always re-executed one parsed AST, and the exec-side
// mutators (window substitution, predicate pushdown) documented in execquery
// and execdml operate on their own copies.
//
// The walk consumes one cached value per literal slot in source order. ANY
// deviation — an unknown node kind, a literal whose cached text is not the
// canonical spelling of its normalized value, a value/count mismatch —
// aborts the clone (ok=false) and the caller falls back to a full parse, so
// a substituted AST is always identical to a fresh parse of the statement
// text. (Statement-level walkers live in template_clone_stmt.go.)

// cloneStmtsValues substitutes values into every statement of a cached
// template. It returns (nil, false) when the template cannot serve the
// values and a re-parse is required instead.
func cloneStmtsValues(stmts []sql.Stmt, values []interface{}) ([]sql.Stmt, bool) {
	out := make([]sql.Stmt, len(stmts))
	idx := 0
	for i, stmt := range stmts {
		cloned, ok := cloneStmtValues(stmt, values, &idx)
		if !ok {
			return nil, false
		}
		out[i] = cloned
	}
	if idx != len(values) {
		return nil, false
	}
	return out, true
}

// cloneStmtValues substitutes values into one template statement. Only the
// statement families the template cache stores are handled; anything else
// refuses the clone (a full parse keeps results identical).
func cloneStmtValues(stmt sql.Stmt, values []interface{}, idx *int) (sql.Stmt, bool) {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		return cloneInsertStmtForTemplate(s, values, idx)
	case *sql.SelectStmt:
		return cloneSelectCOW(s, values, idx)
	case *sql.UpdateStmt:
		return cloneUpdateCOW(s, values, idx)
	case *sql.DeleteStmt:
		return cloneDeleteCOW(s, values, idx)
	}
	return nil, false
}

// cloneInsertStmtForTemplate adapts the INSERT clone to the shared contract:
// cloneInsertStmt consumes tuple values and reports leftovers through the
// shared index, so a count mismatch surfaces in cloneStmtsValues.
func cloneInsertStmtForTemplate(s *sql.InsertStmt, values []interface{}, idx *int) (sql.Stmt, bool) {
	saved := *idx
	cloned, err := cloneInsertStmt(s, values, idx)
	if err != nil {
		*idx = saved
		return nil, false
	}
	return cloned, true
}

// exprClone is the copy-on-write substitution state for one statement.
type exprClone struct {
	values []interface{}
	idx    *int
}

// expr substitutes cached values below e, returning the substituted
// expression and whether the shape is template-safe. A substituted result is
// the SHARED node when no literal below it changed (compared by identity),
// or a fresh copy when one did — literal nodes are always fresh so the
// template's NumericLit value cache is never shared across statements.
func (c *exprClone) expr(e sql.Expr) (_ sql.Expr, ok bool) {
	if e == nil {
		return nil, false
	}
	switch v := e.(type) {
	case *sql.NumericLit:
		return c.numeric(v)
	case *sql.StringLit:
		return c.anyLiteral(v.Value)
	case *sql.NullLit, *sql.ColumnRef, *sql.ParameterExpr:
		return e, true
	case *sql.FuncCall:
		return c.funcCall(v)
	case *sql.CaseExpr:
		return c.caseExpr(v)
	case *sql.BinaryOp, *sql.UnaryOp, *sql.ParenExpr, *sql.Between, *sql.InList, *sql.RowValue, *sql.CastExpr:
		return c.exprOperator(e)
	case *sql.IsNull, *sql.IsNotNull, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom, *sql.IsTrue, *sql.IsFalse:
		return c.exprPredicate(e)
	case *sql.Subquery, *sql.ExistsExpr:
		return c.exprNested(e)
	case *sql.BlobLit, *sql.RaiseExpr:
		// Blob literals are not reconstructible from the normalized string
		// value (hex decode is ambiguous with text); RAISE belongs to
		// trigger programs. Both refuse the template.
		return nil, false
	}
	// Unknown expression kind: refuse the clone (a full parse keeps the
	// result identical).
	return nil, false
}

// exprOperator dispatches operator-shaped expressions.
func (c *exprClone) exprOperator(e sql.Expr) (sql.Expr, bool) {
	switch v := e.(type) {
	case *sql.BinaryOp:
		return c.binaryOp(v)
	case *sql.UnaryOp:
		return c.unaryOp(v)
	case *sql.ParenExpr:
		return c.cloneUnary(v.Expr, func(o sql.Expr) sql.Expr { return &sql.ParenExpr{Expr: o} })
	case *sql.Between:
		return c.between(v)
	case *sql.InList:
		return c.inList(v)
	case *sql.RowValue:
		return c.rowValue(v)
	case *sql.CastExpr:
		return c.cloneUnary(v.Operand, func(o sql.Expr) sql.Expr {
			return &sql.CastExpr{Operand: o, AsType: v.AsType}
		})
	}
	return nil, false
}

// exprPredicate dispatches IS / DISTINCT FROM / truth-value expressions.
func (c *exprClone) exprPredicate(e sql.Expr) (sql.Expr, bool) {
	switch v := e.(type) {
	case *sql.IsNull:
		return c.cloneUnary(v.Operand, func(o sql.Expr) sql.Expr { return &sql.IsNull{Operand: o} })
	case *sql.IsNotNull:
		return c.cloneUnary(v.Operand, func(o sql.Expr) sql.Expr { return &sql.IsNotNull{Operand: o} })
	case *sql.IsDistinctFrom:
		return c.cloneBinary(v.Left, v.Right, func(l, r sql.Expr) sql.Expr {
			return &sql.IsDistinctFrom{Left: l, Right: r}
		})
	case *sql.IsNotDistinctFrom:
		return c.cloneBinary(v.Left, v.Right, func(l, r sql.Expr) sql.Expr {
			return &sql.IsNotDistinctFrom{Left: l, Right: r}
		})
	case *sql.IsTrue:
		return c.cloneUnaryFlag(v.Operand, v.Negated, func(o sql.Expr, neg bool) sql.Expr {
			return &sql.IsTrue{Operand: o, Negated: neg}
		})
	case *sql.IsFalse:
		return c.cloneUnaryFlag(v.Operand, v.Negated, func(o sql.Expr, neg bool) sql.Expr {
			return &sql.IsFalse{Operand: o, Negated: neg}
		})
	}
	return nil, false
}

// exprNested dispatches expression-wrapped subqueries.
func (c *exprClone) exprNested(e sql.Expr) (sql.Expr, bool) {
	switch v := e.(type) {
	case *sql.Subquery:
		cloned, _, ok := c.selectStmt(v.Select)
		if !ok {
			return nil, false
		}
		return &sql.Subquery{Select: cloned}, true
	case *sql.ExistsExpr:
		cloned, _, ok := c.selectStmt(v.Select)
		if !ok {
			return nil, false
		}
		return &sql.ExistsExpr{Select: cloned, Negated: v.Negated}, true
	}
	return nil, false
}

// binaryOp substitutes a binary operator, preserving the LIKE-optimization
// metadata the planner may attach.
func (c *exprClone) binaryOp(v *sql.BinaryOp) (sql.Expr, bool) {
	left, ok := c.expr(v.Left)
	if !ok {
		return nil, false
	}
	right, ok := c.expr(v.Right)
	if !ok {
		return nil, false
	}
	if left == v.Left && right == v.Right {
		return v, true
	}
	return &sql.BinaryOp{
		Left: left, Right: right, Operator: v.Operator,
		Escape: v.Escape, HasEscape: v.HasEscape, LikeRange: v.LikeRange,
	}, true
}

// unaryOp substitutes a unary operator.
func (c *exprClone) unaryOp(v *sql.UnaryOp) (sql.Expr, bool) {
	operand, ok := c.expr(v.Operand)
	if !ok {
		return nil, false
	}
	if operand == v.Operand {
		return v, true
	}
	return &sql.UnaryOp{Operand: operand, Operator: v.Operator}, true
}

// between substitutes a BETWEEN expression.
func (c *exprClone) between(v *sql.Between) (sql.Expr, bool) {
	operand, ok := c.expr(v.Operand)
	if !ok {
		return nil, false
	}
	low, ok := c.expr(v.Low)
	if !ok {
		return nil, false
	}
	high, ok := c.expr(v.High)
	if !ok {
		return nil, false
	}
	if operand == v.Operand && low == v.Low && high == v.High {
		return v, true
	}
	return &sql.Between{Operand: operand, Low: low, High: high, Negated: v.Negated}, true
}

// inList substitutes an IN (list) expression.
func (c *exprClone) inList(v *sql.InList) (sql.Expr, bool) {
	operand, ok := c.expr(v.Operand)
	if !ok {
		return nil, false
	}
	list, changed, ok := c.exprList(v.List)
	if !ok {
		return nil, false
	}
	if !changed && operand == v.Operand {
		return v, true
	}
	return &sql.InList{Operand: operand, List: list, Negated: v.Negated}, true
}

// rowValue substitutes a row-value tuple.
func (c *exprClone) rowValue(v *sql.RowValue) (sql.Expr, bool) {
	values, changed, ok := c.exprList(v.Values)
	if !ok {
		return nil, false
	}
	if !changed {
		return v, true
	}
	return &sql.RowValue{Values: values}, true
}

// anyLiteral substitutes a literal slot by cached-value type: the fresh node
// kind follows the value (a template slot first seen with a quoted literal
// may serve a numeric statement sharing the normalized shape, and vice
// versa), matching what a fresh parse of the statement text produces. Float
// values refuse the substitution — a quoted slot has no numeric text to
// verify the canonical spelling against (5.0 must not degrade to 5).
func (c *exprClone) anyLiteral(original string) (sql.Expr, bool) {
	val, ok := c.next()
	if !ok {
		return nil, false
	}
	switch n := val.(type) {
	case string:
		return &sql.StringLit{Value: n}, true
	case int64:
		return &sql.NumericLit{Value: int64Text(n)}, true
	}
	return nil, false
}

// numeric substitutes a numeric literal slot. SQLite distinguishes integer
// from REAL literals (typeof/quote expose it), so a slot only serves values
// of the same kind: an integer-shaped slot's text must have no '.'/'e' and a
// float-shaped slot's text must be the canonical spelling of the value
// (guards hex "0x1F", which normalizes to the value 0, and "1.50" vs "1.5").
func (c *exprClone) numeric(v *sql.NumericLit) (sql.Expr, bool) {
	val, ok := c.next()
	if !ok {
		return nil, false
	}
	slotIsFloat := strings.ContainsAny(v.Value, ".eE")
	switch n := val.(type) {
	case int64:
		if slotIsFloat {
			return nil, false
		}
		return c.numericValue(int64Text(n), v.Value)
	case float64:
		if !slotIsFloat {
			return nil, false
		}
		return c.numericValue(floatText(n), v.Value)
	case string:
		// The slot is spelled with a quoted literal this time; the fresh
		// parse of that text carries a StringLit.
		return &sql.StringLit{Value: n}, true
	}
	return nil, false
}

// numericValue validates that the slot's original text is the canonical
// spelling of the new value before building the fresh literal.
func (c *exprClone) numericValue(canonical, original string) (sql.Expr, bool) {
	if canonical != original {
		return nil, false
	}
	return &sql.NumericLit{Value: original}, true
}

// next consumes the next cached value.
func (c *exprClone) next() (interface{}, bool) {
	if *c.idx >= len(c.values) {
		return nil, false
	}
	val := c.values[*c.idx]
	*c.idx++
	return val, true
}

// cloneUnary clones a single-operand expression.
func (c *exprClone) cloneUnary(operand sql.Expr, build func(sql.Expr) sql.Expr) (sql.Expr, bool) {
	o, ok := c.expr(operand)
	if !ok {
		return nil, false
	}
	return build(o), true
}

// cloneUnaryFlag clones an operand-plus-flag expression.
func (c *exprClone) cloneUnaryFlag(operand sql.Expr, negated bool, build func(sql.Expr, bool) sql.Expr) (sql.Expr, bool) {
	return c.cloneUnary(operand, func(o sql.Expr) sql.Expr { return build(o, negated) })
}

// cloneBinary clones a two-operand expression.
func (c *exprClone) cloneBinary(left, right sql.Expr, build func(sql.Expr, sql.Expr) sql.Expr) (sql.Expr, bool) {
	l, ok := c.expr(left)
	if !ok {
		return nil, false
	}
	r, ok := c.expr(right)
	if !ok {
		return nil, false
	}
	return build(l, r), true
}

// exprList substitutes an expression slice. A changed slice gets a fresh
// backing array; an unchanged one is shared.
func (c *exprClone) exprList(list []sql.Expr) (_ []sql.Expr, changed, ok bool) {
	var out []sql.Expr
	for i, item := range list {
		cloned, itemOK := c.expr(item)
		if !itemOK {
			return nil, false, false
		}
		if cloned != item {
			changed = true
			if out == nil {
				out = make([]sql.Expr, len(list))
				copy(out, list)
			}
		}
		if out != nil {
			out[i] = cloned
		}
	}
	if !changed {
		return list, false, true
	}
	return out, true, true
}

// funcCall substitutes a function call's arguments, FILTER, and ORDER BY.
func (c *exprClone) funcCall(v *sql.FuncCall) (sql.Expr, bool) {
	args, argsChanged, ok := c.exprList(v.Args)
	if !ok {
		return nil, false
	}
	filter := v.Filter
	filterChanged := false
	if v.Filter != nil {
		var fok bool
		filter, fok = c.expr(v.Filter)
		if !fok {
			return nil, false
		}
		filterChanged = filter != v.Filter
	}
	orderBy, obChanged, ok := c.orderBy(v.OrderBy)
	if !ok {
		return nil, false
	}
	if !argsChanged && !filterChanged && !obChanged {
		return v, true
	}
	return &sql.FuncCall{
		Name:     v.Name,
		Args:     args,
		Distinct: v.Distinct,
		OrderBy:  orderBy,
		Filter:   filter,
		Over:     v.Over, // window context must survive substitution (window1: ntile('zbc') OVER ...)
	}, true
}

// caseExpr substitutes a CASE expression.
func (c *exprClone) caseExpr(v *sql.CaseExpr) (sql.Expr, bool) {
	operand := v.Operand
	if operand != nil {
		var ok bool
		operand, ok = c.expr(operand)
		if !ok {
			return nil, false
		}
	}
	whens, whensChanged, ok := c.caseWhens(v.Whens)
	if !ok {
		return nil, false
	}
	els := v.Else
	if els != nil {
		var ok bool
		els, ok = c.expr(els)
		if !ok {
			return nil, false
		}
	}
	if operand == v.Operand && !whensChanged && els == v.Else {
		return v, true
	}
	return &sql.CaseExpr{Operand: operand, Whens: whens, Else: els}, true
}

// caseWhens substitutes a CASE expression's WHEN arms.
func (c *exprClone) caseWhens(whens []sql.WhenClause) (_ []sql.WhenClause, changed, ok bool) {
	var out []sql.WhenClause
	for i, w := range whens {
		when, wok := c.expr(w.When)
		if !wok {
			return nil, false, false
		}
		then, tok := c.expr(w.Then)
		if !tok {
			return nil, false, false
		}
		if when != w.When || then != w.Then {
			changed = true
			if out == nil {
				out = make([]sql.WhenClause, len(whens))
				copy(out, whens)
			}
		}
		if out != nil {
			out[i] = sql.WhenClause{When: when, Then: then}
		}
	}
	if !changed {
		return whens, false, true
	}
	return out, true, true
}

// orderBy substitutes ORDER BY / PARTITION BY term lists.
func (c *exprClone) orderBy(terms []sql.OrderByTerm) (_ []sql.OrderByTerm, changed, ok bool) {
	var out []sql.OrderByTerm
	for i, t := range terms {
		cloned, tok := c.expr(t.Expr)
		if !tok {
			return nil, false, false
		}
		if cloned != t.Expr {
			changed = true
			if out == nil {
				out = make([]sql.OrderByTerm, len(terms))
				copy(out, terms)
			}
			out[i].Expr = cloned
		}
	}
	if !changed {
		return terms, false, true
	}
	return out, true, true
}

// returning substitutes a RETURNING clause (a single SelectColumn).
func (c *exprClone) returning(col sql.SelectColumn) (sql.SelectColumn, bool, bool) {
	cloned, ok := c.expr(col.Expr)
	if !ok {
		return col, false, false
	}
	if cloned == col.Expr {
		return col, false, true
	}
	col.Expr = cloned
	return col, true, true
}

// assignments substitutes UPDATE SET values.
func (c *exprClone) assignments(assigns []sql.Assignment) (_ []sql.Assignment, changed, ok bool) {
	var out []sql.Assignment
	for i, a := range assigns {
		cloned, aok := c.expr(a.Value)
		if !aok {
			return nil, false, false
		}
		if cloned != a.Value {
			changed = true
			if out == nil {
				out = make([]sql.Assignment, len(assigns))
				copy(out, assigns)
			}
			out[i].Value = cloned
		}
	}
	if !changed {
		return assigns, false, true
	}
	return out, true, true
}

// int64Text renders an integer template value canonically.
func int64Text(v int64) string { return strconv.FormatInt(v, 10) }

// floatText renders a float template value canonically.
func floatText(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
