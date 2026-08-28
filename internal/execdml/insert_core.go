// Package execdml implements DML execution.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

func (e *DMLExecutor) execInsert(s *sql.InsertStmt) (ret *Result) {
	// Generic updatable virtual tables (sqlite_dbpage etc.): INSERT routes to
	// the module's InsertRow (xUpdate parity).
	if res, handled := e.execVTabInsert(s); handled {
		return res
	}
	if res := e.prepareInsertStmt(s); res != nil {
		return res
	}
	tableEntry, dbCtx, err := e.ctx.FindTable(s.Table)
	if err != nil {
		// Not a table — fall back to INSTEAD-OF-trigger view insert support.
		viewEntry, _, viewErr := e.ctx.FindView(s.Table)
		if viewErr != nil {
			return &Result{Error: err}
		}
		return e.execInsertView(s, viewEntry)
	}

	// Track the modified table's database context so trigger firing resolves
	// triggers in the same context (main vs temp shadowing).
	prevDMLCtx := e.currentDMLCtx
	e.currentDMLCtx = dbCtx
	defer func() { e.currentDMLCtx = prevDMLCtx }()

	// Protect system and pragma virtual tables from modification.
	if e.ctx.IsNonModifiableTable(tableEntry) {
		return &Result{Error: fmt.Errorf("table %s may not be modified", tableEntry.Name)}
	}

	// Direct modification of sqlite_sequence changes the AUTOINCREMENT
	// sequences: clear the in-memory sequence cache so the next INSERT reads
	// the real table fresh (SQLite reads sqlite_sequence at statement start).
	if isSQLiteSequenceName(tableEntry.Name) {
		defer e.ctx.ResetAutoIncSeq()
	}

	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// SQLite's autoIncrementEnd (insert.c) writes the AUTOINCREMENT sequence
	// back to sqlite_sequence at statement end. This mirrors that: after a
	// successful INSERT on an AUTOINCREMENT table (directly or via triggers),
	// write the running max back to the real sqlite_sequence table. The
	// write is skipped for empty statements that do not touch the table.
	if e.ctx.TableHasAutoIncrement(tableEntry.Name) && dbCtx != nil {
		seqTable := tableEntry.Name
		seqPg := dbCtx.Pager
		seqRoot := tableEntry.RootPage
		defer func() {
			if ret != nil && ret.Error != nil {
				return
			}
			seq, ok := e.ctx.AutoIncSeqFor(seqPg, seqRoot)
			if !ok {
				seq = 0
			}
			_ = e.ctx.WriteSQLiteSequence(seqPg, seqTable, seq)
		}()
	}

	if s.HasReturning {
		if res := e.validateInsertReturning(s, colDefs, tableEntry.Name); res != nil {
			return res
		}
	}

	// Virtual tables without module-backed storage (rtree, echo, dbstat, ...)
	// accept INSERT as a no-op success; RETURNING projects NULLs for every
	// column. FTS tables are handled by their dedicated paths.
	if e.ctx.IsStoragelessVirtualTable(tableEntry) {
		return e.execStoragelessInsert(tableEntry, colDefs, s)
	}

	if s.Select != nil {
		return e.execInsertSelect(tableEntry, colDefs, s)
	}
	if len(s.Values) == 0 {
		return e.execInsertDefault(tableEntry, colDefs, s)
	}

	// REPLACE deletes rows and may fire triggers before inserting; if anything
	// fails the whole statement must be rolled back (SQLite statement journal).
	defer e.withInsertReplaceSnapshot(dbCtx, s, &ret)()

	return e.execInsertTuples(dbCtx, tableEntry, colDefs, s)
}

// validateInsertReturning validates the RETURNING clause against the table's
// column definitions.

// validateInsertReturning validates the RETURNING clause against the table's
// column definitions.

