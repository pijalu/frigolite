// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import "testing"

// TestStmtStatusCounters pins SQLite's statement-status counters
// (SQLITE_STMTSTATUS_FULLSCAN_STEP / SQLITE_STMTSTATUS_SORT) — the values
// the testfixture reads through `db status step` / `db status sort`.
//
// The reference numbers come from SQLite's own where7.test expectations
// (ori/sqlite/test/where7.test: "2 3 scan 4 sort 0" for
// "SELECT a FROM t1 WHERE b=3 OR +c=6 ORDER BY a" over a five-row table):
// a full table scan of n rows counts n-1 (SQLite reaches the first row with
// OP_Rewind, which is not counted, and increments the counter in OP_Next —
// ../sqlite/src/vdbe.c:6528), and the sort counter is incremented once per
// OP_Sort/OP_SorterSort (vdbe.c:6351).
func TestStmtStatusCounters(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := `CREATE TABLE t1(a INTEGER PRIMARY KEY,b,c,d);
	  INSERT INTO t1 VALUES(1,2,3,4);
	  INSERT INTO t1 VALUES(2,3,4,5);
	  INSERT INTO t1 VALUES(3,4,6,8);
	  INSERT INTO t1 VALUES(4,5,10,15);
	  INSERT INTO t1 VALUES(5,10,100,1000);
	  CREATE INDEX t1b ON t1(b);
	  CREATE INDEX t1c ON t1(c);`
	if res := db.Exec(setup); res.Error != nil {
		t.Fatal(res.Error)
	}

	cases := []struct {
		sql  string
		scan int64
		sort int64
	}{
		// A full table scan of five rows counts four advances.
		{"SELECT a FROM t1", 4, 0},
		// An index seek is not a full scan.
		{"SELECT a FROM t1 WHERE b=3", 0, 0},
		{"SELECT a FROM t1 WHERE c=6", 0, 0},
		// ORDER BY a column with no index: a full scan plus one sorter.
		{"SELECT a FROM t1 ORDER BY d", 4, 1},
	}
	for _, c := range cases {
		res := db.Query(c.sql)
		if res.Error != nil {
			t.Errorf("%s: %v", c.sql, res.Error)
			continue
		}
		scan := db.engine.StmtStatusFullScanStep()
		sortOps := db.engine.StmtStatusSort()
		if scan != c.scan || sortOps != c.sort {
			t.Errorf("%s: scan=%d sort=%d, want scan=%d sort=%d",
				c.sql, scan, sortOps, c.scan, c.sort)
		}
	}
}

// Known planner gap (not pinned here): for "SELECT a FROM t1 ORDER BY a" —
// where a is the INTEGER PRIMARY KEY (the rowid alias) — SQLite satisfies
// the ORDER BY with the table's own rowid order and runs no sorter, while
// the engine still sorts (the counter reports sort 1). Fixing that means
// teaching the ORDER BY planner that a rowid-alias term is already ordered;
// it is tracked as the planner-parity item in portplan/PARITY_EXECUTION.md.

// TestStmtStatusCountersResetOnRead pins the reset-on-read semantics of
// sqlite3_stmt_status(stmt, op, 1): reading the counter zeroes it.
func TestStmtStatusCountersResetOnRead(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t1(a); INSERT INTO t1 VALUES(1),(2),(3)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	res := db.Query("SELECT a FROM t1")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if first := db.engine.StmtStatusFullScanStep(); first != 2 {
		t.Errorf("first read = %d, want 2 (three rows, the first one reached by the rewind)", first)
	}
	if second := db.engine.StmtStatusFullScanStep(); second != 0 {
		t.Errorf("second read = %d, want 0 (the counter resets when read)", second)
	}
}
