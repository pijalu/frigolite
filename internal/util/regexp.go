package util

import (
	"fmt"
	"regexp"
	"strings"
)

// CompileRegexp compiles a regexp pattern with SQLite's regexp extension
// semantics (ext/misc/regexp.c), adapted to Go's regexp engine.
//
// It performs three SQLite-compatible adjustments:
//   - SQLite's extension understands \uXXXX unicode escapes; Go's engine does
//     not, so they are rewritten to Go's \x{XXXX} form.
//   - A '-' immediately before the closing ']' of a character class is an
//     error ("unclosed '['") in SQLite's parser but is legal in Go, so such
//     patterns are rejected.
//   - Go rejects repetition counts over its internal limit with an "invalid
//     repeat count" error; SQLite reports the same situation as "REGEXP
//     pattern too big", so the message is translated.
func CompileRegexp(pattern string) (*regexp.Regexp, error) {
	if err := validateRegexpClass(pattern); err != nil {
		return nil, err
	}
	translated := translateRegexpUnicodeEscapes(pattern)
	re, err := regexp.Compile(translated)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "invalid repeat count") {
			return nil, fmt.Errorf("REGEXP pattern too big")
		}
		return nil, err
	}
	return re, nil
}

// translateRegexpUnicodeEscapes rewrites \uXXXX (and \u{XXXXX}) escapes into
// Go's \x{XXXX} form. \xHH forms are already supported by Go.
func translateRegexpUnicodeEscapes(p string) string {
	if !strings.Contains(p, `\u`) {
		return p
	}
	var sb strings.Builder
	for i := 0; i < len(p); {
		if n, replaced := writeUnicodeEscape(&sb, p, i); replaced {
			i = n
			continue
		}
		sb.WriteByte(p[i])
		i++
	}
	return sb.String()
}

// writeUnicodeEscape attempts to write a translated \u escape starting at i.
// Returns the next index and true if an escape was written.
func writeUnicodeEscape(sb *strings.Builder, p string, i int) (int, bool) {
	if i+1 >= len(p) || p[i] != '\\' || p[i+1] != 'u' {
		return i, false
	}
	j := i + 2
	if j < len(p) && p[j] == '{' {
		end := strings.IndexByte(p[j:], '}')
		if end > 0 {
			sb.WriteString(`\x{`)
			sb.WriteString(p[j+1 : j+end])
			sb.WriteString("}")
			return j + end + 1, true
		}
	}
	if j+4 <= len(p) && isHex4(p[j:j+4]) {
		sb.WriteString(`\x{`)
		sb.WriteString(p[j : j+4])
		sb.WriteString("}")
		return j + 4, true
	}
	return i, false
}

func isHex4(s string) bool {
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// validateRegexpClass rejects character classes with a trailing '-' before
// the closing ']' (SQLite's parser treats that as an unterminated range and
// reports "unclosed '['").
func validateRegexpClass(p string) error {
	inClass := false
	escaped := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '[' {
			inClass = true
			continue
		}
		if c == ']' {
			inClass = false
			continue
		}
		if inClass && c == '-' && i+1 < len(p) && p[i+1] == ']' {
			return fmt.Errorf("unclosed '['")
		}
	}
	return nil
}
