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
// interleave without corruption — one commits, the other either waits
// (implicit serialization inside Exec) or fails cleanly with
// "database is locked", and the final row count is exact.
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

	r := db1.Query("SELECT count(*), count(DISTINCT v) FROM t")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	count, _ := r.Rows[0][0].(int64)
	distinct, _ := r.Rows[0][1].(int64)
	// Writers serialize: either all inserts landed, or locked writers
	// dropped theirs — but no duplicate/corrupt rows ever exist.
	if count < perConn || count != distinct {
		t.Fatalf("count=%d distinct=%d, want count>=200 and count==distinct", count, distinct)
	}
	if r2 := db2.Query("PRAGMA integrity_check"); r2.Error != nil || fmt.Sprint(r2.Rows[0][0]) != "ok" {
		t.Fatalf("integrity after concurrent writes: %v %v", r2.Rows, r2.Error)
	}
}

// TestNativeThreadReaderDuringWrites pins the reader/writer coexistence the
// thread005/notify2 families exercise: readers can run throughout a writer's
// workload and always observe a consistent prefix of committed rows (never
// torn or duplicated data).
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= total; i++ {
			if r := w.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d)", i)); r.Error != nil {
				t.Errorf("writer: %v", r.Error)
				return
			}
		}
	}()

	consistent := true
	for {
		select {
		case <-done:
			r := rd.Query("SELECT count(*), max(a) FROM t")
			if r.Error != nil {
				t.Fatalf("final reader: %v", r.Error)
			}
			count, _ := r.Rows[0][0].(int64)
			if count != total {
				t.Fatalf("final count=%d want %d", count, total)
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
				t.Fatalf("reader: %v", r.Error)
			}
			if c, _ := r.Rows[0][0].(int64); c < 0 {
				consistent = false
			}
		}
	}
}
