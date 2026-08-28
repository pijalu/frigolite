// Package main implements the tcl2go tool.
//
// This file contains TCL string utilities: unescaping, list splitting, and
// regex pattern helpers.
package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// (imports managed by goimports)

// unescapeBareWord applies TCL backslash substitution to an unquoted word,
// preserving the escapes that must survive for buildStringExpr to interpret
// $var and [cmd] references. TCL unquoted words substitute backslash
// sequences (\n, \t, octal, hex, ...), but \$ and \[ yield literal characters
// rather than interpolation triggers; those must stay escaped so
// buildStringExpr does not treat them as variable/command references.
func unescapeBareWord(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i = unescapeBareWordEscape(s, i, &b) - 1
	}
	return b.String()
}

// unescapeBareWordEscape processes one backslash escape at position i (the
// backslash), writing its expansion and returning the index of the next
// unprocessed character.
func unescapeBareWordEscape(s string, i int, b *strings.Builder) int {
	next := s[i+1]
	switch next {
	case '$', '[', ']', '{', '}':
		b.WriteByte('\\')
		b.WriteByte(next)
		return i + 2
	case '\n':
		return i + 2
	case '\r':
		if i+2 < len(s) && s[i+2] == '\n' {
			return i + 3
		}
	}
	j := i + 1 // escape character position
	if nextJ, ok := writeOctEscape(s, j, b); ok {
		return nextJ
	}
	if nextJ, ok := writeHexEscape(s, j, b); ok {
		return nextJ
	}
	writeLetterEscape(s[j], b)
	return j + 1
}

// writeOctEscape writes a TCL octal escape (\ooo, 1-3 octal digits) starting
// at position j (the first digit). Returns the next index and whether an octal
// escape was present.
func writeOctEscape(s string, j int, b *strings.Builder) (int, bool) {
	if j >= len(s) || s[j] < '0' || s[j] > '7' {
		return 0, false
	}
	val := 0
	for count := 0; count < 3 && j < len(s) && s[j] >= '0' && s[j] <= '7'; count++ {
		val = val*8 + int(s[j]-'0')
		j++
	}
	b.WriteByte(byte(val))
	return j, true
}

// writeHexEscape writes a TCL hex escape (\xHH) starting at position j (the
// 'x'). Returns the next index and whether a hex escape was present.
func writeHexEscape(s string, j int, b *strings.Builder) (int, bool) {
	if j >= len(s) || s[j] != 'x' || j+1 >= len(s) || !isHexDigit(s[j+1]) {
		return 0, false
	}
	h := j + 1
	val := 0
	for count := 0; count < 2 && h < len(s) && isHexDigit(s[h]); count++ {
		val = val*16 + hexVal(s[h])
		h++
	}
	b.WriteByte(byte(val))
	return h, true
}

// writeLetterEscape expands a TCL letter/char escape (n, t, r, v, f, b, a,
// backslash, quote; unknown escapes drop the backslash).
func writeLetterEscape(ch byte, b *strings.Builder) {
	switch ch {
	case 'n':
		b.WriteByte('\n')
	case 't':
		b.WriteByte('\t')
	case 'r':
		b.WriteByte('\r')
	case 'v':
		b.WriteByte('\v')
	case 'f':
		b.WriteByte('\f')
	case 'b':
		b.WriteByte('\b')
	case 'a':
		b.WriteByte('\a')
	case '\\', '"':
		b.WriteByte(ch)
	default:
		// TCL drops the backslash for unrecognized escapes.
		b.WriteByte(ch)
	}
}

// tclUnescapeQuoted converts TCL double-quoted string escapes to the
// characters they denote ("\n" -> newline, "\\" -> backslash, ...).
// Escaped $, [, ], {, } are unescaped so interpolation in buildStringExpr
// treats them as literal characters.
func tclUnescapeQuoted(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i = tclUnescapeQuotedEscape(s, i, &b) - 1
	}
	return b.String()
}

// tclUnescapeQuotedEscape processes one backslash escape at position i (the
// backslash) in a double-quoted word, writing its expansion and returning the
// index of the next unprocessed character.
func tclUnescapeQuotedEscape(s string, i int, b *strings.Builder) int {
	// TCL line continuation: backslash-newline (and backslash-CR-newline)
	// is removed entirely.
	if s[i+1] == '\n' {
		return i + 2
	}
	if s[i+1] == '\r' && i+2 < len(s) && s[i+2] == '\n' {
		return i + 3
	}
	j := i + 1 // escape character position
	if nextJ, ok := writeOctEscape(s, j, b); ok {
		return nextJ
	}
	if nextJ, ok := writeHexEscape(s, j, b); ok {
		return nextJ
	}
	if nextJ, ok := writeUnicodeEscape(s, j, b); ok {
		return nextJ
	}
	if isInterpSensitive(s[j]) {
		// Keep the backslash so parseStringParts treats the escaped char
		// as literal text, not as a $var or [cmd] substitution start.
		b.WriteByte('\\')
		b.WriteByte(s[j])
	} else {
		writeLetterEscape(s[j], b)
	}
	return j + 1
}

