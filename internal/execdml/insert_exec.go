package execdml

import (
	"fmt"
	"sort"
	"strconv"
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
			return &Result{Error: err}
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
	ftsValues := make([]interface{}, len(colNames))
	for i := range colNames {
		if i < len(values) {
			ftsValues[i] = values[i]
		} else {
			ftsValues[i] = ""
		}
	}
	// The languageid=<col> option: extract the langid value from the hidden
	// column (the value at the langid column's position in `values`, which is
	// indexed by the FTS colDefs — user columns then hidden table-name/docid/
	// lang_id). SQLite's fts3UpdateMethod reads the langid value (default 0
	// when not supplied; a non-integer coerces to 0; a negative value fails
	// with "constraint failed" — fts4langid 1.9/1.17).
	langID := int64(0)
	if langCol := ftsTable.LangIDColName(); langCol != "" {
		if lv := ftsLangIDFromValues(ftsTable, values, langCol); lv != nil {
			langID = sqlValueToInt64(lv)
			if langID < 0 {
				return &Result{Error: fmt.Errorf("constraint failed")}
			}
		}
		// A language-aware tokenizer may reject the language id outright
		// (fts3_test.c testTokenizerLanguage fails xLanguageid for
		// langid >= 100; the error surfaces as "SQL logic error" —
		// fts4langid 4.1.5).
		if lv, ok := ftsTable.Tokenizer().(fts.LangidValidator); ok {
			if verr := lv.ValidateLangid(langID); verr != nil {
				return &Result{Error: verr}
			}
		}
	}
	// Honor an explicit rowid (INSERT INTO ft(rowid, x) VALUES(-45,'a a')) via
	// InsertWithID; otherwise the FTS module auto-assigns rowids 1..N.
	// SQLite's FTS3 xUpdate enforces the docid PRIMARY KEY: inserting a
	// rowid that already exists is a UNIQUE constraint failure (fts3.c
	// fts3UpdateMethod: "constraint failed"). The OR conflict resolution
	// mirrors regular tables: IGNORE skips, REPLACE deletes the old row
	// and inserts, FAIL/ABORT/ROLLBACK error.
	var nextRowID int64
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
	command := ""
	if len(colNames) < len(values) {
		if s, ok := values[len(colNames)].(string); ok {
			command = s
		}
	}
	special, specialRes := e.handleFTSCommand(tableEntry.Name, command)
	if specialRes != nil {
		return specialRes
	}
	if special {
		e.ctx.SetLastRowID(0)
		return &Result{Changes: 0, LastInsertRowID: 0}
	}
	if fixedRowID != nil {
		if ftsTable.HasDoc(*fixedRowID) {
			ftsTable.Delete(*fixedRowID)
		}
		if langCol := ftsTable.LangIDColName(); langCol != "" {
			ftsTable.InsertWithIDLangID(*fixedRowID, ftsValues, langID)
		} else {
			ftsTable.InsertWithID(*fixedRowID, ftsValues)
		}
		nextRowID = *fixedRowID
	} else {
		// An FTS4 content=<table> table's xUpdate reads the content row for
		// an AUTO-assigned docid; a missing row fails the insert with
		// "constraint failed" BEFORE the index is touched (fts3.c
		// fts3UpdateMethod; fts4content 3.1.1 vs 3.1.2 — an explicit docid
		// trusts the caller).
		if ct := ftsTable.ContentTable(); ct != "" {
			if !e.ctx.ContentRowExists(ct, ftsTable.NextDocID()) {
				return &Result{Error: fmt.Errorf("constraint failed")}
			}
		}
		if langCol := ftsTable.LangIDColName(); langCol != "" {
			nextRowID = ftsTable.InsertLangID(ftsValues, langID)
		} else {
			nextRowID = ftsTable.Insert(ftsValues)
		}
	}
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

// handleFTSCommand detects and runs the FTS special command inserted through
// the hidden table-name column (INSERT INTO t(t) VALUES('command')): the
// 'optimize', 'merge=N[,M]', 'nodesize=N', and 'integrity-check' commands.
// s is the hidden-column value; an empty value or one that matches no command
// is a normal document insert (special=false). An unrecognized NON-empty
// command string is SQL logic error (fts3.c fts3SpecialInsert initializes
// rc=SQLITE_ERROR and only sets it to OK for recognized commands; fts4merge5
// 1.5: 'maxpendinAB64' fails). Returns (special, result): special is true when
// the value was a command (the insert is a no-op), and a non-nil result
// carries a command error (e.g. "database disk image is malformed" for a merge
// over corrupt segments, fts3corrupt 6.10/8.3).
func (e *DMLExecutor) handleFTSCommand(tableName, s string) (bool, *Result) {
	// SQLite reads the hidden-column value through sqlite3_value_text, whose
	// result is NUL-terminated: an embedded NUL truncates the command text
	// (fts3corrupt4 24.7: t2.x holds "merge=1" followed by NUL bytes and
	// SQLite runs merge=1). Mirror the C-string semantics.
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	switch {
	case lower == "optimize":
		// OPTIMIZE reads every segment and content row; a corrupt one
		// aborts it (fts3corrupt4 10.3/14.2: a crash-written content
		// table fails the command with "database disk image is
		// malformed"). A segment whose start_block/end_block metadata is
		// inconsistent is still optimizable (fts3corrupt4 4.4: after
		// UPDATE t1_segdir SET start_block=1, optimize succeeds).
		if t, ok := e.ctx.FTSTables()[tableName]; ok && (t.LoadErr() != nil || t.HasCorruptContent()) {
			return true, &Result{Error: fmt.Errorf("database disk image is malformed")}
		}
		e.optimizeFTSShadow(tableName)
		return true, nil
	case lower == "rebuild":
		// REBUILD drops and rebuilds the FTS index from %_content
		// (fts3.c fts3RebuildMethod). A corrupt shadow btree or a
		// corrupt freelist (the rebuild allocates new segments) fails it
		// (fts3corrupt4 24.7: INSERT INTO t1(t1) SELECT 'rebuild'
		// FROM ... on a corrupt DB).
		if res := e.ctx.RebuildFTSIndex(tableName); res != nil {
			return true, res
		}
		if err := e.ctx.ValidateFreelistForGrowth(); err != nil {
			return true, &Result{Error: err}
		}
		return true, nil
	case lower == "integrity-check":
		// Validate all segment roots AND their referenced blocks, then
		// verify the in-memory index against the content rows; a corrupt
		// or drifted one fails the check (fts3.c
		// sqlite3Fts3IntegrityCheck; fts4check/fts4intck1).
		if res := e.ctx.RunFTSIntegrityCheck(tableName); res != nil {
			return true, res
		}
		return true, nil
	case strings.HasPrefix(lower, "merge="):
		// A merge reads the source segments (roots AND blocks); a corrupt
		// one aborts it (fts3corrupt 6.10/8.3). It also combines the
		// level-0 segments into a level-1 segment whose leaf blocks go in
		// %_segments (fts3corrupt4 2.1: after 12 single-leaf segments,
		// merge=1,4 writes 3 blocks while keeping the segdir rows).
		// Parse merge=A[,B] with SQLite's fts3DoIncrmerge semantics
		// (fts3_write.c): A = max leaf pages to write (fts3Getint — 0 for
		// a non-numeric prefix), optional ,B = min segments on a level
		// (default MergeCount/2 = 8). The command errors ("SQL logic
		// error") when trailing garbage remains or B < 2 (fts4merge 2.x:
		// merge=abc, merge=%%%, merge=,, merge=5,, merge=6,%, merge=6,six,
		// merge=6,1 all fail; merge=1 succeeds).
		rest := s[len("merge="):]
		nMerge, rest := ftsGetint(rest)
		nMin := 8
		// SQLite's fts3DoIncrmerge consumes ",B" only when a digit
		// follows the comma; a bare trailing comma ("5,") leaves it in z
		// and errors.
		if len(rest) > 1 && rest[0] == ',' {
			rest = rest[1:]
			nMin, rest = ftsGetint(rest)
		}
		if len(rest) != 0 || nMin < 2 {
			return true, &Result{Error: fmt.Errorf("SQL logic error")}
		}
		if res := e.ctx.ValidateFTSSegments(tableName, true); res != nil {
			return true, res
		}
		e.ctx.MergeFTS(tableName, nMerge, nMin)
		return true, nil
	case strings.HasPrefix(lower, "nodesize="):
		// The nodesize command sets the segment node size (fts3.c
		// fts3SegReader / the fts3 'nodesize' special command); it does not
		// add a document.
		if n, err := strconv.Atoi(strings.TrimSpace(s[len("nodesize="):])); err == nil {
			if t, ok := e.ctx.FTSTables()[tableName]; ok {
				t.SetNodeSize(n)
			}
		}
		return true, nil
	case strings.HasPrefix(lower, "maxpending="):
		// maxpending=N sets the pending-terms hash size (fts3.c
		// fts3SpecialInsert under SQLITE_TEST); it does not add a document.
		return true, nil
	case strings.HasPrefix(lower, "test-no-incr-doclist="):
		// test-no-incr-doclist=0/1 toggles SQLite's bNoIncrDoclist debug
		// flag (fts3_write.c fts3SpecialInsert, SQLITE_TEST). It changes
		// only a performance optimization (incremental doclists), never
		// query results; the engine accepts and ignores it (fts4incr 2.x
		// runs each query under both settings expecting identical results).
		return true, nil
	case strings.HasPrefix(lower, "mergecount="):
		// mergecount=N sets SQLite's nMergeCount debug toggle (fts3_write.c
		// fts3SpecialInsert under SQLITE_TEST: 4..FTS3_MERGE_COUNT, even).
		// It adjusts the auto-merge segment threshold used during flushes;
		// accepting it without an engine effect keeps the test command a
		// no-op (results are unaffected by the merge threshold).
		return true, nil
	case strings.HasPrefix(lower, "automerge="):
		// automerge=X sets the persistent auto-incr-merge setting (fts3.c
		// fts3DoAutoincrmerge: X==0 turns it off; 1 or > MergeCount map to 8;
		// stored in the %_stat id=2 row). It does not add a document.
		// SQLite's fts3SpecialInsert writes the %_stat row through the shadow
		// btree, so a corrupt shadow table fails the command with "database
		// disk image is malformed" (fts3corrupt4 24.7).
		if res := e.ctx.ValidateFTSShadowRoots(tableName); res != nil {
			return true, res
		}
		v, _ := ftsGetint(s[len("automerge="):])
		if t, ok := e.ctx.FTSTables()[tableName]; ok {
			t.SetAutomerge(v)
		}
		return true, nil
	case s == "":
		return false, nil
	default:
		// An unrecognized non-empty hidden-column value is SQL logic error
		// (fts3.c fts3SpecialInsert's default rc=SQLITE_ERROR; fts4merge5
		// 1.5: 'maxpendinAB64' fails).
		return true, &Result{Error: fmt.Errorf("SQL logic error")}
	}
}

// optimizeFTSShadow merges an FTS table's segment-directory rows into one,
// mirroring SQLite's OPTIMIZE command (fts3.c fts3DoOptimize →
// fts3SegmentMerge merges every segment of the index into one). The merged
// segment is written as a single level-0 row whose root covers all documents
// (fts3SegWriterFlush over the whole table).
func (e *DMLExecutor) optimizeFTSShadow(tableName string) {
	segdir := tableName + "_segdir"
	// SQLite's optimize merges each (langid, index) group into ONE segment
	// whose level is the numerically GREATEST level present in that group
	// (fts3_write.c fts3SegmentMerge, iLevel==FTS3_SEGCURSOR_ALL:
	// "iNewLevel = iMaxLevel"), and it is a NO-OP when a single non-pending
	// segment already covers the table — the existing row (including a
	// user-modified end_block) is left untouched (fts4growth 5.x: the
	// optimized segment keeps its level and end_block across later steps).
	// One row = one segment; SQLite's no-op check is nSegment==1.
	levelsRes := e.ctx.Exec(&sql.SelectStmt{
		Columns: []sql.SelectColumn{{Expr: &sql.ColumnRef{Name: "level"}, As: "level"}},
		From:    sql.TableRef{Name: segdir},
	})
	existingLevels := map[int64]bool{}
	maxLevel := int64(-1)
	nSegments := 0
	if levelsRes.Error == nil {
		for _, row := range levelsRes.Rows {
			if lv, ok := util.UnwrapColumnValue(row[0]).(int64); ok {
				existingLevels[lv] = true
				if lv > maxLevel {
					maxLevel = lv
				}
			}
			// One row = one segment; SQLite's no-op check is nSegment==1.
			nSegments++
		}
	}
	pending := 0
	if t, ok := e.ctx.FTSTables()[tableName]; ok && t != nil {
		pending = len(t.PendingSnapshot())
	}
	if nSegments == 1 && pending == 0 {
		// Single non-pending segment: SQLITE_DONE without rewriting.
		if t, ok := e.ctx.FTSTables()[tableName]; ok && t != nil {
			t.PendingFlush()
		}
		return
	}
	// Delete all segdir rows; one merged row per language is written below.
	_ = e.ctx.Exec(&sql.DeleteStmt{Table: segdir})
	// Every pre-optimize %_segments block belonged to a source segment that
	// OPTIMIZE deletes (fts3DeleteSegment per source): purge them so only the
	// merged output's own leaf blocks remain (fts4growth 5.x parity).
	_ = e.ctx.Exec(&sql.DeleteStmt{Table: tableName + "_segments"})
	var ftsTable *fts.FTS3Table
	if t, ok := e.ctx.FTSTables()[tableName]; ok {
		ftsTable = t
	}
	if ftsTable == nil {
		return
	}
	nodeSize := ftsTable.NodeSize()
	if nodeSize <= 0 {
		nodeSize = int(e.ctx.Pager().PageSize())
	}
	ids := ftsTable.AllRowsMap()

	// A languageid=<col> table merges PER LANGUAGE: SQLite's segment merge
	// works within one (iLangid, iIndex) group, so after 'optimize' the
	// %_segdir holds one row per distinct language, at the language's base
	// absolute level ((iLangid*nIndex+iIndex)*FTS3_SEGDIR_MAXLEVEL =
	// iLangid*1024 with no prefix indexes — fts3_write.c getAbsoluteLevel;
	// fts4langid 2.2: 9 languages → 9 segdir rows after optimize).
	type langGroup struct {
		langid int64
		ids    []int64
	}
	var groups []langGroup
	if ftsTable.LangIDColName() != "" {
		byLang := map[int64][]int64{}
		for _, id := range ids {
			l := ftsTable.DocLangID(id)
			byLang[l] = append(byLang[l], id)
		}
		for l := range byLang {
			groups = append(groups, langGroup{l, byLang[l]})
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].langid < groups[j].langid })
	} else {
		groups = append(groups, langGroup{0, ids})
	}

	nextBlock := e.ctx.NextFTSBlockID(tableName)
	for _, g := range groups {
		if len(g.ids) == 0 {
			// An empty group (no documents) contributes no segment: SQLite's
			// optimize merges existing segments, and a table with none stays
			// with zero %_segdir rows (fts4opt 3.2: CREATE + 'optimize' on an
			// empty table leaves count(*) FROM fts_segdir at 0).
			continue
		}
		rootBlob, blocks := ftsTable.SegmentRootBlocks(g.ids, nodeSize)
		level := int(g.langid * 1024)
		if g.langid == 0 {
			// Main-index group: the output takes the greatest level present
			// before the merge (fts3_write.c iNewLevel = iMaxLevel).
			if maxLevel >= 0 {
				level = int(maxLevel)
			}
		} else {
			// Prefix/language groups keep their absolute base; use the
			// greatest RELATIVE level seen inside this group's range.
			base := g.langid * 1024
			for lv := range existingLevels {
				if lv >= base && lv < base+1024 && lv-base > int64(level)-base {
					level = int(lv)
				}
			}
		}
		e.ctx.WriteFTSShadowRow(tableName, level, 0, blocks, rootBlob)
		for _, blk := range blocks {
			_ = e.ctx.Exec(&sql.InsertStmt{
				Table:   tableName + "_segments",
				Columns: []string{"blockid", "block"},
				Values: [][]sql.Expr{
					{
						&sql.NumericLit{Value: fmt.Sprintf("%d", nextBlock)},
						&sql.BlobLit{Value: blk.Block},
					},
				},
			})
			nextBlock++
		}
	}
	// The optimize deleted every %_segdir row and replaced them with one;
	// the segdir-idx cache is stale and must be rescanned next time. The
	// %_segments block counter is advanced past the new blocks.
	if t, ok := e.ctx.FTSTables()[tableName]; ok && t != nil {
		t.SetNextBlockID(nextBlock)
		t.InvalidateSegmentCache()
		// The merged segment already contains every pending document
		// (fts3DoOptimize flushes pending terms before merging); consuming
		// the pending list prevents a duplicate segment at the next flush
		// and lets optimize() report "Index already optimal" afterwards
		// (fts3f 1.3).
		t.PendingFlush()
	}
}

// prepareInsertRowValues resolves the rowid, fills the IPK, applies STRICT
// checks and affinity, resolves constraints, and fires BEFORE triggers.

// prepareInsertRowValues resolves the rowid, fills the IPK, applies STRICT
// checks and affinity, resolves constraints, and fires BEFORE triggers.

// prepareInsertRowValues resolves the rowid, fills the IPK, applies STRICT
// checks and affinity, resolves constraints, and fires BEFORE triggers.
// prepareInsertRowValues resolves the rowid, fills the IPK, applies STRICT
// checks and affinity, resolves constraints, and fires BEFORE triggers.
