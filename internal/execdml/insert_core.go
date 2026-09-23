// Package execdml implements DML execution.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// execInsert executes an INSERT. An echo write-through statement (INSERT
// INTO <echo vtab>) routes its errors through the echo module's error
// prefix (test8.c echoError: xUpdate reports the failed source write as
// "echo-vtab-error: %s", vtab1.12-2).
func (e *DMLExecutor) execInsert(s *sql.InsertStmt) *Result {
	// resolve.c: a VALUES tuple has no source row, so any column reference
	// in it is a prepare-time "no such column" error (bare or
	// table-qualified alike; trigger NEW./OLD. rows excepted). Subqueries
	// keep their own scope and are not descended into (insert-14.x:
	// INSERT INTO t3 VALUES((SELECT max(a) FROM t3)+1, t3.a, 6) reports
	// "no such column: t3.a").
	if res := e.validateInsertValuesExprs(s); res != nil {
		return res
	}
	if _, ok := e.ctx.EchoVTabSource(s.Table); !ok {
		return e.execInsertInner(s)
	}
	// The echo module's xBegin runs before the statement's first write
	// (vtab.c sqlite3VtabBegin): a failed module transaction start vetoes
	// the whole statement — no row is written (test8.c echoBegin /
	// echo_module_begin_fail, vtab1.10-3).
	if err, ok := e.ctx.EchoVTabBegin(s.Table); ok && err != nil {
		return &Result{Error: err}
	}
	// A non-integer explicit rowid is rejected before the write-through
	// reaches the source table (SQLite's OP_MustBeInt runs before xUpdate,
	// so the error is NOT prefixed by echoError; vtab1-15.4).
	if res := e.checkEchoExplicitRowid(s); res != nil {
		return res
	}
	e.echoWriteDepth++
	res := e.execInsertInner(s)
	if res.Error != nil {
		res.Error = e.wrapEchoWriteError(res.Error)
	}
	e.echoWriteDepth--
	return res
}

// checkEchoExplicitRowid rejects a non-integer explicit rowid value on an
// echo write-through INSERT before the source write (see execInsert).
func (e *DMLExecutor) checkEchoExplicitRowid(s *sql.InsertStmt) *Result {
	for _, tuple := range s.Values {
		if res := e.checkEchoTupleRowid(s.Columns, tuple); res != nil {
			return res
		}
	}
	return nil
}

// checkEchoTupleRowid validates one VALUES tuple's explicit rowid value (see
// checkEchoExplicitRowid).
func (e *DMLExecutor) checkEchoTupleRowid(columns []string, tuple []sql.Expr) *Result {
	for i, expr := range tuple {
		if i >= len(columns) || !execquery.IsRowIDName(columns[i]) {
			continue
		}
		v, err := e.ctx.EvalExpr(expr, nil)
		if err != nil {
			return nil // evaluation errors surface unwrapped later
		}
		if v != nil {
			if _, ok := mustBeIntRowid(v); !ok {
				return &Result{Error: fmt.Errorf("datatype mismatch")}
			}
		}
	}
	return nil
}

