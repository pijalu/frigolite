package frigolite

import (
	"strings"
	"testing"
)

// P5 pre-tests: hand-written tests for G5.EXPLAIN — EXPLAIN QUERY PLAN (EQP)
// shape and plain EXPLAIN column shape. Each expectation was verified against
// sqlite3 3.51 as the oracle.
//
// Scope: EQP emits SQLite-shaped labels (SCAN / SEARCH / USE TEMP B-TREE /
// COMPOUND QUERY / CO-ROUTINE / SCALAR|LIST SUBQUERY n), and plain EXPLAIN
// emits the opcode-dump column shape (addr opcode p1 p2 p3 p4 p5 comment).
// Frigolite is not a bytecode VDBE, so exact opcode-level EXPLAIN output is
// N/A; only the column shape and the EQP detail labels are asserted here.
//
// Assertions use the flattened EQP line list (each "plan" cell joined with
// spaces) because the TCL-testgen harness compares that string. Multi-node
// plans are asserted via substring (the join order may differ from SQLite's
// cost-based order); single-node plans are asserted exactly.

// eqpFlatten returns the flattened EQP output: all plan cells joined with
// spaces (including the QUERY PLAN header), matching the harness flatten.
func eqpFlatten(t *testing.T, db *DB, sql string) string {
	t.Helper()
	r := db.Query("EXPLAIN QUERY PLAN " + sql)
	if r.Error != nil {
		t.Fatalf("eqp error: %v\n  sql: %s", r.Error, sql)
	}
	var parts []string
	for _, row := range r.Rows {
		for _, v := range row {
			if v == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, formatSQLiteValue(v))
			}
		}
	}
	return strings.Join(parts, " ")
}

func TestP5ExplainEqpScanSearch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a INT, b INT);",
		"CREATE INDEX i1 ON t1(a);",
		"CREATE INDEX i2 ON t1(b);",
		"CREATE TABLE t2(a INT, b INT);",
		"INSERT INTO t1 VALUES(1,2),(3,4),(5,6);",
		"INSERT INTO t2 VALUES(1,2),(3,4);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// Full scan: SQLite emits `SCAN t1`.
	if got := eqpFlatten(t, db, "SELECT * FROM t1;"); !strings.Contains(got, "SCAN t1") {
		t.Errorf("full scan: got %q, want it to contain SCAN t1", got)
	}

	// Index search: SQLite emits `SEARCH t1 USING INDEX i1 (a=?)`.
	got := eqpFlatten(t, db, "SELECT * FROM t1 WHERE a=1;")
	want := "QUERY PLAN `--SEARCH t1 USING INDEX i1 (a=?)"
	if got != want {
		t.Errorf("index search: got %q, want %q", got, want)
	}

	// Join: SQLite emits two lines, SCAN on the outer table + SEARCH on the
	// inner (order may vary by cost; the labels must be present).
	got = eqpFlatten(t, db, "SELECT * FROM t1, t2 WHERE t1.a=t2.a;")
	if !strings.Contains(got, "SEARCH t1 USING INDEX i1 (a=?)") {
		t.Errorf("join: got %q, want it to contain SEARCH t1 USING INDEX i1 (a=?)", got)
	}
	if !strings.Contains(got, "SCAN t2") && !strings.Contains(got, "SCAN t1") {
		t.Errorf("join: got %q, want a SCAN line", got)
	}
}

