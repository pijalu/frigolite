// SPDX-License-Identifier: GPL-3.0-or-later
package tcl

import "strings"

// splitList parses a TCL list string into elements.
// TCL lists are whitespace-separated, with {} or "" grouping.
// Example: "a b {c d} e" → ["a", "b", "c d", "e"]
func splitList(s string) []string {
	var result []string
	pos := 0
	for pos < len(s) {
		pos = skipListWS(s, pos)
		if pos >= len(s) {
			break
		}

		var elem string
		switch s[pos] {
		case '{':
			elem, pos = readListBraced(s, pos)
		case '"':
			elem, pos = readListQuoted(s, pos)
		default:
			elem, pos = readListPlain(s, pos)
		}
		result = append(result, elem)
	}
	return result
}

// skipListWS advances past whitespace at the start of a list element.
func skipListWS(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
		pos++
	}
	return pos
}

// readListBraced reads a { ... } list element with nesting, returning the
// element text and the position after the closing brace.
func readListBraced(s string, pos int) (string, int) {
	// pos sits on the opening '{'; find its MATCHING closing '}'
	// (nested braces tracked; backslash escapes are transparent). The
	// element value excludes both delimiters.
	depth := 1
	start := pos + 1
	i := pos + 1
	for i < len(s) {
		if s[i] == '\\' {
			i += 2
			continue
		}
		if s[i] == '{' {
			depth++
		} else if s[i] == '}' {
			depth--
			if depth == 0 {
				break
			}
		}
		i++
	}
	return s[start:i], i + 1
}

// readListQuoted reads a " ... " list element, returning the element text and
// the position after the closing quote.
func readListQuoted(s string, pos int) (string, int) {
	start := pos + 1
	pos++
	for pos < len(s) && s[pos] != '"' {
		if s[pos] == '\\' {
			pos += 2
			continue
		}
		pos++
	}
	if pos < len(s) {
		pos++ // skip closing "
	}
	return s[start:pos], pos
}

// readListPlain reads a plain list element until whitespace, tracking brace
// nesting so unbalanced braces stay within the element.
func readListPlain(s string, pos int) (string, int) {
	start := pos
	braceDepth := 0
	for pos < len(s) {
		c := s[pos]
		if c == '\\' {
			pos += 2
			continue
		}
		if c == '{' {
			braceDepth++
		} else if c == '}' {
			if braceDepth > 0 {
				braceDepth--
			}
		} else if (c == ' ' || c == '\t' || c == '\n' || c == '\r') && braceDepth == 0 {
			break
		}
		pos++
	}
	return s[start:pos], pos
}

// tclList converts a Go string slice to a TCL list string.
// Elements containing spaces are wrapped in braces.
// Example: ["a", "b c", "d"] → "a {b c} d"
func tclList(items []string) string {
	parts := make([]string, len(items))
	for idx, item := range items {
		if needsBracing(item) {
			// Escape any unbalanced braces
			parts[idx] = "{" + item + "}"
		} else {
			parts[idx] = item
		}
	}
	return strings.Join(parts, " ")
}

// needsBracing returns true if the string needs {} wrapping in a TCL list.
func needsBracing(s string) bool {
	if s == "" {
		return true
	}
	for _, c := range s {
		switch c {
		case ' ', '\t', '\n', '\r', '{', '}', '"', ';':
			return true
		}
	}
	return false
}

// tclLLength returns the number of elements in a TCL list.
func tclLLength(s string) int {
	return len(splitList(s))
}

// tclLIndex returns element at index (0-based). Returns "" if out of range.
func tclLIndex(s string, idx int) string {
	items := splitList(s)
	if idx < 0 || idx >= len(items) {
		return ""
	}
	return items[idx]
}

// tclLRange returns elements from start to end (inclusive).
// Negative end means "until the last element".
func tclLRange(s string, start, end int) string {
	items := splitList(s)
	if start < 0 {
		start = 0
	}
	if end < 0 || end >= len(items) {
		end = len(items) - 1
	}
	if start > end || start >= len(items) {
		return ""
	}
	return tclList(items[start : end+1])
}
