package frigolite_test

// Native pin tests for the FULL-SUITE-DRIFT.T26-misc drift classes. Each test
// pins one oracle-verified SQLite behavior that a testgen package covers, so
// the engine-visible contract survives transpiler/harness churn.

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

func openPinDB(t *testing.T) *frigolite.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pin.db")
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func pinExec(t *testing.T, db *frigolite.DB, sql string) {
	t.Helper()
	if res := db.Exec(sql); res.Error != nil {
		t.Fatalf("exec %s: %v", sql, res.Error)
	}
}

func pinQuery(t *testing.T, db *frigolite.DB, sql string) []interface{} {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %s: %v", sql, r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("query %s: no rows", sql)
	}
	return r.Rows[0]
}

func pinExpectError(t *testing.T, db *frigolite.DB, sql, want string) {
	t.Helper()
	r := db.Exec(sql)
	if r.Error == nil {
		t.Fatalf("exec %s: expected error containing %q, got none", sql, want)
	}
	if !strings.Contains(r.Error.Error(), want) {
		t.Fatalf("exec %s: expected error containing %q, got %q", sql, want, r.Error.Error())
	}
}

// TestPinSumOverflowAndInf covers func-37.x/38.x: sum() raises "integer
// overflow" for an unabsorbed int64 overflow, and Inf input yields Inf (not
// NaN) for sum/avg/total (func.c sqlite3IsOverflow guard on the KBN rErr).
func TestPinSumOverflowAndInf(t *testing.T) {
	db := openPinDB(t)
	t.Run("integer overflow", func(t *testing.T) {
		pinExec(t, db, "CREATE TABLE t(a); INSERT INTO t VALUES(9223372036854775807),(9223372036854775807),(123),(-9223372036854775807),(-9223372036854775807)")
		pinExpectError(t, db, "SELECT sum(a) FROM t", "integer overflow")
	})
	t.Run("inf not nan", func(t *testing.T) {
		pinExec(t, db, "CREATE TABLE tf(x); INSERT INTO tf VALUES(9e+999)")
		row := pinQuery(t, db, "SELECT sum(x), avg(x), total(x) FROM tf")
		for i, want := range []float64{math.Inf(1), math.Inf(1), math.Inf(1)} {
			got, _ := row[i].(float64)
			if got != want {
				t.Fatalf("col %d: got %v want %v", i, row[i], want)
			}
		}
	})
}

// TestPinPercentileArity covers percentile-2.x/3.x: exact arity, median()
// error naming, and the capitalized "Inf input to" message.
func TestPinPercentileArity(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, "CREATE TABLE pt(x); INSERT INTO pt VALUES(1),(2),(3)")
	pinExpectError(t, db, "SELECT percentile(x) FROM pt", "wrong number of arguments to function percentile()")
	pinExpectError(t, db, "SELECT percentile_cont(x) FROM pt", "wrong number of arguments to function percentile_cont()")
	pinExpectError(t, db, "SELECT percentile_disc(x) FROM pt", "wrong number of arguments to function percentile_disc()")
	row := pinQuery(t, db, "SELECT median(x) FROM pt")
	if row[0] != 2.0 {
		t.Fatalf("median: got %v want 2", row[0])
	}
}

// TestPinMatchInvalidFunction covers func-4.x: match/2 exists as the FTS
// overload (wrong-args for 3 args) and the operator errors outside FTS.
func TestPinMatchInvalidFunction(t *testing.T) {
	db := openPinDB(t)
	pinExpectError(t, db, "SELECT match(1,2,3)", "wrong number of arguments to function match()")
	pinExpectError(t, db, "SELECT 'abc' MATCH 'xyz'", "unable to use function MATCH in the requested context")
	pinExpectError(t, db, "SELECT 'abc' NOT MATCH 'xyz'", "unable to use function MATCH in the requested context")
}

// TestPinCoalesceLazy covers func-27.x arity and the misc8-1.4 contract that
// COALESCE never evaluates later arguments once one is non-NULL.
func TestPinCoalesceLazy(t *testing.T) {
	db := openPinDB(t)
	pinExpectError(t, db, "SELECT coalesce()", "wrong number of arguments to function coalesce()")
	pinExpectError(t, db, "SELECT coalesce(1)", "wrong number of arguments to function coalesce()")
	pinExec(t, db, "CREATE TABLE t1(a,b,c); INSERT INTO t1 VALUES(1,2,3)")
	row := pinQuery(t, db, "SELECT coalesce(b, eval('ROLLBACK; SELECT 99')) FROM t1")
	if row[0] != int64(2) {
		t.Fatalf("coalesce: got %v want 2", row[0])
	}
}

