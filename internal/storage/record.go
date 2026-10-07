package storage

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
)

// Record format primitives (split from storage.go for file-size hygiene):
// serial types, record header parsing, value decoding, and record encoding.
// SerialType constants for record encoding.
const (
	SerialNull  = 0
	SerialInt8  = 1
	SerialInt16 = 2
	SerialInt24 = 3
	SerialInt32 = 4
	SerialInt48 = 5
	SerialInt64 = 6
	SerialFloat = 7
	SerialZero  = 8
	SerialOne   = 9
	SerialMin   = 12 // first usable string/blob serial type
)

// SerialTypeLength returns the data length for a serial type code.
// Returns the byte length of the value.
func SerialTypeLength(serialType uint64) (int64, error) {
	switch {
	case serialType == SerialNull:
		return 0, nil
	case serialType >= SerialInt8 && serialType <= SerialInt64:
		// Serial type -> byte length: 1→1, 2→2, 3→3, 4→4, 5→6, 6→8
		switch serialType {
		case 5:
			return 6, nil
		case 6:
			return 8, nil
		default:
			return int64(serialType), nil
		}
	case serialType == SerialFloat:
		return 8, nil
	case serialType == SerialZero || serialType == SerialOne:
		return 0, nil
	case serialType >= SerialMin:
		if serialType%2 == 0 {
			return int64((serialType - 12) / 2), nil
		}
		return int64((serialType - 13) / 2), nil
	default:
		return 0, fmt.Errorf("storage: unknown serial type: %d", serialType)
	}
}

// Record represents a decoded SQLite record (row).
type Record struct {
	Values []interface{}
}

// DecodeRecord decodes a record from a byte slice.
func DecodeRecord(data []byte) (*Record, error) {
	pos := 0

	// Header size (varint)
	hdrSize, n := util.GetVarint(data[pos:])
	if n == 0 {
		return nil, fmt.Errorf("storage: corrupt record header size")
	}
	pos += n
	hdrEnd := int(hdrSize)

	// The header must lie within the record's own bytes (vdbe.c OP_Column
	// op_column_corrupt parity): a header extending past the data is a
	// corrupt record, not an invitation to parse unbounded bytes.
	if hdrEnd < pos || hdrEnd > len(data) {
		return nil, fmt.Errorf("database disk image is malformed")
	}

	// Decode serial type codes. Serial-type varints are at least one byte
	// each, so hdrEnd-pos bounds the type count; a stack buffer serves the
	// common (<=16 column) records without touching the heap. append
	// reallocates onto the heap beyond the bound — the parse is identical,
	// only the scratch storage differs.
	var stackSerialTypes [16]uint64
	serialTypes := stackSerialTypes[:0]
	for pos < hdrEnd {
		st, n := util.GetVarint(data[pos:])
		if n == 0 {
			return nil, fmt.Errorf("storage: corrupt record header at offset %d", pos)
		}
		pos += n
		serialTypes = append(serialTypes, st)
	}

	// Decode values
	r := &Record{Values: make([]interface{}, len(serialTypes))}
	for i, st := range serialTypes {
		valLen, err := SerialTypeLength(st)
		if err != nil {
			return nil, err
		}
		if pos+int(valLen) > len(data) {
			return nil, fmt.Errorf("storage: record data too short at value %d: need %d bytes at offset %d, have %d", i, valLen, pos, len(data))
		}
		v := decodeValue(st, data[pos:pos+int(valLen)])
		r.Values[i] = v
		pos += int(valLen)
	}

	return r, nil
}

// ParseRecordHeader parses a SQLite record header and returns the serial type
// codes for each column and the byte offset where the value data begins.
// The value data starts at the returned dataStart offset within the data slice.
// The header must lie within the record's own bytes (vdbe.c OP_Column
// op_column_corrupt parity) — a corrupt record reports
// "database disk image is malformed".
//
// The returned slice is freshly allocated: callers that parse per row should
// use ParseRecordHeaderInto with a reused buffer instead (the per-row form
// allocates — the buffer backing the returned slice cannot stay on the
// caller's stack because it escapes through the return).
func ParseRecordHeader(data []byte) (serialTypes []uint64, dataStart int, err error) {
	return ParseRecordHeaderInto(data, nil)
}

