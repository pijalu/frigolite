package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *DMLExecutor) findRowByUniqueCols(tableName string, rootPage uint32, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, createSQL string) (int64, []interface{}, int, bool) {
	uniqueCols := collectUniqueColsWithPK(colDefs, colIndex, values)

	// Check table-level composite PRIMARY KEY / UNIQUE constraints for
	// REPLACE and UPSERT conflict detection. A composite key conflict occurs
	// when ALL columns in the group match simultaneously.
	if rowID, vals, col, ok := e.compositeConflictRow(tableName, rootPage, colDefs, values); ok {
		return rowID, vals, col, true
	}

	if len(uniqueCols) == 0 {
		return 0, nil, -1, false
	}

	// Fast path: when the only unique column is an INTEGER PRIMARY KEY, the
	// column value IS the rowid, so a conflict can be detected with a direct
	// rowid seek instead of a full-table scan. This matters for large tables
	// (e.g. delete3.test doubles a table via INSERT...SELECT 20 times) where
	// scanning per-row would be O(n²). The seek is definitive: if the rowid
	// does not exist there can be no UNIQUE conflict, so we return the result
	// directly without falling through to scanForConflict.
	if len(uniqueCols) == 1 {
		idx := uniqueCols[0]
		if idx < len(colDefs) && isIPKRowidAliasCol(colDefs[idx]) {
			return e.ipkRowidAliasConflict(tableName, rootPage, colDefs, values, idx)
		}
	}

	tree := e.uniqueScanTree(tableName, rootPage)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, nil, -1, false
	}

	return e.scanForConflict(cursor, uniqueCols, values, colDefs, createSQL)
}

// uniqueScanTree builds the btree used by UNIQUE/PRIMARY KEY conflict scans,
// preferring the modified table's context pager (an ATTACHed table named t1
// lives on the attached pager, not the main pager; resolving by name alone
// would scan the wrong table).
func (e *DMLExecutor) uniqueScanTree(tableName string, rootPage uint32) *btree.BTree {
	if e.currentDMLCtx != nil && e.currentDMLCtx.Pager != nil {
		return e.ctx.TableBTreePg(e.currentDMLCtx.Pager, tableName, rootPage, true)
	}
	return e.ctx.TableBTreeForName(tableName, rootPage, true)
}

// collectUniqueColsWithPK gathers the UNIQUE column indices and adds any
// PRIMARY KEY columns (which imply UNIQUE) whose values are non-NULL.
func collectUniqueColsWithPK(colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) []int {
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}
	return uniqueCols
}

// compositeConflictRow scans table-level composite PRIMARY KEY / UNIQUE groups
// for a row matching all group columns simultaneously.
func (e *DMLExecutor) compositeConflictRow(tableName string, rootPage uint32, colDefs []sql.ColumnDef, values []interface{}) (int64, []interface{}, int, bool) {
	tableEnt, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return 0, nil, -1, false
	}
	createSQL := ""
	if tableEnt != nil {
		createSQL = tableEnt.SQL
	}
	for _, group := range e.compositeUniqueGroups(tableName, tableEnt.SQL, colDefs) {
		// Skip if any group value is NULL (NULL never conflicts).
		if groupHasNull(group, values) {
			continue
		}
		tree := e.uniqueScanTree(tableName, rootPage)
		cursor, err := tree.OpenCursor()
		if err != nil {
			continue
		}
		if rowID, vals, col, ok := e.scanGroupForMatchWR(cursor, colDefs, group, values, createSQL); ok {
			return rowID, vals, col, true
		}
	}
	return 0, nil, -1, false
}

// groupHasNull reports whether any value in the composite group is NULL.
func groupHasNull(group []int, values []interface{}) bool {
	for _, idx := range group {
		if idx >= len(values) || values[idx] == nil {
			return true
		}
	}
	return false
}

