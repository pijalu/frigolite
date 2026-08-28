package frigolite

import (
	"strings"
	"testing"
)

// TestP2AggregateCount verifies COUNT(*) vs COUNT(col) vs COUNT(DISTINCT col)
// with NULL handling.
func TestP2AggregateCount(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b TEXT);
		INSERT INTO t VALUES(1,'x'),(2,NULL),(NULL,'y'),(3,'x');
	`)
	// COUNT(*) counts rows; COUNT(col) skips NULLs; COUNT(DISTINCT col)
	// counts distinct non-NULL values.
	rows := p2QueryRows(t, db, "SELECT count(*), count(a), count(b), count(DISTINCT b) FROM t;")
	if got := flattenRows(rows); got != "4 3 3 2" {
		t.Errorf("COUNT got %q want %q", got, "4 3 3 2")
	}
}

// TestP2AggregateSumAvgMinMaxTotal verifies aggregate functions with NULLs.
func TestP2AggregateSumAvgMinMaxTotal(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b TEXT);
		INSERT INTO t VALUES(1,'x'),(2,NULL),(NULL,'y'),(3,'x');
	`)
	// SUM ignores NULLs; AVG returns a REAL; MIN/MAX pick extremes; TOTAL
	// always returns a REAL (0.0 on empty).
	rows := p2QueryRows(t, db, "SELECT sum(a), avg(a), min(a), max(a), total(a) FROM t;")
	if got := flattenRows(rows); got != "6 2 1 3 6" {
		t.Errorf("SUM/AVG/MIN/MAX/TOTAL got %q want %q", got, "6 2 1 3 6")
	}
}

// TestP2AggregateEmptyTable verifies aggregate semantics over an empty table:
// COUNT returns one row with 0; SUM/AVG/MIN/MAX return one row with NULL;
// TOTAL returns 0.0.
func TestP2AggregateEmptyTable(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, "CREATE TABLE empty(x INT);")
	rows := p2QueryRows(t, db, "SELECT count(*) FROM empty;")
	if got := flattenRows(rows); got != "0" {
		t.Errorf("COUNT(*) empty got %q want %q", got, "0")
	}
	rows = p2QueryRows(t, db, "SELECT sum(x), avg(x), min(x), max(x), total(x) FROM empty;")
	if got := flattenRows(rows); got != "{} {} {} {} 0" {
		t.Errorf("empty aggregates got %q want %q", got, "{} {} {} {} 0")
	}
}

// TestP2AggregateGroupConcat verifies GROUP_CONCAT with and without a
// separator, plus DISTINCT ordering inside the aggregate.
func TestP2AggregateGroupConcat(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(b TEXT);
		INSERT INTO t VALUES('x'),('y'),('x');
	`)
	rows := p2QueryRows(t, db, "SELECT group_concat(b), group_concat(b, '-') FROM t;")
	if got := flattenRows(rows); got != "x,y,x x-y-x" {
		t.Errorf("GROUP_CONCAT got %q want %q", got, "x,y,x x-y-x")
	}
	rows = p2QueryRows(t, db, "SELECT group_concat(DISTINCT b ORDER BY b) FROM t;")
	if got := flattenRows(rows); got != "x,y" {
		t.Errorf("GROUP_CONCAT DISTINCT got %q want %q", got, "x,y")
	}
}

// TestP2AggregateGroupBy verifies GROUP BY with one key, multiple keys, and
// an expression key.
func TestP2AggregateGroupBy(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b INT, c INT);
		INSERT INTO t VALUES(1,10,100),(2,20,200),(1,30,300),(2,40,400);
	`)
	// Single key
	rows := p2QueryRows(t, db, "SELECT a, sum(b) FROM t GROUP BY a ORDER BY a;")
	if got := flattenRows(rows); got != "1 40 2 60" {
		t.Errorf("GROUP BY one key got %q want %q", got, "1 40 2 60")
	}
	// Expression key
	rows = p2QueryRows(t, db, "SELECT a%2, sum(b) FROM t GROUP BY a%2 ORDER BY 1;")
	if got := flattenRows(rows); got != "0 60 1 40" {
		t.Errorf("GROUP BY expr got %q want %q", got, "0 60 1 40")
	}
	// Multiple keys: c depends on a, so (a,b) groups are distinct
	rows = p2QueryRows(t, db, "SELECT a, b, count(*) FROM t GROUP BY a, b ORDER BY a, b;")
	if got := flattenRows(rows); got != "1 10 1 1 30 1 2 20 1 2 40 1" {
		t.Errorf("GROUP BY two keys got %q want %q", got, "1 10 1 1 30 1 2 20 1 2 40 1")
	}
}

