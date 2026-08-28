package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLock5VFSLockingStyles drives the unix VFS locking-style matrix directly
// through the engine (no testgen/transpiler) so a failure is an engine bug.
// The semantics are read from SQLite source (src/os_unix.c dotlockLock /
// flockLock): dotfile and flock collapse every lock level into a single
// EXCLUSIVE mutex that excludes ALL other connections (readers and writers);
// the dotfile style additionally maintains a path+".lock" sentinel directory.
// unix-none performs no cross-connection locking. Verified against
// /usr/bin/sqlite3 -vfs unix-dotfile|unix-flock|unix-none (test/lock5.test).

func TestLock5VFSDotfileSentinel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.db")
	touch(t, p)

	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetLockStyle(LockStyleDotfile)

	// No lock yet -> no sentinel.
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("sentinel should not exist before any lock")
	}
	// BEGIN; CREATE TABLE acquires a write lock -> sentinel appears.
	if r := db.Exec("BEGIN; CREATE TABLE t1(a,b)"); r.Error != nil {
		t.Fatalf("BEGIN;CREATE: %v", r.Error)
	}
	if _, err := os.Stat(p + ".lock"); err != nil {
		t.Fatalf("sentinel missing after BEGIN;CREATE: %v", err)
	}
	// COMMIT releases -> sentinel removed.
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("COMMIT: %v", r.Error)
	}
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("sentinel should be removed after COMMIT")
	}
	// A held read transaction keeps the sentinel.
	if r := db.Exec("BEGIN; SELECT * FROM sqlite_master"); r.Error != nil {
		t.Fatalf("BEGIN;SELECT: %v", r.Error)
	}
	if _, err := os.Stat(p + ".lock"); err != nil {
		t.Fatalf("sentinel missing while read tx open: %v", err)
	}
	if r := db.Exec("ROLLBACK"); r.Error != nil {
		t.Fatalf("ROLLBACK: %v", r.Error)
	}
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("sentinel should be removed after ROLLBACK")
	}
}

func TestLock5VFSDotfileMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.db")
	touch(t, p)

	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetLockStyle(LockStyleDotfile)

	db2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.SetLockStyle(LockStyleDotfile)

	// Seed a table on db (auto-commit, no cross lock needed).
	if r := db.Exec("CREATE TABLE t1(a,b)"); r.Error != nil {
		t.Fatalf("seed: %v", r.Error)
	}
	// db holds a read transaction (SHARED under dotfile = EXCLUSIVE mutex).
	if r := db.Exec("BEGIN; SELECT * FROM t1"); r.Error != nil {
		t.Fatalf("db BEGIN;SELECT: %v", r.Error)
	}
	// db2 read is blocked by db's held lock.
	if r := db2.Exec("SELECT * FROM t1"); r.Error == nil {
		t.Fatalf("expected database is locked for db2 read while db holds lock")
	} else if r.Error.Error() != "database is locked" {
		t.Fatalf("expected 'database is locked', got %q", r.Error.Error())
	}
	// db release -> db2 may read.
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("db COMMIT: %v", r.Error)
	}
	if r := db2.Exec("SELECT * FROM t1"); r.Error != nil {
		t.Fatalf("db2 read after db release: %v", r.Error)
	}
}

func TestLock5VFSFlockMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.db")
	touch(t, p)

	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetLockStyle(LockStyleExclusive)

	db2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.SetLockStyle(LockStyleExclusive)

	if r := db.Exec("CREATE TABLE t1(a,b)"); r.Error != nil {
		t.Fatalf("seed: %v", r.Error)
	}
	// No sentinel for flock style.
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("flock style must not create a sentinel")
	}
	// db holds RESERVED (write tx) -> db2 read blocked.
	if r := db.Exec("BEGIN; INSERT INTO t1 VALUES(1,2)"); r.Error != nil {
		t.Fatalf("db BEGIN;INSERT: %v", r.Error)
	}
	if r := db2.Exec("SELECT * FROM t1"); r.Error == nil {
		t.Fatalf("expected database is locked for db2 while db holds RESERVED")
	} else if r.Error.Error() != "database is locked" {
		t.Fatalf("expected 'database is locked', got %q", r.Error.Error())
	}
	// db COMMIT -> db2 read ok.
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("db COMMIT: %v", r.Error)
	}
	if r := db2.Exec("SELECT * FROM t1"); r.Error != nil {
		t.Fatalf("db2 read after COMMIT: %v", r.Error)
	}
}

func TestLock5VFSNoneNoLocking(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.db")
	touch(t, p)

	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetLockStyle(LockStyleNone)

	db2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.SetLockStyle(LockStyleNone)

	if r := db.Exec("CREATE TABLE t1(a,b)"); r.Error != nil {
		t.Fatalf("seed: %v", r.Error)
	}
	// db holds a write transaction; under unix-none db2 can still read.
	if r := db.Exec("BEGIN; INSERT INTO t1 VALUES(1,2)"); r.Error != nil {
		t.Fatalf("db BEGIN;INSERT: %v", r.Error)
	}
	if r := db2.Exec("SELECT * FROM t1"); r.Error != nil {
		t.Fatalf("unix-none: db2 read must NOT be blocked: %v", r.Error)
	}
}

func touch(t *testing.T, p string) {
	t.Helper()
	if f, err := os.Create(p); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}
}
