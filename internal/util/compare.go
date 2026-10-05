package util

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/value"
)

// SQLiteValueString renders a value the way SQLite's sqlite3_value_text does
// for LIKE/GLOB/CAST-to-TEXT: INTEGER as decimal, REAL with %.15g formatting
// (keeping ".0" on whole values and adding ".0" to a bare mantissa in
// exponential form), TEXT as itself, BLOB as its bytes.
func SQLiteValueString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return FormatSQLiteReal(x)
	case []byte:
		return string(x)
	case value.ZeroBlob:
		return string(x.Bytes())
	default:
		return fmt.Sprintf("%v", v)
	}
}

// FormatSQLiteReal renders a float64 the way SQLite's %!.15g does: 15
// significant digits, fixed-point for exponents in range, always a decimal
// point (alternate form), exponential otherwise. Non-finite values render
// as SQLite's sqlite3StrAccum printf does: "Inf", "-Inf", "NaN" (Go's
// strconv renders "+Inf"/"-Inf"/"NaN", and SQLite never emits a leading
// plus for positive infinity).
func FormatSQLiteReal(f float64) string {
	if math.IsInf(f, 1) {
		return "Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	s := strconv.FormatFloat(f, 'g', 15, 64)
	if e := strings.IndexAny(s, "eE"); e >= 0 {
		mant := s[:e]
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		return mant + s[e:]
	}
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// CollationFunc is a custom collation sequence: it compares two strings and
// returns -1 if a < b, 0 if equal, 1 if a > b (sqlite3_create_collation).
type CollationFunc func(a, b string) int

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
	return CompareValuesCollateFn(a, b, collation, nil)
}

// CompareValuesCollateFn is CompareValuesCollate with support for custom
// collation sequences: when the resolved collation is not a built-in
// (BINARY/NOCASE/RTRIM/""), lookup is consulted for a CollationFunc to apply
// to TEXT comparisons. A nil lookup or an unknown collation falls back to
// BINARY, matching SQLite's default.
func CompareValuesCollateFn(a, b interface{}, collation string, lookup func(string) (CollationFunc, bool)) int {
	if r, handled := compareNil(a, b); handled {
		return r
	}

	// Extract column affinity wrappers and track their type.
	aAff, bAff, a, b := unwrapAffinity(a, b)
	a, b = unwrapSubtypeText(a), unwrapSubtypeText(b)

	ta, tb := classifyValue(a), classifyValue(b)

	// INTEGER and REAL are mutually comparable (both are numeric)
	if bothNumeric(ta, tb) {
		return compareNumericSameClass(a, b, ta, tb)
	}

	// Rule 4: neither side has TEXT or numeric affinity. Compare as-is by
	// SQLite type ordering; the numeric conversion below must not apply.
	if noAffinityPair(aAff, bAff) {
		if ta != tb {
			return int(ta) - int(tb)
		}
	}

	skipConv, isBlob := affinitySkipConversion(aAff, bAff)

	if !skipConv {
		if r, handled := compareAffinityConvert(a, b, ta, tb, aAff, bAff); handled {
			return r
		}
	}

	// When skipConv is true: BLOB affinity compares by type, NONE affinity
	// converts numeric to TEXT and compares as strings.
	if skipConv && ta != tb {
		return compareSkippedAffinity(a, b, ta, tb, isBlob, collation, lookup)
	}

	// Different types: compare by type ordering
	if ta != tb {
		return int(ta) - int(tb)
	}

	// Same type: compare by value
	return compareSameClass(a, b, ta, collation, lookup)
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

// unwrapAffinity extracts column-affinity wrappers from both operands and
// returns the unwrapped values alongside their affinities.
func unwrapAffinity(a, b interface{}) (aAff, bAff rune, av, bv interface{}) {
	return ColumnAffinity(a), ColumnAffinity(b), UnwrapColumnValue(a), UnwrapColumnValue(b)
}

// bothNumeric reports whether both value classes are numeric.
func bothNumeric(ta, tb valueClass) bool {
	return isNumeric(ta) && isNumeric(tb)
}

// noAffinityPair reports whether neither operand carries TEXT or numeric
// affinity (both are BLOB affinity or none).
func noAffinityPair(aAff, bAff rune) bool {
	return (aAff == 0 || aAff == 'B') && (bAff == 0 || bAff == 'B')
}

// affinitySkipConversion computes whether the comparison should skip numeric
// conversion (TEXT vs BLOB, or TEXT vs NONE) and whether a BLOB affinity is
// involved.
func affinitySkipConversion(aAff, bAff rune) (skipConv, isBlob bool) {
	if aAff == 'T' && (bAff == 'B' || bAff == 0) {
		if bAff == 'B' {
			return true, true
		}
		return true, false
	}
	if bAff == 'T' && (aAff == 'B' || aAff == 0) {
		if aAff == 'B' {
			return true, true
		}
		return true, false
	}
	return false, false
}

// compareAffinityConvert applies SQLite's numeric-vs-text conversion rules
// when numeric conversion is allowed. Returns (result, true) when one operand
// is numeric and the other is text.
func compareAffinityConvert(a, b interface{}, ta, tb valueClass, aAff, bAff rune) (int, bool) {
	if isNumeric(ta) && tb == typeText {
		return compareNumericText(a, b, -1, aAff), true
	}
	if isNumeric(tb) && ta == typeText {
		return compareTextNumeric(a, b, 1, bAff), true
	}
	return 0, false
}

// compareSkippedAffinity compares values when numeric conversion is skipped
// due to TEXT vs BLOB/NONE affinity.
func compareSkippedAffinity(a, b interface{}, ta, tb valueClass, isBlob bool, collation string, lookup func(string) (CollationFunc, bool)) int {
	if isBlob {
		// BLOB: type precedence (INTEGER/REAL < TEXT)
		return int(ta) - int(tb)
	}
	// NONE: compare as TEXT by converting numeric to string
	if ta == typeText && isNumeric(tb) {
		return stringCompareFn(toString(a), formatNumeric(b), collation, lookup)
	}
	if tb == typeText && isNumeric(ta) {
		return stringCompareFn(formatNumeric(a), toString(b), collation, lookup)
	}
	// TEXT vs BLOB (or other differing types) with NONE affinity: fall back
	// to SQLite type ordering (NULL < INTEGER/REAL < TEXT < BLOB).
	return int(ta) - int(tb)
}

// compareNumericSameClass compares two numeric values: INTEGER vs REAL via
// SQLite's int-float algorithm, and same-class numerics directly (int64 for
// INTEGERs to avoid float precision loss above 2^53).
func compareNumericSameClass(a, b interface{}, ta, tb valueClass) int {
	// INTEGER vs REAL: use SQLite's int-float compare to avoid precision loss
	if ta != tb {
		return compareIntFloat(a, b)
	}
	// Same type: both INTEGER or both REAL. Compare INTEGERs directly as
	// int64 (converting to float64 loses precision above 2^53, so
	// 288230376151711744 == 288230376151711745 must not compare equal).
	if ta == typeInteger {
		ia, iok := a.(int64)
		ib, iok2 := b.(int64)
		if iok && iok2 {
			return compareInt64(ia, ib)
		}
	}
	fa, fb := toFloat64(a), toFloat64(b)
	return compareFloat64(fa, fb)
}

// compareInt64 compares two int64 values.
func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// compareFloat64 compares two float64 values.
func compareFloat64(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// compareSameClass compares two values of the same (non-numeric) class by
// value.
func compareSameClass(a, b interface{}, ta valueClass, collation string, lookup func(string) (CollationFunc, bool)) int {
	switch ta {
	case typeText:
		return stringCompareFn(toStr(a), toStr(b), collation, lookup)
	case typeBlob:
		return bytes.Compare(toBytes(a), toBytes(b))
	default:
		return 0
	}
}

// compareNumericText compares a numeric value a with a text value b.
// SQLite applies the comparison affinity to the text operand (OP_Lt/OP_Eq
// applyNumericAffinity): value.NumericText converts fully-numeric text
// (whitespace-trimmed, whole string consumed) and leaves anything else TEXT,
// which sorts above every number (typeOrder). The numAff rune is retained
// for call-site documentation: the conversion is identical whether the
// numeric side is a column or a bare operand, because a bare-vs-bare pair
// never reaches the affinity conversion (noAffinityPair short-circuits).
func compareNumericText(a, b interface{}, typeOrder int, numAff rune) int {
	kind, iv, fv := value.NumericText(toStr(b))
	switch kind {
	case value.IntNumeric:
		return compareValueToInt(a, iv)
	case value.RealNumeric:
		return compareValueToReal(a, fv)
	default:
		return typeOrder
	}
}

// compareValueToInt orders the numeric operand a against converted integer
// iv, returning the a-vs-iv ordering. INTEGER vs INTEGER compares as int64
// (no float rounding above 2^53); INTEGER vs REAL goes through SQLite's
// int-float algorithm.
func compareValueToInt(a interface{}, iv int64) int {
	switch v := a.(type) {
	case int64:
		switch {
		case v < iv:
			return -1
		case v > iv:
			return 1
		default:
			return 0
		}
	case float64:
		return -sqlite3IntFloatCompare(iv, v)
	default:
		return 0
	}
}

// compareValueToReal orders the numeric operand a against converted real
// fv, returning the a-vs-fv ordering.
func compareValueToReal(a interface{}, fv float64) int {
	switch v := a.(type) {
	case int64:
		return sqlite3IntFloatCompare(v, fv)
	case float64:
		switch {
		case v < fv:
			return -1
		case v > fv:
			return 1
		default:
			return 0
		}
	default:
		return 0
	}
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
	// SQLite's sqlite3IntFloatCompare (util.c). The int64 range is
	// [-2^63, 2^63-1]. A REAL exactly equal to -2^63 equals MinInt64 (both
	// represent the same value). A REAL equal to 2^63 is ONE MORE than
	// MaxInt64 (2^63-1), so every int64 compares strictly less than it.
	// Doubles beyond those (the next representable double is 2^63±2048)
	// compare strictly less/greater than every int64.
	if math.IsNaN(r) {
		return 1
	}
	if r < -9223372036854775808.0 {
		return 1 // r < i (i >= MinInt64 > r)
	}
	if r == -9223372036854775808.0 {
		if i == math.MinInt64 {
			return 0
		}
		return 1 // r < i (i > MinInt64)
	}
	if r > 9223372036854775808.0 {
		return -1 // r > i (i <= MaxInt64 < r)
	}
	if r == 9223372036854775808.0 {
		// r is 2^63 and i <= 2^63-1, so i < r always (SQLite returns -1
		// here; 9223372036854775807 >= 9223372036854775807+1 is false).
		return -1
	}
	// r is within int64 range: convert r to int64 and compare as integers,
	// falling back to a float compare when the truncated values tie.
	y := int64(r)
	if i < y {
		return -1
	}
	if i > y {
		return 1
	}
	if float64(i) < r {
		return -1
	}
	if float64(i) > r {
		return 1
	}
	return 0
}

// compareTextNumeric compares a text value a with a numeric value b.
// SQLite applies the comparison affinity to the text operand: the same
// value.NumericText conversion as compareNumericText, mirrored.
func compareTextNumeric(a, b interface{}, typeOrder int, numAff rune) int {
	kind, iv, fv := value.NumericText(toStr(a))
	switch kind {
	case value.IntNumeric:
		switch v := b.(type) {
		case int64:
			switch {
			case iv < v:
				return -1
			case iv > v:
				return 1
			default:
				return 0
			}
		case float64:
			return sqlite3IntFloatCompare(iv, v)
		default:
			return 0
		}
	case value.RealNumeric:
		switch v := b.(type) {
		case int64:
			return -sqlite3IntFloatCompare(v, fv)
		case float64:
			switch {
			case fv < v:
				return -1
			case fv > v:
				return 1
			default:
				return 0
			}
		default:
			return 0
		}
	default:
		return typeOrder
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
	case TextCarrier:
		return typeText
	case []byte:
		return typeBlob
	case value.ZeroBlob:
		return typeBlob
	default:
		return typeText
	}
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
	return fmt.Sprintf("%v", v)
}

// formatNumeric formats a numeric value as a string, like SQLite does.
func formatNumeric(v interface{}) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		// SQLite converts a REAL to text with its full representation,
		// keeping ".0" for whole values (2.0 -> "2.0", not "2"). %g drops
		// the trailing ".0", which would make 2.0 == '2' in TEXT affinity
		// comparisons. Whole floats beyond int64 range keep %g precision.
		if x == float64(int64(x)) && x >= -9.223372036854776e18 && x <= 9.223372036854776e18 {
			return strconv.FormatFloat(x, 'f', 1, 64)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return toString(v)
	}
}

func toBytes(v interface{}) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	if z, ok := v.(value.ZeroBlob); ok {
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
	return ApplyColumnAffinityClass(val, Affinity(typeName))
}

// ApplyColumnAffinityClass coerces a Go value by a PRE-COMPUTED affinity
// class (the Affinity(typeName) result, memoized by callers that wrap the
// same columns per row). A nil value passes through; class 0 (no affinity)
// and BLOB store the value unchanged.
func ApplyColumnAffinityClass(val interface{}, aff rune) interface{} {
	if val == nil {
		return nil
	}
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

// SQLite affinities: TEXT, NUMERIC, INTEGER, REAL, BLOB.
// AffinityNone is the type name used to mark a value/column with NO affinity
// (SQLite SQLITE_AFF_NONE = 0). It is distinct from BLOB affinity: a view
// column whose defining expression has no affinity (e.g. AVG(...), or any
// function call) carries NONE, and SQLite's comparison-affinity rules treat
// "no affinity" differently from BLOB (sqlite3CompareAffinity: an operand
// with NONE defers to the other operand's affinity; BLOB does not).
const AffinityNone = "!NONE!"

// Affinity returns the SQLite affinity class for a declared type name.
// An empty type name is BLOB affinity (a real table column with no declared
// type stores values as-is). The AffinityNone sentinel maps to 0 (NONE).
func Affinity(typeName string) rune {
	upper := strings.ToUpper(strings.TrimSpace(typeName))
	if upper == AffinityNone {
		return 0
	}
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

// stringCompareFn is stringCompare with custom collation lookup support: when
// the collation is not a built-in, lookup is consulted for a CollationFunc;
// an unknown collation (or nil lookup) falls back to BINARY.
func stringCompareFn(a, b, collation string, lookup func(string) (CollationFunc, bool)) int {
	switch strings.ToUpper(collation) {
	case "NOCASE":
		// SQLite's NOCASE collation (nocaseCollatingFunc) folds via
		// sqlite3UpperToLower: ASCII 'A'-'Z' fold DOWN to 'a'-'z', every
		// other byte is unchanged. The fold direction matters for the
		// bytes between 'Z' and 'a' (0x5B-0x60), which sort before 'Z'.
		return strings.Compare(value.SQLiteAsciiToLower(a), value.SQLiteAsciiToLower(b))
	case "RTRIM":
		return strings.Compare(strings.TrimRight(a, " "), strings.TrimRight(b, " "))
	default:
		if lookup != nil {
			if fn, ok := lookup(collation); ok {
				return fn(a, b)
			}
		}
		return binaryCompare(a, b)
	}
}

// binaryCompare compares strings byte-wise like SQLite's BINARY collation:
// memcmp over the common prefix, then shorter string sorts first.
func binaryCompare(a, b string) int {
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

func applyIntAffinity(val interface{}) interface{} {
	switch v := val.(type) {
	case float64:
		// Convert a REAL to INTEGER only when it fits exactly in int64 AND is
		// integral (SQLite: 1234.0 → 1234 but 1234.56 stays REAL; the
		// conversion is lossless only for whole numbers). Go's float64→int64
		// conversion saturates (2^63 → MaxInt64), so a range check must guard
		// it: values >= 2^63 stay REAL, matching SQLite's sqlite3VdbeIntValue
		// (e.g. INTEGER DEFAULT -(-9223372036854775808) evaluates to real
		// 9.22337203685478e+18, not a wrapped integer). The lower bound is
		// STRICT: vdbeaux.c sqlite3VdbeIntegerAffinity refuses ix ==
		// SMALLEST_INT64 (doubleToInt64's comment) — the double -2^63 is
		// indistinguishable from a rounded-up overflow, so it stays REAL
		// (func4-3.18: tointeger('-9223372036854775809') must read NULL).
		if v == math.Trunc(v) && v > -9.223372036854776e18 && v < 9.223372036854776e18 {
			return int64(v)
		}
		return v
	case string:
		// Storage-affinity text conversion mirrors sqlite3VdbeMemNumerify /
		// applyNumericAffinity through the shared sqlite3AtoF scanner
		// (value.NumericText): whitespace-trimmed, whole string consumed,
		// one digit minimum. NaN/Inf/hex-float spellings that Go's ParseFloat
		// accepts but sqlite3AtoF rejects stay TEXT.
		kind, iv, fv := value.NumericText(v)
		switch kind {
		case value.IntNumeric:
			return iv
		case value.RealNumeric:
			// INTEGER affinity keeps an out-of-range or non-integral real
			// (sqlite3VdbeIntegerAffinity refuses ix == SMALLEST_INT64 and
			// non-integral doubles; '9223372036854775808' stores REAL).
			if fv == math.Trunc(fv) && fv > -9.223372036854776e18 && fv < 9.223372036854776e18 {
				return int64(fv)
			}
			return fv
		default:
			return val
		}
	default:
		return val
	}
}

func applyRealAffinity(val interface{}) interface{} {
	switch v := val.(type) {
	case int64:
		return float64(v)
	case string:
		// Same sqlite3AtoF scanner as applyIntAffinity: fully-numeric text
		// becomes REAL, NaN/Inf/garbage spellings stay TEXT.
		kind, iv, fv := value.NumericText(v)
		switch kind {
		case value.IntNumeric:
			return float64(iv)
		case value.RealNumeric:
			return fv
		default:
			return val
		}
	default:
		return val
	}
}

func applyTextAffinity(val interface{}) interface{} {
	switch v := val.(type) {
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		// SQLite converts a REAL to text using its full precision string and
		// keeps the ".0" for whole values (e.g. -123.0 -> "-123.0"). %g would
		// drop the trailing ".0", so format whole numbers with one decimal —
		// but only when the value fits in int64 (larger floats like -9.2e18
		// must keep %g precision; %.1f would round them).
		if v == float64(int64(v)) && v >= -9.223372036854776e18 && v <= 9.223372036854776e18 {
			return fmt.Sprintf("%.1f", v)
		}
		return fmt.Sprintf("%g", v)
	default:
		return val
	}
}

func applyNumericAffinity(val interface{}) interface{} {
	switch v := val.(type) {
	case float64:
		// SQLite NUMERIC affinity: "If a floating point value that can be
		// represented exactly as an integer is inserted into a column with
		// NUMERIC affinity, the value is converted into an integer." The
		// range guard keeps the ±2^63 saturation out (a REAL exactly at 2^63
		// is not an int64 value and stays REAL).
		if v == math.Trunc(v) && v > -9.223372036854776e18 && v < 9.223372036854776e18 {
			return int64(v)
		}
		return val
	case string:
		// Same sqlite3AtoF scanner as applyIntAffinity; NUMERIC affinity
		// additionally re-classifies integral reals as INTEGER.
		kind, iv, fv := value.NumericText(v)
		switch kind {
		case value.IntNumeric:
			return iv
		case value.RealNumeric:
			if fv == math.Trunc(fv) && fv > -9.223372036854776e18 && fv < 9.223372036854776e18 {
				return int64(fv)
			}
			return fv
		default:
			return val
		}
	default:
		return val
	}
}
