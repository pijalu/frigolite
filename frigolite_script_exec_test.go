package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Multi-statement script execution (sqlite3_exec's loop over sqlite3_prepare):
// a script is prepared and run statement by statement, each statement consumed
// before the next prepare. These pin the observable contract the split must
// preserve — row sets, prefix execution before a mid-script error, and the
// per-statement identity of the template-cache clones a repeated statement
// shape gets.

// TestScriptExecPlainSplitEquivalence pins the fast split (splitPlainScript,
// taken when every byte is one the tokenizer always classifies as itself)
// against the general tokenizer walk: for every plain script the two must cut
// the same chunks and agree on the TRIGGER word, and a script with a byte that
// can hide a semicolon must NOT take the fast path.
func TestScriptExecPlainSplitEquivalence(t *testing.T) {
	plain := []string{
		"SELECT 1;",
		"SELECT 1; SELECT 2;",
		"SELECT 1;;SELECT 2;",
		"  ;  ",
		"SELECT 1; SELECT 2",
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT); CREATE INDEX i ON t(b);",
		"INSERT INTO t VALUES(1,2,'x');SELECT a,b FROM t WHERE a=1;",
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END;",
		"select 'trigger';", // not plain (quotes)
		"WITH x AS (SELECT 1 AS v) SELECT v FROM x; SELECT 2;",
		"UPDATE t SET (a,b)=(1,2) WHERE a=3; SELECT 1;",
		"PRAGMA page_size; PRAGMA auto_vacuum;",
		"SELECT a FROM t ORDER BY a LIMIT 1 OFFSET 2;",
	}
	for _, s := range plain {
		wantTexts, wantTrigger, wantOK := splitTokenizerScript(s)
		if got, trigger, ok := splitScriptStatements(s); ok != wantOK {
			t.Fatalf("%q: ok=%v want %v", s, ok, wantOK)
		} else if ok {
			if len(got) != len(wantTexts) {
				t.Fatalf("%q: texts=%q want %q", s, got, wantTexts)
			}
			for i := range got {
				if got[i] != wantTexts[i] {
					t.Fatalf("%q: text %d = %q want %q", s, i, got[i], wantTexts[i])
				}
			}
			if trigger != wantTrigger {
				t.Fatalf("%q: trigger=%v want %v", s, trigger, wantTrigger)
			}
		}
		if plain, _ := plainScriptScan(s); plain {
			got := splitPlainScript(s)
			if len(got) != len(wantTexts) {
				t.Fatalf("plain %q: texts=%q want %q", s, got, wantTexts)
			}
			for i := range got {
				if got[i] != wantTexts[i] {
					t.Fatalf("plain %q: text %d = %q want %q", s, i, got[i], wantTexts[i])
				}
			}
		}
	}
	// Bytes that can hide a semicolon must refuse the fast path.
	for _, s := range []string{
		"SELECT 'a;b';",
		"SELECT \"c;d\";",
		"SELECT [e;f];",
		"SELECT 1 /* ; */;",
		"SELECT 1; -- ;\nSELECT 2;",
		"SELECT `g;h`;",
		"SELECT 1; SELECT #x;",
		"SELECT 1; SELECT !;",
	} {
		if plain, _ := plainScriptScan(s); plain {
			t.Fatalf("%q: plainScriptScan accepted a script with a hiding byte", s)
		}
	}
}

// TestScriptExecSameTemplateDistinctValues is the aliasing regression: every
// statement of the script shares one template (same shape, different literal),
// and each must execute with ITS OWN literal. The engine serves them from one
// per-(template, depth) clone, which is only safe because each statement is
// prepared and run before the next prepare reuses the clone.
func TestScriptExecSameTemplateDistinctValues(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "SELECT %d;", i)
	}
	res := db.Query(sb.String())
	if res.Error != nil {
		t.Fatalf("batch query: %v", res.Error)
	}
	if len(res.Rows) != 200 {
		t.Fatalf("rows = %d, want 200", len(res.Rows))
	}
	for i, row := range res.Rows {
		if len(row) != 1 || row[0] != int64(i) {
			t.Fatalf("row %d = %v, want [%d]", i, row, i)
		}
	}
}

// TestScriptExecMatchesLoop runs the same statements as one script and as one
// call per statement, and requires identical rows.
func TestScriptExecMatchesLoop(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b INTEGER, c TEXT)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("BEGIN"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for i := 1; i <= 50; i++ {
		if r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d,%d,'n%d')", i, i*2, i)); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatal(r.Error)
	}

	var sb strings.Builder
	var want [][]interface{}
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "SELECT c FROM t WHERE a=%d;", i)
		one := db.Query(fmt.Sprintf("SELECT c FROM t WHERE a=%d", i))
		if one.Error != nil {
			t.Fatal(one.Error)
		}
		want = append(want, one.Rows...)
	}
	got := db.Query(sb.String())
	if got.Error != nil {
		t.Fatalf("batch query: %v", got.Error)
	}
	if len(got.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(got.Rows), len(want))
	}
	for i := range want {
		if got.Rows[i][0] != want[i][0] {
			t.Fatalf("row %d = %v, want %v", i, got.Rows[i], want[i])
		}
	}
}

