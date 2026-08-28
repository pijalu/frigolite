// SPDX-License-Identifier: GPL-3.0-or-later
//
// Copyright (C) 2026 Pierre Poissinger
//
// Package parse implements an LALR(1) SQL parser using go-lemon generated
// parse tables from SQLite's grammar. This replaces the hand-written
// recursive-descent parser in internal/sql/parser.go.

package parse

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

func hasSavepointStatements(input string) bool {
	for _, stmtText := range splitSQLStatements(input) {
		if _, ok := parseSavepointStatement(stmtText); ok {
			return true
		}
	}
	return false
}

// splitStringLiteral returns the index after a quoted string starting at i
// (input[i] is the quote), honoring backslash escapes.
func splitStringLiteral(input string, i int) int {
	q := input[i]
	i++
	for i < len(input) && input[i] != q {
		if input[i] == '\\' && i+1 < len(input) {
			i += 2
		} else {
			i++
		}
	}
	return i + 1
}

// stmtScanner tracks paren depth while advancing through a statement.
type stmtScanner struct {
	input string
	depth int
}

// advance moves past the construct starting at i (string, comment, paren, or
// single character), updating depth for parentheses. Returns the index after
// the construct.
func (s *stmtScanner) advance(i int) int {
	c := s.input[i]
	switch c {
	case '(':
		s.depth++
		return i + 1
	case ')':
		s.depth--
		return i + 1
	case '\'', '"', '`':
		return splitStringLiteral(s.input, i)
	case '-':
		if i+1 < len(s.input) && s.input[i+1] == '-' {
			return skipLineComment(s.input, i)
		}
	case '/':
		if i+1 < len(s.input) && s.input[i+1] == '*' {
			return skipBlockComment(s.input, i)
		}
	}
	return i + 1
}

// splitSQLStatements splits input into individual statements by top-level
// semicolons (honoring strings and comments).
func splitSQLStatements(input string) []string {
	var parts []string
	start := 0
	s := &stmtScanner{input: input}
	i := 0
	for i < len(input) {
		if input[i] == ';' && s.depth == 0 {
			parts = append(parts, strings.TrimSpace(input[start:i]))
			start = i + 1
			i++
			continue
		}
		i = s.advance(i)
	}
	if start < len(input) {
		parts = append(parts, strings.TrimSpace(input[start:]))
	}
	return parts
}

// collapseEmptyStatements removes empty statements from a SQL string: a
// semicolon that is immediately preceded (modulo whitespace and comments) by
// another semicolon or the start of input. SQLite treats an empty statement as
// a no-op, and the LALR tables mis-parse `stmt;; stmt` by duplicating the
// trailing statement, so the empty ones are dropped before parsing.
//
// Only genuinely empty segments are dropped; the text of every non-empty
// statement is preserved VERBATIM (byte-for-byte, including interior
// whitespace). Earlier split-and-rejoin implementations mangled statements
// with internal top-level semicolons — a CREATE TRIGGER body
// ("BEGIN SELECT 1,2,3; END") was split on its interior ";" and rejoined with
// trimmed parts, losing the space before END in the stored trigger SQL
// (temptrigger-5.2).
func collapseEmptyStatements(input string) string {
	var b strings.Builder
	s := &stmtScanner{input: input}
	start := 0
	i := 0
	dropped := false
	for i < len(input) {
		if input[i] == ';' && s.depth == 0 {
			seg := input[start:i]
			if strings.TrimSpace(seg) == "" {
				// Empty statement: drop it and its semicolon.
				dropped = true
			} else {
				b.WriteString(seg)
				b.WriteByte(';')
			}
			start = i + 1
			i++
			continue
		}
		i = s.advance(i)
	}
	if start < len(input) {
		seg := input[start:]
		if strings.TrimSpace(seg) == "" {
			dropped = true
		} else {
			b.WriteString(seg)
		}
	}
	if !dropped {
		return input
	}
	return b.String()
}

