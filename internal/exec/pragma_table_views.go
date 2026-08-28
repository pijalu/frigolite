package exec

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
	"strings"
)

func (e *Engine) materializeForeignKeyListWithRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "id"},
		{Name: "seq"},
		{Name: "table"},
		{Name: "from"},
		{Name: "to"},
		{Name: "on_update"},
		{Name: "on_delete"},
		{Name: "match"},
	}
	if len(ref.Args) == 0 {
		return cols, nil, nil
	}
	argVal, err := e.evalExpr(ref.Args[0], row)
	if err != nil {
		return nil, nil, err
	}
	tableName, ok := util.UnwrapColumnValue(argVal).(string)
	if !ok {
		return nil, nil, fmt.Errorf("wrong type for argument of %s(): expected string", ref.Name)
	}
	// Optional second argument: schema name (SQLite pragma_foreign_key_list
	// accepts (table, schema)). Resolve the table within that schema.
	if len(ref.Args) >= 2 {
		schemaVal, err := e.evalExpr(ref.Args[1], row)
		if err != nil {
			return nil, nil, err
		}
		if schema, ok := util.UnwrapColumnValue(schemaVal).(string); ok && schema != "" {
			tableName = schema + "." + tableName
		}
	}
	res := e.execPragmaForeignKeyList(tableName)
	if res.Error != nil {
		return nil, nil, res.Error
	}
	return cols, res.Rows, nil
}

// materializeForeignKeyCheckWithRow builds the rows of
// pragma_foreign_key_check (table-valued PRAGMA foreign_key_check) with a row
// context for column-reference arguments. Arguments are the optional child
// table name and optional schema name; the schema argument restricts the
// child lookup to that schema (and errors when the table is not found there,
// matching SQLite).
func (e *Engine) materializeForeignKeyCheckWithRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "table"},
		{Name: "rowid"},
		{Name: "parent"},
		{Name: "fkid"},
	}
	var tableName, schemaName string
	for _, a := range ref.Args {
		v, err := e.evalExpr(a, row)
		if err != nil {
			return nil, nil, err
		}
		s, ok := util.UnwrapColumnValue(v).(string)
		if !ok {
			return nil, nil, fmt.Errorf("wrong type for argument of pragma_foreign_key_check(): expected string")
		}
		if tableName == "" {
			tableName = s
		} else if schemaName == "" {
			schemaName = s
		}
	}
	viols, err := e.constraints.FindFKViolations(tableName, schemaName)
	if err != nil {
		// In a correlated join (pragma_foreign_key_check(name) against
		// sqlite_schema) a non-table name (an index, a dropped object) yields
		// no rows rather than an error; the standalone call with a schema
		// qualifier still errors (fkey5-13.1).
		if pragmaArgsCorrelated(ref) && strings.Contains(err.Error(), "no such table") {
			return cols, nil, nil
		}
		return nil, nil, err
	}
	rows := make([][]interface{}, 0, len(viols))
	for _, v := range viols {
		rows = append(rows, []interface{}{v.ChildTable, v.RowID, v.ParentTable, int64(v.FKID)})
	}
	return cols, rows, nil
}

