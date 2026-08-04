// Package util provides low-level utilities used throughout frigolite.
package util

import "io"

// VarintLen returns the number of bytes required to encode v as a SQLite
// variable-length integer. Values below 2^56 use 1-8 bytes (7 bits per byte);
// values from 2^56 up use 9 bytes (8×7 bits + 8 bits in the last byte).
func VarintLen(v uint64) int {
	if v < (1 << 56) {
		n := 1
		for v >= 0x80 {
			v >>= 7
			n++
		}
		return n
	}
	return 9
}

// PutVarint encodes v as a SQLite varint into buf, returning the number of
// bytes written.  The varint is encoded with standard 7-bit continuation:
// all bytes except the last have the high bit set; the last byte has it clear.
// Panics if buf is too small (use VarintLen to size it).
func PutVarint(buf []byte, v uint64) int {
	// Fast path for common small values
	if v <= 0x7f {
		buf[0] = byte(v)
		return 1
	}
	if v <= 0x3fff {
		buf[0] = byte(v>>7) | 0x80
		buf[1] = byte(v & 0x7f)
		return 2
	}
	return putVarintSlow(buf, v)
}

func putVarintSlow(buf []byte, v uint64) int {
	if v >= (1 << 56) {
		// 9-byte varint (SQLite): the low 8 bits go in the last byte, and
		// the remaining bits in 8 bytes of 7 bits each, all with the
		// continuation bit set.
		buf[8] = byte(v)
		v >>= 8
		for i := 7; i >= 0; i-- {
			buf[i] = byte(v&0x7f) | 0x80
			v >>= 7
		}
		return 9
	}
	var tmp [9]byte
	n := 0
	// Write 7-bit groups from LSB to MSB, all with continuation bit set
	for v != 0 {
		tmp[n] = byte(v&0x7f) | 0x80
		v >>= 7
		n++
	}
	// Clear continuation bit on the first group (LSB, which becomes the last byte)
	tmp[0] &= 0x7f
	// Reverse to big-endian order
	for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
		tmp[i], tmp[j] = tmp[j], tmp[i]
	}
	copy(buf, tmp[:n])
	return n
}

// GetVarint decodes a SQLite varint from buf, returning the value and the
// number of bytes consumed. buf must have at least 1 byte; otherwise returns
// (0, 1). SQLite varints are at most 9 bytes: the first 8 bytes carry 7 bits
// each with a continuation bit; if the 8th byte has the continuation bit set,
// a 9th byte follows carrying 8 bits (values >= 2^56).
//
// Fast path: ~90% of varints in practice are 1 byte (< 128).
func GetVarint(buf []byte) (uint64, int) {
	// Fast path: 1-byte varint (values 0-127)
	if len(buf) > 0 && buf[0] < 0x80 {
		return uint64(buf[0]), 1
	}
	// Fast path: 2-byte varint (values 128-16383)
	if len(buf) > 1 && buf[0] >= 0x80 && buf[1] < 0x80 {
		return uint64(buf[0]&0x7f)<<7 | uint64(buf[1]), 2
	}
	return getVarintSlow(buf)
}

func getVarintSlow(buf []byte) (uint64, int) {
	var v uint64
	n := 0
	for {
		if n >= len(buf) || n >= 9 {
			break
		}
		if n < 8 {
			// Standard 7-bit continuation for first 8 bytes
			v = (v << 7) | uint64(buf[n]&0x7f)
			n++
			if buf[n-1]&0x80 == 0 {
				break
			}
		} else {
			// 9th byte: 8 bits (SQLite extension for values >= 2^56).
			v = (v << 8) | uint64(buf[n])
			n++
			break
		}
	}
	return v, n
}

// ReadVarint reads a varint from an io.ByteReader.
func ReadVarint(r io.ByteReader) (uint64, error) {
	var v uint64
	for n := 0; n < 8; n++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v = (v << 7) | uint64(b&0x7f)
		if b&0x80 == 0 {
			return v, nil
		}
	}
	// 8th byte had the continuation bit: read the 9th byte with 8 bits
	// (SQLite encoding for values >= 2^56).
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	v = (v << 8) | uint64(b)
	return v, nil
}