// rewriteTriggerWhenParenthesize works around a LALR table conflict: a CREATE
// TRIGGER whose WHEN clause ends a top-level `==` (or `=`) comparison followed
// by another statement whose first token sequence includes `=` (an UPDATE SET)
// is mis-parsed ("near '='") because the pre-generated tables lack SQLite's
// scanpt lookahead markers in the trigger_cmd rules. Parenthesizing the WHEN
// expression is semantically a no-op and lets the tables reduce the comparison
// before the statement boundary. The rewrite is applied per CREATE TRIGGER
// statement, scanning only the top-level WHEN..BEGIN span so comparison
// operators inside trigger bodies are untouched.
func rewriteTriggerWhenParenthesize(input string) string {
	if !strings.Contains(strings.ToUpper(input), "WHEN") {
		return input
	}
	var b strings.Builder
	s := &stmtScanner{input: input}
	i := 0
	start := 0
	for i < len(input) {
		if input[i] == ';' && s.depth == 0 {
			stmt := input[start:i]
			b.WriteString(rewriteOneTriggerWhen(stmt))
			b.WriteByte(';')
			start = i + 1
			i++
			continue
		}
		i = s.advance(i)
	}
	if start < len(input) {
		b.WriteString(rewriteOneTriggerWhen(input[start:]))
	}
	return b.String()
}

// rewriteOneTriggerWhen parenthesizes the top-level WHEN expression of a single
// CREATE TRIGGER statement when it contains a top-level == or = comparison.
func rewriteOneTriggerWhen(stmt string) string {
	if !isCreateTriggerStmt(stmt) {
		return stmt
	}
	upper := strings.ToUpper(stmt)
	whenIdx := strings.Index(upper, "WHEN")
	if whenIdx < 0 {
		return stmt
	}
	// The WHEN keyword must be at paren depth 0 (top level of the trigger
	// declaration, not inside the body or a string).
	depth := 0
	kwIdx := -1
	for i := 0; i < len(stmt); {
		c := stmt[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(stmt) && ((stmt[j] >= 'A' && stmt[j] <= 'Z') || (stmt[j] >= 'a' && stmt[j] <= 'z') || stmt[j] == '_' || (stmt[j] >= '0' && stmt[j] <= '9')) {
				j++
			}
			word := strings.ToUpper(stmt[i:j])
			if depth == 0 && word == "WHEN" {
				kwIdx = i
				break
			}
			i = j
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case '\'', '"', '`':
			i = splitStringLiteral(stmt, i)
			continue
		case '-':
			if i+1 < len(stmt) && stmt[i+1] == '-' {
				i = skipLineComment(stmt, i)
				continue
			}
		case '/':
			if i+1 < len(stmt) && stmt[i+1] == '*' {
				i = skipBlockComment(stmt, i)
				continue
			}
		}
		i++
	}
	if kwIdx < 0 {
		return stmt
	}
	// Find the top-level BEGIN that starts the trigger body after the WHEN
	// expression.
	exprStart := kwIdx + len("WHEN")
	depth = 0
	beginIdx := -1
	for i := exprStart; i < len(stmt); {
		c := stmt[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(stmt) && ((stmt[j] >= 'A' && stmt[j] <= 'Z') || (stmt[j] >= 'a' && stmt[j] <= 'z') || stmt[j] == '_' || (stmt[j] >= '0' && stmt[j] <= '9')) {
				j++
			}
			if depth == 0 && strings.ToUpper(stmt[i:j]) == "BEGIN" {
				beginIdx = i
				break
			}
			i = j
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case '\'', '"', '`':
			i = splitStringLiteral(stmt, i)
			continue
		case '-':
			if i+1 < len(stmt) && stmt[i+1] == '-' {
				i = skipLineComment(stmt, i)
				continue
			}
		case '/':
			if i+1 < len(stmt) && stmt[i+1] == '*' {
				i = skipBlockComment(stmt, i)
				continue
			}
		}
		i++
	}
	if beginIdx < 0 {
		return stmt
	}
	expr := stmt[exprStart:beginIdx]
	if !hasTopLevelEq(expr) {
		return stmt
	}
	// Wrap the whole WHEN expression (trimmed) in parentheses.
	trimmed := strings.TrimSpace(expr)
	if strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, ")") {
		return stmt
	}
	return stmt[:exprStart] + " (" + trimmed + ") " + stmt[beginIdx:]
}

