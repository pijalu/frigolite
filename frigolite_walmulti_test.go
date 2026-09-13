package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// Native anchors for P7.WAL-G7 slice 1 (wal-index header + process-wide
// registry + two-connection visibility; plan/goals/P7.WAL-G7.md).

// openWalPair opens two connections on one file and puts the first in WAL
// mode, seeding a table.
func openWalPair(t *testing.T) (*DB, *DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.db")
	c1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("PRAGMA journal_mode=WAL"); res.Error != nil {
		t.Fatalf("journal_mode: %v", res.Error)
	}
	if res := c1.Exec("CREATE TABLE t1(a)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	c2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c1.Close(); c2.Close() })
	return c1, c2
}

// TestWalMultiCrossVisibility pins slice 1's core contract: a second
// connection observes the first connection's WAL-only commits (the recorded
// P7.WAL-D enabling failure was "no such table: b" — a frozen snapshot).
func TestWalMultiCrossVisibility(t *testing.T) {
	c1, c2 := openWalPair(t)

	if res := c1.Exec("INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	r := c2.Query("SELECT count(*) FROM t1")
	if r.Error != nil {
		t.Fatalf("conn2 read: %v", r.Error)
	}
	if got := formatSQLiteValue(r.Rows[0][0]); got != "1" {
		t.Errorf("conn2 sees conn1's commit: got [%s] want [1]", got)
	}

	// Reverse direction: conn2 writes, conn1 sees.
	if res := c2.Exec("INSERT INTO t1 VALUES(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	r = c1.Query("SELECT sum(a) FROM t1")
	if r.Error != nil {
		t.Fatalf("conn1 read: %v", r.Error)
	}
	if got := formatSQLiteValue(r.Rows[0][0]); got != "3" {
		t.Errorf("conn1 sees conn2's commit: got [%s] want [3]", got)
	}

	// Interleaved appends stay visible both ways.
	for i := 3; i <= 5; i++ {
		w := c1
		if i%2 == 0 {
			w = c2
		}
		if res := w.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
			t.Fatalf("interleave %d: %v", i, res.Error)
		}
	}
	r = c2.Query("SELECT count(*) FROM t1")
	if got := formatSQLiteValue(r.Rows[0][0]); got != "5" {
		t.Errorf("interleaved count: got [%s] want [5]", got)
	}
}

// TestWalMultiCheckpointVisibility pins wal_checkpoint working from the
// second connection and both connections agreeing on the checkpointed data.
func TestWalMultiCheckpointVisibility(t *testing.T) {
	c1, c2 := openWalPair(t)

	if res := c1.Exec("INSERT INTO t1 VALUES(7)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	r := c2.Query("PRAGMA wal_checkpoint")
	if r.Error != nil {
		t.Fatalf("checkpoint from conn2: %v", r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatal("checkpoint returned no row")
	}
	r = c1.Query("SELECT count(*) FROM t1")
	if got := formatSQLiteValue(r.Rows[0][0]); got != "1" {
		t.Errorf("post-checkpoint count: got [%s]", got)
	}
}

// TestWalMultiReopenRecovery pins recovery: after closing all connections,
// a fresh open replays the WAL (committed data survives; the -wal file is
// consumed or re-read through the shared index).
func TestWalMultiReopenRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "walmr.db")
	c1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c1.Exec("PRAGMA journal_mode=WAL")
	c1.Exec("CREATE TABLE t1(a)")
	c1.Exec("INSERT INTO t1 VALUES(42)")
	c1.Close()

	// The -wal sidecar must exist while the data lives only in the WAL.
	if _, err := os.Stat(path + "-wal"); err == nil {
		if fi, _ := os.Stat(path); fi.Size() < 4096*4 {
			t.Logf("data likely still in the WAL (main file %d bytes)", fi.Size())
		}
	}

	c2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	r := c2.Query("SELECT a FROM t1")
	if r.Error != nil {
		t.Fatalf("reopen read: %v", r.Error)
	}
	if len(r.Rows) != 1 || formatSQLiteValue(r.Rows[0][0]) != "42" {
		t.Errorf("recovery: got %v", r.Rows)
	}
}
