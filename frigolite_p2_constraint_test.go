package frigolite

import (
	"testing"
)

// P2.CONSTRAINT pre-tests: hand-written tests for the P2.CONSTRAINT testgen
// packages (p_8_3_names, createtab, resolver01, trustschema1, upfrom1-4),
// written BEFORE/WHILE fixing the testgen failures. Each expectation was
// verified against sqlite3 3.51 (via the sqlite3 CLI) as the oracle.

// TestP2Constraint is the top-level entry for the P2 CONSTRAINT pre-tests.
// The verify command runs it via `go test -run TestP2Constraint -count=1 .`
func TestP2Constraint(t *testing.T) {
	for _, sub := range []string{
		"OrderByAlias", "SubqueryAliasRef", "UpdateFrom",
	} {
		ok := t.Run(sub, func(t *testing.T) {
			switch sub {
			case "OrderByAlias":
				TestP2Constraint_OrderByAlias(t)
			case "SubqueryAliasRef":
				TestP2Constraint_SubqueryAliasRef(t)
			case "UpdateFrom":
				TestP2Constraint_UpdateFrom(t)
			}
		})
		if !ok {
			t.Fail()
		}
	}
}

// TestP2Constraint_OrderByAlias verifies resolver01-4.1: a bare SELECT-list
// alias in ORDER BY resolves to the alias value, while the same name inside a
// function (ORDER BY lower(m)) resolves to the source column.
func TestP2Constraint_OrderByAlias(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE TABLE t4(m CHAR(2));
	  INSERT INTO t4 VALUES('az');
	  INSERT INTO t4 VALUES('by');
	  INSERT INTO t4 VALUES('cx');`); res.Error != nil {
		t.Fatal(res.Error)
	}

	// Bare alias: ORDER BY m sorts by substr(m,2) -> x,y,z.
	r := db.Query(`SELECT '1', substr(m,2) AS m FROM t4 ORDER BY m;`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got := flattenResult(r)
	if got != "1 x 1 y 1 z" {
		t.Errorf("ORDER BY bare alias: got %q, want %q", got, "1 x 1 y 1 z")
	}

	// Alias with explicit COLLATE: still the alias value, binary sort.
	r = db.Query(`SELECT '2', substr(m,2) AS m FROM t4 ORDER BY m COLLATE binary;`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got = flattenResult(r)
	if got != "2 x 2 y 2 z" {
		t.Errorf("ORDER BY alias COLLATE: got %q, want %q", got, "2 x 2 y 2 z")
	}

	// Name inside a function resolves to the source column m ('az','by','cx').
	r = db.Query(`SELECT '3', substr(m,2) AS m FROM t4 ORDER BY lower(m);`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got = flattenResult(r)
	if got != "3 z 3 y 3 x" {
		t.Errorf("ORDER BY lower(m): got %q, want %q", got, "3 z 3 y 3 x")
	}
}

// TestP2Constraint_SubqueryAliasRef verifies resolver01-7.1: a scalar
// subquery in WHERE may reference the outer SELECT's result alias.
func TestP2Constraint_SubqueryAliasRef(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := db.Query(`SELECT 2 AS x WHERE (SELECT x AS y WHERE 3>y);`)
	if r.Error != nil {
		t.Fatalf("subquery alias ref: %v", r.Error)
	}
	got := flattenResult(r)
	if got != "2" {
		t.Errorf("subquery alias ref: got %q, want %q", got, "2")
	}
}

// TestP2Constraint_UpdateFrom verifies UPDATE ... FROM (SQLite 3.33+): the
// SET expressions and WHERE can reference the FROM table's columns by table
// name, and the target table by its own name (upfrom1-1.1.2).
func TestP2Constraint_UpdateFrom(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE TABLE t2(a INTEGER PRIMARY KEY, b INTEGER, c INTEGER) WITHOUT ROWID;
	  INSERT INTO t2 VALUES(1, 2, 3);
	  INSERT INTO t2 VALUES(4, 5, 6);
	  INSERT INTO t2 VALUES(7, 8, 9);
	  CREATE TABLE chng(a INTEGER, b INTEGER, c INTEGER);
	  INSERT INTO chng VALUES(1, 100, 1000);
	  INSERT INTO chng VALUES(7, 700, 7000);`); res.Error != nil {
		t.Fatal(res.Error)
	}

	r := db.Query(`UPDATE t2 SET b = chng.b, c = chng.c FROM chng WHERE chng.a = t2.a;
	  SELECT * FROM t2 ORDER BY a;`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got := flattenResult(r)
	want := "1 100 1000 4 5 6 7 700 7000"
	if got != want {
		t.Errorf("UPDATE FROM: got %q, want %q", got, want)
	}
}