func TestP5ExplainEqpTempBTree(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a INT, b INT);",
		"CREATE INDEX i1 ON t1(a);",
		"CREATE TABLE t2(a INT, b INT);",
		"INSERT INTO t1 VALUES(1,2),(3,4);",
		"INSERT INTO t2 VALUES(1,2);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// ORDER BY on an unindexed column sorts in a temp b-tree.
	if got := eqpFlatten(t, db, "SELECT * FROM t1 ORDER BY b;"); !strings.Contains(got, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("order by no index: got %q, want USE TEMP B-TREE FOR ORDER BY", got)
	}

	// ORDER BY on an indexed column that covers the output uses the index.
	if got := eqpFlatten(t, db, "SELECT a FROM t1 ORDER BY a;"); !strings.Contains(got, "SCAN t1 USING COVERING INDEX i1") {
		t.Errorf("order by covering: got %q, want SCAN t1 USING COVERING INDEX i1", got)
	}

	// GROUP BY on an indexed column that covers the output uses the index.
	if got := eqpFlatten(t, db, "SELECT a, count(*) FROM t1 GROUP BY a;"); !strings.Contains(got, "SCAN t1 USING COVERING INDEX i1") {
		t.Errorf("group by covering: got %q, want SCAN t1 USING COVERING INDEX i1", got)
	}

	// GROUP BY on an unindexed column sorts in a temp b-tree.
	if got := eqpFlatten(t, db, "SELECT b, count(*) FROM t1 GROUP BY b;"); !strings.Contains(got, "USE TEMP B-TREE FOR GROUP BY") {
		t.Errorf("group by no index: got %q, want USE TEMP B-TREE FOR GROUP BY", got)
	}

	// DISTINCT on an unindexed column sorts in a temp b-tree.
	if got := eqpFlatten(t, db, "SELECT DISTINCT b FROM t1;"); !strings.Contains(got, "USE TEMP B-TREE FOR DISTINCT") {
		t.Errorf("distinct no index: got %q, want USE TEMP B-TREE FOR DISTINCT", got)
	}

	// DISTINCT on an indexed column uses the index.
	if got := eqpFlatten(t, db, "SELECT DISTINCT a FROM t1;"); !strings.Contains(got, "SCAN t1 USING COVERING INDEX i1") {
		t.Errorf("distinct covering: got %q, want SCAN t1 USING COVERING INDEX i1", got)
	}
}

func TestP5ExplainEqpCompound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a INT, b INT);",
		"CREATE TABLE t2(a INT, b INT);",
		"INSERT INTO t1 VALUES(1,2);",
		"INSERT INTO t2 VALUES(3,4);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// UNION renders a COMPOUND QUERY tree: LEFT-MOST SUBQUERY + branch labels.
	for _, q := range []string{
		"SELECT * FROM t1 UNION SELECT * FROM t2;",
		"SELECT * FROM t1 INTERSECT SELECT * FROM t2;",
		"SELECT * FROM t1 EXCEPT SELECT * FROM t2;",
		"SELECT * FROM t1 UNION ALL SELECT * FROM t2;",
	} {
		got := eqpFlatten(t, db, q)
		if !strings.Contains(got, "COMPOUND QUERY") {
			t.Errorf("%s: got %q, want COMPOUND QUERY", q, got)
		}
		if !strings.Contains(got, "LEFT-MOST SUBQUERY") {
			t.Errorf("%s: got %q, want LEFT-MOST SUBQUERY", q, got)
		}
	}

	// UNION (set semantics) uses a temp b-tree; UNION ALL does not.
	if got := eqpFlatten(t, db, "SELECT * FROM t1 UNION SELECT * FROM t2;"); !strings.Contains(got, "UNION USING TEMP B-TREE") {
		t.Errorf("UNION: got %q, want UNION USING TEMP B-TREE", got)
	}
	if got := eqpFlatten(t, db, "SELECT * FROM t1 UNION ALL SELECT * FROM t2;"); !strings.Contains(got, "UNION ALL") {
		t.Errorf("UNION ALL: got %q, want UNION ALL", got)
	}
	if got := eqpFlatten(t, db, "SELECT * FROM t1 INTERSECT SELECT * FROM t2;"); !strings.Contains(got, "INTERSECT USING TEMP B-TREE") {
		t.Errorf("INTERSECT: got %q, want INTERSECT USING TEMP B-TREE", got)
	}
	if got := eqpFlatten(t, db, "SELECT * FROM t1 EXCEPT SELECT * FROM t2;"); !strings.Contains(got, "EXCEPT USING TEMP B-TREE") {
		t.Errorf("EXCEPT: got %q, want EXCEPT USING TEMP B-TREE", got)
	}
}

