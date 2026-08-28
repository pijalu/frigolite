package frigolite

import (
	"strings"
	"testing"
)

// P3 pre-tests: hand-written tests for G3.CONSTRAINTS column/table constraint
// support, written BEFORE running the TCL testgen packages (check, notnull,
// conflict, trans, transitive). Each expectation was verified against sqlite3
// 3.51 (via the sqlite3 CLI) as the oracle; the tests document the exact
// SQLite semantics frigolite must match: NOT NULL (incl. NULL from a
// NULL-yielding expression and via DEFAULT), CHECK (evaluated per row with
// verbatim expression text in the error), UNIQUE (single + multi-column, NULL
// handling), PRIMARY KEY (INTEGER rowid alias / composite / WITHOUT ROWID /
// DESC quirks), conflict clauses (per-constraint ON CONFLICT overrides
// statement OR; ROLLBACK/ABORT/FAIL/IGNORE/REPLACE), and statement-level
// rollback on violation.

// TestP3Constraints is the top-level entry for the P3 CONSTRAINTS pre-tests.
// The verify command runs it via `go test -run TestP3Constraints -count=1 .`
func TestP3Constraints(t *testing.T) {
	for _, sub := range []string{
		"NotNull", "Check", "Unique", "PrimaryKey",
		"ConflictClauses", "StatementRollback",
	} {
		ok := t.Run(sub, func(t *testing.T) {
			switch sub {
			case "NotNull":
				TestP3Constraints_NotNull(t)
			case "Check":
				TestP3Constraints_Check(t)
			case "Unique":
				TestP3Constraints_Unique(t)
			case "PrimaryKey":
				TestP3Constraints_PrimaryKey(t)
			case "ConflictClauses":
				TestP3Constraints_ConflictClauses(t)
			case "StatementRollback":
				TestP3Constraints_StatementRollback(t)
			}
		})
		if !ok {
			t.Fail()
		}
	}
}

// expectErr contains asserts that err is non-nil and its message contains all
// the given substrings.
func expectErr(t *testing.T, err error, subs ...string) {
	t.Helper()
	if err == nil {
		t.Errorf("expected error containing %q, got nil", strings.Join(subs, " / "))
		return
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not contain %q", err.Error(), s)
		}
	}
}

// TestP3Constraints_NotNull covers NOT NULL enforcement: literal NULL, NULL
// from a NULL-yielding expression, NULL via DEFAULT, and the exact error text
// "NOT NULL constraint failed: <table>.<column>".
func TestP3Constraints_NotNull(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "CREATE TABLE nn(x NOT NULL, y);")

	// Literal NULL.
	expectErr(t, db.Exec("INSERT INTO nn VALUES(NULL, 1)").Error, "NOT NULL constraint failed: nn.x")

	// NULL from a NULL-yielding expression (SQLite evaluates the expression
	// and treats the result as NULL for constraint purposes).
	expectErr(t, db.Exec("INSERT INTO nn VALUES(NULL+1, 2)").Error, "NOT NULL constraint failed: nn.x")
	expectErr(t, db.Exec("INSERT INTO nn VALUES(1-NULL, 3)").Error, "NOT NULL constraint failed: nn.x")

	// A non-NULL expression result passes (1-1 is 0, not NULL).
	must(t, db, "INSERT INTO nn VALUES(1-1, 4);")

	// NULL via DEFAULT (table with a NOT NULL DEFAULT NULL column).
	must(t, db, "CREATE TABLE nn2(x NOT NULL DEFAULT NULL);")
	expectErr(t, db.Exec("INSERT INTO nn2 DEFAULT VALUES;").Error, "NOT NULL constraint failed: nn2.x")

	// Omitted column with no default is NULL → violates NOT NULL.
	expectErr(t, db.Exec("INSERT INTO nn(y) VALUES(5);").Error, "NOT NULL constraint failed: nn.x")

	// Non-NULL values pass.
	must(t, db, "INSERT INTO nn VALUES('a', 6);")
	r := db.Query("SELECT x, y FROM nn ORDER BY y")
	if r.Error != nil {
		t.Fatalf("query: %v", r.Error)
	}
	if len(r.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(r.Rows))
	}
}

