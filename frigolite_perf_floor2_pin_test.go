package frigolite

import (
	"strings"
	"testing"
)

// Pins for the PERF.FLOOR2 per-statement floor work: the speculative-dispatch
// probes, the generated-column validation early-out, the TVF scope fast path,
// and the collation-map memo. Each pin drives the engine directly
// (Open/Exec/Query) and pins the observable contract the optimized path must
// preserve.

// TestPinCollationMapMemoDDLInvalidation pins the WHERE-collation map memo
// (collationMapFor): a table's declared collation drives WHERE matching, and
// after DDL replaces the table with a same-shaped one (same column name,
// different collation) the memo must rebuild — a stale entry would keep the
// old table's collation behavior.
func TestPinCollationMapMemoDDLInvalidation(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	mustOK := func(stage, sql string) {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: exec %q: %v", stage, sql, res.Error)
		}
	}
	seed := func(stage string) {
		mustOK(stage, "INSERT INTO t VALUES('abc')")
		mustOK(stage, "INSERT INTO t VALUES('ABC')")
	}
	q := func(stage string) int {
		r := db.Query("SELECT a FROM t WHERE a = 'abc' ORDER BY a")
		if r.Error != nil {
			t.Fatalf("%s: query: %v", stage, r.Error)
		}
		return len(r.Rows)
	}

	mustOK("setup", "CREATE TABLE t(a COLLATE NOCASE)")
	seed("seed-nocase")
	if got := q("first"); got != 2 {
		t.Fatalf("first nocase match rows = %d, want 2", got)
	}
	if got := q("memo-hit"); got != 2 {
		t.Fatalf("memo-hit nocase match rows = %d, want 2", got)
	}

	// DDL replaces the table with a same-shaped one (single column a,
	// default BINARY). The fingerprint moves; a stale memo entry (a →
	// NOCASE) would still match both rows here.
	mustOK("recreate", "DROP TABLE t")
	mustOK("recreate", "CREATE TABLE t(a)")
	seed("seed-binary")
	if got := q("post-ddl"); got != 1 {
		t.Fatalf("post-DDL binary match rows = %d, want 1", got)
	}

	// And back: same shape again with NOCASE — the memo must rebuild once
	// more.
	mustOK("recreate2", "DROP TABLE t")
	mustOK("recreate2", "CREATE TABLE t(a COLLATE NOCASE)")
	seed("seed-nocase2")
	if got := q("post-ddl2"); got != 2 {
		t.Fatalf("post-DDL nocase match rows = %d, want 2", got)
	}
}

// TestPinCollationMapMemoSiblingTables pins that two same-shaped tables
// (identical column name/count) never share a stale collation-map entry: the
// map is keyed on the colDefs slice identity, and the two tables' colDefs
// slices are distinct cache entries.
func TestPinCollationMapMemoSiblingTables(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, ddl := range []string{
		"CREATE TABLE t1(a COLLATE NOCASE)",
		"CREATE TABLE t2(a)", // same shape, default BINARY
		"INSERT INTO t1 VALUES('abc')",
		"INSERT INTO t1 VALUES('ABC')",
		"INSERT INTO t2 VALUES('abc')",
		"INSERT INTO t2 VALUES('ABC')",
	} {
		if res := db.Exec(ddl); res.Error != nil {
			t.Fatalf("exec %q: %v", ddl, res.Error)
		}
	}
	// Warm the memo on t1, then query t2: t2 is case-sensitive (BINARY),
	// t1 is not — interleaved results must stay independent.
	q1 := db.Query("SELECT a FROM t1 WHERE a = 'abc' ORDER BY a")
	if q1.Error != nil || len(q1.Rows) != 2 {
		t.Fatalf("t1 nocase rows = %v err %v, want 2", q1.Rows, q1.Error)
	}
	q2 := db.Query("SELECT a FROM t2 WHERE a = 'abc' ORDER BY a")
	if q2.Error != nil || len(q2.Rows) != 1 {
		t.Fatalf("t2 binary rows = %v err %v, want 1", q2.Rows, q2.Error)
	}
	q1 = db.Query("SELECT a FROM t1 WHERE a = 'abc' ORDER BY a")
	if q1.Error != nil || len(q1.Rows) != 2 {
		t.Fatalf("t1 nocase rows (second pass) = %v err %v, want 2", q1.Rows, q1.Error)
	}
}