// materializeTableList builds the rows of pragma_table_list. When called with
// a table name argument, it returns one row for that table. Without an
// argument, it returns one row for every table in the schema.
// Columns: schema, name, type, ncol, wr (without rowid), strict.
func (e *Engine) materializeTableList(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	// Note: "strict" is a SQL keyword, so the LALR parser uppercases it.
	// But with case-restoration for TK_ID fallback, the original case is
	// preserved. We use lowercase to match SQLite's pragma_table_list output.
	cols := []sql.ColumnDef{
		{Name: "schema"},
		{Name: "name"},
		{Name: "type"},
		{Name: "ncol"},
		{Name: "wr"},
		{Name: "strict"},
	}

	entries, err := e.mainDB.Schema.GetEntries(schema.TypeTable)
	if err != nil {
		return nil, nil, err
	}

	// If an argument is provided, filter to that table name.
	filterName, err := tableListFilter(e, ref)
	if err != nil {
		return nil, nil, err
	}

	var rows [][]interface{}
	// Views first, in creation order, with ncol=0 (the old SQLite behavior
	// this test suite targets; modern SQLite reports the view's column
	// count).
	viewEntries, err := e.mainDB.Schema.GetEntries(schema.TypeView)
	if err == nil {
		for _, entry := range viewEntries {
			if row := tableViewListRow(entry, filterName); row != nil {
				rows = append(rows, row)
			}
		}
	}

	// Tables in reverse creation order, then the sqlite_schema bootstrap
	// entries.
	for i := len(entries) - 1; i >= 0; i-- {
		if row := e.tableListRow(entries[i], filterName); row != nil {
			rows = append(rows, row)
		}
		// Shadow rows: a virtual-table entry contributes its backing tables
		// right after its own row (pragma.c tableList emits each module's
		// xShadowName results). Only modules frigolite implements with
		// rtree-style %_suffix shadows are recognized.
		for _, sh := range rtreeShadowRows(entries[i], filterName) {
			rows = append(rows, sh)
		}
	}

	// sqlite_schema and sqlite_temp_schema (5 columns each).
	if filterName == "" || filterName == "sqlite_schema" {
		rows = append(rows, []interface{}{"main", "sqlite_schema", "table", int64(5), int64(0), int64(0)})
	}
	if filterName == "" || filterName == "sqlite_temp_schema" {
		rows = append(rows, []interface{}{"temp", "sqlite_temp_schema", "table", int64(5), int64(0), int64(0)})
	}
	return cols, rows, nil
}

// tableListFilter resolves pragma_table_list's optional table-name argument.
func tableListFilter(e *Engine, ref sql.TableRef) (string, error) {
	if len(ref.Args) == 0 {
		return "", nil
	}
	argVal, err := e.evalExpr(ref.Args[0], nil)
	if err != nil {
		return "", err
	}
	s, _ := argVal.(string)
	return s, nil
}

// rtreeShadowRows returns the pragma_table_list rows for an rtree/rtree_i32
// virtual table's three shadow tables (<name>_node/_rowid/_parent), or nil
// when entry is not such a vtab or the name filter excludes the vtab.
// Shape mirrors sqlite3 output: type='shadow', ncol=2 (nodeno/data etc.).
func rtreeShadowRows(entry *schema.Entry, filterName string) [][]interface{} {
	if entry == nil || entry.Type != schema.TypeTable {
		return nil
	}
	upper := strings.ToUpper(entry.SQL)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return nil
	}
	module := ""
	for _, c := range strings.TrimSpace(entry.SQL[idx+len(" USING "):]) {
		if c == '(' || c == ' ' {
			break
		}
		module += string(c)
	}
	module = strings.ToLower(module)
	if module != "rtree" && module != "rtree_i32" {
		return nil
	}
	if filterName != "" && entry.Name != filterName {
		return nil
	}
	out := make([][]interface{}, 0, 3)
	for _, suffix := range []string{"_node", "_rowid", "_parent"} {
		out = append(out, []interface{}{"main", entry.Name + suffix, "shadow", int64(2), int64(0), int64(0)})
	}
	return out
}

// tableViewListRow builds the pragma_table_list row for a view, or nil when
// the filter excludes it.
func tableViewListRow(entry *schema.Entry, filterName string) []interface{} {
	if filterName != "" && entry.Name != filterName {
		return nil
	}
	return []interface{}{"main", entry.Name, "view", int64(0), int64(0), int64(0)}
}

// tableListRow builds the pragma_table_list row for a table, or nil when the
// filter excludes it.
func (e *Engine) tableListRow(entry *schema.Entry, filterName string) []interface{} {
	if entry.Type != schema.TypeTable {
		return nil
	}
	if filterName != "" && entry.Name != filterName {
		return nil
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	wr := int64(0)
	if hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		wr = 1
	}
	strict := int64(0)
	if hasStrictKeyword(strings.ToUpper(entry.SQL)) {
		strict = 1
	}
	return []interface{}{
		"main", entry.Name, string(entry.Type),
		int64(len(colDefs)), wr, strict,
	}
}

// viewColumnDefs derives the column names and declared types of a view from
// its stored SELECT statement, mirroring SQLite's view column typing
// (sqlite3SubqueryColumnTypes in select.c).
func (e *Engine) viewColumnDefs(viewEntry *schema.Entry) ([]sql.ColumnDef, error) {
	return e.viewColumnDefsGuard(viewEntry, nil)
}

