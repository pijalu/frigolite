// ALTER TABLE RENAME operations: table rename, column rename, and the
// per-schema rewrites (views, triggers, indexes, foreign keys, sequences).
package execddl

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/rename"
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
		// The protection error reports the canonical (lowercase) system
		// table name, not the user's spelling: alter.c echoes pTab->zName,
		// so `ALTER TABLE SqLiTe_master RENAME TO master` says "table
		// sqlite_master may not be altered" (alter.test).
		return &Result{Error: fmt.Errorf("table %s may not be altered", strings.ToLower(oldName))}
	}
	// The NEW name is also checked against the sqlite_ prefix (alter.test:
	// `ALTER TABLE t3 RENAME TO sqlite_t3` → "object name reserved for
	// internal use: sqlite_t3").
	if res := e.validateReservedName(newName); res != nil {
		return res
	}
	entry, entryCtx, res := e.findAlterRenameTarget(oldName, newName)
	if res != nil {
		return res
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
	if res := e.validateRenameNames(oldName, newName); res != nil {
		return res
	}
	if res := e.renameTableEntrySQL(entryCtx, entry, oldName, newName); res != nil {
		return res
	}
	// alter.c order: schema rewrite before xRename (see below).
	if res := e.renameFollowupObjects(entryCtx, entry, oldName, newName); res != nil {
		return res
	}
	e.updateRenameCaches(oldName, newName)
	e.renameSQLiteSequence(oldName, newName)
	return &Result{}
}

// validateRenameNames runs the pre-rewrite rename validations when
// writable_schema is off: the legacy rename check and, outside legacy
// ALTER TABLE mode, the column-reference ambiguity check.
func (e *DDLExecutor) validateRenameNames(oldName, newName string) *Result {
	if e.ctx.WritableSchema() {
		return nil
	}
	if err := e.validateRename(oldName, newName); err != nil {
		return &Result{Error: err}
	}
	if !e.ctx.LegacyAlterTable() {
		if amb := e.checkRenameAmbiguity(oldName, newName); amb != nil && amb.Error != nil {
			return amb
		}
	}
	return nil
}

// renameFollowupObjects performs the post-rewrite object renames in alter.c
// order: schema-wide related entries, the FTS3/4 module plus its shadow
// tables, the FTS5 module, and the rtree shadow family.
func (e *DDLExecutor) renameFollowupObjects(entryCtx *DatabaseContext, entry *schema.Entry, oldName, newName string) *Result {
	if !e.ctx.WritableSchema() {
		e.renameUpdateRelatedEntries(entry.Name, newName, entryCtx.Name)
	}
	if ftsMod := e.getFTSModuleForTable(oldName); ftsMod != nil {
		ftsMod.RenameTable(oldName, newName)
		e.renameFTSShadowTables(entryCtx, oldName, newName)
	}
	// An fts5 table renames its shadow family through the module
	// (fts5StorageRename) and follows the engine map key.
	if err := e.renameFTS5(oldName, newName); err != nil {
		return &Result{Error: err}
	}
	// Rename the three shadow tables of an rtree family vtab to follow their
	// owner (rtree.c rtreeRename); collision pre-check ran earlier.
	if vtabFamilyModuleOf(entry.SQL) {
		e.renameRTreeShadowTables(entryCtx, oldName, newName)
	}
	return nil
}

// findAlterRenameTarget resolves the RENAME target: a plain table when found,
// otherwise the view path (SQLite's ALTER TABLE resolves views too) with the
// table-name-collision and reserved-new-name checks in alter.c order. A nil
// Result with a nil entry means the object does not exist (res carries the
// "no such table" error).
func (e *DDLExecutor) findAlterRenameTarget(oldName, newName string) (*schema.Entry, *DatabaseContext, *Result) {
	entry, entryCtx, err := e.ctx.FindTable(oldName)
	if err == nil {
		return entry, entryCtx, nil
	}
	// A view target passes the new-name collision check and is then rejected
	// with its own message (alter.c IsView check).
	if res := e.alterViewRenameTarget(oldName, newName); res != nil {
		return nil, nil, res
	}
	return nil, nil, &Result{Error: err}
}

