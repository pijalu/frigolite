package frigolite_test

// Pin test for the sqlite3_busy_handler contract exercised by lock.test
// 2.3.x/2.4.x (FULL-SUITE-DRIFT pairs-pager cluster).
//
// C semantics (main.c sqlite3BusyHandler, pager.c pager_wait_on_lock and the
// sqlite3PagerSetBusyHandler transition table):
//   - each failed lock attempt invokes the handler with the retry count and
//     retries while it returns non-zero (true here);
//   - a connection that already holds its own SHARED read transaction fails
//     the SHARED→RESERVED upgrade WITHOUT invoking the handler;
//   - the TCL binding (tclsqlite.c DbBusyHandler) aborts on TCL_BREAK or a
//     truthy integer result and retries otherwise.
//
// With db holding a write transaction on the same file:
//   - db2 with an abort-on-first-call handler records count 0 and gets
//     "database is locked" (lock-2.3.1);
//   - db2 with a handler that retries five times records 0 1 2 3 4 5
//     (lock-2.4.1);
//   - db2 holding a read transaction records nothing (lock-2.3.2).

import (
	"strconv"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestLockBusyHandlerContract(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t1(a, b)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t1 VALUES(1, 2)"); r.Error != nil {
		t.Fatalf("insert: %v", r.Error)
	}

	// db holds the write transaction for the whole test.
	if r := db.Exec("BEGIN; UPDATE t1 SET a = 0 WHERE 0"); r.Error != nil {
		t.Fatalf("db txn: %v", r.Error)
	}

	db2, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	var seen []string
	db2.SetBusyHandler(func(count int) bool {
		seen = append(seen, strconv.Itoa(count))
		return false // abort on the first call (lock-2.3.1's `break`)
	})
	if r := db2.Exec("UPDATE t1 SET a=b, b=a"); r.Error == nil {
		t.Fatalf("want database is locked, got success")
	}
	if len(seen) != 1 || seen[0] != "0" {
		t.Fatalf("handler calls: %v, want [0]", seen)
	}

	// Retry five times, then abort (lock-2.4.1's `if {$count>4} break`).
	seen = nil
	db2.SetBusyHandler(func(count int) bool {
		seen = append(seen, strconv.Itoa(count))
		return count <= 4
	})
	if r := db2.Exec("UPDATE t1 SET a=b, b=a"); r.Error == nil {
		t.Fatalf("want database is locked, got success")
	}
	want := []string{"0", "1", "2", "3", "4", "5"}
	if len(seen) != len(want) {
		t.Fatalf("handler calls: %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("handler calls: %v, want %v", seen, want)
		}
	}

	// db2 now holds a read transaction: no busy callback at all
	// (lock-2.3.2 — the SHARED→RESERVED upgrade never invokes the handler).
	seen = nil
	if r := db2.Query("BEGIN; SELECT rowid FROM sqlite_master LIMIT 1"); r.Error != nil {
		t.Fatalf("db2 read txn: %v", r.Error)
	}
	if r := db2.Exec("UPDATE t1 SET a=b, b=a"); r.Error == nil {
		t.Fatalf("want database is locked, got success")
	}
	if len(seen) != 0 {
		t.Fatalf("handler calls with own read txn: %v, want none", seen)
	}
}
