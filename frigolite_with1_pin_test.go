package frigolite

import (
	"testing"
)

// Pins for with1-5.6.4/5.6.5 and with1-17.1 (WITH/CTE resolution).

// with1-5.6.4/5.6.5: SQLite's withExpand checks the declared column list
// against the LEFTMOST (anchor) member's width BEFORE the compound arity
// check — a mismatching anchor reports "table i has N values for M columns"
// while a matching anchor with a wider later arm reports the compound arity
// error.
func TestWith1CTEArityPrecedence(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query("WITH i(x) AS ( SELECT 1, 2 UNION ALL SELECT 1 ) SELECT * FROM i")
	if r.Error == nil || r.Error.Error() != "table i has 2 values for 1 columns" {
		t.Fatalf("5.6.4: got %v, want table-arity error", r.Error)
	}
	r = db.Query("WITH i(x) AS ( SELECT 1 UNION ALL SELECT 1, 2 ) SELECT * FROM i")
	if r.Error == nil || r.Error.Error() != "SELECTs to the left and right of UNION ALL do not have the same number of result columns" {
		t.Fatalf("5.6.5: got %v, want compound arity error", r.Error)
	}
}

// with1-17.1: a WITH clause nested inside a CTE body scopes its inner CTEs
// over the ENTIRE body compound — both arms of
// "WITH x(a) AS (WITH y(b) AS (SELECT 10) SELECT 9 UNION ALL SELECT * FROM y)
// SELECT * FROM x" must resolve y, including during the static
// compound-width validation that runs before the body executes.
func TestWith1NestedWithScopesCompoundBody(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query("WITH x(a) AS (\n    WITH y(b) AS (SELECT 10)\n    SELECT 9 UNION ALL SELECT * FROM y\n  )\n  SELECT * FROM x")
	if r.Error != nil {
		t.Fatalf("17.1: query error: %v", r.Error)
	}
	if got := flattenResult(r); got != "9 10" {
		t.Fatalf("17.1: got [%s], want [9 10]", got)
	}
	// Column-name propagation through the nested-WITH body (with1-17.2).
	r = db.Query("WITH x AS (\n    WITH y(b) AS (SELECT 10)\n    SELECT * FROM y UNION ALL SELECT * FROM y\n  )\n  SELECT * FROM x")
	if r.Error != nil {
		t.Fatalf("17.2: query error: %v", r.Error)
	}
	if len(r.Columns) != 1 || r.Columns[0] != "b" {
		t.Fatalf("17.2: columns %v, want [b]", r.Columns)
	}
}
