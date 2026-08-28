package execexpr

import (
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
)

func likeValues(str, pattern interface{}) bool {
	s := util.SQLiteValueString(unwrapCollatedValue(str))
	p := util.SQLiteValueString(unwrapCollatedValue(pattern))
	return likeMatch(s, p)
}

func likeValuesCaseSensitive(str, pattern interface{}) bool {
	s := util.SQLiteValueString(unwrapCollatedValue(str))
	p := util.SQLiteValueString(unwrapCollatedValue(pattern))
	return likeMatchCS(s, p)
}

// likeValuesWithEscape performs LIKE matching with an escape character.
func likeValuesWithEscape(str, pattern interface{}, escape string) bool {
	s := util.SQLiteValueString(unwrapCollatedValue(str))
	p := util.SQLiteValueString(unwrapCollatedValue(pattern))
	return likeMatchEscaped(s, p, escape)
}

// likeValuesWithEscapeCS performs LIKE matching with an escape character and
// case-sensitive comparison (PRAGMA case_sensitive_like=ON).
func likeValuesWithEscapeCS(str, pattern interface{}, escape string) bool {
	s := util.SQLiteValueString(unwrapCollatedValue(str))
	p := util.SQLiteValueString(unwrapCollatedValue(pattern))
	return likeMatchEscapedCS(s, p, escape)
}

func likeMatch(s, pattern string) bool {
	return likeMatchFold(s, pattern)
}

// likeMatchCS performs LIKE matching with case-sensitive comparison
// (PRAGMA case_sensitive_like=ON). Character-based: the pattern and string
// are matched by code point, not by byte.
func likeMatchCS(s, pattern string) bool {
	return likeMatchRunes(sqliteCodePoints(s), sqliteCodePoints(pattern), 0, 0, 0, false)
}

// likeMatchFold performs LIKE matching with SQLite's default
// case-insensitive comparison (ASCII-only folding, like sqlite3Tolower).
func likeMatchFold(s, pattern string) bool {
	return likeMatchRunes(sqliteCodePoints(s), sqliteCodePoints(pattern), 0, 0, 0, true)
}

func likeMatchEscaped(s, pattern, escape string) bool {
	if escape == "" {
		return likeMatch(s, pattern)
	}
	// Process the pattern, treating escape char + next char as literal
	return likeMatchEscapedFold(s, pattern, escape)
}

func likeMatchEscapedCS(s, pattern, escape string) bool {
	if escape == "" {
		return likeMatchCS(s, pattern)
	}
	return likeMatchRunes(sqliteCodePoints(s), sqliteCodePoints(pattern), 0, 0, sqliteCodePoints(escape)[0], false)
}

func likeMatchEscapedFold(s, pattern, escape string) bool {
	if escape == "" {
		return likeMatchFold(s, pattern)
	}
	return likeMatchRunes(sqliteCodePoints(s), sqliteCodePoints(pattern), 0, 0, sqliteCodePoints(escape)[0], true)
}

// likeRuneEq compares two runes with optional ASCII case folding. SQLite's
// default LIKE is case-insensitive only for ASCII (sqlite3Tolower); non-ASCII
// code points compare exactly.
func likeRuneEq(a, b rune, fold bool) bool {
	if a == b {
		return true
	}
	if !fold {
		return false
	}
	// ASCII-only folding.
	la := a
	if la >= 'A' && la <= 'Z' {
		la = la + ('a' - 'A')
	}
	lb := b
	if lb >= 'A' && lb <= 'Z' {
		lb = lb + ('a' - 'A')
	}
	return la == lb
}

func toFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case value.ZeroBlob:
		// zeroblob expands to N zero bytes; the numeric prefix of a
		// zero blob is 0 (zeroblob(8)+3 is 3).
		_ = x
		return 0, true
	case []byte:
		// SQLite applies the same text->numeric conversion to BLOBs in
		// arithmetic (blob + 3 treats the blob bytes as text; a zero blob
		// contributes 0). zeroblob(8)+3 is 3.
		if f, ok := parseNumericPrefix(string(x)); ok {
			return f, true
		}
		return 0, true
	case string:
		// SQLite text→numeric conversion in arithmetic uses the leading
		// numeric prefix: '1x' → 1, 'x1' → 0, ' 12.5foo' → 12.5, and
		// empty/whitespace/dot strings are 0. (sqlite3AtoF semantics.)
		// A string with no numeric prefix at all ("abc") is numeric 0.
		if f, ok := parseNumericPrefix(x); ok {
			return f, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// sqliteReadCodePoint reads one code point from s starting at i the way
// SQLite's sqlite3Utf8Read does: ASCII bytes and lone continuation bytes are
// single code points (read as themselves), while a byte >= 0xC0 begins a
// multi-byte UTF-8 sequence whose continuation bytes are consumed when valid.
// Returns the code point and the next position.
func sqliteReadCodePoint(s string, i int) (rune, int) {
	c := s[i]
	if c < 0x80 {
		return rune(c), i + 1
	}
	if c < 0xC0 || c >= 0xFE {
		// Lone continuation byte (or 0xFE/0xFF): read as a single
		// code point (sqlite3Utf8Read returns the byte itself).
		return rune(c), i + 1
	}
	width := utf8SeqWidth(c)
	got := 1
	cp := rune(c)
	// Build the code point: strip the leading bits.
	switch width {
	case 2:
		cp = rune(c & 0x1F)
	case 3:
		cp = rune(c & 0x0F)
	case 4:
		cp = rune(c & 0x07)
	}
	// Read the full sequence if continuation bytes follow; otherwise treat
	// the start byte as a lone code point.
	j := i + 1
	for got < width && j < len(s) && s[j]&0xC0 == 0x80 {
		cp = cp<<6 | rune(s[j]&0x3F)
		j++
		got++
	}
	return cp, j
}

// utf8SeqWidth returns the byte width of a UTF-8 sequence starting with the
// given lead byte (2-4 for 0xC0-0xFD lead bytes).
func utf8SeqWidth(c byte) int {
	switch {
	case c >= 0xF0:
		return 4
	case c >= 0xE0:
		return 3
	default:
		return 2
	}
}

// validUTF8 reports whether s is valid UTF-8: every byte >= 0x80 is a
// multi-byte sequence start (not a lone continuation byte) with the expected
// number of continuation bytes.
func validUTF8(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 {
			continue
		}
		// Check it's a valid multi-byte start (not a lone continuation).
		if c < 0xC0 || c >= 0xFE {
			return false
		}
		width := utf8SeqWidth(c)
		if i+width > len(s) {
			return false
		}
		for j := 1; j < width; j++ {
			if s[i+j]&0xC0 != 0x80 {
				return false
			}
		}
		i += width - 1
	}
	return true
}

// scanNumericExponent scans an exponent part ([eE] [sign] digits) starting
// after the 'e'/'E' at position i, returning the position after the exponent
// digits (or i when the exponent has no digits).
func scanNumericExponent(t string, i int) int {
	j := i + 1
	if j < len(t) && (t[j] == '+' || t[j] == '-') {
		j++
	}
	if j < len(t) && t[j] >= '0' && t[j] <= '9' {
		i = j
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
		}
	}
	return i
}

// scanDigits advances i past a run of ASCII digits, returning the new index.
func scanDigits(t string, i int) int {
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	return i
}

// scanNumericPrefix scans a string's leading numeric prefix
// ([sign] digits [.digits] [eE [sign] digits]) and returns the matched text.
func scanNumericPrefix(t string) (string, bool) {
	i := 0
	if i < len(t) && (t[i] == '+' || t[i] == '-') {
		i++
	}
	digits := scanDigits(t, i)
	i = digits
	if i < len(t) && t[i] == '.' {
		i++
		fracDigits := scanDigits(t, i)
		digits = fracDigits
		i = fracDigits
	}
	if digits > 0 && i < len(t) && (t[i] == 'e' || t[i] == 'E') {
		i = scanNumericExponent(t, i)
	}
	if digits > 0 {
		return t[:i], true
	}
	return "", false
}

// likeMatchPercent handles the '%' wildcard case in likeMatchRunes: try every
// remaining position of the string, recursing for the rest of the pattern.
func likeMatchPercent(s, pattern []rune, idx, patIdx int, escapeRune rune, fold bool) bool {
	patIdx++
	if patIdx >= len(pattern) {
		return true
	}
	for idx <= len(s) {
		if likeMatchRunes(s, pattern, idx, patIdx, escapeRune, fold) {
			return true
		}
		idx++
	}
	return false
}