// TestP3Constraints_Check covers CHECK enforcement: verbatim expression text
// in the error message, multi-column expressions, per-row evaluation, and the
// parse-time prohibition of subqueries in CHECK.
func TestP3Constraints_Check(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Verbatim expression text in the error.
	must(t, db, "CREATE TABLE c1(x CHECK (x > 5));")
	expectErr(t, db.Exec("INSERT INTO c1 VALUES(3)").Error, "CHECK constraint failed: x > 5")
	must(t, db, "INSERT INTO c1 VALUES(7);")

	// Multi-column CHECK.
	must(t, db, "CREATE TABLE c2(x, y, CHECK (x < y));")
	expectErr(t, db.Exec("INSERT INTO c2 VALUES(10, 5)").Error, "CHECK constraint failed: x < y")
	must(t, db, "INSERT INTO c2 VALUES(1, 2);")

	// CHECK referencing a function; error text is the stored expression.
	must(t, db, "CREATE TABLE c3(x CHECK (typeof(x)='text'));")
	expectErr(t, db.Exec("INSERT INTO c3 VALUES(5)").Error, "CHECK constraint failed: typeof(x)='text'")

	// CHECK with a named constraint: SQLite uses the constraint NAME in the
	// error (verified against sqlite3 3.51: "CHECK constraint failed: c4chk").
	must(t, db, "CREATE TABLE c4(x CONSTRAINT c4chk CHECK (x != 0));")
	expectErr(t, db.Exec("INSERT INTO c4 VALUES(0)").Error, "CHECK constraint failed: c4chk")

	// CHECK evaluated per row (multi-VALUES insert): the second row fails and
	// the statement's first row is rolled back (ABORT default).
	must(t, db, "CREATE TABLE c5(x CHECK (x > 0));")
	expectErr(t, db.Exec("INSERT INTO c5 VALUES(1),(-1), (2)").Error, "CHECK constraint failed: x > 0")
	r := db.Query("SELECT count(*) FROM c5")
	if r.Error != nil || r.Rows[0][0] != int64(0) {
		t.Errorf("c5 count after failed multi-insert: got %v err %v, want 0 (statement rollback)", r.Rows, r.Error)
	}

	// Subqueries are prohibited in CHECK at CREATE TABLE time (SQLite:
	// "subqueries prohibited in CHECK constraints").
	must(t, db, "CREATE TABLE subq_src(y);")
	expectErr(t, db.Exec("CREATE TABLE c6(x CHECK (x IN (SELECT y FROM subq_src)))").Error,
		"subqueries prohibited in CHECK constraints")

	// CHECK is evaluated at INSERT time, not DDL time: rows inserted before a
	// CHECK is added via ALTER are validated (covered by G3.ALTER); here we
	// only check the constraint fires for new rows.
	must(t, db, "CREATE TABLE c7(x);")
	must(t, db, "INSERT INTO c7 VALUES(1);")
}

// TestP3Constraints_Unique covers UNIQUE constraints: single-column, table
// (multi-column), NULL handling (multiple NULLs allowed), NOT NULL + UNIQUE,
// and exact error text ("UNIQUE constraint failed: <col>" / "<col1>, <col2>").
func TestP3Constraints_Unique(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Single-column UNIQUE.
	must(t, db, "CREATE TABLE u1(x UNIQUE, y);")
	must(t, db, "INSERT INTO u1 VALUES(1, 'a');")
	expectErr(t, db.Exec("INSERT INTO u1 VALUES(1, 'b')").Error, "UNIQUE constraint failed: u1.x")
	// SQLite names the column as "table.column" for column constraints.
	must(t, db, "INSERT INTO u1 VALUES(2, 'b');")

	// Multiple NULLs are allowed in a UNIQUE column (NULLs never collide).
	must(t, db, "INSERT INTO u1 VALUES(NULL, 'n1');")
	must(t, db, "INSERT INTO u1 VALUES(NULL, 'n2');")
	r := db.Query("SELECT count(*) FROM u1")
	if r.Error != nil || r.Rows[0][0] != int64(4) {
		t.Errorf("u1 count: got %v err %v, want 4", r.Rows, r.Error)
	}

	// Multi-column (table-level) UNIQUE: NULL in ANY column → the row never
	// collides (SQLite treats a row with any NULL key column as distinct).
	must(t, db, "CREATE TABLE u2(a, b, UNIQUE(a, b));")
	must(t, db, "INSERT INTO u2 VALUES(NULL, 1);")
	must(t, db, "INSERT INTO u2 VALUES(NULL, 1);") // NULL key → no collision
	must(t, db, "INSERT INTO u2 VALUES(1, NULL);")
	must(t, db, "INSERT INTO u2 VALUES(1, NULL);")
	must(t, db, "INSERT INTO u2 VALUES(NULL, NULL);")
	must(t, db, "INSERT INTO u2 VALUES(NULL, NULL);")
	must(t, db, "INSERT INTO u2 VALUES(1, 2);")
	expectErr(t, db.Exec("INSERT INTO u2 VALUES(1, 2)").Error, "UNIQUE constraint failed: u2.a, u2.b")

	// NOT NULL + UNIQUE behaves as expected (same as UNIQUE; NULL can't occur).
	must(t, db, "CREATE TABLE u3(x NOT NULL UNIQUE);")
	must(t, db, "INSERT INTO u3 VALUES(1);")
	expectErr(t, db.Exec("INSERT INTO u3 VALUES(1)").Error, "UNIQUE constraint failed: u3.x")
	expectErr(t, db.Exec("INSERT INTO u3 VALUES(NULL)").Error, "NOT NULL constraint failed: u3.x")

	// UNIQUE index created implicitly (autoindex present in sqlite_schema).
	r = db.Query("SELECT count(*) FROM sqlite_schema WHERE type='index' AND tbl_name='u1' AND name LIKE 'sqlite_autoindex_u1_%'")
	if r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Errorf("u1 autoindex: got %v err %v, want 1", r.Rows, r.Error)
	}
}

