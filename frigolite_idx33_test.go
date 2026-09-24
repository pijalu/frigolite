package frigolite_test

// T33-idx native pins: engine contracts fixed by this tranche, each verified
// against the sqlite3 oracle (/usr/bin/sqlite3 3.51).

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func idx33Open(t *testing.T) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func idx33Exec(t *testing.T, db *frigolite.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
}

func idx33Flatten(t *testing.T, db *frigolite.DB, query string) string {
	t.Helper()
	r := db.Query(query)
	if r.Error != nil {
		t.Fatalf("%s: %v", query, r.Error)
	}
	var parts []string
	for _, row := range r.Rows {
		for _, v := range row {
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}
	return strings.Join(parts, " ")
}

// TestIdx33SeekPrefixRule pins where.c whereLoopAddBtreeIndex's leading-prefix
// rule: a constraint on an index column with an unconstrained leading prefix
// cannot drive the index (index-14.3 "WHERE b=''" over index t6i1(a,b) plans
// SCAN t6 and emits rows in rowid order; index-14.2's a='' seek emits index
// (a,b) key order with NULLs ordered first).
func TestIdx33SeekPrefixRule(t *testing.T) {
	db := idx33Open(t)
	idx33Exec(t, db,
		"CREATE TABLE t6(a,b,c)",
		"CREATE INDEX t6i1 ON t6(a,b)",
		"INSERT INTO t6 VALUES('','',1)",
		"INSERT INTO t6 VALUES('',NULL,2)",
		"INSERT INTO t6 VALUES(NULL,'',3)",
		"INSERT INTO t6 VALUES('abc',123,4)",
		"INSERT INTO t6 VALUES(123,'abc',5)",
	)
	for _, q := range []struct {
		sql  string
		want string
	}{
		{"SELECT c FROM t6 ORDER BY a,b", "3 5 2 1 4"},
		{"SELECT c FROM t6 WHERE b=''", "1 3"},
		{"SELECT c FROM t6 WHERE a=''", "2 1"},
	} {
		if got := idx33Flatten(t, db, q.sql); got != q.want {
			t.Errorf("%s: got [%s] want [%s]", q.sql, got, q.want)
		}
	}
	if got := idx33Flatten(t, db, "EXPLAIN QUERY PLAN SELECT c FROM t6 WHERE b=''"); !strings.HasSuffix(got, "SCAN t6") {
		t.Errorf("EQP WHERE b='': got [%s] want suffix SCAN t6 (oracle: full scan, no (b=?) search)", got)
	}
	if got := idx33Flatten(t, db, "EXPLAIN QUERY PLAN SELECT c FROM t6 WHERE a=''"); !strings.Contains(got, "SEARCH t6 USING INDEX t6i1 (a=?)") {
		t.Errorf("EQP WHERE a='': got [%s] want SEARCH t6 USING INDEX t6i1 (a=?)", got)
	}
}

// idx33TriggerDB opens the without_rowid4-6.2 fixture: WR table, ai_tbl (the
// section-6.1 AFTER INSERT trigger, a silent no-op for these rows) and au_tbl
// whose body `UPDATE OR IGNORE tbl SET a = new.a, c = 10` is overridden by
// the firing statement's ON CONFLICT clause (trigger.c codeTriggerProgram:
// orconf = (orconf==OE_Default) ? pStep->orconf : orconf).
func idx33TriggerDB(t *testing.T) *frigolite.DB {
	t.Helper()
	db := idx33Open(t)
	idx33Exec(t, db,
		"CREATE TABLE tbl (a PRIMARY KEY, b, c) WITHOUT rowid",
		"CREATE TRIGGER ai_tbl AFTER INSERT ON tbl BEGIN\n      INSERT OR IGNORE INTO tbl values (new.a, 0, 0);\n    END",
	)
	return db
}

func idx33TblState(t *testing.T, db *frigolite.DB) string {
	t.Helper()
	return idx33Flatten(t, db, "SELECT a, b, c FROM tbl ORDER BY a")
}

// TestIdx33WRTriggerOrconfPropagation pins the propagated-clause behavior on
// the WITHOUT ROWID PK btree (without_rowid4-6.2b/6.2d/6.2f/6.2g): the outer
// OR ABORT/FAIL/REPLACE/ROLLBACK replaces the trigger body step's IGNORE, the
// per-row WR conflict check sees the outer statement's own new PK (where.c
// writes the updated row before the AFTER trigger fires), and OR FAIL keeps
// the rows written before the conflict (vdbe.c OP_Halt P2=OE_Fail).
func TestIdx33WRTriggerOrconfPropagation(t *testing.T) {
	t.Run("ORABORT-propagates-inner-conflict", func(t *testing.T) {
		db := idx33TriggerDB(t)
		idx33Exec(t, db,
			"CREATE TRIGGER au_tbl AFTER UPDATE ON tbl BEGIN\n      UPDATE OR IGNORE tbl SET a = new.a, c = 10;\n    END",
			"INSERT INTO tbl values (1, 2, 10)",
			"INSERT INTO tbl values (6, 3, 4)",
		)
		res := db.Exec("UPDATE OR ABORT tbl SET a = 4 WHERE a = 1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed: tbl.a") {
			t.Fatalf("want UNIQUE constraint error, got %v", res.Error)
		}
		if got := idx33TblState(t, db); got != "1 2 10 6 3 4" {
			t.Errorf("state after abort: got [%s] want [1 2 10 6 3 4]", got)
		}
	})
	t.Run("ORABORT-nonconflicting-outer-still-errors", func(t *testing.T) {
		// The oracle errors even when the outer SET value does not itself
		// conflict: the propagated ABORT makes the body's (6,...)->(99,...)
		// write conflict with the body's own (99,2,10) write.
		db := idx33TriggerDB(t)
		idx33Exec(t, db,
			"CREATE TRIGGER au_tbl AFTER UPDATE ON tbl BEGIN\n      UPDATE OR IGNORE tbl SET a = new.a, c = 10;\n    END",
			"INSERT INTO tbl values (4, 2, 3)",
			"INSERT INTO tbl values (6, 3, 4)",
			"BEGIN",
			"UPDATE tbl SET a = 1 WHERE a = 4",
		)
		res := db.Exec("UPDATE OR ABORT tbl SET a = 99 WHERE a = 1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed: tbl.a") {
			t.Fatalf("want UNIQUE constraint error, got %v", res.Error)
		}
		if got := idx33TblState(t, db); got != "1 2 10 6 3 4" {
			t.Errorf("state after abort: got [%s] want [1 2 10 6 3 4]", got)
		}
	})
	t.Run("ORFAIL-keeps-prior-statement-changes", func(t *testing.T) {
		db := idx33TriggerDB(t)
		idx33Exec(t, db,
			"CREATE TRIGGER au_tbl AFTER UPDATE ON tbl BEGIN\n      UPDATE OR IGNORE tbl SET a = new.a, c = 10;\n    END",
			"INSERT INTO tbl values (1, 2, 10)",
			"INSERT INTO tbl values (6, 3, 4)",
		)
		res := db.Exec("UPDATE OR FAIL tbl SET a = 4 WHERE a = 1")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "UNIQUE constraint failed: tbl.a") {
			t.Fatalf("want UNIQUE constraint error, got %v", res.Error)
		}
		if got := idx33TblState(t, db); got != "4 2 10 6 3 4" {
			t.Errorf("state after fail: got [%s] want [4 2 10 6 3 4] (outer change kept)", got)
		}
	})
	t.Run("ORREPLACE-body-replaces-conflicting-row", func(t *testing.T) {
		db := idx33TriggerDB(t)
		idx33Exec(t, db,
			"CREATE TRIGGER au_tbl AFTER UPDATE ON tbl BEGIN\n      UPDATE OR IGNORE tbl SET a = new.a, c = 10;\n    END",
			"INSERT INTO tbl values (4, 2, 10)",
			"INSERT INTO tbl values (6, 3, 4)",
		)
		idx33Exec(t, db, "UPDATE OR REPLACE tbl SET a = 1 WHERE a = 4")
		if got := idx33TblState(t, db); got != "1 3 10" {
			t.Errorf("state after replace: got [%s] want [1 3 10]", got)
		}
	})
	t.Run("plain-outer-keeps-step-IGNORE", func(t *testing.T) {
		// Without an explicit outer clause the body step's own OR IGNORE
		// applies: the conflicting inner row is skipped, no error.
		db := idx33TriggerDB(t)
		idx33Exec(t, db,
			"CREATE TRIGGER au_tbl AFTER UPDATE ON tbl BEGIN\n      UPDATE OR IGNORE tbl SET a = new.a, c = 10;\n    END",
			"INSERT INTO tbl values (1, 2, 10)",
			"INSERT INTO tbl values (6, 3, 4)",
		)
		idx33Exec(t, db, "UPDATE tbl SET a = 4 WHERE a = 1")
		if got := idx33TblState(t, db); got != "4 2 10 6 3 4" {
			t.Errorf("state: got [%s] want [4 2 10 6 3 4]", got)
		}
	})
	t.Run("ORREPLACE-persists-across-statements-in-tx", func(t *testing.T) {
		db := idx33TriggerDB(t)
		idx33Exec(t, db,
			"CREATE TRIGGER au_tbl AFTER UPDATE ON tbl BEGIN\n      UPDATE OR IGNORE tbl SET a = new.a, c = 10;\n    END",
			"INSERT INTO tbl values (4, 2, 10)",
			"INSERT INTO tbl values (6, 3, 4)",
			"BEGIN",
		)
		idx33Exec(t, db, "UPDATE OR REPLACE tbl SET a = 1 WHERE a = 4")
		if got := idx33TblState(t, db); got != "1 3 10" {
			t.Errorf("in-tx state: got [%s] want [1 3 10]", got)
		}
		idx33Exec(t, db, "INSERT INTO tbl VALUES (2, 3, 4)")
		if got := idx33TblState(t, db); got != "1 3 10 2 3 4" {
			t.Errorf("after insert: got [%s] want [1 3 10 2 3 4]", got)
		}
	})
}
