// ALTER TABLE RENAME operations: table rename, column rename, and the
// per-schema rewrites (views, triggers, indexes, foreign keys, sequences).
package execddl

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/vtab"
)

// --- ALTER TABLE RENAME TO ---

func (e *DDLExecutor) execAlterTableRename(s *sql.AlterTableStmt) *Result {
	if s.NewName == "" {
		return &Result{Error: fmt.Errorf("ALTER TABLE RENAME TO requires a new name")}
	}
	oldName := s.Table
	newName := s.NewName
	if isProtectedSystemTable(oldName) {
		return &Result{Error: fmt.Errorf("table %s may not be altered", oldName)}
	}
	entry, entryCtx, err := e.ctx.FindTable(oldName)
	if err != nil {
		return &Result{Error: err}
	}
	if res := e.checkRenameTarget(oldName, newName, entry, entryCtx); res != nil {
		return res
	}
	// rtree family: refuse EARLY when a target shadow name is occupied, so
	// the failed ALTER leaves every name untouched ('SQL logic error',
	// rtree1-7.2.x mirrors sqlite3's midway abort).
	if res := e.ensureRtreeShadowTargets(entry, oldName, newName); res != nil {
		return res
	}
	if !e.ctx.WritableSchema() {
		if err := e.validateRename(oldName, newName); err != nil {
			return &Result{Error: err}
		}
		if !e.ctx.LegacyAlterTable() {
			if amb := e.checkRenameAmbiguity(oldName, newName); amb != nil && amb.Error != nil {
				return amb
			}
		}
	}
	if res := e.renameTableEntrySQL(entryCtx, entry, oldName, newName); res != nil {
		return res
	}
	if ftsMod := e.getFTSModuleForTable(oldName); ftsMod != nil {
		ftsMod.RenameTable(oldName, newName)
		e.renameFTSShadowTables(entryCtx, oldName, newName)
	}
	// Rename the three shadow tables of an rtree family vtab to follow their
	// owner (rtree.c rtreeRename); collision pre-check ran earlier.
	if vtabFamilyModuleOf(entry.SQL) {
		e.renameRTreeShadowTables(entryCtx, oldName, newName)
	}
	e.updateRenameCaches(oldName, newName)
	e.renameSQLiteSequence(oldName, newName)
	if !e.ctx.WritableSchema() {
		e.renameUpdateRelatedEntries(entry.Name, newName)
	}
	return &Result{}
}

// checkRenameTarget rejects renames to an unavailable virtual-table module or
// to a name that already exists as a table or index in the same schema.
func (e *DDLExecutor) checkRenameTarget(oldName, newName string, entry *schema.Entry, entryCtx *DatabaseContext) *Result {
	if e.isVirtualTable(entry) {
		if mod := vtabModuleName(entry.SQL); mod != "" {
			if m, ok := e.ctx.VTables().Find(mod); !ok {
				return &Result{Error: fmt.Errorf("no such module: %s", mod)}
			} else if _, isNoop := m.(*vtab.NoopModule); isNoop {
				return &Result{Error: fmt.Errorf("no such module: %s", mod)}
			}
		}
	}
	if _, err := entryCtx.Schema.FindTable(newName); err == nil {
		return &Result{Error: fmt.Errorf("there is already another table or index with this name: %s", newName)}
	}
	if newName != oldName {
		if _, err := entryCtx.Schema.FindIndex(newName); err == nil {
			return &Result{Error: fmt.Errorf("there is already another table or index with this name: %s", newName)}
		}
	}
	return nil
}