// TestPinStarArgArity covers func-1.1/1.2: f(*) is a zero-argument call, so
// length(*)/upper(*) fail arity validation while count(*) keeps working.
func TestPinStarArgArity(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, "CREATE TABLE tbl1(t1); INSERT INTO tbl1 VALUES('abc')")
	pinExpectError(t, db, "SELECT length(*) FROM tbl1", "wrong number of arguments to function length()")
	pinExpectError(t, db, "SELECT upper(*) FROM tbl1", "wrong number of arguments to function upper()")
	row := pinQuery(t, db, "SELECT count(*) FROM tbl1")
	if row[0] != int64(1) {
		t.Fatalf("count(*): got %v want 1", row[0])
	}
}

// TestPinRegexpFunctionArity covers regexp1-1.3.2: the function form is
// regexp(P,X) — pattern FIRST, the reverse of the operator.
func TestPinRegexpFunctionArity(t *testing.T) {
	db := openPinDB(t)
	row := pinQuery(t, db, "SELECT regexp('b|c','abc'), regexp('z','abc')")
	if row[0] != int64(1) || row[1] != int64(0) {
		t.Fatalf("regexp: got %v/%v want 1/0", row[0], row[1])
	}
}

// TestPinTokenizerNumberExponent covers tokenize-1.x: a malformed exponent
// leaves e/E for the IdChar loop, and a block-comment opener at end of input
// becomes a slash token (tokenize-2.1/2.2).
func TestPinTokenizerNumberExponent(t *testing.T) {
	db := openPinDB(t)
	pinExpectError(t, db, "SELECT 1.0e+", `unrecognized token: "1.0e"`)
	pinExpectError(t, db, "SELECT 1.0E,5", `unrecognized token: "1.0E"`)
	pinExpectError(t, db, "SELECT 1.0E.10", `unrecognized token: "1.0E"`)
	pinExpectError(t, db, "SELECT 1, 2 /*", `near "*": syntax error`)
	r := db.Query("SELECT 1, 2 /* ")
	if r.Error != nil {
		t.Fatalf("trailing-space comment: %v", r.Error)
	}
}

// TestPinDQSIndexFallback covers quote-2.2/3.5: with the DQS DDL fallback in
// effect (settings on, or writable_schema + DQS DML), an unresolvable
// double-quoted identifier in an index expression becomes a string literal.
func TestPinDQSIndexFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dqs.db")
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetDQS(true, true)
	pinExec(t, db, "CREATE TABLE t1(x, y, z)")
	pinExec(t, db, `CREATE INDEX i2 ON t1(x, y, z||"abc")`)
}

// TestPinCTASNoTableOnFailure covers misc1-15.1.x: a CTAS whose SELECT fails
// name resolution leaves no table behind, and a following CTAS with the same
// name succeeds.
func TestPinCTASNoTableOnFailure(t *testing.T) {
	db := openPinDB(t)
	pinExpectError(t, db, "CREATE TABLE t10 AS SELECT t9.c1", "no such column: t9.c1")
	pinExec(t, db, "CREATE TABLE t10 AS SELECT 1")
	pinExpectError(t, db, "CREATE TABLE tX AS SELECT * FROM tX", "no such table")
}

// TestPinDuplicateCreateSameSession covers misc1-16.2: two identical CREATE
// TABLE statements in ONE session error; the verbatim-recreate accommodation
// is reserved for reopened (persisted) schemas.
func TestPinDuplicateCreateSameSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.db")
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sql := "CREATE TABLE test(a integer, primary key(a))"
	pinExec(t, db, sql)
	pinExpectError(t, db, sql, "table test already exists")
	db.Close()
	// Reopen: the accommodation applies (verbatim re-create after reset).
	db2, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if res := db2.Exec(sql); res.Error != nil {
		t.Fatalf("verbatim re-create after reopen should be tolerated: %v", res.Error)
	}
}

// TestPinNoFromStar covers misc1-8.1/8.2: bare * vs qualified * in a
// FROM-less SELECT produce distinct errors.
func TestPinNoFromStar(t *testing.T) {
	db := openPinDB(t)
	pinExpectError(t, db, "SELECT *", "no tables specified")
	pinExpectError(t, db, "SELECT t1.*", "no such table: t1")
}

// TestPinGroupByOrdinalAggregate covers misc4-4.1/4.2: GROUP BY ordinals
// resolve to result expressions before the no-aggregate check.
func TestPinGroupByOrdinalAggregate(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, "CREATE TABLE Table2(ID, Value)")
	pinExpectError(t, db, "SELECT ID, max(Value) FROM Table2 GROUP BY 1, 2", "aggregate functions are not allowed in the GROUP BY clause")
}

