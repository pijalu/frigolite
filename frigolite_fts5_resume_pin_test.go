package frigolite

// Native pins for the P6.FTS5-RESUME engine fixes (2026-09-15). Every
// contract below was validated against the sqlite3 CLI oracle 3.51.0 before
// being pinned; the fts5 testgen packages listed per test are the corpus
// counterparts flipped by the same fixes.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// fts5ResumeOpen opens an in-memory database for one pin test.
func fts5ResumeOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestFTS5ResumePinUTF16BlobRoundTrip pins the utf16 blob round trip: an
// odd-length blob passes through fts5 (and quote()) verbatim under a UTF-16
// database encoding — SQLite truncates an odd trailing byte only when a
// UTF-16 value is actually translated (sqlite3VdbeMemTranslate / sqlite3AtoF,
// ticket 9eda2697f5cc1aba), never when marshalling function arguments
// (fts5blob 1.x; the evalFuncArgs blanket truncation was removed).
func TestFTS5ResumePinUTF16BlobRoundTrip(t *testing.T) {
	db := fts5ResumeOpen(t)
	if res := db.Exec("PRAGMA encoding = 'utf16'"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(x, y)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("INSERT INTO t1(rowid, x, y) VALUES(1, 555, X'0000000041424320444546')"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("INSERT INTO t1(rowid, x, y) VALUES(2, 666, X'41424300444546')"); res.Error != nil {
		t.Fatal(res.Error)
	}
	q := db.Query("SELECT rowid, quote(x), quote(y) FROM t1")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	want := "1 555 X'0000000041424320444546' 2 666 X'41424300444546'"
	if got := fts5ResumeFlatten(q); got != want {
		t.Errorf("blob round trip: got [%s] want [%s]", got, want)
	}
}

// flattenRows joins one result column set the way the harness flatten does.
func fts5ResumeFlatten(res *Result) string {
	parts := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		for _, v := range row {
			parts = append(parts, fts5ResumeCell(v))
		}
	}
	return strings.Join(parts, " ")
}

func fts5ResumeCell(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "{}"
	case string:
		return x
	case int64:
		return fts5ResumeItoa(int(x))
	case []byte:
		return "X'" + fts5ResumeHex(x) + "'"
	}
	return "?"
}

func fts5ResumeItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func fts5ResumeHex(b []byte) string {
	const hexdigits = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0x0F])
	}
	return string(out)
}

// TestFTS5ResumePinRowidMustBeInt pins OP_MustBeInt on an explicit rowid:
// 4.0 becomes rowid 4, while 4.5 / 'xyz' / a blob fail with "datatype
// mismatch" on both ordinary and fts5 tables (fts5blob 4.1).
func TestFTS5ResumePinRowidMustBeInt(t *testing.T) {
	db := fts5ResumeOpen(t)
	checkExecOK(t, db.Exec("CREATE TABLE t(a)"))
	checkExecOK(t, db.Exec("INSERT INTO t(rowid, a) VALUES(4.0, 'w')"))
	q := db.Query("SELECT rowid FROM t")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if len(q.Rows) != 1 || !fts5ResumeIntEq(q.Rows[0][0], 4) {
		t.Errorf("integral REAL rowid: got %v want 4", q.Rows)
	}
	checkExecError(t, db.Exec("INSERT INTO t(rowid, a) VALUES(4.5, 'x')"), "datatype mismatch")
	checkExecError(t, db.Exec("INSERT INTO t(rowid, a) VALUES('xyz', 'x')"), "datatype mismatch")
	checkExecError(t, db.Exec("INSERT INTO t(rowid, a) VALUES(X'001122', 'x')"), "datatype mismatch")

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE x1 USING fts5(x)"))
	checkExecError(t, db.Exec("INSERT INTO x1(rowid, x) VALUES(4.5, 'abcd')"), "datatype mismatch")
	checkExecError(t, db.Exec("INSERT INTO x1(rowid, x) VALUES('xyz', 'abcd')"), "datatype mismatch")
	checkExecError(t, db.Exec("INSERT INTO x1(rowid, x) VALUES(X'001122', 'abcd')"), "datatype mismatch")
}

func fts5ResumeIntEq(v interface{}, want int64) bool {
	n, ok := v.(int64)
	return ok && n == want
}

