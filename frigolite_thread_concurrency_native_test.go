// Native contract test for the TCL thread-test family (thread003, thread004,
// thread005, notify2). The upstream tests bombard the pager's pcache module
// and exercise sqlite3_unlock_notify / sqlite3_blocking_step (test_thread.c
// demonstration APIs) from multiple TCL threads — harness constructs with no
// frigolite SQL-surface equivalent (pure Go, single-process, no C thread
// pool, no test-build mutex instrumentation). The engine-visible contract
// those tests guard is that MULTI-CONNECTION access stays correct under
// interleaved operation: concurrent writers serialize with "database is
// locked", readers coexist, and committed data is visible across
// connections. This file pins that contract with goroutine-driven
// workloads.
package frigolite

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestNativeThreadConcurrentWritersSerialize pins the locking contract the
// thread003 bombardment guards: two connections writing to the same database
// interleave without corruption. SQLite serializes per STATEMENT, not per
// loop: with no busy_timeout, either writer may drop individual INSERTs while
// the peer holds the write lock (sqlite3 3.54 oracle reproduced a final count
// of 18). The invariant is that surviving rows are unique, come only from the
// two writers' key spaces, and leave an integrity-clean file.
func TestNativeThreadConcurrentWritersSerialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thr.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if r := db1.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, v)"); r.Error != nil {
		t.Fatal(r.Error)
	}

	const perConn = 200
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	work := func(db *DB, base int) {
		defer wg.Done()
		for i := 0; i < perConn; i++ {
			r := db.Exec(fmt.Sprintf("INSERT INTO t(v) VALUES(%d)", base+i))
			if r.Error != nil && r.Error.Error() != "database is locked" {
				errs <- r.Error
				return
			}
		}
	}
	wg.Add(2)
	go work(db1, 1000000)
	go work(db2, 2000000)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("unexpected insert error: %v", e)
	}

	r := db1.Query("SELECT a, v FROM t ORDER BY a")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) == 0 || len(r.Rows) > 2*perConn {
		t.Fatalf("row count=%d, want 1..%d", len(r.Rows), 2*perConn)
	}
	seenRowID := make(map[int64]bool, len(r.Rows))
	seenValue := make(map[int64]bool, len(r.Rows))
	for _, row := range r.Rows {
		rowID, idOK := row[0].(int64)
		v, valueOK := row[1].(int64)
		if !idOK || !valueOK {
			t.Fatalf("unexpected row shape: %#v", row)
		}
		if seenRowID[rowID] || seenValue[v] {
			t.Fatalf("duplicate row after concurrent writes: rowid=%d v=%d", rowID, v)
		}
		if !((v >= 1000000 && v < 1000000+perConn) || (v >= 2000000 && v < 2000000+perConn)) {
			t.Fatalf("value outside both writers' key spaces: v=%d", v)
		}
		seenRowID[rowID] = true
		seenValue[v] = true
	}
	if r2 := db2.Query("PRAGMA integrity_check"); r2.Error != nil || fmt.Sprint(r2.Rows[0][0]) != "ok" {
		t.Fatalf("integrity after concurrent writes: %v %v", r2.Rows, r2.Error)
	}
}

// TestNativeThreadReaderDuringWrites pins the reader/writer coexistence the
// thread005/notify2 families exercise: a reader observes a consistent prefix
// of committed rows while a writer runs (never torn or duplicated data). The
// writer may lose individual INSERTs to SQLITE_BUSY with busy_timeout=0, as
// SQLite does when the reader holds SHARED at the commit upgrade.
func TestNativeThreadReaderDuringWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thrr.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	rd, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()

	if r := w.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY)"); r.Error != nil {
		t.Fatal(r.Error)
	}

	const total = 300
	// Seed one committed row synchronously so the reader always has a real
	// snapshot to observe, even if it wins the first lock race.
	inserted := 1
	if r := w.Exec("INSERT INTO t VALUES(1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	done := make(chan struct{})
	var writerErr error
	go func() {
		defer close(done)
		for i := 2; i <= total; i++ {
			r := w.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d)", i))
			if r.Error == nil {
				inserted++
				continue
			}
			// SQLite's writer uses busy_timeout=0 here: a reader holding
			// SHARED makes the commit's EXCLUSIVE upgrade fail with
			// SQLITE_BUSY, which is a clean outcome, not a failure.
			if r.Error.Error() != "database is locked" {
				writerErr = r.Error
			}
			return
		}
	}()

	consistent := true
	var readerErr error
	var last int64
	for {
		select {
		case <-done:
			if writerErr != nil {
				t.Fatalf("writer: %v", writerErr)
			}
			if readerErr != nil {
				t.Fatalf("reader: %v", readerErr)
			}
			r := rd.Query("SELECT count(*), max(a) FROM t")
			if r.Error != nil {
				t.Fatalf("final reader: %v", r.Error)
			}
			count, _ := r.Rows[0][0].(int64)
			maxRowID, _ := r.Rows[0][1].(int64)
			if count != int64(inserted) || maxRowID != int64(inserted) {
				t.Fatalf("final count=%d max=%d want %d", count, maxRowID, inserted)
			}
			if !consistent {
				t.Fatalf("saw an inconsistent snapshot mid-run")
			}
			return
		default:
			r := rd.Query("SELECT count(*) FROM t WHERE a > 0")
			if r.Error != nil {
				if r.Error.Error() == "database is locked" {
					continue // a locked read is a clean outcome
				}
				readerErr = r.Error
				<-done
				t.Fatalf("reader: %v", readerErr)
			}
			c, _ := r.Rows[0][0].(int64)
			if c < last {
				consistent = false
				<-done
				t.Fatalf("reader count went backwards: %d after %d", c, last)
			}
			last = c
		}
	}
}
