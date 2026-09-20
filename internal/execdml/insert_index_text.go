// This file holds the DDL-text parsers DML needs for index maintenance:
// extracting an index key's column list, collations and ORDER direction from
// CREATE INDEX text, and a minimal WHERE-expression parser for partial
// index predicates.
package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

func indexColumnListText(sqlText string) string {
	upper := strings.ToUpper(sqlText)
	onIdx := strings.Index(upper, " ON ")
	if onIdx < 0 {
		return ""
	}
	parenStart := strings.Index(sqlText[onIdx+4:], "(")
	if parenStart < 0 {
		return ""
	}
	parenStart += onIdx + 4
	depth := 0
	for i := parenStart; i < len(sqlText); i++ {
		switch sqlText[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return sqlText[parenStart+1 : i]
			}
		}
	}
	return ""
}

// splitIndexCols splits a CREATE INDEX column-list text on top-level commas,
// keeping commas inside parentheses (function calls like substr(b,2,4)) as
// part of the element.

// splitIndexCols splits a CREATE INDEX column-list text on top-level commas,
// keeping commas inside parentheses (function calls like substr(b,2,4)) as
// part of the element.
func splitIndexCols(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// uniqueIndexColumns returns the UNIQUE indexes defined on the given table
// (cached per table name).

// parseIndexKeyCollations extracts the explicit COLLATE per index key from a
// CREATE INDEX key column-list text ("" for keys without COLLATE).
func parseIndexKeyCollations(colText string) []string {
	parts := splitIndexCols(colText)
	colls := make([]string, len(parts))
	for i, part := range parts {
		upper := strings.ToUpper(part)
		if idx := strings.Index(upper, " COLLATE "); idx >= 0 {
			colls[i] = collationNameToken(part[idx+len(" COLLATE "):])
		}
	}
	return colls
}

// collationNameToken extracts a COLLATE clause's collation name: SQLite's
// grammar takes a single identifier (expr.c "COLLATE id"), so a trailing
// sort-order keyword ("binary ASC") or any following text is not part of the
// name. A quoted name ('my coll') is taken up to its closing quote.
func collationNameToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if q := s[0]; q == '\'' || q == '"' || q == '`' || q == '[' {
		closer := map[byte]byte{'\'': '\'', '"': '"', '`': '`', '[': ']'}[q]
		if end := strings.IndexByte(s[1:], closer); end >= 0 {
			return s[1 : 1+end]
		}
		return strings.Trim(s, "'\"`]")
	}
	if idx := strings.IndexAny(s, " \t\n"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSuffix(strings.TrimSuffix(s, ")"), ",")
}

// parseIndexKeyCols parses a CREATE INDEX key column-list into stripped key
// expressions (plain names or expression text), removing COLLATE/ASC/DESC
// suffixes where they are not part of an expression.
func parseIndexKeyCols(colText string) []string {
	var cols []string
	for _, part := range splitIndexCols(colText) {
		name := strings.TrimSpace(part)
		upper := strings.ToUpper(name)
		// Strip COLLATE / ASC / DESC suffixes. For a plain column key the
		// collation comes from the table definition, so it is dropped;
		// for an expression key the explicit COLLATE is part of the
		// expression and must be kept (indexKeyValue evaluates it).
		if strings.ContainsAny(name, "()") {
			// expression key: keep COLLATE, strip only ASC/DESC
			if idx := strings.Index(upper, " DESC"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			} else if idx := strings.Index(upper, " ASC"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			}
		} else {
			if idx := strings.Index(upper, " COLLATE"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			} else if idx := strings.Index(upper, " DESC"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			} else if idx := strings.Index(upper, " ASC"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			}
		}
		if name != "" {
			cols = append(cols, name)
		}
	}
	return cols
}

// allTableIndexes returns every index defined on the given table (unique and
// non-unique alike), with their key expressions, partial predicates, and root
// pages. This drives index maintenance on INSERT.

// indexDef describes any (unique or non-unique) index for index maintenance.
type indexDef struct {
	Name     string
	Cols     []string
	Where    string // partial-index predicate ("" for full indexes)
	RootPage uint32
	Ctx      *DatabaseContext
	SQL      string // stored CREATE INDEX statement (key collation resolution)
}

// maintainIndexesOnInsert writes the new row's entries into every index on
// the table. Partial-index predicates and expression keys are evaluated in a
// pure context, so a non-deterministic date/time function (e.g. date('now'))
// in an index expression raises SQLite's "non-deterministic use of %s() in an
// index" error, matching OP_PureFunc semantics.

// parseWhereExpr parses a standalone expression string into a sql.Expr.
func parseWhereExpr(exprSQL string) sql.Expr {
	stmts, perr := parse.ParseSQL("SELECT " + exprSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok && len(sel.Columns) > 0 {
		return sel.Columns[0].Expr
	}
	return nil
}

// updateIndexRootPage persists a root page change after an index b-tree split.

// updateIndexRootPage persists a root page change after an index b-tree split.
