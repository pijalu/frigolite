package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestIndexexpr2EnginePins locks the engine contracts behind indexexpr2's
// three remaining leaves: an expression index drives a WHERE range seek, an
// expression index satisfies a same-collation ORDER BY, and a correlated
// aggregate stays in the inner context when its argument also references an
// inner column (resolve.c's sqlite3ReferencesSrcList rule).
func TestIndexexpr2EnginePins(t *testing.T) {
	open := func(t *testing.T, setup ...string) *DB {
		t.Helper()
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		for _, stmt := range setup {
			if res := db.Exec(stmt); res.Error != nil {
				t.Fatalf("setup %q: %v", stmt, res.Error)
			}
		}
		return db
	}
	flat := func(res *Result) string {
		t.Helper()
		if res.Error != nil {
			t.Fatal(res.Error)
		}
		var vals []string
		for _, row := range res.Rows {
			for _, v := range row {
				vals = append(vals, fmt.Sprint(v))
			}
		}
		return "[" + strings.Join(vals, " ") + "]"
	}

	t.Run("expression index WHERE seek", func(t *testing.T) {
		db := open(t,
			`CREATE TABLE t1(a, b)`,
			`INSERT INTO t1 VALUES(1,'one'),(2,'two'),(3,'three')`,
			`CREATE INDEX i1 ON t1(b || 'x')`,
		)
		plan := flat(db.Query(`EXPLAIN QUERY PLAN SELECT a, b FROM t1 WHERE (b || 'x') > 'onex'`))
		if !strings.Contains(plan, "SEARCH t1 USING INDEX i1") {
			t.Errorf("plan = %s, want the i1 range seek", plan)
		}
		rows := flat(db.Query(`SELECT a, b FROM t1 WHERE (b || 'x') > 'onex'`))
		if rows != "[3 three 2 two]" {
			t.Errorf("rows = %s, want [3 three 2 two] (index key order)", rows)
		}
	})

	t.Run("expression index ORDER BY", func(t *testing.T) {
		db := open(t,
			`CREATE TABLE t4(a, b)`,
			`INSERT INTO t4 VALUES('.abc2',2),('.ABC1',1),('.abc4',4),('.ABC3',3)`,
			`CREATE INDEX i4 ON t4(Substr(a,-2) COLLATE nocase)`,
		)
		plan := flat(db.Query(`EXPLAIN QUERY PLAN SELECT * FROM t4 ORDER BY Substr(a,-2) COLLATE nocase`))
		if !strings.Contains(plan, "SCAN t4 USING INDEX i4") {
			t.Errorf("nocase plan = %s, want the i4 index walk", plan)
		}
		plan = flat(db.Query(`EXPLAIN QUERY PLAN SELECT * FROM t4 ORDER BY Substr(a,-2) COLLATE binary`))
		if strings.Contains(plan, "USING INDEX i4") {
			t.Errorf("binary plan = %s, want no i4 walk under a different collation", plan)
		}
	})

	t.Run("correlated aggregate resolution", func(t *testing.T) {
		db := open(t,
			`CREATE TABLE t3(a INT, b AS (-a))`,
			`CREATE INDEX t3x ON t3(b, a)`,
			`INSERT INTO t3(a) VALUES(44)`,
		)
		legal := `SELECT * FROM t3 AS a0 WHERE (SELECT sum(-a0.a=b) FROM t3 GROUP BY b) GROUP BY b`
		if got := flat(db.Query(legal)); got != "[44 -44]" {
			t.Errorf("mixed inner/outer aggregate rows = %s, want [44 -44]", got)
		}
		if got := flat(db.Query(`SELECT * FROM t3 AS a0 WHERE (SELECT sum(1) FROM t3 GROUP BY b) GROUP BY b`)); got != "[44 -44]" {
			t.Errorf("constant aggregate rows = %s, want [44 -44]", got)
		}
		outerOnly := db.Query(`SELECT * FROM t3 AS a0 WHERE (SELECT sum(-a0.a) FROM t3 GROUP BY b) GROUP BY b`)
		if outerOnly.Error == nil || !strings.Contains(outerOnly.Error.Error(), "misuse of aggregate: sum()") {
			t.Errorf("outer-only aggregate error = %v, want misuse of aggregate: sum()", outerOnly.Error)
		}
	})
}
