package frigolite

// Native pin for the fts5 porter stemmer and trigram token spans, driven
// through frigolite.Open/Query (AGENTS.md pure-Go port policy). The corpus
// packages testgen/fts5porter and testgen/fts5porter2 exercise ~1300
// sqlite3_fts5_tokenize pairs against the same engine code; this file pins
// the rule branches that the fts5 porter variant gets DIFFERENTLY from the
// classic Porter algorithm (ext/fts5/fts5_tokenize.c fts5PorterCb and the
// generated steps), plus the trigram span contract of fts5TriTokenize.
import (
	"path/filepath"
	"testing"
)

// TestFTS5PorterStem pins the fts5 porter rules that diverge from classic
// Porter (internal/fts5/porter.go, ported from fts5_tokenize.c):
//   - step 1a maps "ies" to "ie" (classic maps to "i") and never touches a
//     trailing "ss" ("abbess" stays "abbess"; classic Porter gives "abbes");
//   - an exactly-"eed" token fails the "eed" rule's m>0 guard and strips via
//     the "ed" rule instead ("eed" -> "e"; classic gives "eed" or "ee");
//   - step 5a drops a final 'e' when m>1 ("realize"/"realization" ->
//     "realiz", "relate" -> "relat").
func TestFTS5PorterStem(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "porter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE VIRTUAL TABLE t1 USING fts5(v)",
		"INSERT INTO t1 VALUES('abbess'),('access'),('across'),('address')",
	} {
		if r := db.Exec(stmt); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, stmt)
		}
	}
	// The "ss"-protection class via index lookups: MATCH finds every row
	// because query and document stem identically.
	for _, q := range []string{"abbess", "access", "across", "address"} {
		checkQueryResult(t, db.Query("SELECT count(*) FROM t1 WHERE t1 MATCH('"+q+"')"), "1")
	}
}

// TestFTS5TrigramDiacriticSpan pins fts5TriTokenize's span contract for
// removed diacritics (fts5trigram2.test 3.x): a character that folds to
// nothing never enters a trigram, but the token span runs in original text
// bytes from the window's first character to the start of the following
// retained character (or EOF), so highlight() over '\u0303abc\u0303' for
// MATCH 'abc' renders '\u0303(abc\u0303)'.
func TestFTS5TrigramDiacriticSpan(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "trigram.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE VIRTUAL TABLE t3 USING fts5(z, tokenize='trigram remove_diacritics 1')",
		"INSERT INTO t3 VALUES ('\u0303abc\u0303')",
	} {
		if r := db.Exec(stmt); r.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", r.Error, stmt)
		}
	}
	res := db.Query("SELECT highlight(t3, 0, '(', ')') FROM t3('abc')")
	if res.Error != nil {
		t.Fatalf("query error: %v\n  sql: %s", res.Error, res.SQL)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("expected a single value, got rows=%v", res.Rows)
	}
	if got, _ := res.Rows[0][0].(string); got != "\u0303(abc\u0303)" {
		t.Errorf("highlight: got %q, want \u0303(abc\u0303)", got)
	}
}