// ParseRecordHeaderInto is ParseRecordHeader appending into a caller-owned
// buffer: serialTypes = append(buf, ...) — pass buf[:0] (or a retained slice
// re-sliced to [:0]) to reuse one buffer across every row of a scan or the
// statements of a cursor. The parse (varint boundaries, the corrupt-header
// check, the returned dataStart) is byte-identical to ParseRecordHeader's.
// The result is scratch: no callee retains it, and callers must not retain it
// past the next call that reuses the buffer.
func ParseRecordHeaderInto(data []byte, buf []uint64) ([]uint64, int, error) {
	pos := 0

	// Header size (varint)
	hdrSize, n := util.GetVarint(data[pos:])
	pos += n
	hdrEnd := int(hdrSize)
	if hdrEnd < pos || hdrEnd > len(data) {
		return nil, 0, fmt.Errorf("database disk image is malformed")
	}

	for pos < hdrEnd {
		st, n := util.GetVarint(data[pos:])
		pos += n
		buf = append(buf, st)
	}

	return buf, pos, nil
}

// DecodeRecordValuesFromTypes decodes record values into target using pre-parsed
// serial types and data offset. Only columns in colIndices are decoded (nil = all).
// This avoids re-parsing the record header when performing multi-phase decode.
func DecodeRecordValuesFromTypes(data []byte, dataStart int, target []interface{}, serialTypes []uint64, colIndices map[int]bool) int {
	return decodeRecordValuesFromTypes(data, dataStart, target, serialTypes, colIndices)
}

// DecodeRecordValuesFromTypesCols is DecodeRecordValuesFromTypes with the
// decode-selection set as a by-index bool slice (cols[i] selects on-disk
// column i; nil = all). The scan/seek paths precompute the set once per
// statement; a slice index replaces the per-column map lookup in the row loop.
// Selection semantics are identical to the map form.
func DecodeRecordValuesFromTypesCols(data []byte, dataStart int, target []interface{}, serialTypes []uint64, cols []bool) int {
	pos := dataStart
	count := len(serialTypes)
	if count > len(target) {
		count = len(target)
	}
	decodeAll := cols == nil
	for i := 0; i < count; i++ {
		valLen, err := SerialTypeLength(serialTypes[i])
		if err != nil {
			return i
		}
		// bounds check (safety: skip instead of panic for corrupted data)
		if pos+int(valLen) > len(data) {
			// truncated record — stop decoding
			return i
		}
		if decodeAll || cols[i] {
			target[i] = decodeValue(serialTypes[i], data[pos:pos+int(valLen)])
		}
		// For skipped columns (not selected), advance past the data but
		// leave target[i] as its zero value (nil).
		pos += int(valLen)
	}
	return count
}

