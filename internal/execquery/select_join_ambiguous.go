// Package exec — join column-ambiguity validation extracted from
// select_join_validate.go (file-level SRP). Port of SQLite's prepare-time
// "ambiguous column name" detection across joined FROM operands.
package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// validateAmbiguousColumnRefs rejects unqualified column references that are
// ambiguous across the joined tables (SQLite: "ambiguous column name: X" at
// prepare time). Every table contributes its declared columns plus the
// implicit rowid/_rowid_/oid columns; a bare reference naming a column that
// exists in more than one joined table is ambiguous. Qualified references
// (t.col), TRUE/FALSE literals, and output-column aliases are exempt.
func (e *SelectEngine) validateAmbiguousColumnRefs(s *sql.SelectStmt) error {
	names := map[string]bool{}
	collectOuterTableNames(s, names)
	if len(names) == 0 {
		return nil
	}
	mergedCols := map[string]bool{}
	e.collectJoinMergedColumns(s, names, mergedCols)
	colInTables := e.buildAmbiguousColMap(names)
	// Derived-table operands (subquery FROM/JOIN) contribute an implicit
	// rowid/_rowid_/oid to the ambiguity map even though they have no real
	// table to resolve — a bare rowid over two derived tables is ambiguous
	// (misc8-3.0: "ambiguous column name: rowid").
	for _, ref := range derivedTableRefs(s) {
		for _, r := range []string{"rowid", "_rowid_", "oid"} {
			colInTables[r] = append(colInTables[r], ref)
		}
	}
	checker := ambiguousRefChecker{
		colInTables: colInTables,
		mergedCols:  mergedCols,
		names:       names,
		hasDerived:  selectHasSubqueryOperand(s),
	}
	return checker.checkClauses(s)
}

// derivedTableRefs returns the alias (or name) of every FROM/JOIN operand that
// is a subquery (derived table).
func derivedTableRefs(s *sql.SelectStmt) []string {
	var out []string
	if s.From.Subquery != nil {
		ref := s.From.Name
		if s.From.As != "" {
			ref = s.From.As
		}
		if ref != "" {
			out = append(out, ref)
		}
	}
	for _, j := range s.Joins {
		if j.Table.Subquery != nil {
			ref := j.Table.Name
			if j.Table.As != "" {
				ref = j.Table.As
			}
			if ref != "" {
				out = append(out, ref)
			}
		}
	}
	return out
}

// ambiguousRefChecker carries the precomputed column/table data needed to test
// individual column references for ambiguity.
type ambiguousRefChecker struct {
	colInTables map[string][]string
	mergedCols  map[string]bool
	names       map[string]bool
	hasDerived  bool
}

// checkClauses applies the ambiguity check to every clause in a SELECT that can
// reference columns (output columns, WHERE, GROUP BY, HAVING, ORDER BY).
func (c ambiguousRefChecker) checkClauses(s *sql.SelectStmt) error {
	if err := c.checkExprList(columnExprs(s.Columns)); err != nil {
		return err
	}
	if err := c.checkExpr(s.Where); err != nil {
		return err
	}
	if err := c.checkExprList(s.GroupBy); err != nil {
		return err
	}
	if err := c.checkExpr(s.Having); err != nil {
		return err
	}
	for _, ob := range s.OrderBy {
		if err := c.checkExpr(ob.Expr); err != nil {
			return err
		}
	}
	return nil
}

// columnExprs extracts the expression slice from a SELECT's output columns.
func columnExprs(cols []sql.SelectColumn) []sql.Expr {
	var exprs []sql.Expr
	for _, col := range cols {
		exprs = append(exprs, col.Expr)
	}
	return exprs
}

// checkExprList applies the ambiguity check to a list of expressions.
func (c ambiguousRefChecker) checkExprList(exprs []sql.Expr) error {
	for _, expr := range exprs {
		if err := c.checkExpr(expr); err != nil {
			return err
		}
	}
	return nil
}

