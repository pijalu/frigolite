package util

import (
	"bytes"
	"math"
	"testing"
)

func TestVarintRoundTrip(t *testing.T) {
	tests := []uint64{
		0, 1, 127, 128, 16383, 16384, 2097151, 2097152,
		268435455, 268435456, 34359738367, 34359738368,
		4398046511103, 4398046511104, 562949953421311, 562949953421312,
		72057594037927935, math.MaxUint64,
	}
	for _, v := range tests {
		buf := make([]byte, 10)
		n := PutVarint(buf, v)
		got, m := GetVarint(buf[:n])
		if got != v {
			t.Errorf("PutVarint/GetVarint(%d): got %d", v, got)
		}
		if m != n {
			t.Errorf("PutVarint(%d): wrote %d bytes, read %d", v, n, m)
		}
		if n != VarintLen(v) {
			t.Errorf("VarintLen(%d) = %d, but PutVarint wrote %d", v, VarintLen(v), n)
		}
	}
}

func TestVarintBoundaries(t *testing.T) {
	boundaries := []uint64{127, 128, 16383, 16384, 2097151, 2097152}
	for _, v := range boundaries {
		buf := make([]byte, 10)
		n := PutVarint(buf, v)
		got, m := GetVarint(buf[:n])
		if got != v {
			t.Errorf("boundary %d: got %d", v, got)
		}
		if m != n {
			t.Errorf("boundary %d: wrote %d, read %d", v, n, m)
		}
	}
}

func TestVarintLengths(t *testing.T) {
	tests := []struct {
		v    uint64
		want int
	}{
		{0, 1},
		{127, 1},
		{128, 2},
		{16383, 2},
		{16384, 3},
		{2097151, 3},
		{2097152, 4},
		{268435455, 4},
		{268435456, 5},
		{34359738367, 5},
		{34359738368, 6},
		{4398046511103, 6},
		{4398046511104, 7},
		{562949953421311, 7},
		{562949953421312, 8},
		{72057594037927935, 8},
		{72057594037927936, 9},
		{math.MaxUint64, 10},
	}
	for _, tt := range tests {
		got := VarintLen(tt.v)
		if got != tt.want {
			t.Errorf("VarintLen(%d) = %d, want %d", tt.v, got, tt.want)
		}
	}
}

func TestReadVarint(t *testing.T) {
	tests := []uint64{0, 1, 127, 128, 16383, 16384, 2097151, math.MaxUint64}
	for _, v := range tests {
		buf := make([]byte, 10)
		n := PutVarint(buf, v)
		r := bytes.NewReader(buf[:n])
		got, err := ReadVarint(r)
		if err != nil {
			t.Errorf("ReadVarint(%d): %v", v, err)
		}
		if got != v {
			t.Errorf("ReadVarint(%d): got %d", v, got)
		}
	}
}

func TestVarintFuzz(t *testing.T) {
	vals := []uint64{
		0, 1, 2, 126, 127, 128, 129,
		16382, 16383, 16384, 16385,
		0xFFFFFF, 0x1000000,
		0xFFFFFFFF, 0x100000000,
	}
	for _, v := range vals {
		buf := make([]byte, 10)
		n := PutVarint(buf, v)
		got, m := GetVarint(buf[:n])
		if got != v || m != n {
			t.Errorf("Fuzz %d: got %d/%d, want %d/%d", v, got, m, v, n)
		}
	}
}
