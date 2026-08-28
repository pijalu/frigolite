package execddl

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// findTableRefsInTrigger extracts table references from a trigger body.
// Returns a list of referenced table names found in INSERT, UPDATE, DELETE, SELECT statements.
func findTableRefsInTrigger(triggerSQL string) []string {
	var refs []string

	// Extract CTE names from WITH definitions — these should be skipped
	// as they are not real table references.
	cteSet := make(map[string]bool)
	for _, name := range extractCTENames(triggerSQL) {
		cteSet[name] = true
	}
	// Helper to check if a name is a CTE (should be skipped)
	isCTE := func(name string) bool {
		return cteSet[strings.ToUpper(name)] || cteSet[name]
	}

	refs = appendTriggerRefs(triggerSQL, refs, `(?i)INSERT\s+INTO\s+([a-zA-Z_]\w*)`, isCTE, nil)
	refs = appendTriggerRefs(triggerSQL, refs, `(?i)\bFROM\s+([a-zA-Z_]\w*)`, isCTE, skipNewOld)
	refs = appendTriggerRefs(triggerSQL, refs, `(?i)\bUPDATE\s+([a-zA-Z_]\w*)`, isCTE, skipUpdateKeywords)
	refs = appendTriggerRefs(triggerSQL, refs, `(?i)DELETE\s+FROM\s+([a-zA-Z_]\w*)`, isCTE, nil)
	refs = appendTriggerRefs(triggerSQL, refs, `(?i)\bJOIN\s+([a-zA-Z_]\w*)`, isCTE, skipNewOld)

	return refs
}

// appendTriggerRefs appends the table names captured by a regex pattern,
// skipping names rejected by skip or names that are CTEs.
func appendTriggerRefs(triggerSQL string, refs []string, pattern string, isCTE func(string) bool, skip func(string) bool) []string {
	re := regexp.MustCompile(pattern)
	for _, m := range re.FindAllStringSubmatch(triggerSQL, -1) {
		t := m[1]
		if skip != nil && skip(t) {
			continue
		}
		if isCTE(t) {
			continue
		}
		refs = append(refs, t)
	}
	return refs
}

// skipNewOld reports whether a captured name is the NEW/OLD pseudotable.
func skipNewOld(t string) bool {
	return strings.EqualFold(t, "NEW") || strings.EqualFold(t, "OLD")
}

// skipUpdateKeywords reports whether a captured name after UPDATE is a keyword
// (SET/OF/ON) rather than a table reference.
func skipUpdateKeywords(t string) bool {
	return strings.EqualFold(t, "SET") || strings.EqualFold(t, "OF") || strings.EqualFold(t, "ON")
}

// emptyINBareOperandSpans returns the byte spans of the bare identifier
// operands of empty IN lists ("b IN ()" → the span covering b). SQLite's
// rename machinery leaves a bare column operand of an empty IN untouched, but
// still renames references inside function-call operands
// (altertab3-8.2.2: LIKELIHOOD(c0, 1.0) IN () renames c0).
func emptyINBareOperandSpans(sql string) [][2]int {
	var spans [][2]int
	up := strings.ToUpper(sql)
	for i := 0; i < len(sql); i++ {
		k, ok := findEmptyIN(sql, up, i)
		if !ok {
			continue
		}
		// "IN ()" found at i. The operand is a bare identifier if the
		// character before it is not ')' (a parenthesized/function operand).
		p := skipBackWhitespace(sql, i-1)
		if p >= 0 && sql[p] == ')' {
			i = k
			continue
		}
		// Walk back to the identifier start.
		for p >= 0 && (isIdentByte(sql[p]) || sql[p] == '.' || sql[p] == '"') {
			p--
		}
		spans = append(spans, [2]int{p + 1, i})
		i = k
	}
	return spans
}

// findEmptyIN scans for "IN ()" with optional whitespace at position i
// (case-insensitive), returning the position just after the closing ')'.
func findEmptyIN(sql, up string, i int) (k int, ok bool) {
	if i+2 > len(sql) || up[i] != 'I' || up[i+1] != 'N' {
		return 0, false
	}
	j := i + 2
	for j < len(sql) && isSQLSpace(sql[j]) {
		j++
	}
	if j >= len(sql) || sql[j] != '(' {
		return 0, false
	}
	k = j + 1
	for k < len(sql) && isSQLSpace(sql[k]) {
		k++
	}
	if k >= len(sql) || sql[k] != ')' {
		return 0, false
	}
	return k, true
}

