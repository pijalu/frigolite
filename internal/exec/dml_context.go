package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// This file implements the execdml.DMLContext capability interface on the
// Engine. The DML executor (internal/execdml) depends on this interface
// rather than on the concrete Engine type (Dependency Inversion).

// Compile-time probe: Engine implements execdml.DMLContext.
var _ execdml.DMLContext = (*Engine)(nil)

// BuildColumnNames builds the result column names for a SELECT (delegated).
func (e *Engine) BuildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef, sel *sql.SelectStmt) []string {
	return e.buildColumnNames(columns, colDefs, sel)
}

// BuildRowMap builds a RowMap from a stored record (delegated).
func (e *Engine) BuildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) RowMap {
	return e.buildRowMap(rec, colDefs, rowID)
}

// BuildOutputRow builds one output row from an expression list (delegated).
// Used by the DML/DDL RETURNING-clause path, which tolerates expression
// errors by rendering NULL (the SELECT scan path uses the error-returning
// execquery buildOutputRow to abort on function errors).
func (e *Engine) BuildOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) []interface{} {
	outRow, _ := e.buildOutputRow(columns, colDefs, row)
	return outRow
}

// ExecSelect executes a SELECT statement (delegated).
func (e *Engine) ExecSelect(s *sql.SelectStmt) *Result {
	return e.execSelect(s)
}

// ExecSelectView executes a SELECT over a view body (delegated).
func (e *Engine) ExecSelectView(viewEntry *schema.Entry) *Result {
	return e.execSelectView(viewEntry)
}

// HandleSelectAggregates runs aggregate processing for a SELECT (delegated).
func (e *Engine) HandleSelectAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	return e.handleSelectAggregates(s, rowMaps, colDefs)
}

// FinalizeSelectResult applies ORDER BY/LIMIT and final column naming.
func (e *Engine) FinalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result {
	return e.finalizeSelectResult(result, s, rowMaps)
}

// CompareOrderByValues compares two values under an ORDER BY term (delegated).
func (e *Engine) CompareOrderByValues(left, right interface{}, ob sql.OrderByTerm) int {
	return e.compareOrderByValues(left, right, ob)
}

// RowPassesWhere evaluates a WHERE predicate against a row (delegated).
func (e *Engine) RowPassesWhere(where sql.Expr, row Row, cursor *btree.Cursor) (bool, error) {
	return e.rowPassesWhere(where, row, cursor)
}

// ValidateDMLSubqueries validates subqueries inside DML statements (delegated).
func (e *Engine) ValidateDMLSubqueries(stmt sql.Stmt) error {
	return e.validateDMLSubqueries(stmt)
}

// ComputeWindowValues computes a window function's value for every row in
// rowMaps (delegated to the SELECT engine's window pass).
func (e *Engine) ComputeWindowValues(fn *sql.FuncCall, windows []sql.WindowDef, rowMaps []RowMap) ([]interface{}, error) {
	return e.selectEngine.ComputeWindowValues(fn, windows, rowMaps)
}

// TableColumnNames returns the declared column names of a table (delegated).
func (e *Engine) TableColumnNames(tableName string) ([]string, error) {
	return e.tableColumnNames(tableName)
}

// RootPage returns the current root page for a table, checking tracked root
// pages first.
func (e *Engine) RootPage(tableName string, schemaRoot uint32) uint32 {
	return e.rootPage(tableName, schemaRoot)
}

// TablePager returns the pager that owns the given table.
func (e *Engine) TablePager(tableName string) *pager.Pager {
	return e.tablePager(tableName)
}

// TriggerDepthLimit returns the maximum trigger nesting depth (0 = default).
func (e *Engine) TriggerDepthLimit() int {
	return e.triggers.DepthLimit()
}

// Authorize runs the SQL authorizer callback for an action.
func (e *Engine) Authorize(action auth.Action, arg1, arg2, arg3, arg4 string) error {
	return e.authorize(action, arg1, arg2, arg3, arg4)
}

// AuthorizeResult runs the SQL authorizer callback and returns its raw
// Result, so callers can distinguish IGNORE (skip the operation silently)
// from OK. A nil authorizer returns ResultOK.
func (e *Engine) AuthorizeResult(action auth.Action, arg1, arg2, arg3, arg4 string) auth.Result {
	a := e.authorizer
	if a == nil {
		return auth.ResultOK
	}
	return a.Authorize(action, arg1, arg2, arg3, arg4)
}

