package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

func (e *Engine) normalizeCorruptionError(res *Result) *Result {
	if res == nil || res.Error == nil {
		return res
	}
	msg := res.Error.Error()
	if strings.Contains(msg, "ORDER BY term out of range") {
		return res
	}
	if strings.Contains(msg, "zip archive") {
		// zipfile.c reports its own corruption messages ("zip archive is
		// corrupt"); they are module errors, not database corruption —
		// keep them verbatim (zipfile.test 17.x).
		return res
	}
	if strings.Contains(msg, "unknown page type") ||
		strings.Contains(msg, "cell index") ||
		strings.Contains(msg, "out of range") ||
		strings.Contains(msg, "corrupt") {
		res.Error = fmt.Errorf("database disk image is malformed")
	}
	return res
}

// isRollbackStmt reports whether stmt is a ROLLBACK (a top-level ROLLBACK is
// the statement that legitimately rolled back; it must not fail with "abort
// due to ROLLBACK").
func isRollbackStmt(stmt sql.Stmt) bool {
	_, ok := stmt.(*sql.RollbackStmt)
	return ok
}

// isDMLStmt reports whether stmt is an INSERT/UPDATE/DELETE statement.
func (e *Engine) isDMLStmt(stmt sql.Stmt) bool {
	switch stmt.(type) {
	case *sql.InsertStmt, *sql.UpdateStmt, *sql.DeleteStmt:
		return true
	}
	return false
}

// dmlCTEsForPreflight extracts the WITH definitions from a DML statement for
// the preflight scope push. The CTE scope covers the whole single statement
// including its preflight checks.
func dmlCTEsForPreflight(stmt sql.Stmt) []sql.CTEDef {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		return s.CTEs
	case *sql.UpdateStmt:
		return s.CTEs
	case *sql.DeleteStmt:
		return s.CTEs
	}
	return nil
}

// duplicateCTEName reports the first duplicate CTE name in a WITH clause, or ""
// when all names are unique. Defined here (engine_core.go, the Exec method's
// file) so the preflight scope push can check it without importing engine.go
// symbols. The canonical definition lives in engine.go; this one must stay in
// sync.
func duplicateCTENameExec(ctes []sql.CTEDef) string {
	seen := make(map[string]bool, len(ctes))
	for _, c := range ctes {
		if seen[strings.ToLower(c.Name)] {
			return c.Name
		}
		seen[strings.ToLower(c.Name)] = true
	}
	return ""
}

