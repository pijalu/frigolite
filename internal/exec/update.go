// Package exec implements query execution.
package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- UPDATE ---

type updateChange struct {
	rowID     int64
	values    []interface{}
	oldValues []interface{}
}

func (e *Engine) execUpdate(s *sql.UpdateStmt) *Result {
	if err := e.authorize(auth.ActionUpdate, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, err := e.schema.FindTable(s.Table)
	if err != nil {
		// Not a table — route through INSTEAD OF UPDATE triggers on a view.
		viewEntry, _, viewErr := e.findView(s.Table)
		if viewErr == nil {
			return e.execUpdateView(s, viewEntry)
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
			values, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
			if err != nil {
				return &Result{Error: err}
			}
			returningRows = append(returningRows, values)
		}
	}

	// Enforce FOREIGN KEY constraints on the new values (PRAGMA foreign_keys).
	if e.foreignKeys {
		for _, ch := range changes {
			if res := e.checkForeignKeyViolations(tableEntry, colDefs, ch.values); res.Error != nil {
				return res
			}
		}
	}

	var result *Result
	if strings.EqualFold(s.OnConflict, "REPLACE") {
		// UPDATE OR REPLACE: apply each change incrementally, deleting
		// conflicting rows (with DELETE triggers) as we go.
		result = e.applyUpdateReplace(tableEntry, colDefs, changes)
	} else if strings.EqualFold(s.OnConflict, "IGNORE") {
		// UPDATE OR IGNORE: rows whose new values conflict with a
		// UNIQUE/PK constraint are skipped without error.
		result = e.applyUpdateIgnore(tableEntry, colDefs, changes)
	} else if e.hasTriggersForTable(tableEntry.Name) {
		// Plain UPDATE with triggers: SQLite fires BEFORE UPDATE triggers
		// per-row before the row is written. A BEFORE trigger may delete the
		// row being updated (or other rows); SQLite then skips writing rows
		// that no longer exist and checks UNIQUE/PK constraints against the
		// live table state. Process changes one row at a time.
		result = e.applyUpdateWithTriggers(tableEntry, colDefs, changes)
	} else {
		// Plain UPDATE: check UNIQUE/PK constraints on the new values
		// (SQLite errors on conflicts; there is no REPLACE resolution).
		if res := e.checkUpdateConflicts(tableEntry, colDefs, changes); res.Error != nil {
			return res
		}
		// Enforce FOREIGN KEY parent actions: children referencing the old
		// key values are restricted (error) or cascaded/updated.
		if e.foreignKeys {
			for _, ch := range changes {
				oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
				newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
				if res := e.fkParentUpdate(tableEntry, colDefs, oldRow, newRow, ch.rowID); res.Error != nil {
					return res
				}
			}
		}
		result = e.applyUpdateChanges(tableEntry.RootPage, changes)
	}
	if result.Error != nil {
		return result
	}

	// Fire AFTER UPDATE triggers with the new and old row values. The
	// applyUpdateWithTriggers and applyUpdateIgnore paths fire AFTER
	// triggers themselves, so skip this block there.
	afterTriggersFired := false
	if strings.EqualFold(s.OnConflict, "REPLACE") {
		afterTriggersFired = false // applyUpdateReplace fires DELETE triggers, not UPDATE
	} else if e.hasTriggersForTable(tableEntry.Name) {
		afterTriggersFired = true // applyUpdateWithTriggers / applyUpdateIgnore fired them
	}
	if e.hasTriggersForTable(tableEntry.Name) && !afterTriggersFired {
		for _, ch := range changes {
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, newRow, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
	}

	// Direct edits to sqlite_schema (PRAGMA writable_schema=ON) are schema
	// changes: re-read the schema btree on the next table lookup.
	if isSchemaTable(tableEntry.Name) {
		e.schema.InvalidateCache()
		e.tableCache = make(map[string]*cachedTableEntry)
	}

	// If RETURNING clause was present, return result rows instead of change count
	if s.HasReturning {
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: returningRows}
	}

	return result
}

// execUpdateView routes UPDATE on a view through INSTEAD OF UPDATE triggers.
// The view's SELECT is executed (with the UPDATE's WHERE applied) to find
// matching rows; for each, the trigger fires with OLD.* and NEW.* values
// where NEW reflects the SET clause applied to the view's output columns.
func (e *Engine) execUpdateView(s *sql.UpdateStmt, viewEntry *schema.Entry) *Result {
	if !e.hasTriggersForTable(viewEntry.Name) {
		return &Result{Error: fmt.Errorf("cannot modify %s because it is a view", viewEntry.Name)}
	}
	viewResult := e.execSelectView(viewEntry)
	if viewResult.Error != nil {
		return viewResult
	}
	viewCols := viewResult.Columns
	if len(viewCols) == 0 {
		return &Result{}
	}
	// Convert each view row into a RowMap keyed by the view's column names.
	var changed int64
	colDefs := make([]sql.ColumnDef, len(viewCols))
	for i, c := range viewCols {
		colDefs[i] = sql.ColumnDef{Name: c}
	}
	for _, rowVals := range viewResult.Rows {
		oldRow := make(RowMap)
		for i, v := range rowVals {
			if i < len(viewCols) {
				oldRow[viewCols[i]] = v
			}
		}
		oldRow["rowid"] = nil
		// Apply the WHERE clause against the view row.
		if s.Where != nil {
			pass, err := e.evalBool(s.Where, oldRow)
			if err != nil || !pass {
				continue
			}
		}
		// Build the NEW row by applying SET assignments to the old values.
		newRow := make(RowMap, len(oldRow))
		for k, v := range oldRow {
			newRow[k] = v
		}
		for _, a := range s.Assignments {
			v, err := e.evalExpr(a.Value, oldRow)
			if err != nil {
				return &Result{Error: fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)}
			}
			newRow[a.Column] = util.UnwrapColumnValue(v)
		}
		if res := e.fireTriggers(viewEntry.Name, "UPDATE", "BEFORE", newRow, oldRow); res != nil && res.Error != nil {
			return res
		}
		changed++
	}
	return &Result{Changes: changed}
}

func buildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	colIndex := make(map[string]int)
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	colIndex["rowid"] = -1
	return colIndex
}

// cdIndex returns the column index for name, or -1 if not found.
func cdIndex(colDefs []sql.ColumnDef, name string) int {
	for i, cd := range colDefs {
		if strings.EqualFold(cd.Name, name) {
			return i
		}
	}
	return -1
}

// originalColumnName returns the column name as spelled in the CREATE TABLE
// SQL (preserving original case for error messages), or the uppercased name
// if it cannot be found.
func (e *Engine) originalColumnName(createSQL, colName string) string {
	start := strings.IndexByte(createSQL, '(')
	end := strings.LastIndexByte(createSQL, ')')
	if start < 0 || end <= start {
		return colName
	}
	body := createSQL[start+1 : end]
	for _, part := range splitColumnDefs(body) {
		fields := strings.FieldsFunc(part, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r'
		})
		if len(fields) == 0 {
			continue
		}
		first := strings.Trim(fields[0], "`\"[]")
		if strings.EqualFold(first, colName) {
			return first
		}
	}
	return colName
}

// splitColumnDefs splits a CREATE TABLE column list on top-level commas
// (ignoring commas inside parentheses such as CHECK(...) or DEFAULT(...)).
func splitColumnDefs(body string) []string {
	var parts []string
	depth := 0
	last := 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, body[last:])
	return parts
}

