package execddl

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/rename"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

func (e *DDLExecutor) renameUpdateRelatedEntries(oldName, newName, targetSchema string) {
	quotedNew := `"` + newName + `"`
	ctx := &RenameContext{OldName: oldName, NewName: newName, QuotedNew: quotedNew, IsTable: true}
	for _, dbCtx := range e.ctx.Databases() {
		e.renameUpdateRelatedEntriesInSchema(dbCtx.Schema, dbCtx.Name, targetSchema, oldName, newName, quotedNew, ctx)
	}
}

// firstQualifiedRef returns the first "alias.column" reference in a view body
// whose alias matches the given name (used for the ambiguity error message).
func firstQualifiedRef(viewSQL, alias string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(alias) + `\.([a-zA-Z_][a-zA-Z0-9_]*)`)
	if m := re.FindStringSubmatch(viewSQL); len(m) > 1 {
		return m[1]
	}
	return ""
}

// findQualifiedTableRefs finds qualified column references to tableName
// in the given SQL string (e.g., "t1.col" or "t1.a").
// Returns list of matching qualified references found.
func findQualifiedTableRefs(sql, tableName string) []string {
	if sql == "" || tableName == "" {
		return nil
	}
	// Look for patterns like "tablename." followed by an identifier
	// Use word boundary matching to avoid partial matches
	re := regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9_])` + regexp.QuoteMeta(tableName) + `\.(?P<col>[a-zA-Z_][a-zA-Z0-9_]*)`)
	matches := re.FindAllStringSubmatch(sql, -1)
	var refs []string
	for _, m := range matches {
		if len(m) >= 3 {
			// m[0] is full match, m[1] is boundary char, m[2] is column name (named group)
			refs = append(refs, tableName+"."+m[2])
		}
	}
	return refs
}

// funcNameAt reports whether the identifier at byte range [start, end) in
// sqlStr is immediately followed by "(" (allowing whitespace), i.e. it is a
// function-call name rather than a column reference.
func funcNameAt(sqlStr string, start, end int) bool {
	p := end
	for p < len(sqlStr) && (sqlStr[p] == ' ' || sqlStr[p] == '\t' || sqlStr[p] == '\n' || sqlStr[p] == '\r') {
		p++
	}
	return p < len(sqlStr) && sqlStr[p] == '('
}

// validateAddColumnConstraints evaluates a newly added column's CHECK and
// NOT NULL constraints against the table's existing rows, matching SQLite's
// ALTER TABLE ADD COLUMN semantics:
//   - A NOT NULL column with a NULL default cannot be added when the table
//     already has rows (SQLite: "Cannot add a NOT NULL column with default
//     value NULL").
//   - A CHECK constraint is evaluated against every existing row; if any row
//     makes it false the add fails with "CHECK constraint failed".

// evalColumnExpr evaluates a column's DEFAULT expression (with no row context)
// to its concrete value.
func (e *DDLExecutor) evalColumnExpr(col sql.ColumnDef) (interface{}, error) {
	if col.Default == nil {
		return nil, nil
	}
	return e.ctx.EvalExpr(col.Default, nil)
}

// tableHasRows reports whether the table currently has at least one row.
func (e *DDLExecutor) tableHasRows(entry *schema.Entry) bool {
	tree := e.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return false
	}
	return true
}

// columnHasNull reports whether any existing row of the table has NULL in the
// named column. Used by ALTER TABLE ... ALTER COLUMN ... SET NOT NULL.

// sqlHasConstraintName reports whether a CREATE TABLE SQL contains a named
// table-level or column-level constraint. Used to reject DROP CONSTRAINT for
// a non-existent name.
func sqlHasConstraintName(origSQL, constraintName string) bool {
	if constraintName == "" {
		return false
	}
	tokens := ddlSQLTokens(origSQL)
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].quoted || !strings.EqualFold(tokens[i].text, "CONSTRAINT") {
			continue
		}
		name := strings.ReplaceAll(tokens[i+1].text, `""`, `"`)
		if strings.EqualFold(name, constraintName) {
			return true
		}
	}
	return false
}

