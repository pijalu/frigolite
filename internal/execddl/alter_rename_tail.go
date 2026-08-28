// ALTER TABLE RENAME operations: table rename, column rename, and the
// per-schema rewrites (views, triggers, indexes, foreign keys, sequences).
package execddl

import (
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
)

func (e *DDLExecutor) renameUpdateRelatedEntriesInSchema(schemaMgr *schema.Manager, oldName, newName, quotedNew string, ctx *RenameContext) {
	entries, err := schemaMgr.GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		// Auto-generated indexes (sqlite_autoindex_*) store no SQL; they must
		// still be processed when their table is renamed (the autoindex name
		// follows the table: auth3-3.0 TempTable→DoNotRead renames
		// sqlite_autoindex_TempTable_1 to sqlite_autoindex_DoNotRead_1).
		if entry.SQL == "" && !strings.HasPrefix(entry.Name, "sqlite_autoindex_") {
			continue
		}
		if !entryReferencesTable(entry, oldName) {
			continue
		}
		if !e.entryRenameAllowed(schemaMgr, entry, oldName, newName) {
			continue
		}
		if e.entrySQLRenamed(schemaMgr, entry, oldName, newName, quotedNew, ctx) {
			delete(e.ctx.ColCache(), entry.Name)
			e.ctx.DeleteTcCacheTable(entry.Name)
		}
	}
}

// entryReferencesTable reports whether an entry's SQL or TblName references the
// old table name. TblName is compared against the unqualified old name (SQLite
// stores tbl_name without a schema prefix, alterlegacy-11.x).
func entryReferencesTable(entry *schema.Entry, oldName string) bool {
	baseOld := baseTableName(oldName)
	return strings.Contains(entry.SQL, oldName) ||
		strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(oldName)) ||
		strings.EqualFold(entry.TblName, baseOld)
}

// baseTableName strips a schema prefix ("aux.t1" → "t1").
func baseTableName(name string) string {
	if dotIdx := strings.LastIndex(name, "."); dotIdx >= 0 {
		return name[dotIdx+1:]
	}
	return name
}

// entryRenameAllowed applies per-type rename rules (TblName updates, legacy
// mode skips) and reports whether the entry's SQL should be rewritten.
func (e *DDLExecutor) entryRenameAllowed(schemaMgr *schema.Manager, entry *schema.Entry, oldName, newName string) bool {
	switch entry.Type {
	case schema.TypeView:
		return !e.ctx.LegacyAlterTable()
	case schema.TypeTrigger:
		if strings.EqualFold(entry.TblName, baseTableName(oldName)) {
			entry.TblName = newName
			_ = schemaMgr.RemoveEntryOfType(entry.Name, entry.Type)
			_ = schemaMgr.AddEntry(entry)
		}
		return !e.ctx.LegacyAlterTable()
	case schema.TypeIndex:
		if strings.EqualFold(entry.TblName, baseTableName(oldName)) {
			entry.TblName = newName
		}
		// An auto-generated index (sqlite_autoindex_<table>_N) is renamed to
		// sqlite_autoindex_<newtable>_N when its table is renamed, matching
		// SQLite (auth3-3.0: ALTER TABLE TempTable RENAME TO DoNotRead makes
		// sqlite_autoindex_DoNotRead_1).
		if strings.HasPrefix(entry.Name, "sqlite_autoindex_") {
			if idx := strings.LastIndex(entry.Name, "_"); idx > len("sqlite_autoindex_") {
				prefix := entry.Name[len("sqlite_autoindex_"):idx]
				if strings.EqualFold(prefix, oldName) {
					newIndexName := "sqlite_autoindex_" + newName + entry.Name[idx:]
					// Re-add the autoindex under the new name with its tbl_name
					// updated to the renamed table. RemoveEntryOfType + AddEntry
					// preserves the entry's type, root page and SQL; the rowid
					// may shift (acceptable for autoindexes).
					if err := schemaMgr.RemoveEntryOfType(entry.Name, entry.Type); err == nil {
						entry.Name = newIndexName
						entry.TblName = newName
						if err := schemaMgr.AddEntry(entry); err != nil {
							return !e.ctx.LegacyAlterTable()
						}
					}
				}
			}
		}
		return !e.ctx.LegacyAlterTable()
	case schema.TypeTable:
		return !(e.ctx.LegacyAlterTable() && !e.ctx.ForeignKeys())
	default:
		return false
	}
}

// entrySQLRenamed rewrites one entry's SQL (token-level plus string-regex
// complement, with a string-only fallback) and persists the change.
func (e *DDLExecutor) entrySQLRenamed(schemaMgr *schema.Manager, entry *schema.Entry, oldName, newName, quotedNew string, ctx *RenameContext) bool {
	ranges, rErr := FindRenameTokens(entry.SQL, ctx)
	if rErr == nil && len(ranges) > 0 {
		newSQL := ApplyRenames(entry.SQL, ranges, quotedNew)
		if newSQL != entry.SQL && newSQL != "" {
			newSQL = replaceTableNameInSQL(newSQL, oldName, newName)
			if newSQL != entry.SQL {
				saveRenamedEntry(schemaMgr, entry, newSQL)
				return true
			}
		}
	}
	newSQL := replaceTableNameInSQL(entry.SQL, oldName, newName)
	if newSQL != entry.SQL && newSQL != "" {
		saveRenamedEntry(schemaMgr, entry, newSQL)
		return true
	}
	return false
}

