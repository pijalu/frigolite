package frigolite

import (
	"strconv"
	"strings"
	"testing"
)

// FULL-SUITE-DRIFT.T30-tkt pins. Each test reproduces an engine-visible
// failure surfaced by the regenerated testgen corpus (W5-TKT tranche).
// Every expected value was verified against the /usr/bin/sqlite3 3.54
// oracle before the corresponding fix landed.

func w5Open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir() + "/w5.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func w5Render(rows [][]interface{}) string {
	var sb strings.Builder
	for i, row := range rows {
		if i > 0 {
			sb.WriteString(";")
		}
		for j, v := range row {
			if j > 0 {
				sb.WriteString("|")
			}
			switch x := v.(type) {
			case nil:
				sb.WriteString("{}")
			case string:
				sb.WriteString(x)
			case []byte:
				sb.WriteString(string(x))
			case int64:
				sb.WriteString(w5itoa(x))
			case float64:
				sb.WriteString(rtoa(x))
			default:
				sb.WriteString("?")
			}
		}
	}
	return sb.String()
}

func w5itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func rtoa(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func w5Rows(t *testing.T, db *DB, q string) [][]interface{} {
	t.Helper()
	r := db.Query(q)
	if r.Error != nil {
		t.Fatalf("%s: %v", q, r.Error)
	}
	return r.Rows
}

// collate8-1.1: ORDER BY on a column declared COLLATE nocase.
func TestW5Collate8OrderByColumnCollation(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE t1(a TEXT COLLATE nocase);
		INSERT INTO t1 VALUES('aaa');INSERT INTO t1 VALUES('BBB');
		INSERT INTO t1 VALUES('ccc');INSERT INTO t1 VALUES('DDD');`); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := w5Render(w5Rows(t, db, "SELECT a FROM t1 ORDER BY a"))
	want := "aaa;BBB;ccc;DDD"
	if got != want {
		t.Errorf("ORDER BY a (nocase col): got %s want %s", got, want)
	}
	got = w5Render(w5Rows(t, db, "SELECT a FROM t1 ORDER BY +a"))
	if got != want {
		t.Errorf("ORDER BY +a (nocase col): got %s want %s", got, want)
	}
	got = w5Render(w5Rows(t, db, "SELECT rowid FROM t1 WHERE a<'ccc' ORDER BY 1"))
	if got != "1;2" {
		t.Errorf("WHERE a<'ccc' (nocase col): got %s want 1|2", got)
	}
	got = w5Render(w5Rows(t, db, "SELECT rowid FROM t1 WHERE a<'ccc' COLLATE binary ORDER BY 1"))
	if got != "1;2;4" {
		t.Errorf("WHERE a<'ccc' COLLATE binary: got %s want 1|2|4", got)
	}
}

// minmax3-4.x: aggregate min/max with explicit COLLATE in argument.
func TestW5MinMaxCollatedArg(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE t4(x);
		INSERT INTO t4 VALUES('abc');INSERT INTO t4 VALUES('BCD');`); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Render(w5Rows(t, db, "SELECT max(x) FROM t4")); got != "abc" {
		t.Errorf("max(x): got %s want abc", got)
	}
	if got := w5Render(w5Rows(t, db, "SELECT max(x COLLATE nocase) FROM t4")); got != "BCD" {
		t.Errorf("max(x COLLATE nocase): got %s want BCD", got)
	}
	if got := w5Render(w5Rows(t, db, "SELECT max(x), max(x COLLATE nocase) FROM t4")); got != "abc|BCD" {
		t.Errorf("max(x),max(x COLLATE nocase): got %s want abc|BCD", got)
	}
	if got := w5Render(w5Rows(t, db, "SELECT min(x), min(x COLLATE nocase) FROM t4")); got != "BCD|abc" {
		t.Errorf("min pair: got %s want BCD|abc", got)
	}
}

