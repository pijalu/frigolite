package execquery

import (
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// This file holds the index-ordered scan emission contract: when a WHERE
// constraint lets an index drive a single-table scan, the surviving rows are
// emitted in that index's key order (SQLite's where.c outer loop), including
// the no-statistics default that prefers the seek.

// indexScanOrderIndex returns the name of the index whose key order this
// single-table scan's output must follow (mirroring planSingleTable's SEARCH
// decision, including the no-stats default that always prefers the seek), or
// "" when the output stays in table order.
func (e *SelectEngine) indexScanOrderIndex(s *sql.SelectStmt) string {
	if !indexOrderApplicable(s) {
		return ""
	}
	// A rowid / INTEGER PRIMARY KEY constraint makes the table b-tree itself
	// the driving index: rows come out in rowid order (intpkey-2.4.1
	// "WHERE 8>rowid AND 'second'>b" emits rowid order, beating the
	// secondary index on b).
	if e.whereConstrainsRowid(s.From.Name, s.Where) {
		return ""
	}
	nRow := e.tableRowCount(s.From.Name)
	est := float64(nRow)
	bestIndex, _ := e.bestIndexForQuery(s.From.Name, s.Where, &est)
	if bestIndex == "" || bestIndex == "PRIMARY KEY" {
		return ""
	}
	// Without a sqlite_stat1 row for the index, SQLite's default cost model
	// prices an index range seek at one tenth of a full scan, so the index
	// always drives; with stats the plan-level selectivity threshold applies.
	if len(e.stat1Tokens(bestIndex)) == 0 || (nRow > 0 && est < float64(nRow)*0.10) {
		return bestIndex
	}
	return ""
}

// indexOrderApplicable reports whether s is a plain single-table scan whose
// row emission order can follow an index (no joins, ordering, grouping,
// DISTINCT, or compound chain; FROM is a real table).
func indexOrderApplicable(s *sql.SelectStmt) bool {
	return s != nil && s.Where != nil && len(s.Joins) == 0 && len(s.OrderBy) == 0 &&
		len(s.GroupBy) == 0 && !s.Distinct && s.Union == nil &&
		s.From.Name != "" && s.From.Subquery == nil
}

// whereConstrainsRowid reports whether any WHERE conjunct constrains the
// rowid (or the table's INTEGER PRIMARY KEY rowid-alias column) with a
// comparison operator, so the table b-tree drives the scan.
func (e *SelectEngine) whereConstrainsRowid(tableName string, where sql.Expr) bool {
	colDefs, ok := e.rowidTableColDefs(tableName)
	if !ok {
		return false
	}
	shadowed := RowHasRowIDColumn(colDefs)
	for _, conj := range splitAnd(where) {
		if constrainsRowidCol(conj, tableName, colDefs, shadowed) {
			return true
		}
	}
	return false
}

// rowidTableColDefs loads a rowid table's column definitions. ok is false for
// missing tables and WITHOUT ROWID tables (which have no rowid).
func (e *SelectEngine) rowidTableColDefs(tableName string) ([]sql.ColumnDef, bool) {
	tableEntry, _, err := e.ctx.FindTable(tableName)
	if err != nil || tableEntry == nil {
		return nil, false
	}
	if e.ctx.HasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return nil, false
	}
	return e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL), true
}

// constrainsRowidCol reports whether one WHERE conjunct is a comparison on
// the rowid pseudo-column or an INTEGER PRIMARY KEY rowid-alias column.
func constrainsRowidCol(conj sql.Expr, tableName string, colDefs []sql.ColumnDef, shadowed bool) bool {
	bin, ok := conj.(*sql.BinaryOp)
	if !ok {
		return false
	}
	switch bin.Operator {
	case "=", "==", "<", "<=", ">", ">=":
	default:
		return false
	}
	return binaryRefRowidCol(bin, tableName, colDefs, shadowed)
}

// binaryRefRowidCol reports whether either operand of the comparison is a
// rowid reference (pseudo-column, unless shadowed by a declared column, or
// an INTEGER PRIMARY KEY rowid-alias column).
func binaryRefRowidCol(bin *sql.BinaryOp, tableName string, colDefs []sql.ColumnDef, shadowed bool) bool {
	for _, side := range [2]sql.Expr{bin.Left, bin.Right} {
		ref, ok := side.(*sql.ColumnRef)
		if !ok || (ref.Table != "" && !strings.EqualFold(ref.Table, tableName)) {
			continue
		}
		if isRowIDName(ref.Name) && !shadowed {
			return true
		}
		if cd, ok := findColDefByName(colDefs, ref.Name); ok && isIPKRowidAliasCol(cd) {
			return true
		}
	}
	return false
}

// sortScanRowsIndexOrder reorders scan output into the named index's key
// order: index columns in key order under each column's index collation
// (NULLs first), with rowid-ascending ties preserved by the stable sort. The
// sort keys come from the scan's row maps (the output rows may be a
// projection, e.g. "SELECT rowid, *" prepends the rowid), so it no-ops when
// maps are absent.
func (e *SelectEngine) sortScanRowsIndexOrder(rows [][]interface{}, maps []RowMap, tableName, idxName string) {
	idxCols := e.indexColumns(idxName)
	if len(idxCols) == 0 || len(rows) < 2 || len(maps) != len(rows) {
		return
	}
	colls := e.indexColumnCollations(tableName, idxName, idxCols)
	perm := make([]int, len(rows))
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(x, y int) bool {
		a, b := maps[perm[x]], maps[perm[y]]
		for k, col := range idxCols {
			cmp := e.ctx.CompareValuesCollate(
				util.UnwrapColumnValue(lookupRowMapValue(a, col)),
				util.UnwrapColumnValue(lookupRowMapValue(b, col)), colls[k])
			if cmp != 0 {
				return cmp < 0
			}
		}
		return false
	})
	e.permuteScanResults(rows, maps, perm)
}

// indexColumnCollations resolves the effective collation of each index column
// (explicit COLLATE in the index SQL, else the declared column collation).
func (e *SelectEngine) indexColumnCollations(tableName, idxName string, idxCols []string) []string {
	colls := make([]string, len(idxCols))
	for i, col := range idxCols {
		colls[i] = e.indexColumnCollation(tableName, idxName, col)
	}
	return colls
}

// permuteScanResults applies a row permutation to the scan's output rows and
// row maps in lockstep.
func (e *SelectEngine) permuteScanResults(rows [][]interface{}, maps []RowMap, perm []int) {
	sortedRows := make([][]interface{}, len(rows))
	sortedMaps := make([]RowMap, len(maps))
	for i, from := range perm {
		sortedRows[i] = rows[from]
		sortedMaps[i] = maps[from]
	}
	copy(rows, sortedRows)
	copy(maps, sortedMaps)
}
