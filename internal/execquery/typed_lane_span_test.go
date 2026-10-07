package execquery

import (
	"bytes"
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// encodeRecordForSpanTest builds a record payload from serial types and raw
// value bytes (the span table's test fixture needs control over corrupt
// types, so it bypasses the encoder).
func encodeRecordForSpanTest(types []uint64, bodies [][]byte) []byte {
	var hdr []byte
	hdr = append(hdr, 0) // placeholder for header size
	for _, st := range types {
		hdr = append(hdr, byte(st)) // 1-byte serial types only in fixtures
	}
	hdr[0] = byte(len(hdr))
	out := append([]byte{}, hdr...)
	for _, b := range bodies {
		out = append(out, b...)
	}
	return out
}

// TestTypedLaneSpanTableParity pins the typed aggregate lane's O(1) span
// table (parseRecordSerialTypesOffsetsInto + resolveSlotOffs) to the generic
// header walk (parseRecordSerialTypesInto + resolveSlot): for every record
// shape — clean integers, a stored-NULL IPK alias column, a truncated body,
// a corrupt reserved serial type (10/11) at and before the target slot — the
// two resolvers must agree on (ok, serial type) and, when ok, on the value
// bytes. This is the engagement-vs-fallback contract of the R10 scan lane.
func TestTypedLaneSpanTableParity(t *testing.T) {
	intBody := func(v uint64, n int) []byte {
		b := make([]byte, n)
		for i := 0; i < n; i++ {
			b[n-1-i] = byte(v >> (8 * i))
		}
		return b
	}
	stFor := func(n int) uint64 { return uint64(n) } // 1..6 = 1..8-byte ints

	cases := []struct {
		name        string
		types       []uint64
		bodies      [][]byte
		truncateTo  int // -1: keep full body
		targetSlots []int
	}{
		{"clean int pair", []uint64{0, stFor(2)}, [][]byte{intBody(1000003-7, 2)}, -1, []int{0, 1}},
		{"null ipk then int", []uint64{0, 0, stFor(4)}, [][]byte{intBody(70000, 4)}, -1, []int{0, 1, 2}},
		{"text then int", []uint64{21, stFor(1)}, [][]byte{[]byte("wxyz"), intBody(200, 1)}, -1, []int{1}},
		{"truncated body", []uint64{0, stFor(4)}, [][]byte{intBody(7, 2)}, 3, []int{0, 1}},
		{"corrupt type at slot", []uint64{0, 10, stFor(1)}, [][]byte{intBody(9, 1)}, -1, []int{1, 2}},
		{"corrupt type before slot", []uint64{10, 0, stFor(1)}, [][]byte{intBody(9, 1)}, -1, []int{1, 2}},
	}
	for _, tc := range cases {
		payload := encodeRecordForSpanTest(tc.types, tc.bodies)
		if tc.truncateTo >= 0 {
			payload = payload[:tc.truncateTo]
		}
		types, offs, dataStart, err := parseRecordSerialTypesOffsetsInto(payload, nil, nil)
		if err != nil {
			t.Fatalf("%s: offsets parse: %v", tc.name, err)
		}
		genericTypes, genericStart, err := parseRecordSerialTypesInto(payload, nil)
		if err != nil {
			t.Fatalf("%s: generic parse: %v", tc.name, err)
		}
		if dataStart != genericStart || !typesEqual(types, genericTypes) {
			t.Fatalf("%s: header parse diverged: (%v,%d) vs (%v,%d)", tc.name, types, dataStart, genericTypes, genericStart)
		}
		var probe typedAggCall
		for _, slot := range tc.targetSlots {
			probe.diskSlot = slot
			stO, dataO, okO := probe.resolveSlotOffs(payload, dataStart, offs, types)
			stG, dataG, okG := probe.resolveSlot(payload, dataStart, types)
			if okO != okG {
				t.Fatalf("%s slot %d: ok mismatch offs=%v walk=%v", tc.name, slot, okO, okG)
			}
			if !okO {
				continue
			}
			if stO != stG || !bytes.Equal(dataO, dataG) {
				t.Fatalf("%s slot %d: value mismatch (%d,%x) vs (%d,%x)", tc.name, slot, stO, dataO, stG, dataG)
			}
			// The span must agree with a full decode of the same bytes.
			if _, ok := storage.DecodeSerialInt64(stO, dataO); !ok && stO >= storage.SerialInt8 && stO <= storage.SerialInt64 {
				t.Fatalf("%s slot %d: span slice undecodable", tc.name, slot)
			}
		}
	}
}

func typesEqual(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTypedLaneSpanTableVarintHeader covers multi-byte header-size and
// serial-type varints: a wide-text record whose header needs a 2-byte size
// varint and a blob serial type above 127 must parse identically to the
// generic walk and address the same value spans.
func TestTypedLaneSpanTableVarintHeader(t *testing.T) {
	// 70-char text: serial type 13+140=153 (2-byte varint); 60 such columns
	// then one integer: header size = 1 + 60*2 + 1 = 122 (1-byte varint).
	var types []uint64
	var bodies [][]byte
	for i := 0; i < 60; i++ {
		types = append(types, 153)
		bodies = append(bodies, bytes.Repeat([]byte{byte('a' + i%26)}, 70))
	}
	types = append(types, 2)
	bodies = append(bodies, []byte{0x12, 0x34})

	var payload []byte
	var hdrTypes []byte
	for _, st := range types {
		hdrTypes = append(hdrTypes, byte(st>>7|0x80), byte(st&0x7f))
	}
	payload = append(payload, byte(len(hdrTypes)+1))
	payload = append(payload, hdrTypes...)
	for _, b := range bodies {
		payload = append(payload, b...)
	}

	typesOut, offs, dataStart, err := parseRecordSerialTypesOffsetsInto(payload, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gtypes, gstart, err := parseRecordSerialTypesInto(payload, nil)
	if err != nil || dataStart != gstart || !typesEqual(typesOut, gtypes) {
		t.Fatalf("wide header diverged: %v %d vs %v %d (err %v)", typesOut, dataStart, gtypes, gstart, err)
	}
	// The last slot (the integer) must resolve to the same bytes both ways.
	probe := typedAggCall{diskSlot: len(types) - 1}
	stO, dataO, okO := probe.resolveSlotOffs(payload, dataStart, offs, typesOut)
	stG, dataG, okG := probe.resolveSlot(payload, dataStart, typesOut)
	if !okO || !okG || stO != stG || !bytes.Equal(dataO, dataG) {
		t.Fatalf("wide-header slot diverged: (%d,%x,%v) vs (%d,%x,%v)", stO, dataO, okO, stG, dataG, okG)
	}
	if _, n := util.GetVarint(payload); n == 0 {
		t.Fatal("payload sanity")
	}
}
