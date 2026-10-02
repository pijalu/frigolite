package exec

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// Statement-level copy-on-write walkers for the template cache (expression
// dispatch lives in template_clone.go).

// selectParts collects one field group of a SELECT walk. Each group's walk
// order follows the statement's source order; changed reports whether any
// field in the group was substituted (fresh copy needed).
type selectParts struct {
	ctes    []sql.CTEDef
	cols    []sql.SelectColumn
	from    sql.TableRef
	joins   []sql.JoinClause
	where   sql.Expr
	groupBy []sql.Expr
	having  sql.Expr
	windows []sql.WindowDef
	orderBy []sql.OrderByTerm
	limit   sql.Expr
	offset  sql.Expr
	union   *sql.SelectStmt
}

// selectCOW substitutes a top-level SELECT template statement.
func (c *exprClone) selectCOW(s *sql.SelectStmt) (sql.Stmt, bool) {
	cloned, _, ok := c.selectStmt(s)
	if !ok {
		return nil, false
	}
	return cloned, true
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
	var p selectParts
	headChanged, ok := c.selectHead(sel, &p)
	if !ok {
		return nil, false, false
	}
	midChanged, ok := c.selectMid(sel, &p)
	if !ok {
		return nil, false, false
	}
	endChanged, ok := c.selectEnd(sel, &p)
	if !ok {
		return nil, false, false
	}
	if !headChanged && !midChanged && !endChanged {
		return sel, false, true
	}
	out := *sel
	out.CTEs = p.ctes
	out.Columns = p.cols
	out.From = p.from
	out.Joins = p.joins
	out.Where = p.where
	out.GroupBy = p.groupBy
	out.Having = p.having
	out.Windows = p.windows
	out.OrderBy = p.orderBy
	out.Limit = p.limit
	out.Offset = p.offset
	out.Union = p.union
	return &out, true, true
}

// selectHead walks the WITH clause and the select list / FROM head, in
// source order.
func (c *exprClone) selectHead(sel *sql.SelectStmt, p *selectParts) (bool, bool) {
	ctes, cteChanged, ok := c.ctes(sel.CTEs)
	if !ok {
		return false, false
	}
	cols, colsChanged, ok := c.selectColumns(sel.Columns)
	if !ok {
		return false, false
	}
	from, fromChanged, ok := c.tableRef(sel.From)
	if !ok {
		return false, false
	}
	joins, joinsChanged, ok := c.joins(sel.Joins)
	if !ok {
		return false, false
	}
	p.ctes, p.cols, p.from, p.joins = ctes, cols, from, joins
	return cteChanged || colsChanged || fromChanged || joinsChanged, true
}

// selectMid walks WHERE / GROUP BY / HAVING, in source order.
func (c *exprClone) selectMid(sel *sql.SelectStmt, p *selectParts) (bool, bool) {
	where, whereChanged, ok := c.exprField(sel.Where)
	if !ok {
		return false, false
	}
	groupBy, gbChanged, ok := c.exprList(sel.GroupBy)
	if !ok {
		return false, false
	}
	having, havingChanged, ok := c.exprField(sel.Having)
	if !ok {
		return false, false
	}
	p.where, p.groupBy, p.having = where, groupBy, having
	return whereChanged || gbChanged || havingChanged, true
}

