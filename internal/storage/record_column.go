package storage

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/util"
)

// Direct column reads from a record payload (vdbe.c OP_Column parity): a
// column value is addressable WITHOUT decoding the whole record. The record
// header is a sequence of self-sized serial-type varints, so the byte offset
// of column k's value is the running sum of the sizes implied by serial types
// 0..k-1 — the values of the skipped columns are never read or boxed.
//
// Validation mirrors DecodeRecord exactly: a header extending past the
// payload is a corrupt record ("database disk image is malformed"), an
// unparseable serial-type varint or an unknown serial type (10/11) errors,
// and a value whose bytes extend past the payload errors. A column ordinal
// beyond the record's serial-type count is an ABSENT column (a row written
// before ALTER TABLE ADD COLUMN), not an error: the read reports it as NULL.

// DecodeRecordColumn reads one column's value directly from a record payload,
// returning (value, true, nil) for a stored value, (nil, false, nil) for a
// stored NULL or an absent column (ordinal >= the record's column count), and
// an error for a corrupt record. The value's Go type matches DecodeRecord's
// exactly (int64/float64/string/[]byte/nil).
func DecodeRecordColumn(data []byte, colIdx int) (interface{}, bool, error) {
	if colIdx < 0 {
		return nil, false, fmt.Errorf("storage: negative column index %d", colIdx)
	}
	var out [1]interface{}
	cols := [1]int{colIdx}
	if _, err := decodeRecordColumnsInto(data, cols[:], out[:], false); err != nil {
		return nil, false, err
	}
	if out[0] == nil {
		return nil, false, nil
	}
	return out[0], true, nil
}

// DecodeRecordColumns reads the requested columns directly from a record
// payload into the caller-reusable out slice (out[i] receives column cols[i];
// out must be at least len(cols) long and is fully reset). Absent columns
// (ordinal >= the record's column count) and stored NULLs leave nil. The
// returned count is the record's total serial-type count, so a caller can
// apply ALTER TABLE ADD COLUMN defaults for slots >= count exactly like the
// full decode does. Errors mirror DecodeRecord's corrupt-record validation;
// a caller that needs the full decode's silent-truncation semantics on
// crafted payloads must fall back to DecodeRecord on error.
func DecodeRecordColumns(data []byte, cols []int, out []interface{}) (int, error) {
	return decodeRecordColumnsInto(data, cols, out, true)
}

// DecodeRecordColumnsPrefix is DecodeRecordColumns with an early-exit header
// walk: the walk stops once every requested ordinal has been decoded, so the
// returned count is min(record's column count, max(cols)+1) — exact when the
// record is not longer than the highest requested slot, capped otherwise. A
// caller that needs absent-slot defaults for slots beyond the cap must
// re-read with DecodeRecordColumns (whose full walk returns the exact count).
func DecodeRecordColumnsPrefix(data []byte, cols []int, out []interface{}) (int, error) {
	return decodeRecordColumnsInto(data, cols, out, false)
}

// decodeRecordColumnsInto walks the record header, recording the requested
// columns' values. When walkAll is false the walk stops once every requested
// ordinal has been seen (the single-column primitive); when true it walks the
// whole header so the returned count is the record's exact column count.
func decodeRecordColumnsInto(data []byte, cols []int, out []interface{}, walkAll bool) (int, error) {
	if len(out) < len(cols) {
		return 0, fmt.Errorf("storage: output slice shorter than column list")
	}
	for i := range out {
		out[i] = nil
	}
	typesStart, hdrEnd, err := recordHeaderBounds(data)
	if err != nil {
		return 0, err
	}
	lastCol, err := requestedColumnBounds(cols)
	if err != nil {
		return 0, err
	}
	return walkRecordColumns(data, typesStart, hdrEnd, cols, out, lastCol, walkAll)
}

// walkRecordColumns reads the serial-type varints in [typesStart, hdrEnd),
// decoding each requested column from its byte offset (the running sum of the
// preceding serial types' sizes) and returning the column count walked.
func walkRecordColumns(data []byte, typesStart, hdrEnd int, cols []int, out []interface{}, lastCol int, walkAll bool) (int, error) {
	pos, dataOff, col := typesStart, hdrEnd, 0
	for pos < hdrEnd {
		st, n := util.GetVarint(data[pos:])
		if n == 0 {
			return 0, fmt.Errorf("storage: corrupt record header at offset %d", pos)
		}
		pos += n
		valLen, err := SerialTypeLength(st)
		if err != nil {
			return 0, err
		}
		if col <= lastCol {
			if err := decodeColumnAt(data, st, dataOff, int(valLen), col, cols, out); err != nil {
				return 0, err
			}
		}
		dataOff += int(valLen)
		col++
		if !walkAll && col > lastCol {
			break
		}
	}
	return col, nil
}

// recordHeaderBounds parses a record's header-size varint and validates that
// the header lies within the record's own bytes (vdbe.c OP_Column's
// op_column_corrupt check, identical to DecodeRecord's). Returns the offset
// where the serial-type varints start and the offset where they end (the
// value data's start: the header spans [0, hdrSize) including the size
// varint itself).
func recordHeaderBounds(data []byte) (typesStart, hdrEnd int, err error) {
	hdrSize, n := util.GetVarint(data)
	if n == 0 {
		return 0, 0, fmt.Errorf("storage: corrupt record header size")
	}
	hdrEnd = int(hdrSize)
	if hdrEnd < n || hdrEnd > len(data) {
		return 0, 0, fmt.Errorf("database disk image is malformed")
	}
	return n, hdrEnd, nil
}

// requestedColumnBounds returns the highest requested column ordinal,
// rejecting negative indices.
func requestedColumnBounds(cols []int) (int, error) {
	last := -1
	for _, c := range cols {
		if c < 0 {
			return 0, fmt.Errorf("storage: negative column index %d", c)
		}
		if c > last {
			last = c
		}
	}
	return last, nil
}

// decodeColumnAt decodes column col (serial type st, valLen value bytes at
// dataOff) into every requested out slot that names it. Absent columns never
// reach here (the walk ends before their ordinal), so a stored NULL simply
// leaves the slot nil.
func decodeColumnAt(data []byte, st uint64, dataOff, valLen, col int, cols []int, out []interface{}) error {
	for i, c := range cols {
		if c != col {
			continue
		}
		if dataOff+valLen > len(data) {
			return fmt.Errorf("storage: record data too short at value %d: need %d bytes at offset %d, have %d", col, valLen, dataOff, len(data))
		}
		out[i] = decodeValue(st, data[dataOff:dataOff+valLen])
	}
	return nil
}