// isSQLSpace reports whether b is SQL whitespace.
func isSQLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// skipBackWhitespace returns the index of the last non-whitespace byte at or
// before i, or -1 when all preceding bytes are whitespace.
func skipBackWhitespace(s string, i int) int {
	for i >= 0 && isSQLSpace(s[i]) {
		i--
	}
	return i
}

// emptyINOperandSpans returns the byte spans [start, end) of every operand
// of an empty IN list ("x IN ()", "(expr) IN ()") in the SQL text. SQLite's
// rename machinery does not rewrite names inside these operands.
func emptyINOperandSpans(sql string) [][2]int {
	var spans [][2]int
	up := strings.ToUpper(sql)
	for i := 0; i < len(sql); i++ {
		k, ok := findEmptyIN(sql, up, i)
		if !ok {
			continue
		}
		start := inOperandStart(sql, i)
		spans = append(spans, [2]int{start, i})
		i = k
	}
	return spans
}

// inOperandStart walks backward from the IN keyword (at i) to find the start
// of the operand: the matching open paren for a parenthesized operand, or the
// start of the bare identifier/expression.
func inOperandStart(sql string, i int) int {
	depth := 0
	p := i - 1
	for ; p >= 0; p-- {
		switch sql[p] {
		case ')':
			depth++
		case '(':
			if depth > 0 {
				depth--
				if depth == 0 {
					// This open paren matches the operand's closing paren —
					// the operand starts here.
					return p
				}
			} else {
				return p
			}
		}
	}
	// No enclosing paren: the operand is the bare identifier immediately
	// before IN. Walk back to its start.
	p = skipBackWhitespace(sql, i-1)
	for p >= 0 && (isIdentByte(sql[p]) || sql[p] == '.') {
		p--
	}
	return p + 1
}

// skipSQLWhitespaceAndComments advances i past whitespace and SQL comments
// (/* ... */ and -- ...) starting at or after position i in s.
func skipSQLWhitespaceAndComments(s string, i int) int {
	for i < len(s) {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' {
			i++
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			i = skipBlockComment(s, i)
			continue
		}
		if i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			i = skipLineComment(s, i)
			continue
		}
		break
	}
	return i
}

// skipBlockComment advances past a /* ... */ comment starting at i.
func skipBlockComment(s string, i int) int {
	j := i + 2
	for j+1 < len(s) && !(s[j] == '*' && s[j+1] == '/') {
		j++
	}
	if j+1 < len(s) {
		return j + 2
	}
	return len(s)
}

// skipLineComment advances past a -- ... line comment starting at i.
func skipLineComment(s string, i int) int {
	for i < len(s) && s[i] != '\n' {
		i++
	}
	return i
}

// rebuildCreateTableSQL rebuilds a CREATE TABLE SQL string with updated column definitions.
func rebuildCreateTableSQL(origSQL string, colDefs []sql.ColumnDef) string {
	upper := strings.ToUpper(origSQL)
	if !strings.Contains(upper, "CREATE TABLE") {
		return ""
	}
	tableName := extractCreateTableName(origSQL, upper)
	parenStart, parenEnd, ok := outerParenRange(origSQL)
	if !ok {
		return ""
	}
	trailingSQL := strings.TrimSpace(origSQL[parenEnd+1:])
	defText := origSQL[parenStart+1 : parenEnd]

	// Build a set of column names from current column definitions
	colNames := make(map[string]bool)
	for _, cd := range colDefs {
		colNames[strings.ToUpper(cd.Name)] = true
	}

	parts := splitTopLevelParts(defText)
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	constraints := tableConstraintsFromParts(parts, colNames)
	origColDefs := buildOrigColDefs(parts)
	return assembleCreateTableSQL(tableName, colDefs, constraints, origColDefs, trailingSQL)
}

// extractCreateTableName extracts the table name following CREATE TABLE,
// honoring quoted names with spaces and schema prefixes like main.t1.
func extractCreateTableName(origSQL, upper string) string {
	afterCreate := origSQL
	if idx := strings.Index(upper, "CREATE TABLE"); idx >= 0 {
		afterCreate = origSQL[idx+12:]
	}
	afterCreate = strings.TrimSpace(afterCreate)
	// The table name is the next word — scan until the opening paren at depth
	// 0, skipping over quoted identifiers.
	end := len(afterCreate)
	for i := 0; i < len(afterCreate); i++ {
		if afterCreate[i] == '\'' || afterCreate[i] == '"' || afterCreate[i] == '`' {
			q := afterCreate[i]
			i++
			for i < len(afterCreate) && afterCreate[i] != q {
				i++
			}
			continue
		}
		if afterCreate[i] == '(' || afterCreate[i] == ' ' {
			end = i
			break
		}
	}
	return strings.TrimSpace(afterCreate[:end])
}