func saveRenamedEntry(schemaMgr *schema.Manager, entry *schema.Entry, newSQL string) {
	entry.SQL = newSQL
	_ = schemaMgr.RemoveEntryOfType(entry.Name, entry.Type)
	_ = schemaMgr.AddEntry(entry)
}

// replaceTableNameInSQL replaces occurrences of oldName with the quoted new
// name in SQL text, using word-boundary matching; empty-IN operands and
// references to other schemas' tables are left untouched.
func replaceTableNameInSQL(sql, oldName, newName string) string {
	// Quote newName for SQL only when it needs it; a name containing a
	// double quote is stored doubled inside the identifier (SQLite stores
	// the literal characters and prints them via %w with embedded quotes
	// DOUBLED, e.g. ALTER t5 RENAME TO 'raisara "one"'''). The regexp below
	// therefore matches the SQL-text form first; otherwise fall back to the
	// doubled-quote form used by rtreesql-produced identifiers.
	quotedNew := sqlQuoteIdentifier(newName)
	quotedOld := regexp.QuoteMeta(sqlQuoteIdentifier(oldName))
	re := regexp.MustCompile(`(?i)` + quotedOld)
	if re.MatchString(sql) {
		return re.ReplaceAllString(sql, quotedNew)
	}
	quotedOldBare := regexp.QuoteMeta(oldName)
	re2 := regexp.MustCompile(`(?i)"` + quotedOldBare + `"`)
	if re2.MatchString(sql) {
		return re2.ReplaceAllString(sql, quotedNew)
	}
	emptyInSpans := emptyINOperandSpans(sql)
	re = regexp.MustCompile(`\b` + regexp.QuoteMeta(oldName) + `\b`)
	idxs := re.FindAllStringIndex(sql, -1)
	if len(idxs) == 0 {
		return sql
	}
	var b strings.Builder
	last := 0
	changed := false
	for _, idx := range idxs {
		if tableNameInEmptyIN(sql, idx[0], emptyInSpans) {
			continue
		}
		if otherSchemaQualifiedRef(sql, idx[0]) {
			continue
		}
		b.WriteString(sql[last:idx[0]])
		b.WriteString(quotedNew)
		last = idx[1]
		changed = true
	}
	if !changed {
		return sql
	}
	b.WriteString(sql[last:])
	return b.String()
}

func tableNameInEmptyIN(sql string, pos int, emptyInSpans [][2]int) bool {
	for _, sp := range emptyInSpans {
		if pos >= sp[0] && pos < sp[1] {
			return true
		}
	}
	return false
}

// otherSchemaQualifiedRef reports whether a match is the tail of a
// schema-qualified name whose qualifier is not "main" (SQLite keeps
// "aux.t2"/"temp.t2" references untouched when renaming "main.t2").
func otherSchemaQualifiedRef(sql string, pos int) bool {
	if pos <= 0 {
		return false
	}
	p := pos - 1
	for p >= 0 && (sql[p] == ' ' || sql[p] == '\t') {
		p--
	}
	if p < 0 || sql[p] != '.' {
		return false
	}
	q := p - 1
	for q >= 0 && isIdentByte(sql[q]) {
		q--
	}
	qual := strings.ToUpper(sql[q+1 : p])
	return qual != "MAIN"
}

// quoteFixSQLWithColumns rewrites double-quoted tokens that are not valid
// identifiers into single-quoted string literals, preserving genuine
// double-quoted identifiers (optionally restricted to the given column set).
func quoteFixSQLWithColumns(sqlStr string, colSet map[string]bool) string {
	var b strings.Builder
	b.Grow(len(sqlStr))
	for i := 0; i < len(sqlStr); i++ {
		if sqlStr[i] != '"' {
			b.WriteByte(sqlStr[i])
			continue
		}
		end := quotedTokenEnd(sqlStr, i)
		if end >= len(sqlStr) {
			b.WriteString(sqlStr[i:])
			break
		}
		token, next := quoteFixToken(sqlStr, i, end, colSet)
		b.WriteString(token)
		i = next
	}
	return b.String()
}

// quotedTokenEnd finds the closing double quote starting at i, treating "" as
// an escaped quote.
func quotedTokenEnd(sqlStr string, i int) int {
	end := i + 1
	for end < len(sqlStr) {
		if sqlStr[end] == '"' {
			if end+1 < len(sqlStr) && sqlStr[end+1] == '"' {
				end += 2
				continue
			}
			break
		}
		end++
	}
	return end
}

// quoteFixToken renders the replacement for a double-quoted token at
// [i, end]: kept as an identifier when valid, rewritten as a string literal
// otherwise (with a space separator before an adjacent token).
func quoteFixToken(sqlStr string, i, end int, colSet map[string]bool) (string, int) {
	content := sqlStr[i+1 : end]
	content = strings.ReplaceAll(content, `""`, `"`)
	if isSQLIdentifier(content) && (colSet == nil || colSet[strings.ToUpper(content)]) {
		return sqlStr[i : end+1], end
	}
	var b strings.Builder
	b.WriteByte('\'')
	b.WriteString(strings.ReplaceAll(content, "'", "''"))
	b.WriteByte('\'')
	if end+1 < len(sqlStr) && (sqlStr[end+1] == '\'' || sqlStr[end+1] == '"') {
		b.WriteByte(' ')
	}
	return b.String(), end
}

// sqlQuoteIdentifier renders a name as a double-quoted SQL identifier with
// embedded quotes doubled (SQLite %w semantics).
func sqlQuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
