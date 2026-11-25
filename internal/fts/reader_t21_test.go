package fts

import "testing"

// TestReadLeafChainTermZeroFirstLength pins the fts3defer3 1.7 corruption
// shape: an all-zero leaf block starts with a zero term length, which SQLite
// rejects as FTS_CORRUPT_VTAB rather than parsing as an empty term.
func TestReadLeafChainTermZeroFirstLength(t *testing.T) {
	leaf := []byte{0, 0, 0, 0}
	_, _, hard, ok := readLeafChainTerm(leaf, 1, nil, true)
	if ok || !hard {
		t.Fatalf("readLeafChainTerm = (ok=%v, hard=%v), want (false, true)", ok, hard)
	}
}