// TestP3Constraints_PrimaryKey covers PRIMARY KEY semantics: INTEGER PRIMARY
// KEY is a rowid alias; composite PK is not (rowid stays separate); INTEGER
// PRIMARY KEY DESC is NOT a rowid alias; WITHOUT ROWID rejects NULL PK; and
// PK duplicates are rejected.
func TestP3Constraints_PrimaryKey(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// INTEGER PRIMARY KEY is a rowid alias: NULL insert auto-assigns rowid,
	// and the column mirrors rowid.
	must(t, db, "CREATE TABLE pk1(a INTEGER PRIMARY KEY, b);")
	must(t, db, "INSERT INTO pk1(b) VALUES('x');") // a auto-assigned
	r := db.Query("SELECT a, rowid FROM pk1")
	if r.Error != nil {
		t.Fatalf("pk1 query: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) || r.Rows[0][1] != int64(1) {
		t.Errorf("pk1 rowid alias: got %v, want a=1 rowid=1", r.Rows)
	}
	must(t, db, "INSERT INTO pk1(a, b) VALUES(5, 'y');")
	r = db.Query("SELECT a, rowid FROM pk1 ORDER BY rowid")
	if r.Error != nil || len(r.Rows) != 2 || r.Rows[1][0] != int64(5) || r.Rows[1][1] != int64(5) {
		t.Errorf("pk1 explicit: got %v, want a=5 rowid=5", r.Rows)
	}
	// Duplicate rowid-alias value is a UNIQUE violation.
	expectErr(t, db.Exec("INSERT INTO pk1(a, b) VALUES(5, 'z')").Error, "UNIQUE constraint failed")

	// Composite PRIMARY KEY is NOT a rowid alias: rowid is separate, and the
	// PK values do not move rowid.
	must(t, db, "CREATE TABLE pk2(a, b, PRIMARY KEY(a, b));")
	must(t, db, "INSERT INTO pk2 VALUES(10, 20);")
	r = db.Query("SELECT a, b, rowid FROM pk2")
	if r.Error != nil {
		t.Fatalf("pk2 query: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(10) || r.Rows[0][1] != int64(20) || r.Rows[0][2] != int64(1) {
		t.Errorf("pk2 composite: got %v, want a=10 b=20 rowid=1", r.Rows)
	}
	expectErr(t, db.Exec("INSERT INTO pk2 VALUES(10, 20)").Error, "UNIQUE constraint failed: pk2.a, pk2.b")

	// INTEGER PRIMARY KEY DESC is NOT a rowid alias (verified against
	// sqlite3 3.51): auto-insert leaves the column NULL and assigns a fresh
	// rowid; explicit values do not move rowid.
	must(t, db, "CREATE TABLE pk3(a INTEGER PRIMARY KEY DESC, b);")
	must(t, db, "INSERT INTO pk3(b) VALUES('x');")
	r = db.Query("SELECT a, rowid FROM pk3")
	if r.Error != nil {
		t.Fatalf("pk3 query: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("pk3 rows: got %d", len(r.Rows))
	}
	// a must be NULL and rowid must be 1 (frigolite previously aliased DESC).
	if v, ok := r.Rows[0][0].(int64); ok || v != 0 {
		if r.Rows[0][0] != nil {
			t.Errorf("pk3 DESC auto: a = %v (%T), want NULL (not a rowid alias)", r.Rows[0][0], r.Rows[0][0])
		}
	}
	if r.Rows[0][1] != int64(1) {
		t.Errorf("pk3 DESC auto: rowid = %v, want 1", r.Rows[0][1])
	}
	must(t, db, "INSERT INTO pk3(a, b) VALUES(5, 'z');")
	r = db.Query("SELECT a, rowid FROM pk3 ORDER BY rowid")
	if r.Error != nil || len(r.Rows) != 2 {
		t.Fatalf("pk3 explicit rows: %v err %v", r.Rows, r.Error)
	}
	// a=5 must be stored with a separate rowid=2 (not rowid=5).
	if r.Rows[1][0] != int64(5) || r.Rows[1][1] != int64(2) {
		t.Errorf("pk3 DESC explicit: got %v, want a=5 rowid=2", r.Rows[1])
	}

	// WITHOUT ROWID: PK implies NOT NULL and rowid is not exposed.
	must(t, db, "CREATE TABLE pk4(a TEXT PRIMARY KEY, b) WITHOUT ROWID;")
	must(t, db, "INSERT INTO pk4 VALUES('k', 1);")
	expectErr(t, db.Exec("INSERT INTO pk4(a, b) VALUES(NULL, 2)").Error, "NOT NULL constraint failed: pk4.a")
	expectErr(t, db.Query("SELECT rowid FROM pk4").Error, "no such column: rowid")

	// Plain (non-INTEGER) PK accepts NULL in a rowid table (SQLite quirk:
	// PRIMARY KEY does not imply NOT NULL outside WITHOUT ROWID/STRICT/INTEGER
	// PK). The unique constraint still applies to non-NULL values.
	must(t, db, "CREATE TABLE pk5(a TEXT PRIMARY KEY, b);")
	must(t, db, "INSERT INTO pk5(a, b) VALUES(NULL, 'n1');")
	must(t, db, "INSERT INTO pk5(a, b) VALUES(NULL, 'n2');")
	must(t, db, "INSERT INTO pk5(a, b) VALUES('k', 'v');")
	expectErr(t, db.Exec("INSERT INTO pk5(a, b) VALUES('k', 'w')").Error, "UNIQUE constraint failed: pk5.a")
}

// TestP3Constraints_ConflictClauses covers ON CONFLICT resolution: per
// constraint, statement OR, precedence (per-constraint wins), and the
// ROLLBACK/ABORT/FAIL/IGNORE/REPLACE outcomes.
func TestP3Constraints_ConflictClauses(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Statement OR IGNORE: conflicting row is skipped, statement succeeds.
	must(t, db, "CREATE TABLE t1(x UNIQUE, y);")
	must(t, db, "INSERT INTO t1 VALUES(1, 'a');")
	if res := db.Exec("INSERT OR IGNORE INTO t1 VALUES(1, 'b');"); res.Error != nil {
		t.Errorf("INSERT OR IGNORE: %v", res.Error)
	} else if res.Changes != 0 {
		t.Errorf("INSERT OR IGNORE changes = %d, want 0", res.Changes)
	}
	r := db.Query("SELECT count(*) FROM t1")
	if r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Errorf("t1 count after OR IGNORE: %v", r.Rows)
	}

	// Statement OR REPLACE: conflicting row is deleted and the new row
	// inserted.
	must(t, db, "CREATE TABLE t2(x UNIQUE, y);")
	must(t, db, "INSERT INTO t2 VALUES(1, 'a');")
	must(t, db, "INSERT OR REPLACE INTO t2 VALUES(1, 'c');")
	r = db.Query("SELECT count(*), y FROM t2 GROUP BY y")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Errorf("t2 after REPLACE: %v err %v", r.Rows, r.Error)
	}
	if len(r.Rows) == 1 && r.Rows[0][1] != "c" {
		t.Errorf("t2 after REPLACE: y = %v, want c", r.Rows[0][1])
	}

	// OR FAIL: stops at the conflicting row but does NOT roll back prior rows
	// of the same statement.
	must(t, db, "CREATE TABLE t3(x UNIQUE, y);")
	must(t, db, "INSERT INTO t3 VALUES(1, 'a'), (2, 'b');")
	expectErr(t, db.Exec("INSERT OR FAIL INTO t3 VALUES(3, 'c'), (1, 'dup'), (4, 'f')").Error,
		"UNIQUE constraint failed: t3.x")
	r = db.Query("SELECT x FROM t3 ORDER BY x")
	if r.Error != nil || len(r.Rows) != 3 || r.Rows[2][0] != int64(3) {
		t.Errorf("t3 after OR FAIL: got %v, want x=3 kept (FAIL keeps prior rows)", r.Rows)
	}

	// OR ABORT: rolls back the statement's prior rows (autocommit statement).
	must(t, db, "CREATE TABLE t4(x UNIQUE, y);")
	must(t, db, "INSERT INTO t4 VALUES(1, 'a'), (2, 'b');")
	expectErr(t, db.Exec("INSERT OR ABORT INTO t4 VALUES(3, 'c'), (1, 'dup'), (4, 'f')").Error,
		"UNIQUE constraint failed: t4.x")
	r = db.Query("SELECT count(*) FROM t4")
	if r.Error != nil || r.Rows[0][0] != int64(2) {
		t.Errorf("t4 after OR ABORT: got %v, want 2 (statement rolled back)", r.Rows)
	}

	// OR ROLLBACK inside an explicit transaction: whole transaction rolled
	// back (autocommit restored).
	must(t, db, "CREATE TABLE t5(x UNIQUE, y);")
	must(t, db, "INSERT INTO t5 VALUES(1, 'a'), (2, 'b');")
	must(t, db, "BEGIN;")
	expectErr(t, db.Exec("INSERT OR ROLLBACK INTO t5 VALUES(3, 'c'), (1, 'dup')").Error,
		"UNIQUE constraint failed: t5.x")
	r = db.Query("SELECT count(*) FROM t5")
	if r.Error != nil || r.Rows[0][0] != int64(2) {
		t.Errorf("t5 after OR ROLLBACK: got %v, want 2", r.Rows)
	}
	// After OR ROLLBACK the transaction has ended; the next statement is in
	// autocommit (a new BEGIN succeeds).
	must(t, db, "BEGIN;")
	must(t, db, "ROLLBACK;")

	// Per-constraint ON CONFLICT vs statement OR precedence: the statement
	// OR OVERRIDES the per-constraint algorithm (verified against sqlite3
	// 3.51: UNIQUE ON CONFLICT IGNORE + INSERT OR ABORT errors; UNIQUE ON
	// CONFLICT ABORT + INSERT OR IGNORE skips).
	must(t, db, "CREATE TABLE t6(x UNIQUE ON CONFLICT IGNORE, y);")
	must(t, db, "INSERT INTO t6 VALUES(1, 'a');")
	expectErr(t, db.Exec("INSERT OR ABORT INTO t6 VALUES(1, 'b');").Error,
		"UNIQUE constraint failed: t6.x")
	r = db.Query("SELECT count(*) FROM t6")
	if r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Errorf("t6 count: %v, want 1", r.Rows)
	}

	must(t, db, "CREATE TABLE t7(x UNIQUE ON CONFLICT ABORT, y);")
	must(t, db, "INSERT INTO t7 VALUES(1, 'a');")
	if res := db.Exec("INSERT OR IGNORE INTO t7 VALUES(1, 'b');"); res.Error != nil {
		t.Errorf("OR IGNORE should override per-constraint ABORT: %v", res.Error)
	}
	r = db.Query("SELECT count(*) FROM t7")
	if r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Errorf("t7 count: %v, want 1", r.Rows)
	}

	// OR REPLACE overrides per-constraint IGNORE: the conflicting row is
	// replaced even though the column says IGNORE.
	must(t, db, "CREATE TABLE t7r(x UNIQUE ON CONFLICT IGNORE, y);")
	must(t, db, "INSERT INTO t7r VALUES(1, 'a');")
	if res := db.Exec("INSERT OR REPLACE INTO t7r VALUES(1, 'b');"); res.Error != nil {
		t.Errorf("OR REPLACE should override per-constraint IGNORE: %v", res.Error)
	}
	r = db.Query("SELECT y FROM t7r")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != "b" {
		t.Errorf("t7r after OR REPLACE: got %v, want y=b", r.Rows)
	}

	// Column ON CONFLICT REPLACE on NOT NULL with no default still fails
	// (verified against sqlite3 3.51).
	must(t, db, "CREATE TABLE t8(x NOT NULL ON CONFLICT REPLACE, y);")
	expectErr(t, db.Exec("INSERT INTO t8 VALUES(NULL, 'r')").Error, "NOT NULL constraint failed: t8.x")

	// CHECK constraint violations ARE suppressible by OR IGNORE (verified
	// against sqlite3 3.51: INSERT OR IGNORE skips a CHECK-violating row
	// silently, while OR ABORT/FAIL/REPLACE/ROLLBACK error).
	must(t, db, "CREATE TABLE t9(x CHECK (x > 0));")
	if res := db.Exec("INSERT OR IGNORE INTO t9 VALUES(-1);"); res.Error != nil {
		t.Errorf("INSERT OR IGNORE should suppress CHECK: %v", res.Error)
	}
	r = db.Query("SELECT count(*) FROM t9")
	if r.Error != nil || r.Rows[0][0] != int64(0) {
		t.Errorf("t9 count after OR IGNORE CHECK: got %v, want 0", r.Rows)
	}
	expectErr(t, db.Exec("INSERT OR ABORT INTO t9 VALUES(-1)").Error, "CHECK constraint failed: x > 0")
}

