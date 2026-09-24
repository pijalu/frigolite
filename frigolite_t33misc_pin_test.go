package frigolite

import (
	"os"
	"strings"
	"testing"
)

// FULL-SUITE-DRIFT.T33-misc pins. Each test reproduces an engine-visible
// failure surfaced by the regenerated testgen/misc* packages. Every expected
// value was verified against the sqlite3 oracle before the fix landed
// (3.51 reference build with ext/misc/eval.c statically linked for the eval
// probes; /usr/bin/sqlite3 3.54 for the rest).

func t33Open(t *testing.T, name string) *DB {
	t.Helper()
	path := t.TempDir() + "/" + name
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// t33LitAssert checks one literal query yields a REAL whose value matches.
func t33LitAssert(t *testing.T, db *DB, sql string, want float64) {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		t.Fatalf("%s: bad shape %v", sql, r.Rows)
	}
	f, ok := r.Rows[0][0].(float64)
	if !ok {
		t.Fatalf("%s: got %T (%v), want real", sql, r.Rows[0][0], r.Rows[0][0])
	}
	if f != want {
		t.Fatalf("%s: got %v want %v", sql, f, want)
	}
}

// TestT33MiscLeadingDotLiterals pins tokenize.c's leading-dot and trailing-dot
// float mantissas (misc5-5.1..5.4): ".5"/"2."/"3.e0"/".4e+1" are one number
// token each (the dot stays in the token — the pre-fix lexer dropped it, so
// SELECT .1 yielded the integer 1 and SELECT .4e+1 the real 40.0).
func TestT33MiscLeadingDotLiterals(t *testing.T) {
	db := t33Open(t, "t33lit.db")
	t33LitAssert(t, db, "SELECT .1", 0.1)
	t33LitAssert(t, db, "SELECT 2.", 2.0)
	t33LitAssert(t, db, "SELECT 3.e0", 3.0)
	t33LitAssert(t, db, "SELECT .4e+1", 4.0)
	t33LitAssert(t, db, "SELECT .5e2", 50.0)
}

// TestT33MiscLimitSubqueryPrepareError pins resolve.c's prepare-time LIMIT
// resolution (misc5-6.1): a LIMIT expression containing a subquery surfaces
// the subquery's schema errors ("no such table: blah") instead of degrading
// to an unlimited scan.
func TestT33MiscLimitSubqueryPrepareError(t *testing.T) {
	db := t33Open(t, "t33limit.db")
	if r := db.Exec("CREATE TABLE t(a)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Exec("SELECT * FROM sqlite_master UNION ALL SELECT * FROM sqlite_master LIMIT (SELECT count(*) FROM blah)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such table: blah") {
		t.Fatalf("want 'no such table: blah', got %v", r.Error)
	}
}

