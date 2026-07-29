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

	deleted, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			return false
		}
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		return e.rowMatchesWhere(s.Where, row)
	})
	if err != nil {
		return &Result{Error: err}
	}

	// Fire AFTER DELETE triggers
	if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, nil); trigResult.Error != nil {
		return trigResult
	}

	return &Result{Changes: deleted}
}