// renameTableEntrySQL rewrites the table's own CREATE SQL with the new name.
// Legacy mode keeps the SQL as-is; token-level rename is used otherwise, with
// a string-regex fallback for occurrences the token walker missed.
func (e *DDLExecutor) renameTableEntrySQL(entryCtx *DatabaseContext, entry *schema.Entry, oldName, newName string) *Result {
	if e.ctx.LegacyAlterTable() {
		if err := entryCtx.Schema.RenameTableEntryWithSQL(oldName, newName, entry.SQL, schema.TypeTable); err != nil {
			return &Result{Error: err}
		}
		return nil
	}
	ctx := &RenameContext{
		OldName: oldName,
		NewName: newName,
		// QuotedNew doubles embedded quotes so names like raisara "one"'
		// render as legal SQL identifiers ("%w" semantics).
		QuotedNew: sqlQuoteIdentifier(newName),
		IsTable:   true,
	}
	ranges, rErr := FindRenameTokens(entry.SQL, ctx)
	newSQL := ""
	if rErr == nil && len(ranges) > 0 {
		newSQL = ApplyRenames(entry.SQL, ranges, sqlQuoteIdentifier(newName))
		if newSQL != entry.SQL {
			newSQL = replaceTableNameInSQL(newSQL, oldName, newName)
		}
	}
	if newSQL == "" || newSQL == entry.SQL {
		newSQL = replaceTableNameInSQL(entry.SQL, oldName, newName)
	}
	if err := entryCtx.Schema.RenameTableEntryWithSQL(oldName, newName, newSQL, schema.TypeTable); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// updateRenameCaches re-keys column/FTS caches and drops stale entries for the
// old and new table names.
func (e *DDLExecutor) updateRenameCaches(oldName, newName string) {
	if cached, ok := e.ctx.ColCache()[oldName]; ok {
		e.ctx.ColCache()[newName] = cached
		delete(e.ctx.ColCache(), oldName)
	}
	if ftsTable, ok := e.ctx.FTSTables()[oldName]; ok {
		e.ctx.FTSTables()[newName] = ftsTable
		delete(e.ctx.FTSTables(), oldName)
	}
	e.ctx.DeleteTableCache(oldName)
	e.ctx.DeleteTableCache(newName)
	e.ctx.DeleteTcCacheTable(oldName)
	e.ctx.DeleteTcCacheTable(newName)
	e.ctx.DeleteTableRootPage(oldName)
	e.ctx.DeleteTableRootPage(newName)
}

// renameFTSShadowTables renames the backing-store shadow tables of an FTS
// virtual table after ALTER TABLE RENAME (SQLite's fts3RenameMethod calls
// fts3RenameShadowTable for %_content, %_segments, %_segdir, %_docsize and
// %_stat). Each shadow table is a real schema entry whose CREATE SQL keeps
// its column structure but takes the new name; the row keeps its position
// and root page.
func (e *DDLExecutor) renameFTSShadowTables(ctx *DatabaseContext, oldName, newName string) {
	for _, suffix := range []string{"_content", "_segments", "_segdir", "_docsize", "_stat"} {
		oldShadow := oldName + suffix
		newShadow := newName + suffix
		if ent, _, err := e.ctx.FindTable(oldShadow); err == nil && ent != nil {
			newSQL := strings.Replace(ent.SQL, oldShadow, `"`+newShadow+`"`, 1)
			_ = ctx.Schema.RenameTableEntryWithSQL(oldShadow, newShadow, newSQL, schema.TypeTable)
		}
	}
}

// vtabFamilyModuleOf reports whether entry's stored SQL creates an
// rtree-family virtual table ("rtree" / "rtree_i32").
func vtabFamilyModuleOf(sqlStr string) bool {
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return false
	}
	rest := strings.TrimSpace(sqlStr[idx+len(" USING "):])
	end := strings.IndexAny(rest, "( \t\n\r,")
	name := strings.ToLower(rest)
	if end > 0 {
		name = strings.ToLower(rest[:end])
	}
	return name == "rtree" || name == "rtree_i32"
}

// rtreeShadowSuffixes are the backing tables the rtree module manages.
var rtreeShadowSuffixes = []string{"_node", "_rowid", "_parent"}

// ensureRtreeShadowTargets pre-checks that every shadow target name is free
// (or belongs to this same family). sqlite3 aborts the rename midway with
// the generic SQLITE_ERROR text "SQL logic error" (rtree1-7.2.1/7.2.3).
func (e *DDLExecutor) ensureRtreeShadowTargets(entry *schema.Entry, oldName, newName string) *Result {
	if !vtabFamilyModuleOf(entry.SQL) {
		return nil
	}
	for _, suffix := range rtreeShadowSuffixes {
		target := newName + suffix
		if target == oldName+suffix {
			continue
		}
		if ent, _, err := e.ctx.FindTable(target); err == nil && ent != nil {
			return &Result{Error: fmt.Errorf("SQL logic error")}
		}
	}
	return nil
}

