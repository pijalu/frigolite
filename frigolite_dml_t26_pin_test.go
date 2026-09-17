package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteFKMismatchPreparePin pins the fkey.c sqlite3FkCheck contract
// (FULL-SUITE-DRIFT.T26-dml, e_fkey-19.x/20.x): a foreign key whose parent
// table or parent key cannot be located fails INSERT/UPDATE/DELETE at
// prepare time — before any row is read — with
// "foreign key mismatch - "<child>" referencing "<parent>"" or
// "no such table: main.<parent>", regardless of the rows involved.
func TestSQLiteFKMismatchPreparePin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("PRAGMA foreign_keys=ON"); r.Error != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", r.Error)
	}

	// e_fkey-20.2: child DML on an EMPTY table with a missing parent still
	// reports the schema-qualified missing table (prepare-time resolution).
	if r := db.Exec("CREATE TABLE t26c1(c REFERENCES t26nosuch, d)"); r.Error != nil {
		t.Fatalf("setup c1: %v", r.Error)
	}
	if r := db.Exec("UPDATE t26c1 SET c = 'a', d = 'b'"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "no such table: main.t26nosuch") {
		t.Fatalf("UPDATE empty child: got %v, want 'no such table: main.t26nosuch'", r.Error)
	}

	// e_fkey-20.3-8: parent-side DML reports the CHILD's mismatch when the
	// child's parent-key cannot be located (missing parent column,
	// non-unique parent column, collation-mismatched unique index).
	setup := []string{
		"CREATE TABLE t26p2(a, b, UNIQUE(a, b))",
		"CREATE TABLE t26c2(c, d, FOREIGN KEY(c, d) REFERENCES t26p2(a, x))",
		"CREATE TABLE t26p4(a PRIMARY KEY, b)",
		"CREATE UNIQUE INDEX t26p4i ON t26p4(b COLLATE nocase)",
		"CREATE TABLE t26c4(c REFERENCES t26p4(b), d)",
	}
	for _, s := range setup {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("setup %q: %v", s, r.Error)
		}
	}
	// DELETE FROM parent: the child's mismatch error.
	if r := db.Exec("DELETE FROM t26p2"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), `foreign key mismatch - "t26c2" referencing "t26p2"`) {
		t.Fatalf("DELETE parent: got %v, want child4 mismatch", r.Error)
	}
	// UPDATE parent: same contract.
	if r := db.Exec("UPDATE t26p4 SET a = 1"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), `foreign key mismatch - "t26c4" referencing "t26p4"`) {
		t.Fatalf("UPDATE parent: got %v, want c4 mismatch (collation rule)", r.Error)
	}
	// INSERT into parent with a single-row VALUES skips the child scan
	// (fkey.c: inserting a single row cannot cause or fix an immediate FK
	// violation) — e_fkey-19.2's INSERT INTO parent must succeed despite a
	// broken child4.
	for _, s := range []string{
		"CREATE TABLE t26pA(a PRIMARY KEY, e)",
		"CREATE INDEX t26pAi ON t26pA(e)",
		"CREATE TABLE t26cA(l, m REFERENCES t26pA(e))",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("setup %q: %v", s, r.Error)
		}
	}
	if r := db.Exec("INSERT INTO t26pA VALUES('k', 'v')"); r.Error != nil {
		t.Fatalf("single-row INSERT into broken-parent table: %v", r.Error)
	}
}

// TestSQLiteFKRestrictBeforeAfterTriggerPin pins e_fkey-42.5: ON DELETE
// RESTRICT is enforced at the row-delete point, BEFORE the statement's AFTER
// DELETE triggers run — an AFTER trigger repairing the child rows (UPDATE
// child SET c = NULL) must not mask the RESTRICT error.
func TestSQLiteFKRestrictBeforeAfterTriggerPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setup := `
		CREATE TABLE parent(x PRIMARY KEY);
		CREATE TABLE child1(c REFERENCES parent ON DELETE RESTRICT);
		INSERT INTO parent VALUES('key1');
		INSERT INTO child1 VALUES('key1');
		CREATE TRIGGER parent_t AFTER DELETE ON parent BEGIN
			UPDATE child1 SET c = NULL WHERE c = old.x;
		END;
	`
	if r := db.Exec("PRAGMA foreign_keys=ON"); r.Error != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", r.Error)
	}
	if r := db.Exec(setup); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	r := db.Exec("DELETE FROM parent WHERE x = 'key1'")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("RESTRICT delete: got %v, want FOREIGN KEY constraint failed", r.Error)
	}
	// The whole statement rolled back: the parent row survived.
	q := db.Query("SELECT count(*) FROM parent")
	if q.Error != nil || len(q.Rows) == 0 || q.Rows[0][0] != int64(1) {
		t.Fatalf("post-rollback parent count: %v %v", q.Rows, q.Error)
	}
}

