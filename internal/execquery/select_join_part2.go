// Package exec — JOIN execution functions extracted from select.go (file-level SRP).
// All functions remain methods on *SelectEngine in package internal/exec.
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// execJoins — top-level orchestrator

// execJoins processes all JOIN clauses in a SELECT statement, returning the
// combined row maps and column definitions after all joins are applied.

func (e *SelectEngine) collectJoinMergedColumns(s *sql.SelectStmt, names map[string]bool, merged map[string]bool) {
	if s == nil {
		return
	}
	var leftColSets [][]string
	// Operands resolve by their REAL table (an alias names no schema table):
	// "t1 AS t2 NATURAL RIGHT JOIN t1 AS t3" merges t1's columns on both
	// sides (join8-6000), so the NATURAL comparison must see them.
	operandTable := func(name, as string) string {
		if as != "" {
			return name
		}
		return name
	}
	addLeft := func(name, as string) {
		tn := operandTable(name, as)
		if tn == "" {
			return
		}
		if cols, err := e.tableColumnNames(tn); err == nil {
			leftColSets = append(leftColSets, cols)
		}
	}
	addLeft(s.From.Name, s.From.As)
	for _, join := range s.Joins {
		tn := operandTable(join.Table.Name, join.Table.As)
		rightCols, err := e.tableColumnNames(tn)
		if err != nil {
			addLeft(join.Table.Name, join.Table.As)
			continue
		}
		if len(join.Using) > 0 {
			mergeUsingCols(join.Using, merged)
		} else if isNaturalJoinType(join.JoinType) {
			mergeNaturalJoinCols(rightCols, leftColSets, merged)
		}
		leftColSets = append(leftColSets, rightCols)
	}
}

// mergeUsingCols records USING columns as merged.
func mergeUsingCols(using []string, merged map[string]bool) {
	for _, uc := range using {
		merged[strings.ToLower(uc)] = true
	}
}

// mergeNaturalJoinCols records columns common to the accumulated left tables
// and the current right table as merged (NATURAL JOIN semantics).
func mergeNaturalJoinCols(rightCols []string, leftColSets [][]string, merged map[string]bool) {
	rightSet := make(map[string]bool)
	for _, c := range rightCols {
		rightSet[strings.ToLower(c)] = true
	}
	for _, leftSet := range leftColSets {
		for _, c := range leftSet {
			if rightSet[strings.ToLower(c)] {
				merged[strings.ToLower(c)] = true
			}
		}
	}
}

// collectJoinTableCols

// collectJoinTableCols records the column names of a join operand (real
// table, CTE, or view) into availableCols so unqualified ON references
// resolve.
func (e *SelectEngine) collectJoinTableCols(s *sql.SelectStmt, join sql.JoinClause, tn string, availableCols map[string]bool) {
	colNames := join.Table.Name
	if colNames == "" {
		colNames = tn
	}
	if names, err := e.tableColumnNames(colNames); err == nil {
		addAllToSet(availableCols, names)
		return
	}
	cteDef, ok := e.findCTE(s, join.Table.Name)
	if !ok {
		e.addVTabFuncCols(join.Table, availableCols)
		return
	}
	if len(cteDef.Columns) > 0 {
		addAllToSet(availableCols, cteDef.Columns)
		return
	}
	if cteDef.Select == nil {
		return
	}
	if cols, err := e.resolveTableColumnNames(s, join.Table.Name); err == nil {
		addAllToSet(availableCols, cols)
		return
	}
	addSelectColumnsToSet(availableCols, cteDef.Select.Columns)
}

// addVTabFuncCols records a table-valued function operand's column names
// (e.g. generate_series).
func (e *SelectEngine) addVTabFuncCols(ref sql.TableRef, availableCols map[string]bool) {
	if ref.Args == nil {
		return
	}
	defs, _, _, err := e.ctx.MaterializeVtabTableFunc(ref, VtabScanOptions{})
	if err != nil {
		return
	}
	for _, d := range defs {
		if d.Name != "" {
			availableCols[d.Name] = true
		}
	}
}

// Shared helpers

// collectJoinPlainNames builds the set of column names available for ON-clause
// resolution: output aliases and base-table column definitions.
func collectJoinPlainNames(s *sql.SelectStmt, baseDefs []sql.ColumnDef) map[string]bool {
	plainNames := map[string]bool{}
	for _, col := range s.Columns {
		if col.As != "" {
			plainNames[col.As] = true
		}
	}
	for _, d := range baseDefs {
		plainNames[d.Name] = true
	}
	return plainNames
}

