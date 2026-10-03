package frigolite

// Regression pin for the PERF.DML2 freeblock-port corruption report
// (fleet/fix-dml2): a point UPDATE that GROWS a blob toward the page size
// on a 65536-byte page failed with "database disk image is malformed".
//
// Root cause: storage.validatePageHeader compared the page's freeblock
// chain head with uint16(pageSize), which truncates 65536 to 0 — every
// valid nonzero head on a 64KiB page read as corrupt. The check was
// unreachable for engine-written pages before the btree.c dropCell/
// freeSpace port (point deletes compacted the page, so FirstFree stayed
// 0); the O(1) freeblock accounting leaves real freeblocks behind, and
// the next header parse of the touched page failed. sqlite3 3.x oracle
// (page_size=65536, same chain) returns ok with length(b)=3000/64000/3000.

import (
	"fmt"
	"testing"
)

func execOK(t *testing.T, db *DB, step int, sqlStr string) {
	t.Helper()
	if err := db.Exec(sqlStr).Error; err != nil {
		t.Fatalf("step %d (%.60s): %v", step, sqlStr, err)
	}
}

// blobGrowthVariant drives one grow-shape to completion: insert nRows
// base-sized blobs, optionally shrink them all, then grow one row (or all
// rows) to grown bytes. Every blob must round-trip at its final size and
// PRAGMA integrity_check must report ok.
func blobGrowthVariant(t *testing.T, pageSize int, nRows, base, finalBase, growRow, grown int, growAll bool) {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	execOK(t, db, 0, fmt.Sprintf("PRAGMA page_size=%d", pageSize))
	execOK(t, db, 1, "CREATE TABLE t1(a INTEGER PRIMARY KEY, b BLOB)")
	execOK(t, db, 2, fmt.Sprintf("WITH RECURSIVE c(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM c WHERE i<%d) INSERT INTO t1(a,b) SELECT i, zeroblob(%d) FROM c", nRows, base))
	if finalBase != base {
		execOK(t, db, 3, fmt.Sprintf("UPDATE t1 SET b=zeroblob(%d)", finalBase))
	}
	if growAll {
		execOK(t, db, 4, fmt.Sprintf("UPDATE t1 SET b=zeroblob(%d)", grown))
	} else {
		execOK(t, db, 4, fmt.Sprintf("UPDATE t1 SET b=zeroblob(%d) WHERE a=%d", grown, growRow))
	}
	rows := db.Query("SELECT a, length(b) FROM t1 ORDER BY a")
	if rows.Error != nil {
		t.Fatalf("readback: %v", rows.Error)
	}
	for _, r := range rows.Rows {
		a := r[0].(int64)
		want := finalBase
		if growAll || a == int64(growRow) {
			want = grown
		}
		if got := r[1].(int64); got != int64(want) {
			t.Fatalf("row %d: length(b)=%d want %d", a, got, want)
		}
	}
	res := db.Query("PRAGMA integrity_check")
	if res.Error != nil {
		t.Fatalf("integrity_check: %v", res.Error)
	}
	if s := fmt.Sprint(res.Rows[0][0]); s != "ok" {
		t.Fatalf("integrity_check: %s", s)
	}
}

func TestUpdateBlobGrowth64KiBPage(t *testing.T) {
	// Full repro chain: 30x6500, shrink all to 3000, grow row 2 to 64000.
	blobGrowthVariant(t, 65536, 30, 6500, 3000, 2, 64000, false)
	// No shrink: grow row 2 directly to 64000.
	blobGrowthVariant(t, 65536, 30, 6500, 6500, 2, 64000, false)
	// Fresh 30x3000, grow row 2 to 64000 (shrink irrelevant).
	blobGrowthVariant(t, 65536, 30, 3000, 3000, 2, 64000, false)
	// Update ALL rows to 64000 (the multi-row path that always worked).
	blobGrowthVariant(t, 65536, 30, 6500, 3000, 0, 64000, true)
	// Small-page control: 4096-byte pages, 300 -> 3000.
	blobGrowthVariant(t, 4096, 30, 650, 300, 2, 3000, false)
	// Delete+reinsert leaves a freeblock a later parse must accept.
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	execOK(t, db, 0, "PRAGMA page_size=65536")
	execOK(t, db, 1, "CREATE TABLE t2(x INTEGER PRIMARY KEY, y BLOB)")
	execOK(t, db, 2, "INSERT INTO t2 VALUES(1, zeroblob(60000)), (2, zeroblob(6000)), (3, zeroblob(6000))")
	execOK(t, db, 3, "DELETE FROM t2 WHERE x=2")
	execOK(t, db, 4, "INSERT INTO t2 VALUES(4, zeroblob(1000))")
	rows := db.Query("SELECT count(*) FROM t2")
	if rows.Error != nil {
		t.Fatalf("count: %v", rows.Error)
	}
	if got := rows.Rows[0][0].(int64); got != 3 {
		t.Fatalf("count(*)=%d want 3", got)
	}
	res := db.Query("PRAGMA integrity_check")
	if res.Error != nil {
		t.Fatalf("integrity_check: %v", res.Error)
	}
	if s := fmt.Sprint(res.Rows[0][0]); s != "ok" {
		t.Fatalf("integrity_check: %s", s)
	}
}
