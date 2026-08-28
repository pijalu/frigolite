package execddl

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/vtab"
)

func (e *DDLExecutor) execAlterTable(s *sql.AlterTableStmt) *Result {
	// SQLITE_IGNORE on SQLITE_ALTER_TABLE silently skips the ALTER (auth-1.353
	// returns IGNORE for ALTER TABLE RENAME COLUMN and the column stays);
	// DENY errors.
	if res := e.authorizeActionOrSkip(auth.ActionAlterTable, s.Table, "", "", ""); res != nil {
		return res
	}
	if res := e.eponymousAlterGuard(s); res != nil {
		return res
	}
	switch s.Action {
	case "RENAME":
		if s.Column != "" {
			return e.execAlterTableRenameColumn(s)
		}
		return e.execAlterTableRename(s)
	case "ADD":
		return e.execAlterTableAdd(s)
	case "DROP":
		return e.execAlterTableDrop(s)
	case "ALTER":
		return e.execAlterTableAlter(s)
	default:
		// No-op for unsupported ALTER TABLE operations
		return &Result{}
	}
}

// eponymousAlterGuard rejects ALTER TABLE targeting an implicit virtual table
// that has no schema entry: an eponymous module instance (generate_series,
// tabfunc01-8xx) or a table-valued pragma function (pragma_compile_options,
// tabfunc01-9xx). SQLite resolves such names to TF_Eponymous tables:
// ADD COLUMN reports "virtual tables may not be altered" (alter.c) while the
// other ALTER forms report "table %s may not be altered" (isAlterableTable).
func (e *DDLExecutor) eponymousAlterGuard(s *sql.AlterTableStmt) *Result {
	name := s.Table
	lower := strings.ToLower(name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	module, modOk := e.ctx.VTables().Find(lower)
	isEpo := modOk && vtab.ModuleIsEponymous(module)
	if !isEpo && !execquery.IsPragmaTableFuncName(lower) {
		return nil
	}
	// A genuine user-created table or view of this name shadows the implicit
	// relation (schema.FindTable synthesizes entries for PRAGMA_* names).
	for _, typ := range []schema.SchemaType{schema.TypeTable, schema.TypeView} {
		if entries, gerr := e.ctx.Schema().GetEntries(typ); gerr == nil {
			for _, en := range entries {
				if strings.EqualFold(en.Name, name) {
					return nil
				}
			}
		}
	}
	switch s.Action {
	case "ADD":
		return &Result{Error: fmt.Errorf("virtual tables may not be altered")}
	default:
		return &Result{Error: fmt.Errorf("table %s may not be altered", name)}
	}
}

// readCellByRowID scans the tree for the cell with the given rowID.
func (e *DDLExecutor) readCellByRowID(tree *btree.BTree, rowID int64) (*storage.Cell, error) {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, err
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return nil, nil
		}
		if cell.RowID == rowID {
			return cell, nil
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return nil, nil
		}
	}
}

// isProtectedSystemTable reports whether a table name is an internal SQLite
// table that ALTER TABLE may not modify (SQLite: "table %s may not be altered").
func isProtectedSystemTable(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	switch upper {
	case "SQLITE_MASTER", "SQLITE_SCHEMA", "SQLITE_TEMP_MASTER", "SQLITE_TEMP_SCHEMA",
		"SQLITE_STAT1", "SQLITE_STAT4", "SQLITE_SEQUENCE":
		return true
	}
	return false
}

// sqlNameNeedsQuoting reports whether an identifier must be quoted in SQL.
// A bare identifier is letters/digits/underscore starting with a non-digit,
// and not a reserved keyword. Keywords like WHERE/SELECT must be quoted
// (e.g. RENAME COLUMN a TO where -> "where"), otherwise the stored DDL is
// unparsable and the DB becomes corrupt ("database disk image is malformed").
func sqlNameNeedsQuoting(name string) bool {
	if name == "" {
		return true
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch >= 0x80 {
			continue
		}
		if i > 0 && ch >= '0' && ch <= '9' {
			continue
		}
		return true
	}
	return isSQLKeyword(name)
}

