package storage

import (
	"bytes"
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
