package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPager1ShmMaterialized pins the file-backed wal-index contract used by
// pager1-35: after a WAL-mode write transaction starts, test.db-shm must exist
// and cover the wal-index page bytes the TCL fixture patches at offset 16KiB.
func TestPager1ShmMaterialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	checks := []struct {
		sql   string
		label string
	}{
		{"CREATE TABLE t1(x, y); PRAGMA journal_mode = WAL; INSERT INTO t1 VALUES(1, 2);", "after WAL insert"},
		{"BEGIN; CREATE TABLE t2(a, b);", "after write txn begin"},
	}
	for _, check := range checks {
		if r := db.Exec(check.sql); r.Error != nil {
			t.Fatalf("%s: %v", check.label, r.Error)
		}
		info, err := os.Stat(path + "-shm")
		if err != nil {
			t.Fatalf("%s: -shm file missing: %v", check.label, err)
		}
		if got := info.Size(); got < 24576 {
			t.Fatalf("%s: -shm size = %d, want >= 24576", check.label, got)
		}
	}
}
