// Package exec implements query execution.
package exec

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// --- ALTER TABLE ---

func (e *Engine) execAlterTable(s *sql.AlterTableStmt) *Result {
	if err := e.authorize(auth.ActionAlterTable, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
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

func (e *Engine) execAlterTableRename(s *sql.AlterTableStmt) *Result {
	if s.NewName == "" {
		return &Result{Error: fmt.Errorf("ALTER TABLE RENAME TO requires a new name")}
	}
	oldName := s.Table
	newName := s.NewName

	// SQLite protects its internal tables from ALTER TABLE.
	if isProtectedSystemTable(oldName) {
		return &Result{Error: fmt.Errorf("table %s may not be altered", oldName)}
	}

	// Reject renaming to a name that already exists as a table or index.
	// SQLite: "there is already another table or index with this name: %s"
	if _, err := e.schema.FindTable(newName); err == nil {
		return &Result{Error: fmt.Errorf("there is already another table or index with this name: %s", newName)}
	}
	if newName != oldName {
		if _, err := e.schema.FindIndex(newName); err == nil {
			return &Result{Error: fmt.Errorf("there is already another table or index with this name: %s", newName)}
		}
	}

	// Find the table entry and validate it for broken references
	// (writable_schema bypasses this validation).
	if !e.writableSchema {
		if err := e.validateRename(oldName, newName); err != nil {
			return &Result{Error: err}
		}
	}

	// Pre-process: apply token-level rename to the table's own CREATE SQL
	entry, entryCtx, err := e.findTable(oldName)
	if err != nil {
		return &Result{Error: err}
	}

	if e.legacyAlterTable {
		// In legacy mode, the CREATE SQL is used as-is (only the schema entry name
		// changes; internal references like CHECK constraints are NOT updated).
		// The validateRename function above catches cases where this would cause
		// inconsistencies and rejects the rename.
		if err := entryCtx.Schema.RenameEntryWithSQL(oldName, newName, entry.SQL); err != nil {
			return &Result{Error: err}
		}
	} else {
		// Non-legacy mode: use token-level rename + string replacement fallback
		// to update the table's own CREATE SQL with the new table name, including
		// qualified references in CHECK constraints, column defaults, etc.
		ctx := &RenameContext{
			OldName:   oldName,
			NewName:   newName,
			QuotedNew: `"` + newName + `"`,
			IsTable:   true,
		}
		ranges, rErr := FindRenameTokens(entry.SQL, ctx)
		newSQL := ""
		if rErr == nil && len(ranges) > 0 {
			newSQL = ApplyRenames(entry.SQL, ranges, `"`+newName+`"`)
			// Apply string-regex as a complementary pass to catch occurrences the
			// token walker missed (e.g., table names in CHECK constraints that use
			// schema-qualified column references, or DEFAULT expressions).
			if newSQL != entry.SQL {
				newSQL = replaceTableNameInSQL(newSQL, oldName, newName)
			}
		}
		// If token rename produced nothing, fall back to string replacement
		if newSQL == "" || newSQL == entry.SQL {
			newSQL = replaceTableNameInSQL(entry.SQL, oldName, newName)
		}

		if err := entryCtx.Schema.RenameEntryWithSQL(oldName, newName, newSQL); err != nil {
			return &Result{Error: err}
		}
	}

	// Update column cache
	if cached, ok := e.colCache[oldName]; ok {
		e.colCache[newName] = cached
		delete(e.colCache, oldName)
	}

	// Invalidate table/column/constraint caches for the old and new names so
	// stale entries do not outlive the rename.
	delete(e.tableCache, oldName)
	delete(e.tableCache, newName)
	delete(e.tcCache, oldName)
	delete(e.tcCache, newName)
	delete(e.tableRootPages, oldName)
	delete(e.tableRootPages, newName)

	// SQLite's ALTER TABLE RENAME updates sqlite_sequence: any row whose
	// name matches the old table name is rewritten to the new name.
	e.renameSQLiteSequence(oldName, newName)

	// Update views, triggers, and indexes that reference the renamed table.
	// writable_schema performs a raw rename that does NOT rewrite dependent
	// objects (SQLite leaves view/trigger SQL untouched when sqlite_schema
	// is directly editable).
	if !e.writableSchema {
		e.renameUpdateRelatedEntries(oldName, newName)
	}

	return &Result{}
}

// renameSQLiteSequence updates the sqlite_sequence table after ALTER TABLE
// RENAME, matching SQLite: rows with name == oldName become newName. Missing
// or synthetic sqlite_sequence tables are ignored.
func (e *Engine) renameSQLiteSequence(oldName, newName string) {
	entry, err := e.schema.FindTable("sqlite_sequence")
	if err != nil {
		return
	}
	if entry.RootPage == 1 && strings.Contains(entry.SQL, "seq INTEGER") {
		return // synthetic fallback, not a real table
	}
	tree := e.tableBTreeForName(entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return
	}
	var toRename []int64
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		if len(rec.Values) > 0 {
			if name, ok := rec.Values[0].(string); ok && name == oldName {
				toRename = append(toRename, cell.RowID)
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	for _, rowID := range toRename {
		cell, err := e.readCellByRowID(tree, rowID)
		if err != nil || cell == nil {
			continue
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			continue
		}
		values := make([]interface{}, len(rec.Values))
		copy(values, rec.Values)
		if len(values) > 0 {
			values[0] = newName
		}
		newRecord, err := storage.EncodeRecord(values)
		if err != nil {
			continue
		}
		if _, err := tree.DeleteCellsWhere(func(c *storage.Cell) bool {
			return c.RowID == rowID
		}); err != nil {
			continue
		}
		e.invalidateRowIDCache(entry.RootPage)
		newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: rowID, Payload: newRecord}
		_ = tree.InsertCell(newCell)
		e.bumpRowIDCache(entry.RootPage, rowID)
	}
}

// readCellByRowID scans a tree for the cell with the given rowID.
func (e *Engine) readCellByRowID(tree *btree.BTree, rowID int64) (*storage.Cell, error) {
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

// execAlterTableRenameColumn handles ALTER TABLE ... RENAME [COLUMN] old_name TO new_name.
func (e *Engine) execAlterTableRenameColumn(s *sql.AlterTableStmt) *Result {
	tableName := s.Table
	oldColName := s.Column
	newColName := s.NewName

	if oldColName == "" || newColName == "" {
		return &Result{Error: fmt.Errorf("ALTER TABLE RENAME COLUMN requires old and new column names")}
	}

	// SQLite protects its internal tables from ALTER TABLE.
	if isProtectedSystemTable(tableName) {
		return &Result{Error: fmt.Errorf("table %s may not be altered", tableName)}
	}

	// If the name names a VIEW instead of a table, SQLite rejects the
	// rename with a dedicated message (check before validateRename, which
	// would report "no such table").
	if _, _, vErr := e.findView(tableName); vErr == nil {
		return &Result{Error: fmt.Errorf("cannot rename columns of view %q", tableName)}
	}

	// Validate triggers before proceeding - reject rename if any trigger
	// references a non-existent table (matches SQLite behavior). writable_schema
	// bypasses this validation.
	if !e.writableSchema {
		if err := e.validateRename(tableName, tableName); err != nil {
			return &Result{Error: err}
		}
	}

	// Validate views that reference this table for broken column references
	// before mutating anything (SQLite: "error in view %s: no such column: %s").
	// writable_schema bypasses dependency validation (SQLite skips it when
	// sqlite_schema is directly editable).
	if !e.writableSchema {
		if depResult := e.checkViewRenameDependencies(tableName, oldColName, newColName); depResult != nil {
			return depResult
		}

		// Validate indexes on this table for broken column references
		// (SQLite: "error in index %s: no such column: %s").
		if depResult := e.checkIndexRenameDependencies(tableName); depResult != nil {
			return depResult
		}
	}

	// Find the table entry (searching all attached databases, matching
	// SQLite's unqualified name resolution).
	tableEntry, _, err := e.findTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// Check for virtual table
	if e.isVirtualTable(tableEntry) {
		return &Result{Error: fmt.Errorf("cannot rename columns of virtual table %q", tableName)}
	}

	// Get column definitions, parsing them if needed
	colDefs := e.colCache[tableName]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}

	// Reject renaming to an existing column name (matches SQLite's
	// "error in table %s after rename: duplicate column name: %s").
	for _, c := range colDefs {
		if !strings.EqualFold(c.Name, oldColName) && strings.EqualFold(c.Name, newColName) {
			return &Result{Error: fmt.Errorf("error in table %s after rename: duplicate column name: %s", tableName, newColName)}
		}
	}

	// Find and rename the column in colDefs
	found := false
	for i, c := range colDefs {
		if strings.EqualFold(c.Name, oldColName) {
			colDefs[i].Name = newColName
			found = true
			break
		}
	}
	if !found {
		return &Result{Error: fmt.Errorf("no such column: %q", oldColName)}
	}
	e.colCache[tableName] = colDefs

	// Update the CREATE TABLE SQL in the schema entry
	newSQL := renameColumnInCreateTableSQL(tableEntry.SQL, oldColName, newColName)
	if newSQL != "" && newSQL != tableEntry.SQL {
		tableEntry.SQL = newSQL
		delete(e.tableCache, tableName)
		// In-place schema update keeps the row's position in sqlite_schema
		// (matching SQLite, which rewrites the row rather than re-inserting).
		if err := e.schema.UpdateEntry(tableEntry.Name, newSQL); err != nil {
			return &Result{Error: fmt.Errorf("failed to update schema entry: %w", err)}
		}
	}

	// Update triggers that reference the old column name (ON the same table)
	e.renameColumnInTriggers(tableName, oldColName, newColName)

	// Validate: reject if any trigger references the old column name ONLY through
	// a view that depends on the table being renamed. References made directly
	// against the renamed table are updated by renameColumnInTriggers /
	// renameColumnInEntries below (SQLite updates triggers on other tables whose
	// bodies reference the renamed table's column directly).
	entries, gErr := e.schema.GetEntries("")
	if gErr == nil {
		// Collect views that depend on the table being renamed
		viewNames := make(map[string]bool)
		for _, entry := range entries {
			if entry.Type == schema.TypeView && refTableInTrigger(entry.SQL, tableName) {
				viewNames[entry.Name] = true
			}
		}
		if len(viewNames) > 0 {
			for _, entry := range entries {
				if entry.Type != schema.TypeTrigger {
					continue
				}
				if strings.EqualFold(entry.TblName, tableName) {
					continue
				}
				// Only consider triggers that reference the old column name but do
				// NOT reference the renamed table directly. If the trigger touches
				// the table itself, its references were already rewritten above.
				if refTableInTrigger(entry.SQL, tableName) {
					continue
				}
				newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
				if newSQL == entry.SQL {
					continue
				}
				// The trigger references the column name through a view.
				for vn := range viewNames {
					if refTableInTrigger(entry.SQL, vn) {
						return &Result{Error: fmt.Errorf("error in trigger %s after rename: no such column: %s", entry.Name, oldColName)}
					}
				}
			}
		}
	}

	// Update indexes that reference the old column name
	e.renameColumnInIndexes(tableName, oldColName, newColName)

	// Update views that reference the old column name
	e.renameColumnInViews(tableName, oldColName, newColName)

	// Update FOREIGN KEY references in other tables: a child table whose
	// REFERENCES clause names the renamed column must be rewritten (SQLite
	// updates the RefCols list in the child's CREATE TABLE SQL).
	e.renameColumnInForeignKeys(tableName, oldColName, newColName)

	return &Result{}
}

// renameColumnInCreateTableSQL renames a column within CREATE TABLE SQL text.
// It replaces the column name at the beginning of its definition while preserving
// the rest of the column definition text (type, constraints, etc.).
func renameColumnInCreateTableSQL(sqlStr, oldName, newName string) string {
	upperSQL := strings.ToUpper(sqlStr)
	if !strings.Contains(upperSQL, "CREATE TABLE") {
		return ""
	}

	// Find the parenthesized column definitions
	parenStart := strings.Index(sqlStr, "(")
	if parenStart < 0 {
		return ""
	}
	depth := 0
	parenEnd := -1
parenLoop:
	for i := parenStart; i < len(sqlStr); i++ {
		switch sqlStr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break parenLoop
			}
		}
	}
	if parenEnd < 0 {
		return ""
	}

	defText := sqlStr[parenStart+1 : parenEnd]
	// Split by top-level commas
	var parts []string
	depth = 0
	start := 0
	for i := 0; i < len(defText); i++ {
		switch defText[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, defText[start:i])
				start = i + 1
			}
		}
	}
	if start < len(defText) {
		parts = append(parts, defText[start:])
	}

	// Find and rename the column in its definition part
	oldUpper := strings.ToUpper(oldName)
	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Extract the column name (first word, handling quoted names)
		colName := extractColumnName(trimmed)
		if colName != "" && strings.EqualFold(colName, oldName) {
			// Preserve the leading whitespace of the original part so the
			// rebuilt SQL keeps its formatting ("a INTEGER, b TEXT" stays
			// "a INTEGER, d TEXT" rather than "a INTEGER,d TEXT").
			leadWS := part[:len(part)-len(trimmed)]
			// Render the new name quoted if it is not a bare identifier
			// (SQLite quotes column names containing spaces, e.g. "silly name"),
			// or if the original column was quoted (SQLite preserves the
			// original token's quoting style, e.g. "b" → "d").
			quoteNew := sqlNameNeedsQuoting(newName)
			wasQuoted := strings.HasPrefix(trimmed, `"`+colName+`"`)
			wasSingleQuoted := strings.HasPrefix(trimmed, `'`+colName+`'`)
			newToken := newName
			if quoteNew || wasQuoted || wasSingleQuoted {
				newToken = `"` + newName + `"`
			}
			if wasQuoted {
				parts[i] = leadWS + strings.Replace(trimmed, `"`+colName+`"`, newToken, 1)
			} else if wasSingleQuoted {
				// A DQS single-quoted identifier ('a'"b") is rewritten to the
				// double-quoted new name followed by the rest (SQLite emits
				// "x" "b" with a space separating the tokens).
				parts[i] = leadWS + newToken + " " + strings.TrimSpace(trimmed[len(colName)+2:])
			} else {
				// For unquoted names, replace the first word
				spaceIdx := strings.IndexAny(trimmed, " (\"")
				if spaceIdx > 0 {
					parts[i] = leadWS + newToken + trimmed[spaceIdx:]
				} else {
					parts[i] = leadWS + newToken
				}
			}
			break
		}
		_ = oldUpper
	}

	// Rebuild the SQL
	var buf strings.Builder
	buf.WriteString(sqlStr[:parenStart+1])
	for i, part := range parts {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString(part)
	}
	buf.WriteString(sqlStr[parenEnd:])
	result := buf.String()

	// Rewrite references to the renamed column in table-level and
	// column-level constraints (CHECK, PRIMARY KEY, UNIQUE, FOREIGN KEY,
	// defaults, generated expressions). The definition name was rewritten
	// above; this pass updates every other reference within the CREATE SQL.
	result = replaceColumnNameInSQL(result, oldName, newName)
	return result
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
// A bare identifier is letters/digits/underscore starting with a non-digit.
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

