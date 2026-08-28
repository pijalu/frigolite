// Package exec implements query execution.
package execquery

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// This file owns column name resolution, qualified-star expansion,
// ORDER BY row comparison, and PK column identification for SELECT
// execution. Extracted from select.go for file-level SRP.

// tableColumnNames returns the column names of a table (or view), resolving
// schema entries by name. For a view, columns whose alias starts with
// "__hidden__" are excluded from a bare * expansion (SQLite's hidden-column
// feature: they remain usable by qualified/trigger references but do not
// appear in SELECT *).
// viewFallbackColumnNames resolves column names when tableName refers to a
// view rather than a table: declared column list if present, otherwise derived
// from the view's SELECT body with hidden columns filtered out.
func (e *SelectEngine) viewFallbackColumnNames(tableName string) ([]string, error) {
	v, verr := e.ctx.MainDB().Schema.FindView(tableName)
	if verr != nil || v == nil {
		if e.expandingView && !e.expandingTempView {
			return nil, fmt.Errorf("no such table: main.%s", tableName)
		}
		return nil, fmt.Errorf("no such table: %s", tableName)
	}
	if declared := ViewDeclaredColumns(v.SQL); len(declared) > 0 {
		return declared, nil
	}
	names, _ := e.viewSelectColumnNames(v)
	var visible []string
	for _, n := range names {
		if !strings.HasPrefix(n, "__hidden__") {
			visible = append(visible, n)
		}
	}
	return visible, nil
}

// eponymousModuleColumnNames reports the visible (non-hidden) column names of
// an eponymous vtab module's implicit instance. ok is false when tableName
// does not name an eponymous module.
func (e *SelectEngine) eponymousModuleColumnNames(tableName string) ([]string, bool) {
	name := strings.ToLower(tableName)
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:]
	}
	module, ok := e.ctx.VTables().Find(name)
	if !ok || !vtab.ModuleIsEponymous(module) {
		return nil, false
	}
	vt, err := module.Create(nil)
	if err != nil {
		return nil, false
	}
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil, false
	}
	var hidden map[int]bool
	if hc, ok := vt.(vtab.HiddenColumnInfo); ok {
		hidden = hc.HiddenColumns()
	}
	var names []string
	for i, c := range ci.Columns() {
		if !hidden[i] {
			names = append(names, c)
		}
	}
	return names, true
}

func (e *SelectEngine) tableColumnNames(tableName string) ([]string, error) {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		// Eponymous vtab module implicit instances (generate_series,
		// tabfunc01-900: UNION ALL members project * against the module's
		// declared schema). Hidden columns are excluded from *.
		if names, ok := e.eponymousModuleColumnNames(tableName); ok {
			return names, nil
		}
		fallback, ferr := e.viewFallbackColumnNames(tableName)
		return fallback, ferr
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	names := make([]string, 0, len(colDefs))
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		names = append(names, cd.Name)
	}
	return names, nil
}

// compoundStarCount counts the columns produced by a * expansion in a
// compound SELECT member, including every joined table's columns (with
// USING/NATURAL merge deduction).
// joinMergeDeduction returns how many columns to subtract from the star count
// because a USING/NATURAL join merges matching column names from the right
// side into the left side's columns.
func joinMergeDeduction(leftColNames, jcols []string, j sql.JoinClause) int {
	if len(j.Using) == 0 && !isNaturalJoinType(j.JoinType) {
		return 0
	}
	leftNames := map[string]bool{}
	for _, c := range leftColNames {
		leftNames[c] = true
	}
	deduction := 0
	for _, c := range jcols {
		if leftNames[c] {
			deduction++
		}
	}
	return deduction
}

func (e *SelectEngine) compoundStarCount(s *sql.SelectStmt, ref *sql.ColumnRef) (int, error) {
	if ref.Table != "" {
		cols, err := e.resolveTableColumnNames(s, ref.Table)
		if err != nil {
			return 0, err
		}
		return len(cols), nil
	}
	if s.From.Subquery != nil {
		return e.compoundSelectColCount(s.From.Subquery)
	}
	if s.From.Name == "" {
		return 0, fmt.Errorf("no tables specified")
	}
	cols, err := e.resolveTableColumnNames(s, s.From.Name)
	if err != nil {
		return 0, err
	}
	n := len(cols)
	leftColNames := cols
	for _, j := range s.Joins {
		jcols, err := e.compoundJoinColNames(s, j)
		if err != nil {
			return 0, err
		}
		n += len(jcols) - joinMergeDeduction(leftColNames, jcols, j)
		leftColNames = append(leftColNames, jcols...)
	}
	return n, nil
}

