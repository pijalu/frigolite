// Allocation-free lexical scan helpers shared by the parse preprocessing
// pipeline. The pipeline runs once per statement, so every check here must
// avoid copying the SQL text: the historical strings.ToUpper /
// stripSQLComments-based pre-checks allocated a full copy of the statement
// (or more) per parse.

package parse

import (
	"unicode"
	"unicode/utf8"

	"github.com/pijalu/frigolite/internal/util"
)

// indexFoldASCII returns the index of the first occurrence of sub in s under
// ASCII case folding, or -1. Delegates to the shared util implementation.
func indexFoldASCII(s, sub string) int {
	return util.IndexFoldASCII(s, sub)
}

// containsFoldASCII reports whether s contains sub under ASCII case folding.
// It is a superset of strings.Contains(strings.ToUpper(s), sub) for
// uppercase ASCII sub, so callers can gate exact-case work on it.
func containsFoldASCII(s, sub string) bool {
	return indexFoldASCII(s, sub) >= 0
}

// hasPrefixFoldASCII reports whether s starts with prefix under ASCII case
// folding. Equivalent to strings.HasPrefix(strings.ToUpper(s), prefix) for
// uppercase ASCII prefix (Go's ToUpper maps no non-ASCII byte to a byte
// spelling these keywords).
func hasPrefixFoldASCII(s, prefix string) bool {
	return util.HasPrefixFoldASCII(s, prefix)
}

// isEmptySQLSegment reports whether seg is only whitespace and/or comments
// (line "--" and block "/* */"), i.e. an empty statement. Equivalent to
// strings.TrimSpace(stripSQLComments(seg)) == "" without copying the text:
// quoted strings count as content in both.
func isEmptySQLSegment(seg string) bool {
	return isEmptySQLSegmentSkip(seg, nil)
}

// isEmptySQLSegmentSkip extends the emptiness scan with a set of extra bytes
// to ignore (whenOnlyWS is consulted for characters that neither end the scan
// nor belong to a comment). This serves the SAVEPOINT-placeholder check that
// additionally ignores semicolons.
//
// Non-ASCII bytes are decoded so Unicode whitespace (U+00A0 NBSP and friends,
// which strings.TrimSpace strips and the historical
// TrimSpace(stripSQLComments(...)) check therefore treated as empty) behaves
// identically; any other non-ASCII rune counts as content.
func isEmptySQLSegmentSkip(seg string, whenOnlyWS func(byte) bool) bool {
	i := 0
	for i < len(seg) {
		next, ok := skipSegmentFiller(seg, i, whenOnlyWS)
		if !ok {
			return false
		}
		i = next
	}
	return true
}

// skipSegmentFiller consumes the whitespace/comment filler starting at i,
// returning the next index and whether the position was filler. Non-ASCII
// whitespace (which strings.TrimSpace strips, mirroring the historical
// emptiness check) counts as filler; any other rune is content.
func skipSegmentFiller(seg string, i int, whenOnlyWS func(byte) bool) (int, bool) {
	c := seg[i]
	if isASCIIWS(c) {
		return i + 1, true
	}
	if whenOnlyWS != nil && whenOnlyWS(c) {
		return i + 1, true
	}
	if c >= 0x80 {
		r, size := utf8.DecodeRuneInString(seg[i:])
		if !unicode.IsSpace(r) {
			return i, false
		}
		return i + size, true
	}
	if next, ok := skipSegmentComment(seg, i); ok {
		return next, true
	}
	return i, false
}

// skipSegmentComment consumes a "--" or "/*" comment starting at i, returning
// the next index and whether a comment was consumed.
func skipSegmentComment(seg string, i int) (int, bool) {
	if c := seg[i]; c == '-' && i+1 < len(seg) && seg[i+1] == '-' {
		next := skipLineComment(seg, i)
		if next < len(seg) {
			next++ // consume the terminating newline
		}
		return next, true
	}
	// Matches stripSQLComments: an unterminated block comment consumes to
	// end (the emptiness scan intentionally does NOT apply the tokenizer's
	// "/* at EOF is an operator" rule — stripSQLComments never sees it).
	if seg[i] == '/' && i+1 < len(seg) && seg[i+1] == '*' {
		return skipBlockComment(seg, i), true
	}
	return i, false
}

// isASCIIWS reports whether c is ASCII whitespace.
func isASCIIWS(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}