// alterViewRenameTarget rejects an ALTER TABLE RENAME whose target resolves
// to a view, mirroring alter.c sqlite3AlterRenameTable's ordering: the
// new-name collision check runs first ("there is already another table or
// index with this name"), then the reserved-new-name check, then the IsView
// rejection ("view %s may not be altered"). Returns nil when oldName does
// not resolve to a view.
func (e *DDLExecutor) alterViewRenameTarget(oldName, newName string) *Result {
	viewEntry, viewCtx, vErr := e.ctx.FindView(oldName)
	if vErr != nil {
		return nil
	}
	if res := e.checkRenameTarget(oldName, newName, viewEntry, viewCtx); res != nil {
		return res
	}
	if res := e.validateReservedName(newName); res != nil {
		return res
	}
	return &Result{Error: fmt.Errorf("view %s may not be altered", viewEntry.Name)}
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
		sqlText := entry.SQL
		// SQLite's virtual-table rename still records the new object name in
		// CREATE VIRTUAL TABLE SQL even with legacy_alter_table enabled.
		if e.isVirtualTable(entry) {
			sqlText = replaceTableNameInSQL(sqlText, oldName, newName)
		}
		if err := entryCtx.Schema.RenameTableEntryWithSQL(oldName, newName, sqlText, schema.TypeTable); err != nil {
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
			// The stored CREATE may quote the shadow name ("xyz_content" —
			// the persisted form after an earlier rename, or the oracle's
			// own storage): replace the QUOTED identifier first so the
			// bare-name substitution cannot land inside the quotes and
			// double them (fts3d-6.6: ""ott_content"" failed to parse as
			// "malformed database schema (ott_content)"). Fall back to the
			// bare name for the unquoted in-memory form. Mirrors
			// renameRTreeShadowTables and the oracle's stored text
			// CREATE TABLE "ott_content"(...).
			newSQL := strings.Replace(ent.SQL, sqlQuoteIdentifier(oldShadow), sqlQuoteIdentifier(newShadow), 1)
			if newSQL == ent.SQL {
				newSQL = strings.Replace(ent.SQL, oldShadow, sqlQuoteIdentifier(newShadow), 1)
			}
			_ = ctx.Schema.RenameTableEntryWithSQL(oldShadow, newShadow, newSQL, schema.TypeTable)
		}
	}
}

// vtabFamilyModuleOf reports whether entry's stored SQL creates an
// rtree-family virtual table ("rtree" / "rtree_i32" / "geopoly" — the
// geopoly module shares the rtree shadow family and rename lifecycle).
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
	return name == "rtree" || name == "rtree_i32" || name == "geopoly"
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
	// WITHOUT ROWID sqlite_sequence (user-created via PRAGMA writable_schema)
	// stores names keyed by PK, not rowid: match by decoded NAME value.
	if strings.Contains(strings.ToUpper(entry.SQL), "WITHOUT ROWID") {
		e.renameWRSequenceRows(entry, oldName, newName)
		return
	}
	tree := e.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
	for _, rowID := range collectSequenceRenameRows(tree, oldName) {
		e.rewriteSequenceRow(entry, tree, rowID, newName)
	}
}

// wrSequenceRename is one pending WITHOUT ROWID sequence row rewrite: the
// original cell payload (the delete key) and the re-encoded record.
type wrSequenceRename struct {
	old []byte
	new []byte
}

// renameWRSequenceRows renames a WITHOUT ROWID sqlite_sequence entry by NAME
// value: index-leaf cells share synthetic RowID 0, so rowid-addressed
// rewrite misses. Decodes each cell, swaps values[0] == oldName, and
// re-inserts the PK-first record as an index-leaf cell.
func (e *DDLExecutor) renameWRSequenceRows(entry *schema.Entry, oldName, newName string) {
	tree := e.ctx.TableBTreeForName(entry.Name, entry.RootPage, false)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return
	}
	for _, rn := range collectWRSequenceRenames(cursor, oldName, newName) {
		if _, err := tree.DeleteCellsWhere(func(c *storage.Cell) bool {
			return string(c.Payload) == string(rn.old)
		}); err != nil {
			return
		}
		_ = tree.InsertCell(&storage.Cell{Type: storage.CellIndexLeaf, Payload: rn.new})
	}
}

// collectWRSequenceRenames scans a WITHOUT ROWID sqlite_sequence and collects
// the payload rewrites for rows whose name column equals oldName.
func collectWRSequenceRenames(cursor *btree.Cursor, oldName, newName string) []wrSequenceRename {
	var renames []wrSequenceRename
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		if rn, ok := wrRenameCell(cell, oldName, newName); ok {
			renames = append(renames, rn)
		}
		if ok, _ := cursor.Next(); !ok {
			break
		}
	}
	return renames
}