// TestSQLiteFKCascadeUpdateMultiChildPin pins fkey8-7.4: ON UPDATE CASCADE
// propagates a parent key change to EVERY matching child row, not just the
// first (the cascade chain's zero-value success Result must not end the
// per-child loop).
func TestSQLiteFKCascadeUpdateMultiChildPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setup := `
		ATTACH ':memory:' AS aux;
		CREATE TABLE aux.p1 (pid PRIMARY KEY);
		CREATE TABLE aux.c1 (cid PRIMARY KEY, pid REFERENCES p1(pid) ON UPDATE CASCADE);
		INSERT INTO aux.p1 VALUES (10), (20);
		INSERT INTO aux.c1 VALUES(11, 10), (12, 10), (21, 20), (22, 20);
	`
	if r := db.Exec("PRAGMA foreign_keys=ON"); r.Error != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", r.Error)
	}
	if r := db.Exec(setup); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Exec("UPDATE aux.p1 SET pid = pid * 10"); r.Error != nil {
		t.Fatalf("parent UPDATE: %v", r.Error)
	}
	q := db.Query("SELECT cid, pid FROM aux.c1 ORDER BY cid")
	if q.Error != nil {
		t.Fatalf("SELECT c1: %v", q.Error)
	}
	want := "11 100 12 100 21 200 22 200"
	if got := t26flattenRows(q.Rows); got != want {
		t.Fatalf("c1 after cascade: got [%s], want [%s]", got, want)
	}
}

// flattenRows renders query rows the way the harness does (space-separated).
func t26flattenRows(rows [][]interface{}) string {
	parts := make([]string, 0, len(rows)*2)
	for _, row := range rows {
		for _, c := range row {
			if c == nil {
				parts = append(parts, "{}")
				continue
			}
			switch v := c.(type) {
			case string:
				parts = append(parts, v)
			case int64:
				parts = append(parts, t26fmtInt(v))
			default:
				parts = append(parts, "?")
			}
		}
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

func t26fmtInt(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestSQLiteLikeCallCounterPin pins the LIKE/GLOB invocation counter
// (func.c sqlite3_like_count under SQLITE_TEST, TCL-linked as
// sqlite_like_count; FULL-SUITE-DRIFT.T26-dml, like.test 3.x): the counter
// advances once per like()/glob() evaluation and the LIKE optimization's
// elision is observable through it. Also pins PRAGMA case_sensitive_like's
// PragFlg_NoColumns contract (the no-argument form returns no row).
func TestSQLiteLikeCallCounterPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t1(x); INSERT INTO t1 VALUES('abc'),('abd'),('xyz')"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	base := db.LikeCallCount()
	if r := db.Query("SELECT x FROM t1 WHERE x LIKE 'ab%'"); r.Error != nil {
		t.Fatalf("LIKE query: %v", r.Error)
	}
	if got := db.LikeCallCount() - base; got != 3 {
		t.Fatalf("LIKE calls: got %d, want 3 (one per row)", got)
	}
	base = db.LikeCallCount()
	if r := db.Query("SELECT x FROM t1 WHERE x GLOB 'ab*'"); r.Error != nil {
		t.Fatalf("GLOB query: %v", r.Error)
	}
	if got := db.LikeCallCount() - base; got != 3 {
		t.Fatalf("GLOB calls: got %d, want 3", got)
	}
	// Reset (tester.tcl: set sqlite_like_count 0).
	db.ResetLikeCallCount()
	if got := db.LikeCallCount(); got != 0 {
		t.Fatalf("reset: got %d, want 0", got)
	}
	// PragFlg_NoColumns: the getter form returns no row at all.
	q := db.Query("PRAGMA case_sensitive_like; SELECT x FROM t1")
	if q.Error != nil {
		t.Fatalf("pragma batch: %v", q.Error)
	}
	if len(q.Rows) != 3 {
		t.Fatalf("pragma batch rows: got %v, want only the 3 SELECT rows", q.Rows)
	}
}

// TestSQLiteIndexDDLValidationPin pins the CREATE INDEX/VIEW/TRIGGER
// validations (FULL-SUITE-DRIFT.T26-dml, index.test 2.1/6.2/7.x/12.x):
// reserved sqlite_ names, index-name/table-name collisions, TEMP index on a
// non-TEMP table, and the schema-qualified missing-table error.
func TestSQLiteIndexDDLValidationPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t6(a, b, c); CREATE TABLE test1(f1, f2)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	expect := func(sql, want string) {
		t.Helper()
		if r := db.Exec(sql); r.Error == nil || !strings.Contains(r.Error.Error(), want) {
			t.Fatalf("%q: got %v, want %q", sql, r.Error, want)
		}
	}
	expect("CREATE INDEX sqlite_i1 ON t6(c)", "object name reserved for internal use: sqlite_i1")
	expect("CREATE VIEW sqlite_v1 AS SELECT * FROM t6", "object name reserved for internal use: sqlite_v1")
	expect("CREATE TRIGGER sqlite_tr1 BEFORE INSERT ON t6 BEGIN SELECT 1; END",
		"object name reserved for internal use: sqlite_tr1")
	expect("CREATE INDEX test1 ON t6(c)", "there is already a table named test1")
	expect("CREATE INDEX i21 ON nosuch_t26(x)", "no such table: main.nosuch_t26")
	expect("CREATE INDEX temp.i21 ON t6(c)", `cannot create a TEMP index on non-TEMP table "t6"`)
	// index-12.x: a failed CREATE must not register the index in temp.
	if r := db.Exec("CREATE INDEX temp.i21 ON nosuch_t26(x)"); r.Error == nil {
		t.Fatalf("second temp create: expected error")
	}
}

// TestSQLiteInsertValuesColumnRefPin pins insert-14.x: a VALUES tuple has no
// source row, so any column reference in it is a prepare-time "no such
// column" error (bare, qualified, and inside expressions), while subqueries
// keep their own scope.
func TestSQLiteInsertValuesColumnRefPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t3(a, b, c); INSERT INTO t3 VALUES(1, 2, 3)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	expect := func(sql, want string) {
		t.Helper()
		if r := db.Exec(sql); r.Error == nil || !strings.Contains(r.Error.Error(), want) {
			t.Fatalf("%q: got %v, want %q", sql, r.Error, want)
		}
	}
	expect("INSERT INTO t3 VALUES((SELECT max(a) FROM t3)+1, t3.a, 6)", "no such column: t3.a")
	expect("INSERT INTO t3 VALUES(a+1, 2, 3)", "no such column: a")
}

