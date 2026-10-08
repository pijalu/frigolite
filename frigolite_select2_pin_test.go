package frigolite_test

// Native pin for the select2.test engine-visible contract (FULL-SUITE-DRIFT
// T26-select / R13-L1). The JSON fixture testdata/select2.json lost the TCL
// file's setup steps during conversion — the two 30000-row INSERT loops, the
// `catch {execsql {DROP TABLE tbl2}}` between them, and the COMMITs are TCL
// control flow the converter cannot unroll — so tbl2 stays empty there and
// select2-2.0.2/2.1/2.2/3.1/3.2b..e/4.7 cannot match. The same contract is
// covered by the TCL-transpiled package (testgen/select2, which passes) and
// pinned natively here, with every expected value oracle-verified against
// sqlite3 3.54.0.

import (
	"testing"

	"github.com/pijalu/frigolite"
)

// select2Tbl2Fill loads tbl2 with the TCL loop's 30000 rows
// (`for {set i 1} {$i<=30000} {incr i} { INSERT INTO tbl2 VALUES($i,$i*2,$i*3) }`)
// in one statement; the recursive CTE is oracle-equivalent to the loop.
const select2Tbl2Fill = `WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<30000)
	INSERT INTO tbl2 SELECT i, i*2, i*3 FROM s`

// TestSelect2LargeTableIndexScan pins select2-2.0.1/2.0.2/2.1/2.2/3.1/3.2a..e:
// a 30000-row table answers an unindexed and an indexed equality on the
// second column with the same rows, in the same (index-key) order.
func TestSelect2LargeTableIndexScan(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 2.0.1: create + fill + commit, then the TCL file's bare
	// `catch {execsql {DROP TABLE tbl2}}`, then 2.0.2 recreating it.
	if r := db.Exec("CREATE TABLE tbl2(f1 int, f2 int, f3 int); BEGIN;"); r.Error != nil {
		t.Fatalf("create tbl2: %v", r.Error)
	}
	if r := db.Exec(select2Tbl2Fill); r.Error != nil {
		t.Fatalf("fill tbl2: %v", r.Error)
	}
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("commit: %v", r.Error)
	}
	if r := db.Exec("DROP TABLE tbl2"); r.Error != nil {
		t.Fatalf("drop tbl2: %v", r.Error)
	}
	if r := db.Exec("CREATE TABLE tbl2(f1 int, f2 int, f3 int); BEGIN;"); r.Error != nil {
		t.Fatalf("recreate tbl2: %v", r.Error)
	}
	if r := db.Exec(select2Tbl2Fill); r.Error != nil {
		t.Fatalf("refill tbl2: %v", r.Error)
	}
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("commit 2: %v", r.Error)
	}

	scalar := func(sql string) interface{} {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("query %.60q: %v", sql, r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Fatalf("query %.60q: rows=%v", sql, r.Rows)
		}
		return r.Rows[0][0]
	}
	rows := func(sql string) [][]interface{} {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("query %.60q: %v", sql, r.Error)
		}
		return r.Rows
	}

	// 2.1/2.2 (no index yet).
	if got := scalar("SELECT count(*) FROM tbl2"); got != int64(30000) {
		t.Errorf("count(*)=%v, want 30000", got)
	}
	if got := scalar("SELECT count(*) FROM tbl2 WHERE f2>1000"); got != int64(29500) {
		t.Errorf("count(*) WHERE f2>1000 = %v, want 29500", got)
	}
	// 3.1: the constant-first orientation of the equality.
	if got := rows("SELECT f1 FROM tbl2 WHERE 1000=f2"); len(got) != 1 || got[0][0] != int64(500) {
		t.Errorf("unindexed 1000=f2 = %v, want [[500]]", got)
	}
	// 3.2a..e: the same two orientations plus the star form, indexed.
	if r := db.Exec("CREATE INDEX idx1 ON tbl2(f2)"); r.Error != nil {
		t.Fatalf("create idx1: %v", r.Error)
	}
	for _, sql := range []string{
		"SELECT f1 FROM tbl2 WHERE 1000=f2",
		"SELECT f1 FROM tbl2 WHERE f2=1000",
	} {
		if got := rows(sql); len(got) != 1 || got[0][0] != int64(500) {
			t.Errorf("indexed %.40q = %v, want [[500]]", sql, got)
		}
	}
	for _, sql := range []string{
		"SELECT * FROM tbl2 WHERE 1000=f2",
		"SELECT * FROM tbl2 WHERE f2=1000",
	} {
		got := rows(sql)
		if len(got) != 1 || len(got[0]) != 3 ||
			got[0][0] != int64(500) || got[0][1] != int64(1000) || got[0][2] != int64(1500) {
			t.Errorf("indexed %.40q = %v, want [[500 1000 1500]]", sql, got)
		}
	}
	if r := db.Exec("DROP INDEX idx1"); r.Error != nil {
		t.Fatalf("drop idx1: %v", r.Error)
	}
}

// TestSelect2CaseJoinPredicate pins select2-4.7: a CASE expression used as a
// WHERE predicate over a cross join (oracle: 1 4 1 0 3 2 3 0).
func TestSelect2CaseJoinPredicate(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := `CREATE TABLE aa(a); CREATE TABLE bb(b);
		INSERT INTO aa VALUES(1); INSERT INTO aa VALUES(3);
		INSERT INTO bb VALUES(2); INSERT INTO bb VALUES(4);`
	if r := db.Exec(setup); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	r := db.Query("SELECT * FROM aa, bb WHERE CASE WHEN a=b-1 THEN 1 END")
	if r.Error != nil {
		t.Fatalf("query: %v", r.Error)
	}
	want := [][2]int64{{1, 2}, {3, 4}}
	if len(r.Rows) != len(want) {
		t.Fatalf("rows=%v, want %v", r.Rows, want)
	}
	for i, w := range want {
		if r.Rows[i][0] != w[0] || r.Rows[i][1] != w[1] {
			t.Errorf("row %d = %v, want %v", i, r.Rows[i], w)
		}
	}
}