// execPreflight runs statement-level validations that happen only at the
// outermost statement (trigger bodies skip them). Returns a non-nil Result on
// validation failure.
func (e *Engine) execPreflight(stmt sql.Stmt) *Result {
	if e.triggers.Depth() != 0 {
		return nil
	}
	// SQLite's SrcList grows across the whole statement (including nested
	// subqueries and CTE bodies), so the FROM-clause term limit counts all
	// of them (with1 22.1's five-level nesting hits "too many FROM clause
	// terms, max: 200").
	if n := countStatementFromTerms(stmt); n >= 200 {
		return &Result{Error: fmt.Errorf("too many FROM clause terms, max: %d", 200)}
	}
	// RAISE() is only valid inside a trigger program. SQLite rejects it at
	// prepare time; the engine's runtime evaluation would miss it when the
	// containing expression never executes (e.g. GROUP BY/HAVING over an
	// empty table), so validate the whole statement here. SELECT statements
	// are validated inside execSelect AFTER name resolution, because SQLite
	// resolves column names first — SELECT RAISE(abort,a) with an undefined
	// column a reports "no such column: a", not the RAISE error.
	if !shouldDeferRaiseCheck(stmt) {
		if err := e.validateNoRaiseOutsideTrigger(stmt); err != nil {
			return &Result{Error: err}
		}
	}
	// Triggers loaded from sqlite_master may reference objects that no
	// longer resolve (reopen with different attachments); SQLite reports
	// "malformed database schema" at schema load. Validate once per
	// statement.
	if err := e.validateLoadedTriggers(); err != nil {
		return &Result{Error: err}
	}
	// Stored schema validation is performed by schema-loading operations; do
	// not mask ordinary SELECT semantic errors during statement preflight.
	// DML statements validate their embedded subquery arity (INSERT/UPDATE/
	// DELETE SET/WHERE/VALUES expressions) — SELECT does this inside
	// execSelect after name resolution.
	if shouldValidateDMLSubqueries(stmt) {
		if err := e.validateDMLSubqueries(stmt); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

// shouldDeferRaiseCheck reports whether a statement's RAISE() check is deferred
func shouldDeferRaiseCheck(stmt sql.Stmt) bool {
	if _, isSelect := stmt.(*sql.SelectStmt); isSelect {
		return true
	}
	if ct, isCTAS := stmt.(*sql.CreateTableStmt); isCTAS && ct.AsSelect != nil {
		return true
	}
	return false
}

// shouldValidateDMLSubqueries reports whether a statement needs its embedded
// subquery arity validated at prepare time (non-SELECT, non-CTAS statements).
func shouldValidateDMLSubqueries(stmt sql.Stmt) bool {
	if _, isSelect := stmt.(*sql.SelectStmt); isSelect {
		return false
	}
	if ct, isCTAS := stmt.(*sql.CreateTableStmt); isCTAS && ct.AsSelect == nil {
		return false
	}
	return true
}

// execDispatch runs the statement and returns its result.
func (e *Engine) execDispatch(stmt sql.Stmt) *Result {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.execSelect(s)
	case *sql.InsertStmt:
		e.registerWriteIfInTx()
		return e.execDMLWritable(s.CTEs, func() *Result { return e.dml.Insert(s) })
	case *sql.UpdateStmt:
		e.registerWriteIfInTx()
		return e.execDMLWritable(s.CTEs, func() *Result { return e.dml.Update(s) })
	case *sql.DeleteStmt:
		e.registerWriteIfInTx()
		return e.execDMLWritable(s.CTEs, func() *Result { return e.dml.Delete(s) })
	case *sql.CommitStmt:
		return e.execCommit()
	case *sql.BeginStmt:
		return e.execBegin(s)
	case *sql.RollbackStmt:
		return e.execRollback()
	case *sql.SavepointStmt:
		return e.execSavepoint(s)
	default:
		return e.execOtherDDL(stmt)
	}
}

// registerWriteIfInTx marks the connection's database files as having an
// open write transaction when a write statement runs inside an explicit
// transaction (BEGIN ... COMMIT). SQLite's pager only acquires RESERVED on the
// first write, and a read-only statement (e.g. PRAGMA lock_status, EXPLAIN)
// inside a deferred BEGIN must NOT mark a write transaction — otherwise it would
// wrongly block other connections' writes via the cross-connection registry
// (lock7-1.4). Only actual write statements (DML, CREATE/DROP/ALTER, ...) call
// this.
func (e *Engine) registerWriteIfInTx() {
	if e.tx.inTransaction {
		e.registerWriteTx(true)
	}
}

// registerWriteUnlessReadOnly marks the cross-connection write transaction for
// a write statement, but skips read-only statements. SQLite's pager acquires
// RESERVED only on the first real write; a read-only PRAGMA (e.g. PRAGMA
// lock_status) or EXPLAIN inside a deferred BEGIN must not mark a write
// transaction, or it would wrongly block other connections' writes via the
// cross-connection registry (lock7-1.4).
func (e *Engine) registerWriteUnlessReadOnly(stmt sql.Stmt) {
	switch stmt.(type) {
	case *sql.PragmaStmt, *sql.ExplainStmt:
		return // read-only; do not mark a cross-connection write transaction
	}
	e.registerWriteIfInTx()
}

// execDMLWritable runs a DML statement, rejecting writes when queryOnly is set
// and scoping WITH (CTE) definitions to the statement.
func (e *Engine) execDMLWritable(ctes []sql.CTEDef, fn func() *Result) *Result {
	if e.settings.queryOnly {
		return &Result{Error: fmt.Errorf("attempt to write a readonly database")}
	}
	return e.withDMLCTEs(ctes, fn)
}

// dmlTargetTable resolves the table modified by a DML statement (INSERT,
// UPDATE, DELETE) for FK dirty tracking.
func (e *Engine) dmlTargetTable(stmt sql.Stmt) (*schema.Entry, *DatabaseContext, error) {
	var name string
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		name = s.Table
	case *sql.UpdateStmt:
		name = s.Table
	case *sql.DeleteStmt:
		name = s.Table
	default:
		return nil, nil, fmt.Errorf("not DML")
	}
	return e.findTable(name)
}

