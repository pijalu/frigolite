// Native Go regression test for the autovacuum-2.5.1 failure: after dropping
// every table in an auto-vacuum database (so the file shrinks to 1 page),
// the next CREATE TABLE must succeed. Two stacked bugs each produced
// "database disk image is malformed" here:
//
//  1. meta[3] (header[52:56], BTREE_LARGEST_ROOT_PAGE) went stale across
//     DROPs, so ValidateHeader rejected largestRoot > numPages. Fixed by
//     recomputing the largest remaining rootpage on every DROP TABLE
//     (pager.SetLargestRootPage + refreshLargestRootPage, mirroring
//     btree.c btreeDropTable's sqlite3BtreeUpdateMeta(p, 4, ...)).
//  2. The sqlite_schema btree root was left as a 0-cell INTERIOR page
//     (schema splits to interior once ~25 tables exist; drops free all
//     leaves). Reads tolerate it but the next INSERT cannot insert into a
//     0-cell interior root. Fixed by collapsing such roots to empty leaves
//     in clearEmptyRootRightmost (balance_shallower's end state).
//
// The table count (30) is chosen just above the threshold where the schema
// btree splits to an interior root (~25 tables at 1024-byte pages); fewer
// tables never exercise bug 2.
//
// Run with: go test -run TestNativeAutovacuumDropAllRecreate ./...
package frigolite

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestNativeAutovacuumDropAllRecreate(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("Exec %q: %v", sql, r.Error)
		}
	}
	exec("PRAGMA auto_vacuum = 1")
	const n = 30
	for i := 0; i < n; i++ {
		exec(fmt.Sprintf("CREATE TABLE t%d(x)", i))
	}
	exec("BEGIN")
	for i := 0; i < n; i++ {
		exec(fmt.Sprintf("DROP TABLE t%d", i))
	}
	exec("COMMIT")

	// The file must have shrunk back to a single page (auto-vacuum FULL
	// reclaims every freed page at COMMIT).
	r := db.Query("PRAGMA page_count")
	if r.Error != nil {
		t.Fatalf("page_count: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("page_count after drop-all = %v, want 1", r.Rows)
	}
	if r := db.Query("PRAGMA integrity_check"); r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	} else if len(r.Rows) != 1 || fmt.Sprint(r.Rows[0][0]) != "ok" {
		t.Fatalf("integrity_check = %v, want ok", r.Rows)
	}

	// The regression: this CREATE failed with "database disk image is
	// malformed" before the meta[3] + empty-root-collapse fixes.
	exec("CREATE TABLE zz(x)")
	exec("INSERT INTO zz VALUES(1)")
	qr := db.Query("SELECT name, rootpage FROM sqlite_master")
	if qr.Error != nil {
		t.Fatalf("schema read: %v", qr.Error)
	}
	if len(qr.Rows) != 1 {
		t.Fatalf("sqlite_master rows = %v, want single zz entry", qr.Rows)
	}
	// SQLite assigns rootpage 3 here (page 2 is the pointer-map page).
	if root, _ := qr.Rows[0][1].(int64); root != 3 {
		t.Fatalf("zz rootpage = %v, want 3", qr.Rows[0][1])
	}
}

// TestNativePendingBytePageNeverAllocated pins the T7.3 fix: with a
// test-harness pending-byte override (sqlite3_test_control
// TESTCTRL_PENDING_BYTE, what tester.tcl pins to 0x10000), the page holding
// the pending-byte lock (0x10000/1024 + 1 = 65 at 1K pages) is NEVER a
// usable page number. btree.c allocateBtreePage skips it on every
// end-of-file increment (src/btree.c:6740 "if( pBt->nPage==PENDING_BYTE_PAGE
// (pBt) ) pBt->nPage++") regardless of the override; autovacuum-2.4.5's
// expected root list excludes it exactly like the pointer-map pages.
//
// Pre-fix, the extend allocator treated an overridden pending-byte page as
// ordinary: an index leaf was allocated AT page 65, and when the auto-vacuum
// drain later relocated its overflow children, RelocatePage followed the
// stale ptrmap entry (Overflow1, parent=65) into the reserved page and
// failed with "update parent 65: database disk image is malformed".
//
// Run with: go test -run TestNativePendingBytePageNeverAllocated .
func TestNativePendingBytePageNeverAllocated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetPendingByte(0x10000)
	exec := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec %q: %v", sql, res.Error)
		}
	}
	exec("PRAGMA auto_vacuum = 1;\nCREATE TABLE av1(a);\nCREATE INDEX av1_idx ON av1(a);\n")
	// ~90 overflow-heavy rows push the file past page 65 (pre-fix the index
	// leaf landed exactly on the reserved page here).
	val := "x"
	for len(val) < 3500 {
		val += "x"
	}
	for i := 1; i <= 20; i++ {
		exec(fmt.Sprintf("INSERT INTO av1 (oid, a) VALUES(%d, '%s')", i, val))
	}
	// The reserved page must be zeroed, never btree content.
	pg65, err := db.pager.ReadPage(65)
	if err != nil {
		t.Fatalf("read page 65: %v", err)
	}
	for _, b := range pg65.Data {
		if b != 0 {
			t.Fatalf("page 65 (pending-byte slot) holds non-zero data")
		}
	}
	// Drains triggered by DELETE must not consult the reserved page.
	for i := 1; i <= 20; i++ {
		exec(fmt.Sprintf("DELETE FROM av1 WHERE oid = %d", i))
		// The testgen autovacuum-1.x loop runs integrity_check between the
		// per-row deletes; keep that shape (the drain path is read-order
		// sensitive — tracked as a separate FULL-SUITE-DRIFT item).
		icr := db.Query("PRAGMA integrity_check")
		if icr.Error != nil {
			t.Fatalf("integrity_check after delete %d: %v", i, icr.Error)
		}
	}
	// Root pages continue past the reserved slot.
	exec("CREATE TABLE after65(x)")
	qr := db.Query("SELECT rootpage FROM sqlite_master WHERE name='after65'")
	if qr.Error != nil || len(qr.Rows) != 1 {
		t.Fatalf("schema read: %v %v", qr.Error, qr.Rows)
	}
	if root, _ := qr.Rows[0][0].(int64); root == 65 {
		t.Fatalf("rootpage 65 allocated (pending-byte slot)")
	}
}
