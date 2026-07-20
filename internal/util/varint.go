// Package util provides low-level utilities used throughout frigolite.
package util

import "io"

// VarintLen returns the number of bytes required to encode v as a SQLite
// variable-length integer.
func VarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
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
	var tmp [10]byte
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
// number of bytes consumed.
func GetVarint(buf []byte) (uint64, int) {
	var v uint64
	n := 0
	for {
		v = (v << 7) | uint64(buf[n]&0x7f)
		n++
		if buf[n-1]&0x80 == 0 {
			break
		}
	}
	return v, n
}

// ReadVarint reads a varint from an io.ByteReader.
func ReadVarint(r io.ByteReader) (uint64, error) {
	var v uint64
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v = (v << 7) | uint64(b&0x7f)
		if b&0x80 == 0 {
			break
		}
	}
	return v, nil
}
