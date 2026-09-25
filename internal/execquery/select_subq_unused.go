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
// not apply and sub is left untouched. declared is the view's declared
// column list (CREATE VIEW v(x,y) AS ...) when sub is a view body, else nil.
//
// The select.c eligibility conditions are mirrored: the compound chain may
// only use UNION ALL, every member must be non-DISTINCT and free of
// aggregates and window functions, and the subquery must not be correlated.
// Columns the outer statement references — by name through the subquery's
// alias or unqualified, or through a declared view column name, which maps
// POSITIONALLY to the body's output column (resolve.c maps the reference to
// iColumn on the subquery source and sets that colUsed bit) — and the
// subquery's own ORDER BY columns are exempt (select.c marks iOrderByCol
// columns in colUsed).
func (e *SelectEngine) disableUnusedSubqueryColumns(outer *sql.SelectStmt, qualifiers []string, declared []string, sub *sql.SelectStmt) int {
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
	if len(declared) == len(outNames) {
		use.declaredNames = declared
	}
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
	// winRefs accumulates the window names referenced by OVER clauses
	// (OVER w, or OVER (w ...) via BaseName), so the matching WINDOW-clause
	// definitions can be walked too. SQLite prunes unreferenced named
	// windows before name resolution (sqlite3WindowListPrune), so only
	// referenced definitions ever set colUsed.
	winRefs map[string]bool
	// declaredNames, when its length matches outNames, is the view's
	// declared column list: an outer reference to the i-th declared name
	// observes output column i (resolve.c maps a view column reference to
	// iColumn on the subquery source), even when the declared name differs
	// from the body's own output name.
	declaredNames []string
}

