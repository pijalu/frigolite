// Sub-package exec implements query execution.
package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// This file implements SQLite's omit-unused-subquery-column optimization
// (select.c disableUnusedSubqueryResultColumns, enabled by the
// SQLITE_NullUnusedCols setting): result columns of a FROM-clause subquery
// or view body that the outer statement never references are rewritten to
// NULL across every UNION ALL member, so their expressions — notably
// side-effecting or expensive user-defined function calls — are never
// evaluated. selectH.test observes the optimization by counting invocations
// of such a UDF through the SQL counter(N) function.

// disableUnusedSubqueryColumns rewrites sub (the FROM-clause subquery or
// freshly parsed view body of outer) in place, converting every unused
// result column of every eligible compound member to NULL. It returns the
// number of member result columns converted; 0 means the optimization did
// not apply and sub is left untouched.
//
// The select.c eligibility conditions are mirrored: the compound chain may
// only use UNION ALL, every member must be non-DISTINCT and free of
// aggregates and window functions, and the subquery must not be correlated.
// Columns the outer statement references (by name through the subquery's
// alias or unqualified) and the subquery's own ORDER BY columns are exempt
// (select.c marks iOrderByCol columns in colUsed).
func (e *SelectEngine) disableUnusedSubqueryColumns(outer *sql.SelectStmt, qualifiers []string, sub *sql.SelectStmt) int {
	if outer == nil || sub == nil {
		return 0
	}
	if !e.subqueryColumnsOmittable(sub) {
		return 0
	}
	if e.selectUsesExternalTables(sub, nil) {
		// Correlated subquery: leave it alone (select.c isCorrelated).
		return 0
	}
	members, expanded, ok := e.expandCompoundMembers(sub)
	if !ok {
		return 0
	}
	// Output column names come from the leftmost member (the compound's
	// result set). Names that cannot be derived keep "" — an outer
	// reference can then no longer mark them used, which is safe: an
	// unaliased expression column is not addressable by name anyway.
	outNames := make([]string, len(expanded[0]))
	for i, col := range expanded[0] {
		outNames[i] = resultColumnNameOf(col)
	}
	use := newSubqueryColumnUse(outNames, qualifiers)
	use.markOuterReferences(outer)
	use.markSubqueryOrderBy(sub)
	return rewriteUnusedSubqueryColumns(members, expanded, use.used)
}

// subqueryColumnsOmittable reports the select.c member-eligibility conditions
// over the whole compound chain: only UNION ALL, every member non-DISTINCT
// and free of aggregates and window functions.
func (e *SelectEngine) subqueryColumnsOmittable(sub *sql.SelectStmt) bool {
	for m := sub; m != nil; m = m.Union {
		if m.Distinct || m.GroupBy != nil || m.Having != nil {
			return false
		}
		if m.Union != nil && !(m.SetOp == sql.SetUnion && m.UnionAll) {
			// select.c: "This optimization does not work for compound
			// subqueries that use UNION, INTERSECT, or EXCEPT. Only
			// UNION ALL is allowed."
			return false
		}
		if e.memberHasAggregateOrWindowCol(m.Columns) || e.memberHasAggregateOrWindowOb(m.OrderBy) {
			return false
		}
	}
	return true
}

// memberHasAggregateOrWindowCol reports whether any result column expression
// carries an aggregate or window function.
func (e *SelectEngine) memberHasAggregateOrWindowCol(cols []sql.SelectColumn) bool {
	for _, col := range cols {
		if e.exprHasAggregate(col.Expr) || e.exprHasWindowFunc(col.Expr) {
			return true
		}
	}
	return false
}

// memberHasAggregateOrWindowOb reports whether any ORDER BY term expression
// carries an aggregate or window function.
func (e *SelectEngine) memberHasAggregateOrWindowOb(orderBy []sql.OrderByTerm) bool {
	for _, ob := range orderBy {
		if e.exprHasAggregate(ob.Expr) || e.exprHasWindowFunc(ob.Expr) {
			return true
		}
	}
	return false
}

// expandCompoundMembers expands every member's result list (sqlite3ExpandStarArray
// has run before this optimization in sqlite3Select, so column positions —
// and the colUsed bitmask — are expanded positions). ok is false when a
// member's stars cannot be expanded (e.g. over a derived table), which
// disables the optimization entirely.
func (e *SelectEngine) expandCompoundMembers(sub *sql.SelectStmt) (members []*sql.SelectStmt, expanded [][]sql.SelectColumn, ok bool) {
	members = make([]*sql.SelectStmt, 0, 4)
	expanded = make([][]sql.SelectColumn, 0, 4)
	for m := sub; m != nil; m = m.Union {
		cols, colsOK := e.expandMemberResultColumns(m)
		if !colsOK {
			return nil, nil, false
		}
		members = append(members, m)
		expanded = append(expanded, cols)
	}
	return members, expanded, true
}