// isSQLKeyword reports whether name is a SQLite reserved keyword that must be
// quoted when used as an identifier. The list covers all tokens with type
// TokenKeyword in internal/sql/lexer.go.
func isSQLKeyword(name string) bool {
	switch strings.ToUpper(name) {
	case "ABORT", "ACTION", "ADD", "AFTER", "ALL", "ALTER", "ANALYZE", "AND", "AS", "ASC",
		"ATTACH", "AUTOINCREMENT", "BEFORE", "BEGIN", "BETWEEN", "BY", "CASCADE", "CASE",
		"CAST", "CHECK", "COLLATE", "COLUMN", "COMMIT", "CONFLICT", "CONSTRAINT", "CREATE",
		"CROSS", "CURRENT", "DATABASE", "DEFAULT", "DEFERRABLE", "DEFERRED", "DELETE", "DESC",
		"DETACH", "DISTINCT", "DO", "DROP", "EACH", "ELSE", "END", "ESCAPE", "EXCEPT",
		"EXCLUSIVE", "EXISTS", "EXPLAIN", "FAIL", "FILTER", "FIRST", "FOLLOWING", "FOR",
		"FOREIGN", "FROM", "FULL", "GLOB", "GROUP", "GROUPS", "EXCLUDE", "HAVING", "IF",
		"IGNORE", "IMMEDIATE", "IN", "INDEX", "INDEXED", "INITIALLY", "INNER", "INSERT",
		"INSTEAD", "INTERSECT", "INTO", "IS", "ISNULL", "JOIN", "KEY", "LAST", "LEFT",
		"LIKE", "LIMIT", "MATCH", "MATERIALIZED", "NATURAL", "NO", "NOT", "NOTHING",
		"NOTNULL", "NULL", "NULLS", "OF", "OFFSET", "ON", "OTHERS", "OR", "ORDER", "OUTER",
		"OVER", "PARTITION", "PLAN", "PRAGMA", "PRECEDING", "PRIMARY", "QUERY", "RAISE",
		"RANGE", "RECURSIVE", "REFERENCES", "REGEXP", "REINDEX", "RELEASE", "RENAME",
		"REPLACE", "RESTRICT", "RETURNING", "RIGHT", "ROLLBACK", "ROW", "ROWS", "SAVEPOINT",
		"SELECT", "SET", "STORE", "STORED", "STRICT", "TABLE", "TEMP", "TEMPORARY", "THEN",
		"TIES", "TO", "TRANSACTION", "TRIGGER", "UNBOUNDED", "UNION", "UNIQUE", "UPDATE",
		"USING", "VACUUM", "VALUES", "VIEW", "VIRTUAL", "WHEN", "WHERE", "WINDOW", "WITH", "WITHOUT":
		return true
	}
	return false
}

// extractColumnName extracts the column name from the start of a column definition.
func extractColumnName(def string) string {
	def = strings.TrimSpace(def)
	if def == "" {
		return ""
	}
	// Handle quoted identifiers "name"
	if def[0] == '"' {
		end := strings.Index(def[1:], "\"")
		if end >= 0 {
			return def[1 : 1+end]
		}
	}
	// Handle backtick-quoted identifiers `name`
	if def[0] == '`' {
		end := strings.Index(def[1:], "`")
		if end >= 0 {
			return def[1 : 1+end]
		}
	}
	// Handle single-quoted identifiers 'name' (DQS: SQLite accepts a string
	// literal as an identifier in column definitions, e.g. 'a'"b").
	if def[0] == '\'' {
		end := strings.Index(def[1:], "'")
		if end >= 0 {
			return def[1 : 1+end]
		}
	}
	// Regular unquoted name: take first word
	spaceIdx := strings.IndexAny(def, " (\"")
	if spaceIdx > 0 {
		return def[:spaceIdx]
	}
	return def
}

// renameColumnInTriggers updates trigger SQL for triggers that reference the
// given table — either triggers ON the table (TblName matches) or triggers on
// other tables whose bodies directly reference the renamed table's column.
// Uses token-level rename with string-regex complement.
func (e *DDLExecutor) renameColumnInTriggers(tableName, oldColName, newColName string) {
	ctx := &RenameContext{
		OldName:   oldColName,
		NewName:   newColName,
		QuotedNew: newColName,
		IsTable:   false,
		TableName: tableName,
	}
	e.renameColumnInEntries(schema.TypeTrigger, tableName, oldColName, newColName, ctx)
}

