package frigolite

import (
	"strings"
	"testing"
)

// TestP2SetopsUnionVsUnionAll verifies UNION deduplicates while UNION ALL
// preserves every row, with NULLs deduplicated by UNION.
func TestP2SetopsUnionVsUnionAll(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(x);
		INSERT INTO t1 VALUES(1),(2),(2),(NULL),(3);
		CREATE TABLE t2(a);
		INSERT INTO t2 VALUES(2),(3),(4),(NULL);
	`)
	// UNION: dedup + NULL dedup, sorted by value
	rows := p2QueryRows(t, db, "SELECT x FROM t1 UNION SELECT a FROM t2;")
	if got := flattenRows(rows); got != "{} 1 2 3 4" {
		t.Errorf("UNION got %q want %q", got, "{} 1 2 3 4")
	}
	// UNION ALL: no dedup (NULL appears 4 times)
	rows = p2QueryRows(t, db, "SELECT x FROM t1 UNION ALL SELECT a FROM t2;")
	if got := flattenRows(rows); got != "1 2 2 {} 3 2 3 4 {}" {
		t.Errorf("UNION ALL got %q want %q", got, "1 2 2 {} 3 2 3 4 {}")
	}
}

// TestP2SetopsIntersectExcept verifies INTERSECT and EXCEPT semantics with
// NULL dedup.
func TestP2SetopsIntersectExcept(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(x);
		INSERT INTO t1 VALUES(1),(2),(3),(NULL);
		CREATE TABLE t2(a);
		INSERT INTO t2 VALUES(2),(3),(4),(NULL);
	`)
	rows := p2QueryRows(t, db, "SELECT x FROM t1 INTERSECT SELECT a FROM t2;")
	if got := flattenRows(rows); got != "{} 2 3" {
		t.Errorf("INTERSECT got %q want %q", got, "{} 2 3")
	}
	rows = p2QueryRows(t, db, "SELECT x FROM t1 EXCEPT SELECT a FROM t2;")
	if got := flattenRows(rows); got != "1" {
		t.Errorf("EXCEPT got %q want %q", got, "1")
	}
	// Reverse EXCEPT
	rows = p2QueryRows(t, db, "SELECT a FROM t2 EXCEPT SELECT x FROM t1;")
	if got := flattenRows(rows); got != "4" {
		t.Errorf("EXCEPT reverse got %q want %q", got, "4")
	}
}

// TestP2SetopsColumnCountMismatch verifies the arity error message.
func TestP2SetopsColumnCountMismatch(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t1(x); CREATE TABLE t2(a, b);")
	res := db.Query("SELECT x FROM t1 UNION SELECT a, b FROM t2;")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "do not have the same number of result columns") {
		t.Errorf("expected column-count mismatch error, got %v", res.Error)
	}
}

// TestP2SetopsResultNamesFromFirstArm verifies result column names come from
// the first arm of the compound.
func TestP2SetopsResultNamesFromFirstArm(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(x INTEGER, y TEXT);
		INSERT INTO t1 VALUES(1, 'one');
		CREATE TABLE t2(a INTEGER, b TEXT);
		INSERT INTO t2 VALUES(2, 'two');
	`)
	res := db.Query("SELECT x AS first_col, y AS second_col FROM t1 UNION ALL SELECT a, b FROM t2;")
	if res.Error != nil {
		t.Fatalf("query: %v", res.Error)
	}
	if len(res.Columns) != 2 || res.Columns[0] != "first_col" || res.Columns[1] != "second_col" {
		t.Errorf("columns from first arm got %v want [first_col second_col]", res.Columns)
	}
	// Unaliased: names come from the first arm's column references
	res = db.Query("SELECT x, y FROM t1 UNION SELECT a, b FROM t2;")
	if res.Error != nil {
		t.Fatalf("query: %v", res.Error)
	}
	if len(res.Columns) != 2 || res.Columns[0] != "x" || res.Columns[1] != "y" {
		t.Errorf("unaliased columns got %v want [x y]", res.Columns)
	}
}

// TestP2SetopsOrderByLimit verifies ORDER BY / LIMIT apply to the whole
// compound result, not an individual arm.
func TestP2SetopsOrderByLimit(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(x);
		INSERT INTO t1 VALUES(3),(1);
		CREATE TABLE t2(a);
		INSERT INTO t2 VALUES(4),(2);
	`)
	rows := p2QueryRows(t, db, "SELECT x FROM t1 UNION SELECT a FROM t2 ORDER BY 1;")
	if got := flattenRows(rows); got != "1 2 3 4" {
		t.Errorf("ORDER BY got %q want %q", got, "1 2 3 4")
	}
	rows = p2QueryRows(t, db, "SELECT x FROM t1 UNION SELECT a FROM t2 ORDER BY 1 DESC LIMIT 2;")
	if got := flattenRows(rows); got != "4 3" {
		t.Errorf("ORDER BY DESC LIMIT got %q want %q", got, "4 3")
	}
	// UNION ALL with ORDER BY/LIMIT
	rows = p2QueryRows(t, db, "SELECT x FROM t1 UNION ALL SELECT a FROM t2 ORDER BY 1 LIMIT 3;")
	if got := flattenRows(rows); got != "1 2 3" {
		t.Errorf("UNION ALL ORDER BY LIMIT got %q want %q", got, "1 2 3")
	}
}

