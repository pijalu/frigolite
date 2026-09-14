package frigolite

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSharedCacheContract documents the SQLite shared-cache multi-connection
// locking contract exercised by the testgen/shared* packages (P7.LOCK-B) and
// the current Frigolite engine gap.
//
// # N-A status (P7.LOCK-B completion criterion: "NA only w/ evidence")
//
// Shared-cache is a G7 milestone. PORTPLAN.md §G7 states: "No
// WAL/shared-memory/concurrency implementation". Concretely Frigolite has:
//
//   - no sqlite3_enable_shared_cache C-API (Open() cannot be told to share a
//     cache),
//   - no shared pager-cache/schema registry: two Open() calls to the same
//     file each receive an independent *pager.Pager and schema.Manager, and
//   - no table-level lock table (btree.c shared-cache locking /
//     pager.c lockTable).
//
// Until G7 the testgen/shared, shared2, shared3, shared4, shared6, shared7,
// shared8, shared9 packages therefore remain listed in
// tools/tcl2go/skiptestfiles.go as N-A G7, pointing at this file for
// evidence. This mirrors P7.LOCK-A, which marked the G7-class shmlock/
// superlock (WAL shared-memory) packages N-A the same way.
//
// # Oracle contract (validated against /usr/bin/sqlite3 3.51.0)
//
// The exact expected outputs are asserted by SQLite's own test suite at
// /Users/muaddib/dev/sqlite/test/shared*.test; the relevant error texts and
// semantics are:
//
//   - shared.test shared-1.2: two connections in shared-cache mode share one
//     pager cache + schema, so an uncommitted write by conn1 is immediately
//     visible to conn2.
//   - shared.test shared-1.4: a read-lock held by conn1 on table abc blocks
//     conn2's write -> "database table is locked: abc".
//   - shared.test shared-1.5: a schema modification by conn1 blocks all
//     conn2 access -> "database table is locked: sqlite_master".
//   - shared6.test 1.3.3/1.3.4: write-lock on t1 blocks db2 read/write of t1
//     -> "database table is locked: t1" / "database table is locked".
//   - shared6.test 1.4.1: PRAGMA read_uncommitted lets db2 read db1's
//     uncommitted writes; 1.4.2: except schema changes ("database table is
//     locked").
//   - shared6.test 3.4: exclusive transaction upgrade ->
//     "database schema is locked: main"; 3.6 -> "database table is locked".
//   - shared7.test 1.2..1.4: attaching the same file twice ->
//     "database is already attached".
//
// None of the above table-level / schema-lock behaviors can be produced by
// the current engine.
func TestSharedCacheContract(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// --- Current engine behavior that DOES work (non-shared-cache) ---------
	// Two independent connections to the same file share committed data via
	// external-modification detection (P7.LOCK-A infrastructure), which is
	// the part of the shared*.test contract that does not need shared cache.
	db1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if r := db1.Exec("CREATE TABLE abc(a, b, c); INSERT INTO abc VALUES(1, 2, 3)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	// Auto-commit; db1's write is now durable on disk.

	db2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	res := db2.Query("SELECT * FROM abc")
	if res.Error != nil {
		t.Fatalf("db2 read committed data: %v", res.Error)
	}
	if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != "1" {
		t.Fatalf("db2 should see db1's committed row, got %v", res.Rows)
	}

	// --- Documented gap: shared-cache table-level locking is absent -------
	// In shared-cache mode (shared.test shared-1.4) conn1 holding a read-lock
	// on `abc` would make conn2's INSERT fail with "database table is
	// locked: abc". The engine has no such lock table — that is the N-A G7
	// slice-5 gap. The base lock behavior below is ORACLE-VERIFIED
	// (/usr/bin/sqlite3 3.51.0, python3 double-connection probe):
	// rollback-journal mode: the writer's INSERT reports "database is
	// locked" (COMMIT needs EXCLUSIVE while db1 holds SHARED); WAL mode: it
	// succeeds (wal.c has no reader/writer exclusion).
	if r := db1.Exec("BEGIN; SELECT * FROM abc"); r.Error != nil {
		t.Fatalf("db1 read transaction: %v", r.Error)
	}
	if r := db2.Exec("INSERT INTO abc VALUES(4, 5, 6)"); r.Error == nil {
		t.Fatalf("rollback-journal: writer blocked by the reader is the oracle contract — the INSERT unexpectedly succeeded")
	}
	db1.Exec("ROLLBACK")
	db2.Exec("ROLLBACK")
	db1.Close()
	db2.Close()

	// WAL mode: the same scenario succeeds (slice-3's stmtWALMode exemption:
	// no reader/writer exclusion in wal.c).
	db3, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	if r := db3.Exec("PRAGMA journal_mode=WAL"); r.Error != nil {
		t.Fatalf("journal_mode=WAL: %v", r.Error)
	}
	if r := db3.Exec("INSERT INTO abc VALUES(4, 5, 6)"); r.Error != nil {
		t.Fatalf("wal write: %v", r.Error)
	}
	db4, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db4.Close()
	if r := db4.Exec("BEGIN; SELECT * FROM abc"); r.Error != nil {
		t.Fatalf("wal reader begin: %v", r.Error)
	}
	if r := db3.Exec("INSERT INTO abc VALUES(7, 8, 9)"); r.Error != nil {
		t.Fatalf("wal: writer must NOT block on a reader: %v", r.Error)
	}
	db4.Exec("ROLLBACK")
}
