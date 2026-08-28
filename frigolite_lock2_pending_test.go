package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLock2PendingModel drives the lock2-1.1..1.8 cross-connection PENDING
// sequence directly through the engine (no testgen/transpiler) so a failure
// here is an engine bug, not a transpiler artifact. Sequence (Verified against
// /usr/bin/sqlite3 test/lock2.test):
//  1.1 fixture auto-commit read
//  1.2 main BEGIN; CREATE TABLE   -> RESERVED
//  1.3 fixture BEGIN; SELECT       -> SHARED held across later calls
//  1.4 fixture CREATE TABLE         -> "database is locked" (main RESERVED)
//  1.5 main COMMIT                  -> "database is locked" (fixture SHARED blocks EXCLUSIVE), tx stays open, PENDING set
//  1.6 fixture SELECT; COMMIT       -> succeeds (PENDING blocks NEW shared only)
//  1.7 fixture BEGIN; SELECT        -> "database is locked" (NEW shared after release)
//  1.8 main COMMIT                  -> succeeds once SHARED released
func TestLock2PendingModel(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	// Create the DB file (touch) so both connections open the same path.
	if f, err := os.Create(dbPath); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}

	main, err := Open(dbPath)
	if err != nil {
		t.Fatalf("main open: %v", err)
	}
	defer main.Close()
	fixture, err := Open(dbPath)
	if err != nil {
		t.Fatalf("fixture open: %v", err)
	}
	defer fixture.Close()

	// 1.1 fixture auto-commit read (connection stays open).
	if r := fixture.Exec("SELECT * FROM sqlite_master"); r.Error != nil {
		t.Fatalf("1.1: %v", r.Error)
	}
	// 1.2 main BEGIN; CREATE TABLE abc -> RESERVED.
	if r := main.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("1.2 BEGIN: %v", r.Error)
	}
	if r := main.Exec("CREATE TABLE abc(a)"); r.Error != nil {
		t.Fatalf("1.2 CREATE: %v", r.Error)
	}
	// 1.3 fixture BEGIN; SELECT -> SHARED held.
	if r := fixture.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("1.3 BEGIN: %v", r.Error)
	}
	if r := fixture.Exec("SELECT * FROM sqlite_master"); r.Error != nil {
		t.Fatalf("1.3 SELECT: %v", r.Error)
	}
	// 1.4 fixture CREATE TABLE def -> "database is locked" (main RESERVED).
	if r := fixture.Exec("CREATE TABLE def(a)"); r.Error == nil {
		t.Fatalf("1.4 expected database is locked")
	} else if got := r.Error.Error(); got != "database is locked" {
		t.Fatalf("1.4 expected 'database is locked', got %q", got)
	}
	// 1.5 main COMMIT -> "database is locked"; tx STAYS OPEN; PENDING set.
	if r := main.Exec("COMMIT"); r.Error == nil {
		t.Fatalf("1.5 expected database is locked")
	} else if got := r.Error.Error(); got != "database is locked" {
		t.Fatalf("1.5 expected 'database is locked', got %q", got)
	}
	// 1.6 fixture SELECT; COMMIT -> succeeds (PENDING blocks NEW shared only).
	if r := fixture.Exec("SELECT * FROM sqlite_master"); r.Error != nil {
		t.Fatalf("1.6 SELECT: %v", r.Error)
	}
	if r := fixture.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("1.6 COMMIT: %v", r.Error)
	}
	// 1.7 fixture BEGIN; SELECT -> "database is locked" (NEW shared; main PENDING).
	if r := fixture.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("1.7 BEGIN: %v", r.Error)
	}
	if r := fixture.Exec("SELECT * FROM sqlite_master"); r.Error == nil {
		t.Fatalf("1.7 expected database is locked")
	} else if got := r.Error.Error(); got != "database is locked" {
		t.Fatalf("1.7 expected 'database is locked', got %q", got)
	}
	// 1.8 main COMMIT -> succeeds once SHARED released.
	if r := main.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("1.8 COMMIT: %v", r.Error)
	}
}