// viewColumnDefsGuard is viewColumnDefs with a recursion guard: resolving is
// the set of view names currently being expanded (to break cycles such as
// CREATE VIEW v1 AS SELECT * FROM v2; CREATE VIEW v2 AS SELECT * FROM v1).
func (e *Engine) viewColumnDefsGuard(viewEntry *schema.Entry, resolving map[string]bool) ([]sql.ColumnDef, error) {
	// Cache the computed defs so deeply nested view chains (e.g. view3's
	// exponential UNION doubling) resolve each view once instead of re-expanding
	// the whole subtree on every reference (SQLite memoizes view column names
	// on the view object after first resolution).
	if defs, ok := e.caches.viewDefCache[viewEntry.Name]; ok {
		return defs, nil
	}
	if resolving[viewEntry.Name] {
		// Circular view reference: fall back to the declared column list when
		// present, otherwise return no defs (the execution path reports the
		// circular reference).
		return nil, fmt.Errorf("exec: circular view reference: %s", viewEntry.Name)
	}
	if resolving == nil {
		resolving = map[string]bool{}
	}
	resolving[viewEntry.Name] = true
	defer delete(resolving, viewEntry.Name)
	sqlStr := viewEntry.SQL
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS")
	if idx < 0 {
		return nil, fmt.Errorf("exec: invalid view SQL: %s", sqlStr)
	}
	// Skip past " AS" plus any following whitespace (the body may begin on
	// the same line or the next line).
	body := sqlStr[idx+3:]
	body = strings.TrimLeft(body, " \t\r\n")
	stmts, err := parse.ParseSQL(body)
	if err != nil || len(stmts) == 0 {
		return nil, fmt.Errorf("exec: view parse error: %v", err)
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return nil, fmt.Errorf("exec: view does not contain SELECT")
	}
	defs := e.viewColumnDefsFromSelectGuard(sel, resolving)
	// A declared column list must match the SELECT's result width; SQLite
	// raises "expected N columns for 'view' but got M" at view use time.
	if declared := viewDeclaredColumns(sqlStr); len(declared) > 0 && len(declared) != len(defs) {
		return nil, fmt.Errorf("expected %d columns for '%s' but got %d", len(declared), viewEntry.Name, len(defs))
	}
	// Only cache fully computed defs (the circular guard above prevents
	// storing a view mid-expansion).
	e.caches.viewDefCache[viewEntry.Name] = defs
	return defs, nil
}

// viewColumnDefsFromSelect computes column definitions for a view's SELECT.
// Column names come from aliases or column references; declared types follow
// SQLite's expression-based type inference.
func (e *Engine) viewColumnDefsFromSelect(sel *sql.SelectStmt) []sql.ColumnDef {
	return e.viewColumnDefsFromSelectGuard(sel, nil)
}

// viewColumnDefsFromSelectGuard is viewColumnDefsFromSelect with a recursion
// guard threaded through nested view/subquery column resolution.
func (e *Engine) viewColumnDefsFromSelectGuard(sel *sql.SelectStmt, resolving map[string]bool) []sql.ColumnDef {
	// Resolve the FROM sources (base table plus each JOIN operand) so bare "*"
	// can expand to every column, and so column references can inherit types.
	srcDefs := e.viewSourceDefs(sel, resolving)
	var defs []sql.ColumnDef
	for i, col := range sel.Columns {
		if star := e.viewStarDefs(col, sel, srcDefs, resolving); star != nil {
			defs = append(defs, star...)
			continue
		}
		defs = append(defs, sql.ColumnDef{
			Name:    viewColumnName(col),
			Type:    e.viewColumnType(sel, i, srcDefs),
			Collate: viewColumnCollation(col, sel, i, srcDefs),
		})
	}
	return defs
}

// viewSourceDefs collects the column definitions of every FROM source (the
// base table plus each JOIN operand).
func (e *Engine) viewSourceDefs(sel *sql.SelectStmt, resolving map[string]bool) []sql.ColumnDef {
	srcDefs := e.fromSourceColumnDefsGuard(sel.From, resolving)
	for _, j := range sel.Joins {
		srcDefs = append(srcDefs, e.fromSourceColumnDefsGuard(j.Table, resolving)...)
	}
	return srcDefs
}