// Databases returns the attached database contexts (upper-cased key).
func (e *Engine) Databases() map[string]*DatabaseContext {
	return e.databases
}

// GetDB returns the database context for a schema name (nil when absent).
func (e *Engine) GetDB(name string) *DatabaseContext {
	return e.getDB(name)
}

// IsNonModifiableTable reports whether a system/pragma table may not be
// modified.
func (e *Engine) IsNonModifiableTable(entry *schema.Entry) bool {
	return e.isNonModifiableTable(entry)
}

// IsStoragelessVirtualTable reports whether a virtual table has no
// module-backed storage.
func (e *Engine) IsStoragelessVirtualTable(entry *schema.Entry) bool {
	return e.isStoragelessVirtualTable(entry)
}

// IsVirtualTable reports whether the schema entry is a virtual table.
func (e *Engine) IsVirtualTable(entry *schema.Entry) bool {
	return e.isVirtualTable(entry)
}

// ReadCellByRowID reads the cell with the given rowid from a btree.
func (e *Engine) ReadCellByRowID(tree *btree.BTree, rowID int64) (*storage.Cell, error) {
	return e.readCellByRowID(tree, rowID)
}

// TableHasAutoIncrement reports whether the table declares an AUTOINCREMENT
// column.
func (e *Engine) TableHasAutoIncrement(tableName string) bool {
	return e.tableHasAutoIncrement(tableName)
}

// RandomFreeRowID picks a random positive rowid not already in the table.
func (e *Engine) RandomFreeRowID(tree *btree.BTree) int64 {
	return e.randomFreeRowID(tree)
}

// UpdateRootPage tracks a root page change after a b-tree split and persists
// it to sqlite_schema.
func (e *Engine) UpdateRootPage(tableName string, newRoot uint32) {
	e.updateRootPage(tableName, newRoot)
}

// TrackRootPage records a root page in the in-memory tracking map without
// persisting it (used for index root pages, which carry no pager at tracking
// time and stay name-keyed).
func (e *Engine) TrackRootPage(name string, root uint32) {
	e.caches.tableRootPages[tableRootKey{name: name}] = root
}

// RootPagePg returns the current root page for the named table on the given
// pager (tracked root first, then the schema root).
func (e *Engine) RootPagePg(pg *pager.Pager, tableName string, schemaRoot uint32) uint32 {
	return e.rootPagePg(pg, tableName, schemaRoot)
}

// UpdateRootPagePg tracks and persists a root page change for the named table
// on the given pager.
func (e *Engine) UpdateRootPagePg(pg *pager.Pager, tableName string, newRoot uint32) {
	e.updateRootPagePg(pg, tableName, newRoot)
}

// InvalidateTableCaches clears per-table caches that depend on the schema.
func (e *Engine) InvalidateTableCaches() {
	e.invalidateTableCaches()
}

// RestorePager restores a pager snapshot and invalidates all schema caches.
func (e *Engine) RestorePager(pg *pager.Pager, snap *pager.PagerState) {
	e.restorePager(pg, snap)
}

// EchoVTabSource resolves the source table of an echo virtual table.
func (e *Engine) EchoVTabSource(name string) (string, bool) {
	return e.echoVTabSource(name)
}

// RewriteEchoInsert rewrites an INSERT into an echo virtual table.
func (e *Engine) RewriteEchoInsert(s *sql.InsertStmt, srcName string) {
	e.rewriteEchoInsert(s, srcName)
}

// ExecFTSDelete executes a DELETE against an FTS table. A re-entrant DELETE on
// the same FTS table (from a shadow-table trigger body) is rejected with
// "SQL logic error" (SQLite's fts3DeleteMethod recursion guard; fts3aa-10.1).
func (e *Engine) ExecFTSDelete(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) *Result {
	if e.ftsDeleteDepth[tableName] > 0 {
		return &Result{Error: fmt.Errorf("SQL logic error")}
	}
	e.ftsDeleteDepth[tableName]++
	defer func() { e.ftsDeleteDepth[tableName]-- }()

	res := e.execFTSDelete(tableName, ftsTable, colDefs, s)
	if res.Error != nil {
		return res
	}
	// SQLite's FTS3 DELETE writes the %_content shadow table, which fires
	// AFTER DELETE triggers registered on it. Fire them per deleted row so a
	// trigger body re-entering DELETE on the FTS table hits the guard above.
	contentName := tableName + "_content"
	for i := int64(0); i < res.Changes; i++ {
		rowMap := make(RowMap)
		rowMap["rowid"] = &util.ColumnValue{Value: i + 1, Affinity: 'I'}
		if err := e.dml.FireAfterDeleteTriggers(contentName, rowMap); err.Error != nil {
			return err
		}
	}
	return res
}