// scanGroupForMatchWR walks a cursor looking for a record matching all group
// columns against the inserted values; the table's CREATE SQL lets WITHOUT
// ROWID PK-first records be remapped to declared order before the positional
// allMatch comparison.
func (e *DMLExecutor) scanGroupForMatchWR(cursor *btree.Cursor, colDefs []sql.ColumnDef, group []int, values []interface{}, createSQL string) (int64, []interface{}, int, bool) {
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return 0, nil, -1, false
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			return 0, nil, -1, false
		}
		e.ctx.RemapWRRecordToDeclared(rec, createSQL, colDefs)
		if e.allMatch(colDefs, rec.Values, group, values) {
			return cell.RowID, rec.Values, group[0], true
		}
		hasNext, err := cursor.Next()
		if err != nil || !hasNext {
			return 0, nil, -1, false
		}
	}
}

// ipkRowidAliasConflict uses a direct rowid seek when the only unique column
// is an INTEGER PRIMARY KEY alias (its value IS the rowid).
func (e *DMLExecutor) ipkRowidAliasConflict(tableName string, rootPage uint32, colDefs []sql.ColumnDef, values []interface{}, idx int) (int64, []interface{}, int, bool) {
	cd := colDefs[idx]
	if !isIPKRowidAliasCol(cd) {
		return 0, nil, -1, false
	}
	v, ok := values[idx].(int64)
	if !ok {
		return 0, nil, -1, false
	}
	tree := e.uniqueScanTree(tableName, rootPage)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, nil, -1, false
	}
	found, err := cursor.SeekToRowID(v)
	if err != nil || !found {
		return 0, nil, -1, false
	}
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return 0, nil, -1, false
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return 0, nil, -1, false
	}
	return cell.RowID, rec.Values, idx, true
}

// scanForConflict iterates through all rows and looks for a value match
// on any of the given UNIQUE column indices. It returns the conflicting row's
// rowid, its values, and the column index that conflicted.

// scanForConflict iterates through all rows and looks for a value match
// on any of the given UNIQUE column indices. It returns the conflicting row's
// rowid, its values, and the column index that conflicted.
func (e *DMLExecutor) execInsertSelect(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) (ret *Result) {
	// The INSERT's WITH clause (CTEs) applies to its SELECT body. Push the
	// CTEs onto the scope stack so findCTE can resolve references like
	// "INSERT INTO t SELECT ... FROM c" where c is a WITH RECURSIVE CTE.
	if len(s.CTEs) > 0 {
		e.ctx.PushCTEScope(s.CTEs)
		defer e.ctx.PopCTEScope()
	}
	selectResult := e.ctx.ExecSelect(s.Select)
	if selectResult.Error != nil {
		return selectResult
	}
	if res := e.insertSelectGrowthGuard(selectResult); res != nil {
		return res
	}

	// Statement atomicity: REPLACE deletes rows and may fire triggers, and any
	// row may fail a constraint (e.g. CHECK) after earlier rows were already
	// written. If anything fails the whole statement must be rolled back
	// (SQLite statement journal), so snapshot the pager up front. INSERT OR
	// FAIL keeps the rows written before the conflict (SQLite ON CONFLICT
	// FAIL semantics) — the outer execRollbackOnError also skips the restore
	// for OR FAIL, so the snapshot is only rolled back on error for the
	// atomic modes (default/ABORT/REPLACE/ROLLBACK).
	snap := e.ctx.Pager().Snapshot()
	// Per-constraint ON CONFLICT FAIL (e.g. "a PRIMARY KEY ON CONFLICT FAIL")
	// aborts the statement but keeps rows written before the conflict, exactly
	// like statement-level INSERT OR FAIL (e_createtable-4.15/4.16/4.17 t*_fa).
	keepPriorRowsOnError := false
	// Snapshot the FTS in-memory indexes so a failed INSERT ... SELECT can
	// undo FTS writes the pager restore does not cover (the FTS store is
	// in-memory; fts3conf 4.1.1 rolls back a rowid-conflict INSERT SELECT).
	ftsSnaps := e.ftsSnapshots()
	defer e.rollbackInsertSelectOnError(snap, s.OrFail, &keepPriorRowsOnError, &ret, ftsSnaps)
	if res := e.insertSelectArityCheck(tableEntry, colDefs, s, selectResult); res != nil {
		return res
	}
	if res, handled := e.insertSelectFTSRoutes(tableEntry, colDefs, s, selectResult); handled {
		return res
	}

	// Build a column mapping for the INSERT column list.
	// Handle _rowid_ specially (maps to the implicit rowid, not a table column).
	// Handle duplicate column names by only using the first occurrence.
	colMapping := buildInsertColumnMapping(s.Columns, colDefs)

	changes, inserted, returningRows, res := e.insertSelectRows(tableEntry, colDefs, s, selectResult, colMapping, &keepPriorRowsOnError)
	if res != nil {
		return res
	}
	return e.insertSelectResult(s, colDefs, returningRows, changes, inserted)
}