// outerParenRange returns the byte range [start, end] of the first balanced
// pair of outer parentheses in s.
func outerParenRange(s string) (start, end int, ok bool) {
	start = strings.Index(s, "(")
	if start < 0 {
		return 0, 0, false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return start, i, true
			}
		}
	}
	return 0, 0, false
}

// tableConstraintsFromParts returns the parts of a CREATE TABLE definition
// that are table-level constraints (not column definitions).
func tableConstraintsFromParts(parts []string, colNames map[string]bool) []string {
	var constraints []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Check if this part looks like a column definition (starts with a known column name)
		upperPart := strings.ToUpper(trimmed)
		isColumnDef := false
		for name := range colNames {
			if strings.HasPrefix(upperPart, name) || strings.HasPrefix(upperPart, "\""+name+"\"") {
				isColumnDef = true
				break
			}
		}
		if !isColumnDef && (strings.HasPrefix(upperPart, "PRIMARY KEY") ||
			strings.HasPrefix(upperPart, "UNIQUE") ||
			strings.HasPrefix(upperPart, "CHECK") ||
			strings.HasPrefix(upperPart, "FOREIGN KEY") ||
			strings.HasPrefix(upperPart, "CONSTRAINT")) {
			constraints = append(constraints, trimmed)
		}
	}
	return constraints
}

// buildOrigColDefs builds a mapping from column name (upper) to the original
// definition text for each column part.
func buildOrigColDefs(parts []string) map[string]string {
	origColDefs := make(map[string]string)
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Extract the column name (first word)
		spaceIdx := strings.IndexAny(trimmed, " (\"")
		if spaceIdx > 0 {
			name := strings.ToUpper(strings.Trim(trimmed[:spaceIdx], "\""))
			origColDefs[name] = trimmed
		} else if spaceIdx < 0 {
			// Single word column name
			name := strings.ToUpper(strings.Trim(trimmed, "\""))
			origColDefs[name] = trimmed
		}
	}
	return origColDefs
}

// assembleCreateTableSQL writes the final CREATE TABLE SQL from the updated
// column definitions, preserving original text where possible.
func assembleCreateTableSQL(tableName string, colDefs []sql.ColumnDef, constraints []string, origColDefs map[string]string, trailingSQL string) string {
	var buf strings.Builder
	buf.WriteString("CREATE TABLE ")
	buf.WriteString(tableName)
	buf.WriteString("(")
	for i, col := range colDefs {
		if i > 0 {
			buf.WriteString(", ")
		}
		// Use original column text if available, otherwise reconstruct
		if orig, ok := origColDefs[strings.ToUpper(col.Name)]; ok {
			// Reconstruct when the column's NOT NULL constraint differs from the
			// original text (e.g. after ALTER COLUMN SET/DROP NOT NULL).
			if col.NotNull != origHasNotNull(orig) {
				formatColumnDef(&buf, col)
			} else {
				buf.WriteString(orig)
			}
		} else {
			formatColumnDef(&buf, col)
		}
	}
	for _, tc := range constraints {
		buf.WriteString(", ")
		buf.WriteString(tc)
	}
	buf.WriteString(")")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
}

// extractIdentifierTokens splits a SELECT-list fragment into bare identifier
// tokens, discarding operators, literals and punctuation. Function names
// (identifiers immediately followed by an open parenthesis) are excluded —
// they are callable, not column references. Used to validate view column
// references: for "a+10, b*5.0, xyz" it yields [a, 10, b, 5, 0, xyz]
// (numeric literals are later skipped as non-columns).
func extractIdentifierTokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	for i := 0; i < len(s); {
		r := rune(s[i])
		// A double-quoted identifier is an atomic token ("a;b" must not be
		// split into a and b).
		if r == '"' {
			flush()
			end := strings.Index(s[i+1:], "\"")
			if end < 0 {
				break
			}
			out = append(out, s[i+1:i+1+end])
			i = i + 2 + end
			continue
		}
		if isIdentTokenRune(r) {
			cur = append(cur, r)
		} else {
			// An identifier immediately followed by '(' is a function name, not
			// a column reference (e.g. group_concat(a ORDER BY b)).
			if r == '(' && len(cur) > 0 && i > 0 && isIdentByte(s[i-1]) {
				cur = nil
			}
			flush()
		}
		i++
	}
	flush()
	return out
}

