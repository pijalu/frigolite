// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

// Connection hook registration (sqlite3_*_hook C-API surface):
// preupdate, commit, autovacuum-pages, trace/profile/trace_v2, busy, WAL,
// journal-file-op, rollback and update hooks.
import (
	"github.com/pijalu/frigolite/internal/exec"
)

// SetPreupdateHook registers the connection's preupdate hook
// (sqlite3_preupdate_hook). A nil callback clears it. The callback runs after
// every row-level INSERT/UPDATE/DELETE; the current event is available via
// PreupdateCount/PreupdateOld/PreupdateNew.
func (db *DB) SetPreupdateHook(fn func()) {
	if db != nil && db.engine != nil {
		db.engine.SetPreupdateHook(fn)
	}
}

// PreupdateCount returns the number of columns in the current preupdate event
// (sqlite3_preupdate_count).
func (db *DB) PreupdateCount() int {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateCount()
	}
	return 0
}

// PreupdateType returns the operation type of the current preupdate event.
func (db *DB) PreupdateType() string {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateType()
	}
	return ""
}

// PreupdateDB returns the schema name of the current preupdate event.
func (db *DB) PreupdateDB() string {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateDB()
	}
	return ""
}

// PreupdateTable returns the table name of the current preupdate event.
func (db *DB) PreupdateTable() string {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateTable()
	}
	return ""
}

// PreupdateRowID returns the first rowid of the current preupdate event.
func (db *DB) PreupdateRowID() int64 {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateRowID()
	}
	return 0
}

// PreupdateRowID2 returns the second rowid of the current preupdate event.
func (db *DB) PreupdateRowID2() int64 {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateRowID2()
	}
	return 0
}

// PreupdateOld returns the old value of column i in the current preupdate
// event (sqlite3_preupdate_old).
func (db *DB) PreupdateOld(i int) interface{} {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateOld(i)
	}
	return nil
}

// PreupdateNew returns the new value of column i in the current preupdate
// event (sqlite3_preupdate_new).
func (db *DB) PreupdateNew(i int) interface{} {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateNew(i)
	}
	return nil
}

// SetCommitHook registers the connection's commit hook (sqlite3_commit_hook).
// The callback returns 0 to allow the commit or nonzero to abort it with
// "constraint failed". A nil callback clears the hook.
func (db *DB) SetCommitHook(fn func() int) {
	if db != nil && db.engine != nil {
		db.engine.SetCommitHook(fn)
	}
}

// SetAutovacuumPagesCallback registers the per-batch autovacuum-pages callback
// (sqlite3_autovacuum_pages). It fires once before each auto-vacuum commit
// (FULL mode) with (schema, fileSize, nFree, pageSize); the callback returns
// the number of pages to vacuum this batch (clamped to nFree). A nil callback
// clears the registration (default = drain all). P8.INCRVACUUM phase 4.
func (db *DB) SetAutovacuumPagesCallback(fn func(schema string, fileSize, nFree, pageSize uint32) uint32) {
	if db != nil && db.engine != nil {
		db.engine.SetAutovacuumPagesCallback(fn)
	}
}

// sqlite3_trace_v2 event masks (sqlite.h), re-exported for the trace_v2
// hooks.
const (
	TraceStmt    = exec.TraceStmt
	TraceProfile = exec.TraceProfile
	TraceRow     = exec.TraceRow
	TraceClose   = exec.TraceClose
)

// SetTraceHook registers the legacy sqlite3_trace callback: it fires when a
// statement first begins running, with the statement text as prepared. A nil
// callback clears the hook.
func (db *DB) SetTraceHook(fn func(sql string)) {
	if db == nil {
		return
	}
	db.engine.SetTraceHook(fn)
}

// SetProfileHook registers the sqlite3_profile callback: it fires when a
// statement finishes with the statement text and elapsed nanoseconds. A nil
// callback clears the hook.
func (db *DB) SetProfileHook(fn func(sql string, ns int64)) {
	if db == nil {
		return
	}
	db.engine.SetProfileHook(fn)
}

// SetTraceV2Hook registers the sqlite3_trace_v2 callback with an event mask
// (TraceStmt|TraceProfile|TraceRow|TraceClose). A nil callback clears it.
func (db *DB) SetTraceV2Hook(fn func(event int, id int64, text string), mask int) {
	if db == nil {
		return
	}
	db.engine.SetTraceV2Hook(fn, mask)
}

// SetBusyHandler registers the connection's busy handler
// (sqlite3_busy_handler). The callback receives the number of previous
// invocations for the current locked event and returns true to retry the
// lock attempt, false to give up ("database is locked"). A nil callback
// clears the handler.
func (db *DB) SetBusyHandler(fn func(count int) bool) {
	if db == nil {
		return
	}
	db.engine.SetBusyHandler(fn)
}

// SetWalHook registers the connection's WAL hook (sqlite3_wal_hook). The
// callback fires after each WAL-mode commit with (frames appended this
// commit, frames checkpointed). A nil callback clears the hook.
func (db *DB) SetWalHook(fn func(nLog, nCkpt int) int) {
	if db != nil && db.engine != nil {
		db.engine.SetWalHook(fn)
	}
}

// SetJournalFileOpHook installs a callback fired for xOpen/xClose/xDelete
// events on the "<db>-journal" rollback sidecar (testvfs equivalent for
// the journal file). The hook is the narrow observability path through
// which the journal2 TCL test suite verifies the OS-level file-ops
// sequence; frigolite does not have a full VFS plugin system. Pass nil to
// clear. The hook fires synchronously under the pager lock, so the
// callback should be lightweight (e.g. appending to a string).
func (db *DB) SetJournalFileOpHook(fn func(op, path string)) {
	if db != nil && db.engine != nil {
		db.engine.SetJournalFileOpHook(fn)
	}
}

// SetRollbackHook registers the connection's rollback hook
// (sqlite3_rollback_hook). A nil callback clears it.
func (db *DB) SetRollbackHook(fn func()) {
	if db != nil && db.engine != nil {
		db.engine.SetRollbackHook(fn)
	}
}

// SetUpdateHook registers the connection's update hook (sqlite3_update_hook).
// The callback fires for every row-level INSERT/UPDATE/DELETE on a ROWID
// table with the operation, database name, table name, and rowid. A nil
// callback clears the hook.
func (db *DB) SetUpdateHook(fn func(op, dbName, table string, rowid int64)) {
	if db != nil && db.engine != nil {
		db.engine.SetUpdateHook(fn)
	}
}
