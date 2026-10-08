package frigolite

// R12.UPD pin: the pager's commit write path grows the database file
// IMPLICITLY in the page writes (pager_write_pagelist parity — C never
// ftruncates to extend; frigolite dropped the per-page file.Truncate in
// favor of one batched quota gate before the write loop). These pins hold
// the file-shape contract that the removed truncate used to guarantee:
//
//   - every commit leaves the file exactly numPages*pageSize bytes, with
//     every written page readable back (the ascending-page WriteAt chain
//     must produce the same image truncate-then-write produced);
//   - an aborted (rolled back) transaction that had allocated pages beyond
//     the pre-transaction EOF restores both content and file length;
//   - max_page_count refusals and VACUUM/DROP shrinks keep the header
//     size, the freelist and the physical file in lockstep;
//   - the batched quota gate still refuses total growth past the group
//     limit (the refusal now lands before ANY page of the flush moved).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/quota"
)

func openGrowthPinDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "growth.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// queryGrowthPinInt runs a one-value query and returns its integer result.
func queryGrowthPinInt(t *testing.T, db *DB, q string) int {
	t.Helper()
	res := db.Query(q)
	if res.Error != nil {
		t.Fatalf("query %q: %v", q, res.Error)
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		t.Fatalf("query %q: no rows", q)
	}
	switch v := res.Rows[0][0].(type) {
	case int64:
		return int(v)
	case int:
		return v
	default:
		t.Fatalf("query %q: non-integer result %T", q, res.Rows[0][0])
		return 0
	}
}

// TestCommitGrowthPin_FileTracksPageWrites verifies the implicit-growth
// commit: after each autocommit INSERT batch that allocates new pages, the
// file size equals the header page count * page size and every row reads
// back (ascending WriteAt chain == truncate-then-write image).
func TestCommitGrowthPin_FileTracksPageWrites(t *testing.T) {
	db, path := openGrowthPinDB(t)
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, pad TEXT)")
	// ~220 rows x ~200B pad on 4096B pages: forces many page allocations.
	for i := 1; i <= 220; i++ {
		if r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d,'%s')", i, strings.Repeat("x", 200))); r.Error != nil {
			t.Fatalf("insert %d: %v", i, r.Error)
		}
		checkFileShape(t, db, path)
	}
	if n := queryGrowthPinInt(t, db, "SELECT COUNT(*) FROM t"); n != 220 {
		t.Fatalf("rows = %d, want 220", n)
	}
	checkFileShape(t, db, path)
}

// checkFileShape asserts file-size == in-header page count * page size.
func checkFileShape(t *testing.T, db *DB, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() == 0 {
		return // lazy creation: no write materialized the file yet
	}
	pageCount := queryGrowthPinInt(t, db, "PRAGMA page_count")
	pageSize := queryGrowthPinInt(t, db, "PRAGMA page_size")
	want := int64(pageCount) * int64(pageSize)
	if st.Size() != want {
		t.Fatalf("file size %d != page_count %d * page_size %d = %d", st.Size(), pageCount, pageSize, want)
	}
}

// TestCommitGrowthPin_RollbackRestoresLength verifies a rolled back
// transaction that extended the file: playback restores the before-images
// AND the file length (journalDBOrigSize truncation), and the surviving
// content stays readable.
func TestCommitGrowthPin_RollbackRestoresLength(t *testing.T) {
	db, path := openGrowthPinDB(t)
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, pad TEXT)")
	for i := 1; i <= 50; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES(%d,'%s')", i, strings.Repeat("x", 200)))
	}
	st0, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if r := db.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("begin: %v", r.Error)
	}
	for i := 51; i <= 200; i++ {
		if r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d,'%s')", i, strings.Repeat("x", 200))); r.Error != nil {
			t.Fatalf("insert %d: %v", i, r.Error)
		}
	}
	if r := db.Exec("ROLLBACK"); r.Error != nil {
		t.Fatalf("rollback: %v", r.Error)
	}
	st1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st1.Size() > st0.Size() {
		t.Fatalf("file grew past the pre-transaction size after ROLLBACK: %d > %d", st1.Size(), st0.Size())
	}
	if n := queryGrowthPinInt(t, db, "SELECT COUNT(*) FROM t"); n != 50 {
		t.Fatalf("rows after rollback = %d, want 50", n)
	}
	checkFileShape(t, db, path)
}

// TestCommitGrowthPin_MaxPageCountAndVacuumShapes pins the shrink paths
// next to the implicit growth: max_page_count refusals keep the file at the
// cap (no partially-grown tail), and VACUUM shrinks the physical file with
// content intact.
func TestCommitGrowthPin_MaxPageCountAndVacuumShapes(t *testing.T) {
	db, path := openGrowthPinDB(t)
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, pad TEXT)")
	mustExec(t, db, "PRAGMA max_page_count = 10")
	fullErrs := 0
	for i := 1; i <= 100; i++ {
		r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d,'%s')", i, strings.Repeat("x", 400)))
		if r.Error != nil {
			fullErrs++
			if !strings.Contains(r.Error.Error(), "database or disk is full") {
				t.Fatalf("insert %d: unexpected error %v", i, r.Error)
			}
		}
	}
	if fullErrs == 0 {
		t.Fatalf("max_page_count=10 never refused an insert")
	}
	checkFileShape(t, db, path)
	mustExec(t, db, "PRAGMA max_page_count = 0")
	// VACUUM rebuilds the image: the physical file must shrink back to the
	// live page count with every surviving row readable.
	mustExec(t, db, "DELETE FROM t WHERE id > 5")
	before := queryGrowthPinInt(t, db, "PRAGMA page_count")
	mustExec(t, db, "VACUUM")
	after := queryGrowthPinInt(t, db, "PRAGMA page_count")
	if after >= before {
		t.Fatalf("VACUUM did not shrink: page_count %d -> %d", before, after)
	}
	checkFileShape(t, db, path)
	if n := queryGrowthPinInt(t, db, "SELECT COUNT(*) FROM t"); n != 5 {
		t.Fatalf("rows after vacuum = %d, want 5", n)
	}
}

// TestCommitGrowthPin_QuotaRefusesBatchedGrowth verifies the ONE batched
// quota gate: total flush growth past the group limit is refused before any
// page of that commit lands (the old per-page checks refused mid-loop; the
// observable contract — SQLITE_FULL, no over-limit file — is unchanged).
func TestCommitGrowthPin_QuotaRefusesBatchedGrowth(t *testing.T) {
	if code := quota.Initialize("", true); code != quota.OK {
		t.Fatalf("quota init: %v", code)
	}
	t.Cleanup(func() { quota.Shutdown() })
	dir := t.TempDir()
	path := filepath.Join(dir, "qgrowth.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, pad TEXT)")
	mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES(1,'%s')", strings.Repeat("x", 100)))
	if code := quota.Set("*"+filepath.Base(dir)+"*/qgrowth.db", 2*4096, nil); code != quota.OK {
		t.Fatalf("quota set: %v", code)
	}
	// 100 rows x 1000B: ~10 pages of growth — far past the 2-page cap.
	refused := false
	for i := 2; i <= 101; i++ {
		r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d,'%s')", i, strings.Repeat("x", 1000)))
		if r.Error != nil {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatalf("quota limit never refused growth")
	}
	checkFileShape(t, db, path)
}
