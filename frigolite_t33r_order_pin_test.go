package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// t33rFlat flattens a Result's rows with {} for NULL (TCL convention).
func t33rFlat(r *Result) string {
	parts := make([]string, 0, 16)
	for _, row := range r.Rows {
		for _, v := range row {
			if v == nil {
				parts = append(parts, "{}")
			} else {
				parts = append(parts, fmt.Sprintf("%v", v))
			}
		}
	}
	return strings.Join(parts, " ")
}

func t33rExec(t *testing.T, db *DB, sqls ...string) {
	t.Helper()
	for _, s := range sqls {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}
}

func t33rQuery(t *testing.T, db *DB, query, want string) {
	t.Helper()
	r := db.Query(query)
	if r.Error != nil {
		t.Errorf("query error: %v\n  sql: %s", r.Error, query)
		return
	}
	if got := t33rFlat(r); got != want {
		t.Errorf("got [%s] want [%s]\n  sql: %s", got, want, query)
	}
}

// TestT33rAutoindexPin pins the sqlite_autoindex ordinal-mapping fix
// (T33r-order, whereA-3.3): an INTEGER PRIMARY KEY rowid alias owns no
// implicit index and consumes no autoindex slot (DDL createAutoIndexes), so
// sqlite_autoindex_<t>_1 on `CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`
// is the UNIQUE(b) index — value-ordered, collation-ordered storage — not a
// rowid-keyed tree. The stored key order feeds index-satisfied ORDER BY
// emission: ORDER BY b over a b>0 seek emits the stored b order.
func TestT33rAutoindexPin(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t33rExec(t, db,
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b UNIQUE, c);",
		"INSERT INTO t1 VALUES(1,2,3),(2,'hello','world'),(3,4.53,NULL);")
	t33rQuery(t, db, "SELECT * FROM t1 WHERE b>0 ORDER BY b", "1 2 3 3 4.53 {} 2 hello world")
}

// TestT33rIndexOrderPins pins the ORDER-BY-via-index satisfaction fixes
// (T33r-order): an index satisfies an ORDER BY only when each term's
// collation matches the index column's (a COLLATE nocase index cannot
// provide a BINARY ordering — distinct-9.x) and each term's NULLS FIRST /
// NULLS LAST placement matches the scan's null order (an ASC index scans
// NULLs first, a DESC one last — nulls1-4.3/5.2). Otherwise a sorter runs
// under the term's own collation and null ordering.
func TestT33rIndexOrderPins(t *testing.T) {
	setup := []string{
		"CREATE TABLE t1(a, b);",
		"INSERT INTO t1 VALUES('a','a'),('a','b'),('a','c'),('b','a'),('b','b'),('b','c'),('a','a'),('b','b'),('A','A'),('B','B');",
	}
	t.Run("distinct nocase index vs binary ORDER BY", func(t *testing.T) {
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t33rExec(t, db, append(setup, "CREATE INDEX i1 ON t1(a COLLATE nocase, b COLLATE nocase);")...)
		t33rQuery(t, db, "SELECT DISTINCT a, b FROM t1 ORDER BY a, b", "A A B B a a a b a c b a b b b c")
		t33rQuery(t, db, "SELECT a, b FROM t1 ORDER BY a, b", "A A B B a a a a a b a c b a b b b b b c")
	})
	t.Run("NULLS LAST ignores ASC index order", func(t *testing.T) {
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t33rExec(t, db, "CREATE TABLE tx(a INTEGER PRIMARY KEY, b, c);",
			"INSERT INTO tx VALUES(1,1,1),(2,NULL,2),(3,3,3),(4,NULL,4),(5,5,5);",
			"CREATE INDEX i1 ON tx(b);")
		t33rQuery(t, db, "SELECT * FROM tx ORDER BY b NULLS FIRST", "2 {} 2 4 {} 4 1 1 1 3 3 3 5 5 5")
		t33rQuery(t, db, "SELECT * FROM tx ORDER BY b NULLS LAST", "1 1 1 3 3 3 5 5 5 2 {} 2 4 {} 4")
	})
	t.Run("NULLS LAST with IN-driven index scan", func(t *testing.T) {
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t33rExec(t, db, "CREATE TABLE t4(a, b, c);",
			"INSERT INTO t4 VALUES(1,1,11),(1,2,12),(1,NULL,1),(2,NULL,1),(2,2,12),(2,1,11),(3,NULL,1),(3,2,12),(3,NULL,3);",
			"CREATE INDEX t4ab ON t4(a, b);")
		t33rQuery(t, db, "SELECT * FROM t4 WHERE a IN (1,2,3) ORDER BY a, b NULLS LAST",
			"1 1 11 1 2 12 1 {} 1 2 1 11 2 2 12 2 {} 1 3 2 12 3 {} 1 3 {} 3")
	})
}

// TestT33rOmitUnusedPins pins the omit-unused-subquery-column fixes
// (T33r-order): a VALUES-derived table's output columns are named
// column1..columnN so an outer column2 reference marks the column used
// (values-9.2); the use-walk descends into CTE bodies, whose references
// resolve against enclosing scopes (with3-4.0); and a materialized
// subquery exposes only its output columns, so a window subquery's
// unprojected source columns cannot shadow a same-named output column of
// another FROM source (unionall-4.3).
func TestT33rOmitUnusedPins(t *testing.T) {
	t.Run("values-9.2 scalar sub over VALUES derived", func(t *testing.T) {
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t33rQuery(t, db, "\n  VALUES (1, 2), (3, 4), (\n    ( SELECT column1 FROM ( VALUES (5, 6), (7, 8) ) ),\n    ( SELECT max(column2) FROM ( VALUES (5, 1), (7, 6) ) )\n  )\n", "1 2 3 4 5 6")
	})
	t.Run("with3-4.0 nested CTE references derived column", func(t *testing.T) {
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t33rQuery(t, db, "\n  WITH t5(t5col1) AS (\n    SELECT (\n      WITH t3(t3col1) AS (\n        WITH t2 AS (\n          WITH t1 AS (SELECT 1 AS c1 GROUP BY 1) \n          SELECT a.c1 FROM t1 AS a, t1 AS b\n          WHERE anoncol1 = 1\n        )\n        SELECT (SELECT 1 FROM t2) FROM t2\n      ) \n      SELECT t3col1 FROM t3 WHERE t3col1\n    ) FROM (SELECT 1 AS anoncol1)\n  )\n  SELECT t5col1, t5col1 FROM t5\n", "1 1")
	})
	t.Run("unionall-4.3 window derived cannot shadow join output", func(t *testing.T) {
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t33rExec(t, db,
			"CREATE TABLE t1_a(a INTEGER PRIMARY KEY, b TEXT); INSERT INTO t1_a VALUES(123, 't1_a');",
			"CREATE TABLE t1_b(c INTEGER PRIMARY KEY, d TEXT);",
			"CREATE VIEW t1 AS SELECT a, b FROM t1_a UNION ALL SELECT c, d FROM t1_b;")
		t33rQuery(t, db, "SELECT * FROM (SELECT * FROM t1), (SELECT count(a) OVER () AS g FROM t1)", "123 t1_a 1")
		t33rQuery(t, db, "SELECT * FROM t1, (SELECT group_concat(a) OVER (ORDER BY a) AS g FROM t1)", "123 t1_a 123")
	})
}