// primaryKeyColIndices returns the set of column indices that are PRIMARY KEY
// columns: column-level PRIMARY KEY declarations plus table-level PRIMARY KEY
// constraints (honoring integer column positions).
func (e *Engine) primaryKeyColIndices(tableName, createSQL string, colDefs []sql.ColumnDef) map[int]bool {
	idx := make(map[int]bool)
	colIndex := buildColumnIndex(colDefs)
	for i, cd := range colDefs {
		if cd.PrimaryKey {
			idx[i] = true
		}
	}
	for _, tc := range e.tableConstraints(tableName, createSQL) {
		if tc.Type != sql.ConstraintPrimaryKey {
			continue
		}
		for _, ic := range tc.Columns {
			if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
				idx[n-1] = true
				continue
			}
			if i, ok := colIndex[ic.Name]; ok {
				idx[i] = true
			}
		}
	}
	return idx
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
			ch, err := e.buildUpdateChange(cell, rec, colIndex, colDefs, s, row)
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

func (e *Engine) buildUpdateChange(cell *storage.Cell, rec *storage.Record, colIndex map[string]int, colDefs []sql.ColumnDef, s *sql.UpdateStmt, row Row) (*updateChange, error) {
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

	oldValues := make([]interface{}, len(rec.Values))
	copy(oldValues, rec.Values)

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
		// Apply the column's type affinity (e.g. a REAL column stores 1 as 1.0).
		if idx >= 0 && idx < len(values) {
			if idx < len(colDefs) {
				v = util.ApplyColumnAffinity(v, colDefs[idx].Type)
			}
			values[idx] = v
		}
	}
	return &updateChange{cell.RowID, values, oldValues}, nil
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
	e.invalidateRowIDCache(rootPage)

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
		e.bumpRowIDCache(rootPage, c.rowID)
	}

	return &Result{Changes: int64(len(changes))}
}

// applyUpdateWithTriggers processes a plain UPDATE with triggers, matching
// SQLite's ordering: BEFORE UPDATE triggers fire per-row before the row is
// written, and a BEFORE trigger may delete the row being updated — SQLite then
// skips writing rows that no longer exist and checks UNIQUE/PK constraints
// against the live table state. AFTER UPDATE triggers fire phase-based after
// the writes (matching the engine's existing behavior for non-trigger rows).
func (e *Engine) applyUpdateWithTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	rootPage := tableEntry.RootPage
	tableName := tableEntry.Name
	tree := e.tableBTree(tableName, rootPage, true)
	var changesMade int64
	var applied []updateChange

	for _, ch := range changes {
		newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
		oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
		if trigResult := e.fireBeforeUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
			return trigResult
		}
		// If a BEFORE trigger deleted the row being updated, skip the write.
		stillExists, err := e.rowExists(rootPage, ch.rowID)
		if err != nil {
			return &Result{Error: err}
		}
		if !stillExists {
			continue
		}
		// Check UNIQUE/PK conflicts against the live table state. The row
		// being updated is not a conflict; every other live row (including
		// rows already written by earlier changes, which now hold their new
		// values) is.
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: err}
		}
		conflict := false
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			if cell.RowID == ch.rowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			if e.valuesConflict(rec.Values, ch.values, colDefs, colIndex, uniqueCols, idxColsList) {
				conflict = true
				break
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		if conflict {
			return &Result{Error: e.uniqueConflictError(tableName, colDefs, colIndex, nil, ch.values, uniqueCols, idxColsList)}
		}
		// Write the row: delete the old cell and insert the new record.
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == ch.rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(rootPage)
		newRecord, err := storage.EncodeRecord(ch.values)
		if err != nil {
			return &Result{Error: err}
		}
		newCell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   ch.rowID,
			Payload: newRecord,
		}
		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
		e.bumpRowIDCache(rootPage, ch.rowID)
		changesMade++
		applied = append(applied, ch)
	}

	// Fire AFTER UPDATE triggers phase-based (after all writes), matching the
	// engine's behavior for UPDATEs without this per-row path.
	for _, ch := range applied {
		newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
		oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
		if trigResult := e.fireAfterUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	return &Result{Changes: changesMade}
}

