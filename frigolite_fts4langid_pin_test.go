package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// fts4langid pin tests: the engine-visible contract of the FTS4
// languageid=<col> option (SQLite test/fts4langid.test), driving
// frigolite.Open/Exec/Query directly. The transpiled suite lives in
// testgen/fts4langid; these native assertions pin the passing state of the
// section-2/4/5 behaviors (hidden langid column, per-language MATCH
// filtering, per-language flush segments).

// langidQuery runs one query and returns the flattened result as a string
// (the tcl2go flatten convention: values separated by single spaces).
func langidQuery(t *testing.T, db *DB, q string) string {
	t.Helper()
	res := db.Query(q)
	if res.Error != nil {
		t.Fatalf("query %q: %v", q, res.Error)
	}
	var parts []string
	for _, row := range res.Rows {
		for _, v := range row {
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}
	return strings.Join(parts, " ")
}

// TestFTS4LangidHiddenColumnAndFilter covers fts4langid 2.x/4.x: the langid
// hidden column defaults to 0, INSERT can address it, and MATCH results are
// filtered by the languageid constraint (each language sees only its rows).
func TestFTS4LangidHiddenColumnAndFilter(t *testing.T) {
	db, err := Open(t.TempDir() + "/langid.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t2 USING fts4(languageid=lid)"))
	checkExecOK(t, db.Exec("INSERT INTO t2 VALUES('I belong to language 0!')"))

	// 5.3.2: with every row at a non-zero language, an unconstrained MATCH
	// sees only the language-0 row.
	for i := 0; i < 20; i++ {
		checkExecOK(t, db.Exec(fmt.Sprintf(
			"INSERT INTO t2(content, lid) VALUES('I (row %d) belong to language N!', %d)", i, 1<<30)))
	}
	if got := langidQuery(t, db, "SELECT docid FROM t2 WHERE t2 MATCH 'belong'"); got != "1" {
		t.Fatalf("unconstrained MATCH: got %q, want \"1\"", got)
	}
	// 5.3.3: the big-language rows are visible only through lid=<langid>.
	want := "2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21"
	if got := langidQuery(t, db, fmt.Sprintf("SELECT docid FROM t2 WHERE t2 MATCH 'belong' AND lid=%d", 1<<30)); got != want {
		t.Fatalf("langid=%d MATCH: got %q, want %q", 1<<30, got, want)
	}

	// 4.x: per-language content with case-transform tokenizers — each
	// language only matches its own rows (the engine's default tokenizer is
	// language-unaware, so all rows are mutually visible per language).
	checkExecOK(t, db.Exec("DELETE FROM t2"))
	checkExecOK(t, db.Exec("INSERT INTO t2(docid, content, lid) VALUES(10, 'quick brown fox', 1)"))
	checkExecOK(t, db.Exec("INSERT INTO t2(docid, content, lid) VALUES(11, 'quick brown fox', 2)"))
	if got := langidQuery(t, db, "SELECT docid FROM t2 WHERE t2 MATCH 'quick' AND lid=1"); got != "10" {
		t.Fatalf("lid=1 MATCH: got %q, want \"10\"", got)
	}
	if got := langidQuery(t, db, "SELECT docid FROM t2 WHERE t2 MATCH 'quick' AND lid=2"); got != "11" {
		t.Fatalf("lid=2 MATCH: got %q, want \"11\"", got)
	}
	if got := langidQuery(t, db, "SELECT docid FROM t2 WHERE t2 MATCH 'quick' AND lid=3"); got != "" {
		t.Fatalf("lid=3 MATCH: got %q, want \"\"", got)
	}
}

// TestFTS4LangidPerLanguageSegments covers fts4langid 5.1.1: a languageid
// table flushes ONE segment PER LANGUAGE, each at its base absolute level
// (iLangid*1024 — getAbsoluteLevel), so 4 languages yield levels 0, 1024,
// 2048 and 1<<40.
func TestFTS4LangidPerLanguageSegments(t *testing.T) {
	db, err := Open(t.TempDir() + "/langidseg.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t5 USING fts4(languageid=lid)"))
	for _, lang := range []string{"0", "1", "2", fmt.Sprint(1 << 30)} {
		checkExecOK(t, db.Exec(fmt.Sprintf(
			"INSERT INTO t5(content, lid) VALUES('language %s speaks language', %s)", lang, lang)))
	}
	// One flush at COMMIT: one segment per language at (langid*nIndex)*1024.
	if got := langidQuery(t, db, "SELECT level FROM t5_segdir"); got != fmt.Sprintf("0 1024 2048 %d", 1<<40) {
		t.Fatalf("segdir levels: got %q, want %q", got, fmt.Sprintf("0 1024 2048 %d", 1<<40))
	}
	// 5.1.2/5.2: each language's segment answers only its own MATCH (rows
	// went in in language order, so the lang-L row's docid is pos+1).
	for pos, lang := range []string{"0", "1", "2", fmt.Sprint(1 << 30)} {
		q := fmt.Sprintf("SELECT docid FROM t5 WHERE t5 MATCH 'language' AND lid=%s", lang)
		want := fmt.Sprint(pos + 1)
		if got := langidQuery(t, db, q); got != want {
			t.Fatalf("per-language MATCH %s: got %q, want %q", lang, got, want)
		}
	}
	// Integrity check over the per-language segments (6.x contract).
	if got := langidQuery(t, db, "INSERT INTO t5(t5) VALUES('integrity-check')"); got != "" {
		t.Fatalf("integrity-check output: %q", got)
	}
}
