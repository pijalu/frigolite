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
	"github.com/pijalu/frigolite/internal/vtab"
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

	// Find the table entry first so the conflict checks below are scoped to
	// the schema that owns the table. SQLite checks the new name only within
	// the renamed table's schema: renaming aux.t4 to t5 succeeds even when a
	// t5 exists in main.
	entry, entryCtx, err := e.findTable(oldName)
	if err != nil {
		return &Result{Error: err}
	}

	// A virtual table whose module is a NoopModule stub (echo, rtree, ...)
	// cannot be renamed — SQLite reports "no such module: %s" when the module
	// is unavailable (altertab-2.2: after a reopen the echo module is gone).
	if e.isVirtualTable(entry) {
		if mod := vtabModuleName(entry.SQL); mod != "" {
			if m, ok := e.vtabs.Find(mod); !ok {
				return &Result{Error: fmt.Errorf("no such module: %s", mod)}
			} else if _, isNoop := m.(*vtab.NoopModule); isNoop {
				return &Result{Error: fmt.Errorf("no such module: %s", mod)}
			}
		}
	}

	// Reject renaming to a name that already exists as a table or index in
	// the same schema. SQLite: "there is already another table or index with
	// this name: %s"
	if _, err := entryCtx.Schema.FindTable(newName); err == nil {
		return &Result{Error: fmt.Errorf("there is already another table or index with this name: %s", newName)}
	}
	if newName != oldName {
		if _, err := entryCtx.Schema.FindIndex(newName); err == nil {
			return &Result{Error: fmt.Errorf("there is already another table or index with this name: %s", newName)}
		}
	}

	// Validate the rename for broken references (writable_schema bypasses
	// this validation). The view-ambiguity check is skipped in legacy mode
	// (SQLite's legacy rename does not re-validate view dependencies,
	// alterlegacy-5.3).
	if !e.writableSchema {
		if err := e.validateRename(oldName, newName); err != nil {
			return &Result{Error: err}
		}
		// A rename that would make a view's qualified references ambiguous
		// is rejected (altertab-5.3).
		if !e.legacyAlterTable {
			if amb := e.checkRenameAmbiguity(oldName, newName); amb != nil && amb.Error != nil {
				return amb
			}
		}
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

	// Re-key FTS virtual table instances after rename so SELECT from the new
	// name still finds the FTS content table.
	if ftsTable, ok := e.ftsTables[oldName]; ok {
		e.ftsTables[newName] = ftsTable
		delete(e.ftsTables, oldName)
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
	// is directly editable). Use the unqualified table name (entry.Name) so
	// the per-schema rewrite matches references in that schema (e.g.
	// "ALTER TABLE aux.p1 RENAME TO ppp" rewrites "REFERENCES p1" in aux's
	// child tables, not the literal "aux.p1").
	if !e.writableSchema {
		e.renameUpdateRelatedEntries(entry.Name, newName)
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
		e.invalidateRowIDCache(e.tablePager(entry.Name), entry.RootPage)
		newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: rowID, Payload: newRecord}
		_ = tree.InsertCell(newCell)
		e.bumpRowIDCache(e.tablePager(entry.Name), entry.RootPage, rowID)
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
	// Views may live in any schema (main or temp); a TEMP view whose body
	// references the renamed column must be rewritten too (altercol-16.2.3:
	// a temp view over t1 is updated when t1 renames its column).
	for _, dbCtx := range e.databases {
		if dbCtx == nil {
			continue
		}
		entries, err := dbCtx.Schema.GetEntries("")
		if err != nil {
			continue
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
				_ = dbCtx.Schema.UpdateEntry(entry.Name, newSQL)
			}
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
				// The token walker already renamed every column reference;
				// a string pass would over-rename (e.g. a function name that
				// shares the column's name, altertab3-13.2: "SELECT a()
				// FILTER (WHERE a>0)" keeps the function name a). Only the
				// token-level result is authoritative when the parse
				// succeeds.
				entry.SQL = newSQL
				_ = e.schema.UpdateEntry(entry.Name, newSQL)
				continue
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
	emptyInSpans := emptyINBareOperandSpans(sqlStr)
	inSpan := func(pos int) bool {
		for _, sp := range emptyInSpans {
			if pos >= sp[0] && pos < sp[1] {
				return true
			}
		}
		return false
	}
	var b strings.Builder
	last := 0
	for _, idx := range idxs {
		start, end := idx[0], idx[1]
		// Skip matches that are the operand of an empty IN list (SQLite
		// leaves "b IN ()" untouched, altertab3-3.2).
		if inSpan(start) {
			continue
		}
		// Skip matches that are a function-call name (identifier immediately
		// followed by "(", allowing whitespace) — the function name must not
		// be renamed, only column references are (altertab3-13.2:
		// "SELECT a() FILTER (WHERE a>0)" keeps the function name a while
		// renaming the column a in the FILTER).
		if funcNameAt(sqlStr, start, end) {
			continue
		}
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
	tableEntry, tableCtx, err := e.findTable(oldName)
	if err != nil {
		return err
	}
	// A table entry belongs to the TEMP schema when its owning database
	// context is the temp database (its stored SQL is "CREATE TABLE ..."
	// without the TEMP keyword, matching SQLite's sqlite_temp_schema).
	isTempTable := tableCtx != nil && strings.EqualFold(tableCtx.Name, "temp")

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
	// SQLite validates the schema objects in the schema that contains the renamed
	// table (plus temp when the table is non-temp): renaming a TEMP table only
	// re-validates TEMP objects, while a main/attached rename re-validates those
	// plus temp (altercol-17.3: a broken main-schema trigger does not block a
	// TEMP table's rename column; alter-18.1: a broken main trigger blocks a
	// main table's rename column).
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return nil
	}
	if isTempTable {
		// A TEMP table rename validates only TEMP schema objects.
		if tc := e.getDB("temp"); tc != nil {
			entries, err = tc.Schema.GetEntries("")
			if err != nil {
				return nil
			}
		}
	} else {
		// A non-temp rename validates main + temp (temp first? SQLite reports
		// the first broken object in sqlite_master rowid order across the
		// schemas it visits; append temp entries after main so main errors
		// surface first, matching the observed behavior).
		if tc := e.getDB("temp"); tc != nil {
			tempEntries, tErr := tc.Schema.GetEntries("")
			if tErr == nil {
				entries = append(entries, tempEntries...)
			}
		}
	}
	// Check all triggers for references to non-existent tables. SQLite
	// re-parses every schema object during ALTER TABLE RENAME (table or
	// column), so ALL triggers are validated — a broken trigger on an
	// unrelated table blocks the rename (altertab3-4.1.2, alter-18.1). The
	// schema filtering below (temp vs main) already restricts which schema's
	// objects are examined; within that set every trigger is checked.
	for _, entry := range entries {
		if entry.Type == schema.TypeTrigger {
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
			// A view whose FROM clause names a non-existent table blocks the
			// rename (SQLite re-parses every view: "error in view %s: no such
			// table: main.%s", altertab-9.1).
			if missing := e.viewMissingTable(entry); missing != "" {
				refName := missing
				if !strings.Contains(missing, ".") {
					refName = "main." + missing
				}
				return fmt.Errorf("error in view %s: no such table: %s", entry.Name, refName)
			}
		}
	}
	// Check triggers related to the renamed table for column references that
	// don't exist in their ON table. SQLite only re-validates a trigger's
	// column references when the trigger references the renamed table
	// (directly ON it, or its body names it), including through a view that
	// depends on the table (altertab2-8.2: a trigger on t3 doing
	// "SELECT a FROM v1" where v1 = SELECT * FROM t1 blocks renaming
	// t1.a). An unrelated broken trigger must not block an unrelated rename
	// (altertab2-8.5: that same trigger does not block ALTER TABLE t4
	// RENAME a TO c).
	viewNames := make(map[string]bool)
	for _, entry := range entries {
		if entry.Type == schema.TypeView && refTableInTrigger(entry.SQL, oldName) {
			viewNames[strings.ToUpper(entry.Name)] = true
		}
	}
	for _, entry := range entries {
		if entry.Type == schema.TypeTrigger {
			related := strings.EqualFold(entry.TblName, oldName) ||
				refTableInTrigger(entry.SQL, oldName)
			if !related {
				for vn := range viewNames {
					if refTableInTrigger(entry.SQL, vn) {
						related = true
						break
					}
				}
			}
			if !related {
				continue
			}
			if err := e.checkTriggerColRefs(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// viewMissingTable returns the name of the first table referenced by a view's
// FROM clause that does not exist (in any schema), or "" if all resolve.
func (e *Engine) viewMissingTable(view *schema.Entry) string {
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
		if _, _, err := e.findTable(ft); err == nil {
			continue
		}
		if _, _, err := e.findView(ft); err == nil {
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
func (e *Engine) triggerReferencesView(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	for _, ref := range findTableRefsInTrigger(entry.SQL) {
		if _, _, err := e.findView(ref); err == nil {
			return true
		}
	}
	return false
}

// isTempTable reports whether the given table entry lives in the TEMP schema
// (created with CREATE TEMP TABLE or CREATE TEMPORARY TABLE).
func (e *Engine) isTempTable(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	upper := strings.ToUpper(entry.SQL)
	return strings.Contains(upper, "CREATE TEMP TABLE") || strings.Contains(upper, "CREATE TEMPORARY TABLE")
}

// isTempTrigger reports whether a trigger lives in the TEMP schema. A trigger
// is temp if it was created with CREATE TEMP TRIGGER or if its ON table is a
// temp table (CREATE TEMP TABLE). The stored SQL strips the TEMP keyword, so
// the owning schema is detected by looking up the ON table in the temp
// database context.
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
		if tc := e.getDB("temp"); tc != nil {
			if te, err := tc.Schema.FindTable(entry.TblName); err == nil && te != nil {
				return true
			}
		}
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
		// Skip empty-name references (e.g. a double-quoted empty string ""
		// parsed as a DQS identifier with no name).
		if ref.Name == "" {
			continue
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
	for _, dbCtx := range e.databases {
		e.renameUpdateRelatedEntriesInSchema(dbCtx.Schema, oldName, newName, quotedNew, ctx)
	}
}

// checkRenameAmbiguity verifies that renaming a table does not make any
// view's qualified column references ambiguous. SQLite re-parses each view
// after a table rename; if the new table name collides with an existing alias
// or table in a view's FROM clause, the view's qualified references become
// ambiguous and the rename fails with
// "error in view %s after rename: ambiguous column name: %s"
// (altertab-5.3: renaming t2 to one when a view has "FROM t1 AS one, t2"
// makes one.a ambiguous).
func (e *Engine) checkRenameAmbiguity(oldName, newName string) *Result {
	if strings.EqualFold(oldName, newName) {
		return nil
	}
	for _, dbCtx := range e.databases {
		if dbCtx == nil {
			continue
		}
		views, err := dbCtx.Schema.GetEntries(schema.TypeView)
		if err != nil {
			continue
		}
		for _, view := range views {
			if !refTableInTrigger(view.SQL, oldName) {
				continue
			}
			// Collect the FROM-clause table names and aliases (after the
			// rename, the renamed table is known by newName).
			upperSQL := strings.ToUpper(view.SQL)
			fromIdx := strings.Index(upperSQL, " FROM ")
			if fromIdx < 0 {
				continue
			}
			fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
			// Determine if the renamed table (oldName) appears in the view's
			// FROM with an alias equal to newName, OR if another table is
			// aliased to newName already.
			fields := strings.Fields(fromRest)
			hasNewName := false
			hasOldName := false
			for i, f := range fields {
				ff := strings.TrimSuffix(f, ",")
				if strings.EqualFold(ff, newName) {
					hasNewName = true
				}
				if strings.EqualFold(ff, oldName) {
					hasOldName = true
				}
				// AS alias
				if strings.EqualFold(f, "AS") && i+1 < len(fields) {
					alias := strings.TrimSuffix(fields[i+1], ",")
					if strings.EqualFold(alias, newName) {
						hasNewName = true
					}
				}
			}
			if hasNewName && hasOldName {
				// The renamed table's own name counts toward ambiguity if the
				// view qualifies references with it. SQLite reports the first
				// ambiguous qualified reference.
				return &Result{Error: fmt.Errorf("error in view %s after rename: ambiguous column name: %s.%s",
					view.Name, newName, firstQualifiedRef(view.SQL, newName))}
			}
		}
	}
	return nil
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

// renameUpdateRelatedEntriesInSchema applies the table rename to every schema
// entry in one database's schema manager.
func (e *Engine) renameUpdateRelatedEntriesInSchema(schemaMgr *schema.Manager, oldName, newName, quotedNew string, ctx *RenameContext) {
	entries, err := schemaMgr.GetEntries("")
	if err != nil {
		return
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
				_ = schemaMgr.RemoveEntry(entry.Name)
				_ = schemaMgr.AddEntry(entry)
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
			// Also update the table's own CREATE TABLE SQL. SQLite skips FK
			// rewrites only when BOTH legacy_alter_table is on AND foreign_keys
			// is off (altertab2-2.2 vs 2.3: turning foreign_keys on re-enables
			// the rewrite).
			if e.legacyAlterTable && !e.foreignKeys {
				continue
			}
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
					_ = schemaMgr.RemoveEntry(entry.Name)
					_ = schemaMgr.AddEntry(entry)
					// Invalidate cached column/constraint metadata for this entry
					// so FK enforcement and column resolution re-parse the
					// updated SQL (altertab-8.2: a child's REFERENCES clause is
					// rewritten p1 → "ppp" and the FK check must see the new
					// parent name).
					delete(e.colCache, entry.Name)
					delete(e.tcCache, entry.Name)
					continue
				}
			}
		}

		// Fallback: use string-regex alone when token-level rename fails or finds nothing
		newSQL := replaceTableNameInSQL(entry.SQL, oldName, newName)
		if newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			_ = schemaMgr.RemoveEntry(entry.Name)
			_ = schemaMgr.AddEntry(entry)
			delete(e.colCache, entry.Name)
			delete(e.tcCache, entry.Name)
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
	// Fallback: replace unquoted occurrences with word boundaries, adding quotes.
	// SQLite's rename machinery leaves the operand of an empty IN list untouched
	// ("t2 IN ()" and "(SELECT ... FROM t2) IN ()" keep their names,
	// altertab3-10.2). Compute the byte spans of every empty-IN operand and
	// skip replacements inside them.
	emptyInSpans := emptyINOperandSpans(sql)
	re = regexp.MustCompile(`\b` + quotedOld + `\b`)
	idxs := re.FindAllStringIndex(sql, -1)
	if len(idxs) == 0 {
		return sql
	}
	inSpan := func(pos int) bool {
		for _, sp := range emptyInSpans {
			if pos >= sp[0] && pos < sp[1] {
				return true
			}
		}
		return false
	}
	// A match that is the tail of a schema-qualified name ("aux.t2",
	// "temp.t2") belongs to a different schema's table and must NOT be
	// rewritten when renaming the unqualified table (SQLite keeps "aux.t2"
	// when renaming "main.t2", altertab-9.5). A "main.t2" qualifier IS
	// rewritten. Check the character before the match: a '.' means the
	// match is schema-qualified.
	qualifiedOtherSchema := func(pos int) bool {
		if pos <= 0 {
			return false
		}
		p := pos - 1
		for p >= 0 && (sql[p] == ' ' || sql[p] == '\t') {
			p--
		}
		if p < 0 || sql[p] != '.' {
			return false
		}
		// Find the qualifier start.
		q := p - 1
		for q >= 0 && (isIdentByte(sql[q])) {
			q--
		}
		qual := strings.ToUpper(sql[q+1 : p])
		return qual != "MAIN"
	}
	var b strings.Builder
	last := 0
	changed := false
	for _, idx := range idxs {
		if inSpan(idx[0]) {
			continue // leave the empty-IN operand untouched
		}
		if qualifiedOtherSchema(idx[0]) {
			continue // leave aux./temp.-qualified references untouched
		}
		b.WriteString(sql[last:idx[0]])
		b.WriteString(quotedNew)
		last = idx[1]
		changed = true
	}
	if !changed {
		return sql
	}
	b.WriteString(sql[last:])
	return b.String()
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

// emptyINBareOperandSpans returns the byte spans of the bare identifier
// operands of empty IN lists ("b IN ()" → the span covering b). SQLite's
// rename machinery leaves a bare column operand of an empty IN untouched, but
// still renames references inside function-call operands
// (altertab3-8.2.2: LIKELIHOOD(c0, 1.0) IN () renames c0).
func emptyINBareOperandSpans(sql string) [][2]int {
	var spans [][2]int
	up := strings.ToUpper(sql)
	for i := 0; i < len(sql); i++ {
		if i+2 <= len(sql) && up[i] == 'I' && up[i+1] == 'N' {
			j := i + 2
			for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t' || sql[j] == '\n' || sql[j] == '\r') {
				j++
			}
			if j < len(sql) && sql[j] == '(' {
				k := j + 1
				for k < len(sql) && (sql[k] == ' ' || sql[k] == '\t' || sql[k] == '\n' || sql[k] == '\r') {
					k++
				}
				if k < len(sql) && sql[k] == ')' {
					// "IN ()" found at i. The operand is a bare identifier if the
					// character before it is not ')' (a parenthesized/function
					// operand).
					p := i - 1
					for p >= 0 && (sql[p] == ' ' || sql[p] == '\t' || sql[p] == '\n' || sql[p] == '\r') {
						p--
					}
					if p >= 0 && sql[p] == ')' {
						i = k
						continue
					}
					// Walk back to the identifier start.
					for p >= 0 && (isIdentByte(sql[p]) || sql[p] == '.' || sql[p] == '"') {
						p--
					}
					spans = append(spans, [2]int{p + 1, i})
					i = k
				}
			}
		}
	}
	return spans
}

// emptyINOperandSpans returns the byte spans [start, end) of every operand
// of an empty IN list ("x IN ()", "(expr) IN ()") in the SQL text. SQLite's
// rename machinery does not rewrite names inside these operands.
func emptyINOperandSpans(sql string) [][2]int {
	var spans [][2]int
	up := strings.ToUpper(sql)
	for i := 0; i < len(sql); i++ {
		// Find "IN" followed by "()" with optional whitespace.
		if i+2 <= len(sql) && up[i] == 'I' && up[i+1] == 'N' {
			j := i + 2
			for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t' || sql[j] == '\n' || sql[j] == '\r') {
				j++
			}
			if j < len(sql) && sql[j] == '(' {
				k := j + 1
				for k < len(sql) && (sql[k] == ' ' || sql[k] == '\t' || sql[k] == '\n' || sql[k] == '\r') {
					k++
				}
				if k < len(sql) && sql[k] == ')' {
					// Found "IN ()" at i. Walk backward to find the operand
					// start: the matching open paren for a parenthesized
					// operand, or the start of the bare identifier/expression.
					start := i
					depth := 0
					p := i - 1
					for ; p >= 0; p-- {
						switch sql[p] {
						case ')':
							depth++
						case '(':
							if depth > 0 {
								depth--
								if depth == 0 {
									// This open paren matches the operand's
									// closing paren — the operand starts here.
									start = p
									p = -2
								}
							} else {
								start = p
								p = -2
							}
						}
						if p < 0 {
							break
						}
					}
					if start == i {
						// No enclosing paren: the operand is the bare identifier
						// immediately before IN. Walk back to its start.
						p = i - 1
						for p >= 0 && (sql[p] == ' ' || sql[p] == '\t') {
							p--
						}
						for p >= 0 && (isIdentByte(sql[p]) || sql[p] == '.') {
							p--
						}
						start = p + 1
					}
					spans = append(spans, [2]int{start, i})
					i = k
				}
			}
		}
	}
	return spans
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

	// STRICT tables: a DEFAULT whose value is incompatible with the declared
	// column type is rejected when the table already has rows (SQLite:
	// "type mismatch on DEFAULT"). An empty table accepts the column; the
	// mismatch surfaces on the first INSERT.
	if isStrictTable(tableEntry.SQL) && newCol.Default != nil && e.tableHasRows(tableEntry) {
		defVal, derr := e.evalColumnExpr(newCol)
		if derr == nil && defVal != nil {
			if err := enforceStrictType(tableEntry.Name, newCol.Name, newCol.Type, defVal); err != nil {
				return &Result{Error: fmt.Errorf("type mismatch on DEFAULT")}
			}
		}
	}

	if newCol.Check == nil && !newCol.NotNull {
		return &Result{}
	}

	// Generated columns: the generated expression is evaluated for every
	// existing row and NOT NULL/CHECK are enforced per row, matching
	// SQLite (alter3-9.* tests). For each row the CHECK constraint is
	// evaluated before the NOT NULL constraint.
	if newCol.Generated != nil {
		return e.validateGeneratedAddColumn(tableEntry, colDefs, newCol)
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

	// SQLite re-parses the new column's CHECK expression during ADD COLUMN,
	// resolving function and column references regardless of row count. An
	// unknown function (e.g. sqlite_fail) is reported even when the table is
	// empty (alter-22.2). Evaluate the expression once with an empty row to
	// surface resolve errors.
	if _, verr := e.evalExpr(newCol.Check, nil); verr != nil {
		return &Result{Error: fmt.Errorf("error in table %s after add column: %v", tableEntry.Name, verr)}
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
		if verr != nil {
			// SQLite re-parses the CHECK expression during ADD COLUMN and
			// reports a resolve error (e.g. an unknown function) as
			// "error in table %s after add column: %s" (alter-22.2).
			return &Result{Error: fmt.Errorf("error in table %s after add column: %v", tableEntry.Name, verr)}
		}
		if checkVal != nil && !toBool(checkVal) {
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

// validateGeneratedAddColumn enforces NOT NULL and CHECK constraints for a
// generated column added via ALTER TABLE ADD COLUMN. The generated
// expression is evaluated for every existing row (SQLite evaluates the
// constraints per row, in row order).
func (e *Engine) validateGeneratedAddColumn(tableEntry *schema.Entry, colDefs []sql.ColumnDef, newCol sql.ColumnDef) *Result {
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
		genVal, gerr := e.evalExpr(newCol.Generated, row)
		if gerr != nil {
			ok, nerr := cursor.Next()
			if nerr != nil || !ok {
				break
			}
			continue
		}
		row[newCol.Name] = genVal
		// CHECK is evaluated before NOT NULL for each row, matching SQLite.
		if newCol.Check != nil {
			checkVal, verr := e.evalExpr(newCol.Check, row)
			if verr == nil && checkVal != nil && !toBool(checkVal) {
				checkText := e.checkConstraintText(tableEntry.SQL, newCol.Name, newCol.Check)
				return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", checkText)}
			}
		}
		if newCol.NotNull && genVal == nil {
			return &Result{Error: fmt.Errorf("NOT NULL constraint failed: %s", newCol.Name)}
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
func (e *Engine) validateAddConstraint(tableName string, tableEntry *schema.Entry, tc *sql.TableConstraint) *Result {
	if tc == nil || tc.Type != sql.ConstraintCheck || tc.Expr == nil {
		return &Result{}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	tree := e.tableBTreeForName(tableName, tableEntry.RootPage, true)
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
		if verr != nil {
			// A resolve error (e.g. an unknown function) is reported as-is
			// (altercons-10.2: ADD CONSTRAINT CHECK(sqlite_drop_column(...))
			// fails with "no such function: sqlite_drop_column").
			return &Result{Error: verr}
		}
		if checkVal != nil && !toBool(checkVal) {
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
func (e *Engine) columnHasNull(tableName string, entry *schema.Entry, colDefs []sql.ColumnDef, colName string) bool {
	tree := e.tableBTreeForName(tableName, entry.RootPage, true)
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
	tableEntry, ctx, err := e.findTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// SQLite: virtual tables may not be altered (ALTER TABLE ADD COLUMN on a
	// virtual table reports "virtual tables may not be altered").
	if e.isVirtualTable(tableEntry) {
		return &Result{Error: fmt.Errorf("virtual tables may not be altered")}
	}

	// ALTER TABLE ... ADD [CONSTRAINT nm] CHECK(expr): append a table-level
	// constraint to the stored CREATE TABLE SQL and invalidate caches.
	if s.NewConstraint != nil {
		// Validate the constraint against existing rows before committing.
		if vres := e.validateAddConstraint(tableName, tableEntry, s.NewConstraint); vres.Error != nil {
			return vres
		}
		newSQL := addConstraintToCreateTableSQL(tableEntry.SQL, s.NewConstraint)
		if newSQL != "" && newSQL != tableEntry.SQL {
			tableEntry.SQL = newSQL
			delete(e.tableCache, tableName)
			delete(e.tcCache, tableName)
			_ = ctx.Schema.RemoveEntry(tableEntry.Name)
			if err := ctx.Schema.AddEntry(tableEntry); err != nil {
				return &Result{Error: fmt.Errorf("failed to re-add entry after DDL: %w", err)}
			}
			if _, err := ctx.Schema.FindTable(tableEntry.Name); err != nil {
				if retryErr := ctx.Schema.AddEntry(tableEntry); retryErr != nil {
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
			_ = ctx.Schema.RemoveEntry(tableEntry.Name)
			if err := ctx.Schema.AddEntry(tableEntry); err != nil {
				return &Result{Error: fmt.Errorf("failed to re-add entry after DDL: %w", err)}
			}
			// Verify the entry was re-added
			if _, err := ctx.Schema.FindTable(tableEntry.Name); err != nil {
				if retryErr := ctx.Schema.AddEntry(tableEntry); retryErr != nil {
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
		tableEntry, tableCtx, err := e.findTable(tableName)
		if err != nil {
			return &Result{Error: err}
		}
		schemaMgr := e.schema
		if tableCtx != nil && tableCtx.Schema != nil {
			schemaMgr = tableCtx.Schema
		}
		// Remove the named constraint from the CREATE TABLE SQL
		if !sqlHasConstraintName(tableEntry.SQL, constraintName) {
			return &Result{Error: fmt.Errorf("no such constraint: %s", constraintName)}
		}
		newSQL := removeConstraintFromSQL(tableEntry.SQL, constraintName)
		if newSQL != tableEntry.SQL {
			tableEntry.SQL = newSQL
			// Invalidate cached column/constraint info for this table.
			delete(e.colCache, tableEntry.Name)
			delete(e.tcCache, tableEntry.Name)
			delete(e.tableCache, tableName)
			_ = schemaMgr.RemoveEntry(tableEntry.Name)
			if err := schemaMgr.AddEntry(tableEntry); err != nil {
				return &Result{Error: fmt.Errorf("failed to re-add entry after DROP CONSTRAINT: %w", err)}
			}
			// Verify the entry was re-added
			if _, err := schemaMgr.FindTable(tableEntry.Name); err != nil {
				if retryErr := schemaMgr.AddEntry(tableEntry); retryErr != nil {
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

	// Rebuild the table's rows: SQLite's DROP COLUMN copies every row into a
	// fresh table without the dropped column, so the on-disk records no longer
	// contain a slot for it. Rewriting the records here (rather than relying on
	// the Dropped-flag position mapping) keeps reads correct even after the
	// colCache is invalidated by a later statement (e.g. PRAGMA page_count,
	// altertab3-31.2).
	droppedIsGenerated := false
	for _, c := range newColDefs {
		if c.Dropped && c.Generated != nil {
			droppedIsGenerated = true
		}
	}
	e.rebuildRowsAfterDrop(tableEntry, newColDefs, s.Column)

	// After a STORED column is rebuilt out of the records, the colCache must
	// hold the visible definitions (no Dropped flag — the reader would
	// otherwise skip the dropped slot that no longer exists). A VIRTUAL
	// generated column is never stored, so the reader keeps the Dropped flag
	// to skip the slot that was never in the record.
	if !droppedIsGenerated {
		e.colCache[tableName] = sqlColDefs
	}

	return &Result{}
}

// rebuildRowsAfterDrop rewrites every row of a table after DROP COLUMN,
// removing the dropped column's value from each record. The dropped column is
// identified by its name (it is the only Dropped-flagged definition in
// colDefs).
func (e *Engine) rebuildRowsAfterDrop(tableEntry *schema.Entry, colDefs []sql.ColumnDef, droppedName string) {
	// Find the dropped column's index in the OLD record layout.
	dropIdx := -1
	for i, cd := range colDefs {
		if cd.Name == droppedName {
			dropIdx = i
			break
		}
	}
	if dropIdx < 0 {
		return
	}
	// A VIRTUAL generated column is computed, not stored in the record, so
	// dropping it does not shift any on-disk values (alterdropcol-2.5:
	// "my table" has c AS (a+b) and dropping c leaves the (a, b, d) records
	// intact).
	if colDefs[dropIdx].Generated != nil {
		return
	}
	// The scan and rewrite must use the schema-qualified table name so the
	// correct pager is used for an ATTACHed table.
	tree := e.tableBTreeForName(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return
	}
	type rewrite struct {
		rowID  int64
		values []interface{}
	}
	var rewrites []rewrite
	var rowIDs map[int64]bool
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		// A record may have fewer values than the table's column count (short
		// rows written before ADD COLUMN); remove the dropped slot only when
		// present.
		if dropIdx < len(rec.Values) {
			values := make([]interface{}, 0, len(rec.Values)-1)
			values = append(values, rec.Values[:dropIdx]...)
			values = append(values, rec.Values[dropIdx+1:]...)
			rewrites = append(rewrites, rewrite{rowID: cell.RowID, values: values})
			if rowIDs == nil {
				rowIDs = make(map[int64]bool)
			}
			rowIDs[cell.RowID] = true
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	if len(rewrites) == 0 {
		return
	}
	// Delete all rewritten rows in a single pass, then re-insert (avoiding
	// the O(n²) per-row delete+insert that made DROP COLUMN on large tables
	// take minutes, alterdropcol-9.x: 50000 rows).
	if _, err := tree.DeleteCellsWhere(func(c *storage.Cell) bool {
		return rowIDs[c.RowID]
	}); err != nil {
		return
	}
	e.invalidateRowIDCache(e.tablePager(tableEntry.Name), tableEntry.RootPage)
	for _, rw := range rewrites {
		newRecord, err := storage.EncodeRecord(rw.values)
		if err != nil {
			continue
		}
		newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: rw.rowID, Payload: newRecord}
		_ = tree.InsertCell(newCell)
		e.bumpRowIDCache(e.tablePager(tableEntry.Name), tableEntry.RootPage, rw.rowID)
	}
}

func (e *Engine) execAlterTableAlter(s *sql.AlterTableStmt) *Result {
	// ALTER TABLE ... ALTER COLUMN SET NOT NULL / DROP NOT NULL
	if s.AlterColAction == "" {
		return &Result{}
	}
	tableName := s.Table
	// SQLite protects its internal tables ("table sqlite_schema may not be
	// altered") and rejects ALTER COLUMN on a view ("cannot edit constraints
	// of view").
	if isProtectedSystemTable(tableName) {
		return &Result{Error: fmt.Errorf("table %s may not be altered", tableName)}
	}
	if _, _, vErr := e.findView(tableName); vErr == nil {
		return &Result{Error: fmt.Errorf("cannot edit constraints of view %q", tableName)}
	}
	tableEntry, tableCtx, err := e.findTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// Use the cached column definitions (from the original CREATE) when
	// available; a writable_schema edit may corrupt the stored SQL but the
	// in-memory table definition stays valid. Only report malformed when the
	// SQL is fundamentally broken AND no cached definition exists.
	colDefs := e.colCache[tableEntry.Name]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}
	if len(colDefs) == 0 && isMalformedCreateTableSQL(tableEntry.SQL) {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}

	// Find and update the column
	found := false
	for i, c := range colDefs {
		if c.Name == s.Column {
			switch s.AlterColAction {
			case "SET NOT NULL":
				// SQLite: SET NOT NULL fails with "constraint failed" if any
				// existing row has NULL in this column.
				if e.columnHasNull(tableName, tableEntry, colDefs, c.Name) {
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
	e.colCache[tableEntry.Name] = colDefs

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
		schemaMgr := e.schema
		if tableCtx != nil && tableCtx.Schema != nil {
			schemaMgr = tableCtx.Schema
		}
		_ = schemaMgr.RemoveEntry(tableEntry.Name)
		if err := schemaMgr.AddEntry(tableEntry); err != nil {
			return &Result{Error: fmt.Errorf("failed to re-add entry after DDL: %w", err)}
		}
		// Verify the entry was re-added
		if _, err := schemaMgr.FindTable(tableEntry.Name); err != nil {
			if retryErr := schemaMgr.AddEntry(tableEntry); retryErr != nil {
				return &Result{Error: fmt.Errorf("schema consistency check failed: entry %s lost after DDL", tableEntry.Name)}
			}
		}
	}

	return &Result{}
}

// skipSQLWhitespaceAndComments advances i past whitespace and SQL comments
// (/* ... */ and -- ...) starting at or after position i in s.
func skipSQLWhitespaceAndComments(s string, i int) int {
	for i < len(s) {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' {
			i++
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			// Block comment: find the closing */.
			j := i + 2
			for j+1 < len(s) && !(s[j] == '*' && s[j+1] == '/') {
				j++
			}
			if j+1 < len(s) {
				i = j + 2
				continue
			}
			i = len(s)
			continue
		}
		if i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			// Line comment: skip to end of line.
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		break
	}
	return i
}

// removeLeadingConstraintClause removes the first CONSTRAINT <name> clause
// from a constraint-chain fragment and returns what follows (e.g. from
// "abc CONSTRAINT one CHECK(a!=b) CONSTRAINT three" it returns
// "CONSTRAINT one CHECK(a!=b) CONSTRAINT three").
func removeLeadingConstraintClause(rest, constraintName, quotedName, upperName, upperQuotedName string) string {
	tailUpper := strings.ToUpper(rest)
	nameEnd := 0
	if strings.HasPrefix(tailUpper, upperQuotedName) {
		nameEnd = len(quotedName)
	} else if strings.HasPrefix(tailUpper, upperName) {
		nameEnd = len(constraintName)
	}
	i := nameEnd
	// Skip whitespace and comments after the name (e.g.
	// "CONSTRAINT abc /* hello */ CHECK(...)").
	i = skipSQLWhitespaceAndComments(rest, i)
	// If the next token is CONSTRAINT, the removed clause had no type keyword
	// (bare "CONSTRAINT abc" in a chain) — the remainder starts here.
	if strings.HasPrefix(strings.ToUpper(rest[i:]), "CONSTRAINT") {
		return strings.TrimSpace(rest[i:])
	}
	// The next token is the constraint type keyword (CHECK, UNIQUE, ...).
	// Skip it.
	kwStart := i
	for i < len(rest) && rest[i] != ' ' && rest[i] != '(' {
		i++
	}
	kwUpper := strings.ToUpper(strings.TrimSpace(rest[kwStart:i]))
	// Skip whitespace and comments, then the parenthesized expression
	// (CHECK(...)).
	i = skipSQLWhitespaceAndComments(rest, i)
	if i < len(rest) && rest[i] == '(' {
		pdepth := 0
		for i < len(rest) {
			if rest[i] == '(' {
				pdepth++
			} else if rest[i] == ')' {
				pdepth--
				if pdepth == 0 {
					i++
					break
				}
			}
			i++
		}
	}
	// FOREIGN KEY (cols) REFERENCES <table>(cols): after the keyword pair and
	// column list, skip the REFERENCES target too so the whole clause is
	// removed (altercons3-4.2/4.3).
	if kwUpper == "FOREIGN" {
		// Skip the KEY token (part of FOREIGN KEY).
		for i < len(rest) && rest[i] != ' ' && rest[i] != '(' {
			i++
		}
		i = skipSQLWhitespaceAndComments(rest, i)
		// Skip the parenthesized column list.
		if i < len(rest) && rest[i] == '(' {
			pdepth := 0
			for i < len(rest) {
				if rest[i] == '(' {
					pdepth++
				} else if rest[i] == ')' {
					pdepth--
					if pdepth == 0 {
						i++
						break
					}
				}
				i++
			}
		}
		i = skipSQLWhitespaceAndComments(rest, i)
		if strings.HasPrefix(strings.ToUpper(rest[i:]), "REFERENCES") {
			// Skip the REFERENCES keyword.
			i += len("REFERENCES")
			i = skipSQLWhitespaceAndComments(rest, i)
			// Skip the target table name.
			for i < len(rest) && rest[i] != ' ' && rest[i] != '(' {
				i++
			}
			// Skip the optional parenthesized column list.
			if i < len(rest) && rest[i] == '(' {
				pdepth := 0
				for i < len(rest) {
					if rest[i] == '(' {
						pdepth++
					} else if rest[i] == ')' {
						pdepth--
						if pdepth == 0 {
							i++
							break
						}
					}
					i++
				}
			}
		}
	}
	// Everything after the removed clause is the remainder (a following
	// CONSTRAINT <name> ... chain or end of the part).
	return strings.TrimSpace(rest[i:])
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
		// The CONSTRAINT keyword may be preceded by a comment (e.g.
		// "/* world */ CONSTRAINT abc ..."); skip leading comments before
		// detecting the constraint clause.
		constraintStart := skipSQLWhitespaceAndComments(part, 0)
		constraintUpper := strings.ToUpper(part[constraintStart:])
		// Check if this part is a CONSTRAINT clause with the matching name
		if strings.HasPrefix(constraintUpper, "CONSTRAINT ") {
			// Extract the constraint name from the part
			rest := strings.TrimSpace(part[constraintStart+11:]) // after "CONSTRAINT "
			restUpper := strings.ToUpper(rest)
			if strings.HasPrefix(restUpper, upperName) || strings.HasPrefix(restUpper, upperQuotedName) {
				// This is the constraint to drop. Remove the matched CONSTRAINT
				// clause but keep any CONSTRAINT clauses that follow in the same
				// comma-separated part (e.g. "CONSTRAINT abc CONSTRAINT one ...").
				removed := removeLeadingConstraintClause(rest, constraintName, quotedName, upperName, upperQuotedName)
				if strings.TrimSpace(removed) != "" {
					keptParts = append(keptParts, removed)
				}
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
				// Column-level constraint match — remove the CONSTRAINT clause but
				// keep any clauses that follow it (NOT NULL, DEFAULT, COLLATE,
				// REFERENCES). Find where the constraint's CHECK expression ends:
				// scan from the constraint name to the start of the next clause
				// keyword (NOT NULL, DEFAULT, COLLATE, REFERENCES, UNIQUE, CHECK,
				// PRIMARY KEY, CONSTRAINT, GENERATED, AS, or end of part).
				tail := strings.TrimSpace(part[conIdx+11:]) // text after " CONSTRAINT "
				tailUpper := strings.ToUpper(tail)
				// Skip the constraint name.
				nameEnd := 0
				if strings.HasPrefix(tailUpper, upperQuotedName) {
					nameEnd = len(quotedName)
				} else if strings.HasPrefix(tailUpper, upperName) {
					nameEnd = len(constraintName)
				}
				// After the name: optional CHECK (...)/UNIQUE/etc. Skip the first
				// constraint keyword token and its parenthesized expression.
				clauseStart := nameEnd
				for clauseStart < len(tail) && (tail[clauseStart] == ' ' || tail[clauseStart] == '\t') {
					clauseStart++
				}
				// Skip the constraint type keyword (CHECK, UNIQUE, ...).
				kwStart := clauseStart
				for kwStart < len(tail) && tail[kwStart] != ' ' && tail[kwStart] != '(' {
					kwStart++
				}
				kwUpper := strings.ToUpper(strings.TrimSpace(tail[clauseStart:kwStart]))
				clauseStart = kwStart
				// For REFERENCES <table>[(cols)] the target table name follows
				// the keyword; skip it and its optional parenthesized column
				// list so "CONSTRAINT fk REFERENCES p1(a)" removes the whole
				// reference (altercons3-4.x).
				if kwUpper == "REFERENCES" {
					for clauseStart < len(tail) && (tail[clauseStart] == ' ' || tail[clauseStart] == '\t') {
						clauseStart++
					}
					for clauseStart < len(tail) && tail[clauseStart] != ' ' && tail[clauseStart] != '(' &&
						tail[clauseStart] != '\t' && tail[clauseStart] != '\n' && tail[clauseStart] != '\r' {
						clauseStart++
					}
				}
				// Skip any parenthesized expression.
				for clauseStart < len(tail) && (tail[clauseStart] == ' ' || tail[clauseStart] == '\t') {
					clauseStart++
				}
				if clauseStart < len(tail) && tail[clauseStart] == '(' {
					pdepth := 0
					for clauseStart < len(tail) {
						if tail[clauseStart] == '(' {
							pdepth++
						} else if tail[clauseStart] == ')' {
							pdepth--
							if pdepth == 0 {
								clauseStart++
								break
							}
						}
						clauseStart++
					}
				}
				// clauseStart now points at the next clause keyword or end.
				part = strings.TrimSpace(part[:conIdx])
				if rest2 := strings.TrimSpace(tail[clauseStart:]); rest2 != "" {
					part += " " + rest2
				}
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
	buf.WriteString(")")
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
	buf.WriteString(" ")
	if tc.Name != "" {
		buf.WriteString("CONSTRAINT ")
		buf.WriteString(tc.Name)
		buf.WriteString(" ")
	}
	switch tc.Type {
	case sql.ConstraintCheck:
		buf.WriteString("CHECK (")
		if tc.Expr != nil {
			buf.WriteString(sql.ExprString(tc.Expr))
		}
		buf.WriteString(")")
	default:
		if tc.Type != "" {
			buf.WriteString(string(tc.Type))
		}
	}
	buf.WriteString(")")
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
	buf.WriteString(")")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
}

// addColumnToCreateTableSQL adds a new column definition to a CREATE TABLE SQL string.
func addColumnToCreateTableSQL(origSQL string, colDef sql.ColumnDef) string {
	upper := strings.ToUpper(strings.TrimSpace(origSQL))
	if !strings.HasPrefix(upper, "CREATE TABLE") && !strings.HasPrefix(upper, "CREATE TEMP TABLE") && !strings.HasPrefix(upper, "CREATE TEMPORARY TABLE") {
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

	// Find the insertion point. SQLite stores an ADD COLUMN before the
	// table-level constraints (e.g. "CREATE TABLE t(a, b, d DEFAULT 'x',
	// PRIMARY KEY(a,b))"), so insert before the first top-level constraint
	// keyword (PRIMARY/UNIQUE/CHECK/FOREIGN/CONSTRAINT). With no constraints
	// the new column goes before the closing paren.
	insertAt := parenEnd
	depth2 := 0
forInsert:
	for i := parenStart + 1; i < parenEnd; i++ {
		switch origSQL[i] {
		case '(':
			depth2++
		case ')':
			depth2--
		case ',':
			if depth2 != 0 {
				continue
			}
			rest := strings.ToUpper(strings.TrimSpace(origSQL[i+1 : parenEnd]))
			if strings.HasPrefix(rest, "PRIMARY") || strings.HasPrefix(rest, "UNIQUE") ||
				strings.HasPrefix(rest, "CHECK") || strings.HasPrefix(rest, "FOREIGN") ||
				strings.HasPrefix(rest, "CONSTRAINT") {
				// Insert at the comma position so the new column lands
				// between the last column and the constraint.
				insertAt = i
				break forInsert
			}
		}
	}

	// Insert the new column definition before the closing paren (or before
	// the first table-level constraint).
	result := origSQL[:insertAt] + ", " + colText + origSQL[insertAt:]
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
			// Skip numeric literals in expression index keys (e.g. 1.0 in
			// a+1.0) — they are values, not column references.
			if _, err := strconv.ParseFloat(col, 64); err == nil {
				continue
			}
			// Skip SQL keywords that the tokenizer extracts from expressions
			// (e.g. IN in "WHERE a IN (...)").
			if isSQLKeywordOrPseudo(strings.ToUpper(col)) {
				continue
			}
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
			// "no such module: %s" on rename. The engine's NoopModule stubs
			// (echo, rtree, etc.) count as unavailable — they exist only so
			// CREATE VIRTUAL TABLE parses without crashing.
			if e.isVirtualTable(entry) {
				if mod := vtabModuleName(entry.SQL); mod != "" {
					m, ok := e.vtabs.Find(mod)
					if !ok {
						return &Result{Error: fmt.Errorf("error in view %s: no such module: %s", view.Name, mod)}
					}
					if _, isNoop := m.(*vtab.NoopModule); isNoop {
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
