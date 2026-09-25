// Package main implements the tcl2go tool.
//
// This file emits the frigolite.Open calls for `sqlite3 NAME FILE` (split
// from processsqlite3.go for file-size and complexity hygiene): the
// connection-kind dispatch (predeclared dbN, main-db reset modes, new
// variables, closed-then-reopen, temp side-effect opens) and the deferred
// tclConnRegister bookkeeping.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// connReg is a pending (dbName, goName) pair queued for tclConnRegister
// emission after the current sqlite3 NAME FILE call site.
type connReg struct {
	dbName string
	goName string
}

// queueConnRegister queues a (dbName, goName) pair for the deferred
// tclConnRegister emission.
func (tp *transpiler) queueConnRegister(dbName, goName string) {
	tp.pendingConnRegister = append(tp.pendingConnRegister, connReg{dbName: dbName, goName: goName})
}

// flushPendingConnRegisters emits the queued tclConnRegister calls and
// clears the queue (emitSqlite3Open tail bookkeeping).
func (tp *transpiler) flushPendingConnRegisters() {
	for _, r := range tp.pendingConnRegister {
		tp.emitLine("tclConnRegister(%q, %s)", r.dbName, r.goName)
	}
	tp.pendingConnRegister = nil
}

// noteConnVar records that goName holds a *frigolite.DB connection so
// execsql/db dispatch resolves it as a connection rather than a string
// variable, and reports whether it was already recorded.
func (tp *transpiler) noteConnVar(goName string) bool {
	wasOpened := tp.dbConnVars[goName]
	if tp.dbConnVars == nil {
		tp.dbConnVars = make(map[string]bool)
	}
	tp.dbConnVars[goName] = true
	return wasOpened
}

// resetDQS marks double-quoted-string DDL/DML as reset to SQLite defaults
// (a fresh connection).
func (tp *transpiler) resetDQS() {
	tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
	tp.dqsDML = true
}

// emitSqlite3Open emits the frigolite.Open call for a sqlite3 connection,
// dispatching on the connection kind (predeclared dbN, main db reset modes,
// new variable, closed-then-reopen, or already-declared). Returns true when
// the caller should emit the trailing t.Fatal(err) check.
//
// Every Open call is followed by a tclConnRegister(dbName, X) so the runtime
// dispatch (tclConnByName) can resolve arbitrary connection names like
// "db1a"/"db2a" used in foreach dispatch (quota-3.2.1's
// "foreach db {db1a db2a} { execsql {...} $db }").
func (tp *transpiler) emitSqlite3Open(dbName, goName, filename, rawFilename string, args []tcl.RawWord) bool {
	defer tp.flushPendingConnRegisters()
	// `sqlite3 db FILE -readonly 1` — SQLITE_OPEN_READONLY
	// (tkt-5ee23731f-1.1): every write on the returned connection fails with
	// "attempt to write a readonly database". A missing file is an open
	// error (no create).
	if res, ok := tp.emitSqlite3ReadonlyOpen(dbName, goName, filename, args); ok {
		return res
	}
	// Record that goName holds a *frigolite.DB connection so execsql/db
	// dispatch resolves it as a connection rather than a string variable.
	wasOpened := tp.noteConnVar(goName)
	// db1-db9 are pre-declared at function level; always use = for them
	if isPreDeclaredDB(goName) {
		tp.emitLine("%s, err = frigolite.Open(%s)", goName, filename)
		tp.queueConnRegister(dbName, goName)
		return true
	}
	if res, ok := tp.emitSqlite3ResetReopen(dbName, goName, filename, args); ok {
		return res
	}
	if !tp.isVarDeclared(goName) {
		// New DB connection variable
		tp.emitLine("%s, err := frigolite.Open(%s)", goName, filename)
		tp.emitLine("defer %s.Close()", goName)
		tp.vars = append(tp.vars, goName)
		tp.queueConnRegister(dbName, goName)
		return true
	}
	if res, ok := tp.emitSqlite3PreambleOpen(dbName, goName, filename, wasOpened); ok {
		return res
	}
	if res, ok := tp.emitSqlite3ReopenOpen(dbName, goName, filename, rawFilename); ok {
		return res
	}
	return tp.emitSqlite3TmpOpen(goName, filename)
}

// emitSqlite3ReadonlyOpen handles `sqlite3 NAME FILE -readonly 1`
// (SQLITE_OPEN_READONLY). Reports (result, handled).
func (tp *transpiler) emitSqlite3ReadonlyOpen(dbName, goName, filename string, args []tcl.RawWord) (result, handled bool) {
	for i := 2; i+1 < len(args); i++ {
		if strings.TrimSpace(args[i].Text) == "-readonly" && strings.TrimSpace(args[i+1].Text) == "1" {
			tp.emitLine("%s, err = frigolite.OpenReadOnly(%s)", goName, filename)
			if tp.catchMode {
				tp.emitLine("if err != nil { _catchErr = err; %s = nil }", goName)
			} else {
				tp.emitLine("if err != nil { t.Fatal(err) }")
			}
			tp.dbConnVars[goName] = true
			tp.queueConnRegister(dbName, goName)
			return true, true
		}
	}
	return false, false
}