// writeUnicodeEscape writes a TCL unicode escape (\uXXXX, one to four hex
// digits) starting at position j (the 'u'). Returns the next index and whether
// a unicode escape was present.
func writeUnicodeEscape(s string, j int, b *strings.Builder) (int, bool) {
	if j >= len(s) || s[j] != 'u' || j+1 >= len(s) || !isHexDigit(s[j+1]) {
		return 0, false
	}
	cp := 0
	digits := 0
	for digits < 4 && j+1+digits < len(s) && isHexDigit(s[j+1+digits]) {
		cp = cp*16 + hexVal(s[j+1+digits])
		digits++
	}
	b.WriteRune(rune(cp))
	return j + 1 + digits, true
}

// isInterpSensitive reports whether a character must stay backslash-escaped so
// parseStringParts does not treat it as a $var or [cmd] substitution start.
func isInterpSensitive(c byte) bool {
	return c == '$' || c == '[' || c == ']' || c == '{' || c == '}'
}

// tclSplitList splits a TCL-format list string into elements. It mirrors the
// runtime helper of the same name embedded in generated tests, but runs at
// generation time in the transpiler (e.g. to classify foreach loop lists).
func tclSplitList(s string) []string {
	var result []string
	pos := 0
	for pos < len(s) {
		pos = skipListSpace(s, pos)
		// A backslash-newline is a TCL line continuation (semantically a
		// single space): consume it so list elements split correctly.
		if pos < len(s) && s[pos] == '\\' && pos+1 < len(s) && (s[pos+1] == '\n' || s[pos+1] == '\r') {
			pos++
			continue
		}
		if pos >= len(s) {
			break
		}
		switch s[pos] {
		case '{':
			el, next := splitListBraced(s, pos)
			result = append(result, el)
			pos = next
		case '"':
			el, next := splitListQuoted(s, pos)
			result = append(result, el)
			pos = next
		default:
			start := pos
			for pos < len(s) && !isListSpace(s[pos]) {
				pos++
			}
			result = append(result, s[start:pos])
		}
	}
	return result
}

// skipListSpace advances pos past whitespace (space, tab, newline, CR).
func skipListSpace(s string, pos int) int {
	for pos < len(s) && isListSpace(s[pos]) {
		pos++
	}
	return pos
}

// isListSpace reports whether c is a TCL list separator whitespace char.
func isListSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// splitListBraced parses a {...} list element starting at pos (the opening
// brace), returning the element text and the position after the closing brace.
func splitListBraced(s string, pos int) (string, int) {
	depth := 1
	start := pos + 1
	pos++
	for pos < len(s) && depth > 0 {
		if s[pos] == '{' {
			depth++
		}
		if s[pos] == '}' {
			depth--
		}
		if depth > 0 {
			pos++
		}
	}
	el := s[start:pos]
	if pos < len(s) {
		pos++
	}
	return el, pos
}

// splitListQuoted parses a "..." list element starting at pos (the opening
// quote), returning the element text and the position after the closing quote.
// The element is a TCL double-quoted word: backslash escapes are processed
// (\" stays inside, \uXXXX and \xXX become characters, \\ is a literal
// backslash — Tcl 8.6, fts4umlaut.test's "Ha N\u1ed9i" list elements).
func splitListQuoted(s string, pos int) (string, int) {
	start := pos + 1
	pos++
	for pos < len(s) {
		if s[pos] == '\\' && pos+1 < len(s) {
			pos += 2
			continue
		}
		if s[pos] == '"' {
			break
		}
		pos++
	}
	el := s[start:pos]
	if pos < len(s) {
		pos++
	}
	return tclUnescapeQuoted(el), pos
}

