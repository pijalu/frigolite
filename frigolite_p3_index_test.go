package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// P3 pre-tests: hand-written tests for G3.INDEX index support, written
// BEFORE running the TCL testgen packages (index, indexedby, indexexpr,
// unique). Each expectation was verified against sqlite3 3.51 CLI as the
// oracle; the tests document the exact SQLite semantics frigolite must
// match: CREATE INDEX (single/multi-column, ASC/DESC, COLLATE), UNIQUE
// enforcement with the exact error text, expression and partial indexes,
// DROP INDEX with IF EXISTS/IF NOT EXISTS, autoindex lifecycle for
// PK/UNIQUE constraints, index maintenance on UPDATE, and result
// equivalence with and without an index.

// TestP3Index is the top-level entry for the P3 INDEX pre-tests. The verify
// command runs it via `go test -run TestP3Index -count=1 .`
func TestP3Index(t *testing.T) {
	for _, sub := range []string{
		"CreateBasic", "CreateOptions", "UniqueEnforce", "UniqueError",
		"ExprIndex", "PartialIndex", "DropIndex", "Autoindex",
		"UpdateMaintenance", "QueryEquivalence", "WithoutRowid", "StoredSQL",
	} {
		ok := t.Run(sub, func(t *testing.T) {
			switch sub {
			case "CreateBasic":
				TestP3Index_CreateBasic(t)
			case "CreateOptions":
				TestP3Index_CreateOptions(t)
			case "UniqueEnforce":
				TestP3Index_UniqueEnforce(t)
			case "UniqueError":
				TestP3Index_UniqueError(t)
			case "ExprIndex":
				TestP3Index_ExprIndex(t)
			case "PartialIndex":
				TestP3Index_PartialIndex(t)
			case "DropIndex":
				TestP3Index_DropIndex(t)
			case "Autoindex":
				TestP3Index_Autoindex(t)
			case "UpdateMaintenance":
				TestP3Index_UpdateMaintenance(t)
			case "QueryEquivalence":
				TestP3Index_QueryEquivalence(t)
			case "WithoutRowid":
				TestP3Index_WithoutRowid(t)
			case "StoredSQL":
				TestP3Index_StoredSQL(t)
			}
		})
		if !ok {
			t.Fail()
		}
	}
}

// must executes SQL and fails the test on error.
func must(t *testing.T, db *DB, sql string) {
	t.Helper()
	r := db.Exec(sql)
	if r.Error != nil {
		t.Fatalf("exec error: %v\n  sql: %s", r.Error, sql)
	}
}

// indexNames returns the index names for a table, sorted.
func indexNames(t *testing.T, db *DB, table string) []string {
	t.Helper()
	r := db.Query("SELECT name FROM sqlite_schema WHERE type='index' AND tbl_name='" + table + "' ORDER BY name")
	if r.Error != nil {
		t.Fatalf("query index names: %v", r.Error)
	}
	var names []string
	for _, row := range r.Rows {
		if row[0] != nil {
			names = append(names, fmt.Sprintf("%v", row[0]))
		}
	}
	return names
}

// expectIndexNames asserts the sorted index-name list matches exactly.
func expectIndexNames(t *testing.T, db *DB, table string, want ...string) {
	t.Helper()
	got := indexNames(t, db, table)
	if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
		t.Errorf("index names for %s: got %v, want %v", table, got, want)
	}
}

// TestP3Index_CreateBasic covers CREATE INDEX on a single column and a
// multi-column index, plus queries that stay correct with the index present.
func TestP3Index_CreateBasic(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int, b int, c text)")
	must(t, db, "CREATE INDEX i1 ON t1(a)")
	must(t, db, "CREATE INDEX i2 ON t1(a, b)")
	expectIndexNames(t, db, "t1", "i1", "i2")

	// Query results are unaffected by the index.
	must(t, db, "INSERT INTO t1 VALUES(1,10,'x'),(2,20,'y'),(3,30,'z')")
	if got := flattenQuery(t, db, "SELECT a, b FROM t1 WHERE a=2"); got != "2 20" {
		t.Errorf("where a=2: got [%s], want [2 20]", got)
	}
	if got := flattenQuery(t, db, "SELECT a FROM t1 ORDER BY a DESC"); got != "3 2 1" {
		t.Errorf("order desc: got [%s], want [3 2 1]", got)
	}
}

