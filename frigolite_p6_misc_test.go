package frigolite

import (
	"strings"
	"testing"
)

// P6 pre-tests: hand-written tests for G6.MISC root causes, written BEFORE
// running the TCL testgen packages. Each test mirrors a specific testgen
// failure (e.g. tkt_8454a207b9) and documents the engine bug it covers.

// TestP6_AlterAddColumnDefault covers tkt_8454a207b9: ALTER TABLE ADD COLUMN
// with a DEFAULT expression must apply the default to pre-existing rows at
// read time, with the column's affinity applied (so typeof() reflects the
// stored/effective value).
func TestP6_AlterAddColumnDefault(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Column with TEXT affinity and numeric default: value is coerced to text.
	if err := db.Exec(`
		CREATE TABLE t1(a);
		INSERT INTO t1 VALUES(1);
		ALTER TABLE t1 ADD COLUMN b TEXT DEFAULT -123.0;
	`).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := flattenQuery(t, db, "SELECT b, typeof(b) FROM t1")
	if got != "-123.0 text" {
		t.Errorf("TEXT DEFAULT -123.0: got [%s], want [-123.0 text]", got)
	}

	// Unary minus on a string literal evaluates to 0, then TEXT affinity -> "0".
	if err := db.Exec("ALTER TABLE t1 ADD COLUMN c TEXT DEFAULT -'hello';").Error; err != nil {
		t.Fatalf("add c: %v", err)
	}
	got = flattenQuery(t, db, "SELECT c, typeof(c) FROM t1")
	if got != "0 text" {
		t.Errorf("TEXT DEFAULT -'hello': got [%s], want [0 text]", got)
	}

	// No declared type: no affinity, value stays REAL.
	if err := db.Exec("ALTER TABLE t1 ADD COLUMN e DEFAULT -123.0;").Error; err != nil {
		t.Fatalf("add e: %v", err)
	}
	got = flattenQuery(t, db, "SELECT e, typeof(e) FROM t1")
	if got != "-123.0 real" {
		t.Errorf("DEFAULT -123.0: got [%s], want [-123.0 real]", got)
	}

	// A row inserted AFTER the ADD COLUMN with an explicit value must keep it.
	if err := db.Exec("INSERT INTO t1(a,b,c,e) VALUES(2,'x','y',3.5);").Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	got = flattenQuery(t, db, "SELECT b, typeof(b), e, typeof(e) FROM t1 WHERE a=2")
	if got != "x text 3.5 real" {
		t.Errorf("post-add row: got [%s], want [x text 3.5 real]", got)
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

// queryError runs a query and returns its error (nil when the query
// succeeds). Used to assert that invalid SQL is rejected.
func queryError(db *DB, sql string) error {
	return db.Query(sql).Error
}

func flattenQuery(t *testing.T, db *DB, sql string) string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query error: %v\n  sql: %s", r.Error, sql)
	}
	var parts []string
	for _, row := range r.Rows {
		for _, v := range row {
			if v == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, formatSQLiteValue(v))
			}
		}
	}
	return strings.Join(parts, " ")
}

// TestP6_BitwiseOperators covers the lexer/parser/evaluator support for the
// bitwise operators |, <<, >> (randexpr package failed to parse "|" before).
func TestP6_BitwiseOperators(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT 5|3", "7"},
		{"SELECT 5&3", "1"},
		{"SELECT 1<<4", "16"},
		{"SELECT 256>>4", "16"},
		{"SELECT 6|3", "7"},
		{"SELECT 1|2|4", "7"},
		{"SELECT 8<<1", "16"},
		{"SELECT 16>>2", "4"},
		{"SELECT ~0", "-1"},
	}
	for _, c := range cases {
		got := flattenQuery(t, db, c.sql)
		if got != c.want {
			t.Errorf("%s: got [%s], want [%s]", c.sql, got, c.want)
		}
	}

	// Column values in bitwise expressions.
	if err := db.Exec("CREATE TABLE t1(a,b); INSERT INTO t1 VALUES(6,3);").Error; err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := flattenQuery(t, db, "SELECT a|b, a&b, a<<1, a>>1 FROM t1"); got != "7 2 12 3" {
		t.Errorf("column bitwise: got [%s], want [7 2 12 3]", got)
	}
}

