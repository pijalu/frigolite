package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteSavepointRollbackCancelsPin pins the savepoint.test transaction
// contract that keeps savepoint-4.2 green: ROLLBACK cancels EVERY savepoint
// of the transaction (lang_savepoint.html), so a RELEASE of a pre-ROLLBACK
// savepoint afterwards fails with "no such savepoint", and a fresh BEGIN
// succeeds — a stale savepoint stack must not leave the connection inside an
// implicit transaction.
func TestSQLiteSavepointRollbackCancelsPin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// The savepoint-2.x → 4.2 shape: an explicit transaction with nested
	// savepoints, ended by ROLLBACK (not COMMIT).
	if r := db.Exec("CREATE TABLE t1(a, b, c)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Exec("BEGIN; INSERT INTO t1 VALUES(1, 2, 3); SAVEPOINT one; SAVEPOINT two"); r.Error != nil {
		t.Fatalf("begin block: %v", r.Error)
	}
	if r := db.Exec("ROLLBACK"); r.Error != nil {
		t.Fatalf("rollback: %v", r.Error)
	}
	// A pre-ROLLBACK savepoint is gone.
	if r := db.Exec("RELEASE one"); r.Error == nil || !strings.Contains(r.Error.Error(), "no such savepoint") {
		t.Fatalf("RELEASE after ROLLBACK: got %v, want 'no such savepoint'", r.Error)
	}
	// The connection is in autocommit: BEGIN opens a fresh transaction.
	if r := db.Exec("BEGIN; CREATE TABLE t3(g, h); INSERT INTO t3 VALUES('I', 'II'); SAVEPOINT one; DROP TABLE t3"); r.Error != nil {
		t.Fatalf("fresh BEGIN batch (savepoint-4.2): %v", r.Error)
	}
	if r := db.Exec("ROLLBACK"); r.Error != nil {
		t.Fatalf("final rollback: %v", r.Error)
	}
}

// TestSQLiteSavepointReservedLockPin pins savepoint.test 10.2.5→10.2.8's
// pager-lock contract: once a database takes the RESERVED (writer) lock
// inside a transaction, the lock survives a ROLLBACK TO that restores the
// db's pages — PRAGMA lock_status keeps reporting "reserved" until the
// transaction ends (pager.c clears WRITER state only at COMMIT / full
// ROLLBACK).
func TestSQLiteSavepointReservedLockPin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("ATTACH ':memory:' AS aux1"); r.Error != nil {
		t.Fatalf("attach: %v", r.Error)
	}
	if r := db.Exec("CREATE TABLE main.t1(x, y); CREATE TABLE aux1.t2(x, y)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Exec("SAVEPOINT one; INSERT INTO aux1.t2 VALUES(5, 6)"); r.Error != nil {
		t.Fatalf("savepoint write: %v", r.Error)
	}
	reserved := func(want string) {
		t.Helper()
		q := db.Query("PRAGMA lock_status")
		if q.Error != nil {
			t.Fatalf("lock_status: %v", q.Error)
		}
		for _, row := range q.Rows {
			if strings.EqualFold(row[0].(string), "aux1") && row[1].(string) != want {
				t.Fatalf("aux1 lock_status: got %s want %s", row[1], want)
			}
		}
	}
	reserved("reserved")
	// ROLLBACK TO undoes the write (pages restored) but NOT the lock.
	if r := db.Exec("ROLLBACK TO one"); r.Error != nil {
		t.Fatalf("rollback to: %v", r.Error)
	}
	q := db.Query("SELECT * FROM aux1.t2")
	if q.Error != nil || len(q.Rows) != 0 {
		t.Fatalf("t2 after rollback to: %v %v", q.Error, q.Rows)
	}
	reserved("reserved")
	// Full transaction end releases the lock.
	if r := db.Exec("RELEASE one"); r.Error != nil {
		t.Fatalf("release: %v", r.Error)
	}
	reserved("unlocked")
}
