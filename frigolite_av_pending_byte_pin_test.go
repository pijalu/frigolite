package frigolite_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestAutoVacuumPendingByteDrain pins the auto-vacuum FULL drain accounting
// when the PENDING_BYTE page sits inside the drain window (autovacuum-1.x
// with tester.tcl's pinned pending byte 0x10000, i.e. page 65 at a 1024-byte
// page size): btree.c finalDbSize decrements nFin once when the shrink
// crosses the pending-byte page, because that page can never be reclaimed —
// without the adjustment the chain is zeroed while a free page below nFin
// stays in the file, and integrity_check reports "Page N: never used".
func TestAutoVacuumPendingByteDrain(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetPendingByte(0x10000)
	step := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	step("PRAGMA page_size = 1024")
	step("PRAGMA auto_vacuum = 1")
	step("CREATE TABLE av1(a)")
	step("CREATE INDEX av1_idx ON av1(a)")
	fill := func() {
		for i := 1; i <= 20; i++ {
			payload := strings.Repeat(fmt.Sprintf("%d.", i), 3500/len(fmt.Sprintf("%d.", i))+1)[:3500]
			step(fmt.Sprintf("INSERT INTO av1 (oid, a) VALUES(%d, '%s')", i, payload))
		}
	}
	// Two delete-order iterations over one connection mirror the
	// autovacuum-1 loop: fill, drain by ordered deletes, refill.
	orders := [][]string{
		{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20"},
		{"20", "19", "18", "17", "16", "15", "14", "13", "12", "11", "10", "9", "8", "7", "6", "5", "4", "3", "2", "1"},
		{"19 8 17 15", "16 11 9 14", "18 5 3 1", "13 20 7 2", "6 12"},
	}
	for oi, order := range orders {
		fill()
		for _, d := range order {
			step("DELETE FROM av1 WHERE oid = " + strings.ReplaceAll(d, " ", " OR oid = "))
			r := db.Query("PRAGMA integrity_check")
			if r.Error != nil {
				t.Fatalf("order %d delete (%s): %v", oi+1, d, r.Error)
			}
			if got := flatRows(r); got != "ok" {
				t.Fatalf("order %d delete (%s): integrity_check got %q want ok", oi+1, d, got)
			}
		}
	}
}