// DecodeRecordValuesInto decodes a record's values straight into target in
// one call — the header walk and the value fill of the
// ParseRecordHeaderInto + DecodeRecordValuesFromTypesCols pair — with no
// intermediate serial-type slice on the heap (a stack buffer serves the
// common ≤16-column records; append spills to the heap beyond it, parse
// identical) and no Record struct. cols[i] selects on-disk column i for
// decoding (nil = all); a selected column decodes into target[i] directly,
// a skipped column advances past its data leaving target[i] nil.
//
// Returns the record's stored-column count — the header's serial-type count,
// the value the phase-one row assembly needs — even when target is narrower
// than the record or the value data is truncated; a truncation or an unknown
// serial type stops the fill early WITHOUT error (DecodeRecordValuesFromTypesCols
// semantics — the pre-filled target slots and the count are exactly what the
// two-call form produces). Only a header extending past the record's own
// bytes errors ("database disk image is malformed", op_column_corrupt
// parity), the same condition ParseRecordHeader reports.
func DecodeRecordValuesInto(data []byte, target []interface{}, cols []bool) (int, error) {
	pos := 0

	// Header size (varint)
	hdrSize, n := util.GetVarint(data[pos:])
	pos += n
	hdrEnd := int(hdrSize)
	if hdrEnd < pos || hdrEnd > len(data) {
		return 0, fmt.Errorf("database disk image is malformed")
	}

	// Serial type codes. types does not outlive this call, so the stack
	// buffer stays on the stack for the common column counts.
	var stackSerialTypes [16]uint64
	types := stackSerialTypes[:0]
	for pos < hdrEnd {
		st, n := util.GetVarint(data[pos:])
		pos += n
		types = append(types, st)
	}
	// dataStart is the post-header pos, exactly ParseRecordHeader's: a final
	// header varint straddling hdrSize leaves it past hdrEnd, and the value
	// walk starts there (the two-call form's dataStart).
	dataStart := pos

	// Values — the DecodeRecordValuesFromTypesCols fill, verbatim.
	pos = dataStart
	count := len(types)
	if count > len(target) {
		count = len(target)
	}
	decodeAll := cols == nil
	for i := 0; i < count; i++ {
		valLen, err := SerialTypeLength(types[i])
		if err != nil {
			return len(types), nil
		}
		// bounds check (safety: skip instead of panic for corrupted data)
		if pos+int(valLen) > len(data) {
			// truncated record — stop decoding
			return len(types), nil
		}
		if decodeAll || cols[i] {
			target[i] = decodeValue(types[i], data[pos:pos+int(valLen)])
		}
		// For skipped columns (not selected), advance past the data but
		// leave target[i] as its zero value (nil).
		pos += int(valLen)
	}
	return len(types), nil
}

// DecodeSerialInt64 decodes an integer serial type's (st ∈ [SerialInt8,
// SerialInt64]) big-endian signed value without boxing. ok=false when st is
// not an integer serial type.
func DecodeSerialInt64(serialType uint64, data []byte) (int64, bool) {
	switch serialType {
	case SerialInt8:
		return int64(int8(data[0])), true
	case SerialInt16:
		return int64(int16(binary.BigEndian.Uint16(data))), true
	case SerialInt24:
		v := uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2])
		if v&0x800000 != 0 {
			v |= 0xFF000000 // sign extend
		}
		return int64(int32(v)), true
	case SerialInt32:
		return int64(int32(binary.BigEndian.Uint32(data))), true
	case SerialInt48:
		v := uint64(data[0])<<40 | uint64(data[1])<<32 | uint64(data[2])<<24 |
			uint64(data[3])<<16 | uint64(data[4])<<8 | uint64(data[5])
		if v&0x800000000000 != 0 {
			v |= 0xFFFF000000000000
		}
		return int64(v), true
	case SerialInt64:
		return int64(binary.BigEndian.Uint64(data)), true
	}
	return 0, false
}

// DecodeSerialFloat64 decodes the float serial type's big-endian value
// without boxing.
func DecodeSerialFloat64(data []byte) float64 {
	return math.Float64frombits(binary.BigEndian.Uint64(data))
}

// DecodeRecordValue decodes one value of the given serial type from its data
// bytes — decodeValue's exported form for callers that read single columns
// straight off a parsed record header.
func DecodeRecordValue(serialType uint64, data []byte) interface{} {
	return decodeValue(serialType, data)
}

func decodeRecordValuesFromTypes(data []byte, dataStart int, target []interface{}, serialTypes []uint64, colIndices map[int]bool) int {
	pos := dataStart
	count := len(serialTypes)
	if count > len(target) {
		count = len(target)
	}
	decodeAll := colIndices == nil
	for i := 0; i < count; i++ {
		valLen, err := SerialTypeLength(serialTypes[i])
		if err != nil {
			return i
		}
		// bounds check (safety: skip instead of panic for corrupted data)
		if pos+int(valLen) > len(data) {
			// truncated record — stop decoding
			return i
		}
		if decodeAll || colIndices[i] {
			target[i] = decodeValue(serialTypes[i], data[pos:pos+int(valLen)])
		}
		// For skipped columns (not in colIndices), advance past the data but
		// leave target[i] as its zero value (nil).
		pos += int(valLen)
	}
	return count
}

