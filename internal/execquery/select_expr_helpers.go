package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

func (e *SelectEngine) validateRowValueInList(v *sql.InList) error {
	if err := e.validateRowValueUse(v.Operand, false); err != nil {
		return err
	}
	for _, item := range v.List {
		if err := e.validateRowValueUse(item, false); err != nil {
			return err
		}
	}
	// Row-value IN subquery arity: (a,b) IN (SELECT * FROM t) requires
	// the subquery to return exactly len(operand) columns.
	if isRowValueExpr(v.Operand) && len(v.List) == 1 && isSubqueryExpr(v.List[0]) {
		arity := rowValueArity(v.Operand)
		if err := e.validateSubqueryArity(v.List[0], arity); err != nil {
			return err
		}
	}
	return nil
}

// isRowValueExpr reports whether expr is a row value (or a parenthesized row
// value).
func isRowValueExpr(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.RowValue:
		return true
	case *sql.ParenExpr:
		return isRowValueExpr(v.Expr)
	}
	return false
}

// rowValueArity returns the number of elements in a row value expression, or
// -1 if expr is not a row value.
func rowValueArity(expr sql.Expr) int {
	switch v := expr.(type) {
	case *sql.RowValue:
		return len(v.Values)
	case *sql.ParenExpr:
		return rowValueArity(v.Expr)
	}
	return -1
}

// isSubqueryExpr reports whether expr is a subquery (possibly parenthesized).
func isSubqueryExpr(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.Subquery:
		return true
	case *sql.ParenExpr:
		return isSubqueryExpr(v.Expr)
	}
	return false
}

// getCollationName extracts the collation name from a COLLATE expression's
// right operand (a StringLit or ColumnRef).
func getCollationName(expr sql.Expr) string {
	switch v := expr.(type) {
	case *sql.StringLit:
		return v.Value
	case *sql.ColumnRef:
		return v.Name
	}
	return ""
}

// validateSubqueryArity checks that a subquery returns exactly wantCols
// columns, raising "sub-select returns N columns - expected M" otherwise
// (SQLite: `(a,b) IN (SELECT x, y, z ...)` with a 3-column subquery). A
// `SELECT *` column is resolved to the table's column count via the schema.
func (e *SelectEngine) validateSubqueryArity(expr sql.Expr, wantCols int) error {
	sub := expr
	for {
		if p, ok := sub.(*sql.ParenExpr); ok {
			sub = p.Expr
			continue
		}
		break
	}
	sq, ok := sub.(*sql.Subquery)
	if !ok {
		return nil
	}
	n := e.subqueryColumnCount(sq.Select)
	if n != wantCols {
		return fmt.Errorf("sub-select returns %d columns - expected %d", n, wantCols)
	}
	return nil
}

