package frigolite_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// templateCloneDB opens an in-memory database with one table
// t(k INTEGER PRIMARY KEY, v TEXT, n INTEGER) holding rows 0..n-1.
func templateCloneDB(t *testing.T, n int) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t(k INTEGER PRIMARY KEY, v TEXT, n INTEGER)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO t(k,v,n) VALUES")
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'v%d',%d)", i, i, i*10)
	}
	if r := db.Exec(sb.String()); r.Error != nil {
		t.Fatalf("seed: %v", r.Error)
	}
	return db
}

// TestTemplateClonePointSelect drives unique-literal point SELECTs so every
// statement takes the template-cache substitution path, and checks the
// substituted AST returns exactly the fresh-parse result.
func TestTemplateClonePointSelect(t *testing.T) {
	db := templateCloneDB(t, 50)
	for i := 0; i < 50; i++ {
		r := db.Query(fmt.Sprintf("SELECT v, n FROM t WHERE k = %d", i))
		if r.Error != nil {
			t.Fatalf("select %d: %v", i, r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("select %d: got %d rows", i, len(r.Rows))
		}
		if got := r.Rows[0][0]; got != fmt.Sprintf("v%d", i) {
			t.Fatalf("select %d: v = %v", i, got)
		}
		if got := r.Rows[0][1]; got != int64(i*10) {
			t.Fatalf("select %d: n = %v", i, got)
		}
	}
}

// TestTemplateCloneMixedLiteralTypes runs structurally identical statements
// whose only difference is a quoted vs numeric literal in the same slot: the
// substituted node kind must follow the value, not the first template.
func TestTemplateCloneMixedLiteralTypes(t *testing.T) {
	db := templateCloneDB(t, 6)
	if r := db.Exec("INSERT INTO t(k,v,n) VALUES(90,'five',50)"); r.Error != nil {
		t.Fatalf("seed 90: %v", r.Error)
	}
	cases := []struct {
		where string
		want  string
	}{
		{"k = 0", "v0"},
		{"k = '5'", "v5"},
		{"k = 5", "v5"},
		{"k = 90", "five"},
		{"n = 30", "v3"},
		{"n = '50'", "v5,five"}, // rows 5 and 90 both carry n=50
		{"v = 'v2'", "v2"},
	}
	for i, tc := range cases {
		r := db.Query("SELECT v FROM t WHERE " + tc.where)
		if r.Error != nil {
			t.Fatalf("case %d (%s): %v", i, tc.where, r.Error)
		}
		var got []string
		for _, row := range r.Rows {
			got = append(got, row[0].(string))
		}
		if strings.Join(got, ",") != tc.want {
			t.Fatalf("case %d (%s): got %v, want [%s]", i, tc.where, got, tc.want)
		}
	}
}

// TestTemplateCloneBlobRefusesTemplate pins the bail-out for blob literals:
// x'4142' and x'4243' share a normalized shape; the template path must fall
// back to a full parse instead of reusing the first statement's blob.
func TestTemplateCloneBlobRefusesTemplate(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(q string) {
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("%s: %v", q, r.Error)
		}
	}
	must("CREATE TABLE b(id INTEGER PRIMARY KEY, v)")
	must("INSERT INTO b VALUES(1, x'4142')")
	must("INSERT INTO b VALUES(2, x'4243')")
	r := db.Query("SELECT id FROM b WHERE v = x'4243'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("blob select returned %v, want [[2]]", r.Rows)
	}
	r = db.Query("SELECT id FROM b WHERE v = x'4142'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("blob select returned %v, want [[1]]", r.Rows)
	}
}

// TestTemplateCloneUpdateDelete drives unique-literal UPDATE and DELETE
// statements through the template path and verifies the substituted AST
// touches exactly the targeted rows.
func TestTemplateCloneUpdateDelete(t *testing.T) {
	db := templateCloneDB(t, 40)
	for i := 0; i < 20; i++ {
		r := db.Exec(fmt.Sprintf("UPDATE t SET v = 'u%d', n = %d WHERE k = %d", i, i+1000, i))
		if r.Error != nil {
			t.Fatalf("update %d: %v", i, r.Error)
		}
		if r.Changes != 1 {
			t.Fatalf("update %d: changes = %d", i, r.Changes)
		}
	}
	for i := 20; i < 30; i++ {
		r := db.Exec(fmt.Sprintf("DELETE FROM t WHERE k = %d AND n = %d", i, i*10))
		if r.Error != nil {
			t.Fatalf("delete %d: %v", i, r.Error)
		}
		if r.Changes != 1 {
			t.Fatalf("delete %d: changes = %d", i, r.Changes)
		}
	}
	r := db.Query("SELECT k, v, n FROM t WHERE k = 7")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][1] != "u7" || r.Rows[0][2] != int64(1007) {
		t.Fatalf("updated row: %v", r.Rows)
	}
	if r := db.Query("SELECT COUNT(*) FROM t"); r.Error != nil {
		t.Fatal(r.Error)
	} else if got := r.Rows[0][0]; got != int64(30) {
		t.Fatalf("row count after updates+deletes: %v, want 30", got)
	}
}

