package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestInterruptPin pins the sqlite3_interrupt / SQLITE_TEST interrupt-count
// contract on transaction control (test/interrupt.test's interrupt_test proc
// class): SQLITE_INTERRUPT is a special error (src/vdbeaux.c:3358-3383), so
// an interrupted writing statement — including COMMIT itself — aborts AND
// rolls the whole transaction back. An interrupted COMMIT never commits:
// the transaction is gone afterwards (a later COMMIT reports "cannot commit
// - no transaction is active", interrupt-3.x).
func TestInterruptPin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	wantErr := func(sql, substr string) {
		t.Helper()
		if r := db.Exec(sql); r.Error == nil || !strings.Contains(r.Error.Error(), substr) {
			t.Fatalf("%s: want %q error, got %v", sql, substr, r.Error)
		}
	}
	must("CREATE TABLE t1(a,b)")
	must("INSERT INTO t1 VALUES(1,1)")

	// Countdown-armed autocommit write: fails "interrupted", change undone.
	db.SetInterruptCount(1)
	wantErr("INSERT INTO t1 VALUES(2,2)", "interrupted")
	db.SetInterruptCount(0)
	if r := db.Query("SELECT count(*) FROM t1"); r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Fatalf("interrupted insert must be undone, got %v / %v", r.Rows, r.Error)
	}

	// Countdown-armed COMMIT: never commits; transaction rolled back and
	// closed.
	must("BEGIN")
	must("INSERT INTO t1 VALUES(3,3)")
	db.SetInterruptCount(1)
	wantErr("COMMIT", "interrupted")
	db.SetInterruptCount(0)
	if r := db.Query("SELECT count(*) FROM t1"); r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Fatalf("interrupted COMMIT must not commit, got %v / %v", r.Rows, r.Error)
	}
	wantErr("COMMIT", "no transaction is active")

	// Interrupt FLAG at COMMIT entry (sqlite3_interrupt): same special-error
	// rollback.
	must("BEGIN")
	must("INSERT INTO t1 VALUES(4,4)")
	db.Interrupt()
	wantErr("COMMIT", "interrupted")
	if r := db.Query("SELECT count(*) FROM t1"); r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Fatalf("flag-interrupted COMMIT must not commit, got %v / %v", r.Rows, r.Error)
	}
	// The engine remains usable after the rollback.
	must("INSERT INTO t1 VALUES(5,5)")
}
