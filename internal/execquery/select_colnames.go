package execquery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *SelectEngine) qualifiedStarColNames(tableRef string, colDefs []sql.ColumnDef, row Row) []struct {
	name  string
	value interface{}
} {
	var out []struct {
		name  string
		value interface{}
	}
	// Resolve the referenced table's column names. Prefer the schema's full
	// column list: a USING join merges the join column out of the colDefs, but
	// t.* must still include it (SQLite emits the merged value for t.*).
	// When tableRef is a join ALIAS (a in FROM t5 AS a) the schema lookup may
	// find a same-named real table; the row map's alias-qualified keys are the
	// ground truth for what the alias actually exposes.
	colNames := e.qualifiedStarResolveNames(tableRef, colDefs, row)
	for _, name := range colNames {
		if val, ok := row.Get(tableRef + "." + name); ok {
			out = append(out, struct {
				name  string
				value interface{}
			}{name: name, value: val})
			continue
		}
		if val, ok := row.Get(name); ok {
			out = append(out, struct {
				name  string
				value interface{}
			}{name: name, value: val})
		}
	}
	return out
}

// dropHiddenDefNames removes names whose column def is flagged Hidden, so
// t.* value expansion matches the (hidden-filtered) result column names.
func dropHiddenDefNames(colDefs []sql.ColumnDef, names []string) []string {
	hidden := make(map[string]bool)
	for _, cd := range colDefs {
		if cd.Hidden {
			hidden[strings.ToLower(cd.Name)] = true
		}
	}
	if len(hidden) == 0 {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !hidden[strings.ToLower(n)] {
			out = append(out, n)
		}
	}
	return out
}

// qualifiedStarResolveNames resolves the column names for a t.* expansion:
// from the row map's alias-qualified keys, the schema column list, the column
// defs, or the row map keys as a last resort.
func (e *SelectEngine) qualifiedStarResolveNames(tableRef string, colDefs []sql.ColumnDef, row Row) []string {
	var colNames []string
	var err error
	if row != nil {
		if rm, ok := row.(RowMap); ok {
			colNames = qualifiedStarNamesFromRowMap(tableRef, rm)
			colNames = orderQualifiedNamesByDefs(colDefs, tableRef, colNames)
			// HIDDEN vtab columns live in the row map (explicit references
			// like `SELECT step FROM generate_series(1,5)` work) but never
			// expand in t.* (SQLite hidden-column semantics).
			colNames = dropHiddenDefNames(colDefs, colNames)
		}
	}
	if len(colNames) == 0 {
		colNames, err = e.tableColumnNames(tableRef)
	}
	if err != nil || len(colNames) == 0 {
		// Fall back to the column defs in order. For each def, resolve it
		// through the qualified key (alias.col) only — the short key is
		// ambiguous when two operands share column names (the last one wins
		// in the row map). A def that is itself prefixed (table.col from a
		// conflict-renamed operand) is used only when its prefix matches.
		if len(colNames) == 0 && row != nil {
			colNames = qualifiedStarNamesFromDefs(tableRef, colDefs, row)
		}
		// Last resort: derive the column names from the row map's qualified
		// keys (alias.col). Order is not guaranteed (Go map iteration), but
		// this still resolves the values for unusual row shapes.
		if len(colNames) == 0 && row != nil {
			colNames = qualifiedStarNamesFromRow(row, tableRef)
		}
	}
	return colNames
}

// qualifiedStarNamesFromRowMap extracts the column names from a row map's
// alias-qualified keys (alias.col), skipping rowid pseudo-columns.
func qualifiedStarNamesFromRowMap(tableRef string, rm RowMap) []string {
	prefix := tableRef + "."
	var names []string
	seen := make(map[string]bool)
	for k := range rm {
		if strings.HasPrefix(k, prefix) {
			n := strings.TrimPrefix(k, prefix)
			// Skip the rowid pseudo-columns, the FTS hidden docid alias, and
			// the reserved positional key: t.* never expands them.
			if skipQualifiedStarName(n) {
				continue
			}
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names
}

// qualifiedStarNamesFromDefs resolves t.* column names from the column defs in
// order, using the qualified key (alias.col) or the unprefixed key when it
// resolves in the row (table-valued pragma / virtual-table operands expose
// their columns unprefixed).
func qualifiedStarNamesFromDefs(tableRef string, colDefs []sql.ColumnDef, row Row) []string {
	var colNames []string
	seen := make(map[string]bool)
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			colNames = append(colNames, name)
		}
	}
	for _, cd := range colDefs {
		if cd.Dropped || cd.Hidden {
			continue
		}
		if strings.HasPrefix(cd.Name, tableRef+".") {
			add(strings.TrimPrefix(cd.Name, tableRef+"."))
			continue
		}
		if strings.Contains(cd.Name, ".") {
			// Prefixed def belonging to another operand.
			continue
		}
		if _, ok := row.Get(tableRef + "." + cd.Name); ok {
			add(cd.Name)
			continue
		}
		// Table-valued pragma / virtual-table operands expose their
		// columns under unprefixed keys (pragma_foreign_key_check's
		// real "rowid" column, generate_series columns, ...). Include
		// the def when the unprefixed key resolves.
		if _, ok := row.Get(cd.Name); ok {
			add(cd.Name)
		}
	}
	return colNames
}

