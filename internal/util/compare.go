package util

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CompareValues compares two SQL values according to SQLite affinity rules.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
//
// SQLite type ordering: NULL < INTEGER/REAL < TEXT < BLOB
// INTEGER and REAL are compared numerically after promoting both to REAL.
func CompareValues(a, b interface{}) int {
	return CompareValuesCollate(a, b, "")
}

// CompareValuesCollate compares two SQL values with an optional collation.
// collation can be "NOCASE", "RTRIM", "BINARY", or "" (defaults to BINARY).
func CompareValuesCollate(a, b interface{}, collation string) int {
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

	// SQLite affinity: when comparing numeric with text, try to convert
	// text to numeric. This matches SQLite's type affinity rules for
	// comparisons (rules 2, 3 from SQLite docs on type affinity).
	if isNumeric(ta) && tb == typeText {
		return compareNumericText(a, b, -1)
	}
	if isNumeric(tb) && ta == typeText {
		return compareTextNumeric(a, b, 1)
	}

	// Different types: compare by type ordering
	if ta != tb {
		return int(ta) - int(tb)
	}

	// Same type: compare by value
	switch ta {
	case typeText:
		return stringCompare(toStr(a), toStr(b), collation)
	case typeBlob:
		return bytes.Compare(toBytes(a), toBytes(b))
	default:
		return 0
	}
}

// compareNumericText compares a numeric value a with a text value b.
// If b can be parsed as a number, compare numerically; otherwise
// return typeOrder (numeric < text).
func compareNumericText(a, b interface{}, typeOrder int) int {
	if f, err := strconv.ParseFloat(toStr(b), 64); err == nil {
		fa := toFloat64(a)
		switch {
		case fa < f:
			return -1
		case fa > f:
			return 1
		default:
			return 0
		}
	}
	return typeOrder
}

// compareTextNumeric compares a text value a with a numeric value b.
// If a can be parsed as a number, compare numerically; otherwise
// return typeOrder (text > numeric).
func compareTextNumeric(a, b interface{}, typeOrder int) int {
	if f, err := strconv.ParseFloat(toStr(a), 64); err == nil {
		fb := toFloat64(b)
		switch {
		case f < fb:
			return -1
		case f > fb:
			return 1
		default:
			return 0
		}
	}
	return typeOrder
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

// ApplyColumnAffinity coerces a Go value to match the SQL column affinity
// of the given type name. This implements SQLite's type affinity rules.
func ApplyColumnAffinity(val interface{}, typeName string) interface{} {
	if val == nil {
		return nil
	}
	aff := Affinity(typeName)
	switch aff {
	case 'I': // INTEGER
		return applyIntAffinity(val)
	case 'R': // REAL
		return applyRealAffinity(val)
	case 'T': // TEXT
		return applyTextAffinity(val)
	case 'N': // NUMERIC
		return applyNumericAffinity(val)
	default: // BLOB or other — no conversion
		return val
	}
}

// parseInt parses an integer from a string.
func parseInt(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty string")
	}
	var i int64
	var err error
	if strings.Contains(s, ".") || strings.Contains(s, "e") || strings.Contains(s, "E") {
		return 0, fmt.Errorf("not an integer")
	}
	i, err = strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Try float then truncate
		f, err2 := strconv.ParseFloat(s, 64)
		if err2 != nil {
			return 0, err
		}
		return int64(f), nil
	}
	return i, nil
}

// parseFloat parses a float from a string.
func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}
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

// stringCompare compares two strings using the given collation.
// Supported collations: "NOCASE" (case-insensitive), "RTRIM" (right-trim),
// "BINARY" or "" (byte-wise comparison with SQLite BINARY semantics).
func stringCompare(a, b, collation string) int {
	switch strings.ToUpper(collation) {
	case "NOCASE":
		return strings.Compare(strings.ToUpper(a), strings.ToUpper(b))
	case "RTRIM":
		return strings.Compare(strings.TrimRight(a, " "), strings.TrimRight(b, " "))
	default:
		// BINARY or empty: standard byte-wise comparison
		// SQLite BINARY compares using memcmp with the shortest string's length first
		minLen := len(a)
		if len(b) < minLen {
			minLen = len(b)
		}
		if minLen > 0 {
			if a[:minLen] < b[:minLen] {
				return -1
			}
			if a[:minLen] > b[:minLen] {
				return 1
			}
		}
		// All equal up to minLen, shorter string sorts first
		switch {
		case len(a) < len(b):
			return -1
		case len(a) > len(b):
			return 1
		default:
			return 0
		}
	}
}

func applyIntAffinity(val interface{}) interface{} {
	switch v := val.(type) {
	case float64:
		return int64(v)
	case string:
		if i, err := parseInt(v); err == nil {
			return i
		}
		if f, err := parseFloat(v); err == nil {
			return int64(f)
		}
		return val
	default:
		return val
	}
}

func applyRealAffinity(val interface{}) interface{} {
	switch v := val.(type) {
	case int64:
		return float64(v)
	case string:
		if f, err := parseFloat(v); err == nil {
			return f
		}
		return val
	default:
		return val
	}
}

func applyTextAffinity(val interface{}) interface{} {
	switch v := val.(type) {
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	default:
		return val
	}
}

func applyNumericAffinity(val interface{}) interface{} {
	switch v := val.(type) {
	case string:
		if i, err := parseInt(v); err == nil {
			return i
		}
		if f, err := parseFloat(v); err == nil {
			return f
		}
		return val
	default:
		return val
	}
}
