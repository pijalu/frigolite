// SPDX-License-Identifier: GPL-3.0-or-later
package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/value"
)

// xferOptimizationApplies mirrors insert.c's xferOptimization DECISION (the
// function's full contract as far as the corpus sees it: insert4.test
// asserts sqlite3_xferopt_count, which SQLite increments right after these
// checks pass — insert.c:3235). The btree-transfer fast path itself changes
// no observable result, only speed; it is the queued F-lane lever.
//
// The rules, in the C's order:
//	no WITH anywhere; dest not virtual; exactly one FROM term, no subquery;
//	no WHERE/ORDER BY/GROUP BY/HAVING/LIMIT; not a compound SELECT; not
//	DISTINCT; the result set is exactly "*"; src resolves to an ordinary
//	table (not a view/vtab) and a different one; both have (or lack) a
//	rowid; same column count; same IPK ordinal; STRICT cannot feed STRICT;
//	per column: equal affinity, equal collation, dest NOT NULL implies src
//	NOT NULL, equal defaults (from the second column on), equal
//	generated-column parity (and expressions); every dest index has a
//	compatible src index (xferCompatibleIndex); equal CHECK constraints;
//	dest has no FK constraints (when FK enforcement is on).
func (e *DMLExecutor) xferOptimizationApplies(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) bool {
	if s == nil || s.Select == nil {
		return false
	}
	sel := s.Select
	if len(s.CTEs) > 0 || len(sel.CTEs) > 0 {
		return false
	}
	if strings.Contains(strings.ToUpper(tableEntry.SQL), " USING ") {
		return false
	}
	if sel.From.Name == "" || sel.From.Subquery != nil || len(sel.Joins) > 0 {
		return false
	}
	if sel.Where != nil || sel.OrderBy != nil || sel.GroupBy != nil || sel.Having != nil || sel.Limit != nil || sel.Offset != nil {
		return false
	}
	if sel.Union != nil || sel.SetOp != 0 {
		return false
	}
	if sel.Distinct {
		return false
	}
	if sel.ValuesChain {
		return false
	}
	if s.HasReturning {
		return false // RETURNING rows require the row-by-row writer
	}
	// The result set must be exactly "*".
	if len(sel.Columns) != 1 {
		return false
	}
	star, ok := sel.Columns[0].Expr.(*sql.ColumnRef)
	if !ok || star.Name != "*" || star.Table != "" {
		return false
	}
	// src must resolve to an ordinary table of the same schema context.
	srcEntry, err := e.ctx.Schema().FindTable(sel.From.Name)
	if err != nil || srcEntry == nil || srcEntry.Type != schema.TypeTable {
		return false
	}
	if srcEntry.RootPage == tableEntry.RootPage && srcEntry.Name == tableEntry.Name {
		return false
	}
	srcTable := parseCreateTableForConstraints(srcEntry.SQL)
	if srcTable == nil || len(colDefs) != len(srcTable.Columns) {
		return false
	}
	srcCols := srcTable.Columns
	destIPK := -1
	for i := range colDefs {
		if colDefs[i].PrimaryKey && !colDefs[i].PKDesc && isIntegerDeclaredType(colDefs[i].Type) {
			destIPK = i
		}
	}
	srcIPK := -1
	for i := range srcTable.Columns {
		c := srcTable.Columns[i]
		if c.PrimaryKey && !c.PKDesc && isIntegerDeclaredType(c.Type) {
			srcIPK = i
		}
	}
	if destIPK != srcIPK {
		return false
	}
	if tableIsWithoutRowid(tableEntry.SQL) != tableIsWithoutRowid(srcEntry.SQL) {
		return false
	}
	destTable := parseCreateTableForConstraints(tableEntry.SQL)
	if destTable == nil || srcTable == nil {
		return false
	}
	if destTable.Strict && !srcTable.Strict {
		return false
	}
	for i := range colDefs {
		d, sCol := colDefs[i], srcCols[i]
		if value.Affinity(d.Type) != value.Affinity(sCol.Type) {
			return false
		}
		if !strings.EqualFold(d.Collate, sCol.Collate) {
			return false
		}
		if d.NotNull && !sCol.NotNull {
			return false
		}
		if (d.Generated == nil) != (sCol.Generated == nil) {
			return false
		}
		if d.Generated != nil && !e.exprsStructurallyEqual(colDefs, srcCols, d.Generated, sCol.Generated) {
			return false
		}
		if d.Generated == nil && i > 0 {
			if (d.Default == nil) != (sCol.Default == nil) {
				return false
			}
			if d.Default != nil && !e.exprsStructurallyEqual(colDefs, srcCols, d.Default, sCol.Default) {
				return false
			}
		}
	}
	if !e.destIndexesCompatible(tableEntry.Name, srcEntry.Name, colDefs, srcTable.Columns) {
		return false
	}
	// CHECK constraints: dest checks require src checks that compare equal
	// (ticket #2252). A dest with no CHECK accepts anything.
	destChecks := tableChecks(destTable)
	srcChecks := tableChecks(srcTable)
	if len(destChecks) > 0 {
		if len(destChecks) != len(srcChecks) {
			return false
		}
		for i := range destChecks {
			if !e.exprsStructurallyEqual(destTable.Columns, srcTable.Columns, destChecks[i], srcChecks[i]) {
				return false
			}
		}
	}
	// Destination FK constraints only block the transfer when FK
	// enforcement is ON (insert.c: `db->flags & SQLITE_ForeignKeys`).
	if e.ctx.ForeignKeys() {
		for _, tc := range destTable.Constraints {
			if tc.Type == sql.ConstraintForeignKey {
				return false
			}
		}
		for i := range colDefs {
			if colDefs[i].References != "" {
				return false
			}
		}
	}
	return true
}

// isIntegerDeclaredType reports whether the declared type makes the column
// an INTEGER PRIMARY KEY candidate (exact "INTEGER" per SQLite's rule).
func isIntegerDeclaredType(t string) bool {
	return strings.EqualFold(strings.TrimSpace(t), "INTEGER")
}

