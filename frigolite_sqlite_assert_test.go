package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// assertResult checks that a query result matches the expected space-separated values.
func assertResult(t *testing.T, res *Result, expected string) {
	t.Helper()
	if res.Error != nil {
		t.Errorf("unexpected error: %v\n  SQL in test: see test name", res.Error)
		return
	}
	var parts []string
	for _, row := range res.Rows {
		for _, val := range row {
			if val == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, fmt.Sprintf("%v", val))
			}
		}
	}
	got := strings.Join(parts, " ")
	want := strings.TrimSpace(expected)
	if got != want {
		t.Errorf("result mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

func TestCoreSelect(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	assertResult(t, db.Query("SELECT 1"), "1")
	assertResult(t, db.Query("SELECT 1 + 2"), "3")
	assertResult(t, db.Query("SELECT 3 * 4"), "12")
	assertResult(t, db.Query("SELECT 10 / 3"), "3")
	assertResult(t, db.Query("SELECT 10.0 / 3"), "3.3333333333333335")
	assertResult(t, db.Query("SELECT 'hello'"), "hello")
	assertResult(t, db.Query("SELECT 1 AS x, 2 AS y"), "1 2")
}

func TestCoreInsertAndSelect(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	assertResult(t, db.Query("SELECT COUNT(*) FROM t"), "0")

	db.Exec("INSERT INTO t VALUES (1, 'Alice')")
	db.Exec("INSERT INTO t VALUES (2, 'Bob')")
	db.Exec("INSERT INTO t VALUES (3, 'Charlie')")

	assertResult(t, db.Query("SELECT COUNT(*) FROM t"), "3")
	assertResult(t, db.Query("SELECT id FROM t WHERE name = 'Alice'"), "1")
	assertResult(t, db.Query("SELECT name FROM t WHERE id = 2"), "Bob")
	assertResult(t, db.Query("SELECT id, name FROM t ORDER BY id"), "1 Alice 2 Bob 3 Charlie")
	assertResult(t, db.Query("SELECT id FROM t ORDER BY name"), "1 2 3")
}

func TestCoreAggregates(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (v INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (2)")
	db.Exec("INSERT INTO t VALUES (3)")
	db.Exec("INSERT INTO t VALUES (NULL)")

	assertResult(t, db.Query("SELECT COUNT(*) FROM t"), "4")
	assertResult(t, db.Query("SELECT COUNT(v) FROM t"), "3")
	assertResult(t, db.Query("SELECT SUM(v) FROM t"), "6")
	assertResult(t, db.Query("SELECT MIN(v) FROM t"), "1")
	assertResult(t, db.Query("SELECT MAX(v) FROM t"), "3")
	assertResult(t, db.Query("SELECT AVG(v) FROM t"), "2")
	assertResult(t, db.Query("SELECT COUNT(DISTINCT v) FROM t"), "3")
}

func TestCoreGroupBy(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (grp TEXT, val INTEGER)")
	db.Exec("INSERT INTO t VALUES ('a', 1)")
	db.Exec("INSERT INTO t VALUES ('a', 2)")
	db.Exec("INSERT INTO t VALUES ('b', 3)")
	db.Exec("INSERT INTO t VALUES ('b', 4)")

	assertResult(t, db.Query("SELECT grp, SUM(val) FROM t GROUP BY grp ORDER BY grp"), "a 3 b 7")
	assertResult(t, db.Query("SELECT grp, COUNT(*) FROM t GROUP BY grp ORDER BY grp"), "a 2 b 2")
}

func TestCoreJoin(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE a (id INTEGER, name TEXT)")
	db.Exec("CREATE TABLE b (id INTEGER, val INTEGER)")
	db.Exec("INSERT INTO a VALUES (1, 'x')")
	db.Exec("INSERT INTO a VALUES (2, 'y')")
	db.Exec("INSERT INTO b VALUES (1, 100)")
	db.Exec("INSERT INTO b VALUES (2, 200)")

	assertResult(t, db.Query("SELECT a.name, b.val FROM a JOIN b ON a.id = b.id ORDER BY a.id"), "x 100 y 200")
	assertResult(t, db.Query("SELECT a.name FROM a LEFT JOIN b ON a.id = b.id ORDER BY a.id"), "x y")
}

func TestCoreSubquery(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	assertResult(t, db.Query("SELECT * FROM (SELECT 1 AS x)"), "1")
	assertResult(t, db.Query("SELECT x FROM (SELECT 1 AS x)"), "1")
	assertResult(t, db.Query("SELECT a FROM (SELECT x AS a FROM (SELECT 1 AS x))"), "1")
}

func TestCoreDistinctUnion(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (v INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (2)")

	assertResult(t, db.Query("SELECT DISTINCT v FROM t ORDER BY v"), "1 2")
	assertResult(t, db.Query("SELECT v FROM t UNION SELECT v FROM t ORDER BY v"), "1 2")
	assertResult(t, db.Query("SELECT v FROM t UNION ALL SELECT v FROM t"), "1 1 2 1 1 2")
}

func TestCoreBTreeSplit(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)")
	for i := 1; i <= 500; i++ {
		r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i))
		if r.Error != nil {
			t.Fatalf("insert %d failed: %v", i, r.Error)
		}
	}
	r := db.Query("SELECT COUNT(*), MIN(id), MAX(id) FROM t")
	assertResult(t, r, "500 1 500")
}

func TestCoreConstraints(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	// NOT NULL
	db.Exec("CREATE TABLE nn (a TEXT NOT NULL)")
	r := db.Exec("INSERT INTO nn VALUES (NULL)")
	if r.Error == nil {
		t.Error("expected NOT NULL constraint error")
	}

	// UNIQUE
	db.Exec("CREATE TABLE uq (id INTEGER UNIQUE)")
	db.Exec("INSERT INTO uq VALUES (1)")
	r = db.Exec("INSERT INTO uq VALUES (1)")
	if r.Error == nil {
		t.Error("expected UNIQUE constraint error")
	}

	// CHECK
	db.Exec("CREATE TABLE ck (a INTEGER CHECK (a > 0))")
	r = db.Exec("INSERT INTO ck VALUES (-5)")
	if r.Error == nil {
		t.Error("expected CHECK constraint error")
	}
	db.Exec("INSERT INTO ck VALUES (5)")
	assertResult(t, db.Query("SELECT COUNT(*) FROM ck"), "1")
}

func TestCoreLikeGlobRegexp(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (s TEXT)")
	db.Exec("INSERT INTO t VALUES ('hello')")
	db.Exec("INSERT INTO t VALUES ('world')")

	assertResult(t, db.Query("SELECT s FROM t WHERE s LIKE '%el%'"), "hello")
	assertResult(t, db.Query("SELECT s FROM t WHERE s GLOB 'h*'"), "hello")
	assertResult(t, db.Query("SELECT s FROM t WHERE s REGEXP '^w'"), "world")
	assertResult(t, db.Query("SELECT s FROM t WHERE s LIKE 'HELLO'"), "hello")
}

func TestCoreNullHandling(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	db.Exec("CREATE TABLE t (a INTEGER)")
	db.Exec("INSERT INTO t VALUES (1)")
	db.Exec("INSERT INTO t VALUES (NULL)")
	db.Exec("INSERT INTO t VALUES (3)")

	assertResult(t, db.Query("SELECT COUNT(*) FROM t"), "3")
	assertResult(t, db.Query("SELECT * FROM t WHERE a IS NULL"), "NULL")
	assertResult(t, db.Query("SELECT a FROM t WHERE a IS NOT NULL ORDER BY a"), "1 3")
	assertResult(t, db.Query("SELECT a FROM t WHERE a > 1 ORDER BY a"), "3")
}
