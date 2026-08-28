package frigolite

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestNolockNoCrossConnectionLocking is the native engine-contract test that
// supersedes testgen/nolock (P7.LOCK-A). nolock.test counts exact VFS
// xLock/xUnlock call counts via a custom testvfs ({xLock 7 xUnlock 5});
// Frigolite has no VFS layer to instrument, so those counts cannot be
// reproduced. The engine-visible contract of nolock=1 / immutable=1 is that
// cross-connection locking is disabled, which LockStyleNone implements and
// which this test exercises directly (mirroring SQLite's unix-none / nolock=1
// VFS, src/os_unix.c nolock handling).
func TestNolockNoCrossConnectionLocking(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if r := db.Exec("CREATE TABLE t1(a, b)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	db.Close()

	// Two connections opened with the nolock (no cross-connection locking)
	// style must each be able to open a write transaction and commit without
	// seeing "database is locked" — exactly as SQLite's unix-none VFS behaves.
	db1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db1.SetLockStyle(LockStyleNone)
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db2.SetLockStyle(LockStyleNone)

	if r := db1.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("db1 begin: %v", r.Error)
	}
	if r := db2.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("db2 begin: %v", r.Error)
	}
	if r := db1.Exec("INSERT INTO t1 VALUES(1, 1)"); r.Error != nil {
		t.Fatalf("db1 insert should succeed under nolock, got: %v", r.Error)
	}
	if r := db2.Exec("INSERT INTO t1 VALUES(2, 2)"); r.Error != nil {
		t.Fatalf("db2 insert should succeed under nolock, got: %v", r.Error)
	}
	if r := db1.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("db1 commit: %v", r.Error)
	}
	if r := db2.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("db2 commit: %v", r.Error)
	}

	db1.Close()
	db2.Close()

	// The engine-visible nolock contract is that neither connection sees
	// "database is locked": both writers succeed without a lock conflict.
	// (nolock deliberately removes locking, not write races — so concurrent
	// nolock writes may lose rows; that loss is NOT what this test asserts.)
	// Verify by reopening and confirming at least one of the two rows landed.
	verify, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	res := verify.Query("SELECT a, b FROM t1 ORDER BY a")
	if res.Error != nil {
		t.Fatalf("verify query: %v", res.Error)
	}
	if len(res.Rows) < 1 {
		t.Fatalf("expected at least one row written without a lock error, got %d", len(res.Rows))
	}
}

// TestNolockVsDefaultLocking confirms the contrast: under the default locking
// style, a concurrent writer IS blocked (so SetLockStyle(LockStyleNone) is what
// disables cross-connection locking). This documents the engine-visible
// contract that nolock.test asserts via VFS call counting.
func TestNolockVsDefaultLocking(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if r := db.Exec("CREATE TABLE t1(a, b)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	db.Close()

	db1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if r := db1.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("db1 begin: %v", r.Error)
	}
	if r := db1.Exec("INSERT INTO t1 VALUES(1, 1)"); r.Error != nil {
		t.Fatalf("db1 insert: %v", r.Error)
	}
	// db2's write must be blocked by db1's open write transaction under the
	// default locking style.
	r := db2.Exec("INSERT INTO t1 VALUES(2, 2)")
	if r.Error == nil {
		t.Fatalf("expected db2 insert to be blocked under default locking")
	}
	if !strings.Contains(r.Error.Error(), "database is locked") {
		t.Fatalf("expected 'database is locked', got: %v", r.Error)
	}
}
