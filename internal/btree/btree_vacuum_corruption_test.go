package btree

import (
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/pager"
)

// TestVacuumDoesNotCorruptBTree reproduces the P8.INCRVACUUM corruption:
// after DELETE all + BEGIN + PRAGMA incremental_vacuum, the next query
// or INSERT fails with "database disk image is malformed".
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
	pg.SetAutoVacuum(2)

	eng := exec.NewEngine(pg)
	defer eng.Close()

	// Create a table and insert 20 rows of 400 bytes (~2 rows per 1024-byte page)
	if _, err := eng.ExecString("CREATE TABLE t1(x)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 20; i++ {
		if _, err := eng.ExecString("INSERT INTO t1 VALUES(randomblob(400))"); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Commit
	if _, err := eng.ExecString("COMMIT"); err != nil {
		t.Fatalf("commit after inserts: %v", err)
	}

	// Delete all rows
	if _, err := eng.ExecString("DELETE FROM t1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := eng.ExecString("COMMIT"); err != nil {
		t.Fatalf("commit after delete: %v", err)
	}

	// Check freelist count
	r, err := eng.QueryString("PRAGMA freelist_count")
	if err != nil {
		t.Fatalf("freelist_count: %v", err)
	}
	t.Logf("Freelist count before vacuum: %v", r.Rows)

	// Begin + vacuum
	if _, err := eng.ExecString("BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := eng.QueryString("PRAGMA incremental_vacuum = 100"); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	// The query after vacuum must succeed
	r, err = eng.QueryString("SELECT count(*) FROM t1")
	if err != nil {
		t.Fatalf("query after vacuum: %v", err)
	}
	t.Logf("Rows after vacuum: %v", r.Rows)
}
