// pragma_table_info / pragma_table_xinfo table-valued materialization: column
// definition resolution, PRIMARY KEY ordinal parsing, and DEFAULT-expression
// rendering (pragma_table.go split; behavior unchanged).
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// materializeTableInfo builds the rows of pragma_table_info / pragma_table_xinfo
// for the table or view named by the first function argument. The result has
// columns (cid, name, type, notnull, dflt_value, pk), one row per column.
func (e *Engine) materializeTableInfo(ref sql.TableRef) ([]sql.ColumnDef, [][]interface{}, error) {
	return e.materializeTableInfoWithRow(ref, nil)
}

// materializeTableInfoWithRow is materializeTableInfo with a row context for
// column-reference arguments (correlated pragma_table_info) and an optional
// second schema argument (pragma_table_info(table, schema)).
func (e *Engine) materializeTableInfoWithRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	cols := []sql.ColumnDef{
		{Name: "cid"},
		{Name: "name"},
		{Name: "type"},
		{Name: "notnull"},
		{Name: "dflt_value"},
		{Name: "pk"},
	}
	xinfo := strings.HasSuffix(strings.ToLower(ref.Name), "table_xinfo")
	if xinfo {
		cols = append(cols, sql.ColumnDef{Name: "hidden"})
	}
	tableName, err := tableInfoTableName(e, ref, row)
	if err != nil {
		return nil, nil, err
	}

	colDefs, found, err := e.tableInfoColDefs(tableName)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		// Unknown table or view: pragma_table_info returns zero rows.
		return cols, nil, nil
	}
	return cols, tableInfoRows(colDefs, xinfo, e.tableInfoPkOrdinals(tableName, colDefs)), nil
}

// tableInfoPkOrdinals extracts the 1-based ordinal of every PRIMARY KEY
// column from the table's CREATE SQL (sqlite3PragTyp_TABLE_INFO's pk field
// reports the column's position in the key, not a 0/1 flag): a table-level
// PRIMARY KEY(e,b,c) numbers e=1, b=2, c=3 (pragma-6.2.2) and duplicate
// entries shift later positions (pragma-6.8: PRIMARY KEY(a,b,a,c) numbers
// a=1, b=2, c=4). Column-level PRIMARY KEY flags number in declaration
// order. nil means the PK shape could not be parsed (callers fall back to
// the 0/1 flag).
func (e *Engine) tableInfoPkOrdinals(tableName string, colDefs []sql.ColumnDef) map[string]int64 {
	te, _, err := e.findTable(tableName)
	if err != nil || te == nil || te.SQL == "" {
		return nil
	}
	return primaryKeyOrdinalsFromSQL(te.SQL)
}

// primaryKeyOrdinalsFromSQL parses the PRIMARY KEY declaration of a
// CREATE TABLE statement into per-column 1-based ordinals.
func primaryKeyOrdinalsFromSQL(sqlText string) map[string]int64 {
	open, close := primaryKeyBodySpan(sqlText)
	if open < 0 || close < 0 {
		return nil
	}
	pkList := primaryKeyColumnList(sqlText[open+1 : close])
	if len(pkList) == 0 {
		return nil
	}
	return primaryKeyOrdinalMap(pkList)
}

// primaryKeyBodySpan locates the column-definition body of a CREATE TABLE
// statement: the first '(' and its matching ')' (open or close is -1 when
// the statement shape cannot be scanned).
func primaryKeyBodySpan(sqlText string) (open, close int) {
	open = strings.Index(sqlText, "(")
	if open < 0 {
		return -1, -1
	}
	depth := 0
	for i := open; i < len(sqlText); i++ {
		switch sqlText[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return open, i
			}
		}
	}
	return open, -1
}

