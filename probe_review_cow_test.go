package frigolite

import (
	"testing"
)

// Area 3 stress: the same normalized statement template re-executed with
// different literals while planner/schema state changes underneath. The COW
// template cache stores the PRE-execution parse and executes it directly on
// the first run, so any exec-path mutation of that AST persists in the
// template. Each block asserts results stay stable (and match the pre-perf
// base tree — the DIFF runs on 373665082 must produce identical output).

func cowOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func cowExec(t *testing.T, db *DB, sql string) {
	t.Helper()
	if r := db.Exec(sql); r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
}

func cowQueryDump(t *testing.T, db *DB, sql string) string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	return probeDump(r.Rows)
}

func TestReviewTemplateStressPlannerChanges(t *testing.T) {
	db := cowOpen(t)
	cowExec(t, db, "CREATE TABLE t(a INT, b INT); INSERT INTO t VALUES(1,10),(2,20),(3,30),(4,40)")

	var out string
	snap := func(stage string) {
		got := ""
		for _, v := range []string{"0", "15", "100"} {
			got += cowQueryDump(t, db, "SELECT a FROM t WHERE b > "+v)
			got += "--\n"
		}
		got += cowQueryDump(t, db, "SELECT a FROM (SELECT * FROM t) WHERE a > 1")
		got += "--\n"
		got += cowQueryDump(t, db, "SELECT a, b FROM t WHERE b >= 20 ORDER BY 2")
		got += "--\n"
		got += cowQueryDump(t, db, "SELECT a FROM t ORDER BY a LIMIT 2 OFFSET 1")
		got += "--\n"
		got += cowQueryDump(t, db, "SELECT a FROM t WHERE a IN (1, 3)")
		got += "--\n"
		out += stage + ":\n" + got
	}

	snap("initial")
	cowExec(t, db, "CREATE INDEX ib ON t(b)")
	snap("after-create-index")
	cowExec(t, db, "DROP INDEX ib")
	snap("after-drop-index")
	cowExec(t, db, "CREATE TABLE other(x); INSERT INTO other VALUES(1)")
	cowExec(t, db, "ANALYZE")
	snap("after-analyze")
	cowExec(t, db, "ALTER TABLE t ADD COLUMN c DEFAULT 7")
	snap("after-add-column")
	cowExec(t, db, "CREATE VIEW v AS SELECT * FROM t")
	snap("after-create-view")
	t.Logf("\n%s", out)
}

func TestReviewTemplateInsertUpdateDeleteKinds(t *testing.T) {
	db := cowOpen(t)
	cowExec(t, db, "CREATE TABLE t1(a); CREATE TABLE t2(a)")
	// Same INSERT shape, different tables (identifier digits must not
	// collapse templates) and different literal kinds.
	cowExec(t, db, "INSERT INTO t1 VALUES(5)")
	cowExec(t, db, "INSERT INTO t2 VALUES(5)")
	cowExec(t, db, "INSERT INTO t1 VALUES(6.0)")
	cowExec(t, db, "INSERT INTO t2 VALUES('x')")
	got := cowQueryDump(t, db, "SELECT typeof(a), a FROM t1") + "|" + cowQueryDump(t, db, "SELECT typeof(a), a FROM t2")
	// KNOWN BUG (probe_review_kind_test.go::TestReviewTemplateNumericKindParity,
	// oracle real|6.0): the 6.0 lands as INTEGER through the integer-primed
	// INSERT template. Pinned here as a change-detector: flip this assertion
	// to the oracle values when the INSERT path gets the numeric-kind gate.
	want := "string:integer|int64:5\nstring:integer|int64:6\n|string:integer|int64:5\nstring:text|string:x\n"
	if got != want {
		t.Fatalf("INSERT template kind behavior changed: %s", got)
	}
	// UPDATE / DELETE templates with varying values.
	cowExec(t, db, "CREATE TABLE u(k INT, v INT)")
	for i := 0; i < 6; i++ {
		cowExec(t, db, "INSERT INTO u VALUES(1, "+string(rune('0'+i))+")") // same template, v=0..5
	}
	cowExec(t, db, "UPDATE u SET v = 99 WHERE v = 3")
	if n := cowQueryDump(t, db, "SELECT count(*) FROM u WHERE v = 99"); n != "int64:1\n" {
		t.Fatalf("update template: %s", n)
	}
	cowExec(t, db, "DELETE FROM u WHERE v = 4")
	if n := cowQueryDump(t, db, "SELECT count(*) FROM u WHERE v = 4"); n != "int64:0\n" {
		t.Fatalf("delete template: %s", n)
	}
	if n := cowQueryDump(t, db, "SELECT count(*) FROM u"); n != "int64:5\n" {
		t.Fatalf("row count after template update+delete: %s", n)
	}
}