// strictCheckValues enforces STRICT table type checking on non-generated
// column values.
// resolveInsertRowConstraints substitutes REPLACE defaults, computes generated
// columns, and validates constraints for a single row. Returns a non-nil
// Result on failure, and write=true when the row may be written (constraints
// passed or resolved).
// strictCheckValues enforces STRICT table type checking on non-generated
// column values.
// resolveInsertRowConstraints substitutes REPLACE defaults, computes generated
// columns, and validates constraints for a single row. Returns a non-nil
// Result on failure, and write=true when the row may be written (constraints
// passed or resolved).
func (e *DMLExecutor) resolveInsertRowConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64, orConflict string) (*Result, bool) {
	// For statement-level REPLACE, substitute the DEFAULT for NULL values in
	// NOT NULL columns BEFORE computing generated columns: SQLite replaces the
	// NULLs first (so a generated column fed by them is non-NULL). This mirrors
	// the OP_IsNull + ON CONFLICT REPLACE resolution for REPLACE.
	if strings.EqualFold(orConflict, "REPLACE") {
		e.fillReplaceNullDefaults(colDefs, values)
	}

	// Compute any generated columns (b AS(expr)) that were not explicitly set.
	// This must run BEFORE the constraint checks so NOT NULL / CHECK / UNIQUE
	// constraints see the computed values (e.g. m INT AS (a*2) NOT NULL).
	if err := e.computeGeneratedValues(colDefs, values); err != nil {
		return &Result{Error: err}, false
	}

	if err := e.checkConstraints(tableEntry, colDefs, values, nextRowID); err != nil {
		// Per-constraint ON CONFLICT only applies when the statement has no
		// explicit OR clause — a statement-level OR overrides the per-constraint
		// algorithm (verified against sqlite3 3.51: UNIQUE ON CONFLICT IGNORE
		// + INSERT OR ABORT errors; UNIQUE ON CONFLICT ABORT + INSERT OR
		// IGNORE skips). The caller applies the statement OR when present.
		if orConflict != "" && !strings.EqualFold(orConflict, "REPLACE") {
			return &Result{Error: err}, false
		}
		// Column-level ON CONFLICT IGNORE: silence UNIQUE constraint violations
		if e.isIgnoreableConflict(err, tableEntry, colDefs) {
			return nil, false
		}
		// A UNIQUE/PRIMARY KEY conflict on a constraint with ON CONFLICT
		// REPLACE: delete the conflicting rows and let the insert proceed
		// (SQLite replaces the old rows with the new one, e_createtable
		// -4.15/4.16/4.17 t*_re tables).
		if e.uniqueReplaceableConflict(err, tableEntry, colDefs) {
			if res := e.replaceDeleteConflicts(e.ctx.Pager(), tableEntry, colDefs, values, nextRowID); res.Error != nil {
				return res, false
			}
			return nil, true
		}
		// Column-level ON CONFLICT REPLACE on a NOT NULL column: substitute the
		// column's DEFAULT value for the NULL and re-validate (SQLite conflate.c
		// OP_IsNull + ON CONFLICT REPLACE resolution). Without a DEFAULT the
		// constraint error stands. REPLACE (statement OR) does the same.
		return e.resolveReplaceNotNullDefaults(tableEntry, colDefs, values, nextRowID, orConflict, err)
	}
	return nil, true
}

// uniqueReplaceableConflict reports whether a UNIQUE/PRIMARY KEY constraint
// error should resolve as REPLACE because the violated constraint (column-level
// or table-level) carries ON CONFLICT REPLACE.
func (e *DMLExecutor) uniqueReplaceableConflict(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "REPLACE" {
			return true
		}
	}
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey) && tc.OnConflict == "REPLACE" {
			return true
		}
	}
	return false
}

// fillReplaceNullDefaults substitutes the DEFAULT for NULL values in NOT NULL
// columns BEFORE computing generated columns (SQLite REPLACE semantics).

// fillReplaceNullDefaults substitutes the DEFAULT for NULL values in NOT NULL
// columns BEFORE computing generated columns (SQLite REPLACE semantics).

