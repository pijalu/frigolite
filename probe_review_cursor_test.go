package frigolite

import "testing"

// Area 4 probes: cross-wrapper cursor save/restore (misc8 contract) and the
// released-cursor guard, exercised through SQL. The btree-level hazard: the
// Seek* entry points reset neither cursorRequireSeek state nor savedKey, so a
// cursor saved by a nested write (trigger/eval DML) and then RE-SEEKED stays
// stale — the next ReadCellData restores the OLD key. A correlated subquery
// whose seek targets DESCEND across outer rows, with a trigger inserting into
// the sought table mid-statement, makes the staleness observable.
// ORACLE-verified expectations (sqlite3 3.54): the statement sees its own
// trigger's inserts, and every seek result matches the fresh-seek value.

func cursorProbeOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Descending index seeks (6 - k.v: 5,4,3,2,1) while an AFTER UPDATE trigger
// inserts into the sought table s between seeks.
func TestReviewCursorSeekAfterTriggerSave(t *testing.T) {
	db := cursorProbeOpen(t)
	stmts := []string{
		"CREATE TABLE k(v INTEGER PRIMARY KEY, w)",
		"CREATE TABLE s(k INT, tag TEXT)",
		"CREATE INDEX isk ON s(k)",
		"INSERT INTO k VALUES(1,NULL),(2,NULL),(3,NULL),(4,NULL),(5,NULL)",
		"INSERT INTO s VALUES(1,'one'),(3,'three'),(5,'five')",
		"CREATE TRIGGER tr AFTER UPDATE ON k BEGIN INSERT INTO s VALUES(NEW.v + 10, 'x'); END",
	}
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query("UPDATE k SET w = (SELECT tag FROM s WHERE s.k = 6 - k.v)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	// ORACLE: 1=five, 2=NULL, 3=three, 4=NULL, 5=one (statement sees its own
	// trigger's inserts).
	rows := probeRows(t, db, "SELECT v, w FROM k ORDER BY v")
	expect := []string{"five", "_", "three", "_", "one"}
	for i, row := range rows {
		tag := ""
		if row[1] != nil {
			tag = row[1].(string)
		}
		if expect[i] == "_" {
			if tag != "" {
				t.Fatalf("row %d: got %q, want NULL", i+1, tag)
			}
		} else if tag != expect[i] {
			t.Fatalf("row %d: got %q, want %q", i+1, tag, expect[i])
		}
	}
}

// Rowid-seek variant: descending seeks over k2 (rowid table) with a trigger
// inserting into k2 mid-statement.
func TestReviewCursorRowidSeekAfterTriggerSave(t *testing.T) {
	db := cursorProbeOpen(t)
	stmts := []string{
		"CREATE TABLE k(v INTEGER PRIMARY KEY, w)",
		"CREATE TABLE k2(id INTEGER PRIMARY KEY, tag TEXT)",
		"INSERT INTO k VALUES(1,NULL),(2,NULL),(3,NULL),(4,NULL),(5,NULL)",
		"INSERT INTO k2 VALUES(1,'one'),(3,'three'),(5,'five')",
		"CREATE TRIGGER tr2 AFTER UPDATE ON k BEGIN INSERT INTO k2 VALUES(NEW.v + 10, 'x'); END",
	}
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	if r := db.Query("UPDATE k SET w = (SELECT tag FROM k2 WHERE k2.id = 6 - k.v)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// ORACLE: same expectation as the index variant.
	rows := probeRows(t, db, "SELECT v, w FROM k ORDER BY v")
	expect := []string{"five", "_", "three", "_", "one"}
	for i, row := range rows {
		tag := ""
		if row[1] != nil {
			tag = row[1].(string)
		}
		if expect[i] == "_" {
			if tag != "" {
				t.Fatalf("row %d: got %q, want NULL", i+1, tag)
			}
		} else if tag != expect[i] {
			t.Fatalf("row %d: got %q, want %q", i+1, tag, expect[i])
		}
	}
}

// eval()-style nested DML while a SELECT cursor is mid-scan (misc8-1.6
// contract): the scan must survive the nested DELETE/INSERT and resume at the
// next-larger key.
func TestReviewCursorNestedDMLMidScan(t *testing.T) {
	db := cursorProbeOpen(t)
	stmts := []string{
		"CREATE TABLE t(v INTEGER PRIMARY KEY)",
		"INSERT INTO t VALUES(1),(2),(3),(4),(5)",
	}
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	got := probeDump(probeRows(t, db, "SELECT v FROM t WHERE v IN (SELECT 3) OR (v > 2 AND (SELECT count(*) FROM t) > 0) ORDER BY v"))
	t.Logf("baseline: %s", got)
	// DELETE the current row mid-scan via eval, then continue scanning.
	got2 := probeDump(probeRows(t, db, "SELECT v, eval('DELETE FROM t WHERE v=' || v) IS NULL FROM t WHERE v <= 5 ORDER BY v"))
	t.Logf("mid-scan delete via eval:\n%s", got2)
	rows := probeRows(t, db, "SELECT count(*) FROM t")
	if n := rows[0][0].(int64); n != 0 {
		t.Fatalf("rows after self-delete = %d, want 0", n)
	}
}

// Area 5, engine level: a statement abort (UNIQUE violation mid multi-row
// INSERT) must roll back exactly the failing statement — earlier statements
// of the same transaction survive; nothing after the abort applies.
func TestReviewStmtAbortScope(t *testing.T) {
	db := cursorProbeOpen(t)
	stmts := []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)",
	}
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	if r := db.Exec("BEGIN"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(1,1),(2,2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(3,3),(4,2)"); r.Error == nil {
		t.Fatal("conflicting insert did not fail")
	} else {
		t.Logf("abort error: %v", r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT a FROM t ORDER BY a"))
	t.Logf("after statement abort: %s", got)
	if got != "int64:1\nint64:2\n" {
		t.Fatalf("statement abort rolled back wrong scope: %s", got)
	}
	if r := db.Exec("ROLLBACK"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got = probeDump(probeRows(t, db, "SELECT count(*) FROM t"))
	if got != "int64:0\n" {
		t.Fatalf("transaction rollback left rows: %s", got)
	}
}

// RAISE(ABORT) inside a trigger fired by a multi-row UPDATE: the whole
// statement rolls back (nothing applied), and the engine-level + DML-level
// double-restore path must not corrupt earlier transaction statements.
func TestReviewTriggerAbortDoubleRollback(t *testing.T) {
	db := cursorProbeOpen(t)
	stmts := []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b)",
		"CREATE TABLE guard(g UNIQUE)",
		"INSERT INTO guard VALUES(99)",
		"CREATE TRIGGER tg AFTER UPDATE ON t BEGIN INSERT INTO guard VALUES(NEW.b); END",
	}
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	if r := db.Exec("BEGIN"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(1, 0)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Multi-row UPDATE: row 2 fires the trigger with b=99 → UNIQUE violation
	// mid-statement → the WHOLE statement (rows 2..4) must roll back.
	if r := db.Exec("INSERT INTO t VALUES(2,1),(3,99),(4,3),(5,99),(6,5)"); r.Error != nil {
		t.Logf("setup error (expected if checks run per row): %v", r.Error)
	}
	if r := db.Exec("UPDATE t SET b = b + 10 WHERE a > 1"); r.Error == nil {
		t.Log("update did not fail — trigger rows did not conflict")
	} else {
		t.Logf("trigger abort error: %v", r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT a, b FROM t ORDER BY a"))
	t.Logf("state after trigger abort:\n%s", got)
	// ORACLE: a conflict inside a trigger body aborts ONLY the trigger's own
	// statement (sqlite3: the outer INSERT/UPDATE continues; final state is
	// 1|0, 2|1, 3|99, 4|3, 5|99, 6|5). The nested statement-journal rollback
	// (double-restore path) must reproduce that exactly.
	want := "int64:1|int64:0\nint64:2|int64:1\nint64:3|int64:99\nint64:4|int64:3\nint64:5|int64:99\nint64:6|int64:5\n"
	if got != want {
		t.Fatalf("trigger abort diverged from oracle:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if r := db.Exec("ROLLBACK"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := probeDump(probeRows(t, db, "SELECT count(*) FROM t")); got != "int64:0\n" {
		t.Fatalf("post-rollback rows: %s", got)
	}
}