func decodeValue(serialType uint64, data []byte) interface{} {
	switch {
	case serialType == SerialNull:
		return nil
	case serialType == SerialZero, serialType == SerialOne:
		return smallIntValue(serialType)
	case serialType == SerialInt8, serialType == SerialInt16, serialType == SerialInt32, serialType == SerialInt64:
		return decodeBigEndianInt(serialType, data)
	case serialType == SerialInt24:
		v := uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2])
		if v&0x800000 != 0 {
			v |= 0xFF000000 // sign extend
		}
		return int64(int32(v))
	case serialType == SerialInt48:
		v := uint64(data[0])<<40 | uint64(data[1])<<32 | uint64(data[2])<<24 |
			uint64(data[3])<<16 | uint64(data[4])<<8 | uint64(data[5])
		if v&0x800000000000 != 0 {
			v |= 0xFFFF000000000000
		}
		return int64(v)
	case serialType == SerialFloat:
		return float64(math.Float64frombits(binary.BigEndian.Uint64(data)))
	default:
		if serialType%2 == 0 {
			// Blob
			b := make([]byte, len(data))
			copy(b, data)
			return b
		}
		// Text
		return string(data)
	}
}

// smallIntValue returns the int64 value of the SerialZero/SerialOne cases.
func smallIntValue(serialType uint64) interface{} {
	if serialType == SerialOne {
		return int64(1)
	}
	return int64(0)
}

// decodeBigEndianInt decodes a big-endian signed integer of the size implied
// by the given integer serial type.
func decodeBigEndianInt(serialType uint64, data []byte) interface{} {
	switch serialType {
	case SerialInt8:
		return int64(int8(data[0]))
	case SerialInt16:
		return int64(int16(binary.BigEndian.Uint16(data)))
	case SerialInt32:
		return int64(int32(binary.BigEndian.Uint32(data)))
	case SerialInt64:
		return int64(binary.BigEndian.Uint64(data))
	}
	return nil
}

// EncodeRecord encodes a record from a slice of Go values.
func EncodeRecord(values []interface{}) ([]byte, error) {
	return AppendEncodeRecord(nil, values)
}

// AppendEncodeRecord appends the record encoding of values to buf (which may
// be a caller's reusable buffer) and returns the extended slice. The encoding
// is byte-identical to EncodeRecord's.
func AppendEncodeRecord(buf []byte, values []interface{}) ([]byte, error) {
	// Fixed-arity all-integer fast path (the bulk-load shape: an INTEGER
	// PRIMARY KEY rowid-alias NULL plus integer columns, or plain integer
	// tuples): every serial type is at most SerialInt64 (6), so each type
	// varint is one byte and the header size needs no self-consistent
	// varint fixpoint — the record is header [n+1][n type bytes][values],
	// emitted straight from the values with no staging arrays.
	if n := len(values); n > 0 && n <= 16 {
		var ints [16]int64
		allInt := true
		for i, v := range values {
			iv, ok := v.(int64)
			if !ok {
				allInt = false
				break
			}
			ints[i] = iv
		}
		if allInt {
			return appendEncodeIntRecord(buf, ints[:n])
		}
	}
	return appendEncodeRecordGeneric(buf, values)
}

