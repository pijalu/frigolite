package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Native rtreecheck tests (t6/t8 slice): the integrity function must return
// exactly 'ok' on a healthy tree and surface concrete problem lines for the
// corruption classes ext/rtree/rtree.c detects. Corruption is injected by
// rewriting the shadow tables directly, as sqlite's TCL suite does.

func TestNativeRtreeCheckHealthy(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(90))

	if got := scalarText(t, db, "SELECT rtreecheck('rt')"); got != "ok" {
		t.Fatalf("healthy tree report: %q", got)
	}
}

func TestNativeRtreeCheckUnknownTable(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	res := db.Query("SELECT rtreecheck('nosuch')")
	if res.Error == nil {
		t.Fatalf("unknown table: want error got %v", res.Rows)
	}
}

// Truncating a node blob below its 4-byte header is caught verbatim
// ("Node N is too small (M bytes)"), matching rtreeCheckNode.
func TestNativeRtreeCheckShortBlob(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(60))

	victim := scalarInt(t, db, "SELECT max(nodeno) FROM rt_node")
	execRTree(t, db, "UPDATE rt_node SET data=zeroblob(2) WHERE nodeno="+itoa64(victim))

	report := scalarText(t, db, "SELECT rtreecheck('rt')")
	if !strings.Contains(report, "Node "+itoa64(victim)+" is too small (2 bytes)") {
		t.Fatalf("short blob not reported:\n%s", report)
	}
}

// A NaN coordinate pair written over a root cell must surface the
// rtreeCheckCellCoord "is corrupt" class (min<=max invariant broken).
func TestNativeRtreeCheckCorruptCellCoords(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(120)) // interior root ⇒ cells exist on node 1

	// Node layout: header[0:4], then per cell: rowid[0:8], x-pair, y-pair.
	// Splice 8 bytes of NaN over cell 0's x-pair (bytes 13..20, 1-indexed).
	execRTree(t, db,
		"UPDATE rt_node SET data=substr(data,1,12) || x'FFFFFFFFFFFFFFFF'"+
			" || substr(data,21) WHERE nodeno=1")

	report := scalarText(t, db, "SELECT rtreecheck('rt')")
	if !strings.Contains(report, "is corrupt") {
		t.Fatalf("corrupt cell coords not reported:\n%s", report)
	}
}

// Redirecting one child->parent edge in %_parent trips the mapping audit
// ("Found (child -> wrong), expected (child -> right)").
func TestNativeRtreeCheckParentMappingMismatch(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(120))

	rows := queryIDs(t, db, "SELECT nodeno,parentnode FROM rt_parent LIMIT 1")
	if len(rows) == 0 {
		t.Skip("single-level tree; no interior mappings")
	}
	child := rows[0][0].(int64)
	execRTree(t, db,
		"UPDATE rt_parent SET parentnode=parentnode+99 WHERE nodeno="+itoa64(child))

	report := scalarText(t, db, "SELECT rtreecheck('rt')")
	want := "Found (" + itoa64(child) + " -> "
	if !strings.Contains(report, want) {
		t.Fatalf("mapping mismatch not reported:\n%s", report)
	}
}

// Depth far beyond RTREE_MAX_DEPTH in the root header must be reported as
// out of range rather than silently clamped or crashing the walk.
func TestNativeRtreeCheckDepthOutOfRange(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(50))

	// 0xC800 = 51200 big-endian in the header's depth field.
	execRTree(t, db, "UPDATE rt_node SET data=x'C800' || substr(data,3) WHERE nodeno=1")

	report := scalarText(t, db, "SELECT rtreecheck('rt')")
	if !strings.Contains(report, "Rtree depth out of range") {
		t.Fatalf("depth guard missing:\n%s", report)
	}
}

func execRTree(t *testing.T, db *DB, sql string) {
	t.Helper()
	if res := db.Exec(sql); res.Error != nil {
		t.Fatalf("exec %q: %v", sql, res.Error)
	}
}

// A query over a tree whose root blob was truncated below the header must
// fail cleanly — never panic — with the connect-time getNodeSize corruption
// message (the SELECT-side materialization goes through xConnect).
func TestNativeRtreeShortBlobQueryCleanError(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(10))

	execRTree(t, db, "UPDATE rt_node SET data=x'' WHERE rowid=1")
	db.SetDefensive(false)

	res := db.Query("SELECT id FROM rt")
	if res.Error == nil {
		t.Fatal("short root blob: query must error, got rows")
	}
	want := "database disk image is malformed"
	if !strings.Contains(res.Error.Error(), want) {
		t.Fatalf("want %q, got: %v", want, res.Error)
	}
}

// t8: ALTER TABLE RENAME cascades the three shadow tables; entries with
// hostile names (embedded quotes/spaces) must stay queryable afterwards
// (rtree1-7.1.x).
func TestNativeRtreeRenameCascade(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(20))

	execRTree(t, db, `ALTER TABLE rt RENAME TO "a ""q"" b"`)
	if got := scalarInt(t, db, `SELECT count(*) FROM "a ""q"" b"`); got != 20 {
		t.Fatalf("renamed scan: want 20 got %d", got)
	}
	// Shadow tables followed the rename.
	for _, sfx := range []string{"node", "rowid", "parent"} {
		n := scalarInt(t, db, fmt.Sprintf(`SELECT count(*) FROM "a ""q"" b_%s"`, sfx))
		if n == 0 && sfx != "parent" {
			t.Fatalf("shadow %s empty after rename", sfx)
		}
	}
	execRTree(t, db, `ALTER TABLE "a ""q"" b" RENAME TO okname`)
	if got := scalarInt(t, db, "SELECT count(*) FROM okname"); got != 20 {
		t.Fatalf("second rename: want 20 got %d", got)
	}
}

// t8: transactional semantics over the shadow tables come from the SQL layer
// itself — SAVEPOINT/ROLLBACK TO and BEGIN/ROLLBACK must leave the tree
// exactly as before, verified structurally.
func TestNativeRtreeSavepointRollback(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(30))

	execRTree(t, db, "SAVEPOINT sp")
	insertRects(t, db, seedRects(60)) // 30 more rows inside the savepoint
	if got := scalarInt(t, db, "SELECT count(*) FROM rt"); got != 60 {
		t.Fatalf("inside savepoint: want 60 got %d", got)
	}
	execRTree(t, db, "ROLLBACK TO sp")
	execRTree(t, db, "RELEASE sp")
	if got := scalarInt(t, db, "SELECT count(*) FROM rt"); got != 30 {
		t.Fatalf("after rollback: want 30 got %d", got)
	}
	if got := scalarText(t, db, "SELECT rtreecheck('rt')"); got != "ok" {
		t.Fatalf("tree integrity after rollback: %s", got)
	}

	// Delete rows in an aborted transaction.
	execRTree(t, db, "BEGIN")
	execRTree(t, db, "DELETE FROM rt WHERE id<=15")
	execRTree(t, db, "ROLLBACK")
	if got := scalarInt(t, db, "SELECT count(*) FROM rt"); got != 30 {
		t.Fatalf("after tx rollback: want 30 got %d", got)
	}
	if got := scalarText(t, db, "SELECT rtreecheck('rt')"); got != "ok" {
		t.Fatalf("integrity after delete rollback: %s", got)
	}
}