// applyUpdateIgnore implements UPDATE OR IGNORE: each change is applied only
// if its new values do not conflict with a UNIQUE/PK constraint or UNIQUE
// index. Conflicting rows are skipped without error. BEFORE/AFTER UPDATE
// triggers fire per row for the rows that are written.
func (e *Engine) applyUpdateIgnore(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	rootPage := tableEntry.RootPage
	tableName := tableEntry.Name
	tree := e.tableBTree(tableName, rootPage, true)
	hasTriggers := e.hasTriggersForTable(tableName)
	var changesMade int64

	for _, ch := range changes {
		if hasTriggers {
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			if trigResult := e.fireBeforeUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
				return trigResult
			}
			stillExists, err := e.rowExists(rootPage, ch.rowID)
			if err != nil {
				return &Result{Error: err}
			}
			if !stillExists {
				continue
			}
		}
		// Check conflicts against the live table state; skip on conflict.
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: err}
		}
		conflict := false
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			if cell.RowID == ch.rowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			if e.valuesConflict(rec.Values, ch.values, colDefs, colIndex, uniqueCols, idxColsList) {
				conflict = true
				break
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		if conflict {
			continue
		}
		// OR IGNORE also skips rows that would violate a FOREIGN KEY
		// constraint (child direction: the new child value has no parent;
		// parent direction: a child references the old key value).
		if e.foreignKeys {
			if res := e.checkForeignKeyViolations(tableEntry, colDefs, ch.values); res.Error != nil {
				continue
			}
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
				if res := e.fkParentUpdate(tableEntry, colDefs, oldRow, newRow, ch.rowID); res.Error != nil {
				continue
			}
		}
		// Write the row.
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == ch.rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(rootPage)
		newRecord, err := storage.EncodeRecord(ch.values)
		if err != nil {
			return &Result{Error: err}
		}
		newCell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   ch.rowID,
			Payload: newRecord,
		}
		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
		e.bumpRowIDCache(rootPage, ch.rowID)
		changesMade++
		if hasTriggers {
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			if trigResult := e.fireAfterUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
	}
	return &Result{Changes: changesMade}
}

// rowExists reports whether a table contains a cell with the given rowID.
func (e *Engine) rowExists(rootPage uint32, rowID int64) (bool, error) {
	tree := btree.NewBTree(e.pager, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false, err
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			return false, nil
		}
		if cell.RowID == rowID {
			return true, nil
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return false, nil
		}
	}
}

