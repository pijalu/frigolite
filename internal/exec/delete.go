// Package exec implements query execution.
package exec

import (
	"fmt"
	"sort"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- DELETE ---

func (e *Engine) execDelete(s *sql.DeleteStmt) *Result {
	if err := e.authorize(auth.ActionDelete, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, dbCtx, err := e.findTable(s.Table)
	if err != nil {
		// Not a table — route through INSTEAD OF DELETE triggers on a view.
		viewEntry, _, viewErr := e.findView(s.Table)
		if viewErr == nil {
			return e.execDeleteView(s, viewEntry)
		}
		return &Result{Error: err}
	}

	// Protect system and pragma virtual tables from modification.
	if e.isNonModifiableTable(tableEntry) {
		return &Result{Error: fmt.Errorf("table %s may not be modified", tableEntry.Name)}
	}

	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	if s.HasReturning {
		if err := e.validateReturning(s.Returning, colDefs, tableEntry.Name); err != nil {
			return &Result{Error: err}
		}
	}

	// Route FTS virtual table deletes
	if ftsTable, ok := e.ftsTables[tableEntry.Name]; ok {
		return e.execFTSDelete(ftsTable, colDefs, s)
	}

	tree := e.tableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)

	// Collect the rows that match the WHERE clause (needed for trigger firing
	// and RETURNING) before deleting them. Set the current scan table so
	// table-qualified column references ("t6.x") resolve to the row map.
	var deletedRows []RowMap
	{
		prevScan := e.currentScanTable
		e.currentScanTable = tableEntry.Name
		defer func() { e.currentScanTable = prevScan }()
		cursor, err := tree.OpenCursor()
		if err == nil {
			for {
				cell, err := cursor.ReadCell()
				if err != nil || cell == nil {
					break
				}
				rec, err := storage.DecodeRecord(cell.Payload)
				if err != nil {
					break
				}
				row := e.buildRowMap(rec, colDefs, cell.RowID)
				match, err := e.rowMatchesWhere(s.Where, row)
				if err != nil {
					return &Result{Error: err}
				}
				if match {
					deletedRows = append(deletedRows, row)
				}
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
			}
		}
	}

	// Apply DELETE ... ORDER BY ... LIMIT (a SQLite extension): sort the
	// matching rows by the ORDER BY expressions, then keep only the LIMIT
	// window. Without ORDER BY the rows are processed in rowid order; LIMIT
	// alone applies to that natural order.
	if len(s.OrderBy) > 0 {
		e.sortDeleteRows(deletedRows, s.OrderBy)
	}
	if s.Limit != nil {
		deletedRows = e.limitDeleteRows(deletedRows, s)
	}

	// Fire BEFORE DELETE triggers, delete the row, evaluate RETURNING
	// against the post-delete state, and fire AFTER DELETE triggers — one
	// row at a time (SQLite semantics; RETURNING subqueries must observe
	// the table without the current row).
	// Without RETURNING, all BEFORE triggers fire first, rows are deleted
	// in a single pass (O(n), whereas per-row delete is O(n²)), then all
	// AFTER triggers fire.
	if !s.HasReturning {
		// Snapshot the pager before any modification so a FOREIGN KEY
		// violation (or trigger error) can roll the statement back.
		snap := dbCtx.Pager.Snapshot()
		for _, row := range deletedRows {
			if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, unwrapRowMap(row)); trigResult.Error != nil {
				return trigResult
			}
		}

		deleted := int64(0)
		if len(deletedRows) > 0 {
			rowIDs := make(map[int64]struct{}, len(deletedRows))
			for _, row := range deletedRows {
				if rid, ok := util.UnwrapColumnValue(row["rowid"]).(int64); ok {
					rowIDs[rid] = struct{}{}
				}
			}
			n, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
				_, ok := rowIDs[cell.RowID]
				return ok
			})
			if err != nil {
				return &Result{Error: err}
			}
			deleted = n
			e.invalidateRowIDCache(tableEntry.RootPage)
		}

		for _, row := range deletedRows {
			if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, unwrapRowMap(row)); trigResult.Error != nil {
				return trigResult
			}
		}

		// Enforce FOREIGN KEY actions against the post-trigger state: a
		// child referencing a parent key that no longer exists triggers
		// RESTRICT/NO ACTION (error), CASCADE (delete), or SET NULL /
		// SET DEFAULT. The check runs after AFTER triggers because a trigger
		// may re-insert a parent row (restoring the referenced key).
		// On a RESTRICT/NO ACTION error the whole statement is rolled back
		// (SQLite statement journal), restoring the deleted rows.
		if e.foreignKeys {
			for _, row := range deletedRows {
				if res := e.fkParentDelete(tableEntry, colDefs, row); res.Error != nil {
					e.restorePager(dbCtx.Pager, snap)
					e.invalidateRowIDCache(tableEntry.RootPage)
					return res
				}
			}
		}
		return &Result{Changes: deleted}
	}

	// RETURNING path: process one row at a time so RETURNING subqueries
	// observe the table with the current row already removed.
	var returningRows [][]interface{}
	for _, row := range deletedRows {
		rowID, _ := util.UnwrapColumnValue(row["rowid"]).(int64)

		if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, unwrapRowMap(row)); trigResult.Error != nil {
			return trigResult
		}

		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(tableEntry.RootPage)

		values, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
		if err != nil {
			return &Result{Error: err}
		}
		returningRows = append(returningRows, values)

		if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, unwrapRowMap(row)); trigResult.Error != nil {
			return trigResult
		}
	}

	columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
	return &Result{Columns: columns, Rows: returningRows}
}