// insertSelectRows runs the per-SELECT-row write loop: one progress check
// and one insertSelectOneRow per row (SQLITE_TEST interrupt countdown: one
// op per written row — src/vdbe.c per-opcode decrement of
// sqlite3_interrupt_count; interrupt-3.x aborts INSERT INTO t2 SELECT * FROM
// t1 mid-statement this way, which forces the transaction rollback via
// execRollbackOnError's special-error handling). keepPriorRowsOnError is set
// on a per-constraint ON CONFLICT FAIL; the deferred rollback honors it.
func (e *DMLExecutor) insertSelectRows(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, selectResult *Result, colMapping []int, keepPriorRowsOnError *bool) (changes, inserted int64, returningRows [][]interface{}, res *Result) {
	for _, row := range selectResult.Rows {
		if err := e.ctx.CheckProgress(); err != nil {
			return 0, 0, nil, &Result{Error: err}
		}
		skip, rv, r, ins := e.insertSelectOneRow(tableEntry, colDefs, s, row, colMapping)
		if r != nil {
			return 0, 0, nil, e.insertSelectConflictResult(r, tableEntry, colDefs, keepPriorRowsOnError)
		}
		if skip {
			continue
		}
		if rv != nil {
			returningRows = append(returningRows, rv)
		}
		changes++
		if ins {
			inserted++
		}
	}
	return changes, inserted, returningRows, nil
}

