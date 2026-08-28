package execexpr

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
)

func ToBool(v interface{}) bool {
	if v == nil {
		return false
	}
	// Unwrap ColumnValue so HAVING, WHERE, and boolean filters
	// correctly evaluate scalar values from the database.
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	// A column whose stored value is NULL wraps as ColumnValue{Value: nil};
	// after unwrapping, NULL must evaluate to false (SQLite treats NULL as
	// not-true), not fall through to the default true case.
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		// SQLite's sqlite3VdbeBooleanValue converts TEXT/BLOB to REAL and
		// checks != 0.0: 'a' and '0.0' are FALSE, '5' and '5x' are TRUE.
		return sqliteRealValue(x) != 0.0
	case []byte:
		return sqliteRealValue(string(x)) != 0.0
	default:
		return true
	}
}
func IsZeroString(v interface{}) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(s)
	return trimmed == "" || trimmed == "." || trimmed == "+." || trimmed == "-."
}

// numericIsInt reports whether a value should be treated as an integer in
// arithmetic. int64 values are integers; text that parses to a whole number
// (SQLite's text→integer rule: '12' is 12, '1x' is 1, '12.5' is not) is also
// integer-valued so that '12' + 3 yields the integer 15, matching SQLite.
// A string with no numeric prefix at all ("abc") is integer 0.
func NumericIsInt(v interface{}) bool {
	if _, ok := v.(int64); ok {
		return true
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	// A decimal point or exponent in the numeric prefix makes the value REAL
	// even when the parsed number is whole ('10.y' -> 10.0, '1e1y' -> 10.0;
	// SQLite arithmetic treats them as REAL, so 1234/'10.y' is 123.4, not
	// 123).
	t := strings.TrimSpace(s)
	if numericPrefixIsReal(t) {
		return false
	}
	f, ok := parseNumericPrefix(s)
	if !ok {
		return true
	}
	return f == float64(int64(f))
}

// numericPrefixIsReal reports whether a numeric text value's prefix contains
// a decimal point or exponent (making it REAL, not INTEGER).
func numericPrefixIsReal(t string) bool {
	i := 0
	if i < len(t) && (t[i] == '+' || t[i] == '-') {
		i++
	}
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	return i < len(t) && (t[i] == '.' || t[i] == 'e' || t[i] == 'E')
}

// toIntNumeric parses a string's numeric prefix as an int64 when it is a
// pure integer (optionally signed) that fits int64; returns ok=false for
// non-strings, fractional prefixes, or values outside int64 range.
func ToIntNumeric(v interface{}) (int64, bool) {
	// SQLite's sqlite3VdbeIntValue applies sqlite3Atoi64 to a BLOB's bytes
	// just like TEXT (fts3corrupt6 1.1: 0+matchinfo(...) is INTEGER 0).
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	t := strings.TrimSpace(s)
	if t == "" || t == "." || t == "+." || t == "-" {
		return 0, false
	}
	digitsEnd := scanDigitsWithSign(t)
	if digitsEnd == 0 {
		return 0, false
	}
	// A '.' or exponent after the digits makes it a REAL, not an int.
	if digitsEnd < len(t) && (t[digitsEnd] == '.' || t[digitsEnd] == 'e' || t[digitsEnd] == 'E') {
		return 0, false
	}
	n, err := strconv.ParseInt(t[:digitsEnd], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// scanDigitsWithSign returns the index just past an optional sign and digit
// run at the start of t, or 0 when no digits are present.
func scanDigitsWithSign(t string) int {
	i := 0
	if t[0] == '+' || t[0] == '-' {
		i++
	}
	start := i
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == start {
		return 0
	}
	return i
}

// nanToNil converts a NaN arithmetic result to SQL NULL, matching SQLite's
// rule that any arithmetic producing NaN yields NULL (Inf-Inf, Inf*0,
// Inf/Inf). Inf results pass through unchanged.
func NanToNil(v interface{}) interface{} {
	if f, ok := v.(float64); ok && math.IsNaN(f) {
		return nil
	}
	return v
}

func ToIntValue(v interface{}) int64 {
	if IsZeroString(v) {
		return 0
	}
	if i, ok := v.(int64); ok {
		return i
	}
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

// toInt64 converts a value to int64 with an ok flag, matching SQLite's
// integer conversion for bitwise operators: int64 stays, float64 truncates
// toward zero, numeric strings parse, everything else coerces to 0 (SQLite
// applies NUMERIC affinity to text/blob operands, so 'a' → 0, x'00' → 0).
func ToInt64(v interface{}) (int64, bool) {
	switch x := util.UnwrapColumnValue(v).(type) {
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case string:
		s := strings.TrimSpace(x)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
		return 0, true
	case []byte:
		s := strings.TrimSpace(string(x))
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
		return 0, true
	case nil:
		return 0, false // NULL propagates NULL
	default:
		return 0, true
	}
}

func mulValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if NumericIsInt(a) && NumericIsInt(b) {
			return int64(af) * int64(bf), nil
		}
		return NanToNil(af * bf), nil
	}
	return nil, fmt.Errorf("cannot multiply non-numeric values")
}

func divValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if bf == 0 {
			return nil, nil
		}
		if NumericIsInt(a) && NumericIsInt(b) {
			return int64(af) / int64(bf), nil
		}
		return NanToNil(af / bf), nil
	}
	return nil, fmt.Errorf("cannot divide non-numeric values")
}

func modValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if bf == 0 {
			return nil, nil
		}
		// SQLite's % truncates both operands to integers then applies integer
		// modulo (5.5 % 2 is 1, not 1.5). The result type is REAL when either
		// operand was REAL, INTEGER otherwise (5 % 2 → 1, 5.0 % 2 → 1.0).
		r := int64(af) % int64(bf)
		if NumericIsInt(a) && NumericIsInt(b) {
			return r, nil
		}
		return float64(r), nil
	}
	return nil, fmt.Errorf("cannot mod non-numeric values")
}

func bitwiseAnd(a, b interface{}) (interface{}, error) {
	ai, aok := a.(int64)
	bi, bok := b.(int64)
	if aok && bok {
		return ai & bi, nil
	}
	return nil, fmt.Errorf("cannot bitwise-AND non-integer values")
}

func bitwiseOr(a, b interface{}) (interface{}, error) {
	ai, aok := ToInt64(a)
	bi, bok := ToInt64(b)
	if aok && bok {
		return ai | bi, nil
	}
	return nil, fmt.Errorf("cannot bitwise-OR non-integer values")
}

func shiftLeft(a, b interface{}) (interface{}, error) {
	ai, aok := ToInt64(a)
	bi, bok := ToInt64(b)
	if aok && bok {
		if bi < 0 {
			return shiftRight(a, int64(-bi))
		}
		if bi >= 64 {
			return int64(0), nil
		}
		return ai << uint(bi), nil
	}
	return nil, fmt.Errorf("cannot shift non-integer values")
}

func shiftRight(a, b interface{}) (interface{}, error) {
	ai, aok := ToInt64(a)
	bi, bok := ToInt64(b)
	if aok && bok {
		if bi < 0 {
			return shiftLeft(a, int64(-bi))
		}
		if bi >= 64 {
			if ai < 0 {
				return int64(-1), nil
			}
			return int64(0), nil
		}
		return ai >> uint(bi), nil
	}
	return nil, fmt.Errorf("cannot shift non-integer values")
}

func ConcatValues(a, b interface{}) (interface{}, error) {
	if a == nil || b == nil {
		return nil, nil
	}
	// Unwrap column-affinity wrappers so blobs concatenate as raw bytes.
	a = util.UnwrapColumnValue(a)
	b = util.UnwrapColumnValue(b)
	ab, aIsBlob := a.([]byte)
	bb, bIsBlob := b.([]byte)
	az, aIsZero := a.(value.ZeroBlob)
	bz, bIsZero := b.(value.ZeroBlob)
	if aIsBlob || bIsBlob || aIsZero || bIsZero {
		// Concatenate raw bytes; SQLite yields a TEXT value (not a blob).
		var buf []byte
		switch {
		case aIsBlob:
			buf = append(buf, ab...)
		case aIsZero:
			buf = append(buf, make([]byte, az.N)...)
		default:
			buf = append(buf, renderConcatValue(a)...)
		}
		switch {
		case bIsBlob:
			buf = append(buf, bb...)
		case bIsZero:
			buf = append(buf, make([]byte, bz.N)...)
		default:
			buf = append(buf, renderConcatValue(b)...)
		}
		return string(buf), nil
	}
	return renderConcatValue(a) + renderConcatValue(b), nil
}

