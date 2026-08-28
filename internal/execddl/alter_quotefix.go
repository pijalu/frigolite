package execddl

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

func (e *DDLExecutor) renameUpdateRelatedEntries(oldName, newName string) {
	// Always quote the new name with double quotes, matching SQLite's behavior
	// in ALTER TABLE RENAME (the replacement text is always quoted).
	quotedNew := `"` + newName + `"`
	ctx := &RenameContext{
		OldName:   oldName,
		NewName:   newName,
		QuotedNew: quotedNew,
		IsTable:   true,
	}

	// Update related entries in EVERY attached database (main, temp, and any
	// ATTACHed schema). The rename target's schema owns the entry, but child
	// tables/views/triggers referencing it may live in any database.
	for _, dbCtx := range e.ctx.Databases() {
		e.renameUpdateRelatedEntriesInSchema(dbCtx.Schema, oldName, newName, quotedNew, ctx)
	}
}

// firstQualifiedRef returns the first "alias.column" reference in a view body
// whose alias matches the given name (used for the ambiguity error message).
func firstQualifiedRef(viewSQL, alias string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(alias) + `\.([a-zA-Z_][a-zA-Z0-9_]*)`)
	if m := re.FindStringSubmatch(viewSQL); len(m) > 1 {
		return m[1]
	}
	return ""
}