// fireInsertRowBeforeTriggers fires BEFORE INSERT triggers for a row about to
// be written, re-allocating the rowid when the triggers consumed the
// pre-computed one. Returns a non-nil Result on trigger failure (errRowSkipped
// for RAISE(IGNORE)).
// fireInsertRowBeforeTriggers fires BEFORE INSERT triggers for a row about to
// be written, re-allocating the rowid when the triggers consumed the
// pre-computed one. Returns a non-nil Result on trigger failure (errRowSkipped
// for RAISE(IGNORE)).
func (e *DMLExecutor) insertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64, orConflict string) *Result {
	// Route FTS virtual table inserts directly to the FTS table.
	if res := e.insertFTSRow(tableEntry, values, fixedRowID, orConflict); res != nil {
		return res
	}

	// Snapshot the pager so a statement-end FOREIGN KEY failure (checked
	// after AFTER triggers, SQLite checks immediate FKs at statement end) can
	// roll back the row, index entries, and any trigger side effects. Skip the
	// snapshot for the FTS flush's internal shadow-table writes: they are part
	// of the enclosing statement's rollback scope, and copying the whole
	// pager (which holds the growing %_segments blocks) per block insert is
	// O(n^2) across the automerge's many flushes (fts4merge4 2.2.x).
	var snap *pager.PagerState
	if !e.ctx.InFTSFlush() {
		snap = pg.Snapshot()
	}

	nextRowID, res := e.prepareInsertRowValues(tableEntry, colDefs, values, fixedRowID, orConflict)
	if res != nil {
		return res
	}

	// Unwrap collation wrappers (a trigger body may pass a column value
	// wrapped with its collation) so only raw values are stored.
	unwrapCollationWrappers(values)

	tree, res := e.writeTableRow(pg, tableEntry, colDefs, values, nextRowID)
	if res != nil {
		return res
	}

	// Fire the preupdate hook (sqlite3_preupdate_hook) with the new row's
	// values. WITHOUT ROWID tables report rowid 0 (SQLite uses the key
	// columns instead); rowid tables report the assigned rowid.
	rowID := nextRowID
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		rowID = 0
	}
	if res := e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "INSERT",
		DB:    e.schemaNameForPager(pg),
		Table: tableEntry.Name,
		RowID: rowID, RowID2: rowID,
		RowidTable: !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)),
		Old:        nil,
		New:        append([]interface{}(nil), values...),
	}); res != nil {
		return res
	}

	// Maintain indexes: evaluate partial predicates and expression keys in a
	// pure context (a non-deterministic date function raises SQLite's
	// "non-deterministic use of ... in an index" error) and write the new
	// row's index entries. On failure the just-written table row is removed
	// (SQLite rolls the whole statement back).
	if err := e.maintainIndexesOnInsert(tableEntry, colDefs, values, nextRowID); err != nil {
		if _, derr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == nextRowID
		}); derr == nil {
			e.ctx.InvalidateRowIDCache(pg, tableEntry.RootPage)
		}
		return &Result{Error: err}
	}

	// Fire AFTER INSERT triggers — but only if triggers exist for this table.
	if res := e.fireAfterInsertRowTriggers(tableEntry, colDefs, values, nextRowID); res != nil {
		return res
	}

	// Enforce FOREIGN KEY constraints at statement end (only when PRAGMA
	// foreign_keys is ON). The check runs after the AFTER triggers so a
	// trigger may repair the violation (e_fkey-31.3). On failure the whole
	// statement is rolled back to the pre-insert snapshot.
	if e.ctx.ForeignKeys() {
		if res := e.ctx.CheckForeignKeyViolations(tableEntry, colDefs, values, 0); res.Error != nil {
			e.ctx.RestorePager(pg, snap)
			e.ctx.InvalidateRowIDCache(pg, tableEntry.RootPage)
			return res
		}
	}
	return &Result{Changes: 1, LastInsertRowID: nextRowID}
}

// insertFTSRow routes a row insert to an FTS virtual table, or returns nil
// when the table is not FTS-backed.

// insertFTSRow routes a row insert to an FTS virtual table, or returns nil
// when the table is not FTS-backed.

// hasTriggersForTable returns true if any AFTER INSERT/UPDATE/DELETE triggers
// exist for the given table across all databases. This is a fast check to avoid
// building trigger row maps when no triggers are registered.
// checkConstraints validates NOT NULL, CHECK, UNIQUE, and PRIMARY KEY
// constraints for a row being inserted.
// hasTriggersForTable returns true if any AFTER INSERT/UPDATE/DELETE triggers
// exist for the given table across all databases. This is a fast check to avoid
// building trigger row maps when no triggers are registered.
// checkConstraints validates NOT NULL, CHECK, UNIQUE, and PRIMARY KEY
// constraints for a row being inserted.
func (e *DMLExecutor) checkConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, rowID int64) error {
	// Fast path: if there are no constraints at all, skip allocation entirely
	if !e.hasInsertConstraints(tableEntry, colDefs) {
		return nil
	}

	// Set the DML table context so table-qualified column references inside
	// CHECK/default expressions (e.g. CHECK (5 IN (false.false))) resolve
	// against this row's unqualified column keys.
	prevDML := e.currentDMLTable
	e.currentDMLTable = tableEntry.Name
	defer func() { e.currentDMLTable = prevDML }()

	row := buildRowMapFromValues(values, colDefs, rowID)
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))

	if err := e.checkColumnConstraints(tableEntry, colDefs, values, row, withoutRowid); err != nil {
		return err
	}

	// UNIQUE and PRIMARY KEY uniqueness check
	if err := e.checkUniqueConstraints(tableEntry, colDefs, values); err != nil {
		return err
	}

	return e.checkTableLevelCheckConstraints(tableEntry, colDefs, row)
}

