package exec

import (
	"strconv"
	"strings"
)

// normalizeSQL replaces all numeric and string literals in a SQL string with '?'.
// Returns the normalized string and the extracted literal values.
// This is a fast pre-parse scan — it does NOT use the full parser.
// Only handles simple quoted strings and decimal integers/floats.
func normalizeSQL(sql string) (norm string, values []interface{}) {
	var buf strings.Builder
	buf.Grow(len(sql))
	values = make([]interface{}, 0, 16)
	i := 0
	for i < len(sql) {
		ch := sql[i]
		switch {
		case ch == '\'':
			// String literal: '...'
			buf.WriteByte('?')
			var s string
			var ok bool
			i, s, ok = scanStringLiteral(sql, i)
			if ok {
				values = append(values, s)
			}
		case ch >= '0' && ch <= '9':
			// Numeric literal (decimal integer or float starting with digit)
			buf.WriteByte('?')
			var val interface{}
			i, val = scanNumericLiteral(sql, i)
			values = append(values, val)
		case ch == '.':
			// Numeric literal starting with dot (e.g., .5)
			buf.WriteByte('?')
			var v float64
			i, v = scanDotNumeric(sql, i)
			values = append(values, v)
		default:
			buf.WriteByte(ch)
			i++
		}
	}
	norm = buf.String()
	return
}

// scanStringLiteral scans a single-quoted string literal beginning at index i
// (the opening quote). It returns the index just past the closing quote, the
// unescaped string value, and whether a closing quote was found.
func scanStringLiteral(sql string, i int) (next int, val string, ok bool) {
	start := i + 1
	i++
	for i < len(sql) {
		if sql[i] == '\'' {
			if i+1 < len(sql) && sql[i+1] == '\'' {
				// Escaped quote '' — include both, Replaces will handle
				i += 2
				continue
			}
			s := sql[start:i]
			// Only unescape if needed (avoids allocation for common case)
			if containsDoubleQuote(s) {
				s = strings.ReplaceAll(s, "''", "'")
			}
			return i + 1, s, true
		}
		i++
	}
	return i, "", false
}

// scanNumericLiteral scans a numeric literal (integer or float) beginning at
// index i (a digit). It returns the index just past the literal and its parsed
// value (int64 for plain integers, float64 when it has a dot or exponent).
func scanNumericLiteral(sql string, i int) (int, interface{}) {
	start := i
	hasDot := false
	for {
		next, nd, done := advanceNumeric(sql, i, hasDot)
		hasDot = nd
		i = next
		if done {
			break
		}
	}
	numStr := sql[start:i]
	if hasDot || containsExp(numStr) {
		v, _ := strconv.ParseFloat(numStr, 64)
		return i, v
	}
	return i, fastParseInt64(numStr)
}

// advanceNumeric consumes one character of a numeric literal, returning the
// next index, whether a dot has been seen, and whether scanning is done. An
// exponent consumes the optional sign and following digits in one step.
func advanceNumeric(sql string, i int, hasDot bool) (int, bool, bool) {
	if i >= len(sql) {
		return i, hasDot, true
	}
	switch c := sql[i]; {
	case c >= '0' && c <= '9':
		return i + 1, hasDot, false
	case c == '.':
		return i + 1, true, false
	case c == 'e' || c == 'E':
		j := i + 1
		if j < len(sql) && (sql[j] == '+' || sql[j] == '-') {
			j++
		}
		return scanNumDigits(sql, j), true, true
	}
	return i, hasDot, true
}

// scanNumDigits advances j past a run of decimal digits.
func scanNumDigits(sql string, j int) int {
	for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
		j++
	}
	return j
}

// scanDotNumeric scans a numeric literal that starts with a dot (e.g. .5),
// returning the index just past the literal and its float value.
func scanDotNumeric(sql string, i int) (int, float64) {
	start := i
	i++
	for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
		i++
	}
	v, _ := strconv.ParseFloat(sql[start:i], 64)
	return i, v
}

// fastParseInt64 parses a non-negative decimal integer string without sign.
// Faster than strconv.ParseInt for the common case of simple digits.
func fastParseInt64(s string) int64 {
	n := int64(0)
	for _, c := range []byte(s) {
		n = n*10 + int64(c-'0')
	}
	return n
}

// containsDoubleQuote checks if a string contains SQL escaped quotes (”).
func containsDoubleQuote(s string) bool {
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '\'' && s[i+1] == '\'' {
			return true
		}
	}
	return false
}

// containsExp checks if a string contains 'e' or 'E' (scientific notation marker).
func containsExp(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 'e' || s[i] == 'E' {
			return true
		}
	}
	return false
}
