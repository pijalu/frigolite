package frigolite

// frigolite_stmtvtab1_test.go is the native Go port of SQLite's
// test/stmtvtab1.test. The TCL package testgen/stmtvtab1 is superseded by
// this file (see AGENTS.md "Pure-Go supersession").
//
// The TCL test exercises the sqlite_stmt eponymous virtual table
// (ext/misc/stmt.c, built when SQLITE_ENABLE_STMTVTAB is defined). That
// module walks the database connection's linked list of *live* Vdbe prepared
// statements and reports, per statement, the C sqlite3_stmt_status() counters
// (nscan/nsort/naidx/nstep/reprep/run/mem), sqlite3_stmt_busy(),
// sqlite3_stmt_readonly() and sqlite3_column_count(). Its schema is:
//
//	CREATE TABLE x(sql,ncol,ro,busy,nscan,nsort,naidx,nstep,reprep,run,mem)
//
// The TCL assertions pin C prepared-statement lifecycle behaviour, e.g. the
// REPREPARE counter incrementing when a schema change (CREATE INDEX t1b)
// forces the cached INSERT to be re-prepared, and the RUN counter
// accumulating across executions.
//
// frigolite is a pure-Go engine: it has no C Vdbe register machine, no
// per-connection linked list of live sqlite3_stmt handles, and no per-
// statement sqlite3_stmt_status() accounting (STMTSTATUS_MEMUSED etc.). The
// sqlite_stmt module is therefore a C-runtime introspection hook with no
// frigolite analogue — porting it means fabricating a C statement cache the
// engine does not have, not exposing a real database capability.
//
// Per the t14 design decision (plan/goals/P6.VTAB.md Session 12), this port
// documents the superseded C contract and pins the engine-gap boundary:
// frigolite rejects the unregistered sqlite_stmt module name with the same
// "no such table" the engine reports for any unknown relation, rather than
// emulating C-runtime statement counters.

import (
	"strings"
	"testing"
)

// TestStmtvtab1_ModuleNotRegistered pins the engine-gap boundary: sqlite_stmt
// is an eponymous C-API introspection module (SQLITE_ENABLE_STMTVTAB) that
// frigolite does not implement, so referencing it resolves to "no such table"
// like any other unknown relation.
func TestStmtvtab1_ModuleNotRegistered(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if res := db.Exec("CREATE TABLE t1(a,b,c)"); res.Error != nil {
		t.Fatalf("create t1: %v", res.Error)
	}

	// Querying the unregistered eponymous vtab reports an unknown table.
	res := db.Query("SELECT run, sql FROM sqlite_stmt ORDER BY 1")
	if res.Error == nil {
		t.Fatalf("expected error querying sqlite_stmt, got rows %v", res.Rows)
	}
	if !strings.Contains(res.Error.Error(), "sqlite_stmt") {
		t.Errorf("error %q should name the sqlite_stmt relation", res.Error)
	}

	// The engine itself remains fully functional around the rejected
	// introspection query (the TCL test's underlying DML/DDL contract).
	if res := db.Exec("INSERT INTO t1 VALUES(1,2,3)"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	if res := db.Exec("CREATE INDEX t1a ON t1(a)"); res.Error != nil {
		t.Fatalf("create index: %v", res.Error)
	}
	got := db.Query("SELECT count(*) FROM t1")
	if got.Error != nil {
		t.Fatalf("count: %v", got.Error)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("count rows = %v, want 1 row", got.Rows)
	}
}
