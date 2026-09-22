package frigolite

// Pins for FULL-SUITE-DRIFT.T30-fts3: the FTS3/4 latent engine gaps exposed
// by the regenerated testgen corpus, each fixed C-faithfully and verified
// against a default-build (no SQLITE_ENABLE_FTS3_PARENTHESIS) sqlite oracle
// built from the porting reference tree. Apple's /usr/bin/sqlite3 has
// ENABLE_FTS3_PARENTHESIS and is NOT ground truth for fts3 MATCH syntax.

import (
	"fmt"
	"strings"
	"testing"
)

// w5Query runs one query and flattens every cell TCL-style.
func w5Query(t *testing.T, db interface {
	Query(sql string) *Result
}, sql string) string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Errorf("query error: %v\n  sql: %s", r.Error, sql)
		return ""
	}
	var parts []string
	for _, row := range r.Rows {
		for _, v := range row {
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}
	return strings.Join(parts, " ")
}

// TestW5FTS3LegacyQueryPins pins the legacy MATCH query syntax
// (fts3_expr.c fts3ExprParse with sqlite3_fts3_enable_parentheses==0):
// OR binds tighter than the implicit AND, AND/NOT are plain terms,
// parentheses are tokenizer delimiters, and a unary '-' builds the
// pNotBranch NOT chain.
func TestW5FTS3LegacyQueryPins(t *testing.T) {
	db, err := Open(t.TempDir() + "/pin1.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(content); INSERT INTO t1(content)" +
		" VALUES('one'),('two'),('one two'),('three'),('one three'),('two three'),('one two three');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	cases := map[string]string{
		// 'a b OR c' = a AND (b OR c): the OR group binds tighter than the
		// implicit AND (fts3aa-4.4; legacy opPrecedence OR=2 < AND=3).
		"SELECT rowid FROM t1 WHERE content MATCH 'one two OR three'": "3 5 7",
		// "(three OR two) AND one": OR tighter than implicit AND on the left.
		"SELECT rowid FROM t1 WHERE content MATCH 'three OR two one'": "3 5 7",
		// Keywords AND/NOT are not operators in legacy mode (they are only
		// recognized with SQLITE_ENABLE_FTS3_PARENTHESIS); lowercase is a
		// plain term either way.
		"SELECT rowid FROM t1 WHERE content MATCH 'one OR two'": "1 2 3 5 6 7",
	}
	for q, w := range cases {
		if got := w5Query(t, db, q); got != w {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", q, got, w)
		}
	}

	// '-' qualifier: "abc-def" is abc AND NOT def (fts3_expr.c getNextToken
	// sets ParseContext.isNot for the '-' directly before a token; oracle:
	// MATCH 'abc-def' on doc 'abc-def' matches nothing).
	db2, err := Open(t.TempDir() + "/pin2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if r := db2.Exec("CREATE VIRTUAL TABLE t USING fts3(c); INSERT INTO t VALUES('abc-def');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Query(t, db2, "SELECT rowid FROM t WHERE c MATCH 'abc-def'"); got != "" {
		t.Errorf("MATCH 'abc-def' on 'abc-def': got [%s], want empty (abc AND NOT def)", got)
	}
	// All-negated queries are a syntax error (fts3ExprParse: pRet==nil with
	// a non-empty pNotBranch).
	if r := db2.Exec("SELECT rowid FROM t WHERE c MATCH '-abc'"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "malformed MATCH expression") {
		t.Errorf("MATCH '-abc': got %v, want malformed MATCH expression", r.Error)
	}
}

// TestW5FTS3NearPin pins fts3PoslistPhraseMerge's STRICT directional pairing:
// the same token position never pairs with itself, so 'X NEAR X' on a
// document with a single X matches nothing (fts3near-1.14).
func TestW5FTS3NearPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/pin3.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(content);" +
		" INSERT INTO t1 VALUES('one three four five');" +
		" INSERT INTO t1 VALUES('two three four five');" +
		" INSERT INTO t1 VALUES('one two three four five');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Query(t, db, "SELECT docid FROM t1 WHERE content MATCH 'four NEAR four'"); got != "" {
		t.Errorf("'four NEAR four': got [%s], want empty (no second instance)", got)
	}
	// Directional pairing with the phrase-length slack: A NEAR/0 B only
	// pairs adjacent tokens (fts3near-2.1 on 'A X B C D A B').
	db2, err := Open(t.TempDir() + "/pin4.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if r := db2.Exec("CREATE VIRTUAL TABLE t1 USING fts3(content); INSERT INTO t1 VALUES('A X B C D A B');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := w5Query(t, db2, "SELECT offsets(t1) FROM t1 WHERE content MATCH 'A NEAR/0 B'")
	if want := "0 0 10 1 0 1 12 1"; got != want {
		t.Errorf("'A NEAR/0 B' offsets\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// TestW5FTS3OffsetsColumnPin pins the SQL-side column restriction on the aux
// functions: `subject MATCH 'gas x'` scopes every unprefixed term to the
// subject column, so offsets() reports only subject hits (fts3.c
// fts3FilterMethod iDefaultCol; fts3ac-2.4).
func TestW5FTS3OffsetsColumnPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/pin5.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE VIRTUAL TABLE email USING fts3(subject, body);" +
		" INSERT INTO email VALUES('gas reminder here', 'gas elsewhere');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := w5Query(t, db, "SELECT rowid, offsets(email) FROM email WHERE subject MATCH 'gas reminder'")
	if !strings.Contains(got, "0 0 0 3") || strings.Contains(got, "1 0 0 3") {
		t.Errorf("column-scoped offsets\n  got:  [%s]\n  want: subject-column (0 x) entries only", got)
	}
}

// TestW5FTS3PorterCopyStemmerPin pins fts3_porter.c's copy_stemmer fallback:
// long no-digit tokens keep first+last 10 bytes; digit tokens keep first+last
// 3 (fts3ad-1.3..1.6).
func TestW5FTS3PorterCopyStemmerPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/pin6.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(content, tokenize porter);" +
		" INSERT INTO t1(rowid, content) VALUES(2, 'abcdefghijklmnopqrstuvwyxz');" +
		" INSERT INTO t1(rowid, content) VALUES(3, 'The value is 123456789');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Query(t, db, "SELECT rowid FROM t1 WHERE t1 MATCH 'abcdefghijqrstuvwyxz'"); got != "2" {
		t.Errorf("26-char token vs first10+last10 query: got [%s], want 2", got)
	}
	if got := w5Query(t, db, "SELECT rowid FROM t1 WHERE t1 MATCH '123789'"); got != "3" {
		t.Errorf("digit token vs first3+last3 query: got [%s], want 3", got)
	}
}

