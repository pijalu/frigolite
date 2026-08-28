package exec

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