// addRightDefNames records right-side column names into the plain-names set.
func addRightDefNames(rightDefs []sql.ColumnDef, plainNames map[string]bool) {
	for _, d := range rightDefs {
		plainNames[d.Name] = true
	}
}

// selectCorrelatedRightMaps returns the right rows paired with a specific left
// row for a correlated pragma join, or all right rows when not correlated.
func selectCorrelatedRightMaps(rightMaps []RowMap, corrLeftIdx []int, leftIdx int) ([]RowMap, []int) {
	if corrLeftIdx == nil {
		return rightMaps, corrLeftIdx
	}
	var rowRightMaps []RowMap
	var rowCorrLeft []int
	for ri, li := range corrLeftIdx {
		if li == leftIdx {
			rowRightMaps = append(rowRightMaps, rightMaps[ri])
			rowCorrLeft = append(rowCorrLeft, 0) // placeholder; no further filtering
		}
	}
	return rowRightMaps, rowCorrLeft
}

// joinTableName returns the effective table name for a join's right operand
// (alias if present, else the table name).
func joinTableName(join sql.JoinClause) string {
	return aliasOrName(join.Table.Name, join.Table.As)
}

// aliasOrName returns the alias if present, else the name.
func aliasOrName(name, alias string) string {
	if alias != "" {
		return alias
	}
	return name
}

// resultColumnDefs builds ColumnDefs from result column names.
func resultColumnDefs(columns []string) []sql.ColumnDef {
	defs := make([]sql.ColumnDef, 0, len(columns))
	for _, colName := range columns {
		defs = append(defs, sql.ColumnDef{Name: colName})
	}
	return defs
}

// subqueryAffinity returns the affinity for subquery column i: the subquery's
// computed affinity if present, else the column def's type affinity.
func subqueryAffinity(subqAff []rune, i int, cd sql.ColumnDef) rune {
	if i < len(subqAff) && subqAff[i] != 0 {
		return subqAff[i]
	}
	return util.Affinity(cd.Type)
}

// subqueryColumnAffinity resolves the affinity of derived-table output column
// i: the subquery's expression affinity if present, else the result column
// def's declared type, else the type computed from the subquery's SELECT body
// (sqlite3SubqueryColumnTypes types a derived table's columns exactly like a
// view's — compound members refine the affinity across the UNION chain).
func (e *SelectEngine) subqueryColumnAffinity(subqAff []rune, i int, cd sql.ColumnDef, subquery *sql.SelectStmt) rune {
	if aff := subqueryAffinity(subqAff, i, cd); aff != 0 {
		return aff
	}
	if subquery != nil {
		if defs := e.ctx.ViewColumnDefsFromSelect(subquery); i < len(defs) {
			return util.Affinity(defs[i].Type)
		}
	}
	return 0
}

// unprefixedColName strips a "table." prefix from a column name.
func unprefixedColName(name string) string {
	if idx := strings.Index(name, "."); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

// rightDefBaseNames returns the set of unprefixed column names from right defs.
func rightDefBaseNames(rightDefs []sql.ColumnDef) map[string]bool {
	names := make(map[string]bool)
	for _, cd := range rightDefs {
		names[unprefixedColName(cd.Name)] = true
	}
	return names
}

// addAllToSet adds all strings to a set.
func addAllToSet(set map[string]bool, names []string) {
	for _, n := range names {
		set[n] = true
	}
}

// addSelectColumnsToSet records select-column aliases and bare column refs into
// a set.
func addSelectColumnsToSet(set map[string]bool, columns []sql.SelectColumn) {
	for _, col := range columns {
		if col.As != "" {
			set[col.As] = true
		} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			set[ref.Name] = true
		}
	}
}

// onQualifierKey returns the name-set lookup key for a qualified column
// reference: the schema prefix is stripped and the qualifier lower-cased,
// mirroring SQLite's case-insensitive name resolution (sqlite3StrICmp).
func onQualifierKey(cr *sql.ColumnRef) string {
	t := strings.ToLower(cr.Table)
	if dot := strings.LastIndexByte(t, '.'); dot >= 0 {
		t = t[dot+1:]
	}
	return t
}