// TestP3Index_CreateOptions covers ASC/DESC per column and per-column
// COLLATE, and the stored SQL.
func TestP3Index_CreateOptions(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int, b text, c int)")
	must(t, db, "CREATE INDEX i1 ON t1(a ASC, b DESC)")
	must(t, db, "CREATE INDEX i2 ON t1(c COLLATE binary)")
	expectIndexNames(t, db, "t1", "i1", "i2")

	if sql := tableSQL(t, db, "i1"); !strings.Contains(sql, "DESC") {
		t.Errorf("i1 stored SQL should keep DESC: %q", sql)
	}
}

// TestP3Index_UniqueEnforce covers UNIQUE index uniqueness enforcement on
// INSERT and UPDATE with the exact SQLite error text.
func TestP3Index_UniqueEnforce(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int, b int)")
	must(t, db, "CREATE UNIQUE INDEX u1 ON t1(a)")
	must(t, db, "INSERT INTO t1 VALUES(1, 10), (2, 20)")

	execFails(t, db, "INSERT INTO t1 VALUES(1, 99)", "UNIQUE constraint failed: t1.a")
	execFails(t, db, "INSERT INTO t1 VALUES(3, 99), (1, 100)", "UNIQUE constraint failed: t1.a")

	// Multi-column UNIQUE index.
	must(t, db, "CREATE UNIQUE INDEX u2 ON t1(a, b)")
	must(t, db, "INSERT INTO t1 VALUES(3, 30)")
	execFails(t, db, "INSERT INTO t1 VALUES(3, 30)", "UNIQUE constraint failed: t1.a, t1.b")
}

// TestP3Index_UniqueError covers UNIQUE conflict handling with
// ON CONFLICT IGNORE semantics on the index.
func TestP3Index_UniqueError(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int, b int)")
	must(t, db, "CREATE UNIQUE INDEX u1 ON t1(a)")
	must(t, db, "INSERT INTO t1 VALUES(1, 10)")
	// INSERT OR IGNORE silently skips the conflicting row (no error).
	must(t, db, "INSERT OR IGNORE INTO t1 VALUES(1, 20)")
	if got := flattenQuery(t, db, "SELECT count(*) FROM t1 WHERE a=1"); got != "1" {
		t.Errorf("OR IGNORE kept duplicate: got [%s], want [1]", got)
	}

	// UNIQUE index with NULLs: SQLite allows multiple NULLs.
	must(t, db, "CREATE TABLE t2(a int, b int)")
	must(t, db, "CREATE UNIQUE INDEX u2 ON t2(a)")
	must(t, db, "INSERT INTO t2 VALUES(NULL, 1), (NULL, 2), (1, 3)")
	if got := flattenQuery(t, db, "SELECT count(*) FROM t2"); got != "3" {
		t.Errorf("NULL unique rows: got [%s], want [3]", got)
	}
}

// TestP3Index_ExprIndex covers expression indexes: creation, stored SQL,
// and query equivalence.
func TestP3Index_ExprIndex(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int, b int)")
	must(t, db, "CREATE INDEX e1 ON t1(a+b)")
	expectIndexNames(t, db, "t1", "e1")

	if sql := tableSQL(t, db, "e1"); !strings.Contains(sql, "a+b") {
		t.Errorf("expr index stored SQL should keep expression: %q", sql)
	}

	must(t, db, "INSERT INTO t1 VALUES(1,10),(2,20),(3,30)")
	if got := flattenQuery(t, db, "SELECT a FROM t1 WHERE a+b > 25"); got != "3" {
		t.Errorf("expr index query: got [%s], want [3]", got)
	}
}