// renameRTreeShadowTables renames the three shadow tables of an rtree family
// vtab to follow their owner (mirror of renameFTSShadowTables). The stored
// CREATE keeps its quoting style: replace the quoted identifier form first,
// falling back to the bare name.
func (e *DDLExecutor) renameRTreeShadowTables(ctx *DatabaseContext, oldName, newName string) {
	for _, suffix := range rtreeShadowSuffixes {
		oldShadow := oldName + suffix
		newShadow := newName + suffix
		if ent, _, err := e.ctx.FindTable(oldShadow); err == nil && ent != nil {
			newSQL := strings.Replace(ent.SQL, sqlQuoteIdentifier(oldShadow), sqlQuoteIdentifier(newShadow), 1)
			if newSQL == ent.SQL {
				newSQL = strings.Replace(ent.SQL, oldShadow, sqlQuoteIdentifier(newShadow), 1)
			}
			_ = ctx.Schema.RenameTableEntryWithSQL(oldShadow, newShadow, newSQL, schema.TypeTable)
		}
	}
}

// renameSQLiteSequence updates the sqlite_sequence table after ALTER TABLE
// RENAME: rows with name == oldName become newName. Missing or synthetic
// sqlite_sequence tables are ignored.
func (e *DDLExecutor) renameSQLiteSequence(oldName, newName string) {
	entry, err := e.ctx.Schema().FindTable("sqlite_sequence")
	if err != nil || isSyntheticSequence(entry) {
		return
	}
	tree := e.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
	for _, rowID := range collectSequenceRenameRows(tree, oldName) {
		e.rewriteSequenceRow(entry, tree, rowID, newName)
	}
}

func isSyntheticSequence(entry *schema.Entry) bool {
	return entry.RootPage == 1 && strings.Contains(entry.SQL, "seq INTEGER")
}

// collectSequenceRenameRows returns the rowIDs of sqlite_sequence rows whose
// name column matches oldName.
func collectSequenceRenameRows(tree *btree.BTree, oldName string) []int64 {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
	}
	var toRename []int64
	for {
		cell, rec, ok := readSequenceRow(cursor)
		if !ok {
			break
		}
		if sequenceRowNameMatches(rec, oldName) {
			toRename = append(toRename, cell.RowID)
		}
		if !advanceSequenceCursor(cursor) {
			break
		}
	}
	return toRename
}

// readSequenceRow reads and decodes the current cell; ok is false at end/error.
func readSequenceRow(cursor *btree.Cursor) (*storage.Cell, *storage.Record, bool) {
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return nil, nil, false
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return nil, nil, false
	}
	return cell, rec, true
}

// advanceSequenceCursor advances the cursor, reporting whether a next cell
// exists without error.
func advanceSequenceCursor(cursor *btree.Cursor) bool {
	ok, err := cursor.Next()
	return err == nil && ok
}

func sequenceRowNameMatches(rec *storage.Record, oldName string) bool {
	if len(rec.Values) > 0 {
		if name, ok := rec.Values[0].(string); ok && name == oldName {
			return true
		}
	}
	return false
}

// rewriteSequenceRow replaces the name column of one sqlite_sequence row with
// newName, preserving the row's rowID.
func (e *DDLExecutor) rewriteSequenceRow(entry *schema.Entry, tree *btree.BTree, rowID int64, newName string) {
	cell, err := e.ctx.ReadCellByRowID(tree, rowID)
	if err != nil || cell == nil {
		return
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return
	}
	values := make([]interface{}, len(rec.Values))
	copy(values, rec.Values)
	if len(values) > 0 {
		values[0] = newName
	}
	newRecord, err := storage.EncodeRecord(values)
	if err != nil {
		return
	}
	if _, err := tree.DeleteCellsWhere(func(c *storage.Cell) bool {
		return c.RowID == rowID
	}); err != nil {
		return
	}
	e.ctx.InvalidateRowIDCache(e.ctx.TablePager(entry.Name), entry.RootPage)
	newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: rowID, Payload: newRecord}
	_ = tree.InsertCell(newCell)
	e.ctx.BumpRowIDCache(e.ctx.TablePager(entry.Name), entry.RootPage, rowID)
}

