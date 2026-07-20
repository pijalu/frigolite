package util

import (
	"bytes"
	"math"
	"strings"
)

// CompareValues compares two SQL values according to SQLite affinity rules.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
//
// SQLite type ordering: NULL < INTEGER/REAL < TEXT < BLOB
// INTEGER and REAL are compared numerically after promoting both to REAL.
func CompareValues(a, b interface{}) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}

	ta, tb := classifyValue(a), classifyValue(b)

	// INTEGER and REAL are mutually comparable (both are numeric)
	if isNumeric(ta) && isNumeric(tb) {
		fa, fb := toFloat64(a), toFloat64(b)
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		default:
			return 0
		}
	}

	// Different types: compare by type ordering
	if ta != tb {
		return int(ta) - int(tb)
	}

	// Same type: compare by value
	switch ta {
	case typeText:
		return strings.Compare(toStr(a), toStr(b))
	case typeBlob:
		return bytes.Compare(toBytes(a), toBytes(b))
	default:
		return 0
	}
}

type valueClass int

const (
	typeNull valueClass = iota
	typeInteger
	typeReal
	typeText
	typeBlob
)

func isNumeric(c valueClass) bool {
	return c == typeInteger || c == typeReal
}

func classifyValue(v interface{}) valueClass {
	if v == nil {
		return typeNull
	}
	switch v.(type) {
	case int64:
		return typeInteger
	case float64:
		return typeReal
	case string:
		return typeText
	case []byte:
		return typeBlob
	default:
		return typeText
	}
}

func toFloat64(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	default:
		return math.NaN()
	}
}

func toStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toBytes(v interface{}) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	return nil
}

// Affinity determines the column affinity for a type name.
// SQLite affinities: TEXT, NUMERIC, INTEGER, REAL, BLOB.
func Affinity(typeName string) rune {
	upper := strings.ToUpper(strings.TrimSpace(typeName))
	if strings.Contains(upper, "INT") {
		return 'I' // INTEGER
	}
	if strings.Contains(upper, "CHAR") || strings.Contains(upper, "CLOB") || strings.Contains(upper, "TEXT") {
		return 'T' // TEXT
	}
	if strings.Contains(upper, "BLOB") || typeName == "" {
		return 'B' // BLOB
	}
	if strings.Contains(upper, "REAL") || strings.Contains(upper, "FLOA") || strings.Contains(upper, "DOUB") {
		return 'R' // REAL
	}
	return 'N' // NUMERIC
}
