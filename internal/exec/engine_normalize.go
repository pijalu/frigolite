package exec

import (
	"hash/maphash"
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

// normSpan is one literal's [start, end) byte range in the ORIGINAL sql
// text. The span list is the normalized form's recipe: the normalized text
// is the original with every span replaced by a single '?', which lets the
// template-cache lookup verify a candidate entry against the original bytes
// (and the store path materialize the normalized text) without a second
// rewrite pass per statement.
type normSpan struct {
	start, end int
}

// normalizeScan is the one-pass text pipeline behind Prepare's template
// cache: it extracts the literal values, records each literal's span, and —
// when h is non-nil — hashes the normalized form (the original with every
// literal replaced by '?') into h incrementally, so the per-statement path
// never materializes the normalized bytes. The scan itself skips bytes that
// cannot start a literal with a tight class test, and each candidate is
// resolved by the same nextLiteral the scratch form uses, so the extracted
// values (and the implied normalized text) are byte-identical to
// normalizeSQLScratch's.
//
// ok=false reports no literals (values/spans are nil): the value count gates
// every template-cache use. The values and spans slices are the caller's
// recycled scratch (the engine's per-statement buffers); callers must not
// retain them past the statement.
func normalizeScan(s string, values []interface{}, spans []normSpan, h *maphash.Hash) (outValues []interface{}, outSpans []normSpan, ok bool) {
	started := false
	i, last := 0, 0
	for i < len(s) {
		c := s[i]
		// Fast screen: only a quote, a digit, or a dot can start a literal
		// (nextLiteral's three cases). Everything else advances one byte.
		if c != '\'' && c != '.' && (c < '0' || c > '9') {
			i++
			continue
		}
		next, val, isLit := nextLiteral(s, i)
		if !isLit {
			i++
			continue
		}
		if !started {
			values, spans = resetNormScratch(values, spans)
			if h != nil {
				h.Reset()
			}
			started = true
		}
		values, spans = emitNormLiteral(s, values, spans, h, normSpan{start: i, end: next}, last, val)
		i = next
		last = i
	}
	if !started {
		return nil, nil, false
	}
	if h != nil {
		h.WriteString(s[last:])
	}
	return values, spans, true
}

// resetNormScratch readies the recycled value/span buffers for a statement
// that turned out to carry literals.
func resetNormScratch(values []interface{}, spans []normSpan) ([]interface{}, []normSpan) {
	if values == nil {
		values = make([]interface{}, 0, 4)
	} else {
		values = values[:0]
	}
	if spans == nil {
		spans = make([]normSpan, 0, 4)
	} else {
		spans = spans[:0]
	}
	return values, spans
}

// emitNormLiteral records one found literal: its value, its span, and the
// normalized-form segments up to it (a hash of them when h is non-nil —
// WriteString copies into the hash's internal buffer, no per-segment copy of
// the SQL text remains).
func emitNormLiteral(s string, values []interface{}, spans []normSpan, h *maphash.Hash, sp normSpan, last int, val interface{}) ([]interface{}, []normSpan) {
	if h != nil {
		h.WriteString(s[last:sp.start])
		h.WriteByte('?')
	}
	values = append(values, val)
	spans = append(spans, sp)
	return values, spans
}

// materializeNorm rebuilds the normalized text from the original and its
// literal spans into buf (appending, reusing cap). Only the cache-store path
// pays this — once per unique template.
func materializeNorm(s string, spans []normSpan, buf *[]byte) []byte {
	b := *buf
	if b == nil {
		b = make([]byte, 0, len(s))
	} else {
		b = b[:0]
	}
	last := 0
	for _, sp := range spans {
		b = append(b, s[last:sp.start]...)
		b = append(b, '?')
		last = sp.end
	}
	b = append(b, s[last:]...)
	*buf = b
	return b
}

// templateMatchesSpans verifies that template equals the normalized form of
// s implied by spans: the original text with every span replaced by a single
// '?'. Substring compares only — no normalized bytes are materialized — so
// the per-statement lookup's verification never allocates.
func templateMatchesSpans(template, s string, spans []normSpan) bool {
	ti := 0
	last := 0
	for _, sp := range spans {
		seg := s[last:sp.start]
		if ti+len(seg) > len(template) || template[ti:ti+len(seg)] != seg {
			return false
		}
		ti += len(seg)
		if ti >= len(template) || template[ti] != '?' {
			return false
		}
		ti++
		last = sp.end
	}
	return template[ti:] == s[last:]
}

// normalizeSQLScratch replaces all numeric and string literals in a SQL
// string with '?', returning the normalized text and the extracted literal
// values. This is a fast pre-parse scan — it does NOT use the full parser.
// Only handles simple quoted strings and decimal integers/floats.
//
// The scan shares normalizeScan's single pass; the normalized text is
// materialized here (the legacy form). The exec path uses the fused
// normalizeScan directly and materializes only on the template-store path.
//
// Literal-free input is returned unchanged (no copy): the template and
// statement caches compare the normalized text with the input for identity.
//
// The caller's byte and value buffers are reused as scratch. It returns the
// substituted text AS SCRATCH BYTES (nil when the input held no literals —
// the value count gates every template-cache use), the values, and the
// (possibly grown) scratch buffers for the next statement. Callers must not
// retain the returned bytes or values slice past the statement.
func normalizeSQLScratch(s string, buf []byte, values []interface{}) (norm []byte, outValues []interface{}, outBuf []byte) {
	spans := make([]normSpan, 0, 4)
	vals, spans, ok := normalizeScan(s, values, spans, nil)
	if !ok {
		return nil, nil, buf
	}
	return materializeNorm(s, spans, &buf), vals, buf
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