// execInsertInner is execInsert's statement pipeline (the echo write-through
// wrapper above re-routes its errors).
func (e *DMLExecutor) execInsertInner(s *sql.InsertStmt) (ret *Result) {
	// Generic updatable virtual tables (sqlite_dbpage etc.): INSERT routes to
	// the module's InsertRow (xUpdate parity); prepareInsertStmt then
	// validates the statement shape.
	if res := e.execInsertPrecheck(s); res != nil {
		return res
	}
	// Publish the statement's ON CONFLICT policy for trigger-body steps
	// without an explicit OR clause (SQLite trigger.c codeTriggerProgram:
	// pParse->eOrconf inheritance). Only the outermost DML statement sets
	// it; nested trigger-body statements leave it untouched. This runs
	// BEFORE the table/view dispatch so an INSTEAD OF trigger fired by a
	// view INSERT (fts5connect 4.x: REPLACE INTO v4) inherits the policy.
	outerPrev := e.ctx.OuterOrConflict()
	// The firing (depth-0) statement always publishes its policy — including
	// a nested depth-0 re-entry from a trigger body (each trigger step runs
	// at depth 0 via Engine.Exec). A trigger body step that fires its own
	// triggers must not clobber the outer firing policy with its own weaker
	// one: only publish when no outer policy is active.
	if e.shouldPublishOuterConflict(outerPrev) {
		e.publishOuterInsertConflict(s)
		defer e.ctx.SetOuterOrConflict(outerPrev)
	}
	tableEntry, dbCtx, res := e.resolveInsertTarget(s)
	if res != nil {
		return res
	}
	// fts5 INSERTs persist the index blob once, at the statement boundary
	// (sqlite3Fts5StorageSync's statement-end flush): the per-row writes only
	// mark the table dirty and the pending blob flushes here.
	if _, isFTS5 := e.ctx.FTS5Tables()[tableEntry.Name]; isFTS5 {
		defer func() {
			e.flushInsertFTS5Shadow(tableEntry, &ret)
		}()
	}
	// build.c sqlite3AddColumnToList: every name in the INSERT column list
	// must be a real table column ("table t has no column named z").
	if res := e.checkInsertColumnList(tableEntry, s); res != nil {
		return res
	}

	// The statement's ON CONFLICT policy was published at the top of
	// execInsert (before the table/view dispatch).

	// Track the modified table's database context so trigger firing resolves
	// triggers in the same context (main vs temp shadowing).
	prevDMLCtx := e.currentDMLCtx
	e.currentDMLCtx = dbCtx
	if res := e.CheckSameFileWriteConflictRes(dbCtx); res != nil {
		return res
	}
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

	// An fts5 table cannot be written while a fts5vocab cursor over it feeds
	// the same statement (fts5vocab2.test 5.1/5.2: the vocab vtab holds a
	// read on the index; SQLite aborts the conflicting write with
	// SQLITE_ABORT). The engine materializes the source scan up front, so
	// the conflict is detected from the statement's source references.
	// The statement also maintains every index on the target table, so each
	// index key's collation must resolve at prepare time (build.c
	// sqlite3LocateCollSeq; collate3-3.1: an INSERT fails after a
	// close/reopen that did not re-register the indexed column's collation).
	if res := e.validateInsertSourceAndCollations(tableEntry, colDefs, s); res != nil {
		return res
	}

	// SQLite's autoIncrementEnd (insert.c) writes the AUTOINCREMENT sequence
	// back to sqlite_sequence at statement end. This mirrors that: after a
	// successful INSERT on an AUTOINCREMENT table (directly or via triggers),
	// write the running max back to the real sqlite_sequence table. The
	// write is skipped for empty statements that do not touch the table.
	cleanupAutoInc, res := e.autoIncStatementSetup(dbCtx, tableEntry, &ret)
	if res != nil {
		return res
	}
	defer cleanupAutoInc()

	// RETURNING validation runs first (a bad column list fails the statement
	// even for the routed paths), then the routed bodies: virtual tables
	// without module-backed storage (rtree, echo, dbstat, ...) accept INSERT
	// as a no-op success; RETURNING projects NULLs for every column. FTS
	// tables are handled by their dedicated paths. INSERT...SELECT and
	// DEFAULT VALUES also complete in their dedicated paths.
	if res, done := e.execInsertRoutedBody(dbCtx, tableEntry, colDefs, s); done {
		return res
	}

	// REPLACE deletes rows and may fire triggers before inserting; if anything
	// fails the whole statement must be rolled back (SQLite statement journal).
	defer e.withInsertReplaceSnapshot(dbCtx, s, &ret)()

	return e.execInsertTuples(dbCtx, tableEntry, colDefs, s)
}