// hasInsertConstraints reports whether the table imposes any constraints at
// all: column-level NOT NULL/CHECK/PRIMARY KEY/UNIQUE, UNIQUE indexes, or
// table-level constraints.

// hasInsertConstraints reports whether the table imposes any constraints at
// all: column-level NOT NULL/CHECK/PRIMARY KEY/UNIQUE, UNIQUE indexes, or
// table-level constraints.

// checkUniqueConstraints validates UNIQUE and PRIMARY KEY constraints by scanning
// for existing rows with the same values on UNIQUE or PRIMARY KEY columns.
// maintainIndexesOnInsert writes the new row's entries into every index on
// the table. Partial-index predicates and expression keys are evaluated in a
// pure context, so a non-deterministic date/time function (e.g. date('now'))
// in an index expression raises SQLite's "non-deterministic use of %s() in an
// index" error, matching OP_PureFunc semantics.
// checkUniqueConstraints validates UNIQUE and PRIMARY KEY constraints by scanning
// for existing rows with the same values on UNIQUE or PRIMARY KEY columns.
// maintainIndexesOnInsert writes the new row's entries into every index on
// the table. Partial-index predicates and expression keys are evaluated in a
// pure context, so a non-deterministic date/time function (e.g. date('now'))
// in an index expression raises SQLite's "non-deterministic use of %s() in an
// index" error, matching OP_PureFunc semantics.
func (e *DMLExecutor) maintainIndexesOnInsert(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, rowID int64) error {
	defs := e.allTableIndexes(tableEntry.Name)
	if len(defs) == 0 {
		return nil
	}
	colIndex := buildColumnIndex(colDefs)
	row := buildRowMapFromValues(values, colDefs, rowID)
	for _, def := range defs {
		// Evaluate the partial predicate in a pure context: SQLite evaluates
		// index expressions with OP_PureFunc, so 'now'/'localtime'/'utc' in
		// a partial-index WHERE raise the non-determinism error.
		inIndex, werr := e.indexRowIncluded(def, row)
		if werr != nil {
			return werr
		}
		if !inIndex {
			continue
		}
		indexValues, kerr := e.indexKeyValuesForRow(def, colDefs, colIndex, values, row)
		if kerr != nil {
			return kerr
		}
		if err := e.writeIndexCell(def, append(indexValues, rowID)); err != nil {
			return err
		}
	}
	return nil
}

// indexRowIncluded evaluates a partial-index predicate for one row in a pure
// context. Indexes without a WHERE include every row.

// indexRowIncluded evaluates a partial-index predicate for one row in a pure
// context. Indexes without a WHERE include every row.

// parseWhereExpr parses a standalone expression string into a sql.Expr.
// checkUniqueIndex scans the table for a row whose values match the new row
// on all columns of a UNIQUE index. Returns a SQLite-style error on conflict.
// NULL values never conflict (SQL UNIQUE allows multiple NULLs).
// parseWhereExpr parses a standalone expression string into a sql.Expr.
// checkUniqueIndexExcluding scans the table for a row whose values match the
// new row on all columns of a UNIQUE index, optionally excluding a rowid.
// Returns a SQLite-style error on conflict. NULL values never conflict.
func (e *DMLExecutor) checkUniqueIndexExcluding(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, def uniqueIndexDef, excludeRowID int64, haveExclude bool) error {
	colIndex := buildColumnIndex(colDefs)
	// The new row must itself satisfy the partial-index predicate to be in
	// the index; otherwise it cannot conflict via this index.
	row := buildRowMapFromValues(values, colDefs, 0)
	if inIndex, _ := e.evalIndexWhere(def.Where, row); !inIndex {
		return nil
	}
	idxCols := def.Cols
	key := make([]interface{}, len(idxCols))
	for i, cn := range idxCols {
		kv, ok := e.indexKeyValue(cn, colDefs, colIndex, values, row)
		if !ok {
			return nil
		}
		key[i] = kv
	}
	cell, _, _ := e.scanTableForMatch(tableEntry, func(rc *storage.Record, cl *storage.Cell) bool {
		if haveExclude && cl.RowID == excludeRowID {
			return false
		}
		return e.rowMatchesIndexKey(rc, cl, colDefs, colIndex, idxCols, key, def)
	})
	if cell == nil {
		return nil
	}
	return uniqueIndexConflictError(tableEntry, colIndex, def, idxCols)
}

