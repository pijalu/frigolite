// ALTER TABLE constraint SQL text rewriting helpers: parsing and
// reconstructing CREATE TABLE SQL for ADD/DROP COLUMN and ADD/DROP CONSTRAINT.
package execddl

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// skipConstraintKeyword advances i past a constraint type keyword (CHECK,
// UNIQUE, ...) in s and returns the index after it plus the keyword text.
func skipConstraintKeyword(s string, i int) (int, string) {
	kwStart := i
	for i < len(s) && s[i] != ' ' && s[i] != '(' {
		i++
	}
	return i, strings.ToUpper(strings.TrimSpace(s[kwStart:i]))
}

// skipFKConstraintTail advances i past the rest of a FOREIGN KEY clause: the
// KEY keyword, column list, REFERENCES keyword, target table name, and optional
// column list.
func skipFKConstraintTail(s string, i int) int {
	// Skip the KEY token (part of FOREIGN KEY).
	for i < len(s) && s[i] != ' ' && s[i] != '(' {
		i++
	}
	i = skipSQLWhitespaceAndComments(s, i)
	// Skip the parenthesized column list.
	i = skipParenGroup(s, i)
	i = skipSQLWhitespaceAndComments(s, i)
	if strings.HasPrefix(strings.ToUpper(s[i:]), "REFERENCES") {
		// Skip the REFERENCES keyword, target table name, and optional
		// parenthesized column list.
		i += len("REFERENCES")
		i = skipSQLWhitespaceAndComments(s, i)
		for i < len(s) && s[i] != ' ' && s[i] != '(' {
			i++
		}
		i = skipParenGroup(s, i)
	}
	return i
}

// skipColumnConstraintTail advances i past a column-level constraint type
// keyword and its parenthesized expression (and for REFERENCES, the target
// table and optional column list), returning the index of the next clause.
func skipColumnConstraintTail(tail string, i int) int {
	i = skipSpaces(tail, i)
	kwStart := i
	for i < len(tail) && tail[i] != ' ' && tail[i] != '(' {
		i++
	}
	kwUpper := strings.ToUpper(strings.TrimSpace(tail[kwStart:i]))
	// For REFERENCES <table>[(cols)] the target table name follows the keyword;
	// skip it and its optional parenthesized column list so "CONSTRAINT fk
	// REFERENCES p1(a)" removes the whole reference (altercons3-4.x).
	if kwUpper == "REFERENCES" {
		i = skipReferenceTarget(tail, i)
	}
	i = skipSpaces(tail, i)
	if i < len(tail) && tail[i] == '(' {
		return skipParenGroup(tail, i)
	}
	return i
}

// skipReferenceTarget advances i past a REFERENCES <table>[(cols)] clause.
func skipReferenceTarget(tail string, i int) int {
	i = skipSpaces(tail, i)
	for i < len(tail) && tail[i] != ' ' && tail[i] != '(' &&
		tail[i] != '\t' && tail[i] != '\n' && tail[i] != '\r' {
		i++
	}
	return i
}

// skipSpaces advances i past spaces and tabs in s.
func skipSpaces(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// filterDroppedConstraintParts removes the definition parts containing the
// named table-level or column-level constraint from a CREATE TABLE part list.
func filterDroppedConstraintParts(parts []string, constraintName, upperName, quotedName, upperQuotedName string) []string {
	var kept []string
	for _, rawPart := range parts {
		part := strings.TrimSpace(rawPart)
		if part == "" {
			continue
		}
		upperPart := strings.ToUpper(part)
		// The CONSTRAINT keyword may be preceded by a comment (e.g.
		// "/* world */ CONSTRAINT abc ..."); skip leading comments before
		// detecting the constraint clause.
		constraintStart := skipSQLWhitespaceAndComments(part, 0)
		constraintUpper := strings.ToUpper(part[constraintStart:])
		if strings.HasPrefix(constraintUpper, "CONSTRAINT ") {
			rest := strings.TrimSpace(part[constraintStart+11:]) // after "CONSTRAINT "
			restUpper := strings.ToUpper(rest)
			if strings.HasPrefix(restUpper, upperName) || strings.HasPrefix(restUpper, upperQuotedName) {
				// Remove the matched CONSTRAINT clause but keep any CONSTRAINT
				// clauses that follow in the same part (e.g. "CONSTRAINT abc
				// CONSTRAINT one ...").
				removed := removeLeadingConstraintClause(rest, constraintName, quotedName, upperName, upperQuotedName)
				if strings.TrimSpace(removed) != "" {
					kept = append(kept, removed)
				}
				continue
			}
		}
		// Check for column-level constraint: colName CONSTRAINT name ...
		if newPart, ok := removeColumnLevelConstraint(part, upperPart, upperName, upperQuotedName, quotedName, constraintName); ok {
			part = newPart
		}
		kept = append(kept, part)
	}
	return kept
}

// writeConstraintClause writes a table constraint (CONSTRAINT name TYPE ...)
// to buf.
func writeConstraintClause(buf *strings.Builder, tc *sql.TableConstraint) {
	if tc.Name != "" {
		buf.WriteString("CONSTRAINT ")
		buf.WriteString(tc.Name)
		buf.WriteString(" ")
	}
	switch tc.Type {
	case sql.ConstraintCheck:
		buf.WriteString("CHECK (")
		if tc.Expr != nil {
			buf.WriteString(sql.ExprString(tc.Expr))
		}
		buf.WriteString(")")
	default:
		if tc.Type != "" {
			buf.WriteString(string(tc.Type))
		}
	}
}

// findConstraintInsertPoint returns the byte position before which an added
// column should be inserted: before the first table-level constraint keyword
// (PRIMARY/UNIQUE/CHECK/FOREIGN/CONSTRAINT) at top level, or the closing paren
// when there are none. SQLite stores an ADD COLUMN before the table-level
// constraints (e.g. "CREATE TABLE t(a, b, d DEFAULT 'x', PRIMARY KEY(a,b))").
func findConstraintInsertPoint(origSQL string, parenStart, parenEnd int) int {
	insertAt := parenEnd
	depth := 0
	for i := parenStart + 1; i < parenEnd; i++ {
		switch origSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth != 0 {
				continue
			}
			rest := strings.ToUpper(strings.TrimSpace(origSQL[i+1 : parenEnd]))
			if strings.HasPrefix(rest, "PRIMARY") || strings.HasPrefix(rest, "UNIQUE") ||
				strings.HasPrefix(rest, "CHECK") || strings.HasPrefix(rest, "FOREIGN") ||
				strings.HasPrefix(rest, "CONSTRAINT") {
				// Insert at the comma position so the new column lands between
				// the last column and the constraint.
				return i
			}
		}
	}
	return insertAt
}

// findOuterParens returns the byte positions of the first '(' in s and its
// matching ')' (or -1 for parenEnd when no closing paren exists).
func findOuterParens(s string) (int, int) {
	start := strings.Index(s, "(")
	if start < 0 {
		return -1, -1
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return start, i
			}
		}
	}
	return start, -1
}

// skipParenGroup advances i past the parenthesized group beginning at s[i]
// (which must be '(') and returns the index just after the closing ')'. If no
// closing paren is found, returns len(s).
func skipParenGroup(s string, i int) int {
	if i >= len(s) || s[i] != '(' {
		return i
	}
	depth := 0
	for i < len(s) {
		if s[i] == '(' {
			depth++
		} else if s[i] == ')' {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return len(s)
}
