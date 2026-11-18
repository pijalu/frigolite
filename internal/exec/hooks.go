package exec

import (
	"fmt"
	"strconv"
	"time"
)

// SetCommitHook registers the connection's commit hook (sqlite3_commit_hook).
// The callback returns 0 to allow the commit or nonzero to abort it with
// "constraint failed" (SQLITE_CONSTRAINT_COMMITHOOK). A nil callback clears
// the hook. The hook fires after a successful COMMIT (explicit or autocommit);
// when it aborts, the transaction remains open.
func (e *Engine) SetCommitHook(fn func() int) {
	e.commitHook = fn
}

// CommitHook returns the registered commit hook (nil when none).
func (e *Engine) CommitHook() func() int {
	return e.commitHook
}

// SetRollbackHook registers the connection's rollback hook
// (sqlite3_rollback_hook). A nil callback clears it. The hook fires after a
// ROLLBACK completes (explicit ROLLBACK or a failed statement's rollback).
func (e *Engine) SetRollbackHook(fn func()) {
	e.rollbackHook = fn
}

// SetUpdateHook registers the connection's update hook (sqlite3_update_hook).
// The callback fires for every row-level INSERT/UPDATE/DELETE on a ROWID
// table with the operation, database name, table name, and rowid. A nil
// callback clears the hook.
func (e *Engine) SetUpdateHook(fn func(op, db, table string, rowid int64)) {
	e.updateHook = fn
}

// SetWalHook registers the connection's WAL hook (sqlite3_wal_hook). The
// callback fires after each WAL-mode commit completes, at the committing
// statement's completion (vdbeapi.c doWalCallbacks), with the committing
// database's name and the log's total frame count (R-18913-19355). A
// non-zero return fails that statement with the raw code's sqlite3ErrStr
// text — the commit itself stands (R-14034-00929). A nil callback clears
// the hook (and disables the autocheckpoint mechanism, C's
// sqlite3_wal_hook contract).
func (e *Engine) SetWalHook(fn func(dbName string, nEntry int) int) {
	e.walHook = fn
}

// fireWalCallbacks ports vdbeapi.c doWalCallbacks (L723-742): at an
// autocommit statement's successful completion (`rc==SQLITE_DONE &&
// db->autoCommit` — the explicit-transaction statements never dispatch;
// the COMMIT statement's own tail does, autoCommit being true again by
// then), every database's pager hands over the frame count its last commit
// appended (sqlite3PagerWalCallback — wal.c:4415's read-and-clear
// iCallback) and the registered sqlite3_wal_hook runs ONCE per committing
// pager. A non-OK return fails the statement with the raw code's
// sqlite3ErrStr text; the commit itself stands (R-14034-00929).
func (e *Engine) fireWalCallbacks() error {
	if e.walHook == nil {
		return nil
	}
	if e.tx.inTransaction {
		return nil
	}
	rc := 0
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager == nil || rc != 0 {
			continue
		}
		if nEntry := ctx.Pager.TakeWALCallbackEntry(); nEntry > 0 {
			rc = e.walHook(ctx.Name, nEntry)
		}
	}
	if rc != 0 {
		return walHookErrStr(rc)
	}
	return nil
}

// walHookErrStr renders a raw result code the way sqlite3ErrStr does for a
// callback return: the primary code masked (0xff), the canned text. The
// sqlite3_wal_hook contract propagates whatever code the callback returns
// (e_walhook-4.1/4.2/4.3's 1/5/14).
func walHookErrStr(rc int) error {
	switch rc & 0xff {
	case 1: // SQLITE_ERROR
		return fmt.Errorf("SQL logic error")
	case 3: // SQLITE_PERM
		return fmt.Errorf("access permission denied")
	case 4: // SQLITE_ABORT
		return fmt.Errorf("query aborted")
	case 5, 6: // SQLITE_BUSY / SQLITE_LOCKED
		return fmt.Errorf("database is locked")
	case 7: // SQLITE_NOMEM
		return fmt.Errorf("out of memory")
	case 8: // SQLITE_READONLY
		return fmt.Errorf("attempt to write a readonly database")
	case 9: // SQLITE_INTERRUPT
		return fmt.Errorf("interrupted")
	case 10: // SQLITE_IOERR
		return fmt.Errorf("disk I/O error")
	case 11: // SQLITE_CORRUPT
		return fmt.Errorf("database disk image is malformed")
	case 13: // SQLITE_FULL
		return fmt.Errorf("database or disk is full")
	case 14: // SQLITE_CANTOPEN
		return fmt.Errorf("unable to open database file")
	case 19: // SQLITE_CONSTRAINT
		return fmt.Errorf("constraint failed")
	default:
		return fmt.Errorf("SQL logic error")
	}
}

