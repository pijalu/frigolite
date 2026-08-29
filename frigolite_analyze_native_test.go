// Native Go test that exercises the ANALYZE / sqlite_stat1 engine contract
// independently from the tcl2go transpiler. Each block mirrors a TCL
// analyze.test case but executes via frigolite.Open / Exec / Query directly
// and asserts the actual returned row-set + error vs the expected TCL value.
//
// Run with: go test -run TestNativeAnalyze ./...
package frigolite

import (
	"strings"
	"testing"
)

func openAnalyze(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func doExec(t *testing.T, db *DB, sql string) error {
	t.Helper()
	r := db.Exec(sql)
	if r.Error != nil {
		return r.Error
	}
	return nil
}

func doQuery(t *testing.T, db *DB, sql string) []Row {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("Query %q: %v", sql, r.Error)
	}
	return r.Rows
}

func flattenAnalyzeRows(rows []Row) string {
	var parts []string
	for _, row := range rows {
		for _, v := range row {
			parts = append(parts, renderAnalyzeValue(v))
		}
	}
	if len(parts) == 0 {
		return "{}"
	}
	return strings.Join(parts, " ")
}

func renderAnalyzeValue(v interface{}) string {
	if v == nil {
		return "{}"
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return "{}"
		}
		// TCL renders a string cell containing whitespace as a braced list
		// element (e.g. "2 2" -> "{2 2}") so the flatten() output matches
		// the expected TCL literal "{2 2}" used in analyze.test.
		if strings.ContainsAny(x, " \t\n") {
			return "{" + x + "}"
		}
		return x
	case []byte:
		if len(x) == 0 {
			return "{}"
		}
		s := string(x)
		if strings.ContainsAny(s, " \t\n") {
			return "{" + s + "}"
		}
		return s
	case int64:
		return analyzeIntToString(x)
	case float64:
		return analyzeIntToString(int64(x))
	case int:
		return analyzeIntToString(int64(x))
	default:
		return ""
	}
}