// isIdentTokenRune reports whether a rune can be part of a bare SQL token
// (letters, digits, underscore, dot).
func isIdentTokenRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '_' || r == '.'
}

// createTableColumnNames extracts the bare column names from a CREATE TABLE
// statement's parenthesized column list.
func createTableColumnNames(sqlStr string) []string {
	inner, ok := outerParenContent(sqlStr)
	if !ok {
		return nil
	}
	var cols []string
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// First token is the column name (strip quotes).
		name := part
		if idx := strings.IndexAny(part, " \t\n"); idx > 0 {
			name = part[:idx]
		}
		name = strings.Trim(name, "\"`")
		if name != "" && isColumnDefKeyword(name) {
			cols = append(cols, name)
		}
	}
	return cols
}

// outerParenContent returns the text inside the first balanced pair of outer
// parentheses in s.
func outerParenContent(s string) (string, bool) {
	open := strings.Index(s, "(")
	if open < 0 {
		return "", false
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}

// isColumnDefKeyword reports whether a column name is not a table-level
// constraint keyword (PRIMARY, UNIQUE, CHECK, FOREIGN, CONSTRAINT).
func isColumnDefKeyword(name string) bool {
	upper := strings.ToUpper(name)
	return !strings.Contains(upper, "PRIMARY") &&
		!strings.Contains(upper, "UNIQUE") &&
		!strings.Contains(upper, "CHECK") &&
		!strings.Contains(upper, "FOREIGN") &&
		!strings.Contains(upper, "CONSTRAINT")
}

// validateTriggerSQL checks if a trigger's SQL is valid.
// Extracts NEW/OLD column references and checks if they exist in the target table.
func (e *DDLExecutor) validateTriggerSQL(triggerSQL string) string {
	// Find the table name from the trigger SQL
	upperSQL := strings.ToUpper(triggerSQL)
	// Extract ON <table> to find the target table
	onIdx := strings.Index(upperSQL, " ON ")
	if onIdx < 0 {
		return ""
	}
	afterOn := strings.TrimSpace(upperSQL[onIdx+4:])
	spaceIdx := strings.IndexAny(afterOn, " \n\t\r")
	if spaceIdx <= 0 {
		return ""
	}
	refTable := afterOn[:spaceIdx]
	validCols := e.triggerValidCols(refTable)
	if validCols == nil {
		return ""
	}
	body := triggerBody(triggerSQL, upperSQL)
	return scanNewOldRefs(body, validCols)
}

// triggerValidCols returns the set of valid (non-dropped) column names of a
// table, or nil when the table does not exist.
func (e *DDLExecutor) triggerValidCols(refTable string) map[string]bool {
	entry, err := e.ctx.Schema().FindTable(refTable)
	if err != nil {
		return nil
	}
	colDefs := e.ctx.ColCache()[refTable]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	}
	validCols := make(map[string]bool)
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		validCols[strings.ToUpper(cd.Name)] = true
	}
	return validCols
}

// triggerBody returns the text between BEGIN and the final END of a trigger.
func triggerBody(triggerSQL, upperSQL string) string {
	begIdx := strings.Index(upperSQL, "BEGIN")
	if begIdx < 0 {
		return ""
	}
	body := triggerSQL[begIdx+len("BEGIN"):]
	endIdx := strings.LastIndex(strings.ToUpper(body), "END")
	if endIdx >= 0 {
		body = body[:endIdx]
	}
	return body
}

// scanNewOldRefs scans for NEW.xxx and OLD.xxx references in a trigger body,
// returning the first reference to a column missing from validCols.
func scanNewOldRefs(body string, validCols map[string]bool) string {
	upperBody := strings.ToUpper(body)
	for i := 0; i < len(upperBody); i++ {
		prefix, nextIdx := findNewOldRef(upperBody, i)
		if nextIdx < 0 {
			break
		}
		colName, ok := newOldColumn(body, nextIdx)
		if ok && !validCols[strings.ToUpper(colName)] {
			return fmt.Sprintf("no such column: %s%s", prefix, colName)
		}
		i = nextIdx + 1
	}
	return ""
}