// TestP2SetopsNullDedup verifies NULLs are equal for UNION/INTERSECT/EXCEPT
// dedup (DISTINCT rules).
func TestP2SetopsNullDedup(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(x);
		INSERT INTO t1 VALUES(NULL),(NULL),(1);
	`)
	rows := p2QueryRows(t, db, "SELECT x FROM t1 UNION SELECT x FROM t1;")
	if got := flattenRows(rows); got != "{} 1" {
		t.Errorf("UNION NULL dedup got %q want %q", got, "{} 1")
	}
	rows = p2QueryRows(t, db, "SELECT x FROM t1 INTERSECT SELECT x FROM t1;")
	if got := flattenRows(rows); got != "{} 1" {
		t.Errorf("INTERSECT NULL dedup got %q want %q", got, "{} 1")
	}
	rows = p2QueryRows(t, db, "SELECT x FROM t1 EXCEPT SELECT 1;")
	if got := flattenRows(rows); got != "{}" {
		t.Errorf("EXCEPT NULL dedup got %q want %q", got, "{}")
	}
}

// TestP2SetopsChain verifies chains of 3+ arms and mixed operators evaluate
// left to right.
func TestP2SetopsChain(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(x);
		INSERT INTO t1 VALUES(1),(2),(3);
	`)
	// 3-arm UNION chain dedups across all arms
	rows := p2QueryRows(t, db, "SELECT x FROM t1 UNION SELECT x FROM t1 UNION SELECT x FROM t1;")
	if got := flattenRows(rows); got != "1 2 3" {
		t.Errorf("3-arm chain got %q want %q", got, "1 2 3")
	}
	// Mixed operators: (1 UNION 2) INTERSECT 2 = {2} (left to right)
	rows = p2QueryRows(t, db, "SELECT 1 UNION SELECT 2 INTERSECT SELECT 2;")
	if got := flattenRows(rows); got != "2" {
		t.Errorf("mixed ops got %q want %q", got, "2")
	}
}

// TestP2SetopsCrossAffinityCoercion verifies int/real coercion in dedup.
func TestP2SetopsCrossAffinityCoercion(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	// 1 and 1.0 are equal numerically → UNION dedups to one row
	rows := p2QueryRows(t, db, "SELECT 1.0 UNION SELECT 1;")
	if got := flattenRows(rows); got != "1" {
		t.Errorf("1.0 UNION 1 got %q want %q", got, "1")
	}
	// 1 and '1' are NOT equal (different types, no affinity) → two rows
	rows = p2QueryRows(t, db, "SELECT 1 UNION SELECT '1';")
	if got := flattenRows(rows); got != "1 1" {
		t.Errorf("1 UNION '1' got %q want %q", got, "1 1")
	}
	// INTERSECT with numeric coercion
	rows = p2QueryRows(t, db, "SELECT 1.0 INTERSECT SELECT 1;")
	if got := flattenRows(rows); got != "1" {
		t.Errorf("1.0 INTERSECT 1 got %q want %q", got, "1")
	}
}
