package execquery

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns SELECT clause validation, extracted from select.go for
// single-responsibility cohesion. It contains the 11 validation functions
// that check row-value usage, subquery arity, ORDER BY constraints, DML
// expression subqueries, trigger bodies, and compound-query ORDER BY terms.
//
// All functions here are in the same exec package and share the Engine
// receiver and helper functions that remain in select.go.

// ---------------------------------------------------------------------------
// ORDER BY length validation
// ---------------------------------------------------------------------------

// validateOrderByLength checks that no aggregate function's ORDER BY clause in
// expr exceeds limit terms. It recurses through all sub-expressions.

func (e *SelectEngine) validateCaseOrderBy(v *sql.CaseExpr) error {
	if v.Operand != nil {
		if err := e.validateExprOrderBy(v.Operand); err != nil {
			return err
		}
	}
	for _, w := range v.Whens {
		if err := e.validateExprOrderBy(w.When); err != nil {
			return err
		}
		if err := e.validateExprOrderBy(w.Then); err != nil {
			return err
		}
	}
	if v.Else != nil {
		return e.validateExprOrderBy(v.Else)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Compound ORDER BY validation
// ---------------------------------------------------------------------------

// validateCompoundOrderBy validates that every ORDER BY term in a compound
// query (UNION/INTERSECT/EXCEPT) matches a result column, an ordinal, an
// expression, or an aggregate.
// applyCompoundOrderByCollations wraps compound ORDER BY terms that have no
// explicit COLLATE with the compound result column's collation (when one is
// defined). SQLite's compound ORDER BY inherits the result column collation
// (first member with a defined collation wins — select.c
// sqlite3MultiSelectCollSeq), so an ordinal ORDER BY 1 must sort with that
// column's collation (with1 10.8.4.1: ORDER BY 1 over "SELECT a COLLATE
// nocase ..." sorts nocase) and a bare term naming a result column ("ORDER BY
// c" over "SELECT a,b,c COLLATE nocase ...", selectA-2.7) must resolve its
// position against the result column NAMES, not the collation list.
// resultCols holds the compound's output column names (leftmost member); colls
// holds the per-column collations ("" = BINARY). An explicit COLLATE on the
// term is preserved and wins over the column collation (resolve.c
// resolveCompoundOrderBy keeps the TK_COLLATE node).
func (e *SelectEngine) applyCompoundOrderByCollations(orderBy []sql.OrderByTerm, resultCols []string, colls []string) []sql.OrderByTerm {
	out := make([]sql.OrderByTerm, len(orderBy))
	copy(out, orderBy)
	for i := range out {
		ob := &out[i]
		if orderByTermCollation(ob.Expr) != "" {
			continue // explicit COLLATE already applied
		}
		if pos := compoundTermCollationPos(ob.Expr, resultCols); pos >= 1 && pos <= len(colls) && colls[pos-1] != "" {
			ob.Expr = &sql.BinaryOp{
				Operator: "COLLATE",
				Left:     ob.Expr,
				Right:    &sql.StringLit{Value: colls[pos-1]},
			}
		}
	}
	return out
}

// compoundTermCollationPos resolves the 1-based result-column position a
// compound ORDER BY term sorts by: an explicit ordinal, or a bare column
// name matched against the compound's result column names.
func compoundTermCollationPos(expr sql.Expr, resultCols []string) int {
	pos := 0
	if nl, ok := stripCollate(expr).(*sql.NumericLit); ok {
		if n, err := strconv.Atoi(nl.Value); err == nil && n >= 1 {
			pos = n
		}
	} else if ref, ok := stripCollate(expr).(*sql.ColumnRef); ok && ref.Table == "" {
		if p := resultColumnIndex(resultCols, ref.Name); p >= 0 {
			pos = p + 1
		}
	}
	return pos
}

// resolveCompoundOrderByTerms rewrites compound ORDER BY terms so a column
// reference that names a column of a NON-first compound member (e.g. `ORDER BY
// c` where c is d6's column) resolves to the result-column position that
// member contributes. SQLite sorts by that position; without the rewrite the
// term would evaluate to NULL against the merged result rows (which carry only
// the first member's column names).
func (e *SelectEngine) resolveCompoundOrderByTerms(s *sql.SelectStmt, orderBy []sql.OrderByTerm) []sql.OrderByTerm {
	for i := range orderBy {
		ob := &orderBy[i]
		ref, ok := unwrapCollate(ob.Expr).(*sql.ColumnRef)
		if ok && ref.Table == "" {
			// Resolve the term against the compound members leftmost-first
			// (resolve.c resolveCompoundOrderBy: resolveAsName against each
			// member's output list until one matches). The declaring member's
			// 1-based position within that member is the compound result
			// position — including a first-member name, which is rewritten to
			// an ordinal so the sort reads the merged result rows (selectB
			// "SELECT * FROM (SELECT e ... UNION ALL SELECT f ...) EXCEPT
			// SELECT c FROM t1 ORDER BY c": c is the RIGHT member's column and
			// the merged rows only carry the left member's names).
			if pos := e.compoundMemberColumnPosition(s, ref.Name); pos > 0 {
				ob.Expr = replaceCollateOperand(ob.Expr, &sql.NumericLit{Value: strconv.Itoa(pos)})
				continue
			}
		}
		// A compound ORDER BY expression that matches a member's SELECT
		// expression (e.g. ORDER BY x*z where the first member is
		// "SELECT x*z FROM d1") resolves to that result column.
		if pos := e.compoundMemberExprPosition(s, ob.Expr); pos > 0 {
			ob.Expr = replaceCollateOperand(ob.Expr, &sql.NumericLit{Value: strconv.Itoa(pos)})
		}
	}
	return orderBy
}

// replaceCollateOperand returns expr with its innermost COLLATE operand
// replaced by repl (or repl itself when expr carries no COLLATE). Mirrors
// resolve.c resolveCompoundOrderBy, which converts a resolved ORDER BY term
// to an integer column number "taking care to preserve the COLLATE clause if
// it exists" (pParent->pLeft = pNew through the TK_COLLATE chain).
func replaceCollateOperand(expr sql.Expr, repl sql.Expr) sql.Expr {
	if b, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(b.Operator, "COLLATE") {
		b.Left = replaceCollateOperand(b.Left, repl)
		return b
	}
	return repl
}

// compoundMemberExprPosition returns the 1-based result position of expr when
// it matches a compound member's SELECT expression, or 0.
//
// A table-qualified reference ("ORDER BY t6b.x" over "SELECT x XX ... FROM
// t6b") matches a member column written unqualified when the member's FROM
// binds that table or alias — resolve.c resolveCompoundOrderBy resolves the
// duplicated term against each member's sources and then compares it against
// that member's result list (tkt2822-6.5/6.6).
func (e *SelectEngine) compoundMemberExprPosition(s *sql.SelectStmt, expr sql.Expr) int {
	tref, isQualifiedRef := expr.(*sql.ColumnRef)
	if isQualifiedRef && tref.Table == "" {
		isQualifiedRef = false
	}
	cur := s
	for cur != nil {
		for i, col := range cur.Columns {
			if col.Expr != nil && sql.ExprString(expr) == sql.ExprString(col.Expr) {
				return i + 1
			}
			if isQualifiedRef && col.Expr != nil {
				if cref, ok := col.Expr.(*sql.ColumnRef); ok &&
					cref.Table == "" && cref.Name != "*" &&
					strings.EqualFold(cref.Name, tref.Name) &&
					compoundMemberBindsTable(cur, tref.Table) {
					return i + 1
				}
			}
		}
		cur = cur.Union
	}
	return 0
}

// compoundMemberBindsTable reports whether member's FROM clause declares the
// given table or alias (case-insensitive).
func compoundMemberBindsTable(member *sql.SelectStmt, table string) bool {
	if table == "" {
		return false
	}
	if strings.EqualFold(member.From.Name, table) || strings.EqualFold(member.From.As, table) {
		return true
	}
	for _, j := range member.Joins {
		if strings.EqualFold(j.Table.Name, table) || strings.EqualFold(j.Table.As, table) {
			return true
		}
	}
	return false
}

// compoundMemberColumnPosition returns the 1-based position of name within the
// compound member that declares it (columns are aligned by position across
// members), or 0 when no member declares it.
//
// Within a member an AS-alias match takes precedence over a source-column
// match (resolve.c resolveCompoundOrderBy tries resolveAsName — which only
// matches AS-names — before the resolve-and-compare expression rule;
// tkt2822-3.4: over "SELECT a AS b, CAST(b AS TEXT) AS a, c ... ORDER BY a"
// the leftmost member's alias a (column 2) wins over its source column a
// (column 1)).
func (e *SelectEngine) compoundMemberColumnPosition(s *sql.SelectStmt, name string) int {
	cur := s
	for cur != nil {
		for i, col := range cur.Columns {
			if col.As != "" && strings.EqualFold(col.As, name) {
				return i + 1
			}
		}
		for i, col := range cur.Columns {
			if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name != "*" && strings.EqualFold(ref.Name, name) {
				return i + 1
			}
		}
		cur = cur.Union
	}
	return 0
}

func (e *SelectEngine) validateCompoundOrderBy(s *sql.SelectStmt, orderBy []sql.OrderByTerm) error {
	colNames := make(map[string]bool)
	e.collectCompoundColumnNames(s, colNames)
	for i, ob := range orderBy {
		// SQLite resolves each ORDER BY term's nested subqueries (raising
		// their errors) before deciding whether the term matches a result
		// column (window1 67.1: a nested (SELECT 1 FROM v1) in a window's
		// ORDER BY surfaces "no such table: v1", not "term does not match").
		if err := e.validateExprSubqueries(ob.Expr); err != nil {
			return err
		}
		if e.compoundOrderByTermMatches(s, ob, colNames) {
			continue
		}
		return fmt.Errorf("%d%s ORDER BY term does not match any column in the result set",
			i+1, ordinalSuffix(i+1))
	}
	return nil
}

// collectCompoundColumnNames collects all available result column names across
// all compound members into colNames.
func (e *SelectEngine) collectCompoundColumnNames(s *sql.SelectStmt, colNames map[string]bool) {
	cur := s
	for cur != nil {
		e.collectMemberColumnNames(cur, colNames)
		cur = cur.Union
	}
}

// collectMemberColumnNames collects column names from a single compound member.
func (e *SelectEngine) collectMemberColumnNames(m *sql.SelectStmt, colNames map[string]bool) {
	for _, col := range m.Columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name != "*" {
			colNames[strings.ToLower(ref.Name)] = true
		}
		if col.As != "" {
			colNames[strings.ToLower(col.As)] = true
		}
	}
	for _, col := range m.Columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			e.collectStarColumns(m, ref.Table, colNames)
		}
	}
}