// TestP2AggregateHaving verifies HAVING with an aggregate and with a grouping
// column.
func TestP2AggregateHaving(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b INT);
		INSERT INTO t VALUES(1,10),(1,20),(2,30),(2,40),(3,50);
	`)
	// HAVING with an aggregate
	rows := p2QueryRows(t, db, "SELECT a, sum(b) FROM t GROUP BY a HAVING sum(b) > 50 ORDER BY a;")
	if got := flattenRows(rows); got != "2 70" {
		t.Errorf("HAVING aggregate got %q want %q", got, "2 70")
	}
	// HAVING with a grouping column
	rows = p2QueryRows(t, db, "SELECT a, sum(b) FROM t GROUP BY a HAVING a > 1 ORDER BY a;")
	if got := flattenRows(rows); got != "2 70 3 50" {
		t.Errorf("HAVING grouping col got %q want %q", got, "2 70 3 50")
	}
}

// TestP2AggregateSelectDistinct verifies SELECT DISTINCT and DISTINCT inside
// aggregates are independent.
func TestP2AggregateSelectDistinct(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b TEXT);
		INSERT INTO t VALUES(1,'x'),(2,'y'),(1,'x'),(3,'z');
	`)
	rows := p2QueryRows(t, db, "SELECT count(DISTINCT a), sum(DISTINCT a) FROM t;")
	if got := flattenRows(rows); got != "3 6" {
		t.Errorf("DISTINCT in aggregates got %q want %q", got, "3 6")
	}
	rows = p2QueryRows(t, db, "SELECT DISTINCT a FROM t ORDER BY a;")
	if got := flattenRows(rows); got != "1 2 3" {
		t.Errorf("SELECT DISTINCT got %q want %q", got, "1 2 3")
	}
	// DISTINCT aggregate result column is NOT deduplicated by SELECT DISTINCT
	rows = p2QueryRows(t, db, "SELECT DISTINCT count(*) FROM t;")
	if got := flattenRows(rows); got != "4" {
		t.Errorf("DISTINCT over aggregate got %q want %q", got, "4")
	}
}

// TestP2AggregateBareColumnGroupBy verifies SQLite's default (non-ONLY_FULL_
// GROUP_BY) behavior: a bare column not in GROUP BY is allowed and takes a
// value from an arbitrary row of the group.
func TestP2AggregateBareColumnGroupBy(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b INT);
		INSERT INTO t VALUES(1,2),(1,3);
	`)
	rows := p2QueryRows(t, db, "SELECT a, b FROM t GROUP BY a;")
	if got := flattenRows(rows); got != "1 2" {
		t.Errorf("bare column in GROUP BY got %q want %q", got, "1 2")
	}
}

// TestP2AggregateDistinctZeroArgs verifies DISTINCT aggregates must have
// exactly one argument.
func TestP2AggregateDistinctZeroArgs(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(x);")
	res := db.Exec("SELECT count(DISTINCT) FROM t;")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "DISTINCT aggregates must have exactly one argument") {
		t.Errorf("count(DISTINCT) error = %v, want DISTINCT aggregates error", res.Error)
	}
}
