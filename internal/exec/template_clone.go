package exec

import (
	"strconv"

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
// text.

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
	switch v := e.(type) {
	case *sql.NumericLit:
		return c.numeric(v)
	case *sql.StringLit:
		return c.anyLiteral(v.Value)
	case *sql.NullLit, *sql.ColumnRef, *sql.ParameterExpr:
		return e, true
	case *sql.ParenExpr:
		inner, ok := c.expr(v.Expr)
		if !ok {
			return nil, false
		}
		if inner == v.Expr {
			return e, true
		}
		return &sql.ParenExpr{Expr: inner}, true
	case *sql.UnaryOp:
		operand, ok := c.expr(v.Operand)
		if !ok {
			return nil, false
		}
		if operand == v.Operand {
			return e, true
		}
		return &sql.UnaryOp{Operand: operand, Operator: v.Operator}, true
	case *sql.BinaryOp:
		left, ok := c.expr(v.Left)
		if !ok {
			return nil, false
		}
		right, ok := c.expr(v.Right)
		if !ok {
			return nil, false
		}
		if left == v.Left && right == v.Right {
			return e, true
		}
		return &sql.BinaryOp{
			Left: left, Right: right, Operator: v.Operator,
			Escape: v.Escape, HasEscape: v.HasEscape, LikeRange: v.LikeRange,
		}, true
	case *sql.IsNull:
		return c.cloneUnary(v.Operand, func(o sql.Expr) sql.Expr {
			return &sql.IsNull{Operand: o}
		})
	case *sql.IsNotNull:
		return c.cloneUnary(v.Operand, func(o sql.Expr) sql.Expr {
			return &sql.IsNotNull{Operand: o}
		})
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
	case *sql.Between:
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
			return e, true
		}
		return &sql.Between{Operand: operand, Low: low, High: high, Negated: v.Negated}, true
	case *sql.InList:
		operand, ok := c.expr(v.Operand)
		if !ok {
			return nil, false
		}
		list, changed, ok := c.exprList(v.List)
		if !ok {
			return nil, false
		}
		if !changed && operand == v.Operand {
			return e, true
		}
		return &sql.InList{Operand: operand, List: list, Negated: v.Negated}, true
	case *sql.RowValue:
		values, changed, ok := c.exprList(v.Values)
		if !ok {
			return nil, false
		}
		if !changed {
			return e, true
		}
		return &sql.RowValue{Values: values}, true
	case *sql.FuncCall:
		return c.funcCall(v)
	case *sql.CastExpr:
		return c.cloneUnary(v.Operand, func(o sql.Expr) sql.Expr {
			return &sql.CastExpr{Operand: o, AsType: v.AsType}
		})
	case *sql.CaseExpr:
		return c.caseExpr(v)
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

// anyLiteral substitutes a literal slot by cached-value type: the fresh node
// kind follows the value (a template slot first seen with a quoted literal
// may serve a numeric statement sharing the normalized shape, and vice
// versa), matching what a fresh parse of the statement text produces.
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
	case float64:
		return &sql.NumericLit{Value: floatText(n)}, true
	}
	return nil, false
}