// rewriteUnusedSubqueryColumns converts every unused column to NULL in every
// member (select.c walks the members per column and sets TK_NULL). Members
// without a change keep their original (star-shaped) result list. Returns
// the number of member result columns converted.
func rewriteUnusedSubqueryColumns(members []*sql.SelectStmt, expanded [][]sql.SelectColumn, used map[int]bool) int {
	n := 0
	for mi, cols := range expanded {
		changed := false
		for j := range cols {
			if used[j] {
				continue
			}
			if _, isNull := cols[j].Expr.(*sql.NullLit); isNull {
				continue
			}
			cols[j].Expr = &sql.NullLit{}
			changed = true
			n++
		}
		if changed {
			members[mi].Columns = cols
		}
	}
	return n
}

// subqueryColumnUse tracks which of the subquery's output columns the outer
// statement observes while the omit-unused-column optimization runs.
type subqueryColumnUse struct {
	outNames   []string
	used       map[int]bool
	qualifiers []string
}

// newSubqueryColumnUse starts an empty used-column set over the expanded
// output column names.
func newSubqueryColumnUse(outNames []string, qualifiers []string) *subqueryColumnUse {
	return &subqueryColumnUse{outNames: outNames, used: make(map[int]bool), qualifiers: qualifiers}
}

// qualMatch reports whether q names one of the subquery's qualifiers
// (table/alias name).
func (u *subqueryColumnUse) qualMatch(q string) bool {
	for _, qual := range u.qualifiers {
		if qual != "" && strings.EqualFold(q, qual) {
			return true
		}
	}
	return false
}

// markName marks every output column whose name matches (case-insensitively).
func (u *subqueryColumnUse) markName(name string) {
	if name == "" {
		return
	}
	for i, n := range u.outNames {
		if n != "" && strings.EqualFold(n, name) {
			u.used[i] = true
		}
	}
}

// markAll marks every output column.
func (u *subqueryColumnUse) markAll() {
	for i := range u.outNames {
		u.used[i] = true
	}
}

// markExprWalk descends expression children, but never treats a star
// function argument as a column wildcard: count(*) references no column
// (expr.c TK_AGG_COUNT carries no aggregate operand), while a bare
// SELECT-list star does.
func (u *subqueryColumnUse) markExprWalk(expr sql.Expr, fn func(sql.Expr)) {
	if expr == nil {
		return
	}
	fn(expr)
	if fc, ok := expr.(*sql.FuncCall); ok {
		for _, a := range fc.Args {
			if ref, isRef := a.(*sql.ColumnRef); isRef && ref.Name == "*" {
				continue
			}
			u.markExprWalk(a, fn)
		}
		return
	}
	for _, child := range exprChildren(expr) {
		u.markExprWalk(child, fn)
	}
}

// markReferencedExpr marks the output columns referenced by the column
// references inside expr. A qualified reference through one of the
// subquery's qualifiers marks the named column; an unqualified reference
// marks every same-named column (it may resolve elsewhere — over-marking
// only shrinks the optimization). A wildcard over the subquery marks all.
func (u *subqueryColumnUse) markReferencedExpr(expr sql.Expr) {
	u.markExprWalk(expr, func(n sql.Expr) {
		cr, ok := n.(*sql.ColumnRef)
		if !ok {
			return
		}
		if cr.Name == "*" {
			if cr.Table == "" || u.qualMatch(cr.Table) {
				u.markAll()
			}
			return
		}
		if cr.Table == "" || u.qualMatch(cr.Table) {
			u.markName(cr.Name)
		}
	})
}

// markOuterReferences marks the subquery output columns observed by the
// outer statement's clauses.
func (u *subqueryColumnUse) markOuterReferences(outer *sql.SelectStmt) {
	for _, col := range outer.Columns {
		u.markReferencedExpr(col.Expr)
	}
	u.markReferencedExpr(outer.Where)
	for i := range outer.Joins {
		u.markReferencedExpr(outer.Joins[i].On)
	}
	for _, g := range outer.GroupBy {
		u.markReferencedExpr(g)
	}
	u.markReferencedExpr(outer.Having)
	for _, ob := range outer.OrderBy {
		u.markReferencedExpr(ob.Expr)
	}
	u.markReferencedExpr(outer.Limit)
	u.markReferencedExpr(outer.Offset)
}