// buildColumnNames builds the column name list from SELECT columns.
// selectAliasTarget returns the underlying table name for a FROM/JOIN alias
// in the SELECT, or "" when name is not an alias. Used to resolve qualified
// stars: SELECT a.* FROM t5 AS a must resolve to t5 even when a real table
// named a exists (a join alias shadows a same-named table).
func selectAliasTarget(s *sql.SelectStmt, name string) string {
	if s == nil || name == "" {
		return ""
	}
	if target := tableRefAliasTarget(s.From, name); target != "" {
		return target
	}
	for _, j := range s.Joins {
		if target := tableRefAliasTarget(j.Table, name); target != "" {
			return target
		}
	}
	return ""
}

// tableRefAliasTarget resolves a name against one FROM table reference: the
// table's alias when the alias matches, else the table name when the reference
// is unaliased and matches. An alias shadows a same-named table.
func tableRefAliasTarget(ref sql.TableRef, name string) string {
	if ref.As != "" && strings.EqualFold(ref.As, name) {
		return ref.Name
	}
	if ref.Name != "" && strings.EqualFold(ref.Name, name) && ref.As == "" {
		return ref.Name
	}
	return ""
}

func (e *SelectEngine) buildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef, sel *sql.SelectStmt) []string {
	var names []string
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			if ref.Table != "" {
				// Qualified star (t.*): only that table's columns.
				names = append(names, e.buildQualifiedStarNames(ref, colDefs, sel)...)
				continue
			}
			names = append(names, expandStarColNames(colDefs)...)
		} else if rv, ok := col.Expr.(*sql.RowValue); ok {
			// Multi-expression RETURNING (RETURNING a, b, *): expand * inline
			// and name each expression like a SELECT column list.
			names = append(names, e.buildRowValueNames(rv, colDefs)...)
		} else if col.As != "" {
			names = append(names, col.As)
		} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			names = append(names, e.resolveColumnRefName(ref, colDefs))
		} else {
			// Unaliased expression: SQLite names the result column after the
			// expression text (e.g. SELECT a+b names it "a+b"). Without this,
			// CREATE TABLE ... AS SELECT of an expression produces a column
			// with an empty name and SELECT * exposes zero columns.
			names = append(names, sql.ExprString(col.Expr))
		}
	}
	return names
}

// expandStarColNames returns the non-dropped, non-hidden column names for a
// plain * expansion.
func expandStarColNames(colDefs []sql.ColumnDef) []string {
	var names []string
	for _, cd := range colDefs {
		if cd.Dropped || IsHiddenColumnDef(cd) {
			continue
		}
		names = append(names, cd.Name)
	}
	return names
}

// buildRowValueNames names each expression in a multi-expression RETURNING
// row value, expanding * inline.
func (e *SelectEngine) buildRowValueNames(rv *sql.RowValue, colDefs []sql.ColumnDef) []string {
	var names []string
	for _, sub := range rv.Values {
		if ref, ok := sub.(*sql.ColumnRef); ok && ref.Name == "*" {
			names = append(names, expandStarColNames(colDefs)...)
		} else if ref, ok := sub.(*sql.ColumnRef); ok {
			names = append(names, ref.Name)
		} else {
			names = append(names, "")
		}
	}
	return names
}

// selectProjectsPlainColumns reports whether every SELECT column is a bare
// column reference or star (no computed expressions, aliases, or aggregates).
// When true, a query's joined row maps align with its output rows, so they can
// be reused when the query is materialized as a derived table.
func selectProjectsPlainColumns(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if col.As != "" {
			return false
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			if ref.Name == "*" || ref.Name != "" {
				continue
			}
		}
		return false
	}
	return true
}