// tclExprToGo converts a TCL expression string into a form the runtime tclExpr
// helper can evaluate. It returns the list of $var names referenced (in order)
// and the transformed expression string.
//
// Transformations:
//   - $name references are left in place (the runtime helper substitutes them
//     from a provided map).
//   - TCL int(rand()*N) is replaced with a deterministic constant so generated
//     tests are reproducible (the same value is used by both the query and the
//     expected answer since they share the same Go variable).
func tclExprToGo(expr string, vars []string) ([]string, string) {
	s := expr
	var names []string
	seen := make(map[string]bool)
	searchFrom := 0
	for {
		i := strings.Index(s[searchFrom:], "$")
		if i < 0 {
			break
		}
		i += searchFrom
		// Braced variable ${name} — consume through matching }
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.Index(s[i+2:], "}")
			if end >= 0 {
				j := i + 2 + end + 1
				name := s[i+2 : j-1]
				base := name
				if idx := strings.Index(base, "("); idx >= 0 {
					base = base[:idx]
				}
				if !seen[name] && !seen[base] {
					// Record base name for var map lookup
					n := name
					if base != name {
						n = base
					}
					seen[n] = true
					seen[name] = true
					names = append(names, n)
				}
				searchFrom = j
				continue
			}
		}
		j := i + 1
		for j < len(s) && isVarChar(s[j]) {
			j++
		}
		// Consume a TCL array suffix $name(key) so the full reference maps
		// to the predeclared var name (tclVarToGo turns "::name(key)" into
		// "name_key"). Without this, "$::cmdlinearg(INFO_SCRIPT)" would be
		// read as "$::cmdlinearg" and reference an undeclared map variable.
		if j < len(s) && s[j] == '(' {
			if end := strings.IndexByte(s[j+1:], ')'); end >= 0 {
				j = j + 1 + end + 1
			}
		}
		if j == i+1 {
			break
		}
		name := s[i+1 : j]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		searchFrom = j
	}
	// Replace TCL rand usage with the deterministic tclRand() helper so the
	// SQL-building and expected-answer generation call the same sequence.
	re := regexp.MustCompile(`int\(\s*rand\(\)\s*\*\s*([0-9]+)\s*\)`)
	s = re.ReplaceAllString(s, "int(tclRand()*$1)")
	s = strings.ReplaceAll(s, "rand()", "tclRand()")
	return names, coerceRandFloatOperands(s)
}

// buildStringExpr converts TCL text (with possible $var and [cmd] refs)
// into a Go string expression (a concatenation of literals, variables,
// and function calls).
// isTCLRegexPattern reports whether a Go-quoted expected value is a TCL regex
// pattern (e.g. the `"/B-TREE/"` or `"~/SCAN/"` forms used by do_eqp_test).
func isTCLRegexPattern(goQuoted string) bool {
	s := goQuoted
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, "~\"") {
		return true
	}
	if len(s) >= 2 && s[0] == '/' && s[len(s)-1] == '/' {
		return true
	}
	return false
}

// regexPatternNegated reports whether a TCL regex-pattern expected value uses
// the "~/.../" form, meaning the pattern must NOT match (SQLite's do_test
// "~" prefix inverts the regex comparison).
func regexPatternNegated(goQuoted string) bool {
	s := goQuoted
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "~/") || strings.HasPrefix(s, "~\"")
}

// regexPatternExpr converts a TCL regex-pattern expected value (a Go-quoted
// string like `"/B-TREE/"` or `"~/SCAN/"`) into a Go regex pattern string
// literal. The `~/.../` prefix means a regex; `/.../` is treated as a regex
// too for EXPLAIN-plan comparisons.
func regexPatternExpr(goQuoted string) string {
	s := goQuoted
	// expectedExpr is a Go string literal (e.g. "\"/.../\"" from
	// goStringLiteral), so decode it with strconv.Unquote to get the real
	// pattern text; the escapes would otherwise be doubled on re-quoting.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if unquoted, err := strconv.Unquote(s); err == nil {
			s = unquoted
		} else {
			s = s[1 : len(s)-1]
		}
	}
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "~/") && strings.HasSuffix(s, "/") {
		s = s[2 : len(s)-1]
	} else if len(s) >= 2 && s[0] == '/' && s[len(s)-1] == '/' {
		s = s[1 : len(s)-1]
	}
	// TCL regex uses \y for a word boundary; RE2 (Go) uses \b. Convert so
	// patterns like "SCAN t2\y" match in Go.
	s = strings.ReplaceAll(s, `\y`, `\b`)
	// do_select_tests uses `#` as a wildcard for any integer (including a
	// leading minus) in result patterns: `#,#` matches "4,5", "-12,7".
	// The TCL framework substitutes this before regex matching.
	s = strings.ReplaceAll(s, "#", `-?[0-9]+`)
	// Result patterns are written with alignment spaces; flatten() joins
	// cells with single spaces, so collapse runs of spaces to one.
	s = strings.Join(strings.Fields(s), " ")
	return fmt.Sprintf("%q", s)
}

// isSingleBraceGroup reports whether text consists of exactly one top-level
// {...} group spanning the whole string. TCL renders a one-element list whose
// element contains list-special characters (space, comma, brace, ...) with a
// single brace layer: 'Mass in B Minor, BWV 232' renders as
// {Mass in B Minor, BWV 232}. A multi-element list keeps its braces as
// separators ('{a b} c'), which must not be stripped.
func isSingleBraceGroup(text string) bool {
	if len(text) < 2 || text[0] != '{' || text[len(text)-1] != '}' {
		return false
	}
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i == len(text)-1
			}
		}
	}
	return false
}