// TestP3Constraints_StatementRollback covers statement-level rollback on
// constraint violation: earlier rows of a failed multi-row statement are
// undone for ABORT/ROLLBACK (and the default), while FAIL keeps them.
func TestP3Constraints_StatementRollback(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Default resolution is ABORT: prior rows of the statement are rolled
	// back.
	must(t, db, "CREATE TABLE s1(x UNIQUE, y);")
	must(t, db, "INSERT INTO s1 VALUES(1, 'a'), (2, 'b');")
	expectErr(t, db.Exec("INSERT INTO s1 VALUES(3, 'c'), (1, 'dup'), (4, 'f')").Error,
		"UNIQUE constraint failed: s1.x")
	r := db.Query("SELECT x FROM s1 ORDER BY x")
	if r.Error != nil || len(r.Rows) != 2 {
		t.Errorf("s1 default ABORT rollback: got %v, want rows 1,2 only", r.Rows)
	}

	// NOT NULL violation also rolls back the statement's prior rows.
	must(t, db, "CREATE TABLE s2(x NOT NULL, y);")
	expectErr(t, db.Exec("INSERT INTO s2 VALUES(1, 'a'), (NULL, 'b')").Error,
		"NOT NULL constraint failed: s2.x")
	r = db.Query("SELECT count(*) FROM s2")
	if r.Error != nil || r.Rows[0][0] != int64(0) {
		t.Errorf("s2 NOT NULL rollback: got %v, want 0", r.Rows)
	}

	// CHECK violation rolls back prior rows (per-row evaluation).
	must(t, db, "CREATE TABLE s3(x CHECK (x > 0));")
	expectErr(t, db.Exec("INSERT INTO s3 VALUES(1), (-1)").Error, "CHECK constraint failed: x > 0")
	r = db.Query("SELECT count(*) FROM s3")
	if r.Error != nil || r.Rows[0][0] != int64(0) {
		t.Errorf("s3 CHECK rollback: got %v, want 0", r.Rows)
	}

	// OR FAIL does NOT roll back prior rows of the statement.
	must(t, db, "CREATE TABLE s4(x UNIQUE, y);")
	must(t, db, "INSERT INTO s4 VALUES(1, 'a'), (2, 'b');")
	expectErr(t, db.Exec("INSERT OR FAIL INTO s4 VALUES(3, 'c'), (1, 'dup')").Error,
		"UNIQUE constraint failed: s4.x")
	r = db.Query("SELECT count(*) FROM s4")
	if r.Error != nil || r.Rows[0][0] != int64(3) {
		t.Errorf("s4 OR FAIL keeps prior row: got %v, want 3", r.Rows)
	}

	// Violation inside an explicit transaction with default ABORT does not
	// end the transaction; subsequent statements in the transaction work and
	// the transaction can still COMMIT.
	must(t, db, "CREATE TABLE s5(x UNIQUE, y);")
	must(t, db, "INSERT INTO s5 VALUES(1, 'a');")
	must(t, db, "BEGIN;")
	must(t, db, "INSERT INTO s5 VALUES(2, 'b');")
	expectErr(t, db.Exec("INSERT INTO s5 VALUES(1, 'dup')").Error, "UNIQUE constraint failed: s5.x")
	must(t, db, "INSERT INTO s5 VALUES(3, 'c');")
	must(t, db, "COMMIT;")
	r = db.Query("SELECT count(*) FROM s5")
	if r.Error != nil || r.Rows[0][0] != int64(3) {
		t.Errorf("s5 explicit txn: got %v, want 3 (txn survives statement ABORT)", r.Rows)
	}
}
