package frigolite

import (
	"bytes"
	"testing"
)

// TestIns2PinZeroBlobAfterBlobRow pins the PERF.INSERT2-4 record-buffer
// reuse contract: zeroblob(N) values are written as N zero bytes even when
// the encode lands in the executor's REUSABLE record buffer (the previous
// row's bytes used to leak into the zeroblob tail, rtree xCreate seeded
// every new tree's root node from the previous rtree's node blob —
// TestT30PinRTreeNonNumericConstraints rt1 rid=1 shape).
func TestIns2PinZeroBlobAfterBlobRow(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE z(k INTEGER PRIMARY KEY, b BLOB)")
	// Row 1: an 820-byte non-zero blob (fills the reusable record buffer).
	must("INSERT INTO z VALUES(1, randomblob(820))")
	// Row 2: a zeroblob(820) — the tail must be zeros, not row 1's bytes.
	must("INSERT INTO z VALUES(2, zeroblob(820))")
	r := db.Query("SELECT b FROM z WHERE k=2")
	if r.Error != nil {
		t.Fatalf("zeroblob read: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("zeroblob row missing")
	}
	b, ok := r.Rows[0][0].([]byte)
	if !ok {
		t.Fatalf("zeroblob cell type %T, want []byte", r.Rows[0][0])
	}
	if len(b) != 820 {
		t.Fatalf("zeroblob length %d, want 820", len(b))
	}
	if !bytes.Equal(b, make([]byte, 820)) {
		t.Fatalf("zeroblob tail contains stale record bytes (first nonzero at %d)", firstNonZero(b))
	}
	// The row-1 blob must be intact too (REPLACE-adjacent corruption probe).
	r2 := db.Query("SELECT length(b) FROM z WHERE k=1")
	if got := r2.Rows[0][0].(int64); got != 820 {
		t.Fatalf("row 1 blob length %d, want 820", got)
	}
	// And a fresh rtree family must not inherit a previous one's node blob
	// (the original corruption shape: xCreate's zeroblob seed row).
	must("CREATE VIRTUAL TABLE ta USING rtree(ii, x1, x2)")
	must("INSERT INTO ta VALUES(1, 3, 7)")
	must("CREATE VIRTUAL TABLE tb USING rtree_i32(rid, c1, c2)")
	must("INSERT INTO tb(rid, c1, c2) VALUES(1,2,3)")
	r3 := db.Query("SELECT * FROM tb WHERE rid=1")
	if r3.Error != nil {
		t.Fatalf("rtree_i32 rid=1: %v", r3.Error)
	}
	if len(r3.Rows) != 1 || len(r3.Rows[0]) != 3 {
		t.Fatalf("rtree_i32 rid=1: got %v, want exactly [1 2 3]", r3.Rows)
	}
	if got := r3.Rows[0][0].(int64); got != 1 {
		t.Fatalf("rtree_i32 rid=1 first cell %v, want 1", got)
	}
	if got, ok := r3.Rows[0][1].(int64); !ok || got != 2 {
		t.Fatalf("rtree_i32 rid=1 second cell %v (%T), want int64(2)", r3.Rows[0][1], r3.Rows[0][1])
	}
}

func firstNonZero(b []byte) int {
	for i, c := range b {
		if c != 0 {
			return i
		}
	}
	return -1
}