// isCreateTriggerStmt reports whether a statement begins with CREATE TRIGGER
// (or CREATE TEMP TRIGGER), case-insensitively.
func isCreateTriggerStmt(stmt string) bool {
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	if !strings.HasPrefix(upper, "CREATE") {
		return false
	}
	rest := strings.TrimSpace(upper[len("CREATE"):])
	if strings.HasPrefix(rest, "TEMP") || strings.HasPrefix(rest, "TEMPORARY") {
		rest = strings.TrimSpace(rest[4:])
	}
	return strings.HasPrefix(rest, "TRIGGER")
}

// hasTopLevelEq reports whether the expression span contains a == or = operator
// at parenthesis depth 0 (outside strings, comments, and nested parens).
func hasTopLevelEq(span string) bool {
	depth := 0
	for i := 0; i < len(span); {
		c := span[i]
		if c == '=' {
			if depth == 0 {
				// Skip a second = (the == operator is a single token).
				return true
			}
			i++
			continue
		}
		switch c {
		case '(':
			depth++
			i++
		case ')':
			depth--
			i++
		case '\'', '"', '`':
			i = splitStringLiteral(span, i)
		case '-':
			if i+1 < len(span) && span[i+1] == '-' {
				i = skipLineComment(span, i)
			} else {
				i++
			}
		case '/':
			if i+1 < len(span) && span[i+1] == '*' {
				i = skipBlockComment(span, i)
			} else {
				i++
			}
		default:
			i++
		}
	}
	return false
}

// savepointOp parses the leading SAVEPOINT / RELEASE / ROLLBACK TO keyword
// from a statement, returning the operation and the remaining text. ok=false
// when the text is not a savepoint statement.
func savepointOp(text string) (op, rest string, ok bool) {
	upper := strings.ToUpper(text)
	switch {
	case strings.HasPrefix(upper, "SAVEPOINT"):
		return "SAVEPOINT", strings.TrimSpace(text[len("SAVEPOINT"):]), true
	case strings.HasPrefix(upper, "RELEASE"):
		rest := strings.TrimSpace(text[len("RELEASE"):])
		// RELEASE [SAVEPOINT] name
		if strings.HasPrefix(strings.ToUpper(rest), "SAVEPOINT") {
			rest = strings.TrimSpace(rest[len("SAVEPOINT"):])
		}
		return "RELEASE", rest, true
	case strings.HasPrefix(upper, "ROLLBACK"):
		rest := strings.TrimSpace(text[len("ROLLBACK"):])
		// ROLLBACK [TRANSACTION] TO [SAVEPOINT] name
		if strings.HasPrefix(strings.ToUpper(rest), "TRANSACTION") {
			rest = strings.TrimSpace(rest[len("TRANSACTION"):])
		}
		if !strings.HasPrefix(strings.ToUpper(rest), "TO") {
			return "", "", false
		}
		rest = strings.TrimSpace(rest[2:])
		if strings.HasPrefix(strings.ToUpper(rest), "SAVEPOINT") {
			rest = strings.TrimSpace(rest[len("SAVEPOINT"):])
		}
		return "ROLLBACK", rest, true
	}
	return "", "", false
}

// isSavepointIdentChar reports whether r is a valid unquoted savepoint name
// character.
func isSavepointIdentChar(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// savepointName strips quoting and trailing junk from a savepoint name.
// A double/single-quoted name keeps its full content (savepoint names may
// contain spaces: RELEASE "including Ws").
func savepointName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) >= 2 && (name[0] == '"' || name[0] == '\'') && name[len(name)-1] == name[0] {
		return name[1 : len(name)-1]
	}
	for i, r := range name {
		if !isSavepointIdentChar(r) {
			return name[:i]
		}
	}
	return name
}

// parseSavepointStatement parses a SAVEPOINT / RELEASE / ROLLBACK TO statement
// into a *sql.SavepointStmt, returning ok=false when the text is not one.
func parseSavepointStatement(text string) (*sql.SavepointStmt, bool) {
	t := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ";"))
	if t == "" {
		return nil, false
	}
	op, rest, ok := savepointOp(t)
	if !ok || rest == "" {
		return nil, false
	}
	name := savepointName(rest)
	if name == "" {
		return nil, false
	}
	return &sql.SavepointStmt{Type: op, Name: name}, true
}
