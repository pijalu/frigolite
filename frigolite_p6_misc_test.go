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
