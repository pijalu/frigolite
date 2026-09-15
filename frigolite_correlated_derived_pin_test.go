package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteCorrelatedDerivedTablePin pins tkt-54844eea3f: a derived table
// in FROM may reference an enclosing query's column when it executes inside
// a correlated subquery (SQLite re-runs the subquery per outer row). A bad
// reference with no enclosing scope still errors.
func TestSQLiteCorrelatedDerivedTablePin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE t1(a INTEGER PRIMARY KEY)")
	mustExec("INSERT INTO t1 VALUES(1)")
	mustExec("INSERT INTO t1 VALUES(4)")
	mustExec("CREATE TABLE t2(b INTEGER PRIMARY KEY)")
	mustExec("INSERT INTO t2 VALUES(1)")
	mustExec("CREATE TABLE t3(c INTEGER PRIMARY KEY)")
	mustExec("INSERT INTO t3 VALUES(1)")
	mustExec("INSERT INTO t3 VALUES(2)")

	r := db.Query("SELECT t3.c, (SELECT count(*) FROM t1 JOIN (SELECT DISTINCT t3.c AS p FROM t2) AS x ON t1.a = x.p) FROM t3")
	if r.Error != nil {
		t.Fatalf("correlated derived table: %v", r.Error)
	}
	want := []struct {
		c interface{}
		n interface{}
	}{{int64(1), int64(1)}, {int64(2), int64(0)}}
	for i, w := range want {
		if r.Rows[i][0] != w.c || r.Rows[i][1] != w.n {
			t.Fatalf("row %d: got %v, want {%v %v}", i, r.Rows[i], w.c, w.n)
		}
	}

	// Without an enclosing row the same reference is a true error.
	r = db.Query("SELECT * FROM t1 JOIN (SELECT t9.z AS p FROM t2) AS x ON t1.a = x.p")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such column: t9.z") {
		t.Fatalf("uncorrelated bad ref: got %v, want no-such-column error", r.Error)
	}
}