// ExecFTSUpdate executes an UPDATE against an FTS table.
func (e *Engine) ExecFTSUpdate(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt) *Result {
	return e.execFTSUpdate(tableName, ftsTable, colDefs, s)
}

// JoinUpdateFromRows builds the combined row maps for an UPDATE ... FROM's
// target row (delegates to the DML executor; fts4upfrom 1.x).
func (e *Engine) JoinUpdateFromRows(s *sql.UpdateStmt, targetRow RowMap) ([]RowMap, error) {
	return e.dml.JoinUpdateFromRows(s, targetRow)
}

// CheckForeignKeyViolations verifies FK constraints for a row write.
func (e *Engine) CheckForeignKeyViolations(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, excludeRowID int64) *Result {
	return e.constraints.CheckForeignKeyViolations(tableEntry, colDefs, values, excludeRowID)
}

// FKChildTableNames returns the names of child tables whose FOREIGN KEY
// constraints reference the given parent table (EXPLAIN QUERY PLAN models the
// FK-check scans SQLite plans for a parent DELETE/UPDATE).
func (e *Engine) FKChildTableNames(tableName string) []string {
	if !e.settings.foreignKeys {
		return nil
	}
	entry, ctx, err := e.FindTable(tableName)
	if err != nil || ctx == nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, ref := range e.constraints.ChildRefs(entry, ctx) {
		if ref.ChildTable == "" || seen[ref.ChildTable] {
			continue
		}
		seen[ref.ChildTable] = true
		names = append(names, ref.ChildTable)
	}
	return names
}

// FkParentDelete handles ON DELETE actions when a parent row is deleted.
func (e *Engine) FkParentDelete(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return e.constraints.FkParentDelete(parentTable, parentColDefs, oldRow)
}

// FkParentDeleteReplace handles REPLACE delete actions on a parent row.
func (e *Engine) FkParentDeleteReplace(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return e.constraints.FkParentDeleteReplace(parentTable, parentColDefs, oldRow)
}

// FkParentUpdate handles ON UPDATE actions when a parent row is updated.
func (e *Engine) FkParentUpdate(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, skipRowID int64) *Result {
	return e.constraints.FkParentUpdate(parentTable, parentColDefs, oldRow, newRow, skipRowID)
}

// FkCheckReplaceChildren re-validates FK children after a REPLACE.
func (e *Engine) FkCheckReplaceChildren(parentEntry *schema.Entry, parentCtx *DatabaseContext) *Result {
	return e.constraints.FkCheckReplaceChildren(parentEntry, parentCtx)
}

// SetTriggerDepth sets the current trigger execution depth.
func (e *Engine) SetTriggerDepth(depth int) {
	e.triggers.SetDepth(depth)
}

// TriggerTables returns the chain of tables currently in trigger programs.
func (e *Engine) TriggerTables() []string {
	return e.triggers.Tables()
}

// SetTriggerTables sets the chain of tables in trigger programs.
func (e *Engine) SetTriggerTables(tables []string) {
	e.triggers.SetTables(tables)
}

// SetTriggerNewRow sets the new-row values for trigger program execution.
func (e *Engine) SetTriggerNewRow(row Row) {
	e.triggers.SetNewRow(row)
}

// SetTriggerOldRow sets the old-row values for trigger program execution.
func (e *Engine) SetTriggerOldRow(row Row) {
	e.triggers.SetOldRow(row)
}

// SetLastRowID sets the rowid of the last inserted row.
func (e *Engine) SetLastRowID(id int64) {
	e.lastRowID = id
}

// SetReturningStrict sets whether RETURNING treats unknown columns as errors.
func (e *Engine) SetReturningStrict(strict bool) {
	e.returning.strict = strict
}

// SetReturningTable sets the table name in scope for RETURNING evaluation.
func (e *Engine) SetReturningTable(table string) {
	e.returning.table = table
}

// CachedTriggerFlag returns the cached trigger-existence flag for a table.
func (e *Engine) CachedTriggerFlag(tableName string) (bool, bool) {
	return e.triggers.CachedTriggerFlag(tableName)
}