// collectStarColumns resolves a SELECT * table's columns and adds them to
// colNames.
func (e *SelectEngine) collectStarColumns(m *sql.SelectStmt, tbl string, colNames map[string]bool) {
	if tbl == "" {
		tbl = m.From.Name
	}
	if tbl == "" {
		return
	}
	cols, err := e.resolveTableColumnNames(m, tbl)
	if err != nil {
		return
	}
	for _, n := range cols {
		colNames[strings.ToLower(n)] = true
	}
}

// compoundOrderByTermMatches checks whether a single ORDER BY term is valid in
// a compound query.
func (e *SelectEngine) compoundOrderByTermMatches(s *sql.SelectStmt, ob sql.OrderByTerm, colNames map[string]bool) bool {
	expr := unwrapCollate(ob.Expr)
	if isOrdinal(expr) {
		return true
	}
	if ref, ok := expr.(*sql.ColumnRef); ok && colNames[strings.ToLower(ref.Name)] {
		return true
	}
	if e.expressionMatchesCompoundResult(s, expr) {
		return true
	}
	if ref, ok := expr.(*sql.ColumnRef); ok && ref.Table == "" && isRowIDName(ref.Name) {
		return true
	}
	if e.exprHasAggregate(ob.Expr) {
		return true
	}
	return false
}

// unwrapCollate strips chained COLLATE operators from an expression, returning
// the inner expression.
func unwrapCollate(expr sql.Expr) sql.Expr {
	for {
		bop, ok := expr.(*sql.BinaryOp)
		if !ok || !strings.EqualFold(bop.Operator, "COLLATE") {
			break
		}
		expr = bop.Left
	}
	return expr
}

// isOrdinal reports whether expr is a positive-integer ordinal literal.
func isOrdinal(expr sql.Expr) bool {
	nl, ok := expr.(*sql.NumericLit)
	if !ok {
		return false
	}
	_, ok = parsePositiveInt(nl.Value)
	return ok
}