// appendEncodeIntRecord encodes an all-int64 record (arity ≤ 16) byte-identical
// to the generic walk: encodeInt64Size classifies each value, the header is
// the 1-byte size varint plus the 1-byte serial-type varints, and each value's
// big-endian two's-complement bytes follow in order.
func appendEncodeIntRecord(buf []byte, ints []int64) ([]byte, error) {
	n := len(ints)
	var serialTypes [16]uint64
	var dataLens [16]int
	hdrSize := n + 1
	totalDataLen := 0
	for i, v := range ints {
		st, dl := encodeInt64Size(v)
		serialTypes[i] = st
		dataLens[i] = dl
		totalDataLen += dl
	}

	// SQLite test instrumentation (UPDATE_MAX_BLOBSIZE on OP_MakeRecord): no
	// zeroblob can appear on this path, so the record size counts in full.
	updateMaxBlobsize(hdrSize + totalDataLen)

	start := len(buf)
	grow := hdrSize + totalDataLen
	if cap(buf)-start >= grow {
		buf = buf[:start+grow]
	} else {
		buf = append(buf, make([]byte, grow)...)
	}
	pos := start
	buf[pos] = byte(hdrSize)
	pos++
	for i := 0; i < n; i++ {
		buf[pos] = byte(serialTypes[i])
		pos++
	}
	for i, v := range ints {
		encodeInt64Into(v, buf[pos:pos+dataLens[i]])
		pos += dataLens[i]
	}
	return buf, nil
}

// appendEncodeRecordGeneric is AppendEncodeRecord's two-pass walk for mixed
// or wide rows (the pre-fast-path body, unchanged).
func appendEncodeRecordGeneric(buf []byte, values []interface{}) ([]byte, error) {
	// Optimized: avoid per-value byte slice allocations by computing sizes
	// first, then writing directly into a single output buffer.

	// First pass: compute serial types and data sizes
	// Use stack arrays for common column counts
	var stackSerialTypes [16]uint64
	var stackDataLens [16]int
	serialTypes := stackSerialTypes[:0]
	dataLens := stackDataLens[:0]

	for _, v := range values {
		st, dl := encodeValueSize(v)
		serialTypes = append(serialTypes, st)
		dataLens = append(dataLens, dl)
	}

	// Compute serial type varint total length
	var serialTypesLen int
	for _, st := range serialTypes {
		serialTypesLen += util.VarintLen(st)
	}

	// Header size = size of header-size varint + sum of serial-type varints
	hdrSize := serialTypesLen + 1
	for {
		hdrSizeLen := util.VarintLen(uint64(hdrSize))
		newHdrSize := serialTypesLen + hdrSizeLen
		if newHdrSize == hdrSize {
			break
		}
		hdrSize = newHdrSize
	}

	// Total data length
	var totalDataLen int
	for _, dl := range dataLens {
		totalDataLen += dl
	}

	// SQLite test instrumentation (UPDATE_MAX_BLOBSIZE on OP_MakeRecord,
	// src/vdbe.c): the record Mem is nHdr+nData bytes; zeroblobs met while
	// scanning backwards before any real data (i.e. followed only by NULLs
	// and other zeroblobs) stay unexpanded as an nZero tail and are NOT
	// counted. NULLs carry zero data bytes so they pass through.
	instrumented := hdrSize + totalDataLen
	for i := len(values) - 1; i >= 0; i-- {
		switch v := values[i].(type) {
		case value.ZeroBlob:
			instrumented -= v.N
		case nil:
			// zero data bytes; keep scanning
		default:
			goto blobsizeDone
		}
	}
blobsizeDone:
	updateMaxBlobsize(instrumented)

	// Build the record

	start := len(buf)
	grow := hdrSize + totalDataLen
	if cap(buf)-start >= grow {
		buf = buf[:start+grow]
	} else {
		buf = append(buf, make([]byte, grow)...)
	}
	pos := start

	// Header size varint
	pos += util.PutVarint(buf[pos:], uint64(hdrSize))

	// Serial types
	for _, st := range serialTypes {
		pos += util.PutVarint(buf[pos:], st)
	}

	// Values — write directly into the buffer
	for i, v := range values {
		encodeValueInto(v, buf[pos:pos+dataLens[i]])
		pos += dataLens[i]
	}

	return buf, nil
}

// encodeValueSize returns the serial type and data length for a value
// without allocating any byte slices.
func encodeValueSize(v interface{}) (uint64, int) {
	switch val := v.(type) {
	case nil:
		return SerialNull, 0
	case int64:
		return encodeInt64Size(val)
	case float64:
		return SerialFloat, 8
	case string:
		return uint64(13 + len(val)*2), len(val)
	case []byte:
		return uint64(12 + len(val)*2), len(val)
	case value.ZeroBlob:
		// zeroblob(N): blob serial type covers N bytes; the zeros are
		// written into the record payload without materializing a buffer
		// (SQLite MEM_Zero, expanded on demand).
		return uint64(12 + val.N*2), val.N
	default:
		s := fmt.Sprintf("%v", v)
		return uint64(13 + len(s)*2), len(s)
	}
}

