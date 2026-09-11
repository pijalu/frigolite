package parse

import "strings"

// updateDeleteLimitError implements the prepare-time half of SQLite's
// SQLITE_ENABLE_UPDATE_DELETE_LIMIT support (delete.c:201, update.c:212):
// a DELETE or UPDATE statement carrying a top-level ORDER BY without a
// top-level LIMIT is rejected with "ORDER BY without LIMIT on DELETE" /
// "on UPDATE". The scan is lexical: strings, bracket-quoted identifiers,
// comments, parenthesised sub-expressions and the WITH header are skipped so
// only the statement's own ORDER/LIMIT tokens count. Returns "" when the
// statement is fine or is not a DELETE/UPDATE.
func updateDeleteLimitError(input string) string {
	for _, stmt := range splitRawStatements(input) {
		head := stmt
		// Strip a WITH ... header (CTEs may contain subqueries with their
		// own ORDER BY).
		u := strings.ToUpper(strings.TrimSpace(head))
		if strings.HasPrefix(u, "WITH") {
			if idx := topLevelKeyword(head, []string{"DELETE", "UPDATE"}); idx >= 0 {
				head = head[idx:]
				u = strings.ToUpper(strings.TrimSpace(head))
			} else {
				continue
			}
		}
		kind := ""
		if strings.HasPrefix(u, "DELETE") {
			kind = "DELETE"
		} else if strings.HasPrefix(u, "UPDATE") {
			kind = "UPDATE"
		} else {
			continue
		}
		// Is there a top-level ORDER BY after the statement's main body, and
		// does a top-level LIMIT follow it?
		orderIdx := topLevelOrderBy(head)
		if orderIdx < 0 {
			continue
		}
		if topLevelLimitAfter(head, orderIdx) {
			continue
		}
		return "ORDER BY without LIMIT on " + kind
	}
	return ""
}

// splitRawStatements splits on semicolons outside strings/comments.
func splitRawStatements(input string) []string {
	var out []string
	var cur strings.Builder
	inS, inD, inBracket, inLine, inBlock := false, false, false, false, false
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				cur.WriteByte(c)
			}
		case inBlock:
			if c == '*' && i+1 < len(input) && input[i+1] == '/' {
				inBlock = false
				i++
			}
		case !inS && !inD && !inBracket && c == '-' && i+1 < len(input) && input[i+1] == '-':
			inLine = true
		case !inS && !inD && !inBracket && c == '/' && i+1 < len(input) && input[i+1] == '*':
			inBlock = true
			i++
		case inS:
			if c == '\'' {
				inS = false
			}
		case inD:
			if c == '"' {
				inD = false
			}
		case inBracket:
			if c == ']' {
				inBracket = false
			}
		case c == '\'':
			inS = true
		case c == '"':
			inD = true
		case c == '[':
			inBracket = true
		case c == ';':
			if strings.TrimSpace(cur.String()) != "" {
				out = append(out, cur.String())
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

// topLevelKeyword finds the first keyword token at paren-depth 0.
func topLevelKeyword(s string, keywords []string) int {
	depth := 0
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case '\'', '"', '[', '-', '/':
			// skip strings/comments the same way; simplified: use skipLexeme
			j := skipLexeme(s, i)
			if j > i {
				i = j
				continue
			}
		case ' ', '\t', '\n', '\r', ',':
		default:
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
				j := i
				for j < len(s) && (s[j] == '_' || (s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= 'a' && s[j] <= 'z') || (s[j] >= '0' && s[j] <= '9')) {
					j++
				}
				word := s[i:j]
				for _, kw := range keywords {
					if depth == 0 && strings.EqualFold(word, kw) {
						return i
					}
				}
				i = j
				continue
			}
		}
		i++
	}
	return -1
}

// skipLexeme skips a string/bracket/comment starting at i; returns i when the
// byte does not start one.
func skipLexeme(s string, i int) int {
	switch s[i] {
	case '\'':
		for j := i + 1; j < len(s); j++ {
			if s[j] == '\'' {
				return j + 1
			}
		}
	case '"':
		for j := i + 1; j < len(s); j++ {
			if s[j] == '"' {
				return j + 1
			}
		}
	case '[':
		for j := i + 1; j < len(s); j++ {
			if s[j] == ']' {
				return j + 1
			}
		}
	case '-':
		if i+1 < len(s) && s[i+1] == '-' {
			for j := i + 2; j < len(s); j++ {
				if s[j] == '\n' {
					return j + 1
				}
			}
			return len(s)
		}
	case '/':
		if i+1 < len(s) && s[i+1] == '*' {
			for j := i + 2; j+1 < len(s); j++ {
				if s[j] == '*' && s[j+1] == '/' {
					return j + 2
				}
			}
			return len(s)
		}
	}
	return i
}

// topLevelOrderBy returns the byte offset of a depth-0 ORDER BY in s, or -1.
func topLevelOrderBy(s string) int {
	idx := topLevelKeyword(s, []string{"ORDER"})
	if idx < 0 {
		return -1
	}
	rest := s[idx+5:]
	fields := leadingWord(rest)
	if strings.EqualFold(fields, "BY") {
		return idx
	}
	// ORDER not followed by BY: keep scanning for a real ORDER BY
	sub := topLevelOrderBy(rest)
	if sub < 0 {
		return -1
	}
	return idx + 5 + sub
}

// topLevelLimitAfter reports whether a depth-0 LIMIT token appears in s at or
// after offset from.
func topLevelLimitAfter(s string, from int) bool {
	rest := s[from:]
	for {
		idx := topLevelKeyword(rest, []string{"LIMIT"})
		if idx < 0 {
			return false
		}
		// A LIMIT inside a parenthesised sub-expression would have been
		// skipped only if depth tracked it; topLevelKeyword already ignores
		// depth>0 tokens, so this is top-level.
		return true
	}
}

// leadingWord returns the first alphabetic word of s (for BY checks).
func leadingWord(s string) string {
	s = strings.TrimLeft(s, " \t\n\r")
	i := 0
	for i < len(s) && ((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
		i++
	}
	return s[:i]
}