// --- ALTER TABLE RENAME COLUMN ---

func (e *DDLExecutor) execAlterTableRenameColumn(s *sql.AlterTableStmt) *Result {
	tableName := s.Table
	oldColName := s.Column
	newColName := s.NewName
	if res := e.checkColumnRenamePreconditions(tableName, oldColName, newColName); res != nil {
		return res
	}
	tableEntry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}
	if e.isVirtualTable(tableEntry) {
		return &Result{Error: fmt.Errorf("cannot rename columns of virtual table %q", tableName)}
	}
	colDefs := findColumnDefs(e, tableName, tableEntry)
	if err := renameColumnInDefs(tableName, oldColName, newColName, colDefs); err != nil {
		return &Result{Error: err}
	}
	e.ctx.ColCache()[tableName] = colDefs
	if res := updateRenamedColumnSchema(e, tableEntry, tableName, oldColName, newColName); res != nil {
		return res
	}
	e.renameColumnInTriggers(tableName, oldColName, newColName)
	if res := checkTriggerViewColRefs(e, tableName, oldColName, newColName); res != nil {
		return res
	}
	e.renameColumnInIndexes(tableName, oldColName, newColName)
	e.renameColumnInViews(tableName, oldColName, newColName)
	e.renameColumnInForeignKeys(tableName, oldColName, newColName)
	return &Result{}
}

// checkColumnRenamePreconditions validates names, protected tables, views,
// trigger references, and view/index dependencies before renaming a column.
func (e *DDLExecutor) checkColumnRenamePreconditions(tableName, oldColName, newColName string) *Result {
	if oldColName == "" || newColName == "" {
		return &Result{Error: fmt.Errorf("ALTER TABLE RENAME COLUMN requires old and new column names")}
	}
	if isProtectedSystemTable(tableName) {
		return &Result{Error: fmt.Errorf("table %s may not be altered", tableName)}
	}
	if _, _, vErr := e.ctx.FindView(tableName); vErr == nil {
		return &Result{Error: fmt.Errorf("cannot rename columns of view %q", tableName)}
	}
	if e.ctx.WritableSchema() {
		return nil
	}
	if err := e.validateRename(tableName, tableName); err != nil {
		return &Result{Error: err}
	}
	if depResult := e.checkViewRenameDependencies(tableName, oldColName, newColName); depResult != nil {
		return depResult
	}
	if depResult := e.checkIndexRenameDependencies(tableName); depResult != nil {
		return depResult
	}
	return nil
}

func findColumnDefs(e *DDLExecutor, tableName string, tableEntry *schema.Entry) []sql.ColumnDef {
	colDefs := e.ctx.ColCache()[tableName]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}
	return colDefs
}

// renameColumnInDefs renames the column in the parsed definitions, rejecting
// duplicates and missing columns.
func renameColumnInDefs(tableName, oldColName, newColName string, colDefs []sql.ColumnDef) error {
	for _, c := range colDefs {
		if !strings.EqualFold(c.Name, oldColName) && strings.EqualFold(c.Name, newColName) {
			return fmt.Errorf("error in table %s after rename: duplicate column name: %s", tableName, newColName)
		}
	}
	for i, c := range colDefs {
		if strings.EqualFold(c.Name, oldColName) {
			colDefs[i].Name = newColName
			return nil
		}
	}
	return fmt.Errorf("no such column: %q", oldColName)
}

// updateRenamedColumnSchema rewrites the CREATE TABLE SQL for the renamed
// column in the schema entry, preserving the row's position.
func updateRenamedColumnSchema(e *DDLExecutor, tableEntry *schema.Entry, tableName, oldColName, newColName string) *Result {
	newSQL := renameColumnInCreateTableSQL(tableEntry.SQL, oldColName, newColName)
	if newSQL == "" || newSQL == tableEntry.SQL {
		return nil
	}
	tableEntry.SQL = newSQL
	e.ctx.DeleteTableCache(tableName)
	e.ctx.DeleteTcCacheTable(tableName)
	if err := e.ctx.Schema().UpdateEntry(tableEntry.Name, newSQL); err != nil {
		return &Result{Error: fmt.Errorf("failed to update schema entry: %w", err)}
	}
	return nil
}