// selectEnd walks WINDOW / ORDER BY / LIMIT / OFFSET and the compound tail,
// in source order.
func (c *exprClone) selectEnd(sel *sql.SelectStmt, p *selectParts) (bool, bool) {
	windows, winChanged, ok := c.windows(sel.Windows)
	if !ok {
		return false, false
	}
	orderBy, obChanged, ok := c.orderBy(sel.OrderBy)
	if !ok {
		return false, false
	}
	limit, limitChanged, ok := c.exprField(sel.Limit)
	if !ok {
		return false, false
	}
	offset, offsetChanged, ok := c.exprField(sel.Offset)
	if !ok {
		return false, false
	}
	// LIMIT <a>, <b> and LIMIT <a> OFFSET <b> share one normalize key but
	// bind their values in opposite text order (comma form: a=OFFSET b=LIMIT;
	// keyword form: a=LIMIT b=OFFSET), and this walk substitutes in fixed
	// field order — a two-slot limit shape would cross-assign its values
	// (limit-1.4.2: LIMIT 30, 50 executed as LIMIT 50 OFFSET 30). Both
	// slots present ⇒ decline; the statement full-parses. One slot is
	// unambiguous (it is the LIMIT, or the OFFSET of a keyword-form).
	if limit != nil && offset != nil {
		return false, false
	}
	unionChanged := false
	union := sel.Union
	if sel.Union != nil {
		union, unionChanged, ok = c.selectStmt(sel.Union)
		if !ok {
			return false, false
		}
	}
	p.windows, p.orderBy, p.limit, p.offset, p.union = windows, orderBy, limit, offset, union
	return winChanged || obChanged || limitChanged || offsetChanged || unionChanged, true
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

// updateParts collects one field group of an UPDATE walk, in source order
// (WITH, SET, FROM, WHERE, ORDER BY, LIMIT, OFFSET, RETURNING).
type updateParts struct {
	ctes        []sql.CTEDef
	assignments []sql.Assignment
	from        sql.TableRef
	fromJoins   []sql.JoinClause
	where       sql.Expr
	orderBy     []sql.OrderByTerm
	limit       sql.Expr
	offset      sql.Expr
	returning   sql.SelectColumn
}

// cloneUpdateCOW substitutes a top-level UPDATE template statement.
func (c *exprClone) updateCOW(s *sql.UpdateStmt) (sql.Stmt, bool) {
	var p updateParts
	headChanged, ok := c.updateHead(s, &p)
	if !ok {
		return nil, false
	}
	tailChanged, ok := c.updateTail(s, &p)
	if !ok {
		return nil, false
	}
	if !headChanged && !tailChanged {
		return s, true
	}
	out := *s
	out.CTEs = p.ctes
	out.Assignments = p.assignments
	out.From = p.from
	out.FromJoins = p.fromJoins
	out.Where = p.where
	out.OrderBy = p.orderBy
	out.Limit = p.limit
	out.Offset = p.offset
	out.Returning = p.returning
	return &out, true
}

// updateHead walks WITH / SET / FROM / WHERE in source order.
func (c *exprClone) updateHead(s *sql.UpdateStmt, p *updateParts) (bool, bool) {
	ctes, cteChanged, ok := c.ctes(s.CTEs)
	if !ok {
		return false, false
	}
	assignments, aChanged, ok := c.assignments(s.Assignments)
	if !ok {
		return false, false
	}
	from, fromChanged, ok := c.tableRef(s.From)
	if !ok {
		return false, false
	}
	fromJoins, fjChanged, ok := c.joins(s.FromJoins)
	if !ok {
		return false, false
	}
	where, wChanged, ok := c.exprField(s.Where)
	if !ok {
		return false, false
	}
	p.ctes, p.assignments, p.from, p.fromJoins, p.where = ctes, assignments, from, fromJoins, where
	return cteChanged || aChanged || fromChanged || fjChanged || wChanged, true
}

// updateTail walks ORDER BY / LIMIT / OFFSET / RETURNING in source order.
func (c *exprClone) updateTail(s *sql.UpdateStmt, p *updateParts) (bool, bool) {
	orderBy, obChanged, ok := c.orderBy(s.OrderBy)
	if !ok {
		return false, false
	}
	limit, lChanged, ok := c.exprField(s.Limit)
	if !ok {
		return false, false
	}
	offset, oChanged, ok := c.exprField(s.Offset)
	if !ok {
		return false, false
	}
	// LIMIT <a>, <b> vs LIMIT <a> OFFSET <b> share one normalize key but
	// bind values in opposite text order; both slots present ⇒ decline
	// (same cross-assignment hazard as the SELECT walk).
	if limit != nil && offset != nil {
		return false, false
	}
	if !ok {
		return false, false
	}
	returning, retChanged, ok := c.returning(s.Returning)
	if !ok {
		return false, false
	}
	p.orderBy, p.limit, p.offset, p.returning = orderBy, limit, offset, returning
	return obChanged || lChanged || oChanged || retChanged, true
}

// deleteParts collects one field group of a DELETE walk, in source order
// (WITH, WHERE, ORDER BY, LIMIT, OFFSET, RETURNING).
type deleteParts struct {
	ctes      []sql.CTEDef
	where     sql.Expr
	orderBy   []sql.OrderByTerm
	limit     sql.Expr
	offset    sql.Expr
	returning sql.SelectColumn
}

// cloneDeleteCOW substitutes a top-level DELETE template statement.
func (c *exprClone) deleteCOW(s *sql.DeleteStmt) (sql.Stmt, bool) {
	var p deleteParts
	changed, ok := c.deleteWalk(s, &p)
	if !ok {
		return nil, false
	}
	if !changed {
		return s, true
	}
	out := *s
	out.CTEs = p.ctes
	out.Where = p.where
	out.OrderBy = p.orderBy
	out.Limit = p.limit
	out.Offset = p.offset
	out.Returning = p.returning
	return &out, true
}

// deleteWalk walks a DELETE's fields in source order.
func (c *exprClone) deleteWalk(s *sql.DeleteStmt, p *deleteParts) (bool, bool) {
	ctes, cteChanged, ok := c.ctes(s.CTEs)
	if !ok {
		return false, false
	}
	where, wChanged, ok := c.exprField(s.Where)
	if !ok {
		return false, false
	}
	orderBy, obChanged, ok := c.orderBy(s.OrderBy)
	if !ok {
		return false, false
	}
	limit, lChanged, ok := c.exprField(s.Limit)
	if !ok {
		return false, false
	}
	offset, oChanged, ok := c.exprField(s.Offset)
	if !ok {
		return false, false
	}
	// LIMIT <a>, <b> vs LIMIT <a> OFFSET <b> share one normalize key but
	// bind values in opposite text order; both slots present ⇒ decline
	// (same cross-assignment hazard as the SELECT walk).
	if limit != nil && offset != nil {
		return false, false
	}
	if !ok {
		return false, false
	}
	returning, retChanged, ok := c.returning(s.Returning)
	if !ok {
		return false, false
	}
	p.ctes, p.where, p.orderBy, p.limit, p.offset, p.returning = ctes, where, orderBy, limit, offset, returning
	return cteChanged || wChanged || obChanged || lChanged || oChanged || retChanged, true
}
