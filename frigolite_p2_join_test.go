package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestP2JoinInner verifies INNER JOIN with an ON predicate.
func TestP2JoinInner(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(c INT, d INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30);
		INSERT INTO t2 VALUES(10,100),(20,200),(40,400);
	`)
	rows := p2QueryRows(t, db, "SELECT a, d FROM t1 INNER JOIN t2 ON t1.b=t2.c ORDER BY a;")
	want := "1 100 2 200"
	if got := flattenRows(rows); got != want {
		t.Errorf("INNER JOIN got %q want %q", got, want)
	}
}

// TestP2JoinLeft verifies LEFT OUTER JOIN NULL-filling on the right side.
func TestP2JoinLeft(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(c INT, d INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30);
		INSERT INTO t2 VALUES(10,100),(40,400);
	`)
	rows := p2QueryRows(t, db, "SELECT a, d FROM t1 LEFT JOIN t2 ON t1.b=t2.c ORDER BY a;")
	want := "1 100 2 {} 3 {}"
	if got := flattenRows(rows); got != want {
		t.Errorf("LEFT JOIN got %q want %q", got, want)
	}
}

// TestP2JoinRight verifies RIGHT OUTER JOIN NULL-filling on the left side.
func TestP2JoinRight(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(c INT, d INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30);
		INSERT INTO t2 VALUES(10,100),(40,400);
	`)
	rows := p2QueryRows(t, db, "SELECT a, d FROM t1 RIGHT JOIN t2 ON t1.b=t2.c ORDER BY d;")
	want := "1 100 {} 400"
	if got := flattenRows(rows); got != want {
		t.Errorf("RIGHT JOIN got %q want %q", got, want)
	}
}

// TestP2JoinFull verifies FULL OUTER JOIN NULL-filling on both sides.
func TestP2JoinFull(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT);
		CREATE TABLE t2(c INT, d INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30);
		INSERT INTO t2 VALUES(10,100),(40,400);
	`)
	rows := p2QueryRows(t, db, "SELECT a, d FROM t1 FULL JOIN t2 ON t1.b=t2.c ORDER BY d;")
	want := "2 {} 3 {} 1 100 {} 400"
	if got := flattenRows(rows); got != want {
		t.Errorf("FULL JOIN got %q want %q", got, want)
	}
}

// TestP2JoinCross verifies CROSS JOIN (cartesian product).
func TestP2JoinCross(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INT);
		CREATE TABLE t2(b INT);
		INSERT INTO t1 VALUES(1),(2);
		INSERT INTO t2 VALUES(10),(20);
	`)
	rows := p2QueryRows(t, db, "SELECT a, b FROM t1 CROSS JOIN t2 ORDER BY a, b;")
	want := "1 10 1 20 2 10 2 20"
	if got := flattenRows(rows); got != want {
		t.Errorf("CROSS JOIN got %q want %q", got, want)
	}
}

// TestP2JoinComma verifies comma joins (cross) with WHERE.
func TestP2JoinComma(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INT);
		CREATE TABLE t2(b INT);
		INSERT INTO t1 VALUES(1),(2);
		INSERT INTO t2 VALUES(10),(20);
	`)
	rows := p2QueryRows(t, db, "SELECT a, b FROM t1, t2 WHERE a=1 ORDER BY b;")
	want := "1 10 1 20"
	if got := flattenRows(rows); got != want {
		t.Errorf("comma JOIN got %q want %q", got, want)
	}
}

// TestP2JoinUsing verifies USING column deduplication in the output.
func TestP2JoinUsing(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(id INT, a INT);
		CREATE TABLE t2(id INT, b INT);
		INSERT INTO t1 VALUES(1,10),(2,20);
		INSERT INTO t2 VALUES(1,100),(3,300);
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM t1 JOIN t2 USING(id) ORDER BY id;")
	want := "1 10 100"
	if got := flattenRows(rows); got != want {
		t.Errorf("USING JOIN got %q want %q", got, want)
	}
}

// TestP2JoinNatural verifies NATURAL JOIN merges common columns.
func TestP2JoinNatural(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(id INT, a INT);
		CREATE TABLE t2(id INT, b INT);
		INSERT INTO t1 VALUES(1,10),(2,20);
		INSERT INTO t2 VALUES(1,100),(3,300);
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM t1 NATURAL JOIN t2 ORDER BY id;")
	want := "1 10 100"
	if got := flattenRows(rows); got != want {
		t.Errorf("NATURAL JOIN got %q want %q", got, want)
	}
}

