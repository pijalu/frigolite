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

	// Decode serial type codes
	var serialTypes []uint64
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
// Serial types are allocated on a stack buffer when there are ≤16 columns.
// The header must lie within the record's own bytes (vdbe.c OP_Column
// op_column_corrupt parity) — a corrupt record reports
// "database disk image is malformed".
func ParseRecordHeader(data []byte) (serialTypes []uint64, dataStart int, err error) {
	pos := 0

	// Header size (varint)
	hdrSize, n := util.GetVarint(data[pos:])
	pos += n
	hdrEnd := int(hdrSize)
	if hdrEnd < pos || hdrEnd > len(data) {
		return nil, 0, fmt.Errorf("database disk image is malformed")
	}

	// Decode serial type codes. Use a stack-allocated array for common
	// column counts (≤16) to avoid heap allocation per row.
	var stackSerialTypes [16]uint64
	if hdrEnd-pos <= len(stackSerialTypes)*9 { // rough upper bound: each varint ≤ 9 bytes
		serialTypes = stackSerialTypes[:0]
	}
	for pos < hdrEnd {
		st, n := util.GetVarint(data[pos:])
		pos += n
		serialTypes = append(serialTypes, st)
	}

	return serialTypes, pos, nil
}

// DecodeRecordValuesFromTypes decodes record values into target using pre-parsed
// serial types and data offset. Only columns in colIndices are decoded (nil = all).
// This avoids re-parsing the record header when performing multi-phase decode.
func DecodeRecordValuesFromTypes(data []byte, dataStart int, target []interface{}, serialTypes []uint64, colIndices map[int]bool) int {
	return decodeRecordValuesFromTypes(data, dataStart, target, serialTypes, colIndices)
}

// decodeRecordValuesFromTypes decodes record values into target using pre-parsed
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

	buf := make([]byte, hdrSize+totalDataLen)
	pos := 0

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
		// buf is already zero-filled (allocated with make([]byte, n));
		// zeroblob content needs no copy
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
