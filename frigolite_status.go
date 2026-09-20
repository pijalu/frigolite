// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

// Connection introspection: column metadata, change counters and status
// metrics (sqlite3_status / sqlite3_db_status / sqlite3_stmt_status surface).
import (
	"fmt"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/execexpr"
)

// LastInsertRowID returns the rowid of the last inserted row.
func (db *DB) LastInsertRowID() int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.LastInsertRowID()
}

// FileDataVersion returns the database file's change counter (the
// SQLITE_FCNTL_DATA_VERSION equivalent). The schema argument defaults to
// "main"; the counter advances on every write commit, including this
// connection's own commits.
func (db *DB) FileDataVersion(schema string) int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.FileDataVersion(schema)
}

// TotalChanges returns the total number of rows changed by INSERT, UPDATE or
// DELETE statements since the connection opened, including changes made by
// trigger bodies and foreign-key actions (sqlite3_total_changes).
func (db *DB) TotalChanges() int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.TotalChanges()
}

// Changes returns the number of rows changed by the most recently completed
// INSERT, UPDATE or DELETE statement, exclusive of changes made by lower-level
// triggers (sqlite3_changes).
func (db *DB) Changes() int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.LastChanges()
}

// ResetChangesCounters zeroes the changes()/total_changes() counters, matching
// a fresh sqlite3 connection.
func (db *DB) ResetChangesCounters() {
	if db != nil && db.engine != nil {
		db.engine.ResetChangesCounters()
	}
}

// TableColumnMetadata reports a table column's declared type, collation,
// NOT NULL, PRIMARY KEY, and AUTOINCREMENT flags (sqlite3_table_column_metadata).
func (db *DB) TableColumnMetadata(schemaName, table, column string) (*exec.ColumnMetadata, error) {
	if db == nil || db.engine == nil {
		return nil, fmt.Errorf("database not open")
	}
	return db.engine.TableColumnMetadata(schemaName, table, column)
}

// DbStatus reports a per-connection status counter (sqlite3_db_status).
// name is a SQLITE_DBSTATUS_* name ("SQLITE_DBSTATUS_CACHE_USED", ...); the
// result is the current value (the highwater mark equals it in this engine's
// deterministic model).
func (db *DB) DbStatus(name string) (current, highwater int64) {
	if db != nil && db.engine != nil {
		return db.engine.DbStatus(name)
	}
	return 0, 0
}

// Status reports a global status counter (sqlite3_status). name is a
// SQLITE_STATUS_* name ("SQLITE_STATUS_MEMORY_USED", ...).
func (db *DB) Status(name string) (current, highwater int64) {
	if db != nil && db.engine != nil {
		return db.engine.Status(name)
	}
	return 0, 0
}

// LikeCallCount reports the number of LIKE/GLOB comparisons the engine has
// evaluated since the last reset (func.c sqlite3_like_count under
// SQLITE_TEST, linked to tester.tcl's sqlite_like_count variable). The LIKE
// optimization replaces a prefix LIKE with an index range scan and removes
// the per-row invocation entirely, so the counter observes the optimization
// the same way SQLite's does (like.test 3.x: 12 calls without the
// optimization, 0 calls with it).
func (db *DB) LikeCallCount() int64 { return execexpr.LikeCallCount() }

// ResetLikeCallCount zeroes the LIKE/GLOB invocation counter
// (tester.tcl: set sqlite_like_count 0).
func (db *DB) ResetLikeCallCount() { execexpr.ResetLikeCallCount() }

// PagerCacheSize reports the number of pages currently held in the pager
// cache (test3.c btree_pager_stats "page" field; cache.test pager_cache_size).
func (db *DB) PagerCacheSize() int {
	if db != nil && db.pager != nil {
		return int(db.pager.NumPages())
	}
	return 0
}

// StmtStatus reports a prepared-statement status counter (sqlite3_stmt_status).
// name is a SQLITE_STMTSTATUS_* name ("SQLITE_STMTSTATUS_VM_STEP", ...).
func (db *DB) StmtStatus(name string) int64 {
	if db != nil && db.engine != nil {
		return db.engine.StmtStatus(name)
	}
	return 0
}
