package frigolite

// FULL-SUITE-DRIFT.T32-kernel pin: the FIRST root split of an index b-tree
// (WITHOUT ROWID table) rewrites the root from a leaf into an interior node.
// writeInteriorRootAt guarded its freeInteriorDividerChains pre-pass with the
// TREE kind (!t.isTable) instead of the PAGE type, so on a first root split
// the index-LEAF content was decoded as index-interior cells: a leaf cell
// sitting near the page end (offset 1021 on a 1024-byte page) made
// decodeIndexInteriorCell read LeftPtr past the page end and panicked with
// "slice bounds out of range [:1025] with capacity 1024" (testgen/changes
// 1.4: INSERT INTO t1 SELECT i FROM s on a WITHOUT ROWID table).
//
// btree.c parity: balance_deeper moves the root's leaf content verbatim into
// the new child (src/btree.c:8978-9040); the cells keep their own overflow
// chains — nothing is freed. Chain release applies only to DISPLACED
// INTERIOR divider cells (balance_nonroot's freePageChain), i.e. when the old
// root was already an interior index page.

import (
	"fmt"
	"strings"
	"testing"
)

// t32rowString renders a single-column result set for assertions (values
// joined by newline, nils as {}).
func t32rowString(r *Result) string {
	var lines []string
	for _, row := range r.Rows {
		if len(row) == 0 || row[0] == nil {
			lines = append(lines, "{}")
			continue
		}
		lines = append(lines, fmt.Sprint(row[0]))
	}
	return strings.Join(lines, "\n")
}

func TestT32KernelPinIndexRootSplitLeafToInterior(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// WITHOUT ROWID => index b-tree; default 1024-byte pages put the first
	// root split near ~164 entries.
	if res := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT) WITHOUT ROWID"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 500) INSERT INTO t1 SELECT i, 'row' || i FROM s"); res.Error != nil {
		t.Fatalf("insert-select: %v", res.Error)
	}
	if got := t32rowString(db.Query("SELECT count(*) FROM t1")); got != "500" {
		t.Fatalf("row count: got [%s] want [500]", got)
	}
	if got := t32rowString(db.Query("PRAGMA integrity_check")); got != "ok" {
		t.Fatalf("integrity_check: got [%s] want [ok]", got)
	}
	// A second-level split (root already interior) exercises the displaced
	// divider free path and must stay chain-consistent.
	if res := db.Exec("WITH s(i) AS (SELECT 501 UNION ALL SELECT i+1 FROM s WHERE i < 5000) INSERT INTO t1 SELECT i, 'row' || i FROM s"); res.Error != nil {
		t.Fatalf("insert-select 2: %v", res.Error)
	}
	if got := t32rowString(db.Query("SELECT count(*) FROM t1")); got != "5000" {
		t.Fatalf("row count 2: got [%s] want [5000]", got)
	}
	if got := t32rowString(db.Query("PRAGMA integrity_check")); got != "ok" {
		t.Fatalf("integrity_check 2: got [%s] want [ok]", got)
	}
	// Spot-check seeks across the split boundaries.
	for _, probe := range []int{1, 164, 165, 499, 500, 501, 2500, 5000} {
		got := t32rowString(db.Query(fmt.Sprintf("SELECT b FROM t1 WHERE a=%d", probe)))
		if want := fmt.Sprintf("row%d", probe); got != want {
			t.Fatalf("seek a=%d: got [%s] want [%s]", probe, got, want)
		}
	}
}

// TestT32KernelPinMaxVariableBind pins bind-9.5/9.7's engine contract:
// sqlite3_bind_int accepts indices up to SQLITE_MAX_VARIABLE_NUMBER (32766)
// and the bound values land in the row ("1 999 1000 1001 {} {}"). The
// testgen/bind package covers this via the regenerated emitter (dynamic
// [expr ...]/$var bind indices now emit runtime tclToInt evaluation); the
// native form guards the engine side against regression.
func TestT32KernelPinMaxVariableBind(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec("CREATE TABLE t2(a,b,c,d,e,f)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	stmt, err := db.Prepare("INSERT INTO t2(a,b,c,d) VALUES(?1,?32764,?,?)")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Finalize()
	for _, b := range []struct {
		idx int
		v   int
	}{{1, 1}, {32764, 999}, {32765, 1000}, {32766, 1001}} {
		if err := stmt.Bind(b.idx, b.v); err != nil {
			t.Fatalf("bind %d: %v", b.idx, err)
		}
	}
	if res := stmt.Exec(); res.Error != nil {
		t.Fatalf("exec: %v", res.Error)
	}
	r := db.Query("SELECT * FROM t2")
	if r.Error != nil {
		t.Fatalf("select: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("row count: got %d want 1", len(r.Rows))
	}
	row := r.Rows[0]
	for i, want := range []int64{1, 999, 1000, 1001} {
		if got, ok := row[i].(int64); !ok || got != want {
			t.Fatalf("col %d: got %#v want %d", i, row[i], want)
		}
	}
	if row[4] != nil || row[5] != nil {
		t.Fatalf("unbound cols: got %#v want nil", row[4:])
	}
}