// compoundJoinColNames resolves the column names of a compound member's join
// operand (subquery or table), expanding * through the subquery's sources.
// subqueryJoinColNames derives column names from a subquery join operand's
// SELECT list, expanding bare * through the subquery's FROM sources.
func (e *SelectEngine) subqueryJoinColNames(sub *sql.SelectStmt) []string {
	var jcols []string
	for _, col := range sub.Columns {
		if col.As != "" {
			jcols = append(jcols, col.As)
		} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			jcols = append(jcols, ref.Name)
		} else {
			jcols = append(jcols, "")
		}
	}
	// If the subquery is a bare * (e.g. SELECT * FROM t13), expand
	// through all its FROM sources (joins included).
	var expanded []string
	for _, n := range jcols {
		if n == "*" {
			for _, cd := range e.ctx.ViewColumnDefsFromSelect(sub) {
				expanded = append(expanded, cd.Name)
			}
		} else {
			expanded = append(expanded, n)
		}
	}
	return expanded
}

func (e *SelectEngine) compoundJoinColNames(s *sql.SelectStmt, j sql.JoinClause) ([]string, error) {
	if j.Table.Subquery == nil {
		return e.resolveTableColumnNames(s, j.Table.Name)
	}
	subCols, err := e.compoundSelectColCount(j.Table.Subquery)
	if err != nil {
		return nil, err
	}
	if expanded := e.subqueryJoinColNames(j.Table.Subquery); len(expanded) > 0 {
		return expanded, nil
	}
	var jcols []string
	for i := 0; i < subCols; i++ {
		jcols = append(jcols, fmt.Sprintf("_c%d", i))
	}
	return jcols, nil
}

// buildQualifiedStarNames expands a qualified star reference (t.*) into the
// table's column names, using the colDefs prefixed names when present (a JOIN
// alias shadows a same-named real table) and the schema column list otherwise.
// schemaFallbackNames derives qualified-star names from colDefs when the
// schema lookup returns nothing (prefixed defs for this operand plus
// unprefixed defs that belong to the first operand in a join).
func schemaFallbackNames(colDefs []sql.ColumnDef, tableRef string) []string {
	var names []string
	for _, cd := range colDefs {
		if cd.Dropped || cd.Hidden {
			continue
		}
		if strings.HasPrefix(cd.Name, tableRef+".") {
			names = append(names, strings.TrimPrefix(cd.Name, tableRef+"."))
		} else if !strings.Contains(cd.Name, ".") {
			names = append(names, cd.Name)
		}
	}
	return names
}

// matchPrefixedColName returns the table-qualified name when the column
// conflicted with a same-named column (colDefs stores it as table.col),
// otherwise returns the bare name to keep result column names unique.
func matchPrefixedColName(colDefs []sql.ColumnDef, prefixed, bare string) string {
	for _, cd := range colDefs {
		if cd.Name == prefixed {
			return prefixed
		}
	}
	return bare
}

func (e *SelectEngine) buildQualifiedStarNames(ref *sql.ColumnRef, colDefs []sql.ColumnDef, sel *sql.SelectStmt) []string {
	refTable := ref.Table
	if sel != nil {
		if t := selectAliasTarget(sel, ref.Table); t != "" {
			refTable = t
		}
	}
	schemaNames, _ := e.tableColumnNames(refTable)
	if len(schemaNames) == 0 {
		schemaNames = schemaFallbackNames(colDefs, ref.Table)
	}
	var tblNames []string
	hidden := e.hiddenColumnNames(refTable)
	for _, n := range schemaNames {
		// Hidden columns (FTS table-name/docid aliases) never expand in t.*.
		if hidden[n] {
			continue
		}
		prefixed := refTable + "." + n
		tblNames = append(tblNames, matchPrefixedColName(colDefs, prefixed, n))
	}
	return tblNames
}

