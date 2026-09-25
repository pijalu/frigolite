package frigolite

import "testing"

// Pins for the T33-win regression: the omit-unused-subquery-column
// optimization (select.c disableUnusedSubqueryResultColumns) must treat a
// FROM-clause subquery column as USED when the outer statement references it
// from inside an expression subquery (IN / EXISTS / scalar) or from inside a
// window definition (OVER PARTITION BY / ORDER BY / frame bounds, aggregate
// ORDER BY, FILTER, or a referenced WINDOW-clause definition). SQLite sets
// the source's colUsed bit during name resolution, which walks those
// constructs; missing any of them NULLs a live column out and corrupts the
// result (window1-31.2/31.3/48.0/48.1/78.2).
//
// Oracle: /usr/bin/sqlite3 3.x on 2026-09-25 for every want below.

// TestWin33InSubqueryWindowOuterRef pins window1-31.2/31.3: an IN-subquery
// whose body is a window select referencing the outer subquery's columns
// (c, d) keeps those columns alive in the FROM-subquery (SELECT * FROM t2).
func TestWin33InSubqueryWindowOuterRef(t *testing.T) {
	db := openProbeDB(t)
	if err := db.Exec(`CREATE TABLE t1(a, b); CREATE TABLE t2(c, d); CREATE TABLE t3(e, f);
		INSERT INTO t1 VALUES(1, 1);
		INSERT INTO t2 VALUES(1, 1);
		INSERT INTO t3 VALUES(1, 1);`).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, sql, want string }{
		{"window1-31.2", `SELECT d IN (SELECT sum(c) OVER (ORDER BY e+c) FROM t3) FROM (SELECT * FROM t2)`, "1"},
		{"window1-31.3", `SELECT d IN (SELECT sum(c) OVER (PARTITION BY d ORDER BY e+c) FROM t3) FROM (SELECT * FROM t2)`, "1"},
	} {
		res := db.Query(tc.sql)
		if res.Error != nil {
			t.Errorf("%s: query error: %v", tc.name, res.Error)
			continue
		}
		if got := flatRows(res); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestWin33ScalarSubqueryWindowOverDerivedCol pins window1-48.0/48.1: a
// scalar subquery in the result list reads column x of the FROM-subquery
// through window OVER (ORDER BY x); x must not be omitted as unused.
func TestWin33ScalarSubqueryWindowOverDerivedCol(t *testing.T) {
	db := openProbeDB(t)
	if err := db.Exec(`CREATE TABLE t1(a);
		INSERT INTO t1 VALUES(1); INSERT INTO t1 VALUES(2); INSERT INTO t1 VALUES(3);`).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, sql, want string }{
		{"window1-48.0", `SELECT (SELECT max(x)OVER(ORDER BY x) + min(x)OVER(ORDER BY x))
			FROM (SELECT (SELECT sum(a) FROM t1) AS x FROM t1)`, "12 12 12"},
		{"window1-48.1", `SELECT (SELECT max(x)OVER(ORDER BY x) + min(x)OVER(ORDER BY x))
			FROM (SELECT (SELECT sum(a) FROM t1 GROUP BY a) AS x FROM t1)`, "2 2 2"},
	} {
		res := db.Query(tc.sql)
		if res.Error != nil {
			t.Errorf("%s: query error: %v", tc.name, res.Error)
			continue
		}
		if got := flatRows(res); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestWin33WindowDefRangeSubqueryCol pins window1-78.2: a window ORDER BY
// term over the FROM-subquery's column y controls a RANGE frame; y must not
// be omitted (with y omitted the frame degrades and the result flips from
// NULL to the frame's group_concat value).
func TestWin33WindowDefRangeSubqueryCol(t *testing.T) {
	db := openProbeDB(t)
	res := db.Query(`SELECT quote(group_concat(x) OVER (
		ORDER BY y RANGE BETWEEN 1 FOLLOWING AND 2 FOLLOWING
	)) FROM (SELECT 'abc' AS x, 1 AS y)`)
	if res.Error != nil {
		t.Fatalf("query error: %v", res.Error)
	}
	if got, want := flatRows(res), "NULL"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestWin33WindowDefUseShapes pins the remaining outer constructs whose
// expressions resolution walks (and therefore must keep subquery columns
// alive): a FILTER clause, an aggregate ORDER BY inside the call, and a
// WINDOW-clause definition referenced by name from OVER.
func TestWin33WindowDefUseShapes(t *testing.T) {
	db := openProbeDB(t)
	if err := db.Exec(`CREATE TABLE t8(y, x);
		INSERT INTO t8 VALUES(1, 2); INSERT INTO t8 VALUES(10, 3);
		CREATE TABLE t9(y, x);
		INSERT INTO t9 VALUES('a', 2); INSERT INTO t9 VALUES('b', 1);`).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, sql, want string }{
		{"filter", `SELECT sum(x) FILTER (WHERE y > 0) FROM (SELECT 1 AS x, 2 AS y)`, "1"},
		// ORDER BY inside the aggregate call is the only reference to x;
		// with x omitted the sort key vanishes and the arms' order shows.
		{"agg-orderby", `SELECT group_concat(y ORDER BY x) FROM (
			SELECT y, x FROM t9 UNION ALL SELECT 'c', 3)`, "b,a,c"},
		// PARTITION BY of the named window is the only reference to x; with
		// x omitted all three rows share one partition and the sums collapse.
		{"named-window", `SELECT sum(y) OVER w FROM (
			SELECT y, x FROM t8 UNION ALL SELECT 10, 3) WINDOW w AS (PARTITION BY x)`, "1 20 20"},
	} {
		res := db.Query(tc.sql)
		if res.Error != nil {
			t.Errorf("%s: query error: %v", tc.name, res.Error)
			continue
		}
		if got := flatRows(res); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