// execPostFK records the modified table for the deferred FK check and runs the
// deferred check at statement end in autocommit mode.
func (e *Engine) execPostFK(stmt sql.Stmt, res *Result, isDML bool) *Result {
	if res == nil {
		return nil
	}
	// Record the modified table so the deferred FK check at COMMIT (or at the
	// end of this statement in autocommit mode) re-validates only the tables
	// whose FK relationships changed.
	e.execMarkFKDirty(stmt, res, isDML)
	// In autocommit mode every statement is its own transaction, so deferred
	// foreign key constraints (DEFERRABLE INITIALLY DEFERRED, or immediate
	// ones while PRAGMA defer_foreign_keys is ON) are checked at the end of
	// the statement, exactly as if an implicit COMMIT ran. Only the outermost
	// statement checks (trigger bodies execute as nested Exec calls).
	return e.execCheckDeferredFK(res, isDML)
}

// execMarkFKDirty records the table modified by a successful outer DML
// statement for the deferred FK check. UPDATE/DELETE also mark the table's
// children for re-validation (parent keys may disappear); INSERT cannot orphan
// children (alterlegacy-8.2: inserting into a parent must not fail because a
// child has a pre-existing FK mismatch).
func (e *Engine) execMarkFKDirty(stmt sql.Stmt, res *Result, isDML bool) {
	if !isDML || res.Error != nil || !e.settings.foreignKeys || e.triggers.Depth() != 0 {
		return
	}
	if entry, ctx, terr := e.dmlTargetTable(stmt); terr == nil {
		e.constraints.MarkFKDirty(entry, ctx)
		if _, isUpdate := stmt.(*sql.UpdateStmt); isUpdate {
			e.constraints.MarkFKParentDirty(entry, ctx)
		} else if _, isDelete := stmt.(*sql.DeleteStmt); isDelete {
			e.constraints.MarkFKParentDirty(entry, ctx)
		}
	}
}

// execCheckDeferredFK runs the deferred foreign key check at statement end.
// In autocommit mode every statement is its own transaction, so deferred
// foreign key constraints (DEFERRABLE INITIALLY DEFERRED, or immediate ones
// while PRAGMA defer_foreign_keys is ON) are checked at the end of the
// statement, exactly as if an implicit COMMIT ran. Inside an explicit
// transaction, only IMMEDIATE constraints are checked per-statement (SQLite
// checks immediate FKs when each statement completes); constraints deferred to
// COMMIT are skipped here and checked by execCommit.
func (e *Engine) execCheckDeferredFK(res *Result, isDML bool) *Result {
	if !isDML || res.Error != nil || !e.settings.foreignKeys || e.triggers.Depth() != 0 {
		return res
	}
	err := e.constraints.CheckDeferredFK(e.tx.inTransaction)
	// Clear the dirty set whether or not the check passed, matching SQLite's
	// per-statement deferred-FK cycle (a failing check must not leak dirty
	// state into the next statement). Inside an explicit transaction the
	// immediate-only check must NOT clear dirty state: deferred constraints
	// still need re-validation at COMMIT. Only the failing immediate check
	// clears it (the statement is rolled back, so its changes are gone).
	if !e.tx.inTransaction {
		e.constraints.ResetFKDirty()
	} else if err != nil {
		e.constraints.ResetFKDirty()
	}
	if err != nil {
		fkRes := &Result{Error: err}
		fkRes.SetForceRollbackOnError()
		return fkRes
	}
	return res
}

