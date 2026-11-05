package frigolite_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestWalCloseCheckpointSidecars pins the close-time lifecycle (the S7e
// sidecar-lifecycle contract, C's sqlite3WalClose at the last-connection
// close): the close-time checkpoint backfills the log into the main file,
// the "-wal"/"-shm" are deleted, the version bytes stay 2/2, and the reopen
// re-attaches the WAL from the header — the schema must survive the cycle.
func TestWalCloseCheckpointSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")

	w, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if res := w.Exec("PRAGMA page_size=1024; PRAGMA journal_mode=wal; CREATE TABLE t1(x, y); CREATE TABLE t2(x, y); INSERT INTO t1 VALUES('A', 1);"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("the -wal must be deleted at the last close (wal.c isDelete): %v", err)
	}
	if _, err := os.Stat(path + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("the -shm must be deleted at the last close: %v", err)
	}

	// The reopen re-attaches the WAL from the 2/2 header and the schema
	// (backfilled into the main file) must be intact.
	r, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if res := r.Query("PRAGMA journal_mode"); res.Error != nil || len(res.Rows) == 0 || res.Rows[0][0] != "wal" {
		t.Fatalf("journal_mode after reopen: %v %v (want wal)", res.Rows, res.Error)
	}
	if res := r.Query("SELECT count(*) FROM sqlite_master WHERE name='t1'"); res.Error != nil {
		t.Fatal(res.Error)
	} else if len(res.Rows) == 0 || res.Rows[0][0] != int64(1) {
		t.Fatalf("t1 missing after the close-time checkpoint cycle: %v", res.Rows)
	}
	if res := r.Query("SELECT count(*) FROM sqlite_master WHERE name='t2'"); res.Error != nil {
		t.Fatal(res.Error)
	} else if len(res.Rows) == 0 || res.Rows[0][0] != int64(1) {
		t.Fatalf("t2 missing after the close-time checkpoint cycle: %v", res.Rows)
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() < 2048 {
		t.Fatalf("main file %d bytes: the close-time checkpoint did not backfill the schema pages", fi.Size())
	}
	if res := r.Exec("UPDATE t1 SET y = 0 WHERE x = 'A'"); res.Error != nil {
		t.Fatalf("update after reopen: %v", res.Error)
	}
}