// execDeleteView routes DELETE on a view through INSTEAD OF DELETE triggers.
// The view's SELECT is executed (with the DELETE's WHERE applied) to find
// matching rows; for each, the trigger fires with OLD.* values.
func (e *Engine) execDeleteView(s *sql.DeleteStmt, viewEntry *schema.Entry) *Result {
	if !e.hasTriggersForTable(viewEntry.Name) {
		return &Result{Error: fmt.Errorf("cannot modify %s because it is a view", viewEntry.Name)}
	}
	// Qualified view column references (main.v5.x, v5.b) must resolve against
	// the view row during WHERE evaluation.
	prevDML := e.currentDMLTable
	e.currentDMLTable = viewEntry.Name
	defer func() { e.currentDMLTable = prevDML }()
	viewResult := e.execSelectView(viewEntry)
	if viewResult.Error != nil {
		return viewResult
	}
	viewCols := viewResult.Columns
	var changed int64
	for _, rowVals := range viewResult.Rows {
		oldRow := make(RowMap)
		for i, v := range rowVals {
			if i < len(viewCols) {
				oldRow[viewCols[i]] = v
			}
		}
		oldRow["rowid"] = nil
		if s.Where != nil {
			pass, err := e.evalBool(s.Where, oldRow)
			if err != nil || !pass {
				continue
			}
		}
		if res := e.fireTriggers(viewEntry.Name, "DELETE", "BEFORE", nil, oldRow); res != nil && res.Error != nil {
			return res
		}
		changed++
	}
	return &Result{Changes: changed}
}

// sortDeleteRows sorts the rows to delete by the ORDER BY expressions evaluated
// against each row's values (SQLite DELETE ... ORDER BY ... LIMIT).
func (e *Engine) sortDeleteRows(rows []RowMap, orderBy []sql.OrderByTerm) {
	if len(rows) <= 1 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, ob := range orderBy {
			left, _ := e.evalExpr(ob.Expr, rows[i])
			right, _ := e.evalExpr(ob.Expr, rows[j])
			cmp := compareOrderByValues(left, right, ob)
			if cmp < 0 {
				return true
			} else if cmp > 0 {
				return false
			}
		}
		return false
	})
}

// limitDeleteRows applies DELETE ... LIMIT n [OFFSET m] to the row list, keeping
// the first n entries after skipping m (SQLite semantics for DELETE LIMIT: the
// first N rows matched by the scan order are deleted).
func (e *Engine) limitDeleteRows(rows []RowMap, s *sql.DeleteStmt) []RowMap {
	limit, err := e.evalConstInt(s.Limit)
	if err != nil || limit < 0 {
		return rows
	}
	offset := int64(0)
	if s.Offset != nil {
		if v, err := e.evalConstInt(s.Offset); err == nil && v > 0 {
			offset = v
		}
	}
	if offset >= int64(len(rows)) {
		return nil
	}
	end := offset + limit
	if end > int64(len(rows)) {
		end = int64(len(rows))
	}
	return rows[offset:end]
}