// checkTriggerViewColRefs rejects a column rename when a trigger references
// the old column name only through a view that depends on the renamed table.
func checkTriggerViewColRefs(e *DDLExecutor, tableName, oldColName, newColName string) *Result {
	entries, gErr := e.ctx.Schema().GetEntries("")
	if gErr != nil {
		return nil
	}
	viewNames := make(map[string]bool)
	for _, entry := range entries {
		if entry.Type == schema.TypeView && refTableInTrigger(entry.SQL, tableName) {
			viewNames[entry.Name] = true
		}
	}
	if len(viewNames) == 0 {
		return nil
	}
	return triggerViewColRefError(entries, viewNames, tableName, oldColName, newColName)
}

// triggerViewColRefError rejects a column rename when a trigger references the
// old column name only through a view that depends on the renamed table.
func triggerViewColRefError(entries []*schema.Entry, viewNames map[string]bool, tableName, oldColName, newColName string) *Result {
	for _, entry := range entries {
		if entry.Type != schema.TypeTrigger {
			continue
		}
		if strings.EqualFold(entry.TblName, tableName) || refTableInTrigger(entry.SQL, tableName) {
			continue
		}
		newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
		if newSQL == entry.SQL {
			continue
		}
		for vn := range viewNames {
			if refTableInTrigger(entry.SQL, vn) {
				return &Result{Error: fmt.Errorf("error in trigger %s after rename: no such column: %s", entry.Name, oldColName)}
			}
		}
	}
	return nil
}

// renameColumnInCreateTableSQL renames a column within CREATE TABLE SQL text,
// preserving the rest of each column definition (type, constraints, etc.).
func renameColumnInCreateTableSQL(sqlStr, oldName, newName string) string {
	if !strings.Contains(strings.ToUpper(sqlStr), "CREATE TABLE") {
		return ""
	}
	parenStart, parenEnd, ok := findColumnDefsParens(sqlStr)
	if !ok {
		return ""
	}
	defText := sqlStr[parenStart+1 : parenEnd]
	parts := splitTopLevelParts(defText)
	for i, part := range parts {
		renamed, ok := renameColumnDefPart(part, oldName, newName)
		if ok {
			parts[i] = renamed
			break
		}
	}
	var buf strings.Builder
	buf.WriteString(sqlStr[:parenStart+1])
	for i, part := range parts {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString(part)
	}
	buf.WriteString(sqlStr[parenEnd:])
	return replaceColumnNameInSQL(buf.String(), oldName, newName)
}

// findColumnDefsParens locates the outer parentheses wrapping the column
// definitions in a CREATE TABLE statement.
func findColumnDefsParens(sqlStr string) (int, int, bool) {
	parenStart := strings.Index(sqlStr, "(")
	if parenStart < 0 {
		return 0, 0, false
	}
	depth := 0
	for i := parenStart; i < len(sqlStr); i++ {
		switch sqlStr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return parenStart, i, true
			}
		}
	}
	return 0, 0, false
}

// splitTopLevelParts splits a column-definition body on top-level commas.
func splitTopLevelParts(defText string) []string {
	var parts []string
	depth := 0
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
	return parts
}

// renameColumnDefPart renames the leading column name in one definition part,
// preserving quoting style and leading whitespace.
func renameColumnDefPart(part, oldName, newName string) (string, bool) {
	trimmed := strings.TrimSpace(part)
	if trimmed == "" {
		return "", false
	}
	colName := extractColumnName(trimmed)
	if colName == "" || !strings.EqualFold(colName, oldName) {
		return "", false
	}
	leadWS := part[:len(part)-len(trimmed)]
	quoteNew := sqlNameNeedsQuoting(newName)
	wasQuoted := strings.HasPrefix(trimmed, `"`+colName+`"`)
	wasSingleQuoted := strings.HasPrefix(trimmed, `'`+colName+`'`)
	newToken := newName
	if quoteNew || wasQuoted || wasSingleQuoted {
		newToken = `"` + newName + `"`
	}
	if wasQuoted {
		return leadWS + strings.Replace(trimmed, `"`+colName+`"`, newToken, 1), true
	}
	if wasSingleQuoted {
		return leadWS + newToken + " " + strings.TrimSpace(trimmed[len(colName)+2:]), true
	}
	spaceIdx := strings.IndexAny(trimmed, " (\"")
	if spaceIdx > 0 {
		return leadWS + newToken + trimmed[spaceIdx:], true
	}
	return leadWS + newToken, true
}