// resolveUpdateConflicts implements UPDATE OR REPLACE: for each row being
// updated, delete other rows whose values conflict with the new values on a
// UNIQUE/PRIMARY KEY column or UNIQUE index, firing BEFORE/AFTER DELETE
// triggers with the deleted row's OLD values.
func (e *Engine) resolveUpdateConflicts(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)

	// Scan the table, collecting rows that conflict with any change.
	type conflictInfo struct {
		values []interface{}
	}
	conflicts := make(map[int64]conflictInfo)
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		for _, c := range changes {
			if cell.RowID == c.rowID {
				continue // the row being updated is not a conflict
			}
			conflict := false
			for _, idx := range uniqueCols {
				if idx < len(rec.Values) && idx < len(c.values) {
					if rec.Values[idx] == nil || c.values[idx] == nil {
						continue
					}
					if util.CompareValues(rec.Values[idx], c.values[idx]) == 0 {
						conflict = true
						break
					}
				}
			}
			if !conflict {
				for _, def := range idxColsList {
					match := true
					// The new row must satisfy the partial predicate too.
					nrow := buildRowMapFromValues(c.values, colDefs, c.rowID)
					if inIndex, _ := e.evalIndexWhere(def.Where, nrow); !inIndex {
						continue
					}
					orow := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
					if inIndex, _ := e.evalIndexWhere(def.Where, orow); !inIndex {
						continue
					}
					for _, cn := range def.Cols {
						idx, ok := colIndex[cn]
						if !ok || idx >= len(rec.Values) || idx >= len(c.values) || rec.Values[idx] == nil || c.values[idx] == nil || util.CompareValues(rec.Values[idx], c.values[idx]) != 0 {
							match = false
							break
						}
					}
					if match {
						conflict = true
						break
					}
				}
			}
			if conflict {
				conflicts[cell.RowID] = conflictInfo{values: rec.Values}
				break
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	hasTriggers := e.hasTriggersForTable(tableEntry.Name)
	for rowID, ci := range conflicts {
		oldRow := buildRowMapFromValues(ci.values, colDefs, rowID)
		if hasTriggers {
			if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(tableEntry.RootPage)
		if hasTriggers {
			if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
	}
	return &Result{}
}

// applyUpdateReplace implements UPDATE OR REPLACE by processing each change
// incrementally (matching SQLite's row-by-row semantics): for each row, delete
// other rows whose values conflict with the row's NEW values on a UNIQUE/
// PRIMARY KEY column or UNIQUE index (firing BEFORE/AFTER DELETE triggers),
// then delete the row itself and insert its new version. Processing in order
// lets later changes see earlier applied rows, so conflicts between updated
// rows are resolved too (e.g. UPDATE OR REPLACE SET x=1 on two NULL rows).
func (e *Engine) applyUpdateReplace(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	hasTriggers := e.hasTriggersForTable(tableEntry.Name)
	changesMade := int64(0)
	// Snapshot so a FOREIGN KEY violation mid-statement rolls back any
	// conflict rows already deleted.
	snap := e.pager.Snapshot()

	for _, c := range changes {
		type conflictInfo struct {
			rowID  int64
			values []interface{}
		}
		var conflicts []conflictInfo
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: err}
		}
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			if cell.RowID == c.rowID {
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
			conflict := false
			for _, idx := range uniqueCols {
				if idx < len(rec.Values) && idx < len(c.values) {
					if rec.Values[idx] == nil || c.values[idx] == nil {
						continue
					}
					if util.CompareValues(rec.Values[idx], c.values[idx]) == 0 {
						conflict = true
						break
					}
				}
			}
			if !conflict {
				for _, def := range idxColsList {
					nrow := buildRowMapFromValues(c.values, colDefs, c.rowID)
					if inIndex, _ := e.evalIndexWhere(def.Where, nrow); !inIndex {
						continue
					}
					orow := buildRowMapFromValues(rec.Values, colDefs, cell.RowID)
					if inIndex, _ := e.evalIndexWhere(def.Where, orow); !inIndex {
						continue
					}
					match := true
					for _, cn := range def.Cols {
						idx, ok := colIndex[cn]
						if !ok || idx >= len(rec.Values) || idx >= len(c.values) || rec.Values[idx] == nil || c.values[idx] == nil || util.CompareValues(rec.Values[idx], c.values[idx]) != 0 {
							match = false
							break
						}
					}
					if match {
						conflict = true
						break
					}
				}
			}
			if conflict {
				conflicts = append(conflicts, conflictInfo{cell.RowID, rec.Values})
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}

		for _, cf := range conflicts {
			oldRow := buildRowMapFromValues(cf.values, colDefs, cf.rowID)
			if hasTriggers {
				if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
					return trigResult
				}
			}
			if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
				return cell.RowID == cf.rowID
			}); err != nil {
				return &Result{Error: err}
			}
			e.invalidateRowIDCache(tableEntry.RootPage)
			if hasTriggers {
				if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
					return trigResult
				}
			}
		}

		// UPDATE OR REPLACE still enforces FOREIGN KEY constraints: a new
		// value that orphans a child (or a child value with no parent) is an
		// error, matching SQLite.
		if e.foreignKeys {
			if res := e.checkForeignKeyViolations(tableEntry, colDefs, c.values); res.Error != nil {
				e.pager.Restore(snap)
				e.invalidateRowIDCache(tableEntry.RootPage)
				return res
			}
			oldRow := buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
			newRow := buildRowMapFromValues(c.values, colDefs, c.rowID)
			if res := e.fkParentUpdate(tableEntry, colDefs, oldRow, newRow, c.rowID); res.Error != nil {
				e.pager.Restore(snap)
				e.invalidateRowIDCache(tableEntry.RootPage)
				return res
			}
		}

		// Delete the row being updated (no DELETE trigger: this is the UPDATE
		// itself, not a conflict-replacement) and insert its new version.
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == c.rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.invalidateRowIDCache(tableEntry.RootPage)
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
		e.bumpRowIDCache(tableEntry.RootPage, c.rowID)
		changesMade++
	}
	return &Result{Changes: changesMade}
}