func analyzeIntToString(x int64) string {
	if x == 0 {
		return "0"
	}
	neg := false
	if x < 0 {
		neg = true
		x = -x
	}
	var buf [20]byte
	i := len(buf)
	for x > 0 {
		i--
		buf[i] = byte('0' + x%10)
		x /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestNativeAnalyze_1(t *testing.T) {
	dir := t.TempDir()
	db := openAnalyze(t, dir+"/test.db")
	defer db.Close()

	if err := doExec(t, db, "ANALYZE no_such_table"); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("analyze-1.1: want no_such_table err, got %v", err)
	}
	rows := doQuery(t, db, "SELECT count(*) FROM sqlite_master WHERE name='sqlite_stat1'")
	if got := flattenAnalyzeRows(rows); got != "0" {
		t.Errorf("analyze-1.2: want [0], got [%s]", got)
	}
}

func TestNativeAnalyze_3(t *testing.T) {
	dir := t.TempDir()
	db := openAnalyze(t, dir+"/test.db")
	defer db.Close()

	must := func(s string, want string) {
		t.Helper()
		rows := doQuery(t, db, s)
		got := flattenAnalyzeRows(rows)
		if got != want {
			t.Errorf("\n  sql: %s\n  got:  [%s]\n  want: [%s]", s, got, want)
		}
	}

	if err := doExec(t, db, "CREATE TABLE t1(a,b);"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "CREATE INDEX t1i1 ON t1(a); CREATE INDEX t1i2 ON t1(b); CREATE INDEX t1i3 ON t1(a,b);"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "INSERT INTO t1 VALUES(1,2); INSERT INTO t1 VALUES(1,3);"); err != nil {
		t.Fatal(err)
	}
	must("ANALYZE main.t1; SELECT idx, stat FROM sqlite_stat1 ORDER BY idx;", "t1i1 {2 2} t1i2 {2 1} t1i3 {2 2 1}")
	if err := doExec(t, db, "INSERT INTO t1 VALUES(1,4); INSERT INTO t1 VALUES(1,5);"); err != nil {
		t.Fatal(err)
	}
	must("ANALYZE t1; SELECT idx, stat FROM sqlite_stat1 ORDER BY idx;", "t1i1 {4 4} t1i2 {4 1} t1i3 {4 4 1}")
	if err := doExec(t, db, "INSERT INTO t1 VALUES(2,5);"); err != nil {
		t.Fatal(err)
	}
	must("ANALYZE main; SELECT idx, stat FROM sqlite_stat1 ORDER BY idx;", "t1i1 {5 3} t1i2 {5 2} t1i3 {5 3 1}")
}

func TestNativeAnalyze_4_0(t *testing.T) {
	dir := t.TempDir()
	db := openAnalyze(t, dir+"/test.db")
	defer db.Close()

	if err := doExec(t, db, "CREATE TABLE t3(a,b,c);"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "CREATE TABLE t4(x,y,z);"); err != nil {
		t.Fatal(err)
	}
	// Mirror the upstream analyze.test data flow: 5 rows in t3 with
	// a={1,1,1,1,2}, b={2,3,4,5,5}. t4 inherits those values via
	// `INSERT INTO t4 SELECT a,b,c FROM t3`, so t4i1 sees 2 distinct x and
	// t4i2 sees 4 distinct y (analyze-4.0).
	if err := doExec(t, db, "INSERT INTO t3 VALUES(1,2,100); INSERT INTO t3 VALUES(1,3,101); INSERT INTO t3 VALUES(1,4,102); INSERT INTO t3 VALUES(1,5,103); INSERT INTO t3 VALUES(2,5,104);"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "INSERT INTO t4 SELECT a,b,c FROM t3;"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "CREATE INDEX t4i1 ON t4(x); CREATE INDEX t4i2 ON t4(y);"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "ANALYZE;"); err != nil {
		t.Fatal(err)
	}
	rows := doQuery(t, db, "SELECT idx, stat FROM sqlite_stat1 WHERE tbl='t4' ORDER BY idx;")
	gotStr := flattenAnalyzeRows(rows)
	want := "t4i1 {5 3} t4i2 {5 2}"
	if gotStr != want {
		t.Errorf("analyze-4.0 t4 stats: want [%s], got [%s]", want, gotStr)
	}
}

func TestNativeAnalyze_5_4(t *testing.T) {
	dir := t.TempDir()
	db := openAnalyze(t, dir+"/test.db")
	defer db.Close()

	if err := doExec(t, db, "CREATE TABLE t3(a,b,c,d);"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "CREATE INDEX t3i1 ON t3(a); CREATE INDEX t3i2 ON t3(a,b,c,d);"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "INSERT INTO t3 VALUES(1,2,3,4); INSERT INTO t3 VALUES(5,6,7,8); INSERT INTO t3 SELECT a+8,b+8,c+8,d+8 FROM t3; INSERT INTO t3 SELECT a+16,b+16,c+16,d+16 FROM t3; INSERT INTO t3 SELECT a+32,b+32,c+32,d+32 FROM t3; INSERT INTO t3 SELECT a+64,b+64,c+64,d+64 FROM t3;"); err != nil {
		t.Fatal(err)
	}
	if err := doExec(t, db, "ANALYZE;"); err != nil {
		t.Fatal(err)
	}
	rows := doQuery(t, db, "SELECT DISTINCT idx FROM sqlite_stat1 ORDER BY 1;")
	if got := flattenAnalyzeRows(rows); got != "t3i1 t3i2" {
		t.Errorf("analyze-5.0 idx: want [t3i1 t3i2], got [%s]", got)
	}
	if err := doExec(t, db, "DROP TABLE t3;"); err != nil {
		t.Fatal(err)
	}
	rows = doQuery(t, db, "SELECT DISTINCT tbl FROM sqlite_stat1 ORDER BY 1;")
	if got := flattenAnalyzeRows(rows); got != "{}" {
		t.Errorf("analyze-5.4 tbl: want [{}], got [%s]", got)
	}
}

func TestNativeAnalyze_6(t *testing.T) {
	dir := t.TempDir()
	db := openAnalyze(t, dir+"/test.db")
	defer db.Close()

	if err := doExec(t, db, "CREATE TABLE sqliteDemo(a); INSERT INTO sqliteDemo(a) VALUES(1),(2),(3),(4),(5); CREATE TABLE SQLiteDemo2(a INTEGER PRIMARY KEY AUTOINCREMENT); INSERT INTO SQLiteDemo2 SELECT * FROM sqliteDemo; CREATE TABLE t1(b); INSERT INTO t1(b) SELECT a FROM sqliteDemo; ANALYZE;"); err != nil {
		t.Fatal(err)
	}
	rows := doQuery(t, db, "SELECT tbl FROM sqlite_stat1 WHERE idx IS NULL ORDER BY tbl;")
	if got := flattenAnalyzeRows(rows); got != "SQLiteDemo2 sqliteDemo t1" {
		t.Errorf("analyze-6.1: want [SQLiteDemo2 sqliteDemo t1], got [%s]", got)
	}
}

func TestNativeAnalyze_3_10(t *testing.T) {
	dir := t.TempDir()
	db := openAnalyze(t, dir+"/test.db")
	defer db.Close()

	if err := doExec(t, db, `CREATE TABLE [silly " name](a, b, c);
CREATE INDEX 'foolish '' name' ON [silly " name](a, b);
CREATE INDEX 'another foolish '' name' ON [silly " name](c);
INSERT INTO [silly " name] VALUES(1, 2, 3);
INSERT INTO [silly " name] VALUES(4, 5, 6);
ANALYZE;`); err != nil {
		t.Fatal(err)
	}
	rows := doQuery(t, db, "SELECT idx, stat FROM sqlite_stat1 ORDER BY idx;")
	got := flattenAnalyzeRows(rows)
	want := "{another foolish ' name} {2 1} {foolish ' name} {2 1 1}"
	if got != want {
		t.Errorf("analyze-3.10: want [%s], got [%s]", want, got)
	}
}

// Row alias matching the engine's Query Result.Rows.
type Row = []interface{}