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
	// Member eligibility over the whole compound chain.
	for m := sub; m != nil; m = m.Union {
		if m.Distinct || m.GroupBy != nil || m.Having != nil {
			return 0
		}
		if m.Union != nil && !(m.SetOp == sql.SetUnion && m.UnionAll) {
			// select.c: "This optimization does not work for compound
			// subqueries that use UNION, INTERSECT, or EXCEPT. Only
			// UNION ALL is allowed."
			return 0
		}
		for _, col := range m.Columns {
			if e.exprHasAggregate(col.Expr) || e.exprHasWindowFunc(col.Expr) {
				return 0
			}
		}
		for _, ob := range m.OrderBy {
			if e.exprHasAggregate(ob.Expr) || e.exprHasWindowFunc(ob.Expr) {
				return 0
			}
		}
	}
	if e.selectUsesExternalTables(sub, nil) {
		// Correlated subquery: leave it alone (select.c isCorrelated).
		return 0
	}
	// Expand every member's result list: sqlite3ExpandStarArray has run
	// before this optimization in sqlite3Select, so column positions (and
	// the colUsed bitmask) are expanded positions. A member whose stars
	// cannot be expanded (e.g. over a derived table) disables the
	// optimization entirely.
	members := make([]*sql.SelectStmt, 0, 4)
	expanded := make([][]sql.SelectColumn, 0, 4)
	for m := sub; m != nil; m = m.Union {
		cols, ok := e.expandMemberResultColumns(m)
		if !ok {
			return 0
		}
		members = append(members, m)
		expanded = append(expanded, cols)
	}
	// Output column names come from the leftmost member (the compound's
	// result set). Names that cannot be derived keep "" — an outer
	// reference can then no longer mark them used, which is safe: an
	// unaliased expression column is not addressable by name anyway.
	outNames := make([]string, len(expanded[0]))
	for i, col := range expanded[0] {
		outNames[i] = resultColumnNameOf(col)
	}
	used := make(map[int]bool)
	markName := func(name string) {
		if name == "" {
			return
		}
		for i, n := range outNames {
			if n != "" && strings.EqualFold(n, name) {
				used[i] = true
			}
		}
	}
	markAll := func() {
		for i := range outNames {
			used[i] = true
		}
	}
	// Which output columns does the outer statement observe? A qualified
	// reference through one of the subquery's qualifiers (table/alias
	// name) marks the named column; an unqualified reference marks every
	// same-named column (it may resolve elsewhere — over-marking only
	// shrinks the optimization). A wildcard over the subquery marks all.
	qualMatch := func(q string) bool {
		for _, qual := range qualifiers {
			if qual != "" && strings.EqualFold(q, qual) {
				return true
			}
		}
		return false
	}
	// outerWalk descends expression children, but never treats a star
	// function argument as a column wildcard: count(*) references no
	// column (expr.c TK_AGG_COUNT carries no aggregate operand), while a
	// bare SELECT-list star does.
	var outerWalk func(expr sql.Expr, fn func(sql.Expr))
	outerWalk = func(expr sql.Expr, fn func(sql.Expr)) {
		if expr == nil {
			return
		}
		fn(expr)
		if fc, ok := expr.(*sql.FuncCall); ok {
			for _, a := range fc.Args {
				if ref, isRef := a.(*sql.ColumnRef); isRef && ref.Name == "*" {
					continue
				}
				outerWalk(a, fn)
			}
			return
		}
		for _, child := range exprChildren(expr) {
			outerWalk(child, fn)
		}
	}
	outerRef := func(expr sql.Expr) {
		outerWalk(expr, func(n sql.Expr) {
			cr, ok := n.(*sql.ColumnRef)
			if !ok {
				return
			}
			if cr.Name == "*" {
				if cr.Table == "" || qualMatch(cr.Table) {
					markAll()
				}
				return
			}
			if cr.Table == "" || qualMatch(cr.Table) {
				markName(cr.Name)
			}
		})
	}
	for _, col := range outer.Columns {
		outerRef(col.Expr)
	}
	outerRef(outer.Where)
	for i := range outer.Joins {
		outerRef(outer.Joins[i].On)
	}
	for _, g := range outer.GroupBy {
		outerRef(g)
	}
	outerRef(outer.Having)
	for _, ob := range outer.OrderBy {
		outerRef(ob.Expr)
	}
	outerRef(outer.Limit)
	outerRef(outer.Offset)
	// The subquery's own ORDER BY pins its sort columns: an ordinal marks
	// that position; a bare name marks every same-named output column
	// (select.c sets colUsed bits for iOrderByCol terms). The trailing
	// compound ORDER BY is attached to the LAST member of the chain in the
	// parser's AST, so every member's terms are scanned.
	for m := sub; m != nil; m = m.Union {
		for _, ob := range m.OrderBy {
			if nl, ok := stripCollate(ob.Expr).(*sql.NumericLit); ok {
				if pos, err := strconv.Atoi(nl.Value); err == nil && pos >= 1 && pos <= len(outNames) {
					used[pos-1] = true
				}
				continue
			}
			if ref, ok := stripCollate(ob.Expr).(*sql.ColumnRef); ok && ref.Table == "" {
				markName(ref.Name)
			}
		}
	}
	// Rewrite: unused columns become NULL in every member (select.c walks
	// the members per column and sets TK_NULL). Members without a change
	// keep their original (star-shaped) result list.
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
		if ref.Table != "" {
			names, err := e.resolveTableColumnNames(m, ref.Table)
			if err != nil {
				return nil, false
			}
			for _, n := range names {
				out = append(out, sql.SelectColumn{Expr: &sql.ColumnRef{Table: ref.Table, Name: n}})
			}
			continue
		}
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
	inner = append(inner, local)
	uses := false
	check := func(expr sql.Expr) {
		if !uses && expr != nil && e.exprUsesExternalTables(expr, inner) {
			uses = true
		}
	}
	for _, col := range s.Columns {
		check(col.Expr)
	}
	check(s.Where)
	for i := range s.Joins {
		check(s.Joins[i].On)
	}
	for _, g := range s.GroupBy {
		check(g)
	}
	check(s.Having)
	for _, ob := range s.OrderBy {
		check(ob.Expr)
	}
	check(s.Limit)
	check(s.Offset)
	// FROM-clause subqueries, join derived tables, and CTE bodies resolve
	// with this select's scope pushed (they may reference it), and their
	// own contents are checked recursively.
	if s.From.Subquery != nil {
		check2 := e.selectUsesExternalTables(s.From.Subquery, inner)
		if check2 {
			return true
		}
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
	return uses
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
