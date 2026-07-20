package util

import "hash/crc32"

// SQLite uses a specific CRC-32 variant (ISO 3309 / IEEE 802.3) with
// polynomial 0xEDB88320.
//
// MakeTable returns a CRC table with the SQLite polynomial.
func MakeTable() *crc32.Table {
	return crc32.MakeTable(crc32.IEEE)
}

// CRC32 computes the SQLite CRC-32 of data.
func CRC32(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}


