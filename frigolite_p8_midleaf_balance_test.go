// Pure-Go native test for the balance_nonroot mid-leaf rebalance bug
// (P8.INCRVACUUM BUG C, autovacuum-1.4 / sqlite tcl autovacuum-1.4 shape).
//
// Scenario: 20 rows of ~3500-byte payloads in an auto-vacuum table with a
// secondary index; rows deleted in a scattered order. Deleting rowid 9
// empties a MIDDLE leaf (not the rightmost), so balance_nonroot must
// redistribute the gathered window [left..empty..right] and rebuild the
// parent's dividers from the POST-rebuild survivor pages. The pre-fix code
// read the separator keys from sibling pages BEFORE those pages were
// rebuilt (an empty leaf yielded separator 0), which produced garbage
// divider keys, dropped the window's left siblings from the parent, and
// silently discarded the rows they held.
//
// Oracle: sqlite3 CLI 3.51.0 with the same statements keeps every
// surviving row visible in rowid order and reports integrity_check "ok"
// after every step. Both the auto_vacuum=FULL and the plain (no
// auto-vacuum) variants are pinned here.
package frigolite_test

import (
	"os"
	"testing"

	"github.com/pijalu/frigolite"
)

// p8BugCDeleteOrder is the delete order from autovacuum-1.4: scattered
// rowids whose removal empties interior (non-rightmost) leaves.
var p8BugCDeleteOrder = []int{10, 3, 11, 17, 19, 20, 7, 4, 13, 6, 1, 14, 16, 12, 9}

// p8BugCFinalRowids is the surviving rowid set after the deletes.
var p8BugCFinalRowids = []int64{2, 5, 8, 15, 18}

func p8BugCPayload(i int) string {
	v := itoa(i) + "."
	for len(v) < 3500 {
		v += v
	}
	return v[:3500]
}

// p8BugCSetup creates the schema and loads 20 large rows.
func p8BugCSetup(t *testing.T, db *frigolite.DB) {
	t.Helper()
	if err := db.Exec("CREATE TABLE av1(a)").Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Exec("CREATE INDEX av1_idx ON av1(a)").Error; err != nil {
		t.Fatalf("create index: %v", err)
	}
	for i := 1; i <= 20; i++ {
		sql := "INSERT INTO av1 (oid, a) VALUES(" + itoa(i) + ", '" + p8BugCPayload(i) + "')"
		if err := db.Exec(sql).Error; err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
}

// p8BugCRowids returns the table's rowids in scan order.
func p8BugCRowids(t *testing.T, db *frigolite.DB) []int64 {
	t.Helper()
	r := db.Query("SELECT rowid FROM av1 ORDER BY rowid")
	if r.Error != nil {
		t.Fatalf("select rowids: %v", r.Error)
	}
	ids := make([]int64, 0, len(r.Rows))
	for _, row := range r.Rows {
		ids = append(ids, row[0].(int64))
	}
	return ids
}

func p8BugCCheckRowids(t *testing.T, db *frigolite.DB, deleted map[int]bool) {
	t.Helper()
	want := make([]int64, 0, len(p8BugCFinalRowids))
	for i := 1; i <= 20; i++ {
		if !deleted[i] {
			want = append(want, int64(i))
		}
	}
	got := p8BugCRowids(t, db)
	if len(got) != len(want) {
		t.Fatalf("rowid set after deletes: got %v, want %v", got, want)
	}
	for k := range got {
		if got[k] != want[k] {
			t.Fatalf("rowid order after deletes: got %v, want %v", got, want)
		}
	}
}

// TestP8BalanceMidLeafDeleteNoVacuum pins the no-auto-vacuum variant of
// the mid-leaf rebalance: the btree must not lose rows or emit duplicate
// rowids when a middle leaf empties and the parent's divider cells are
// rebuilt.
func TestP8BalanceMidLeafDeleteNoVacuum(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Exec("PRAGMA auto_vacuum = 0").Error; err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	p8BugCSetup(t, db)

	deleted := map[int]bool{}
	for _, d := range p8BugCDeleteOrder {
		deleted[d] = true
		if err := db.Exec("DELETE FROM av1 WHERE oid = " + itoa(d)).Error; err != nil {
			t.Fatalf("delete %d: %v", d, err)
		}
		p8BugCCheckRowids(t, db, deleted)
	}
}

// TestP8BalanceMidLeafDeleteAutoVacuum pins the auto_vacuum=FULL variant:
// the same rebalance plus the commit-time page drain must preserve every
// surviving row and leave integrity_check clean.
func TestP8BalanceMidLeafDeleteAutoVacuum(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Exec("PRAGMA auto_vacuum = 1").Error; err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	p8BugCSetup(t, db)

	deleted := map[int]bool{}
	for _, d := range p8BugCDeleteOrder {
		deleted[d] = true
		if err := db.Exec("DELETE FROM av1 WHERE oid = " + itoa(d)).Error; err != nil {
			t.Fatalf("delete %d: %v", d, err)
		}
		p8BugCCheckRowids(t, db, deleted)
		ic := db.Query("PRAGMA integrity_check")
		if ic.Error != nil {
			t.Fatalf("integrity_check after delete %d: %v", d, ic.Error)
		}
		if len(ic.Rows) == 0 || ic.Rows[0][0] != "ok" {
			t.Fatalf("integrity_check after delete %d: %v", d, ic.Rows[0][0])
		}
	}
}