// uniqueIndexConflictError builds the SQLite-style UNIQUE conflict message for
// an index: expression keys report the index name, plain column keys list the
// columns.

// uniqueIndexConflictError builds the SQLite-style UNIQUE conflict message for
// an index: expression keys report the index name, plain column keys list the
// columns.

// findRowByIndexCols finds a row that matches the given values on every column
// of the named UNIQUE index. Returns its rowid, values, and true if found.
// replaceDeleteConflicts deletes every row that conflicts with the new values
// on a UNIQUE/PRIMARY KEY column, a UNIQUE index, or the explicit rowid
// (replaceRowID), firing BEFORE and AFTER DELETE triggers for each deleted row
// (SQLite REPLACE semantics).
// findRowByIndexCols finds a row that matches the given values on every column
// of the named UNIQUE index. Returns its rowid, values, and true if found.
// replaceDeleteConflicts deletes every row that conflicts with the new values
// on a UNIQUE/PRIMARY KEY column, a UNIQUE index, or the explicit rowid
// (replaceRowID), firing BEFORE and AFTER DELETE triggers for each deleted row
// (SQLite REPLACE semantics).
func (e *DMLExecutor) replaceDeleteConflicts(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, replaceRowID int64) *Result {
	colIndex := buildColumnIndex(colDefs)
	// Collect ALL currently-conflicting rows BEFORE firing any triggers.
	// Rows inserted by triggers during the deletes are NOT re-deleted; if
	// they conflict with the new row, the subsequent INSERT reports the
	// UNIQUE/CHECK error (matching SQLite, which does not loop over
	// trigger-inserted rows).
	conflicts, conflictValueMap := e.collectReplaceConflicts(pg, tableEntry, colDefs, colIndex, values, replaceRowID)

	hasTriggers := e.hasTriggersForTable(tableEntry.Name)
	tree := e.ctx.TableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, true)
	for _, conflictRowID := range conflicts {
		if res := e.deleteReplaceConflictRow(tree, tableEntry, colDefs, conflictRowID, conflictValueMap[conflictRowID], hasTriggers); res != nil {
			return res
		}
	}
	return &Result{}
}

// collectReplaceConflicts gathers every row conflicting with the new values:
// the explicit rowid, UNIQUE/PRIMARY KEY columns, and UNIQUE indexes.

// collectReplaceConflicts gathers every row conflicting with the new values:
// the explicit rowid, UNIQUE/PRIMARY KEY columns, and UNIQUE indexes.