// newSubqueryColumnUse starts an empty used-column set over the expanded
// output column names.
func newSubqueryColumnUse(outNames []string, qualifiers []string) *subqueryColumnUse {
	return &subqueryColumnUse{outNames: outNames, used: make(map[int]bool), qualifiers: qualifiers, winRefs: make(map[string]bool)}
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

// markName marks every output column whose name matches (case-insensitively),
// either as the body's own output name or — for view bodies — positionally
// through the declared column list.
func (u *subqueryColumnUse) markName(name string) {
	if name == "" {
		return
	}
	for i, n := range u.outNames {
		if n != "" && strings.EqualFold(n, name) {
			u.used[i] = true
		}
	}
	for i, n := range u.declaredNames {
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
// SELECT-list star does. The walk mirrors SQLite's name resolution scope:
// a function call's aggregate ORDER BY terms, FILTER condition, and window
// definition all resolve and set colUsed (resolve.c TK_FUNCTION), and an
// expression subquery's body resolves against the enclosing sources, so
// correlated IN/EXISTS/scalar subqueries observe the FROM-subquery's
// output columns.
func (u *subqueryColumnUse) markExprWalk(expr sql.Expr, fn func(sql.Expr)) {
	if expr == nil {
		return
	}
	fn(expr)
	switch v := expr.(type) {
	case *sql.FuncCall:
		u.markFuncCallUse(v, fn)
		return
	case *sql.Subquery:
		u.markSelectReferences(v.Select)
		return
	case *sql.ExistsExpr:
		u.markSelectReferences(v.Select)
		return
	}
	for _, child := range exprChildren(expr) {
		u.markExprWalk(child, fn)
	}
}

// markFuncCallUse descends a function call's arguments (keeping the star
// wildcard skip), aggregate ORDER BY terms, FILTER condition, and window
// definition.
func (u *subqueryColumnUse) markFuncCallUse(fc *sql.FuncCall, fn func(sql.Expr)) {
	for _, a := range fc.Args {
		if ref, isRef := a.(*sql.ColumnRef); isRef && ref.Name == "*" {
			continue
		}
		u.markExprWalk(a, fn)
	}
	for _, ob := range fc.OrderBy {
		u.markExprWalk(ob.Expr, fn)
	}
	u.markExprWalk(fc.Filter, fn)
	u.markWindowDefUse(fc.Over, fn)
}

// markWindowDefUse records an OVER clause's window-name references and
// descends its window specification.
func (u *subqueryColumnUse) markWindowDefUse(w *sql.WindowDef, fn func(sql.Expr)) {
	if w == nil {
		return
	}
	u.referWindowName(w.Name)
	u.referWindowName(w.BaseName)
	u.markWindowSpecUse(w, fn)
}

// markWindowSpecUse descends a window definition's PARTITION BY and ORDER BY
// terms and frame bound offsets, chaining the definition's base-window name.
func (u *subqueryColumnUse) markWindowSpecUse(w *sql.WindowDef, fn func(sql.Expr)) {
	u.referWindowName(w.BaseName)
	for _, p := range w.Partitions {
		u.markExprWalk(p, fn)
	}
	for _, ob := range w.OrderBy {
		u.markExprWalk(ob.Expr, fn)
	}
	if w.Frame != nil {
		u.markExprWalk(w.Frame.Start.Expr, fn)
		u.markExprWalk(w.Frame.End.Expr, fn)
	}
}

// referWindowName records a window-name reference (OVER w, or a base-window
// chain link) for the WINDOW-clause walk.
func (u *subqueryColumnUse) referWindowName(name string) {
	if name != "" {
		u.winRefs[strings.ToLower(name)] = true
	}
}

// markColumnRef is the walk callback behind markReferencedExpr: it marks the
// output columns one column reference observes.
func (u *subqueryColumnUse) markColumnRef(n sql.Expr) {
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
}

// markReferencedExpr marks the output columns referenced by the column
// references inside expr. A qualified reference through one of the
// subquery's qualifiers marks the named column; an unqualified reference
// marks every same-named column (it may resolve elsewhere — over-marking
// only shrinks the optimization). A wildcard over the subquery marks all.
func (u *subqueryColumnUse) markReferencedExpr(expr sql.Expr) {
	u.markExprWalk(expr, u.markColumnRef)
}

// markOuterReferences marks the subquery output columns observed by the
// outer statement's clauses and by the WINDOW-clause definitions its OVER
// clauses reference.
func (u *subqueryColumnUse) markOuterReferences(outer *sql.SelectStmt) {
	u.markClauseReferences(outer)
	u.markReferencedNamedWindows(outer)
}

// markClauseReferences walks the column references of a statement's clauses
// (columns, WHERE, join ONs, GROUP BY, HAVING, ORDER BY, LIMIT, OFFSET).
// Shared with expression-subquery bodies, whose references resolve against
// enclosing sources exactly like these clauses do.
func (u *subqueryColumnUse) markClauseReferences(s *sql.SelectStmt) {
	if s == nil {
		return
	}
	for _, col := range s.Columns {
		u.markReferencedExpr(col.Expr)
	}
	u.markReferencedExpr(s.Where)
	for i := range s.Joins {
		u.markReferencedExpr(s.Joins[i].On)
	}
	for _, g := range s.GroupBy {
		u.markReferencedExpr(g)
	}
	u.markReferencedExpr(s.Having)
	for _, ob := range s.OrderBy {
		u.markReferencedExpr(ob.Expr)
	}
	u.markReferencedExpr(s.Limit)
	u.markReferencedExpr(s.Offset)
}

// markSelectReferences walks the clauses of an expression subquery body
// (and its compound members). Its FROM/join derived tables resolve in their
// own scope and are not walked: SQLite sets colUsed only for references that
// resolve to the subquery source itself. Over-marking through name collisions
// is acceptable — it only shrinks the optimization.
func (u *subqueryColumnUse) markSelectReferences(sel *sql.SelectStmt) {
	for m := sel; m != nil; m = m.Union {
		u.markClauseReferences(m)
	}
}

// markReferencedNamedWindows walks the WINDOW-clause definitions referenced
// by some OVER clause (directly or through a base-window chain); the walk
// repeats while new base-window names surface, visiting each definition once.
// Unreferenced definitions are pruned before resolution in SQLite
// (sqlite3WindowListPrune) and their expressions never set colUsed. Names
// recorded by subquery bodies may over-approximate the referenced set, which
// only shrinks the optimization.
func (u *subqueryColumnUse) markReferencedNamedWindows(outer *sql.SelectStmt) {
	if len(outer.Windows) == 0 {
		return
	}
	visited := make(map[string]bool, len(outer.Windows))
	for {
		progressed := false
		for i := range outer.Windows {
			w := &outer.Windows[i]
			if !u.winRefs[strings.ToLower(w.Name)] || visited[strings.ToLower(w.Name)] {
				continue
			}
			visited[strings.ToLower(w.Name)] = true
			u.markWindowSpecUse(w, u.markColumnRef)
			progressed = true
		}
		if !progressed {
			return
		}
	}
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
