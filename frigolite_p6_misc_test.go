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

// TestP6_CtasExpressionColumn covers distinct: CREATE TABLE ... AS SELECT of
// an unaliased expression must derive a real column (SQLite names it after
// the expression text), so SELECT * and ORDER BY 1 work on the new table.
func TestP6_CtasExpressionColumn(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t5(a INT, b INT);
		CREATE UNIQUE INDEX t5x ON t5(a+b);
		INSERT INTO t5(a,b) VALUES(0,0),(1,0),(1,1),(0,3);
		CREATE TEMP TABLE out AS SELECT DISTINCT a+b FROM t5;
	`).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := flattenQuery(t, db, "SELECT * FROM out ORDER BY 1")
	if got != "0 1 2 3" {
		t.Errorf("CTAS expression column: got [%s] want [0 1 2 3]", got)
	}
	got = flattenQuery(t, db, "SELECT count(*) FROM out")
	if got != "4" {
		t.Errorf("CTAS row count: got [%s] want [4]", got)
	}
	// The derived column must be queryable by name (SQLite names it after
	// the expression text; the engine uses its normalized form).
	r := db.Query("PRAGMA table_info(out)")
	if r.Error != nil {
		t.Fatalf("table_info: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Errorf("CTAS table should have exactly 1 column, got %d", len(r.Rows))
	}
}

// TestP6_INAffinityCollation covers in4/in5: SQLite's scalar IN-list
// comparison uses the LHS operand's affinity and collation, and IN (subquery)
// uses the merged affinity with explicit-COLLATE override.
func TestP6_INAffinityCollation(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Column collation on the LHS applies to IN: d IN ('FUZZ') matches 'fuzz'
	// (d is COLLATE NOCASE), while 'FUZZ' IN (d) is BINARY (LHS literal).
	if err := db.Exec(`
		CREATE TABLE t5(c INTEGER PRIMARY KEY, d TEXT COLLATE nocase);
		INSERT INTO t5 VALUES(17, 'fuzz');
	`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, "SELECT 1 FROM t5 WHERE 'fuzz' IN (d) INTERSECT SELECT 1 FROM t5 WHERE d IN ('fuzz') INTERSECT SELECT 1 FROM t5 WHERE d IN ('FUZZ')")
	if got != "1" {
		t.Errorf("IN with LHS collation: got [%s] want [1]", got)
	}
	if r := db.Query("SELECT 1 FROM t5 WHERE 'FUZZ' IN (d)"); r.Error != nil || len(r.Rows) != 0 {
		t.Errorf("'FUZZ' IN (d) should not match (BINARY LHS): rows=%v err=%v", r.Rows, r.Error)
	}

	// LHS affinity applies to IN: b NUMERIC IN (a TEXT '1.0') matches.
	if err := db.Exec(`
		CREATE TABLE t4b(a TEXT, b NUMERIC, c);
		INSERT INTO t4b VALUES('1.0',1,4);
	`).Error; err != nil {
		t.Fatal(err)
	}
	got = flattenQuery(t, db, "SELECT c FROM t4b WHERE b IN (a)")
	if got != "4" {
		t.Errorf("IN with LHS affinity: got [%s] want [4]", got)
	}
	// A literal LHS has no affinity: 0 IN ('0') is false.
	if r := db.Query("SELECT 1 WHERE 0 IN ('0')"); r.Error != nil || len(r.Rows) != 0 {
		t.Errorf("0 IN ('0') should not match: rows=%v err=%v", r.Rows, r.Error)
	}

	// Explicit COLLATE overrides in IN (subquery): a COLLATE BINARY IN
	// (SELECT DISTINCT a) is case-sensitive.
	if err := db.Exec(`
		CREATE TABLE t1(a COLLATE nocase);
		INSERT INTO t1 VALUES('one');
		INSERT INTO t1 VALUES('ONE');
	`).Error; err != nil {
		t.Fatal(err)
	}
	got = flattenQuery(t, db, "SELECT count(*) FROM t1 WHERE a COLLATE BINARY IN (SELECT DISTINCT a FROM t1)")
	if got != "1" {
		t.Errorf("COLLATE BINARY IN (subquery): got [%s] want [1]", got)
	}
}

// TestP6_JoinAliasShadowing covers in-15.6: a JOIN alias shadows a same-named
// real table for qualified-star expansion (SELECT a.* FROM t5 AS a must
// resolve to t5's columns even when a table named a exists).
func TestP6_JoinAliasShadowing(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE a(id INTEGER);
		INSERT INTO a VALUES(1);
		CREATE TABLE t5(id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO t5 VALUES(1,'Alice'),(2,'Emma');
	`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, "SELECT a.* FROM t5 AS 'a' JOIN t5 AS 'b' ON b.id=a.id")
	if got != "1 Alice 2 Emma" {
		t.Errorf("alias-shadowed a.*: got [%s] want [1 Alice 2 Emma]", got)
	}
	// Plain SELECT * on the same join still shows both sides.
	got = flattenQuery(t, db, "SELECT * FROM t5 AS 'a' JOIN t5 AS 'b' ON b.id=a.id")
	if got != "1 Alice 1 Alice 2 Emma 2 Emma" {
		t.Errorf("join SELECT *: got [%s]", got)
	}
}