// hiddenColumnNames returns the set of hidden column names of a table (FTS
// tables expose hidden table-name/docid aliases; they are readable by explicit
// reference but excluded from * expansion and PRAGMA table_info).
func (e *SelectEngine) hiddenColumnNames(tableName string) map[string]bool {
	hidden := map[string]bool{}
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return hidden
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if cd.Hidden {
			hidden[cd.Name] = true
		}
	}
	return hidden
}

// skipQualifiedStarName reports whether a qualified-star column name (the
// part after the table prefix) must be excluded from t.* expansion: the rowid
// pseudo-columns, the FTS hidden docid alias, and the reserved positional key.
func skipQualifiedStarName(n string) bool {
	return n == "rowid" || n == "_rowid_" || n == "oid" || n == "docid" || n == positionalRowKey
}

// uniqueKeys returns deduplicated keys from a string-keyed map in a
// deterministic-enough order for star expansion.
func uniqueKeys(getKeys func() []string) []string {
	keys := getKeys()
	seen := make(map[string]bool)
	var result []string
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			result = append(result, k)
		}
	}
	return result
}

// qualifiedStarNamesFromRow derives t.* column names from the row's qualified
// keys (RowMap alias.col, including pseudo-columns — the last-resort path) or
// a StructRow's index (unprefixed column names).
func qualifiedStarNamesFromRow(row Row, tableRef string) []string {
	if rm, ok := row.(RowMap); ok {
		prefix := tableRef + "."
		return uniqueKeys(func() []string {
			var keys []string
			for k := range rm {
				if strings.HasPrefix(k, prefix) && !skipQualifiedStarName(k[len(prefix):]) {
					keys = append(keys, strings.TrimPrefix(k, prefix))
				}
			}
			return keys
		})
	}
	if sr, ok := row.(*StructRow); ok {
		return uniqueKeys(func() []string {
			var keys []string
			for n := range sr.Index {
				keys = append(keys, n)
			}
			return keys
		})
	}
	return nil
}

// resolveColumnRefName resolves an unaliased column reference to its result
// column name: the RESOLVED column (declared name / subquery alias) matched
// case-insensitively, with the table qualifier stripped unless
// full_column_names is on or the column conflicts.
// matchUnqualifiedColDef finds the resolved column name for an unqualified
// reference by matching the bare name or a table-qualified def (table.col)
// case-insensitively. Returns the name and true if found.
func matchUnqualifiedColDef(colDefs []sql.ColumnDef, refName string) (string, bool) {
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		if strings.EqualFold(cd.Name, refName) {
			return cd.Name, true
		}
		if dot := strings.Index(cd.Name, "."); dot >= 0 && strings.EqualFold(cd.Name[dot+1:], refName) {
			return cd.Name[dot+1:], true
		}
	}
	return "", false
}

func (e *SelectEngine) resolveColumnRefName(ref *sql.ColumnRef, colDefs []sql.ColumnDef) string {
	if ref.Table == "" {
		if name, ok := matchUnqualifiedColDef(colDefs, ref.Name); ok {
			return name
		}
		return ref.Name
	}
	if e.ctx.FullColumnNames() {
		return ref.Name
	}
	// Qualified reference with full_column_names=OFF: strip the
	// table qualifier unless the column conflicts (join colDefs
	// store conflicting columns as table.col — keep those).
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		if dot := strings.Index(cd.Name, "."); dot >= 0 && strings.EqualFold(cd.Name[dot+1:], ref.Name) {
			return ref.Name
		}
	}
	return ref.Name
}

// orderQualifiedNamesByDefs reorders qualified star names to match the
// deterministic colDefs order when the alias maps to the join operand's real
// column names (colDefs is deterministic; map iteration is not).
func orderQualifiedNamesByDefs(colDefs []sql.ColumnDef, tableRef string, names []string) []string {
	if len(colDefs) == 0 {
		return names
	}
	prefix := tableRef + "."
	seen := make(map[string]bool)
	var ordered []string
	for _, cd := range colDefs {
		if !strings.HasPrefix(cd.Name, prefix) && strings.Contains(cd.Name, ".") {
			continue
		}
		n := strings.TrimPrefix(cd.Name, prefix)
		if seen[n] || !containsString(names, n) {
			continue
		}
		seen[n] = true
		ordered = append(ordered, n)
	}
	if len(ordered) == len(names) {
		return ordered
	}
	return names
}

