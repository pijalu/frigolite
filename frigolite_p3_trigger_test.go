package frigolite

import (
	"strings"
	"testing"
)

// P3 pre-tests: hand-written tests for G3.TRIGGER CREATE/DROP TRIGGER and
// trigger firing, written BEFORE running the TCL testgen packages (trigger,
// triggerA-G, temptrigger, triggerupfrom). Each expectation was verified
// against sqlite3 3.51 (via the sqlite3 CLI) as the oracle; the tests
// document the exact SQLite semantics frigolite must match: BEFORE/AFTER
// INSERT/UPDATE/DELETE triggers (including UPDATE OF <cols>), INSTEAD OF
// triggers on views, the OLD/NEW row contexts, WHEN clause gating, multi-
// statement bodies, body-error statement rollback, cross-table chaining with
// recursive_triggers OFF (same-table recursion blocked), TEMP trigger
// scoping, DROP TRIGGER, and the "triggers nested too deep" depth limit.

// TestP3Trigger is the top-level entry for the P3 TRIGGER pre-tests. The
// verify command runs it via `go test -run TestP3Trigger -count=1 .`
func TestP3Trigger(t *testing.T) {
	for _, sub := range []string{
		"Basics", "UpdateOf", "WhenClause", "MultiStatement",
		"ErrorRollback", "InsteadOf", "Chaining", "Recursion",
		"DropTrigger", "TempScoping",
	} {
		ok := t.Run(sub, func(t *testing.T) {
			switch sub {
			case "Basics":
				TestP3Trigger_Basics(t)
			case "UpdateOf":
				TestP3Trigger_UpdateOf(t)
			case "WhenClause":
				TestP3Trigger_WhenClause(t)
			case "MultiStatement":
				TestP3Trigger_MultiStatement(t)
			case "ErrorRollback":
				TestP3Trigger_ErrorRollback(t)
			case "InsteadOf":
				TestP3Trigger_InsteadOf(t)
			case "Chaining":
				TestP3Trigger_Chaining(t)
			case "Recursion":
				TestP3Trigger_Recursion(t)
			case "DropTrigger":
				TestP3Trigger_DropTrigger(t)
			case "TempScoping":
				TestP3Trigger_TempScoping(t)
			}
		})
		if !ok {
			t.Fail()
		}
	}
}

// TestP3Trigger_Basics covers BEFORE/AFTER INSERT/UPDATE/DELETE triggers with
// OLD/NEW access in the body.
func TestP3Trigger_Basics(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`)
	exec(`CREATE TABLE log(timing TEXT, event TEXT, olda TEXT, newa TEXT)`)
	exec(`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('before','insert',NULL,new.a); END`)
	exec(`CREATE TRIGGER ai AFTER INSERT ON t BEGIN INSERT INTO log VALUES('after','insert',NULL,new.a); END`)
	exec(`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES('before','update',old.a,new.a); END`)
	exec(`CREATE TRIGGER au AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('after','update',old.a,new.a); END`)
	exec(`CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('before','delete',old.a,NULL); END`)
	exec(`CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('after','delete',old.a,NULL); END`)

	exec(`INSERT INTO t VALUES(1,'x')`)
	exec(`UPDATE t SET b='y' WHERE a=1`)
	exec(`DELETE FROM t WHERE a=1`)

	got := flattenQuery(t, db, `SELECT timing,event,IFNULL(olda,'-'),IFNULL(newa,'-') FROM log ORDER BY rowid`)
	want := "before insert - 1 after insert - 1 before update 1 1 after update 1 1 before delete 1 - after delete 1 -"
	if got != want {
		t.Errorf("trigger log: got [%s] want [%s]", got, want)
	}
}

// TestP3Trigger_UpdateOf covers UPDATE OF <cols>: the trigger fires only when
// a listed column is actually changed.
func TestP3Trigger_UpdateOf(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t(a, b, c)`)
	exec(`CREATE TABLE log(x)`)
	exec(`CREATE TRIGGER tr BEFORE UPDATE OF b ON t BEGIN INSERT INTO log VALUES('b:'||old.b||'->'||new.b); END`)
	exec(`INSERT INTO t VALUES(1,2,3)`)

	// Fires when b changes.
	exec(`UPDATE t SET b=20 WHERE a=1`)
	if got := flattenQuery(t, db, `SELECT x FROM log`); got != "b:2->20" {
		t.Errorf("after b update: got [%s] want [b:2->20]", got)
	}

	// Does NOT fire when another column changes.
	exec(`UPDATE t SET a=10, c=30 WHERE a=1`)
	if got := flattenQuery(t, db, `SELECT x FROM log`); got != "b:2->20" {
		t.Errorf("after a/c update: got [%s] want [b:2->20] (trigger should not fire)", got)
	}

	// Fires when b changes to the same value (UPDATE OF is by column list,
	// not by value difference).
	exec(`UPDATE t SET b=20 WHERE a=10`)
	if got := flattenQuery(t, db, `SELECT count(*) FROM log`); got != "2" {
		t.Errorf("after b same-value update: got [%s] want [2]", got)
	}
}

