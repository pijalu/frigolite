package frigolite_test

// Native pins for the select1.test engine-visible contract (FULL-SUITE-DRIFT
// T26-select). Each pin mirrors one oracle-verified SQLite behavior that the
// TCL corpus exercises through select1.

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func mustOpenSelect1(t *testing.T) *frigolite.DB {
	t.Helper()
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestSelect1AggregateArityStar pins select1-2.6/2.9/2.14: a non-COUNT
// aggregate never accepts a * argument, and aggregates with an invalid
// argument count report sqlite3WrongNumArgs's canonical text with the name
// spelled as in the SQL (oracle 3.51: "wrong number of arguments to
// function min()" / "MAX()" / "sum()").
func TestSelect1AggregateArityStar(t *testing.T) {
	db := mustOpenSelect1(t)
	if err := db.Exec("CREATE TABLE test1(f1,f2); INSERT INTO test1 VALUES(11,22),(33,44);").Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sql, want string }{
		{"SELECT min(*) FROM test1", "wrong number of arguments to function min()"},
		{"SELECT MAX() FROM test1", "wrong number of arguments to function MAX()"},
		{"SELECT sum() FROM test1", "wrong number of arguments to function sum()"},
	} {
		r := db.Query(tc.sql)
		if r.Error == nil || r.Error.Error() != tc.want {
			t.Errorf("%s: got %v, want %q", tc.sql, r.Error, tc.want)
		}
	}
	// count(*) stays valid.
	if r := db.Query("SELECT count(*) FROM test1;"); r.Error != nil {
		t.Errorf("count(*): unexpected error %v", r.Error)
	}
}

// TestSelect1HavingAliasedAggregate pins select1-7.2/7.3: a HAVING aggregate
// whose argument references a SELECT alias that IS an aggregate nests
// aggregates — SQLite resolve.c reports "misuse of aliased aggregate m".
func TestSelect1HavingAliasedAggregate(t *testing.T) {
	db := mustOpenSelect1(t)
	if err := db.Exec("CREATE TABLE test1(f1,f2); INSERT INTO test1 VALUES(11,22),(33,44);").Error; err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"SELECT min(f1) AS m FROM test1 GROUP BY f1 HAVING max(m+5)<10",
		"SELECT min(f1) AS m FROM test1 GROUP BY f1 HAVING max(m+5)>10",
	} {
		r := db.Query(sql)
		if r.Error == nil || !strings.Contains(r.Error.Error(), "misuse of aliased aggregate m") {
			t.Errorf("%s: got %v, want misuse of aliased aggregate m", sql, r.Error)
		}
	}
}

// TestSelect1OrderByNegativeOrdinal pins select1-10.x: ORDER BY -1 on a
// two-column table is an out-of-range positional term (sqlite3ExprIsInteger
// folds the unary minus; resolve.c reports the canonical out-of-range text).
func TestSelect1OrderByNegativeOrdinal(t *testing.T) {
	db := mustOpenSelect1(t)
	if err := db.Exec("CREATE TABLE t5(a,b); INSERT INTO t5 VALUES(1,2),(3,4);").Error; err != nil {
		t.Fatal(err)
	}
	r := db.Query("SELECT * FROM t5 ORDER BY -1")
	if r.Error == nil || r.Error.Error() != "1st ORDER BY term out of range - should be between 1 and 2" {
		t.Errorf("ORDER BY -1: got %v, want 1st ORDER BY term out of range - should be between 1 and 2", r.Error)
	}
}

// TestSelect3GroupByOrdinalRange pins select3-1.x: integer GROUP BY terms are
// positional references into the result columns; 0 or past the width errors
// with "Nth GROUP BY term out of range - should be between 1 and M".
func TestSelect3GroupByOrdinalRange(t *testing.T) {
	db := mustOpenSelect1(t)
	if err := db.Exec("CREATE TABLE t1(log,x); INSERT INTO t1 VALUES(1,1),(2,2);").Error; err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"SELECT log, count(*) FROM t1 GROUP BY 0 ORDER BY log",
		"SELECT log, count(*) FROM t1 GROUP BY 3 ORDER BY log",
	} {
		r := db.Query(sql)
		if r.Error == nil || r.Error.Error() != "1st GROUP BY term out of range - should be between 1 and 2" {
			t.Errorf("%s: got %v, want 1st GROUP BY term out of range - should be between 1 and 2", sql, r.Error)
		}
	}
}