// execRollbackOnError rolls back the whole statement on error, restoring all
// pagers and dropping row-id caches that may reference restored pages. INSERT
// OR FAIL does NOT back out earlier rows of the statement (SQLite ON CONFLICT
// FAIL semantics: only ABORT/ROLLBACK undo prior changes); the failing row
// itself was never written, so earlier rows survive. UPDATE OR FAIL keeps the
// same semantics (the DML executor writes rows incrementally and the error
// aborts with prior rows surviving). UPDATE OR ROLLBACK rolls back the whole
// transaction instead of just the statement. Foreign key violations (flagged
// with SetForceRollbackOnError by the statement-end FK check) ALWAYS roll back
// the whole statement, even under OR FAIL: SQLite treats them as statement-
// level aborts, not row-level ON CONFLICT resolutions.
func (e *Engine) execRollbackOnError(stmt sql.Stmt, res *Result, snaps []pagerSnap, isDML bool) *Result {
	if res == nil {
		return nil
	}
	isOrFail := false
	if is, ok := stmt.(*sql.InsertStmt); ok && is.OrFail {
		isOrFail = true
	}
	if u, ok := stmt.(*sql.UpdateStmt); ok && strings.EqualFold(u.OnConflict, "FAIL") {
		isOrFail = true
	}
	isOrRollback := false
	if u, ok := stmt.(*sql.UpdateStmt); ok && strings.EqualFold(u.OnConflict, "ROLLBACK") {
		isOrRollback = true
	}
	if is, ok := stmt.(*sql.InsertStmt); ok && strings.EqualFold(is.OrConflict, "ROLLBACK") {
		isOrRollback = true
	}
	// SQLite forces a full transaction rollback (sqlite3RollbackAll +
	// autoCommit=1) for the "special" errors SQLITE_INTERRUPT / SQLITE_FULL /
	// SQLITE_IOERR / SQLITE_NOMEM when the statement is not read-only
	// (src/vdbeaux.c:3358-3383). An interrupted write inside an explicit
	// transaction must therefore roll back the whole transaction, so a
	// subsequent bare ROLLBACK fails with "cannot rollback - no transaction is
	// active" (interrupt-3.x). The error message produced by checkProgress()
	// and Exec() is exactly "interrupted".
	isInterrupted := res.Error != nil && strings.EqualFold(res.Error.Error(), "interrupted")
	if isDML && res.Error != nil && !res.KeepPriorRowsOnError() &&
		(!isOrFail || res.ForceRollbackOnError()) {
		forceTxRollback := isOrRollback || res.RollbackTxOnError() || (isInterrupted && e.tx.inTransaction)
		if forceTxRollback && e.tx.inTransaction {
			// OR ROLLBACK (or a per-constraint ON CONFLICT ROLLBACK, or an
			// interrupted write) aborts the statement AND rolls back the whole
			// transaction (SQLite: "the current transaction is rolled back").
			// execRollback restores the BEGIN snapshots, closes the transaction,
			// and invalidates caches.
			e.execRollback()
		} else {
			e.restoreAllPagers(snaps)
			e.restoreAllFTS()
		}
		e.caches.nextRowIDCache = make(map[rowidCacheKey]int64)

		e.caches.autoIncSeq = make(map[rowidCacheKey]int64)
		res.Changes = 0
		res.LastInsertRowID = 0
	}
	return res
}

// execTrackChanges updates the CHANGES() / TOTAL_CHANGES() / LAST_INSERT_ROWID()
// state. SQLite's sqlite3_changes() reflects the last INSERT/UPDATE/DELETE
// only; SELECT/DDL statements do not reset the counter. sqlite3_total_changes()
// accumulates every row change since the connection opened, INCLUDING changes
// made by trigger bodies and FK actions (each nested Exec accumulates its
// statement's changes here).
func (e *Engine) execTrackChanges(res *Result, isDML bool) {
	if res == nil {
		return
	}
	if isDML {
		e.lastChanges = res.Changes
		e.totalChanges += res.Changes
	}
	// LAST_INSERT_ROWID() reflects the last rowid written by any DML, including
	// negative docids (an FTS or explicit-rowid insert with rowid -22 sets
	// last_insert_rowid() to -22 — SQLite's OP_Insert stores the rowid
	// unconditionally). Only 0 is "unset".
	if res.LastInsertRowID != 0 {
		e.lastRowID = res.LastInsertRowID
	}
}

// InFTSFlush reports whether the engine is currently flushing FTS segments
// (inside execFlushAutocommit or COMMIT). The flush's internal shadow-table
// writes are part of the enclosing statement's rollback scope, so the DML
// executor skips per-write pager snapshots for them (fts4merge4 automerge).
func (e *Engine) InFTSFlush() bool {
	return e.tx.inFTSFlush
}

