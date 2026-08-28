package execdml

import "strings"

// parseSchemaName splits a possibly schema-qualified name into its schema
// prefix and object name. Returns ("", name) when there is no prefix.
func parseSchemaName(name string) (schema string, object string) {
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		return name[:dotIdx], name[dotIdx+1:]
	}
	return "", name
}

// parseIndexColumns extracts indexed column names from a CREATE INDEX SQL.
func parseIndexColumns(sqlStr string) []string {
	upper := strings.ToUpper(sqlStr)
	start := strings.Index(upper, "(")
	if start < 0 {
		return nil
	}
	end := strings.LastIndex(upper, ")")
	if end < 0 || end <= start {
		return nil
	}
	colsStr := sqlStr[start+1 : end]
	var cols []string
	for _, c := range strings.Split(colsStr, ",") {
		col := strings.TrimSpace(c)
		if col == "" {
			continue
		}
		// Strip a COLLATE clause and ASC/DESC suffix so "c COLLATE nocase"
		// resolves to the column "c". The collation is captured separately by
		// parseIndexColumnCollations.
		if ci := strings.Index(strings.ToUpper(col), " COLLATE "); ci >= 0 {
			col = strings.TrimSpace(col[:ci])
		}
		cu := strings.ToUpper(col)
		if di := strings.Index(cu, " DESC"); di >= 0 {
			col = strings.TrimSpace(col[:di])
		} else if ai := strings.Index(cu, " ASC"); ai >= 0 {
			col = strings.TrimSpace(col[:ai])
		}
		cols = append(cols, col)
	}
	return cols
}

// constraintNameBefore extracts the CONSTRAINT name from a column-definition
// fragment, e.g. "b INTEGER NOT NULL CONSTRAINT 'b-check' CHECK(...)" returns
// "b-check". Returns "" when the fragment has no CONSTRAINT clause before
// position ci (the CHECK keyword offset).
func constraintNameBefore(part string, ci int) string {
	pUpper := strings.ToUpper(part)
	cIdx := strings.Index(pUpper, "CONSTRAINT")
	if cIdx < 0 || cIdx >= ci {
		return ""
	}
	rest := strings.TrimSpace(part[cIdx+len("CONSTRAINT"):])
	// The name is the next bare token (identifier or quoted string).
	return firstSQLToken(rest)
}

// firstSQLToken returns the first token of a SQL fragment: a quoted string or
// a bare identifier.
func firstSQLToken(rest string) string {
	if rest == "" {
		return ""
	}
	if rest[0] == '\'' || rest[0] == '"' || rest[0] == '`' {
		return quotedToken(rest)
	}
	return bareToken(rest)
}

// quotedToken returns the content between the leading quote and its match.
func quotedToken(rest string) string {
	quote := rest[0]
	for i := 1; i < len(rest); i++ {
		if rest[i] == quote {
			return rest[1:i]
		}
	}
	return ""
}

// bareToken returns the leading identifier characters of a fragment.
func bareToken(rest string) string {
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return rest[:i]
		}
	}
	return rest
}