// renameColumnInForeignKeys updates the REFERENCES clauses in child tables
// that reference the renamed column of the given parent table. SQLite rewrites
// the parent-column list in every child's FOREIGN KEY declaration (e.g.
// `REFERENCES p1(c, d)` becomes `REFERENCES p1(c, "silly name")`).
func (e *Engine) renameColumnInForeignKeys(parentTable, oldColName, newColName string) {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type != schema.TypeTable {
			continue
		}
		if strings.EqualFold(entry.TblName, parentTable) {
			continue // the parent's own SQL is rewritten by renameColumnInCreateTableSQL
		}
		if entry.SQL == "" {
			continue
		}
		if !strings.Contains(entry.SQL, oldColName) &&
			!strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(oldColName)) {
			continue
		}
		if !strings.Contains(strings.ToUpper(entry.SQL), "REFERENCES") {
			continue
		}
		// Only rewrite REFERENCES clauses that name the parent table.
		if !refTableInTrigger(entry.SQL, parentTable) {
			continue
		}
		newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
		if newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			_ = e.schema.UpdateEntry(entry.Name, newSQL)
		}
	}
}

// renameColumnInTriggers updates trigger SQL for triggers that reference the
// given table — either triggers ON the table (TblName matches) or triggers on
// other tables whose bodies directly reference the renamed table's column.
// Uses token-level rename with string-regex complement.
func (e *Engine) renameColumnInTriggers(tableName, oldColName, newColName string) {
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
func (e *Engine) renameColumnInIndexes(tableName, oldColName, newColName string) {
	ctx := &RenameContext{
		OldName:   oldColName,
		NewName:   newColName,
		QuotedNew: newColName,
		IsTable:   false,
		TableName: tableName,
	}
	e.renameColumnInEntries(schema.TypeIndex, tableName, oldColName, newColName, ctx)
}

// renameColumnInViews updates view SQL for views that reference the given
// table, replacing old column name references with the new column name.
// Views store their own name in TblName, so they are matched by SQL content
// referencing the table rather than by TblName.
func (e *Engine) renameColumnInViews(tableName, oldColName, newColName string) {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type != schema.TypeView {
			continue
		}
		if !refTableInTrigger(entry.SQL, tableName) {
			continue
		}
		if !strings.Contains(entry.SQL, oldColName) &&
			!strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(oldColName)) {
			continue
		}
		newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
		if newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			_ = e.schema.UpdateEntry(entry.Name, newSQL)
		}
	}
}

// renameColumnInEntries applies column rename to schema entries of the given type.
// Uses token-level rename with string-regex as a complementary pass.
func (e *Engine) renameColumnInEntries(entryType schema.SchemaType, tblName string, oldColName, newColName string, ctx *RenameContext) {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type != entryType {
			continue
		}
		if tblName != "" {
			// For triggers, match either triggers ON the renamed table OR triggers
			// whose bodies directly reference the renamed table (their column
			// references to the renamed table must be rewritten too).
			if entryType == schema.TypeTrigger {
				if !strings.EqualFold(entry.TblName, tblName) &&
					!refTableInTrigger(entry.SQL, tblName) {
					continue
				}
			} else if !strings.EqualFold(entry.TblName, tblName) {
				continue
			}
		}
		if entry.SQL == "" {
			continue
		}
		// Skip entries that don't contain the old column name
		if !strings.Contains(entry.SQL, oldColName) &&
			!strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(oldColName)) {
			continue
		}

		// Try token-level rename first
		ranges, rErr := FindRenameTokens(entry.SQL, ctx)
		if rErr == nil && len(ranges) > 0 {
			newSQL := ApplyRenames(entry.SQL, ranges, newColName)
			if newSQL != entry.SQL && newSQL != "" {
				// Apply string-regex as a complementary pass
				newSQL = replaceColumnNameInSQL(newSQL, oldColName, newColName)
				if newSQL != entry.SQL {
					entry.SQL = newSQL
					_ = e.schema.UpdateEntry(entry.Name, newSQL)
					continue
				}
			}
		}

		// Fallback: string-regex alone
		newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
		if newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			_ = e.schema.UpdateEntry(entry.Name, newSQL)
		}
	}
}

