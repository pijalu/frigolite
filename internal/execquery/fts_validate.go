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
	// An unqualified MATCH column (a MATCH 'x') must resolve against THIS
	// query's FROM tables in FROM order: the connection-wide FTS table map
	// may hold other tables whose columns share the name (e_fts3 7.3.x:
	// t7(a,b) while an earlier t1(a,b) is still registered) — resolving via
	// map iteration is nondeterministic and splits two same-column MATCHes
	// across tables, missing the duplicate-MATCH rejection.
	queryTables := make([]string, 0, 1+len(s.Joins))
	if s.From.Name != "" {
		queryTables = append(queryTables, s.From.Name)
	}
	for _, j := range s.Joins {
		if j.Table.Name != "" {
			queryTables = append(queryTables, j.Table.Name)
		}
	}
	matches, matchErr := e.collectFTSMatches(s, queryTables)
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

// ftsMatchInfo carries one collected MATCH constraint: its resolved FTS table
// and whether the RHS references another joined table's column.
type ftsMatchInfo struct {
	table    string
	crossTbl bool
}

// collectFTSMatches gathers every MATCH constraint in WHERE and the join ON
// clauses. See ftsMatchTableName for the resolution rules; `rowid MATCH`/
// `docid MATCH` (any table) has no match function and fails at prepare time
// (fts3cov 14.6, 14.7; fts3.c sqlite3Fts3ExprLoad... via xBestIndex).
func (e *SelectEngine) collectFTSMatches(s *sql.SelectStmt, queryTables []string) ([]ftsMatchInfo, error) {
	var matches []ftsMatchInfo
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
			info, err := e.classifyFTSMatch(bop, queryTables)
			if err != nil {
				matchErr = err
				return
			}
			if info != nil {
				matches = append(matches, *info)
			}
		})
	}
	count(s.Where)
	for _, j := range s.Joins {
		count(j.On)
	}
	return matches, matchErr
}

// classifyFTSMatch resolves one MATCH constraint to its FTS table, or reports
// the unusable-MATCH error for a rowid/docid target.
func (e *SelectEngine) classifyFTSMatch(bop *sql.BinaryOp, queryTables []string) (*ftsMatchInfo, error) {
	tableName := ftsMatchTableName(bop, e.ctx.FTSTables(), queryTables)
	if tableName == "" {
		if matchOnRowidAlias(bop) {
			return nil, fmt.Errorf("unable to use function MATCH in the requested context")
		}
		return nil, nil
	}
	return &ftsMatchInfo{
		table:    tableName,
		crossTbl: matchRHSCrossTable(bop, tableName, e.ctx.FTSTables()),
	}, nil
}

// matchOnRowidAlias reports whether the MATCH's left operand is a bare
// rowid/docid alias (no match function; fails at prepare).
func matchOnRowidAlias(bop *sql.BinaryOp) bool {
	cref, ok := bop.Left.(*sql.ColumnRef)
	if !ok {
		return false
	}
	return cref.Table == "" && (IsRowIDName(cref.Name) || strings.EqualFold(cref.Name, "docid"))
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
		if bareColInOtherFTSTable(cr.Name, tableName, ftsTables) {
			cross = true
		}
	})
	return cross
}

// bareColInOtherFTSTable reports whether a bare column name is a column of an
// FTS table other than the given one.
func bareColInOtherFTSTable(name, selfTable string, ftsTables map[string]*fts.FTS3Table) bool {
	for tname, ft := range ftsTables {
		if strings.EqualFold(tname, selfTable) {
			continue
		}
		for _, col := range ft.ColumnNames() {
			if strings.EqualFold(col, name) {
				return true
			}
		}
	}
	return false
}

// ftsMatchTableName resolves the FTS table a MATCH expression targets: the
// qualified left operand's table, or a bare left operand that names an FTS
// table or a column of one. Returns "" when the expression is not an FTS
// MATCH (mirrors matchFTSLookup's table resolution without the row context).
// queryTables lists THIS query's FROM tables in FROM order: a bare column
// name resolves against them first (deterministically), falling back to the
// connection-wide FTS table scan only when none matches.
func ftsMatchTableName(bop *sql.BinaryOp, ftsTables map[string]*fts.FTS3Table, queryTables []string) string {
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
	if qt := queryTableForColumn(colRef.Name, queryTables, ftsTables); qt != "" {
		return qt
	}
	return anyFTSTableForColumn(colRef.Name, ftsTables)
}

// queryTableForColumn resolves a bare column name against THIS query's FROM
// tables in FROM order (deterministically).
func queryTableForColumn(name string, queryTables []string, ftsTables map[string]*fts.FTS3Table) string {
	for _, qt := range queryTables {
		ft, ok := ftsTables[qt]
		if !ok || ft == nil {
			continue
		}
		for _, col := range ft.ColumnNames() {
			if strings.EqualFold(col, name) {
				return qt
			}
		}
	}
	return ""
}

// anyFTSTableForColumn falls back to the connection-wide FTS table scan.
func anyFTSTableForColumn(name string, ftsTables map[string]*fts.FTS3Table) string {
	for tname, ft := range ftsTables {
		for _, col := range ft.ColumnNames() {
			if strings.EqualFold(col, name) {
				return tname
			}
		}
	}
	return ""
}