// TestP3Trigger_WhenClause covers WHEN clause gating of trigger bodies.
func TestP3Trigger_WhenClause(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t(a)`)
	exec(`CREATE TABLE log(x)`)
	exec(`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.a > 5 BEGIN INSERT INTO log VALUES(new.a); END`)

	exec(`INSERT INTO t VALUES(1)`)
	exec(`INSERT INTO t VALUES(9)`)
	exec(`INSERT INTO t VALUES(6)`)

	got := flattenQuery(t, db, `SELECT x FROM log`)
	if got != "9 6" {
		t.Errorf("WHEN gating: got [%s] want [9 6]", got)
	}
}

// TestP3Trigger_MultiStatement covers a trigger body with multiple
// statements; every statement runs.
func TestP3Trigger_MultiStatement(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t(a)`)
	exec(`CREATE TABLE log(x)`)
	exec(`CREATE TRIGGER tr AFTER INSERT ON t BEGIN
		INSERT INTO log VALUES(new.a);
		INSERT INTO log VALUES(new.a*10);
		UPDATE log SET x = x || '!' WHERE x = new.a;
	END`)

	exec(`INSERT INTO t VALUES(3)`)
	got := flattenQuery(t, db, `SELECT x FROM log ORDER BY rowid`)
	if got != "3! 30" {
		t.Errorf("multi-statement body: got [%s] want [3! 30]", got)
	}
}

// TestP3Trigger_ErrorRollback covers a failing statement inside a trigger
// body rolling back the entire triggering statement.
func TestP3Trigger_ErrorRollback(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t(a UNIQUE)`)
	exec(`CREATE TABLE log(x)`)
	exec(`CREATE TRIGGER tr AFTER INSERT ON t BEGIN
		INSERT INTO log VALUES(new.a);
		SELECT RAISE(ABORT, 'boom');
	END`)

	r := db.Exec(`INSERT INTO t VALUES(5)`)
	if r.Error == nil || !strings.Contains(r.Error.Error(), "boom") {
		t.Fatalf("expected 'boom' error, got: %v", r.Error)
	}
	// Both the trigger's log row and the INSERT must be rolled back.
	if got := flattenQuery(t, db, `SELECT count(*) FROM log`); got != "0" {
		t.Errorf("log not rolled back: got [%s] want [0]", got)
	}
	if got := flattenQuery(t, db, `SELECT count(*) FROM t`); got != "0" {
		t.Errorf("t not rolled back: got [%s] want [0]", got)
	}
}

// TestP3Trigger_InsteadOf covers INSTEAD OF INSERT/UPDATE/DELETE triggers on
// views.
func TestP3Trigger_InsteadOf(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t(a, b)`)
	exec(`INSERT INTO t VALUES(1,2)`)
	exec(`CREATE VIEW v AS SELECT a, b FROM t`)
	exec(`CREATE TABLE log(x)`)

	// Without an INSTEAD OF trigger, modifying a view fails.
	r := db.Exec(`INSERT INTO v VALUES(9,9)`)
	if r.Error == nil {
		t.Errorf("insert into view without trigger: expected error, got success")
	}

	exec(`CREATE TRIGGER ins INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES('ins:'||new.a||','||new.b); END`)
	exec(`INSERT INTO v VALUES(9,9)`)
	if got := flattenQuery(t, db, `SELECT x FROM log`); got != "ins:9,9" {
		t.Errorf("INSTEAD OF INSERT: got [%s] want [ins:9,9]", got)
	}

	exec(`CREATE TRIGGER upd INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES('upd:'||old.a||','||old.b||'->'||new.a||','||new.b); END`)
	exec(`UPDATE v SET a=9 WHERE b=2`)
	if got := flattenQuery(t, db, `SELECT x FROM log ORDER BY rowid`); got != "ins:9,9 upd:1,2->9,2" {
		t.Errorf("INSTEAD OF UPDATE: got [%s] want [ins:9,9 upd:1,2->9,2]", got)
	}

	exec(`CREATE TRIGGER del INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES('del:'||old.a||','||old.b); END`)
	exec(`DELETE FROM v WHERE a=1`)
	if got := flattenQuery(t, db, `SELECT x FROM log ORDER BY rowid`); got != "ins:9,9 upd:1,2->9,2 del:1,2" {
		t.Errorf("INSTEAD OF DELETE: got [%s] want [ins:9,9 upd:1,2->9,2 del:1,2]", got)
	}
}

