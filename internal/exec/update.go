// Package exec implements query execution.
package exec

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- UPDATE ---

type updateChange struct {
	rowID  int64
	values []interface{}
}

func (e *Engine) execUpdate(s *sql.UpdateStmt) *Result {
	if err := e.authorize(auth.ActionUpdate, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, err := e.schema.FindTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	colIndex := buildColumnIndex(colDefs)

	changes, err := e.collectUpdateChanges(tableEntry.RootPage, colIndex, colDefs, s)
	if err != nil {
		return &Result{Error: err}
	}

	// Handle RETURNING clause — evaluate against updated rows before applying
	var returningRows [][]interface{}
	if s.HasReturning {
		for _, ch := range changes {
			row := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			values, err := e.evalReturningExprs(s.Returning, row, colDefs)
			if err != nil {
				return &Result{Error: err}
			}
			returningRows = append(returningRows, values)
		}
	}

	result := e.applyUpdateChanges(tableEntry.RootPage, changes)
	if result.Error != nil {
		return result
	}

	// Fire AFTER UPDATE triggers
	if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, nil, nil); trigResult.Error != nil {
		return trigResult
	}

	// If RETURNING clause was present, return result rows instead of change count
	if s.HasReturning {
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: returningRows}
	}

	return result
}

func buildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	colIndex := make(map[string]int)
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	colIndex["rowid"] = -1
	return colIndex
}

func (e *Engine) collectUpdateChanges(rootPage uint32, colIndex map[string]int, colDefs []sql.ColumnDef, s *sql.UpdateStmt) ([]updateChange, error) {
	tree := btree.NewBTree(e.pager, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, fmt.Errorf("exec: cursor error: %w", err)
	}

	var changes []updateChange
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}

		row := e.buildRowMap(rec, colDefs, cell.RowID)
		if e.rowMatchesWhere(s.Where, row) {
			ch, err := e.buildUpdateChange(cell, rec, colIndex, s, row)
			if err != nil {
				return nil, err
			}
			changes = append(changes, *ch)
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return changes, nil
}

func (e *Engine) buildUpdateChange(cell *storage.Cell, rec *storage.Record, colIndex map[string]int, s *sql.UpdateStmt, row Row) (*updateChange, error) {
	// Allocate values array large enough to hold all columns,
	// not just those present in the current record.
	maxIdx := len(rec.Values)
	for _, idx := range colIndex {
		if idx+1 > maxIdx {
			maxIdx = idx + 1
		}
	}
	values := make([]interface{}, maxIdx)
	copy(values, rec.Values)

	for _, a := range s.Assignments {
		idx, ok := colIndex[a.Column]
		if !ok {
			// Column not in schema - this happens when SQLite tests dynamically
			// add columns via PRAGMA writable_schema. Extend values array.
			idx = len(values)
			values = append(values, nil)
			colIndex[a.Column] = idx
		}
		v, err := e.evalExpr(a.Value, row)
		if err != nil {
			return nil, fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)
		}
		// Unwrap ColumnValue to avoid storing internal wrapper types
		// in the record — only raw values should be serialized.
		v = util.UnwrapColumnValue(v)
		if idx >= 0 && idx < len(values) {
			values[idx] = v
		}
	}
	return &updateChange{cell.RowID, values}, nil
}

func (e *Engine) rowMatchesWhere(where sql.Expr, row Row) bool {
	if where == nil {
		return true
	}
	match, err := e.evalBool(where, row)
	return err == nil && match
}

func (e *Engine) applyUpdateChanges(rootPage uint32, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}

	// Build a set of rowIDs to update
	type rowIDSet map[int64]bool
	toUpdate := make(rowIDSet, len(changes))
	for _, c := range changes {
		toUpdate[c.rowID] = true
	}

	tree := btree.NewBTree(e.pager, rootPage, true)

	// Step 1: Delete all existing rows in a single pass
	_, delErr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return toUpdate[cell.RowID]
	})
	if delErr != nil {
		return &Result{Error: delErr}
	}

	// Step 2: Insert all new rows
	for _, c := range changes {
		newRecord, err := storage.EncodeRecord(c.values)
		if err != nil {
			return &Result{Error: err}
		}
		newCell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   c.rowID,
			Payload: newRecord,
		}
		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
	}

	return &Result{Changes: int64(len(changes))}
}

