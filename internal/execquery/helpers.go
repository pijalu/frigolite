package execquery

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// pragmaTableFuncs is the set of table-valued pragma function names Frigolite
// supports as FROM sources (pragma_table_info(x), etc.).
var pragmaTableFuncs = map[string]bool{
	"pragma_table_info":        true,
	"pragma_table_xinfo":       true,
	"pragma_table_list":        true,
	"pragma_index_info":        true,
	"pragma_index_xinfo":       true,
	"pragma_index_list":        true,
	"pragma_foreign_key_list":  true,
	"pragma_foreign_key_check": true,
	"pragma_function_list":     true,
	"pragma_module_list":       true,
	"pragma_pragma_list":       true,
	"pragma_integrity_check":   true,
	"pragma_quick_check":       true,
	"pragma_cache_size":        true,
	"pragma_compile_options":   true,
	"pragma_database_list":     true,
	"pragma_collation_list":    true,
}

// isPragmaTableFunc reports whether name names a table-valued pragma function.
func isPragmaTableFunc(name string) bool {
	lower := strings.ToLower(name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	return pragmaTableFuncs[lower]
}

// isNoSuchVtabErr reports whether err is a "no such module" error from
// materializeVtabTableFunc (meaning the FROM name is an ordinary table, not a
// table-valued function).
func isNoSuchVtabErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such module")
}

// unwrapCollatedValue strips a collation wrapper from a value.
func unwrapCollatedValue(v interface{}) interface{} {
	return execexpr.UnwrapCollatedValue(v)
}

// extractValue extracts the raw value and collation from a possibly-wrapped
// value.
func extractValue(v interface{}) (interface{}, string) {
	return execexpr.ExtractValue(v)
}

// duplicateCTEName returns the name of the first duplicate CTE in the list,
// or "" when all CTE names are distinct.
func duplicateCTEName(ctes []sql.CTEDef) string {
	seen := make(map[string]bool, len(ctes))
	for _, c := range ctes {
		key := strings.ToLower(c.Name)
		if seen[key] {
			return c.Name
		}
		seen[key] = true
	}
	return ""
}

// buildRowMapFromValues builds a RowMap from a positional value slice,
// wrapping values with their column affinities and adding rowid aliases.
func buildRowMapFromValues(values []interface{}, colDefs []sql.ColumnDef, rowID int64) RowMap {
	row := buildRowMapFromValuesNoRowID(values, colDefs)
	if !RowHasRowIDColumn(colDefs) {
		row["rowid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		row["_rowid_"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		row["oid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
	}
	return row
}

// buildRowMapFromValuesNoRowID builds a RowMap from a positional value slice
// without adding implicit rowid/_rowid_/oid aliases. Used for CTE and view
// row maps: SQLite reports "no such column: rowid" for a CTE or view
// reference (with1 15.1), so the aliases must not resolve.
func buildRowMapFromValuesNoRowID(values []interface{}, colDefs []sql.ColumnDef) RowMap {
	row := make(RowMap)
	for i, v := range values {
		if i < len(colDefs) {
			row[colDefs[i].Name] = wrapValueForRowMap(v, colDefs[i])
		}
	}
	return row
}

// wrapValueForRowMap wraps a value with its column's affinity and collation.
func wrapValueForRowMap(v interface{}, cd sql.ColumnDef) interface{} {
	if cd.Type != "" {
		cv := &util.ColumnValue{Value: v, Affinity: util.Affinity(cd.Type)}
		if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
			return &CollatedValue{Value: cv, Collation: strings.ToUpper(coll)}
		}
		return cv
	}
	if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
		return &CollatedValue{Value: v, Collation: strings.ToUpper(coll)}
	}
	return v
}