// TestP6_ChangesRecursiveCTE covers the changes() function after DML and
// recursive CTEs beyond SQLite's 1000 default recursion limit (changes.test
// uses up to 50000 rows).
func TestP6_ChangesRecursiveCTE(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec("PRAGMA journal_mode=off;").Error; err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	r := db.Query("PRAGMA journal_mode=off")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Errorf("PRAGMA journal_mode=off: rows=%v err=%v", r.Rows, r.Error)
	} else if got := flattenQuery(t, db, "PRAGMA journal_mode=off"); got != "off" {
		t.Errorf("journal_mode=off: got [%s], want [off]", got)
	}

	if err := db.Exec("CREATE TABLE t1(x INTEGER PRIMARY KEY);").Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Exec("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 5000) INSERT INTO t1 SELECT i FROM s;").Error; err != nil {
		t.Fatalf("recursive CTE insert: %v", err)
	}
	if got := flattenQuery(t, db, "SELECT count(*) FROM t1"); got != "5000" {
		t.Errorf("recursive CTE count: got [%s], want [5000]", got)
	}
	// changes() after the insert reports the number of inserted rows.
	if got := flattenQuery(t, db, "SELECT changes()"); got != "5000" {
		t.Errorf("changes(): got [%s], want [5000]", got)
	}
}