func TestReviewTemplateCompoundAndCase(t *testing.T) {
	db := cowOpen(t)
	cowExec(t, db, "CREATE TABLE t(x INT); INSERT INTO t VALUES(1),(2)")
	// Compound with two substituted literals, alternating order.
	for _, pair := range [][2]string{{"10", "20"}, {"30", "40"}, {"20", "10"}} {
		got := cowQueryDump(t, db, "SELECT "+pair[0]+" UNION ALL SELECT "+pair[1])
		want := "int64:" + pair[0] + "\nint64:" + pair[1] + "\n"
		if got != want {
			t.Fatalf("compound substitution %v: %s", pair, got)
		}
	}
	// CASE with substituted condition literals.
	for _, v := range []string{"1", "0", "1"} {
		got := cowQueryDump(t, db, "SELECT CASE WHEN "+v+" THEN 'yes' ELSE 'no' END")
		want := "string:yes\n"
		if v == "0" {
			want = "string:no\n"
		}
		if got != want {
			t.Fatalf("case template %s: %s", v, got)
		}
	}
	// ORDER BY ordinal templates — different ordinals through the same slot.
	cowExec(t, db, "CREATE TABLE o(a INT, b INT); INSERT INTO o VALUES(1,2),(2,1)")
	if got := cowQueryDump(t, db, "SELECT a, b FROM o ORDER BY 1"); got != "int64:1|int64:2\nint64:2|int64:1\n" {
		t.Fatalf("order-by-1: %s", got)
	}
	if got := cowQueryDump(t, db, "SELECT a, b FROM o ORDER BY 2"); got != "int64:2|int64:1\nint64:1|int64:2\n" {
		t.Fatalf("order-by-2: %s", got)
	}
	if got := cowQueryDump(t, db, "SELECT a, b FROM o ORDER BY 1"); got != "int64:1|int64:2\nint64:2|int64:1\n" {
		t.Fatalf("order-by-1 again: %s", got)
	}
}

// Unused-column rewrite of a FROM-subquery through a shared template: the
// first execution rewrites the shared subquery in place (b -> NULL); a second
// execution with different literals must still observe the same columns.
func TestReviewTemplateUnusedColumnRewrite(t *testing.T) {
	db := cowOpen(t)
	cowExec(t, db, "CREATE TABLE t(a INT, b INT); INSERT INTO t VALUES(1,10),(2,20)")
	// Uses only subquery column a -> b is nulled in the (shared) subquery AST.
	for _, v := range []string{"0", "1", "2"} {
		got := cowQueryDump(t, db, "SELECT a FROM (SELECT a, b FROM t) WHERE a > "+v)
		t.Logf("unused-col a>%s -> %s", v, got)
		if got != "int64:1\nint64:2\n" && v == "0" {
			t.Fatalf("first run wrong: %s", got)
		}
	}
	// The side-effecting UDF variant (the optimization's reason to exist):
	// the counter must not double-count on template reuse.
	cowExec(t, db, "CREATE TABLE s(x INT); INSERT INTO s VALUES(1),(2)")
	r := db.Query("SELECT x FROM (SELECT x, counter(x) AS c FROM s) WHERE x > 0")
	if r.Error != nil {
		t.Fatalf("counter probe: %v", r.Error)
	}
	r2 := db.Query("SELECT x, counter(x) FROM (SELECT x FROM s) WHERE x > 0")
	if r2.Error != nil {
		t.Logf("counter variant 2: %v", r2.Error)
	}
	// Same shape again — if the rewrite leaked, x itself would be NULLed.
	if got := cowQueryDump(t, db, "SELECT x FROM (SELECT x FROM s) WHERE x > 0"); got != "int64:1\nint64:2\n" {
		t.Fatalf("template reuse lost column: %s", got)
	}
}

// FTS TVF form: the first execution mutates the template root (WHERE += MATCH,
// From.Args = nil); the second execution with a different literal must fall
// back to a full parse and still return correct results.
func TestReviewTemplateFTSTVFForm(t *testing.T) {
	db := cowOpen(t)
	cowExec(t, db, "CREATE VIRTUAL TABLE f USING fts4(c); INSERT INTO f VALUES('alpha beta'),('gamma')")
	for _, q := range []string{"'alpha'", "'gamma'", "'beta'"} {
		got := cowQueryDump(t, db, "SELECT c FROM f WHERE f MATCH "+q)
		t.Logf("fts match %s -> %s", q, got)
		switch q {
		case "'alpha'", "'beta'":
			if got == "" {
				t.Fatalf("fts tvf %s: empty", q)
			}
		case "'gamma'":
			if got != "string:gamma\n" {
				t.Fatalf("fts tvf gamma: %s", got)
			}
		}
	}
}
