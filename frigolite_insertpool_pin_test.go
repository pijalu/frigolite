package frigolite

// Pin for the VALUES-tuple scratch pool (execdml evalTuplePooled): a
// statement that nests another INSERT while its row values are in flight —
// a BEFORE INSERT trigger body writing a log table — must not observe (or
// inflict) scratch reuse. The outer row's values are consumed AFTER the
// nested statement returned, so the pool is gated off for trigger-bearing
// tables and FK-enforced connections; this pin holds the exact shape the
// pool corrupted before that gate (tkt3832's trigger INSERTs into a log
// table while the outer row is mid-insert).
import "testing"

func TestInsertTuplePoolNestedInsertPin(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	mustExec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %q: %v", sql, r.Error)
		}
	}
	// BEFORE INSERT trigger nests an INSERT into log while the outer row's
	// tuple is being validated and written.
	mustExec("CREATE TABLE t1(a INT, b INTEGER PRIMARY KEY)")
	mustExec("CREATE TABLE log(x)")
	mustExec("CREATE TRIGGER t1r1 BEFORE INSERT ON t1 BEGIN INSERT INTO log VALUES(new.a); END")
	mustExec("INSERT INTO t1 VALUES(NULL, 1)")
	mustExec("INSERT INTO t1 VALUES(NULL, 2)")
	if got, want := flattenQuery(t, db, "SELECT rowid, a, b FROM t1 ORDER BY rowid"), "1 NULL 1 2 NULL 2"; got != want {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	// new.a is NULL in both rows (the VALUES supply NULL for a) — the pin
	// is that the log rows are the OUTER rows' a values, not scratch
	// leakage from the nested INSERT's own tuple.
	if got, want := flattenQuery(t, db, "SELECT x FROM log ORDER BY rowid"), "NULL NULL"; got != want {
		t.Fatalf("log rows = %q, want %q", got, want)
	}

	// FK enforcement gates the pool too: with foreign_keys ON a REPLACE
	// conflict's delete can fire FK actions that nest DML on the same
	// executor while an outer row's tuple is in flight. Plain RESTRICT
	// semantics: the parent delete must fail and the child row survive.
	mustExec("PRAGMA foreign_keys=ON")
	mustExec("CREATE TABLE parent(id INTEGER PRIMARY KEY)")
	mustExec("CREATE TABLE child(id INTEGER PRIMARY KEY, pid REFERENCES parent(id))")
	mustExec("INSERT INTO parent VALUES(1)")
	mustExec("INSERT INTO child VALUES(10, 1)")
	if r := db.Exec("DELETE FROM parent WHERE id=1"); r.Error == nil {
		t.Fatalf("parent delete succeeded over a RESTRICT child reference")
	}
	if got, want := flattenQuery(t, db, "SELECT count(*) FROM child"), "1"; got != want {
		t.Fatalf("child rows = %q, want %q", got, want)
	}
}