// pkColumnNames extracts the column names for the PRIMARY KEY of a
// WITHOUT ROWID table from the CREATE TABLE SQL. It supports both
// column-level PK (e.g., "a INTEGER PRIMARY KEY") and table-level PK
// (e.g., "PRIMARY KEY(c,a)"). Returns the names in PK order.
// parseTableLevelPK extracts column names from a table-level "PRIMARY KEY(...)"
// clause in CREATE TABLE SQL. Returns nil when no table-level PK is found.
func parseTableLevelPK(createSQL string) []string {
	upper := strings.ToUpper(createSQL)
	pkStart := strings.Index(upper, "PRIMARY KEY")
	if pkStart < 0 {
		pkStart = strings.Index(upper, "PRIMARY  KEY")
	}
	if pkStart < 0 {
		return nil
	}
	parenStart := strings.Index(createSQL[pkStart:], "(")
	if parenStart < 0 {
		return nil
	}
	parenStart += pkStart
	parenEnd := strings.Index(createSQL[parenStart:], ")")
	if parenEnd < 0 {
		return nil
	}
	parenEnd += parenStart
	var result []string
	for _, cn := range strings.Split(createSQL[parenStart+1:parenEnd], ",") {
		fields := strings.Fields(strings.TrimSpace(cn))
		if len(fields) > 0 {
			result = append(result, fields[0])
		}
	}
	return result
}

func PKColumnNames(createSQL string, colDefs []sql.ColumnDef) []string {
	if result := parseTableLevelPK(createSQL); len(result) > 0 {
		return result
	}
	for _, cd := range colDefs {
		if cd.PrimaryKey {
			return []string{cd.Name}
		}
	}
	return nil
}

// lessRows returns true if row i should come before row j according to ORDER BY.
// resultCols maps ORDER BY aliases/column names to result column positions.
// normalizeOrderByExpr unwraps UnaryOp(+/-) over a NumericLit and strips all
// COLLATE operators so the underlying positional or column-ref operand is
// visible. The term's collation is applied later by compareOrderByValues.
func normalizeOrderByExpr(obExpr sql.Expr) sql.Expr {
	if uo, ok := obExpr.(*sql.UnaryOp); ok && (uo.Operator == "+" || uo.Operator == "-") {
		if num, ok := uo.Operand.(*sql.NumericLit); ok {
			if uo.Operator == "-" {
				return &sql.NumericLit{Value: "-" + num.Value}
			}
			return num
		}
	}
	for {
		prev := obExpr
		obExpr = stripCollate(obExpr)
		if obExpr == prev {
			break
		}
	}
	return obExpr
}

// resolveOrderByValue resolves an ORDER BY expression to a value in the output
// row at position idx. Returns false when neither positional nor column-name
// resolution applies (caller should fall back to evaluating the raw expression).
func resolveOrderByValue(obExpr sql.Expr, rows [][]interface{}, resultCols []string, idx int) (interface{}, bool) {
	if nl, ok := obExpr.(*sql.NumericLit); ok {
		if pos, err := strconv.ParseInt(nl.Value, 10, 64); err == nil && pos >= 1 && pos <= int64(len(rows[idx])) {
			return rows[idx][pos-1], true
		}
	}
	if ref, ok := stripCollate(obExpr).(*sql.ColumnRef); ok {
		if pos := resultColumnIndex(resultCols, ref.Name); pos >= 0 && pos < len(rows[idx]) {
			return rows[idx][pos], true
		}
	}
	return nil, false
}

func (e *SelectEngine) lessRows(orderBy []sql.OrderByTerm, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) bool {
	for _, ob := range orderBy {
		cmp := e.compareOrderByTerm(ob, rowMaps, rows, resultCols, i, j)
		if cmp < 0 {
			return true
		}
		if cmp > 0 {
			return false
		}
	}
	return false
}

