package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// validateInsertReturning validates the RETURNING clause against the table's
// column definitions.
func (e *DMLExecutor) validateInsertReturning(s *sql.InsertStmt, colDefs []sql.ColumnDef, tableName string) *Result {
	if !s.HasReturning {
		return nil
	}
	if err := e.validateReturning(s.Returning, colDefs, tableName); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// withInsertReplaceSnapshot installs the statement-journal rollback for
// INSERT OR REPLACE, keyed off the caller's named return value.

// withInsertReplaceSnapshot installs the statement-journal rollback for
// INSERT OR REPLACE, keyed off the caller's named return value.

// withInsertReplaceSnapshot installs the statement-journal rollback for
// INSERT OR REPLACE, keyed off the caller's named return value.
// withInsertReplaceSnapshot installs the statement-journal rollback for
// INSERT OR REPLACE, keyed off the caller's named return value.
func (e *DMLExecutor) withInsertReplaceSnapshot(dbCtx *DatabaseContext, s *sql.InsertStmt, ret **Result) func() {
	if !s.IsReplace {
		return func() {}
	}
	// Skip the snapshot for the FTS flush's internal shadow-table REPLACEs
	// (the %_stat hint write per automerge): they are part of the enclosing
	// statement's rollback scope and have no constraints to violate, and
	// copying the whole pager per flush was ~15% of the fts4merge4 automerge
	// profile (fts4merge4 2.2.x).
	if e.ctx.InFTSFlush() {
		return func() {}
	}
	snap := dbCtx.Pager.Snapshot()
	return func() {
		if *ret != nil && (*ret).Error != nil {
			e.ctx.RestorePager(dbCtx.Pager, snap)
			// Rows whose rowids were computed for the aborted statement
			// are gone; the cached rowid counter must not survive.
			e.ctx.ResetNextRowIDCache()
			e.ctx.ResetAutoIncSeq()
		}
	}
}

// prepareInsertStmt performs pre-execution checks and rewrites for an INSERT:
// UPSERT-on-vtab rejection, echo virtual-table write-through, and authorization.

// prepareInsertStmt performs pre-execution checks and rewrites for an INSERT:
// UPSERT-on-vtab rejection, echo virtual-table write-through, and authorization.

// prepareInsertStmt performs pre-execution checks and rewrites for an INSERT:
// UPSERT-on-vtab rejection, echo virtual-table write-through, and authorization.
// prepareInsertStmt performs pre-execution checks and rewrites for an INSERT:
// UPSERT-on-vtab rejection, echo virtual-table write-through, and authorization.
func (e *DMLExecutor) prepareInsertStmt(s *sql.InsertStmt) *Result {
	// UPSERT (INSERT ... ON CONFLICT) is not supported on virtual tables:
	// SQLite raises "UPSERT not implemented for virtual table \"t1\""
	// before any echo-module write-through rewrite. Look the table up
	// directly (the echo rewrite below would mask the vtab) and reject.
	if s.OnConflict != nil {
		if te, _, terr := e.ctx.FindTable(s.Table); terr == nil && e.ctx.IsVirtualTable(te) {
			return &Result{Error: fmt.Errorf("UPSERT not implemented for virtual table %q", s.Table)}
		}
		// SQLite rejects UPSERT on a view at prepare time.
		if _, _, verr := e.ctx.FindView(s.Table); verr == nil {
			return &Result{Error: fmt.Errorf("cannot UPSERT a view")}
		}
	}
	// The echo virtual-table module mirrors its underlying table: INSERT
	// into an echo vtab writes through to the source table. Rewrite the
	// statement to target the source (adjusting the column list for hidden
	// columns) and run the normal insert machinery. This keeps echo write
	// semantics (INSERT/UPDATE/DELETE route to the source, vtabA.test,
	// vtabC.test triggers) without duplicating the insert pipeline.
	if srcName, ok := e.ctx.EchoVTabSource(s.Table); ok {
		e.ctx.RewriteEchoInsert(s, srcName)
	}
	if err := e.ctx.Authorize(auth.ActionInsert, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// execStoragelessInsert handles INSERT into a virtual table without
// module-backed storage: a no-op success, with RETURNING projecting NULLs.

// execStoragelessInsert handles INSERT into a virtual table without
// module-backed storage: a no-op success, with RETURNING projecting NULLs.

// execStoragelessInsert handles INSERT into a virtual table without
// module-backed storage: a no-op success, with RETURNING projecting NULLs.
// execStoragelessInsert handles INSERT into a virtual table without
// module-backed storage: a no-op success, with RETURNING projecting NULLs.
func (e *DMLExecutor) execStoragelessInsert(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) *Result {
	// Virtual tables without module-backed storage (rtree, echo, dbstat, ...)
	// accept INSERT as a no-op success; RETURNING projects NULLs for every
	// column. FTS tables are handled by their dedicated paths.
	if s.HasReturning {
		row := make(RowMap, len(colDefs)+1)
		for _, cd := range colDefs {
			row[cd.Name] = nil
		}
		row["rowid"] = nil
		vals, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
		if err != nil {
			return &Result{Error: err}
		}
		columns := e.ctx.BuildColumnNames([]sql.SelectColumn{s.Returning}, colDefs, nil)
		return &Result{Columns: columns, Rows: [][]interface{}{vals}}
	}
	return &Result{Changes: 1, LastInsertRowID: 1}
}

// execInsertTuples writes every VALUES tuple for an INSERT statement,
// accumulating change count and RETURNING rows.

// execInsertTuples writes every VALUES tuple for an INSERT statement,
// accumulating change count and RETURNING rows.

// execInsertTuples writes every VALUES tuple for an INSERT statement,
// accumulating change count and RETURNING rows.
// execInsertTuples writes every VALUES tuple for an INSERT statement,
// accumulating change count and RETURNING rows.
func (e *DMLExecutor) execInsertTuples(dbCtx *DatabaseContext, tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) *Result {
	var totalChanges int64
	var totalInserted int64
	var returningRows [][]interface{}
	var lastRowID int64
	for _, tuple := range s.Values {
		changes, inserted, rowValues, rowid, skip, err := e.insertOneTuple(dbCtx, tableEntry, colDefs, s, tuple)
		if err != nil {
			return e.tupleErrorResult(err, tableEntry, colDefs)
		}
		if skip {
			continue
		}
		totalChanges += changes
		totalInserted += inserted
		// The LAST_INSERT_ROWID() after a multi-row INSERT is the rowid of
		// the LAST row actually inserted (SQLite's OP_Insert stores the
		// final rowid in db->lastRowid). The per-tuple result carries the
		// rowid (insertFTSRow, insert_core); keep the last one.
		if rowid != 0 {
			lastRowID = rowid
		}
		if rowValues != nil {
			returningRows = append(returningRows, rowValues)
		}
	}

	// If RETURNING clause was present, return result rows instead of change count
	if res := e.insertReturningResult(s, colDefs, returningRows); res != nil {
		res.InsertedChanges = totalInserted
		if lastRowID != 0 {
			res.LastInsertRowID = lastRowID
		}
		return res
	}
	return &Result{Changes: totalChanges, InsertedChanges: totalInserted, LastInsertRowID: lastRowID}
}

// tupleErrorResult builds the Result for a failed VALUES tuple, applying the
// ON CONFLICT FAIL keep-prior-rows and ON CONFLICT ROLLBACK extensions.
func (e *DMLExecutor) tupleErrorResult(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	res := &Result{Error: err}
	// A constraint with its own ON CONFLICT FAIL keeps the rows
	// written before the conflict (insert.c FAIL semantics: the
	// failing row itself was never written — conflict3.test 1.x-11.x
	// multi-row VALUES), exactly like the INSERT ... SELECT path.
	if e.uniqueFailConflict(err, tableEntry, colDefs) {
		res.SetKeepPriorRowsOnError()
	}
	// ON CONFLICT ROLLBACK extends the abort to the whole
	// transaction (uniqueRollbackConflict parity with insertSelect).
	if e.uniqueRollbackConflict(err, tableEntry, colDefs) {
		res.SetRollbackTxOnError()
	}
	applyRaiseUndoScope(res, err)
	return res
}

// insertOneTuple evaluates, writes, and (for RETURNING) projects one VALUES
// tuple. skip reports an OR IGNORE row that must not count.

// insertOneTuple evaluates, writes, and (for RETURNING) projects one VALUES
// tuple. skip reports an OR IGNORE row that must not count.

// insertOneTuple evaluates, writes, and (for RETURNING) projects one VALUES
// tuple. skip reports an OR IGNORE row that must not count.
// insertOneTuple evaluates, writes, and (for RETURNING) projects one VALUES
// tuple. skip reports an OR IGNORE row that must not count.
func (e *DMLExecutor) insertOneTuple(dbCtx *DatabaseContext, tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, tuple []sql.Expr) (changes int64, inserted int64, rowValues []interface{}, rowid int64, skip bool, err error) {
	values, evalErr := e.evalTuple(tableEntry.Name, tuple, s.Columns, colDefs)
	if evalErr != nil {
		return 0, 0, nil, 0, false, evalErr
	}
	res, writtenRow := e.execInsertRow(dbCtx, tableEntry, colDefs, tuple, values, s)
	if res.Error != nil {
		// INSERT OR IGNORE: silently skip UNIQUE / NOT NULL / CHECK
		// constraint violations (SQLite's OR IGNORE applies to any
		// constraint conflict, not just UNIQUE).
		if s.OrIgnore && isIgnoreableConstraintError(res.Error) {
			return 0, 0, nil, 0, true, nil
		}
		return 0, 0, nil, 0, false, res.Error
	}
	rowValues, err = e.evalInsertReturningRow(s, writtenRow, colDefs, tableEntry.Name, res.LastInsertRowID)
	if err != nil {
		return 0, 0, nil, 0, false, err
	}
	// InsertedChanges counts rows written as NEW inserts (SQLite's
	// count_changes pragma excludes upsert DO UPDATE / DO NOTHING rows).
	ins := res.InsertedChanges
	if s.OnConflict == nil && ins == 0 && res.Changes > 0 && writtenRow != nil {
		// Plain (non-upsert) insert path: every changed row was inserted.
		ins = res.Changes
	}
	return res.Changes, ins, rowValues, res.LastInsertRowID, false, nil
}

// evalInsertReturningRow evaluates RETURNING against the row that was actually
// written (upsert DO UPDATE writes a different row than the attempted VALUES;
// DO NOTHING writes nothing).

// evalInsertReturningRow evaluates RETURNING against the row that was actually
// written (upsert DO UPDATE writes a different row than the attempted VALUES;
// DO NOTHING writes nothing).

// evalInsertReturningRow evaluates RETURNING against the row that was actually
// written (upsert DO UPDATE writes a different row than the attempted VALUES;
// DO NOTHING writes nothing).
// evalInsertReturningRow evaluates RETURNING against the row that was actually
// written (upsert DO UPDATE writes a different row than the attempted VALUES;
// DO NOTHING writes nothing).
func (e *DMLExecutor) evalInsertReturningRow(s *sql.InsertStmt, writtenRow []interface{}, colDefs []sql.ColumnDef, tableName string, lastRowID int64) ([]interface{}, error) {
	if !s.HasReturning || writtenRow == nil {
		return nil, nil
	}
	if lastRowID <= 0 {
		lastRowID = 0
	}
	row := buildRowMapFromValues(writtenRow, colDefs, lastRowID)
	return e.evalReturningStrict(s.Returning, row, colDefs, tableName)
}

// insertReturningResult builds the RETURNING result when present.

// insertReturningResult builds the RETURNING result when present.

// insertReturningResult builds the RETURNING result when present.
// insertReturningResult builds the RETURNING result when present.
func (e *DMLExecutor) insertReturningResult(s *sql.InsertStmt, colDefs []sql.ColumnDef, returningRows [][]interface{}) *Result {
	if !s.HasReturning {
		return nil
	}
	columns := e.ctx.BuildColumnNames([]sql.SelectColumn{s.Returning}, colDefs, nil)
	return &Result{Columns: columns, Rows: returningRows}
}

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

// fillReplaceNullDefaults substitutes the DEFAULT for NULL values in NOT NULL
// columns BEFORE computing generated columns (SQLite REPLACE semantics).
// fillReplaceNullDefaults substitutes the DEFAULT for NULL values in NOT NULL
// columns BEFORE computing generated columns (SQLite REPLACE semantics).
func (e *DMLExecutor) fillReplaceNullDefaults(colDefs []sql.ColumnDef, values []interface{}) {
	for i := range colDefs {
		cd := &colDefs[i]
		if cd.Generated != nil || cd.Default == nil {
			continue
		}
		if i < len(values) && values[i] == nil {
			if dv, derr := e.ctx.EvalExpr(cd.Default, nil); derr == nil {
				values[i] = dv
			}
		}
	}
}

// resolveReplaceNotNullDefaults loops substituting each NOT NULL column's
// DEFAULT for the violating NULL until constraints pass, re-computing
// generated columns and re-checking each time. Returns (result, write) like
// resolveInsertRowConstraints.

// resolveReplaceNotNullDefaults loops substituting each NOT NULL column's
// DEFAULT for the violating NULL until constraints pass, re-computing
// generated columns and re-checking each time. Returns (result, write) like
// resolveInsertRowConstraints.

// resolveReplaceNotNullDefaults loops substituting each NOT NULL column's
// DEFAULT for the violating NULL until constraints pass, re-computing
// generated columns and re-checking each time. Returns (result, write) like
// resolveInsertRowConstraints.
// resolveReplaceNotNullDefaults loops substituting each NOT NULL column's
// DEFAULT for the violating NULL until constraints pass, re-computing
// generated columns and re-checking each time. Returns (result, write) like
// resolveInsertRowConstraints.
func (e *DMLExecutor) resolveReplaceNotNullDefaults(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64, orConflict string, err error) (*Result, bool) {
	// Loop so every NOT NULL column with a DEFAULT is substituted
	// (e.g. REPLACE INTO t1(a,c) VALUES(NULL,NULL) substitutes both).
	for {
		cd := notNullReplaceColumn(err, colDefs, strings.EqualFold(orConflict, "REPLACE"))
		if cd == nil || cd.Default == nil {
			break
		}
		dv, derr := e.ctx.EvalExpr(cd.Default, nil)
		if derr != nil {
			return &Result{Error: derr}, false
		}
		idx := cdIndex(colDefs, cd.Name)
		if idx < 0 || idx >= len(values) {
			break
		}
		values[idx] = dv
		// Recompute generated columns (the default may feed them).
		if gerr := e.computeGeneratedValues(colDefs, values); gerr != nil {
			return &Result{Error: gerr}, false
		}
		if rerr := e.checkConstraints(tableEntry, colDefs, values, nextRowID); rerr != nil {
			err = rerr
			continue
		}
		// Substitution succeeded: proceed with the insert.
		return nil, true
	}
	return &Result{Error: err}, false
}

// fireInsertRowBeforeTriggers fires BEFORE INSERT triggers for a row about to
// be written, re-allocating the rowid when the triggers consumed the
// pre-computed one. Returns a non-nil Result on trigger failure (errRowSkipped
// for RAISE(IGNORE)).

// fireInsertRowBeforeTriggers fires BEFORE INSERT triggers for a row about to
// be written, re-allocating the rowid when the triggers consumed the
// pre-computed one. Returns a non-nil Result on trigger failure (errRowSkipped
// for RAISE(IGNORE)).

// insertFTSRow routes a row insert to an FTS virtual table, or returns nil
// when the table is not FTS-backed.
// insertFTSRow routes a row insert to an FTS virtual table, or returns nil
// when the table is not FTS-backed.
func (e *DMLExecutor) insertFTSRow(tableEntry *schema.Entry, values []interface{}, fixedRowID *int64, orConflict string) *Result {
	// fts5 tables route to their own engine (fts5UpdateMethod).
	if t5, ok := e.ctx.FTS5Tables()[tableEntry.Name]; ok {
		return e.insertFTS5Row(t5, tableEntry, values, fixedRowID, orConflict)
	}
	ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]
	if !ok {
		return nil
	}
	// Re-map the values onto the FTS table's real columns: the values array
	// from mapNamedTupleValues is indexed by ParseColumnDefs (which includes
	// the hidden docid/table-name/__langid vtab columns), while Insert/InsertWithID
	// expect one value per ftsTable.ColumnNames(). The rowid-alias position
	// (docid for FTS) holds the explicit docid and must be dropped.
	colNames := ftsTable.ColumnNames()
	ftsValues := remapFTSValues(colNames, values)
	// The languageid=<col> option: extract the langid value from the hidden
	// column (the value at the langid column's position in `values`, which is
	// indexed by the FTS colDefs — user columns then hidden table-name/docid/
	// lang_id). SQLite's fts3UpdateMethod reads the langid value (default 0
	// when not supplied; a non-integer coerces to 0; a negative value fails
	// with "constraint failed" — fts4langid 1.9/1.17).
	langID, res := ftsInsertLangID(ftsTable, values)
	if res != nil {
		return res
	}
	// Honor an explicit rowid (INSERT INTO ft(rowid, x) VALUES(-45,'a a')) via
	// InsertWithID; otherwise the FTS module auto-assigns rowids 1..N.
	// SQLite's FTS3 xUpdate enforces the docid PRIMARY KEY: inserting a
	// rowid that already exists is a UNIQUE constraint failure (fts3.c
	// fts3UpdateMethod: "constraint failed"). The OR conflict resolution
	// mirrors regular tables: IGNORE skips, REPLACE deletes the old row
	// and inserts, FAIL/ABORT/ROLLBACK error.
	isReplace := strings.EqualFold(orConflict, "REPLACE")
	if fixedRowID != nil && ftsTable.HasDoc(*fixedRowID) && !isReplace {
		return &Result{Error: fmt.Errorf("UNIQUE constraint failed: %s.rowid", tableEntry.Name)}
	}
	// SQLite's FTS3 accumulates inserted documents into a pending-terms hash
	// and flushes them as one segment at transaction boundaries (fts3.c
	// fts3PendingTermsFlush). The engine records the doc ID here; the segment
	// (with its root blob) is written to %_segdir at COMMIT (see
	// DDLExecutor.FlushFTSSegments). An insert of a special command value
	// (optimize, merge=N, nodesize=N, integrity-check) runs the command
	// INSTEAD of adding a document — check before inserting so the command
	// value never enters the index (fts3matchinfo 8.1: the nodesize/optimize
	// commands must not count as documents). The hidden table-name column
	// (values[len(colNames)] — the column named after the table) carries the
	// command; an unrecognized command string there is SQL logic error
	// (fts3.c fts3SpecialInsert returns SQLITE_ERROR for unknown values;
	// fts4merge5 1.5: 'maxpendinAB64' fails).
	command := ftsSpecialCommand(values, len(colNames))
	special, specialRes := e.handleFTSCommand(tableEntry.Name, command)
	if specialRes != nil {
		return specialRes
	}
	if special {
		e.ctx.SetLastRowID(0)
		return &Result{Changes: 0, LastInsertRowID: 0}
	}
	var nextRowID int64
	if fixedRowID != nil {
		nextRowID, res = e.insertFTSFixedDocid(tableEntry, ftsTable, *fixedRowID, ftsValues, langID)
	} else {
		nextRowID, res = e.insertFTSAutoDocid(tableEntry, ftsTable, ftsValues, langID)
	}
	if res != nil {
		return res
	}
	return e.finishFTSInsert(tableEntry, ftsTable, nextRowID, ftsValues, langID)
}

// remapFTSValues copies the insert values onto one slot per FTS user column,
// padding missing trailing values with "".
func remapFTSValues(colNames []string, values []interface{}) []interface{} {
	ftsValues := make([]interface{}, len(colNames))
	for i := range colNames {
		if i < len(values) {
			ftsValues[i] = values[i]
		} else {
			ftsValues[i] = ""
		}
	}
	return ftsValues
}

// ftsSpecialCommand reads the special-command value carried by the hidden
// table-name column (values[len(colNames)] — the column named after the
// table); "" when absent or not a string.
func ftsSpecialCommand(values []interface{}, colCount int) string {
	if colCount < len(values) {
		if s, ok := values[colCount].(string); ok {
			return s
		}
	}
	return ""
}

// ftsInsertLangID extracts and validates the languageid=<col> column's value
// for an FTS insert, returning the resolved language id (0 when the option is
// absent) or an error Result: a negative value fails with "constraint failed"
// (fts4langid 1.9/1.17), and a language-aware tokenizer may reject the
// language id outright (fts3_test.c testTokenizerLanguage fails xLanguageid
// for langid >= 100; the error surfaces as "SQL logic error" —
// fts4langid 4.1.5).
func ftsInsertLangID(ftsTable *fts.FTS3Table, values []interface{}) (int64, *Result) {
	langID := int64(0)
	if langCol := ftsTable.LangIDColName(); langCol != "" {
		if lv := ftsLangIDFromValues(ftsTable, values, langCol); lv != nil {
			langID = sqlValueToInt64(lv)
			if langID < 0 {
				return 0, &Result{Error: fmt.Errorf("constraint failed")}
			}
		}
		if lv, ok := ftsTable.Tokenizer().(fts.LangidValidator); ok {
			if verr := lv.ValidateLangid(langID); verr != nil {
				return 0, &Result{Error: verr}
			}
		}
	}
	return langID, nil
}

// insertFTSFixedDocid performs the OR REPLACE delete-then-insert phase for an
// explicitly targeted docid and returns the docid written.
func (e *DMLExecutor) insertFTSFixedDocid(tableEntry *schema.Entry, ftsTable *fts.FTS3Table, fixedRowID int64, ftsValues []interface{}, langID int64) (int64, *Result) {
	if ftsTable.HasDoc(fixedRowID) {
		// OR REPLACE's xUpdate delete phase: C's fts3PendingTermsDocid
		// (bDelete=1) flushes on a docid-restart (fts3conf 4.x).
		if ftsTable.PendingDocidRestart(fixedRowID, true, langID) && ftsTable.HasPendingOps() {
			if res := e.ctx.FlushFTSPendingTable(tableEntry.Name); res != nil {
				return 0, res
			}
		}
		ftsTable.Delete(fixedRowID)
	}
	// Insert phase (bDelete=0): a docid moving backward, or repeating
	// the previous insert's docid, restarts the pending batch
	// (fts4onepass-4.0).
	if ftsTable.PendingDocidRestart(fixedRowID, false, langID) && ftsTable.HasPendingOps() {
		if res := e.ctx.FlushFTSPendingTable(tableEntry.Name); res != nil {
			return 0, res
		}
	}
	if langCol := ftsTable.LangIDColName(); langCol != "" {
		ftsTable.InsertWithIDLangID(fixedRowID, ftsValues, langID)
	} else {
		ftsTable.InsertWithID(fixedRowID, ftsValues)
	}
	return fixedRowID, nil
}

// insertFTSAutoDocid inserts with a module-assigned docid, flushing a pending
// batch when the next docid restarts, and returns the docid written.
func (e *DMLExecutor) insertFTSAutoDocid(tableEntry *schema.Entry, ftsTable *fts.FTS3Table, ftsValues []interface{}, langID int64) (int64, *Result) {
	// An FTS4 content=<table> table's xUpdate reads the content row for
	// an AUTO-assigned docid; a missing row fails the insert with
	// "constraint failed" BEFORE the index is touched (fts3.c
	// fts3UpdateMethod; fts4content 3.1.1 vs 3.1.2 — an explicit docid
	// trusts the caller).
	if ct := ftsTable.ContentTable(); ct != "" {
		if !e.ctx.ContentRowExists(ct, ftsTable.NextDocID()) {
			return 0, &Result{Error: fmt.Errorf("constraint failed")}
		}
	}
	// fts3PendingTermsDocid runs for auto docids too: a DELETE-all in
	// the tx drops the next docid below iPrevDocid (the restart flush
	// fires, usually on an empty pending batch — a no-op).
	if r := ftsTable.PendingDocidRestart(ftsTable.NextDocID(), false, langID); r && ftsTable.HasPendingOps() {
		if res := e.ctx.FlushFTSPendingTable(tableEntry.Name); res != nil {
			return 0, res
		}
	}
	if langCol := ftsTable.LangIDColName(); langCol != "" {
		return ftsTable.InsertLangID(ftsValues, langID), nil
	}
	return ftsTable.Insert(ftsValues), nil
}

// finishFTSInsert completes an FTS insert: shadow-root validation, pending
// recording, %_content/%_docsize/%_stat writes, and the result rowid.
func (e *DMLExecutor) finishFTSInsert(tableEntry *schema.Entry, ftsTable *fts.FTS3Table, nextRowID int64, ftsValues []interface{}, langID int64) *Result {
	// Writing to an FTS table whose shadow btrees are structurally corrupt
	// fails: real SQLite reads the index during the insert and hits the
	// damage (fts3corrupt4 24.1: t1_segments page 4 free-space corruption).
	// A corrupt %_segdir root is NOT checked here: a plain insert does not
	// read it (fts3corrupt 1.2 inserts succeed after the root is corrupted).
	if res := e.ctx.ValidateFTSShadowRoots(tableEntry.Name); res != nil {
		return res
	}
	// SQLite's xUpdate adds the new docid to the pending-terms hash for
	// every insert, including an OR REPLACE that first deleted the old row
	// (fts3DeleteTerms removed the old terms above; fts3conf 3.x REPLACE
	// sequences stay integrity-check clean).
	ftsTable.RecordPending(nextRowID)
	// SQLite's FTS3 xUpdate writes the original document text to the %_content
	// shadow table (fts3.c fts3InsertDoc: INSERT INTO %_content(docid, ...)
	// VALUES(rowid, ...)). The engine mirrors that so SELECT FROM
	// <name>_content returns the stored documents (fts3comp1 1.x.2). FTS4
	// compress= applies the named function to each value before storage.
	if res := e.writeFTSContentRow(tableEntry.Name, nextRowID, ftsValues, ftsTable.CompressFn(), ftsTable, langID); res != nil {
		return res
	}
	// FTS4 maintains a %_docsize row per document (docid, size BLOB) where
	// size is the FTS3-varint array of per-column token counts (fts3.c
	// fts3InsertDocsize). SELECT hex(size) FROM t1_docsize exposes it
	// (fts4aa 1.6), so it must exist for FTS4 tables.
	if res := e.writeFTSDocsizeRow(tableEntry.Name, nextRowID, ftsTable); res != nil {
		return res
	}
	// fts3UpdateDocTotals runs at the end of xUpdate: the %_stat doctotal is
	// current even when the segment flush is skipped (REPLACE — fts3conf 3.1
	// reads matchinfo 'na' right after a REPLACE INTO).
	e.ctx.WriteFTSStat(tableEntry.Name)
	e.ctx.SetLastRowID(nextRowID)
	return &Result{Changes: 1, LastInsertRowID: nextRowID}
}

// ftsLangIDFromValues extracts the languageid=<col> column's value from the
// FTS insert's values array (indexed by ParseColumnDefs: user columns then
// hidden table-name/docid/lang_id). Returns nil when the value is absent.
// A non-integer value coerces to 0 (SQLite's sqlite3_value_int on text like
// 'xyz' → 0; fts4langid 1.9 inserts 'xyz' as langid and reads 0 back).
func ftsLangIDFromValues(ftsTable *fts.FTS3Table, values []interface{}, langCol string) interface{} {
	// The langid hidden column follows the user columns + table-name + docid
	// (colDefs order). values is indexed by the same colDefs, so the langid
	// value is at len(columnNames)+2 (table-name, docid) when the option is
	// present.
	pos := len(ftsTable.ColumnNames()) + 2
	if pos < len(values) {
		return values[pos]
	}
	return nil
}

// sqlValueToInt64 coerces a SQL value to int64 the way SQLite's
// sqlite3_value_int does: integers pass through, floats truncate, and a TEXT
// value is parsed as an integer prefix ('xyz' → 0, '4abc' → 4). NULL becomes
// 0. Used for the FTS4 languageid=<col> value (fts4langid 1.9: 'xyz' → 0).
func sqlValueToInt64(v interface{}) int64 {
	switch x := util.UnwrapColumnValue(v).(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		// Parse a leading integer (SQLite sqlite3_atoi): optional sign +
		// digits.
		s := strings.TrimSpace(x)
		neg := false
		if strings.HasPrefix(s, "-") {
			neg = true
			s = s[1:]
		} else if strings.HasPrefix(s, "+") {
			s = s[1:]
		}
		var n int64
		for _, c := range s {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int64(c-'0')
		}
		if neg {
			return -n
		}
		return n
	}
	return 0
}

// writeFTSContentRow writes one document row into an FTS table's %_content
// shadow table (docid plus one column per user column). The content table's
// column names follow SQLite's c%d%s convention (createFTSShadowTables). When
// the FTS4 table declares compress=, each value is passed through the
// compress function before storage (fts3.c fts3WriteExprList builds
// "?,compress(?),..." and executes it as SQL — under PRAGMA trusted_schema=OFF
// an unsafe compress function makes that INSERT fail with "SQL logic error",
// fts3comp1 3.4). Returns a non-nil Result when the compress function is
// unsafe in the current trusted_schema setting.
func (e *DMLExecutor) writeFTSContentRow(tableName string, docID int64, values []interface{}, compressFn string, ftsTable *fts.FTS3Table, langID int64) *Result {
	content := tableName + "_content"
	contentEntry, dbCtx, err := e.ctx.FindTable(content)
	if err != nil || contentEntry == nil {
		return nil
	}
	// Reuse the content table's actual column definitions so the values
	// target the real c%d%s column names regardless of the user columns.
	colDefs := e.ctx.ParseColumnDefs(contentEntry.Name, contentEntry.SQL)
	stored := make([]interface{}, 0, len(values)+1)
	stored = append(stored, docID)
	for _, v := range values {
		sv := v
		if compressFn != "" {
			// A non-innocuous compress function is rejected when the
			// trusted_schema pragma is OFF (fts3.c builds the %_content
			// INSERT with the compress expression; the SQLite core refuses
			// unsafe schema functions, returning SQLITE_ERROR "SQL logic
			// error").
			if !e.ctx.SchemaFunctionSafe(compressFn) {
				return &Result{Error: fmt.Errorf("SQL logic error")}
			}
			// Compress the value through the named SQL function.
			if cv, cerr := e.ctx.EvalExpr(&sql.FuncCall{
				Name: compressFn,
				Args: []sql.Expr{&sql.StringLit{Value: fmt.Sprintf("%v", v)}},
			}, nil); cerr == nil && cv != nil {
				sv = cv
			}
		}
		stored = append(stored, sv)
	}
	// A languageid=<col> table's %_content row carries the langid value as a
	// trailing column (fts3.c fts3InsertDoc writes "?, ..., langid").
	if ftsTable != nil && ftsTable.LangIDColName() != "" {
		stored = append(stored, langID)
	}
	// Write the row directly to the content table btree (no trigger/statement
	// machinery) — this runs per FTS insert, so a full Exec per row would be
	// O(n) statement overhead (fts3b inserts 10k documents in a transaction).
	if dbCtx != nil {
		e.writeTableRow(dbCtx.Pager, contentEntry, colDefs, stored, docID)
	}
	return nil
}

// writeFTSDocsizeRow writes the FTS4 %_docsize row for one document: the size
// blob is the FTS3-varint array of per-column token counts (fts3.c
// fts3InsertDocsize / fts3EncodeIntArray). The docsize table is created for
// FTS4 tables (matchinfo=fts3 tables omit it); a missing table is tolerated.
func (e *DMLExecutor) writeFTSDocsizeRow(tableName string, docID int64, ftsTable *fts.FTS3Table) *Result {
	docsize := tableName + "_docsize"
	docsizeEntry, dbCtx, err := e.ctx.FindTable(docsize)
	if err != nil || docsizeEntry == nil {
		return nil
	}
	counts, _ := ftsTable.DocSize(docID)
	blob := encodeFTSIntArray(counts)
	// REPLACE INTO %_docsize VALUES(docid, size) (fts3.c SQL_REPLACE_DOCSIZE);
	// an existing row (e.g. a REPLACE insert or a re-index) is overwritten.
	// Write directly to the docsize btree (keyed by rowid) instead of two
	// full Exec statements per insert — the FTS build paths insert tens of
	// thousands of rows (fts4check builds 30k) and per-row statement
	// execution dominates (the content row is written the same way).
	tree := e.ctx.TableBTreePg(dbCtx.Pager, docsizeEntry.Name, docsizeEntry.RootPage, true)
	existing, _ := e.ctx.ReadCellByRowID(tree, docID)
	if existing != nil {
		_, _ = tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == docID
		})
	}
	colDefs := e.ctx.ParseColumnDefs(docsizeEntry.Name, docsizeEntry.SQL)
	_, res := e.writeTableRow(dbCtx.Pager, docsizeEntry, colDefs, []interface{}{docID, blob}, docID)
	return res
}

