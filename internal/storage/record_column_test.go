package storage

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
)

// mustRecord encodes values into a record payload or fails the test.
func mustRecord(t *testing.T, values []interface{}) []byte {
	t.Helper()
	data, err := EncodeRecord(values)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	return data
}

// TestDecodeRecordColumnMatchesFullDecode pins the single-column read against
// the whole-record decode over every serial-type family: the value and its Go
// type must be identical, and NULL must report ok=false.
func TestDecodeRecordColumnMatchesFullDecode(t *testing.T) {
	values := []interface{}{
		int64(-5),                      // SerialInt8
		int64(300),                     // SerialInt16
		int64(70000),                   // SerialInt24 range
		int64(1 << 30),                 // SerialInt32
		int64(1 << 45),                 // SerialInt48
		int64(1 << 60),                 // SerialInt64
		3.25,                           // SerialFloat
		int64(0),                       // SerialZero
		int64(1),                       // SerialOne
		nil,                            // SerialNull
		"hello",                        // text
		[]byte{0xde, 0xad, 0xbe, 0xef}, // blob
		"",                             // empty text
		[]byte{},                       // empty blob
	}
	data := mustRecord(t, values)
	full, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	for i, want := range full.Values {
		got, ok, err := DecodeRecordColumn(data, i)
		if err != nil {
			t.Fatalf("DecodeRecordColumn(col %d): %v", i, err)
		}
		if want == nil {
			if ok {
				t.Errorf("col %d: want NULL (ok=false), got ok=true (%v)", i, got)
			}
			continue
		}
		if !ok {
			t.Fatalf("col %d: want %v, got ok=false", i, want)
		}
		if gotType, wantType := reflect.TypeOf(got), reflect.TypeOf(want); gotType != wantType {
			t.Errorf("col %d: type %v, want %v (got %v)", i, gotType, wantType, got)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("col %d: got %#v, want %#v", i, got, want)
		}
	}
}

// TestDecodeRecordColumnsMatchesFullDecode checks the multi-column read into a
// caller-reusable slice, including the exact serial-type count and the reset
// of untouched out slots.
func TestDecodeRecordColumnsMatchesFullDecode(t *testing.T) {
	values := []interface{}{int64(7), "text", 1.5, nil, []byte{1, 2}, int64(1), "x"}
	data := mustRecord(t, values)
	full, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	cols := []int{5, 0, 3, 5} // unsorted, with a duplicate
	out := make([]interface{}, 8)
	out[7] = "sentinel-must-reset"
	count, err := DecodeRecordColumns(data, cols, out)
	if err != nil {
		t.Fatalf("DecodeRecordColumns: %v", err)
	}
	if count != len(values) {
		t.Errorf("count = %d, want %d", count, len(values))
	}
	for i, col := range cols {
		if !reflect.DeepEqual(out[i], full.Values[col]) {
			t.Errorf("out[%d] (col %d) = %#v, want %#v", i, col, out[i], full.Values[col])
		}
	}
	for i := len(cols); i < len(out); i++ {
		if out[i] != nil {
			t.Errorf("out[%d] not reset: %#v", i, out[i])
		}
	}
}

