package storage

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"
)

func TestDefaultHeader(t *testing.T) {
	h := DefaultHeader(0)
	if h.PageSize != PageSizeDefault {
		t.Errorf("PageSize: got %d, want %d", h.PageSize, PageSizeDefault)
	}
	if h.TextEncoding != 1 {
		t.Errorf("TextEncoding: got %d, want 1", h.TextEncoding)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := DefaultHeader(4096)
	h.DatabaseSize = 42
	h.SchemaCookie = 1
	h.ApplicationID = 0x12345678

	data := h.Encode()
	got, err := ParseHeader(data)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}

	// Compare fields
	if got.PageSize != h.PageSize {
		t.Errorf("PageSize: got %d, want %d", got.PageSize, h.PageSize)
	}
	if got.DatabaseSize != h.DatabaseSize {
		t.Errorf("DatabaseSize: got %d, want %d", got.DatabaseSize, h.DatabaseSize)
	}
	if got.ApplicationID != h.ApplicationID {
		t.Errorf("ApplicationID: got %08x, want %08x", got.ApplicationID, h.ApplicationID)
	}
}

func TestHeaderInvalidMagic(t *testing.T) {
	data := make([]byte, HeaderSize)
	_, err := ParseHeader(data)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestHeaderShort(t *testing.T) {
	_, err := ParseHeader(make([]byte, 10))
	if err == nil {
		t.Fatal("expected error for short header")
	}
}

func TestParsePageTypes(t *testing.T) {
	tests := []struct {
		name    string
		pageData []byte
		want    byte
		wantErr bool
	}{
		{"interior index", []byte{PageTypeInteriorIndex, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, PageTypeInteriorIndex, false},
		{"interior table", []byte{PageTypeInteriorTable, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, PageTypeInteriorTable, false},
		{"leaf index", []byte{PageTypeLeafIndex, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, PageTypeLeafIndex, false},
		{"leaf table", []byte{PageTypeLeafTable, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, PageTypeLeafTable, false},
		{"invalid type", []byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParsePage(tt.pageData, 4096, 0)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.PageType != tt.want {
				t.Errorf("PageType: got %02x, want %02x", p.PageType, tt.want)
			}
		})
	}
}

func TestCellPointer(t *testing.T) {
	data := make([]byte, 20)
	binary.BigEndian.PutUint16(data[8:10], 100)
	binary.BigEndian.PutUint16(data[10:12], 200)

	p1 := CellPointer(data, 0, 0)
	if p1 != 100 {
		t.Errorf("CellPointer[0]: got %d, want 100", p1)
	}
	p2 := CellPointer(data, 0, 1)
	if p2 != 200 {
		t.Errorf("CellPointer[1]: got %d, want 200", p2)
	}
}

func TestRecordRoundTripEmpty(t *testing.T) {
	r := &Record{Values: []interface{}{}}
	data, err := EncodeRecord(r.Values)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	if len(got.Values) != 0 {
		t.Errorf("got %d values, want 0", len(got.Values))
	}
}

func TestRecordRoundTripSingleNull(t *testing.T) {
	r := &Record{Values: []interface{}{nil}}
	data, err := EncodeRecord(r.Values)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	if got.Values[0] != nil {
		t.Errorf("got %v, want nil", got.Values[0])
	}
}

func TestRecordRoundTripIntegers(t *testing.T) {
	vals := []interface{}{
		int64(0),
		int64(1),
		int64(42),
		int64(127),
		int64(128),
		int64(255),
		int64(32767),
		int64(32768),
		int64(65535),
		int64(100000),
		int64(1 << 24),
		int64(1 << 30),
		int64(1 << 40),
		int64(1 << 50),
		int64(math.MaxInt64),
		int64(math.MinInt64),
	}
	data, err := EncodeRecord(vals)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	if len(got.Values) != len(vals) {
		t.Fatalf("got %d values, want %d", len(got.Values), len(vals))
	}
	for i := range vals {
		if got.Values[i] != vals[i] {
			t.Errorf("values[%d]: got %v, want %v", i, got.Values[i], vals[i])
		}
	}
}

func TestRecordRoundTripFloat(t *testing.T) {
	vals := []interface{}{
		float64(3.14159),
		float64(0.0),
		float64(-1.5),
		float64(math.Inf(1)),
		float64(math.NaN()),
	}
	data, err := EncodeRecord(vals)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	if len(got.Values) != len(vals) {
		t.Fatalf("got %d values, want %d", len(got.Values), len(vals))
	}
	for i := range vals {
		f1, ok1 := got.Values[i].(float64)
		f2, ok2 := vals[i].(float64)
		if !ok1 || !ok2 {
			t.Errorf("values[%d]: type mismatch", i)
			continue
		}
		if math.IsNaN(f2) && math.IsNaN(f1) {
			continue // NaN != NaN but should be treated as equal
		}
		if f1 != f2 {
			t.Errorf("values[%d]: got %v, want %v", i, f1, f2)
		}
	}
}

func TestRecordRoundTripStrings(t *testing.T) {
	vals := []interface{}{
		"hello",
		"",
		"a",
		"Hello, 世界",
		"strings with spaces",
	}
	data, err := EncodeRecord(vals)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	for i := range vals {
		if got.Values[i] != vals[i] {
			t.Errorf("values[%d]: got %v, want %v", i, got.Values[i], vals[i])
		}
	}
}

func TestRecordRoundTripBlobs(t *testing.T) {
	vals := []interface{}{
		[]byte{},
		[]byte{0x00, 0x01, 0x02},
		[]byte{0xFF, 0xFE},
	}
	data, err := EncodeRecord(vals)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	for i := range vals {
		b1, ok1 := got.Values[i].([]byte)
		b2, ok2 := vals[i].([]byte)
		if !ok1 || !ok2 {
			t.Errorf("values[%d]: type mismatch", i)
			continue
		}
		if !reflect.DeepEqual(b1, b2) {
			t.Errorf("values[%d]: got %v, want %v", i, b1, b2)
		}
	}
}

func TestRecordRoundTripMixed(t *testing.T) {
	vals := []interface{}{
		nil,
		int64(42),
		float64(3.14),
		"hello",
		[]byte{0x01, 0x02},
		int64(0),
		int64(1),
	}
	data, err := EncodeRecord(vals)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	for i := range vals {
		if !valuesEqual(got.Values[i], vals[i]) {
			t.Errorf("values[%d]: got %v (%T), want %v (%T)", i, got.Values[i], got.Values[i], vals[i], vals[i])
		}
	}
}

func valuesEqual(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	switch x := a.(type) {
	case []byte:
		y, ok := b.([]byte)
		return ok && reflect.DeepEqual(x, y)
	case float64:
		y, ok := b.(float64)
		if !ok {
			return false
		}
		if math.IsNaN(x) && math.IsNaN(y) {
			return true
		}
		return x == y
	default:
		return a == b
	}
}

func TestSerialTypeLength(t *testing.T) {
	tests := []struct {
		st     uint64
		want   int64
		err    bool
	}{
		{0, 0, false},
		{1, 1, false},
		{2, 2, false},
		{3, 3, false},
		{4, 4, false},
		{5, 6, false},
		{6, 8, false},
		{7, 8, false},
		{8, 0, false},
		{9, 0, false},
		{12, 0, false},  // blob length 0
		{13, 0, false},  // text length 0
		{14, 1, false},  // blob length 1
		{15, 1, false},  // text length 1
		{100, 44, false}, // text length 44
	}
	for _, tt := range tests {
		got, err := SerialTypeLength(tt.st)
		if tt.err {
			if err == nil {
				t.Errorf("SerialTypeLength(%d): expected error", tt.st)
			}
			return
		}
		if err != nil {
			t.Errorf("SerialTypeLength(%d): unexpected error: %v", tt.st, err)
			return
		}
		if got != tt.want {
			t.Errorf("SerialTypeLength(%d): got %d, want %d", tt.st, got, tt.want)
		}
	}
}

func TestCellTableLeafRoundTrip(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03, 0x04}
	c := &Cell{
		Type:    CellTableLeaf,
		RowID:   int64(42),
		Payload: payload,
	}
	data := EncodeCell(c)

	// We need to decode from within a page context
	// Create a minimal page buffer with the cell at offset 100
	pageData := make([]byte, 200)
	copy(pageData[100:], data)

	got, err := DecodeCell(pageData, 100, CellTableLeaf, 4096)
	if err != nil {
		t.Fatalf("DecodeCell: %v", err)
	}
	if got.RowID != c.RowID {
		t.Errorf("RowID: got %d, want %d", got.RowID, c.RowID)
	}
	if !reflect.DeepEqual(got.Payload, c.Payload) {
		t.Errorf("Payload: got %v, want %v", got.Payload, c.Payload)
	}
}

func TestCellTableInteriorRoundTrip(t *testing.T) {
	c := &Cell{
		Type:    CellTableInterior,
		LeftPtr: 42,
		RowID:   int64(100),
	}
	data := EncodeCell(c)
	got, err := DecodeCell(data, 0, CellTableInterior, 4096)
	if err != nil {
		t.Fatalf("DecodeCell: %v", err)
	}
	if got.LeftPtr != c.LeftPtr {
		t.Errorf("LeftPtr: got %d, want %d", got.LeftPtr, c.LeftPtr)
	}
	if got.RowID != c.RowID {
		t.Errorf("RowID: got %d, want %d", got.RowID, c.RowID)
	}
}

func TestCellIndexLeafRoundTrip(t *testing.T) {
	payload := []byte("hello index")
	c := &Cell{
		Type:    CellIndexLeaf,
		Payload: payload,
	}
	data := EncodeCell(c)
	pageData := make([]byte, 200)
	copy(pageData[100:], data)
	got, err := DecodeCell(pageData, 100, CellIndexLeaf, 4096)
	if err != nil {
		t.Fatalf("DecodeCell: %v", err)
	}
	if !reflect.DeepEqual(got.Payload, c.Payload) {
		t.Errorf("Payload: got %v, want %v", got.Payload, c.Payload)
	}
}

func TestCellIndexInteriorRoundTrip(t *testing.T) {
	payload := []byte("index key")
	c := &Cell{
		Type:    CellIndexInterior,
		LeftPtr: 7,
		Payload: payload,
	}
	data := EncodeCell(c)
	got, err := DecodeCell(data, 0, CellIndexInterior, 4096)
	if err != nil {
		t.Fatalf("DecodeCell: %v", err)
	}
	if got.LeftPtr != c.LeftPtr {
		t.Errorf("LeftPtr: got %d, want %d", got.LeftPtr, c.LeftPtr)
	}
	if !reflect.DeepEqual(got.Payload, c.Payload) {
		t.Errorf("Payload: got %v, want %v", got.Payload, c.Payload)
	}
}

func TestEncodeDecodeLargeRecord(t *testing.T) {
	// Build a record with many values to test header sizing
	vals := make([]interface{}, 100)
	for i := range vals {
		vals[i] = int64(i)
	}
	data, err := EncodeRecord(vals)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	got, err := DecodeRecord(data)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	for i := range vals {
		if got.Values[i] != vals[i] {
			t.Errorf("values[%d]: got %v, want %v", i, got.Values[i], vals[i])
		}
	}
}