// NULL keyword as a top-level constraint (not inside a DEFAULT string or
// CHECK expression). Used by rebuildCreateTableSQL to decide whether to
// reconstruct a column after ALTER COLUMN SET/DROP NOT NULL.
func origHasNotNull(orig string) bool {
	up := strings.ToUpper(orig)
	// Remove parenthesised groups (DEFAULT expressions, CHECK(...)) so a
	// "NOT NULL" inside them does not count as the column constraint.
	var cleaned strings.Builder
	depth := 0
	for i := 0; i < len(up); i++ {
		switch up[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				cleaned.WriteByte(up[i])
			}
		}
	}
	re := regexp.MustCompile(`\bNOT\s+NULL\b`)
	return re.MatchString(cleaned.String())
}

// isMalformedCreateTableSQL reports whether a CREATE TABLE statement is
// malformed (e.g. truncated by a writable_schema edit so the column list is
// unbalanced). SQLite reports "database disk image is malformed" when ALTER
// TABLE encounters such a schema.
func isMalformedCreateTableSQL(sqlStr string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sqlStr))
	if !strings.HasPrefix(upper, "CREATE TABLE") {
		return true
	}
	parenStart := strings.Index(sqlStr, "(")
	if parenStart < 0 {
		return true
	}
	depth := 0
	for i := parenStart; i < len(sqlStr); i++ {
		switch sqlStr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return true
			}
		}
	}
	return depth != 0
}

func (e *DDLExecutor) isVirtualTable(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	// Check if the table SQL contains "USING" which indicates a virtual table
	if strings.Contains(strings.ToUpper(entry.SQL), " USING ") {
		return true
	}
	// Check if the table's root page type is a virtual table (if available)
	return false
}

// getFTSModule finds and returns the FTS3Module for a given module name.
// Returns nil if the module is not found or is not an FTS module.
func (e *DDLExecutor) getFTSModule(moduleName string) *fts.FTS3Module {
	m, ok := e.ctx.VTables().Find(moduleName)
	if !ok {
		return nil
	}
	ftsMod, ok := m.(*fts.FTS3Module)
	if !ok {
		return nil
	}
	return ftsMod
}

// getFTSModuleForTable finds the FTS module that owns the given table.
func (e *DDLExecutor) getFTSModuleForTable(tableName string) *fts.FTS3Module {
	for _, modName := range []string{"fts3", "fts4", "fts5"} {
		ftsMod := e.getFTSModule(modName)
		if ftsMod != nil {
			if _, ok := ftsMod.GetTable(tableName); ok {
				return ftsMod
			}
		}
	}
	return nil
}

// indexColumnRefs extracts the column names referenced by a CREATE INDEX
// statement's column list (the parenthesized expressions after ON).
func indexColumnRefs(indexSQL string) []string {
	upper := strings.ToUpper(indexSQL)
	onIdx := strings.Index(upper, " ON ")
	if onIdx < 0 {
		return nil
	}
	tblStart := onIdx + 4
	tblEnd := tblStart
	for tblEnd < len(indexSQL) && indexSQL[tblEnd] != '(' {
		tblEnd++
	}
	if tblEnd >= len(indexSQL) {
		return nil
	}
	open := tblEnd + 1
	depth := 1
	i := open
	for i < len(indexSQL) && depth > 0 {
		if indexSQL[i] == '(' {
			depth++
		} else if indexSQL[i] == ')' {
			depth--
		}
		i++
	}
	if depth != 0 {
		return nil
	}
	inner := indexSQL[open : i-1]
	// Strip double-quoted tokens (DQS string literals like "c" in
	// "c"=b are not column references).
	inner = regexp.MustCompile(`"[^"]*"`).ReplaceAllString(inner, " ")
	return extractIdentifierTokens(inner)
}

// tableHasColumn reports whether the named table defines the given column.
func (e *DDLExecutor) tableHasColumn(tableName, colName string) bool {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return false
	}
	colDefs := e.ctx.ColCache()[tableName]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	}
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, colName) {
			return true
		}
	}
	return false
}