// findQualifiedTableRefs finds qualified column references to tableName
// in the given SQL string (e.g., "t1.col" or "t1.a").
// Returns list of matching qualified references found.
func findQualifiedTableRefs(sql, tableName string) []string {
	if sql == "" || tableName == "" {
		return nil
	}
	// Look for patterns like "tablename." followed by an identifier
	// Use word boundary matching to avoid partial matches
	re := regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9_])` + regexp.QuoteMeta(tableName) + `\.(?P<col>[a-zA-Z_][a-zA-Z0-9_]*)`)
	matches := re.FindAllStringSubmatch(sql, -1)
	var refs []string
	for _, m := range matches {
		if len(m) >= 3 {
			// m[0] is full match, m[1] is boundary char, m[2] is column name (named group)
			refs = append(refs, tableName+"."+m[2])
		}
	}
	return refs
}

// funcNameAt reports whether the identifier at byte range [start, end) in
// sqlStr is immediately followed by "(" (allowing whitespace), i.e. it is a
// function-call name rather than a column reference.
func funcNameAt(sqlStr string, start, end int) bool {
	p := end
	for p < len(sqlStr) && (sqlStr[p] == ' ' || sqlStr[p] == '\t' || sqlStr[p] == '\n' || sqlStr[p] == '\r') {
		p++
	}
	return p < len(sqlStr) && sqlStr[p] == '('
}

// validateAddColumnConstraints evaluates a newly added column's CHECK and
// NOT NULL constraints against the table's existing rows, matching SQLite's
// ALTER TABLE ADD COLUMN semantics:
//   - A NOT NULL column with a NULL default cannot be added when the table
//     already has rows (SQLite: "Cannot add a NOT NULL column with default
//     value NULL").
//   - A CHECK constraint is evaluated against every existing row; if any row
//     makes it false the add fails with "CHECK constraint failed".

// evalColumnExpr evaluates a column's DEFAULT expression (with no row context)
// to its concrete value.
func (e *DDLExecutor) evalColumnExpr(col sql.ColumnDef) (interface{}, error) {
	if col.Default == nil {
		return nil, nil
	}
	return e.ctx.EvalExpr(col.Default, nil)
}

// tableHasRows reports whether the table currently has at least one row.
func (e *DDLExecutor) tableHasRows(entry *schema.Entry) bool {
	tree := e.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return false
	}
	return true
}

// columnHasNull reports whether any existing row of the table has NULL in the
// named column. Used by ALTER TABLE ... ALTER COLUMN ... SET NOT NULL.

// sqlHasConstraintName reports whether a CREATE TABLE SQL contains a named
// table-level or column-level constraint. Used to reject DROP CONSTRAINT for
// a non-existent name.
func sqlHasConstraintName(origSQL, constraintName string) bool {
	if constraintName == "" {
		return false
	}
	upper := strings.ToUpper(origSQL)
	upperName := strings.ToUpper(constraintName)
	re := regexp.MustCompile(`(?i)\bCONSTRAINT\s+"?` + regexp.QuoteMeta(constraintName) + `"?\b`)
	return re.MatchString(upper) || re.MatchString(origSQL) || strings.Contains(upper, "CONSTRAINT "+upperName)
}

// NULL keyword as a top-level constraint (not inside a DEFAULT string or
// CHECK expression). Used by rebuildCreateTableSQL to decide whether to
// reconstruct a column after ALTER COLUMN SET/DROP NOT NULL.
func origHasNotNull(orig string) bool {
	up := strings.ToUpper(orig)
	// Remove parenthesised groups (DEFAULT expressions, CHECK(...)) so a
	// "NOT NULL" inside them does not count as the column constraint.
	var cleaned strings.Builder
	depth := 0
	for i := 0; i < len(up); i++ {
		switch up[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				cleaned.WriteByte(up[i])
			}
		}
	}
	re := regexp.MustCompile(`\bNOT\s+NULL\b`)
	return re.MatchString(cleaned.String())
}

// isMalformedCreateTableSQL reports whether a CREATE TABLE statement is
// malformed (e.g. truncated by a writable_schema edit so the column list is
// unbalanced). SQLite reports "database disk image is malformed" when ALTER
// TABLE encounters such a schema.
func isMalformedCreateTableSQL(sqlStr string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sqlStr))
	if !strings.HasPrefix(upper, "CREATE TABLE") {
		return true
	}
	parenStart := strings.Index(sqlStr, "(")
	if parenStart < 0 {
		return true
	}
	depth := 0
	for i := parenStart; i < len(sqlStr); i++ {
		switch sqlStr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return true
			}
		}
	}
	return depth != 0
}

func (e *DDLExecutor) isVirtualTable(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	// Check if the table SQL contains "USING" which indicates a virtual table
	if strings.Contains(strings.ToUpper(entry.SQL), " USING ") {
		return true
	}
	// Check if the table's root page type is a virtual table (if available)
	return false
}

// getFTSModule finds and returns the FTS3Module for a given module name.
// Returns nil if the module is not found or is not an FTS module.
func (e *DDLExecutor) getFTSModule(moduleName string) *fts.FTS3Module {
	m, ok := e.ctx.VTables().Find(moduleName)
	if !ok {
		return nil
	}
	ftsMod, ok := m.(*fts.FTS3Module)
	if !ok {
		return nil
	}
	return ftsMod
}

// getFTSModuleForTable finds the FTS module that owns the given table.
func (e *DDLExecutor) getFTSModuleForTable(tableName string) *fts.FTS3Module {
	for _, modName := range []string{"fts3", "fts4", "fts5"} {
		ftsMod := e.getFTSModule(modName)
		if ftsMod != nil {
			if _, ok := ftsMod.GetTable(tableName); ok {
				return ftsMod
			}
		}
	}
	return nil
}

// indexColumnRefs extracts the column names referenced by a CREATE INDEX
// statement's column list (the parenthesized expressions after ON).
func indexColumnRefs(indexSQL string) []string {
	upper := strings.ToUpper(indexSQL)
	onIdx := strings.Index(upper, " ON ")
	if onIdx < 0 {
		return nil
	}
	tblStart := onIdx + 4
	tblEnd := tblStart
	for tblEnd < len(indexSQL) && indexSQL[tblEnd] != '(' {
		tblEnd++
	}
	if tblEnd >= len(indexSQL) {
		return nil
	}
	open := tblEnd + 1
	depth := 1
	i := open
	for i < len(indexSQL) && depth > 0 {
		if indexSQL[i] == '(' {
			depth++
		} else if indexSQL[i] == ')' {
			depth--
		}
		i++
	}
	if depth != 0 {
		return nil
	}
	inner := indexSQL[open : i-1]
	// Strip double-quoted tokens (DQS string literals like "c" in
	// "c"=b are not column references).
	inner = regexp.MustCompile(`"[^"]*"`).ReplaceAllString(inner, " ")
	return extractIdentifierTokens(inner)
}

// tableHasColumn reports whether the named table defines the given column.
func (e *DDLExecutor) tableHasColumn(tableName, colName string) bool {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return false
	}
	colDefs := e.ctx.ColCache()[tableName]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	}
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, colName) {
			return true
		}
	}
	return false
}