// emitSqlite3ResetReopen handles the main-connection reset forms: a reopen
// on :memory: (or empty) starts from a fresh database, and a reopen after a
// forcedelete starts from a fresh file database. Reports (result, handled).
func (tp *transpiler) emitSqlite3ResetReopen(dbName, goName, filename string, args []tcl.RawWord) (result, handled bool) {
	if goName == "db" && (filename == `""` || filename == `":memory:"` || filename == `"'':memory:''"`) {
		// SQLite's "db close; sqlite3 db :memory:" resets the main test
		// connection to a fresh database (dropping all prior tables).
		// Reopen it empty. (The preceding "db close" already emitted Close.)
		tp.emitLine("db, err = frigolite.Open(\"\")")
		tp.resetDQS()
		tp.queueConnRegister(dbName, goName)
		return true, true
	}
	if goName == "db" && len(args) >= 2 && tp.pendingFileReset[args[1].Text] {
		// "forcedelete test.db; sqlite3 db test.db": start from a fresh
		// database on the real file (deleted by forcedelete, recreated
		// empty by the reopen). Reopening on the actual filename matters:
		// a later "db close; sqlite3 db test.db" must find writes made
		// after the reset, matching SQLite's file-based close+reopen
		// semantics (see default-4.0/default-4.1).
		delete(tp.pendingFileReset, args[1].Text)
		tp.emitLine("db, err = frigolite.Open(%s)", filename)
		tp.resetDQS()
		tp.queueConnRegister(dbName, goName)
		return true, true
	}
	return false, false
}

// emitSqlite3PreambleOpen opens a named connection pre-declared in the
// preamble (`sqlite3 tmp ""`) that has not been opened yet: the var is in
// tp.vars (predeclared) but not in dbConnVars (never opened). Reports
// (result, handled).
func (tp *transpiler) emitSqlite3PreambleOpen(dbName, goName, filename string, wasOpened bool) (result, handled bool) {
	if goName == "db" || isPreDeclaredDB(goName) || !tp.isVarDeclared(goName) || wasOpened {
		return false, false
	}
	tp.emitLine("%s, err = frigolite.Open(%s)", goName, filename)
	if tp.catchMode {
		tp.emitLine("if err != nil { _catchErr = err; %s = nil } else { tclConnRegister(%q, %s) }", goName, dbName, goName)
	} else {
		tp.emitLine("if err != nil { t.Fatal(err) }")
	}
	tp.queueConnRegister(dbName, goName)
	return true, true
}

// emitSqlite3ReopenOpen handles the main-connection reopen forms: after a
// `db close` (reopen on the same file so prior writes persist) and inside an
// eval-inlined script or a variable filename (a real close+reopen — the TCL
// replaces the connection). Reports (result, handled).
func (tp *transpiler) emitSqlite3ReopenOpen(dbName, goName, filename, rawFilename string) (result, handled bool) {
	if goName == "db" && tp.dbClosed {
		// "db close" then "sqlite3 db <file>": the main connection was
		// closed, so reopen it on the same file so prior writes persist
		// (matching SQLite's close+reopen semantics). The compat suite
		// runs in-memory; the filename keeps the logical database alive.
		tp.emitLine("db, err = frigolite.Open(%s)", filename)
		tp.resetDQS()
		tp.dbClosed = false
		tp.queueConnRegister(dbName, goName)
		return true, true
	}
	// Variable already declared (possibly as string from set) —
	// use a temp variable to avoid type conflicts. Reopening a FILE
	// database ("sqlite3 db test.db") is a no-op: the compat suite
	// expects the test to keep running in-memory, and forcedelete
	// emits os.Remove for explicit resets. Inside an eval-inlined script
	// (backup.test's `eval $zOpenScript` with `sqlite3 db $zSrcFile`), or
	// when the filename is a variable (backup-10's `sqlite3 db $file` in a
	// foreach), the reopen must create a FRESH connection (the TCL replaces
	// the connection), so emit a real close+reopen.
	if goName == "db" && (tp.inEvalScript || strings.HasPrefix(strings.TrimSpace(rawFilename), "$")) {
		tp.emitLine("db.Close()")
		tp.emitLine("db, err = frigolite.Open(%s)", filename)
		if tp.catchMode {
			tp.emitLine("if err != nil { _catchErr = err; db = nil } else { tclConnRegister(%q, db) }", dbName)
		} else {
			tp.emitLine("if err != nil { t.Fatal(err) }")
		}
		tp.resetDQS()
		tp.queueConnRegister(dbName, goName)
		return false, true
	}
	return false, false
}

// emitSqlite3TmpOpen emits the temp-variable side-effect open (a reopen
// whose handle is discarded), resetting the changes() counters when the main
// connection is reopened ("sqlite3 db test.db" — e_totalchanges.test). It
// always returns false (no trailing t.Fatal check at the call site).
func (tp *transpiler) emitSqlite3TmpOpen(goName, filename string) bool {
	tmpVar := fmt.Sprintf("_dbtmp%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s, err := frigolite.Open(%s)", tmpVar, filename)
	tp.emitLine("_ = %s // sqlite3 db connection", tmpVar)
	tp.emitLine("if err != nil { t.Logf(\"open connection side effect failed: %%v (not fatal)\", err) }")
	tp.emitLine("_ = err")
	// Reopening the MAIN connection ("sqlite3 db test.db") creates a fresh
	// sqlite3 handle whose changes()/total_changes() counters start at zero
	// (e_totalchanges.test resets total_changes this way). The in-memory
	// engine keeps the same DB handle for schema/data continuity, so reset
	// the counters explicitly.
	if goName == "db" {
		tp.emitLine("db.ResetChangesCounters()")
	}
	return false
}