// TestP6_CTEWithValues covers values: WITH VVV AS (VALUES(...)) references in
// compound queries and JOIN ON clauses (the CTE body exposes column1..columnN).
func TestP6_CTEWithValues(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Compound query referencing the VALUES CTE twice.
	got := flattenQuery(t, db, `WITH VVV AS (VALUES('a','b'),('c','d'),(123,NULL)) SELECT * FROM VVV UNION ALL SELECT * FROM VVV`)
	if got != "a b c d 123 NULL a b c d 123 NULL" {
		t.Errorf("CTE VALUES union: got [%s]", got)
	}
	// INTERSECT.
	got = flattenQuery(t, db, `WITH VVV AS (VALUES('a','b'),('c','d'),(123,NULL)) SELECT * FROM VVV INTERSECT SELECT * FROM VVV`)
	if got != "123 NULL a b c d" {
		t.Errorf("CTE VALUES intersect: got [%s]", got)
	}
	// LEFT JOIN with ON referencing the CTE's value column and a table column.
	if err := db.Exec(`CREATE TABLE t1(x); INSERT INTO t1 VALUES('a'),('z');`).Error; err != nil {
		t.Fatal(err)
	}
	got = flattenQuery(t, db, `WITH VVV AS (VALUES('a','b'),('c','d'),(123,NULL)) SELECT * FROM t1 LEFT JOIN VVV ON (column1=x)`)
	if got != "a a b z NULL NULL" {
		t.Errorf("CTE VALUES join: got [%s]", got)
	}
}

// TestP6_CTEInDML covers with: WITH clauses prefixing DML statements are
// scoped to that statement (DELETE/UPDATE subqueries can reference the CTE).
func TestP6_CTEInDML(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`CREATE TABLE t1(x); INSERT INTO t1 VALUES(1),(2),(3),(4);`).Error; err != nil {
		t.Fatal(err)
	}
	// DELETE with CTE filter.
	if err := db.Exec(`WITH dset AS (SELECT 2 UNION ALL SELECT 4) DELETE FROM t1 WHERE x IN dset;`).Error; err != nil {
		t.Fatalf("DELETE with CTE: %v", err)
	}
	got := flattenQuery(t, db, "SELECT * FROM t1")
	if got != "1 3" {
		t.Errorf("DELETE with CTE: got [%s] want [1 3]", got)
	}
	// UPDATE with CTE subquery (runs on the remaining rows [1,3]; the
	// subquery only matches x=2/4 which were deleted, so no row changes).
	if err := db.Exec(`WITH uset(a, b) AS (SELECT 2, 8 UNION ALL SELECT 4, 9) UPDATE t1 SET x = COALESCE((SELECT b FROM uset WHERE a=x), x);`).Error; err != nil {
		t.Fatalf("UPDATE with CTE: %v", err)
	}
	got = flattenQuery(t, db, "SELECT * FROM t1")
	if got != "1 3" {
		t.Errorf("UPDATE with CTE: got [%s] want [1 3]", got)
	}

	// A fresh table demonstrates the UPDATE subquery actually matching.
	if err := db.Exec(`DROP TABLE t1; CREATE TABLE t1(x); INSERT INTO t1 VALUES(1),(2),(3),(4);`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`WITH uset(a, b) AS (SELECT 2, 8 UNION ALL SELECT 4, 9) UPDATE t1 SET x = COALESCE((SELECT b FROM uset WHERE a=x), x);`).Error; err != nil {
		t.Fatalf("UPDATE with CTE (full): %v", err)
	}
	got = flattenQuery(t, db, "SELECT * FROM t1")
	if got != "1 8 3 9" {
		t.Errorf("UPDATE with CTE (full): got [%s] want [1 8 3 9]", got)
	}
}

