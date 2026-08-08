package util

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
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
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}

	// Extract column affinity wrappers and track their type.
	aAff := ColumnAffinity(a)
	bAff := ColumnAffinity(b)
	a = UnwrapColumnValue(a)
	b = UnwrapColumnValue(b)

	ta, tb := classifyValue(a), classifyValue(b)

	// INTEGER and REAL are mutually comparable (both are numeric)
	if isNumeric(ta) && isNumeric(tb) {
		// INTEGER vs REAL: use SQLite's int-float compare to avoid precision loss
		if ta != tb {
			return compareIntFloat(a, b)
		}
		// Same type: both INTEGER or both REAL
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

	// SQLite affinity rules for comparisons:
	//
	// Rule 1: NUMERIC vs non-numeric → convert non-numeric to REAL
	//   - INTEGER/REAL/NUMERIC column vs TEXT column → convert TEXT to REAL
	//   - But only if the TEXT column has column affinity (not a bare literal)
	//
	// Rule 2: TEXT vs BLOB → TEXT is preferred (no numeric conversion)
	//
	// Rule 3: TEXT vs NONE (no column affinity) → TEXT is preferred
	//   (no numeric conversion). This applies when the other operand
	//   comes from an expression like unary + or a bare literal.
	//
	// Rule 4: neither operand carries affinity (bare literals, expression
	//   results like function calls, or BLOB-affinity columns) → no
	//   conversion, compare by type ordering. This is why `1 <= '0'` is TRUE
	//   (INTEGER sorts before TEXT) while `rowid <= '0'` is FALSE (rowid's
	//   INTEGER affinity converts '0' to 0).
	//
	// In modern SQLite (3.41+), expressions like +col have NO affinity,
	// even though their value is numeric. This is the key difference
	// from bare column references which preserve their column affinity.

	// Rule 4: neither side has TEXT or numeric affinity. Compare as-is by
	// SQLite type ordering (NULL < INTEGER/REAL < TEXT < BLOB); the numeric
	// conversion below must not apply.
	if (aAff == 0 || aAff == 'B') && (bAff == 0 || bAff == 'B') {
		if ta != tb {
			return int(ta) - int(tb)
		}
		// Same class: fall through to the value comparison below.
	}

	// Determine if we should skip numeric conversion.
	// Skip when comparing TEXT with BLOB, or TEXT with NONE (no affinity).
	// Per SQLite rule: "If one operand has TEXT affinity and the other has
	// BLOB or no affinity, then no conversion is applied."
	skipConv := false
	isBlob := false
	if aAff == 'T' && (bAff == 'B' || bAff == 0) {
		skipConv = true
		if bAff == 'B' {
			isBlob = true
		}
	}
	if bAff == 'T' && (aAff == 'B' || aAff == 0) {
		skipConv = true
		if aAff == 'B' {
			isBlob = true
		}
	}

	if !skipConv {
		if isNumeric(ta) && tb == typeText {
			return compareNumericText(a, b, -1, aAff)
		}
		if isNumeric(tb) && ta == typeText {
			return compareTextNumeric(a, b, 1, bAff)
		}
	}

	// When skipConv is true:
	// - BLOB affinity: compare by type (INTEGER/REAL < TEXT)
	// - NONE affinity: convert numeric to TEXT and compare as strings
	if skipConv && ta != tb {
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
	}

	// Different types: compare by type ordering
	if ta != tb {
		return int(ta) - int(tb)
	}

	// Same type: compare by value
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
// numAff is the affinity of the numeric operand ('I'/'R'/'N' for a column,
// 0 for a bare literal/expression). A numeric COLUMN converts any
// well-formed number text ('3.0' → 3.0), while a bare numeric literal only
// matches an exact integer string ('123' = 123 is FALSE for '123.0').
func compareNumericText(a, b interface{}, typeOrder int, numAff rune) int {
	s := toStr(b)
	if _, ok := a.(int64); ok && numAff == 0 {
		if isExactIntString(s) {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return typeOrder
			}
			ai := a.(int64)
			switch {
			case ai < n:
				return -1
			case ai > n:
				return 1
			default:
				return 0
			}
		}
		return typeOrder
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
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
// numAff is the affinity of the numeric operand ('I'/'R'/'N' for a column,
// 0 for a bare literal/expression). A numeric COLUMN converts any
// well-formed number text ('3.0' → 3.0), while a bare numeric literal only
// matches an exact integer string ('123.0' = 123 is FALSE).
func compareTextNumeric(a, b interface{}, typeOrder int, numAff rune) int {
	s := toStr(a)
	if _, ok := b.(int64); ok && numAff == 0 {
		// Bare integer operand: only exact integer strings convert.
		if isExactIntString(s) {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return typeOrder
			}
			bi := b.(int64)
			switch {
			case n < bi:
				return -1
			case n > bi:
				return 1
			default:
				return 0
			}
		}
		return typeOrder
	}
	// Real operand (or numeric column): a well-formed real string converts
	// (SQLite converts '123.0' to 123.0, and '123' to 123.0 as well).
	if f, err := strconv.ParseFloat(s, 64); err == nil {
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

// isExactIntString reports whether s is a plain integer literal with no
// decimal point, exponent, sign, or surrounding whitespace (SQLite's
// integer-looking TEXT rule: '123' yes, '123.0' no, ' 123' no).
func isExactIntString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
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
		// Try float then truncate. Reject out-of-int64-range values (Go's
		// float→int64 conversion saturates, but SQLite's affinity leaves
		// them REAL: '-9223372036854775809' stores as real -9.223e18).
		f, err2 := strconv.ParseFloat(s, 64)
		if err2 != nil {
			return 0, err
		}
		if f >= 9.223372036854776e18 || f < -9.223372036854776e18 {
			return 0, fmt.Errorf("out of int64 range")
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

// stringCompare compares two strings using the given collation.
// Supported collations: "NOCASE" (case-insensitive), "RTRIM" (right-trim),
// "BINARY" or "" (byte-wise comparison with SQLite BINARY semantics).
func stringCompare(a, b, collation string) int {
	return stringCompareFn(a, b, collation, nil)
}

// stringCompareFn is stringCompare with custom collation lookup support: when
// the collation is not a built-in, lookup is consulted for a CollationFunc;
// an unknown collation (or nil lookup) falls back to BINARY.
func stringCompareFn(a, b, collation string, lookup func(string) (CollationFunc, bool)) int {
	switch strings.ToUpper(collation) {
	case "NOCASE":
		return strings.Compare(strings.ToUpper(a), strings.ToUpper(b))
	case "RTRIM":
		return strings.Compare(strings.TrimRight(a, " "), strings.TrimRight(b, " "))
	default:
		if lookup != nil {
			if fn, ok := lookup(collation); ok {
				return fn(a, b)
			}
		}
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
		// Convert a REAL to INTEGER only when it fits exactly in int64 AND is
		// integral (SQLite: 1234.0 → 1234 but 1234.56 stays REAL; the
		// conversion is lossless only for whole numbers). Go's float64→int64
		// conversion saturates (2^63 → MaxInt64), so a range check must guard
		// it: values >= 2^63 stay REAL, matching SQLite's sqlite3VdbeIntValue
		// (e.g. INTEGER DEFAULT -(-9223372036854775808) evaluates to real
		// 9.22337203685478e+18, not a wrapped integer).
		if v == math.Trunc(v) && v >= -9.223372036854776e18 && v < 9.223372036854776e18 {
			return int64(v)
		}
		return v
	case string:
		t := strings.TrimSpace(v)
		if i, err := parseInt(t); err == nil {
			return i
		}
		if f, err := parseFloat(t); err == nil {
			if f == math.Trunc(f) && f >= -9.223372036854776e18 && f < 9.223372036854776e18 {
				return int64(f)
			}
			// Out of int64 range or non-integral: SQLite stores the REAL
			// (e.g. '-9223372036854775809' → real -9.22337203685478e+18,
			// '1234.56' → real 1234.56).
			return f
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
		t := strings.TrimSpace(v)
		if f, err := parseFloat(t); err == nil {
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
		// NUMERIC affinity, the value is converted into an integer."
		if v == float64(int64(v)) {
			return int64(v)
		}
		return val
	case string:
		// SQLite strips leading/trailing whitespace before numeric coercion.
		t := strings.TrimSpace(v)
		if i, err := parseInt(t); err == nil {
			return i
		}
		if f, err := parseFloat(t); err == nil {
			// If the float is a whole number, store as integer (NUMERIC affinity)
			if f == float64(int64(f)) {
				return int64(f)
			}
			return f
		}
		return val
	default:
		return val
	}
}
