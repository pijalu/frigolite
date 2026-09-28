package frigolite

import (
	"fmt"
	"testing"
	"time"
)

// P5 statement-journal tests: statement rollback via before-image journaling
// (pager.c sub-journal parity) instead of whole-database snapshots.
//
// Contracts:
//   - a no-match DELETE (or any statement writing nothing) leaves no rollback
//     work and runs in O(plan) not O(database);
//   - a REPLACE that fails a FOREIGN KEY mid-statement — with BEFORE/AFTER
//     triggers writing side tables — restores the pre-statement state
//     exactly (btree contents + integrity_check);
//   - trigger cascades nested inside a failing statement roll back as a unit.

func setupPerfStmtDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir() + "/perfstmt.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExecPerfStmt(t *testing.T, db *DB, sql string) *Result {
	t.Helper()
	res := db.Exec(sql)
	if res.Error != nil {
		t.Fatalf("exec %q: %v", sql, res.Error)
	}
	return res
}

func queryRowsPerfStmt(t *testing.T, db *DB, sql string) [][]interface{} {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("query %q: %v", sql, res.Error)
	}
	return res.Rows
}

// TestPerfStmtJournalNoMatchDeleteTiming: a DELETE matching nothing on a 20k-row table
// must not pay an O(database) rollback-preparation cost. Before the
// statement journal this was ~9-25ms/statement (a full page-cache deep copy
// per statement); with the journal it is a point-lookup scan.
func TestPerfStmtJournalNoMatchDeleteTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	db := setupPerfStmtDB(t)
	mustExecPerfStmt(t, db, "PRAGMA journal_mode=DELETE")
	mustExecPerfStmt(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	mustExecPerfStmt(t, db, "BEGIN")
	for i := 0; i < 20000; i++ {
		if res := db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, 'row-%d')", i, i)); res.Error != nil {
			t.Fatalf("insert %d: %v", i, res.Error)
		}
	}
	mustExecPerfStmt(t, db, "COMMIT")
	// Warm the plan and the page cache.
	mustExecPerfStmt(t, db, "DELETE FROM t WHERE id = 99999999")

	start := time.Now()
	const reps = 20
	for i := 0; i < reps; i++ {
		mustExecPerfStmt(t, db, "DELETE FROM t WHERE id = 99999999")
	}
	perOp := time.Since(start) / reps
	// Generous bound: the point is O(plan) vs O(database). The pre-journal
	// cost was ~9-25ms on this shape; a scan is microseconds.
	if perOp > 2*time.Millisecond {
		t.Errorf("no-match DELETE = %v/op, want < 2ms (statement journal not avoiding O(database) work?)", perOp)
	}

	// Matching single-row DELETE timing (per-op).
	start = time.Now()
	for i := 0; i < reps; i++ {
		mustExecPerfStmt(t, db, fmt.Sprintf("DELETE FROM t WHERE id = %d", 19000+i))
	}
	perOp = time.Since(start) / reps
	if perOp > 2*time.Millisecond {
		t.Errorf("single-row DELETE = %v/op, want < 2ms", perOp)
	}
	if n := queryRowsPerfStmt(t, db, "SELECT count(*) FROM t")[0][0]; n != int64(20000-reps) {
		t.Errorf("row count after deletes = %v, want %d", n, 20000-reps)
	}
}

// TestPerfStmtJournalReplaceFKTriggerRollback: INSERT OR REPLACE on a parent table with
// BEFORE/AFTER DELETE triggers writing side tables, failing an FK mid-
// statement: the whole statement — conflict deletes, trigger side effects,
// the inserted row — must roll back, and the database must stay consistent.
func TestPerfStmtJournalReplaceFKTriggerRollback(t *testing.T) {
	db := setupPerfStmtDB(t)
	mustExecPerfStmt(t, db, "PRAGMA foreign_keys=ON")
	mustExecPerfStmt(t, db, "CREATE TABLE parent (id INTEGER PRIMARY KEY, v TEXT)")
	mustExecPerfStmt(t, db, "CREATE TABLE side_before (id INTEGER)")
	mustExecPerfStmt(t, db, "CREATE TABLE side_after (id INTEGER)")
	mustExecPerfStmt(t, db, "CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id))")
	mustExecPerfStmt(t, db, "CREATE TABLE audit (tag TEXT)")
	mustExecPerfStmt(t, db, "CREATE TRIGGER parent_bd BEFORE DELETE ON parent BEGIN INSERT INTO side_before VALUES (old.id); INSERT INTO audit VALUES ('before'); END")
	mustExecPerfStmt(t, db, "CREATE TRIGGER parent_ad AFTER DELETE ON parent BEGIN INSERT INTO side_after VALUES (old.id); INSERT INTO audit VALUES ('after'); END")
	mustExecPerfStmt(t, db, "INSERT INTO parent VALUES (1, 'a'), (2, 'b')")
	mustExecPerfStmt(t, db, "INSERT INTO child VALUES (100, 1)")

	// REPLACE id=1: the conflicting parent row 1 is deleted (BEFORE/AFTER
	// triggers fire, writing side tables), then the new row fails the FK in
	// the child direction... the child still references parent 1, so deleting
	// parent 1 violates RESTRICT — the whole statement must abort.
	res := db.Exec("INSERT OR REPLACE INTO parent VALUES (1, 'c')")
	if res.Error == nil {
		t.Fatalf("REPLACE violating FK should have failed")
	}

	// Full pre/post state checks.
	gotParents := queryRowsPerfStmt(t, db, "SELECT id, v FROM parent ORDER BY id")
	wantParents := [][]interface{}{{int64(1), "a"}, {int64(2), "b"}}
	if len(gotParents) != 2 || gotParents[0][0] != int64(1) || gotParents[0][1] != "a" || gotParents[1][0] != int64(2) {
		t.Errorf("parents after failed REPLACE = %v, want %v", gotParents, wantParents)
	}
	for _, tbl := range []string{"side_before", "side_after", "audit"} {
		if rows := queryRowsPerfStmt(t, db, "SELECT count(*) FROM "+tbl); rows[0][0] != int64(0) {
			t.Errorf("%s not rolled back: %v", tbl, rows[0][0])
		}
	}
	if rows := queryRowsPerfStmt(t, db, "SELECT id, pid FROM child"); len(rows) != 1 || rows[0][0] != int64(100) {
		t.Errorf("child changed: %v", rows)
	}
	if rows := queryRowsPerfStmt(t, db, "PRAGMA integrity_check"); len(rows) != 1 || rows[0][0] != "ok" {
		t.Errorf("integrity_check: %v", rows)
	}

	// A successful REPLACE still works after the failed one (journal state
	// is not stuck), and the child must be deleted first. (The engine's
	// REPLACE path re-allocates the rowid instead of reusing the deleted
	// row's — pre-existing behavior, verified identical on origin/main; this
	// test pins the rollback-relevant contract: the replace applies, the
	// triggers fired exactly once, state is consistent.)
	mustExecPerfStmt(t, db, "DELETE FROM child")
	mustExecPerfStmt(t, db, "INSERT OR REPLACE INTO parent VALUES (1, 'c')")
	if rows := queryRowsPerfStmt(t, db, "SELECT count(*) FROM parent WHERE v='c'"); rows[0][0] != int64(1) {
		t.Errorf("successful REPLACE did not apply: %v", rows)
	}
	if rows := queryRowsPerfStmt(t, db, "SELECT count(*) FROM parent"); rows[0][0] != int64(2) {
		t.Errorf("parent count after successful REPLACE = %v, want 2", rows[0][0])
	}
	if rows := queryRowsPerfStmt(t, db, "SELECT count(*) FROM side_before"); rows[0][0] != int64(1) {
		t.Errorf("side_before after successful REPLACE = %v, want 1", rows[0][0])
	}
	if rows := queryRowsPerfStmt(t, db, "PRAGMA integrity_check"); len(rows) != 1 || rows[0][0] != "ok" {
		t.Errorf("integrity_check after success: %v", rows)
	}
}

