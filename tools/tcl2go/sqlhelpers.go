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
func cteMainVerbIsQuery(stmt string) bool {
	rest := strings.TrimSpace(stmt[len("WITH"):])
	// Skip RECURSIVE.
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "RECURSIVE"))
	// Skip the CTE name and column list.
	// Find the AS (...) of the first CTE, then the next keyword after it.
	depth := 0
	inParen := false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
			inParen = true
		case ')':
			depth--
		}
		if depth == 0 && inParen {
			// End of the first CTE's parenthesized SELECT; the next token is
			// the main verb.
			tail := strings.TrimSpace(rest[i+1:])
			// Strip a following comma+CTE (multiple CTEs).
			upper := strings.ToUpper(tail)
			if strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "VALUES") {
				return true
			}
			if strings.HasPrefix(upper, "INSERT") || strings.HasPrefix(upper, "UPDATE") || strings.HasPrefix(upper, "DELETE") {
				return false
			}
			// A comma continues another CTE definition; keep scanning.
			if strings.HasPrefix(upper, ",") {
				rest = tail[1:]
				depth = 0
				inParen = false
				continue
			}
			// Any other keyword after the CTE is the main verb; only SELECT/
			// VALUES produce rows.
			return false
		}
	}
	return false
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
