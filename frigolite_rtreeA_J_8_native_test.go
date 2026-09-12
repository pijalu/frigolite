package frigolite

import (
	"strings"
	"testing"
)

// Native supersession anchors for the rtreeA/rtreeJ/rtree8 assertions whose
// TCL bodies depend on untranspilable harness procs (set_tree_depth's binary
// blob surgery, db-eval callbackprocs mutating the scanned table, and the
// long-lived-cursor SQLITE_LOCKED_VTAB guard the materializing harness cannot
// observe). Each test pins the same engine-visible contract the TCL proc
// induced. See plan/goals/P6.RTREE.md T29 and portplan/NA_EVIDENCE.md.

// queryText renders a single-cell query result as text (any value type).
func queryText(t *testing.T, db *DB, q string) string {
	t.Helper()
	r := db.Query(q)
	if r.Error != nil {
		t.Fatalf("%s: %v", q, r.Error)
	}
	if len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
		t.Fatalf("%s: no rows", q)
	}
	if r.Rows[0][0] == nil {
		return "NULL"
	}
	return formatSQLiteValue(r.Rows[0][0])
}

// TestNativeRtreeCheckEntryCountAudit pins rtreecheck's shadow-table entry
// audit (rtreeA-8.x's "Wrong number of entries in %_rowid/%_parent table"
// report lines, oracle-verified against /usr/bin/sqlite3 3.51.0).
func TestNativeRtreeCheckEntryCountAudit(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(10))

	// Remove one %_rowid mapping: the referenced cell's (rowid -> nodeno)
	// entry vanishes while the node blob still holds the cell.
	execRTree(t, db, "DELETE FROM rt_rowid WHERE rowid=5")
	report := scalarText(t, db, "SELECT rtreecheck('rt')")
	if !strings.Contains(report, "Wrong number of entries in %_rowid table") ||
		!strings.Contains(report, "expected 10, actual 9") {
		t.Fatalf("rowid-count audit missing:\n%s", report)
	}
}

// TestNativeRtreeCheckParentCountAudit pins the %_parent entry-count audit
// line of the same report family.
func TestNativeRtreeCheckParentCountAudit(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(50)) // multi-level tree: interior + parent rows

	execRTree(t, db, "DELETE FROM rt_parent WHERE nodeno=(SELECT min(nodeno) FROM rt_parent)")
	report := scalarText(t, db, "SELECT rtreecheck('rt')")
	if !strings.Contains(report, "%_parent table") {
		t.Fatalf("parent-count audit missing:\n%s", report)
	}
}

// TestNativeRtreeShadowBackupRestore pins rtreeJ-2.x's restore_t1 contract in
// engine-visible form: wholesale shadow-table replacement (DELETE + re-INSERT
// from backup copies) leaves the vtab consistent — rows added since the save
// are gone, prior rows readable, and the module keeps accepting DML.
func TestNativeRtreeShadowBackupRestore(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	execRTree(t, db, "CREATE VIRTUAL TABLE t1 USING rtree(id, x1, x2)")
	execRTree(t, db, "INSERT INTO t1 VALUES(1, 1, 2)")
	execRTree(t, db, "INSERT INTO t1 VALUES(2, 2, 3)")
	// save_t1
	execRTree(t, db, "CREATE TABLE bak_rowid AS SELECT * FROM t1_rowid")
	execRTree(t, db, "CREATE TABLE bak_node AS SELECT * FROM t1_node")
	execRTree(t, db, "CREATE TABLE bak_parent AS SELECT * FROM t1_parent")
	// A row added after the save...
	execRTree(t, db, "INSERT INTO t1 VALUES(3, 3, 4)")
	// restore_t1
	execRTree(t, db, "DELETE FROM t1_node; DELETE FROM t1_parent; DELETE FROM t1_rowid")
	execRTree(t, db, "INSERT INTO t1_node SELECT * FROM bak_node")
	execRTree(t, db, "INSERT INTO t1_parent SELECT * FROM bak_parent")
	execRTree(t, db, "INSERT INTO t1_rowid SELECT * FROM bak_rowid")

	if got := queryText(t, db, "SELECT group_concat(id) FROM (SELECT id FROM t1 ORDER BY id)"); got != "1,2" {
		t.Fatalf("restored rows: got [%s] want [1,2]", got)
	}
	// The module keeps working after wholesale shadow replacement.
	execRTree(t, db, "INSERT INTO t1 VALUES(4, 4, 5)")
	if got := queryText(t, db, "SELECT count(*) FROM t1"); got != "3" {
		t.Fatalf("post-restore insert: got [%s] want [3]", got)
	}
	if got := queryText(t, db, "SELECT rtreecheck('t1')"); got != "ok" {
		t.Fatalf("post-restore check: got [%s] want [ok]", got)
	}
}

// TestNativeRtreeInterleavedReadWrite pins rtree8's 1.1.2b contrast (the
// ORDER-BY-sorter write that must KEEP succeeding): a read of the vtab and a
// following write, across statements, stay consistent. The sibling 1.1.2/6.1
// lock assertions (SQLITE_LOCKED_VTAB "database table is locked" while a
// cursor is open) observe C's long-lived statement cursors; frigolite
// materializes each statement's rows before the next runs, so no lock state
// is observable there (documented N-A, not an engine gap this test covers).
func TestNativeRtreeInterleavedReadWrite(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(10))

	// Read pass (statement fully materialized, cursor closed).
	if got := queryText(t, db, "SELECT count(*) FROM (SELECT id FROM rt ORDER BY x1)"); got != "10" {
		t.Fatalf("read pass: got [%s] want [10]", got)
	}
	// Write pass.
	execRTree(t, db, "INSERT INTO rt VALUES(11, 110, 111)")
	// Re-read reflects the write; the sorter-ordered query keeps succeeding.
	if got := queryText(t, db, "SELECT count(*) FROM (SELECT id FROM rt ORDER BY x1 LIMIT 3)"); got != "3" {
		t.Fatalf("sorter read: got [%s] want [3]", got)
	}
	if got := queryText(t, db, "SELECT count(*) FROM rt"); got != "11" {
		t.Fatalf("count after write: got [%s] want [11]", got)
	}
}
