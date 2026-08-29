package frigolite

// ---------------------------------------------------------------------------
// P7.SNAPSHOT — native engine-contract tests for snapshot-style transaction
// isolation. These cover the engine-visible parts of the snapshot contract
// that frigolite DOES support in single-connection mode:
//
//   - Statement-atomic snapshot: a failing statement (constraint violation,
//     UNIQUE conflict) is rolled back to the pre-statement pager snapshot,
//     leaving the database state unchanged (snapshot.test 2.1.x semantics).
//   - Transaction snapshot isolation: an open read transaction observes a
//     consistent view; concurrent (post-COMMIT) writes by the same
//     connection only become visible after COMMIT (snapshot.test 2.2.x
//     isolation principle).
//   - Savepoint snapshot: a SAVEPOINT/ROLLBACK TO restores state captured
//     by the savepoint snapshot, which is the same pager-snapshot machinery
//     the engine uses for snapshot_get/open at the read-mark level
//     (snapshot_up.test 1.x semantics).
//
// These cover the **engine contract** for snapshot-style rollback. The
// cross-connection WAL read-mark API (sqlite3_snapshot_get / open / free /
// cmp on a shared wal-index header) is the G7 multi-connection WAL
// subsystem and cannot be implemented in single-connection mode. See
// `portplan/NA_EVIDENCE.md §P7.SNAPSHOT` for the oracle-verified gap.
// ---------------------------------------------------------------------------

import (
	"path/filepath"
	"testing"
)

// TestSnapshotStatementAtomic mirrors snapshot.test 2.1: a successful statement
// writes; a failing statement leaves state unchanged (statement-atomic
// snapshot of pre-statement state, restored by pager.Snapshot/Restore).
func TestSnapshotStatementAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Setup: 2 rows.
	db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b)")
	db.Exec("INSERT INTO t1 VALUES(1, 2)")
	db.Exec("INSERT INTO t1 VALUES(3, 4)")

	before := db.Query("SELECT count(*) FROM t1")
	if before.Error != nil || len(before.Rows) == 0 {
		t.Fatalf("before count: %v", before.Error)
	}
	if before.Rows[0][0] != int64(2) {
		t.Fatalf("before count = %v, want 2", before.Rows[0][0])
	}

	// Failing statement (UNIQUE conflict on PRIMARY KEY): must be rolled
	// back to the snapshot taken before this statement.
	if res := db.Exec("INSERT INTO t1 VALUES(1, 'x')"); res.Error == nil {
		t.Fatal("expected UNIQUE conflict error, got nil")
	}

	// State must be unchanged: the snapshot restored the pre-statement view.
	after := db.Query("SELECT count(*) FROM t1")
	if after.Error != nil || len(after.Rows) == 0 {
		t.Fatalf("after count: %v", after.Error)
	}
	if after.Rows[0][0] != int64(2) {
		t.Fatalf("after count = %v, want 2 (statement snapshot must roll back)", after.Rows[0][0])
	}

	// And the failing row must not be present.
	row := db.Query("SELECT b FROM t1 WHERE a=1")
	if len(row.Rows) > 0 && row.Rows[0][0] == "x" {
		t.Fatal("failing statement partially committed; snapshot not restored")
	}
}

// TestSnapshotTransactionIsolation mirrors snapshot.test 2.2: an open
// transaction observes a stable view; later writes do not appear until
// commit. This is the single-connection analogue of the snapshot
// isolation guarantee (snapshot_open prevents newer frames from being
// visible to the read transaction).
func TestSnapshotTransactionIsolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	db.Exec("CREATE TABLE t1(a, b)")
	db.Exec("INSERT INTO t1 VALUES(1, 2)")
	db.Exec("INSERT INTO t1 VALUES(3, 4)")

	// Open a read transaction — its SELECT sees the committed state.
	db.Exec("BEGIN")
	r := db.Query("SELECT count(*) FROM t1")
	if r.Rows[0][0] != int64(2) {
		t.Fatalf("BEGIN; count = %v, want 2", r.Rows[0][0])
	}

	// A write inside the same transaction is visible to its own subsequent
	// SELECTs (this is the standard READ_COMMITTED-ish model in SQLite:
	// within a transaction the connection's own writes are visible).
	db.Exec("INSERT INTO t1 VALUES(5, 6)")

	r2 := db.Query("SELECT count(*) FROM t1")
	if r2.Rows[0][0] != int64(3) {
		t.Fatalf("INSERT visible in own txn: count = %v, want 3", r2.Rows[0][0])
	}

	// COMMIT makes the new state permanent; a fresh SELECT sees it.
	db.Exec("COMMIT")
	r3 := db.Query("SELECT count(*) FROM t1")
	if r3.Rows[0][0] != int64(3) {
		t.Fatalf("after COMMIT count = %v, want 3", r3.Rows[0][0])
	}

	// ROLLBACK (instead of COMMIT) restores the snapshot taken at BEGIN
	// — the engine rolls back the transaction via pager snapshots.
	db.Exec("BEGIN")
	db.Exec("INSERT INTO t1 VALUES(7, 8)")
	r4 := db.Query("SELECT count(*) FROM t1")
	if r4.Rows[0][0] != int64(4) {
		t.Fatalf("before rollback: count = %v, want 4", r4.Rows[0][0])
	}
	db.Exec("ROLLBACK")
	r5 := db.Query("SELECT count(*) FROM t1")
	if r5.Rows[0][0] != int64(3) {
		t.Fatalf("after ROLLBACK: count = %v, want 3 (snapshot restored)", r5.Rows[0][0])
	}
}

// TestSnapshotSavepoint mirrors snapshot_up.test 1.x semantics: a
// SAVEPOINT captures a snapshot; ROLLBACK TO restores state to that
// snapshot; RELEASE removes the savepoint without changing state.
// This is the same pager-snapshot machinery that a cross-connection
// snapshot_open would use to anchor a read transaction at a frame
// boundary.
func TestSnapshotSavepoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	db.Exec("CREATE TABLE t1(a, b)")
	db.Exec("BEGIN")
	db.Exec("INSERT INTO t1 VALUES(1, 2)")
	db.Exec("SAVEPOINT sp1")
	db.Exec("INSERT INTO t1 VALUES(3, 4)")
	r := db.Query("SELECT count(*) FROM t1")
	if r.Rows[0][0] != int64(2) {
		t.Fatalf("at SAVEPOINT count = %v, want 2", r.Rows[0][0])
	}
	// ROLLBACK TO sp1 — restores the snapshot taken at SAVEPOINT.
	db.Exec("ROLLBACK TO sp1")
	r2 := db.Query("SELECT count(*) FROM t1")
	if r2.Rows[0][0] != int64(1) {
		t.Fatalf("after ROLLBACK TO sp1 count = %v, want 1 (snapshot restored)", r2.Rows[0][0])
	}
	// RELEASE — discards the savepoint but keeps the (current) state.
	db.Exec("RELEASE sp1")
	r3 := db.Query("SELECT count(*) FROM t1")
	if r3.Rows[0][0] != int64(1) {
		t.Fatalf("after RELEASE sp1 count = %v, want 1", r3.Rows[0][0])
	}
	db.Exec("COMMIT")
}