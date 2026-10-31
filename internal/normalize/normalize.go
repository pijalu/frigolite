package normalize

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrSyntax is returned (wrapped) when the input contains an illegal SQL
// token. In C, sqlite3_normalize() signals this by returning NULL.
var ErrSyntax = errors.New("normalize: illegal token")

// Normalize returns a normalized copy of the SQL text in sql, or an error
// wrapping ErrSyntax if sql contains an illegal token.
//
// See the package documentation for the exact transformations and their
// provenance in ext/misc/normalize.c.
func Normalize(sql string) (string, error) {
	src := []byte(sql)
	if i := bytes.IndexByte(src, 0); i >= 0 {
		// C-string semantics: sqlite3_normalize() never sees past an
		// embedded NUL byte.
		src = src[:i]
	}
	// First pass: tokenize and emit, literals as "?", lowercased, comments
	// and superfluous whitespace gone (sqlite3_normalize's main loop).
	out := make([]byte, 0, len(src)+2)
	for i := 0; i < len(src); {
		n, typ := getToken(src[i:])
		switch typ {
		case tokSpace:
			// drop
		case tokError:
			// The C returns NULL here; the offset is Go-side diagnostics.
			return "", fmt.Errorf("%w at byte offset %d (byte 0x%02x)", ErrSyntax, i, src[i])
		case tokLiteral:
			out = append(out, '?')
		case tokPunct, tokName:
			out = appendToken(out, src[i:i+n])
		}
		i += n
	}
	out = terminate(out)
	return string(collapseINLists(out)), nil
}

// terminate strips trailing spaces and appends ';' to non-empty output,
// mirroring the C's trailing fixup.
func terminate(out []byte) []byte {
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	if len(out) > 0 && out[len(out)-1] != ';' {
		out = append(out, ';')
	}
	return out
}

// appendToken emits one tokPunct/tokName token (with the C's special
// handling of "NULL") onto the output buffer.
func appendToken(out, tok []byte) []byte {
	if len(tok) == 4 && eqNullCI(tok) && !nullIsKeyword(out) {
		// Here the NULL is a literal value.
		return append(out, '?')
	}
	// Separate two adjacent identifier-ish tokens with a single space.
	// Note this never fires after a "?" (not an identifier character),
	// faithfully reproducing the C's output spacing quirks.
	if len(out) > 0 && idChar(out[len(out)-1]) && idChar(tok[0]) {
		out = append(out, ' ')
	}
	for _, c := range tok {
		out = append(out, toLower(c))
	}
	return out
}

// eqNullCI reports whether tok is "NULL" in any ASCII case mixture,
// matching sqlite3_strnicmp(tok, "NULL", 4) == 0 over UpperToLower.
func eqNullCI(tok []byte) bool {
	return toLower(tok[0]) == 'n' && toLower(tok[1]) == 'u' &&
		toLower(tok[2]) == 'l' && toLower(tok[3]) == 'l'
}

// nullIsKeyword reports whether the output built so far ends with "is" or
// "not" as a complete token, in which case a following "NULL" is the
// keyword (IS NULL / NOT NULL) rather than a literal. This inspects the
// OUTPUT text exactly like the C's strncmp(z+j-2,"is",2) checks.
func nullIsKeyword(out []byte) bool {
	j := len(out)
	if j >= 3 && out[j-2] == 'i' && out[j-1] == 's' && !idChar(out[j-3]) {
		return true
	}
	return j >= 4 && out[j-3] == 'n' && out[j-2] == 'o' && out[j-1] == 't' && !idChar(out[j-4])
}

// inLParen is the second-pass search needle.
var inLParen = []byte("in(")

// collapseINLists is normalize.c's second pass: rewrite every "in(...)"
// whose parenthesized list is not a WITH or SELECT into "in(?,?,?)",
// scanning the (lowercased) first-pass output in place.
func collapseINLists(b []byte) []byte {
	j := len(b)
	for i := 0; i < j; {
		off := bytes.Index(b[i:j], inLParen)
		if off < 0 {
			break
		}
		off += i
		n := off + 3 // index of the first char past "in("
		if inSkip(b, off) {
			i = n
			continue
		}
		k := inListLen(b, n, j)
		b = replaceWithQQQ(b, n, k, j)
		j = len(b)
		i = n
	}
	return b[:j]
}

// inSkip reports whether the "in(" at b[off:] must be left alone: either
// "in" is the tail of a longer identifier ("min(", "origin(", ...), or the
// list is a WITH/SELECT subquery. At offset 0 there is no preceding
// character, so the replacement proceeds (see doc.go deviation note).
func inSkip(b []byte, off int) bool {
	if off > 0 && idChar(b[off-1]) {
		return true
	}
	return endsWithNonID(b, off, "in(select") || endsWithNonID(b, off, "in(with")
}

// inListLen returns the number of bytes k between the parentheses that
// follow "in(" — the '(' is at b[n] — by paren counting, or the rest of
// the string when they are unbalanced.
func inListLen(b []byte, n, j int) int {
	nParen := 1
	for k := 0; n+k < j; k++ {
		c := b[n+k]
		if c == '(' {
			nParen++
		}
		if c == ')' {
			nParen--
			if nParen == 0 {
				return k
			}
		}
	}
	return j - n
}

// replaceWithQQQ overwrites the k bytes at b[n:] with "?,?,?" (5 bytes),
// shifting the tail text accordingly, and returns the updated buffer.
func replaceWithQQQ(b []byte, n, k, j int) []byte {
	tail := bytes.Clone(b[n+k : j])
	b = append(b[:n], "?,?,?"...)
	return append(b, tail...)
}

// endsWithNonID reports whether b[off:] begins with prefix and the byte
// after the prefix is not an identifier character (NUL counts, matching
// strncmp against a NUL-terminated string).
func endsWithNonID(b []byte, off int, prefix string) bool {
	end := off + len(prefix)
	if end > len(b) || string(b[off:end]) != prefix {
		return false
	}
	return !idChar(byteAt(b, end))
}