// wrRenameCell decodes one sequence cell and, when its name column equals
// oldName, returns the rewrite: the original payload as the delete key and
// the record re-encoded with newName in values[0]. Undecodable cells and
// encode failures never produce a rewrite.
func wrRenameCell(cell *storage.Cell, oldName, newName string) (wrSequenceRename, bool) {
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil || len(rec.Values) == 0 {
		return wrSequenceRename{}, false
	}
	if name, ok := rec.Values[0].(string); ok && name == oldName {
		rec.Values[0] = newName
		newPayload, err := storage.EncodeRecord(rec.Values)
		if err == nil {
			return wrSequenceRename{old: append([]byte(nil), cell.Payload...), new: newPayload}, true
		}
	}
	return wrSequenceRename{}, false
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
	oldColDefs := append([]sql.ColumnDef(nil), colDefs...)
	if res := e.quoteFixRenameSchema(tableName, tableEntry); res != nil {
		return res
	}
	colDefs = append([]sql.ColumnDef(nil), colDefs...)
	if err := renameColumnInDefs(tableName, oldColName, newColName, colDefs); err != nil {
		return &Result{Error: err}
	}
	e.ctx.ColCache()[tableName] = colDefs
	if res := checkTriggerSelectAmbiguity(e, tableName, oldColName, newColName); res != nil {
		e.ctx.ColCache()[tableName] = oldColDefs
		return res
	}
	if res := updateRenamedColumnSchema(e, tableEntry, tableName, oldColName, newColName, s.NewNameQuoted); res != nil {
		e.ctx.ColCache()[tableName] = oldColDefs
		return res
	}
	e.renameColumnInTriggers(tableName, oldColName, newColName, newColName, s.NewNameQuoted)
	if res := validateTriggersAgainstRenamedColumns(e, tableName); res != nil {
		return res
	}
	if res := checkTriggerViewColRefs(e, tableName, oldColName, newColName); res != nil {
		return res
	}
	e.renameColumnInIndexes(tableName, oldColName, newColName, newColName, s.NewNameQuoted)
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

// quoteFixRenameSchema normalizes DQS schema SQL before changing the table's column set.
func (e *DDLExecutor) quoteFixRenameSchema(tableName string, tableEntry *schema.Entry) *Result {
	for _, dbCtx := range e.ctx.Databases() {
		if dbCtx.Name != "main" && dbCtx.Name != "temp" {
			continue
		}
		entries, err := dbCtx.Schema.GetEntries("")
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.Type == schema.TypeTable && strings.EqualFold(entry.Name, tableName) {
				continue
			}
			if !strings.EqualFold(entry.TblName, tableName) || (entry.Type != schema.TypeIndex && entry.Type != schema.TypeTrigger) {
				continue
			}
			fixed := e.quoteFixWithSchema("main", entry.SQL)
			if fixed != entry.SQL {
				entry.SQL = fixed
				if err := dbCtx.Schema.UpdateEntry(entry.Name, fixed); err != nil {
					return &Result{Error: err}
				}
			}
		}
	}
	fixedTable := e.quoteFixWithSchema("main", tableEntry.SQL)
	if fixedTable != tableEntry.SQL {
		tableEntry.SQL = fixedTable
		if err := e.ctx.Schema().UpdateEntry(tableEntry.Name, fixedTable); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

// updateRenamedColumnSchema rewrites the CREATE TABLE SQL for the renamed column.
func updateRenamedColumnSchema(e *DDLExecutor, tableEntry *schema.Entry, tableName, oldColName, newColName string, quoted bool) *Result {
	renameCtx := &RenameContext{OldName: oldColName, NewName: newColName, QuotedNew: newColName, ForceQuote: quoted, TableName: tableName}
	newSQL := renameColumnInCreateTableSQL(tableEntry.SQL, renameCtx)
	if newSQL == "" || newSQL == tableEntry.SQL {
		return nil
	}
	tableEntry.SQL = newSQL
	if err := e.ctx.Schema().UpdateEntry(tableEntry.Name, newSQL); err != nil {
		return &Result{Error: fmt.Errorf("failed to update schema entry: %w", err)}
	}
	return nil
}

func validateTriggersAgainstRenamedColumns(e *DDLExecutor, tableName string) *Result {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Type != schema.TypeTrigger || !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		if err := e.checkTriggerColRefs(entry); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

func renamedColumnInUpdateFromSubquery(e *DDLExecutor, entry *schema.Entry, update *sql.UpdateStmt, tableName, oldColName, newColName string) error {
	refs := make([]*sql.ColumnRef, 0)
	collectNestedSelectRefs(update.From.Subquery, &refs)
	for _, join := range update.FromJoins {
		collectNestedSelectRefs(join.Table.Subquery, &refs)
		if subquery, ok := join.On.(*sql.Subquery); ok {
			collectNestedSelectRefs(subquery.Select, &refs)
		} else {
			collectExprTriggerColRefs(join.On, &refs, false)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	tables := []sql.TableRef{update.From}
	for _, join := range update.FromJoins {
		tables = append(tables, join.Table)
	}
	for _, ref := range refs {
		if ref.Table != "" || (!strings.EqualFold(ref.Name, oldColName) && !strings.EqualFold(ref.Name, newColName)) {
			continue
		}
		matches := 0
		for _, table := range tables {
			name := baseTableName(table.Name)
			if strings.EqualFold(name, tableName) {
				matches++
				continue
			}
			other, _, err := e.ctx.FindTable(name)
			if err != nil || other == nil {
				continue
			}
			for _, def := range e.ctx.ParseColumnDefs(other.Name, other.SQL) {
				if strings.EqualFold(def.Name, newColName) {
					matches++
					break
				}
			}
		}
		if matches > 1 {
			return fmt.Errorf("error in trigger %s after rename: no such column: %s", entry.Name, oldColName)
		}
	}
	return nil
}

func collectNestedSelectRefs(sel *sql.SelectStmt, refs *[]*sql.ColumnRef) {
	if sel == nil {
		return
	}
	for _, column := range sel.Columns {
		collectExprTriggerColRefs(column.Expr, refs, false)
	}
	collectNestedSelectRefs(sel.From.Subquery, refs)
	for _, join := range sel.Joins {
		collectNestedSelectRefs(join.Table.Subquery, refs)
		collectExprTriggerColRefs(join.On, refs, false)
	}
}

func checkTriggerSelectAmbiguity(e *DDLExecutor, tableName, oldColName, newColName string) *Result {
	for _, dbCtx := range e.ctx.Databases() {
		entries, err := dbCtx.Schema.GetEntries("")
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.Type != schema.TypeTrigger {
				continue
			}
			stmts, err := parse.ParseSQL(entry.SQL)
			if err != nil {
				continue
			}
			for _, stmt := range stmts {
				trigger, ok := stmt.(*sql.CreateTriggerStmt)
				if !ok {
					continue
				}
				for _, body := range trigger.Statements {
					if update, ok := body.(*sql.UpdateStmt); ok {
						if err := renamedColumnInUpdateFromSubquery(e, entry, update, tableName, oldColName, newColName); err != nil {
							return &Result{Error: err}
						}
					}
					var selectStmt *sql.SelectStmt
					switch s := body.(type) {
					case *sql.SelectStmt:
						selectStmt = s
					case *sql.InsertStmt:
						selectStmt = s.Select
					}
					for member := selectStmt; member != nil; member = member.Union {
						var refs []*sql.ColumnRef
						collectSelectTriggerColRefs(member, &refs, false)
						for _, ref := range refs {
							if ref.Table != "" || !strings.EqualFold(ref.Name, newColName) {
								continue
							}
							matches := 0
							for _, from := range selectTables(member) {
								if strings.EqualFold(baseTableName(from.Name), baseTableName(tableName)) {
									matches++
									continue
								}
								other, _, findErr := e.ctx.FindTable(from.Name)
								if findErr != nil {
									continue
								}
								for _, def := range e.ctx.ParseColumnDefs(other.Name, other.SQL) {
									if strings.EqualFold(def.Name, newColName) {
										matches++
										break
									}
								}
							}
							if matches > 1 {
								return &Result{Error: fmt.Errorf("error in trigger %s after rename: ambiguous column name: %s", entry.Name, newColName)}
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func selectTables(sel *sql.SelectStmt) []sql.TableRef {
	if sel == nil {
		return nil
	}
	tables := []sql.TableRef{sel.From}
	for _, join := range sel.Joins {
		tables = append(tables, join.Table)
	}
	return tables
}

// checkTriggerViewColRefs rejects a column rename when a trigger references
// the old column name only through a view that depends on the renamed table.
func checkTriggerViewColRefs(e *DDLExecutor, tableName, oldColName, newColName string) *Result {
	if res := checkTriggerSelectAmbiguity(e, tableName, oldColName, newColName); res != nil {
		return res
	}
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
func renameColumnInCreateTableSQL(sqlText string, ctx *RenameContext) string {
	statements, err := parse.ParseSQL(sqlText)
	if err != nil || len(statements) != 1 {
		return ""
	}
	create, ok := statements[0].(*sql.CreateTableStmt)
	if !ok {
		return ""
	}
	ranges, err := FindRenameTokens(sqlText, ctx)
	if err != nil {
		return ""
	}
	ranges = appendColumnDeclarationRange(sqlText, create, ctx, ranges)
	return rename.ApplyContextRenames(sqlText, ranges, ctx)
}

func renameTokenEnd(source string, token sql.Token) int {
	if token.End > token.Pos {
		return token.End
	}
	if token.Pos < 0 || token.Pos >= len(source) || source[token.Pos] != '\'' {
		return token.End
	}
	for i := token.Pos + 1; i < len(source); i++ {
		if source[i] == '\'' {
			if i+1 < len(source) && source[i+1] == '\'' {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(source)
}

func appendColumnDeclarationRange(source string, create *sql.CreateTableStmt, ctx *RenameContext, ranges []RenameRange) []RenameRange {
	tokens := make([]sql.Token, 0)
	tokenizer := sql.NewTokenizer(source)
	for {
		token := tokenizer.Next()
		if token.Type == sql.TokenEOF || token.Type == sql.TokenError {
			break
		}
		tokens = append(tokens, token)
	}
	open := -1
	for i, token := range tokens {
		if token.Type == sql.TokenLParen {
			open = i
			break
		}
	}
	if open < 0 {
		return ranges
	}
	depth := 1
	for i := open + 1; i < len(tokens); {
		start := i
		for i < len(tokens) && depth > 0 {
			switch tokens[i].Type {
			case sql.TokenLParen:
				depth++
			case sql.TokenRParen:
				depth--
			case sql.TokenComma:
				if depth == 1 {
					i++
					goto nextDefinition
				}
			}
			if depth > 0 {
				i++
			}
		}
		if start < len(tokens) && (tokens[start].Type == sql.TokenIdentifier || tokens[start].Type == sql.TokenString) && strings.EqualFold(tokens[start].Value, ctx.OldName) {
			ranges = append(ranges, RenameRange{Start: tokens[start].Pos, End: renameTokenEnd(source, tokens[start])})
		}
		break
	nextDefinition:
		if start < len(tokens) && (tokens[start].Type == sql.TokenIdentifier || tokens[start].Type == sql.TokenString) && strings.EqualFold(tokens[start].Value, ctx.OldName) {
			ranges = append(ranges, RenameRange{Start: tokens[start].Pos, End: renameTokenEnd(source, tokens[start])})
		}
	}
	return appendConstraintColumnRanges(source, create, ctx, ranges)
}

func appendConstraintColumnRanges(source string, create *sql.CreateTableStmt, ctx *RenameContext, ranges []RenameRange) []RenameRange {
	if len(create.Constraints) == 0 {
		return ranges
	}
	tokenizer := sql.NewTokenizer(source)
	var tokens []sql.Token
	for {
		token := tokenizer.Next()
		if token.Type == sql.TokenEOF || token.Type == sql.TokenError {
			break
		}
		tokens = append(tokens, token)
	}
	for i := 0; i < len(tokens); i++ {
		isColumnList := strings.EqualFold(tokens[i].Value, "UNIQUE") ||
			strings.EqualFold(tokens[i].Value, "PRIMARY") && i+1 < len(tokens) && strings.EqualFold(tokens[i+1].Value, "KEY") ||
			strings.EqualFold(tokens[i].Value, "FOREIGN") && i+1 < len(tokens) && strings.EqualFold(tokens[i+1].Value, "KEY")
		if !isColumnList {
			continue
		}
		open := i + 1
		if strings.EqualFold(tokens[i].Value, "PRIMARY") || strings.EqualFold(tokens[i].Value, "FOREIGN") {
			open = i + 2
		}
		for open < len(tokens) && tokens[open].Type != sql.TokenLParen && tokens[open].Type != sql.TokenComma && tokens[open].Type != sql.TokenRParen {
			open++
		}
		if open >= len(tokens) || tokens[open].Type != sql.TokenLParen {
			continue
		}
		depth := 1
		for j := open + 1; j < len(tokens) && depth > 0; j++ {
			switch tokens[j].Type {
			case sql.TokenLParen:
				depth++
			case sql.TokenRParen:
				depth--
			default:
				if depth == 1 && (tokens[j].Type == sql.TokenIdentifier || tokens[j].Type == sql.TokenString) && strings.EqualFold(tokens[j].Value, ctx.OldName) {
					ranges = append(ranges, RenameRange{Start: tokens[j].Pos, End: renameTokenEnd(source, tokens[j])})
				}
			}
		}
	}
	return ranges
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
func appendViewColumnRenameRanges(source, tableName, oldName string, ranges []RenameRange) []RenameRange {
	tokenizer := sql.NewTokenizer(source)
	var tokens []sql.Token
	for {
		token := tokenizer.Next()
		if token.Type == sql.TokenEOF || token.Type == sql.TokenError {
			break
		}
		tokens = append(tokens, token)
	}
	for i, token := range tokens {
		if token.Type != sql.TokenIdentifier || !strings.EqualFold(token.Value, oldName) {
			continue
		}
		if i > 0 && tokens[i-1].Type == sql.TokenDot {
			if i > 1 && strings.EqualFold(tokens[i-2].Value, tableName) {
				appendRenameRange(&ranges, token)
			}
			continue
		}
		if i > 0 && strings.EqualFold(tokens[i-1].Value, "AS") {
			continue
		}
		appendRenameRange(&ranges, token)
	}
	return ranges
}

func appendRenameRange(ranges *[]RenameRange, token sql.Token) {
	for _, existing := range *ranges {
		if existing.Start == token.Pos && existing.End == token.End {
			return
		}
	}
	*ranges = append(*ranges, RenameRange{Start: token.Pos, End: token.End})
}

func sqlRenameIdentifier(name string) string {
	token := sql.NewTokenizer(name).Next()
	if token.Type == sql.TokenIdentifier && token.Value == name {
		return name
	}
	return sqlQuoteIdentifier(name)
}

func (e *DDLExecutor) renameColumnInViews(tableName, oldColName, newColName string) {
	ctx := &RenameContext{OldName: oldColName, NewName: newColName, QuotedNew: newColName, IsTable: false, TableName: tableName}
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
			ranges, err := FindRenameTokens(entry.SQL, ctx)
			if err == nil {
				ranges = appendViewColumnRenameRanges(entry.SQL, tableName, oldColName, ranges)
			}
			if err != nil || len(ranges) == 0 {
				continue
			}
			newSQL := ApplyRenames(entry.SQL, ranges, sqlRenameIdentifier(newColName))
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
	for _, dbCtx := range e.ctx.Databases() {
		if entryType == schema.TypeTrigger {
			if dbCtx.Name != "main" && dbCtx.Name != "temp" {
				continue
			}
		} else if dbCtx != e.ctx.MainDB() {
			continue
		}
		entries, err := dbCtx.Schema.GetEntries("")
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !columnRenameEntryMatches(entry, entryType, tblName, oldColName) {
				continue
			}
			if applyColumnRenameToEntry(entry, oldColName, newColName, ctx) {
				_ = dbCtx.Schema.UpdateEntry(entry.Name, entry.SQL)
			}
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
	if entry.Type == schema.TypeIndex {
		newSQL, err := renameColumnInIndexSQL(entry.SQL, oldColName, ctx.NewName, entry.TblName, ctx.ForceQuote)
		if err == nil && newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			return true
		}
		return false
	}
	if entry.Type == schema.TypeTrigger {
		newSQL, err := renameColumnInTriggerSQL(entry.SQL, oldColName, ctx.NewName, strings.EqualFold(entry.TblName, ctx.TableName), ctx.ForceQuote)
		if err == nil && newSQL != entry.SQL && newSQL != "" {
			entry.SQL = newSQL
			return true
		}
		return false
	}
	ranges, rErr := FindRenameTokens(entry.SQL, ctx)
	if rErr == nil && len(ranges) > 0 {
		newSQL := rename.ApplyContextRenames(entry.SQL, ranges, ctx)
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
