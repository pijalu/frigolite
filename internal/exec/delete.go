// Package exec implements query execution.
package exec

import (
	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// --- DELETE ---

func (e *Engine) execDelete(s *sql.DeleteStmt) *Result {
	if err := e.authorize(auth.ActionDelete, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, dbCtx, err := e.findTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// Route FTS virtual table deletes
	if ftsTable, ok := e.ftsTables[tableEntry.Name]; ok {
		return e.execFTSDelete(ftsTable, colDefs, s)
	}

	tree := e.tableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)

	// Collect the rows that match the WHERE clause (needed for trigger firing
	// and RETURNING) before deleting them.
	var deletedRows []RowMap
	{
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
				if e.rowMatchesWhere(s.Where, row) {
					deletedRows = append(deletedRows, row)
				}
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
			}
		}
	}

	// Fire BEFORE DELETE triggers for each matching row.
	for _, row := range deletedRows {
		if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, row); trigResult.Error != nil {
			return trigResult
		}
	}

	// Delete the matching cells in a single pass. Calling DeleteCellsWhere
	// per-row is O(n²): each call scans the entire tree for one rowid
	// (delete3.test deletes 262144 rows this way and hangs).
	deleted := int64(0)
	if len(deletedRows) > 0 {
		rowIDs := make(map[int64]struct{}, len(deletedRows))
		for _, row := range deletedRows {
			if rid, ok := row["rowid"].(int64); ok {
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

	// Fire AFTER DELETE triggers for each deleted row.
	for _, row := range deletedRows {
		if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, row); trigResult.Error != nil {
			return trigResult
		}
	}

	// Handle RETURNING clause — evaluate against deleted rows
	if s.HasReturning {
		var returningRows [][]interface{}
		for _, row := range deletedRows {
			values, err := e.evalReturningExprs(s.Returning, row, colDefs)
			if err != nil {
				return &Result{Error: err}
			}
			returningRows = append(returningRows, values)
		}
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: returningRows}
	}

	return &Result{Changes: deleted}
}



