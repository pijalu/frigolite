package frigolite

import (
	"strings"
	"testing"
)

// TestP2SubqueryScalarSelect verifies a scalar subquery in the SELECT list.
func TestP2SubqueryScalarSelect(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(x INT, y INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40);
		INSERT INTO t2 VALUES(2,200),(3,300),(5,500);
	`)
	rows := p2QueryRows(t, db, "SELECT a, (SELECT max(x) FROM t2) FROM t1 ORDER BY a;")
	want := "1 5 2 5 3 5 4 5"
	if got := flattenRows(rows); got != want {
		t.Errorf("scalar subquery in SELECT got %q want %q", got, want)
	}
}

// TestP2SubqueryScalarWhere verifies a scalar subquery in the WHERE clause.
func TestP2SubqueryScalarWhere(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(x INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40);
		INSERT INTO t2 VALUES(3),(3);
	`)
	// WHERE a = (SELECT max(x) FROM t2) → a = 3.
	rows := p2QueryRows(t, db, "SELECT a FROM t1 WHERE a = (SELECT max(x) FROM t2);")
	want := "3"
	if got := flattenRows(rows); got != want {
		t.Errorf("scalar subquery in WHERE got %q want %q", got, want)
	}
}

// TestP2SubqueryIn verifies IN (SELECT ...) with NULL semantics.
func TestP2SubqueryIn(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(x INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40),(5,50),(NULL,60);
		INSERT INTO t2 VALUES(2),(4);
	`)
	// a IN (2,4) → rows 2,4.
	rows := p2QueryRows(t, db, "SELECT a FROM t1 WHERE a IN (SELECT x FROM t2) ORDER BY a;")
	want := "2 4"
	if got := flattenRows(rows); got != want {
		t.Errorf("IN (SELECT) got %q want %q", got, want)
	}
	// a NOT IN (2,4) → 1,3,5. The NULL row auto-assigns rowid 6 (INTEGER
	// PRIMARY KEY), so it becomes 6 and IS included; verified against SQLite.
	rows = p2QueryRows(t, db, "SELECT a FROM t1 WHERE a NOT IN (SELECT x FROM t2) ORDER BY a;")
	want = "1 3 5 6"
	if got := flattenRows(rows); got != want {
		t.Errorf("NOT IN (SELECT) got %q want %q", got, want)
	}
}

// TestP2SubqueryExists verifies EXISTS / NOT EXISTS.
func TestP2SubqueryExists(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(x INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40);
		INSERT INTO t2 VALUES(2),(4);
	`)
	rows := p2QueryRows(t, db, "SELECT a FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.x = t1.a) ORDER BY a;")
	want := "2 4"
	if got := flattenRows(rows); got != want {
		t.Errorf("EXISTS got %q want %q", got, want)
	}
	rows = p2QueryRows(t, db, "SELECT a FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.x = t1.a) ORDER BY a;")
	want = "1 3"
	if got := flattenRows(rows); got != want {
		t.Errorf("NOT EXISTS got %q want %q", got, want)
	}
}

// TestP2SubqueryCorrelated verifies a correlated subquery (inner references
// the outer row's column).
func TestP2SubqueryCorrelated(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(x INT, y INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40);
		INSERT INTO t2 VALUES(1,100),(2,200),(4,400);
	`)
	// Correlated scalar: return t2.y for the matching t2.x, NULL when absent.
	rows := p2QueryRows(t, db, "SELECT a, (SELECT y FROM t2 WHERE t2.x = t1.a) FROM t1 ORDER BY a;")
	want := "1 100 2 200 3 {} 4 400"
	if got := flattenRows(rows); got != want {
		t.Errorf("correlated subquery got %q want %q", got, want)
	}
}

// TestP2SubqueryDerivedTable verifies a derived table in FROM with alias and
// column list, plus a star projection over it.
func TestP2SubqueryDerivedTable(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30);
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM (SELECT a, b FROM t1 WHERE a >= 2) AS d ORDER BY a;")
	want := "2 20 3 30"
	if got := flattenRows(rows); got != want {
		t.Errorf("derived table got %q want %q", got, want)
	}
	// Derived table alias is usable for qualified references.
	rows = p2QueryRows(t, db, "SELECT d.a, d.b FROM (SELECT a, b FROM t1) AS d WHERE d.a = 1;")
	want = "1 10"
	if got := flattenRows(rows); got != want {
		t.Errorf("derived table alias got %q want %q", got, want)
	}
}

// TestP2SubqueryRowValueIn verifies a row-value subquery in IN.
func TestP2SubqueryRowValueIn(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(x INT, y INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40);
		INSERT INTO t2 VALUES(2,20),(3,30),(5,50);
	`)
	rows := p2QueryRows(t, db, "SELECT a FROM t1 WHERE (a, b) IN (SELECT x, y FROM t2) ORDER BY a;")
	want := "2 3"
	if got := flattenRows(rows); got != want {
		t.Errorf("row-value IN (SELECT) got %q want %q", got, want)
	}
}

// TestP2SubqueryScalarArityErrors verifies the "sub-select returns N columns
// - expected 1" errors for scalar subqueries returning multiple columns.
func TestP2SubqueryScalarArityErrors(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY);
		CREATE TABLE t2(x INT, y INT);
		INSERT INTO t1 VALUES(1);
		INSERT INTO t2 VALUES(2,20);
	`)
	// Multi-column subquery in a scalar SELECT position.
	res := db.Query("SELECT (SELECT x, y FROM t2) FROM t1;")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "sub-select returns 2 columns - expected 1") {
		t.Errorf("scalar multi-column subquery: got %v, want arity error", res.Error)
	}
	// Multi-column subquery on the right of IN with a scalar operand.
	res = db.Query("SELECT a IN (SELECT x, y FROM t2) FROM t1;")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "sub-select returns 2 columns - expected 1") {
		t.Errorf("scalar IN multi-column subquery: got %v, want arity error", res.Error)
	}
	// A row-value comparison with a multi-column subquery is legal (not an error).
	res = db.Query("SELECT (a, 1) = (SELECT x, y FROM t2) FROM t1;")
	if res.Error != nil {
		t.Errorf("row-value comparison with multi-column subquery: unexpected error %v", res.Error)
	}
}