// primaryKeyColumnList collects the PRIMARY KEY column names from a CREATE
// TABLE body: table-level PRIMARY KEY(...) / CONSTRAINT ... PRIMARY KEY(...)
// terms contribute their key-list order; column-level PRIMARY KEY flags
// contribute in declaration order.
func primaryKeyColumnList(body string) []string {
	var pkList []string
	for _, term := range splitTopLevelCommas(body) {
		f := strings.Fields(strings.TrimSpace(term))
		if len(f) == 0 {
			continue
		}
		trimmed := strings.TrimSpace(term)
		up := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(up, "PRIMARY") || strings.HasPrefix(up, "CONSTRAINT"):
			// Table-level PRIMARY KEY(...) — capture the key list order.
			pkList = appendTableLevelKeyColumns(pkList, trimmed, up)
		case strings.Contains(up, "PRIMARY KEY"):
			name := strings.Trim(f[0], `"`)
			pkList = append(pkList, name)
		}
	}
	return pkList
}

// appendTableLevelKeyColumns appends a table-level PRIMARY KEY term's key-list
// columns, in key-list order (entries whose list cannot be scanned are
// skipped).
func appendTableLevelKeyColumns(pkList []string, trimmed, up string) []string {
	pOpen := strings.Index(up, "(")
	if pOpen < 0 {
		return pkList
	}
	pClose := strings.LastIndex(trimmed, ")")
	if pClose <= pOpen {
		return pkList
	}
	for _, k := range strings.Split(trimmed[pOpen+1:pClose], ",") {
		k = strings.TrimSpace(strings.SplitN(strings.TrimSpace(k), " ", 2)[0])
		k = strings.Trim(k, `"`)
		if k != "" {
			pkList = append(pkList, k)
		}
	}
	return pkList
}

// primaryKeyOrdinalMap numbers the PRIMARY KEY columns 1-based in key order;
// duplicate entries keep their first occurrence's position (pragma-6.8:
// PRIMARY KEY(a,b,a,c) numbers a=1, b=2, c=4).
func primaryKeyOrdinalMap(pkList []string) map[string]int64 {
	ord := make(map[string]int64, len(pkList))
	pos := int64(0)
	for _, k := range pkList {
		pos++
		key := strings.ToUpper(k)
		if _, dup := ord[key]; !dup {
			ord[key] = pos
		}
	}
	return ord
}

// splitTopLevelCommas splits s on commas that sit outside any parentheses.
func splitTopLevelCommas(s string) []string {
	var parts []string
	depth := 0
	cur := strings.Builder{}
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
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(s[i])
	}
	return append(parts, cur.String())
}

// tableInfoTableName resolves the first argument of pragma_table_info(xinfo)
// to a table name, honoring the optional second (schema) argument.
func tableInfoTableName(e *Engine, ref sql.TableRef, row Row) (string, error) {
	if len(ref.Args) == 0 {
		return "", fmt.Errorf("wrong number of arguments to function %s()", ref.Name)
	}
	argVal, err := e.evalExpr(ref.Args[0], row)
	if err != nil {
		return "", err
	}
	tableName, ok := argVal.(string)
	if !ok {
		return "", fmt.Errorf("wrong type for argument of %s(): expected string", ref.Name)
	}
	// Optional second argument: schema name. Resolve the table within that
	// schema (SQLite pragma_table_info(table, schema)).
	if len(ref.Args) >= 2 {
		schemaVal, err := e.evalExpr(ref.Args[1], row)
		if err != nil {
			return "", err
		}
		if schema, ok := schemaVal.(string); ok && schema != "" {
			tableName = schema + "." + tableName
		}
	}
	return tableName, nil
}

// tableInfoColDefs resolves the column definitions of a table-info target: a
// pragma table-valued function, an ordinary table, or a view. found is false
// when the name is not a known object (zero rows).
func (e *Engine) tableInfoColDefs(tableName string) (colDefs []sql.ColumnDef, found bool, err error) {
	// A pragma table-valued function name (e.g. pragma_function_list) is
	// materialized as a virtual table; PRAGMA table_info(pragma_function_list)
	// must report the FUNCTION's columns, not the synthetic schema entry that
	// findTable synthesizes for PRAGMA_* names.
	if isPragmaTableFunc(tableName) {
		if defs, _, err := e.materializePragmaTableWithRowImpl(sql.TableRef{Name: tableName}, nil); err == nil {
			return defs, true, nil
		}
	}
	if te, _, err := e.findTable(tableName); err == nil {
		return e.tableInfoTableColDefs(te)
	}
	if ve, _, err := e.findView(tableName); err == nil {
		defs, err := e.viewColumnDefs(ve)
		if err != nil {
			return nil, true, err
		}
		return defs, true, nil
	}
	// Eponymous-only module implicit tables (generate_series): visible to
	// PRAGMA table_info/table_xinfo with hidden columns flagged (tabfunc01-1.1b).
	if defs, found, err := e.eponymousVtabColDefs(tableName); found {
		return defs, true, err
	}
	return nil, false, nil
}