// rowHasQualifiedKeys reports whether a row map has any table-qualified keys
// (e.g. "t.col"), meaning it was built from a join with qualified column names.
func rowHasQualifiedKeys(row Row) bool {
	if row == nil {
		return false
	}
	if rm, ok := row.(RowMap); ok {
		for k := range rm {
			if strings.Contains(k, ".") {
				return true
			}
		}
	}
	return false
}

// parseTriggerHeader extracts the timing (BEFORE/AFTER/INSTEAD OF) and event
// (INSERT/UPDATE/DELETE) from a trigger's CREATE SQL header.
func parseTriggerHeader(triggerSQL string) (timing, event string) {
	upper := strings.ToUpper(triggerSQL)
	header := upper
	if beginIdx := strings.Index(upper, "BEGIN"); beginIdx >= 0 {
		header = upper[:beginIdx]
	}
	if strings.Contains(header, "INSTEAD OF") {
		timing = "BEFORE"
	} else if strings.Contains(header, "AFTER") {
		timing = "AFTER"
	} else if strings.Contains(header, "BEFORE") {
		timing = "BEFORE"
	}
	for _, ev := range []string{"INSERT", "UPDATE", "DELETE"} {
		if regexp.MustCompile(`\b` + ev + `\b`).MatchString(header) {
			event = ev
			break
		}
	}
	return timing, event
}

// IsPragmaTableFuncName reports whether name is one of the table-valued
// pragma functions (pragma_table_info, pragma_compile_options, ...).
func IsPragmaTableFuncName(name string) bool {
	return isPragmaTableFunc(name)
}

// PragmaArgsCorrelated reports whether a table-valued pragma or vtab
// function reference has any argument containing a column reference (i.e.
// it is correlated to the outer query). Nested references count too — e.g.
// json_tree(jsonb(big.json)) correlates on big.json just like json_each(t.j).
func PragmaArgsCorrelated(ref sql.TableRef) bool {
	for _, a := range ref.Args {
		if exprHasColumnRef(a) {
			return true
		}
	}
	return false
}

// pragmaArgsCorrelated is the unexported spelling kept for existing call
// sites in this package.
func pragmaArgsCorrelated(ref sql.TableRef) bool {
	return PragmaArgsCorrelated(ref)
}

// exprHasColumnRef reports whether any column reference appears anywhere in
// the expression tree.
func exprHasColumnRef(expr sql.Expr) bool {
	found := false
	WalkExprFull(expr, func(n sql.Expr) {
		if _, ok := n.(*sql.ColumnRef); ok {
			found = true
		}
	})
	return found
}

// relationExists reports whether name resolves to a CTE of this statement,
// a base or virtual table, or a view in the database schema. Used by the
// "'%s' is not a function" guard: the error applies only when the FROM term
// written with call syntax actually names an existing ordinary relation.
func (e *SelectEngine) relationExists(s *sql.SelectStmt, name string) bool {
	if _, ok := e.findCTE(s, name); ok {
		return true
	}
	if _, _, err := e.ctx.FindTable(name); err == nil {
		return true
	}
	_, _, err := e.ctx.FindView(name)
	return err == nil
}

// vtabCorrelatedInput extracts a correlated first-column equality constraint
// from a WHERE clause: `input = <column>` where the RHS is a column reference
// (fts3tokenize in a join — fts3tok1.test 1.13.2: `WHERE input = x AND
// c1.rowid=t1.rowid`). Returns the RHS column name and true when found; the
// column is resolved per left row at join materialization.
func vtabCorrelatedInput(where sql.Expr) (string, bool) {
	if cmp, ok := where.(*sql.BinaryOp); ok && strings.EqualFold(cmp.Operator, "AND") {
		if v, ok := vtabCorrelatedInput(cmp.Left); ok {
			return v, true
		}
		return vtabCorrelatedInput(cmp.Right)
	}
	cmp, ok := where.(*sql.BinaryOp)
	if !ok || cmp == nil || strings.ToUpper(cmp.Operator) != "=" {
		return "", false
	}
	var cr *sql.ColumnRef
	var rhs sql.Expr
	if c, ok := cmp.Left.(*sql.ColumnRef); ok {
		cr, rhs = c, cmp.Right
	} else if c, ok := cmp.Right.(*sql.ColumnRef); ok {
		cr, rhs = c, cmp.Left
	}
	if cr == nil || !strings.EqualFold(cr.Name, "input") {
		return "", false
	}
	col, ok := rhs.(*sql.ColumnRef)
	if !ok || col.Name == "" {
		return "", false
	}
	return col.Name, true
}

