package frigolite

import (
	"strings"
	"testing"
)

// Pin for windowE-3.1 (windowE.test): a window frame keyword written in
// lowercase ("range 366.0 preceding") must behave like its uppercase form —
// SQLite's grammar selects frame semantics by token TYPE (TK_ROWS/TK_RANGE),
// not spelling. Before the parser normalization, a lowercase frame type fell
// through the executor's frame switch to "whole partition".
func TestWindowELowercaseFrameKeywords(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t2(c1 INT, c2 REAL)",
		"INSERT INTO t2 VALUES (447,0.0),(448,0.0),(449,0.0),(536,0.0),(537,1.0),(538,0.0),(544,0.0)",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, s)
		}
	}
	r := db.Query("select c1, max(c2) over (order by c1 range 366.0 preceding) from t2;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := flattenResult(r)
	want := "447 0.0 448 0.0 449 0.0 536 0.0 537 1.0 538 1.0 544 1.0"
	if got != want {
		t.Fatalf("RANGE 366.0 PRECEDING result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
	r = db.Query("select c1, max(c2) over (order by c1 rows between 1 preceding and current row) from t2;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got = flattenResult(r)
	want = "447 0.0 448 0.0 449 0.0 536 0.0 537 1.0 538 1.0 544 0.0"
	if got != want {
		t.Fatalf("ROWS frame result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// Pin for windowE-5.2: window sum() over an int64-overflowing frame switches
// to a compensated double sum (func.c sumStep: ovrfl promotes to the
// Kahan-Babuška-Neumaier accumulator); a later non-integer input absorbs the
// overflow (clears ovrfl), so the frame yields a double instead of raising
// "integer overflow". Frames that end still-overflowing raise the error at
// finalize (sumFinalize).
func TestWindowESumOverflowFloatFallback(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t(id, x)",
		"INSERT INTO t VALUES(1, -1)",
		"INSERT INTO t VALUES(2, 9223372036854775807)",
		"INSERT INTO t VALUES(3, 1)",
		"INSERT INTO t VALUES(4, 0.5)",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, s)
		}
	}
	r := db.Query("SELECT id, sum(x) OVER (ORDER BY id ROWS BETWEEN CURRENT ROW AND 2 FOLLOWING) FROM t;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := flattenResult(r)
	want := "1 9223372036854775807 2 9.22337203685478e+18 3 1.5 4 0.5"
	if got != want {
		t.Fatalf("result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// Pin for windowE-2.1: an application-registered aggregate (SQLite
// sqlite3_create_aggregate — no xValue window callback) used with OVER raises
// "x() may not be used as a window function" at prepare time
// (resolve.c: pDef->xValue==0 && pWin), including inside a WINDOW clause's
// PARTITION BY.
type windowEPinAgg struct{ n int64 }

func (a *windowEPinAgg) Step(args []interface{}) error {
	if len(args) > 0 && args[0] != nil {
		a.n++
	}
	return nil
}
func (a *windowEPinAgg) Final() (interface{}, error) { return a.n, nil }

func TestWindowEClassicAggregateRejectsOver(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec("CREATE TABLE t1(x)")
	db.RegisterAggregate("x_count", func() AggregateFunction { return &windowEPinAgg{} }, 1, 1)
	r := db.Query("SELECT min(x) OVER w1 FROM t1 WINDOW w1 AS (PARTITION BY x_count(x) OVER w1)")
	if r.Error == nil {
		t.Fatalf("expected error, got success")
	}
	if !strings.Contains(r.Error.Error(), "x_count() may not be used as a window function") {
		t.Fatalf("wrong error: %v", r.Error)
	}
}
