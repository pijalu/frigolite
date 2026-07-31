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

	// Collect deleted row data for RETURNING clause before deletion
	var deletedRows []RowMap

	deleted, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			return false
		}
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		if e.rowMatchesWhere(s.Where, row) {
			if s.HasReturning {
				deletedRows = append(deletedRows, row)
			}
			return true
		}
		return false
	})
	if err != nil {
		return &Result{Error: err}
	}

	// Fire AFTER DELETE triggers
	if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, nil); trigResult.Error != nil {
		return trigResult
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