// viewStarDefs expands a star expression (bare * or qualified t.*) to the
// source columns, or returns nil when col is not a star. For a bare * in a
// compound SELECT (UNION/INTERSECT/EXCEPT), the expanded columns keep the
// LEFT-MOST member's column NAMES but their AFFINITY is the compound affinity
// (SQLite's selectAddColumnTypeAndCollation: when the corresponding columns
// of the members disagree, the result column affinity becomes BLOB).
func (e *Engine) viewStarDefs(col sql.SelectColumn, sel *sql.SelectStmt, srcDefs []sql.ColumnDef, resolving map[string]bool) []sql.ColumnDef {
	ref, ok := col.Expr.(*sql.ColumnRef)
	if !ok || ref.Name != "*" {
		return nil
	}
	var expand []sql.ColumnDef
	if ref.Table == "" {
		// A bare * expands to every column of the FROM sources (a single
		// table, or a joined result).
		expand = srcDefs
	} else {
		// A qualified star (t.* / alias.*) expands to the referenced source's
		// columns in order (SQLite: SELECT episode.*, files.f ...).
		expand = e.qualifiedSourceColumnDefs(ref.Table, sel, srcDefs, resolving)
	}
	defs := make([]sql.ColumnDef, 0, len(expand))
	for i, sd := range expand {
		def := sql.ColumnDef{Name: sd.Name, Type: sd.Type, Collate: sd.Collate}
		// Compound members disagreeing on column i's affinity force BLOB on
		// the result column (SQLite compoundSelectAffinity). Only the first
		// member's defs are in srcDefs; the compound affinity is computed
		// across every member via the select engine.
		if sel.Union != nil {
			if aff := e.compoundViewColumnAffinity(sel, i); aff != 0 {
				def.Type = compoundAffinityTypeName(aff)
			}
		}
		defs = append(defs, def)
	}
	return defs
}

// compoundViewColumnAffinity computes the result-column affinity of compound
// member column i, expanding each member's star against its own FROM sources
// (SQLite's selectAddColumnTypeAndCollation / compoundSelectAffinity): when
// the corresponding columns disagree on affinity (TEXT vs numeric family),
// the result column affinity is BLOB; otherwise the most specific non-BLOB
// affinity wins (INTEGER > REAL > NUMERIC > TEXT).
func (e *Engine) compoundViewColumnAffinity(sel *sql.SelectStmt, i int) rune {
	hasText := false
	hasNumeric := false
	best := rune(0)
	for cur := sel; cur != nil; cur = cur.Union {
		aff := e.compoundMemberExprAffinity(cur, i)
		switch aff {
		case 'T':
			hasText = true
		case 'I', 'R', 'N':
			hasNumeric = true
		}
		if viewAffinityPrecedence(aff) > viewAffinityPrecedence(best) {
			best = aff
		}
	}
	if hasText && hasNumeric {
		return 'B'
	}
	return best
}

// compoundMemberExprAffinity returns the affinity of compound member cur's
// output column i, expanding a star expression against the member's own FROM
// sources so `SELECT * FROM t` contributes the source column's declared
// affinity.
func (e *Engine) compoundMemberExprAffinity(cur *sql.SelectStmt, i int) rune {
	if cur == nil || i < 0 {
		return 0
	}
	// Walk the member's star-expanded columns; a star at a position before
	// column i contributes its source defs (the star may cover several
	// columns).
	colIdx := 0
	for _, col := range cur.Columns {
		ref, isStar := col.Expr.(*sql.ColumnRef)
		if isStar && ref.Name == "*" {
			srcDefs := e.compoundMemberSources(cur)
			var expanded []sql.ColumnDef
			if ref.Table == "" {
				expanded = srcDefs
			} else {
				expanded = e.compoundQualifiedSources(ref.Table, cur, srcDefs)
			}
			if i < colIdx+len(expanded) {
				sd := expanded[i-colIdx]
				return value.Affinity(sd.Type)
			}
			colIdx += len(expanded)
			continue
		}
		if colIdx == i {
			return e.exprAffinity(col.Expr, e.compoundMemberSources(cur))
		}
		colIdx++
	}
	return 0
}

// compoundMemberSources returns the FROM-source column defs of a compound
// member (base table plus JOIN operands), mirroring viewSourceDefs.
func (e *Engine) compoundMemberSources(cur *sql.SelectStmt) []sql.ColumnDef {
	if cur == nil {
		return nil
	}
	var defs []sql.ColumnDef
	defs = append(defs, e.fromSourceColumnDefsGuard(cur.From, nil)...)
	for _, j := range cur.Joins {
		defs = append(defs, e.fromSourceColumnDefsGuard(j.Table, nil)...)
	}
	return defs
}

