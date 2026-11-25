package frigolite

import (
	"fmt"
	"testing"
)

// TestFTS4SignedBoundaryRowids pins fts3query-7.2: explicit negative and
// positive rowids near the signed 64-bit boundaries must stay distinct across
// repeated template-cache hits.
func TestFTS4SignedBoundaryRowids(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	exec(`CREATE VIRTUAL TABLE ft USING fts4(x)`)
	const minInt64 = int64(-1 << 63)
	const maxInt64 = int64(1<<63 - 1)
	for i := int64(0); i < 100; i++ {
		exec(fmt.Sprintf("INSERT INTO ft(rowid, x) VALUES(%d, 'x y z')", minInt64+i))
	}
	for i := int64(0); i < 100; i++ {
		exec(fmt.Sprintf("INSERT INTO ft(rowid, x) VALUES(%d, 'x y z')", maxInt64-99+i))
	}

	r := db.Query(`SELECT rowid FROM ft ORDER BY rowid`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 200 {
		t.Fatalf("rows = %d, want 200 boundary rows", len(r.Rows))
	}
	if got := fmt.Sprint(r.Rows[0][0]); got != fmt.Sprint(minInt64) {
		t.Fatalf("first rowid = %s, want %d", got, minInt64)
	}
	if got := fmt.Sprint(r.Rows[len(r.Rows)-1][0]); got != fmt.Sprint(maxInt64) {
		t.Fatalf("last rowid = %s, want %d", got, maxInt64)
	}
}