// TestP3Trigger_Chaining covers a trigger on one table firing a trigger on
// another table (cross-table chaining fires even with recursive_triggers
// OFF).
func TestP3Trigger_Chaining(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t1(a)`)
	exec(`CREATE TABLE t2(b)`)
	exec(`CREATE TABLE log(x)`)
	exec(`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN INSERT INTO t2 VALUES(new.a); END`)
	exec(`CREATE TRIGGER tr2 AFTER INSERT ON t2 BEGIN INSERT INTO log VALUES('t2 got: '||new.b); END`)

	exec(`INSERT INTO t1 VALUES(42)`)
	if got := flattenQuery(t, db, `SELECT x FROM log`); got != "t2 got: 42" {
		t.Errorf("cross-table chaining: got [%s] want [t2 got: 42]", got)
	}
}

// TestP3Trigger_Recursion covers recursive_triggers OFF (default): a trigger
// on table T does not re-fire when its body changes T again, but with
// recursive_triggers ON the recursion runs until the depth limit.
func TestP3Trigger_Recursion(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	// recursive_triggers OFF (default): the trigger body's INSERT into the
	// same table does NOT re-fire the trigger.
	exec(`CREATE TABLE t(a)`)
	exec(`CREATE TABLE log(x)`)
	exec(`CREATE TRIGGER tr AFTER INSERT ON t BEGIN
		INSERT INTO log VALUES(new.a);
		INSERT INTO t VALUES(new.a+1);
	END`)
	exec(`INSERT INTO t VALUES(1)`)
	if got := flattenQuery(t, db, `SELECT a FROM t ORDER BY a`); got != "1 2" {
		t.Errorf("recursion OFF t: got [%s] want [1 2]", got)
	}
	if got := flattenQuery(t, db, `SELECT x FROM log`); got != "1" {
		t.Errorf("recursion OFF log: got [%s] want [1]", got)
	}

	// recursive_triggers ON: the trigger re-fires until "triggers nested too
	// deep" (the depth limit).
	exec(`PRAGMA recursive_triggers=on`)
	exec(`DELETE FROM t`)
	exec(`DELETE FROM log`)
	r := db.Exec(`INSERT INTO t VALUES(1)`)
	if r.Error == nil || !strings.Contains(r.Error.Error(), "recursion") {
		t.Errorf("recursion ON: expected 'recursion' error, got: %v", r.Error)
	}
	// The statement is rolled back completely.
	if got := flattenQuery(t, db, `SELECT count(*) FROM t`); got != "0" {
		t.Errorf("recursion ON t: got [%s] want [0]", got)
	}
	if got := flattenQuery(t, db, `SELECT count(*) FROM log`); got != "0" {
		t.Errorf("recursion ON log: got [%s] want [0]", got)
	}
}

// TestP3Trigger_DropTrigger covers DROP TRIGGER and DROP TRIGGER IF EXISTS.
func TestP3Trigger_DropTrigger(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t(a)`)
	exec(`CREATE TABLE log(x)`)
	exec(`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`)

	exec(`INSERT INTO t VALUES(1)`)
	if got := flattenQuery(t, db, `SELECT x FROM log`); got != "1" {
		t.Fatalf("before drop: got [%s] want [1]", got)
	}

	exec(`DROP TRIGGER tr`)
	exec(`INSERT INTO t VALUES(2)`)
	if got := flattenQuery(t, db, `SELECT x FROM log`); got != "1" {
		t.Errorf("after drop: got [%s] want [1]", got)
	}

	// DROP TRIGGER IF EXISTS on a missing trigger succeeds.
	exec(`DROP TRIGGER IF EXISTS tr`)

	// DROP TRIGGER on a missing trigger fails.
	r := db.Exec(`DROP TRIGGER tr`)
	if r.Error == nil {
		t.Errorf("drop missing trigger: expected error, got success")
	}
}

// TestP3Trigger_TempScoping covers TEMP triggers: they fire on the matching
// table even when the table is in the main database.
func TestP3Trigger_TempScoping(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
		}
	}

	exec(`CREATE TABLE t(a)`)
	exec(`CREATE TABLE log(x)`)
	exec(`CREATE TEMP TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`)

	exec(`INSERT INTO t VALUES(7)`)
	if got := flattenQuery(t, db, `SELECT x FROM log`); got != "7" {
		t.Errorf("temp trigger firing: got [%s] want [7]", got)
	}

	// The TEMP trigger appears in sqlite_temp_master.
	if got := flattenQuery(t, db, `SELECT name FROM sqlite_temp_master WHERE type='trigger'`); got != "tr" {
		t.Errorf("temp master listing: got [%s] want [tr]", got)
	}
}