// TestP3Index_PartialIndex covers partial indexes: WHERE clause stored
// verbatim, rows outside the predicate excluded from the index, and
// uniqueness only enforced within the partial index.
func TestP3Index_PartialIndex(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(x int, y int)")
	must(t, db, "CREATE INDEX p1 ON t1(x) WHERE x > 0")
	expectIndexNames(t, db, "t1", "p1")

	if sql := tableSQL(t, db, "p1"); !strings.Contains(sql, "WHERE x > 0") {
		t.Errorf("partial index stored SQL should keep WHERE: %q", sql)
	}

	must(t, db, "INSERT INTO t1 VALUES(1,1),(2,2),(-1,3),(-2,4)")
	// Partial UNIQUE index: duplicate keys outside the predicate are fine.
	must(t, db, "CREATE UNIQUE INDEX up ON t1(x) WHERE x > 0")
	must(t, db, "INSERT INTO t1 VALUES(-5, 5)") // x<0, not in index
	execFails(t, db, "INSERT INTO t1 VALUES(1, 99)", "UNIQUE constraint failed: t1.x")
}

// TestP3Index_DropIndex covers DROP INDEX, IF EXISTS, and the autoindex
// drop error.
func TestP3Index_DropIndex(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int)")
	must(t, db, "CREATE INDEX i1 ON t1(a)")
	must(t, db, "DROP INDEX i1")
	expectIndexNames(t, db, "t1")

	// IF EXISTS on missing index is a no-op; without IF EXISTS it errors.
	must(t, db, "DROP INDEX IF EXISTS nope")
	execFails(t, db, "DROP INDEX nope", "no such index: nope")

	// IF NOT EXISTS.
	must(t, db, "CREATE INDEX IF NOT EXISTS i1 ON t1(a)")
	must(t, db, "CREATE INDEX IF NOT EXISTS i1 ON t1(a)")
	expectIndexNames(t, db, "t1", "i1")
}

// TestP3Index_Autoindex covers sqlite_autoindex_* naming and numbering for
// PK and UNIQUE constraints, plus the drop-protection error.
func TestP3Index_Autoindex(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Column-level non-integer PK + column UNIQUE.
	must(t, db, "CREATE TABLE t6(a int PRIMARY KEY, b text UNIQUE)")
	expectIndexNames(t, db, "t6", "sqlite_autoindex_t6_1", "sqlite_autoindex_t6_2")

	// INTEGER PRIMARY KEY is a rowid alias: no autoindex.
	must(t, db, "CREATE TABLE t11(a INTEGER PRIMARY KEY, b text UNIQUE)")
	expectIndexNames(t, db, "t11", "sqlite_autoindex_t11_1")

	// Dedup: same column set for PK and UNIQUE.
	must(t, db, "CREATE TABLE t9(a int PRIMARY KEY UNIQUE)")
	expectIndexNames(t, db, "t9", "sqlite_autoindex_t9_1")

	// Table-level ordering: column-level first, then table-level.
	must(t, db, "CREATE TABLE t7(c, d UNIQUE, UNIQUE(c), PRIMARY KEY(c, d))")
	expectIndexNames(t, db, "t7", "sqlite_autoindex_t7_1", "sqlite_autoindex_t7_2", "sqlite_autoindex_t7_3")

	// Cannot drop an autoindex.
	execFails(t, db, "DROP INDEX sqlite_autoindex_t7_1", "index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped")
	execFails(t, db, "DROP INDEX IF EXISTS sqlite_autoindex_t7_1", "index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped")
}