// execFlushAutocommit applies PRAGMA count_changes and flushes attached
// database pagers after a successful autocommit statement so a later connection
// on the attached file sees the writes immediately. Inside an explicit
// transaction the writes stay dirty until COMMIT, so PRAGMA lock_status can
// report the reserved/exclusive lock held by the transaction. The MAIN pager is
// flushed only for DDL (a schema change another connection may observe);
// per-DML main flushes are skipped to avoid corrupting in-memory btree state.
func (e *Engine) execFlushAutocommit(stmt sql.Stmt, res *Result, isDML bool) *Result {
	if res == nil || res.Error != nil || e.tx.inTransaction {
		return nil
	}
	// Nested Exec calls (trigger bodies, the FTS segment flush's internal
	// shadow-table writes) are part of the enclosing statement, not separate
	// autocommit transactions: their dirty pages are flushed once by the
	// outermost statement. Skipping the per-inner flush removes the
	// dominant per-row FTS build cost (fts3_build_db_2 30040: every
	// %_segdir/%_stat shadow write re-flushed the whole dirty set).
	if e.tx.execDepth > 1 {
		return nil
	}
	// Fire the commit hook before the autocommit flush. The hook observes the
	// statement's uncommitted changes (SQLite runs the hook during the
	// transaction's commit phase); a nonzero return aborts the commit.
	if e.commitHook != nil && e.statementWrote(stmt, res, isDML) {
		if e.runCommitHook() {
			return &Result{Error: fmt.Errorf("constraint failed")}
		}
	}
	// PRAGMA count_changes: a DML statement returns a single row with the
	// changed-row count (SQLite's legacy behavior when the pragma is on). For
	// INSERT ... ON CONFLICT, count_changes counts only rows actually written
	// as new inserts (upsert DO UPDATE / DO NOTHING rows are excluded).
	if isDML && e.settings.countChanges && len(res.Rows) == 0 {
		count := res.Changes
		if res.InsertedChanges > 0 {
			count = res.InsertedChanges
		}
		res.Rows = [][]interface{}{{count}}
	}
	// Autocommit statement: bump the change counter of every database that
	// was written so other connections observe the change.
	e.bumpChangeCounters()
	// Flush attached database pagers so a later connection on the attached
	// file sees the writes immediately. The MAIN pager is flushed only for
	// DDL (a schema change another connection may observe); per-DML main
	// flushes are skipped to avoid corrupting in-memory btree state.
	e.flushAttachedPagers()
	// Flush pending FTS3 segments for the autocommit statement (a single
	// INSERT without an explicit transaction writes its segment immediately).
	// Mark the flush so its internal shadow-table writes skip the per-write
	// pager snapshot (they are part of this statement's rollback scope).
	e.tx.inFTSFlush = true
	// The FTS flush executes internal %_segdir/%_segments/%_stat writes
	// through nested Exec calls, which would clobber the connection's
	// last_insert_rowid with the shadow tables' rowids (a %_stat REPLACE at
	// id=0 sets it to 0). SQLite's FTS xUpdate runs inside OP_VUpdate, whose
	// `db->lastRowid = rowid` fires AFTER the module's internal writes
	// (src/vdbe.c case OP_VUpdate), so the statement's own final rowid wins.
	// Preserve the pre-flush value and restore it after the flush.
	savedRowID := e.lastRowID
	flushRes := e.FlushFTSSegments()
	e.lastRowID = savedRowID
	e.tx.inFTSFlush = false
	if flushRes != nil {
		return flushRes
	}
	// Flush the main pager so the file reflects autocommit writes and
	// lock_status reports "unlocked" after the statement completes (SQLite
	// commits autocommit statements by flushing). Flushing writes dirty pages
	// to disk without touching the in-memory page cache, so btree state is
	// preserved. A flush failure (e.g. a WAL write I/O error under fault
	// injection) is a real commit error and must propagate: the caller rolls
	// the statement's in-memory pages back via the pre-statement snapshot and
	// surfaces SQLITE_IOERR, mirroring SQLite's "commit fails → transaction
	// rolled back" semantics (a swallowed error would leave the statement
	// reporting success while nothing reached durable storage).
	if e.pager != nil {
		if err := e.pager.Flush(); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

// statementWrote reports whether an autocommit statement wrote to the
// database (so the commit hook fires only for real commits, matching SQLite
// which invokes sqlite3_commit_hook only when a transaction actually
// commits). DML with changed rows and DDL both write; plain SELECT does not.
func (e *Engine) statementWrote(stmt sql.Stmt, res *Result, isDML bool) bool {
	if isDML && res != nil && res.Changes > 0 {
		return true
	}
	if !isDML {
		switch stmt.(type) {
		case *sql.CreateTableStmt, *sql.CreateIndexStmt, *sql.CreateViewStmt,
			*sql.CreateTriggerStmt, *sql.CreateVirtualTableStmt,
			*sql.DropTableStmt, *sql.DropIndexStmt, *sql.DropViewStmt,
			*sql.DropTriggerStmt, *sql.AlterTableStmt, *sql.AttachStmt:
			return true
		}
	}
	return false
}

// bumpChangeCounters increments the file change counter of every database
// whose pager has dirty pages, so other connections observe the change.
func (e *Engine) bumpChangeCounters() {
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && dbCtx.Pager != nil && dbCtx.Pager.HasDirtyPages() {
			e.updateFileChangeCounter(dbCtx)
		}
	}
}

// flushAttachedPagers flushes the pagers of all attached (non-main, non-temp)
// databases.
func (e *Engine) flushAttachedPagers() {
	for name, ctx := range e.databases {
		upper := strings.ToUpper(name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		if ctx.Pager != nil {
			_ = ctx.Pager.Flush()
		}
	}
}
