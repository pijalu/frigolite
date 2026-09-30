package exec

import (
	"strconv"
	"strings"
)

// identContTable classifies bytes that continue an identifier or parameter
// token: identifier characters plus the parameter sigils (\$, :, @, #) whose
// names admit digits.
var identContTable = func() [256]bool {
	var t [256]bool
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	t['_'] = true
	t['$'] = true
	t[':'] = true
	t['@'] = true
	t['#'] = true
	for c := 0x80; c < 256; c++ {
		t[c] = true
	}
	return t
}()

// continuesIdentToken reports whether the byte at i continues an identifier
// or parameter token: the previous byte is an identifier character or one of
// the parameter sigils (\$, :, @, #, whose names admit digits).
func continuesIdentToken(s string, i int) bool {
	return i > 0 && identContTable[s[i-1]]
}

// normalizeSQL replaces all numeric and string literals in a SQL string with '?'.
// Returns the normalized string and the extracted literal values.
// This is a fast pre-parse scan — it does NOT use the full parser.
// Only handles simple quoted strings and decimal integers/floats.
//
// Literal-free input is returned unchanged (no copy): the template and
// statement caches compare the normalized text with the input for identity.
//
// Bytes that continue an identifier or parameter token (digits after a
// letter, as in "t1" or "u2", or after a parameter sigil) are NOT literal
// starts: treating them as literals would merge unrelated statements into
// one template-cache key with phantom values, and the substitution walk
// would then refuse every such statement (forcing a full parse).
func normalizeSQL(s string) (norm string, values []interface{}) {
	buf, values, _ := normalizeSQLScratch(s, nil, nil)
	return string(buf), values
}

// normalizeSQLScratch is normalizeSQL reusing the caller's byte and value
// buffers (the engine's per-statement scratch: the substitution buffer alone
// is one SQL-text-sized allocation per statement otherwise). It returns the
// substituted text, the values, and the (possibly grown) scratch buffers for
// the next statement. Callers must not retain the returned values slice past
// the statement — it is the recycled scratch.
func normalizeSQLScratch(s string, buf []byte, values []interface{}) (norm string, outValues []interface{}, outBuf []byte) {
	last := 0
	i := 0
	started := false
	for i < len(s) {
		next, val, ok := nextLiteral(s, i)
		if !ok {
			i++
			continue
		}
		if !started {
			started = true
			if buf == nil {
				buf = make([]byte, 0, len(s))
			}
			if values == nil {
				values = make([]interface{}, 0, 4)
			} else {
				values = values[:0]
			}
		}
		buf = append(buf, s[last:i]...)
		buf = append(buf, '?')
		values = append(values, val)
		i = next
		last = i
	}
	if !started {
		return s, nil, buf
	}
	buf = append(buf, s[last:]...)
	return string(buf), values, buf
}

// nextLiteral scans the literal starting at i (if any), returning the index
// just past it, its value, and whether a literal starts at i.
func nextLiteral(s string, i int) (next int, val interface{}, ok bool) {
	switch c := s[i]; {
	case c == '\'':
		j, str, found := scanStringLiteral(s, i)
		if !found {
			return j, nil, false
		}
		return j, str, true
	case c >= '0' && c <= '9' && !continuesIdentToken(s, i):
		j, v := scanNumericLiteral(s, i)
		return j, v, true
	case c == '.' && !continuesIdentToken(s, i):
		j, v := scanDotNumeric(s, i)
		return j, v, true
	}
	return i, nil, false
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
	if v, ok := fastParseInt64(numStr); ok {
		return i, v
	}
	// Overflow (e.g. 2^64): keep the value as float64. Substitution into an
	// integer-shaped slot then refuses (numeric's kind gate) and the
	// statement full-parses — two distinct overflowing literals must never
	// share one template substitution (func4-5.29: tointeger(toreal(
	// 18446744073709551616)) would otherwise serve a toreal(0) template,
	// because the wrapped value equals literal 0's).
	v, _ := strconv.ParseFloat(numStr, 64)
	return i, v
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
// ok=false reports overflow (the returned value is meaningless).
func fastParseInt64(s string) (int64, bool) {
	n := int64(0)
	for i := 0; i < len(s); i++ {
		d := int64(s[i] - '0')
		if n > (1<<63-1-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
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
