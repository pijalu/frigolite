package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns aggregate/outer-reference analysis and SELECT expression
// validation, extracted from select.go for single-responsibility cohesion.
//
// Two logical groups:
//   - Aggregate & outer-ref analysis: aggHasColumnRef, selectHasCorrelatedAggSubquery,
//     aggRefsMatchFromTable, subqueryOuterAggRef, aggRefsOuter, exprRefsOuterCol.
//   - SELECT expression validation: validateSelectExprs, validateSelectColumnRefs,
//     validateSelectRowValues, validateDistinctAggArgs.

// ---------------------------------------------------------------------------
// Aggregate & outer-reference analysis
// ---------------------------------------------------------------------------

// aggHasColumnRef reports whether any SELECT column is an aggregate function
// whose arguments contain a column reference (excluding bare "*" which means
// "all rows").

func (e *SelectEngine) rowValueSubqCorrelatedAggWidth(rowSide, subqSide sql.Expr) int {
	rv, ok := rowSide.(*sql.RowValue)
	if !ok {
		return 0
	}
	subq, ok := subqSide.(*sql.Subquery)
	if !ok || subq.Select == nil {
		return 0
	}
	if e.subqueryOuterAggRef(subq.Select) == "" {
		return 0
	}
	return len(rv.Values)
}

// whereSubqueryOuterAggRef walks a predicate expression for a scalar subquery
// whose aggregate references columns outside the subquery's own FROM (a
// correlated aggregate in a WHERE context — misuse).
func (e *SelectEngine) whereSubqueryOuterAggRef(expr sql.Expr) string {
	if expr == nil {
		return ""
	}
	if sub, ok := expr.(*sql.Subquery); ok && sub.Select != nil {
		if name := e.subqueryOuterAggRef(sub.Select); name != "" {
			return name
		}
	}
	for _, child := range aggValidateChildExprs(expr) {
		if name := e.whereSubqueryOuterAggRef(child); name != "" {
			return name
		}
	}
	return ""
}

// checkOrderByNestedAgg rejects aggregates in ORDER BY when the SELECT doesn't
// use aggregates and has no GROUP BY (catches aggregates nested inside
// expressions like 10+max(x)).
func (e *SelectEngine) checkOrderByNestedAgg(s *sql.SelectStmt) error {
	if len(s.OrderBy) == 0 || len(s.GroupBy) > 0 || e.inCompoundMember || e.hasAggregates(s.Columns) {
		return nil
	}
	for _, ob := range s.OrderBy {
		if nested := FindAggregateInExpr(ob.Expr); nested != "" {
			return fmt.Errorf("misuse of aggregate: %s()", nested)
		}
	}
	return nil
}

// validateSelectColumnRefs validates that every column reference in a SELECT
// resolves to a known table column, trigger row value, rowid alias, or
// output-column alias. Single-table queries only (no JOINs). allowRowID
// controls whether unqualified rowid/_rowid_/oid references are accepted even
// when no column is named that (real tables: yes; CTEs and views: no — with1
// 15.1 expects "no such column: rowid" on a recursive CTE reference).
func (e *SelectEngine) validateSelectColumnRefs(s *sql.SelectStmt, colDefs []sql.ColumnDef, tableName, fromAlias string, allowRowID bool) error {
	v := &columnRefValidator{
		engine:     e,
		colByName:  buildColNameMap(colDefs),
		tableName:  strings.ToLower(tableName),
		fromAlias:  strings.ToLower(fromAlias),
		allowRowID: allowRowID,
	}
	for _, col := range s.Columns {
		if err := v.walkColumns(col.Expr); err != nil {
			return err
		}
	}
	aliases := collectSelectAliases(s.Columns)
	if err := v.walkClause(s.Where, aliases); err != nil {
		return err
	}
	for _, g := range s.GroupBy {
		if err := v.walkClause(g, aliases); err != nil {
			return err
		}
	}
	if err := v.walkClause(s.Having, aliases); err != nil {
		return err
	}
	if !e.inCompoundMember {
		for _, ob := range s.OrderBy {
			if err := v.walkClause(ob.Expr, aliases); err != nil {
				return err
			}
		}
	}
	return nil
}

// columnRefValidator carries the table context needed to resolve column
// references during SELECT validation.
type columnRefValidator struct {
	engine     *SelectEngine
	colByName  map[string]bool
	tableName  string
	fromAlias  string
	allowRowID bool
}

// walkColumns walks SELECT-column expressions checking every ColumnRef (no
// alias awareness — SELECT columns define aliases, they don't use them).
func (v *columnRefValidator) walkColumns(expr sql.Expr) error {
	var checkErr error
	WalkExprFull(expr, func(en sql.Expr) {
		if checkErr != nil {
			return
		}
		if ref, ok := en.(*sql.ColumnRef); ok {
			checkErr = v.checkRef(ref)
		}
	})
	return checkErr
}

// walkClause walks WHERE/GROUP BY/HAVING/ORDER BY expressions with alias
// awareness: references to output-column aliases and enclosing-scope aliases
// are skipped.
func (v *columnRefValidator) walkClause(expr sql.Expr, aliases map[string]bool) error {
	if expr == nil {
		return nil
	}
	var checkErr error
	WalkExprFull(expr, func(en sql.Expr) {
		if checkErr != nil {
			return
		}
		ref, ok := en.(*sql.ColumnRef)
		if !ok {
			return
		}
		if aliases[strings.ToLower(ref.Name)] {
			return
		}
		// An unqualified reference may name an output-column alias of an
		// ENCLOSING SELECT (e.g. an inner subquery's WHERE referencing the
		// outer query's "SELECT expr AS aaa"); such references resolve
		// through the alias stack at evaluation time, so skip them here.
		if ref.Table == "" {
			if _, found := v.engine.resolveAliasRef(ref.Name); found {
				return
			}
		}
		checkErr = v.checkRef(ref)
	})
	return checkErr
}