// renameColumnInIndexes updates index SQL for indexes on the given table,
// replacing old column name references with the new column name.
func (e *DDLExecutor) renameColumnInIndexes(tableName, oldColName, newColName string) {
	ctx := &RenameContext{
		OldName:   oldColName,
		NewName:   newColName,
		QuotedNew: newColName,
		IsTable:   false,
		TableName: tableName,
	}
	e.renameColumnInEntries(schema.TypeIndex, tableName, oldColName, newColName, ctx)
}

// insideDoubleQuoted reports whether byte position pos in s is inside a
// double-quoted span (counting unescaped " quotes before pos).
func insideDoubleQuoted(s string, pos int) bool {
	quotes := 0
	for i := 0; i < pos; i++ {
		if s[i] == '"' && (i == 0 || s[i-1] != '\\') {
			quotes++
		}
	}
	return quotes%2 == 1
}

// refTableInTrigger checks if a trigger's SQL references the given table name.
// Uses word-boundary matching to avoid partial matches.
func refTableInTrigger(sqlStr, tableName string) bool {
	if sqlStr == "" || tableName == "" {
		return false
	}
	// Check for quoted table name "tablename"
	quoted := regexp.QuoteMeta(tableName)
	re := regexp.MustCompile(`(?i)"` + quoted + `"`)
	if re.MatchString(sqlStr) {
		return true
	}
	// Check for unquoted table name with word boundaries
	re = regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9_])` + quoted + `([^a-zA-Z0-9_]|$)`)
	return re.MatchString(sqlStr)
}

// validateRename checks if the table can be renamed by verifying that
// no CHECK constraints or index WHERE clauses reference the old table name,
// and that no views have circular references.
// gatherRenameEntries returns the schema entries to re-validate during ALTER
// TABLE RENAME. A TEMP table rename validates only TEMP objects; a main or
// attached rename validates main (and other schemas) plus TEMP entries appended
// last so main-schema errors surface first (SQLite rowid order).
func (e *DDLExecutor) gatherRenameEntries(isTempTable bool) []*schema.Entry {
	if isTempTable {
		// A TEMP table rename validates only TEMP schema objects.
		if tc := e.ctx.GetDB("temp"); tc != nil {
			if entries, err := tc.Schema.GetEntries(""); err == nil {
				return entries
			}
		}
		return nil
	}
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	// A non-temp rename validates main + temp (temp entries appended last). If
	// the temp schema has no temp tables (is not open), there is nothing to add.
	if tc := e.ctx.GetDB("temp"); tc != nil {
		if tempEntries, tErr := tc.Schema.GetEntries(""); tErr == nil {
			entries = append(entries, tempEntries...)
		}
	}
	return entries
}
func (e *DDLExecutor) validateRename(oldName, newName string) error {
	tableEntry, tableCtx, err := e.ctx.FindTable(oldName)
	if err != nil {
		return err
	}
	// A table entry belongs to the TEMP schema when its owning database
	// context is the temp database (its stored SQL is "CREATE TABLE ..."
	// without the TEMP keyword, matching SQLite's sqlite_temp_schema).
	isTempTable := tableCtx != nil && strings.EqualFold(tableCtx.Name, "temp")

	if e.ctx.LegacyAlterTable() {
		return e.validateRenameLegacy(tableEntry, oldName, newName)
	}

	// Non-legacy mode: the token-level rename will update qualified references in
	// the table's own SQL and indexes. Only check triggers and views for
	// references to tables OTHER than the one being renamed.
	entries := e.gatherRenameEntries(isTempTable)
	// Check all triggers for references to non-existent tables. SQLite
	// re-parses every schema object during ALTER TABLE RENAME (table or
	// column), so ALL triggers are validated — a broken trigger on an
	// unrelated table blocks the rename (altertab3-4.1.2, alter-18.1). The
	// schema filtering below (temp vs main) already restricts which schema's
	// objects are examined; within that set every trigger is checked.
	if err := e.validateRenameTriggerTables(entries, oldName); err != nil {
		return err
	}
	if err := e.validateRenameViews(entries); err != nil {
		return err
	}
	return e.validateRenameTriggerColRefs(entries, oldName)
}

// validateRenameLegacy runs the ALTER TABLE RENAME check for legacy_alter_table
// mode, where the CREATE SQL is not updated with token-level rename. It looks
// for qualified references to the old table name in the table's own SQL and its
// indexes.
func (e *DDLExecutor) validateRenameLegacy(tableEntry *schema.Entry, oldName, newName string) error {
	refs := findQualifiedTableRefs(tableEntry.SQL, oldName)
	if len(refs) > 0 {
		return fmt.Errorf("error in table %s after rename: no such column: %s", newName, refs[0])
	}
	entries, gErr := e.ctx.Schema().GetEntries("")
	if gErr == nil {
		for _, entry := range entries {
			if entry.Type == schema.TypeIndex && strings.EqualFold(entry.TblName, oldName) {
				refs := findQualifiedTableRefs(entry.SQL, oldName)
				if len(refs) > 0 {
					return fmt.Errorf("error in index %s after rename: no such column: %s", entry.Name, refs[0])
				}
			}
		}
	}
	return nil
}

// validateRenameTriggerTables checks every trigger body for references to
// non-existent tables (a broken trigger blocks the rename, matching SQLite).
func (e *DDLExecutor) validateRenameTriggerTables(entries []*schema.Entry, oldName string) error {
	for _, entry := range entries {
		if entry.Type == schema.TypeTrigger {
			if err := e.checkTriggerTableRefs(entry, oldName); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateRenameViews rejects circularly-defined views and views whose FROM
// clause names a non-existent table (both block the rename).
func (e *DDLExecutor) validateRenameViews(entries []*schema.Entry) error {
	for _, entry := range entries {
		if entry.Type == schema.TypeView {
			if hasViewCircularRef(entry.SQL, entry.Name) {
				return fmt.Errorf("error in view %s: view %s is circularly defined", entry.Name, entry.Name)
			}
			if missing := e.viewMissingTable(entry); missing != "" {
				refName := missing
				if !strings.Contains(missing, ".") {
					refName = "main." + missing
				}
				return fmt.Errorf("error in view %s: no such table: %s", entry.Name, refName)
			}
		}
	}
	return nil
}

// validateRenameTriggerColRefs checks column references of triggers related to
// the renamed table (directly, via its body, or through a dependent view).
func (e *DDLExecutor) validateRenameTriggerColRefs(entries []*schema.Entry, oldName string) error {
	viewNames := make(map[string]bool)
	for _, entry := range entries {
		if entry.Type == schema.TypeView && refTableInTrigger(entry.SQL, oldName) {
			viewNames[strings.ToUpper(entry.Name)] = true
		}
	}
	for _, entry := range entries {
		if entry.Type != schema.TypeTrigger {
			continue
		}
		related := strings.EqualFold(entry.TblName, oldName) || refTableInTrigger(entry.SQL, oldName) ||
			e.triggerReferencesNamedViews(entry, viewNames)
		if related {
			if err := e.checkTriggerColRefs(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// triggerReferencesNamedViews reports whether a trigger body references any of
// the given view names (views that depend on the renamed table).
func (e *DDLExecutor) triggerReferencesNamedViews(entry *schema.Entry, viewNames map[string]bool) bool {
	for vn := range viewNames {
		if refTableInTrigger(entry.SQL, vn) {
			return true
		}
	}
	return false
}

// viewMissingTable returns the name of the first table referenced by a view's
// FROM clause that does not exist (in any schema), or "" if all resolve.
func (e *DDLExecutor) viewMissingTable(view *schema.Entry) string {
	if view == nil {
		return ""
	}
	upperSQL := strings.ToUpper(view.SQL)
	fromIdx := strings.Index(upperSQL, " FROM ")
	if fromIdx < 0 {
		return ""
	}
	// Split the ORIGINAL-case FROM text so the reported name matches SQLite's
	// casing (splitFromTables needs the original text; the keyword boundary
	// search uses the uppercase copy).
	// A view whose body declares CTEs has its own table scope; the FROM
	// parser cannot reliably distinguish CTE names from tables, so skip the
	// missing-table validation for CTE views (they are validated at USE time,
	// and rename tests with CTEs expect the rename to proceed).
	if strings.Contains(upperSQL, " WITH ") {
		return ""
	}
	origFrom := strings.TrimSpace(view.SQL[fromIdx+6:])
	// Only validate views whose body is a simple single-table SELECT (no
	// subqueries, JOINs, VALUES, or CTEs). Complex FROM clauses (parenthesized
	// joins, subquery operands) cannot be reliably parsed by splitFromTables
	// and would produce false positives (altertab3-30.1: a view with a
	// VALUES-in-subquery body must not block the rename).
	if strings.ContainsAny(origFrom, "()") ||
		strings.Contains(strings.ToUpper(origFrom), " JOIN ") ||
		strings.Contains(strings.ToUpper(origFrom), " VALUES") ||
		strings.Contains(strings.ToUpper(origFrom), ",") {
		return ""
	}
	fromNames := splitFromTables(origFrom)
	for _, ft := range fromNames {
		if ft == "" {
			continue
		}
		// Strip double-quote characters from quoted identifiers ("idx2"
		// resolves as idx2).
		ft = strings.Trim(ft, `"`)
		if _, _, err := e.ctx.FindTable(ft); err == nil {
			continue
		}
		if _, _, err := e.ctx.FindView(ft); err == nil {
			continue
		}
		return ft
	}
	return ""
}

