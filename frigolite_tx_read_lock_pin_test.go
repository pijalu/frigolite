package frigolite_test

import (
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestTxReadLockStatusShared pins the pager lock model of a read inside an
// explicit transaction (backup-8.9; pager.c sqlite3PagerSharedLock): a
// deferred BEGIN alone holds no lock ("main unlocked"); the first read
// statement inside the transaction takes the SHARED file lock, reported by
// PRAGMA lock_status as "main shared" (schema reads included); COMMIT
// releases it.
func TestTxReadLockStatusShared(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"CREATE TABLE t1(a, b)",
		"INSERT INTO t1 VALUES(1, 2)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	lockStatus := func() string {
		r := db.Query("PRAGMA lock_status")
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		var main string
		for _, row := range r.Rows {
			if len(row) >= 2 {
				if name, ok := row[0].(string); ok && name == "main" {
					main = row[1].(string)
				}
			}
		}
		return main
	}
	if got := lockStatus(); got != "unlocked" {
		t.Fatalf("autocommit: got %q want unlocked", got)
	}
	if r := db.Exec("BEGIN"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := lockStatus(); got != "unlocked" {
		t.Fatalf("deferred BEGIN alone: got %q want unlocked", got)
	}
	if r := db.Query("SELECT * FROM t1"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := lockStatus(); got != "shared" {
		t.Fatalf("read in tx: got %q want shared", got)
	}
	if r := db.Query("SELECT * FROM sqlite_master"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := lockStatus(); got != "shared" {
		t.Fatalf("schema read in tx: got %q want shared", got)
	}
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := lockStatus(); got != "unlocked" {
		t.Fatalf("after COMMIT: got %q want unlocked", got)
	}
}