// SetJournalFileOpHook installs a callback fired for xOpen/xClose/xDelete
// events on the "<db>-journal" rollback sidecar. The hook is the testvfs
// equivalent for the journal file (frigolite has no full VFS plugin
// system). It is registered against the main database's pager so it
// observes every journal-sidecar event for the connection.
func (e *Engine) SetJournalFileOpHook(fn func(op, path string)) {
	if e.mainDB != nil && e.mainDB.Pager != nil {
		e.mainDB.Pager.SetJournalFileOpHook(fn)
	}
}

// fireUpdateHook reports a row-level INSERT/UPDATE/DELETE to the update hook
// (only for rowid tables; WITHOUT ROWID tables use the preupdate hook).
func (e *Engine) fireUpdateHook(op, db, table string, rowid int64) {
	if e.updateHook != nil {
		e.updateHook(op, db, table, rowid)
	}
}

// runCommitHook invokes the registered commit hook, reporting whether the
// commit should be aborted (SQLITE_CONSTRAINT_COMMITHOOK). The hook only
// fires for a real commit (the engine tracks whether the current statement
// wrote anything).
func (e *Engine) runCommitHook() bool {
	if e.commitHook != nil && e.commitHook() != 0 {
		e.lastErrMsg = "constraint failed"
		e.lastErrCode = "SQLITE_CONSTRAINT_COMMITHOOK"
		return true
	}
	return false
}

// fireRollbackHook invokes the registered rollback hook after a rollback.
func (e *Engine) fireRollbackHook() {
	if e.rollbackHook != nil {
		e.rollbackHook()
	}
}

// SetBusyHandler registers the connection's busy handler
// (sqlite3_busy_handler). The callback receives the number of previous
// invocations for the current locked event and returns true to retry the
// lock attempt or false to return "database is locked" to the caller
// (main.c sqlite3BusyHandler: a non-zero handler result retries). A nil
// callback clears the handler. The handler reaches the pager's WAL
// checkpoint locks too (sqlite3PagerCheckpoint passes the pager's busy
// dispatch down to sqlite3WalCheckpoint).
func (e *Engine) SetBusyHandler(fn func(count int) bool) {
	e.busyHandler = fn
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && dbCtx.Pager != nil {
			dbCtx.Pager.SetBusyHandler(fn)
		}
	}
}

// SetBusyTimeout sets the connection's busy timeout in milliseconds
// (sqlite3_busy_timeout / PRAGMA busy_timeout). With no custom busy
// handler, a positive timeout installs sqliteDefaultBusyCallback's
// sleep-retry behavior between lock attempts.
func (e *Engine) SetBusyTimeout(ms int) {
	e.busyTimeoutMs = ms
}