// checkIndexDependencies checks if any indexes reference the given column and returns
// an error if so. This prevents dropping columns that are used by indexes.
func (e *DDLExecutor) checkIndexDependencies(tableName, columnName string) *Result {
	entries, err := e.ctx.Schema().GetEntries(schema.TypeIndex)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		if refs, quoted := indexReferencesColumn(entry.SQL, columnName); refs {
			// SQLite re-parses each schema object with DQS OFF when validating
			// a DROP COLUMN (sqlite3_rename_test bNoDQS=1). A double-quoted
			// reference to the dropped column therefore reports the DQS-off
			// hint message; a bare or single-quoted (string-to-identifier)
			// reference reports the plain "no such column" message.
			if quoted {
				return &Result{Error: fmt.Errorf("error in index %s after drop column: no such column: \"%s\" - should this be a string literal in single-quotes?",
					entry.Name, columnName)}
			}
			return &Result{Error: fmt.Errorf("error in index %s after drop column: no such column: %s",
				entry.Name, columnName)}
		}
	}
	return nil
}

// isIdentByte reports whether b is a valid identifier character.
func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') || b == '_'
}

// quoteFixWithSchema rewrites non-column double-quoted tokens in a schema
// object's SQL to single-quoted string literals, resolving valid-identifier
// tokens against the referenced table's columns (SQLite's
// sqlite_rename_quotefix behavior: "a" stays an identifier when a is a
// column of the table, otherwise it becomes the string 'a').
func (e *DDLExecutor) quoteFixWithSchema(schemaName, sqlStr string) string {
	if sqlStr == "" {
		return ""
	}
	// Determine the main table name from the SQL (CREATE TABLE x, CREATE
	// INDEX ... ON x, CREATE TRIGGER ... ON x, SELECT ... FROM x).
	tableName := mainTableFromSchemaSQL(sqlStr)
	var colSet map[string]bool
	if tableName != "" {
		if entry, _, err := e.ctx.FindTable(tableName); err == nil {
			colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
			colSet = make(map[string]bool)
			for _, cd := range colDefs {
				colSet[strings.ToUpper(cd.Name)] = true
			}
		} else if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sqlStr)), "CREATE TABLE") {
			// The table is being created: its columns are in the statement
			// itself, so use them for identifier resolution.
			if cols := createTableColumnNames(sqlStr); len(cols) > 0 {
				colSet = make(map[string]bool)
				for _, c := range cols {
					colSet[strings.ToUpper(c)] = true
				}
			}
		}
	}
	return quoteFixSQLWithColumns(sqlStr, colSet)
}