// triggerReferencesView reports whether a trigger body references any view
// (a FROM/INSERT/UPDATE target that resolves to a view rather than a table).
// Such references are rewritten by RENAME COLUMN, so a column resolved through
// them is broken by the rename ("after rename" wording).
func (e *DDLExecutor) triggerReferencesView(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	for _, ref := range findTableRefsInTrigger(entry.SQL) {
		if _, _, err := e.ctx.FindView(ref); err == nil {
			return true
		}
	}
	return false
}

// checkTriggerColRefs checks that all column references in a trigger's SQL
// exist in the trigger's ON table or in tables referenced by its body.
// Returns an error formatted to match SQLite's "error in trigger %s: ..." pattern.
func (e *DDLExecutor) checkTriggerColRefs(entry *schema.Entry) error {
	// Parse the trigger SQL to get its AST
	stmts, perr := parse.ParseSQL(entry.SQL)
	if perr != nil || len(stmts) == 0 {
		return nil // Can't validate if we can't parse
	}

	// Walk all statements looking for unqualified ColumnRefs at the top level
	// (not inside subqueries, which have their own table scope)
	colRefs := findTriggerColRefs(stmts)

	// Get column names for the ON table
	onTableEntry, err := e.ctx.Schema().FindTable(entry.TblName)
	if err != nil {
		return nil // If the table doesn't exist, can't validate columns
	}
	onTableCols := e.ctx.ParseColumnDefs(onTableEntry.Name, onTableEntry.SQL)
	onColMap := make(map[string]bool)
	for _, c := range onTableCols {
		onColMap[strings.ToUpper(c.Name)] = true
	}
	// Check each column reference
	for _, ref := range colRefs {
		if err := e.validateTriggerColRef(entry, ref, onColMap); err != nil {
			return err
		}
	}
	// Validate ON CONFLICT conflict-target columns against the INSERT target
	// table (not the ON table — the conflict target belongs to the table the
	// INSERT writes into).
	if err := e.checkTriggerConflictCols(entry); err != nil {
		return err
	}
	return nil
}

