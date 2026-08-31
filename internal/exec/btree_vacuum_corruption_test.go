package exec

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

// TestVacuumDoesNotCorruptBTree reproduces the P8.INCRVACUUM corruption:
// after DELETE all + BEGIN + PRAGMA incremental_vacuum, the next query
// fails with "database disk image is malformed".
//
// The minimal scenario: 20 rows of 400 bytes on 1024-byte pages, DELETE all,
// COMMIT (freelist=0 because pages are empty but not freed), BEGIN, vacuum.
// The vacuum relocates the last page to a "free" slot. With no actual free
// pages, the relocation corrupts the btree.
//
// Expected: after vacuum, the empty table can still be queried.
func TestVacuumDoesNotCorruptBTree(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	pg, err := pager.Open(dbPath, 1024)
	if err != nil {
		t.Fatalf("open pager: %v", err)
	}
	defer pg.Close()

	e := NewEngine(pg)
	if err := e.schema.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer e.Close()

	// Set INCREMENTAL autovacuum mode (auto_vacuum=2) while DB is empty
	mustExec(t, e, "PRAGMA auto_vacuum=INCREMENTAL")

	// Create a table and insert 20 rows of 400 bytes (~2 rows per 1024-byte page)
	mustExec(t, e, "CREATE TABLE t1(x)")
	for i := 0; i < 20; i++ {
		mustExec(t, e, fmt.Sprintf("INSERT INTO t1 VALUES(randomblob(400))"))
	}

	// Commit the inserts
	mustExec(t, e, "COMMIT")

	// Delete all rows
	mustExec(t, e, "DELETE FROM t1")
	mustExec(t, e, "COMMIT")

	// Check freelist count (may be 0 if pages weren't freed)
	r := mustExec(t, e, "PRAGMA freelist_count")
	t.Logf("Freelist count before vacuum: %v", r.Rows)

	// Begin + vacuum
	mustExec(t, e, "BEGIN")
	mustExec(t, e, "PRAGMA incremental_vacuum = 100")

	// The query after vacuum must succeed
	r = mustExec(t, e, "SELECT count(*) FROM t1")
	t.Logf("Rows after vacuum: %v", r.Rows)
}