// buildRowMapFromValues creates a column-name-to-value map from a values slice.
// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.
// buildRowMapFromValues creates a column-name-to-value map from a values slice.
// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.
func (e *DMLExecutor) execInsertOnConflict(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, s *sql.InsertStmt) *Result {
	// Apply column affinity to the attempted VALUES row BEFORE conflict
	// detection and before exposing it through the "excluded" pseudo-table:
	// SQLite's upsert uses the affinity-applied row (e.g. a REAL column
	// coerces 22 → 22.0 in excluded.c).
	applyInsertAffinity(colDefs, values)

	// Validate every ON CONFLICT clause's target: it must exist in the table
	// and match a PRIMARY KEY or UNIQUE constraint (SQLite raises "no such
	// column" / "ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE
	// constraint" at prepare time).
	if res := e.validateOnConflictTarget(s.OnConflict, tableEntry, colDefs); res != nil {
		return res
	}
	// Validate DO UPDATE expressions: a table-qualified column reference must
	// name the target table or its alias ("excluded" is always valid).
	if res := e.validateUpsertExpressions(tableEntry.Name, s.Alias, s.OnConflict); res != nil {
		return res
	}

	// Fire BEFORE INSERT triggers for the attempted row (SQLite fires them
	// for an UPSERT row before conflict resolution; a RAISE(IGNORE) skips
	// the row). The rowid is computed first so triggers see new.rowid.
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	nextRowID, rerr := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, withoutRowid)
	if rerr != nil {
		return &Result{Error: rerr}
	}
	ipkWasNil, ipkIndex := e.fillIPKRowID(colDefs, values, nextRowID, withoutRowid, isStrictTable(tableEntry.SQL))
	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := buildBeforeTriggerRow(colDefs, values, ipkWasNil, ipkIndex, withoutRowid)
		if trigResult := e.fireBeforeInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
			if trigResult.Error == errRaiseIgnore {
				return &Result{Changes: 0, Row: nil}
			}
			return trigResult
		}
	}

	// Try to find existing conflicting rows (via UNIQUE columns, composite
	// constraints, and UNIQUE indexes).
	colIndex := buildColumnIndex(colDefs)
	hits := e.findOnConflictRow(tableEntry, colDefs, colIndex, values)

	if len(hits) == 0 {
		res := e.insertRow(pg, tableEntry, colDefs, values, nil, s.OrConflict)
		if res.Error != nil {
			return res
		}
		// insertRow mutates values in place (rowid fill, affinity, generated
		// columns), so values holds the row that was actually written.
		res.Row = values
		res.InsertedChanges = res.Changes
		return res
	}

	// Walk the chained ON CONFLICT clauses in statement order; the first
	// whose target matches the conflict source applies.
	res := e.resolveUpsertConflicts(tableEntry, colDefs, colIndex, values, hits, s.OnConflict, s.Alias)
	if res.Error == nil {
		return res
	}
	// No clause matched the conflict. With INSERT OR REPLACE the conflicting
	// rows are deleted and the new row inserted (SQLite: the upsert clauses
	// intercept only their own target conflicts; other conflicts fall back to
	// the REPLACE semantics).
	if s.IsReplace && strings.Contains(res.Error.Error(), "UNIQUE constraint failed") {
		rr := e.replaceDeleteConflicts(pg, tableEntry, colDefs, values, e.pkRowIDOrZero(tableEntry, colDefs, values))
		if rr.Error != nil {
			return rr
		}
		ins := e.insertRow(pg, tableEntry, colDefs, values, nil, s.OrConflict)
		if ins.Error != nil {
			return ins
		}
		ins.Row = values
		ins.InsertedChanges = ins.Changes
		return ins
	}
	return res
}

// pkRowIDOrZero returns the PK-derived rowid for a row, or 0 when none can be
// derived (REPLACE conflict deletion needs it to remove the conflicting rows).
func (e *DMLExecutor) pkRowIDOrZero(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) int64 {
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	rid, err := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, withoutRowid)
	if err != nil {
		return 0
	}
	return rid
}

// resolveUpsertConflicts walks the chained ON CONFLICT clauses in statement
// order and applies the first whose target matches the conflict source.
// Returns the UNIQUE constraint error when no clause matches.
func (e *DMLExecutor) resolveUpsertConflicts(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, hits []conflictHit, oc *sql.OnConflictClause, alias string) *Result {
	for c := oc; c != nil; c = c.Next {
		for _, hit := range hits {
			if !e.clauseMatchesHit(c, hit) {
				continue
			}
			switch c.Action {
			case sql.ConflictDoNothing:
				// The insert is skipped; RETURNING must not emit a row.
				return &Result{Changes: 0, Row: nil}
			case sql.ConflictDoUpdate:
				res := e.applyUpsertUpdate(tableEntry, colDefs, colIndex, hit.rowID, hit.values, values, c, alias)
				if res.Error != nil {
					return res
				}
				// RETURNING projects against the updated row, not the attempted values.
				return res
			}
		}
	}
	// No clause matched the conflict: report the underlying UNIQUE error.
	return &Result{Error: e.uniqueConstraintError(hits[0])}
}

// validateOnConflictTarget validates the ON CONFLICT target column against the
// table's PRIMARY KEY / UNIQUE constraints and indexes.

// validateOnConflictTarget validates the ON CONFLICT target column against the
// table's PRIMARY KEY / UNIQUE constraints and indexes.

