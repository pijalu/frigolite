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
		// Rule 4 (no affinity either side → type ordering):
		// INTEGER sorts before TEXT regardless of numeric content, so
		// 1 <= '0' is TRUE even though 1 > 0 numerically. The magnitude
		// returned is the class delta (typeInteger-typeText = -2); callers
		// only rely on the sign.
		{int64(1), "0", -2},  // 1 < '0' (integer < text)
		{"0", int64(1), 2},   // '0' > 1
		{int64(3), "2", -2},  // 3 < '2'
		{int64(3), "10", -2}, // 3 < '10'
		{int64(5), "abc", -2},
		{"abc", int64(5), 2},
		// BLOB-affinity column value vs bare literal also uses type ordering.
		{&ColumnValue{Value: int64(3), Affinity: 'B'}, "0", -2}, // 3 < '0'
		{&ColumnValue{Value: "0", Affinity: 'B'}, int64(3), 2},  // '0' > 3
		// Numeric-affinity column vs bare literal converts the literal.
		{&ColumnValue{Value: int64(1), Affinity: 'I'}, "0", 1}, // 1 > 0
		{&ColumnValue{Value: "0", Affinity: 'N'}, int64(5), -1}, // 0 < 5
		// TEXT-affinity column vs bare numeric compares as text:
		// '10' < '5' and '10' < '9' are both true (lexical).
		{&ColumnValue{Value: "10", Affinity: 'T'}, int64(5), -1}, // '10' < '5'
		{&ColumnValue{Value: "10", Affinity: 'T'}, int64(9), -1}, // '10' < '9'
		// BLOB vs TEXT column with differing types: type ordering.
		{&ColumnValue{Value: int64(5), Affinity: 'B'}, &ColumnValue{Value: "2", Affinity: 'T'}, -2}, // 5 < '2'
		// Same-type values compare by value even across affinities.
		{&ColumnValue{Value: "5", Affinity: 'B'}, &ColumnValue{Value: "10", Affinity: 'T'}, 1}, // '5' > '10'
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
