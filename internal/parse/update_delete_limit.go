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

// rawSplitState tracks the string/bracket/comment lexer state of the
// raw-statement splitter.
type rawSplitState struct {
	inS, inD, inBracket, inLine, inBlock bool
}

// splitRawStatements splits on semicolons outside strings/comments.
func splitRawStatements(input string) []string {
	var out []string
	var cur strings.Builder
	st := rawSplitState{}
	for i := 0; i < len(input); i++ {
		i = splitRawStep(&cur, &out, input, i, &st)
	}
	flushRawStatement(&cur, &out)
	return out
}

// splitRawStep consumes input[i] for the raw-statement splitter, updating the
// splitter state and returning the index to continue from (the caller's loop
// adds its own step).
func splitRawStep(cur *strings.Builder, out *[]string, input string, i int, st *rawSplitState) int {
	c := input[i]
	switch {
	case st.inLine:
		if c == '\n' {
			st.inLine = false
			cur.WriteByte(c)
		}
	case st.inBlock:
		if endsBlockComment(input, i) {
			st.inBlock = false
			return i + 1
		}
	case st.inS:
		if c == '\'' {
			st.inS = false
		}
	case st.inD:
		if c == '"' {
			st.inD = false
		}
	case st.inBracket:
		if c == ']' {
			st.inBracket = false
		}
	default:
		return splitRawPlainStep(cur, out, input, i, st)
	}
	return i
}

// splitRawPlainStep consumes input[i] while no string/bracket/comment state is
// active, detecting comment starts, quote/bracket openers, and the statement
// separator. Returns the index to continue from.
func splitRawPlainStep(cur *strings.Builder, out *[]string, input string, i int, st *rawSplitState) int {
	c := input[i]
	switch {
	case c == '-' && i+1 < len(input) && input[i+1] == '-':
		st.inLine = true
	case c == '/' && i+1 < len(input) && input[i+1] == '*':
		st.inBlock = true
		return i + 1
	case c == '\'':
		st.inS = true
	case c == '"':
		st.inD = true
	case c == '[':
		st.inBracket = true
	case c == ';':
		flushRawStatement(cur, out)
	default:
		cur.WriteByte(c)
	}
	return i
}

// endsBlockComment reports whether the /* comment being scanned ends at i
// (a "*/" terminator starts there).
func endsBlockComment(input string, i int) bool {
	return input[i] == '*' && i+1 < len(input) && input[i+1] == '/'
}

// flushRawStatement appends the pending statement when non-blank and resets
// the buffer.
func flushRawStatement(cur *strings.Builder, out *[]string) {
	if strings.TrimSpace(cur.String()) != "" {
		*out = append(*out, cur.String())
	}
	cur.Reset()
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
			if isIdentStart(c) {
				j := scanIdentifierEnd(s, i)
				if keywordMatches(s[i:j], depth, keywords) {
					return i
				}
				i = j
				continue
			}
		}
		i++
	}
	return -1
}

// keywordMatches reports whether word equals any of keywords at paren-depth 0.
func keywordMatches(word string, depth int, keywords []string) bool {
	if depth != 0 {
		return false
	}
	for _, kw := range keywords {
		if strings.EqualFold(word, kw) {
			return true
		}
	}
	return false
}

// skipLexeme skips a string/bracket/comment starting at i; returns i when the
// byte does not start one.
func skipLexeme(s string, i int) int {
	switch s[i] {
	case '\'':
		return scanQuoteEnd(s, i, '\'')
	case '"':
		return scanQuoteEnd(s, i, '"')
	case '[':
		return scanQuoteEnd(s, i, ']')
	case '-':
		return skipDashLexeme(s, i)
	case '/':
		return skipSlashLexeme(s, i)
	}
	return i
}

// scanQuoteEnd returns the index just after the closing quote/bracket that
// terminates the lexeme starting at i, or i when unterminated.
func scanQuoteEnd(s string, i int, close byte) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] == close {
			return j + 1
		}
	}
	return i
}

// skipDashLexeme returns the index just after the -- comment's terminating
// newline (or len(s) at EOF); i when s[i] does not start a -- comment.
func skipDashLexeme(s string, i int) int {
	if !(i+1 < len(s) && s[i+1] == '-') {
		return i
	}
	for j := i + 2; j < len(s); j++ {
		if s[j] == '\n' {
			return j + 1
		}
	}
	return len(s)
}

// skipSlashLexeme returns the index just after the /* comment's terminator
// (or len(s) at EOF); i when s[i] does not start a /* comment.
func skipSlashLexeme(s string, i int) int {
	if !(i+1 < len(s) && s[i+1] == '*') {
		return i
	}
	for j := i + 2; j+1 < len(s); j++ {
		if s[j] == '*' && s[j+1] == '/' {
			return j + 2
		}
	}
	return len(s)
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
	idx := topLevelKeyword(rest, []string{"LIMIT"})
	// A LIMIT inside a parenthesised sub-expression would have been
	// skipped only if depth tracked it; topLevelKeyword already ignores
	// depth>0 tokens, so this is top-level.
	return idx >= 0
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