// TestP6_NestedJoinSameName covers selectD: joining two tables with the same
// name (main.t4 JOIN aux1.t4) inside a parenthesized group used as a join
// operand must keep both rows distinct. Synthetic derived-table names (_subqN)
// were derived from the current column count, colliding across nesting levels
// and overwriting one operand's row in the combined map.
func TestP6_NestedJoinSameName(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		ATTACH ':memory:' AS aux1;
		CREATE TABLE t1(a,b); INSERT INTO t1 VALUES(111,'x1');
		CREATE TABLE t2(a,b); INSERT INTO t2 VALUES(222,'x2');
		CREATE TABLE main.t4(a,b); INSERT INTO main.t4 VALUES(444,'x4');
		CREATE TABLE aux1.t4(a,b); INSERT INTO aux1.t4 VALUES(555,'x5');
	`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, `SELECT * FROM t1 JOIN (t2 JOIN (main.t4 JOIN aux1.t4 ON aux1.t4.a=main.t4.a+111) ON main.t4.a=t2.a+222) ON t2.a=t1.a+111`)
	if got != "111 x1 222 x2 444 x4 555 x5" {
		t.Errorf("nested join same-name tables: got [%s] want [111 x1 222 x2 444 x4 555 x5]", got)
	}
	got = flattenQuery(t, db, `SELECT * FROM t1 JOIN (t2 JOIN (main.t4 AS x JOIN aux1.t4 ON aux1.t4.a=x.a+111) ON x.a=t2.a+222) ON t2.a=t1.a+111`)
	if got != "111 x1 222 x2 444 x4 555 x5" {
		t.Errorf("nested join aliased: got [%s]", got)
	}
}

// TestP6_CompoundOrderByStar covers select1: a compound subquery member that
// projects * makes the underlying table's columns available to ORDER BY, and
// SELECT-list aliases resolve in join ON clauses.
func TestP6_CompoundOrderByStar(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t1(a,b);
		CREATE TABLE t2(x,y,z);
		INSERT INTO t1 VALUES(1,2),(3,4);
		INSERT INTO t2 VALUES(1,2,3),(4,5,6),(7,8,9);
	`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, `SELECT * FROM t1,(SELECT * FROM t2 WHERE y=2 UNION ALL SELECT * FROM t2 WHERE y=3 ORDER BY y,z LIMIT 4)`)
	if got != "1 2 1 2 3 3 4 1 2 3" {
		t.Errorf("compound subquery ORDER BY *: got [%s]", got)
	}

	// SELECT alias resolves in ON.
	if err := db.Exec(`CREATE TABLE t3(a INTEGER PRIMARY KEY, R); INSERT INTO t3 VALUES(1,9);`).Error; err != nil {
		t.Fatal(err)
	}
	r := db.Query(`SELECT a,(+a)b FROM t3 LEFT JOIN (SELECT 1 AS z, 2 AS y) v ON z=b`)
	if r.Error != nil {
		t.Errorf("ON referencing SELECT alias: %v", r.Error)
	}
}