// valuesConflict reports whether two value sets conflict on any UNIQUE/PRIMARY
// KEY column or UNIQUE index (partial-index predicates evaluated).
func (e *Engine) valuesConflict(a, b []interface{}, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef) bool {
	for _, idx := range uniqueCols {
		if idx < len(a) && idx < len(b) {
			if a[idx] == nil || b[idx] == nil {
				continue
			}
			if util.CompareValues(a[idx], b[idx]) == 0 {
				return true
			}
		}
	}
	for _, def := range idxColsList {
		arow := buildRowMapFromValues(a, colDefs, 0)
		if inIndex, _ := e.evalIndexWhere(def.Where, arow); !inIndex {
			continue
		}
		brow := buildRowMapFromValues(b, colDefs, 0)
		if inIndex, _ := e.evalIndexWhere(def.Where, brow); !inIndex {
			continue
		}
		match := true
		for _, cn := range def.Cols {
			idx, ok := colIndex[cn]
			if !ok || idx >= len(a) || idx >= len(b) || a[idx] == nil || b[idx] == nil || util.CompareValues(a[idx], b[idx]) != 0 {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// checkUpdateConflicts validates that an UPDATE's new values do not violate
// UNIQUE/PRIMARY KEY constraints against non-updated rows or other updated
// rows. SQLite checks constraints per-row during UPDATE (a plain UPDATE has no
// OR REPLACE resolution, so a conflict is an error).
func (e *Engine) checkUpdateConflicts(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	colIndex := buildColumnIndex(colDefs)
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	if len(uniqueCols) == 0 && len(idxColsList) == 0 {
		return &Result{}
	}
	changed := make(map[int64]bool, len(changes))
	for _, c := range changes {
		changed[c.rowID] = true
	}
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	for _, c := range changes {
		cursor, err := tree.OpenCursor()
		if err != nil {
			return &Result{Error: err}
		}
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			if !changed[cell.RowID] && e.valuesConflict(rec.Values, c.values, colDefs, colIndex, uniqueCols, idxColsList) {
				return &Result{Error: e.uniqueConflictError(tableEntry.Name, colDefs, colIndex, rec.Values, c.values, uniqueCols, idxColsList)}
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
	}
	for i := 0; i < len(changes); i++ {
		for j := i + 1; j < len(changes); j++ {
			if e.valuesConflict(changes[i].values, changes[j].values, colDefs, colIndex, uniqueCols, idxColsList) {
				return &Result{Error: e.uniqueConflictError(tableEntry.Name, colDefs, colIndex, changes[i].values, changes[j].values, uniqueCols, idxColsList)}
			}
		}
	}
	return &Result{}
}

// uniqueConflictError builds a SQLite-style UNIQUE constraint error for the
// first conflicting column.
func (e *Engine) uniqueConflictError(tableName string, colDefs []sql.ColumnDef, colIndex map[string]int, a, b []interface{}, uniqueCols []int, idxColsList []uniqueIndexDef) error {
	for _, idx := range uniqueCols {
		if idx < len(a) && idx < len(b) && a[idx] != nil && b[idx] != nil && util.CompareValues(a[idx], b[idx]) == 0 {
			return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableName, colDefs[idx].Name)
		}
	}
	for _, def := range idxColsList {
		if e.valuesConflict(a, b, colDefs, colIndex, nil, []uniqueIndexDef{def}) {
			parts := make([]string, len(def.Cols))
			for i, cn := range def.Cols {
				parts[i] = tableName + "." + cn
			}
			return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
		}
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", tableName)
}
