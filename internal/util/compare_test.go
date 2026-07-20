package util

import "testing"

func TestCompareValues(t *testing.T) {
	tests := []struct {
		a, b interface{}
		want int
	}{
		{nil, nil, 0},
		{nil, int64(1), -1},
		{int64(1), nil, 1},
		{int64(1), int64(2), -1},
		{int64(2), int64(1), 1},
		{int64(1), int64(1), 0},
		{float64(1.5), float64(2.5), -1},
		{float64(2.5), float64(1.5), 1},
		{float64(1.0), float64(1.0), 0},
		{"abc", "def", -1},
		{"def", "abc", 1},
		{"abc", "abc", 0},
		{[]byte{1}, []byte{2}, -1},
		{[]byte{2}, []byte{1}, 1},
		{[]byte{1}, []byte{1}, 0},
		{int64(1), float64(1.0), 0}, // integer compare with float
	}

	for _, tt := range tests {
		got := CompareValues(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareValues(%v, %v) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestAffinity(t *testing.T) {
	tests := []struct {
		typeName string
		want     rune
	}{
		{"INTEGER", 'I'},
		{"INT", 'I'},
		{"BIGINT", 'I'},
		{"TEXT", 'T'},
		{"VARCHAR", 'T'},
		{"CHARACTER", 'T'},
		{"BLOB", 'B'},
		{"", 'B'},
		{"REAL", 'R'},
		{"FLOAT", 'R'},
		{"DOUBLE", 'R'},
		{"NUMERIC", 'N'},
		{"DECIMAL", 'N'},
	}
	for _, tt := range tests {
		got := Affinity(tt.typeName)
		if got != tt.want {
			t.Errorf("Affinity(%q) = %c, want %c", tt.typeName, got, tt.want)
		}
	}
}