// checkRef validates a single column reference against the table context.
func (v *columnRefValidator) checkRef(ref *sql.ColumnRef) error {
	if ref.Table != "" {
		return v.checkQualifiedRef(ref)
	}
	return v.checkUnqualifiedRef(ref)
}

// checkQualifiedRef handles table-qualified references (e.g. t.col, new.col).
func (v *columnRefValidator) checkQualifiedRef(ref *sql.ColumnRef) error {
	q := strings.ToLower(ref.Table)
	// NEW.col / OLD.col references are valid inside trigger bodies when the
	// named column actually exists in the trigger's row.
	if q == "new" && v.engine.ctx.TriggerNewRow() != nil {
		if _, ok := v.engine.ctx.TriggerNewRow().Get(ref.Name); ok {
			return nil
		}
	}
	if q == "old" && v.engine.ctx.TriggerOldRow() != nil {
		if _, ok := v.engine.ctx.TriggerOldRow().Get(ref.Name); ok {
			return nil
		}
	}
	// Strip a schema prefix (main./temp./aux.) for comparison.
	if dot := strings.LastIndex(q, "."); dot >= 0 {
		q = q[dot+1:]
	}
	if q != v.tableName && q != v.fromAlias {
		return fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)
	}
	return nil
}

// checkUnqualifiedRef handles bare column references (e.g. col, *).
func (v *columnRefValidator) checkUnqualifiedRef(ref *sql.ColumnRef) error {
	if ref.Name == "*" {
		return nil
	}
	// TRUE/FALSE are boolean literals, not column references (the parser
	// represents them as unqualified ColumnRefs).
	if strings.EqualFold(ref.Name, "TRUE") || strings.EqualFold(ref.Name, "FALSE") {
		return nil
	}
	if v.colByName[strings.ToLower(ref.Name)] {
		return nil
	}
	if v.allowRowID && isRowIDName(ref.Name) {
		return nil
	}
	// Double-quoted identifiers fall back to string literals when DQS is
	// enabled (handled at evaluation); do not reject them here.
	if ref.Quoted {
		return nil
	}
	return fmt.Errorf("no such column: %s", ref.Name)
}

// buildColNameMap creates a lower-cased column-name set from column defs.
func buildColNameMap(colDefs []sql.ColumnDef) map[string]bool {
	m := make(map[string]bool, len(colDefs))
	for _, cd := range colDefs {
		m[strings.ToLower(cd.Name)] = true
	}
	return m
}

// collectSelectAliases gathers lower-cased output-column aliases.
func collectSelectAliases(columns []sql.SelectColumn) map[string]bool {
	aliases := make(map[string]bool)
	for _, col := range columns {
		if col.As != "" {
			aliases[strings.ToLower(col.As)] = true
		}
	}
	return aliases
}

// validateSelectRowValues validates row-value usage across a SELECT's columns,
// WHERE, HAVING, LIMIT, OFFSET, and ORDER BY clauses.
func (e *SelectEngine) validateSelectRowValues(s *sql.SelectStmt) error {
	for _, col := range s.Columns {
		if err := e.validateRowValueUse(col.Expr, true); err != nil {
			return err
		}
	}
	if err := e.validateRowValueClause(s.Where, false); err != nil {
		return err
	}
	if err := e.validateRowValueClause(s.Having, false); err != nil {
		return err
	}
	if err := e.validateRowValueClause(s.Limit, true); err != nil {
		return err
	}
	if err := e.validateRowValueClause(s.Offset, true); err != nil {
		return err
	}
	for _, ob := range s.OrderBy {
		if err := e.validateRowValueUse(ob.Expr, false); err != nil {
			return err
		}
	}
	return nil
}

// validateRowValueClause validates a single optional expression for row-value
// misuse, returning nil when the expression is absent.
func (e *SelectEngine) validateRowValueClause(expr sql.Expr, scalarOnly bool) error {
	if expr == nil {
		return nil
	}
	return e.validateRowValueUse(expr, scalarOnly)
}

// validateDistinctAggArgs walks an expression tree and reports an error for
// any aggregate function used with DISTINCT but no arguments (SQLite:
// "DISTINCT aggregates must have exactly one argument"). Subqueries are not
// descended into — they have their own validation.
func validateDistinctAggArgs(expr sql.Expr) error {
	if expr == nil {
		return nil
	}
	if fn, ok := expr.(*sql.FuncCall); ok {
		if fn.Distinct && len(fn.Args) != 1 {
			return fmt.Errorf("DISTINCT aggregates must have exactly one argument")
		}
	}
	for _, child := range distinctAggChildExprs(expr) {
		if err := validateDistinctAggArgs(child); err != nil {
			return err
		}
	}
	return nil
}

// distinctAggChildExprs returns the immediate sub-expressions of expr for
// DISTINCT aggregate validation traversal, including FuncCall args.
func distinctAggChildExprs(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return v.Args
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.IsNull:
		return []sql.Expr{v.Operand}
	case *sql.IsNotNull:
		return []sql.Expr{v.Operand}
	case *sql.IsDistinctFrom:
		return []sql.Expr{v.Left, v.Right}
	case *sql.IsNotDistinctFrom:
		return []sql.Expr{v.Left, v.Right}
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		return append([]sql.Expr{v.Operand}, v.List...)
	case *sql.CaseExpr:
		return caseExprChildren(v)
	}
	return nil
}
