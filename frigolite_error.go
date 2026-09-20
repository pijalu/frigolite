// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

// SQLITE_* result-code classification for engine errors
// (sqlite3_errcode emulation).
import (
	"errors"
	"strings"
)

// ErrorCodeFor returns the SQLITE_* result code string the connection
// would report for err (sqlite3_errcode emulation). Exported for generated
// C-API tests that record an error with SetLastErr but need the proper
// result code (SetLastErr itself does not classify messages).
func (db *DB) ErrorCodeFor(err error) string {
	return db.errorCode(err)
}

// errorCode maps an engine error to the SQLITE_* result code string reported
// by sqlite3_errcode for that failure. The mapping is heuristic, based on the
// error message prefix, matching the SQLite result codes the C-API tests
// assert (capi2/capi3/capi3c). Constraint and DML errors surface as
// SQLITE_ERROR from sqlite3_step (the C API reports the specific extended
// code only from sqlite3_finalize / sqlite3_extended_errcode). The lock /
// snapshot / open-file family is classified by lockFamilyErrorCode first.
// specialErrorCode classifies the families checked BEFORE the message switch
// so their codes win: the lock/snapshot/open-file family and the step-time
// halt carrier. The carrier reports the generic SQLITE_ERROR sqlite3_step
// returns for a constraint halt (vdbe.c OP_Halt "rc = p->rc ? SQLITE_ERROR :
// SQLITE_DONE"); the wrapped original classifies to the specific code on the
// finalize path (stmt.go stepHaltError).
func (db *DB) specialErrorCode(err error) (string, bool) {
	if code, ok := lockFamilyErrorCode(err.Error()); ok {
		return code, true
	}
	var halt stepHaltError
	if errors.As(err, &halt) {
		return "SQLITE_ERROR", true
	}
	return "", false
}

func (db *DB) errorCode(err error) string {
	if err == nil {
		return "SQLITE_OK"
	}
	if code, ok := db.specialErrorCode(err); ok {
		return code
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "malformed"):
		// "database disk image is malformed" — SQLite's SQLITE_CORRUPT
		// message (btree.c SQLITE_CORRUPT_BKPT). Checked before the generic
		// clauses so any malformed-database error reports SQLITE_CORRUPT
		// (sqlite3_errcode, incrcorrupt-1.4).
		return "SQLITE_CORRUPT"
	case strings.Contains(msg, "interrupted"):
		return "SQLITE_INTERRUPT"
	case strings.Contains(msg, "database schema has changed"):
		return "SQLITE_SCHEMA"
	case strings.Contains(msg, "misuse"), strings.Contains(msg, "bad parameter"),
		strings.Contains(msg, "no more rows available"):
		// vdbeapi.c: stepping a completed statement without a reset reports
		// SQLITE_MISUSE ("no more rows available" state text).
		return "SQLITE_MISUSE"
	case strings.Contains(msg, "column index out of range"):
		// vdbeapi.c SQLITE_RANGE from sqlite3_bind_* with an index outside
		// 1..sqlite3_bind_parameter_count.
		return "SQLITE_RANGE"
	case strings.Contains(msg, "out of memory"):
		return "SQLITE_NOMEM"
	case strings.Contains(msg, "no such table"), strings.Contains(msg, "no such column"),
		strings.Contains(msg, "syntax error"), strings.Contains(msg, "near "):
		return "SQLITE_ERROR"
	case strings.Contains(msg, "constraint"), strings.Contains(msg, "UNIQUE"),
		strings.Contains(msg, "NOT NULL"), strings.Contains(msg, "CHECK"),
		strings.Contains(msg, "FOREIGN KEY"), strings.Contains(msg, "PRIMARY KEY"):
		// Constraint violations report SQLITE_CONSTRAINT (sqlite3_errcode
		// after a NOT NULL/UNIQUE/CHECK/FK failure: vdbe.c OP_Halt carries
		// P2=SQLITE_CONSTRAINT; the commit-hook abort in vdbe.c reports the
		// bare "constraint failed" message with the same code).
		// altercons-5.2.2 asserts the code via the TCL errorcode fixture.
		return "SQLITE_CONSTRAINT"
	case strings.Contains(msg, "too big"), strings.Contains(msg, "string or blob too big"):
		// vdbemem.c SQLITE_TOOBIG: "string or blob too big" from sqlite3_bind_*
		// for a value whose byte length exceeds SQLITE_LIMIT_LENGTH
		// (sqllimits1-5.14.x).
		return "SQLITE_TOOBIG"
	default:
		return "SQLITE_ERROR"
	}
}

// lockFamilyErrorCode classifies the lock/busy/snapshot/open-file error
// family (checked before the generic switch so the extended codes win):
// SQLITE_BUSY ("database is locked" / "busy"), SQLITE_CANTOPEN, and the
// SQLITE_ERROR_SNAPSHOT carrier (see SnapshotOpen).
func lockFamilyErrorCode(msg string) (string, bool) {
	switch {
	case strings.Contains(msg, "snapshot is out of date"):
		// SQLITE_ERROR_SNAPSHOT (wal.c L3433/L4582 via sqlite3_errcode): a
		// snapshot can no longer be opened because the WAL was wrapped or
		// checkpointed past it. C defines no message text for the extended
		// code; "snapshot is out of date" is frigolite's carrier.
		return "SQLITE_ERROR_SNAPSHOT", true
	case strings.Contains(msg, "database is locked"), strings.Contains(msg, "busy"):
		return "SQLITE_BUSY", true
	case strings.Contains(msg, "unable to open database file"), strings.Contains(msg, "no such file"),
		strings.Contains(msg, "unable to open"):
		return "SQLITE_CANTOPEN", true
	}
	return "", false
}