// TestSQLiteLimitExprResolutionPin pins limit-12.x: LIMIT/OFFSET expressions
// resolve at prepare time — any column reference errors "no such column"
// (even a real FROM column) and builtin arity mismatches error before rows
// are read.
func TestSQLiteLimitExprResolutionPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t1(a); INSERT INTO t1 VALUES(1)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	expect := func(sql, want string) {
		t.Helper()
		if r := db.Query(sql); r.Error == nil || !strings.Contains(r.Error.Error(), want) {
			t.Fatalf("%q: got %v, want %q", sql, r.Error, want)
		}
	}
	expect("SELECT * FROM t1 LIMIT replace(1)", "wrong number of arguments to function replace()")
	expect("SELECT * FROM t1 LIMIT 5 OFFSET replace(1)", "wrong number of arguments to function replace()")
	expect("SELECT * FROM t1 LIMIT x", "no such column: x")
	expect("SELECT * FROM t1 LIMIT 1 OFFSET x", "no such column: x")
	expect("SELECT * FROM t1 LIMIT a", "no such column: a")
}

// TestSQLiteIndexedByViewPin pins indexedby-6.4: INDEXED BY against a view
// (which has no indexes) reports "no such index".
func TestSQLiteIndexedByViewPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t26i(a); CREATE VIEW v1 AS SELECT * FROM t26i"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Query("SELECT * FROM v1 INDEXED BY i1 WHERE a = 'one'"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "no such index: i1") {
		t.Fatalf("got %v, want 'no such index: i1'", r.Error)
	}
}

// TestSQLiteSubqueryFromPrepareResolutionPin pins in3-5.2: a subquery's FROM
// tables resolve at prepare time — a DELETE whose target table is empty must
// still report the missing table inside its WHERE IN-subquery.
func TestSQLiteSubqueryFromPrepareResolutionPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE Folders(folderid INTEGER PRIMARY KEY)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	r := db.Exec("DELETE FROM Folders WHERE folderid IN (SELECT folderid FROM Folder)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such table: Folder") {
		t.Fatalf("got %v, want 'no such table: Folder'", r.Error)
	}
}

// TestSQLiteExprIndexUniquePin pins indexexpr1-4.x: an expression-index
// UNIQUE key evaluates the key expression per row — substr(b,2,4) COLLATE
// nocase over distinct rows must not raise a false UNIQUE conflict.
func TestSQLiteExprIndexUniquePin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec(`
		CREATE TABLE t8(a INTEGER PRIMARY KEY, b TEXT);
		CREATE UNIQUE INDEX t8bx ON t8(substr(b,2,4) COLLATE nocase);
		INSERT INTO t8(a,b) VALUES(1,'Alice'),(2,'Bartholemew'),(3,'Cynthia');`); r.Error != nil {
		t.Fatalf("setup+inserts: %v", r.Error)
	}
	q := db.Query("SELECT * FROM t8 WHERE substr(b,2,4)='ARTH' COLLATE nocase")
	if q.Error != nil {
		t.Fatalf("SELECT: %v", q.Error)
	}
	if len(q.Rows) != 1 || t26fmtInt(q.Rows[0][0].(int64)) != "2" {
		t.Fatalf("rows: got %v, want row 2", q.Rows)
	}
}
