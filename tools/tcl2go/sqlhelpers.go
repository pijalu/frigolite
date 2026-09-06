// Package main implements the tcl2go tool.
//
// This file contains small SQL statement helpers.
package main

import "strings"

// (imports managed by goimports)

// ---- SQL Helpers ----

func lastStatementSQL(sql string) string {
	stmts := strings.Split(sql, ";")
	for i := len(stmts) - 1; i >= 0; i-- {
		s := strings.TrimSpace(stmts[i])
		if s != "" {
			return s
		}
	}
	return ""
}

// splitSQLStatements splits a multi-statement SQL body on ';', dropping
// empty statements. It is intentionally simple (no string/quote awareness);
// statement bodies containing a quoted ';' are extremely rare in the TCL
// test corpus and the existing lastStatementSQL has the same limitation.
func splitSQLStatements(sql string) []string {
	var out []string
	for _, st := range strings.Split(sql, ";") {
		if t := strings.TrimSpace(st); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func isQueryStmt(stmt string) bool {
	stmt = strings.TrimSpace(stmt)
	// Skip leading SQL line comments (-- ...) that precede the statement.
	for strings.HasPrefix(stmt, "--") {
		if nl := strings.IndexByte(stmt, '\n'); nl >= 0 {
			stmt = strings.TrimSpace(stmt[nl+1:])
		} else {
			stmt = ""
		}
	}
	if len(stmt) < 6 {
		return false
	}
	upper := strings.ToUpper(stmt[:min(len(stmt), 10)])
	if strings.HasPrefix(upper, "SELECT") ||
		strings.HasPrefix(upper, "PRAGMA") ||
		strings.HasPrefix(upper, "EXPLAIN") ||
		strings.HasPrefix(upper, "VALUES") {
		return true
	}
	// WITH starts a CTE; it is a query only when the main verb (after the CTE
	// definition) is SELECT/VALUES, not INSERT/UPDATE/DELETE.
	if strings.HasPrefix(upper, "WITH") {
		return cteMainVerbIsQuery(stmt)
	}
	// INSERT/UPDATE/DELETE with RETURNING should use db.Query
	return strings.Contains(strings.ToUpper(stmt), "RETURNING")
}

// cteMainVerbIsQuery reports whether a WITH statement's main verb (the first
// keyword after the CTE definitions) is a query (SELECT/VALUES) rather than
// DML (INSERT/UPDATE/DELETE). A WITH...INSERT produces no result rows.
//
// The scan is structured: for each CTE, skip the name, an optional balanced
// column list (`c(x,y)` — a paren group in the NAME must not be mistaken for
// the AS body, stmtrand 1.x), the AS keyword and its balanced body, then
// either another `,`-separated CTE follows or the remainder is the main
// statement.
func cteMainVerbIsQuery(stmt string) bool {
	rest := strings.TrimSpace(stmt[len("WITH"):])
	// Skip RECURSIVE.
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "RECURSIVE"))
	for {
		// Skip the CTE name (up to whitespace or '(').
		nameEnd := len(rest)
		for i := 0; i < len(rest); i++ {
			if rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r' || rest[i] == '(' {
				nameEnd = i
				break
			}
		}
		if nameEnd == 0 {
			return false
		}
		rest = strings.TrimSpace(rest[nameEnd:])
		// Skip an optional balanced column list.
		if strings.HasPrefix(rest, "(") {
			after, ok := skipBalancedParen(rest)
			if !ok {
				return false
			}
			rest = strings.TrimSpace(after)
		}
		// Skip AS and its balanced body.
		if len(rest) < 2 || !strings.EqualFold(rest[:2], "AS") {
			return false
		}
		rest = strings.TrimSpace(rest[2:])
		if !strings.HasPrefix(rest, "(") {
			return false
		}
		after, ok := skipBalancedParen(rest)
		if !ok {
			return false
		}
		rest = strings.TrimSpace(after)
		// Either another CTE (comma) or the main statement.
		if strings.HasPrefix(rest, ",") {
			rest = strings.TrimSpace(rest[1:])
			continue
		}
		upper := strings.ToUpper(rest)
		return strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "VALUES")
	}
}

// skipBalancedParen returns the text after the ')' matching the leading '('
// of s (string literals and comments are not expected in a CTE column list
// or its AS body separator position; nested parens are tracked).
func skipBalancedParen(s string) (string, bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[i+1:], true
			}
		}
	}
	return "", false
}

// rowProducingQuery reports whether a statement is a top-level query that
// produces result rows (SELECT / WITH / VALUES / EXPLAIN). Unlike
// isQueryStmt, it excludes PRAGMA: PRAGMA setters (auto_vacuum=OFF,
// page_size, cache_size) and PRAGMA calls inside a VACUUM-heavy body are
// side effects the engine can run, not row-producing queries whose result
// order would be VACUUM-dependent.
func rowProducingQuery(stmt string) bool {
	stmt = strings.TrimSpace(stmt)
	for strings.HasPrefix(stmt, "--") {
		if nl := strings.IndexByte(stmt, '\n'); nl >= 0 {
			stmt = strings.TrimSpace(stmt[nl+1:])
		} else {
			stmt = ""
		}
	}
	if len(stmt) < 6 {
		return false
	}
	upper := strings.ToUpper(stmt[:min(len(stmt), 10)])
	return strings.HasPrefix(upper, "SELECT") ||
		strings.HasPrefix(upper, "WITH") ||
		strings.HasPrefix(upper, "VALUES") ||
		strings.HasPrefix(upper, "EXPLAIN")
}

// bodyHasRowProducingQuery reports whether a multi-statement SQL body
// contains a row-producing query (SELECT / WITH / VALUES / EXPLAIN). Used to
// decide whether a VACUUM-heavy body can be split: bodies with such queries
// may have VACUUM-dependent result order and are kept fully skipped.
func bodyHasRowProducingQuery(sql string) bool {
	for _, st := range splitSQLStatements(sql) {
		if rowProducingQuery(st) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