// execInsertRoutedBody handles the INSERT paths that bypass the row-by-row
// VALUES writer: the RETURNING column-list validation, storageless vtabs
// (no-op success, RETURNING projects NULLs), INSERT...SELECT, and
// empty-VALUES (DEFAULT VALUES). done reports that the statement completed
// inside the helper.
func (e *DMLExecutor) execInsertRoutedBody(dbCtx *DatabaseContext, tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) (*Result, bool) {
	if res := e.insertReturningValidation(s, colDefs, tableEntry.Name); res != nil {
		return res, true
	}
	if e.ctx.IsStoragelessVirtualTable(tableEntry) {
		return e.execStoragelessInsert(tableEntry, colDefs, s), true
	}
	if s.Select != nil {
		return e.execInsertSelect(tableEntry, colDefs, s), true
	}
	if len(s.Values) == 0 {
		return e.execInsertDefault(tableEntry, colDefs, s), true
	}
	return nil, false
}

// execInsertPrecheck runs the pre-target INSERT routing: a vtab-backed insert
// completes there (sqlite_dbpage etc. route to the module's InsertRow,
// xUpdate parity), and prepareInsertStmt validates the statement shape.
func (e *DMLExecutor) execInsertPrecheck(s *sql.InsertStmt) *Result {
	if res, handled := e.execVTabInsert(s); handled {
		return res
	}
	return e.prepareInsertStmt(s)
}

// autoIncStatementSetup validates the sqlite_sequence table up front for an
// AUTOINCREMENT insert and returns the statement-end sequence write (a no-op
// for non-AUTOINCREMENT targets). The write is skipped when the statement
// failed.
func (e *DMLExecutor) autoIncStatementSetup(dbCtx *DatabaseContext, tableEntry *schema.Entry, ret **Result) (func(), *Result) {
	if !(e.ctx.TableHasAutoIncrement(tableEntry.Name) && dbCtx != nil) {
		return func() {}, nil
	}
	// The sqlite_sequence table must exist and be an ordinary rowid
	// table before an AUTOINCREMENT insert uses it (autoinc-12.2/12.3:
	// a renamed-away or impostor sqlite_sequence fails the insert with
	// SQLITE_CORRUPT, "database disk image is malformed").
	if res := e.validateSequenceTable(dbCtx); res != nil {
		return nil, res
	}
	seqTable := tableEntry.Name
	seqPg := dbCtx.Pager
	seqRoot := tableEntry.RootPage
	return func() {
		e.writeAutoIncSeqOnSuccess(seqPg, seqRoot, seqTable, ret)
	}, nil
}

// insertReturningValidation validates the RETURNING column list when the
// statement carries one.
func (e *DMLExecutor) insertReturningValidation(s *sql.InsertStmt, colDefs []sql.ColumnDef, tableName string) *Result {
	if !s.HasReturning {
		return nil
	}
	return e.validateInsertReturning(s, colDefs, tableName)
}

// checkInsertColumnList validates a named INSERT column list against the
// target table when one is present.
func (e *DMLExecutor) checkInsertColumnList(tableEntry *schema.Entry, s *sql.InsertStmt) *Result {
	if len(s.Columns) == 0 {
		return nil
	}
	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	return validateInsertColumnList(tableEntry.Name, s.Columns, colDefs)
}

// shouldPublishOuterConflict reports whether this statement is the firing
// depth-0 statement with no outer policy active.
func (e *DMLExecutor) shouldPublishOuterConflict(outerPrev string) bool {
	return e.ctx.TriggerDepth() == 0 && outerPrev == ""
}

// publishOuterInsertConflict publishes the statement's ON CONFLICT policy:
// the explicit OR clause, else REPLACE for INSERT OR REPLACE semantics.
func (e *DMLExecutor) publishOuterInsertConflict(s *sql.InsertStmt) {
	if s.OrConflict != "" {
		e.ctx.SetOuterOrConflict(s.OrConflict)
	} else if s.IsReplace {
		e.ctx.SetOuterOrConflict("REPLACE")
	} else {
		e.ctx.SetOuterOrConflict("")
	}
}