// TestP6_DerivedTableShadowsInner covers select6: a derived table's output
// columns shadow its inner tables for ambiguity validation (SELECT q FROM
// (SELECT t3.q AS q, ... FROM t3 NATURAL JOIN t4) n is not ambiguous even
// though inner t3 and t4 both have q).
func TestP6_DerivedTableShadowsInner(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t1(x,y);
		CREATE TABLE t2(a,b);
		CREATE TABLE t3(p,q);
		CREATE TABLE t4(q,r);
		INSERT INTO t1 VALUES(1,10),(2,20);
		INSERT INTO t2 VALUES(1,100),(2,200);
		INSERT INTO t3 VALUES(5,50),(6,60);
		INSERT INTO t4 VALUES(50,500),(60,600);
	`).Error; err != nil {
		t.Fatal(err)
	}
	r := db.Query(`SELECT y, p, q, r FROM (SELECT t1.y AS y, t2.b AS b FROM t1, t2 WHERE t1.x=t2.a) AS m, (SELECT t3.p AS p, t3.q AS q, t4.r AS r FROM t3 NATURAL JOIN t4) as n WHERE y=p`)
	if r.Error != nil {
		t.Errorf("derived-table ambiguity: %v", r.Error)
	}
}

// TestP6_JoinOnBothLeftCols covers whereG: an ON clause equating two LEFT
// table columns (SELECT * FROM t3 JOIN t2 ON x=y where t2 has no y) must not
// trigger the autoindex (which would build an empty index on t2.y and
// short-circuit the join to zero rows).
func TestP6_JoinOnBothLeftCols(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t2(z);
		INSERT INTO t2 VALUES('t2');
		CREATE TABLE t3(x PRIMARY KEY, y);
		INSERT INTO t3 VALUES('AAA','AAA');
	`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, `SELECT * FROM t3 JOIN t2 ON x=y`)
	if got != "AAA AAA t2" {
		t.Errorf("join ON both-left columns: got [%s] want [AAA AAA t2]", got)
	}
	got = flattenQuery(t, db, `SELECT * FROM t3 JOIN t2 ON x=y AND y='AAA'`)
	if got != "AAA AAA t2" {
		t.Errorf("join ON both-left + literal: got [%s]", got)
	}
}

// TestP6_IntFloatBoundary covers where-27.2: comparing int64 MaxInt64 against
// the REAL 2^63 (from 9223372036854775807+1) must be false — 2^63 is one more
// than MaxInt64, unlike the -2^63 == MinInt64 boundary which is equal.
func TestP6_IntFloatBoundary(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`CREATE TABLE t1(a INTEGER PRIMARY KEY); INSERT INTO t1(a) VALUES(9223372036854775807);`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, `SELECT a>=9223372036854775807+1 FROM t1`)
	if got != "0" {
		t.Errorf("MaxInt64 >= 2^63: got [%s] want [0]", got)
	}
	got = flattenQuery(t, db, `SELECT a FROM t1 WHERE a>=9223372036854775807+1`)
	if got != "" {
		t.Errorf("WHERE MaxInt64 >= 2^63: got [%s] want []", got)
	}
	// MinInt64 == -2^63 is equal.
	got = flattenQuery(t, db, `SELECT -9223372036854775808 = -9223372036854775808.0`)
	if got != "1" {
		t.Errorf("MinInt64 == -2^63: got [%s] want [1]", got)
	}
}

