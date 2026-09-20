// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

// Connection configuration and test-control knobs: authorizer, limits,
// pending byte, active-statement emulation, progress/interrupt control and
// DBCONFIG-style switches (DQS / defensive / QPSG).
import (
	"github.com/pijalu/frigolite/internal/auth"
)

// SetAuthorizer sets the authorization callback for the database.
// A nil authorizer allows all operations (default behavior).
// The callback is invoked before each database operation to check
// whether it should be allowed.
func (db *DB) SetAuthorizer(a auth.Authorizer) {
	if db != nil && db.engine != nil {
		db.engine.SetAuthorizer(a)
	}
}

// SetPendingByte overrides the PENDING_BYTE lock-byte offset for this
// database's main pager. Mirrors the SQLite C test harness
// (sqlite3_test_control_pending_byte from src/test2.c), which lowers the
// byte to 0x10000 so file-size checks in autovacuum-9.3 / 9.5 / corrupt2
// / lock4 etc. observe a small expected value without creating a 1GB
// database. Pass 0 to restore the production default (0x40000000).
//
// Returns the previous offset (default 0x40000000), as the C
// sqlite3_test_control_pending_byte does, so callers can restore it.
func (db *DB) SetPendingByte(byteOffset uint32) uint32 {
	if db != nil && db.engine != nil {
		return db.engine.SetPendingByteMain(byteOffset)
	}
	return 0x40000000
}

// BeginActiveStatement marks the start of a harness-emulated active read
// statement. Upstream, a sqlite3_stmt that has returned SQLITE_ROW stays in
// RUN state until it is finalized, and DROP TABLE/DROP INDEX executed while
// such a statement exists fails with SQLITE_LOCKED "database table is locked"
// (src/vdbe.c OP_Destroy: db->nVdbeRead > db->nVDestroy+1). The Go harness
// materializes query rows, so a db-eval callback loop that executes DDL wraps
// the loop with Begin/EndActiveStatement to reproduce that behavior.
func (db *DB) BeginActiveStatement() {
	if db != nil && db.engine != nil {
		db.engine.BeginActiveStatement()
	}
}

// EndActiveStatement ends a harness-emulated active read statement opened by
// BeginActiveStatement.
func (db *DB) EndActiveStatement() {
	if db != nil && db.engine != nil {
		db.engine.EndActiveStatement()
	}
}

// SetExprDepthLimit sets the maximum view/subquery nesting depth
// (SQLITE_LIMIT_EXPR_DEPTH). A negative value queries (and returns) the
// current limit without changing it.
func (db *DB) SetExprDepthLimit(n int) int {
	if db != nil && db.engine != nil {
		return db.engine.SetExprDepthLimit(n)
	}
	return 0
}

// SetReservedBytes sets the per-page reserved-space byte count (the database
// header's byte 20; sqlite3_file_control SQLITE_FCNTL_RESERVE_BYTES). The
// btree usable size becomes page-size minus this value; the change is
// flushed to the header with the next write.
func (db *DB) SetReservedBytes(n int) {
	if db != nil && db.engine != nil {
		if pg := db.engine.Pager(); pg != nil {
			pg.SetReservedBytes(uint32(n))
		}
	}
}

// SetTriggerDepthLimit sets the maximum trigger nesting depth
// (SQLITE_LIMIT_TRIGGER_DEPTH). A negative value queries (and returns) the
// current limit without changing it.
func (db *DB) SetTriggerDepthLimit(n int) int {
	if db != nil && db.engine != nil {
		return db.engine.SetTriggerDepthLimit(n)
	}
	return 0
}

// Limit returns the current value of a named SQLite limit (e.g.
// "SQLITE_LIMIT_ATTACHED"). Unknown limits return 0.
func (db *DB) Limit(name string) int {
	if db != nil && db.engine != nil {
		return db.engine.Limit(name)
	}
	return 0
}

// SetLimit sets a named SQLite runtime limit (SQLITE_LIMIT_COLUMN,
// SQLITE_LIMIT_LENGTH). A negative value queries the current limit without
// changing it. A raise above the compile-time default is capped at the
// default.
func (db *DB) SetLimit(name string, n int) int {
	if db != nil && db.engine != nil {
		return db.engine.SetLimit(name, n)
	}
	return 0
}

// SetProgressHandler registers a progress callback invoked after every n
// engine operations. A true return interrupts the running statement with an
// "interrupted" error (SQLite sqlite3_progress_handler).
func (db *DB) SetProgressHandler(n int, fn func() bool) {
	if db != nil && db.engine != nil {
		db.engine.SetProgressHandler(n, fn)
	}
}

// SetInterruptCount arms SQLite's SQLITE_TEST interrupt countdown
// (::sqlite_interrupt_count, src/vdbe.c:68 + src/test1.c:9316): n > 0
// interrupts the connection after n engine operations (the running statement
// fails with "interrupted"); n <= 0 disables it. The leftover count stays
// readable via InterruptCount after a statement finishes.
func (db *DB) SetInterruptCount(n int) {
	if db != nil && db.engine != nil {
		db.engine.SetInterruptCount(n)
	}
}

// InterruptCount returns the leftover SQLITE_TEST interrupt countdown — the
// value the TCL harness reads from ::sqlite_interrupt_count after a statement
// to learn how many engine operations it consumed.
func (db *DB) InterruptCount() int {
	if db != nil && db.engine != nil {
		return db.engine.InterruptCount()
	}
	return 0
}

// Interrupt sets the connection's interrupt flag (sqlite3_interrupt). The
// next statement executed on this connection fails with an "interrupted"
// error and the flag is consumed.
func (db *DB) Interrupt() {
	if db != nil && db.engine != nil {
		db.engine.Interrupt()
	}
}

// IsInterrupted reports whether the connection's interrupt flag is currently
// set (sqlite3_is_interrupted).
func (db *DB) IsInterrupted() bool {
	if db != nil && db.engine != nil {
		return db.engine.IsInterrupted()
	}
	return false
}

// ClearInterrupt clears the connection's interrupt flag without running a
// statement (used by the test harness after a db-eval callback aborts).
func (db *DB) ClearInterrupt() {
	if db != nil && db.engine != nil {
		db.engine.ClearInterrupt()
	}
}

// SetDQS configures SQLite's double-quoted-string (DQS) behavior.
// ddl=true allows double-quoted strings in DDL statements (CREATE TABLE
// CHECK/DEFAULT expressions, CREATE INDEX keys); dml=true allows them in DML
// (SELECT/INSERT/UPDATE expressions). Both default to true, matching SQLite.
// When disabled, an unresolved double-quoted identifier is an error
// ("no such column: \"X\" - should this be a string literal in single-quotes?").
func (db *DB) SetDQS(ddl, dml bool) {
	if db != nil && db.engine != nil {
		db.engine.SetDQS(ddl, dml)
	}
}

// SetDefensive mirrors SQLITE_DBCONFIG_DEFENSIVE: when enabled, certain
// write operations (e.g. PRAGMA schema_version=...) are ignored.
func (db *DB) SetDefensive(enabled bool) {
	if db != nil && db.engine != nil {
		db.engine.SetDefensive(enabled)
	}
}

// SetQPSG mirrors SQLITE_DBCONFIG_ENABLE_QPSG (query planner stability
// guarantee): when enabled, the LIKE optimization does not examine
// bound-parameter patterns (whereexpr.c isLikeOrGlob's TK_VARIABLE branch).
func (db *DB) SetQPSG(enabled bool) {
	if db != nil && db.engine != nil {
		db.engine.SetQPSG(enabled)
	}
}