// busyRetry invokes the busy handler after a failed lock attempt
// (pager.c pager_wait_on_lock: sqlite3OsLock retries run the handler
// between attempts until it returns zero).
func (e *Engine) busyRetry(count int) bool {
	if e.busyHandler != nil {
		return e.busyHandler(count)
	}
	if e.busyTimeoutMs > 0 {
		// sqliteDefaultBusyCallback (main.c): sleep delays[count] ms and
		// give up once the cumulative delay exceeds the timeout.
		delay, prior := busyDefaultDelay(count)
		if prior+delay > e.busyTimeoutMs {
			return false
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
		return true
	}
	return false
}

// busyDefaultDelay ports main.c sqliteDefaultBusyCallback's delay tables:
// per-attempt sleep and the cumulative sleep before the attempt.
func busyDefaultDelay(count int) (delay, prior int) {
	delays := [...]int{1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, 100}
	totals := [...]int{0, 1, 3, 8, 18, 33, 53, 78, 103, 128, 178, 228}
	const n = len(delays)
	if count < n {
		return delays[count], totals[count]
	}
	return delays[n-1], totals[n-1] + delays[n-1]*(count-(n-1))
}

// sqlite3_trace_v2 event masks (sqlite.h).
const (
	TraceStmt    = 1
	TraceProfile = 2
	TraceRow     = 4
	TraceClose   = 8
)

// SetTraceHook registers the legacy sqlite3_trace callback: it fires when a
// statement first begins running, with the statement text as prepared
// (sqlite3_stmt_sql semantics, trailing semicolon included). A nil callback
// clears the hook.
func (e *Engine) SetTraceHook(fn func(sql string)) {
	e.traceHook = fn
}

// SetProfileHook registers the sqlite3_profile callback: it fires when a
// statement finishes, with the statement text and the elapsed nanoseconds.
// A nil callback clears the hook.
func (e *Engine) SetProfileHook(fn func(sql string, ns int64)) {
	e.profileHook = fn
}

// SetTraceV2Hook registers the sqlite3_trace_v2 callback. The event is one
// of TraceStmt/TraceProfile/TraceRow/TraceClose; id is the statement id
// (connection id for TraceClose) and text is the statement SQL for
// TraceStmt, the elapsed nanoseconds rendering for TraceProfile, and empty
// otherwise. Only events in mask fire (a zero mask disables all events).
func (e *Engine) SetTraceV2Hook(fn func(event int, id int64, text string), mask int) {
	e.traceV2Hook = fn
	e.traceMask = mask
}

// BeginStmtTrace fires the legacy trace hook and the v2 STMT event for a
// statement about to run and returns the statement id used by the matching
// EndStmtTrace/FireTraceRow calls.
func (e *Engine) BeginStmtTrace(sqlText string) int64 {
	if e.traceInternal {
		return 0
	}
	e.traceNextID++
	e.traceCurID = e.traceNextID
	e.traceCurSQL = sqlText
	if e.traceHook != nil {
		e.traceHook(sqlText)
	}
	if e.traceV2Hook != nil && e.traceMask&TraceStmt != 0 {
		e.traceV2Hook(TraceStmt, e.traceCurID, sqlText)
	}
	return e.traceCurID
}

// FireFKProgramTrace fires the legacy trace hook for one FK-action trigger
// sub-program invocation (fkey.c runs FK ON DELETE/ON UPDATE actions as
// per-row trigger programs; sqlite3VdbeExec's OP_Trace fires the trace
// callback for every program run, with nVdbeExec>1 sub-programs reporting
// the top-level statement SQL). No-op when no hook is registered, while
// internal statements run, or outside a traced statement.
func (e *Engine) FireFKProgramTrace() {
	if e.traceInternal || e.traceHook == nil || e.traceCurSQL == "" {
		return
	}
	e.traceHook(e.traceCurSQL)
}

// EndStmtTrace fires the profile hook and the v2 PROFILE event after a
// statement ran.
func (e *Engine) EndStmtTrace(sqlText string, ns int64) {
	if e.traceInternal {
		return
	}
	if e.profileHook != nil {
		e.profileHook(sqlText, ns)
	}
	if e.traceV2Hook != nil && e.traceMask&TraceProfile != 0 {
		e.traceV2Hook(TraceProfile, e.traceCurID, strconv.FormatInt(ns, 10))
	}
}

// FireTraceRow fires the v2 ROW event for a produced result row.
// SetTraceInternal toggles suppression of trace/profile events while the
// engine runs internal statements (VACUUM's logical copy on the user
// connection — C's internal vdbe programs never fire the user trace
// callback).
func (e *Engine) SetTraceInternal(on bool) {
	e.traceInternal = on
}

func (e *Engine) FireTraceRow() {
	if e.traceInternal {
		return
	}
	if e.traceV2Hook != nil && e.traceMask&TraceRow != 0 {
		e.traceV2Hook(TraceRow, e.traceCurID, "")
	}
}

// FireTraceClose fires the v2 CLOSE event for the connection.
func (e *Engine) FireTraceClose() {
	if e.traceV2Hook != nil && e.traceMask&TraceClose != 0 {
		e.traceV2Hook(TraceClose, e.connID, "")
	}
}
