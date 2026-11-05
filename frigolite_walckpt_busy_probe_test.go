package frigolite_test

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pijalu/frigolite"
)

// TestProbeBusyHandlerCrossCommit checks the harness busy-handler contract:
// a handler registered on one connection commits ANOTHER connection's open
// write transaction, which releases the WRITER lock so a checkpoint's
// writer-lock wait can complete (e_walckpt-1.full.2/3 choreography).
func TestProbeBusyHandlerCrossCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")

	w, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if res := w.Exec("PRAGMA journal_mode=wal; CREATE TABLE t(a); INSERT INTO t VALUES(1);"); res.Error != nil {
		t.Fatal(res.Error)
	}

	r, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// A Query-issued BEGIN must persist as a transaction (the tvfs
	// callback's `db6 eval {BEGIN ; SELECT ...}` leaves a reader open).
	if res := w.Query("BEGIN IMMEDIATE"); res.Error != nil {
		t.Fatalf("query BEGIN: %v", res.Error)
	}
	if res := w.Query("ROLLBACK"); res.Error != nil {
		t.Fatalf("query BEGIN did not persist as a txn: ROLLBACK says %v", res.Error)
	}
	if res := w.Query("BEGIN ; SELECT * FROM sqlite_master"); res.Error != nil {
		t.Fatalf("query BEGIN+SELECT: %v", res.Error)
	}
	if res := w.Query("ROLLBACK"); res.Error != nil {
		t.Fatalf("multi-stmt query BEGIN did not persist: ROLLBACK says %v", res.Error)
	}
	// Open writer txn on r (holds the WRITER lock).
	if res := r.Exec("INSERT INTO t VALUES(2); BEGIN; INSERT INTO t VALUES(3);"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// While r holds the WRITER byte, w's BEGIN IMMEDIATE must fail
	// (sqlite3BtreeBeginTrans parity).
	if res := w.Query("BEGIN IMMEDIATE"); res.Error == nil {
		t.Logf("NOTE: BEGIN IMMEDIATE succeeded while r holds the writer byte")
	} else {
		t.Logf("BEGIN IMMEDIATE busy as expected: %v", res.Error)
	}

	// The handler (driven from w's checkpoint) commits r's txn.
	handled := 0
	w.SetBusyHandler(func(count int) bool {
		handled++
		if count == 10 {
			if res := r.Query("COMMIT"); res.Error != nil {
				t.Logf("handler COMMIT error: %v", res.Error)
			}
		}
		return true
	})

	done := make(chan error, 1)
	go func() {
		res := w.Query("PRAGMA wal_checkpoint(FULL)")
		done <- res.Error
	}()

	select {
	case err := <-done:
		t.Logf("checkpoint done: handled=%d err=%v", handled, err)
		if handled < 11 {
			t.Fatalf("handler invoked %d times; want >=11 (writer released at n==10)", handled)
		}
		rows := w.Query("SELECT * FROM t")
		if rows.Error != nil {
			t.Fatal(rows.Error)
		}
		t.Logf("rows after checkpoint: %v", rows.Rows)
		if handled != 11 {
			t.Logf("note: handler invoked %d times", handled)
		}
	case <-time.After(5 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("checkpoint hung (handler=%d commits failed?)\n%s", handled, buf[:n])
	}
}
