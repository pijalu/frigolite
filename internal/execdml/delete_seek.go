package execdml

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// seekDeleteRows collects DELETE candidate rows through a point-lookup plan:
// rowid = <const> seeks the table b-tree directly, col = <const> reads the
// driving index and seeks each candidate rowid. ok=false falls back to the
// full scan (no plan, or a candidate lookup/evaluation anomaly).
func (e *DMLExecutor) seekDeleteRows(tree *btree.BTree, s *sql.DeleteStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) ([]*dmlRow, bool) {
	plan := e.planDMLSeek(tableEntry, colDefs, s.Where, tableEntry.Name, e.currentDMLCtx)
	if plan == nil {
		return nil, false
	}
	rowIDs, ok := e.seekCandidateRowIDs(tableEntry.Name, tableEntry.RootPage, plan, colDefs)
	if !ok {
		return nil, false
	}
	// Small candidate sets keep the map representation: the per-statement
	// positional plan only amortizes from ~5 candidates up, and a point
	// DELETE ("WHERE id=<const>") must not pay for it.
	if len(rowIDs) <= deleteSeekMapPathMaxCandidates {
		return e.seekDeleteRowsMaps(tree, s, tableEntry, colDefs, rowIDs)
	}
	rowPlan := e.ctx.NewDMLRowPlan(colDefs, []sql.Expr{s.Where}, s.OrderBy)
	srow := rowPlan.NewRow()
	var deletedRows []*dmlRow
	for _, rowID := range rowIDs {
		// SQLITE_TEST interrupt countdown: one op per row examined
		// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count).
		if err := e.ctx.CheckProgress(); err != nil {
			return nil, false
		}
		values, realRowID, found, err := e.fetchSeekRowValues(tree, tableEntry.Name, tableEntry.RootPage, tableEntry.SQL, colDefs, rowID)
		if err != nil {
			return nil, false // the scan fallback re-evaluates and surfaces it
		}
		if !found {
			// The pinned rowid has no row: a no-match point DELETE ("DELETE
			// FROM t WHERE id=<absent>") or a stale index candidate. The
			// equality conjunct pins the candidate set exactly, so the
			// candidate contributes nothing — falling back to the full scan
			// here degraded every no-match point DELETE to O(table)
			// (PERF_REPORT 3.4).
			continue
		}
		e.ctx.FillDMLRow(rowPlan, srow, values, len(values), realRowID)
		match, err := e.rowMatchesWhere(s.Where, srow)
		if err != nil {
			return nil, false // the scan fallback re-evaluates and surfaces it
		}
		if match {
			deletedRows = append(deletedRows, &dmlRow{
				plan:       rowPlan,
				values:     e.ctx.DMLRowSnapshot(rowPlan, values, len(values), realRowID),
				valueCount: len(values),
				rowID:      realRowID,
			})
		}
	}
	return deletedRows, true
}

// deleteSeekMapPathMaxCandidates is the seek candidate count below which the
// DELETE collect keeps the per-row map representation (the positional plan's
// fixed cost outweighs one or two row maps).
const deleteSeekMapPathMaxCandidates = 4

// seekDeleteRowsMaps is seekDeleteRows' map-backed small-candidate form: the
// full WHERE evaluates against the row map built per candidate (the exact
// collected map the scan built before positional collection), and matched
// rows are retained as dmlRows wrapping that map.
func (e *DMLExecutor) seekDeleteRowsMaps(tree *btree.BTree, s *sql.DeleteStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, rowIDs []int64) ([]*dmlRow, bool) {
	var deletedRows []*dmlRow
	for _, rowID := range rowIDs {
		// SQLITE_TEST interrupt countdown: one op per row examined
		// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count).
		if err := e.ctx.CheckProgress(); err != nil {
			return nil, false
		}
		row, found, err := e.fetchSeekRowMap(tree, tableEntry.Name, tableEntry.RootPage, tableEntry.SQL, colDefs, rowID)
		if err != nil {
			return nil, false // the scan fallback re-evaluates and surfaces it
		}
		if !found {
			continue // the pinned rowid has no row (see seekDeleteRows)
		}
		match, err := e.rowMatchesWhere(s.Where, row)
		if err != nil {
			return nil, false // the scan fallback re-evaluates and surfaces it
		}
		if match {
			deletedRows = append(deletedRows, e.dmlRowFromMap(row, colDefs))
		}
	}
	return deletedRows, true
}

// dmlRowFromMap wraps an already-materialized name-keyed map into a dmlRow:
// values is the map's positional extraction (the raw declared-order values
// the map was built from, rowid-alias substituted), and the map serves as
// the row's lazy map.
func (e *DMLExecutor) dmlRowFromMap(row RowMap, colDefs []sql.ColumnDef) *dmlRow {
	rowID, _ := rowTrueRowID(row)
	return &dmlRow{
		values:     e.rowMapColumnValues(row, colDefs),
		valueCount: len(colDefs),
		rowID:      rowID,
		m:          row,
	}
}
