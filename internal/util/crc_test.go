package util

import (
	"hash/crc32"
	"testing"
)

func TestCRC32(t *testing.T) {
	data := []byte("hello world")
	got := CRC32(data)
	expected := crc32.ChecksumIEEE(data)
	if got != expected {
		t.Errorf("CRC32(%q) = %08x, want %08x", data, got, expected)
	}
}

func TestCRC32Empty(t *testing.T) {
	got := CRC32(nil)
	expected := crc32.ChecksumIEEE(nil)
	if got != expected {
		t.Errorf("CRC32(empty) = %08x, want %08x", got, expected)
	}
}