// compareOrderByTerm compares rows i and j for a single ORDER BY term,
// resolving alias references, applying the column's declared collation via
// the row maps, and falling back to expression evaluation when a value is
// missing from the output row.
func (e *SelectEngine) compareOrderByTerm(ob sql.OrderByTerm, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) int {
	obExpr := normalizeOrderByExpr(ob.Expr)
	ref, isRef := stripCollate(obExpr).(*sql.ColumnRef)
	if !isRef || ref.Table != "" || ref.Name == "*" {
		return e.compareOrderByFallback(ob, obExpr, rowMaps, rows, resultCols, i, j)
	}
	left, lok := resolveOrderByValue(obExpr, rows, resultCols, i)
	right, rok := resolveOrderByValue(obExpr, rows, resultCols, j)
	left, right = e.resolveOrderByRowValues(ref.Name, obExpr, rowMaps, rows, resultCols, i, j, left, right)
	if !lok || !rok {
		return e.compareOrderByFallback(ob, obExpr, rowMaps, rows, resultCols, i, j)
	}
	return e.compareOrderByValues(left, right, ob)
}

// resolveOrderByRowValues overrides the output-row values for an unqualified
// column-reference ORDER BY term with the row-map values (which carry the
// column's declared collation), resolving a SELECT-list alias to the aliased
// column first.
func (e *SelectEngine) resolveOrderByRowValues(name string, obExpr sql.Expr, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int, left, right interface{}) (interface{}, interface{}) {
	// A SELECT-list alias in ORDER BY shadows a same-named real column
	// (SQLite: "SELECT z AS x, x AS z FROM d1 ORDER BY z" sorts by the
	// aliased x value, not the real z column).
	resolvedName := ""
	_, isAlias := e.aliasStackTop(name)
	if isAlias {
		if aliasExpr, ok := e.resolveAliasRef(name); ok {
			if aliasRef, ok2 := stripCollate(aliasExpr).(*sql.ColumnRef); ok2 && aliasRef.Table == "" {
				resolvedName = aliasRef.Name
			}
		}
	}
	// A plain unqualified column reference carries its declared collation on
	// the row-map value (a CollatedValue marker); the output-row value loses
	// it. Prefer the row map so ORDER BY honors the column's collation
	// (d4.x COLLATE nocase).
	// When the ORDER BY name is a SELECT-list alias whose expression is not a
	// plain column reference (e.g. SELECT substr(m,2) AS m ... ORDER BY m),
	// the row map would wrongly grab a same-named source column; SQLite
	// resolves the bare alias to the output value instead (resolver01-4.1).
	if isAlias && resolvedName == "" {
		return left, right
	}
	rowName := name
	if resolvedName != "" {
		// The name is an alias; the value comes from the aliased column's
		// position in the output row.
		rowName = resolvedName
		if pos := resultColumnIndex(resultCols, name); pos >= 0 && pos < len(rows[i]) {
			left = rows[i][pos]
		}
		if pos := resultColumnIndex(resultCols, name); pos >= 0 && pos < len(rows[j]) {
			right = rows[j][pos]
		}
	}
	if lm, ok := rowMaps[i].Get(rowName); ok {
		left = lm
	}
	if rm, ok := rowMaps[j].Get(rowName); ok {
		right = rm
	}
	return left, right
}

