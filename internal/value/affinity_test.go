package value

import (
	"testing"
)

func TestAffinity(t *testing.T) {
	tests := []struct {
		typeName string
		want     rune
	}{
		// INTEGER affinity
		{"INT", 'I'},
		{"INTEGER", 'I'},
		{"TINYINT", 'I'},
		{"BIGINT", 'I'},
		{"INT8", 'I'},

		// TEXT affinity
		{"TEXT", 'T'},
		{"CHARACTER", 'T'},
		{"VARCHAR(255)", 'T'},
		{"CLOB", 'T'},
		{"NCHAR(10)", 'T'},

		// BLOB affinity (empty type name or BLOB)
		{"BLOB", 'B'},
		{"", 'B'},

		// REAL affinity
		{"REAL", 'R'},
		{"FLOAT", 'R'},
		{"DOUBLE", 'R'},
		{"DOUBLE PRECISION", 'R'},

		// NUMERIC affinity (default)
		{"NUMERIC", 'N'},
		{"DECIMAL", 'N'},
		{"BOOLEAN", 'N'},
		{"DATE", 'N'},
	}
	for _, tt := range tests {
		got := Affinity(tt.typeName)
		if got != tt.want {
			t.Errorf("Affinity(%q) = %q, want %q", tt.typeName, got, tt.want)
		}
	}
}

func TestApplyColumnAffinity(t *testing.T) {
	// INTEGER affinity: convert float to int
	result := ApplyColumnAffinity(float64(3.99), "INTEGER")
	if result != int64(3) {
		t.Errorf("ApplyColumnAffinity(3.99, INTEGER) = %v, want 3", result)
	}

	// REAL affinity: convert int to float
	result = ApplyColumnAffinity(int64(42), "REAL")
	if result != float64(42) {
		t.Errorf("ApplyColumnAffinity(42, REAL) = %v, want 42.0", result)
	}

	// TEXT affinity: convert int to string
	result = ApplyColumnAffinity(int64(42), "TEXT")
	if result != "42" {
		t.Errorf("ApplyColumnAffinity(42, TEXT) = %v, want \"42\"", result)
	}

	// NULL passes through
	result = ApplyColumnAffinity(nil, "INTEGER")
	if result != nil {
		t.Errorf("ApplyColumnAffinity(nil, INTEGER) = %v, want nil", result)
	}

	// BLOB affinity: no conversion
	result = ApplyColumnAffinity("hello", "BLOB")
	if result != "hello" {
		t.Errorf("ApplyColumnAffinity(hello, BLOB) = %v, want hello", result)
	}
}