// applyUpsertUpdate applies DO UPDATE SET assignments to the existing row
// and writes the updated row back to the table.
// findRowByUniqueCols searches for a row that conflicts with the given values
// on any UNIQUE column. Returns the RowID, existing values, and whether a
// conflict was found.
// insertSelectWrittenRow encodes, inserts, indexes, and fires AFTER triggers
// for one written INSERT ... SELECT row, then evaluates RETURNING. Returns a
// non-nil Result on failure, and the RETURNING row (or nil) on success.
func (e *DMLExecutor) execInsertDefault(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) *Result {
	// DEFAULT VALUES: every column takes its default value (NULL if none),
	// and an INTEGER PRIMARY KEY column receives the auto-assigned rowid.
	values, nextRowID, err := e.defaultValuesWithRowID(colDefs, tableEntry.Name, tableEntry.RootPage)
	if err != nil {
		return &Result{Error: err}
	}

	if ok, err := e.resolveInsertDefaultConstraints(tableEntry, colDefs, values, nextRowID); !ok {
		return &Result{Error: err}
	}

	// Fire BEFORE INSERT triggers — the row is not in the table yet.
	if res := e.fireDefaultBeforeTriggers(tableEntry, colDefs, values); res != nil {
		return res
	}

	if res := e.insertDefaultRow(tableEntry, colDefs, values, nextRowID); res != nil {
		return res
	}

	// Fire AFTER INSERT triggers.
	if res := e.fireDefaultAfterTriggers(tableEntry, colDefs, values, nextRowID); res != nil {
		return res
	}

	// Handle RETURNING clause — evaluate against the actual written row.
	if res := e.defaultReturningResult(s, colDefs, tableEntry, values, nextRowID); res != nil {
		return res
	}

	return &Result{Changes: 1, LastInsertRowID: nextRowID}
}

// defaultValuesWithRowID fills every column with its DEFAULT (NULL if none)
// and assigns an auto-generated rowid to an empty INTEGER PRIMARY KEY column.

// defaultValuesWithRowID fills every column with its DEFAULT (NULL if none)
// and assigns an auto-generated rowid to an empty INTEGER PRIMARY KEY column.

// fireAfterInsertTriggers fires AFTER INSERT triggers for the given table.
// triggerOwningCtx returns the database context whose schema contains the
// trigger entry (temp first, then main/attached). Used to scope trigger-body
// name resolution: TEMP triggers may reference tables in any database.
func (e *DMLExecutor) triggerOwningCtx(t *schema.Entry) *DatabaseContext {
	if tc := e.ctx.GetDB("temp"); tc != nil {
		if _, err := tc.Schema.FindTrigger(t.Name); err == nil {
			return tc
		}
	}
	for _, ctx := range e.ctx.Databases() {
		if ctx == nil || ctx == e.ctx.GetDB("temp") {
			continue
		}
		if _, err := ctx.Schema.FindTrigger(t.Name); err == nil {
			return ctx
		}
	}
	return nil
}

// fireTriggers fires triggers matching the given event and timing for the table.
func (e *DMLExecutor) fireTriggers(tableName, event, timing string, newRow, oldRow RowMap) *Result {
	// Resolve the table's context first (the trigger lookup needs it).
	tableCtx := e.triggerTableContext(tableName)

	// Search for triggers on the table. SQLite scopes triggers to their
	// database, but TEMP triggers fire on any table they reference (a TEMP
	// trigger on a main table fires when the main table changes). So include
	// triggers from the table's own context PLUS the TEMP context. A main
	// trigger does NOT fire for a temp-table event (temp shadows main).
	var triggers []*schema.Entry
	// Triggers in the table's own context.
	if t, err := tableCtx.Schema.FindTriggersForTable(tableName); err == nil {
		triggers = append(triggers, t...)
	}
	triggers = e.appendTempTriggers(tableCtx, tableName, triggers)

	if len(triggers) == 0 {
		return &Result{}
	}
	// SQLite fires AFTER triggers in REVERSE creation order (the most recently
	// created AFTER trigger fires first; e_droptrigger.test's aux.tr3 fires
	// before aux.tr2). BEFORE triggers fire in creation order. fireTrigger
	// filters by timing, so reverse the whole slice for the AFTER pass.
	if strings.EqualFold(timing, "AFTER") {
		for i, j := 0, len(triggers)-1; i < j; i, j = i+1, j-1 {
			triggers[i], triggers[j] = triggers[j], triggers[i]
		}
	}
	for _, t := range triggers {
		// Recursive-trigger guard (recursive_triggers OFF): a trigger does not
		// re-fire itself for a nested statement on the same table, but OTHER
		// triggers on the table DO fire (SQLite: the currently-executing
		// trigger program is excluded; e_changes autoinc-3928 fires r2 for
		// r1's inner inserts). fireTrigger pushes its own key onto the chain.
		if e.triggerInChain(tableCtx.Name, t.Name) {
			continue
		}
		if res := e.fireTrigger(t, event, timing, newRow, oldRow); res != nil {
			return res
		}
	}
	return &Result{}
}

