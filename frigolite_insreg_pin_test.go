package frigolite_test

// FIX.INSREG regression pins: the PERF.INSQUICK-2 reusable insert Results
// (insRowRes/insStmtRes) are only staged at INSERT nesting depth 1. A nested
// INSERT issued by the statement's own side work (fts5 statement-end shadow
// flush, triggers, FK actions) must build its Result fresh, or the engine's
// execTrackChanges publishes the nested write's rowid/changes for the outer
// statement.

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestInsRegPin_FTS5LastRowID pins last_insert_rowid() across fts5 inserts
// (fts5lastrowid 1.1/1.3; sqlite3 oracle: CREATE VIRTUAL TABLE t1 USING
// fts5(str); 3x INSERT ... VALUES -> 3; INSERT (rowid,-22) -> -22).
func TestInsRegPin_FTS5LastRowID(t *testing.T) {
	db, err := frigolite.Open(t.TempDir() + "/fts5lastrowid.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mustQuery := func(sql, want string) {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("query error: %v\n  sql: %s", r.Error, sql)
		}
		got := ""
		for i, row := range r.Rows {
			if i > 0 {
				got += " "
			}
			for j, v := range row {
				if j > 0 {
					got += " "
				}
				got += fmt.Sprintf("%v", v)
			}
		}
		if got != want {
			t.Errorf("result mismatch\n  got:  [%s]\n  want: [%s]\n  sql: %s", got, want, sql)
		}
	}

	if r := db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(str);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Autocommit inserts flush the %_data shadow write at statement end: the
	// nested INSERT must not overwrite the statement's own last-insert-rowid.
	mustQuery("INSERT INTO t1 VALUES('one string');"+
		" INSERT INTO t1 VALUES('two string');"+
		" INSERT INTO t1 VALUES('three string');"+
		" SELECT last_insert_rowid();", "3")
	// Explicit (including negative) fts5 rowids propagate unchanged.
	mustQuery("INSERT INTO t1(rowid, str) VALUES(-22, 'some more text');"+
		" SELECT last_insert_rowid();", "-22")
}

// TestInsRegPin_SpellfixRowidConflict pins the UNIQUE rowid constraint on a
// spellfix1 shadow table (spellfix 7.4.2; TCL contract: an INSERT with
// explicit rowids that collides fails with "constraint failed" and leaves
// the table unchanged). The poison window matters: a schema change
// (CREATE TABLE) resets the largest-rowid cache, a low explicit rowid must
// not re-seed it, and the shadow table's IPK uniqueness probe must still
// reject rowids above the re-seeded value.
func TestInsRegPin_SpellfixRowidConflict(t *testing.T) {
	db, err := frigolite.Open(t.TempDir() + "/spellfixrowid.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mustExec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}
	mustExec("CREATE VIRTUAL TABLE t4 USING spellfix1;")
	mustExec("INSERT INTO t4(rowid, word) VALUES(10, 'Agamemnon');")
	mustExec("INSERT INTO t4(rowid, word) VALUES(20, 'Patroclus');")
	mustExec("INSERT INTO t4(rowid, word) VALUES(30, 'Chryses');")
	// The schema change invalidates the rowid cache mid-scenario (the
	// transpiled test creates t5 between the seed and the conflicting
	// INSERT — exactly the window where a bogus cache re-seed hides).
	mustExec("CREATE TABLE t5(i, w);")
	mustExec("INSERT INTO t5 VALUES(5, 'Poseidon');")
	mustExec("INSERT INTO t5 VALUES(20, 'Chronos');")
	mustExec("INSERT INTO t5 VALUES(30, 'Hera');")

	r := db.Query("INSERT INTO t4(rowid, word) SELECT i, w FROM t5")
	wantErr := "constraint failed"
	if r.Error == nil {
		t.Fatalf("insert accepted duplicate rowids, want %q error\n  sql: %s", wantErr, "INSERT INTO t4(rowid, word) SELECT i, w FROM t5")
	}
	r = db.Query("SELECT rowid, word FROM t4")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got := ""
	for i, row := range r.Rows {
		if i > 0 {
			got += " "
		}
		for j, v := range row {
			if j > 0 {
				got += " "
			}
			got += fmt.Sprintf("%v", v)
		}
	}
	if want := "10 Agamemnon 20 Patroclus 30 Chryses"; got != want {
		t.Errorf("table changed by failed insert\n  got:  [%s]\n  want: [%s]", got, want)
	}
}