// TestP6_DMLReturningOrderLimit covers wherelimit: DELETE/UPDATE with
// RETURNING + ORDER BY + LIMIT (a SQLite extension the LALR grammar lacks),
// WITH-prefixed DML with LIMIT, and FTS explicit rowids.
func TestP6_DMLReturningOrderLimit(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t1(x int, y int);
		INSERT INTO t1 VALUES(1,1),(2,2),(3,3);
	`).Error; err != nil {
		t.Fatal(err)
	}
	// DELETE ... RETURNING ... ORDER BY ... LIMIT.
	r := db.Query("DELETE FROM t1 RETURNING x, y, '|' ORDER BY x, y LIMIT 2")
	if r.Error != nil {
		t.Fatalf("DELETE RETURNING ORDER LIMIT: %v", r.Error)
	}
	// The remaining row must be the third (x=3).
	got := flattenQuery(t, db, "SELECT x FROM t1")
	if got != "3" {
		t.Errorf("DELETE RETURNING LIMIT left: got [%s] want [3]", got)
	}

	// WITH-prefixed UPDATE with LIMIT (CTE name shadows the table for SELECT
	// but the DML target resolves to the real table).
	if err := db.Exec(`CREATE TABLE t2(a INT); INSERT INTO t2(a) VALUES(0),(1),(2);`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`WITH t2(b) AS (SELECT * FROM (SELECT * FROM (VALUES(5)))) UPDATE t2 SET a=9 LIMIT 1;`).Error; err != nil {
		t.Fatalf("WITH UPDATE LIMIT: %v", err)
	}
	got = flattenQuery(t, db, "SELECT a FROM t2 ORDER BY a")
	if got != "1 2 9" {
		t.Errorf("WITH UPDATE LIMIT: got [%s] want [1 2 9]", got)
	}

	// FTS explicit rowids.
	if err := db.Exec(`CREATE VIRTUAL TABLE ft USING fts5(x); INSERT INTO ft(rowid, x) VALUES(-45, 'a a'),(12,'a b');`).Error; err != nil {
		t.Fatalf("FTS insert: %v", err)
	}
	got = flattenQuery(t, db, "SELECT rowid FROM ft ORDER BY rowid")
	if got != "-45 12" {
		t.Errorf("FTS explicit rowids: got [%s] want [-45 12]", got)
	}
}

// TestP6_Int64PrecisionCompare covers numindex: comparing two INTEGER values
// must use int64 arithmetic, not float64 (which loses precision above 2^53).
// 288230376151711744 == 288230376151711745 must be false.
func TestP6_Int64PrecisionCompare(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	got := flattenQuery(t, db, `SELECT 288230376151711744 = 288230376151711745`)
	if got != "0" {
		t.Errorf("int64 precision =: got [%s] want [0]", got)
	}
	got = flattenQuery(t, db, `SELECT 288230376151711744 < 288230376151711745`)
	if got != "1" {
		t.Errorf("int64 precision <: got [%s] want [1]", got)
	}
	// The numindex CASE expression over stored large ints.
	if err := db.Exec(`
		CREATE TABLE t2(a, b);
		INSERT INTO t2 VALUES('b', 288230376151711744);
		INSERT INTO t2 VALUES('c', 2.88230376151712e+17);
		INSERT INTO t2 VALUES('d', 288230376151711745);
	`).Error; err != nil {
		t.Fatal(err)
	}
	got = flattenQuery(t, db, `SELECT x.a || CASE WHEN x.b==y.b THEN '==' ELSE '<>' END || y.a FROM t2 AS x, t2 AS y ORDER BY +x.a, +x.b`)
	if got != "b==b b<>c b<>d c<>b c==c c<>d d<>b d<>c d==d" {
		t.Errorf("numindex CASE: got [%s]", got)
	}
}

// TestP6_RowValueCaseAndParenSet covers rowvalue7/8: row-value CASE
// expressions (CASE (a,b) WHEN (1,2) THEN ...) and UPDATE SET (c,d) =
// (subquery) row-value assignment.
func TestP6_RowValueCaseAndParenSet(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t1(a INTEGER PRIMARY KEY,b,c,d);
		INSERT INTO t1(a,b,c,d) VALUES(1,1,2,3),(2,2,3,4),(3,1,2,4),(4,2,3,5),(5,3,4,6),(6,4,5,9);
	`).Error; err != nil {
		t.Fatal(err)
	}
	// Row-value CASE.
	got := flattenQuery(t, db, `SELECT a, CASE (b,c) WHEN (1,2) THEN 'aleph' WHEN (2,3) THEN 'bet' WHEN (3,4) THEN 'gimel' ELSE '-' END FROM t1 ORDER BY a`)
	if got != "1 aleph 2 bet 3 aleph 4 bet 5 gimel 6 -" {
		t.Errorf("row-value CASE: got [%s]", got)
	}
	// Row-value CASE with subquery operand.
	got = flattenQuery(t, db, `SELECT a, CASE (SELECT b,c FROM t1 WHERE a=1) WHEN (1,2) THEN 'y' ELSE 'n' END FROM t1 WHERE a=1`)
	if got != "1 y" {
		t.Errorf("row-value CASE subquery: got [%s]", got)
	}
	// UPDATE SET (c,d) = (subquery).
	if err := db.Exec(`UPDATE t1 SET (c,d) = (SELECT 11,22 WHERE a=1) WHERE a=1`).Error; err != nil {
		t.Fatalf("paren-set update: %v", err)
	}
	got = flattenQuery(t, db, "SELECT c, d FROM t1 WHERE a=1")
	if got != "11 22" {
		t.Errorf("paren-set update: got [%s] want [11 22]", got)
	}
	// Arity mismatch.
	if err := db.Exec(`UPDATE t1 SET (c,d) = (SELECT 1,2,3) WHERE a=1`).Error; err == nil || !strings.Contains(err.Error(), "2 columns assigned 3 values") {
		t.Errorf("paren-set arity: got %v", err)
	}
}
