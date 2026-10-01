package exec

import (
	"math"
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
// deviation — an unknown node kind, a slot shape the value cannot reproduce
// as a fresh parse would (folded unary-minus literals, hex, non-finite or
// boundary REAL values), a value/count mismatch — aborts the clone
// (ok=false) and the caller falls back to a full parse, so a substituted AST
// is always identical to a fresh parse of the statement text.
// (Statement-level walkers live in template_clone_stmt.go.)

// cloneStmtsValues substitutes values into every statement of a cached
// template. It returns (nil, false) when the template cannot serve the
// values and a re-parse is required instead.
func cloneStmtsValues(stmts []sql.Stmt, values []interface{}) ([]sql.Stmt, bool) {
	out := make([]sql.Stmt, len(stmts))
	c := exprClone{values: values}
	for i, stmt := range stmts {
		cloned, ok := c.stmt(stmt)
		if !ok {
			return nil, false
		}
		out[i] = cloned
	}
	if c.idx != len(values) {
		return nil, false
	}
	return out, true
}

// stmt substitutes values into one template statement. Only the statement
// families the template cache stores are handled; anything else refuses the
// clone (a full parse keeps results identical).
func (c *exprClone) stmt(stmt sql.Stmt) (sql.Stmt, bool) {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		return c.insertStmt(s)
	case *sql.SelectStmt:
		return c.selectCOW(s)
	case *sql.UpdateStmt:
		return c.updateCOW(s)
	case *sql.DeleteStmt:
		return c.deleteCOW(s)
	}
	return nil, false
}

// insertStmt adapts the INSERT clone to the shared contract: the INSERT
// clone consumes template values through the shared index, so a count
// mismatch surfaces in cloneStmtsValues. The walker state carries the index
// and the bind state itself; a refused INSERT leaves the index untouched for
// the caller's rollback.
func (c *exprClone) insertStmt(s *sql.InsertStmt) (sql.Stmt, bool) {
	saved := c.idx
	cloned, err := c.insertStmtValues(s)
	if err != nil {
		c.idx = saved
		return nil, false
	}
	return cloned, true
}

// exprClone is the copy-on-write substitution state for one statement.
// It serves two modes, never both at once:
//   - template mode (values set): substitute normalized template-cache
//     values for NumericLit/StringLit slots in walk order.
//   - bind mode (bind set): substitute prepared-statement bound values for
//     sql.ParameterExpr markers (see bind.go); statement literals are
//     immutable text and are shared unchanged.
type exprClone struct {
	values     []interface{}
	idx        int
	bind       *BindPlan     // bind mode: occurrence→slot plan (nil in template mode)
	bindValues []interface{} // bind mode: slot→value table
	bindOccI   int           // bind mode: next occurrence index
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
	if c.bind != nil {
		return c.bindExpr(e)
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

// bindExpr walks expressions in bind mode: the statement's own literals are
// immutable text shared verbatim (blobs and RAISE included), parameter
// markers substitute through the bind plan, and every operator shape
// recurses through the same walker as the template mode. Unknown kinds
// refuse the clone (the caller falls back to the rendered-SQL path).
func (c *exprClone) bindExpr(e sql.Expr) (sql.Expr, bool) {
	switch v := e.(type) {
	case *sql.ParameterExpr:
		return c.bindParam(v)
	case *sql.NumericLit, *sql.StringLit, *sql.NullLit, *sql.ColumnRef, *sql.BlobLit, *sql.RaiseExpr:
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
	}
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
// of the same kind: an integer value into a decimal-integer slot, a REAL
// value into a REAL slot ('.0' kept), a quoted-string value into any slot.
// The gate exists because one normalized key can serve statements whose
// parsed ASTs differ in shape, and the substitution must reproduce exactly
// what a fresh parse of the current statement text produces:
//
//   - The parser folds the unary minus of a 2^63-magnitude decimal literal
//     into the literal itself (rule 216, expr.c sqlite3ExprCodeInteger):
//     "-9223372036854775808" becomes a single NumericLit, NOT UnaryOp{'-'}.
//     Hex literals fold the same way ("-0x…"). A folded slot's text starts
//     with '-' and never passes the slot-shape checks below, so any template
//     whose statement folded refuses substitution and full-parses.
//   - normalizeSQL extracts the UNSIGNED magnitude (the scan starts at the
//     digits), and a 2^63-magnitude digit run extracts as float64 (int64
//     overflow) — the very same float64 a "….0" spelling yields. A fresh
//     parse of that text folds only for the integer spelling, so the exact
//     double 2^63 is shape-ambiguous and refuses.
//   - Hex spellings extract lossily (value 0, residual "x…" in the key);
//     their slot text carries 'x'/'X' and never qualifies (hex digits
//     include 'E', so a naive exponent check would admit "0xE8").
func (c *exprClone) numeric(v *sql.NumericLit) (sql.Expr, bool) {
	val, ok := c.next()
	if !ok {
		return nil, false
	}
	switch n := val.(type) {
	case int64:
		// A fresh parse of the current text yields NumericLit{D} where D is
		// an int64-range digit run (no fold is possible at these magnitudes);
		// the canonical rendering has the same value and kind, leading zeros
		// included (SQLite numerals are decimal, never octal).
		if !isDecimalSlot(v.Value) {
			return nil, false
		}
		return &sql.NumericLit{Value: int64Text(n)}, true
	case float64:
		// Slot must be a decimal REAL spelling; the value must be finite and
		// not the ambiguous 2^63 double (see above).
		if !isRealSlot(v.Value) || math.IsInf(n, 0) || math.IsNaN(n) || n == twoPow63 {
			return nil, false
		}
		return &sql.NumericLit{Value: floatSlotText(n)}, true
	case string:
		// The slot is spelled with a quoted literal this time; the fresh
		// parse of that text carries a StringLit.
		return &sql.StringLit{Value: n}, true
	}
	return nil, false
}

// next consumes the next cached value.
func (c *exprClone) next() (interface{}, bool) {
	if c.idx >= len(c.values) {
		return nil, false
	}
	val := c.values[c.idx]
	c.idx++
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

// returning substitutes a RETURNING clause (a single SelectColumn). A nil
// Expr is a statement without RETURNING — nothing to substitute (the
// historical walk refused here, silently disabling the template cache for
// every plain UPDATE/DELETE).
func (c *exprClone) returning(col sql.SelectColumn) (sql.SelectColumn, bool, bool) {
	if col.Expr == nil {
		return col, false, true
	}
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

// twoPow63 is 2^63 as a float64: the one numeric value whose normalized
// spelling is ambiguous between a unary-minus integer literal the parser
// FOLDS ("-9223372036854775808") and a plain REAL spelling
// ("-9223372036854775808.0"). Substitution refuses it.
const twoPow63 = float64(1 << 63)

// floatSlotText renders a float template value canonically, restoring the
// decimal point 'g' formatting drops so the slot stays REAL (5.0, not 5).
func floatSlotText(v float64) string {
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0" // keep the REAL kind
	}
	return s
}

// isDecimalSlot reports whether text is a plain decimal integer spelling —
// digits only. Signed ("-…", the parser's folded forms), hex ("0x…"), and
// REAL ("…./…e…") slot texts all contain a non-digit and refuse.
func isDecimalSlot(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// isRealSlot reports whether text is a decimal REAL spelling (contains '.'
// or an exponent, never 'x'/'X'). Hex digits include 'E', so the exponent
// check alone would admit a hex slot like "0xE8"; hex is excluded first.
func isRealSlot(text string) bool {
	return !strings.ContainsAny(text, "xX") && strings.ContainsAny(text, ".eE")
}