// findNewOldRef returns the prefix ("new." or "old.") and byte offset of the
// next NEW./OLD. reference at or after i.
func findNewOldRef(upperBody string, i int) (string, int) {
	newIdx := strings.Index(upperBody[i:], "NEW.")
	oldIdx := strings.Index(upperBody[i:], "OLD.")
	if newIdx >= 0 && (oldIdx < 0 || newIdx < oldIdx) {
		return "new.", i + newIdx
	}
	if oldIdx >= 0 {
		return "old.", i + oldIdx
	}
	return "", -1
}

// newOldColumn extracts the column name following a NEW./OLD. reference at
// nextIdx, returning whether a name was found.
func newOldColumn(body string, nextIdx int) (string, bool) {
	colStart := nextIdx + 4 // skip "NEW." or "OLD."
	colEnd := colStart
	for colEnd < len(body) && (isAlpha(body[colEnd]) || body[colEnd] == '_') {
		colEnd++
	}
	if colEnd > colStart {
		return body[colStart:colEnd], true
	}
	return "", false
}

// indexReferencesColumn checks if the CREATE INDEX SQL references a given
// column. The second return value reports whether the reference was written
// as a double-quoted identifier ("name") — which determines the DQS-off
// error message wording when the column is dropped.
func indexReferencesColumn(sqlStr, columnName string) (bool, bool) {
	upperSQL := strings.ToUpper(sqlStr)
	// Find the column-list paren after the ON table clause.
	onIdx := strings.Index(upperSQL, " ON ")
	if onIdx < 0 {
		return false, false
	}
	parenIdx := strings.Index(upperSQL[onIdx:], "(")
	if parenIdx < 0 {
		return false, false
	}
	exprText := upperSQL[onIdx+parenIdx+1:]
	exprText = trimToMatchingParen(exprText)
	return wordReferencesColumn(exprText, columnName)
}

// trimToMatchingParen trims s to the content before its final top-level
// closing paren, then removes one trailing ')' if present.
func trimToMatchingParen(s string) string {
	depth := 0
	endIdx := -1
	for i, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				endIdx = i
			} else {
				depth--
			}
		}
	}
	if endIdx > 0 {
		s = s[:endIdx]
	}
	return strings.TrimSuffix(s, ")")
}

// wordReferencesColumn reports whether a column name appears as a whole word in
// the expression text, tracking double-quoted references.
func wordReferencesColumn(exprText, columnName string) (bool, bool) {
	words := strings.FieldsFunc(exprText, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '"')
	})
	found := false
	quoted := false
	for _, raw := range words {
		// A double-quoted reference "name" keeps its quotes in the word scan
		// (the FieldsFunc set includes '"'), so record quotedness before
		// trimming.
		if len(raw) >= 2 && strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) {
			if strings.EqualFold(raw[1:len(raw)-1], columnName) {
				found = true
				quoted = true
			}
		}
		w := strings.Trim(raw, `"`)
		if strings.EqualFold(w, columnName) {
			found = true
		}
	}
	return found, quoted
}

// formatTableConstraint writes a table-level constraint to the SQL buffer.
func formatTableConstraint(buf *strings.Builder, tc sql.TableConstraint) {
	if tc.Name != "" {
		buf.WriteString("CONSTRAINT ")
		buf.WriteString(tc.Name)
		buf.WriteString(" ")
	}
	switch tc.Type {
	case sql.ConstraintCheck:
		buf.WriteString("CHECK(")
		if tc.Expr != nil {
			buf.WriteString(sql.ExprString(tc.Expr))
		}
		buf.WriteString(")")
	case sql.ConstraintPrimaryKey:
		formatConstraintColumns(buf, "PRIMARY KEY", tc.Columns)
	case sql.ConstraintUnique:
		formatConstraintColumns(buf, "UNIQUE", tc.Columns)
	case sql.ConstraintForeignKey:
		buf.WriteString("FOREIGN KEY ... REFERENCES ...")
	}
}

// formatConstraintColumns writes a constraint keyword followed by its
// parenthesized, comma-separated column list with COLLATE/DESC suffixes.
func formatConstraintColumns(buf *strings.Builder, keyword string, columns []sql.IndexedColumn) {
	buf.WriteString(keyword)
	buf.WriteString("(")
	for i, col := range columns {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(col.Name)
		if col.Collate != "" {
			buf.WriteString(" COLLATE ")
			buf.WriteString(col.Collate)
		}
		if col.Desc {
			buf.WriteString(" DESC")
		}
	}
	buf.WriteString(")")
}