// likeMatchLiteral matches one literal pattern rune (default case) or the '_'
// single-character wildcard at position idx. Returns the advanced positions
// and whether the match succeeded.
func likeMatchLiteral(s, pattern []rune, idx, patIdx int, c rune, fold bool) (int, int, bool) {
	if c == '_' {
		if idx >= len(s) {
			return 0, 0, false
		}
		return idx + 1, patIdx + 1, true
	}
	if idx >= len(s) || !likeRuneEq(s[idx], c, fold) {
		return 0, 0, false
	}
	return idx + 1, patIdx + 1, true
}

// likeMatchEscapedRune handles the ESCAPE-character case in likeMatchRunes:
// the rune after the escape character matches literally. Returns the advanced
// (idx, patIdx) and whether the literal matched.
func likeMatchEscapedRune(s, pattern []rune, idx, patIdx int, escapeRune rune, fold bool) (int, int, bool) {
	nextChar := pattern[patIdx+1]
	if idx >= len(s) || !likeRuneEq(s[idx], nextChar, fold) {
		return 0, 0, false
	}
	return idx + 1, patIdx + 2, true
}

// normalizeINItemShape validates an IN-list item's shape against the operand
// (scalar or row value) and returns the item's row-value form. A scalar item
// is promoted to a 1-element row value when the operand is a row value of
// arity 1; other shape mismatches are SQLite "row value misused" / arity
// errors.

// likeMatchRunes matches a string rune slice against a pattern rune slice,
// honoring % and _ wildcards and an optional escape rune. Runes are used (not
// bytes) so multi-byte UTF-8 sequences match correctly — SQLite matches LIKE
// against code points, not raw bytes (e.g. '%\x80' requires the string to end
// in U+0080, not just any byte 0x80 continuation).
func likeMatchRunes(s, pattern []rune, idx, patIdx int, escapeRune rune, fold bool) bool {
	for patIdx < len(pattern) {
		c := pattern[patIdx]
		if escapeRune != 0 && c == escapeRune && patIdx+1 < len(pattern) {
			// Escape char followed by another char: treat the next char as
			// literal.
			var ok bool
			idx, patIdx, ok = likeMatchEscapedRune(s, pattern, idx, patIdx, escapeRune, fold)
			if !ok {
				return false
			}
			continue
		}
		switch c {
		case '%':
			return likeMatchPercent(s, pattern, idx, patIdx, escapeRune, fold)
		default:
			var ok bool
			idx, patIdx, ok = likeMatchLiteral(s, pattern, idx, patIdx, c, fold)
			if !ok {
				return false
			}
		}
	}
	return idx >= len(s)
}

// parseNumericPrefix parses the leading numeric prefix of a string the way
// SQLite's sqlite3AtoF does: optional whitespace, sign, digits, optional
// fraction, optional exponent, stopping at the first non-numeric character.
// It returns false when the string has no numeric prefix at all (e.g. "x1").
func parseNumericPrefix(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	// Empty/whitespace-only strings and a lone '.' are numeric 0.
	if t == "" || t == "." || t == "+." || t == "-." {
		return 0, true
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f, true
	}
	// Numeric prefix parse: [sign] digits [.digits] [eE [sign] digits]
	prefix, ok := scanNumericPrefix(t)
	if !ok {
		return 0, false
	}
	if f, err := strconv.ParseFloat(prefix, 64); err == nil {
		return f, true
	}
	// Overflow: SQLite returns +/-Inf.
	if t[0] == '-' {
		return math.Inf(-1), true
	}
	return math.Inf(1), true
}

// sqliteCodePoints converts a byte string to code points the way SQLite's
// sqlite3Utf8Read does: ASCII bytes and lone continuation bytes (0x80-0xBF)
// are single code points (a lone continuation byte reads as itself), while a
// byte >= 0xC0 begins a multi-byte UTF-8 sequence. Go's []rune would replace
// invalid sequences (like a lone 0x80 byte) with U+FFFD, which disagrees with
// SQLite's LIKE matching (e.g. '%\x80' must match a string ending in U+0080).
func sqliteCodePoints(s string) []rune {
	// Fast path: valid UTF-8 decodes identically to SQLite's reader.
	if validUTF8(s) {
		return []rune(s)
	}
	// Invalid UTF-8: read code points the SQLite way.
	var out []rune
	for i := 0; i < len(s); {
		cp, j := sqliteReadCodePoint(s, i)
		out = append(out, cp)
		i = j
	}
	return out
}