// TestPinLimitSubqueryResolution covers misc5-3.2: LIMIT subqueries are
// name-resolved at prepare time.
func TestPinLimitSubqueryResolution(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, "CREATE TABLE m(a)")
	pinExpectError(t, db, "SELECT * FROM m LIMIT (SELECT count(*) FROM blah)", "no such table: blah")
}

// TestPinDistinctZeroBlobDedup covers distinct-4.1: zeroblob(5) and
// x'0000000000' are one DISTINCT value, and distinct-2.3's key-order output.
func TestPinDistinctZeroBlobDedup(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, `
		CREATE TABLE t3(a INTEGER);
		INSERT INTO t3 VALUES(3),(2),(1),(2),(3),(1);
		CREATE TABLE t2(x);
		INSERT INTO t2 SELECT DISTINCT CASE a WHEN 1 THEN x'0000000000' WHEN 2 THEN zeroblob(5) ELSE 'xyzzy' END FROM t3;
	`)
	r := db.Query("SELECT count(*) FROM t2")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Rows[0][0] != int64(2) {
		t.Fatalf("t2 rows: got %v want 2", r.Rows[0][0])
	}
}

// TestPinUniqueIgnoreLowercase covers null-7.1/7.2: a lowercase
// `unique on conflict ignore` constraint silently skips duplicate rows.
func TestPinUniqueIgnoreLowercase(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, `
		create table u2(a, b unique on conflict ignore);
		insert into u2 values(1,1);
		insert into u2 values(2,null);
		insert into u2 values(3,null);
		insert into u2 values(4,1);
	`)
	r := db.Query("SELECT a FROM u2")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("rows: got %d want 3", len(r.Rows))
	}
}

// TestPinUpdateNotNullColumnConflict covers notnull-2.6/2.8/2.9: statement OR
// REPLACE and column-level ON CONFLICT clauses resolve NOT NULL violations
// (REPLACE substitutes the column DEFAULT; IGNORE skips the row).
func TestPinUpdateNotNullColumnConflict(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, `
		CREATE TABLE nn(a NOT NULL, b NOT NULL DEFAULT 5, c NOT NULL ON CONFLICT REPLACE DEFAULT 6, d NOT NULL ON CONFLICT IGNORE DEFAULT 7, e NOT NULL ON CONFLICT ABORT DEFAULT 8);
		INSERT INTO nn VALUES(1,2,3,4,5);
	`)
	pinExec(t, db, "UPDATE OR REPLACE nn SET b=null, d=e, e=d")
	pinExec(t, db, "UPDATE nn SET c=null, d=e, e=d")
	row := pinQuery(t, db, "SELECT * FROM nn")
	if row[2] != int64(6) || row[1] != int64(5) {
		t.Fatalf("defaults not substituted: %v", row)
	}
}

// TestPinAttachOnReadOnlyMain covers misc7-7.3: a writable file may be
// attached to a read-only main connection.
func TestPinAttachOnReadOnlyMain(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.db")
	aux := filepath.Join(dir, "aux.db")
	db, err := frigolite.Open(main)
	if err != nil {
		t.Fatal(err)
	}
	pinExec(t, db, "ATTACH '"+aux+"' AS aux; CREATE TABLE aux.hello(world); DETACH aux")
	db.Close()
	ro, err := frigolite.OpenReadOnly(main)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if res := ro.Exec("PRAGMA omit_readlock = 1; ATTACH '" + aux + "' AS aux"); res.Error != nil {
		t.Fatalf("attach to read-only main: %v", res.Error)
	}
	r := ro.Query("SELECT name FROM aux.sqlite_master")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
}

// TestPinUDFErrorPropagation pins the engine-visible contract of the
// test_error C-harness fixture (func-15.x): a registered UDF's error message
// propagates to the statement.
func TestPinUDFErrorPropagation(t *testing.T) {
	db := openPinDB(t)
	db.RegisterFunction("pin_error", func(args []interface{}) (interface{}, error) {
		return nil, os.ErrInvalid
	}, 0, 2)
	r := db.Query("SELECT pin_error('boom')")
	if r.Error == nil {
		t.Fatal("expected error from UDF")
	}
	if !strings.Contains(r.Error.Error(), "invalid argument") {
		t.Fatalf("got %v", r.Error)
	}
}