// TestSelect1AmbiguousJoinColumn pins select1-6.8/6.8b/6.8c: an unqualified
// reference naming a column that exists in more than one joined table errors
// "ambiguous column name: X" (X echoes the written reference, qualifier
// included).
func TestSelect1AmbiguousJoinColumn(t *testing.T) {
	db := mustOpenSelect1(t)
	if err := db.Exec("CREATE TABLE test1(f1,f2); CREATE TABLE test2(f1,f2); INSERT INTO test1 VALUES(11,22); INSERT INTO test2 VALUES(33,44);").Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sql, want string }{
		{"SELECT f1, f2 FROM test1, test2", "ambiguous column name: f1"},
		{"SELECT f2 FROM test1, test2", "ambiguous column name: f2"},
	} {
		r := db.Query(tc.sql)
		if r.Error == nil || r.Error.Error() != tc.want {
			t.Errorf("%s: got %v, want %q", tc.sql, r.Error, tc.want)
		}
	}
	// select1-6.8c's corpus expectation ("ambiguous column name: A.f1" for
	// two instances of the SAME table) is oracle drift: sqlite 3.51 returns
	// the A row (qualified alias references between same-table instances are
	// unambiguous), so the engine must NOT error here.
	r := db.Query("SELECT A.f1 FROM test1 AS A, test1 AS B")
	if r.Error != nil {
		t.Errorf("SELECT A.f1 FROM test1 AS A, test1 AS B: unexpected error %v (oracle returns 11)", r.Error)
	}
}

// TestSelect1QualifiedStarNoSuchTable pins select1-6.44a/6.44b: a t.* star
// whose table does not appear (or appears only under a different alias) in
// the FROM clause errors "no such table: tX".
func TestSelect1QualifiedStarNoSuchTable(t *testing.T) {
	db := mustOpenSelect1(t)
	if err := db.Exec("CREATE TABLE t3(a); CREATE TABLE t4(b); INSERT INTO t3 VALUES(1); INSERT INTO t4 VALUES(2);").Error; err != nil {
		t.Fatal(err)
	}
	r := db.Query("SELECT t5.* FROM t3, t4")
	if r.Error == nil || r.Error.Error() != "no such table: t5" {
		t.Errorf("t5.*: got %v, want no such table: t5", r.Error)
	}
	r = db.Query("SELECT t3.* FROM t3 AS x, t4")
	if r.Error == nil || r.Error.Error() != "no such table: t3" {
		t.Errorf("t3.* under alias x: got %v, want no such table: t3", r.Error)
	}
}

// TestSelect1FullColumnNames pins select1-6.1.1: PRAGMA full_column_names=ON
// takes precedence over short_column_names and qualifies plain column
// references as TABLE.COLUMN (the FROM spelling: alias when aliased).
func TestSelect1FullColumnNames(t *testing.T) {
	db := mustOpenSelect1(t)
	if err := db.Exec("CREATE TABLE test1(f1,f2); INSERT INTO test1 VALUES(11,22),(33,44);").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA full_column_names=on").Error; err != nil {
		t.Fatal(err)
	}
	r := db.Query("SELECT f1 FROM test1 ORDER BY f2")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Columns) != 1 || r.Columns[0] != "test1.f1" {
		t.Errorf("full_column_names columns = %v, want [test1.f1]", r.Columns)
	}
	r = db.Query("SELECT f1+F2 FROM test1 ORDER BY 1")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	// An expression never carries the table prefix; the name is the
	// expression text without injected spaces (SQLite uses the raw span).
	if r.Columns[0] != "f1+F2" {
		t.Errorf("expression column name = %q, want f1+F2", r.Columns[0])
	}
}