// triggerTableContext resolves the database context for a table's triggers:
// the current DML context when set, otherwise the table's owning context.

// triggerTableContext resolves the database context for a table's triggers:
// the current DML context when set, otherwise the table's owning context.

// maxTriggerDepth is SQLite's SQLITE_MAX_TRIGGER_DEPTH default: recursive
// trigger programs abort with "too many levels of trigger recursion" once
// the nesting exceeds this limit.
// validateLoadedTriggerSchemaCtx parses a trigger body loaded from sqlite_master
// and checks that every referenced table exists in the trigger's database
// context. A trigger whose references no longer resolve (after a reopen with
// different attachments) is malformed: SQLite reports "malformed database
// schema (NAME) - trigger NAME cannot reference objects in database X".
// Unqualified references resolve in the trigger's owning database context (a
// trigger inside an ATTACHed database references tables there).
// maxTriggerDepth is SQLite's SQLITE_MAX_TRIGGER_DEPTH default: recursive
// trigger programs abort with "too many levels of trigger recursion" once
// the nesting exceeds this limit.
// validateLoadedTriggerSchemaCtx parses a trigger body loaded from sqlite_master
// and checks that every referenced table exists in the trigger's database
// context. A trigger whose references no longer resolve (after a reopen with
// different attachments) is malformed: SQLite reports "malformed database
// schema (NAME) - trigger NAME cannot reference objects in database X".
// Unqualified references resolve in the trigger's owning database context (a
// trigger inside an ATTACHed database references tables there).
func (e *DMLExecutor) validateLoadedTriggerSchemaCtx(t *schema.Entry, trigCtx *DatabaseContext) error {
	stmts, perr := parse.ParseSQL(t.SQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	var trig *sql.CreateTriggerStmt
	for _, st := range stmts {
		if c, ok := st.(*sql.CreateTriggerStmt); ok {
			trig = c
			break
		}
	}
	if trig == nil {
		return nil
	}
	for _, stmt := range trig.Statements {
		if err := e.checkTriggerStmtRefs(stmt, t, trigCtx); err != nil {
			return err
		}
	}
	return nil
}

// visitExprsInStmt walks every expression of a DML/SELECT statement (the
// statement kinds that can appear in a trigger body), invoking fn on each.
func visitExprsInStmt(stmt sql.Stmt, fn func(sql.Expr)) {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		for _, tuple := range s.Values {
			for _, e := range tuple {
				fn(e)
			}
		}
		if s.Select != nil {
			visitSelectExprs(s.Select, fn)
		}
	case *sql.UpdateStmt:
		for _, a := range s.Assignments {
			fn(a.Value)
		}
		fn(s.Where)
	case *sql.DeleteStmt:
		fn(s.Where)
	case *sql.SelectStmt:
		visitSelectExprs(s, fn)
	}
}

// visitSelectExprs walks a SELECT's result columns, WHERE, GROUP BY, HAVING,
// and ORDER BY expressions, invoking fn on each.
func visitSelectExprs(s *sql.SelectStmt, fn func(sql.Expr)) {
	if s == nil {
		return
	}
	for _, col := range s.Columns {
		fn(col.Expr)
	}
	fn(s.Where)
	for _, g := range s.GroupBy {
		fn(g)
	}
	fn(s.Having)
	for _, ob := range s.OrderBy {
		fn(ob.Expr)
	}
}

// isTempTrigger reports whether a trigger entry lives in the TEMP schema
// (TEMP triggers are always trusted, so the trusted_schema function-safety
// check skips them — trustschema1-2.120/2.150/3.120).