// TestP3Index_UpdateMaintenance covers index maintenance when an indexed
// column changes via UPDATE.
func TestP3Index_UpdateMaintenance(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int, b int)")
	must(t, db, "CREATE UNIQUE INDEX u1 ON t1(a)")
	must(t, db, "INSERT INTO t1 VALUES(1,10),(2,20)")

	// Updating an indexed column to a fresh value works and is queryable.
	must(t, db, "UPDATE t1 SET a=5 WHERE b=20")
	if got := flattenQuery(t, db, "SELECT a, b FROM t1 ORDER BY a"); got != "1 10 5 20" {
		t.Errorf("after update: got [%s], want [1 10 5 20]", got)
	}

	// Updating to a conflicting value fails with the UNIQUE error.
	execFails(t, db, "UPDATE t1 SET a=1 WHERE b=20", "UNIQUE constraint failed: t1.a")

	// Updating to the same value it already has is allowed.
	must(t, db, "UPDATE t1 SET a=1 WHERE b=10")
}

// TestP3Index_QueryEquivalence verifies queries return identical results
// whether or not an index exists (correctness, not planner preference).
func TestP3Index_QueryEquivalence(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int, b int)")
	must(t, db, "INSERT INTO t1 VALUES(1,10),(2,20),(3,30),(4,40),(5,50),(6,60),(7,70),(8,80),(9,90),(10,100)")

	queries := []string{
		"SELECT a FROM t1 WHERE b=50",
		"SELECT a, b FROM t1 WHERE a BETWEEN 3 AND 7 ORDER BY a DESC",
		"SELECT a FROM t1 WHERE b IN (10, 60) ORDER BY a",
		"SELECT count(*) FROM t1 WHERE a > 5",
		"SELECT a, b FROM t1 ORDER BY b LIMIT 3",
		"SELECT a FROM t1 WHERE a+b > 100",
	}
	baseline := make([]string, len(queries))
	for i, q := range queries {
		baseline[i] = flattenQuery(t, db, q)
	}

	must(t, db, "CREATE INDEX i1 ON t1(a)")
	must(t, db, "CREATE INDEX i2 ON t1(b)")
	must(t, db, "CREATE INDEX i3 ON t1(a+b)")
	for i, q := range queries {
		if got := flattenQuery(t, db, q); got != baseline[i] {
			t.Errorf("query [%s]: with-index got [%s], want [%s]", q, got, baseline[i])
		}
	}
}

// TestP3Index_WithoutRowid covers indexes on WITHOUT ROWID tables: the PK
// uniqueness works and the PK autoindex is not stored.
func TestP3Index_WithoutRowid(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t5(a int PRIMARY KEY, b int) WITHOUT ROWID")
	expectIndexNames(t, db, "t5")

	must(t, db, "INSERT INTO t5 VALUES(1,1),(2,2)")
	execFails(t, db, "INSERT INTO t5 VALUES(1,99)", "UNIQUE constraint failed: t5.a")
	if got := flattenQuery(t, db, "SELECT a, b FROM t5 WHERE a=2"); got != "2 2" {
		t.Errorf("without rowid query: got [%s], want [2 2]", got)
	}
}

// TestP3Index_StoredSQL verifies the sqlite_schema.sql column stores the
// original CREATE INDEX verbatim, including expression keys and WHERE.
func TestP3Index_StoredSQL(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must(t, db, "CREATE TABLE t1(a int, b int)")
	must(t, db, "CREATE INDEX ie ON t1(a+b)")
	must(t, db, "CREATE INDEX ip ON t1(a) WHERE a > 1 AND b < 5")

	// Index SQL is stored verbatim.
	if sql := tableSQL(t, db, "ie"); strings.TrimSpace(sql) != "CREATE INDEX ie ON t1(a+b)" {
		t.Errorf("expr index sql: got %q", sql)
	}
	if sql := tableSQL(t, db, "ip"); strings.TrimSpace(sql) != "CREATE INDEX ip ON t1(a) WHERE a > 1 AND b < 5" {
		t.Errorf("partial index sql: got %q", sql)
	}
}