// TestScriptExecEmptyFragments: empty statements and comment-only fragments are
// no-ops, exactly as the whole-script parser's collapse pass makes them.
func TestScriptExecEmptyFragments(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	res := db.Query("SELECT 1;;SELECT 2;/* comment only */;  ;SELECT 3;")
	if res.Error != nil {
		t.Fatalf("query: %v", res.Error)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("rows = %v, want 3 rows", res.Rows)
	}
	for i, want := range []int64{1, 2, 3} {
		if res.Rows[i][0] != want {
			t.Fatalf("row %d = %v, want %d", i, res.Rows[i], want)
		}
	}
}

// TestScriptExecErrorMidScript: the parseable prefix runs once and the trailing
// error is reported (sqlite3_exec's contract) — and the fall back to the
// whole-script parser must not run the prefix a second time.
func TestScriptExecErrorMidScript(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(a)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	res := db.Exec("INSERT INTO t VALUES(1); INSERT INTO t VALUES(2); SELEC bad; INSERT INTO t VALUES(3);")
	if res.Error == nil {
		t.Fatal("expected a syntax error")
	}
	got := db.Query("SELECT count(*) FROM t")
	if got.Error != nil {
		t.Fatal(got.Error)
	}
	if n := got.Rows[0][0]; n != int64(2) {
		t.Fatalf("rows inserted = %v, want 2 (prefix once, no replay)", n)
	}
}

// TestScriptExecCreateTrigger: a CREATE TRIGGER body carries top-level
// semicolons, so the script is not splittable into statements and must fall
// back to the whole-script parser — with the trigger body firing exactly once
// per insert.
func TestScriptExecCreateTrigger(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	script := "CREATE TABLE t(a); " +
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO t VALUES(new.a+100); END; " +
		"INSERT INTO t VALUES(1);"
	if r := db.Exec(script); r.Error != nil {
		t.Fatalf("trigger script: %v", r.Error)
	}
	got := db.Query("SELECT a FROM t ORDER BY a")
	if got.Error != nil {
		t.Fatal(got.Error)
	}
	if len(got.Rows) != 2 || got.Rows[0][0] != int64(1) || got.Rows[1][0] != int64(101) {
		t.Fatalf("rows = %v, want [[1] [101]]", got.Rows)
	}
}

// TestScriptExecSavepoint: SAVEPOINT / RELEASE statements are prepared on their
// own and still nest correctly inside the script.
func TestScriptExecSavepoint(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	script := "CREATE TABLE t(a); BEGIN; INSERT INTO t VALUES(1); SAVEPOINT s1; " +
		"INSERT INTO t VALUES(2); RELEASE s1; COMMIT; SELECT count(*) FROM t;"
	res := db.Query(script)
	if res.Error != nil {
		t.Fatalf("script: %v", res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("rows = %v, want [[2]]", res.Rows)
	}
}

// TestScriptExecSQLLengthLimit: the limit counts the text a single prepare call
// receives, so a splittable script must still be rejected before anything runs.
func TestScriptExecSQLLengthLimit(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	prior := db.SetLimit("SQLITE_LIMIT_SQL_LENGTH", 30)
	defer db.SetLimit("SQLITE_LIMIT_SQL_LENGTH", prior)

	res := db.Query("SELECT 1; SELECT 2; SELECT 3; SELECT 4;")
	if res.Error == nil {
		t.Fatal("expected the SQL length limit error")
	}
	if !strings.Contains(res.Error.Error(), "too big") {
		t.Fatalf("error = %v, want a too-big error", res.Error)
	}
}

// TestScriptExecTraceTexts: the trace hook reports each statement's own source
// text, in order.
func TestScriptExecTraceTexts(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	var seen []string
	db.SetTraceHook(func(sql string) { seen = append(seen, sql) })
	if r := db.Query("SELECT 1; SELECT 2;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	db.SetTraceHook(nil)
	if len(seen) != 2 {
		t.Fatalf("trace texts = %q, want 2 entries", seen)
	}
	if !strings.Contains(seen[0], "SELECT 1") || !strings.Contains(seen[1], "SELECT 2") {
		t.Fatalf("trace texts = %q, want the two statements in order", seen)
	}
}
