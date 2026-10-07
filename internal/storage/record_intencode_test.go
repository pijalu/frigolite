package storage

import (
	"bytes"
	"math"
	"testing"
)

// TestAppendEncodeIntRecordParity pins the fixed-arity all-integer fast path
// to the generic encode walk: for every integer pair shape (including the
// serial-type magnitude edges: 0/1 constants, 1/2/3/4-byte magnitudes, the
// 6-byte boundary 2^47, negatives via ^val, and the int64 extremes) the fast
// path must produce byte-identical records. A non-int64 or mixed row must
// fall back to the generic walk unchanged.
func TestAppendEncodeIntRecordParity(t *testing.T) {
	edges := []int64{
		0, 1, -1, 2, 127, 128, -128, -129, 255, 32767, 32768, -32768, -32769,
		8388607, 8388608, -8388608, -8388609, 2147483647, 2147483648,
		-2147483648, -2147483649, 140737488355327, 140737488355328,
		-140737488355328, -140737488355329, math.MaxInt64, math.MinInt64,
	}
	for _, a := range edges {
		for _, b := range edges {
			ints := []int64{a, b}
			vals := []interface{}{a, b}
			fast, err := AppendEncodeRecord(nil, vals)
			if err != nil {
				t.Fatalf("fast(%d,%d): %v", a, b, err)
			}
			generic, err := appendEncodeRecordGeneric(nil, vals)
			if err != nil {
				t.Fatalf("generic(%d,%d): %v", a, b, err)
			}
			if !bytes.Equal(fast, generic) {
				t.Fatalf("fast path diverged for (%d,%d): fast %x vs generic %x", a, b, fast, generic)
			}
			_ = ints
		}
	}
	// Reuse parity: encoding into a pre-sized buffer must not disturb the
	// prefix and must produce the same bytes.
	buf := make([]byte, 0, 64)
	buf = append(buf, 0xAB, 0xCD)
	withPrefix, err := AppendEncodeRecord(buf, []interface{}{int64(7), int64(999999)})
	if err != nil {
		t.Fatal(err)
	}
	if withPrefix[0] != 0xAB || withPrefix[1] != 0xCD {
		t.Fatalf("prefix clobbered: %x", withPrefix[:2])
	}
	fresh, _ := AppendEncodeRecord(nil, []interface{}{int64(7), int64(999999)})
	if !bytes.Equal(withPrefix[2:], fresh) {
		t.Fatalf("reuse encode diverged: %x vs %x", withPrefix[2:], fresh)
	}

	// Non-int64 and mixed rows take the generic path (identical output to
	// the generic walk by construction — pinned so the gate stays honest).
	mixed := []interface{}{int64(5), nil, "txt"}
	m1, _ := AppendEncodeRecord(nil, mixed)
	m2, _ := appendEncodeRecordGeneric(nil, mixed)
	if !bytes.Equal(m1, m2) {
		t.Fatalf("mixed fallback diverged: %x vs %x", m1, m2)
	}
	// NULL alone is not the int fast path either (arity>0 but not all-int).
	nl, _ := AppendEncodeRecord(nil, []interface{}{nil})
	ng, _ := appendEncodeRecordGeneric(nil, []interface{}{nil})
	if !bytes.Equal(nl, ng) {
		t.Fatalf("nil fallback diverged")
	}
}
