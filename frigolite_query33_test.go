package frigolite

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// openProbeDB opens a fresh in-file database for probe tests.
func openProbeDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestProbeSelectHCompoundOrderBy is the engine-first probe for selectH-2.1:
// a compound subquery with ORDER BY b must apply the sort (b: c61 < c62 →
// the second arm's row first).
func TestProbeSelectHCompoundOrderBy(t *testing.T) {
	db := openProbeDB(t)
	if err := db.Exec(`CREATE TABLE t1(c15, c16, c61, c62);
		INSERT INTO t1 VALUES(15, 16, 61, 62);`).Error; err != nil {
		t.Fatal(err)
	}
	res := db.Query(`SELECT a FROM (
		SELECT 1 AS cnt, c15 AS a, *, c62 AS b FROM t1
		UNION ALL
		SELECT 1 AS cnt, c16 AS a, *, c61 AS b FROM t1
		ORDER BY b )`)
	if res.Error != nil {
		t.Fatalf("query error: %v", res.Error)
	}
	if got, want := flatRows(res), "16 15"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestProbeSelectHCounter counts invocations of a counter UDF via a Go
// variable (engine-first probe for selectH 1.3/2.2/3.2/3.5 — the
// omit-unused-subquery-column optimization must keep the UDF un-invoked when
// its output column is never referenced by the outer query).
func TestProbeSelectHCounter(t *testing.T) {
	db := openProbeDB(t)
	if err := db.Exec(`CREATE TABLE t1(c0, c44, c60);
		INSERT INTO t1 VALUES(0, 44, 60);`).Error; err != nil {
		t.Fatal(err)
	}
	cnt := 0
	db.RegisterFunction("counter", func(args []interface{}) (interface{}, error) {
		amt := int64(1)
		if len(args) > 0 {
			if n, ok := args[0].(int64); ok {
				amt = n
			}
		}
		cnt += int(amt)
		return int64(cnt), nil
	}, 1, 1)
	res := db.Query(`SELECT DISTINCT c44 FROM (
		SELECT c0 AS a, *, counter(1) FROM t1
		UNION ALL
		SELECT c0 AS a, *, counter(1) FROM t1
	  ) WHERE c60=60`)
	if res.Error != nil {
		t.Fatalf("query error: %v", res.Error)
	}
	if got, want := flatRows(res), "44"; got != want {
		t.Errorf("rows: got %q want %q", got, want)
	}
	if cnt != 0 {
		t.Errorf("counter invoked %d times; SQLite's omit-unused-subquery-column keeps it at 0", cnt)
	}
}

// TestProbeSelectHStarAliasColumn is the engine-first probe for selectH-3.6:
// SELECT x over a compound view whose arms are "c16 AS a, *, <expr> AS x"
// (68 output columns: alias, 66-star columns, trailing expression) must
// return the expression values, not source columns.
func TestProbeSelectHStarAliasColumn(t *testing.T) {
	db := openProbeDB(t)
	cols := make([]string, 66)
	vals := make([]string, 66)
	for i := 0; i < 66; i++ {
		cols[i] = fmt.Sprintf("c%d", i)
		vals[i] = strconv.Itoa(i)
	}
	ddl := fmt.Sprintf("CREATE TABLE t1(%s);\nINSERT INTO t1 VALUES(%s);\nCREATE INDEX t1c60 ON t1(c60);",
		strings.Join(cols, ", "), strings.Join(vals, ", "))
	if err := db.Exec(ddl).Error; err != nil {
		t.Fatal(err)
	}
	cnt := 0
	db.RegisterFunction("counter", func(args []interface{}) (interface{}, error) {
		cnt++
		return int64(cnt), nil
	}, 1, 1)
	// selectH-3.1: the view is created (and first used) inside one
	// multi-statement Exec.
	if err := db.Exec(`CREATE VIEW v1 AS
		  SELECT c16 AS a, *, counter(1) AS x FROM t1
		  UNION ALL
		  SELECT c17 AS a, *, counter(1) AS x FROM t1
		  UNION ALL
		  SELECT c18 AS a, *, counter(1) AS x FROM t1
		  UNION ALL
		  SELECT c19 AS a, *, counter(1) AS x FROM t1;
		  SELECT count(*) FROM v1 WHERE c60=60;`).Error; err != nil {
		t.Fatal(err)
	}
	// selectH-3.6: the x column IS selected, so counter runs once per arm
	// row and x carries 1 2 3 4.
	res := db.Query(`SELECT x FROM v1 WHERE c60=60`)
	if res.Error != nil {
		t.Fatalf("query x where error: %v", res.Error)
	}
	if got, want := flatRows(res), "1 2 3 4"; got != want {
		t.Errorf("x where c60=60: got %q want %q", got, want)
	}
}

// TestProbeHavingNondeter is the engine-first probe for having.test 4.2/4.3:
// a non-deterministic HAVING term is evaluated ONCE PER GROUP (never moved
// into WHERE), while a non-deterministic WHERE term is evaluated per row.
func TestProbeHavingNondeter(t *testing.T) {
	db := openProbeDB(t)
	if err := db.Exec(`CREATE TABLE t3(a, b);
		INSERT INTO t3 VALUES(1, 1);
		INSERT INTO t3 VALUES(1, 2);
		INSERT INTO t3 VALUES(1, 3);
		INSERT INTO t3 VALUES(2, 1);
		INSERT INTO t3 VALUES(2, 2);
		INSERT INTO t3 VALUES(2, 3);`).Error; err != nil {
		t.Fatal(err)
	}
	mkNondeter := func() func(args []interface{}) (interface{}, error) {
		ret := 0
		return func(args []interface{}) (interface{}, error) {
			ret++
			return int64(ret % 2), nil
		}
	}

	// 4.2: HAVING evaluated once per group → groups a=1 (call→1, truthy),
	// a=2 (call→0, falsy) → {1 6}.
	db.RegisterFunction("nondeter", mkNondeter(), 0, -1)
	res := db.Query(`SELECT a, sum(b) FROM t3 GROUP BY a HAVING nondeter(a)`)
	if res.Error != nil {
		t.Fatalf("4.2 query error: %v", res.Error)
	}
	if got := flatRows(res); got != "1 6" {
		t.Errorf("4.2: got %q want %q", got, "1 6")
	}

	// 4.3: WHERE evaluated per row → calls 1,0,1,0,1,0 → rows (1,1),(1,3),
	// (2,2) survive → groups {1 4 2 2}.
	db2 := openProbeDB(t)
	if err := db2.Exec(`CREATE TABLE t3(a, b);
		INSERT INTO t3 VALUES(1, 1);
		INSERT INTO t3 VALUES(1, 2);
		INSERT INTO t3 VALUES(1, 3);
		INSERT INTO t3 VALUES(2, 1);
		INSERT INTO t3 VALUES(2, 2);
		INSERT INTO t3 VALUES(2, 3);`).Error; err != nil {
		t.Fatal(err)
	}
	db2.RegisterFunction("nondeter", mkNondeter(), 0, -1)
	res2 := db2.Query(`SELECT a, sum(b) FROM t3 WHERE nondeter(a) GROUP BY a`)
	if res2.Error != nil {
		t.Fatalf("4.3 query error: %v", res2.Error)
	}
	if got := flatRows(res2); got != "1 4 2 2" {
		t.Errorf("4.3: got %q want %q", got, "1 4 2 2")
	}
}

func flatRows(res *Result) string {
	out := ""
	for _, row := range res.Rows {
		for _, v := range row {
			if out != "" {
				out += " "
			}
			out += renderProbeVal(v)
		}
	}
	return out
}

func renderProbeVal(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "{}"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// TestProbeWhere6LeftJoinOrder is the engine-first probe for where6-3.1:
// ORDER BY 1, 2, 3 over a self-join whose result columns collide on the
// name "x" (positions 1 and 2). Key 2 must still distinguish the rows.
func TestProbeWhere6LeftJoinOrder(t *testing.T) {
	db := openProbeDB(t)
	if err := db.Exec(`CREATE TABLE t4(x UNIQUE);
		INSERT INTO t4 VALUES('abc');
		INSERT INTO t4 VALUES('def');
		INSERT INTO t4 VALUES('ghi');
		CREATE TABLE t5(a, b, c, PRIMARY KEY(a,b));
		INSERT INTO t5 VALUES('abc','def',123);
		INSERT INTO t5 VALUES('def','ghi',456);`).Error; err != nil {
		t.Fatal(err)
	}
	res := db.Query(`SELECT t4a.x, t4b.x, t5.c, t6.v
		FROM t4 AS t4a
			INNER JOIN t4 AS t4b
			LEFT JOIN t5 ON t5.a=t4a.x AND t5.b=t4b.x
			LEFT JOIN (SELECT 1 AS v) AS t6 ON t4a.x=t4b.x
		ORDER BY 1, 2, 3`)
	if res.Error != nil {
		t.Fatalf("query error: %v", res.Error)
	}
	want := "abc abc {} 1 abc def 123 {} abc ghi {} {} def abc {} {} def def {} 1 def ghi 456 {} ghi abc {} {} ghi def {} {} ghi ghi {} 1"
	if got := flatRows(res); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestProbeWindow8OrderByWindowCol is the engine-first probe for window8
// 1.8.8: the final ORDER BY 1, 2, 3 must break (a,b) ties by the window
// value in column 3.
func TestProbeWindow8OrderByWindowCol(t *testing.T) {
	db := openProbeDB(t)
	if err := db.Exec(`CREATE TABLE t3(a TEXT, b TEXT, c INTEGER);
		INSERT INTO t3 VALUES
		  ('AA', 'aa', 934), ('AA', 'bb', 309), ('AA', 'aa', 911),
		  ('AA', 'bb', 572), ('AA', 'aa', 239), ('AA', 'bb', 870),
		  ('AA', 'bb', 627), ('AA', 'aa', 223);`).Error; err != nil {
		t.Fatal(err)
	}
	res := db.Query(`SELECT a, b,
		sum(c) OVER (ORDER BY a  GROUPS BETWEEN 3 PRECEDING AND 0 PRECEDING EXCLUDE CURRENT ROW),
		sum(c) OVER (ORDER BY a  GROUPS BETWEEN 3 PRECEDING AND 0 PRECEDING ),
		sum(c) OVER (ORDER BY a,b  GROUPS BETWEEN 3 PRECEDING AND 0 PRECEDING EXCLUDE CURRENT ROW),
		sum(c) OVER (ORDER BY a,b  GROUPS BETWEEN 3 PRECEDING AND 0 PRECEDING )
	  FROM t3 ORDER BY 1, 2, 3`)
	if res.Error != nil {
		t.Fatalf("query error: %v", res.Error)
	}
	want := "AA aa 3751 4685 1373 2307 AA aa 3774 4685 1396 2307 AA aa 4446 4685 2068 2307 AA aa 4462 4685 2084 2307 " +
		"AA bb 3815 4685 3815 4685 AA bb 4058 4685 4058 4685 AA bb 4113 4685 4113 4685 AA bb 4376 4685 4376 4685"
	if got := flatRows(res); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