// validateOrderBy checks that any positional ORDER BY terms (integer literals)
// fall within the range of result columns. Returns an error matching SQLite's
// message format when a term is out of range.
func validateOrderBy(orderBy []sql.OrderByTerm, numCols int) error {
	for i, ob := range orderBy {
		if nl, ok := ob.Expr.(*sql.NumericLit); ok {
			// Parse the positional reference
			n, ok := parsePositiveInt(nl.Value)
			if !ok || n < 1 {
				continue // not a valid positional reference
			}
			if n > numCols {
				return fmt.Errorf("%d%s ORDER BY term out of range - should be between 1 and %d",
					i+1, ordinalSuffix(i+1), numCols)
			}
		}
	}
	return nil
}

// validateCompoundOrderBy enforces SQLite's compound-SELECT ORDER BY rule:
// each term must be a result-column ordinal or a name matching one of the
// result columns of any SELECT member (case-insensitively). Expressions that
// do not match a member column (e.g. ORDER BY a+b when no member selects a
// column named a+b) are rejected with "Nth ORDER BY term does not match any
// column in the result set".
// expressionMatchesCompoundResult reports whether expr exactly matches one of
// the compound members' result-column expressions (ignoring the column alias;
// SQLite allows ORDER BY <expr> when <expr> is also a result column).
func (e *SelectEngine) expressionMatchesCompoundResult(s *sql.SelectStmt, expr sql.Expr) bool {
	cur := s
	for cur != nil {
		for _, col := range cur.Columns {
			if col.Expr == nil {
				continue
			}
			if sql.ExprString(expr) == sql.ExprString(col.Expr) {
				return true
			}
		}
		cur = cur.Union
	}
	return false
}

func parsePositiveInt(s string) (int, bool) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1000000 {
			return 0, false
		}
	}
	return n, n > 0
}

func ordinalSuffix(n int) string {
	switch n % 100 {
	case 11, 12, 13:
		return "th"
	}
	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}

// sortRowsWithMaps sorts result rows using the original row maps. It returns
// an error when an ORDER BY expression evaluation fails (e.g. an FTS rank()
// function rejecting an invalid matchinfo blob — fts3rank.test 1.3/1.5): the
// sort comparator itself cannot propagate errors, so the non-column ORDER BY
// expressions are evaluated once per row up front (their values are stored in
// the row maps under the rendered-expression key, which compareOrderByFallback
// reuses).
func (e *SelectEngine) sortRowsWithMaps(result *Result, orderBy []sql.OrderByTerm, rowMaps []RowMap) error {
	n := len(rowMaps)
	if n <= 1 {
		return nil
	}
	// Ensure result.Rows has at least as many elements as rowMaps
	if len(result.Rows) < n {
		n = len(result.Rows)
	}
	if n <= 1 {
		return nil
	}
	// Pre-evaluate ORDER BY expressions that are not plain unqualified column
	// references (the comparator would otherwise discard evaluation errors).
	for _, ob := range orderBy {
		obExpr := normalizeOrderByExpr(ob.Expr)
		ref, isRef := stripCollate(obExpr).(*sql.ColumnRef)
		if isRef && ref.Table == "" && ref.Name != "*" {
			continue
		}
		key := sql.ExprString(ob.Expr)
		for i := 0; i < n; i++ {
			if _, ok := rowMaps[i].Get(key); ok {
				continue
			}
			v, err := e.ctx.EvalExpr(ob.Expr, rowMaps[i])
			if err != nil {
				return err
			}
			rowMaps[i][key] = v
		}
	}
	// Sort indices, then reorder both slices in-place
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool {
		return e.lessRows(orderBy, rowMaps, result.Rows, result.Columns, indices[i], indices[j])
	})
	newRows := make([][]interface{}, n)
	newMaps := make([]RowMap, n)
	for i, idx := range indices {
		newRows[i] = result.Rows[idx]
		newMaps[i] = rowMaps[idx]
	}
	result.Rows = newRows
	copy(rowMaps, newMaps)
	return nil
}