// compoundQualifiedSources resolves a qualified star (t.*) in a compound
// member against the member's FROM sources, mirroring qualifiedSourceColumnDefs.
func (e *Engine) compoundQualifiedSources(qualifier string, cur *sql.SelectStmt, srcDefs []sql.ColumnDef) []sql.ColumnDef {
	base := cur.From
	baseName := base.Name
	if base.As != "" {
		baseName = base.As
	}
	if baseName != "" && strings.EqualFold(baseName, qualifier) {
		return e.fromSourceColumnDefsGuard(base, nil)
	}
	for _, j := range cur.Joins {
		jName := j.Table.Name
		if j.Table.As != "" {
			jName = j.Table.As
		}
		if jName != "" && strings.EqualFold(jName, qualifier) {
			return e.fromSourceColumnDefsGuard(j.Table, nil)
		}
	}
	return srcDefs
}

// viewAffinityPrecedence orders affinities so the most specific column
// affinity wins when merging compound members: INTEGER > REAL > NUMERIC >
// TEXT > BLOB.
func viewAffinityPrecedence(a rune) int {
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

// compoundAffinityTypeName maps a compound result column affinity to the type
// name SQLite reports for a view column (BLOB/INT/INTEGER/REAL/TEXT/NUM).
func compoundAffinityTypeName(aff rune) string {
	switch aff {
	case 'B':
		return "BLOB"
	case 'I':
		return "INT"
	case 'R':
		return "REAL"
	case 'T':
		return "TEXT"
	case 'N', 'F':
		return "NUM"
	default:
		return ""
	}
}

// viewColumnName resolves a view column's name from its alias or column
// reference.
func viewColumnName(col sql.SelectColumn) string {
	if col.As != "" {
		return col.As
	}
	if ref, ok := col.Expr.(*sql.ColumnRef); ok {
		return ref.Name
	}
	return ""
}

// viewColumnCollation resolves a view column's collation: the underlying
// column's declared collation, an explicit COLLATE on the expression, or the
// leftmost compound member with a collation.
func viewColumnCollation(col sql.SelectColumn, sel *sql.SelectStmt, i int, srcDefs []sql.ColumnDef) string {
	// Carry the declared collation of the underlying column so outer
	// comparisons use it (SQLite view/subquery column collation rules).
	if ref, ok := col.Expr.(*sql.ColumnRef); ok {
		if c := sourceColumnCollation(ref.Name, srcDefs); c != "" {
			return c
		}
	}
	// An explicit COLLATE on the expression (e.g. SELECT 'B' COLLATE
	// NOCASE) sets the view column's collation (tkt_a7debbe).
	if bop, ok := col.Expr.(*sql.BinaryOp); ok && strings.EqualFold(bop.Operator, "COLLATE") {
		if rc, ok := bop.Right.(*sql.ColumnRef); ok {
			return rc.Name
		}
		if sl, ok := bop.Right.(*sql.StringLit); ok {
			return sl.Value
		}
	}
	return compoundSourceCollation(sel, i, srcDefs)
}

// compoundSourceCollation returns the leftmost compound (UNION) member with a
// collation for column i, matching selectOutputCollations' leftmost-member
// rule.
func compoundSourceCollation(sel *sql.SelectStmt, i int, srcDefs []sql.ColumnDef) string {
	for p := sel.Union; p != nil; p = p.Union {
		if i >= len(p.Columns) {
			continue
		}
		if ref, ok := p.Columns[i].Expr.(*sql.ColumnRef); ok {
			if c := sourceColumnCollation(ref.Name, srcDefs); c != "" {
				return c
			}
		}
	}
	return ""
}

// sourceColumnCollation returns the declared collation of a column reference
// resolved against a set of source column definitions (case-insensitive), or
// "" when the column is not found or has no declared collation.
func sourceColumnCollation(name string, srcDefs []sql.ColumnDef) string {
	for _, sd := range srcDefs {
		if strings.EqualFold(sd.Name, name) {
			if sd.Collate != "" && !strings.EqualFold(sd.Collate, "BINARY") {
				return sd.Collate
			}
			return ""
		}
	}
	return ""
}

// fromSourceColumnDefsGuard returns the column definitions of a single FROM
// source (a table, view, derived-table subquery, or table-valued function),
// with a recursion guard threaded through nested view resolution.
func (e *Engine) fromSourceColumnDefsGuard(ref sql.TableRef, resolving map[string]bool) []sql.ColumnDef {
	if ref.Subquery != nil {
		return e.viewColumnDefsFromSelectGuard(ref.Subquery, resolving)
	}
	if ref.Name != "" {
		if te, _, err := e.findTable(ref.Name); err == nil {
			return e.parseColumnDefs(te.Name, te.SQL)
		}
		if ve, _, err := e.findView(ref.Name); err == nil {
			if defs, derr := e.viewColumnDefsGuard(ve, resolving); derr == nil {
				return defs
			}
		}
		if isPragmaTableFunc(ref.Name) {
			if defs, _, _, merr := e.materializeVtabTableFunc(ref, execquery.VtabScanOptions{}); merr == nil {
				return defs
			}
		}
	}
	return nil
}

// qualifiedSourceColumnDefs returns the column definitions of the FROM source
// (base table/view or a JOIN operand) referenced by a qualified star
// (t.* / alias.*). The qualifier matches the table name or its alias,
// case-insensitively.
func (e *Engine) qualifiedSourceColumnDefs(qualifier string, sel *sql.SelectStmt, srcDefs []sql.ColumnDef, resolving map[string]bool) []sql.ColumnDef {
	// Base source (FROM t or FROM t AS a): match the table name or alias.
	base := sel.From
	baseName := base.Name
	if base.As != "" {
		baseName = base.As
	}
	if baseName != "" && strings.EqualFold(baseName, qualifier) {
		return e.fromSourceColumnDefsGuard(base, resolving)
	}
	// JOIN operands: match the operand's table name or alias.
	for _, j := range sel.Joins {
		jName := j.Table.Name
		if j.Table.As != "" {
			jName = j.Table.As
		}
		if jName != "" && strings.EqualFold(jName, qualifier) {
			return e.fromSourceColumnDefsGuard(j.Table, resolving)
		}
	}
	// Fall back to the flattened source defs (single-source queries without
	// an alias match, e.g. a subquery-derived source).
	return srcDefs
}

// viewColumnType computes the declared type string of view column i, following
// sqlite3SubqueryColumnTypes: affinity of the expression is refined across
// compound (UNION / multi-row VALUES) members, then mapped to a type name.
func (e *Engine) viewColumnType(sel *sql.SelectStmt, i int, srcDefs []sql.ColumnDef) string {
	first := sel
	aff, pS2, m := e.viewCompoundAffinity(first, i, srcDefs)
	if aff == 0 {
		// No affinity: the expression (e.g. a function call) has no declared
		// affinity. SQLite's view columns default to SQLITE_AFF_NONE (not
		// BLOB) in sqlite3SubqueryColumnTypes. Return the sentinel type name
		// so downstream affinity extraction yields 0 (NONE).
		return util.AffinityNone
	}
	// Compound queries refine the affinity using the datatypes of later members.
	if isTextOrNumericAff(aff) && (pS2.Union != nil || pS2 != first) {
		aff = e.refineViewAffinity(aff, first, pS2, i, m)
	}
	zType := e.exprColumnType(first.Columns[i].Expr, srcDefs)
	return viewTypeName(zType, aff)
}

// viewCompoundAffinity walks the compound chain from first while the current
// member has no affinity, returning the first member with affinity (pS2), its
// affinity, and the OR of the datatypes of the skipped members. Bounds-check
// each member: a malformed/uneven compound (e.g. an expected-error case) must
// not panic when a later member has fewer columns.
func (e *Engine) viewCompoundAffinity(first *sql.SelectStmt, i int, srcDefs []sql.ColumnDef) (aff rune, pS2 *sql.SelectStmt, m int) {
	pS2 = first
	aff = e.exprAffinity(first.Columns[i].Expr, srcDefs)
	for aff == 0 && pS2.Union != nil {
		if i < len(pS2.Columns) {
			m |= exprDataType(pS2.Columns[i].Expr)
		}
		pS2 = pS2.Union
		if i >= len(pS2.Columns) {
			break
		}
		aff = e.exprAffinity(pS2.Columns[i].Expr, srcDefs)
	}
	return
}

// refineViewAffinity refines an affinity across the compound members after
// pS2, mirroring sqlite3SubqueryColumnTypes: a TEXT affinity meeting a numeric
// member becomes BLOB, a numeric affinity meeting a text member becomes BLOB,
// and a CAST over a compound numeric column becomes FLEXNUM.
func (e *Engine) refineViewAffinity(aff rune, first, pS2 *sql.SelectStmt, i, m int) rune {
	for p := pS2.Union; p != nil; p = p.Union {
		m |= exprDataType(p.Columns[i].Expr)
	}
	if aff == 'T' && (m&0x01) != 0 {
		aff = 'B'
	} else if isNumericAff(aff) && (m&0x02) != 0 {
		aff = 'B'
	}
	if isNumericAff(aff) && isCastExpr(first.Columns[i].Expr) {
		aff = 'F' // FLEXNUM: CAST over a compound numeric column
	}
	return aff
}

// viewTypeName maps a column's declared type and refined affinity to SQLite's
// reported type name: the declared type when its affinity matches, "NUM" for
// NUMERIC/FLEXNUM affinities, the standard type for the affinity, or "".
func viewTypeName(zType string, aff rune) string {
	if zType != "" && value.Affinity(zType) == aff {
		return zType
	}
	if aff == 'N' || aff == 'F' {
		return "NUM"
	}
	for _, st := range viewStdTypes {
		if st.aff == aff {
			return st.name
		}
	}
	return ""
}

// viewStdTypes maps an affinity to SQLite's standard type names
// (sqlite3StdType[1..] in 3.53: BLOB, INT, INTEGER, REAL, TEXT). The first
// match wins, so INTEGER affinity yields "INT".
var viewStdTypes = []struct {
	aff  rune
	name string
}{
	{'B', "BLOB"},
	{'I', "INT"},
	{'I', "INTEGER"},
	{'R', "REAL"},
	{'T', "TEXT"},
}

// exprAffinity returns the affinity of a view column expression: CAST targets
// carry the affinity of their type name, column references inherit the
// affinity of the source column's declared type, and anything else has none.
func (e *Engine) exprAffinity(expr sql.Expr, srcDefs []sql.ColumnDef) rune {
	switch x := expr.(type) {
	case *sql.CastExpr:
		return value.Affinity(x.AsType)
	case *sql.ColumnRef:
		for _, cd := range srcDefs {
			if strings.EqualFold(cd.Name, x.Name) {
				return value.Affinity(cd.Type)
			}
		}
		return 0
	default:
		return 0
	}
}

// exprColumnType returns the declared type of a view column expression, or ""
// if the expression has none. Column references inherit the source column's
// declared type; CAST and other expressions have none (mirroring SQLite's
// columnType(), which only reports types for column and subselect expressions).
func (e *Engine) exprColumnType(expr sql.Expr, srcDefs []sql.ColumnDef) string {
	switch x := expr.(type) {
	case *sql.ColumnRef:
		for _, cd := range srcDefs {
			if strings.EqualFold(cd.Name, x.Name) {
				return cd.Type
			}
		}
		return ""
	default:
		return ""
	}
}

// exprDataType returns a bitmask of possible result datatypes for an
// expression, mirroring sqlite3ExprDataType: 0x01 numeric, 0x02 text, 0x04 blob.
func exprDataType(expr sql.Expr) int {
	switch x := expr.(type) {
	case *sql.NumericLit:
		return 0x01
	case *sql.StringLit:
		return 0x02
	case *sql.BlobLit:
		return 0x04
	case *sql.CastExpr:
		return exprDataType(x.Operand)
	default:
		return 0
	}
}

// isCastExpr reports whether expr is a CAST expression.
func isCastExpr(expr sql.Expr) bool {
	_, ok := expr.(*sql.CastExpr)
	return ok
}

// isTextOrNumericAff reports whether aff is TEXT or one of the numeric
// affinities (NUMERIC, INTEGER, REAL) — SQLite's "affinity >= SQLITE_AFF_TEXT".
func isTextOrNumericAff(aff rune) bool {
	return aff == 'T' || aff == 'N' || aff == 'I' || aff == 'R'
}

// isNumericAff reports whether aff is a numeric affinity
// (NUMERIC, INTEGER, REAL) — SQLite's "affinity >= SQLITE_AFF_NUMERIC".
func isNumericAff(aff rune) bool {
	return aff == 'N' || aff == 'I' || aff == 'R'
}
