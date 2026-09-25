// Package main implements the tcl2go tool.
//
// This file holds the SQL text sanitizer used before keyword matching:
// SQL line/block comment stripping with single-quoted literal preservation.
package main

import "strings"

// scanSQLStringChar writes one literal character at i and reports whether the
// literal continues: a doubled quote is an escaped quote (consumed here, the
// literal continues); a lone quote ends it; any other character continues.
func scanSQLStringChar(sql string, i int, b *strings.Builder) (int, bool) {
	c := sql[i]
	b.WriteByte(c)
	if c != '\'' {
		return i, true
	}
	if i+1 < len(sql) && sql[i+1] == '\'' {
		b.WriteByte('\'')
		return i + 1, true
	}
	return i, false
}

// skipLineComment consumes a `-- ...` comment starting at i (sql[i] == '-'),
// returning the index of the comment terminator: the '\n' itself (which is
// preserved so line numbers still align) or len(sql).
func skipLineComment(sql string, i int, b *strings.Builder) int {
	i += 2
	for i < len(sql) && sql[i] != '\n' {
		i++
	}
	if i < len(sql) {
		b.WriteByte('\n')
	}
	return i
}

// skipBlockComment consumes a `/* ... */` comment starting at i
// (sql[i] == '/'), returning the index of the '/' of the terminator (the
// caller's loop increment consumes it). Unterminated comments run to EOF.
func skipBlockComment(sql string, i int, b *strings.Builder) int {
	i += 2
	for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
		i++
	}
	i++ // consume '*' of '*/'; the loop increment consumes '/'
	b.WriteByte(' ')
	return i
}

// stripSQLComments removes SQL line comments (`-- ...` to end of line) and
// block comments (`/* ... */`) from sql so keyword matching in
// unsupportedSQL does not misfire on words inside comments. Single-quoted
// string literals are preserved verbatim (doubled-quote escape respected) so a literal
// containing '--' is not truncated.
func stripSQLComments(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	inStr := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if inStr {
			var cont bool
			i, cont = scanSQLStringChar(sql, i, &b)
			inStr = cont
			continue
		}
		switch {
		case c == '\'':
			inStr = true
			b.WriteByte(c)
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			i = skipLineComment(sql, i, &b)
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = skipBlockComment(sql, i, &b)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