// TestW5FTS3DeleteAllPin pins fts3DeleteByRowid's isEmpty branch: the delete
// that empties the table wipes the index (fts3DeleteAll) instead of writing
// delete-marker segments, so after DELETE all + re-insert exactly ONE level-0
// segdir remains (fts3d-1.segments).
func TestW5FTS3DeleteAllPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/pin7.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(c);" +
		" INSERT INTO t1(docid,c) VALUES(1,'This is a test');" +
		" INSERT INTO t1(docid,c) VALUES(2,'That was a test');" +
		" INSERT INTO t1(docid,c) VALUES(3,'This is a test');" +
		" DELETE FROM t1 WHERE 1=1;" +
		" INSERT INTO t1(docid,c) VALUES(1,'This is a test');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := w5Query(t, db, "SELECT level, idx FROM t1_segdir ORDER BY level, idx"); got != "0 0" {
		t.Errorf("segdir after delete-all + re-insert: got [%s], want [0 0]", got)
	}
}

// TestW5FTS3CorruptPin pins the incrmerge-path corruption checks: a segdir
// root of X'FFFFFFFFFFFFFFFF' fails MATCH (nodeReader suffix overflow), and
// merge=1 over an output-level segdir row with a NULL/zero-length root fails
// with "database disk image is malformed" (fts3_write.c fts3IncrmergeWriter:
// aRoot==0 → nRoot ? NOMEM : FTS_CORRUPT_VTAB).
func TestW5FTS3CorruptPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/pin8.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetDefensive(false)
	if r := db.Exec("CREATE VIRTUAL TABLE t1 USING fts3;" +
		" BEGIN; INSERT INTO t1 VALUES('hello'); INSERT INTO t1 VALUES('world'); COMMIT;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("UPDATE t1_segdir SET root = X'FFFFFFFFFFFFFFFF'"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("SELECT rowid FROM t1 WHERE t1 MATCH 'world'"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "database disk image is malformed") {
		t.Errorf("FF root MATCH: got %v, want database disk image is malformed", r.Error)
	}

	db2, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.SetDefensive(false)
	r := db2.Exec("CREATE VIRTUAL TABLE f using fts3(a,b);\nCREATE TABLE f_stat(id INTEGER PRIMARY KEY, value BLOB);\nINSERT INTO f_segdir VALUES (2000, 0,0,0, '16', '');\nINSERT INTO f_segdir VALUES (1999, 0,0,0, '0 18',\n                             x'000131030102000103323334050101010200');\nINSERT INTO f_segments (blockid) values (16);\nINSERT INTO f_segments values (0, x'');\nINSERT INTO f_stat VALUES (1,x'cf0f01');\nINSERT INTO f(f) VALUES ('merge=1');")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "database disk image is malformed") {
		t.Errorf("merge=1 over empty-root output row: got %v, want database disk image is malformed", r.Error)
	}
}

// TestW5FTS3abEngineDataPin proves the ENGINE satisfies the fts3ab contract
// when fed the TCL source's real data: the generated test's setup is broken
// emitter-side (tcl2go dropped the [set $lang] indirection, storing the
// literal column-name strings), so the package fails before the engine is
// exercised. Word j of each language's column lands in row i iff bit j of i
// is set (k restarts at 1 per language).
func TestW5FTS3abEngineDataPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/pin9.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE VIRTUAL TABLE t1 USING fts3(english,spanish,german);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	langs := [][]string{
		{"one", "two", "three", "four", "five"},
		{"un", "dos", "tres", "cuatro", "cinco"},
		{"eine", "zwei", "drei", "vier", "funf"},
	}
	for i := 1; i <= 31; i++ {
		vals := make([]string, 3)
		for l, words := range langs {
			k := 1
			var keep []string
			for j := 0; j < 5; j++ {
				if k&i != 0 {
					keep = append(keep, words[j])
				}
				k += k
			}
			vals[l] = "'" + strings.Join(keep, " ") + "'"
		}
		if r := db.Exec(fmt.Sprintf("INSERT INTO t1(english,spanish,german) VALUES(%s)", strings.Join(vals, ","))); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	for _, c := range [][2]string{
		{"SELECT rowid FROM t1 WHERE english MATCH 'one'", "1 3 5 7 9 11 13 15 17 19 21 23 25 27 29 31"},
		{"SELECT rowid FROM t1 WHERE spanish MATCH 'one'", ""},
		{"SELECT rowid FROM t1 WHERE t1 MATCH 'one dos drei'", "7 15 23 31"},
		{"SELECT english, spanish, german FROM t1 WHERE rowid=1", "one un eine"},
	} {
		if got := w5Query(t, db, c[0]); got != c[1] {
			t.Errorf("%s\n  got:  [%s]\n  want: [%s]", c[0], got, c[1])
		}
	}
}