// TestFTS5ResumePinFTS5LocaleRequiresLocale1 pins the fts5_locale() contract:
// writing an fts5_locale() value into a table without locale=1 fails with
// 'fts5_locale() requires locale=1' (fts5blob 3.x, fts5_main.c:2005).
func TestFTS5ResumePinFTS5LocaleRequiresLocale1(t *testing.T) {
	db := fts5ResumeOpen(t)
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE x1 USING fts5(a, b)"))
	checkExecError(t, db.Exec("INSERT INTO x1(rowid, a, b) VALUES(113, 'hello world', fts5_locale('en_AU', 'abc'))"),
		"fts5_locale() requires locale=1")
	// xUpdate runs per matched row, so seed one row before the UPDATE.
	checkExecOK(t, db.Exec("INSERT INTO x1(rowid, a, b) VALUES(1, 'x', 'y')"))
	checkExecError(t, db.Exec("UPDATE x1 SET a = fts5_locale('en_AU', 'abc')"), "fts5_locale() requires locale=1")
}

// TestFTS5ResumePinBeforeTriggerNewRowid pins the BEFORE INSERT trigger's
// new.rowid contract: the EXPLICIT rowid is visible (the value is bound
// before the trigger programs run), an auto-assigned rowid reads -1
// (oracle 3.51.0: explicit → 1/2; auto → -1; IPK-explicit 7/7; IPK-auto
// -1/-1) (fts5connect 2.x/3.x).
func TestFTS5ResumePinBeforeTriggerNewRowid(t *testing.T) {
	db := fts5ResumeOpen(t)
	for _, s := range []string{
		"CREATE VIRTUAL TABLE ft USING fts5(a, b)",
		"CREATE TABLE t3(a, b)",
		"CREATE TABLE log(m TEXT)",
		"CREATE TRIGGER t3_ai BEFORE INSERT ON t3 BEGIN " +
			"INSERT INTO ft(rowid, a, b) VALUES(new.rowid, new.a, new.b); " +
			"INSERT INTO log VALUES(CAST(new.rowid AS TEXT)); END",
		"INSERT INTO t3(rowid, a, b) VALUES(1, 'one', 'two')",
		"INSERT INTO t3(rowid, a, b) VALUES(2, 'three', 'four')",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
	q := db.Query("SELECT rowid FROM ft ORDER BY rowid")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if len(q.Rows) != 2 || !fts5ResumeIntEq(q.Rows[0][0], 1) || !fts5ResumeIntEq(q.Rows[1][0], 2) {
		t.Errorf("trigger-inserted ft rowids: got %v want [[1] [2]]", q.Rows)
	}
	q = db.Query("SELECT m FROM log ORDER BY rowid")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if len(q.Rows) != 2 || q.Rows[0][0] != "1" || q.Rows[1][0] != "2" {
		t.Errorf("new.rowid in BEFORE trigger: got %v want [[1] [2]]", q.Rows)
	}
}

// TestFTS5ResumePinSecureDeleteVersionBump pins the one-time format upgrade:
// after a DELETE on a secure-delete table, %_config carries version=5
// (fts5_index.c fts5DoSecureDeleteEntry's REPLACE INTO %_config) while a
// table never secure-deleted keeps version 4 (fts5secure2 1.2/1.4).
func TestFTS5ResumePinSecureDeleteVersionBump(t *testing.T) {
	db := fts5ResumeOpen(t)
	for _, s := range []string{
		"CREATE VIRTUAL TABLE ft USING fts5(col)",
		"INSERT INTO ft VALUES('data for the table')",
		"INSERT INTO ft VALUES('more of the same')",
		"INSERT INTO ft(ft, rank) VALUES('secure-delete', 1)",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
	q := db.Query("SELECT * FROM ft_config")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if !strings.Contains(fts5ResumeConfigText(q), "version 4") {
		t.Errorf("pre-delete config: got %v, want version 4 row", q.Rows)
	}
	if res := db.Exec("DELETE FROM ft WHERE rowid=2"); res.Error != nil {
		t.Fatal(res.Error)
	}
	q = db.Query("SELECT * FROM ft_config")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if !strings.Contains(fts5ResumeConfigText(q), "version 5") {
		t.Errorf("post-delete config: got %v, want version 5 row", q.Rows)
	}
}

func fts5ResumeConfigText(res *Result) string {
	var b strings.Builder
	for _, row := range res.Rows {
		for _, v := range row {
			if s, ok := v.(string); ok {
				b.WriteString(s)
				b.WriteByte(' ')
			}
			if n, ok := v.(int64); ok {
				b.WriteString(fts5ResumeItoa(int(n)))
				b.WriteByte(' ')
			}
		}
	}
	return b.String()
}

// TestFTS5ResumePinSpecialDeleteSemantics pins fts5SpecialDelete: a 'delete'
// with an absent rowid is a silent no-op (no row-existence check in
// fts5StorageDeleteFromIndex), while a 'delete' whose supplied values exceed
// a column's total token count reports the corruption error
// (fts5secure4 1.1/1.11).
func TestFTS5ResumePinSpecialDeleteSemantics(t *testing.T) {
	db := fts5ResumeOpen(t)
	for _, s := range []string{
		"CREATE VIRTUAL TABLE t1 USING fts5(a, b, content=x1)",
		"CREATE TABLE x1(rowid INTEGER PRIMARY KEY, a, b)",
		"INSERT INTO x1 VALUES (1, 'hello world', 'today xyz'), (2, 'not the day', 'crunch crumble and chomp'), (3, 'one', 'two')",
		"INSERT INTO t1(t1) VALUES('rebuild')",
		"INSERT INTO t1(t1, rank) VALUES('secure-delete', 1)",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
	// Absent rowid + phantom tokens: a silent no-op (column totals stay
	// positive), and the index survives.
	if res := db.Exec("INSERT INTO t1(t1, rowid, a, b) VALUES('delete', 4, 'nosuchtoken', '')"); res.Error != nil {
		t.Errorf("absent-rowid 'delete' must be a no-op, got %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t1(t1) VALUES('integrity-check')"); res.Error != nil {
		t.Errorf("integrity-check after no-op delete: %v", res.Error)
	}
	q := db.Query("SELECT count(*) FROM t1")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if len(q.Rows) != 1 || !fts5ResumeIntEq(q.Rows[0][0], 3) {
		t.Errorf("no-op delete must keep 3 docs: got %v", q.Rows)
	}

	// Contentless table whose only doc holds zero tokens: deleting a token
	// that was never there drives the column total negative → malformed.
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE w1 USING fts5(ww, content=\"\")"))
	checkExecOK(t, db.Exec("INSERT INTO w1(rowid, ww) VALUES(123, '')"))
	checkExecError(t, db.Exec("INSERT INTO w1(w1, rowid, ww) VALUES('delete', 123, 'xyz')"),
		"database disk image is malformed")
}

// TestFTS5ResumePinNEAREmptyPhraseMerge pins sqlite3Fts5ParseNearset's
// incremental empty-phrase rule: NEAR("" c, 5) ≡ NEAR(c, 5) — row 1 — while
// a lone NEAR("") stays an EOF node (fts5fuzz1 2.3/2.5/2.6).
func TestFTS5ResumePinNEAREmptyPhraseMerge(t *testing.T) {
	db := fts5ResumeOpen(t)
	for _, s := range []string{
		"CREATE VIRTUAL TABLE f1 USING fts5(a, b)",
		"INSERT INTO f1 VALUES('a b', 'c d')",
		"INSERT INTO f1 VALUES('e f', 'a b')",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
	q := db.Query("SELECT a, b FROM f1('NEAR(\"\" c, 5)')")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if len(q.Rows) != 1 || q.Rows[0][0] != "a b" {
		t.Errorf("NEAR(\"\" c, 5): got %v want [[a b] [c d] row]", q.Rows)
	}
	q = db.Query("SELECT rowid FROM f1('NEAR(\"\")')")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if len(q.Rows) != 0 {
		t.Errorf("NEAR(\"\") must match nothing: got %v", q.Rows)
	}
}

// TestFTS5ResumePinFormFeedModuleArg pins the two-level whitespace split:
// the SQL lexer skips a form feed (sqlite3Isspace), but the CREATE VIRTUAL
// TABLE argument it lands in keeps it verbatim, and fts5's config parser —
// whose fts5_iswhitespace matches only the space character — reports
// 'parse error in "a b"' (fts5fuzz1 1.1).
func TestFTS5ResumePinFormFeedModuleArg(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/fuzz1.db"
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE VIRTUAL TABLE f1 USING fts5(a\fb)"); res.Error == nil {
		t.Errorf("form-feed module arg must fail")
	} else if !strings.Contains(res.Error.Error(), "parse error in") {
		t.Errorf("expected 'parse error in', got %q", res.Error.Error())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// TestFTS5ResumePinDeferredTokenizerError pins the reopen contract of a
// stored tokenize= spec naming an unresolvable tokenizer: reads and writes
// of the table report the constructor error, while fts5vocab still reads the
// index (fts5tokenizer 10.2/10.3/10.5/10.6/10.9).
func TestFTS5ResumePinDeferredTokenizerError(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tok10.db"
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE x1 USING fts5(x, tokenize=unicode61)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	checkExecOK(t, db.Exec("INSERT INTO x1 VALUES('a b c'), ('d e f')"))
	checkExecOK(t, db.Exec("PRAGMA writable_schema = 1"))
	checkExecOK(t, db.Exec("UPDATE sqlite_schema SET sql = 'CREATE VIRTUAL TABLE x1 USING fts5(x, tokenize=\"nosuch error\");' WHERE name = 'x1'"))
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	checkExecError(t, db2.Exec("INSERT INTO x1 VALUES('g h i')"), "no such tokenizer: nosuch")
	q := db2.Query("SELECT * FROM x1('abc')")
	if q.Error == nil || !strings.Contains(q.Error.Error(), "no such tokenizer: nosuch") {
		t.Errorf("TVF read must report the deferred tokenizer error, got %v", q.Error)
	}
}

// TestFTS5ResumePinLeftJoinUnusableMatch pins the LEFT JOIN unusable-MATCH
// rule: a MATCH constraint inside a LEFT JOIN's ON clause that references the
// outer fts5 table is unusable at every scan position (an outer-join ON term
// must not filter the outer table, and it binds no inner constraint), so
// fts5's xBestIndex returns SQLITE_CONSTRAINT and the join solver fails with
// "no query solution" (fts5_main.c fts5BestIndexMethod, where.c
// whereLoopAddVtab; fts5leftjoin 2.2/3.1). The unary-plus form and WHERE-side
// MATCH stay plain predicates / usable constraints.
func TestFTS5ResumePinLeftJoinUnusableMatch(t *testing.T) {
	db := fts5ResumeOpen(t)
	if res := db.Exec("CREATE VIRTUAL TABLE t0 USING fts5(a, b)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("INSERT INTO t0(a, b) VALUES(1, 0)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("CREATE TABLE t1(x)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	for _, q := range []string{
		"SELECT * FROM t0 LEFT JOIN t1 ON t0.b MATCH '1'",
		"SELECT * FROM t0 LEFT JOIN (SELECT 0 AS col_0) ON ((t0.a MATCH '1' AND CASE WHEN t0.b THEN CAST(t0.a AS INTEGER) ELSE 1 END))",
	} {
		if res := db.Exec(q); res.Error == nil || !strings.Contains(res.Error.Error(), "no query solution") {
			t.Errorf("expected 'no query solution', got: %v\n  sql: %s", res.Error, q)
		}
	}
	// +b MATCH is not a vtab constraint: plain predicate, LEFT JOIN runs.
	if r := db.Query("SELECT * FROM t0 LEFT JOIN t1 ON +b MATCH '1'"); r.Error != nil {
		t.Errorf("+b MATCH must not error: %v", r.Error)
	} else if got := fts5PinFlattenNull(r.Rows); got != "1 0 {}" {
		t.Errorf("+b MATCH rows: got [%s] want [1 0 {}]", got)
	}
	// WHERE-side MATCH on the fts5 table keeps working across a LEFT JOIN.
	if r := db.Query("SELECT t0.a FROM t0 LEFT JOIN t1 ON t1.x = t0.a WHERE t0.b MATCH '0'"); r.Error != nil {
		t.Errorf("WHERE MATCH must not error: %v", r.Error)
	} else if got := fts5PinFlattenNull(r.Rows); got != "1" {
		t.Errorf("WHERE MATCH rows: got [%s] want [1]", got)
	}
}

// fts5PinFlattenNull renders rows TCL-style: NULL becomes "{}".
func fts5PinFlattenNull(rows [][]interface{}) string {
	parts := make([]string, 0, len(rows)*2)
	for _, row := range rows {
		for _, v := range row {
			if v == nil {
				parts = append(parts, "{}")
				continue
			}
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}
	return strings.Join(parts, " ")
}