// TestTemplateCloneIdentifierDigits pins the guard against normalized-shape
// collisions between identifiers that differ only in a digit (ta1 vs ta2):
// both statements share a normalized key with a phantom value; the template
// path must fall back to a fresh parse and each statement must read its own
// table.
func TestTemplateCloneIdentifierDigits(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(q string) {
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("%s: %v", q, r.Error)
		}
	}
	must("CREATE TABLE ta1(v TEXT)")
	must("CREATE TABLE ta2(v TEXT)")
	must("INSERT INTO ta1 VALUES('one-ta1')")
	must("INSERT INTO ta2 VALUES('two-ta2')")
	// Prime with ta1, then query ta2: the response must come from ta2.
	if r := db.Query("SELECT v FROM ta1"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query("SELECT v FROM ta2")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "two-ta2" {
		t.Fatalf("ta2 select returned %v, want [[two-ta2]]", r.Rows)
	}
}

// TestTemplateCloneExplainParity checks the EXPLAIN QUERY PLAN (and EXPLAIN)
// output of a template-substituted statement equals the fresh parse of the
// same text: substitution must not change compiled behavior.
func TestTemplateCloneExplainParity(t *testing.T) {
	db := templateCloneDB(t, 10)
	// First statement parses and seeds both caches; the second statement of
	// the same shape takes the substitution path.
	explain := func(sql string) [][]interface{} {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		return r.Rows
	}
	eqpFirst := explain("EXPLAIN QUERY PLAN SELECT v FROM t WHERE k = 3")
	eqpSecond := explain("EXPLAIN QUERY PLAN SELECT v FROM t WHERE k = 4")
	if fmt.Sprint(eqpFirst) != fmt.Sprint(eqpSecond) {
		t.Fatalf("EQP diverged:\n first: %v\n second: %v", eqpFirst, eqpSecond)
	}
	// A brand-new shape: first parse vs later substituted parse.
	eqpNew1 := explain("EXPLAIN QUERY PLAN SELECT v FROM t WHERE v = 'v3' AND n = 30")
	eqpNew2 := explain("EXPLAIN QUERY PLAN SELECT v FROM t WHERE v = 'v4' AND n = 40")
	if fmt.Sprint(eqpNew1) != fmt.Sprint(eqpNew2) {
		t.Fatalf("EQP (new shape) diverged:\n first: %v\n second: %v", eqpNew1, eqpNew2)
	}
	// Plain EXPLAIN opcodes must be structurally identical too (op counts).
	ex1 := explain("EXPLAIN SELECT v FROM t WHERE k = 3")
	ex2 := explain("EXPLAIN SELECT v FROM t WHERE k = 4")
	if len(ex1) != len(ex2) {
		t.Fatalf("EXPLAIN op counts diverged: %d vs %d", len(ex1), len(ex2))
	}
}