// tkt2822-6.x: compound SELECT ORDER BY output aliases / ordinals.
func TestW5Tkt2822CompoundOrderBy(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE t6a(p,q);INSERT INTO t6a VALUES(1,8);
		INSERT INTO t6a VALUES(9,2);
		CREATE TABLE t6b(x,y);INSERT INTO t6b VALUES(1,7);
		INSERT INTO t6b VALUES(7,2);`); r.Error != nil {
		t.Fatal(r.Error)
	}
	qs := []struct {
		q, want string
	}{
		{"SELECT p,q FROM t6a UNION ALL SELECT x,y FROM t6b ORDER BY 1,2", "1|7;1|8;7|2;9|2"},
		{"SELECT p PX,q QX FROM t6a UNION ALL SELECT x XX,y YX FROM t6b ORDER BY PX,YX", "1|7;1|8;7|2;9|2"},
		{"SELECT p PX,q QX FROM t6a UNION ALL SELECT x XX,y YX FROM t6b ORDER BY XX,QX", "1|7;1|8;7|2;9|2"},
		{"SELECT p PX,q QX FROM t6a UNION ALL SELECT x XX,y YX FROM t6b ORDER BY QX,XX", "7|2;9|2;1|7;1|8"},
		{"SELECT p PX,q QX FROM t6a UNION ALL SELECT x XX,y YX FROM t6b ORDER BY t6b.x,QX", "1|7;1|8;7|2;9|2"},
		{"SELECT p PX,q QX FROM t6a UNION ALL SELECT x XX,y YX FROM t6b ORDER BY t6a.q,XX", "7|2;9|2;1|7;1|8"},
	}
	for _, tc := range qs {
		got := w5Render(w5Rows(t, db, tc.q))
		if got != tc.want {
			t.Errorf("%s: got %s want %s", tc.q, got, tc.want)
		}
	}
}

// tkt3992-2.x: UPDATE after ALTER TABLE ADD COLUMN with DEFAULT.
func TestW5Tkt3992AddColumnDefault(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE t1(a,b);INSERT INTO t1 VALUES(1,2);
		ALTER TABLE t1 ADD COLUMN c DEFAULT 3;`); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Render(w5Rows(t, db, "SELECT * FROM t1")); got != "1|2|3" {
		t.Errorf("select after add col: got %s want 1;2;3", got)
	}
	if r := db.Exec("UPDATE t1 SET a='one'"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Render(w5Rows(t, db, "SELECT * FROM t1")); got != "one|2|3" {
		t.Errorf("select after update: got %s want one|2|3", got)
	}
}

// tkt-9a8b09f8e6-2.2: IN comparison applies LHS column affinity to comparand.
func TestW5Tkt9a8bInAffinity(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE t1(x TEXT);INSERT INTO t1 VALUES('1');`); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Render(w5Rows(t, db, "SELECT x FROM t1 WHERE x IN (1.0)")); got != "" {
		t.Errorf("x TEXT IN (1.0): got %q want empty (no rows)", got)
	}
	if got := w5Render(w5Rows(t, db, "SELECT x FROM t1 WHERE x IN (1)")); got != "1" {
		t.Errorf("x TEXT IN (1): got %s want 1", got)
	}
}

// tkt-38cb5df375: compound SELECT with LIMIT.
func TestW5Tkt38cbCompoundLimit(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE t1(a);
		INSERT INTO t1 VALUES(1);INSERT INTO t1 VALUES(2);
		INSERT INTO t1 SELECT a+2 FROM t1;INSERT INTO t1 SELECT a+4 FROM t1;`); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := w5Render(w5Rows(t, db, "SELECT * FROM (SELECT * FROM t1 ORDER BY a) UNION ALL SELECT 9 FROM (SELECT a FROM t1) LIMIT 1;"))
	if got != "1" {
		t.Errorf("compound LIMIT 1: got %s want 1", got)
	}
	got = w5Render(w5Rows(t, db, "SELECT * FROM (SELECT * FROM t1 ORDER BY a) UNION ALL SELECT 9 FROM (SELECT a FROM t1) LIMIT 3;"))
	if got != "1;2;3" {
		t.Errorf("compound LIMIT 3: got %s want 1;2;3", got)
	}
}