// vtabUpperBound extracts an upper bound on the vtab "value" column from a
// WHERE comparison (value < N / value <= N), for generate_series bounds.
func vtabUpperBound(where sql.Expr) (int64, bool) {
	cmp, ok := where.(*sql.BinaryOp)
	if !ok || cmp == nil {
		return 0, false
	}
	switch strings.ToUpper(cmp.Operator) {
	case "<", "<=":
		return boundFromColVal(cmp.Left, cmp.Right, strings.ToUpper(cmp.Operator) == "<")
	case ">", ">=":
		return boundFromColVal(cmp.Right, cmp.Left, strings.ToUpper(cmp.Operator) == ">")
	}
	return 0, false
}

// vtabInputConstraint extracts a literal equality constraint on a virtual
// table's first column from a WHERE clause of the form "col = <constant>"
// (fts3tokenize requires `input = <string>` — fts3_tokenize_vtab.c
// fts3tokBestIndexMethod). AND operands are searched so `input = 'a b c' AND
// token = 'b'` still supplies the input (fts3tok1 1.10/1.12). Returns
// (value, true) when found.
func vtabInputConstraint(where sql.Expr) (string, bool) {
	if cmp, ok := where.(*sql.BinaryOp); ok && strings.EqualFold(cmp.Operator, "AND") {
		if v, ok := vtabInputConstraint(cmp.Left); ok {
			return v, true
		}
		return vtabInputConstraint(cmp.Right)
	}
	cmp, ok := where.(*sql.BinaryOp)
	if !ok || cmp == nil || strings.ToUpper(cmp.Operator) != "=" {
		return "", false
	}
	var cr *sql.ColumnRef
	var rhs sql.Expr
	if c, ok := cmp.Left.(*sql.ColumnRef); ok {
		cr, rhs = c, cmp.Right
	} else if c, ok := cmp.Right.(*sql.ColumnRef); ok {
		cr, rhs = c, cmp.Left
	}
	if cr == nil {
		return "", false
	}
	// The constraint must target the first column (input).
	if !strings.EqualFold(cr.Name, "input") {
		return "", false
	}
	switch lit := rhs.(type) {
	case *sql.StringLit:
		return lit.Value, true
	case *sql.BlobLit:
		return string(lit.Value), true
	case *sql.NumericLit:
		return lit.Value, true
	case *sql.NullLit:
		// `input = NULL` supplies an empty input (SQLite's xFilter coerces
		// the NULL value to text "", producing zero tokens — fts3tok1 1.8).
		return "", true
	}
	return "", false
}

// boundFromColVal computes a vtab upper bound from a "value OP n" comparison.
// colSide must be the "value" column and numSide a numeric literal; when
// subtract is true (strict < or >) the bound is n∓1.
func boundFromColVal(colSide, numSide sql.Expr, subtract bool) (int64, bool) {
	cr, ok := colSide.(*sql.ColumnRef)
	if !ok || !strings.EqualFold(cr.Name, "value") {
		return 0, false
	}
	nl, ok := numSide.(*sql.NumericLit)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(nl.Value, 10, 64)
	if err != nil {
		return 0, false
	}
	if subtract {
		n--
	}
	return n, true
}

