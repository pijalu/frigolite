package frigolite

import (
	"strings"
	"testing"
)

// Repro for P4.WINDOW window1 batch A targets: 73.3, 65.3, 47.2.

func TestReproWindow73_3_ViewJoinWindowPass(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	execs := []string{
		"CREATE TABLE t1(a INT);",
		"INSERT INTO t1(a) VALUES(1),(2),(4);",
		"CREATE VIEW t2(b,c) AS SELECT * FROM t1 JOIN t1 A ORDER BY sum(0) OVER(PARTITION BY 0);",
		"CREATE TRIGGER x1 INSTEAD OF UPDATE ON t2 BEGIN SELECT true; END;",
	}
	for _, sql := range execs {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, sql)
		}
	}
	r := db.Query("SELECT *, nth_value(15,2) OVER() FROM t2, t1 WHERE b=4;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := flattenResult(r)
	want := "4 1 1 15 4 2 1 15 4 4 1 15 4 1 2 15 4 2 2 15 4 4 2 15 4 1 4 15 4 2 4 15 4 4 4 15"
	if got != want {
		t.Fatalf("73.3 result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

func TestReproWindow65_3_InLhsCollation(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	execs := []string{
		"CREATE TABLE t1(c1);",
		"INSERT INTO t1 VALUES('abcd');",
	}
	for _, sql := range execs {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, sql)
		}
	}
	r := db.Query("SELECT count() OVER (), group_concat(c1 COLLATE nocase) IN (SELECT 'aBCd') FROM t1;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := flattenResult(r)
	want := "1 1"
	if got != want {
		t.Fatalf("65.3 result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

func TestReproWindow47_2_GroupByOrdinalWindowMisuse(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	execs := []string{
		"CREATE TABLE t1(a, e, f, g UNIQUE, h UNIQUE);",
		"CREATE VIEW t2(k) AS SELECT e FROM t1 WHERE g = 'abc' OR h BETWEEN 10 AND f;",
	}
	for _, sql := range execs {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, sql)
		}
	}
	r := db.Query("SELECT 234 FROM t2 WHERE k=1 OR (SELECT k FROM t2 WHERE (SELECT sum(a) OVER() FROM t1 GROUP BY 1));")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "misuse of window function sum()") {
		t.Fatalf("47.2 expected error containing %q, got: %v", "misuse of window function sum()", r.Error)
	}
}

// TestReproWindow73_4_ViewUpdateSetWindow verifies that a window function in
// a view UPDATE ... FROM SET expression is computed over the matched joined
// rows (window1 73.4: UPDATE t2 SET c=nth_value(15,2) OVER() FROM ... returns
// 9 rows of 4 15).
func TestReproWindow73_4_ViewUpdateSetWindow(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	execs := []string{
		"CREATE TABLE t1(a INT);",
		"INSERT INTO t1(a) VALUES(1),(2),(4);",
		"CREATE VIEW t2(b,c) AS SELECT * FROM t1 JOIN t1 A ORDER BY sum(0) OVER(PARTITION BY 0);",
		"CREATE TRIGGER x1 INSTEAD OF UPDATE ON t2 BEGIN SELECT true; END;",
	}
	for _, sql := range execs {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, sql)
		}
	}
	r := db.Query("UPDATE t2 SET c=nth_value(15,2) OVER() FROM (SELECT * FROM t1) WHERE b=4 RETURNING *;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := flattenResult(r)
	want := "4 15 4 15 4 15 4 15 4 15 4 15 4 15 4 15 4 15"
	if got != want {
		t.Fatalf("73.4 result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestReproWindow75_1_CorrelatedAggInAggregateArg verifies that an aggregate
// whose scalar-subquery argument contains a correlated aggregate is a misuse
// (window1 75.1: SELECT count((SELECT count(a))) FROM t → "misuse of
// aggregate: count()").
func TestReproWindow75_1_CorrelatedAggInAggregateArg(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	execs := []string{
		"CREATE TABLE t1(a INT, b INT);",
		"INSERT INTO t1 VALUES(1,2);",
	}
	for _, sql := range execs {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, sql)
		}
	}
	r := db.Query("SELECT count((SELECT count(a0.a+a0.b) ORDER BY sum(0) OVER (PARTITION BY 0))) FROM t1 AS a0 JOIN t1 AS a1 GROUP BY a1.a;")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "misuse of aggregate: count()") {
		t.Fatalf("75.1 expected error containing %q, got: %v", "misuse of aggregate: count()", r.Error)
	}
}

// TestReproWindow76_5_GroupByCorrelatedAggSubquery verifies that a GROUP BY
// query whose column is a correlated-aggregate scalar subquery evaluates the
// subquery per group (window1 76.5: SELECT (SELECT max(y)+sum(0) OVER ())
// FROM t3 LEFT JOIN t4 ON x=y GROUP BY x → 100 NULL 400).
func TestReproWindow76_5_GroupByCorrelatedAggSubquery(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	execs := []string{
		"CREATE TABLE t3(x);",
		"CREATE TABLE t4(y);",
		"INSERT INTO t3 VALUES(100), (200), (400);",
		"INSERT INTO t4 VALUES(100), (300), (400);",
	}
	for _, sql := range execs {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, sql)
		}
	}
	r := db.Query("SELECT (SELECT max(y)+sum(0) OVER ()) FROM t3 LEFT JOIN t4 ON x=y GROUP BY x;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := flattenResult(r)
	want := "100 NULL 400"
	if got != want {
		t.Fatalf("76.5 result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestReproWindow78_2_EmptyFrameAggregateNull verifies that group_concat over
// an empty window frame returns NULL, not the empty string (window1 78.2:
// quote(group_concat(x) OVER (ORDER BY y RANGE 1 FOLLOWING TO 2 FOLLOWING))
// over one row → "NULL").
func TestReproWindow78_2_EmptyFrameAggregateNull(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := db.Query("SELECT quote(group_concat(x) OVER (ORDER BY y RANGE BETWEEN 1 FOLLOWING AND 2 FOLLOWING)) FROM (SELECT 'abc' AS x, 1 AS y);")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := flattenResult(r)
	want := "NULL"
	if got != want {
		t.Fatalf("78.2 result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}