// replaceColumnNameInSQL replaces occurrences of oldColName with newColName
// in a SQL string, using word-boundary matching to avoid partial matches.
// Quoting is preserved per occurrence: a match that was double-quoted
// ("b") stays quoted ("d"), and a new name that requires quoting (e.g.
// "silly name") is always emitted quoted.
func replaceColumnNameInSQL(sqlStr, oldColName, newColName string) string {
	if sqlStr == "" || oldColName == "" || newColName == "" {
		return sqlStr
	}
	quotedOld := regexp.QuoteMeta(oldColName)
	// \b word boundaries never consume the neighboring non-word characters,
	// so consecutive matches (e.g. x+x) are all found. \b also matches
	// around double-quoted identifiers ("b") without consuming the quotes.
	re := regexp.MustCompile(`(?i)\b` + quotedOld + `\b`)
	idxs := re.FindAllStringIndex(sqlStr, -1)
	if len(idxs) == 0 {
		return sqlStr
	}
	quoteNew := sqlNameNeedsQuoting(newColName)
	var b strings.Builder
	last := 0
	for _, idx := range idxs {
		start, end := idx[0], idx[1]
		// Skip matches that fall inside a larger double-quoted identifier
		// (e.g. the f in "big f" after the definition was already renamed to
		// the quoted new name) — unless the match IS the quoted identifier
		// content ("b" → start-1 is the opening quote).
		if insideDoubleQuoted(sqlStr, start) && !(start > 0 && sqlStr[start-1] == '"') {
			continue
		}
		// A quoted occurrence is `"b"`; extend the span so the quotes are
		// replaced together with the name (avoids `""d""` duplication).
		wasQuoted := start > 0 && sqlStr[start-1] == '"' &&
			end < len(sqlStr) && sqlStr[end] == '"'
		if wasQuoted {
			start--
			end++
		}
		b.WriteString(sqlStr[last:start])
		token := newColName
		if wasQuoted || quoteNew {
			token = `"` + newColName + `"`
		}
		b.WriteString(token)
		last = end
	}
	b.WriteString(sqlStr[last:])
	return b.String()
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
func (e *Engine) validateRename(oldName, newName string) error {
	tableEntry, _, err := e.findTable(oldName)
	if err != nil {
		return err
	}

	if e.legacyAlterTable {
		// In legacy mode, the CREATE SQL is NOT updated with token-level rename.
		// Check the table's own SQL for qualified references to the old table name
		// that would break after rename (they won't be updated).
		refs := findQualifiedTableRefs(tableEntry.SQL, oldName)
		if len(refs) > 0 {
			return fmt.Errorf("error in table %s after rename: no such column: %s", newName, refs[0])
		}
		// Also check indexes on this table for qualified references
		entries, gErr := e.schema.GetEntries("")
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

	// Non-legacy mode: the token-level rename will update qualified references in
	// the table's own SQL and indexes. Only check triggers and views for references
	// to tables OTHER than the one being renamed (which won't be updated).
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return nil
	}
	// Check all triggers for references to non-existent tables
	// Only check references to tables OTHER than the one being renamed.
	for _, entry := range entries {
		if entry.Type == schema.TypeTrigger {
			// Only triggers ON the renamed table or whose body references the
			// renamed table are re-validated by SQLite; triggers on unrelated
			// tables must not block the rename.
			if !strings.EqualFold(entry.TblName, oldName) &&
				!refTableInTrigger(entry.SQL, oldName) {
				continue
			}
			// Extract the trigger body and check for table references
			bodyRefs := findTableRefsInTrigger(entry.SQL)
			for _, ref := range bodyRefs {
				// Strip schema prefix for lookup
				lookupName := ref
				if dotIdx := strings.Index(lookupName, "."); dotIdx >= 0 {
					lookupName = lookupName[dotIdx+1:]
				}
				// Skip the table being renamed (its references will be updated)
				if strings.EqualFold(lookupName, oldName) {
					continue
				}
				// Skip special keywords or pseudo-tables
				if strings.EqualFold(lookupName, "NEW") || strings.EqualFold(lookupName, "OLD") {
					continue
				}
				// Skip SET keyword (from "UPDATE tablename SET" pattern)
				if strings.EqualFold(lookupName, "SET") {
					continue
				}
				_, _, err := e.findTable(lookupName)
				if err != nil {
					// Check if it's a view before reporting error
					if _, _, err2 := e.findView(lookupName); err2 != nil {
						// Format error message: prepend "main." for main-schema
						// triggers; TEMP triggers omit the schema prefix
						// (SQLite: "no such table: u8" for temp, "no such
						// table: main.u8" for main).
						refName := ref
						if !strings.Contains(ref, ".") && !e.isTempTrigger(entry) {
							refName = "main." + ref
						}
						return fmt.Errorf("error in trigger %s: no such table: %s", entry.Name, refName)
					}
				}
			}
		}
	}
	// Check for views with circular references (self-referencing views).
	// ALTER TABLE RENAME rejects if any view has a circular reference,
	// even if the view does not reference the renamed table.
	for _, entry := range entries {
		if entry.Type == schema.TypeView {
			if hasViewCircularRef(entry.SQL, entry.Name) {
				return fmt.Errorf("error in view %s: view %s is circularly defined", entry.Name, entry.Name)
			}
		}
	}
	// Check all triggers for column references that don't exist in their ON table.
	// SQLite does this during ALTER TABLE RENAME to catch broken trigger definitions.
	for _, entry := range entries {
		if entry.Type == schema.TypeTrigger {
			if err := e.checkTriggerColRefs(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// isTempTrigger reports whether a trigger lives in the TEMP schema. A trigger
// is temp if it was created with CREATE TEMP TRIGGER or if its ON table is a
// temp table (CREATE TEMP TABLE).
func (e *Engine) isTempTrigger(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	upper := strings.ToUpper(entry.SQL)
	if strings.Contains(upper, "CREATE TEMP TRIGGER") || strings.Contains(upper, "CREATE TEMPORARY TRIGGER") {
		return true
	}
	// Check if the ON table is a temp table.
	if entry.TblName != "" {
		if te, err := e.schema.FindTable(entry.TblName); err == nil && te != nil {
			if strings.Contains(strings.ToUpper(te.SQL), "CREATE TEMP TABLE") ||
				strings.Contains(strings.ToUpper(te.SQL), "CREATE TEMPORARY TABLE") {
				return true
			}
		}
	}
	return false
}

// checkTriggerColRefs checks that all column references in a trigger's SQL
// exist in the trigger's ON table or in tables referenced by its body.
// Returns an error formatted to match SQLite's "error in trigger %s: ..." pattern.
func (e *Engine) checkTriggerColRefs(entry *schema.Entry) error {
	// Parse the trigger SQL to get its AST
	stmts, perr := parse.ParseSQL(entry.SQL)
	if perr != nil || len(stmts) == 0 {
		return nil // Can't validate if we can't parse
	}

	// Get the ON table for this trigger
	onTableName := entry.TblName

	// Walk all statements looking for unqualified ColumnRefs at the top level
	// (not inside subqueries, which have their own table scope)
	colRefs := findTriggerColRefs(stmts)

	// Get column names for the ON table
	onTableEntry, err := e.schema.FindTable(onTableName)
	if err != nil {
		return nil // If the table doesn't exist, can't validate columns
	}
	onTableCols := e.parseColumnDefs(onTableEntry.Name, onTableEntry.SQL)
	onColMap := make(map[string]bool)
	for _, c := range onTableCols {
		onColMap[strings.ToUpper(c.Name)] = true
	}

	// Check each column reference
	for _, ref := range colRefs {
		upperName := strings.ToUpper(ref.Name)
		// Skip special pseudo-columns and keywords
		if ref.Table != "" {
			upperTable := strings.ToUpper(ref.Table)
			if upperTable == "NEW" || upperTable == "OLD" {
				continue
			}
			// Qualified reference to a non-pseudo table - just skip for now
			// as proper validation would require resolving the table
			continue
		}
		// Skip SQL keywords and special names that might be parsed as column refs
		if isSQLKeywordOrPseudo(upperName) {
			continue
		}
		// Unqualified column reference - check against ON table's columns
		if !onColMap[upperName] {
			return fmt.Errorf("error in trigger %s: no such column: %s", entry.Name, ref.Name)
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

// checkTriggerConflictCols validates ON CONFLICT (...) target columns in a
// trigger's INSERT statements against the INSERT target table's columns.
func (e *Engine) checkTriggerConflictCols(entry *schema.Entry) error {
	stmts, perr := parse.ParseSQL(entry.SQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	for _, stmt := range stmts {
		trig, ok := stmt.(*sql.CreateTriggerStmt)
		if !ok {
			continue
		}
		for _, bodyStmt := range trig.Statements {
			ins, ok := bodyStmt.(*sql.InsertStmt)
			if !ok || ins.OnConflict == nil || ins.OnConflict.ConflictColumn == "" {
				continue
			}
			targetEntry, err := e.schema.FindTable(ins.Table)
			if err != nil {
				continue
			}
			targetCols := e.parseColumnDefs(targetEntry.Name, targetEntry.SQL)
			found := false
			for _, c := range targetCols {
				if strings.EqualFold(c.Name, ins.OnConflict.ConflictColumn) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("error in trigger %s: no such column: %s", entry.Name, ins.OnConflict.ConflictColumn)
			}
		}
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

// collectTriggerColRefs walks a statement and collects ColumnRefs, tracking
// whether we're inside a subquery (which has its own table scope).
func collectTriggerColRefs(stmt sql.Stmt, refs *[]*sql.ColumnRef, inSubquery bool) {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		collectSelectTriggerColRefs(s, refs, inSubquery)
	case *sql.InsertStmt:
		if s.Select != nil {
			collectSelectTriggerColRefs(s.Select, refs, true) // INSERT ... SELECT is a subquery
		}
		// ON CONFLICT ... DO UPDATE SET assignments and WHERE can reference
		// columns of the conflict target table.
		if s.OnConflict != nil {
			for _, a := range s.OnConflict.Assignments {
				collectExprTriggerColRefs(a.Value, refs, false)
			}
			if s.OnConflict.Where != nil {
				collectExprTriggerColRefs(s.OnConflict.Where, refs, false)
			}
		}
	case *sql.UpdateStmt:
		// UPDATE SET values can reference columns from the FROM clause
		// (UPDATE ... SET x=y FROM ...), which frigolite doesn't fully parse.
		// Treat SET values as being in a subquery scope to avoid false positive
		// column validation errors.
		for _, a := range s.Assignments {
			collectExprTriggerColRefs(a.Value, refs, true) // true = subquery scope
		}
		if s.Where != nil {
			collectExprTriggerColRefs(s.Where, refs, inSubquery)
		}
	case *sql.DeleteStmt:
		if s.Where != nil {
			collectExprTriggerColRefs(s.Where, refs, inSubquery)
		}
	case *sql.CreateTriggerStmt:
		for _, bodyStmt := range s.Statements {
			collectTriggerColRefs(bodyStmt, refs, false)
		}
	}
}

// collectSelectTriggerColRefs walks a SELECT and collects ColumnRefs.
func collectSelectTriggerColRefs(sel *sql.SelectStmt, refs *[]*sql.ColumnRef, inSubquery bool) {
	if sel == nil {
		return
	}
	// Column refs in SELECT columns are in the current scope
	for _, col := range sel.Columns {
		collectExprTriggerColRefs(col.Expr, refs, inSubquery)
	}
	// WHERE, HAVING, GROUP BY, ORDER BY are in the current scope
	if sel.Where != nil {
		collectExprTriggerColRefs(sel.Where, refs, inSubquery)
	}
	if sel.Having != nil {
		collectExprTriggerColRefs(sel.Having, refs, inSubquery)
	}
	for _, expr := range sel.GroupBy {
		collectExprTriggerColRefs(expr, refs, inSubquery)
	}
	for _, ob := range sel.OrderBy {
		collectExprTriggerColRefs(ob.Expr, refs, inSubquery)
	}
	// JOIN conditions are in the current scope
	for _, join := range sel.Joins {
		if join.On != nil {
			collectExprTriggerColRefs(join.On, refs, inSubquery)
		}
	}
	// UNION subqueries have their own scope
	if sel.Union != nil {
		collectSelectTriggerColRefs(sel.Union, refs, true)
	}
	// CTE subqueries have their own scope
	for _, cte := range sel.CTEs {
		if cte.Select != nil {
			collectSelectTriggerColRefs(cte.Select, refs, true)
		}
	}
	// Window definitions contain PARTITION BY and ORDER BY expressions
	// that may reference columns of the current scope
	for _, w := range sel.Windows {
		collectWindowDefTriggerColRefs(&w, refs, inSubquery)
	}
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

// collectExprTriggerColRefs walks an expression tree and collects ColumnRefs,
// marking when we enter a subquery (which has its own table scope).
func collectExprTriggerColRefs(expr sql.Expr, refs *[]*sql.ColumnRef, inSubquery bool) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *sql.ColumnRef:
		// Only collect qualified refs or top-level unqualified refs
		if !inSubquery {
			*refs = append(*refs, e)
		}
	case *sql.BinaryOp:
		collectExprTriggerColRefs(e.Left, refs, inSubquery)
		collectExprTriggerColRefs(e.Right, refs, inSubquery)
	case *sql.UnaryOp:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	case *sql.FuncCall:
		for _, arg := range e.Args {
			collectExprTriggerColRefs(arg, refs, inSubquery)
		}
		if e.Filter != nil {
			collectExprTriggerColRefs(e.Filter, refs, inSubquery)
		}
		if e.Over != nil {
			collectWindowDefTriggerColRefs(e.Over, refs, inSubquery)
		}
	case *sql.ParenExpr:
		collectExprTriggerColRefs(e.Expr, refs, inSubquery)
	case *sql.CaseExpr:
		if e.Operand != nil {
			collectExprTriggerColRefs(e.Operand, refs, inSubquery)
		}
		for _, w := range e.Whens {
			collectExprTriggerColRefs(w.When, refs, inSubquery)
			collectExprTriggerColRefs(w.Then, refs, inSubquery)
		}
		if e.Else != nil {
			collectExprTriggerColRefs(e.Else, refs, inSubquery)
		}
	case *sql.CastExpr:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	case *sql.IsNull:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	case *sql.IsNotNull:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	case *sql.IsTrue:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	case *sql.IsFalse:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	case *sql.Between:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
		collectExprTriggerColRefs(e.Low, refs, inSubquery)
		collectExprTriggerColRefs(e.High, refs, inSubquery)
	case *sql.InList:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
		for _, item := range e.List {
			collectExprTriggerColRefs(item, refs, inSubquery)
		}
	case *sql.Subquery:
		// Subquery expressions have their own scope - don't collect refs inside
		// but still recurse to find any nested refs (marked with inSubquery=true)
		if e.Select != nil {
			collectSelectTriggerColRefs(e.Select, refs, true)
		}
	case *sql.ExistsExpr:
		if e.Select != nil {
			collectSelectTriggerColRefs(e.Select, refs, true)
		}
	case *sql.RowValue:
		for _, v := range e.Values {
			collectExprTriggerColRefs(v, refs, inSubquery)
		}
	}
}

// findTableRefsInTrigger extracts table references from a trigger body.
// Returns a list of referenced table names found in INSERT, UPDATE, DELETE, SELECT statements.
func findTableRefsInTrigger(triggerSQL string) []string {
	var refs []string

	// Extract CTE names from WITH definitions — these should be skipped
	// as they are not real table references.
	cteNames := extractCTENames(triggerSQL)
	cteSet := make(map[string]bool)
	for _, name := range cteNames {
		cteSet[name] = true
	}

	// Helper to check if a name is a CTE (should be skipped)
	isCTE := func(name string) bool {
		return cteSet[strings.ToUpper(name)] || cteSet[name]
	}

	// Find "INSERT INTO tablename" patterns (case-insensitive)
	// Use word-character matching to avoid capturing trailing punctuation like ";"
	re := regexp.MustCompile(`(?i)INSERT\s+INTO\s+([a-zA-Z_]\w*)`)
	matches := re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		if !isCTE(m[1]) {
			refs = append(refs, m[1])
		}
	}

	// Find "FROM tablename" patterns (case-insensitive)
	re = regexp.MustCompile(`(?i)\bFROM\s+([a-zA-Z_]\w*)`)
	matches = re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		t := m[1]
		// Skip special keywords or pseudo-tables
		if strings.EqualFold(t, "NEW") || strings.EqualFold(t, "OLD") {
			continue
		}
		if isCTE(t) {
			continue
		}
		refs = append(refs, t)
	}

	// Find "UPDATE tablename" patterns (case-insensitive)
	re = regexp.MustCompile(`(?i)\bUPDATE\s+([a-zA-Z_]\w*)`)
	matches = re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		t := m[1]
		// "UPDATE OF col1, col2" (trigger event column list) is not a table
		// reference; OF is a keyword in that clause.
		if strings.EqualFold(t, "SET") || strings.EqualFold(t, "OF") {
			continue
		}
		if isCTE(t) {
			continue
		}
		refs = append(refs, t)
	}

	// Find "DELETE FROM tablename" patterns (case-insensitive)
	re = regexp.MustCompile(`(?i)DELETE\s+FROM\s+([a-zA-Z_]\w*)`)
	matches = re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		if !isCTE(m[1]) {
			refs = append(refs, m[1])
		}
	}

	// Find "JOIN tablename" patterns (case-insensitive) — captures table names
	// in JOIN clauses like "FROM t1 JOIN t2 ON ..." or "t1 INNER JOIN t2 ..."
	re = regexp.MustCompile(`(?i)\bJOIN\s+([a-zA-Z_]\w*)`)
	matches = re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		t := m[1]
		if strings.EqualFold(t, "NEW") || strings.EqualFold(t, "OLD") {
			continue
		}
		if isCTE(t) {
			continue
		}
		refs = append(refs, t)
	}

	return refs
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

// hasViewCircularRef checks if a view has a circular reference (references its own name).
// View SQL format: "CREATE VIEW name AS SELECT ..."
func hasViewCircularRef(viewSQL, viewName string) bool {
	if viewSQL == "" || viewName == "" {
		return false
	}
	// Find " AS " after the view definition — extract the SELECT body
	upper := strings.ToUpper(viewSQL)
	idx := strings.Index(upper, " AS ")
	if idx < 0 {
		return false
	}
	// Get the SELECT body part after " AS "
	bodySQL := viewSQL[idx+4:]

	// Parse the SELECT to check for circular references
	stmts, perr := parse.ParseSQL(bodySQL)
	if perr != nil || len(stmts) == 0 {
		return false
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return false
	}
	// Check if the view's own name appears as a table reference in FROM or JOINs.
	// A CTE declared in the view body shadows the view name, so FROM v0 when
	// "WITH v0 AS (...)" exists refers to the CTE, not the view.
	hasCTE := false
	for _, cte := range sel.CTEs {
		if strings.EqualFold(cte.Name, viewName) {
			hasCTE = true
		}
	}
	if !hasCTE && strings.EqualFold(sel.From.Name, viewName) {
		return true
	}
	if !hasCTE {
		for _, j := range sel.Joins {
			if strings.EqualFold(j.Table.Name, viewName) {
				return true
			}
		}
	}
	// Check CTE definitions for circular references. A CTE that shares the
	// view's own name shadows it inside the body (not circular). Only a CTE
	// whose body references the view is circular.
	for _, cte := range sel.CTEs {
		if cte.Select != nil && strings.EqualFold(cte.Select.From.Name, viewName) {
			// The CTE references the view in its FROM clause.
			// Only flag as circular if the CTE is actually used in the main statement.
			// If the main statement has no FROM (e.g., VALUES subquery), the CTE is unused.
			if isCTEReferencedInMain(sel, cte.Name) {
				return true
			}
		}
	}
	return false
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
func (e *Engine) renameUpdateRelatedEntries(oldName, newName string) {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return
	}

	// Always quote the new name with double quotes, matching SQLite's behavior
	// in ALTER TABLE RENAME (the replacement text is always quoted).
	quotedNew := `"` + newName + `"`
	ctx := &RenameContext{
		OldName:   oldName,
		NewName:   newName,
		QuotedNew: quotedNew,
		IsTable:   true,
	}

	for _, entry := range entries {
		if entry.SQL == "" {
			continue
		}

		// Skip entries that don't reference the old table name.
		// Check both SQL content and TblName (triggers may have updated TblName
		// but unchanged SQL in legacy mode).
		if !strings.Contains(entry.SQL, oldName) &&
			!strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(oldName)) &&
			!strings.EqualFold(entry.TblName, oldName) {
			continue
		}

		switch entry.Type {
		case schema.TypeView:
			if e.legacyAlterTable {
				// In legacy mode, views are NOT updated — they keep old references
				continue
			}
		case schema.TypeTrigger:
			if strings.EqualFold(entry.TblName, oldName) {
				entry.TblName = newName
				// Persist the TblName change to the schema even in legacy mode.
				// GetEntries returns copies, so modifications are lost without saving.
				_ = e.schema.RemoveEntry(entry.Name)
				_ = e.schema.AddEntry(entry)
			}
			if e.legacyAlterTable {
				continue
			}
		case schema.TypeIndex:
			if strings.EqualFold(entry.TblName, oldName) {
				entry.TblName = newName
			}
			if e.legacyAlterTable {
				// In legacy mode, indexes keep their old SQL (only TblName updated)
				continue
			}
		case schema.TypeTable:
			// Update child tables that reference the old table name via FOREIGN KEY
			// Also update the table's own CREATE TABLE SQL
		default:
			continue
		}

		// Try token-level rename first, then apply string-regex as a complementary
		// pass to catch any occurrences the token walker missed (e.g., table names
		// in UPDATE statements inside trigger bodies, unparseable SQL).
		ranges, rErr := FindRenameTokens(entry.SQL, ctx)
		if rErr == nil && len(ranges) > 0 {
			newSQL := ApplyRenames(entry.SQL, ranges, quotedNew)
			if newSQL != entry.SQL && newSQL != "" {
				// Apply string-regex as a complementary pass to catch remaining occurrences
				newSQL = replaceTableNameInSQL(newSQL, oldName, newName)
				if newSQL != entry.SQL {
					entry.SQL = newSQL
					_ = e.schema.RemoveEntry(entry.Name)
					_ = e.schema.AddEntry(entry)
					continue
				}
			}
		}

		// Fallback: use string-regex alone when token-level rename fails or finds nothing
		newSQL := replaceTableNameInSQL(entry.SQL, oldName, newName)
		if newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			_ = e.schema.RemoveEntry(entry.Name)
			_ = e.schema.AddEntry(entry)
		}
	}
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

// replaceTableNameInSQL replaces occurrences of oldTableName with newTableName in SQL text.
// Uses word-boundary matching to avoid partial matches (e.g., renaming t1 should not match t10).
// Always quotes the new name with double quotes to handle names with spaces or special chars.
func replaceTableNameInSQL(sql, oldName, newName string) string {
	quotedNew := `"` + newName + `"`
	quotedOld := regexp.QuoteMeta(oldName)
	// First, replace already-quoted occurrences: "t3" -> "t4" (avoids double-quoting)
	re := regexp.MustCompile(`(?i)"` + quotedOld + `"`)
	if re.MatchString(sql) {
		return re.ReplaceAllString(sql, quotedNew)
	}
	// Fallback: replace unquoted occurrences with word boundaries, adding quotes
	re = regexp.MustCompile(`\b` + quotedOld + `\b`)
	return re.ReplaceAllString(sql, quotedNew)
}

// validateAddColumnConstraints evaluates a newly added column's CHECK and
// NOT NULL constraints against the table's existing rows, matching SQLite's
// ALTER TABLE ADD COLUMN semantics:
//   - A NOT NULL column with a NULL default cannot be added when the table
//     already has rows (SQLite: "Cannot add a NOT NULL column with default
//     value NULL").
//   - A CHECK constraint is evaluated against every existing row; if any row
//     makes it false the add fails with "CHECK constraint failed".
func (e *Engine) validateAddColumnConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, newCol sql.ColumnDef) *Result {
	// SQLite: "Cannot add a REFERENCES column with non-NULL default value" —
	// a column with a REFERENCES clause may not have a non-NULL constant
	// default (the FK would be ambiguous for existing rows).
	if newCol.References != "" && e.foreignKeys {
		if newCol.Default != nil {
			// DEFAULT NULL is allowed; any other non-NULL constant is not.
			defVal, derr := e.evalColumnExpr(newCol)
			if derr == nil && defVal != nil {
				if s, ok := defVal.(string); ok && strings.EqualFold(strings.Trim(s, `'"`), "NULL") {
					// NULL literal
				} else {
					return &Result{Error: fmt.Errorf("Cannot add a REFERENCES column with non-NULL default value")}
				}
			}
		}
	}
	if newCol.Check == nil && !newCol.NotNull {
		return &Result{}
	}
	// Determine the default value for the new column.
	defVal, err := e.evalColumnExpr(newCol)
	if err != nil {
		return &Result{Error: err}
	}

	if newCol.NotNull && defVal == nil {
		// NOT NULL without a non-NULL default is only allowed when the table
		// has no rows.
		hasRows := e.tableHasRows(tableEntry)
		if hasRows {
			return &Result{Error: fmt.Errorf("Cannot add a NOT NULL column with default value NULL")}
		}
	}
	if newCol.Check == nil {
		return &Result{}
	}

	// Evaluate CHECK against existing rows. Each existing row gets the new
	// column set to its default value (matching SQLite).
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{}
	}
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		row[newCol.Name] = defVal
		checkVal, verr := e.evalExpr(newCol.Check, row)
		if verr == nil && checkVal != nil && !toBool(checkVal) {
			checkText := e.checkConstraintText(tableEntry.SQL, newCol.Name, newCol.Check)
			return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", checkText)}
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return &Result{}
}

// validateAddConstraint evaluates a table-level constraint added by
// ALTER TABLE ... ADD CONSTRAINT against the table's existing rows. SQLite
// rejects the ALTER if any existing row violates the new constraint.
func (e *Engine) validateAddConstraint(tableEntry *schema.Entry, tc *sql.TableConstraint) *Result {
	if tc == nil || tc.Type != sql.ConstraintCheck || tc.Expr == nil {
		return &Result{}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{}
	}
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		checkVal, verr := e.evalExpr(tc.Expr, row)
		if verr == nil && checkVal != nil && !toBool(checkVal) {
			name := tc.Name
			if name == "" {
				name = sql.ExprString(tc.Expr)
			}
			return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", name)}
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return &Result{}
}

// evalColumnExpr evaluates a column's DEFAULT expression (with no row context)
// to its concrete value.
func (e *Engine) evalColumnExpr(col sql.ColumnDef) (interface{}, error) {
	if col.Default == nil {
		return nil, nil
	}
	return e.evalExpr(col.Default, nil)
}

// tableHasRows reports whether the table currently has at least one row.
func (e *Engine) tableHasRows(entry *schema.Entry) bool {
	tree := e.tableBTreeForName(entry.Name, entry.RootPage, true)
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
func (e *Engine) columnHasNull(entry *schema.Entry, colDefs []sql.ColumnDef, colName string) bool {
	tree := e.tableBTreeForName(entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		for i, cd := range colDefs {
			if cd.Name == colName {
				if i >= len(rec.Values) || rec.Values[i] == nil {
					return true
				}
				break
			}
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return false
}

func (e *Engine) execAlterTableAdd(s *sql.AlterTableStmt) *Result {
	// ALTER TABLE ... ADD [COLUMN] column_def
	tableName := s.Table
	tableEntry, _, err := e.findTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// ALTER TABLE ... ADD [CONSTRAINT nm] CHECK(expr): append a table-level
	// constraint to the stored CREATE TABLE SQL and invalidate caches.
	if s.NewConstraint != nil {
		// Validate the constraint against existing rows before committing.
		if vres := e.validateAddConstraint(tableEntry, s.NewConstraint); vres.Error != nil {
			return vres
		}
		newSQL := addConstraintToCreateTableSQL(tableEntry.SQL, s.NewConstraint)
		if newSQL != "" && newSQL != tableEntry.SQL {
			tableEntry.SQL = newSQL
			delete(e.tableCache, tableName)
			delete(e.tcCache, tableName)
			_ = e.schema.RemoveEntry(tableEntry.Name)
			if err := e.schema.AddEntry(tableEntry); err != nil {
				return &Result{Error: fmt.Errorf("failed to re-add entry after DDL: %w", err)}
			}
			if _, err := e.schema.FindTable(tableEntry.Name); err != nil {
				if retryErr := e.schema.AddEntry(tableEntry); retryErr != nil {
					return &Result{Error: fmt.Errorf("schema consistency check failed: entry %s lost after DDL", tableEntry.Name)}
				}
			}
		}
		return &Result{}
	}

	// Validate column name
	if s.ColDef.Name != "" {
		// Check for duplicate column name
		colDefs := e.colCache[tableName]
		if colDefs == nil {
			colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
		}
		for _, c := range colDefs {
			if strings.EqualFold(c.Name, s.ColDef.Name) {
				return &Result{Error: fmt.Errorf("duplicate column name: %q", s.ColDef.Name)}
			}
		}

		// STRICT tables only allow the standard datatypes (INT, INTEGER,
		// REAL, TEXT, BLOB, ANY). Reject custom or missing datatypes with
		// SQLite's "error in table ... after add column:" message.
		if hasStrictKeyword(strings.ToUpper(tableEntry.SQL)) {
			switch strings.ToUpper(strings.TrimSpace(s.ColDef.Type)) {
			case "INT", "INTEGER", "REAL", "TEXT", "BLOB", "ANY":
				// valid STRICT datatype
			case "":
				return &Result{Error: fmt.Errorf("error in table %s after add column: missing datatype for %s.%s",
					tableEntry.Name, tableEntry.Name, s.ColDef.Name)}
			default:
				return &Result{Error: fmt.Errorf("error in table %s after add column: unknown datatype for %s.%s: %q",
					tableEntry.Name, tableEntry.Name, s.ColDef.Name, s.ColDef.Type)}
			}
		}

		// Add column to cached column definitions
		// Validate CHECK/NOT NULL against existing rows before committing.
		if vres := e.validateAddColumnConstraints(tableEntry, colDefs, s.ColDef); vres.Error != nil {
			return vres
		}
		colDefs = append(colDefs, s.ColDef)
		e.colCache[tableName] = colDefs

		// Update the stored CREATE TABLE SQL to include the new column
		newSQL := addColumnToCreateTableSQL(tableEntry.SQL, s.ColDef)
		if newSQL != "" && newSQL != tableEntry.SQL {
			tableEntry.SQL = newSQL
			delete(e.tableCache, tableName)
			_ = e.schema.RemoveEntry(tableEntry.Name)
			if err := e.schema.AddEntry(tableEntry); err != nil {
				return &Result{Error: fmt.Errorf("failed to re-add entry after DDL: %w", err)}
			}
			// Verify the entry was re-added
			if _, err := e.schema.FindTable(tableEntry.Name); err != nil {
				if retryErr := e.schema.AddEntry(tableEntry); retryErr != nil {
					return &Result{Error: fmt.Errorf("schema consistency check failed: entry %s lost after DDL", tableEntry.Name)}
				}
			}
		}
	}

	return &Result{}
}

func (e *Engine) execAlterTableDrop(s *sql.AlterTableStmt) *Result {
	tableName := s.Table

	// Handle DROP CONSTRAINT - remove named constraint from schema SQL
	if s.Column == "CONSTRAINT" {
		constraintName := s.NewName
		if constraintName == "" {
			return &Result{}
		}
		tableEntry, err := e.schema.FindTable(tableName)
		if err != nil {
			return &Result{Error: err}
		}
		// Remove the named constraint from the CREATE TABLE SQL
		if !sqlHasConstraintName(tableEntry.SQL, constraintName) {
			return &Result{Error: fmt.Errorf("no such constraint: %s", constraintName)}
		}
		newSQL := removeConstraintFromSQL(tableEntry.SQL, constraintName)
		if newSQL != tableEntry.SQL {
			tableEntry.SQL = newSQL
			// Invalidate cached column/constraint info for this table.
			delete(e.colCache, tableName)
			delete(e.tcCache, tableName)
			delete(e.tableCache, tableName)
			_ = e.schema.RemoveEntry(tableEntry.Name)
			if err := e.schema.AddEntry(tableEntry); err != nil {
				return &Result{Error: fmt.Errorf("failed to re-add entry after DROP CONSTRAINT: %w", err)}
			}
			// Verify the entry was re-added
			if _, err := e.schema.FindTable(tableEntry.Name); err != nil {
				if retryErr := e.schema.AddEntry(tableEntry); retryErr != nil {
					return &Result{Error: fmt.Errorf("schema consistency check failed: entry %s lost after DROP CONSTRAINT", tableEntry.Name)}
				}
			}
		}
		return &Result{}
	}

	// Find the table entry first
	tableEntry, err := e.schema.FindTable(tableName)
	if err != nil {
		// Check if it's a view
		if viewEntry, viewErr := e.schema.FindView(tableName); viewErr == nil && viewEntry != nil {
			return &Result{Error: fmt.Errorf("cannot drop column from view %q", tableName)}
		}
		// Return the table not found error
		return &Result{Error: err}
	}

	// Check if it's a virtual table (has "USING" in SQL or uses a known module)
	if strings.Contains(tableEntry.SQL, "USING") || e.isVirtualTable(tableEntry) {
		return &Result{Error: fmt.Errorf("cannot drop column from virtual table %q", tableName)}
	}

	// Check if the table's SQL is malformed (doesn't look like a CREATE TABLE)
	upperSQL := strings.ToUpper(strings.TrimSpace(tableEntry.SQL))
	if !strings.HasPrefix(upperSQL, "CREATE TABLE") {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}

	// Check index dependencies before dropping
	if depResult := e.checkIndexDependencies(tableName, s.Column); depResult != nil {
		return depResult
	}

	// Check table-level constraint dependencies before dropping
	if depResult := e.checkTableConstraintDependencies(tableEntry.SQL, tableName, s.Column); depResult != nil {
		return depResult
	}

	// Check view dependencies (existing errors only)
	if depResult := e.checkViewDependencies(tableName, s.Column); depResult != nil {
		return depResult
	}
	// Check trigger dependencies (existing errors)
	if depResult := e.checkTriggerDependencies(tableName, s.Column); depResult != nil {
		return depResult
	}
	// Check "after drop column" view dependencies
	if depResult := e.checkViewDropDependencies(tableName, s.Column); depResult != nil {
		return depResult
	}

	// Check if it's the sqlite_master system table
	if strings.EqualFold(tableName, "sqlite_master") ||
		strings.EqualFold(tableName, "sqlite_temp_master") ||
		strings.EqualFold(tableName, "sqlite_schema") {
		return &Result{Error: fmt.Errorf("table sqlite_master may not be altered")}
	}

	// Remove column from cached column definitions
	colDefs := e.colCache[tableName]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}
	found := false
	var newColDefs []sql.ColumnDef
	for _, c := range colDefs {
		if c.Name == s.Column {
			// Cannot drop PRIMARY KEY columns
			if c.PrimaryKey {
				return &Result{Error: fmt.Errorf("cannot drop PRIMARY KEY column: %q", s.Column)}
			}
			// Cannot drop UNIQUE columns
			if c.Unique {
				return &Result{Error: fmt.Errorf("cannot drop UNIQUE column: %q", s.Column)}
			}
			found = true
			// Mark as dropped but keep in the list for correct record position mapping
			c.Dropped = true
			newColDefs = append(newColDefs, c)
			continue
		}
		newColDefs = append(newColDefs, c)
	}
	if !found {
		return &Result{Error: fmt.Errorf("no such column: \"%s\"", s.Column)}
	}
	// Cannot drop the last remaining visible column
	var visibleCount int
	for _, c := range newColDefs {
		if !c.Dropped {
			visibleCount++
		}
	}
	if visibleCount == 0 {
		e.colCache[tableName] = colDefs // restore original column list
		return &Result{Error: fmt.Errorf("cannot drop column %q: no other columns exist", s.Column)}
	}
	e.colCache[tableName] = newColDefs

	// Update the table's stored SQL to reflect the dropped column
	// Build a filtered list without dropped columns for the SQL
	var sqlColDefs []sql.ColumnDef
	for _, c := range newColDefs {
		if !c.Dropped {
			sqlColDefs = append(sqlColDefs, c)
		}
	}
	updateSQL := rebuildCreateTableSQL(tableEntry.SQL, sqlColDefs)
	if updateSQL != "" {
		tableEntry.SQL = updateSQL
		delete(e.tableCache, tableName)
		_ = e.schema.RemoveEntry(tableEntry.Name)
		if err := e.schema.AddEntry(tableEntry); err != nil {
			return &Result{Error: fmt.Errorf("failed to re-add entry after DDL: %w", err)}
		}
		// Verify the entry was re-added
		if _, err := e.schema.FindTable(tableEntry.Name); err != nil {
			if retryErr := e.schema.AddEntry(tableEntry); retryErr != nil {
				return &Result{Error: fmt.Errorf("schema consistency check failed: entry %s lost after DDL", tableEntry.Name)}
			}
		}
	}

	return &Result{}
}

func (e *Engine) execAlterTableAlter(s *sql.AlterTableStmt) *Result {
	// ALTER TABLE ... ALTER COLUMN SET NOT NULL / DROP NOT NULL
	if s.AlterColAction == "" {
		return &Result{}
	}
	tableName := s.Table
	tableEntry, err := e.schema.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	colDefs := e.colCache[tableName]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}

	// Find and update the column
	found := false
	for i, c := range colDefs {
		if c.Name == s.Column {
			switch s.AlterColAction {
			case "SET NOT NULL":
				// SQLite: SET NOT NULL fails with "constraint failed" if any
				// existing row has NULL in this column.
				if e.columnHasNull(tableEntry, colDefs, c.Name) {
					return &Result{Error: fmt.Errorf("constraint failed")}
				}
				colDefs[i].NotNull = true
			case "DROP NOT NULL":
				colDefs[i].NotNull = false
			}
			found = true
			break
		}
	}
	if !found {
		return &Result{Error: fmt.Errorf("no such column: \"%s\"", s.Column)}
	}
	e.colCache[tableName] = colDefs

	// Rebuild the CREATE TABLE SQL with updated column definitions
	// Filter out dropped columns
	var sqlColDefs []sql.ColumnDef
	for _, c := range colDefs {
		if !c.Dropped {
			sqlColDefs = append(sqlColDefs, c)
		}
	}
	updateSQL := rebuildCreateTableSQL(tableEntry.SQL, sqlColDefs)
	if updateSQL != "" {
		tableEntry.SQL = updateSQL
		delete(e.tableCache, tableName)
		_ = e.schema.RemoveEntry(tableEntry.Name)
		if err := e.schema.AddEntry(tableEntry); err != nil {
			return &Result{Error: fmt.Errorf("failed to re-add entry after DDL: %w", err)}
		}
		// Verify the entry was re-added
		if _, err := e.schema.FindTable(tableEntry.Name); err != nil {
			if retryErr := e.schema.AddEntry(tableEntry); retryErr != nil {
				return &Result{Error: fmt.Errorf("schema consistency check failed: entry %s lost after DDL", tableEntry.Name)}
			}
		}
	}

	return &Result{}
}

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

// removeConstraintFromSQL removes a named constraint from a CREATE TABLE SQL string.
func removeConstraintFromSQL(origSQL, constraintName string) string {
	upper := strings.ToUpper(origSQL)
	if !strings.Contains(upper, "CREATE TABLE") {
		return origSQL
	}
	// Find the content between outer parentheses
	parenStart := strings.Index(origSQL, "(")
	if parenStart < 0 {
		return origSQL
	}
	depth := 0
	parenEnd := -1
parenLoop2:
	for i := parenStart; i < len(origSQL); i++ {
		switch origSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break parenLoop2
			}
		}
	}
	if parenEnd < 0 {
		// No closing paren — treat end of string as the virtual closing paren
		parenEnd = len(origSQL)
	}

	trailingSQL := ""
	if parenEnd+1 < len(origSQL) {
		trailingSQL = strings.TrimSpace(origSQL[parenEnd+1:])
	}
	defText := origSQL[parenStart+1 : parenEnd]

	// Split by top-level commas
	var parts []string
	depth = 0
	start := 0
	for i := 0; i < len(defText); i++ {
		switch defText[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(defText[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(defText) {
		parts = append(parts, strings.TrimSpace(defText[start:]))
	}

	// Identify and remove the constraint with the matching name
	upperName := strings.ToUpper(constraintName)
	quotedName := `"` + constraintName + `"`
	upperQuotedName := strings.ToUpper(quotedName)
	var keptParts []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		upperPart := strings.ToUpper(part)
		// Check if this part is a CONSTRAINT clause with the matching name
		if strings.HasPrefix(upperPart, "CONSTRAINT ") {
			// Extract the constraint name from the part
			rest := strings.TrimSpace(part[11:]) // after "CONSTRAINT "
			restUpper := strings.ToUpper(rest)
			if strings.HasPrefix(restUpper, upperName) || strings.HasPrefix(restUpper, upperQuotedName) {
				// This is the constraint to drop - skip it entirely
				continue
			}
		}
		// Check for column-level constraint: colName CONSTRAINT name ...
		// Find " CONSTRAINT " within the part and check if the following name matches
		conIdx := strings.Index(upperPart, " CONSTRAINT ")
		if conIdx >= 0 {
			rest := strings.TrimSpace(part[conIdx+11:]) // after " CONSTRAINT "
			restUpper := strings.ToUpper(rest)
			if strings.HasPrefix(restUpper, upperName) || strings.HasPrefix(restUpper, upperQuotedName) {
				// Column-level constraint match — remove from CONSTRAINT to end
				// Keep only the column name and type, removing all constraints
				part = strings.TrimSpace(part[:conIdx])
			}
		}
		keptParts = append(keptParts, part)
	}

	// Rebuild the SQL
	var buf strings.Builder
	buf.WriteString(origSQL[:parenStart+1])
	for i, part := range keptParts {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(part)
	}
	buf.WriteString("\n)")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
}

// addConstraintToCreateTableSQL appends a table-level constraint (e.g.
// CONSTRAINT nm CHECK(expr)) to the stored CREATE TABLE SQL, inserting it
// before the closing parenthesis of the column list.
func addConstraintToCreateTableSQL(origSQL string, tc *sql.TableConstraint) string {
	if tc == nil {
		return origSQL
	}
	// Find the outer closing parenthesis of the CREATE TABLE column list.
	parenStart := strings.Index(origSQL, "(")
	if parenStart < 0 {
		return origSQL
	}
	depth := 0
	parenEnd := -1
addLoop:
	for i := parenStart; i < len(origSQL); i++ {
		switch origSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break addLoop
			}
		}
	}
	if parenEnd < 0 {
		return origSQL
	}

	trailingSQL := ""
	if parenEnd+1 < len(origSQL) {
		trailingSQL = strings.TrimSpace(origSQL[parenEnd+1:])
	}

	var buf strings.Builder
	buf.WriteString(origSQL[:parenEnd])
	if parenEnd > parenStart && !strings.HasSuffix(strings.TrimRight(origSQL[:parenEnd], " \t\n"), ",") {
		buf.WriteString(",")
	}
	buf.WriteString("\n")
	if tc.Name != "" {
		buf.WriteString("CONSTRAINT ")
		buf.WriteString(tc.Name)
		buf.WriteString(" ")
	}
	switch tc.Type {
	case sql.ConstraintCheck:
		buf.WriteString("CHECK(")
		if tc.Expr != nil {
			buf.WriteString(sql.ExprString(tc.Expr))
		}
		buf.WriteString(")")
	default:
		if tc.Type != "" {
			buf.WriteString(string(tc.Type))
		}
	}
	buf.WriteString("\n)")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
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

// rebuildCreateTableSQL rebuilds a CREATE TABLE SQL string with updated column definitions.
func rebuildCreateTableSQL(origSQL string, colDefs []sql.ColumnDef) string {
	upper := strings.ToUpper(origSQL)
	if !strings.Contains(upper, "CREATE TABLE") {
		return ""
	}
	// Extract table name (handles schema prefixes like main.t1)
	tableName := ""
	afterCreate := origSQL
	if idx := strings.Index(upper, "CREATE TABLE"); idx >= 0 {
		afterCreate = origSQL[idx+12:]
	}
	afterCreate = strings.TrimSpace(afterCreate)
	// The table name is the next word
	if idx := strings.IndexAny(afterCreate, " ("); idx >= 0 {
		tableName = strings.TrimSpace(afterCreate[:idx])
	} else {
		return ""
	}

	// Find the content between outer parentheses to extract table-level constraints
	parenStart := strings.Index(origSQL, "(")
	if parenStart < 0 {
		return ""
	}
	depth := 0
	parenEnd := -1
parenLoop3:
	for i := parenStart; i < len(origSQL); i++ {
		switch origSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break parenLoop3
			}
		}
	}
	if parenEnd < 0 {
		return ""
	}

	trailingSQL := strings.TrimSpace(origSQL[parenEnd+1:])
	defText := origSQL[parenStart+1 : parenEnd]

	// Build a set of column names from current column definitions
	colNames := make(map[string]bool)
	for _, cd := range colDefs {
		colNames[strings.ToUpper(cd.Name)] = true
	}

	// Parse the original definition text to extract table-level constraints.
	// Split by top-level commas (not inside nested parens).
	var parts []string
	depth = 0
	start := 0
	for i := 0; i < len(defText); i++ {
		switch defText[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(defText[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(defText) {
		parts = append(parts, strings.TrimSpace(defText[start:]))
	}

	// Separate column definitions from table-level constraints
	var constraints []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Check if this part looks like a column definition (starts with a known column name)
		upperPart := strings.ToUpper(trimmed)
		isColumnDef := false
		for name := range colNames {
			if strings.HasPrefix(upperPart, name) || strings.HasPrefix(upperPart, "\""+name+"\"") {
				isColumnDef = true
				break
			}
		}
		if !isColumnDef && (strings.HasPrefix(upperPart, "PRIMARY KEY") ||
			strings.HasPrefix(upperPart, "UNIQUE") ||
			strings.HasPrefix(upperPart, "CHECK") ||
			strings.HasPrefix(upperPart, "FOREIGN KEY") ||
			strings.HasPrefix(upperPart, "CONSTRAINT")) {
			constraints = append(constraints, trimmed)
		}
	}

	// Build a mapping from column name to original definition text
	origColDefs := make(map[string]string)
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Extract the column name (first word)
		spaceIdx := strings.IndexAny(trimmed, " (\"")
		if spaceIdx > 0 {
			name := strings.ToUpper(strings.Trim(trimmed[:spaceIdx], "\""))
			origColDefs[name] = trimmed
		} else if spaceIdx < 0 {
			// Single word column name
			name := strings.ToUpper(strings.Trim(trimmed, "\""))
			origColDefs[name] = trimmed
		}
	}

	// Build the final SQL
	var buf strings.Builder
	buf.WriteString("CREATE TABLE ")
	buf.WriteString(tableName)
	buf.WriteString("(")
	for i, col := range colDefs {
		if i > 0 {
			buf.WriteString(", ")
		}
		// Use original column text if available, otherwise reconstruct
		if orig, ok := origColDefs[strings.ToUpper(col.Name)]; ok {
			// Reconstruct when the column's NOT NULL constraint differs from the
			// original text (e.g. after ALTER COLUMN SET/DROP NOT NULL).
			if col.NotNull != origHasNotNull(orig) {
				formatColumnDef(&buf, col)
			} else {
				buf.WriteString(orig)
			}
		} else {
			formatColumnDef(&buf, col)
		}
	}
	for _, tc := range constraints {
		buf.WriteString(", ")
		buf.WriteString(tc)
	}
	buf.WriteString("\n)")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
}

// addColumnToCreateTableSQL adds a new column definition to a CREATE TABLE SQL string.
func addColumnToCreateTableSQL(origSQL string, colDef sql.ColumnDef) string {
	upper := strings.ToUpper(strings.TrimSpace(origSQL))
	if !strings.HasPrefix(upper, "CREATE TABLE") {
		return ""
	}

	// Find the closing paren of the table definition
	parenStart := strings.Index(origSQL, "(")
	if parenStart < 0 {
		return ""
	}
	depth := 0
	parenEnd := -1
parenLoop4:
	for i := parenStart; i < len(origSQL); i++ {
		switch origSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break parenLoop4
			}
		}
	}
	if parenEnd < 0 {
		return ""
	}

	// Build the column definition text
	var colBuf strings.Builder
	formatColumnDef(&colBuf, colDef)
	colText := colBuf.String()
	if colText == "" {
		return origSQL
	}

	// Insert the new column definition before the closing paren
	result := origSQL[:parenEnd] + ", " + colText + origSQL[parenEnd:]
	return result
}

func (e *Engine) isVirtualTable(entry *schema.Entry) bool {
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
func (e *Engine) getFTSModule(moduleName string) *fts.FTS3Module {
	m, ok := e.vtabs.Find(moduleName)
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
func (e *Engine) getFTSModuleForTable(tableName string) *fts.FTS3Module {
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

// checkIndexRenameDependencies validates that every index on the given table
// references only existing columns. SQLite re-parses index definitions when
// re-running ALTER TABLE RENAME COLUMN and rejects the rename if any indexed
// column does not exist ("error in index %s: no such column: %s").
func (e *Engine) checkIndexRenameDependencies(tableName string) *Result {
	entries, err := e.schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		// Auto-generated indexes (sqlite_autoindex_*) have empty SQL and are
		// not re-validated by SQLite.
		if strings.HasPrefix(strings.ToLower(entry.Name), "sqlite_autoindex_") {
			continue
		}
		// An index with empty SQL (e.g. edited via writable_schema) is
		// malformed; SQLite reports "error in index %s: " on rename.
		if strings.TrimSpace(entry.SQL) == "" {
			return &Result{Error: fmt.Errorf("error in index %s: ", entry.Name)}
		}
		cols := indexColumnRefs(entry.SQL)
		for _, col := range cols {
			if !e.tableHasColumn(tableName, col) {
				return &Result{Error: fmt.Errorf("error in index %s: no such column: %s", entry.Name, col)}
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
func (e *Engine) tableHasColumn(tableName, colName string) bool {
	entry, _, err := e.findTable(tableName)
	if err != nil {
		return false
	}
	colDefs := e.colCache[tableName]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(entry.Name, entry.SQL)
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
func (e *Engine) checkIndexDependencies(tableName, columnName string) *Result {
	entries, err := e.schema.GetEntries(schema.TypeIndex)
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

// checkViewDependencies validates all views before dropping a column.
// Uses simple text-based scanning to find column references.
func (e *Engine) checkViewDependencies(tableName, columnName string) *Result {
	views, err := e.schema.GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		upperSQL := strings.ToUpper(view.SQL)
		// Find the table referenced by this view (after FROM)
		fromIdx := strings.Index(upperSQL, " FROM ")
		if fromIdx < 0 {
			continue
		}
		fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
		spaceIdx := strings.IndexAny(fromRest, " \n\t\r")
		refTable := ""
		if spaceIdx > 0 {
			refTable = fromRest[:spaceIdx]
		} else {
			refTable = fromRest
		}
		if refTable == "" {
			continue
		}
		// Check if this view references the target table
		refersToTarget := strings.EqualFold(refTable, tableName)

		// Get the referenced table's column definitions
		entry, findErr := e.schema.FindTable(refTable)
		if findErr != nil {
			// Table doesn't exist - the view references a non-existent table
			return &Result{Error: fmt.Errorf("error in view %s: %s", view.Name, findErr.Error())}
		}
		colDefs := e.colCache[refTable]
		if colDefs == nil {
			colDefs = e.parseColumnDefs(entry.Name, entry.SQL)
		}
		// Build set of valid column names (excluding dropped)
		validCols := make(map[string]bool)
		for _, cd := range colDefs {
			if cd.Dropped {
				continue
			}
			validCols[strings.ToUpper(cd.Name)] = true
		}
		// Also include the column being dropped IF it exists (for "after drop" check)
		if refersToTarget {
			// Check if the column being dropped is in the current valid columns
			_ = columnName
		}
		// Extract column names from the SELECT part of the view
		selIdx := strings.Index(strings.ToUpper(view.SQL), "SELECT ")
		if selIdx < 0 {
			continue
		}
		afterSelect := view.SQL[selIdx+7 : fromIdx]
		// Split by commas and extract column names
		viewCols := strings.FieldsFunc(afterSelect, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
		})
		// Phase 1: Check for existing errors (all views)
		// Skip column references that match the column being dropped (checked later)
		for _, col := range viewCols {
			col = strings.TrimSpace(col)
			if col == "" {
				continue
			}
			upperCol := strings.ToUpper(col)
			if upperCol == "DISTINCT" || upperCol == "ALL" || upperCol == "AS" {
				continue
			}
			if strings.Contains(upperCol, ".") {
				parts := strings.Split(upperCol, ".")
				if len(parts) == 2 {
					upperCol = parts[1]
				}
			}
			// Skip the column being dropped — its validity is checked later
			if refersToTarget && strings.EqualFold(col, columnName) {
				continue
			}
			if !validCols[strings.ToUpper(upperCol)] && upperCol != "*" {
				return &Result{Error: fmt.Errorf("error in view %s: no such column: %s",
					view.Name, col)}
			}
		}
	}
	return nil
}

// extractIdentifierTokens splits a SELECT-list fragment into bare identifier
// tokens, discarding operators, literals and punctuation. Function names
// (identifiers immediately followed by an open parenthesis) are excluded —
// they are callable, not column references. Used to validate view column
// references: for "a+10, b*5.0, xyz" it yields [a, 10, b, 5, 0, xyz]
// (numeric literals are later skipped as non-columns).
func extractIdentifierTokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	for i := 0; i < len(s); {
		r := rune(s[i])
		// A double-quoted identifier is an atomic token ("a;b" must not be
		// split into a and b).
		if r == '"' {
			flush()
			end := strings.Index(s[i+1:], "\"")
			if end < 0 {
				break
			}
			out = append(out, s[i+1:i+1+end])
			i = i + 2 + end
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '.' {
			cur = append(cur, r)
		} else {
			// An identifier immediately followed by '(' is a function name, not
			// a column reference (e.g. group_concat(a ORDER BY b)).
			if r == '(' && len(cur) > 0 {
				// Skip any pending identifier that is directly attached to '('.
				// There is no whitespace between the name and '(' (the tokenizer
				// would otherwise have split it), so drop the current token.
				if i > 0 && isIdentByte(s[i-1]) {
					cur = nil
				}
				flush()
				i++
				continue
			}
			flush()
		}
		i++
	}
	flush()
	return out
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
func (e *Engine) quoteFixWithSchema(schemaName, sqlStr string) string {
	if sqlStr == "" {
		return ""
	}
	// Determine the main table name from the SQL (CREATE TABLE x, CREATE
	// INDEX ... ON x, CREATE TRIGGER ... ON x, SELECT ... FROM x).
	tableName := mainTableFromSchemaSQL(sqlStr)
	var colSet map[string]bool
	if tableName != "" {
		if entry, _, err := e.findTable(tableName); err == nil {
			colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
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

// createTableColumnNames extracts the bare column names from a CREATE TABLE
// statement's parenthesized column list.
func createTableColumnNames(sqlStr string) []string {
	open := strings.Index(sqlStr, "(")
	if open < 0 {
		return nil
	}
	depth := 0
	end := -1
	for i := open; i < len(sqlStr); i++ {
		switch sqlStr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
				i = len(sqlStr)
			}
		}
	}
	if end < 0 {
		return nil
	}
	inner := sqlStr[open+1 : end]
	var cols []string
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// First token is the column name (strip quotes).
		name := part
		if idx := strings.IndexAny(part, " \t\n"); idx > 0 {
			name = part[:idx]
		}
		name = strings.Trim(name, "\"`")
		if name != "" && !strings.Contains(strings.ToUpper(name), "PRIMARY") &&
			!strings.Contains(strings.ToUpper(name), "UNIQUE") &&
			!strings.Contains(strings.ToUpper(name), "CHECK") &&
			!strings.Contains(strings.ToUpper(name), "FOREIGN") &&
			!strings.Contains(strings.ToUpper(name), "CONSTRAINT") {
			cols = append(cols, name)
		}
	}
	return cols
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

// quoteFixSQLWithColumns rewrites double-quoted tokens: tokens that are not
// valid identifiers, or that are valid identifiers but NOT columns of the
// referenced table (when colSet is non-nil), become single-quoted strings.
func quoteFixSQLWithColumns(sqlStr string, colSet map[string]bool) string {
	var b strings.Builder
	b.Grow(len(sqlStr))
	for i := 0; i < len(sqlStr); i++ {
		ch := sqlStr[i]
		if ch != '"' {
			b.WriteByte(ch)
			continue
		}
		// Find the closing double quote, treating "" as an escaped quote
		// (SQLite's double-quoted string escaping).
		end := i + 1
		for end < len(sqlStr) {
			if sqlStr[end] == '"' {
				if end+1 < len(sqlStr) && sqlStr[end+1] == '"' {
					end += 2
					continue
				}
				break
			}
			end++
		}
		if end >= len(sqlStr) {
			b.WriteString(sqlStr[i:])
			break
		}
		content := sqlStr[i+1 : end]
		content = strings.ReplaceAll(content, "\"\"", "\"")
		// Keep as identifier when it is a valid identifier AND (no column
		// set to consult, or it names one of the table's columns).
		if isSQLIdentifier(content) && (colSet == nil || colSet[strings.ToUpper(content)]) {
			b.WriteString(sqlStr[i : end+1])
		} else {
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(content, "'", "''"))
			b.WriteByte('\'')
			// Adjacent tokens (e.g. 'string''alias' from "string"'alias')
			// need a space separator (SQLite emits 'string' 'alias').
			if end+1 < len(sqlStr) && (sqlStr[end+1] == '\'' || sqlStr[end+1] == '"') {
				b.WriteByte(' ')
			}
		}
		i = end
	}
	return b.String()
}

// fnSQLiteRenameQuoteFix implements SQLite's internal sqlite_rename_quotefix
// function used by ALTER TABLE RENAME machinery: it rewrites double-quoted
// tokens that are NOT valid identifiers (e.g. "notacolumn!", "a;b") into
// single-quoted string literals, leaving genuine double-quoted identifiers
// ("a", "b") untouched. Single quotes inside converted strings are doubled.
// The first argument (schema name) is ignored — the function only rewrites
// the SQL text.
func fnSQLiteRenameQuoteFix(args []interface{}) (interface{}, error) {
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
func (e *Engine) checkViewRenameDependencies(tableName string, oldColName, newColName string) *Result {
	views, err := e.schema.GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		upperSQL := strings.ToUpper(view.SQL)
		fromIdx := strings.Index(upperSQL, " FROM ")
		if fromIdx < 0 {
			continue
		}
		// Parse the FROM clause table list (may be multi-table: "FROM t1, t2").
		fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
		fromTables := splitFromTables(fromRest)
		if len(fromTables) == 0 {
			continue
		}
		// The view only matters if the renamed table is one of its FROM tables.
		renamedInFrom := false
		for _, ft := range fromTables {
			if strings.EqualFold(ft, tableName) {
				renamedInFrom = true
				break
			}
		}
		if !renamedInFrom {
			continue
		}
		// Build the union of columns across all FROM tables (for validating
		// unqualified references) and a per-table map (for ambiguity checks).
		validCols := make(map[string]bool)
		tableCols := make(map[string]map[string]bool)
		for _, ft := range fromTables {
			entry, findErr := e.schema.FindTable(ft)
			if findErr != nil {
				// If the FROM table itself doesn't exist, the view is broken;
				// SQLite reports this on rename.
				if v, vErr := e.schema.FindView(ft); vErr != nil {
					return &Result{Error: fmt.Errorf("error in view %s: %s", view.Name, findErr.Error())}
				} else if v != nil {
					continue
				}
				continue
			}
			// A view that references a virtual table whose module is not
			// registered cannot be re-validated; SQLite reports
			// "no such module: %s" on rename.
			if e.isVirtualTable(entry) {
				if mod := vtabModuleName(entry.SQL); mod != "" {
					if _, ok := e.vtabs.Find(mod); !ok {
						return &Result{Error: fmt.Errorf("error in view %s: no such module: %s", view.Name, mod)}
					}
				}
			}
			colDefs := e.colCache[ft]
			if colDefs == nil {
				colDefs = e.parseColumnDefs(entry.Name, entry.SQL)
			}
			cols := make(map[string]bool)
			for _, cd := range colDefs {
				cols[strings.ToUpper(cd.Name)] = true
				validCols[strings.ToUpper(cd.Name)] = true
			}
			tableCols[strings.ToUpper(ft)] = cols
		}
		selIdx := strings.Index(strings.ToUpper(view.SQL), "SELECT ")
		if selIdx < 0 {
			continue
		}
		afterSelect := view.SQL[selIdx+7 : fromIdx]
		// Extract bare identifier tokens (not whole comma/space chunks) so
		// expressions like "a+10" yield the identifier "a" rather than the
		// literal chunk "a+10". SQLite reports the first unresolvable
		// identifier in the view's SELECT list.
		viewCols := extractIdentifierTokens(afterSelect)
		for i, col := range viewCols {
			col = strings.TrimSpace(col)
			if col == "" {
				continue
			}
			// An identifier following AS is an output alias, not a column
			// reference (e.g. "SELECT a AS d FROM t").
			if i > 0 && strings.EqualFold(strings.TrimSpace(viewCols[i-1]), "AS") {
				continue
			}
			// Skip function calls and expressions (e.g. group_concat(a ORDER BY
			// b)); only bare column references need validation.
			if strings.Contains(col, "(") || strings.Contains(col, ")") {
				continue
			}
			upperCol := strings.ToUpper(col)
			if upperCol == "DISTINCT" || upperCol == "ALL" || upperCol == "AS" ||
				upperCol == "ORDER" || upperCol == "BY" || upperCol == "COLLATE" {
				continue
			}
			// Skip numeric literals extracted from expressions (e.g. 10 in a+10).
			if _, err := strconv.ParseFloat(upperCol, 64); err == nil {
				continue
			}
			if strings.Contains(upperCol, ".") {
				parts := strings.Split(upperCol, ".")
				if len(parts) == 2 {
					if parts[1] == "" {
						// Trailing dot (e1. from e1.*): the table name is parts[0].
						upperCol = parts[0]
					} else {
						upperCol = parts[1]
					}
				}
			}
			// A token like "e1." (from e1.*) is a table-qualified wildcard;
			// strip the trailing dot so the table name can be recognized.
			if strings.HasSuffix(upperCol, ".") {
				upperCol = strings.TrimSuffix(upperCol, ".")
			}
			// Skip tokens that are FROM table names (e.g. e1 in e1.* — a
			// table-qualified wildcard reference, not a column).
			isTableRef := false
			for _, ft := range fromTables {
				if strings.EqualFold(ft, upperCol) {
					isTableRef = true
					break
				}
			}
			if isTableRef {
				continue
			}
			if !validCols[strings.ToUpper(upperCol)] && upperCol != "*" {
				return &Result{Error: fmt.Errorf("error in view %s: no such column: %s",
					view.Name, col)}
			}
			// Post-rename ambiguity: if this reference is to the renamed column
			// and the new name also exists in another FROM table, the rename
			// makes the reference ambiguous. The renamed table itself will have
			// the new name after the rename, so it counts toward ambiguity.
			if oldColName != "" && strings.EqualFold(upperCol, oldColName) &&
				newColName != "" && newColName != oldColName {
				count := 1 // the renamed table will have the new column after rename
				for _, ft := range fromTables {
					if strings.EqualFold(ft, tableName) {
						continue
					}
					if cols, ok := tableCols[strings.ToUpper(ft)]; ok && cols[strings.ToUpper(newColName)] {
						count++
					}
				}
				if count > 1 {
					return &Result{Error: fmt.Errorf("error in view %s after rename: ambiguous column name: %s",
						view.Name, newColName)}
				}
			}
		}
	}
	return nil
}

// splitFromTables splits a FROM clause into its table names, handling commas
// ("FROM t1, t2") and stopping at the first keyword that ends the list
// (WHERE, JOIN, GROUP, ORDER, LIMIT, etc.).
func splitFromTables(fromRest string) []string {
	// Stop at common clause keywords.
	stopIdx := len(fromRest)
	for _, kw := range []string{" WHERE ", " JOIN ", " GROUP ", " ORDER ", " LIMIT ", " HAVING ", " ON "} {
		if idx := strings.Index(fromRest, kw); idx >= 0 && idx < stopIdx {
			stopIdx = idx
		}
	}
	fromRest = fromRest[:stopIdx]
	parts := strings.Split(fromRest, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Strip trailing AS alias and schema prefixes.
		fields := strings.Fields(p)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
			name = name[dotIdx+1:]
		}
		out = append(out, name)
	}
	return out
}

// checkViewDropDependencies checks if dropping the column would break
// views that reference the target table.
func (e *Engine) checkViewDropDependencies(tableName, columnName string) *Result {
	views, err := e.schema.GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		upperSQL := strings.ToUpper(view.SQL)
		fromIdx := strings.Index(upperSQL, " FROM ")
		if fromIdx < 0 {
			continue
		}
		fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
		spaceIdx := strings.IndexAny(fromRest, " \n\t\r")
		refTable := ""
		if spaceIdx > 0 {
			refTable = fromRest[:spaceIdx]
		} else {
			refTable = fromRest
		}
		if refTable == "" || !strings.EqualFold(refTable, tableName) {
			continue
		}
		// Extract column names from the SELECT part of the view
		selIdx := strings.Index(strings.ToUpper(view.SQL), "SELECT ")
		if selIdx < 0 {
			continue
		}
		afterSelect := view.SQL[selIdx+7 : fromIdx]
		viewCols := strings.FieldsFunc(afterSelect, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
		})
		for _, col := range viewCols {
			col = strings.TrimSpace(col)
			if strings.EqualFold(col, columnName) {
				return &Result{Error: fmt.Errorf("error in view %s after drop column: no such column: %s",
					view.Name, columnName)}
			}
		}
	}
	return nil
}

// validateViewSQL checks if a view's SQL references a valid table and columns.
// Returns an error message if the view has issues, empty string otherwise.
//
//lint:ignore U1000  Planned for P1 ALTER TABLE
func (e *Engine) validateViewSQL(viewSQL, tableName, columnName string) string {
	stmts, perr := parse.ParseSQL(viewSQL)
	if perr != nil || len(stmts) == 0 {
		return ""
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || sel == nil {
		return ""
	}
	// Find referenced table in FROM clause
	refTable := sel.From.Name
	// Check if the referenced table exists and has the expected columns
	if refTable != "" {
		entry, err := e.schema.FindTable(refTable)
		if err != nil {
			// Table doesn't exist
			return fmt.Sprintf("no such table: %s", refTable)
		}
		// Parse the view's column references
		colRefs := collectColumnRefs(sel)
		colDefs := e.colCache[refTable]
		if colDefs == nil {
			colDefs = e.parseColumnDefs(entry.Name, entry.SQL)
		}
		// Build set of valid column names (including dropped for position check)
		validCols := make(map[string]bool)
		for _, cd := range colDefs {
			validCols[strings.ToUpper(cd.Name)] = true
		}
		// Check each column reference in the view
		for _, ref := range colRefs {
			if !validCols[strings.ToUpper(ref)] {
				return fmt.Sprintf("no such column: %s", ref)
			}
		}
	}
	return ""
}

// collectColumnRefs collects column references from a SELECT statement.
//
//lint:ignore U1000  Utility for future use
func collectColumnRefs(sel *sql.SelectStmt) []string {
	var refs []string
	for _, col := range sel.Columns {
		collectExprRefs(col.Expr, &refs)
	}
	if sel.Where != nil {
		collectExprRefs(sel.Where, &refs)
	}
	return refs
}

// collectExprRefs collects column references from an expression.
//
//lint:ignore U1000  Utility for future use
func collectExprRefs(expr sql.Expr, refs *[]string) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *sql.ParenExpr:
		collectExprRefs(e.Expr, refs)
	case *sql.ColumnRef:
		*refs = append(*refs, e.Name)
	case *sql.BinaryOp:
		collectExprRefs(e.Left, refs)
		collectExprRefs(e.Right, refs)
	case *sql.UnaryOp:
		collectExprRefs(e.Operand, refs)
	case *sql.FuncCall:
		for _, arg := range e.Args {
			collectExprRefs(arg, refs)
		}
	case *sql.CaseExpr:
		collectExprRefs(e.Operand, refs)
		for _, w := range e.Whens {
			collectExprRefs(w.When, refs)
			collectExprRefs(w.Then, refs)
		}
		if e.Else != nil {
			collectExprRefs(e.Else, refs)
		}
	case *sql.RowValue:
		for _, v := range e.Values {
			collectExprRefs(v, refs)
		}
	case *sql.CastExpr:
		collectExprRefs(e.Operand, refs)
	case *sql.InList:
		collectExprRefs(e.Operand, refs)
	case *sql.IsNull:
		collectExprRefs(e.Operand, refs)
	case *sql.IsNotNull:
		collectExprRefs(e.Operand, refs)
	case *sql.IsTrue:
		collectExprRefs(e.Operand, refs)
	case *sql.IsFalse:
		collectExprRefs(e.Operand, refs)
	case *sql.IsDistinctFrom:
		collectExprRefs(e.Left, refs)
		collectExprRefs(e.Right, refs)
	case *sql.IsNotDistinctFrom:
		collectExprRefs(e.Left, refs)
		collectExprRefs(e.Right, refs)
	case *sql.Between:
		collectExprRefs(e.Operand, refs)
		collectExprRefs(e.Low, refs)
		collectExprRefs(e.High, refs)
	}
}

// checkTriggerDependencies checks if any triggers on the table reference the dropped column.
func (e *Engine) checkTriggerDependencies(tableName, columnName string) *Result {
	triggers, err := e.schema.FindTriggersForTable(tableName)
	if err != nil {
		return nil
	}
	for _, trig := range triggers {
		// Check if the trigger body references the dropped column
		upperSQL := strings.ToUpper(trig.SQL)
		upperCol := strings.ToUpper(columnName)
		// Check for NEW.column and OLD.column references
		if strings.Contains(upperSQL, "NEW."+upperCol) || strings.Contains(upperSQL, "OLD."+upperCol) {
			// The trigger references the column being dropped
			// Extract the trigger's SQL to find other issues
			errMsg := e.validateTriggerSQL(trig.SQL)
			if errMsg != "" {
				return &Result{Error: fmt.Errorf("error in trigger %s: %s", trig.Name, errMsg)}
			}
		}
	}
	return nil
}

// validateTriggerSQL checks if a trigger's SQL is valid.
// Extracts NEW/OLD column references and checks if they exist in the target table.
func (e *Engine) validateTriggerSQL(triggerSQL string) string {
	// Find the table name from the trigger SQL
	upperSQL := strings.ToUpper(triggerSQL)
	// Extract ON <table> to find the target table
	onIdx := strings.Index(upperSQL, " ON ")
	if onIdx < 0 {
		return ""
	}
	afterOn := strings.TrimSpace(upperSQL[onIdx+4:])
	spaceIdx := strings.IndexAny(afterOn, " \n\t\r")
	refTable := ""
	if spaceIdx > 0 {
		refTable = afterOn[:spaceIdx]
	} else {
		return ""
	}
	// Get the table's column definitions
	entry, err := e.schema.FindTable(refTable)
	if err != nil {
		return ""
	}
	colDefs := e.colCache[refTable]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(entry.Name, entry.SQL)
	}
	// Build set of valid column names (excluding dropped columns)
	validCols := make(map[string]bool)
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		validCols[strings.ToUpper(cd.Name)] = true
	}
	// Find all NEW.xxx and OLD.xxx references in the trigger body
	// Using simple text scanning
	body := triggerSQL
	BEGIN_MARKER := "BEGIN"
	begIdx := strings.Index(upperSQL, BEGIN_MARKER)
	if begIdx < 0 {
		return ""
	}
	body = triggerSQL[begIdx+len(BEGIN_MARKER):]
	// Find END
	endIdx := strings.LastIndex(strings.ToUpper(body), "END")
	if endIdx >= 0 {
		body = body[:endIdx]
	}
	// Scan for NEW. and OLD. references
	upperBody := strings.ToUpper(body)
	for i := 0; i < len(upperBody); i++ {
		prefix := ""
		nextIdx := -1
		newIdx := strings.Index(upperBody[i:], "NEW.")
		oldIdx := strings.Index(upperBody[i:], "OLD.")
		if newIdx >= 0 && (oldIdx < 0 || newIdx < oldIdx) {
			nextIdx = i + newIdx
			prefix = "new."
		} else if oldIdx >= 0 {
			nextIdx = i + oldIdx
			prefix = "old."
		} else {
			break
		}
		// Extract the column name after NEW. or OLD.
		colStart := nextIdx + 4 // skip "NEW." or "OLD."
		colEnd := colStart
		for colEnd < len(body) && (isAlpha(body[colEnd]) || body[colEnd] == '_') {
			colEnd++
		}
		if colEnd > colStart {
			colName := body[colStart:colEnd]
			if !validCols[strings.ToUpper(colName)] {
				return fmt.Sprintf("no such column: %s%s", prefix, colName)
			}
		}
		i = nextIdx + 1
	}
	return ""
}

// isAlpha checks if a byte is an ASCII letter.
func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// reference the given column and returns an error if so.
func (e *Engine) checkTableConstraintDependencies(createSQL, tableName, columnName string) *Result {
	stmts, perr := parse.ParseSQL(createSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if !ok || ct == nil {
		return nil
	}
	// Check table-level constraints (not column-level)
	for _, tc := range ct.Constraints {
		if tc.Type == sql.ConstraintCheck && tc.Expr != nil {
			if exprReferencesColumn(tc.Expr, columnName) {
				return &Result{Error: fmt.Errorf("error in table %s after drop column: no such column: %s",
					tableName, columnName)}
			}
		}
	}
	return nil
}

// exprReferencesColumn checks if an expression references a specific column.
func exprReferencesColumn(expr sql.Expr, columnName string) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *sql.ColumnRef:
		return strings.EqualFold(e.Name, columnName)
	case *sql.BinaryOp:
		return exprReferencesColumn(e.Left, columnName) || exprReferencesColumn(e.Right, columnName)
	case *sql.UnaryOp:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.NumericLit, *sql.StringLit, *sql.NullLit:
		return false
	case *sql.FuncCall:
		for _, arg := range e.Args {
			if exprReferencesColumn(arg, columnName) {
				return true
			}
		}
		return false
	case *sql.IsNull:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.IsNotNull:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.IsDistinctFrom:
		return exprReferencesColumn(e.Left, columnName) || exprReferencesColumn(e.Right, columnName)
	case *sql.IsNotDistinctFrom:
		return exprReferencesColumn(e.Left, columnName) || exprReferencesColumn(e.Right, columnName)
	case *sql.Between:
		return exprReferencesColumn(e.Operand, columnName) ||
			exprReferencesColumn(e.Low, columnName) || exprReferencesColumn(e.High, columnName)
	case *sql.InList:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.CaseExpr:
		if exprReferencesColumn(e.Operand, columnName) {
			return true
		}
		for _, when := range e.Whens {
			if exprReferencesColumn(when.When, columnName) || exprReferencesColumn(when.Then, columnName) {
				return true
			}
		}
		if e.Else != nil && exprReferencesColumn(e.Else, columnName) {
			return true
		}
		return false
	case *sql.CastExpr:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.RowValue:
		for _, v := range e.Values {
			if exprReferencesColumn(v, columnName) {
				return true
			}
		}
		return false
	case *sql.ExistsExpr, *sql.Subquery:
		return false // subqueries are complex, skip for now
	default:
		return false
	}
}

// indexReferencesColumn checks if the CREATE INDEX SQL references a given
// column. The second return value reports whether the reference was written
// as a double-quoted identifier ("name") — which determines the DQS-off
// error message wording when the column is dropped.
func indexReferencesColumn(sqlStr, columnName string) (bool, bool) {
	upperSQL := strings.ToUpper(sqlStr)
	// Check for simple column reference (word boundary)
	// The column name appears after the ON table_name ( or after ON clause
	// We use a simple approach: check if the column name appears as a standalone word
	// by looking for it with surrounding non-alphanumeric characters
	onIdx := strings.Index(upperSQL, " ON ")
	if onIdx < 0 {
		return false, false
	}
	parenIdx := strings.Index(upperSQL[onIdx:], "(")
	if parenIdx < 0 {
		return false, false
	}
	exprText := upperSQL[onIdx+parenIdx+1:]
	// Find the matching closing paren
	depth := 0
	endIdx := -1
	for i, ch := range exprText {
		switch ch {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				endIdx = i
				break
			}
			depth--
		}
	}
	if endIdx > 0 {
		exprText = exprText[:endIdx]
	}
	// Remove the last closing paren if any
	exprText = strings.TrimSuffix(exprText, ")")

	// Check if the column name appears as a whole word in the expression
	words := strings.FieldsFunc(exprText, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '"')
	})
	found := false
	quoted := false
	for _, raw := range words {
		// A double-quoted reference "name" keeps its quotes in the word scan
		// (the FieldsFunc set includes '"'), so record quotedness before
		// trimming.
		if len(raw) >= 2 && strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) {
			if strings.EqualFold(raw[1:len(raw)-1], columnName) {
				found = true
				quoted = true
			}
		}
		w := strings.Trim(raw, `"`)
		if strings.EqualFold(w, columnName) {
			found = true
		}
	}
	return found, quoted
}