// insertSelectGrowthGuard validates the schema's table btrees only when the
// INSERT ... SELECT's write GROWS the database file (its allocation path
// reads the pointer-map/auto-vacuum state on growth). A write that fits in
// the existing free space performs no allocation and succeeds even when a
// table's btree is corrupt (fts3corrupt4 24.4: a 2-row insert fits, the
// oracle succeeds). A write that exceeds the free space grows the file and
// fails (24.1: a 19-row insert exceeds t1_content's free space, the oracle
// fails). A SELECT that produces no rows performs no write and is never
// validated (25.3).
func (e *DMLExecutor) insertSelectGrowthGuard(selectResult *Result) *Result {
	if len(selectResult.Rows) == 0 {
		return nil
	}
	est := int64(0)
	for _, row := range selectResult.Rows {
		for _, v := range row {
			switch tv := v.(type) {
			case []byte:
				est += int64(len(tv)) + 32
			case string:
				est += int64(len(tv)) + 32
			default:
				est += 24
			}
		}
	}
	if est > e.ctx.EstimateFreeSpace() {
		if err := e.ctx.ValidateAllTableRoots(); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

// ftsSnapshots snapshots every live FTS in-memory index.
func (e *DMLExecutor) ftsSnapshots() []ftsSnapshotPair {
	var ftsSnaps []ftsSnapshotPair
	for name, t := range e.ctx.FTSTables() {
		if t != nil {
			ftsSnaps = append(ftsSnaps, ftsSnapshotPair{name: name, table: t, state: t.Snapshot(), pending: t.PendingSnapshot()})
		}
	}
	return ftsSnaps
}

// insertSelectArityCheck checks the SELECT's column count against the INSERT:
// with an explicit column list the counts must match exactly; without one,
// SQLite accepts the SELECT when its column count matches EITHER the full
// table column count (positional mapping; generated columns absorb the
// SELECT values and are recomputed — gencol: INSERT INTO t1 SELECT * FROM t0
// where both tables have generated columns) OR the non-generated column
// count (mapping to the non-generated columns in order — strict1-8.1: a
// 2-column SELECT into (debit, credit, amount GENERATED)).
func (e *DMLExecutor) insertSelectArityCheck(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, selectResult *Result) *Result {
	expectedCount := insertSelectExpectedCount(s.Columns, colDefs)
	numSelectCols := len(selectResult.Columns)
	if numSelectCols == expectedCount {
		return nil
	}
	if len(s.Columns) == 0 && numSelectCols == nonGeneratedColumnCount(colDefs) {
		return nil
	}
	if len(s.Columns) > 0 {
		// With an explicit column list SQLite reports just the counts.
		return &Result{Error: fmt.Errorf("%d values for %d columns", numSelectCols, expectedCount)}
	}
	return &Result{Error: fmt.Errorf("table %s has %d columns but %d values were supplied",
		tableEntry.Name, expectedCount, numSelectCols)}
}

// insertSelectFTSRoutes dispatches FTS and fts5 targets: the SELECT result
// rows become FTS documents. handled=false continues to the ordinary row
// inserts.
func (e *DMLExecutor) insertSelectFTSRoutes(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, selectResult *Result) (*Result, bool) {
	// Route fts5 virtual table inserts through the fts5 engine: each SELECT
	// row becomes an fts5 document.
	if t5, ok := e.ctx.FTS5Tables()[tableEntry.Name]; ok {
		return e.insertSelectIntoFTS5(t5, tableEntry, colDefs, s, selectResult), true
	}
	// Route FTS virtual table inserts directly to the FTS table (same as
	// insertRow): the SELECT result rows become FTS documents.
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		return e.insertSelectIntoFTS(ftsTable, tableEntry, colDefs, s, selectResult), true
	}
	return nil, false
}

// insertSelectConflictResult applies the per-constraint conflict flags to a
// failed row's result: per-constraint ON CONFLICT FAIL keeps the rows
// written before the conflict, ROLLBACK marks a transaction rollback.
func (e *DMLExecutor) insertSelectConflictResult(res *Result, tableEntry *schema.Entry, colDefs []sql.ColumnDef, keepPriorRowsOnError *bool) *Result {
	if e.uniqueFailConflict(res.Error, tableEntry, colDefs) {
		*keepPriorRowsOnError = true
		res.SetKeepPriorRowsOnError()
	}
	if e.uniqueRollbackConflict(res.Error, tableEntry, colDefs) {
		res.SetRollbackTxOnError()
	}
	return res
}

// insertSelectResult builds the INSERT ... SELECT statement result, handling
// RETURNING projection and change counts.
func (e *DMLExecutor) insertSelectResult(s *sql.InsertStmt, colDefs []sql.ColumnDef, returningRows [][]interface{}, changes, inserted int64) *Result {
	if s.HasReturning {
		columns := e.ctx.BuildColumnNames([]sql.SelectColumn{s.Returning}, colDefs, nil)
		res := &Result{Columns: columns, Rows: returningRows}
		res.InsertedChanges = inserted
		return res
	}
	return &Result{Changes: changes, InsertedChanges: inserted, LastInsertRowID: e.ctx.LastRowID()}
}

// ftsSnapshotPair records an FTS table's in-memory index snapshot so a failed
// INSERT ... SELECT can restore it (the FTS store is in-memory; the pager
// restore covers only btree pages).
type ftsSnapshotPair struct {
	name    string
	table   *fts.FTS3Table
	state   *fts.InvertedIndex
	pending []int64
}

// rollbackInsertSelectOnError restores the pager and the FTS in-memory
// indexes when an INSERT ... SELECT statement failed partway through
// (statement atomicity). orFail keeps the rows written before the conflict
// (SQLite ON CONFLICT FAIL semantics: the failing row itself was never
// written, so earlier rows survive); keepPriorRowsOnError is the same for a
// per-constraint ON CONFLICT FAIL (e_createtable t*_fa tables).
func (e *DMLExecutor) rollbackInsertSelectOnError(snap *pager.PagerState, orFail bool, keepPriorRowsOnError *bool, ret **Result, ftsSnaps []ftsSnapshotPair) {
	if *ret != nil && (*ret).Error != nil && !orFail && !*keepPriorRowsOnError {
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		// The pager rollback can invalidate cached rowid counters (rows
		// whose rowids were computed for the aborted statement are gone).
		e.ctx.ResetNextRowIDCache()
		e.ctx.ResetAutoIncSeq()
		// Restore the FTS in-memory indexes captured at statement start.
		for _, fs := range ftsSnaps {
			if fs.table != nil {
				if fs.state != nil {
					fs.table.Restore(fs.state)
				}
				fs.table.RestorePending(fs.pending)
			}
		}
	}
}

// uniqueFailConflict reports whether a constraint error should keep prior
// rows because the violated constraint (column-level or table-level) carries
// ON CONFLICT FAIL. Handles UNIQUE/PRIMARY KEY and NOT NULL violations.
func (e *DMLExecutor) uniqueFailConflict(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	return e.uniqueConflictActionMatches(err, tableEntry, colDefs, "FAIL")
}

// uniqueRollbackConflict reports whether a constraint error must roll back
// the whole transaction because the violated constraint (column-level or
// table-level) carries ON CONFLICT ROLLBACK. Handles UNIQUE/PRIMARY KEY and
// NOT NULL violations.
func (e *DMLExecutor) uniqueRollbackConflict(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	return e.uniqueConflictActionMatches(err, tableEntry, colDefs, "ROLLBACK")
}

// uniqueConflictActionMatches reports whether the violated constraint
// (column-level or table-level) carries the given ON CONFLICT action.
func (e *DMLExecutor) uniqueConflictActionMatches(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef, action string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	isUnique := strings.Contains(msg, "UNIQUE constraint failed")
	isNotNull := strings.Contains(msg, "NOT NULL constraint failed")
	if !isUnique && !isNotNull {
		return false
	}
	if e.columnHasConflictAction(colDefs, action, msg, isUnique, isNotNull) {
		return true
	}
	return e.tableConstraintHasConflictAction(tableEntry, action, isUnique)
}

// columnHasConflictAction reports whether a column with the given ON
// CONFLICT action is violated: a UNIQUE conflict matches any column with the
// action, a NOT NULL conflict only the column the error names.
func (e *DMLExecutor) columnHasConflictAction(colDefs []sql.ColumnDef, action, msg string, isUnique, isNotNull bool) bool {
	for _, cd := range colDefs {
		if cd.OnConflict == action && (isUnique || (isNotNull && strings.HasSuffix(msg, "."+cd.Name))) {
			return true
		}
	}
	return false
}

// tableConstraintHasConflictAction reports whether a UNIQUE/PK table-level
// constraint carries the given ON CONFLICT action.
func (e *DMLExecutor) tableConstraintHasConflictAction(tableEntry *schema.Entry, action string, isUnique bool) bool {
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey) && tc.OnConflict == action && isUnique {
			return true
		}
	}
	return false
}