// TestTemplateCloneSourceOrderSubstitution pins the value-consumption order:
// the substitution walk consumes normalized values in SOURCE order (WITH
// clause first), so two statements sharing a normalized shape must each see
// their own literals — json501-2.1/3.1 regressed here when the walk visited
// the select list before the WITH body.
func TestTemplateCloneSourceOrderSubstitution(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Prime: the WITH-body literal is '{"a":5, "b":6, }' and the select-list
	// literal is 'b'.
	r := db.Query(`WITH c(x) AS (VALUES('{"a":5, "b":6, }')) SELECT x->>'b', json(x), json_valid(x), NOT json_error_position(x) FROM c;`)
	if r.Error != nil {
		t.Fatalf("prime: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(6) {
		t.Fatalf("prime row: %v", r.Rows)
	}
	// Same normalized shape, but now the WITH body is '[5, 6,]' and the
	// select-list literal is numeric 1. A swapped substitution evaluates
	// x ->> '[5, 6,]' (a bogus JSON path) instead of x ->> 1.
	r = db.Query(`WITH c(x) AS (VALUES('[5, 6,]')) SELECT x->>1, json(x), json_valid(x), NOT json_error_position(x) FROM c;`)
	if r.Error != nil {
		t.Fatalf("swapped-shape query: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(6) {
		t.Fatalf("swapped-shape row: %v, want [[6 ...]]", r.Rows)
	}
	// Same class for UPDATE: WITH literal vs SET literal.
	mustExecIdx(t, db, "CREATE TABLE so(k INTEGER PRIMARY KEY, v TEXT)")
	mustExecIdx(t, db, `WITH w AS (SELECT 'a' AS p) INSERT INTO so VALUES(1, (SELECT p FROM w))`)
	r = db.Query("SELECT v FROM so WHERE k = 1")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "a" {
		t.Fatalf("WITH-INSERT row: %v", r.Rows)
	}
}

// TestTemplateCloneNumericKindPreservation pins integer/REAL distinction
// through the template path: SQLite distinguishes 5 from 5.0 (typeof/quote
// expose it), so an integer-shaped slot must never serve a float value whose
// canonical spelling coincides, and vice versa (p4 quote(0)/quote(0.0)
// regressed here).
func TestTemplateCloneNumericKindPreservation(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	quote := func(lit string) string {
		t.Helper()
		r := db.Query("SELECT quote(" + lit + ")")
		if r.Error != nil {
			t.Fatalf("quote(%s): %v", lit, r.Error)
		}
		return r.Rows[0][0].(string)
	}
	typ := func(lit string) string {
		t.Helper()
		r := db.Query("SELECT typeof(" + lit + ")")
		if r.Error != nil {
			t.Fatalf("typeof(%s): %v", lit, r.Error)
		}
		return r.Rows[0][0].(string)
	}
	// Prime the shared normalized shape with each kind, then query the other.
	if got := quote("0"); got != "0" {
		t.Fatalf("quote(0) = %q", got)
	}
	if got := quote("0.0"); got != "0.0" {
		t.Fatalf("quote(0.0) = %q, want 0.0", got)
	}
	if got := quote("5"); got != "5" {
		t.Fatalf("quote(5) = %q", got)
	}
	if got := quote("5.0"); got != "5.0" {
		t.Fatalf("quote(5.0) = %q, want 5.0", got)
	}
	if got := quote("1e2"); got != "100.0" {
		t.Fatalf("quote(1e2) = %q, want 100.0", got)
	}
	if got := typ("0.0"); got != "real" {
		t.Fatalf("typeof(0.0) = %q, want real", got)
	}
	if got := typ("0"); got != "integer" {
		t.Fatalf("typeof(0) = %q, want integer", got)
	}
	if got := typ("1e2"); got != "real" {
		t.Fatalf("typeof(1e2) = %q, want real", got)
	}
}

// TestTemplateCloneMultiStatement covers a multi-statement string whose
// literals substitute across statements in order.
func TestTemplateCloneMultiStatement(t *testing.T) {
	db := templateCloneDB(t, 20)
	for i := 0; i < 5; i++ {
		r := db.Exec(fmt.Sprintf("UPDATE t SET n = %d WHERE k = %d; DELETE FROM t WHERE k = %d;", 500+i, i, 10+i))
		if r.Error != nil {
			t.Fatalf("batch %d: %v", i, r.Error)
		}
	}
	r := db.Query("SELECT n FROM t WHERE k = 3")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(503) {
		t.Fatalf("batch-updated row: %v", r.Rows)
	}
	if r := db.Query("SELECT COUNT(*) FROM t WHERE k BETWEEN 10 AND 14"); r.Error != nil {
		t.Fatal(r.Error)
	} else if got := r.Rows[0][0]; got != int64(0) {
		t.Fatalf("batch-deleted rows remain: %v", got)
	}
}

// TestTemplateCloneOverflowLiteral pins two template-cache hazards around
// integer literals that exceed int64 (func4-5.29, window1 ntile):
//   - an overflowing literal (2^64) must not share a template substitution
//     with a smaller literal whose wrapped value it equals (fastParseInt64
//     used to wrap 18446744073709551616 to 0, so tointeger(toreal(2^64))
//     served a toreal(0) template and returned 0 instead of NULL);
//   - a window function whose argument substitutes must keep its OVER
//     clause (the COW FuncCall clone used to drop it, so
//     ntile('zbc') OVER (ORDER BY a) reported "misuse of window function").
func TestTemplateCloneOverflowLiteral(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE t2(a INTEGER)")
	must("INSERT INTO t2 VALUES(1),(2),(3)")
	queries := []struct {
		sql  string
		want string // flatten() of Rows: "{}" = NULL
	}{
		{"SELECT tointeger(toreal(18446744073709551616))", "{}"},
		{"SELECT tointeger(toreal(18446744073709551615))", "{}"},
		{"SELECT tointeger(toreal(0))", "[0]"},
		{"SELECT tointeger(toreal(18446744073709551616))", "{}"},
		{"SELECT ntile('zbc') OVER (ORDER BY a) FROM t2", "ntile-arg"},
	}
	for _, q := range queries {
		r := db.Query(q.sql)
		if q.want == "ntile-arg" {
			// The OVER clause must survive substitution: the error is about
			// the ARGUMENT (sqlite3: "argument of ntile must be a positive
			// integer"), never "misuse of window function" (a dropped OVER).
			if r.Error == nil || !strings.Contains(r.Error.Error(), "argument of ntile must be a positive integer") {
				t.Fatalf("%s: expected ntile argument error, got %v / %v", q.sql, r.Error, r.Rows)
			}
			continue
		}
		if r.Error != nil {
			t.Fatalf("%s: %v", q.sql, r.Error)
		}
		var got string
		if len(r.Rows) == 1 && len(r.Rows[0]) == 1 {
			if v, ok := r.Rows[0][0].(int64); ok {
				got = "[" + fmt.Sprint(v) + "]"
			} else if r.Rows[0][0] == nil {
				got = "{}"
			}
		}
		if got != q.want {
			t.Fatalf("%s = %s, want %s", q.sql, got, q.want)
		}
	}
}