// renameColumnInForeignKeys rewrites REFERENCES clauses in child tables that
// name the parent table and reference the renamed column.
func (e *DDLExecutor) renameColumnInForeignKeys(parentTable, oldColName, newColName string) {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !fkEntryNeedsColumnRename(entry, parentTable, oldColName) {
			continue
		}
		newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
		if newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			e.ctx.DeleteTcCacheTable(entry.Name)
			_ = e.ctx.Schema().UpdateEntry(entry.Name, newSQL)
		}
	}
}

func fkEntryNeedsColumnRename(entry *schema.Entry, parentTable, oldColName string) bool {
	if entry.Type != schema.TypeTable || strings.EqualFold(entry.TblName, parentTable) || entry.SQL == "" {
		return false
	}
	if !strings.Contains(entry.SQL, oldColName) &&
		!strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(oldColName)) {
		return false
	}
	if !strings.Contains(strings.ToUpper(entry.SQL), "REFERENCES") {
		return false
	}
	return refTableInTrigger(entry.SQL, parentTable)
}

// renameColumnInViews updates view SQL in every attached database for views
// that reference the renamed column.
func (e *DDLExecutor) renameColumnInViews(tableName, oldColName, newColName string) {
	for _, dbCtx := range e.ctx.Databases() {
		if dbCtx == nil {
			continue
		}
		entries, err := dbCtx.Schema.GetEntries("")
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !viewNeedsColumnRename(entry, tableName, oldColName) {
				continue
			}
			newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
			if newSQL != entry.SQL && newSQL != "" {
				entry.SQL = newSQL
				e.ctx.DeleteTcCacheTable(entry.Name)
				_ = dbCtx.Schema.UpdateEntry(entry.Name, newSQL)
			}
		}
	}
}

func viewNeedsColumnRename(entry *schema.Entry, tableName, oldColName string) bool {
	if entry.Type != schema.TypeView || !refTableInTrigger(entry.SQL, tableName) {
		return false
	}
	if !strings.Contains(entry.SQL, oldColName) &&
		!strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(oldColName)) {
		return false
	}
	return true
}

// renameColumnInEntries applies a column rename to schema entries of the given
// type, using token-level rename with a string-regex fallback.
func (e *DDLExecutor) renameColumnInEntries(entryType schema.SchemaType, tblName string, oldColName, newColName string, ctx *RenameContext) {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !columnRenameEntryMatches(entry, entryType, tblName, oldColName) {
			continue
		}
		if applyColumnRenameToEntry(entry, oldColName, newColName, ctx) {
			_ = e.ctx.Schema().UpdateEntry(entry.Name, entry.SQL)
		}
	}
}

func columnRenameEntryMatches(entry *schema.Entry, entryType schema.SchemaType, tblName, oldColName string) bool {
	if entry.Type != entryType {
		return false
	}
	if tblName != "" {
		if entryType == schema.TypeTrigger {
			if !strings.EqualFold(entry.TblName, tblName) && !refTableInTrigger(entry.SQL, tblName) {
				return false
			}
		} else if !strings.EqualFold(entry.TblName, tblName) {
			return false
		}
	}
	if entry.SQL == "" {
		return false
	}
	if !strings.Contains(entry.SQL, oldColName) &&
		!strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(oldColName)) {
		return false
	}
	return true
}

// applyColumnRenameToEntry rewrites one entry's SQL; token-level result is
// authoritative when the parse succeeds, string-regex otherwise.
func applyColumnRenameToEntry(entry *schema.Entry, oldColName, newColName string, ctx *RenameContext) bool {
	ranges, rErr := FindRenameTokens(entry.SQL, ctx)
	if rErr == nil && len(ranges) > 0 {
		newSQL := ApplyRenames(entry.SQL, ranges, newColName)
		if newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			return true
		}
	}
	newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
	if newSQL != entry.SQL && newSQL != "" {
		entry.SQL = newSQL
		return true
	}
	return false
}