// TestP2JoinNaturalLeft verifies NATURAL LEFT JOIN NULL-fills the right.
func TestP2JoinNaturalLeft(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(id INT, a INT);
		CREATE TABLE t2(id INT, b INT);
		INSERT INTO t1 VALUES(1,10),(2,20);
		INSERT INTO t2 VALUES(1,100);
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM t1 NATURAL LEFT JOIN t2 ORDER BY id;")
	want := "1 10 100 2 20 {}"
	if got := flattenRows(rows); got != want {
		t.Errorf("NATURAL LEFT JOIN got %q want %q", got, want)
	}
}

// TestP2JoinOnNonEqui verifies ON with a non-equality predicate.
func TestP2JoinOnNonEqui(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INT);
		CREATE TABLE t2(b INT);
		INSERT INTO t1 VALUES(1),(2),(3);
		INSERT INTO t2 VALUES(2),(3),(4);
	`)
	rows := p2QueryRows(t, db, "SELECT a, b FROM t1 JOIN t2 ON a < b ORDER BY a, b;")
	want := "1 2 1 3 1 4 2 3 2 4 3 4"
	if got := flattenRows(rows); got != want {
		t.Errorf("non-equi ON JOIN got %q want %q", got, want)
	}
}

// TestP2JoinStarOrder verifies SELECT * column order: left then right.
func TestP2JoinStarOrder(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INT, b INT);
		CREATE TABLE t2(c INT);
		INSERT INTO t1 VALUES(1,2);
		INSERT INTO t2 VALUES(3);
	`)
	rows := p2QueryRows(t, db, "SELECT * FROM t1 JOIN t2 ON 1;")
	want := "1 2 3"
	if got := flattenRows(rows); got != want {
		t.Errorf("star order got %q want %q", got, want)
	}
}

// TestP2JoinQualified verifies qualified column resolution in joins.
func TestP2JoinQualified(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INT, b INT);
		CREATE TABLE t2(a INT, c INT);
		INSERT INTO t1 VALUES(1,10);
		INSERT INTO t2 VALUES(1,100);
	`)
	rows := p2QueryRows(t, db, "SELECT t1.a, t2.a FROM t1 JOIN t2 ON t1.a=t2.a;")
	want := "1 1"
	if got := flattenRows(rows); got != want {
		t.Errorf("qualified join got %q want %q", got, want)
	}
}

// TestP2JoinSelf verifies self-joins with aliases.
func TestP2JoinSelf(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INT, b INT);
		INSERT INTO t1 VALUES(1,10),(2,20),(3,30);
	`)
	rows := p2QueryRows(t, db, "SELECT x.a, y.a FROM t1 x JOIN t1 y ON x.b=y.b-10 ORDER BY x.a;")
	want := "1 2 2 3"
	if got := flattenRows(rows); got != want {
		t.Errorf("self join got %q want %q", got, want)
	}
}

// TestP2JoinThreeWay verifies a three-table join chain.
func TestP2JoinThreeWay(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t1(a INT, b INT);
		CREATE TABLE t2(b INT, c INT);
		CREATE TABLE t3(c INT, d INT);
		INSERT INTO t1 VALUES(1,10);
		INSERT INTO t2 VALUES(10,20);
		INSERT INTO t3 VALUES(20,30);
	`)
	rows := p2QueryRows(t, db, "SELECT a, d FROM t1 JOIN t2 ON t1.b=t2.b JOIN t3 ON t2.c=t3.c;")
	want := "1 30"
	if got := flattenRows(rows); got != want {
		t.Errorf("three-way join got %q want %q", got, want)
	}
}

func mustExec(t *testing.T, db *DB, sql string) {
	t.Helper()
	if r := db.Exec(sql); r.Error != nil {
		t.Fatalf("exec %q: %v", sql, r.Error)
	}
}

func p2QueryRows(t *testing.T, db *DB, sql string) [][]string {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("query %q: %v", sql, res.Error)
	}
	var out [][]string
	for _, row := range res.Rows {
		var s []string
		for _, v := range row {
			if v == nil {
				s = append(s, "{}")
			} else {
				s = append(s, fmt.Sprintf("%v", v))
			}
		}
		out = append(out, s)
	}
	return out
}

func flattenRows(rows [][]string) string {
	var parts []string
	for _, row := range rows {
		parts = append(parts, row...)
	}
	return strings.Join(parts, " ")
}