// validateTriggerColRef validates one column reference in a trigger body
// against the ON table's columns, skipping pseudo-columns (NEW/OLD),
// qualified references, SQL keywords, and empty names.
func (e *DDLExecutor) validateTriggerColRef(entry *schema.Entry, ref *sql.ColumnRef, onColMap map[string]bool) error {
	upperName := strings.ToUpper(ref.Name)
	// Skip special pseudo-columns and keywords
	if ref.Table != "" {
		upperTable := strings.ToUpper(ref.Table)
		if upperTable == "NEW" || upperTable == "OLD" {
			return nil
		}
		// Qualified reference to a non-pseudo table - just skip for now
		// as proper validation would require resolving the table
		return nil
	}
	// Skip SQL keywords and special names that might be parsed as column refs
	if isSQLKeywordOrPseudo(upperName) {
		return nil
	}
	// Skip empty-name references (e.g. a double-quoted empty string ""
	// parsed as a DQS identifier with no name).
	if ref.Name == "" {
		return nil
	}
	// Unqualified column reference - check against ON table's columns
	if !onColMap[upperName] {
		// A reference that resolves through a VIEW over the renamed table is
		// broken BY the rename (SQLite: "after rename"); a direct missing
		// column is a pre-existing break (plain wording).
		if e.triggerReferencesView(entry) {
			return fmt.Errorf("error in trigger %s after rename: no such column: %s", entry.Name, ref.Name)
		}
		return fmt.Errorf("error in trigger %s: no such column: %s", entry.Name, ref.Name)
	}
	return nil
}