// markSubqueryOrderBy pins the subquery's own ORDER BY sort columns: an
// ordinal marks that position; a bare name marks every same-named output
// column (select.c sets colUsed bits for iOrderByCol terms). The trailing
// compound ORDER BY is attached to the LAST member of the chain in the
// parser's AST, so every member's terms are scanned.
func (u *subqueryColumnUse) markSubqueryOrderBy(sub *sql.SelectStmt) {
	for m := sub; m != nil; m = m.Union {
		for _, ob := range m.OrderBy {
			expr := stripCollate(ob.Expr)
			if nl, ok := expr.(*sql.NumericLit); ok {
				if pos, err := strconv.Atoi(nl.Value); err == nil && pos >= 1 && pos <= len(u.outNames) {
					u.used[pos-1] = true
				}
				continue
			}
			if ref, ok := expr.(*sql.ColumnRef); ok && ref.Table == "" {
				u.markName(ref.Name)
			}
		}
	}
}

// expandMemberResultColumns returns member's result list with every
// "*" / "t.*" item replaced by the concrete output columns of the member's
// FROM sources (sqlite3ExpandStarArray). ok is false when a star cannot be
// expanded (no FROM source, or a star over a derived table).
func (e *SelectEngine) expandMemberResultColumns(m *sql.SelectStmt) ([]sql.SelectColumn, bool) {
	out := make([]sql.SelectColumn, 0, len(m.Columns)+8)
	for _, col := range m.Columns {
		ref, isStar := col.Expr.(*sql.ColumnRef)
		if !isStar || ref.Name != "*" {
			out = append(out, col)
			continue
		}
		var ok bool
		if ref.Table != "" {
			out, ok = e.expandQualifiedStarColumns(m, out, ref)
		} else {
			out, ok = e.expandBareStarColumns(m, out)
		}
		if !ok {
			return nil, false
		}
	}
	return out, true
}

// expandQualifiedStarColumns appends the concrete columns of a "t.*" item
// (resolved through the member's FROM sources). ok is false on resolution
// failure.
func (e *SelectEngine) expandQualifiedStarColumns(m *sql.SelectStmt, out []sql.SelectColumn, ref *sql.ColumnRef) ([]sql.SelectColumn, bool) {
	names, err := e.resolveTableColumnNames(m, ref.Table)
	if err != nil {
		return nil, false
	}
	for _, n := range names {
		out = append(out, sql.SelectColumn{Expr: &sql.ColumnRef{Table: ref.Table, Name: n}})
	}
	return out, true
}

// expandBareStarColumns appends the concrete columns of a bare "*" item
// through the member's FROM sources. ok is false when the star cannot be
// expanded (no FROM source, a derived/table-valued source, or a resolution
// failure): the caller disables the optimization.
func (e *SelectEngine) expandBareStarColumns(m *sql.SelectStmt, out []sql.SelectColumn) ([]sql.SelectColumn, bool) {
	sources := memberSourceRefs(m)
	if len(sources) == 0 {
		return nil, false
	}
	for _, src := range sources {
		if src.Subquery != nil || src.Name == "" {
			// A star over a derived/table-valued source is left
			// unexpanded: the caller disables the optimization.
			return nil, false
		}
		names, err := e.resolveTableColumnNames(m, src.Name)
		if err != nil {
			return nil, false
		}
		for _, n := range names {
			out = append(out, sql.SelectColumn{Expr: &sql.ColumnRef{Name: n}})
		}
	}
	return out, true
}

// memberSourceRefs lists a compound member's FROM sources in declaration
// order (the FROM term first, then the joins).
func memberSourceRefs(m *sql.SelectStmt) []sql.TableRef {
	out := make([]sql.TableRef, 0, len(m.Joins)+1)
	if m.From.Name != "" || m.From.As != "" || m.From.Subquery != nil {
		out = append(out, m.From)
	}
	for _, j := range m.Joins {
		out = append(out, j.Table)
	}
	return out
}

// resultColumnNameOf derives a result column's output name: the alias when
// present, then the plain column name, then the rendered expression.
func resultColumnNameOf(col sql.SelectColumn) string {
	if col.As != "" {
		return col.As
	}
	if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name != "*" {
		return ref.Name
	}
	if col.Expr == nil {
		return ""
	}
	return sql.ExprString(col.Expr)
}