// insertSelectExpectedCount returns the number of values an INSERT ... SELECT
// must produce: the explicit column-list length, or the full column count.
func insertSelectExpectedCount(columns []string, colDefs []sql.ColumnDef) int {
	if len(columns) > 0 {
		return len(columns)
	}
	return len(colDefs)
}

// nonGeneratedColumnCount returns how many columns are not GENERATED and not
// hidden (hidden virtual-table columns are not positional-insert targets).
func nonGeneratedColumnCount(colDefs []sql.ColumnDef) int {
	n := 0
	for _, cd := range colDefs {
		if cd.Generated == nil && !execquery.IsHiddenColumnDef(cd) {
			n++
		}
	}
	return n
}

// buildInsertColumnMapping maps each INSERT column name to its colDefs index:
// -1 for _rowid_/rowid, -2 for duplicate names (skip), -3 for unknown columns.
func buildInsertColumnMapping(columns []string, colDefs []sql.ColumnDef) []int {
	if len(columns) == 0 {
		return nil
	}
	colMapping := make([]int, len(columns))
	seen := make(map[string]bool)
	for i, col := range columns {
		// FTS virtual tables also accept docid as the rowid alias
		// (fts3first.test: INSERT INTO x2(docid, a, b, c) SELECT ...).
		if strings.EqualFold(col, "_rowid_") || strings.EqualFold(col, "rowid") ||
			strings.EqualFold(col, "docid") {
			colMapping[i] = -1 // _rowid_ marker
			continue
		}
		if seen[col] {
			colMapping[i] = -2 // duplicate, skip
			continue
		}
		seen[col] = true
		found := false
		for j, cd := range colDefs {
			if strings.EqualFold(cd.Name, col) {
				colMapping[i] = j
				found = true
				break
			}
		}
		if !found {
			colMapping[i] = -3 // column not found in table
		}
	}
	return colMapping
}

