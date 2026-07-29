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
		// Skip whitespace
		for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
			pos++
		}
		if pos >= len(s) {
			break
		}

		switch s[pos] {
		case '{':
			// Braced element — read until matching }
			depth := 1
			start := pos + 1
			pos++
			for pos < len(s) && depth > 0 {
				if s[pos] == '\\' {
					pos += 2
					continue
				}
				if s[pos] == '{' {
					depth++
				} else if s[pos] == '}' {
					depth--
				}
				if depth > 0 {
					pos++
				}
			}
			result = append(result, s[start:pos])
			if pos < len(s) {
				pos++ // skip closing }
			}

		case '"':
			// Quoted element
			start := pos + 1
			pos++
			for pos < len(s) && s[pos] != '"' {
				if s[pos] == '\\' {
					pos += 2
					continue
				}
				pos++
			}
			result = append(result, s[start:pos])
			if pos < len(s) {
				pos++ // skip closing "
			}

		default:
			// Plain element — read until whitespace
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
			result = append(result, s[start:pos])
		}
	}
	return result
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
