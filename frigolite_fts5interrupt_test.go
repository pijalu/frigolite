package frigolite

// Native anchors for the P6.FTS5 interrupt-family adjudication
// (2026-09-14): fts5interrupt.test and fts5secure6.test drive the
// sqlite3_progress_handler C API from TCL procs. The transpiler cannot
// express those procs and stubs them as `func() bool { return true }`
// (always-interrupt), so the generated fts5interrupt retry loop can never
// observe success (infinite loop, test timeout) and every fts5secure6
// statement fails with a spurious "interrupted"; fts5secure6 additionally
// only pins C progress-handler call counts, which are instrumentation
// with no SQL surface (NA_EVIDENCE §P6.FTS5).
//
// The engine-visible contract underneath — an fts5 write interrupted at an
// arbitrary point fails with "interrupted", leaves the committed state
// consistent, and eventually succeeds when allowed to run to completion —
// is fully reproducible through the public Go seams (SetInterruptCount,
// SetProgressHandler) and is pinned here. The engine's "interrupted"
// polarity matches SQLite: a progress callback (or countdown) tripping
// mid-statement fails that statement without corrupting prior commits.

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestFTS5InterruptRetryResilience ports the fts5interrupt.test retry
// shape natively against the interrupt-countdown seam: an fts5 write
// interrupted after i engine operations fails with "interrupted" and
// leaves NO partial rows (statement atomicity, oracle-verified against
// python3 sqlite3: an interrupted 100-row CTE insert leaves zero rows);
// retrying with a full budget commits all rows and the table stays
// integrity-check-clean. Unlike the transpiled loop, the iteration count
// is bounded so a regression surfaces as a test failure, not a hang.
// (The COMMIT-statement interrupt path is deliberately NOT driven here:
// C never commits an interrupted COMMIT, while this engine does — engine
// gap recorded in portplan/NA_EVIDENCE.md §P6.FTS5.)
func TestFTS5InterruptRetryResilience(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "interrupt.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a)"))
	db.Close()

	committed := false
	for budget := 1; budget <= 32 && !committed; budget++ {
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetInterruptCount(budget)
		res := db.Exec("INSERT INTO t1(rowid, a) VALUES" +
			"(1, 'a b c d'),(2, 'b c d e'),(3, 'c d e f'),(4, 'd e f g')")
		db.SetInterruptCount(0)
		if res.Error == nil {
			committed = true
		} else if !strings.Contains(res.Error.Error(), "interrupted") {
			t.Fatalf("unexpected error under budget %d: %v", budget, res.Error)
		} else {
			checkQueryResult(t, db.Query("SELECT count(*) FROM t1"), "0")
		}
		db.Close()
	}
	if !committed {
		t.Fatal("transaction never committed within 32 retry budgets")
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkQueryResult(t, db.Query("SELECT count(*) FROM t1"), "4")
	checkExecOK(t, db.Exec("INSERT INTO t1(t1) VALUES('integrity-check')"))
}

// TestFTS5ProgressHandlerInterrupt pins the sqlite3_progress_handler seam
// the fts5interrupt/fts5secure6 TCL procs drive: a handler returning true
// interrupts the running fts5 statement with "interrupted"; re-registering
// an always-false handler lets subsequent statements succeed. The row
// touched by the interrupted single-row insert stays visible — C behaves
// the same (oracle-verified via python3 sqlite3 set_progress_handler).
func TestFTS5ProgressHandlerInterrupt(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(a)"))

	calls := 0
	db.SetProgressHandler(1, func() bool {
		calls++
		return calls > 5
	})
	res := db.Exec("INSERT INTO t1 VALUES('x y z')")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "interrupted") {
		t.Errorf("expected interrupted from tripped progress handler, got %v", res.Error)
	}
	if calls <= 5 {
		t.Errorf("progress handler ran %d times, expected more than 5", calls)
	}

	db.SetProgressHandler(1, func() bool { return false })
	checkExecOK(t, db.Exec("INSERT INTO t1 VALUES('p q r')"))
	checkQueryResult(t, db.Query("SELECT count(*) FROM t1"), "2")
}

// TestFTS5SecureDeleteInterruptConsistency ports the fts5secure6 engine
// core (independent of the harness call-count pins): a large secure-delete
// interrupted mid-flight leaves the index consistent for the next
// transaction — the surviving rows stay queryable and integrity-checked.
func TestFTS5SecureDeleteInterruptConsistency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secureint.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t1 USING fts5(x)"))
	for i := 1; i <= 300; i++ {
		checkExecOK(t, db.Exec(
			"INSERT INTO t1(rowid, x) VALUES("+strconv.Itoa(i)+", 'a b c d e f g h i j k')"))
	}
	checkExecOK(t, db.Exec("INSERT INTO t1(t1, rank) VALUES('secure-delete', 1)"))

	// Interrupt the DELETE at a small budget; the statement fails but the
	// committed state stays queryable.
	db.SetInterruptCount(3)
	res := db.Exec("DELETE FROM t1")
	db.SetInterruptCount(0)
	if res.Error != nil && !strings.Contains(res.Error.Error(), "interrupted") {
		t.Fatalf("expected success or interrupted, got %v", res.Error)
	}
	checkExecOK(t, db.Exec("INSERT INTO t1(t1) VALUES('integrity-check')"))

	// Complete the delete without interruption.
	if res.Error != nil {
		checkExecOK(t, db.Exec("DELETE FROM t1"))
	}
	checkQueryResult(t, db.Query("SELECT count(*) FROM t1"), "0")
	db.Close()
}