// SetCachedTriggerFlag caches the trigger-existence flag for a table.
func (e *Engine) SetCachedTriggerFlag(tableName string, has bool) {
	e.triggers.SetCachedTriggerFlag(tableName, has)
}

// InitValidatedTriggers ensures the validated-trigger cache is non-nil.
func (e *Engine) InitValidatedTriggers() {
	e.triggers.InitValidatedTriggers()
}

// IsTriggerValidated reports whether a trigger was already validated.
func (e *Engine) IsTriggerValidated(key string) bool {
	return e.triggers.IsTriggerValidated(key)
}

// MarkTriggerValidated records a trigger as validated.
func (e *Engine) MarkTriggerValidated(key string) {
	e.triggers.MarkTriggerValidated(key)
}

// InitUniqueIdxCache ensures the unique-index cache is non-nil.
func (e *Engine) InitUniqueIdxCache() {
	if e.caches.uniqueIdxCache == nil {
		e.caches.uniqueIdxCache = make(map[string][]uniqueIndexDef)
	}
}

// CachedUniqueIdx returns the cached UNIQUE index definitions for a table.
func (e *Engine) CachedUniqueIdx(tableName string) ([]execquery.UniqueIndexDef, bool) {
	defs, ok := e.caches.uniqueIdxCache[tableName]
	return defs, ok
}

// SetCachedUniqueIdx caches UNIQUE index definitions for a table.
func (e *Engine) SetCachedUniqueIdx(tableName string, defs []execquery.UniqueIndexDef) {
	e.caches.uniqueIdxCache[tableName] = defs
}

// NextRowIDFor returns the cached largest rowid for a (pager, root page).
func (e *Engine) NextRowIDFor(pg *pager.Pager, page uint32) (int64, bool) {
	v, ok := e.caches.nextRowIDCache[e.rowidCacheKey(pg, page)]
	return v, ok
}

// SetNextRowIDFor records the largest rowid for a (pager, root page).
func (e *Engine) SetNextRowIDFor(pg *pager.Pager, page uint32, id int64) {
	e.caches.nextRowIDCache[e.rowidCacheKey(pg, page)] = id
}

// ResetNextRowIDCache drops all cached largest-rowid entries.
func (e *Engine) ResetNextRowIDCache() {
	e.caches.nextRowIDCache = make(map[rowidCacheKey]int64)
}

// AutoIncSeqFor returns the cached AUTOINCREMENT sequence for a table.
func (e *Engine) AutoIncSeqFor(pg *pager.Pager, page uint32) (int64, bool) {
	v, ok := e.caches.autoIncSeq[e.rowidCacheKey(pg, page)]
	return v, ok
}

// SetAutoIncSeqFor records the AUTOINCREMENT sequence for a table.
func (e *Engine) SetAutoIncSeqFor(pg *pager.Pager, page uint32, seq int64) {
	e.caches.autoIncSeq[e.rowidCacheKey(pg, page)] = seq
}

// ResetAutoIncSeq drops all cached AUTOINCREMENT sequences.
func (e *Engine) ResetAutoIncSeq() {
	e.caches.autoIncSeq = make(map[rowidCacheKey]int64)
}

// sqliteSequenceEntry returns the real sqlite_sequence table entry in the
// given database context, or nil if none exists (the engine creates it lazily
// with the first AUTOINCREMENT table, mirroring SQLite build.c:2922-2931).
func sqliteSequenceEntry(ctx *DatabaseContext) *schema.Entry {
	if ctx == nil || ctx.Schema == nil {
		return nil
	}
	entries, err := ctx.Schema.GetEntries(schema.TypeTable)
	if err != nil {
		return nil
	}
	for _, ent := range entries {
		if strings.EqualFold(ent.Name, "sqlite_sequence") || strings.EqualFold(ent.TblName, "sqlite_sequence") {
			return ent
		}
	}
	return nil
}

// SQLiteSequenceSeqFor reads the sqlite_sequence row for tableName from the
// real sqlite_sequence table, returning (seq, found, error). This mirrors
// SQLite's sqlite3AutoincrementBegin (insert.c): the INSERT statement reads
// the current sequence fresh from sqlite_sequence at statement start.
func (e *Engine) SQLiteSequenceSeqFor(pg *pager.Pager, tableName string) (int64, bool, error) {
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager != pg {
			continue
		}
		entry := sqliteSequenceEntry(ctx)
		if entry == nil {
			return 0, false, nil
		}
		tree := btree.NewBTree(pg, entry.RootPage, true)
		cursor, err := tree.OpenCursor()
		if err != nil {
			return 0, false, err
		}
		for {
			cell, err := cursor.ReadCell()
			if err != nil {
				return 0, false, nil
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err == nil && len(rec.Values) >= 2 {
				if name, ok := rec.Values[0].(string); ok && name == tableName {
					seq, _ := toInt64(rec.Values[1])
					return seq, true, nil
				}
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				return 0, false, nil
			}
		}
	}
	return 0, false, nil
}