// mainTableFromSchemaSQL extracts the primary table name referenced by a
// schema-object CREATE statement (the table being created / indexed /
// triggered on).
func mainTableFromSchemaSQL(sqlStr string) string {
	upper := strings.ToUpper(strings.TrimSpace(sqlStr))
	switch {
	case strings.HasPrefix(upper, "CREATE TABLE") || strings.HasPrefix(upper, "CREATE TEMP TABLE"):
		// Table name after CREATE TABLE (skip TEMP/TEMPORARY).
		re := regexp.MustCompile(`(?i)^CREATE\s+(?:TEMP|TEMPORARY\s+)?TABLE\s+([A-Za-z_][A-Za-z0-9_$]*)`)
		if m := re.FindStringSubmatch(upper); len(m) > 1 {
			return m[1]
		}
	case strings.HasPrefix(upper, "CREATE INDEX") || strings.HasPrefix(upper, "CREATE UNIQUE INDEX"):
		re := regexp.MustCompile(`(?i)ON\s+([A-Za-z_][A-Za-z0-9_$]*)`)
		if m := re.FindStringSubmatch(upper); len(m) > 1 {
			return m[1]
		}
	case strings.HasPrefix(upper, "CREATE TRIGGER") || strings.HasPrefix(upper, "CREATE TEMP TRIGGER"):
		re := regexp.MustCompile(`(?i)ON\s+([A-Za-z_][A-Za-z0-9_$]*)`)
		if m := re.FindStringSubmatch(upper); len(m) > 1 {
			return m[1]
		}
	case strings.HasPrefix(upper, "CREATE VIEW"):
		// View body references the table in its FROM clause.
		re := regexp.MustCompile(`(?i)\bFROM\s+([A-Za-z_][A-Za-z0-9_$]*)`)
		if m := re.FindStringSubmatch(upper); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// fnSQLiteRenameQuoteFix implements SQLite's internal sqlite_rename_quotefix
// function used by ALTER TABLE RENAME machinery: it rewrites double-quoted
// tokens that are NOT valid identifiers (e.g. "notacolumn!", "a;b") into
// single-quoted string literals, leaving genuine double-quoted identifiers
// ("a", "b") untouched. Single quotes inside converted strings are doubled.
// The first argument (schema name) is ignored — the function only rewrites
// the SQL text.

func renameColumnInTriggerSQL(source, oldName, newName string, renamePseudoColumns bool, forceQuoteOption ...bool) (string, error) {
	forceQuote := len(forceQuoteOption) > 0 && forceQuoteOption[0]
	ctx := &RenameContext{OldName: oldName, NewName: newName, QuotedNew: newName, ForceQuote: forceQuote}
	ranges, err := FindRenameTokens(source, ctx)
	if err != nil {
		return "", err
	}
	appendRange := func(token sql.Token) {
		for _, existing := range ranges {
			if existing.Start == token.Pos && existing.End == token.End {
				return
			}
		}
		ranges = append(ranges, RenameRange{Start: token.Pos, End: token.End})
	}
	tokenizer := sql.NewTokenizer(source)
	tokens := make([]sql.Token, 0, 32)
	for {
		token := tokenizer.Next()
		if token.Type == sql.TokenEOF || token.Type == sql.TokenError {
			break
		}
		tokens = append(tokens, token)
	}
	bodyStarted := false
	updateOfList := false
	updateKeyword := false
	for i, token := range tokens {
		if token.Type == sql.TokenKeyword && strings.EqualFold(token.Value, "BEGIN") {
			bodyStarted = true
		}
		if !bodyStarted {
			if strings.EqualFold(token.Value, "UPDATE") {
				updateKeyword = true
			} else if updateKeyword && strings.EqualFold(token.Value, "OF") {
				updateOfList = true
			} else if strings.EqualFold(token.Value, "ON") {
				updateOfList = false
			}
		}
		if !bodyStarted && renamePseudoColumns && updateOfList && token.Type == sql.TokenIdentifier && strings.EqualFold(token.Value, oldName) {
			appendRange(token)
		}
		if !bodyStarted && renamePseudoColumns && (token.Type == sql.TokenIdentifier || token.Type == sql.TokenKeyword) && strings.EqualFold(token.Value, oldName) && i >= 2 && tokens[i-1].Type == sql.TokenDot && (strings.EqualFold(tokens[i-2].Value, "new") || strings.EqualFold(tokens[i-2].Value, "old")) {
			appendRange(token)
		}
		if bodyStarted && (token.Type == sql.TokenIdentifier || token.Type == sql.TokenKeyword) && strings.EqualFold(token.Value, oldName) {
			if !renamePseudoColumns && i >= 2 && tokens[i-1].Type == sql.TokenDot && (strings.EqualFold(tokens[i-2].Value, "new") || strings.EqualFold(tokens[i-2].Value, "old")) {
				continue
			}
			alreadyAdded := false
			for _, r := range ranges {
				if r.Start == token.Pos && r.End == token.End {
					alreadyAdded = true
					break
				}
			}
			if !alreadyAdded {
				appendRange(token)
			}
			continue
		}
	}

	return rename.ApplyContextRenames(source, ranges, ctx), nil
}

func findForeignKeyColumnRanges(source, tableName, oldName string) []RenameRange {
	tokenizer := sql.NewTokenizer(source)
	var tokens []sql.Token
	for {
		token := tokenizer.Next()
		if token.Type == sql.TokenEOF || token.Type == sql.TokenError {
			break
		}
		tokens = append(tokens, token)
	}
	var ranges []RenameRange
	for i := 0; i+3 < len(tokens); i++ {
		if tokens[i].Type != sql.TokenKeyword || !strings.EqualFold(tokens[i].Value, "REFERENCES") || !strings.EqualFold(tokens[i+1].Value, tableName) {
			continue
		}
		if tokens[i+2].Type != sql.TokenLParen || !strings.EqualFold(tokens[i+3].Value, oldName) {
			continue
		}
		token := tokens[i+3]
		ranges = append(ranges, RenameRange{Start: token.Pos, End: token.End})
	}
	return ranges
}

func renameColumnInTableSQL(source, oldName, newName, tableName, objectName string, column int, quotedName string) (string, error) {
	ctx := &RenameContext{OldName: oldName, NewName: newName, QuotedNew: quotedName, TableName: tableName}
	ranges, err := FindRenameTokens(source, ctx)
	if err != nil {
		return "", err
	}
	statements, err := parse.ParseSQL(source)
	if err != nil || len(statements) != 1 {
		return "", err
	}
	create, ok := statements[0].(*sql.CreateTableStmt)
	if !ok {
		return "", nil
	}
	ranges = append(ranges, findForeignKeyColumnRanges(source, tableName, oldName)...)
	if !strings.EqualFold(create.Name, objectName) {
		return ApplyRenames(source, ranges, quotedName), nil
	}
	tokenizer := sql.NewTokenizer(source)
	tokens := make([]sql.Token, 0, 32)
	for {
		token := tokenizer.Next()
		if token.Type == sql.TokenEOF || token.Type == sql.TokenError {
			break
		}
		tokens = append(tokens, token)
	}
	depth, columnIndex, targetToken := 0, 0, -1
	inDefinitions := false
	for i, token := range tokens {
		if token.Type == sql.TokenLParen {
			depth++
			if depth == 1 {
				inDefinitions = true
				if i+1 < len(tokens) {
					targetToken = i + 1
				}
			}
			continue
		}
		if token.Type == sql.TokenRParen {
			depth--
			if depth == 0 {
				inDefinitions = false
			}
			continue
		}
		if inDefinitions && depth == 1 && token.Type == sql.TokenComma {
			columnIndex++
			if columnIndex == column && i+1 < len(tokens) {
				targetToken = i + 1
			}
		}
	}
	if targetToken < 0 || targetToken >= len(tokens) || !strings.EqualFold(tokens[targetToken].Value, oldName) {
		return "", nil
	}
	target := tokens[targetToken]
	ranges = append(ranges, RenameRange{Start: target.Pos, End: target.End})
	return ApplyRenames(source, ranges, quotedName), nil
}

func renameColumnInIndexSQL(source, oldName, newName, tableName string, forceQuote bool) (string, error) {
	ctx := &RenameContext{OldName: oldName, NewName: newName, QuotedNew: newName, ForceQuote: forceQuote, TableName: tableName}
	ranges, err := FindRenameTokens(source, ctx)
	if err != nil {
		return "", err
	}
	tokenizer := sql.NewTokenizer(source)
	tokens := make([]sql.Token, 0, 16)
	for {
		token := tokenizer.Next()
		if token.Type == sql.TokenEOF || token.Type == sql.TokenError {
			break
		}
		tokens = append(tokens, token)
	}
	depth := 1
	indexStart := -1
	for i, token := range tokens {
		if token.Type == sql.TokenKeyword && strings.EqualFold(token.Value, "ON") && i+1 < len(tokens) && strings.EqualFold(tokens[i+1].Value, tableName) {
			for j := i + 2; j < len(tokens); j++ {
				if tokens[j].Type == sql.TokenLParen {
					indexStart = j + 1
					break
				}
			}
			break
		}
	}
	if indexStart < 0 {
		return source, nil
	}
	for _, token := range tokens[indexStart:] {
		if depth <= 0 {
			break
		}
		if token.Type == sql.TokenLParen {
			depth++
			continue
		}
		if token.Type == sql.TokenRParen {
			depth--
			continue
		}
		if depth == 1 && (token.Type == sql.TokenIdentifier || token.Type == sql.TokenKeyword) && strings.EqualFold(token.Value, oldName) {
			ranges = append(ranges, RenameRange{Start: token.Pos, End: token.End})
		}
	}
	return rename.ApplyContextRenames(source, ranges, ctx), nil
}

// FnSQLiteRenameColumn rewrites an ALTER TABLE column rename expression using
// the same parsed token-range machinery used by the DDL rename implementation.
// The caller supplies the resolved old column name from the target schema.
func FnSQLiteRenameColumn(args []interface{}, oldColumn string, column int) (interface{}, error) {
	if len(args) < 9 || args[0] == nil || args[6] == nil {
		return nil, nil
	}
	sqlText := fmt.Sprint(args[0])
	newName := fmt.Sprint(args[6])
	tableName := fmt.Sprint(args[4])
	quote := false
	if v, ok := args[7].(int64); ok {
		quote = v != 0
	} else if v, ok := args[7].(int); ok {
		quote = v != 0
	}
	quotedName := newName
	if quote {
		quotedName = `"` + strings.ReplaceAll(newName, `"`, `""`) + `"`
	}
	ctx := &RenameContext{OldName: oldColumn, NewName: newName, QuotedNew: quotedName, TableName: tableName}
	if strings.Contains(strings.ToUpper(sqlText), "CREATE TABLE") {
		result, err := renameColumnInTableSQL(sqlText, oldColumn, newName, tableName, fmt.Sprint(args[2]), column, quotedName)
		if err != nil || result == "" {
			return nil, err
		}
		return result, nil
	}
	if strings.Contains(strings.ToUpper(sqlText), "CREATE INDEX") {
		return renameColumnInIndexSQL(sqlText, oldColumn, newName, tableName, quote)
	}
	if strings.Contains(strings.ToUpper(sqlText), "CREATE TRIGGER") {
		return renameColumnInTriggerSQL(sqlText, oldColumn, newName, true, quote)
	}
	ranges, err := FindRenameTokens(sqlText, ctx)
	if err != nil {
		return nil, err
	}
	return ApplyRenames(sqlText, ranges, quotedName), nil
}

// FnSQLiteRenameTable implements SQLite's internal sqlite_rename_table
// rewrite helper (alter.c renameTableFunc). SQL text, old name, and new name
// are required; NULL in any of those inputs produces SQL NULL.
func FnSQLiteRenameTable(args []interface{}) (interface{}, error) {
	if len(args) < 7 || args[3] == nil || args[4] == nil || args[5] == nil {
		return nil, nil
	}
	sqlText := fmt.Sprint(args[3])
	oldName, newName := fmt.Sprint(args[4]), fmt.Sprint(args[5])
	ctx := &RenameContext{
		OldName: oldName, NewName: newName,
		QuotedNew: `"` + strings.ReplaceAll(newName, `"`, `""`) + `"`, IsTable: true,
	}
	ranges, err := FindRenameTokens(sqlText, ctx)
	if err != nil {
		return nil, err
	}
	return ApplyRenames(sqlText, ranges, ctx.QuotedNew), nil
}

func FnSQLiteRenameQuoteFix(args []interface{}) (interface{}, error) {
	if len(args) < 2 || args[1] == nil {
		return "", nil
	}
	sqlStr, ok := args[1].(string)
	if !ok {
		return "", nil
	}
	return quoteFixSQL(sqlStr), nil
}

// quoteFixSQL rewrites non-identifier double-quoted tokens to single-quoted
// string literals, preserving everything else verbatim.
func quoteFixSQL(sqlStr string) string {
	return quoteFixSQLWithColumns(sqlStr, nil)
}

// isSQLIdentifier reports whether s is a valid unquoted SQL identifier
// (letter or underscore first, then letters/digits/underscores).
func isSQLIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' ||
			(i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// vtabModuleName extracts the module name from a virtual table CREATE
// statement ("CREATE VIRTUAL TABLE e1 USING echo(x1)" → "echo").
func vtabModuleName(sqlStr string) string {
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(sqlStr[idx+len(" USING "):])
	end := strings.IndexAny(rest, " (")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// checkViewRenameDependencies checks whether any view that references the
// given table has broken column references or would become ambiguous after a
// column rename. ALTER TABLE RENAME COLUMN fails if a view selects a column
// that does not exist on the referenced tables (SQLite: "error in view %s: no
// such column: %s") or if the rename makes a column reference ambiguous
// ("error in view %s after rename: ambiguous column name: %s").
func (e *DDLExecutor) checkViewRenameDependencies(tableName string, oldColName, newColName string) *Result {
	views, err := e.ctx.Schema().GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		if res := e.processViewRenameDependency(view, tableName, oldColName, newColName); res != nil {
			return res
		}
	}
	return nil
}

// processViewRenameDependency checks a single view for references to the
// renamed table, reporting column-resolution or post-rename-ambiguity errors.
func (e *DDLExecutor) processViewRenameDependency(view *schema.Entry, tableName, oldColName, newColName string) *Result {
	upperSQL := strings.ToUpper(view.SQL)
	fromIdx := strings.Index(upperSQL, " FROM ")
	if fromIdx < 0 {
		return nil
	}
	fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
	fromTables := splitFromTables(fromRest)
	if len(fromTables) == 0 {
		return nil
	}
	// The view only matters if the renamed table is one of its FROM tables.
	for _, ft := range fromTables {
		if strings.EqualFold(ft, tableName) {
			validCols, tableCols, res := e.buildViewFromTableCols(fromTables, view.Name)
			if res != nil {
				return res
			}
			selIdx := strings.Index(strings.ToUpper(view.SQL), "SELECT ")
			if selIdx < 0 {
				return nil
			}
			return e.validateViewRenameSelect(view, view.SQL[selIdx+7:fromIdx], fromTables, validCols, tableCols, tableName, oldColName, newColName)
		}
	}
	return nil
}

// viewUnavailableModule reports a result when a view's FROM table is broken
// (findErr != nil and not a view) or references a virtual-table module that is
// unavailable (unregistered or a NoopModule stub). It returns nil when the
// FROM table is valid/recoverable.
func (e *DDLExecutor) viewUnavailableModule(ft string, entry *schema.Entry, viewName string, findErr error) *Result {
	if findErr != nil {
		return e.brokenViewFromError(ft, viewName, findErr)
	}
	// A view that references a virtual table whose module is not registered
	// cannot be re-validated; SQLite reports "no such module: %s" on rename.
	// NoopModule stubs (rtree, etc.) count as unavailable, and so does the
	// echo module: it is a test-only SQLite module that is only registered on
	// the connection that ran register_echo_module. Frigolite registers echo
	// globally, so for rename validation it must be treated as unavailable to
	// match a fresh connection (altercol-11.3).
	if !e.isVirtualTable(entry) {
		return nil
	}
	return e.viewModuleUnavailable(entry, viewName)
}

// brokenViewFromError resolves a FROM table lookup failure: a view that
// doesn't exist either is recoverable (nil); anything else leaves the view
// broken and SQLite reports the lookup error on rename.
func (e *DDLExecutor) brokenViewFromError(ft, viewName string, findErr error) *Result {
	// If the FROM table itself doesn't exist, the view is broken; SQLite
	// reports this on rename.
	if v, vErr := e.ctx.Schema().FindView(ft); vErr != nil {
		return &Result{Error: fmt.Errorf("error in view %s: %s", viewName, findErr.Error())}
	} else if v != nil {
		return nil
	}
	return nil
}

// viewModuleUnavailable reports the rename-blocking error for a view whose
// FROM virtual table's module is unregistered, a NoopModule stub, or the
// test-only echo module.
func (e *DDLExecutor) viewModuleUnavailable(entry *schema.Entry, viewName string) *Result {
	if mod := vtabModuleName(entry.SQL); mod != "" {
		m, ok := e.ctx.VTables().Find(mod)
		if !ok {
			return &Result{Error: fmt.Errorf("error in view %s: no such module: %s", viewName, mod)}
		}
		if _, isNoop := m.(*vtab.NoopModule); isNoop {
			return &Result{Error: fmt.Errorf("error in view %s: no such module: %s", viewName, mod)}
		}
		if _, isEcho := m.(*vtab.EchoModule); isEcho {
			return &Result{Error: fmt.Errorf("error in view %s: no such module: %s", viewName, mod)}
		}
	}
	return nil
}

// buildViewFromTableCols builds the union of columns across a view's FROM
// tables (validCols) and a per-table column map (tableCols) for validating
// references and detecting post-rename ambiguity. It returns an error result
// when a broken FROM table or unavailable module blocks re-validation.
func (e *DDLExecutor) buildViewFromTableCols(fromTables []string, viewName string) (map[string]bool, map[string]map[string]bool, *Result) {
	validCols := make(map[string]bool)
	tableCols := make(map[string]map[string]bool)
	for _, ft := range fromTables {
		entry, findErr := e.ctx.Schema().FindTable(ft)
		if findErr != nil {
			if res := e.viewUnavailableModule(ft, entry, viewName, findErr); res != nil {
				return nil, nil, res
			}
			continue
		}
		if res := e.viewUnavailableModule(ft, entry, viewName, nil); res != nil {
			return nil, nil, res
		}
		colDefs := e.ctx.ColCache()[ft]
		if colDefs == nil {
			colDefs = e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
		}
		cols := make(map[string]bool)
		for _, cd := range colDefs {
			cols[strings.ToUpper(cd.Name)] = true
			validCols[strings.ToUpper(cd.Name)] = true
		}
		tableCols[strings.ToUpper(ft)] = cols
	}
	return validCols, tableCols, nil
}

// validateViewRenameSelect checks the identifiers in a view's SELECT list
// against the current column set, reporting "no such column" for unresolvable
// references and "ambiguous column name" for post-rename collisions.
func (e *DDLExecutor) validateViewRenameSelect(view *schema.Entry, afterSelect string, fromTables []string, validCols map[string]bool, tableCols map[string]map[string]bool, tableName, oldColName, newColName string) *Result {
	// Extract bare identifier tokens (not whole comma/space chunks) so
	// expressions like "a+10" yield the identifier "a" rather than the literal
	// chunk "a+10". SQLite reports the first unresolvable identifier.
	viewCols := extractIdentifierTokens(afterSelect)
	for i, col := range viewCols {
		col = strings.TrimSpace(col)
		if col == "" {
			continue
		}
		if skipViewSelectToken(col, i, viewCols, fromTables) {
			continue
		}
		upperCol := normalizeViewColRef(strings.ToUpper(col))
		if upperCol == "" || upperCol == "*" {
			continue
		}
		if !validCols[strings.ToUpper(upperCol)] {
			return &Result{Error: fmt.Errorf("error in view %s: no such column: %s", view.Name, col)}
		}
		if res := e.checkViewRenameAmbiguity(view, fromTables, tableCols, tableName, oldColName, newColName, upperCol); res != nil {
			return res
		}
	}
	return nil
}

// skipViewSelectToken reports whether a token extracted from a view's SELECT
// list should be skipped (empty, an AS alias, a function call/expression,
// a SQL keyword, a numeric literal, or a FROM table reference).

// QuoteFixWithSchema rewrites a schema-qualified SQL string's identifier
// quoting. Exported for the expression evaluator's rename_quotefix function.
func (e *DDLExecutor) QuoteFixWithSchema(schemaName, sqlStr string) string {
	return e.quoteFixWithSchema(schemaName, sqlStr)
}