// checkIndexDependencies checks if any indexes reference the given column and returns
// an error if so. This prevents dropping columns that are used by indexes.
func (e *DDLExecutor) checkIndexDependencies(tableName, columnName string) *Result {
	entries, err := e.ctx.Schema().GetEntries(schema.TypeIndex)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		if refs, quoted := indexReferencesColumn(entry.SQL, columnName); refs {
			// SQLite re-parses each schema object with DQS OFF when validating
			// a DROP COLUMN (sqlite3_rename_test bNoDQS=1). A double-quoted
			// reference to the dropped column therefore reports the DQS-off
			// hint message; a bare or single-quoted (string-to-identifier)
			// reference reports the plain "no such column" message.
			if quoted {
				return &Result{Error: fmt.Errorf("error in index %s after drop column: no such column: \"%s\" - should this be a string literal in single-quotes?",
					entry.Name, columnName)}
			}
			return &Result{Error: fmt.Errorf("error in index %s after drop column: no such column: %s",
				entry.Name, columnName)}
		}
	}
	return nil
}

// isIdentByte reports whether b is a valid identifier character.
func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') || b == '_'
}

// quoteFixWithSchema rewrites non-column double-quoted tokens in a schema
// object's SQL to single-quoted string literals, resolving valid-identifier
// tokens against the referenced table's columns (SQLite's
// sqlite_rename_quotefix behavior: "a" stays an identifier when a is a
// column of the table, otherwise it becomes the string 'a').
func (e *DDLExecutor) quoteFixWithSchema(schemaName, sqlStr string) string {
	if sqlStr == "" {
		return ""
	}
	// Determine the main table name from the SQL (CREATE TABLE x, CREATE
	// INDEX ... ON x, CREATE TRIGGER ... ON x, SELECT ... FROM x).
	tableName := mainTableFromSchemaSQL(sqlStr)
	var colSet map[string]bool
	if tableName != "" {
		if entry, _, err := e.ctx.FindTable(tableName); err == nil {
			colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
			colSet = make(map[string]bool)
			for _, cd := range colDefs {
				colSet[strings.ToUpper(cd.Name)] = true
			}
		} else if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sqlStr)), "CREATE TABLE") {
			// The table is being created: its columns are in the statement
			// itself, so use them for identifier resolution.
			if cols := createTableColumnNames(sqlStr); len(cols) > 0 {
				colSet = make(map[string]bool)
				for _, c := range cols {
					colSet[strings.ToUpper(c)] = true
				}
			}
		}
	}
	return quoteFixSQLWithColumns(sqlStr, colSet)
}

