// P8.INCRVACUUM phase 5.5 regression: after DELETE all from a multi-leaf
// btree, the root (if interior) must drop its stale rightmost-child
// pointer to the last freed leaf. Without this, the next read follows
// the stale pointer into a freed page whose first 4 bytes are a
// freelist chain pointer (interpreted as a cell pointer) and fails
// with "database disk image is malformed". The btree.c reference
// (balance_shallower) collapses the root into its only child or
// converts it to a leaf when the last child is freed; this engine's
// simplified port implements the minimal equivalent: clear the
// rightmost-child when the root has 0 cells and the rightmost-child
// is on the freelist.
//
// This test reproduces the minimal scenario, asserts SELECT works
// both before and after PRAGMA incremental_vacuum, and exercises the
// vacuum's Truncate path (the on-disk header must be flushed so the
// truncated file size matches the header's nPage, otherwise
// HeaderBeyondFile reports corruption on the next statement).
package exec

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

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

	mustExec(t, e, "PRAGMA auto_vacuum=INCREMENTAL")
	mustExec(t, e, "CREATE TABLE t1(x)")
	for i := 0; i < 20; i++ {
		mustExec(t, e, fmt.Sprintf("INSERT INTO t1 VALUES(randomblob(400))"))
	}
	mustExec(t, e, "COMMIT")

	// Sanity: SELECT works after inserts.
	if r := mustExec(t, e, "SELECT count(*) FROM t1"); len(r.Rows) == 0 || r.Rows[0][0] != int64(20) {
		t.Fatalf("expected count=20 after inserts, got %v", r.Rows)
	}

	mustExec(t, e, "DELETE FROM t1")
	mustExec(t, e, "COMMIT")

	// After DELETE all, the btree must still be navigable. The fix
	// (clearEmptyRootRightmost + cursor RightmostPtr==0 handling)
	// makes the root a 0-cell interior page with rmp=0, which the
	// cursor treats as an empty subtree.
	if r := mustExec(t, e, "SELECT count(*) FROM t1"); len(r.Rows) == 0 || r.Rows[0][0] != int64(0) {
		t.Fatalf("expected count=0 after DELETE, got %v (err=%v)", r.Rows, r.Error)
	}

	// Vacuum must not corrupt the btree. The Truncate path writes
	// the updated header to disk so the next statement's
	// HeaderBeyondFile check sees a consistent file.
	mustExec(t, e, "BEGIN")
	mustExec(t, e, "PRAGMA incremental_vacuum = 100")
	if r := mustExec(t, e, "SELECT count(*) FROM t1"); len(r.Rows) == 0 || r.Rows[0][0] != int64(0) {
		t.Fatalf("expected count=0 after vacuum, got %v (err=%v)", r.Rows, r.Error)
	}
}
