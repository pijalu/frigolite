package value

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

// ColumnValue wraps a value retrieved from a column, carrying the column's
// affinity type. This is used by CompareValuesCollate to correctly apply
// SQLite's type affinity rules for comparisons.
//
// SQLite affinity rules:
//   - TEXT vs BLOB → TEXT is preferred (no numeric conversion)
//   - NUMERIC vs TEXT → TEXT is converted to REAL
//   - TEXT vs NONE (no affinity) → TEXT is preferred (no numeric conversion)
//
// Affinity is stripped by expression operators like unary + and CAST.
type ColumnValue struct {
	Value    interface{}
	Affinity rune // 'B'=BLOB, 'T'=TEXT, 'I'=INTEGER, 'R'=REAL, 'N'=NUMERIC
}

// UnwrapColumnValue extracts the underlying value from a ColumnValue wrapper.
// Returns the value unchanged if it is not a ColumnValue.
func UnwrapColumnValue(v interface{}) interface{} {
	if cv, ok := v.(*ColumnValue); ok {
		return cv.Value
	}
	return v
}

// ColumnAffinity returns the column affinity stored in a ColumnValue wrapper,
// or 0 if the value is not wrapped.
func ColumnAffinity(v interface{}) rune {
	if cv, ok := v.(*ColumnValue); ok {
		return cv.Affinity
	}
	return 0
}

// CompareValuesCollate compares two SQL values with an optional collation.
// collation can be "NOCASE", "RTRIM", "BINARY", or "" (defaults to BINARY).
func CompareValuesCollate(a, b interface{}, collation string) int {
	if r, handled := compareNil(a, b); handled {
		return r
	}

	// Extract column affinity wrappers and track their type.
	aAff := ColumnAffinity(a)
	bAff := ColumnAffinity(b)
	a = UnwrapColumnValue(a)
	b = UnwrapColumnValue(b)

	ta, tb := classifyValue(a), classifyValue(b)

	// Subtype-carrying TEXT (e.g. function results marked as JSON) compares
	// by its underlying text: unwrap any non-scalar carrying a String().
	a, b = unwrapSubtypeText(a), unwrapSubtypeText(b)

	// INTEGER and REAL are mutually comparable (both are numeric)
	if bothNumeric(ta, tb) {
		return compareNumericValues(a, b, ta, tb)
	}

	// Determine if we should skip numeric conversion based on affinity rules.
	skipConv, isBlob := affinitySkipConversion(aAff, bAff)

	if !skipConv {
		if r, handled := compareAffinityConvert(a, b, ta, tb); handled {
			return r
		}
	}

	// When skipConv is true: BLOB or NONE affinity rules
	if skipConv && ta != tb {
		return compareWithAffinityOverride(a, b, ta, tb, isBlob, collation)
	}

	// Different types: compare by type ordering
	if ta != tb {
		return int(ta) - int(tb)
	}

	// Same type: compare by value
	return compareSameType(a, b, ta, collation)
}

// compareNil handles NULL comparisons. Returns (result, true) when either
// operand is NULL.
func compareNil(a, b interface{}) (int, bool) {
	if a == nil && b == nil {
		return 0, true
	}
	if a == nil {
		return -1, true
	}
	if b == nil {
		return 1, true
	}
	return 0, false
}

// bothNumeric reports whether both value classes are numeric.
func bothNumeric(ta, tb valueClass) bool {
	return isNumeric(ta) && isNumeric(tb)
}

// compareAffinityConvert applies SQLite's numeric-vs-text conversion rules
// when numeric conversion is allowed. Returns (result, true) when one operand
// is numeric and the other is text.
func compareAffinityConvert(a, b interface{}, ta, tb valueClass) (int, bool) {
	if isNumeric(ta) && tb == typeText {
		return compareNumericText(a, b, -1), true
	}
	if isNumeric(tb) && ta == typeText {
		return compareTextNumeric(a, b, 1), true
	}
	return 0, false
}