// WriteSQLiteSequence writes (tableName, seq) to the real sqlite_sequence
// table, mirroring SQLite's autoIncrementEnd (insert.c): if the table has no
// row yet the row is inserted; otherwise it is updated only when seq exceeds
// the stored value. Returns an error on btree failure.
func (e *Engine) WriteSQLiteSequence(pg *pager.Pager, tableName string, seq int64) error {
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager != pg {
			continue
		}
		entry := sqliteSequenceEntry(ctx)
		if entry == nil {
			return nil
		}
		tree := btree.NewBTree(pg, entry.RootPage, true)
		// Locate the row by name.
		cursor, err := tree.OpenCursor()
		if err != nil {
			return err
		}
		var rowid int64 = -1
		var oldSeq int64
		for {
			cell, err := cursor.ReadCell()
			if err != nil {
				break
			}
			rec, derr := storage.DecodeRecord(cell.Payload)
			if derr == nil && len(rec.Values) >= 2 {
				if name, ok := rec.Values[0].(string); ok && name == tableName {
					rowid = cell.RowID
					oldSeq, _ = toInt64(rec.Values[1])
					break
				}
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		record, rerr := storage.EncodeRecord([]interface{}{tableName, seq})
		if rerr != nil {
			return rerr
		}
		if rowid < 0 {
			// No row yet: insert (name, seq) with the next available rowid.
			cursor, err := tree.OpenCursor()
			if err != nil {
				return err
			}
			last := int64(0)
			for {
				cell, err := cursor.ReadCell()
				if err != nil {
					break
				}
				if cell.RowID > last {
					last = cell.RowID
				}
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
			}
			return tree.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: last + 1, Payload: record})
		}
		if seq > oldSeq {
			if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
				return cell.RowID == rowid
			}); err != nil {
				return err
			}
			return tree.InsertCell(&storage.Cell{Type: storage.CellTableLeaf, RowID: rowid, Payload: record})
		}
		return nil
	}
	return nil
}

// toInt64 converts a record value to int64 for sequence arithmetic.
func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		return parseSequenceText(n)
	case []byte:
		return parseSequenceText(string(n))
	}
	return 0, false
}

// parseSequenceText applies SQLite integer affinity to sqlite_sequence.seq.
// Non-numeric text coerces to zero; out-of-range integers clamp to int64 bounds.
func parseSequenceText(text string) (int64, bool) {
	if n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
		return n, true
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, true
	}
	if f >= float64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1), true
	}
	if f <= -float64(^uint64(0)>>1)-1 {
		return -1 << 63, true
	}
	return int64(f), true
}

// BumpRowIDCache records a row with the given rowid as present in the table.
func (e *Engine) BumpRowIDCache(pg *pager.Pager, rootPage uint32, rowID int64) {
	e.bumpRowIDCache(pg, rootPage, rowID)
}

// InvalidateRowIDCache drops the cached largest rowid for a table.
func (e *Engine) InvalidateRowIDCache(pg *pager.Pager, rootPage uint32) {
	e.invalidateRowIDCache(pg, rootPage)
}

// SelectNeedsRowMaps reports whether a SELECT needs per-row maps.
func (e *Engine) SelectNeedsRowMaps(s *sql.SelectStmt, tableName string) bool {
	return selectNeedsRowMaps(e, s, tableName)
}

// SetCurrentScanTable sets the table name being scanned (for qualified column
// resolution during DML execution).
func (e *Engine) SetCurrentScanTable(name string) {
	e.selectEngine.SetCurrentScanTable(name)
}

// PushCTEScope pushes a CTE scope for DML WITH-clause execution.
func (e *Engine) PushCTEScope(ctes []sql.CTEDef) {
	e.selectEngine.PushCTEScope(ctes)
}

// PopCTEScope pops the most recent CTE scope.
func (e *Engine) PopCTEScope() {
	e.selectEngine.PopCTEScope()
}
