package execdml

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"strings"
)

func (e *DMLExecutor) findRowByUniqueCols(tableName string, rootPage uint32, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) (int64, []interface{}, int, bool) {
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

	return scanForConflict(cursor, uniqueCols, values, colDefs)
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
		if rowID, vals, col, ok := e.scanGroupForMatch(cursor, colDefs, group, values); ok {
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

// scanGroupForMatch walks a cursor looking for a record matching all group
// columns against the inserted values.
func (e *DMLExecutor) scanGroupForMatch(cursor *btree.Cursor, colDefs []sql.ColumnDef, group []int, values []interface{}) (int64, []interface{}, int, bool) {
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return 0, nil, -1, false
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			return 0, nil, -1, false
		}
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
	// Real SQLite validates the schema's table btrees only when an INSERT ...
	// SELECT's write GROWS the database file (its allocation path reads the
	// pointer-map/auto-vacuum state on growth). A write that fits in the
	// existing free space performs no allocation and succeeds even when a
	// table's btree is corrupt (fts3corrupt4 24.4: a 2-row insert fits, the
	// oracle succeeds). A write that exceeds the free space grows the file
	// and fails (24.1: a 19-row insert exceeds t1_content's free space, the
	// oracle fails). A SELECT that produces no rows performs no write and is
	// never validated (25.3).
	if len(selectResult.Rows) > 0 {
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
	var ftsSnaps []ftsSnapshotPair
	for name, t := range e.ctx.FTSTables() {
		if t != nil {
			ftsSnaps = append(ftsSnaps, ftsSnapshotPair{name: name, table: t, state: t.Snapshot(), pending: t.PendingSnapshot()})
		}
	}
	defer e.rollbackInsertSelectOnError(snap, s.OrFail, &keepPriorRowsOnError, &ret, ftsSnaps)

	// Determine the effective number of columns we expect.
	// If specific columns are given in the INSERT, the SELECT
	// must produce that many values. With no column list, SQLite accepts the
	// SELECT when its column count matches EITHER the full table column count
	// (positional mapping; generated columns absorb the SELECT values and are
	// recomputed — gencol: INSERT INTO t1 SELECT * FROM t0 where both tables
	// have generated columns) OR the non-generated column count (mapping to
	// the non-generated columns in order — strict1-8.1: a 2-column SELECT
	// into (debit, credit, amount GENERATED)).
	expectedCount := insertSelectExpectedCount(s.Columns, colDefs)
	numSelectCols := len(selectResult.Columns)
	// With an explicit INSERT column list the SELECT must match that count.
	// Only a no-column-list INSERT may fall back to the non-generated column
	// count (generated columns absorb extra SELECT values).
	if numSelectCols != expectedCount && (len(s.Columns) > 0 || numSelectCols != nonGeneratedColumnCount(colDefs)) {
		if len(s.Columns) > 0 {
			// With an explicit column list SQLite reports just the counts.
			return &Result{Error: fmt.Errorf("%d values for %d columns", numSelectCols, expectedCount)}
		}
		return &Result{Error: fmt.Errorf("table %s has %d columns but %d values were supplied",
			tableEntry.Name, expectedCount, numSelectCols)}
	}

	// Route FTS virtual table inserts directly to the FTS table (same as
	// insertRow): the SELECT result rows become FTS documents.
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		return e.insertSelectIntoFTS(ftsTable, tableEntry, colDefs, s, selectResult)
	}

	// Build a column mapping for the INSERT column list.
	// Handle _rowid_ specially (maps to the implicit rowid, not a table column).
	// Handle duplicate column names by only using the first occurrence.
	colMapping := buildInsertColumnMapping(s.Columns, colDefs)

	var changes int64
	var inserted int64
	var returningRows [][]interface{}
	for _, row := range selectResult.Rows {
		// SQLITE_TEST interrupt countdown: one op per written row
		// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count);
		// interrupt-3.x aborts INSERT INTO t2 SELECT * FROM t1 mid-statement
		// this way, which forces the transaction rollback via
		// execRollbackOnError's special-error handling.
		if err := e.ctx.CheckProgress(); err != nil {
			return &Result{Error: err}
		}
		skip, rv, res, ins := e.insertSelectOneRow(tableEntry, colDefs, s, row, colMapping)
		if res != nil {
			if e.uniqueFailConflict(res.Error, tableEntry, colDefs) {
				keepPriorRowsOnError = true
				res.SetKeepPriorRowsOnError()
			}
			if e.uniqueRollbackConflict(res.Error, tableEntry, colDefs) {
				res.SetRollbackTxOnError()
			}
			return res
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
	return e.insertSelectResult(s, colDefs, returningRows, changes, inserted)
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
	if err == nil {
		return false
	}
	isUnique := strings.Contains(err.Error(), "UNIQUE constraint failed")
	isNotNull := strings.Contains(err.Error(), "NOT NULL constraint failed")
	if !isUnique && !isNotNull {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "FAIL" && (isUnique || (isNotNull && strings.HasSuffix(err.Error(), "."+cd.Name))) {
			return true
		}
	}
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey) && tc.OnConflict == "FAIL" && isUnique {
			return true
		}
	}
	return false
}

// uniqueRollbackConflict reports whether a constraint error must roll back
// the whole transaction because the violated constraint (column-level or
// table-level) carries ON CONFLICT ROLLBACK. Handles UNIQUE/PRIMARY KEY and
// NOT NULL violations.
func (e *DMLExecutor) uniqueRollbackConflict(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	if err == nil {
		return false
	}
	isUnique := strings.Contains(err.Error(), "UNIQUE constraint failed")
	isNotNull := strings.Contains(err.Error(), "NOT NULL constraint failed")
	if !isUnique && !isNotNull {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "ROLLBACK" && (isUnique || (isNotNull && strings.HasSuffix(err.Error(), "."+cd.Name))) {
			return true
		}
	}
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey) && tc.OnConflict == "ROLLBACK" && isUnique {
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

// insertSelectIntoFTS inserts SELECT result rows directly into an FTS table.
func (e *DMLExecutor) insertSelectIntoFTS(ftsTable *fts.FTS3Table, tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, selectResult *Result) *Result {
	// Writing to an FTS table whose shadow btrees are structurally corrupt
	// fails (fts3corrupt4 24.1: t1_segments page 4 free-space corruption).
	if res := e.ctx.ValidateFTSShadowRoots(tableEntry.Name); res != nil {
		return res
	}
	// A write that allocates pages on a DB with a corrupt freelist fails
	// (fts3corrupt4 29.1: an INSERT into t1 on an auto-vacuum DB whose
	// freelist/ptr-map trunk page is corrupt).
	if err := e.ctx.ValidateFreelistForGrowth(); err != nil {
		return &Result{Error: err}
	}
	// Build the column mapping so the SELECT's rowid column (an explicit
	// (rowid, x) INSERT list) is applied via InsertWithID and conflicts are
	// detected (fts3conf 1.$tn.6/8/10: INSERT OR * INTO t1(rowid, x)
	// SELECT * FROM source conflicts when the source rowid already exists).
	colMapping := buildInsertColumnMapping(s.Columns, colDefs)
	isReplace := strings.EqualFold(s.OrConflict, "REPLACE")
	// An INSERT ... SELECT into the FTS table-name column (INSERT INTO
	// t1(t1) SELECT x FROM t2) runs each SELECT value as a special command
	// (fts3corrupt4 24.7: x='optimize','rebuild',... — the rebuild fails on
	// a corrupt DB). Process them here before inserting as documents.
	if len(s.Columns) > 0 && strings.EqualFold(s.Columns[0], tableEntry.Name) {
		// Each SELECT value in the table-name column is a special command
		// (fts3corrupt4 24.7: x='optimize','rebuild',... — the rebuild fails
		// on a corrupt DB). Process them here before inserting as documents.
		for _, row := range selectResult.Rows {
			if len(row) == 0 {
				continue
			}
			cmdStr, ok := row[0].(string)
			if !ok {
				continue
			}
			special, res := e.handleFTSCommand(tableEntry.Name, cmdStr)
			if special {
				if res != nil && res.Error != nil {
					return res
				}
			}
		}
		// All rows were special commands (no documents to insert).
		return &Result{Changes: 0, LastInsertRowID: 0}
	}
	var changes int64
	e.lastFTSDocRowID = 0
	for _, row := range selectResult.Rows {
		values, explicitRowID, hasExplicitRowID := e.buildInsertSelectValues(row, s.Columns, colMapping, colDefs)
		// Re-map the values onto the FTS table's real columns: the values
		// array from buildInsertSelectValues is indexed by ParseColumnDefs
		// (which includes the hidden docid/table-name vtab columns), while
		// Insert/InsertWithID expect one value per ftsTable.ColumnNames()
		// (insertFTSRow applies the same trimming for VALUES inserts).
		colNames := ftsTable.ColumnNames()
		ftsValues := make([]interface{}, len(colNames))
		for i := range colNames {
			if i < len(values) {
				ftsValues[i] = values[i]
			} else {
				ftsValues[i] = ""
			}
		}
		// The FTS rowid: an explicit (rowid/docid) INSERT column wins;
		// otherwise the FTS module auto-assigns (Insert assigns 1..N).
		langID := int64(0)
		if langCol := ftsTable.LangIDColName(); langCol != "" {
			if lv := ftsLangIDFromValues(ftsTable, values, langCol); lv != nil {
				langID = sqlValueToInt64(lv)
			}
			if langID < 0 {
				return &Result{Error: fmt.Errorf("constraint failed")}
			}
		}
		if !hasExplicitRowID {
			var nextRowID int64
			if langCol := ftsTable.LangIDColName(); langCol != "" {
				nextRowID = ftsTable.InsertLangID(ftsValues, langID)
			} else {
				nextRowID = ftsTable.Insert(ftsValues)
			}
			e.ctx.SetLastRowID(nextRowID)
			changes++
			ftsTable.RecordPending(nextRowID)
			if res := e.writeFTSContentRow(tableEntry.Name, nextRowID, ftsValues, ftsTable.CompressFn(), ftsTable, langID); res != nil {
				return res
			}
			if res := e.writeFTSDocsizeRow(tableEntry.Name, nextRowID, ftsTable); res != nil {
				return res
			}
			e.lastFTSDocRowID = nextRowID
			continue
		}
		// Explicit rowid: enforce the docid UNIQUE constraint like
		// insertFTSRow (fts3.c fts3UpdateMethod).
		if ftsTable.HasDoc(explicitRowID) && !isReplace {
			if strings.EqualFold(s.OrConflict, "IGNORE") {
				continue
			}
			return &Result{Error: fmt.Errorf("UNIQUE constraint failed: %s.rowid", tableEntry.Name)}
		}
		if ftsTable.HasDoc(explicitRowID) {
			ftsTable.Delete(explicitRowID)
		}
		if langCol := ftsTable.LangIDColName(); langCol != "" {
			ftsTable.InsertWithIDLangID(explicitRowID, ftsValues, langID)
		} else {
			ftsTable.InsertWithID(explicitRowID, ftsValues)
		}
		e.ctx.SetLastRowID(explicitRowID)
		changes++
		// SQLite's xUpdate records every inserted docid as pending, also
		// for an OR REPLACE that deleted a flushed row (delete-marker
		// segments are handled by DeletedFlush).
		ftsTable.RecordPending(explicitRowID)
		if res := e.writeFTSContentRow(tableEntry.Name, explicitRowID, ftsValues, ftsTable.CompressFn(), ftsTable, langID); res != nil {
			return res
		}
		if res := e.writeFTSDocsizeRow(tableEntry.Name, explicitRowID, ftsTable); res != nil {
			return res
		}
		e.lastFTSDocRowID = explicitRowID
	}
	if changes > 0 {
		// fts3UpdateDocTotals runs at the end of xUpdate (fts3conf 3.2:
		// matchinfo 'na' after a REPLACE INTO ... SELECT path).
		e.ctx.WriteFTSStat(tableEntry.Name)
	}
	return &Result{Changes: changes, LastInsertRowID: e.lastInsertedFTSRowID()}
}

// lastInsertedFTSRowID re-asserts the connection's last_insert_rowid after
// an FTS insert-select: the loop's internal %_content/%_docsize/%_stat
// shadow writes run through nested Exec calls whose results clobber
// lastRowID (a %_stat REPLACE at id=0 sets it to 0). SQLite's OP_VUpdate
// stores the module's final rowid AFTER its internal writes; the engine
// mirrors that by restoring the last DOCUMENT rowid here.
func (e *DMLExecutor) lastInsertedFTSRowID() int64 {
	if id := e.lastFTSDocRowID; id != 0 {
		e.ctx.SetLastRowID(id)
		return id
	}
	return e.ctx.LastRowID()
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

// viewDeclaredColumns returns the explicit column list from a CREATE VIEW
// declaration (CREATE VIEW v(a,b) AS ...). Returns nil when the view has no
// declared column list.
// execInsertView handles INSERT statements whose target is a view. SQLite
// routes such statements through INSTEAD OF triggers; resolving the view's
// columns (which validates collations in its SELECT) happens first.
func (e *DMLExecutor) execInsertView(s *sql.InsertStmt, viewEntry *schema.Entry) *Result {
	// Qualified view column references (main.v5.b) must resolve against the
	// view row during trigger NEW-row evaluation.
	prevDML := e.currentDMLTable
	e.currentDMLTable = viewEntry.Name
	defer func() { e.currentDMLTable = prevDML }()

	// Resolve the view definition: parse and validate its SELECT expressions.
	// This surfaces errors like "no such collation sequence: X" at insert time.
	viewSelect, err := e.viewSelectFromEntry(viewEntry)
	if err != nil {
		return &Result{Error: err}
	}
	if viewSelect != nil {
		if err := e.validateCollationsInSelect(viewSelect); err != nil {
			return &Result{Error: err}
		}
	}

	// Views only accept INSERT when an INSTEAD OF INSERT trigger exists.
	if !e.hasTriggersForTable(viewEntry.Name) {
		return &Result{Error: fmt.Errorf("cannot modify %s because it is a view", viewEntry.Name)}
	}

	// Build the NEW row: map the INSERTed values to the view's output column
	// names so trigger bodies can reference NEW.col. Column names come from
	// the view's SELECT (aliases when present, else expression text), with
	// bare "*" expanded through the view's FROM source.
	viewCols := e.viewColumnNames(viewSelect)
	// A declared column list (CREATE VIEW v(a,b) AS ...) overrides the
	// SELECT-derived names for NEW-row mapping.
	if decl := e.viewDeclaredColumns(viewEntry); len(decl) > 0 {
		viewCols = decl
	}

	// INSERT ... SELECT into a view: each produced row fires the trigger.
	if s.Select != nil {
		return e.insertViewSelect(s, viewEntry, viewCols)
	}

	// Multi-row VALUES: fire the trigger once per tuple.
	if len(s.Values) > 0 {
		return e.insertViewValues(s, viewEntry, viewCols)
	}

	// DEFAULT VALUES into a view: fire once with an empty NEW row.
	if res := e.fireViewInsertRow(viewEntry, viewNewRow(nil, s.Columns, viewCols)); res != nil {
		return res
	}
	return &Result{Changes: 1}
}

// insertViewSelect fires INSTEAD OF INSERT triggers for each row produced by
// an INSERT ... SELECT whose target is a view. The view insert itself counts
// 0 changes (SQLite: "Changes to a view that are intercepted by INSTEAD OF
// triggers are not counted"); the trigger body's DML counts via its own Exec.
func (e *DMLExecutor) insertViewSelect(s *sql.InsertStmt, viewEntry *schema.Entry, viewCols []string) *Result {
	selResult := e.ctx.ExecSelect(s.Select)
	if selResult.Error != nil {
		return selResult
	}
	for _, rrow := range selResult.Rows {
		if res := e.fireViewInsertRow(viewEntry, viewNewRow(rrow, s.Columns, viewCols)); res != nil {
			return res
		}
	}
	return &Result{}
}

// insertViewValues fires INSTEAD OF INSERT triggers for each tuple of an
// INSERT ... VALUES whose target is a view. The view insert itself counts 0
// changes (the trigger body's DML counts via its own Exec).
func (e *DMLExecutor) insertViewValues(s *sql.InsertStmt, viewEntry *schema.Entry, viewCols []string) *Result {
	for _, tuple := range s.Values {
		values, evalErr := e.evalTuple(viewEntry.Name, tuple, s.Columns, nil)
		if evalErr != nil {
			return &Result{Error: evalErr}
		}
		if res := e.fireViewInsertRow(viewEntry, viewNewRow(values, s.Columns, viewCols)); res != nil {
			return res
		}
	}
	return &Result{}
}

// viewSelectFromEntry parses a view's stored SQL and returns its SELECT.
func (e *DMLExecutor) viewSelectFromEntry(viewEntry *schema.Entry) (*sql.SelectStmt, error) {
	stmts, perr := parse.ParseSQL(viewEntry.SQL)
	if perr != nil {
		return nil, perr
	}
	for _, st := range stmts {
		if c, ok := st.(*sql.CreateViewStmt); ok {
			return c.Select, nil
		}
	}
	return nil, nil
}

// fireViewInsertRow fires INSTEAD OF INSERT triggers for one NEW row.
func (e *DMLExecutor) fireViewInsertRow(viewEntry *schema.Entry, row RowMap) *Result {
	if res := e.fireTriggers(viewEntry.Name, "INSERT", "INSTEAD", row, nil); res != nil && res.Error != nil {
		return res
	}
	return nil
}

// viewNewRow builds the NEW row map for a view INSERT from a value tuple,
// mapping by explicit column list when given, else by view column order.
func viewNewRow(values []interface{}, columns []string, viewCols []string) RowMap {
	row := make(RowMap)
	row["rowid"] = nil
	if len(columns) > 0 {
		for i, col := range columns {
			if i < len(values) {
				row[col] = values[i]
			}
		}
	} else {
		for i, val := range values {
			if i < len(viewCols) {
				row[viewCols[i]] = val
			}
		}
	}
	return row
}

// viewColumnNames returns the output column names of a view's SELECT: the
// explicit alias when present, otherwise the column reference name or the
// expression text. A bare "*" is expanded through the FROM source (SQLite
// resolves view output columns the same way as the result columns of a plain
// SELECT). For a compound SELECT the head member determines the output names.

// validateCollationsInExpr verifies COLLATE operators in an expression tree.