// subqueryColumnCount returns the number of result columns a SELECT produces,
// resolving a single `SELECT *` column to the FROM table's column count.
func (e *SelectEngine) subqueryColumnCount(s *sql.SelectStmt) int {
	if len(s.Columns) != 1 {
		return len(s.Columns)
	}
	ref, ok := s.Columns[0].Expr.(*sql.ColumnRef)
	if !ok || ref.Name != "*" {
		return len(s.Columns)
	}
	// SELECT * FROM (subquery): star expands to the subquery's columns.
	// A nil/empty From (no FROM clause) contributes no columns.
	if s.From.Subquery != nil {
		return e.subqueryColumnCount(s.From.Subquery)
	}
	if s.From.Name == "" {
		return 0
	}
	entry, _, err := e.ctx.FindTable(s.From.Name)
	if err != nil {
		return len(s.Columns)
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	count := 0
	for _, cd := range colDefs {
		if !cd.Dropped {
			count++
		}
	}
	return count
}

// validateSelectColumnRefs checks that every column reference in a SELECT
// (select list, WHERE, GROUP BY, HAVING, ORDER BY) resolves to a column of
// the scanned table. SQLite reports unknown columns at prepare time; without
// this check an unknown column would silently evaluate to NULL.
//
// selectAliasMap builds the output-column alias map for a SELECT statement:
// alias name → select-list expression (e.g. "SELECT a AS x" maps x → a). The
// map is used at evaluation time so WHERE/GROUP BY/HAVING can reference an
// alias when the name is not a table column (SQLite resolves the reference to
// the alias's expression). Returns nil when the SELECT has no aliases.
func selectAliasMap(s *sql.SelectStmt) map[string]sql.Expr {
	if s == nil {
		return nil
	}
	var m map[string]sql.Expr
	for _, col := range s.Columns {
		if col.As != "" {
			if m == nil {
				m = make(map[string]sql.Expr)
			}
			m[strings.ToLower(col.As)] = col.Expr
		}
	}
	return m
}

// resolveAliasRef looks up an unqualified column reference in the enclosing
// SELECTs' output-column alias maps (innermost first). It returns the alias
// expression and true when found.
func (e *SelectEngine) resolveAliasRef(name string) (sql.Expr, bool) {
	for i := len(e.aliasStack) - 1; i >= 0; i-- {
		if expr, ok := e.aliasStack[i][strings.ToLower(name)]; ok {
			return expr, true
		}
	}
	return nil, false
}

// aliasStackTop reports whether name is an output-column alias in the
// innermost SELECT's alias map.
func (e *SelectEngine) aliasStackTop(name string) (sql.Expr, bool) {
	if len(e.aliasStack) == 0 {
		return nil, false
	}
	expr, ok := e.aliasStack[len(e.aliasStack)-1][strings.ToLower(name)]
	return expr, ok
}

// validateUnionSubqueryNoAggs checks that a subquery used in FROM does not
// contain aggregates inside a UNION ALL. SQLite prohibits this pattern:
// SELECT * FROM (SELECT 1 UNION ALL SELECT sum(x) FROM t) -- invalid
func validateUnionSubqueryNoAggs(s *sql.SelectStmt) error {
	if s.Union != nil {
		// SQLite rejects an aggregate in a UNION member only when the member
		// is not an aggregate query itself (no GROUP BY and no aggregate
		// context). A grouped SELECT (SELECT a, sum(b) FROM t GROUP BY a)
		// legitimately uses aggregates inside a UNION.
		checkMember := func(m *sql.SelectStmt) error {
			if len(m.GroupBy) == 0 && !hasAggregateInColumns(m.Columns) {
				if nested := findAggregateInSelect(m); nested != "" {
					return fmt.Errorf("misuse of aggregate: %s()", nested)
				}
			}
			return nil
		}
		if err := checkMember(s); err != nil {
			return err
		}
		if err := checkMember(s.Union); err != nil {
			return err
		}
	}
	// Recurse into nested FROM subqueries
	if s.From.Subquery != nil {
		return validateUnionSubqueryNoAggs(s.From.Subquery)
	}
	return nil
}

// hasAggregateInColumns reports whether any column expression is an aggregate
// function call (used to recognize aggregate queries without GROUP BY).
func hasAggregateInColumns(cols []sql.SelectColumn) bool {
	for _, c := range cols {
		if FindAggregateInExpr(c.Expr) != "" {
			return true
		}
	}
	return false
}

// findAggregateInSelect checks if a SELECT statement directly contains an aggregate function.
func findAggregateInSelect(s *sql.SelectStmt) string {
	for _, col := range s.Columns {
		if nested := FindAggregateInExpr(col.Expr); nested != "" {
			return nested
		}
	}
	return ""
}
func applyLimitOffset(rows [][]interface{}, limit, offset sql.Expr) [][]interface{} {
	if limit == nil {
		return rows
	}
	l, ok := sql.EvalNumber(limit)
	if !ok || l < 0 {
		// Can't evaluate or negative limit → no upper bound
		l = int64(len(rows))
	}
	o := int64(0)
	if offset != nil {
		o, _ = sql.EvalNumber(offset)
	}
	if o < 0 {
		o = 0
	}
	if o > int64(len(rows)) {
		return [][]interface{}{}
	}
	if l == 0 {
		return [][]interface{}{}
	}
	end := o + l
	if end > int64(len(rows)) {
		end = int64(len(rows))
	}
	return rows[o:end]
}

// lookupRowMapValue fetches a column value from a RowMap, trying both the
// qualified (alias.col / table.col) and unqualified forms.

// SubqueryColumnCount returns the number of output columns of a subquery
// SELECT. Exported for the expression evaluator's BETWEEN subquery arity.
func (e *SelectEngine) SubqueryColumnCount(s *sql.SelectStmt) int {
	return e.subqueryColumnCount(s)
}

// ResolveAliasRef looks up a SELECT output-column alias by name. Exported
// for the expression evaluator's alias resolution.
func (e *SelectEngine) ResolveAliasRef(name string) (sql.Expr, bool) {
	return e.resolveAliasRef(name)
}

// selectReferencesRowID reports whether the statement's output columns or
// WHERE clause contain an unqualified rowid/_rowid_/oid reference.
func selectReferencesRowID(s *sql.SelectStmt) bool {
	if s == nil {
		return false
	}
	check := func(ex sql.Expr) bool {
		found := false
		WalkExprFull(ex, func(e2 sql.Expr) {
			if cr, ok := e2.(*sql.ColumnRef); ok && cr.Table == "" && isRowIDName(cr.Name) {
				found = true
			}
		})
		return found
	}
	for _, col := range s.Columns {
		if check(col.Expr) {
			return true
		}
	}
	return check(s.Where)
}
