package frigolite

// Native anchor for the P6.FTS5 fts5restart.test adjudication
// (2026-09-14). The TCL file's red assertions are cursor-model artifacts,
// not engine gaps: 1.4.x needs db2's half-stepped MATCH cursor to hold a
// read transaction while db runs the 'optimize' special command, and 4.x
// needs mid-scan visibility of same-connection DELETEs — both assume C's
// incremental sqlite3_step cursor model, which the materializing Go
// public API cannot express (Query runs to completion before the caller
// sees rows). Sections 2.x additionally drive sqlite3_prepare/step
// directly (test1.c C-API class).
//
// The engine-visible contract underneath IS reproducible across
// connections and is pinned here (oracle: /usr/bin/sqlite3 3.51.0 and the
// TCL want of 1.4.2): a writer's 'optimize' against a table another
// connection is reading fails with "database is locked", succeeds once
// the reader's transaction ends, and never disturbs committed query
// results. Model: rollback-journal mode (the test default).

import (
	"path/filepath"
	"testing"
)

// TestFTS5OptimizeVsConcurrentReader ports fts5restart.test 1.4: while
// db2 holds an open read transaction over an fts5 table, db's
// INSERT ... VALUES('optimize') fails with "database is locked"; after
// the reader rolls back, optimize succeeds and the index stays correct.
func TestFTS5OptimizeVsConcurrentReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restart.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE f1 USING fts5(ff)"))
	for i := 0; i < 1000; i++ {
		checkExecOK(t, db.Exec("INSERT INTO f1 VALUES('a b c d e')"))
	}
	checkQueryResult(t, db.Query("SELECT count(*) FROM f1 WHERE f1 MATCH 'c'"), "1000")

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	checkExecOK(t, db2.Exec("BEGIN"))
	checkQueryResult(t, db2.Query("SELECT count(*) FROM f1 WHERE f1 MATCH 'c'"), "1000")

	checkExecError(t, db.Exec("INSERT INTO f1(f1) VALUES('optimize')"),
		"database is locked")

	// The blocked writer must not have corrupted the index.
	checkQueryResult(t, db.Query("SELECT count(*) FROM f1 WHERE f1 MATCH 'c'"), "1000")

	// Once the reader's transaction ends, optimize proceeds.
	checkExecOK(t, db2.Exec("ROLLBACK"))
	checkExecOK(t, db.Exec("INSERT INTO f1(f1) VALUES('optimize')"))
	checkQueryResult(t, db.Query("SELECT count(*) FROM f1 WHERE f1 MATCH 'c'"), "1000")
	db.Close()
}