// parseVTabSQL parses a virtual-table CREATE SQL into its module name and
// argument strings.
func parseVTabSQL(sql string) (moduleName string, args []string, err error) {
	upper := strings.ToUpper(sql)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return "", nil, fmt.Errorf("vtab: invalid virtual table SQL: %s", sql)
	}
	rest := sql[idx+7:]
	parts := strings.SplitN(rest, "(", 2)
	moduleName = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		args = splitVTabArgs(parts[1])
	}
	return moduleName, args, nil
}

// splitVTabArgs splits a virtual-table module argument list (the text between
// the outer parentheses) into its individual arguments. Commas inside quoted
// strings ('...', "...", `...`, [...]) or inside nested parentheses do not
// separate arguments, matching the SQL parser's module-argument splitting
// (SQLite tokenizes the CREATE VIRTUAL TABLE argument list before passing it
// to the module constructor). This matters for FTS4 options whose values
// contain commas, e.g. prefix='1,3,6' or notindexed=a,b.
func splitVTabArgs(argsStr string) []string {
	var args []string
	var cur strings.Builder
	depth := 0
	var quote byte
	for i := 0; i < len(argsStr); i++ {
		c := argsStr[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				// Doubled quote is an escaped quote inside the string; only
				// close when not doubled (SQLite quote rules).
				if i+1 < len(argsStr) && argsStr[i+1] == quote {
					cur.WriteByte(argsStr[i+1])
					i++
				} else {
					quote = 0
				}
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
			cur.WriteByte(c)
		case c == '[':
			quote = ']'
			cur.WriteByte(c)
		case c == '(':
			depth++
			cur.WriteByte(c)
		case c == ')':
			depth--
			if depth < 0 {
				// Past the final close paren: stop.
				if s := strings.TrimSpace(cur.String()); s != "" {
					args = append(args, s)
				}
				return args
			}
			cur.WriteByte(c)
		case c == ',' && depth == 0:
			if s := strings.TrimSpace(cur.String()); s != "" {
				args = append(args, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		args = append(args, s)
	}
	return args
}

// buildColumnIndex builds a case-insensitive column-name→position index for
// the given column definitions. rowid/_rowid_/oid map to -1 (the pseudo-rowid)
// unless a real column shadows the alias. Mirrors the execution engine's
// helper of the same name so the query planner can build column indexes
// without depending on internal/exec.
func buildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	colIndex := make(map[string]int)
	for i, cd := range colDefs {
		colIndex[strings.ToLower(cd.Name)] = i
	}
	if !RowHasRowIDColumn(colDefs) {
		colIndex["rowid"] = -1
	}
	return colIndex
}

// isIPKRowidAliasCol reports whether a column is an INTEGER PRIMARY KEY
// (rowid alias): PRIMARY KEY, not DESC, INTEGER affinity.
func isIPKRowidAliasCol(cd sql.ColumnDef) bool {
	return cd.PrimaryKey && !cd.PKDesc && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER")
}

// indexColumnListText extracts the parenthesized column list from a
// CREATE INDEX statement (balanced-paren aware).
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

// parseIndexKeyCols parses a CREATE INDEX key column-list into stripped key
// expressions (plain names or expression text), removing COLLATE/ASC/DESC
// suffixes where they are not part of an expression.
func parseIndexKeyCols(colText string) []string {
	var cols []string
	for _, part := range splitIndexCols(colText) {
		name := strings.TrimSpace(part)
		upper := strings.ToUpper(name)
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

// constraintColumnNames resolves a table-level UNIQUE/PRIMARY KEY constraint's
// column list to declared column names (handling positional 1-based ordinals).
func constraintColumnNames(tc sql.TableConstraint, colIndex map[string]int, colDefs []sql.ColumnDef) []string {
	var names []string
	for _, ic := range tc.Columns {
		if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
			names = append(names, colDefs[n-1].Name)
			continue
		}
		if idx, ok := colIndex[ic.Name]; ok {
			names = append(names, colDefs[idx].Name)
		} else {
			names = append(names, ic.Name)
		}
	}
	return names
}
