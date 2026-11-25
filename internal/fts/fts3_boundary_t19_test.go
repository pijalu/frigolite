package fts

import "testing"

// TestFTS3TableSignedBoundaryDocIDs checks the in-memory FTS index directly:
// negative docids near math.MinInt64 must not alias positive boundary docids.
func TestFTS3TableSignedBoundaryDocIDs(t *testing.T) {
	tbl, err := NewFTS3Table("ft", "fts4", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	const minInt64 = int64(-1 << 63)
	const maxInt64 = int64(1<<63 - 1)
	for i := int64(0); i < 100; i++ {
		tbl.InsertWithID(minInt64+i, []interface{}{"x y z"})
	}
	for i := int64(0); i < 100; i++ {
		id := maxInt64 - 99 + i
		if tbl.HasDoc(id) {
			t.Fatalf("HasDoc(%d) after negative run = true", id)
		}
		tbl.InsertWithID(id, []interface{}{"x y z"})
	}
	if got := len(tbl.AllRowsMap()); got != 200 {
		t.Fatalf("AllRowsMap count = %d, want 200", got)
	}
}
