package sql

import "strings"

// ParseIndexColumnCollations extracts per-column COLLATE names from a CREATE
// INDEX column list ("CREATE INDEX i ON t(a, b COLLATE rtrim)" -> ["", "rtrim"]).
// Key expressions may contain commas (Substr(a,-2)) and COLLATE suffixes, so
// the column list is split with balanced parentheses and quote awareness.
func ParseIndexColumnCollations(sqlStr string) []string {
	colText := indexColumnList(sqlStr)
	if colText == "" {
		return nil
	}
	parts := splitIndexKeyParts(colText)
	colls := make([]string, len(parts))
	for i, part := range parts {
		colls[i] = indexKeyCollation(part)
	}
	return colls
}

// indexKeyCollation returns one key's explicit COLLATE name ("" when none),
// ignoring a trailing ASC/DESC sort directive.
func indexKeyCollation(part string) string {
	upper := strings.ToUpper(part)
	idx := strings.LastIndex(upper, " COLLATE ")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(part[idx+len(" COLLATE "):])
	ru := strings.ToUpper(rest)
	if di := strings.Index(ru, " DESC"); di >= 0 {
		return strings.TrimSpace(rest[:di])
	}
	if ai := strings.Index(ru, " ASC"); ai >= 0 {
		return strings.TrimSpace(rest[:ai])
	}
	return rest
}

// indexColumnList returns the parenthesized key list of a CREATE INDEX,
// honoring nested parentheses and quoted strings.
func indexColumnList(sqlStr string) string {
	upper := strings.ToUpper(sqlStr)
	on := strings.Index(upper, " ON ")
	if on < 0 {
		return ""
	}
	start := strings.IndexByte(sqlStr[on+4:], '(')
	if start < 0 {
		return ""
	}
	start += on + 4
	depth, quote := 0, byte(0)
	for i := start; i < len(sqlStr); i++ {
		c := scanIndexByte(sqlStr, &i, &depth, &quote)
		if c == ')' && depth == 0 {
			return sqlStr[start+1 : i]
		}
	}
	return ""
}

// splitIndexKeyParts splits a CREATE INDEX key list on top-level commas.
func splitIndexKeyParts(colText string) []string {
	var parts []string
	depth, quote, start := 0, byte(0), 0
	for i := 0; i < len(colText); i++ {
		c := scanIndexByte(colText, &i, &depth, &quote)
		if c == ',' && depth == 0 && quote == 0 {
			parts = append(parts, strings.TrimSpace(colText[start:i]))
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(colText[start:]))
}

// scanIndexByte advances one CREATE INDEX key-list byte, tracking paren depth
// and SQL quoting. Doubled quote escapes consume both bytes.
func scanIndexByte(text string, i *int, depth *int, quote *byte) byte {
	c := text[*i]
	if *quote != 0 {
		if c != *quote {
			return c
		}
		if *i+1 < len(text) && text[*i+1] == c {
			*i++
			return c
		}
		*quote = 0
		return c
	}
	switch c {
	case '\'', '"', '`':
		*quote = c
	case '[':
		*quote = ']'
	case '(':
		*depth++
	case ')':
		if *depth > 0 {
			*depth--
		}
	}
	return c
}