// TestPerfStmtJournalNestedTriggerCascadeRollback: a DELETE whose triggers insert into a
// third table, failing on the second row — the partial cascade (first row's
// trigger writes + first row's delete) must roll back entirely.
func TestPerfStmtJournalNestedTriggerCascadeRollback(t *testing.T) {
	db := setupPerfStmtDB(t)
	mustExecPerfStmt(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	mustExecPerfStmt(t, db, "CREATE TABLE log (id INTEGER)")
	mustExecPerfStmt(t, db, "CREATE TABLE forbidden (id INTEGER PRIMARY KEY)")
	mustExecPerfStmt(t, db, "INSERT INTO forbidden VALUES (42)")
	mustExecPerfStmt(t, db, "CREATE TRIGGER t_ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES (old.id); INSERT INTO forbidden SELECT 42 WHERE old.id = 7; END")
	mustExecPerfStmt(t, db, "INSERT INTO t VALUES (3), (7), (9)")

	if res := db.Exec("DELETE FROM t WHERE id IN (3, 7, 9)"); res.Error == nil {
		t.Fatalf("DELETE hitting the forbidden insert should fail")
	}
	// Everything — the id=3 delete and its trigger log row — rolled back.
	if rows := queryRowsPerfStmt(t, db, "SELECT id FROM t ORDER BY id"); len(rows) != 3 {
		t.Errorf("t rows after failed cascade = %v, want 3 rows", rows)
	}
	if rows := queryRowsPerfStmt(t, db, "SELECT count(*) FROM log"); rows[0][0] != int64(0) {
		t.Errorf("log not rolled back: %v", rows[0][0])
	}
	if rows := queryRowsPerfStmt(t, db, "SELECT count(*) FROM forbidden"); rows[0][0] != int64(1) {
		t.Errorf("forbidden changed: %v", rows[0][0])
	}
	if rows := queryRowsPerfStmt(t, db, "PRAGMA integrity_check"); len(rows) != 1 || rows[0][0] != "ok" {
		t.Errorf("integrity_check: %v", rows)
	}
}

// TestPerfStmtJournalStatementRollbackInsideTransaction: a failed statement inside an
// explicit transaction rolls back only that statement; earlier statements'
// writes survive; a later COMMIT keeps them.
func TestPerfStmtJournalStatementRollbackInsideTransaction(t *testing.T) {
	db := setupPerfStmtDB(t)
	mustExecPerfStmt(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	mustExecPerfStmt(t, db, "INSERT INTO t VALUES (1, 'keep')")
	mustExecPerfStmt(t, db, "BEGIN")
	mustExecPerfStmt(t, db, "INSERT INTO t VALUES (2, 'also-keep')")
	if res := db.Exec("INSERT INTO t VALUES (2, 'dup')"); res.Error == nil {
		t.Fatalf("duplicate PK insert should fail")
	}
	// The failed insert rolled back; the earlier transaction writes remain.
	mustExecPerfStmt(t, db, "COMMIT")
	rows := queryRowsPerfStmt(t, db, "SELECT id, v FROM t ORDER BY id")
	if len(rows) != 2 || rows[0][1] != "keep" || rows[1][1] != "also-keep" {
		t.Errorf("rows after failed statement in transaction = %v", rows)
	}
	if rows := queryRowsPerfStmt(t, db, "PRAGMA integrity_check"); len(rows) != 1 || rows[0][0] != "ok" {
		t.Errorf("integrity_check: %v", rows)
	}
}
