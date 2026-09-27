package frigolite

import (
	"fmt"
	"math/rand"
	"testing"
)

// T34r-split pins: the index b-tree's child-split separator chain must be
// applied ONE divider at a time with an atomic (never-partially-mutating)
// room precheck, mirroring btree.c's insertCell + balance_nonroot pair.
//
// Two defects behind the testgen/vacuum6 "btree: interior page N has no
// cells to split" flake (randomblob content decides which splits fire):
//
//   - applyChildSplitsRightmost appended its divider cells one by one and
//     could return errInteriorFull AFTER the first cell had landed, leaving
//     a cell whose leftChild equalled the (still unchanged) rightmost
//     pointer — a duplicated subtree pointer. Scans counted the duplicated
//     subtree's rows twice (harness vacuum6 4.0 read sum(length(b)) =
//     18018886 instead of 18009000: row 9886 twice), and the corruption
//     cascaded through the split retry loop until the insert died.
//   - the retry loop applied the WHOLE chain per attempt; with the
//     full-payload index dividers the chain's space need (two dividers plus
//     two carrier cells) exceeds what any 1KiB page can offer, so the left
//     page drained 7→5→3→1 cells and hit the no-cells-to-split guard.

// vacuum6SplitBuild runs the vacuum6 4.0 shape: an index over ~2000
// randomblobs sized 8000..10000 bytes, seeded so the split pattern (and the
// historical failures) are reproducible. The index is created BEFORE the
// inserts, so every row takes the incremental index-insert path.
func vacuum6SplitBuild(t *testing.T, seed int64) *DB {
	t.Helper()
	db, err := Open(t.TempDir()+"/vacuum6split.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, sql := range []string{
		"CREATE TABLE tx(a, b)",
		"CREATE INDEX i1 ON tx(b)",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rng := rand.New(rand.NewSource(seed))
	for i := 8000; i <= 10000; i++ {
		blob := make([]byte, i)
		rng.Read(blob)
		// tcl_rand's blobs are arbitrary bytes; hex-encoding keeps the
		// statement a plain literal (no bind API on this layer).
		if err := db.Exec(fmt.Sprintf("INSERT INTO tx VALUES(%d, x'%x')", i, blob)).Error; err != nil {
			t.Fatalf("seed %d, insert i=%d: %v", seed, i, err)
		}
	}
	return db
}

// vacuum6SplitCheck asserts the row count, the deterministic payload-length
// sum (length(randomblob(N)) == N, so 8000+...+10000 = 18009000) and
// integrity_check over the built tree.
func vacuum6SplitCheck(t *testing.T, db *DB, seed int64) {
	t.Helper()
	r := db.Query("SELECT count(*), sum(length(b)) FROM tx")
	if r.Error != nil {
		t.Fatalf("seed %d: stats query: %v", seed, r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("seed %d: stats rows = %d, want 1", seed, len(r.Rows))
	}
	if got := r.Rows[0][0]; got != int64(2001) {
		t.Fatalf("seed %d: count(*) = %v, want 2001", seed, got)
	}
	if got := r.Rows[0][1]; got != int64(18009000) {
		t.Fatalf("seed %d: sum(length(b)) = %v, want 18009000 (a duplicated subtree pointer double-counts rows)", seed, got)
	}
	ic := db.Query("PRAGMA integrity_check")
	if ic.Error != nil {
		t.Fatalf("seed %d: integrity_check: %v", seed, ic.Error)
	}
	if len(ic.Rows) != 1 || ic.Rows[0][0] != "ok" {
		t.Fatalf("seed %d: integrity_check = %v, want [ok]", seed, ic.Rows)
	}
}

// TestNativeVacuum6IndexSplitSeed6 pins the exact random content that made
// testgen/vacuum6 die with "btree: interior page 1848 has no cells to split"
// at insert i=9403.
func TestNativeVacuum6IndexSplitSeed6(t *testing.T) {
	db := vacuum6SplitBuild(t, 6)
	defer db.Close()
	vacuum6SplitCheck(t, db, 6)
}

// TestNativeVacuum6IndexSplitSeed34 pins the second reproduction found in
// the sweep: "btree: interior page 349 has no cells to split" at insert
// i=9364, same defect, different split pattern.
func TestNativeVacuum6IndexSplitSeed34(t *testing.T) {
	db := vacuum6SplitBuild(t, 34)
	defer db.Close()
	vacuum6SplitCheck(t, db, 34)
}
