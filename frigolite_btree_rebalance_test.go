package frigolite_test

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestBtreeRebalanceUnderChompLoad guards the two btree balance bugs behind
// the fts4opt [SEG3] failure mass: (1) insertInteriorPage's single-split retry
// left full interior pages unabsorbed, making InsertCell fail outright — and
// since UPDATE is deleteRowCells+InsertCell, the swallowed error silently
// deleted rows; (2) cascadeChildless unlinked a childless rightmost child by
// zeroing the parent's rightmost pointer, leaving an interior page with N
// cells but only N children, which made later cursor walks terminate early.
// The scenario mirrors the chomp's workload: ~1000 rows with ~1KB payloads,
// then in-place UPDATEs whose new payloads are both smaller and larger than
// the originals, forcing splits and rebalances; every rowid must survive with
// the right content.
func TestBtreeRebalanceUnderChompLoad(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	hexBlob := func(n int, seed byte) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = seed + byte(i%251)
		}
		return "X'" + hex.EncodeToString(b) + "'"
	}

	if res := db.Exec(`CREATE TABLE seg(blockid INTEGER PRIMARY KEY, block BLOB)`); res.Error != nil {
		t.Fatal(res.Error)
	}
	// ~1000 contiguous rows with ~1KB payloads, then sparse high rowids, then
	// a NULL marker far above (the merge writer's reservation marker).
	for id := 1; id <= 1081; id++ {
		if res := db.Exec(fmt.Sprintf(`INSERT INTO seg VALUES(%d, %s)`, id, hexBlob(900+id%100, 1))); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	for id := 1082; id <= 1200; id += 80 {
		if res := db.Exec(fmt.Sprintf(`INSERT INTO seg VALUES(%d, %s)`, id, hexBlob(64, 2))); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	if res := db.Exec(`INSERT INTO seg VALUES(7154, NULL)`); res.Error != nil {
		t.Fatal(res.Error)
	}

	// The chomp's in-place rewrites: UPDATE ~20 rows to new payloads sized
	// 129..962 bytes (some larger, some smaller than the originals).
	sizes := []int{129, 204, 109, 136, 145, 908, 854, 916, 890, 883, 914, 890, 962, 892, 917, 911, 896, 939, 865, 875}
	for i, sz := range sizes {
		id := 104 + i*47
		if id > 1081 {
			id = 1081
		}
		if res := db.Exec(fmt.Sprintf(`UPDATE seg SET block = %s WHERE blockid = %d`, hexBlob(sz, 3), id)); res.Error != nil {
			t.Fatal(res.Error)
		}
	}

	// Scan the whole table once: every rowid must appear exactly once, in
	// order, and the scan must terminate at the marker.
	r := db.Query(`SELECT blockid FROM seg ORDER BY blockid`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	seen := map[int]int{}
	prev := 0
	for _, row := range r.Rows {
		var id int
		fmt.Sscan(fmt.Sprint(row[0]), &id)
		seen[id]++
		if id <= prev {
			t.Fatalf("rowids out of order: %d after %d", id, prev)
		}
		prev = id
	}
	if want := 1081 + 2 + 1; len(seen) != want { // contiguous + 1082,1162 + marker
		t.Fatalf("distinct rowids: got %d want %d", len(seen), want)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("rowid %d appeared %d times", id, n)
		}
	}
	// Probe one chomp-rewritten row (the last update lands at 997) and one
	// untouched row (1036) — every row must survive with the right content.
	r2 := db.Query(`SELECT length(block) FROM seg WHERE blockid = 997`)
	if r2.Error != nil {
		t.Fatal(r2.Error)
	}
	if len(r2.Rows) != 1 {
		t.Fatalf("row 997 missing after rewrites (got %d rows)", len(r2.Rows))
	}
	if got := fmt.Sprint(r2.Rows[0][0]); got != "875" {
		t.Fatalf("row 997 length: got %s want 875", got)
	}
	r3 := db.Query(`SELECT length(block) FROM seg WHERE blockid = 1036`)
	if r3.Error != nil {
		t.Fatal(r3.Error)
	}
	if len(r3.Rows) != 1 {
		t.Fatalf("row 1036 missing after rewrites (got %d rows)", len(r3.Rows))
	}
	if got := fmt.Sprint(r3.Rows[0][0]); got != "936" { // original: 900+1036%100
		t.Fatalf("row 1036 length: got %s want 936", got)
	}
}
