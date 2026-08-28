// Package execquery implements SELECT execution.
// This file owns FTS MATCH constraint validation (SQLite's "unable to use
// function MATCH in the requested context" rejection of unusable MATCH
// constraints), kept separate from select_agg_validate.go for file-size SRP.
package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
)

// validateMultipleFTSMatch rejects queries that use MATCH more than once on
// the same FTS virtual table, or that combine a cross-table MATCH with another
// MATCH. SQLite's FTS3 xBestIndex can use only one MATCH constraint per table;
// a second MATCH on the same table is an unusable constraint, and a MATCH
// whose RHS references a column of another joined table is unusable in some
// loop order, so the planner reports "unable to use function MATCH in the
// requested context" (fts3.c fts3BestIndexMethod, whereexpr.c
// isAuxiliaryVtabOperator). The check walks the WHERE and every JOIN ON clause.
func (e *SelectEngine) validateMultipleFTSMatch(s *sql.SelectStmt) error {
	type matchInfo struct {
		table    string
		crossTbl bool
	}
	var matches []matchInfo
	var matchErr error
	count := func(expr sql.Expr) {
		if expr == nil || matchErr != nil {
			return
		}
		WalkExprFull(expr, func(n sql.Expr) {
			bop, ok := n.(*sql.BinaryOp)
			if !ok || (bop.Operator != "MATCH" && bop.Operator != "NOT MATCH") {
				return
			}
			tableName := ftsMatchTableName(bop, e.ctx.FTSTables())
			if tableName == "" {
				// MATCH is only usable against an FTS table or one of its
				// columns; `rowid MATCH`/`docid MATCH` (any table) has no
				// match function and fails at prepare time (fts3cov 14.6,
				// 14.7; fts3.c sqlite3Fts3ExprLoad... via xBestIndex).
				if cref, ok := bop.Left.(*sql.ColumnRef); ok && cref.Table == "" && (IsRowIDName(cref.Name) || strings.EqualFold(cref.Name, "docid")) {
					matchErr = fmt.Errorf("unable to use function MATCH in the requested context")
				}
				return
			}
			matches = append(matches, matchInfo{
				table:    tableName,
				crossTbl: matchRHSCrossTable(bop, tableName, e.ctx.FTSTables()),
			})
		})
	}
	count(s.Where)
	for _, j := range s.Joins {
		count(j.On)
	}
	if matchErr != nil {
		return matchErr
	}
	// A MATCH constraint that references another joined table's column is
	// unusable when the planner scans the MATCH's table before that other
	// table is bound; combined with any other MATCH, SQLite rejects it.
	if len(matches) < 2 {
		return nil
	}
	perTable := make(map[string]int)
	for _, m := range matches {
		perTable[m.table]++
		if m.crossTbl {
			return fmt.Errorf("unable to use function MATCH in the requested context")
		}
	}
	for _, n := range perTable {
		if n > 1 {
			return fmt.Errorf("unable to use function MATCH in the requested context")
		}
	}
	return nil
}

// matchRHSCrossTable reports whether a MATCH expression's RHS references a
// column of a joined table other than the MATCH's own FTS table. A bare RHS
// column is resolved against all FTS tables' columns; a qualified RHS column
// is compared against the MATCH table name directly. A constant or
// non-column RHS is never cross-table.
func matchRHSCrossTable(bop *sql.BinaryOp, tableName string, ftsTables map[string]*fts.FTS3Table) bool {
	cross := false
	WalkExprFull(bop.Right, func(n sql.Expr) {
		if cross {
			return
		}
		cr, ok := n.(*sql.ColumnRef)
		if !ok {
			return
		}
		if cr.Table != "" {
			if !strings.EqualFold(cr.Table, tableName) {
				cross = true
			}
			return
		}
		// Bare column: cross-table when it is a column of a different FTS
		// table than the MATCH's own table.
		for tname, ft := range ftsTables {
			if strings.EqualFold(tname, tableName) {
				continue
			}
			for _, col := range ft.ColumnNames() {
				if strings.EqualFold(col, cr.Name) {
					cross = true
					return
				}
			}
		}
	})
	return cross
}

// ftsMatchTableName resolves the FTS table a MATCH expression targets: the
// qualified left operand's table, or a bare left operand that names an FTS
// table or a column of one. Returns "" when the expression is not an FTS
// MATCH (mirrors matchFTSLookup's table resolution without the row context).
func ftsMatchTableName(bop *sql.BinaryOp, ftsTables map[string]*fts.FTS3Table) string {
	colRef, ok := bop.Left.(*sql.ColumnRef)
	if !ok {
		return ""
	}
	if colRef.Table != "" {
		if _, ok := ftsTables[colRef.Table]; ok {
			return colRef.Table
		}
		return ""
	}
	if _, ok := ftsTables[colRef.Name]; ok {
		return colRef.Name
	}
	for tname, ft := range ftsTables {
		for _, col := range ft.ColumnNames() {
			if strings.EqualFold(col, colRef.Name) {
				return tname
			}
		}
	}
	return ""
}