// tableInfoTableColDefs resolves defs for a plain table target, surfacing
// the unregistered-module error for unreachable created vtabs.
func (e *Engine) tableInfoTableColDefs(te *schema.Entry) ([]sql.ColumnDef, bool, error) {
	// A created virtual table whose module is not registered on this
	// connection is unreachable: SQLite reports "no such module" when
	// the schema is next required (vtab1.2.6: PRAGMA table_info(t1)
	// after a reopen with the echo module unregistered).
	if te.RootPage == 0 {
		if modName, _, isVtab := vtabModuleFromSQL(te.SQL); isVtab {
			if _, found := e.vtabs.Find(modName); !found {
				return nil, false, fmt.Errorf("no such module: %s", modName)
			}
		}
	}
	return e.parseColumnDefs(te.Name, te.SQL), true, nil
}

// tableInfoRows renders column definitions as pragma_table_info(xinfo) rows.
// pkOrd maps PK columns to their 1-based key position (nil falls back to the
// 0/1 flag rendering).
func tableInfoRows(colDefs []sql.ColumnDef, xinfo bool, pkOrd map[string]int64) [][]interface{} {
	rows := make([][]interface{}, 0, len(colDefs))
	cid := int64(0)
	for _, cd := range colDefs {
		row, skip := tableInfoRow(cd, cid, xinfo, pkOrd)
		if skip {
			continue
		}
		rows = append(rows, row)
		cid++
	}
	return rows
}

// tableInfoRow renders one column definition as a pragma_table_info(xinfo)
// row. skip is true for dropped columns and (in table_info) hidden columns.
func tableInfoRow(cd sql.ColumnDef, cid int64, xinfo bool, pkOrd map[string]int64) (row []interface{}, skip bool) {
	// Skip dropped columns (removed via ALTER TABLE DROP COLUMN).
	if cd.Dropped {
		return nil, true
	}
	// PRAGMA table_info excludes hidden columns; table_xinfo includes
	// them with a nonzero hidden flag (SQLite pragma.c).
	if !xinfo && isHiddenColumnDef(cd) {
		return nil, true
	}
	notnull, pk, typeName, dflt := tableInfoRowFields(cd, pkOrd)
	if xinfo {
		hiddenFlag := int64(0)
		if isHiddenColumnDef(cd) {
			hiddenFlag = 1
		}
		return []interface{}{cid, cd.Name, typeName, notnull, dflt, pk, hiddenFlag}, false
	}
	return []interface{}{cid, cd.Name, typeName, notnull, dflt, pk}, false
}

// tableInfoRowFields renders a column definition's row fields: notnull and pk
// as 0/1, the declared type (NONE-affinity sentinel rendered as empty), and
// the rendered DEFAULT expression.
func tableInfoRowFields(cd sql.ColumnDef, pkOrd map[string]int64) (notnull, pk int64, typeName string, dflt interface{}) {
	if cd.NotNull {
		notnull = 1
	}
	if ord, ok := pkOrd[strings.ToUpper(cd.Name)]; ok {
		// The parsed ordinal is authoritative: table-level PRIMARY KEY
		// columns carry no column-level PrimaryKey flag.
		pk = ord
	} else if cd.PrimaryKey {
		pk = 1
	}
	// The NONE-affinity sentinel (an expression-derived view column with
	// no declared type) renders as an empty type, matching SQLite.
	typeName = cd.Type
	if typeName == util.AffinityNone {
		typeName = ""
	}
	if cd.Default != nil {
		dflt = renderDefaultValue(cd.Default)
	}
	return
}