// TestP6_TextAffinityFloatCompare covers formatNumeric: comparing a TEXT
// column value with a whole-number REAL literal must use the REAL's full
// text form ("2.0"), so '2' != 2.0 but '2.0' == 2.0 (indexA tests).
func TestP6_TextAffinityFloatCompare(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE x1(a TEXT, b, c);
		INSERT INTO x1 VALUES('2', 'two', 'ii');
		INSERT INTO x1 VALUES('2.0', 'twopointoh', 'ii.0');
	`).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}
	// a=2.0 with TEXT affinity: '2.0' matches, '2' does not.
	if got := flattenQuery(t, db, "SELECT *, typeof(a) FROM x1 WHERE a=2.0"); got != "2.0 twopointoh ii.0 text" {
		t.Errorf("a=2.0: got [%s], want [2.0 twopointoh ii.0 text]", got)
	}
	// a=2: TEXT '2' matches (TEXT affinity converts 2 to '2').
	if got := flattenQuery(t, db, "SELECT *, typeof(a) FROM x1 WHERE a=2"); got != "2 two ii text" {
		t.Errorf("a=2: got [%s], want [2 two ii text]", got)
	}
	// a='2.0' string literal matches the second row.
	if got := flattenQuery(t, db, "SELECT *, typeof(a) FROM x1 WHERE a='2.0'"); got != "2.0 twopointoh ii.0 text" {
		t.Errorf("a='2.0': got [%s], want [2.0 twopointoh ii.0 text]", got)
	}
}

// TestP6_AggregateInDefault covers the table package: aggregate functions
// are rejected in DEFAULT expressions with "unknown function: <name>()".
func TestP6_AggregateInDefault(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, ddl := range []string{
		"CREATE TABLE t1(x DEFAULT(avg(1)))",
		"CREATE TABLE t2(x DEFAULT(max(1)))",
		"CREATE TABLE t3(x DEFAULT(count(*)))",
	} {
		r := db.Exec(ddl)
		if r.Error == nil || !strings.Contains(r.Error.Error(), "unknown function:") {
			t.Errorf("%s: expected 'unknown function' error, got: %v", ddl, r.Error)
		}
	}
	// Non-aggregate defaults still work.
	if err := db.Exec("CREATE TABLE t4(x DEFAULT 5)").Error; err != nil {
		t.Errorf("normal default: %v", err)
	}
}

// TestP6_RowidInTableConstraint covers unique2: rowid may not be used in
// table-level UNIQUE or PRIMARY KEY constraints ("no such column: rowid").
func TestP6_RowidInTableConstraint(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, ddl := range []string{
		"CREATE TABLE err1(a,b,c,UNIQUE(rowid))",
		"CREATE TABLE err2(a,b,c,PRIMARY KEY(rowid))",
		"CREATE TABLE err3(a,b,c,UNIQUE(_rowid_))",
	} {
		r := db.Exec(ddl)
		if r.Error == nil || !strings.Contains(r.Error.Error(), "no such column:") {
			t.Errorf("%s: expected 'no such column:' error, got: %v", ddl, r.Error)
		}
	}
	// Valid table-level unique constraints still work.
	if err := db.Exec("CREATE TABLE ok1(a,b,c,UNIQUE(a,b))").Error; err != nil {
		t.Errorf("valid unique: %v", err)
	}
}

// TestP6_VacuumReindex covers the parser gap for VACUUM, REINDEX with a
// [schema.]name, and DETACH. The LALR grammar productions exist (rules 249
// `cmd ::= VACUUM into_opt`, 285 `cmd ::= DETACH database_kw_opt nm`, 289
// `cmd ::= REINDEX nm dbnm`) but had no handleRule case, so the generic
// passthrough dropped the statement ("no statements parsed"). Mirrors testgen
// reindex (REINDEX t1/i1/main.t1/main.i1, multi-statement REINDEX+SELECT),
// tkt_c48d99d (bare VACUUM), and exclusive (DETACH aux;).
func TestP6_VacuumReindex(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec("CREATE TABLE t1(a); CREATE INDEX i1 ON t1(a);").Error; err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Bare VACUUM — tkt_c48d99d.
	if err := db.Exec("VACUUM").Error; err != nil {
		t.Errorf("VACUUM: %v", err)
	}

	// VACUUM INTO <file> parses (no-op execution).
	if err := db.Exec("VACUUM INTO 'vacuum_out.db'").Error; err != nil {
		t.Errorf("VACUUM INTO: %v", err)
	}

	// REINDEX bare and with [schema.]name — reindex-1.x.
	for _, sql := range []string{
		"REINDEX;",
		"REINDEX t1;",
		"REINDEX i1;",
		"REINDEX main.t1;",
		"REINDEX main.i1;",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}

	// Multi-statement input: REINDEX followed by SELECT must preserve both
	// statements in order (reindex-2.6 style).
	if err := db.Exec("REINDEX i1; SELECT a FROM t1;").Error; err != nil {
		t.Errorf("REINDEX + SELECT: %v", err)
	}
	got := flattenQuery(t, db, "SELECT a FROM t1")
	if got != "" {
		t.Errorf("unexpected rows after REINDEX+SELECT: %q", got)
	}

	// DETACH with and without the DATABASE keyword parses and executes
	// (exclusive). ATTACH first so DETACH has a real database to detach.
	if err := db.Exec("ATTACH ':memory:' AS aux;").Error; err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := db.Exec("DETACH aux;").Error; err != nil {
		t.Errorf("DETACH aux: %v", err)
	}
	if err := db.Exec("ATTACH ':memory:' AS aux2;").Error; err != nil {
		t.Fatalf("attach aux2: %v", err)
	}
	if err := db.Exec("DETACH DATABASE aux2;").Error; err != nil {
		t.Errorf("DETACH DATABASE aux2: %v", err)
	}
}

// TestP6_RowValueComparison covers row-value (a,b,c) semantics: lexicographic
// comparisons, IN with row values (arity checks and subquery rows), IS
// NULL-safe row equality, and row-value-vs-subquery comparisons. Mirrors
// testgen rowvalueA (which PASSES) and the bulk of rowvalue (reduced from
// ~3900 failures to edge cases after this fix).
func TestP6_RowValueComparison(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Lexicographic row-value comparison.
	got := flattenQuery(t, db, "SELECT (1, 2) < (2, 0)")
	if got != "1" {
		t.Errorf("(1,2) < (2,0): got %q want 1", got)
	}
	got = flattenQuery(t, db, "SELECT (1, 2) < (1, 1)")
	if got != "0" {
		t.Errorf("(1,2) < (1,1): got %q want 0", got)
	}
	got = flattenQuery(t, db, "SELECT (1, 2) = (1, 2)")
	if got != "1" {
		t.Errorf("(1,2) = (1,2): got %q want 1", got)
	}

	// Row-value IN with arity errors.
	r := db.Query("SELECT (1, 2) IN ( (1, 2), (3, 4, 5) )")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "IN(...) element has 3 terms - expected 2") {
		t.Errorf("IN arity: expected error, got %v", r.Error)
	}
	r = db.Query("SELECT 2 IN ( (1, 2), (3, 4) )")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "row value misused") {
		t.Errorf("scalar IN row: expected 'row value misused', got %v", r.Error)
	}

	// Row-value IN subquery (2-column table).
	if err := db.Exec("CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1, 2); INSERT INTO t1 VALUES(3, 4);").Error; err != nil {
		t.Fatal(err)
	}
	got = flattenQuery(t, db, "SELECT (1, 2) IN (SELECT a, b FROM t1)")
	if got != "1" {
		t.Errorf("(1,2) IN (SELECT a,b): got %q want 1", got)
	}

	// Row-value vs subquery comparison.
	got = flattenQuery(t, db, "SELECT (3, 4) = (SELECT a, b FROM t1 WHERE a = 3)")
	if got != "1" {
		t.Errorf("(3,4) = (SELECT a,b): got %q want 1", got)
	}

	// Row-value IS: NULL-safe equality.
	got = flattenQuery(t, db, "SELECT (1, NULL) IS (1, NULL)")
	if got != "1" {
		t.Errorf("(1,NULL) IS (1,NULL): got %q want 1", got)
	}

	// Column collation applies to row-value elements only from a column on
	// the left; a column on the right does not force its collation
	// (SQLite datatype3 collation resolution).
	if err := db.Exec("CREATE TABLE x2(y); INSERT INTO x2 VALUES('abc'); CREATE TABLE x1(b PRIMARY KEY COLLATE NOCASE) WITHOUT ROWID; INSERT INTO x1 VALUES('ABCD');").Error; err != nil {
		t.Fatal(err)
	}
	got = flattenQuery(t, db, "SELECT * FROM x2 CROSS JOIN x1 WHERE (1234, x2.y) > (1234, x1.b)")
	if got != "abc ABCD" {
		t.Errorf("cross-join row-value comparison: got %q want [abc ABCD]", got)
	}

	// Bare row value in a SELECT list is misuse.
	r = db.Query("SELECT (1, 2, 3)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "row value misused") {
		t.Errorf("bare SELECT (1,2,3): expected 'row value misused', got %v", r.Error)
	}
}

// TestP6_IsTrueNullSemantics covers istrue: IS TRUE / IS FALSE / IS NOT
// TRUE / IS NOT FALSE with NULL operands must match SQLite (NULL is neither
// true nor false: IS X -> 0, IS NOT X -> 1). Also covers CHECK constraints
// over NULL columns, where the row value wraps as ColumnValue{Value:nil}.
func TestP6_IsTrueNullSemantics(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Direct expression semantics (SQLite: NULL IS TRUE=0, NULL IS NOT
	// TRUE=1, NULL IS FALSE=0, NULL IS NOT FALSE=1).
	got := flattenQuery(t, db,
		"SELECT NULL IS TRUE, NULL IS NOT TRUE, NULL IS FALSE, NULL IS NOT FALSE")
	if got != "0 1 0 1" {
		t.Errorf("NULL IS TRUE/FALSE semantics: got [%s] want [0 1 0 1]", got)
	}

	// CHECK constraints must treat NULL as passing for IS NOT TRUE / IS NOT
	// FALSE (the column value is wrapped as ColumnValue{Value:nil}).
	if err := db.Exec(`
		CREATE TABLE t2(
			a INTEGER PRIMARY KEY,
			b BOOLEAN CHECK(b IS TRUE),
			c BOOLEAN CHECK(c IS FALSE),
			d BOOLEAN CHECK(d IS NOT TRUE),
			e BOOLEAN CHECK(e IS NOT FALSE)
		);
		INSERT INTO t2 VALUES(1,true,false,null,null);
	`).Error; err != nil {
		t.Fatalf("insert with NULL IS NOT TRUE/FALSE checks: %v", err)
	}
	got = flattenQuery(t, db, "SELECT a,b,c,d,e FROM t2")
	if got != "1 1 0 NULL NULL" {
		t.Errorf("t2 row: got [%s] want [1 1 0 NULL NULL]", got)
	}

	// NULL column in Kleene AND/OR: NULL AND false = false, NULL AND true =
	// NULL, NULL OR true = true, NULL OR false = NULL.
	if err := db.Exec("CREATE TABLE t3(x INT); INSERT INTO t3 VALUES(NULL),(1),(0);").Error; err != nil {
		t.Fatal(err)
	}
	got = flattenQuery(t, db, "SELECT x AND 0, x AND 1, x OR 1, x OR 0 FROM t3 ORDER BY x")
	// Rows ordered NULL, 0, 1 (SQLite sorts NULLs first ascending).
	// NULL: NULL AND 0=0, NULL AND 1=NULL, NULL OR 1=1, NULL OR 0=NULL
	// 0:    0 AND 0=0,  0 AND 1=0,   0 OR 1=1,  0 OR 0=0
	// 1:    1 AND 0=0,  1 AND 1=1,   1 OR 1=1,  1 OR 0=1
	if got != "0 NULL 1 NULL 0 0 1 0 0 1 1 1" {
		t.Errorf("Kleene AND/OR with NULL column: got [%s]", got)
	}
}