// compareNumericValues compares two numeric values (INTEGER or REAL).
func compareNumericValues(a, b interface{}, ta, tb valueClass) int {
	if ta != tb {
		return compareIntFloat(a, b)
	}
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

// affinitySkipConversion determines whether to skip numeric conversion based on
// SQLite affinity rules: TEXT vs BLOB/NONE → no numeric conversion.
func affinitySkipConversion(aAff, bAff rune) (skipConv, isBlob bool) {
	if aAff == 'T' && (bAff == 'B' || bAff == 0) {
		return true, bAff == 'B'
	}
	if bAff == 'T' && (aAff == 'B' || aAff == 0) {
		return true, aAff == 'B'
	}
	return false, false
}

// compareWithAffinityOverride handles comparison when skipConv is true,
// applying BLOB or NONE affinity rules.
func compareWithAffinityOverride(a, b interface{}, ta, tb valueClass, isBlob bool, collation string) int {
	if isBlob {
		return int(ta) - int(tb)
	}
	if ta == typeText && isNumeric(tb) {
		return stringCompare(toString(a), formatNumeric(b), collation)
	}
	if tb == typeText && isNumeric(ta) {
		return stringCompare(formatNumeric(a), toString(b), collation)
	}
	return int(ta) - int(tb)
}

// compareSameType compares two values of the same type.
func compareSameType(a, b interface{}, ta valueClass, collation string) int {
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

// compareIntFloat compares an int64 value with a float64 value using SQLite's
// int-float comparison algorithm (sqlite3IntFloatCompare). This avoids precision
// loss that occurs when converting both to float64.
func compareIntFloat(a, b interface{}) int {
	var i int64
	var r float64
	swap := false

	switch v := a.(type) {
	case int64:
		i = v
		r = b.(float64)
	case float64:
		i = b.(int64)
		r = v
		swap = true
	}

	result := sqlite3IntFloatCompare(i, r)
	if swap {
		result = -result
	}
	return result
}

// sqlite3IntFloatCompare implements SQLite's comparison between int64 and float64.
// Returns -1 if i < r, 0 if i == r, 1 if i > r.
func sqlite3IntFloatCompare(i int64, r float64) int {
	// If r is outside int64 range, compare as floats (i converted to float64)
	// — the boundary 2^63 itself is NOT representable as int64, so every
	// int64 i < r there (C sqlite3IntFloatCompare uses >= TWOPOWER63).
	if r >= float64(math.MaxInt64) {
		return -1 // r > i (since i <= MaxInt64 < r)
	}
	if r < float64(math.MinInt64) {
		return 1 // r < i (since i >= MinInt64 > r)
	}
	// r is within int64 range: convert r to int64 and compare as integers
	ri := int64(r)
	if ri < i {
		return 1 // i > r
	}
	if ri > i {
		return -1 // i < r
	}
	return 0 // equal
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

// TextCarrier is implemented by string-carrying subtype wrappers (e.g. the
// JSON subtype marker on function results): they compare and classify as
// ordinary TEXT with the payload returned by CarrierText.
type TextCarrier interface {
	CarrierText() string
}

// unwrapSubtypeText replaces subtype-carrying TEXT wrappers with their
// payload string so comparison sees plain TEXT.
func unwrapSubtypeText(v interface{}) interface{} {
	if tc, ok := v.(TextCarrier); ok {
		return tc.CarrierText()
	}
	return v
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
	case TextCarrier:
		return typeText
	case []byte:
		return typeBlob
	case ZeroBlob:
		return typeBlob // zeroblob(N) has typeof 'blob' (lazy zero-filled)
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
	if tc, ok := v.(TextCarrier); ok {
		return tc.CarrierText()
	}
	return ""
}

// toString converts any value to its string representation, including numeric types.
func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if i, ok := v.(int64); ok {
		return strconv.FormatInt(i, 10)
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	if z, ok := v.(ZeroBlob); ok {
		return string(z.Bytes()) // expandBlob: reinterpret zeros as text
	}
	return fmt.Sprintf("%v", v)
}

// formatNumeric formats a numeric value as a string, like SQLite does.
func formatNumeric(v interface{}) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return toString(v)
	}
}

func toBytes(v interface{}) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	if z, ok := v.(ZeroBlob); ok {
		return z.Bytes()
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

// parseInt parses an integer from a string. This is used for affinity
// application during INSERT/UPDATE, where SQLite's sqlite3Atoi64 accepts
// leading zeros (e.g., '03' → 3). Leading-zero rejection only applies
// in the comparison path (see compareNumericText/compareTextNumeric).
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
