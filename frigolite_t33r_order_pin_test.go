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