// insertSelectOneRow handles a single SELECT-result row for INSERT ... SELECT:
// affinity conversion, generated columns, REPLACE deletes, rowid resolution,
// conflict checks, BEFORE triggers, and the physical insert. Returns a skip
// flag (IGNORE conflict), the RETURNING row (or nil), and an error Result.
func (e *DMLExecutor) insertSelectOneRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, row []interface{}, colMapping []int) (skip bool, rv []interface{}, res *Result, inserted bool) {
	values, explicitRowID, hasExplicitRowID := e.buildInsertSelectValues(row, s.Columns, colMapping, colDefs)
	applyInsertAffinity(colDefs, values)

	// Compute any generated columns (b AS(expr)) that were not explicitly set.
	if err := e.computeGeneratedValues(colDefs, values); err != nil {
		return false, nil, &Result{Error: err}, false
	}

	// Handle REPLACE: delete conflicting rows before inserting. The new
	// row's rowid is computed BEFORE the deletes (SQLite keeps the rowid
	// through the REPLACE retry, so a trigger may grab it and conflict).
	replaceRowID, res := e.insertSelectReplaceRowID(tableEntry, colDefs, values, s.IsReplace)
	if res != nil {
		return false, nil, res, false
	}

	// Determine rowID BEFORE constraint checks (CHECK(rowid<=5) needs it)
	rowID, ipkWasNil, ipkIndex := e.resolveInsertRowID(tableEntry, colDefs, values, explicitRowID, hasExplicitRowID, s.IsReplace, replaceRowID)

	// Validate constraints before inserting. With an ON CONFLICT (UPSERT)
	// clause, find conflicting rows first and apply the matching clause's
	// action; the normal constraint path handles OR IGNORE / REPLACE modes.
	if s.OnConflict != nil {
		if skip, rv, res, handled := e.selectRowUpsert(tableEntry, colDefs, s, values, rowID); handled {
			return skip, rv, res, false
		}
	}
	if res := e.execInsertSelectConflict(s, tableEntry, colDefs, values, rowID, s.OrIgnore); res != nil {
		if res.Error == errRowSkipped {
			return true, nil, nil, false
		}
		return false, nil, res, false
	}

	// A trigger may have inserted a row with our rowid during the
	// REPLACE's delete; report it as a rowid UNIQUE conflict.
	if res := e.replaceRowIDRecheck(tableEntry, colDefs, s.IsReplace, hasExplicitRowID, rowID); res != nil {
		return false, nil, res, false
	}

	// Fire BEFORE INSERT triggers — the row is not in the table yet.
	if skip, res := e.maybeFireInsertBeforeTriggers(tableEntry, colDefs, values, ipkWasNil, ipkIndex); skip {
		return true, nil, nil, false
	} else if res != nil {
		return false, nil, res, false
	}

	if res, rv := e.insertSelectWrittenRow(tableEntry, colDefs, values, rowID, s); res != nil {
		return false, nil, res, false
	} else if rv != nil {
		return false, rv, nil, true
	}
	e.ctx.SetLastRowID(rowID)
	return false, nil, nil, true
}