// renderConcatValue converts a value to its TEXT form for the || operator,
// matching SQLite's sqlite3_value_text rendering (REALs use the 15-digit
// %!.15g format with a trailing .0 for whole numbers, e.g. 11.0).
func renderConcatValue(v interface{}) string {
	switch x := v.(type) {
	case float64:
		return util.FormatSQLiteReal(x)
	case []byte:
		return string(x)
	case value.ZeroBlob:
		return string(x.Bytes())
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

func NegateValue(v interface{}) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	// Try numeric negation first
	switch val := v.(type) {
	case int64:
		return negateInt64(val), nil
	case float64:
		// SQLite keeps -0.0 as a REAL -0.0 (typeof(-0.0)='real'); do not
		// coerce it to an integer here.
		return -val, nil
	}
	// Try string as number
	if IsZeroString(v) {
		// SQLite: -'.' == 0 (integer), -'' == 0.
		return int64(0), nil
	}
	// A string with an integer numeric prefix negates as int64
	// (-'hello' -> 0, -'0' -> 0; SQLite keeps the integer type).
	if n, ok := ToIntNumeric(v); ok {
		return negateInt64(n), nil
	}
	f, ok := toFloat(v)
	if ok {
		// A non-integer-prefixed string whose numeric value is a whole
		// number negates as int64 ('hello' -> 0, '0x' -> 0, '1x' -> -1),
		// matching SQLite's text->integer rule; only fractional/exponent
		// values negate as REAL ('1.5x' -> -1.5).
		if neg, isInt := negateFloatAsInt(v, f); isInt {
			return neg, nil
		}
		return -f, nil
	}
	// Non-numeric values: return 0 (SQLite behavior, e.g. -'abc' = 0, -x'ce' = 0)
	return int64(0), nil
}

// negateInt64 negates an int64, promoting math.MinInt64 to REAL (2^63) to
// match SQLite's overflow behavior.
func negateInt64(val int64) interface{} {
	if val == math.MinInt64 {
		return -float64(val)
	}
	return -val
}

// negateFloatAsInt negates a float as int64 when the value came from a string
// (or a BLOB whose bytes are text, e.g. -x'3132' → -12) whose numeric value
// is a whole number (SQLite's text->integer rule).
func negateFloatAsInt(v interface{}, f float64) (interface{}, bool) {
	_, isStr := v.(string)
	_, isBlob := v.([]byte)
	if (isStr || isBlob) && f == float64(int64(f)) && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return negateInt64(int64(f)), true
	}
	return nil, false
}

// sqliteRealValue converts a text value to REAL like SQLite's
// sqlite3VdbeRealValue: the numeric prefix is parsed (an empty or
// non-numeric string yields 0.0).
func sqliteRealValue(s string) float64 {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0.0
	}
	// Parse the leading numeric prefix; stop at the first non-numeric char.
	prefix := scanRealPrefix(trimmed)
	if isDegenerateNumericPrefix(prefix) {
		return 0.0
	}
	f, err := strconv.ParseFloat(prefix, 64)
	if err != nil {
		return 0.0
	}
	return f
}

// scanRealPrefix scans the leading numeric prefix of a string the way SQLite's
// sqlite3VdbeRealValue does: optional sign, digits, a single '.', and a single
// e/E exponent with an optional sign, stopping at the first other character.
func scanRealPrefix(trimmed string) string {
	i := 0
	if i < len(trimmed) && (trimmed[i] == '+' || trimmed[i] == '-') {
		i++
	}
	dot := false
	exp := false
	for i < len(trimmed) {
		var stop bool
		i, dot, exp, stop = realPrefixStep(trimmed, i, dot, exp)
		if stop {
			break
		}
	}
	return trimmed[:i]
}

// realPrefixStep classifies one character of a numeric prefix scan and returns
// the next position, the updated dot/exp flags, and whether scanning stops
// (the character is not part of the numeric prefix).
func realPrefixStep(trimmed string, i int, dot, exp bool) (int, bool, bool, bool) {
	c := trimmed[i]
	if c >= '0' && c <= '9' {
		return i + 1, dot, exp, false
	}
	if c == '.' && !dot && !exp {
		return i + 1, true, exp, false
	}
	if (c == 'e' || c == 'E') && !exp {
		return skipRealExponentSign(trimmed, i) + 1, true, true, false
	}
	return i, dot, exp, true
}

// skipRealExponentSign advances past the optional sign after an e/E exponent.
func skipRealExponentSign(trimmed string, i int) int {
	if i+1 < len(trimmed) && (trimmed[i+1] == '+' || trimmed[i+1] == '-') {
		return i + 1
	}
	return i
}

// isDegenerateNumericPrefix reports whether a scanned numeric prefix is a
// lone sign or dot (no digits), which parses as numeric 0.
func isDegenerateNumericPrefix(prefix string) bool {
	switch prefix {
	case "", "+", "-", ".", "+.", "-.":
		return true
	}
	return false
}