// resolveInsertTarget finds the INSERT target table; a non-table name falls
// back to INSTEAD-OF-trigger view insert support. A non-nil Result is the
// statement's outcome (target lookup failure or the completed view insert).
func (e *DMLExecutor) resolveInsertTarget(s *sql.InsertStmt) (*schema.Entry, *DatabaseContext, *Result) {
	tableEntry, dbCtx, err := e.ctx.FindTable(s.Table)
	if err != nil {
		// Not a table — fall back to INSTEAD-OF-trigger view insert support.
		viewEntry, _, viewErr := e.ctx.FindView(s.Table)
		if viewErr != nil {
			return nil, nil, &Result{Error: err}
		}
		return nil, nil, e.execInsertView(s, viewEntry)
	}
	return tableEntry, dbCtx, nil
}

// flushInsertFTS5Shadow persists a dirty fts5 index at statement end,
// surfacing a flush error only when the statement did not already fail.
func (e *DMLExecutor) flushInsertFTS5Shadow(tableEntry *schema.Entry, ret **Result) {
	if t5, ok := e.ctx.FTS5Tables()[tableEntry.Name]; ok && t5 != nil {
		if ferr := e.flushFTS5Shadow(t5); ferr != nil && (*ret == nil || (*ret).Error == nil) {
			*ret = &Result{Error: ferr}
		}
	}
}

// validateInsertSourceAndCollations rejects an fts5 write fed by an fts5vocab
// cursor over the same table (fts5vocab2.test 5.1/5.2: the vocab vtab holds a
// read on the index; SQLite aborts the conflicting write with SQLITE_ABORT),
// then validates every index key's collation resolves at prepare time
// (build.c sqlite3LocateCollSeq; collate3-3.1: an INSERT fails after a
// close/reopen that did not re-register the indexed column's collation).
func (e *DMLExecutor) validateInsertSourceAndCollations(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) *Result {
	if t5, ok := e.ctx.FTS5Tables()[tableEntry.Name]; ok && t5 != nil && s.Select != nil {
		if insertSourceIsVocabOver(e.ctx, s.Select, tableEntry.Name) {
			return &Result{Error: fmt.Errorf("query aborted")}
		}
	}
	return e.validateIndexCollations(tableEntry, colDefs, nil)
}

// writeAutoIncSeqOnSuccess writes the AUTOINCREMENT running max back to the
// real sqlite_sequence table at statement end (insert.c autoIncrementEnd),
// skipping the write when the statement failed.
func (e *DMLExecutor) writeAutoIncSeqOnSuccess(seqPg *pager.Pager, seqRoot uint32, seqTable string, ret **Result) {
	if *ret != nil && (*ret).Error != nil {
		return
	}
	seq, ok := e.ctx.AutoIncSeqFor(seqPg, seqRoot)
	if !ok {
		seq = 0
	}
	_ = e.ctx.WriteSQLiteSequence(seqPg, seqTable, seq)
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
		// -4.15/4.16/4.17 t*_re tables). A conflict on ANOTHER unique
		// constraint with a non-REPLACE algorithm fails the statement and
		// undoes the deletes (insert.c statement journal; tkt-4a03edc4c8),
		// so check it BEFORE deleting anything.
		if e.uniqueReplaceableConflict(err, tableEntry, colDefs) {
			return e.resolveReplaceConflict(tableEntry, colDefs, values, nextRowID, err)
		}
		// Column-level ON CONFLICT REPLACE on a NOT NULL column: substitute the
		// column's DEFAULT value for the NULL and re-validate (SQLite conflate.c
		// OP_IsNull + ON CONFLICT REPLACE resolution). Without a DEFAULT the
		// constraint error stands. REPLACE (statement OR) does the same.
		return e.resolveReplaceNotNullDefaults(tableEntry, colDefs, values, nextRowID, orConflict, err)
	}
	return nil, true
}