// TestDecodeRecordColumnAbsent pins the short-record (ALTER TABLE ADD COLUMN)
// case: an ordinal beyond the record's column count reads as NULL, no error.
func TestDecodeRecordColumnAbsent(t *testing.T) {
	data := mustRecord(t, []interface{}{int64(1), "a"})
	if _, ok, err := DecodeRecordColumn(data, 2); err != nil || ok {
		t.Errorf("absent column: got (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	out := make([]interface{}, 2)
	count, err := DecodeRecordColumns(data, []int{0, 2}, out)
	if err != nil {
		t.Fatalf("DecodeRecordColumns: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	if out[1] != nil {
		t.Errorf("absent column slot: %#v, want nil", out[1])
	}
}

// TestDecodeRecordColumnCorrupt pins corrupt-record validation parity with
// DecodeRecord: header past the payload, unknown serial type, truncated value
// bytes — every case DecodeRecord rejects must error here too.
func TestDecodeRecordColumnCorrupt(t *testing.T) {
	good := mustRecord(t, []interface{}{int64(1), "abcdef", 2.0})

	// Header size extending past the payload.
	bad := append([]byte(nil), good...)
	bad[0] = 0x20 // header size 32 > payload
	if _, _, err := DecodeRecordColumn(bad, 1); err == nil {
		t.Error("oversized header: expected error")
	}

	// Unknown serial type 10 in the header.
	rec := []byte{0x04, 0x0A, 0x2D} // hdrSize=4 (bad: only 3 bytes... use exact)
	_ = rec
	data := []byte{0x03, 0x01, 0x0A, 0x2D} // hdrSize=3: types {1(int8), 10(bad)} + payload 0x2D
	if full, err := DecodeRecord(append([]byte(nil), data...)); err == nil {
		// DecodeRecord must reject it too; if not, the parity premise is off.
		t.Logf("DecodeRecord accepted bad type: %v", full)
	} else if _, _, err2 := DecodeRecordColumn(data, 1); err2 == nil {
		t.Error("unknown serial type: expected error")
	}

	// Value bytes truncated: column 1 declares 6 text bytes, payload has 2.
	short := []byte{0x03, 0x01, 0x19, 0x41, 0x42} // hdrSize=3: int8, text(6); payload "AB"
	if _, _, err := DecodeRecordColumn(short, 1); err == nil {
		t.Error("truncated value: expected error")
	}

	// Negative index.
	if _, _, err := DecodeRecordColumn(good, -1); err == nil {
		t.Error("negative index: expected error")
	}
}

// TestDecodeRecordColumnsReuse pins the caller-reusable contract: the out
// slice is filled in place across calls without growth.
func TestDecodeRecordColumnsReuse(t *testing.T) {
	cols := []int{1}
	out := make([]interface{}, 1)
	first := mustRecord(t, []interface{}{int64(1), "one"})
	second := mustRecord(t, []interface{}{int64(2), "two"})
	if _, err := DecodeRecordColumns(first, cols, out); err != nil {
		t.Fatalf("first: %v", err)
	}
	if out[0] != "one" {
		t.Fatalf("first out = %#v", out[0])
	}
	if _, err := DecodeRecordColumns(second, cols, out); err != nil {
		t.Fatalf("second: %v", err)
	}
	if out[0] != "two" {
		t.Fatalf("second out = %#v (not reused/reset)", out[0])
	}
}

// TestDecodeRecordColumnsShortOut pins the defensive contract: an out slice
// shorter than the column list is an error, not a panic.
func TestDecodeRecordColumnsShortOut(t *testing.T) {
	data := mustRecord(t, []interface{}{int64(1), "a"})
	if _, err := DecodeRecordColumns(data, []int{0, 1}, make([]interface{}, 1)); err == nil {
		t.Error("short out slice: expected error")
	}
}

// TestDecodeRecordColumnsBlobCopy pins that a blob read is a fresh copy, so a
// caller retaining it across row overwrites cannot see later mutations of the
// page buffer.
func TestDecodeRecordColumnsBlobCopy(t *testing.T) {
	payload := []interface{}{[]byte{1, 2, 3}}
	data := mustRecord(t, payload)
	out := make([]interface{}, 1)
	if _, err := DecodeRecordColumns(data, []int{0}, out); err != nil {
		t.Fatalf("DecodeRecordColumns: %v", err)
	}
	b, ok := out[0].([]byte)
	if !ok {
		t.Fatalf("want []byte, got %T", out[0])
	}
	if !bytes.Equal(b, []byte{1, 2, 3}) {
		t.Fatalf("blob = %v", b)
	}
	b[0] = 0xFF
	if data[len(data)-3] == 0xFF {
		t.Fatal("blob read aliases the payload buffer")
	}
}

// TestDecodeRecordColumnsPrefixMatchesFullDecode pins the prefix walk's
// contract: values identical to DecodeRecordColumns, and the returned count
// is min(record column count, max(cols)+1) — exact below the ceiling, capped
// at it once every requested slot is present.
func TestDecodeRecordColumnsPrefixMatchesFullDecode(t *testing.T) {
	values := []interface{}{int64(1), "two", 3.5, []byte("four"), nil, int64(6), "seven", 8.0, "nine", int64(10)}
	data := mustRecord(t, values)
	cases := [][]int{
		{0}, {1}, {9}, {0, 1}, {1, 4}, {0, 9}, {2, 5, 8}, {0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	}
	for _, cols := range cases {
		full := make([]interface{}, len(cols))
		wantCount, err := DecodeRecordColumns(data, cols, full)
		if err != nil {
			t.Fatalf("full decode %v: %v", cols, err)
		}
		got := make([]interface{}, len(cols))
		gotCount, err := DecodeRecordColumnsPrefix(data, cols, got)
		if err != nil {
			t.Fatalf("prefix decode %v: %v", cols, err)
		}
		for i := range cols {
			if fmt.Sprint(full[i]) != fmt.Sprint(got[i]) {
				t.Errorf("cols %v slot %d: prefix %v, full %v", cols, i, got[i], full[i])
			}
		}
		wantCeil := cols[len(cols)-1] + 1
		want := wantCount
		if want > wantCeil {
			want = wantCeil
		}
		if gotCount != want {
			t.Errorf("cols %v: prefix count %d, want min(%d, %d)", cols, gotCount, wantCount, wantCeil)
		}
	}
}

// TestDecodeRecordColumnsPrefixShortRecord pins the absent-slot behavior: a
// record shorter than the requested ordinal returns the exact (smaller)
// count so the caller can fall back for defaults.
func TestDecodeRecordColumnsPrefixShortRecord(t *testing.T) {
	data := mustRecord(t, []interface{}{int64(1), "two"})
	if count, err := DecodeRecordColumnsPrefix(data, []int{0, 5}, make([]interface{}, 2)); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v, want 2/nil", count, err)
	}
	if count, err := DecodeRecordColumnsPrefix(data, []int{1}, make([]interface{}, 1)); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v, want 2/nil", count, err)
	}
}

// TestDecodeRecordValuesIntoMatchesTwoCallForm pins the fused decode against
// the ParseRecordHeaderInto + DecodeRecordValuesFromTypesCols pair it
// replaces: identical filled slots, identical stored-column count — over
// every serial-type family and a wider-than-stack record (heap spill).
func TestDecodeRecordValuesIntoMatchesTwoCallForm(t *testing.T) {
	wide := make([]interface{}, 20)
	for i := range wide {
		switch i % 5 {
		case 0:
			wide[i] = int64(i * 7919)
		case 1:
			wide[i] = float64(i) + 0.5
		case 2:
			wide[i] = nil
		case 3:
			wide[i] = "s"
		case 4:
			wide[i] = []byte{byte(i)}
		}
	}
	cases := [][]interface{}{
		{int64(-5), int64(300), int64(70000), int64(1 << 30), int64(1 << 45), int64(1 << 60)},
		{3.25, int64(0), int64(1), nil, "hello", []byte{0xde, 0xad}, "", []byte{}},
		{int64(7), "text", 1.5, nil, []byte{1, 2}, int64(1), "x"},
		wide,
	}
	for _, values := range cases {
		data := mustRecord(t, values)
		// Two-call reference.
		types, dataStart, err := ParseRecordHeaderInto(data, nil)
		if err != nil {
			t.Fatalf("ParseRecordHeaderInto: %v", err)
		}
		ref := make([]interface{}, len(data)) // wider than the record
		DecodeRecordValuesFromTypesCols(data, dataStart, ref, types, nil)
		// Fused form into an equally wide target.
		got := make([]interface{}, len(data))
		count, err := DecodeRecordValuesInto(data, got, nil)
		if err != nil {
			t.Fatalf("DecodeRecordValuesInto: %v", err)
		}
		if count != len(types) {
			t.Errorf("count = %d, want %d", count, len(types))
		}
		for i := 0; i < len(values); i++ {
			if !reflect.DeepEqual(got[i], ref[i]) {
				t.Errorf("values %v col %d: fused %#v, two-call %#v", values, i, got[i], ref[i])
			}
		}
		// Target narrower than the record: the fill stops at the target,
		// the count still reports the record's stored columns.
		if len(values) > 1 {
			narrow := make([]interface{}, 1)
			n, err := DecodeRecordValuesInto(data, narrow, nil)
			if err != nil || n != len(types) {
				t.Errorf("narrow target: count=%d err=%v, want %d/nil", n, err, len(types))
			}
			if !reflect.DeepEqual(narrow[0], ref[0]) {
				t.Errorf("narrow target slot: %#v, want %#v", narrow[0], ref[0])
			}
		}
	}
}

// TestDecodeRecordValuesIntoSelection pins the cols selection: selected
// columns fill, skipped columns leave nil while their bytes are skipped.
func TestDecodeRecordValuesIntoSelection(t *testing.T) {
	data := mustRecord(t, []interface{}{int64(7), "text", 1.5, []byte{1, 2}})
	cols := []bool{false, true, false, true}
	got := make([]interface{}, 4)
	count, err := DecodeRecordValuesInto(data, got, cols)
	if err != nil || count != 4 {
		t.Fatalf("count=%d err=%v, want 4/nil", count, err)
	}
	if got[0] != nil || got[2] != nil {
		t.Errorf("skipped slots not nil: %#v %#v", got[0], got[2])
	}
	if got[1] != "text" {
		t.Errorf("selected text: %#v", got[1])
	}
	if !reflect.DeepEqual(got[3], []byte{1, 2}) {
		t.Errorf("selected blob: %#v", got[3])
	}
}

// TestDecodeRecordValuesIntoCorrupt pins the error contract against the
// two-call form: an oversized header errors (both forms), while an unknown
// serial type or truncated value bytes stop the fill early WITHOUT error,
// reporting the full stored-column count (the two-call form's behavior —
// ParseRecordHeader succeeds, the fill stops).
func TestDecodeRecordValuesIntoCorrupt(t *testing.T) {
	good := mustRecord(t, []interface{}{int64(1), "abcdef", 2.0})

	// Header size extending past the payload: both forms error.
	bad := append([]byte(nil), good...)
	bad[0] = 0x20
	if _, _, err := ParseRecordHeaderInto(bad, nil); err == nil {
		t.Error("oversized header (ParseRecordHeaderInto): expected error")
	}
	if _, err := DecodeRecordValuesInto(bad, make([]interface{}, 3), nil); err == nil {
		t.Error("oversized header (DecodeRecordValuesInto): expected error")
	}

	// Unknown serial type 10: header parses, the fill stops before it.
	unknown := []byte{0x03, 0x01, 0x0A, 0x2D}
	types, _, err := ParseRecordHeaderInto(unknown, nil)
	if err != nil {
		t.Fatalf("ParseRecordHeaderInto: %v", err)
	}
	ref := make([]interface{}, 2)
	stopped := DecodeRecordValuesFromTypesCols(unknown, 3, ref, types, nil)
	got := make([]interface{}, 2)
	count, err := DecodeRecordValuesInto(unknown, got, nil)
	if err != nil {
		t.Errorf("unknown serial type: fused errored (%v), two-call stops early", err)
	}
	if count != len(types) {
		t.Errorf("unknown serial type: count=%d, want %d", count, len(types))
	}
	for i := 0; i < stopped; i++ {
		if !reflect.DeepEqual(got[i], ref[i]) {
			t.Errorf("unknown serial type slot %d: fused %#v, two-call %#v", i, got[i], ref[i])
		}
	}

	// Truncated value bytes: col 1 declares 6 text bytes, payload has 2.
	short := []byte{0x03, 0x01, 0x19, 0x41, 0x42}
	typesS, _, err := ParseRecordHeaderInto(short, nil)
	if err != nil {
		t.Fatalf("ParseRecordHeaderInto: %v", err)
	}
	gotS := make([]interface{}, 2)
	countS, err := DecodeRecordValuesInto(short, gotS, nil)
	if err != nil || countS != len(typesS) {
		t.Errorf("truncated: count=%d err=%v, want %d/nil", countS, err, len(typesS))
	}
	if gotS[0] != int64(0x41) {
		t.Errorf("truncated: first slot %#v, want int64(65)", gotS[0])
	}
	if gotS[1] != nil {
		t.Errorf("truncated: second slot %#v, want nil", gotS[1])
	}
}

// TestParseRecordHeaderIntoReuse pins the caller-buffer contract: one buffer
// refilled across parses grows in place and reports the right dataStart.
func TestParseRecordHeaderIntoReuse(t *testing.T) {
	buf := make([]uint64, 0, 8) // caps both parses: no growth realloc
	a := mustRecord(t, []interface{}{int64(1), "one", 2.0})
	types, dataStart, err := ParseRecordHeaderInto(a, buf[:0])
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(types) != 3 || dataStart != 4 { // hdr varint + 3 type varints
		t.Fatalf("first: types=%v dataStart=%d len=%d", types, dataStart, len(a))
	}
	b := mustRecord(t, []interface{}{int64(2), "two", 3.0, nil, "extra"})
	types2, dataStart2, err := ParseRecordHeaderInto(b, types[:0])
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(types2) != 5 {
		t.Fatalf("second: types=%v", types2)
	}
	if dataStart2 != 6 { // hdr varint + 5 type varints
		t.Fatalf("second: dataStart=%d len=%d", dataStart2, len(b))
	}
	if &types2[0] != &types[0] {
		t.Error("buffer did not grow in place from the caller's slice")
	}
}