// encodeFTSIntArray encodes a slice of integers as an FTS3 varint array
// (fts3.c fts3EncodeIntArray: each value is put with sqlite3Fts3PutVarint).
func encodeFTSIntArray(values []int) []byte {
	var out []byte
	for _, v := range values {
		out = fts.AppendFTS3Varint(out, uint64(v))
	}
	return out
}

// ftsGetint converts the leading decimal digits of s into an integer and
// returns the value plus the unconsumed suffix (fts3_write.c fts3Getint:
// digits only, stops at the first non-digit; a non-digit prefix yields 0).
func ftsGetint(s string) (int, string) {
	i := 0
	v := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		v = v*10 + int(s[i]-'0')
		i++
	}
	return v, s[i:]
}

// prepareInsertRowValues resolves the rowid, fills the IPK, applies STRICT
// checks and affinity, resolves constraints, and fires BEFORE triggers.

// prepareInsertRowValues resolves the rowid, fills the IPK, applies STRICT
// checks and affinity, resolves constraints, and fires BEFORE triggers.

// prepareInsertRowValues resolves the rowid, fills the IPK, applies STRICT
// checks and affinity, resolves constraints, and fires BEFORE triggers.
// prepareInsertRowValues resolves the rowid, fills the IPK, applies STRICT
// checks and affinity, resolves constraints, and fires BEFORE triggers.
