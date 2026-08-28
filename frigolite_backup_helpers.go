package frigolite

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// bareTableName returns the table name for a qualified reference
// (schema.tablename). The engine's INSERT rejects a quoted table after a
// schema prefix ("temp.\"t1\"" → "no such table"), so simple identifiers
// are emitted bare.
func bareTableName(name string) string {
	return name
}

// qualifyCreateObjectSQL rewrites a stored CREATE VIEW/INDEX/TRIGGER DDL to
// create the object in the destination schema (e.g. "CREATE VIEW v1(...)" →
// "CREATE VIEW temp.v1(...)") for non-main destination schemas. kind is the
// schema.Entry type ("view", "index", "trigger").
func qualifyCreateObjectSQL(sql, qual, kind string) string {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	prefix := "CREATE " + strings.ToUpper(kind) + " "
	if !strings.HasPrefix(upper, prefix) {
		return sql
	}
	trimmed := strings.TrimSpace(sql)
	lead := trimmed[len(prefix):]
	lead = lead[:len(lead)-len(strings.TrimLeft(lead, " \t\n"))]
	rest := strings.TrimLeft(trimmed[len(prefix):], " \t\n")
	// Optional IF NOT EXISTS.
	ifIfNot := ""
	if strings.HasPrefix(strings.ToUpper(rest), "IF NOT EXISTS") {
		ifIfNot = rest[:len("IF NOT EXISTS")]
		rest = strings.TrimLeft(rest[len("IF NOT EXISTS"):], " \t\n")
	}
	name, after := splitTableName(rest)
	if name == "" {
		return sql
	}
	return "CREATE " + strings.ToUpper(kind) + " " + lead + ifIfNot + qual + name + after
}

// quoteIdent wraps a SQL identifier in double quotes, doubling embedded
// quotes (SQLite identifier quoting).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// schemaQualifier returns the schema prefix ("schema.") for a non-main schema
// name, or "" for main (object names are unqualified in main). Schema names
// (temp, aux, ...) are bare identifiers; quoting them ("temp".t1) breaks the
// engine's INSERT/DML schema resolution, which only accepts the unquoted form.
func schemaQualifier(schemaName string) string {
	upper := strings.ToUpper(schemaName)
	if upper == "" || upper == "MAIN" {
		return ""
	}
	return schemaName + "."
}

// isSystemSchemaTable reports whether a schema table is an internal SQLite
// table that must not be copied by a backup. sqlite_schema and sqlite_sequence
// are engine-managed; the sqlite_statN tables are ANALYZE statistics whose
// CREATE TABLE DDL is reserved (the engine refuses "CREATE TABLE
// sqlite_stat1") and whose rows are excluded from the tester.tcl dbcksum
// (name NOT LIKE 'sqlite_%'), so a logical backup skips them.
func isSystemSchemaTable(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "SQLITE_SCHEMA", "SQLITE_MASTER", "SQLITE_SEQUENCE",
		"SQLITE_STAT1", "SQLITE_STAT2", "SQLITE_STAT3", "SQLITE_STAT4":
		return true
	}
	return false
}

// qualifyCreateTableSQL rewrites the schema-qualified table name of a stored
// CREATE TABLE DDL (e.g. "CREATE TABLE t1(...)" → "CREATE TABLE \"aux\".t1(...)")
// so the object is created in the destination schema. It handles the optional
// "CREATE TABLE" / "CREATE TEMP TABLE" / "CREATE TABLE IF NOT EXISTS" prefixes
// and quoted/unquoted table names.
func qualifyCreateTableSQL(sql, qual string) string {
	trimmed := strings.TrimSpace(sql)
	upper := strings.ToUpper(trimmed)
	var prefix string
	var rest string
	for _, p := range []string{"CREATE TEMPORARY TABLE", "CREATE TEMP TABLE", "CREATE TABLE"} {
		if strings.HasPrefix(upper, p) {
			prefix = p
			rest = trimmed[len(p):]
			break
		}
	}
	if prefix == "" {
		// Not a CREATE TABLE; leave unchanged.
		return sql
	}
	// Preserve the whitespace between the prefix and the rest.
	lead := rest[:len(rest)-len(strings.TrimLeft(rest, " \t\n"))]
	trimmedRest := strings.TrimLeft(rest, " \t\n")
	// Optional IF NOT EXISTS.
	ifIfNot := ""
	if strings.HasPrefix(strings.ToUpper(trimmedRest), "IF NOT EXISTS") {
		ifIfNot = trimmedRest[:len("IF NOT EXISTS")]
		trimmedRest = strings.TrimLeft(trimmedRest[len("IF NOT EXISTS"):], " \t\n")
	}
	// Extract the table name (quoted or bare) up to the first space or '('.
	name, after := splitTableName(trimmedRest)
	if name == "" {
		return sql
	}
	// Insert the qualifier before the table name, preserving original spacing
	// (so the stored SQL stays byte-identical to the source's sqlite_master).
	return prefix + lead + ifIfNot + qual + name + after
}

// splitTableName splits a CREATE TABLE's name from the rest of the statement.
func splitTableName(rest string) (name, after string) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", ""
	}
	if rest[0] == '"' || rest[0] == '`' || rest[0] == '[' {
		quote := rest[0]
		closer := `"`
		if quote == '`' {
			closer = "`"
		} else if quote == '[' {
			closer = "]"
		}
		end := strings.Index(rest[1:], closer)
		if end < 0 {
			return rest, ""
		}
		return rest[:end+2], rest[end+2:]
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == ' ' || rest[i] == '(' || rest[i] == '\t' || rest[i] == '\n' {
			return rest[:i], rest[i:]
		}
	}
	return rest, ""
}

// sqlLiteral renders a Go value as a SQL literal for INSERT statements.
func sqlLiteral(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64)
	case string:
		return "'" + strings.ReplaceAll(val, "'", "''") + "'"
	case []byte:
		return "X'" + hex.EncodeToString(val) + "'"
	case bool:
		if val {
			return "1"
		}
		return "0"
	default:
		return "'" + strings.ReplaceAll(fmt.Sprint(v), "'", "''") + "'"
	}
}
