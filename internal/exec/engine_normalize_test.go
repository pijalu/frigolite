package exec

import (
	"hash/maphash"
	"testing"
)

// TestNormalizeScanEquivalence pins the fused normalization scan
// (normalizeScan — hash + spans in one pass) against the legacy materializing
// form (normalizeSQLScratch): for every shape, the extracted values must be
// identical and the span-implied normalized text must equal the scratch
// buffer's, the span verification must accept exactly the matching template
// text, and the incrementally-built hash must equal a direct maphash over the
// materialized bytes (same Hash object → same seed).
func TestNormalizeScanEquivalence(t *testing.T) {
	shapes := []string{
		"INSERT INTO t VALUES(300000, 2100009)",
		"SELECT c FROM t WHERE id=25000",
		"UPDATE t SET c=c+1 WHERE id=42",
		"SELECT 'it''s', 1.5, .5, 1e3, 2E+2 FROM t WHERE x='a' AND y=7",
		"SELECT t1.c FROM t1, t2 WHERE t1.id=:name AND t2.k=$p",
		"DELETE FROM t WHERE id=3 LIMIT 1 OFFSET 1",
		"SELECT x FROM t WHERE hex=0x1F",
		"SELECT a1, b2, c3 FROM u2 WHERE id=9",
		"SELECT ?1, ?2 FROM t WHERE k=:v",
		"SELECT '' FROM t WHERE id=1",
		"INSERT INTO t VALUES(18446744073709551616, 0)",
		"SELECT t. c FROM t WHERE d=1",          // lone dot after space: dot-literal
		"SELECT 1.e FROM t",                     // float-ish scan shapes
		"SELECT * FROM t WHERE s='unterminated", // unterminated string
		"SELECT .5",
	}
	for _, stmt := range shapes {
		norm, wantValues, _ := normalizeSQLScratch(stmt, nil, nil)

		var h maphash.Hash
		var spans []normSpan
		gotValues, spans, ok := normalizeScan(stmt, nil, spans, &h)
		if (norm != nil) != ok {
			t.Fatalf("%q: scan ok=%v, legacy norm nil=%v", stmt, ok, norm == nil)
		}
		if norm == nil {
			continue
		}
		if len(gotValues) != len(wantValues) {
			t.Fatalf("%q: value count %d, want %d", stmt, len(gotValues), len(wantValues))
		}
		for i := range wantValues {
			a, b := gotValues[i], wantValues[i]
			if a != b {
				t.Fatalf("%q: value %d = %#v, want %#v", stmt, i, a, b)
			}
		}
		// Span-implied normalized text must equal the materialized form.
		if !templateMatchesSpans(string(norm), stmt, spans) {
			t.Fatalf("%q: span verification rejected its own normalized text %q", stmt, norm)
		}
		// A different template must be rejected.
		if templateMatchesSpans(string(norm)+"?", stmt, spans) {
			t.Fatalf("%q: span verification accepted a wrong template", stmt)
		}
		// The fused hash must hash the SAME byte sequence as the materialized
		// form: replaying the normalized bytes through the same Hash (Reset
		// keeps its seed) must reproduce the scan's sum.
		fusedSum := h.Sum64()
		h.Reset()
		h.Write(norm)
		if h.Sum64() != fusedSum {
			t.Fatalf("%q: fused hash mismatch (self-check)", stmt)
		}
		if fusedSum == 0 {
			t.Fatalf("%q: hash self-check degenerate", stmt)
		}
		_ = fusedSum
	}
}
