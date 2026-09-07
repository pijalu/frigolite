// Value rendering for recovered SQL: mirrors quote() + escape_crlf
// (ext/recover/sqlite3recover.c recoverEscapeCrlf) and the CREATE-statement
// column extraction used to build INSERT column lists.

package recover

import (
	"strconv"
	"strings"
)

// renderValue renders one decoded record value as recovered SQL text:
// integers bare, reals in SQLite's %.15g form, text single-quoted with
// embedded newlines routed through replace(..., char(10)), blobs as
// X'hex', NULL for nil.
func renderValue(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return renderReal(x)
	case []byte:
		return "X'" + strings.ToUpper(hexString(x)) + "'"
	case string:
		return escapeCrlf(quoteText(x))
	default:
		return quoteText(toStringValue(x))
	}
}

// renderReal formats a real the way SQLite's quote() does: %.15g with a
// trailing ".0" for integral values.
func renderReal(f float64) string {
	s := strconv.FormatFloat(f, 'g', 15, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}

// quoteText renders a text value as a single-quoted SQL string with embedded
// quotes doubled.
func quoteText(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// escapeCrlf wraps text containing literal newline/CR characters in
// replace(..., char(10)) / char(13) expressions so the recovered SQL holds
// no raw control characters (recover.c recoverEscapeCrlf). The escape
// sequence chosen for each character is "\\n"/"\\r" unless the text already
// contains that two-character form, in which case "\\012"/"\\015" is used.
func escapeCrlf(quoted string) string {
	if quoted == "" || quoted[0] != '\'' {
		return quoted
	}
	useNL := strings.Contains(quoted, "\n")
	useCR := strings.Contains(quoted, "\r")
	if !useNL && !useCR {
		return quoted
	}
	escNL, escCR := "\\n", "\\r"
	if strings.Contains(quoted, escNL) {
		escNL = "\\012"
	}
	if strings.Contains(quoted, escCR) {
		escCR = "\\015"
	}
	var b strings.Builder
	if useNL && useCR {
		b.WriteString("replace(replace(")
	} else {
		b.WriteString("replace(")
	}
	for i := 0; i < len(quoted); i++ {
		switch {
		case useNL && quoted[i] == '\n':
			b.WriteString(escNL)
		case useCR && quoted[i] == '\r':
			b.WriteString(escCR)
		default:
			b.WriteByte(quoted[i])
		}
	}
	if useNL {
		b.WriteString(",'")
		b.WriteString(escNL)
		b.WriteString("', char(10))")
	}
	if useCR {
		b.WriteString(",'")
		b.WriteString(escCR)
		b.WriteString("', char(13))")
	}
	return b.String()
}

// parseTableColumns extracts a CREATE TABLE's column names and flags
// (INTEGER PRIMARY KEY rowid alias, AUTOINCREMENT, WITHOUT ROWID) from the
// stored schema SQL.
func parseTableColumns(sql string, e *tableEntry) {
	e.ipkIndex = -1
	up := strings.ToUpper(sql)
	e.autoInc = strings.Contains(up, "AUTOINCREMENT")
	e.withoutRowid = strings.Contains(up, "WITHOUT ROWID")
	open := strings.Index(sql, "(")
	if open < 0 {
		return
	}
	// Close paren matching the open one.
	depth, end := 0, -1
	inStr := byte(0)
	for i := open; i < len(sql); i++ {
		c := sql[i]
		if inStr != 0 {
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '\'':
			inStr = '\''
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		end = len(sql)
	}
	fields := splitTopLevel(sql[open+1 : end])
	constraintStarters := map[string]bool{
		"PRIMARY": true, "UNIQUE": true, "CHECK": true,
		"FOREIGN": true, "CONSTRAINT": true,
	}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		name := firstIdentifier(f)
		if name == "" {
			continue
		}
		if constraintStarters[strings.ToUpper(name)] {
			continue
		}
		e.columns = append(e.columns, name)
		if e.ipkIndex < 0 && isIntegerPrimaryKey(f) {
			e.ipkIndex = len(e.columns) - 1
		}
	}
}

// splitTopLevel splits a column-definition list at top-level commas.
func splitTopLevel(s string) []string {
	var out []string
	depth, inStr := 0, byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '\'':
			inStr = '\''
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// firstIdentifier extracts the leading identifier of a column definition,
// honoring quoting (double quotes, backticks, brackets).
func firstIdentifier(f string) string {
	f = strings.TrimSpace(f)
	if f == "" {
		return ""
	}
	switch {
	case f[0] == '"':
		if end := strings.Index(f[1:], "\""); end >= 0 {
			return f[1 : 1+end]
		}
	case f[0] == '`':
		if end := strings.Index(f[1:], "`"); end >= 0 {
			return f[1 : 1+end]
		}
	case f[0] == '[':
		if end := strings.Index(f[1:], "]"); end >= 0 {
			return f[1 : 1+end]
		}
	}
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '(' {
			return f[:i]
		}
	}
	return f
}

// isIntegerPrimaryKey reports whether a column definition declares
// "INTEGER PRIMARY KEY" (the rowid alias).
func isIntegerPrimaryKey(f string) bool {
	up := " " + strings.ToUpper(strings.Join(strings.Fields(f), " ")) + " "
	return strings.Contains(up, " INTEGER PRIMARY KEY ")
}

// textOf renders a decoded record value as Go text (schema fields).
func textOf(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return ""
}

// intOf renders a decoded record value as Go int (schema rootpage).
func intOf(v interface{}) int {
	switch x := v.(type) {
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// toStringValue renders a non-string scalar as text.
func toStringValue(v interface{}) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return ""
}

// hexString renders bytes as lowercase hex (uppercase applied by callers
// that need the X'...' literal form).
func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}
