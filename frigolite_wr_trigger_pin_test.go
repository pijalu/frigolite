package frigolite

import "testing"

// Pin tests for the WITHOUT ROWID trigger-path class (FULL-SUITE-DRIFT.T26):
//  1. DROP TABLE (with triggers) followed by CREATE TABLE of the same name
//     must route the new trigger-less table through the plain DML paths —
//     a stale has-triggers flag made UPDATE corrupt WR rows through the
//     trigger merge path (without_rowid3-16.4.1.2, without_rowid4-2.x).
//  2. A real trigger on a WITHOUT ROWID table: the post-trigger row re-read
//     must match by OLD PK key and merge in declared column order, keeping
//     both the trigger's column write and the SET columns.

// TestPin_DropTableTriggerFlagReset covers fix 1: after dropping a table that
// had triggers, an UPDATE on a recreated same-name trigger-less WITHOUT ROWID
// table must write correct row values (no stale trigger-flag routing).
func TestPin_DropTableTriggerFlagReset(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	// Section 1: a table named t1 WITH a trigger (warms the has-triggers flag).
	mustExec(`CREATE TABLE t1(a PRIMARY KEY, b) WITHOUT rowid;`)
	mustExec(`CREATE TRIGGER t1_tr AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES(1); END;`)
	mustExec(`CREATE TABLE log(a);`)
	// Section 2: drop everything, recreate t1 WITHOUT triggers.
	mustExec(`DROP TABLE t1;`)
	mustExec(`CREATE TABLE t1(x, y, z);`)
	if r := db.Exec(`INSERT INTO t1 VALUES(7, 8, 9)`); r.Error != nil {
		t.Fatalf("insert: %v", r.Error)
	}
	// The UPDATE must go through the plain path and write exact values.
	if r := db.Exec(`UPDATE t1 SET y = 80`); r.Error != nil {
		t.Fatalf("update: %v", r.Error)
	}
	rows := db.Query(`SELECT x, y, z FROM t1`)
	if len(rows.Rows) != 1 || rows.Rows[0][0].(int64) != 7 || rows.Rows[0][1].(int64) != 80 || rows.Rows[0][2].(int64) != 9 {
		t.Fatalf("update corrupted row: %v", rows.Rows)
	}
	if len(db.Query(`SELECT * FROM log`).Rows) != 0 {
		t.Fatalf("trigger fired for trigger-less recreated table")
	}
}

// TestPin_WRTriggerMergeDeclaredOrder covers fix 2: an UPDATE on a WITHOUT
// ROWID table with a real BEFORE trigger that writes to a non-SET column —
// the trigger's write must survive AND the SET columns must land in the
// right columns (the row re-read/merge must be declared-order, PK-matched).
func TestPin_WRTriggerMergeDeclaredOrder(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	// PK (e,c) makes the storage order (e,c,a,b,d,f) — a non-trivial
	// permutation of declaration order (a,b,c,d,e,f).
	mustExec(`CREATE TABLE t1(a,b,c,d,e,f,
		UNIQUE (a,b),
		PRIMARY KEY (e,c)
	) WITHOUT rowid;`)
	mustExec(`CREATE TABLE log(v TEXT);`)
	mustExec(`CREATE TRIGGER t1_bu BEFORE UPDATE ON t1 BEGIN INSERT INTO log VALUES('hit'); END;`)
	mustExec(`INSERT INTO t1 VALUES(1,2,3,5,5,3);`)
	// Update d and f (neither is a PK column; declared idx 3 and 5, storage
	// slot 4 and 5 — the storage slot of d differs from its declared index).
	if r := db.Exec(`UPDATE t1 SET d=99, f=98`); r.Error != nil {
		t.Fatalf("update: %v", r.Error)
	}
	rows := db.Query(`SELECT a,b,c,d,e,f FROM t1`)
	if len(rows.Rows) != 1 {
		t.Fatalf("want 1 row, got %v", rows.Rows)
	}
	got := rows.Rows[0]
	want := []int64{1, 2, 3, 99, 5, 98}
	for i, w := range want {
		if got[i].(int64) != w {
			t.Fatalf("column %d: want %d, got %v (row %v)", i, w, got[i], got)
		}
	}
	if n := len(db.Query(`SELECT * FROM log`).Rows); n != 1 {
		t.Fatalf("BEFORE UPDATE trigger fired %d times, want 1", n)
	}
	// PK update: both key columns change in one statement (self-consistent).
	if r := db.Exec(`UPDATE t1 SET e=10, c=11`); r.Error != nil {
		t.Fatalf("pk update: %v", r.Error)
	}
	rows = db.Query(`SELECT a,b,c,d,e,f FROM t1`)
	got = rows.Rows[0]
	want = []int64{1, 2, 11, 99, 10, 98}
	for i, w := range want {
		if got[i].(int64) != w {
			t.Fatalf("pk-update column %d: want %d, got %v (row %v)", i, w, got[i], got)
		}
	}
}