// mainTableFromSchemaSQL extracts the primary table name referenced by a
// schema-object CREATE statement (the table being created / indexed /
// triggered on).
func mainTableFromSchemaSQL(sqlStr string) string {
	upper := strings.ToUpper(strings.TrimSpace(sqlStr))
	switch {
	case strings.HasPrefix(upper, "CREATE TABLE") || strings.HasPrefix(upper, "CREATE TEMP TABLE"):
		// Table name after CREATE TABLE (skip TEMP/TEMPORARY).
		re := regexp.MustCompile(`(?i)^CREATE\s+(?:TEMP|TEMPORARY\s+)?TABLE\s+([A-Za-z_][A-Za-z0-9_$]*)`)
		if m := re.FindStringSubmatch(upper); len(m) > 1 {
			return m[1]
		}
	case strings.HasPrefix(upper, "CREATE INDEX") || strings.HasPrefix(upper, "CREATE UNIQUE INDEX"):
		re := regexp.MustCompile(`(?i)ON\s+([A-Za-z_][A-Za-z0-9_$]*)`)
		if m := re.FindStringSubmatch(upper); len(m) > 1 {
			return m[1]
		}
	case strings.HasPrefix(upper, "CREATE TRIGGER") || strings.HasPrefix(upper, "CREATE TEMP TRIGGER"):
		re := regexp.MustCompile(`(?i)ON\s+([A-Za-z_][A-Za-z0-9_$]*)`)
		if m := re.FindStringSubmatch(upper); len(m) > 1 {
			return m[1]
		}
	case strings.HasPrefix(upper, "CREATE VIEW"):
		// View body references the table in its FROM clause.
		re := regexp.MustCompile(`(?i)\bFROM\s+([A-Za-z_][A-Za-z0-9_$]*)`)
		if m := re.FindStringSubmatch(upper); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// fnSQLiteRenameQuoteFix implements SQLite's internal sqlite_rename_quotefix
// function used by ALTER TABLE RENAME machinery: it rewrites double-quoted
// tokens that are NOT valid identifiers (e.g. "notacolumn!", "a;b") into
// single-quoted string literals, leaving genuine double-quoted identifiers
// ("a", "b") untouched. Single quotes inside converted strings are doubled.
// The first argument (schema name) is ignored — the function only rewrites
// the SQL text.
func FnSQLiteRenameQuoteFix(args []interface{}) (interface{}, error) {
	if len(args) < 2 || args[1] == nil {
		return "", nil
	}
	sqlStr, ok := args[1].(string)
	if !ok {
		return "", nil
	}
	return quoteFixSQL(sqlStr), nil
}

// quoteFixSQL rewrites non-identifier double-quoted tokens to single-quoted
// string literals, preserving everything else verbatim.
func quoteFixSQL(sqlStr string) string {
	return quoteFixSQLWithColumns(sqlStr, nil)
}

// isSQLIdentifier reports whether s is a valid unquoted SQL identifier
// (letter or underscore first, then letters/digits/underscores).
func isSQLIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' ||
			(i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// vtabModuleName extracts the module name from a virtual table CREATE
// statement ("CREATE VIRTUAL TABLE e1 USING echo(x1)" → "echo").
func vtabModuleName(sqlStr string) string {
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(sqlStr[idx+len(" USING "):])
	end := strings.IndexAny(rest, " (")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// checkViewRenameDependencies checks whether any view that references the
// given table has broken column references or would become ambiguous after a
// column rename. ALTER TABLE RENAME COLUMN fails if a view selects a column
// that does not exist on the referenced tables (SQLite: "error in view %s: no
// such column: %s") or if the rename makes a column reference ambiguous
// ("error in view %s after rename: ambiguous column name: %s").
func (e *DDLExecutor) checkViewRenameDependencies(tableName string, oldColName, newColName string) *Result {
	views, err := e.ctx.Schema().GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		if res := e.processViewRenameDependency(view, tableName, oldColName, newColName); res != nil {
			return res
		}
	}
	return nil
}

// processViewRenameDependency checks a single view for references to the
// renamed table, reporting column-resolution or post-rename-ambiguity errors.
func (e *DDLExecutor) processViewRenameDependency(view *schema.Entry, tableName, oldColName, newColName string) *Result {
	upperSQL := strings.ToUpper(view.SQL)
	fromIdx := strings.Index(upperSQL, " FROM ")
	if fromIdx < 0 {
		return nil
	}
	fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
	fromTables := splitFromTables(fromRest)
	if len(fromTables) == 0 {
		return nil
	}
	// The view only matters if the renamed table is one of its FROM tables.
	for _, ft := range fromTables {
		if strings.EqualFold(ft, tableName) {
			validCols, tableCols, res := e.buildViewFromTableCols(fromTables, view.Name)
			if res != nil {
				return res
			}
			selIdx := strings.Index(strings.ToUpper(view.SQL), "SELECT ")
			if selIdx < 0 {
				return nil
			}
			return e.validateViewRenameSelect(view, view.SQL[selIdx+7:fromIdx], fromTables, validCols, tableCols, tableName, oldColName, newColName)
		}
	}
	return nil
}

// viewUnavailableModule reports a result when a view's FROM table is broken
// (findErr != nil and not a view) or references a virtual-table module that is
// unavailable (unregistered or a NoopModule stub). It returns nil when the
// FROM table is valid/recoverable.
func (e *DDLExecutor) viewUnavailableModule(ft string, entry *schema.Entry, viewName string, findErr error) *Result {
	if findErr != nil {
		// If the FROM table itself doesn't exist, the view is broken; SQLite
		// reports this on rename.
		if v, vErr := e.ctx.Schema().FindView(ft); vErr != nil {
			return &Result{Error: fmt.Errorf("error in view %s: %s", viewName, findErr.Error())}
		} else if v != nil {
			return nil
		}
		return nil
	}
	// A view that references a virtual table whose module is not registered
	// cannot be re-validated; SQLite reports "no such module: %s" on rename.
	// NoopModule stubs (rtree, etc.) count as unavailable, and so does the
	// echo module: it is a test-only SQLite module that is only registered on
	// the connection that ran register_echo_module. Frigolite registers echo
	// globally, so for rename validation it must be treated as unavailable to
	// match a fresh connection (altercol-11.3).
	if e.isVirtualTable(entry) {
		if mod := vtabModuleName(entry.SQL); mod != "" {
			m, ok := e.ctx.VTables().Find(mod)
			if !ok {
				return &Result{Error: fmt.Errorf("error in view %s: no such module: %s", viewName, mod)}
			}
			if _, isNoop := m.(*vtab.NoopModule); isNoop {
				return &Result{Error: fmt.Errorf("error in view %s: no such module: %s", viewName, mod)}
			}
			if _, isEcho := m.(*vtab.EchoModule); isEcho {
				return &Result{Error: fmt.Errorf("error in view %s: no such module: %s", viewName, mod)}
			}
		}
	}
	return nil
}

// buildViewFromTableCols builds the union of columns across a view's FROM
// tables (validCols) and a per-table column map (tableCols) for validating
// references and detecting post-rename ambiguity. It returns an error result
// when a broken FROM table or unavailable module blocks re-validation.
func (e *DDLExecutor) buildViewFromTableCols(fromTables []string, viewName string) (map[string]bool, map[string]map[string]bool, *Result) {
	validCols := make(map[string]bool)
	tableCols := make(map[string]map[string]bool)
	for _, ft := range fromTables {
		entry, findErr := e.ctx.Schema().FindTable(ft)
		if findErr != nil {
			if res := e.viewUnavailableModule(ft, entry, viewName, findErr); res != nil {
				return nil, nil, res
			}
			continue
		}
		if res := e.viewUnavailableModule(ft, entry, viewName, nil); res != nil {
			return nil, nil, res
		}
		colDefs := e.ctx.ColCache()[ft]
		if colDefs == nil {
			colDefs = e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
		}
		cols := make(map[string]bool)
		for _, cd := range colDefs {
			cols[strings.ToUpper(cd.Name)] = true
			validCols[strings.ToUpper(cd.Name)] = true
		}
		tableCols[strings.ToUpper(ft)] = cols
	}
	return validCols, tableCols, nil
}

// validateViewRenameSelect checks the identifiers in a view's SELECT list
// against the current column set, reporting "no such column" for unresolvable
// references and "ambiguous column name" for post-rename collisions.
func (e *DDLExecutor) validateViewRenameSelect(view *schema.Entry, afterSelect string, fromTables []string, validCols map[string]bool, tableCols map[string]map[string]bool, tableName, oldColName, newColName string) *Result {
	// Extract bare identifier tokens (not whole comma/space chunks) so
	// expressions like "a+10" yield the identifier "a" rather than the literal
	// chunk "a+10". SQLite reports the first unresolvable identifier.
	viewCols := extractIdentifierTokens(afterSelect)
	for i, col := range viewCols {
		col = strings.TrimSpace(col)
		if col == "" {
			continue
		}
		if skipViewSelectToken(col, i, viewCols, fromTables) {
			continue
		}
		upperCol := normalizeViewColRef(strings.ToUpper(col))
		if upperCol == "" || upperCol == "*" {
			continue
		}
		if !validCols[strings.ToUpper(upperCol)] {
			return &Result{Error: fmt.Errorf("error in view %s: no such column: %s", view.Name, col)}
		}
		if res := e.checkViewRenameAmbiguity(view, fromTables, tableCols, tableName, oldColName, newColName, upperCol); res != nil {
			return res
		}
	}
	return nil
}

// skipViewSelectToken reports whether a token extracted from a view's SELECT
// list should be skipped (empty, an AS alias, a function call/expression,
// a SQL keyword, a numeric literal, or a FROM table reference).

// QuoteFixWithSchema rewrites a schema-qualified SQL string's identifier
// quoting. Exported for the expression evaluator's rename_quotefix function.
func (e *DDLExecutor) QuoteFixWithSchema(schemaName, sqlStr string) string {
	return e.quoteFixWithSchema(schemaName, sqlStr)
}