// replaceColumnNameInSQL replaces occurrences of oldColName with newColName in
// a SQL string using word-boundary matching; quoting is preserved per
// occurrence and matches inside empty-IN operands, function names, and larger
// double-quoted identifiers are left untouched.
func replaceColumnNameInSQL(sqlStr, oldColName, newColName string) string {
	if sqlStr == "" || oldColName == "" || newColName == "" {
		return sqlStr
	}
	quotedOld := regexp.QuoteMeta(oldColName)
	re := regexp.MustCompile(`(?i)\b` + quotedOld + `\b`)
	idxs := re.FindAllStringIndex(sqlStr, -1)
	if len(idxs) == 0 {
		return sqlStr
	}
	quoteNew := sqlNameNeedsQuoting(newColName)
	emptyInSpans := emptyINBareOperandSpans(sqlStr)
	var b strings.Builder
	last := 0
	for _, idx := range idxs {
		start, end := idx[0], idx[1]
		if columnRenameMatchSkipped(sqlStr, start, end, emptyInSpans) {
			continue
		}
		token, start, end := columnRenameToken(sqlStr, start, end, newColName, quoteNew)
		b.WriteString(sqlStr[last:start])
		b.WriteString(token)
		last = end
	}
	b.WriteString(sqlStr[last:])
	return b.String()
}

// columnRenameToken renders the replacement for a column-name match at
// [start, end), adjusting the span when the match was double-quoted so the
// quotes are replaced together with the name.
func columnRenameToken(sqlStr string, start, end int, newColName string, quoteNew bool) (string, int, int) {
	wasQuoted := start > 0 && sqlStr[start-1] == '"' && end < len(sqlStr) && sqlStr[end] == '"'
	if wasQuoted {
		start--
		end++
	}
	token := newColName
	if wasQuoted || quoteNew {
		token = `"` + newColName + `"`
	}
	return token, start, end
}

// columnRenameMatchSkipped reports whether a column-name match must be left
// untouched (empty-IN operand, function-call name, or inside a larger
// double-quoted identifier).
func columnRenameMatchSkipped(sqlStr string, start, end int, emptyInSpans [][2]int) bool {
	for _, sp := range emptyInSpans {
		if start >= sp[0] && start < sp[1] {
			return true
		}
	}
	if funcNameAt(sqlStr, start, end) {
		return true
	}
	if insideDoubleQuoted(sqlStr, start) && !(start > 0 && sqlStr[start-1] == '"') {
		return true
	}
	return false
}

// checkRenameAmbiguity verifies that renaming a table does not make any view's
// qualified column references ambiguous (SQLite re-parses each view after a
// table rename; a new name colliding with an existing alias or table in a
// view's FROM clause makes its qualified references ambiguous).
func (e *DDLExecutor) checkRenameAmbiguity(oldName, newName string) *Result {
	if strings.EqualFold(oldName, newName) {
		return nil
	}
	for _, dbCtx := range e.ctx.Databases() {
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
			ambiguous, col := viewHasAmbiguousRename(view.SQL, oldName, newName)
			if ambiguous {
				return &Result{Error: fmt.Errorf("error in view %s after rename: ambiguous column name: %s.%s",
					view.Name, newName, col)}
			}
		}
	}
	return nil
}

// viewHasAmbiguousRename reports whether a view's FROM clause contains both the
// old table name and the new name (as a table or alias), making references to
// the renamed table ambiguous.
func viewHasAmbiguousRename(viewSQL, oldName, newName string) (bool, string) {
	upperSQL := strings.ToUpper(viewSQL)
	fromIdx := strings.Index(upperSQL, " FROM ")
	if fromIdx < 0 {
		return false, ""
	}
	fields := strings.Fields(strings.TrimSpace(upperSQL[fromIdx+6:]))
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
		if strings.EqualFold(f, "AS") && i+1 < len(fields) {
			alias := strings.TrimSuffix(fields[i+1], ",")
			if strings.EqualFold(alias, newName) {
				hasNewName = true
			}
		}
	}
	if hasNewName && hasOldName {
		return true, firstQualifiedRef(viewSQL, newName)
	}
	return false, ""
}

// renameUpdateRelatedEntriesInSchema applies a table rename to every schema
// entry in one database's schema manager.