// checkExpr walks a single expression and returns the first ambiguity error.
func (c ambiguousRefChecker) checkExpr(expr sql.Expr) error {
	if expr == nil {
		return nil
	}
	var checkErr error
	WalkExprFull(expr, func(e2 sql.Expr) {
		if checkErr != nil {
			return
		}
		ref, ok := e2.(*sql.ColumnRef)
		if !ok || ref.Name == "*" {
			return
		}
		checkErr = c.checkColumnRef(ref)
	})
	return checkErr
}

// checkColumnRef tests a single column reference for ambiguity or unknown
// qualifier.
func (c ambiguousRefChecker) checkColumnRef(ref *sql.ColumnRef) error {
	if ref.Table != "" {
		return c.checkQualifiedRef(ref)
	}
	if strings.EqualFold(ref.Name, "TRUE") || strings.EqualFold(ref.Name, "FALSE") {
		return nil
	}
	l := strings.ToLower(ref.Name)
	if c.mergedCols[l] {
		return nil
	}
	if len(c.colInTables[l]) > 1 {
		return fmt.Errorf("ambiguous column name: %s", ref.Name)
	}
	return nil
}

// checkQualifiedRef validates that a qualified reference (t.col) names a
// visible table. When a derived table is present in the FROM/JOIN operands,
// the qualifier may name a table inside the derived table (resolved at
// execution), so the check is skipped. NEW/OLD trigger references are exempt.
// A schema-qualified reference (main.t4.col) matches an operand written either
// way (main.t4 or t4), mirroring SQLite's db-qualified name resolution.
func (c ambiguousRefChecker) checkQualifiedRef(ref *sql.ColumnRef) error {
	if c.hasDerived {
		return nil
	}
	q := strings.ToLower(ref.Table)
	if q == "new" || q == "old" {
		return nil
	}
	if dot := strings.IndexByte(q, '.'); dot >= 0 {
		// A schema-qualified reference (main.t4.col) matches an operand
		// written either way (main.t4 or t4) — try both candidates.
		cands := map[string]bool{q: true, q[dot+1:]: true}
		for tn := range c.names {
			if cands[strings.ToLower(tn)] {
				return nil
			}
		}
		return fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)
	}
	for tn := range c.names {
		if strings.EqualFold(tn, q) {
			return nil
		}
	}
	return fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)
}

// selectHasSubqueryOperand reports whether a SELECT's FROM or JOIN operands
// include a subquery (derived table).
func selectHasSubqueryOperand(s *sql.SelectStmt) bool {
	if s.From.Subquery != nil {
		return true
	}
	for _, j := range s.Joins {
		if j.Table.Subquery != nil {
			return true
		}
	}
	return false
}

// buildAmbiguousColMap builds a map from lowercased column name to the list of
// table names that contain it. Each table contributes its declared columns
// plus the implicit rowid/_rowid_/oid pseudo-columns (unless WITHOUT ROWID).
func (e *SelectEngine) buildAmbiguousColMap(names map[string]bool) map[string][]string {
	colInTables := map[string][]string{}
	for tn := range names {
		cols, err := e.tableColumnNames(tn)
		if err != nil {
			// A table we cannot resolve (e.g. a CTE reference) — skip; the
			// execution path reports the missing table.
			continue
		}
		for _, c := range cols {
			l := strings.ToLower(c)
			colInTables[l] = append(colInTables[l], tn)
		}
		e.addRowidCols(colInTables, tn)
	}
	return colInTables
}

// addRowidCols adds the implicit rowid/_rowid_/oid columns for a table, unless
// it is declared WITHOUT ROWID (such tables have no rowid pseudo-column).
func (e *SelectEngine) addRowidCols(colInTables map[string][]string, tn string) {
	te, _, terr := e.ctx.FindTable(tn)
	if terr != nil || !e.ctx.HasWithoutRowidKeyword(strings.ToUpper(te.SQL)) {
		for _, r := range []string{"rowid", "_rowid_", "oid"} {
			colInTables[r] = append(colInTables[r], tn)
		}
	}
}