// tkt-54844eea3f-1.2: correlated derived table with ORDER BY ... OFFSET.
func TestW5Tkt54844CorrelatedDerivedOffset(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE t4(a,b,c);
		INSERT INTO t4 VALUES('a',1,'one');INSERT INTO t4 VALUES('a',2,'two');
		INSERT INTO t4 VALUES('b',1,'three');INSERT INTO t4 VALUES('b',2,'four');`); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := w5Render(w5Rows(t, db, "SELECT ( SELECT c FROM (SELECT * FROM t4 WHERE a=out.a ORDER BY b LIMIT 10 OFFSET 1) WHERE b=out.b ) FROM t4 AS out;"))
	if got != "{};two;{};four" {
		t.Errorf("correlated derived offset: got %s want {}|two|{}|four", got)
	}
}

// misc5-5.4: numeric literal with leading dot plus exponent.
func TestW5Misc5LeadingDotExponent(t *testing.T) {
	db := w5Open(t)
	for _, tc := range [][2]string{
		{"SELECT .1", "0.1"},
		{"SELECT 2.", "2"},
		{"SELECT 3.e0", "3"},
		{"SELECT .4e+1", "4"},
	} {
		rows := w5Rows(t, db, tc[0])
		got := w5Render(rows)
		if got != tc[1] {
			t.Errorf("%s: got %s want %s", tc[0], got, tc[1])
		}
	}
}

// func-4.4.2: abs() of a text value is 0.0 (REAL), not 0 (INTEGER).
func TestW5FuncAbsTextReal(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE tbl1(t1);INSERT INTO tbl1 VALUES('abc');`); r.Error != nil {
		t.Fatal(r.Error)
	}
	rows := w5Rows(t, db, "SELECT abs(t1) FROM tbl1")
	if len(rows) != 1 {
		t.Fatal("no rows")
	}
	f, ok := rows[0][0].(float64)
	if !ok {
		t.Errorf("abs('abc') type %T want float64", rows[0][0])
	} else if f != 0.0 {
		t.Errorf("abs('abc') = %v want 0.0", f)
	}
}

// func-9.5: randomblob with a negative size yields a 1-byte blob.
func TestW5FuncRandomBlobNegative(t *testing.T) {
	db := w5Open(t)
	rows := w5Rows(t, db, "SELECT length(randomblob(-5))")
	if got := w5Render(rows); got != "1" {
		t.Errorf("length(randomblob(-5)): got %s want 1", got)
	}
}

// func-22.2x: trim with NULL separator returns NULL.
func TestW5FuncTrimNull(t *testing.T) {
	db := w5Open(t)
	rows := w5Rows(t, db, "SELECT typeof(trim('hello',NULL))")
	if got := w5Render(rows); got != "null" {
		t.Errorf("typeof(trim('hello',NULL)): got %s want null", got)
	}
}

// func-24.5: group_concat with NULL separator = empty separator.
func TestW5FuncGroupConcatNullSep(t *testing.T) {
	db := w5Open(t)
	if r := db.Exec(`CREATE TABLE tbl1(t1);INSERT INTO tbl1 VALUES('this');
		INSERT INTO tbl1 VALUES('program');INSERT INTO tbl1 VALUES('is');
		INSERT INTO tbl1 VALUES('free');INSERT INTO tbl1 VALUES('software');`); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := w5Render(w5Rows(t, db, "SELECT group_concat(t1,NULL) FROM tbl1"))
	if got != "thisprogramisfreesoftware" {
		t.Errorf("group_concat(t1,NULL): got %s", got)
	}
}

// Multi-statement Query batch: ORDER BY must still apply to the final SELECT.
func TestW5MultiStatementOrderBy(t *testing.T) {
	db := w5Open(t)
	r := db.Query("CREATE TABLE t1(a TEXT COLLATE nocase);\nINSERT INTO t1 VALUES('aaa');\nINSERT INTO t1 VALUES('BBB');\nINSERT INTO t1 VALUES('ccc');\nINSERT INTO t1 VALUES('DDD');\nSELECT a FROM t1 ORDER BY a;\n")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Render(r.Rows); got != "aaa;BBB;ccc;DDD" {
		t.Errorf("multi-stmt ORDER BY: got %s", got)
	}
}
