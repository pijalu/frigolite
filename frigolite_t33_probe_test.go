package frigolite

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	_ = os.Remove
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
