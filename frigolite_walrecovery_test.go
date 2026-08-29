package frigolite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

// ---------------------------------------------------------------------------
// P7.WAL-C — native engine-contract tests.
//
// These replace the seven TCL testgen packages (e_walhook, walcrash,
// walcrash2..4, walfault, walfault2) which the transpiler cannot emit because
// they depend on harness-only crash/fault simulation (crashsql fork+abort,
// test_syscall fault VFS, the sqlite3_wal_hook C-API). The engine WAL
// subsystem (internal/pager/wal.go) is implemented and these pure-Go tests
// exercise the SAME functional contract end-to-end through the public API:
//
//   - Crash recovery (walcrash*):  a committed transaction survives a crash
//     (modeled by truncating the "-wal" at the previous commit boundary);
//     an uncommitted transaction is discarded; integrity is preserved.
//   - WAL hook (e_walhook):        sqlite3_wal_hook fires once per WAL commit
//     and NOT in rollback (legacy) mode.
//   - Fault handling (walfault*):  an I/O error during a WAL write is
//     propagated and previously-committed data remains recoverable (no
//     corruption).
// ---------------------------------------------------------------------------

// walPager returns the main database's pager so the test can inspect the WAL
// file size and simulate crashes at frame boundaries.
func walPager(t *testing.T, db *DB) *pager.Pager {
	t.Helper()
	ctxs := db.engine.DBList()
	for _, c := range ctxs {
		if c != nil && c.Pager != nil {
			return c.Pager
		}
	}
	t.Fatal("no pager on connection")
	return nil
}

// TestWalCrashRecoveryEngine mirrors walcrash-1.*: a committed transaction is
// recovered after a later transaction is lost to a crash, with integrity.
func TestWalCrashRecoveryEngine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res := db.Exec("PRAGMA journal_mode=WAL"); res.Error != nil {
		t.Fatalf("journal_mode=WAL: %v", res.Error)
	}
	if res := db.Exec("CREATE TABLE t1(a, b)"); res.Error != nil {
		t.Fatalf("CREATE: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t1 VALUES(1,1),(2,3),(3,6)"); res.Error != nil {
		t.Fatalf("INSERT T1: %v", res.Error)
	}

	// Crash boundary: size of the "-wal" after the committed T1.
	p := walPager(t, db)
	s1 := p.WalFileSize()

	// T2: a second committed transaction whose frames are appended beyond s1.
	if res := db.Exec("INSERT INTO t1 VALUES(4, 10), (5, 15)"); res.Error != nil {
		t.Fatalf("INSERT T2: %v", res.Error)
	}
	if p.WalFileSize() <= s1 {
		t.Fatal("T2 did not grow the -wal")
	}
	// Simulate a crash during T2: discard T2's frames.
	if err := os.Truncate(path+"-wal", s1); err != nil {
		t.Fatalf("truncate -wal: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and verify T1 recovered, T2 lost.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	res := db2.Query("SELECT sum(a) == max(b) FROM t1")
	if res.Error != nil {
		t.Fatalf("recovery query: %v", res.Error)
	}
	if len(res.Rows) == 0 || res.Rows[0][0] != int64(1) {
		t.Fatalf("recovered invariant = %v, want 1 (T1: sum(a)=max(b)=6)", row0(res))
	}
	cnt := db2.Query("SELECT count(*) FROM t1")
	if cnt.Error != nil {
		t.Fatalf("count: %v", cnt.Error)
	}
	if cnt.Rows[0][0] != int64(3) {
		t.Fatalf("recovered row count = %v, want 3 (T2 lost)", cnt.Rows[0][0])
	}
	// journal_mode reports WAL after recovery.
	jm := db2.Query("PRAGMA main.journal_mode")
	if jm.Error != nil || len(jm.Rows) == 0 || jm.Rows[0][0] != "wal" {
		t.Fatalf("journal_mode after recovery = %v, want wal", row0(jm))
	}
}

// TestWalHookEngine mirrors e_walhook: the WAL hook fires per WAL commit and is
// silent in rollback (legacy) mode.
func TestWalHookEngine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var hookCalls int
	db.SetWalHook(func(nLog, nCkpt int) int {
		hookCalls++
		return 0
	})

	// Rollback (legacy) mode: the hook must NOT fire.
	db.Exec("CREATE TABLE t1(x)")
	db.Exec("INSERT INTO t1 VALUES(1)")
	db.Exec("INSERT INTO t1 VALUES(2)")
	if hookCalls != 0 {
		t.Fatalf("wal hook fired %d times in rollback mode, want 0", hookCalls)
	}

	// WAL mode: the hook fires once per commit.
	if res := db.Exec("PRAGMA journal_mode=WAL"); res.Error != nil {
		t.Fatalf("journal_mode=WAL: %v", res.Error)
	}
	db.Exec("INSERT INTO t1 VALUES(3)")
	db.Exec("INSERT INTO t1 VALUES(4)")
	if hookCalls != 2 {
		t.Fatalf("wal hook fired %d times in WAL mode, want 2", hookCalls)
	}
}

// TestWalFaultHandlingEngine mirrors walfault: an I/O error during a WAL write
// is surfaced and previously-committed data stays recoverable (no corruption).
func TestWalFaultHandlingEngine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res := db.Exec("PRAGMA journal_mode=WAL"); res.Error != nil {
		t.Fatalf("journal_mode=WAL: %v", res.Error)
	}
	if res := db.Exec("CREATE TABLE t1(a)"); res.Error != nil {
		t.Fatalf("CREATE: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatalf("INSERT T1: %v", res.Error)
	}

	// Inject an I/O fault: every WAL write fails (test_syscall faultsim).
	p := walPager(t, db)
	p.SetWalFault(func(op string) error { return fmt.Errorf("injected I/O fault") })
	defer p.SetWalFault(nil)
	// The commit must fail (WAL write faulted), not silently corrupt.
	res := db.Exec("INSERT INTO t1 VALUES(2)")
	if res.Error == nil {
		t.Fatal("expected I/O error on WAL write under fault, got nil")
	}

	// Recovery still yields the committed T1 (no corruption).
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	cnt := db2.Query("SELECT count(*) FROM t1")
	if cnt.Error != nil {
		t.Fatalf("recovery count: %v", cnt.Error)
	}
	if cnt.Rows[0][0] != int64(1) {
		t.Fatalf("recovered row count after fault = %v, want 1", cnt.Rows[0][0])
	}
}

// row0 returns the first column of the first row, or nil.
func row0(r *Result) interface{} {
	if r == nil || len(r.Rows) == 0 {
		return nil
	}
	return r.Rows[0][0]
}