// compareOrderByFallback compares rows i and j for an ORDER BY term that is
// not a simple unqualified column reference (or whose output-row values are
// missing): evaluate the expression against the row maps.
func (e *SelectEngine) compareOrderByFallback(ob sql.OrderByTerm, obExpr sql.Expr, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) int {
	// Resolve a non-column ORDER BY expression by its rendered text: when it
	// matches a result column name (e.g. "10+sum(a) OVER (ORDER BY a)" is the
	// SELECT-list expression), use the output row value — the window pass
	// already computed it.
	if pos := resultColumnIndex(resultCols, sql.ExprString(stripCollate(obExpr))); pos >= 0 {
		left, right := interface{}(nil), interface{}(nil)
		if i < len(rows) && pos < len(rows[i]) {
			left = rows[i][pos]
		}
		if j < len(rows) && pos < len(rows[j]) {
			right = rows[j][pos]
		}
		return e.compareOrderByValues(left, right, ob)
	}
	left, lok := resolveOrderByValue(obExpr, rows, resultCols, i)
	right, rok := resolveOrderByValue(obExpr, rows, resultCols, j)
	if !lok {
		// A window function expression in ORDER BY (e.g. RANK() OVER w) is a
		// distinct AST node from its SELECT-list twin; the window pass stores
		// its computed value under the rendered expression key.
		if v, ok := rowMaps[i].Get(sql.ExprString(ob.Expr)); ok {
			left = v
		} else {
			left, _ = e.ctx.EvalExpr(ob.Expr, rowMaps[i])
		}
	}
	if !rok {
		if v, ok := rowMaps[j].Get(sql.ExprString(ob.Expr)); ok {
			right = v
		} else {
			right, _ = e.ctx.EvalExpr(ob.Expr, rowMaps[j])
		}
	}
	return e.compareOrderByValues(left, right, ob)
}

// compareOrderByValues compares two values for an ORDER BY term, applying
// the term's direction and explicit NULLS FIRST/LAST rules. SQLite defaults:
// NULLs sort first for ASC, last for DESC; explicit NULLS FIRST/LAST win.
// The ORDER BY term's explicit COLLATE (e.g. "ORDER BY x COLLATE nocase")
// is applied when the compared values do not already carry a collation
// marker (e.g. positional/alias ORDER BY terms resolve to output values).
// nullOrderByCmp returns the comparison result when one or both ORDER BY
// values are NULL. Returns (0, false) when neither value is NULL.
func nullOrderByCmp(leftNull, rightNull bool, ob sql.OrderByTerm) (int, bool) {
	if !leftNull && !rightNull {
		return 0, false
	}
	if leftNull && rightNull {
		return 0, true
	}
	nullsFirst := ob.NullsFirst
	if ob.NullsLast {
		nullsFirst = false
	}
	if !ob.NullsFirst && !ob.NullsLast {
		nullsFirst = !ob.Desc
	}
	if leftNull {
		if nullsFirst {
			return -1, true
		}
		return 1, true
	}
	if nullsFirst {
		return 1, true
	}
	return -1, true
}

func (e *SelectEngine) compareOrderByValues(left, right interface{}, ob sql.OrderByTerm) int {
	if cmp, isNull := nullOrderByCmp(execexpr.IsSQLNull(left), execexpr.IsSQLNull(right), ob); isNull {
		return cmp
	}
	coll := orderByTermCollation(ob.Expr)
	// An ORDER BY term that names a SELECT-list alias (e.g. ORDER BY y where
	// the SELECT is "SELECT x AS y FROM d4") inherits the aliased
	// expression's collation (SELECT x COLLATE binary AS x → ORDER BY x
	// sorts binary).
	if coll == "" {
		if ref, ok := stripCollate(ob.Expr).(*sql.ColumnRef); ok && ref.Table == "" {
			if aliasExpr, ok := e.aliasStackTop(ref.Name); ok {
				coll = orderByTermCollation(aliasExpr)
			}
		}
	}
	if coll != "" {
		// An explicit COLLATE in the ORDER BY term (or one inherited from a
		// SELECT-list alias) overrides the column's declared collation:
		// SQLite sorts ORDER BY x COLLATE binary with BINARY even when the
		// column is declared COLLATE nocase. Apply the term's collation to
		// the raw values unconditionally here.
		lc, _ := extractValue(left)
		rc, _ := extractValue(right)
		cmp := e.ctx.CompareValuesCollate(lc, rc, coll)
		if ob.Desc {
			cmp = -cmp
		}
		return cmp
	}
	cmp := e.ctx.CompareValuesWithCollate(left, right)
	if ob.Desc {
		cmp = -cmp
	}
	return cmp
}

// eponymousModuleResolvable reports whether name resolves to an eponymous
// vtab module's implicit FROM source (prepare-time name resolution for
// scalar subqueries).
func (e *SelectEngine) eponymousModuleResolvable(name string) bool {
	lower := strings.ToLower(name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	module, ok := e.ctx.VTables().Find(lower)
	return ok && vtab.ModuleIsEponymous(module)
}