// selectUsesExternalTables reports whether any column reference inside s
// (including nested subqueries and CTE bodies) names a source that is not in
// scope at that point inside s — i.e. s is correlated with an enclosing
// statement. Unqualified references are treated as internal (they normally
// resolve to an in-scope source; treating them as external would only
// shrink the optimization).
func (e *SelectEngine) selectUsesExternalTables(s *sql.SelectStmt, scopes []map[string]bool) bool {
	if s == nil {
		return false
	}
	inner := subqueryScopeWithLocals(s, scopes)
	if e.selectClausesUseExternalTables(s, inner) {
		return true
	}
	// FROM-clause subqueries, join derived tables, and CTE bodies resolve
	// with this select's scope pushed (they may reference it), and their
	// own contents are checked recursively.
	return e.nestedSourcesUseExternalTables(s, inner)
}

// subqueryScopeWithLocals returns scopes extended with s's own FROM/join
// sources and CTE names.
func subqueryScopeWithLocals(s *sql.SelectStmt, scopes []map[string]bool) []map[string]bool {
	local := map[string]bool{}
	addSource := func(t sql.TableRef) {
		if t.Name != "" {
			local[strings.ToLower(t.Name)] = true
		}
		if t.As != "" {
			local[strings.ToLower(t.As)] = true
		}
	}
	addSource(s.From)
	for _, j := range s.Joins {
		addSource(j.Table)
	}
	for _, cte := range s.CTEs {
		if cte.Name != "" {
			local[strings.ToLower(cte.Name)] = true
		}
	}
	inner := make([]map[string]bool, 0, len(scopes)+1)
	inner = append(inner, scopes...)
	return append(inner, local)
}

// selectClausesUseExternalTables checks the statement's own clauses
// (columns, WHERE, join ONs, GROUP BY, HAVING, ORDER BY, LIMIT, OFFSET).
func (e *SelectEngine) selectClausesUseExternalTables(s *sql.SelectStmt, inner []map[string]bool) bool {
	for _, col := range s.Columns {
		if e.exprUsesExternalTables(col.Expr, inner) {
			return true
		}
	}
	for _, j := range s.Joins {
		if e.exprUsesExternalTables(j.On, inner) {
			return true
		}
	}
	for _, g := range s.GroupBy {
		if e.exprUsesExternalTables(g, inner) {
			return true
		}
	}
	for _, ob := range s.OrderBy {
		if e.exprUsesExternalTables(ob.Expr, inner) {
			return true
		}
	}
	// exprUsesExternalTables treats a nil expression as internal.
	return e.exprUsesExternalTables(s.Where, inner) ||
		e.exprUsesExternalTables(s.Having, inner) ||
		e.exprUsesExternalTables(s.Limit, inner) ||
		e.exprUsesExternalTables(s.Offset, inner)
}

// nestedSourcesUseExternalTables checks FROM-clause subqueries, join derived
// tables, and CTE bodies.
func (e *SelectEngine) nestedSourcesUseExternalTables(s *sql.SelectStmt, inner []map[string]bool) bool {
	if s.From.Subquery != nil && e.selectUsesExternalTables(s.From.Subquery, inner) {
		return true
	}
	for i := range s.Joins {
		if s.Joins[i].Table.Subquery != nil &&
			e.selectUsesExternalTables(s.Joins[i].Table.Subquery, inner) {
			return true
		}
	}
	for _, cte := range s.CTEs {
		if cte.Select != nil && e.selectUsesExternalTables(cte.Select, inner) {
			return true
		}
	}
	return false
}

// exprUsesExternalTables walks expr (recursing through expression children
// and nested subqueries) for a qualified column reference whose table is not
// in any of the given scopes.
func (e *SelectEngine) exprUsesExternalTables(expr sql.Expr, scopes []map[string]bool) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		if v.Table == "" {
			return false
		}
		t := strings.ToLower(v.Table)
		for _, sc := range scopes {
			if sc[t] {
				return false
			}
		}
		return true
	case *sql.Subquery:
		return e.selectUsesExternalTables(v.Select, scopes)
	case *sql.ExistsExpr:
		return e.selectUsesExternalTables(v.Select, scopes)
	}
	for _, child := range exprChildren(expr) {
		if e.exprUsesExternalTables(child, scopes) {
			return true
		}
	}
	return false
}