// isSQLKeywordOrPseudo checks if a name is a SQL keyword or pseudo-column
// that should be skipped during column reference validation.
func isSQLKeywordOrPseudo(name string) bool {
	switch name {
	case "*", "TRUE", "FALSE", "NULL", "ROWID", "_ROWID_", "OID",
		"ROW", "ROWS", "RANGE", "GROUPS", "UNBOUNDED", "PRECEDING", "FOLLOWING",
		"CURRENT", "RECURSIVE", "EXCLUDE", "TIES", "OTHERS",
		// Common SQL keywords that can appear in expression tokens
		"IN", "NOT", "AND", "OR", "IS", "LIKE", "GLOB", "BETWEEN", "COLLATE",
		"ASC", "DESC", "CAST", "CASE", "WHEN", "THEN", "ELSE", "END",
		"EXISTS", "SELECT", "FROM", "WHERE", "GROUP", "HAVING", "ORDER", "BY",
		"LIMIT", "OFFSET", "UNION", "INTERSECT", "EXCEPT", "JOIN", "ON",
		"DISTINCT", "ALL", "AS", "OVER", "FILTER", "PARTITION", "NULLS",
		// Window function names (common ones that might appear as identifiers)
		"RANK", "DENSE_RANK", "PERCENT_RANK", "ROW_NUMBER", "NTILE",
		"LEAD", "LAG", "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE",
		"CUME_DIST":
		return true
	}
	return false
}

// findTriggerColRefs extracts unqualified ColumnRef nodes from the top-level
// statements in a trigger body (not inside subqueries).
func findTriggerColRefs(stmts []sql.Stmt) []*sql.ColumnRef {
	var refs []*sql.ColumnRef
	for _, stmt := range stmts {
		collectTriggerColRefs(stmt, &refs, false) // false = not in subquery initially
	}
	return refs
}

// collectWindowDefTriggerColRefs walks a WindowDef and collects ColumnRefs
// from PARTITION BY and ORDER BY clauses.
func collectWindowDefTriggerColRefs(w *sql.WindowDef, refs *[]*sql.ColumnRef, inSubquery bool) {
	if w == nil {
		return
	}
	for _, p := range w.Partitions {
		collectExprTriggerColRefs(p, refs, inSubquery)
	}
	for _, ob := range w.OrderBy {
		collectExprTriggerColRefs(ob.Expr, refs, inSubquery)
	}
}

// extractCTENames collects all CTE names defined in WITH clauses within a SQL string.
// Returns the CTE names in uppercase for easy comparison.
func extractCTENames(sql string) []string {
	var names []string
	// Match WITH name AS (...) or WITH name(col1,col2) AS (...), including
	// nested WITH clauses.
	re := regexp.MustCompile(`(?i)\bWITH\s+(\w+)\s*(?:\([^)]*\))?\s+AS\s*\(`)
	matches := re.FindAllStringSubmatch(sql, -1)
	for _, m := range matches {
		names = append(names, strings.ToUpper(m[1]))
	}
	return names
}

// isCTEReferencedInMain checks if a CTE with the given name is actually referenced
// in the main body of a SELECT statement (FROM clause, JOINs, WHERE, etc.).
// CTEs that are defined but never referenced don't create circular view references.
func isCTEReferencedInMain(sel *sql.SelectStmt, cteName string) bool {
	if sel == nil || cteName == "" {
		return false
	}
	// Check FROM clause
	if strings.EqualFold(sel.From.Name, cteName) {
		return true
	}
	// Check JOINs
	for _, j := range sel.Joins {
		if strings.EqualFold(j.Table.Name, cteName) {
			return true
		}
	}
	return false
}

// renameUpdateRelatedEntries updates views, triggers, and indexes that
// reference the old table name to use the new table name.
// Uses token-level rename (FindRenameTokens + ApplyRenames) to avoid
// string-regex issues with aliases, string literals, and partial matches.
// Falls back to string-regex when the SQL cannot be parsed.