// renderDefaultValue renders a column DEFAULT expression as SQLite's
// dflt_value text. Numeric unary signs are glued to the number ("-1", "+4.0")
// and string literals keep their quotes.
func renderDefaultValue(d sql.Expr) string {
	if un, ok := d.(*sql.UnaryOp); ok {
		switch un.Operator {
		case "-", "+":
			if nl, ok := un.Operand.(*sql.NumericLit); ok {
				return un.Operator + nl.Value
			}
		}
	}
	return compactExprText(sql.ExprString(d))
}

// exprQuoteState tracks which quoted span (single/double/backtick string or
// [bracket] identifier) a byte position sits in while compacting an
// expression's text.
type exprQuoteState struct {
	inSingle, inDouble, inBacktick, inBracket bool
}

// advance transitions the quote state machine by one byte: inside a quoted
// span only its terminator closes it; outside, an opening quote or bracket
// enters one.
func (q *exprQuoteState) advance(c byte) {
	if !q.tryClose(c) {
		q.tryOpen(c)
	}
}

// tryClose closes the quoted span c terminates (single/double/backtick
// string or bracket identifier); reports whether any span was open.
func (q *exprQuoteState) tryClose(c byte) bool {
	switch {
	case q.inSingle:
		if c == '\'' {
			q.inSingle = false
		}
		return true
	case q.inDouble:
		if c == '"' {
			q.inDouble = false
		}
		return true
	case q.inBacktick:
		if c == '`' {
			q.inBacktick = false
		}
		return true
	case q.inBracket:
		if c == ']' {
			q.inBracket = false
		}
		return true
	}
	return false
}

// tryOpen opens a quoted span when c starts one.
func (q *exprQuoteState) tryOpen(c byte) {
	switch c {
	case '\'':
		q.inSingle = true
	case '"':
		q.inDouble = true
	case '`':
		q.inBacktick = true
	case '[':
		q.inBracket = true
	}
}

// active reports whether the byte stream is currently inside any quoted span.
func (q *exprQuoteState) active() bool {
	return q.inSingle || q.inDouble || q.inBacktick || q.inBracket
}

// isExprSpace reports whether c is whitespace SQLite's expression text may
// carry.
func isExprSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// compactExprText removes whitespace around SQL operators outside quoted
// spans: SQLite renders DEFAULT expressions via sqlite3ExprPrint, which
// glues binary operators to their operands — DEFAULT (5+3) reports "5+3",
// not "5 + 3" (pragma-6.2.2).
func compactExprText(s string) string {
	var b strings.Builder
	var q exprQuoteState
	for i := 0; i < len(s); i++ {
		c := s[i]
		q.advance(c)
		if q.active() || !isExprSpace(c) {
			b.WriteByte(c)
			continue
		}
		if compactExprDropsSpace(s, &b, i) {
			continue // whitespace glued to an operator disappears
		}
		b.WriteByte(c)
	}
	return b.String()
}

// compactExprDropsSpace reports whether the whitespace byte at s[i] must be
// dropped: an operator already written on the left glues to its right
// operand, and whitespace before an operator is dropped too.
func compactExprDropsSpace(s string, b *strings.Builder, i int) bool {
	prev := byte(0)
	if b.Len() > 0 {
		prev = b.String()[b.Len()-1]
	}
	if isExprOperatorByte(prev) {
		return true // operator on the left glues to its right operand
	}
	// Look ahead: whitespace before an operator is dropped.
	j := i + 1
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	return j < len(s) && isExprOperatorByte(s[j])
}

// isExprOperatorByte reports whether c is a binary-operator byte whose sides
// SQLite's expression printer renders without surrounding spaces.
func isExprOperatorByte(c byte) bool {
	switch c {
	case '+', '-', '*', '/', '%', '|', '&', '=', '<', '>':
		return true
	}
	return false
}