// EncodeValueSize returns the serial type and data length for a value
// without allocating any byte slices (exported for size checks outside the
// storage package).
func EncodeValueSize(v interface{}) (uint64, int) {
	return encodeValueSize(v)
}

// encodeInt64Size returns the serial type and data length for an int64
// without allocating.
func encodeInt64Size(val int64) (uint64, int) {
	if val == 0 || val == 1 {
		return smallIntSerial(val)
	}
	return intSizeSerial(val)
}

// smallIntSerial returns the serial type for the int64 values 0 and 1.
func smallIntSerial(val int64) (uint64, int) {
	if val == 1 {
		return SerialOne, 0
	}
	return SerialZero, 0
}

// intSizeSerial returns the serial type for a non-0/1 int64 based on its
// magnitude. Negative magnitudes use ^val (bitwise NOT = -(val+1)) so the
// range boundaries match the signed bounds exactly and MinInt64 falls through
// to the 8-byte SerialInt64.
func intSizeSerial(val int64) (uint64, int) {
	mag := val
	if val < 0 {
		mag = ^val
	}
	switch {
	case mag <= 127:
		return SerialInt8, 1
	case mag <= 32767:
		return SerialInt16, 2
	case mag <= 8388607:
		return SerialInt24, 3
	case mag <= 2147483647:
		return SerialInt32, 4
	case mag <= 140737488355327:
		return SerialInt48, 6
	default:
		return SerialInt64, 8
	}
}

// encodeValueInto writes a value's data directly into the given buffer.
// The buffer must be pre-sized correctly (use encodeValueSize to compute).
func encodeValueInto(v interface{}, buf []byte) {
	switch val := v.(type) {
	case nil:
		// no data
	case int64:
		encodeInt64Into(val, buf)
	case float64:
		binary.BigEndian.PutUint64(buf, math.Float64bits(val))
	case string:
		copy(buf, val)
	case []byte:
		copy(buf, val)
	case value.ZeroBlob:
		// zeroblob content is all zeros. Callers encoding into a FRESH
		// buffer (EncodeRecord's make) relied on the zero-fill; the insert
		// path's REUSABLE record buffer (PERF.INSERT2-4's insRecBuf) keeps
		// the previous record's bytes in exactly this tail, which then
		// leaked onto disk as the zeroblob's content (rtree xCreate seeded
		// each new tree's root node from the previous rtree's node blob).
		// Expand the zeros explicitly, SQLite MEM_Zero-on-demand parity.
		clear(buf)
	default:
		s := fmt.Sprintf("%v", v)
		copy(buf, s)
	}
}

func encodeInt64Into(val int64, buf []byte) {
	switch {
	case val == 0, val == 1:
		// no data
	case val >= -128 && val <= 127:
		buf[0] = byte(int8(val))
	case val >= -32768 && val <= 32767:
		binary.BigEndian.PutUint16(buf, uint16(int16(val)))
	case val >= -8388608 && val <= 8388607:
		v := uint32(int32(val))
		buf[0] = byte(v >> 16)
		buf[1] = byte(v >> 8)
		buf[2] = byte(v)
	case val >= -2147483648 && val <= 2147483647:
		binary.BigEndian.PutUint32(buf, uint32(int32(val)))
	case val >= -140737488355328 && val <= 140737488355327:
		v := uint64(val)
		buf[0] = byte(v >> 40)
		buf[1] = byte(v >> 32)
		buf[2] = byte(v >> 24)
		buf[3] = byte(v >> 16)
		buf[4] = byte(v >> 8)
		buf[5] = byte(v)
	default:
		binary.BigEndian.PutUint64(buf, uint64(val))
	}
}