// resultColumnIndex returns the index of a column name in resultCols
// (case-insensitive), or -1.
func resultColumnIndex(resultCols []string, name string) int {
	for i, c := range resultCols {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

// derivedTableBadColumnRef returns the first column reference in a derived
// table (subquery in FROM/JOIN) that does NOT resolve to a table within the
// subquery's own FROM scope. Derived tables cannot be correlated: a reference
// like t6.a inside (SELECT ... FROM t7 JOIN t8 ON t6.a) is "no such column".
func derivedTableBadColumnRef(s *sql.SelectStmt) string {
	if s == nil {
		return ""
	}
	local := map[string]bool{}
	collectFromTableNames(s, local)
	bad := ""
	walkSelectJoinExprs(s, func(e sql.Expr) {
		if bad != "" {
			return
		}
		cr, ok := e.(*sql.ColumnRef)
		if !ok || cr.Table == "" {
			return
		}
		// NEW.col / OLD.col inside a derived table's WHERE are trigger
		// context (the trigger body's subqueries may reference the triggering
		// row), not references to the derived table's own FROM sources
		// (tkt_7bbfb7d).
		if strings.EqualFold(cr.Table, "new") || strings.EqualFold(cr.Table, "old") {
			return
		}
		if !local[cr.Table] {
			bad = cr.Table + "." + cr.Name
		}
	})
	return bad
}

// subqueryColumnAffinities returns the affinity rune for each output column of
// a subquery's SELECT, derived from the expression (CAST, column refs, etc.).
// A zero rune means "unknown" (caller falls back to the column def type).
func subqueryColumnAffinities(s *sql.SelectStmt) []rune {
	if s == nil {
		return nil
	}
	affs := make([]rune, len(s.Columns))
	for i, col := range s.Columns {
		affs[i] = exprAffinitySimple(col.Expr)
	}
	return affs
}

// exprAffinitySimple computes a coarse affinity for an expression used as a
// subquery output column (CAST and numeric/string literals). Returns 0 when
// the affinity cannot be determined simply.
func exprAffinitySimple(e sql.Expr) rune {
	switch v := e.(type) {
	case *sql.CastExpr:
		return util.Affinity(v.AsType)
	case *sql.NumericLit:
		return 'R' // numeric literals behave like REAL in SQLite
	case *sql.ColumnRef:
		return 0 // resolved through the table at runtime
	default:
		return 0
	}
}

// compoundColumnAffinity returns the affinity of output column i of a compound
// SELECT (UNION/INTERSECT/EXCEPT), matching SQLite's selectAddColumnTypeAnd-
// Collation: each member's expression contributes an affinity (a table column
// contributes its declared affinity; literals contribute their expression
// affinity), and the affinities are merged. A TEXT affinity combined with a
// NUMERIC-family affinity (INTEGER/REAL/NUMERIC) yields BLOB (the affinities
// are incompatible); otherwise the most specific non-BLOB affinity wins
// (INTEGER > REAL > NUMERIC > TEXT, then BLOB). The merge is order-independent,
// matching SQLite (both "lit UNION col" and "col UNION lit" give the same
// result column affinity). Returns 0 when the affinity cannot be determined
// (no members / unknown).
func (e *SelectEngine) compoundColumnAffinity(s *sql.SelectStmt, i int) rune {
	var affs []rune
	for cur := s; cur != nil; cur = cur.Union {
		if i >= len(cur.Columns) {
			continue
		}
		affs = append(affs, e.memberExprAffinity(cur, i))
	}
	if len(affs) == 0 {
		return 0
	}
	hasText := false
	hasNumeric := false
	best := rune(0)
	for _, a := range affs {
		switch a {
		case 'T':
			hasText = true
		case 'I', 'R', 'N':
			hasNumeric = true
		}
		if affinityPrecedence(a) > affinityPrecedence(best) {
			best = a
		}
	}
	if hasText && hasNumeric {
		return 'B'
	}
	return best
}

// affinityPrecedence orders affinities so the most specific column affinity
// wins when merging compound members: INTEGER > REAL > NUMERIC > TEXT > BLOB.
func affinityPrecedence(a rune) int {
	switch a {
	case 'I':
		return 5
	case 'R':
		return 4
	case 'N':
		return 3
	case 'T':
		return 2
	case 'B':
		return 1
	default:
		return 0
	}
}

// memberExprAffinity returns the affinity contribution of a compound member's
// expression at column index i: a column reference contributes its declared
// column affinity (resolved through the member's FROM table), literals
// contribute their expression affinity (string → TEXT, numeric → NUMERIC,
// blob → BLOB, NULL → BLOB/neutral), and CAST contributes its target type.
// Returns 0 (unknown → treated as BLOB) for everything else.
func (e *SelectEngine) memberExprAffinity(member *sql.SelectStmt, i int) rune {
	if member == nil || i >= len(member.Columns) {
		return 0
	}
	expr := member.Columns[i].Expr
	switch v := expr.(type) {
	case *sql.ColumnRef:
		if v.Name == "*" {
			return 0
		}
		if aff, ok := e.memberFromColumnAffinity(member, v.Name); ok {
			return aff
		}
		return 0
	case *sql.CastExpr:
		return util.Affinity(v.AsType)
	case *sql.NumericLit:
		return 'N'
	case *sql.StringLit:
		return 'T'
	case *sql.BlobLit:
		return 'B'
	case *sql.NullLit:
		return 'B'
	default:
		return 0
	}
}

// memberFromColumnAffinity resolves a column reference in a compound member
// against the member's FROM table (or, for a member whose FROM is a subquery,
// the subquery's output column affinity). Returns (aff, true) when the
// column's declared affinity is known.
func (e *SelectEngine) memberFromColumnAffinity(member *sql.SelectStmt, colName string) (rune, bool) {
	if member == nil || member.From.Name == "" {
		return 0, false
	}
	entry, _, err := e.ctx.FindTable(member.From.Name)
	if err != nil {
		return 0, false
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, colName) {
			aff := util.Affinity(cd.Type)
			return aff, true
		}
	}
	return 0, false
}

// walkSelectJoinExprs walks a SELECT's columns, WHERE, HAVING, and each join's
// ON expression, used to detect outer-table references inside subqueries of a
// join ON clause.
func walkSelectJoinExprs(s *sql.SelectStmt, fn func(sql.Expr)) {
	if s == nil {
		return
	}
	for _, col := range s.Columns {
		walkJoinOnExpr(col.Expr, fn)
	}
	walkJoinOnExpr(s.Where, fn)
	walkJoinOnExpr(s.Having, fn)
	for i := range s.Joins {
		walkJoinOnExpr(s.Joins[i].On, fn)
	}
	for _, g := range s.GroupBy {
		walkJoinOnExpr(g, fn)
	}
	for _, ob := range s.OrderBy {
		walkJoinOnExpr(ob.Expr, fn)
	}
}

// sortRowMapsByPKNames sorts rowMaps by the PK column values in ascending order.
// pkColNames is the ordered list of PK column names to sort by.
func sortRowMapsByPKNames(rowMaps []RowMap, pkColNames []string) {
	if len(rowMaps) <= 1 {
		return
	}
	sort.SliceStable(rowMaps, func(a, b int) bool {
		for _, name := range pkColNames {
			va := util.UnwrapColumnValue(rowMaps[a][name])
			vb := util.UnwrapColumnValue(rowMaps[b][name])
			cmp := util.CompareValues(va, vb)
			if cmp != 0 {
				return cmp < 0
			}
		}
		return false
	})
}

// findRowIDRef returns the first rowid/_rowid_/oid column reference in a
// SELECT statement that resolves to the named table, or "" if there is none.
// Used to reject rowid references on WITHOUT ROWID tables. References
// qualified to a different table (e.g. t42.rowid in a join) are allowed, as
// are unqualified references when the query joins other tables that may
// provide a rowid.
// tableHasRealRowIDCol reports whether colDefs declares a column literally
// named rowid/_rowid_/oid. Such a column shadows the pseudo-rowid, making
// rowid references resolve to the real column (SQLite, expridx1).
func tableHasRealRowIDCol(colDefs []sql.ColumnDef) bool {
	for _, cd := range colDefs {
		if isRowIDName(cd.Name) {
			return true
		}
	}
	return false
}

// hasRowIDRef reports whether expr references rowid, _rowid_, or oid.
func HasRowIDRef(expr sql.Expr) bool {
	found := false
	WalkExprFull(expr, func(e2 sql.Expr) {
		if found {
			return
		}
		if cr, ok := e2.(*sql.ColumnRef); ok && isRowIDName(cr.Name) {
			found = true
		}
	})
	return found
}

// CompoundColumnAffinity returns the affinity of output column i of a
// compound SELECT (SQLite's compoundSelectAffinity). Exported for the
// expression evaluator's column-affinity analysis.
func (e *SelectEngine) CompoundColumnAffinity(s *sql.SelectStmt, i int) rune {
	return e.compoundColumnAffinity(s, i)
}