// numeric substitutes a numeric literal slot. The node's cached text must be
// the canonical spelling of the normalized value; this guards hex literals
// ("0x1F" normalizes to the value 0), underscore separators, and other
// spellings whose text diverges from what the substitution would write.
func (c *exprClone) numeric(v *sql.NumericLit) (sql.Expr, bool) {
	val, ok := c.next()
	if !ok {
		return nil, false
	}
	switch n := val.(type) {
	case int64:
		return c.numericValue(int64Text(n), v.Value)
	case float64:
		return c.numericValue(floatText(n), v.Value)
	case string:
		// The slot is spelled with a quoted literal this time.
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
	if o == operand {
		return build(o), true
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
	whens := v.Whens
	whensChanged := false
	if len(v.Whens) > 0 {
		whens = make([]sql.WhenClause, len(v.Whens))
		for i, w := range v.Whens {
			when, wok := c.expr(w.When)
			if !wok {
				return nil, false
			}
			then, tok := c.expr(w.Then)
			if !tok {
				return nil, false
			}
			whens[i] = sql.WhenClause{When: when, Then: then}
			if when != w.When || then != w.Then {
				whensChanged = true
			}
		}
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

// selectStmt substitutes a (possibly nested) SELECT. It returns the shared
// statement when nothing under it changed.
//
// The field walk order MUST match the statement's source order (WITH first,
// then select list, FROM, WHERE, GROUP BY, HAVING, WINDOW, ORDER BY, LIMIT,
// OFFSET, compound tail): normalized values are consumed in text order, so a
// walk that visits fields out of order would substitute the wrong values
// into matching-shaped statements (the value COUNT would still line up and
// nothing would bail).
func (c *exprClone) selectStmt(sel *sql.SelectStmt) (*sql.SelectStmt, bool, bool) {
	if sel == nil {
		return nil, false, true
	}
	ctes, cteChanged, ok := c.ctes(sel.CTEs)
	if !ok {
		return nil, false, false
	}
	cols, colsChanged, ok := c.selectColumns(sel.Columns)
	if !ok {
		return nil, false, false
	}
	from, fromChanged, ok := c.tableRef(sel.From)
	if !ok {
		return nil, false, false
	}
	joins, joinsChanged, ok := c.joins(sel.Joins)
	if !ok {
		return nil, false, false
	}
	where, whereChanged, ok := c.exprField(sel.Where)
	if !ok {
		return nil, false, false
	}
	groupBy, gbChanged, ok := c.exprList(sel.GroupBy)
	if !ok {
		return nil, false, false
	}
	having, havingChanged, ok := c.exprField(sel.Having)
	if !ok {
		return nil, false, false
	}
	windows, winChanged, ok := c.windows(sel.Windows)
	if !ok {
		return nil, false, false
	}
	orderBy, obChanged, ok := c.orderBy(sel.OrderBy)
	if !ok {
		return nil, false, false
	}
	limit, limitChanged, ok := c.exprField(sel.Limit)
	if !ok {
		return nil, false, false
	}
	offset, offsetChanged, ok := c.exprField(sel.Offset)
	if !ok {
		return nil, false, false
	}
	var union *sql.SelectStmt
	unionChanged := false
	if sel.Union != nil {
		union, unionChanged, ok = c.selectStmt(sel.Union)
		if !ok {
			return nil, false, false
		}
	}
	if !cteChanged && !colsChanged && !fromChanged && !joinsChanged && !whereChanged &&
		!gbChanged && !havingChanged && !winChanged && !obChanged && !limitChanged &&
		!offsetChanged && !unionChanged {
		return sel, false, true
	}
	out := *sel
	out.CTEs = ctes
	out.Columns = cols
	out.From = from
	out.Joins = joins
	out.Where = where
	out.GroupBy = groupBy
	out.Having = having
	out.Windows = windows
	out.OrderBy = orderBy
	out.Limit = limit
	out.Offset = offset
	out.Union = union
	return &out, true, true
}

// exprField substitutes an optional expression field, reporting change.
func (c *exprClone) exprField(e sql.Expr) (sql.Expr, bool, bool) {
	if e == nil {
		return nil, false, true
	}
	cloned, ok := c.expr(e)
	if !ok {
		return nil, false, false
	}
	return cloned, cloned != e, true
}

// selectColumns substitutes the SELECT list.
func (c *exprClone) selectColumns(cols []sql.SelectColumn) (_ []sql.SelectColumn, changed, ok bool) {
	var out []sql.SelectColumn
	for i, col := range cols {
		cloned, cok := c.expr(col.Expr)
		if !cok {
			return nil, false, false
		}
		if cloned != col.Expr {
			changed = true
			if out == nil {
				out = make([]sql.SelectColumn, len(cols))
				copy(out, cols)
			}
			out[i].Expr = cloned
		}
	}
	if !changed {
		return cols, false, true
	}
	return out, true, true
}

// tableRef substitutes a FROM term (subquery or table-valued args).
func (c *exprClone) tableRef(ref sql.TableRef) (sql.TableRef, bool, bool) {
	changed := false
	if ref.Subquery != nil {
		sub, subChanged, ok := c.selectStmt(ref.Subquery)
		if !ok {
			return ref, false, false
		}
		if subChanged {
			ref.Subquery = sub
			changed = true
		}
	}
	if len(ref.Args) > 0 {
		args, argsChanged, ok := c.exprList(ref.Args)
		if !ok {
			return ref, false, false
		}
		if argsChanged {
			ref.Args = args
			changed = true
		}
	}
	return ref, changed, true
}

// joins substitutes JOIN ON conditions and joined FROM terms.
func (c *exprClone) joins(joins []sql.JoinClause) (_ []sql.JoinClause, changed, ok bool) {
	var out []sql.JoinClause
	for i, j := range joins {
		on, onChanged, onOK := c.exprField(j.On)
		if !onOK {
			return nil, false, false
		}
		tbl, tblChanged, tblOK := c.tableRef(j.Table)
		if !tblOK {
			return nil, false, false
		}
		if onChanged || tblChanged {
			changed = true
			if out == nil {
				out = make([]sql.JoinClause, len(joins))
				copy(out, joins)
			}
			out[i].On = on
			out[i].Table = tbl
		}
	}
	if !changed {
		return joins, false, true
	}
	return out, true, true
}

// ctes substitutes WITH-clause bodies.
func (c *exprClone) ctes(defs []sql.CTEDef) (_ []sql.CTEDef, changed, ok bool) {
	var out []sql.CTEDef
	for i, def := range defs {
		sel, selChanged, selOK := c.selectStmt(def.Select)
		if !selOK {
			return nil, false, false
		}
		if selChanged {
			changed = true
			if out == nil {
				out = make([]sql.CTEDef, len(defs))
				copy(out, defs)
			}
			out[i].Select = sel
		}
	}
	if !changed {
		return defs, false, true
	}
	return out, true, true
}

// windows substitutes WINDOW definitions.
func (c *exprClone) windows(defs []sql.WindowDef) (_ []sql.WindowDef, changed, ok bool) {
	var out []sql.WindowDef
	for i, def := range defs {
		partitions, pChanged, pok := c.exprList(def.Partitions)
		if !pok {
			return nil, false, false
		}
		orderBy, oChanged, ook := c.orderBy(def.OrderBy)
		if !ook {
			return nil, false, false
		}
		if pChanged || oChanged {
			changed = true
			if out == nil {
				out = make([]sql.WindowDef, len(defs))
				copy(out, defs)
			}
			out[i].Partitions = partitions
			out[i].OrderBy = orderBy
		}
	}
	if !changed {
		return defs, false, true
	}
	return out, true, true
}

// cloneSelectCOW substitutes a top-level SELECT template statement.
func cloneSelectCOW(s *sql.SelectStmt, values []interface{}, idx *int) (sql.Stmt, bool) {
	c := &exprClone{values: values, idx: idx}
	cloned, _, ok := c.selectStmt(s)
	if !ok {
		return nil, false
	}
	return cloned, true
}

// cloneUpdateCOW substitutes a top-level UPDATE template statement. Field
// order matches source order: WITH first, then SET, FROM, WHERE,
// ORDER BY, LIMIT, OFFSET, RETURNING.
func cloneUpdateCOW(s *sql.UpdateStmt, values []interface{}, idx *int) (sql.Stmt, bool) {
	c := &exprClone{values: values, idx: idx}
	out := *s
	changed := false

	ctes, cteChanged, ok := c.ctes(s.CTEs)
	if !ok {
		return nil, false
	}
	if cteChanged {
		out.CTEs = ctes
		changed = true
	}
	assignments, aChanged, ok := c.assignments(s.Assignments)
	if !ok {
		return nil, false
	}
	if aChanged {
		out.Assignments = assignments
		changed = true
	}
	from, fromChanged, ok := c.tableRef(s.From)
	if !ok {
		return nil, false
	}
	if fromChanged {
		out.From = from
		changed = true
	}
	fromJoins, fjChanged, ok := c.joins(s.FromJoins)
	if !ok {
		return nil, false
	}
	if fjChanged {
		out.FromJoins = fromJoins
		changed = true
	}
	where, wChanged, ok := c.exprField(s.Where)
	if !ok {
		return nil, false
	}
	if wChanged {
		out.Where = where
		changed = true
	}
	orderBy, obChanged, ok := c.orderBy(s.OrderBy)
	if !ok {
		return nil, false
	}
	if obChanged {
		out.OrderBy = orderBy
		changed = true
	}
	limit, lChanged, ok := c.exprField(s.Limit)
	if !ok {
		return nil, false
	}
	if lChanged {
		out.Limit = limit
		changed = true
	}
	offset, oChanged, ok := c.exprField(s.Offset)
	if !ok {
		return nil, false
	}
	if oChanged {
		out.Offset = offset
		changed = true
	}
	returning, retChanged, ok := c.returning(s.Returning)
	if !ok {
		return nil, false
	}
	if retChanged {
		out.Returning = returning
		changed = true
	}
	if !changed {
		return s, true
	}
	return &out, true
}

// cloneDeleteCOW substitutes a top-level DELETE template statement. Field
// order matches source order: WITH first, then WHERE, ORDER BY, LIMIT,
// OFFSET, RETURNING.
func cloneDeleteCOW(s *sql.DeleteStmt, values []interface{}, idx *int) (sql.Stmt, bool) {
	c := &exprClone{values: values, idx: idx}
	out := *s
	changed := false

	ctes, cteChanged, ok := c.ctes(s.CTEs)
	if !ok {
		return nil, false
	}
	if cteChanged {
		out.CTEs = ctes
		changed = true
	}
	where, wChanged, ok := c.exprField(s.Where)
	if !ok {
		return nil, false
	}
	if wChanged {
		out.Where = where
		changed = true
	}
	orderBy, obChanged, ok := c.orderBy(s.OrderBy)
	if !ok {
		return nil, false
	}
	if obChanged {
		out.OrderBy = orderBy
		changed = true
	}
	limit, lChanged, ok := c.exprField(s.Limit)
	if !ok {
		return nil, false
	}
	if lChanged {
		out.Limit = limit
		changed = true
	}
	offset, oChanged, ok := c.exprField(s.Offset)
	if !ok {
		return nil, false
	}
	if oChanged {
		out.Offset = offset
		changed = true
	}
	returning, retChanged, ok := c.returning(s.Returning)
	if !ok {
		return nil, false
	}
	if retChanged {
		out.Returning = returning
		changed = true
	}
	if !changed {
		return s, true
	}
	return &out, true
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
