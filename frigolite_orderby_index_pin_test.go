package frigolite

import (
	"fmt"
	"testing"
)

// SQLite satisfies an ORDER BY without a temp-b-tree sort when the loop
// delivers the order (where.c orderByConsumed): a table scan delivers rowid
// order (the rowid's uniqueness retires later terms; DESC is the reverse
// scan), and an index loop delivers the order when its equality-bound
// prefix and remaining key columns match the ORDER BY terms — and where.c
// prices the sort into every non-delivering loop, so `WHERE a=1 ORDER BY c`
// plans SEARCH via (a,c,b), not the cheaper-per-row (a) index. These pins
// hold the plan text, the emitted rows and the statement's sort counter
// (SQLITE_STMTSTATUS_SORT — what where.test's cksort helper reads).
func TestOrderByDeliveredByLoopPins(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	setup := []string{
		"CREATE TABLE t1(w int, x int, y int)",
		// where.test's MySQL-compatibility quoting: backtick, single-quote.
		"CREATE INDEX i1w ON t1(\"w\")",
		"CREATE INDEX i1xy ON t1(`x`,'y' ASC)",
		"CREATE TABLE t3(a,b,c)",
		"CREATE INDEX t3a ON t3(a)",
		"CREATE INDEX t3bc ON t3(b,c)",
		"CREATE INDEX t3acb ON t3(a,c,b)",
	}
	for _, stmt := range setup {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	for i := 1; i <= 12; i++ {
		ins := fmt.Sprintf("INSERT INTO t1 VALUES(%d, %d, %d)", 13-i, i%3, i*i)
		if res := db.Exec(ins); res.Error != nil {
			t.Fatalf("%s: %v", ins, res.Error)
		}
	}
	for i := 0; i < 8; i++ {
		ins := fmt.Sprintf("INSERT INTO t3 VALUES(%d, %d, %d)", i%3, i, 8-i)
		if res := db.Exec(ins); res.Error != nil {
			t.Fatalf("%s: %v", ins, res.Error)
		}
	}

	cases := []struct {
		sql      string
		wantRows string
		wantPlan string
	}{
		// A plain table scan emits rowid order; the rowid's uniqueness
		// retires later terms (oracle: "SCAN t1", no temp b-tree).
		{"SELECT y FROM t1 WHERE y>4 ORDER BY rowid", "9 16 25 36 49 64 81 100 121 144 ", "`--SCAN t1"},
		{"SELECT y FROM t1 ORDER BY rowid DESC LIMIT 3", "144 121 100 ", "`--SCAN t1"},
		{"SELECT y FROM t1 ORDER BY rowid, x DESC, w", "1 4 9 16 25 36 49 64 81 100 121 144 ", "`--SCAN t1"},
		// Equality prefix on (a,c,b): where.c prefers the index that also
		// delivers the ORDER BY (oracle: "SEARCH t3 USING COVERING INDEX
		// t3acb (a=?)").
		{"SELECT * FROM t3 WHERE a=1 ORDER BY c", "1 7 1 1 4 4 1 1 7 ", "`--SEARCH t3 USING COVERING INDEX t3acb (a=?)"},
		{"SELECT * FROM t3 WHERE a=1 AND c>4 ORDER BY a,c LIMIT 2", "1 1 7 ", "`--SEARCH t3 USING COVERING INDEX t3acb (a=? AND c>?)"},
		// Single-quoted / backtick index columns resolve to their bare
		// names (sqlite3NameFromToken): i1xy('y' ASC) orders by y.
		{"SELECT y FROM t1 WHERE x=2 ORDER BY y", "4 25 64 121 ", "`--SEARCH t1 USING COVERING INDEX i1xy (x=?)"},
	}
	for _, tc := range cases {
		res := db.Query(tc.sql)
		if res.Error != nil {
			t.Fatalf("%s: %v", tc.sql, res.Error)
		}
		if got := renderRows(res.Rows); got != tc.wantRows {
			t.Errorf("%s: rows = [%s], want [%s]", tc.sql, got, tc.wantRows)
		}
		if n := db.engine.StmtStatusSort(); n != 0 {
			t.Errorf("%s: sort counter = %d, want 0 (the loop delivers the order)", tc.sql, n)
		}
		plan := db.Query("EXPLAIN QUERY PLAN " + tc.sql)
		if plan.Error != nil || len(plan.Rows) < 2 {
			t.Fatalf("EQP %s: %v", tc.sql, plan.Error)
		}
		gotPlan := fmt.Sprint(plan.Rows[len(plan.Rows)-1][0])
		if gotPlan != tc.wantPlan {
			t.Errorf("%s: plan = %q, want %q", tc.sql, gotPlan, tc.wantPlan)
		}
	}

	// Shapes that keep the sorter: a mixed-direction order no index
	// delivers, and an ORDER BY on a column outside the index.
	db.Query("SELECT * FROM t3 ORDER BY c DESC, a")
	if n := db.engine.StmtStatusSort(); n != 1 {
		t.Errorf("mixed-direction ORDER BY: sort counter = %d, want 1", n)
	}

	// A single-row result and an EMPTY result under a delivered order must
	// not run a sorter (SQLite emits no OP_Sort for an index-ordered plan at
	// any row count).
	db.Exec("DELETE FROM t3 WHERE a != 2")
	if res := db.Query("SELECT * FROM t3 WHERE b=5 ORDER BY c"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if n := db.engine.StmtStatusSort(); n != 0 {
		t.Errorf("single-row delivered order: sort counter = %d, want 0", n)
	}
	if res := db.Query("SELECT * FROM t3 WHERE b=3 ORDER BY c"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if n := db.engine.StmtStatusSort(); n != 0 {
		t.Errorf("empty delivered order: sort counter = %d, want 0", n)
	}
}
