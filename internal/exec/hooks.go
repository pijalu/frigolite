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

// SetWalHook registers the connection's WAL hook (sqlite3_wal_hook). The
// callback fires after each WAL-mode commit with (frames appended this
// commit, frames checkpointed). A nil callback clears the hook. The hook is
// also pushed onto the main database's pager so it fires once that pager
// enters WAL mode (even if registered beforehand, mirroring SQLite).
func (e *Engine) SetWalHook(fn func(nLog, nCkpt int) int) {
	e.walHook = fn
	if e.mainDB != nil && e.mainDB.Pager != nil {
		e.mainDB.Pager.SetWalHook(fn)
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