// TestPinMayScanCreatedVTabDispatch pins the FROM-dispatch eligibility probes:
// ordinary tables skip the speculative vtab materialization paths, while
// eponymous/created virtual tables and table-valued forms still execute
// exactly as before.
func TestPinMayScanCreatedVTabDispatch(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.RegisterEchoModule()
	for _, ddl := range []string{
		"CREATE TABLE plain(id INTEGER PRIMARY KEY, c TEXT)",
		"INSERT INTO plain VALUES(1, 'x')",
		"CREATE TABLE src(v)",
		"INSERT INTO src VALUES('a')",
		"CREATE VIRTUAL TABLE echo1 USING echo(src)",
		"INSERT INTO echo1 VALUES('b')",
	} {
		if res := db.Exec(ddl); res.Error != nil {
			t.Fatalf("exec %q: %v", ddl, res.Error)
		}
	}
	// Ordinary table: full statement pipeline unchanged.
	q := db.Query("SELECT c FROM plain WHERE id = 1")
	if q.Error != nil || len(q.Rows) != 1 || q.Rows[0][0] != "x" {
		t.Fatalf("plain select = %v err %v", q.Rows, q.Error)
	}
	// Eponymous module (generate_series): eponymous dispatch intact.
	q = db.Query("SELECT value FROM generate_series(1, 3) ORDER BY value")
	if q.Error != nil || len(q.Rows) != 3 {
		t.Fatalf("generate_series rows = %v err %v", q.Rows, q.Error)
	}
	// Created virtual table (echo): created-vtab dispatch intact. The echo
	// scan returns the source table's rows (echo1's own insert forwarded
	// into src, so src holds 'a' and 'b').
	q = db.Query("SELECT v FROM echo1 ORDER BY v")
	if q.Error != nil || len(q.Rows) != 2 || q.Rows[0][0] != "a" || q.Rows[1][0] != "b" {
		t.Fatalf("echo scan rows = %v err %v", q.Rows, q.Error)
	}
	// Echo vtab writes still flow through to the source table.
	if res := db.Exec("INSERT INTO echo1 VALUES('c')"); res.Error != nil {
		t.Fatalf("echo insert: %v", res.Error)
	}
	q = db.Query("SELECT count(*) FROM src")
	if q.Error != nil || q.Rows[0][0] != int64(3) {
		t.Fatalf("src count = %v err %v, want 3", q.Rows, q.Error)
	}
	// Missing table still reports the plain resolution error (the probes
	// fall through to the real-table path).
	q = db.Query("SELECT * FROM nosuchtable")
	if q.Error == nil || q.Error.Error() != "no such table: nosuchtable" {
		t.Fatalf("missing table error = %v", q.Error)
	}
}

// TestPinGeneratedColumnSafetyCheckPreserved pins that the generated-column
// early-out in prevalidateSchemaFunctionSafety skips only the reference
// collection: tables WITH generated columns still reject unsafe functions
// under trusted_schema=off, and unreferenced generated columns stay exempt.
func TestPinGeneratedColumnSafetyCheckPreserved(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if res := db.Exec("PRAGMA trusted_schema=OFF"); res.Error != nil {
		t.Fatalf("pragma: %v", res.Error)
	}
	// No generated columns: the early-out path; statement runs.
	if res := db.Exec("CREATE TABLE tg(a, b)"); res.Error != nil {
		t.Fatalf("create plain: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO tg VALUES(1, 2)"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	if q := db.Query("SELECT a, b FROM tg"); q.Error != nil {
		t.Fatalf("select over generated-free table: %v", q.Error)
	}
	// Generated column with a function later re-registered DIRECTONLY: the
	// referenced generated column must still be rejected under
	// trusted_schema=off (the check runs only when generated columns exist,
	// and only for columns the statement actually references).
	db.RegisterFunctionFlags("safefn", func(args []interface{}) (interface{}, error) {
		return args[0], nil
	}, 1, 1, true, false)
	if res := db.Exec("CREATE TABLE tu(a, g AS (safefn(a)))"); res.Error != nil {
		t.Fatalf("create generated: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO tu(a) VALUES(1)"); res.Error != nil {
		t.Fatalf("insert generated: %v", res.Error)
	}
	q := db.Query("SELECT a, g FROM tu")
	if q.Error != nil || len(q.Rows) != 1 || q.Rows[0][1] != int64(1) {
		t.Fatalf("innocuous generated rows = %v err %v", q.Rows, q.Error)
	}
	// Re-register the same function DIRECTONLY: now the referencing select
	// errors, the non-referencing one stays clean.
	db.RegisterFunctionFlags("safefn", func(args []interface{}) (interface{}, error) {
		return args[0], nil
	}, 1, 1, false, true)
	q = db.Query("SELECT a, g FROM tu")
	if q.Error == nil || !strings.Contains(q.Error.Error(), "unsafe use of safefn()") {
		t.Fatalf("unsafe generated error = %v, want unsafe use of safefn()", q.Error)
	}
	q = db.Query("SELECT a FROM tu")
	if q.Error != nil || len(q.Rows) != 1 {
		t.Fatalf("unreferenced generated select = %v err %v", q.Rows, q.Error)
	}
}

// TestPinTVFArgScopeFastPathPreserved pins that the no-TVF fast path in
// validateTVFArgScope preserves both scope errors (tabfunc01-1410/1420
// shapes): a join operand with table-function arguments still triggers the
// full scope check.
func TestPinTVFArgScopeFastPathPreserved(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, ddl := range []string{"CREATE TABLE t1(x)", "CREATE TABLE t2(y)"} {
		if res := db.Exec(ddl); res.Error != nil {
			t.Fatalf("exec %q: %v", ddl, res.Error)
		}
	}
	// tabfunc01-1410: the function sits right of a RIGHT join and its
	// argument references t2 to its right.
	q := db.Query("SELECT x, y, value\n  FROM (t1 RIGHT JOIN generate_series(t2.y,5) AS value) JOIN t2")
	if q.Error == nil || q.Error.Error() != "table-function argument references tables to its right" {
		t.Fatalf("1410 error = %v", q.Error)
	}
	// tabfunc01-1420: parenthesized JOIN group shields outer names.
	q = db.Query("SELECT x, y, value \n  FROM t2 JOIN (t1 RIGHT JOIN generate_series(t2.y,5) AS value)")
	if q.Error == nil || q.Error.Error() != "no such column: t2.y" {
		t.Fatalf("1420 error = %v", q.Error)
	}
	// Plain single-table statement (the fast path) keeps working.
	if res := db.Exec("INSERT INTO t1 VALUES(7)"); res.Error != nil {
		t.Fatalf("insert t1: %v", res.Error)
	}
	q = db.Query("SELECT x FROM t1 WHERE x = 7")
	if q.Error != nil || len(q.Rows) != 1 {
		t.Fatalf("plain select = %v err %v", q.Rows, q.Error)
	}
}