// replaceRowIDRecheck reports a rowid UNIQUE conflict when a REPLACE's
// BEFORE-trigger insert consumed the target rowid.
func (e *DMLExecutor) replaceRowIDRecheck(tableEntry *schema.Entry, colDefs []sql.ColumnDef, isReplace, hasExplicitRowID bool, rowID int64) *Result {
	if !isReplace || hasExplicitRowID {
		return nil
	}
	if e.rowIDExists(tableEntry.Name, tableEntry.RootPage, rowID) {
		return &Result{Error: e.rowIDConflictError(tableEntry, colDefs)}
	}
	return nil
}

// selectRowUpsert applies the ON CONFLICT clause for one INSERT ... SELECT
// row. Returns (skip, returning-row, result); skip=true for DO NOTHING,
// rv non-nil for a RETURNING projection of a DO UPDATE row, and res non-nil
// for a validation error or an unmatched conflict.
func (e *DMLExecutor) selectRowUpsert(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, values []interface{}, rowID int64) (skip bool, rv []interface{}, res *Result, handled bool) {
	if res := e.validateOnConflictTarget(s.OnConflict, tableEntry, colDefs); res != nil {
		return false, nil, res, true
	}
	if res := e.validateUpsertExpressions(tableEntry.Name, s.Alias, s.OnConflict); res != nil {
		return false, nil, res, true
	}
	colIndex := buildColumnIndex(colDefs)
	hits := e.findOnConflictRow(tableEntry, colDefs, colIndex, values)
	if len(hits) == 0 {
		return false, nil, nil, false
	}
	res = e.resolveUpsertConflicts(tableEntry, colDefs, colIndex, values, hits, s.OnConflict, s.Alias)
	if res == nil || res.Error == nil {
		// DO NOTHING / DO UPDATE handled the row: skip the normal insert;
		// RETURNING gets the updated row (Row).
		if res != nil && res.Row != nil {
			rv, rerr := e.evalInsertReturningRow(s, res.Row, colDefs, tableEntry.Name, rowID)
			if rerr != nil {
				return false, nil, &Result{Error: rerr}, false
			}
			return false, rv, nil, true
		}
		return true, nil, nil, true
	}
	return false, nil, res, true
}

// maybeFireInsertBeforeTriggers fires BEFORE INSERT triggers when the table
// has any, reporting whether a trigger suppressed the insert (INSERT OR
// IGNORE) or returned an error.
func (e *DMLExecutor) maybeFireInsertBeforeTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, ipkWasNil bool, ipkIndex int) (bool, *Result) {
	if !e.hasTriggersForTable(tableEntry.Name) {
		return false, nil
	}
	res, skip := e.fireBeforeInsertTriggersForRow(tableEntry, colDefs, values, ipkWasNil, ipkIndex)
	if skip {
		return true, nil
	}
	if res != nil {
		return false, res
	}
	return false, nil
}

// applyInsertAffinity applies each column's type affinity to the values.
func applyInsertAffinity(colDefs []sql.ColumnDef, values []interface{}) {
	for i, v := range values {
		if i < len(colDefs) {
			values[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
		}
	}
}

// insertSelectReplaceRowID computes the replacement rowid for REPLACE inserts
// and performs the conflict deletes. Returns nil Result when not REPLACE or on
// success.
func (e *DMLExecutor) insertSelectReplaceRowID(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, isReplace bool) (int64, *Result) {
	if !isReplace {
		return 0, nil
	}
	rr, err := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)))
	if err != nil {
		return 0, &Result{Error: err}
	}
	if res := e.replaceDeleteConflicts(e.ctx.Pager(), tableEntry, colDefs, values, rr); res.Error != nil {
		return 0, res
	}
	return rr, nil
}

// insertSelectWrittenRow encodes, inserts, indexes, and fires AFTER triggers
// for one written INSERT ... SELECT row, then evaluates RETURNING. Returns a
// non-nil Result on failure, and the RETURNING row (or nil) on success.