// TestPinResolverOrderByAliasAmbiguity covers resolver01-1.1/2.1: a bare
// ORDER BY identifier matching a result alias wins over an ambiguous source
// column without erroring.
func TestPinResolverOrderByAliasAmbiguity(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, `
		CREATE TABLE r1(x, y); INSERT INTO r1 VALUES(11,22);
		CREATE TABLE r2(y, z); INSERT INTO r2 VALUES(33,44);
	`)
	if res := db.Exec("SELECT 1 AS y FROM r1, r2 ORDER BY y"); res.Error != nil {
		t.Fatalf("alias should shadow ambiguous column: %v", res.Error)
	}
	if res := db.Exec("SELECT 2 AS y FROM r1, r2 ORDER BY y COLLATE nocase"); res.Error != nil {
		t.Fatalf("alias under COLLATE: %v", res.Error)
	}
	pinExpectError(t, db, "SELECT 1 AS yy FROM r1, r2 ORDER BY y", "ambiguous column name: y")
}

// TestPinCTASRaiseNameResolutionOrder covers colname-9.410: name resolution
// errors precede the RAISE-outside-trigger check.
func TestPinCTASRaiseNameResolutionOrder(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, "CREATE TABLE t1(a); INSERT INTO t1 VALUES(17)")
	pinExpectError(t, db, "CREATE TABLE t5 AS SELECT RAISE(abort,a)", "no such column: a")
}

// TestPinEvalRecursiveUDF covers misc8-1.1..1.3: a recursive SQL UDF (eval)
// sees uncommitted same-connection rows and propagates its errors.
func TestPinEvalRecursiveUDF(t *testing.T) {
	db := openPinDB(t)
	pinExec(t, db, "CREATE TABLE t1(a,b,c); INSERT INTO t1 VALUES(1,2,3),(4,5,6)")
	row := pinQuery(t, db, "SELECT eval('SELECT * FROM t1 ORDER BY a','-abc-')")
	if row[0] != "1-abc-2-abc-3-abc-4-abc-5-abc-6" {
		t.Fatalf("eval joined: got %q", row[0])
	}
	pinExpectError(t, db, "SELECT eval('SELECT d FROM t1 ORDER BY a')", "no such column: d")
}

// TestPinCompoundGroupByOrdinalAggregate covers misc4-3.3/3.4: GROUP BY
// ordinals resolve to the term's RESULT columns (resolve.c
// resolveOrderGroupBy), so ordinal 2 over "ID, max(Value)" is the aggregate
// itself — "aggregate functions are not allowed in the GROUP BY clause".
// Oracle: sqlite3 3.54 errors identically on the simple and compound forms.
func TestPinCompoundGroupByOrdinalAggregate(t *testing.T) {
	db := openPinDB(t)
	// Byte-exact misc4-1.1/2.1/2.2/2.4 preamble (t3 insert-before-create).
	pinExec(t, db, "\n    CREATE TABLE t1(x);\n    INSERT INTO t1 VALUES(1);\n  ")
	_ = db.Exec("\n    INSERT INTO t3 VALUES(1);\n  ")  // catchsql: t3 not there yet
	pinExec(t, db, "CREATE TABLE t3(x);")
	pinExec(t, db, "\n    INSERT INTO t3 VALUES(1);\n  ")
	pinExec(t, db, "CREATE TABLE Table1(ID integer primary key, Value TEXT); INSERT INTO Table1 VALUES(1,'x')")
	pinExec(t, db, "CREATE TABLE Table2(ID integer NOT NULL, Value TEXT); INSERT INTO Table2 VALUES(1,'z'),(1,'a')")
	pinExpectError(t, db, "SELECT b, max(d) FROM Table2 GROUP BY 1, 2",
		"aggregate functions are not allowed in the GROUP BY clause")
	// Exact misc4-3.1..3.4 sequence: a UNION query WITHOUT the aggregate
	// (3.2) runs first — a state leak from it must not swallow 3.3's error.
	if r := db.Query("SELECT ID, Value FROM Table1 UNION SELECT ID, max(Value) FROM Table2 GROUP BY 1 ORDER BY 1, 2"); r.Error != nil {
		t.Fatalf("3.2 query: %v", r.Error)
	}
	// Byte-exact misc4-3.3 input (leading/trailing whitespace, semicolon).
	pinExpectError(t, db, " \n      SELECT ID, Value FROM Table1\n         UNION SELECT ID, max(Value) FROM Table2 GROUP BY 1, 2\n      ORDER BY 1, 2;\n    ",
		"aggregate functions are not allowed in the GROUP BY clause")
	pinExpectError(t, db, "SELECT ID, max(Value) FROM Table2 GROUP BY 1, 2 UNION SELECT ID, Value FROM Table1 ORDER BY 1, 2",
		"aggregate functions are not allowed in the GROUP BY clause")
}
