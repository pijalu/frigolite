package frigolite

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWalSetlkRecoverEngine ports walsetlk_recover's essential assertions
// (the testfixture_nb subprocess class — tester.tcl's background recovery
// against a delayed VFS read — is not transpilable): a concurrent
// recovery-in-progress holds the wal-index recovery lock, so a second
// connection's read reports "database is locked" once its busy timeout
// expires, with the busy path sleeping through the VFS xSleep hook
// (the 1.5.2 sleep_count>0 observable); the read succeeds once the
// recovery completes (1.4's post-recovery rows).
func TestWalSetlkRecoverEngine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// 1.0: the WAL database with rows.
	db0, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res := db0.Exec("PRAGMA journal_mode=wal; CREATE TABLE t1(a,b);" +
		"INSERT INTO t1 VALUES(1,2),(3,4),(5,6)"); res.Error != nil {
		t.Fatalf("setup: %v", res.Error)
	}
	// db0 STAYS OPEN: the -wal holds the committed frames, the recovery
	// below has a log to rebuild from (C's db_save/db_restore pair leaves
	// the same state).

	// The main side opens clean (its attach recovered the live index), with
	// the busy timeout and the xSleep counter (1.5.2's sleep_count).
	sleeps := 0
	conn1, err := Open(path)
	if err != nil {
		t.Fatalf("Open conn1: %v", err)
	}
	conn1.engine.InstallVFSHook(func(op, hookPath string, details []string) error {
		if op == "xSleep" {
			sleeps++
		}
		return nil
	}, -1)
	if res := conn1.Exec("PRAGMA busy_timeout=500"); res.Error != nil {
		t.Fatalf("busy_timeout: %v", res.Error)
	}

	// The recovery side: the shared wal-index is corrupted, so its next
	// read runs the recovery (the WRITER shm lock, wal.c's walIndexRecover)
	// — the recovery's -wal xRead parks on the hook until the main side has
	// timed out, the testfixture_nb subprocess stand-in.
	release := make(chan struct{})
	conn2, err := Open(path)
	if err != nil {
		t.Fatalf("Open conn2: %v", err)
	}
	conn2.engine.InstallVFSHook(func(op, hookPath string, details []string) error {
		if op == "xRead" && strings.HasSuffix(hookPath, "-wal") {
			<-release
		}
		return nil
	}, -1)
	conn2.engine.Pager().CorruptWalIndex()
	done := make(chan error, 1)
	go func() {
		done <- conn2.Query("SELECT * FROM t1").Error
	}()
	// conn2 enters the recovery (the xRead hook parks inside it, the
	// recovery lock held).
	time.Sleep(300 * time.Millisecond)

	// conn1's read: the corrupted index forces its own recovery attempt,
	// the WRITER lock is held by conn2's in-flight recovery — BUSY_RECOVERY
	// retried until the busy timeout expires (1.2's "database is locked"),
	// the retries sleeping through xSleep.
	qerr := conn1.Query("SELECT * FROM t1").Error
	if qerr == nil || !strings.Contains(qerr.Error(), "database is locked") {
		t.Fatalf("read under recovery: got %v, want \"database is locked\"", qerr)
	}
	if sleeps == 0 {
		t.Fatalf("the busy path never slept (the xSleep hook never fired)")
	}

	// The recovery completes: the read works (1.4's rows).
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("recovery-side read: %v", err)
	}
	res := conn1.Query("SELECT * FROM t1")
	if res.Error != nil {
		t.Fatalf("post-recovery read: %v", res.Error)
	}
	if got := renderRows(res.Rows); got != "1 2 3 4 5 6 " {
		t.Fatalf("post-recovery rows = %q, want \"1 2 3 4 5 6 \"", got)
	}
	db0.Close()
	conn1.Close()
	conn2.Close()
}