// violatedConstraintName extracts the violated column's name from a UNIQUE
// constraint error ("UNIQUE constraint failed: t1.b" → "b"), or "" when the
// error names no column.
func violatedConstraintName(err error) string {
	if err == nil {
		return ""
	}
	errStr := err.Error()
	if dot := strings.LastIndex(errStr, "."); dot >= 0 {
		return errStr[dot+1:]
	}
	return ""
}

// resolveReplaceConflict completes a REPLACE resolution for a conflict on a
// REPLACE-carrying constraint: a secondary conflict with a non-REPLACE
// algorithm returns the error (or skips the row for IGNORE) BEFORE any delete
// (insert.c statement journal); otherwise the conflicting rows are deleted and
// the row is written. The boolean return means "write the row".
func (e *DMLExecutor) resolveReplaceConflict(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64, err error) (*Result, bool) {
	replacedCol := violatedConstraintName(err)
	if res, skip := e.replaceSecondaryConflictResult(tableEntry, colDefs, buildColumnIndex(colDefs), values, replacedCol); res != nil || skip {
		// skip mirrors the IGNORE path: drop the row without writing it.
		return res, false
	}
	if res := e.replaceDeleteConflicts(e.ctx.Pager(), tableEntry, colDefs, values, nextRowID); res.Error != nil {
		return res, false
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
	// The violated column: the error names it as the last dotted token
	// ("UNIQUE constraint failed: t1.x" → x).
	errStr := err.Error()
	violated := ""
	if dot := strings.LastIndex(errStr, "."); dot >= 0 {
		violated = errStr[dot+1:]
	}
	if violated == "" {
		return false
	}
	// Declaration order decides which constraint's clause applies: the
	// violated column's own UNIQUE/PK constraint (declared at the column)
	// precedes every table-level UNIQUE. conflict-15.20: x PRIMARY KEY
	// (ABORT) declared before UNIQUE(x,x) ON CONFLICT REPLACE — the PK's
	// ABORT wins, so the duplicate INSERT errors.
	for i := range colDefs {
		cd := &colDefs[i]
		if !strings.EqualFold(cd.Name, violated) {
			continue
		}
		return e.columnReplaceVerdict(cd, tableEntry)
	}
	return false
}

// columnReplaceVerdict decides ON CONFLICT REPLACE for a violated column:
// the column's own UNIQUE/PK declaration wins when present (the
// first-declared constraint covering this column); otherwise any
// table-level UNIQUE/PK containing it with ON CONFLICT REPLACE applies.
func (e *DMLExecutor) columnReplaceVerdict(cd *sql.ColumnDef, tableEntry *schema.Entry) bool {
	if cd.Unique || cd.PrimaryKey {
		// The column's own unique constraint is the first-declared
		// constraint covering this column.
		return cd.OnConflict == "REPLACE"
	}
	// The column has no own unique constraint; check table-level
	// UNIQUE constraints containing it, in declaration order.
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
	//
	// P8.PRAGMA (tkt2686): also skip when FK enforcement is OFF — the
	// RestorePager call site is gated on ForeignKeys(), so the snapshot
	// would be dead weight for plain inserts in the test's max_page_count
	// loop. Cap enforcement happens at the pager itself (AllocatePage
	// returns nil once numPages exceeds maxPageCount), so transaction-
	// level consistency is preserved by the BEGIN/ROLLBACK pairing without
	// a per-row pager snapshot.
	var snap *pager.PagerState
	if !e.ctx.InFTSFlush() && e.ctx.ForeignKeys() {
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
		e.rollbackInsertedRow(pg, tableEntry, tree, nextRowID)
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
		if err := e.writeIndexCell(def, colDefs, append(indexValues, rowID)); err != nil {
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
// new row on all columns of a UNIQUE index, skipping the row being updated
// (excludeCell). Returns a SQLite-style error on conflict. NULL values never
// conflict.
func (e *DMLExecutor) checkUniqueIndexExcluding(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, def uniqueIndexDef, excludeCell func(rc *storage.Record, cl *storage.Cell) bool) error {
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
		if excludeCell(rc, cl) {
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
	conflicts, _ := e.collectReplaceConflicts(pg, tableEntry, colDefs, colIndex, values, replaceRowID)

	hasTriggers := e.hasTriggersForTable(tableEntry.Name)
	tree := e.ctx.TableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, true)
	for _, cr := range conflicts {
		if res := e.deleteReplaceConflictRow(tree, tableEntry, colDefs, cr.rowID, cr.values, hasTriggers); res != nil {
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
func (e *DMLExecutor) execInsertOnConflict(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, s *sql.InsertStmt, explicitRowID *int64) *Result {
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
	if skipped, res := e.fireUpsertBeforeTriggers(tableEntry, colDefs, values, nextRowID, explicitRowID, withoutRowid); skipped {
		return res
	}

	// Try to find existing conflicting rows (via UNIQUE columns, composite
	// constraints, and UNIQUE indexes).
	colIndex := buildColumnIndex(colDefs)
	hits := e.findOnConflictRow(tableEntry, colDefs, colIndex, values)

	if len(hits) == 0 {
		// insertRow mutates values in place (rowid fill, affinity, generated
		// columns), so values holds the row that was actually written.
		return e.insertUpsertRow(pg, tableEntry, colDefs, values, s.OrConflict)
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
		return e.insertUpsertRow(pg, tableEntry, colDefs, values, s.OrConflict)
	}
	return res
}

// fireUpsertBeforeTriggers fills the IPK rowid and fires BEFORE INSERT
// triggers for the attempted upsert row. skipped reports the row must not
// proceed (RAISE(IGNORE) or a trigger failure); res carries the outcome.
func (e *DMLExecutor) fireUpsertBeforeTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64, explicitRowID *int64, withoutRowid bool) (bool, *Result) {
	ipkWasNil, ipkIndex := e.fillIPKRowID(colDefs, values, nextRowID, withoutRowid, isStrictTable(tableEntry.SQL))
	// The trigger-visible new.rowid is the EXPLICIT rowid (statement rowid
	// column or explicit IPK value); an auto-assigned rowid reads -1.
	expRowID := explicitTriggerRowid(explicitRowID, values, ipkIndex, withoutRowid)
	if !e.hasTriggersForTable(tableEntry.Name) {
		return false, nil
	}
	newRow := buildBeforeTriggerRow(colDefs, values, ipkWasNil, ipkIndex, withoutRowid, expRowID)
	if trigResult := e.fireBeforeInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
		if trigResult.Error == errRaiseIgnore {
			return true, &Result{Changes: 0, Row: nil}
		}
		return true, trigResult
	}
	return false, nil
}

// insertUpsertRow writes the attempted row and projects the written row:
// insertRow mutates values in place (rowid fill, affinity, generated
// columns), so values holds the row that was actually written.
func (e *DMLExecutor) insertUpsertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, orConflict string) *Result {
	res := e.insertRow(pg, tableEntry, colDefs, values, nil, orConflict)
	if res.Error != nil {
		return res
	}
	res.Row = values
	res.InsertedChanges = res.Changes
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

// rollbackInsertedRow removes a just-written row after an index-maintenance
// failure and invalidates the rowid cache (the statement rolls back).
func (e *DMLExecutor) rollbackInsertedRow(pg *pager.Pager, tableEntry *schema.Entry, tree *btree.BTree, nextRowID int64) {
	if _, derr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == nextRowID
	}); derr == nil {
		e.ctx.InvalidateRowIDCache(pg, tableEntry.RootPage)
	}
}