func TestP5ExplainEqpSubqueries(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a INT, b INT);",
		"CREATE INDEX i1 ON t1(a);",
		"CREATE TABLE t2(a INT, b INT);",
		"INSERT INTO t1 VALUES(1,2),(3,4);",
		"INSERT INTO t2 VALUES(1,2);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// Correlated EXISTS in WHERE: SQLite emits CORRELATED SCALAR SUBQUERY 1.
	if got := eqpFlatten(t, db, "SELECT * FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.a=t1.a);"); !strings.Contains(got, "CORRELATED SCALAR SUBQUERY 1") {
		t.Errorf("correlated exists: got %q, want CORRELATED SCALAR SUBQUERY 1", got)
	}

	// Non-correlated scalar subquery in the select list: SCALAR SUBQUERY 1.
	if got := eqpFlatten(t, db, "SELECT (SELECT max(b) FROM t2) FROM t1;"); !strings.Contains(got, "SCALAR SUBQUERY 1") {
		t.Errorf("scalar subquery: got %q, want SCALAR SUBQUERY 1", got)
	}

	// IN subquery: LIST SUBQUERY 1.
	if got := eqpFlatten(t, db, "SELECT * FROM t1 WHERE a IN (SELECT a FROM t2);"); !strings.Contains(got, "LIST SUBQUERY 1") {
		t.Errorf("in subquery: got %q, want LIST SUBQUERY 1", got)
	}
}

func TestP5ExplainEqpCoRoutine(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a INT, b INT);",
		"CREATE TABLE t2(a INT, b INT);",
		"INSERT INTO t1 VALUES(1,2);",
		"INSERT INTO t2 VALUES(3,4);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// A compound FROM-clause subquery is materialized by a CO-ROUTINE; SQLite
	// emits CO-ROUTINE <alias> + SCAN <alias>.
	got := eqpFlatten(t, db, "SELECT * FROM (SELECT a FROM t1 UNION SELECT a FROM t2) AS sub;")
	if !strings.Contains(got, "CO-ROUTINE sub") {
		t.Errorf("from subquery compound: got %q, want CO-ROUTINE sub", got)
	}
	if !strings.Contains(got, "SCAN sub") {
		t.Errorf("from subquery compound: got %q, want SCAN sub", got)
	}

	// A simple single-table FROM subquery is inlined (SCAN t1, no CO-ROUTINE).
	got = eqpFlatten(t, db, "SELECT * FROM (SELECT * FROM t1) AS sub;")
	if strings.Contains(got, "CO-ROUTINE") {
		t.Errorf("from subquery simple: got %q, should not contain CO-ROUTINE", got)
	}
	if !strings.Contains(got, "SCAN t1") {
		t.Errorf("from subquery simple: got %q, want SCAN t1", got)
	}
}

func TestP5ExplainPlainColumns(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a INT, b INT);",
		"INSERT INTO t1 VALUES(1,2);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// Plain EXPLAIN emits the opcode-dump column shape (addr opcode p1 p2 p3
	// p4 p5 comment). Frigolite is not a bytecode VDBE, so the opcode ROWS are
	// N/A; only the column shape is asserted.
	r := db.Query("EXPLAIN SELECT * FROM t1;")
	if r.Error != nil {
		t.Fatalf("explain: %v", r.Error)
	}
	wantCols := []string{"addr", "opcode", "p1", "p2", "p3", "p4", "p5", "comment"}
	if len(r.Columns) != len(wantCols) {
		t.Fatalf("explain columns: got %v, want %v", r.Columns, wantCols)
	}
	for i, c := range wantCols {
		if r.Columns[i] != c {
			t.Errorf("explain column %d: got %q, want %q", i, r.Columns[i], c)
		}
	}
	// At least one opcode row is produced (the exact opcodes are N/A).
	if len(r.Rows) == 0 {
		t.Errorf("explain: no opcode rows")
	}
}