// TestT33MiscEvalDeleteAllDuringScan pins misc8-1.6: eval('DELETE FROM t1')
// runs while the outer SELECT scans t1. Oracle (3.51 + eval.c): statement
// succeeds with exactly 3 rows; row 3 is (7, 'bam', NULL) because SQLite
// reads c AFTER the delete invalidated the cursor. Frigolite decodes all
// columns before evaluating the output expressions, so c still carries the
// pre-delete value 9 — a read-timing divergence, asserted here as a prefix.
func TestT33MiscEvalDeleteAllDuringScan(t *testing.T) {
	db := t33Open(t, "t33evaldelete.db")
	if r := db.Exec("CREATE TABLE t1(a,b,c); INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,null,9)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Exec("SELECT a, coalesce(b, eval('DELETE FROM t1; SELECT ''bam''')), c FROM t1 ORDER BY rowid")
	if r.Error != nil {
		t.Fatalf("statement must succeed, got: %v", r.Error)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("got %d rows (%v), want 3", len(r.Rows), r.Rows)
	}
	if t33renderRow(r.Rows[0]) != "1|2|3" || t33renderRow(r.Rows[1]) != "4|5|6" {
		t.Fatalf("rows 1-2 wrong: %v", r.Rows)
	}
	got := t33renderRow(r.Rows[2])
	if !strings.HasPrefix(got, "7|bam|") {
		t.Fatalf("row 3 wrong: %q (oracle: 7|bam|{} — c read after invalidation)", got)
	}
}

// TestT33MiscEvalDeleteEachCurrentRow pins the misc2-7.4 scan contract with
// the oracle shape: eval deletes the CURRENT row of the outer scan for every
// row (t1 has 5 rows, eval('DELETE FROM t1 WHERE a='||a)). Oracle: ALL 5 rows
// are still emitted and the table is empty afterwards (each row's own delete
// invalidates the scan cursor; the restore lands on the next-larger rowid
// which becomes the Next result).
func TestT33MiscEvalDeleteEachCurrentRow(t *testing.T) {
	db := t33Open(t, "t33eachrow.db")
	if r := db.Exec("CREATE TABLE t1(a,b); INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40),(5,50)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query("SELECT a, eval('DELETE FROM t1 WHERE a=' || a) FROM t1 ORDER BY rowid")
	if r.Error != nil {
		t.Fatalf("statement must succeed, got: %v", r.Error)
	}
	if len(r.Rows) != 5 {
		t.Fatalf("got %d rows (%v), want 5", len(r.Rows), r.Rows)
	}
	for i, row := range r.Rows {
		if t33renderRow(row[:1]) != t33itoa(int64(i+1)) {
			t.Fatalf("row %d wrong: %v", i+1, row)
		}
	}
	after := db.Query("SELECT a FROM t1")
	if after.Error != nil {
		t.Fatal(after.Error)
	}
	if len(after.Rows) != 0 {
		t.Fatalf("table must be empty after, got %v", after.Rows)
	}
}

// TestT33MiscEvalDeleteLaterRow pins the mid-scan delete of a NOT-YET-REACHED
// row (oracle: 3.51 + eval.c): rows 1..5, eval deletes a=2 while the scan is
// on row 1. Output is rows 1,3,4,5 (row 2 gone; the cursor advance after row
// 1 skips straight past the deleted 2) and the after-state keeps 1,3,4,5.
func TestT33MiscEvalDeleteLaterRow(t *testing.T) {
	db := t33Open(t, "t33laterrow.db")
	if r := db.Exec("CREATE TABLE t1(a,b); INSERT INTO t1 VALUES(1,NULL),(2,NULL),(3,NULL),(4,NULL),(5,NULL)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query("SELECT a, eval('DELETE FROM t1 WHERE a=2') FROM t1 ORDER BY rowid")
	if r.Error != nil {
		t.Fatalf("statement must succeed, got: %v", r.Error)
	}
	var gotA []string
	for _, row := range r.Rows {
		gotA = append(gotA, t33renderRow(row[:1]))
	}
	if got, want := strings.Join(gotA, ";"), "1;3;4;5"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	after := db.Query("SELECT a FROM t1")
	if after.Error != nil {
		t.Fatal(after.Error)
	}
	if got, want := t33renderRows(after.Rows), "1;3;4;5"; got != want {
		t.Fatalf("after: got %q want %q", got, want)
	}
}

// TestT33MiscScanOwnTableDeleteLoop pins misc2-7.2/7.3's engine contract:
// rows materialized from SELECT rowid may be deleted one by one while the
// Go-side iteration continues (SQLite permits modifying a scanned table since
// 2006), leaving the table empty.
func TestT33MiscScanOwnTableDeleteLoop(t *testing.T) {
	db := t33Open(t, "t33loop.db")
	if r := db.Exec("CREATE TABLE t1(x); INSERT INTO t1 VALUES(1),(2),(3)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	q := db.Query("SELECT rowid FROM t1")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	for _, row := range q.Rows {
		if r := db.Exec("DELETE FROM t1 WHERE rowid=" + t33itoa(row[0].(int64))); r.Error != nil {
			t.Fatalf("delete during iteration: %v", r.Error)
		}
	}
	after := db.Query("SELECT * FROM t1")
	if after.Error != nil {
		t.Fatal(after.Error)
	}
	if len(after.Rows) != 0 {
		t.Fatalf("t1 not empty: %v", after.Rows)
	}
}

func t33itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
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

func t33renderRow(row []interface{}) string {
	var sb strings.Builder
	for j, v := range row {
		if j > 0 {
			sb.WriteString("|")
		}
		switch x := v.(type) {
		case nil:
			sb.WriteString("{}")
		case int64:
			sb.WriteString(t33itoa(x))
		case float64:
			sb.WriteString(strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(fmtFloatT33(x), "0"), "."), ""))
		case string:
			sb.WriteString(x)
		default:
			sb.WriteString("?")
		}
	}
	return sb.String()
}

func fmtFloatT33(f float64) string {
	// SQLite renders integral reals as "N.0"; only integral values appear in
	// these pins.
	if f == float64(int64(f)) {
		return t33itoa(int64(f)) + ".0"
	}
	return "?"
}

func t33renderRows(rows [][]interface{}) string {
	var sb strings.Builder
	for i, row := range rows {
		if i > 0 {
			sb.WriteString(";")
		}
		sb.WriteString(t33renderRow(row))
	}
	return sb.String()
}

var _ = os.Remove // reserved for file-backed probes
