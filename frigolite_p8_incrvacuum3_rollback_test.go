package frigolite

import (
	"path/filepath"
	"testing"
)

// TestIncrVacuum3RollbackPreservesBTree mirrors incrvacuum3-2.3:
// BEGIN; PRAGMA incremental_vacuum=100; INSERT ...; ROLLBACK must
// leave the btree intact. Pre-fix, the engine truncates the file as
// part of the vacuum step, which corrupts the btree when the
// transaction is later rolled back. The fix is a transactional
// guard in IncrementalVacuum: if a transaction is active, the step
// is skipped and the only effect is DecrementFreelistCount(1).
func TestIncrVacuum3RollbackPreservesBTree(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Set up: page_size=1024, auto_vacuum=2 (INCREMENTAL), 256 rows.
	for _, sql := range []string{
		"PRAGMA page_size = 1024",
		"PRAGMA auto_vacuum = 2",
		"CREATE TABLE t1(x UNIQUE)",
		"INSERT INTO t1 VALUES(randomblob(400))",
		"INSERT INTO t1 VALUES(randomblob(400))",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", //   4
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", //   8
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", //  16
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", //  32
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", //  64
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", // 128
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", // 256
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("setup: %q: %v", sql, r.Error)
		}
	}
	// DELETE rowid%8 to free pages.
	if r := db.Exec("DELETE FROM t1 WHERE rowid%8"); r.Error != nil {
		t.Fatalf("DELETE: %v", r.Error)
	}
	// BEGIN + vacuum + INSERT + ROLLBACK.
	for _, sql := range []string{
		"BEGIN",
		"PRAGMA incremental_vacuum = 100",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", //  64
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", // 128
		"INSERT INTO t1 SELECT randomblob(400) FROM t1", // 256
		"ROLLBACK",
	} {
		if rr := db.Exec(sql); rr.Error != nil {
			t.Fatalf("txn step: %q: %v", sql, rr.Error)
		}
	}
	// After ROLLBACK, integrity_check must be "ok" (not empty, not an
	// error). The original testgen/incrvacuum3 fails with
	// "PRAGMA integrity_check" returning empty rows here.
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	}
	if len(r.Rows) == 0 || r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check after ROLLBACK: got %v, want [ok]", r.Rows)
	}
	// And the count must be the pre-txn value (32 rows = 256/8).
	r = db.Query("SELECT count(*) FROM t1")
	if r.Error != nil {
		t.Fatalf("count: %v", r.Error)
	}
	if r.Rows[0][0] != int64(32) {
		t.Fatalf("count after ROLLBACK: got %v, want 32", r.Rows)
	}
	// Subsequent INSERT must succeed (was: "database disk image is
	// malformed" before the fix).
	r = db.Exec("INSERT INTO t1 VALUES(randomblob(400))")
	if r.Error != nil {
		t.Fatalf("post-rollback INSERT: %v", r.Error)
	}
}
