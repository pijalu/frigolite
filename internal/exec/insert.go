// Package exec implements query execution.
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- INSERT ---

func (e *Engine) execInsert(s *sql.InsertStmt) *Result {
	if err := e.authorize(auth.ActionInsert, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, dbCtx, err := e.findTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}

	// Protect system tables from modification
	if strings.EqualFold(tableEntry.Name, "sqlite_master") ||
		strings.EqualFold(tableEntry.Name, "sqlite_schema") ||
		strings.EqualFold(tableEntry.Name, "sqlite_temp_master") ||
		strings.EqualFold(tableEntry.Name, "sqlite_temp_schema") {
		return &Result{Error: fmt.Errorf("table %s may not be modified", tableEntry.Name)}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	if s.Select != nil {
		return e.execInsertSelect(tableEntry, colDefs, s.Select, s.Columns, s.IsReplace)
	}
	if len(s.Values) == 0 {
		return e.execInsertDefault(tableEntry, colDefs, s)
	}

	var totalChanges int64
	var lastRowID int64
	var returningRows [][]interface{}
	for _, tuple := range s.Values {
		values := e.evalTuple(tuple, s.Columns, colDefs)
		if values == nil {
			return &Result{Error: fmt.Errorf("exec: failed to evaluate INSERT values")}
		}

		// Handle REPLACE (INSERT OR REPLACE): delete conflicting rows before inserting
		if s.IsReplace {
			colIndex := buildColumnIndex(colDefs)
			conflictRowID, _, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
			if found {
				tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
				_, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
					return cell.RowID == conflictRowID
				})
				if err != nil {
					return &Result{Error: err}
				}
			}
		}

		// Check for ON CONFLICT (UPSERT)
		if s.OnConflict != nil {
			res := e.execInsertOnConflict(dbCtx.Pager, tableEntry, colDefs, values, s)
			if res.Error != nil {
				return res
			}
			totalChanges += res.Changes
			if res.LastInsertRowID > 0 {
				lastRowID = res.LastInsertRowID
			}
		} else {
			res := e.insertRow(dbCtx.Pager, tableEntry, colDefs, values)
			if res.Error != nil {
				return res
			}
			totalChanges += res.Changes
			lastRowID = res.LastInsertRowID
		}

		// Handle RETURNING clause — evaluate RETURNING expression against inserted row
		if s.HasReturning {
			row := buildRowMapFromValues(values, colDefs, lastRowID)
			rowValues, err := e.evalReturningExprs(s.Returning, row, colDefs)
			if err != nil {
				return &Result{Error: err}
			}
			returningRows = append(returningRows, rowValues)
		}
	}

	// If RETURNING clause was present, return result rows instead of change count
	if s.HasReturning {
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: returningRows}
	}
	return &Result{Changes: totalChanges}
}

func (e *Engine) insertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) *Result {
	// Route FTS virtual table inserts directly to the FTS table
	if ftsTable, ok := e.ftsTables[tableEntry.Name]; ok {
		nextRowID := ftsTable.Insert(values)
		e.lastRowID = nextRowID
		return &Result{Changes: 1, LastInsertRowID: nextRowID}
	}

	// Validate constraints before inserting
	if err := e.checkConstraints(tableEntry, colDefs, values); err != nil {
		// Column-level ON CONFLICT IGNORE: silence UNIQUE constraint violations
		if isIgnoreableConflict(err, colDefs) {
			return &Result{Changes: 0}
		}
		return &Result{Error: err}
	}

	// Determine rowID: if an INTEGER PRIMARY KEY column has an explicit non-nil
	// value, use that value as the rowid (the column IS the rowid). Otherwise
	// auto-assign the next available rowid.
	nextRowID := e.pkRowID(colDefs, values, tableEntry.RootPage)
	e.lastRowID = nextRowID

	// If INTEGER PRIMARY KEY column value is nil, set it to the auto-assigned rowid.
	// SQLite behavior: inserting NULL into an INTEGER PRIMARY KEY column causes
	// the column to contain the auto-generated rowid.
	for i, cd := range colDefs {
		if cd.PrimaryKey && i < len(values) && values[i] == nil {
			values[i] = nextRowID
			break
		}
	}

	// Apply type affinity to each value based on column type.
	// Apply in-place to avoid allocating a separate affValues slice.
	for i, v := range values {
		if i < len(colDefs) {
			values[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
		}
	}

	record, err := storage.EncodeRecord(values)
	if err != nil {
		return &Result{Error: err}
	}

	tree := e.tableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, true)
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   nextRowID,
		Payload: record,
	}
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	// Track root page changes (after splits)
	if tree.RootPage() != e.rootPage(tableEntry.Name, tableEntry.RootPage) {
		e.updateRootPage(tableEntry.Name, tree.RootPage())
	}

	// Fire AFTER INSERT triggers — but only if triggers exist for this table.
	// The trigger check is cheap (cached schema lookup) but building the RowMap
	// and calling fireTriggers is wasteful when no triggers are registered.
	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := make(RowMap)
		for i, v := range values {
			if i < len(colDefs) {
				newRow[colDefs[i].Name] = v
			}
		}
		if trigResult := e.fireAfterInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
			return trigResult
		}
	}
	return &Result{Changes: 1, LastInsertRowID: nextRowID}
}

// hasTriggersForTable returns true if any AFTER INSERT/UPDATE/DELETE triggers
// exist for the given table across all databases. This is a fast check to avoid
// building trigger row maps when no triggers are registered.
func (e *Engine) hasTriggersForTable(tableName string) bool {
	// Check cache first
	if has, ok := e.hasTriggersCache[tableName]; ok {
		return has
	}
	has := false
	for _, ctx := range e.databases {
		triggers, err := ctx.Schema.FindTriggersForTable(tableName)
		if err == nil && len(triggers) > 0 {
			has = true
			break
		}
	}
	e.hasTriggersCache[tableName] = has
	return has
}

// checkConstraints validates NOT NULL, CHECK, UNIQUE, and PRIMARY KEY
// constraints for a row being inserted.
func (e *Engine) checkConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) error {
	// Fast path: if there are no constraints at all, skip allocation entirely
	hasConstraints := false
	for _, cd := range colDefs {
		if cd.NotNull || cd.Check != nil || cd.PrimaryKey {
			hasConstraints = true
			break
		}
	}

	// Check for UNIQUE constraints (defined separately from column defs)
	// by looking for indexes with unique=true for this table
	if !hasConstraints {
		// Quick scan for any constraint — including UNIQUE via unique indices
		for _, cd := range colDefs {
			if cd.Unique {
				hasConstraints = true
				break
			}
		}
	}

	if !hasConstraints {
		return nil
	}

	row := buildRowMapFromValues(values, colDefs, 0)

	for _, cd := range colDefs {
		val := columnValue(values, colDefs, cd.Name)

		// NOT NULL constraint — skip for INTEGER PRIMARY KEY columns
		// since they get their value from the auto-generated rowid.
		if cd.NotNull && val == nil && !(cd.PrimaryKey && cd.Type == "INTEGER") {
			return fmt.Errorf("NOT NULL constraint failed: %s.%s", tableEntry.Name, cd.Name)
		}

		// CHECK constraint: only fails when result is explicitly false.
		// NULL (unknown) and true both pass.
		if cd.Check != nil {
			checkVal, err := e.evalExpr(cd.Check, row)
			if err == nil && checkVal != nil && !toBool(checkVal) {
				return fmt.Errorf("CHECK constraint failed: %s", sql.ExprString(cd.Check))
			}
		}
	}

	// UNIQUE and PRIMARY KEY uniqueness check
	if err := e.checkUniqueConstraints(tableEntry, colDefs, values); err != nil {
		return err
	}

	return nil
}

// checkUniqueConstraints validates UNIQUE and PRIMARY KEY constraints by scanning
// for existing rows with the same values on UNIQUE or PRIMARY KEY columns.
func (e *Engine) checkUniqueConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) error {
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}
	if len(uniqueCols) > 0 {
		_, _, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
		if found {
			for _, idx := range uniqueCols {
				if idx < len(colDefs) {
					return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableEntry.Name, colDefs[idx].Name)
				}
			}
			return fmt.Errorf("UNIQUE constraint failed: %s", tableEntry.Name)
		}
	}
	return nil
}

// buildRowMapFromValues creates a column-name-to-value map from a values slice.
func buildRowMapFromValues(values []interface{}, colDefs []sql.ColumnDef, rowID int64) RowMap {
	row := make(RowMap)
	for i, v := range values {
		if i < len(colDefs) {
			row[colDefs[i].Name] = v
		}
	}
	row["rowid"] = rowID
	return row
}

// columnValue returns the value for a named column from a values array.
func columnValue(values []interface{}, colDefs []sql.ColumnDef, name string) interface{} {
	for i, cd := range colDefs {
		if cd.Name == name && i < len(values) {
			return values[i]
		}
	}
	return nil
}

// contains returns true if the slice contains the value.
func contains(s []int, v int) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.
func (e *Engine) execInsertOnConflict(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, s *sql.InsertStmt) *Result {
	oc := s.OnConflict

	// Build a map of column name → index for easy lookup
	colIndex := buildColumnIndex(colDefs)

	// Try to find an existing conflicting row by scanning for UNIQUE violations
	existingRowID, existingValues, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)

	if !found {
		return e.insertRow(pg, tableEntry, colDefs, values)
	}

	switch oc.Action {
	case sql.ConflictDoNothing:
		return &Result{Changes: 0}
	case sql.ConflictDoUpdate:
		return e.applyUpsertUpdate(tableEntry, colDefs, colIndex, existingRowID, existingValues, oc)
	}
	return &Result{Changes: 0}
}

// applyUpsertUpdate applies DO UPDATE SET assignments to the existing row
// and writes the updated row back to the table.
func (e *Engine) applyUpsertUpdate(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, existingRowID int64, existingValues []interface{}, oc *sql.OnConflictClause) *Result {
	updated := e.buildUpdatedRow(colDefs, colIndex, existingValues, oc)

	record, err := storage.EncodeRecord(updated)
	if err != nil {
		return &Result{Error: err}
	}

	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	deleted, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == existingRowID
	})
	if err != nil || deleted == 0 {
		return &Result{Error: fmt.Errorf("upsert: row not found for update")}
	}

	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   existingRowID,
		Payload: record,
	}
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}

	if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, nil, nil); trigResult.Error != nil {
		return trigResult
	}
	return &Result{Changes: 1}
}

// buildUpdatedRow applies ON CONFLICT DO UPDATE SET assignments to the
// existing values and returns the updated row.
func (e *Engine) buildUpdatedRow(colDefs []sql.ColumnDef, colIndex map[string]int, existingValues []interface{}, oc *sql.OnConflictClause) []interface{} {
	updated := make([]interface{}, len(existingValues))
	copy(updated, existingValues)

	row := make(RowMap)
	for _, col := range colDefs {
		if idx, ok := colIndex[col.Name]; ok && idx < len(existingValues) {
			row[col.Name] = existingValues[idx]
		}
	}

	for _, assign := range oc.Assignments {
		if idx, ok := colIndex[assign.Column]; ok {
			val, err := e.evalExpr(assign.Value, row)
			if err == nil && idx < len(updated) {
				updated[idx] = val
			}
		}
	}
	return updated
}

// findRowByUniqueCols searches for a row that conflicts with the given values
// on any UNIQUE column. Returns the RowID, existing values, and whether a
// conflict was found.
func (e *Engine) findRowByUniqueCols(tableName string, rootPage uint32, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) (int64, []interface{}, bool) {
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	// Also include PRIMARY KEY columns (they imply UNIQUE)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}
	if len(uniqueCols) == 0 {
		return 0, nil, false
	}

	tree := e.tableBTree(tableName, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, nil, false
	}

	return scanForConflict(cursor, uniqueCols, values)
}


// scanForConflict iterates through all rows and looks for a value match
// on any of the given UNIQUE column indices.
func scanForConflict(cursor *btree.Cursor, uniqueCols []int, values []interface{}) (int64, []interface{}, bool) {
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}

		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}

		if hasConflictAt(rec.Values, uniqueCols, values) {
			return cell.RowID, rec.Values, true
		}

		hasNext, err := cursor.Next()
		if err != nil || !hasNext {
			break
		}
	}
	return 0, nil, false
}

// hasConflictAt returns true if any of the UNIQUE column values match.
// Per SQL standard, NULL != NULL for UNIQUE constraint purposes.
func hasConflictAt(recValues []interface{}, uniqueCols []int, values []interface{}) bool {
	for _, idx := range uniqueCols {
		if idx < len(recValues) && idx < len(values) {
			// NULL != NULL — two NULLs never violate a UNIQUE constraint
			if recValues[idx] == nil || values[idx] == nil {
				continue
			}
			if util.CompareValues(recValues[idx], values[idx]) == 0 {
				return true
			}
		}
	}
	return false
}

// isIgnoreableConflict checks if a constraint error should be silently ignored
// due to a column-level ON CONFLICT IGNORE clause.
func isIgnoreableConflict(err error, colDefs []sql.ColumnDef) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "UNIQUE constraint failed") {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "IGNORE" {
			return true
		}
	}
	return false
}

// gatherUniqueColIndices returns the column indices that have UNIQUE constraints
// and are present in both the column definitions and the provided values.
func gatherUniqueColIndices(colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) []int {
	var uniqueCols []int
	for _, cd := range colDefs {
		if cd.Unique {
			if idx, ok := colIndex[cd.Name]; ok && idx < len(values) {
				uniqueCols = append(uniqueCols, idx)
			}
		}
	}
	return uniqueCols
}

func (e *Engine) execInsertSelect(tableEntry *schema.Entry, colDefs []sql.ColumnDef, selectStmt *sql.SelectStmt, columns []string, isReplace bool) *Result {
	selectResult := e.execSelect(selectStmt)
	if selectResult.Error != nil {
		return selectResult
	}

	// Determine the effective number of columns we expect.
	// If specific columns are given in the INSERT, the SELECT
	// must produce that many values; otherwise it must produce
	// one per table column.
	expectedCount := len(colDefs)
	if len(columns) > 0 {
		expectedCount = len(columns)
	}
	numSelectCols := len(selectResult.Columns)
	if expectedCount != numSelectCols {
		return &Result{Error: fmt.Errorf("table %s has %d columns but %d values were supplied",
			tableEntry.Name, expectedCount, numSelectCols)}
	}

	// Build a column mapping for the INSERT column list.
	// Handle _rowid_ specially (maps to the implicit rowid, not a table column).
	// Handle duplicate column names by only using the first occurrence.
	var colMapping []int // maps SELECT column index → colDefs index (-1 for _rowid_)
	if len(columns) > 0 {
		colMapping = make([]int, len(columns))
		seen := make(map[string]bool)
		for i, col := range columns {
			if strings.EqualFold(col, "_rowid_") || strings.EqualFold(col, "rowid") {
				colMapping[i] = -1 // _rowid_ marker
			} else {
				if seen[col] {
					colMapping[i] = -2 // duplicate, skip
				} else {
					seen[col] = true
					found := false
					for j, cd := range colDefs {
						if cd.Name == col {
							colMapping[i] = j
							found = true
							break
						}
					}
					if !found {
						colMapping[i] = -3 // column not found in table
					}
				}
			}
		}
	}

	var changes int64
	for _, row := range selectResult.Rows {
		var values []interface{}
		var explicitRowID int64
		hasExplicitRowID := false

		if len(columns) > 0 {
			values = make([]interface{}, len(colDefs))
			// First, apply default values for all columns
			for j, cd := range colDefs {
				if cd.Default != nil {
					// Evaluate the default expression to get the default value
					if dv, err := e.evalExpr(cd.Default, nil); err == nil {
						values[j] = dv
					}
				}
			}
			// Then map SELECT values to column positions
			for i, colIdx := range colMapping {
				if colIdx >= 0 && i < len(row) {
					values[colIdx] = row[i]
				} else if colIdx == -1 && i < len(row) {
					// _rowid_ column
					if v, ok := row[i].(int64); ok {
						explicitRowID = v
						hasExplicitRowID = true
					}
				}
				// colIdx == -2: duplicate column, skip
				// colIdx == -3: column not found, skip (will be caught by validation)
			}
		} else {
			values = row
		}

		// Apply type affinity to each value based on column type
		for i, v := range values {
			if i < len(colDefs) {
				values[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
			}
		}

		// Handle REPLACE: delete conflicting rows before inserting
		if isReplace {
			colIndex := buildColumnIndex(colDefs)
			conflictRowID, _, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
			if found {
				tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
				_, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
					return cell.RowID == conflictRowID
				})
				if err != nil {
					return &Result{Error: err}
				}
			}
		}

		// Validate constraints before inserting
		if err := e.checkConstraints(tableEntry, colDefs, values); err != nil {
			return &Result{Error: err}
		}

		// Determine rowID
		var rowID int64
		if hasExplicitRowID {
			rowID = explicitRowID
		} else {
			rowID = e.pkRowID(colDefs, values, tableEntry.RootPage)
			// If INTEGER PRIMARY KEY column is nil, set it to the auto-assigned rowid
			for i, cd := range colDefs {
				if cd.PrimaryKey && i < len(values) && values[i] == nil {
					values[i] = rowID
					break
				}
			}
		}

		record, err := storage.EncodeRecord(values)
		if err != nil {
			return &Result{Error: err}
		}
		cell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   rowID,
			Payload: record,
		}
		tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
		if err := tree.InsertCell(cell); err != nil {
			return &Result{Error: err}
		}
		changes++
		e.lastRowID = rowID
	}
	return &Result{Changes: changes, LastInsertRowID: e.lastRowID}
}

// pkRowID returns the rowid for a new row, using the INTEGER PRIMARY KEY value
// if one is explicitly provided, or auto-assigning the next available rowid.
func (e *Engine) pkRowID(colDefs []sql.ColumnDef, values []interface{}, rootPage uint32) int64 {
	for i, cd := range colDefs {
		if cd.PrimaryKey && i < len(values) && values[i] != nil {
			if v, ok := values[i].(int64); ok {
				return v
			}
			break
		}
	}
	return e.findNextRowID(rootPage)
}

func (e *Engine) execInsertDefault(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt) *Result {
	record, err := storage.EncodeRecord(nil)
	if err != nil {
		return &Result{Error: err}
	}
	nextRowID := e.findNextRowID(tableEntry.RootPage)
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   nextRowID,
		Payload: record,
	}
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}

	// Handle RETURNING clause
	if s.HasReturning {
		row := make(RowMap)
		for _, cd := range colDefs {
			row[cd.Name] = nil
		}
		row["rowid"] = nextRowID
		values, err := e.evalReturningExprs(s.Returning, row, colDefs)
		if err != nil {
			return &Result{Error: err}
		}
		columns := e.buildColumnNames([]sql.SelectColumn{s.Returning}, colDefs)
		return &Result{Columns: columns, Rows: [][]interface{}{values}}
	}

	return &Result{Changes: 1}
}

// fireAfterInsertTriggers fires AFTER INSERT triggers for the given table.
func (e *Engine) fireAfterInsertTriggers(tableName string, newRow RowMap) *Result {
	return e.fireTriggers(tableName, "INSERT", newRow, nil)
}

// fireAfterUpdateTriggers fires AFTER UPDATE triggers for the given table.
func (e *Engine) fireAfterUpdateTriggers(tableName string, newRow, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "UPDATE", newRow, oldRow)
}

// fireAfterDeleteTriggers fires AFTER DELETE triggers for the given table.
func (e *Engine) fireAfterDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "DELETE", nil, oldRow)
}

// fireTriggers fires triggers matching the given event for the table.
func (e *Engine) fireTriggers(tableName, event string, newRow, oldRow RowMap) *Result {
	// Prevent recursive trigger firing by default (matches SQLite behavior
	// where recursive_triggers pragma is OFF by default)
	if e.triggerDepth > 0 {
		return &Result{}
	}

	// Search for triggers across all databases
	var triggers []*schema.Entry
	for _, ctx := range e.databases {
		t, err := ctx.Schema.FindTriggersForTable(tableName)
		if err == nil && len(t) > 0 {
			triggers = append(triggers, t...)
		}
	}

	if len(triggers) == 0 {
		return &Result{}
	}
	for _, t := range triggers {
		if res := e.fireTrigger(t, event, newRow, oldRow); res != nil {
			return res
		}
	}
	return &Result{}
}

// fireTrigger fires a single trigger matching the given event.
// Returns a Result with an error if execution fails, or nil on success.
func (e *Engine) fireTrigger(t *schema.Entry, event string, newRow, oldRow RowMap) *Result {
	upper := strings.ToUpper(t.SQL)
	// Check event matches: "event ON table" pattern
	if !strings.Contains(upper, " "+event+" ") && !strings.Contains(upper, " "+event+" ON") {
		return nil
	}
	// Extract statements between BEGIN and END
	beginIdx := strings.Index(upper, "BEGIN")
	if beginIdx < 0 {
		return nil
	}
	endIdx := strings.LastIndex(upper, "END")
	if endIdx < 0 {
		return nil
	}
	body := t.SQL[beginIdx+5 : endIdx]
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	parser := sql.NewParser(body)
	stmts := parser.Parse()
	if parser.Err() != nil {
		return nil
	}
	// Increment trigger depth to prevent recursive trigger firing
	e.triggerDepth++
	defer func() { e.triggerDepth-- }()

	// Set NEW and OLD row values for trigger body execution
	prevNewRow := e.triggerNewRow
	prevOldRow := e.triggerOldRow
	e.triggerNewRow = newRow
	e.triggerOldRow = oldRow
	defer func() {
		e.triggerNewRow = prevNewRow
		e.triggerOldRow = prevOldRow
	}()

	for _, stmt := range stmts {
		res := e.Exec(stmt)
		if res.Error != nil {
			// Add "main." schema prefix for table-not-found errors during trigger execution,
			// matching SQLite's behavior where trigger execution errors include the default schema.
			errMsg := res.Error.Error()
			if strings.Contains(errMsg, "no such table:") {
				// Extract the table name and add "main." prefix if not already qualified
				if parts := strings.SplitN(errMsg, "no such table: ", 2); len(parts) == 2 {
					tableName := parts[1]
					if !strings.Contains(tableName, ".") {
						res.Error = fmt.Errorf("no such table: main.%s", tableName)
					}
				}
			}
			return res
		}
	}
	return nil
}

func (e *Engine) evalTuple(tuple []sql.Expr, columns []string, colDefs []sql.ColumnDef) []interface{} {
	values := make([]interface{}, len(tuple))
	for i, expr := range tuple {
		v, err := e.evalExpr(expr, nil)
		if err != nil {
			return nil
		}
		values[i] = v
	}
	if len(columns) > 0 {
		// Start with default values for all columns, then override with provided values
		mapped := make([]interface{}, len(colDefs))
		for j, cd := range colDefs {
			if cd.Default != nil {
				if dv, err := e.evalExpr(cd.Default, nil); err == nil {
					mapped[j] = dv
				}
			}
		}
		for i, col := range columns {
			for j, cd := range colDefs {
				if cd.Name == col && i < len(values) {
					mapped[j] = values[i]
					break
				}
			}
		}
		values = mapped
	} else if len(values) < len(colDefs) {
		// Pad with default values for any missing trailing columns
		padded := make([]interface{}, len(colDefs))
		copy(padded, values)
		for j := len(values); j < len(colDefs); j++ {
			if colDefs[j].Default != nil {
				if dv, err := e.evalExpr(colDefs[j].Default, nil); err == nil {
					padded[j] = dv
				}
			}
		}
		values = padded
	}
	return values
}

// evalReturningExprs evaluates RETURNING expressions against a row and
// returns a flat list of values. It handles three cases:
//   - RETURNING * : expands to all column values
//   - RETURNING expr (single) : returns the single expression value
//   - RETURNING expr, ..., * , ... : multi-expression with * expanded inline
func (e *Engine) evalReturningExprs(ret sql.SelectColumn, row Row, colDefs []sql.ColumnDef) ([]interface{}, error) {
	switch expr := ret.Expr.(type) {
	case *sql.ColumnRef:
		if expr.Name == "*" && expr.Table == "" {
			// RETURNING * — expand to all column values
			var values []interface{}
			for _, cd := range colDefs {
				if cd.Dropped {
					continue
				}
				if v, ok := row.Get(cd.Name); ok {
					values = append(values, util.UnwrapColumnValue(v))
				}
			}
			return values, nil
		}
		// Single column reference
		val, err := e.evalExpr(expr, row)
		if err != nil {
			return nil, err
		}
		return []interface{}{util.UnwrapColumnValue(val)}, nil

	case *sql.RowValue:
		// Multi-expression RETURNING — evaluate each sub-expression
		var values []interface{}
		for _, subExpr := range expr.Values {
			if ref, ok := subExpr.(*sql.ColumnRef); ok && ref.Name == "*" && ref.Table == "" {
				// Expand * to all column values inline
				for _, cd := range colDefs {
					if cd.Dropped {
						continue
					}
					if v, ok := row.Get(cd.Name); ok {
						values = append(values, util.UnwrapColumnValue(v))
					}
				}
			} else {
				val, err := e.evalExpr(subExpr, row)
				if err != nil {
					return nil, err
				}
				values = append(values, util.UnwrapColumnValue(val))
			}
		}
		return values, nil

	default:
		// Single expression not * and not a row value
		val, err := e.evalExpr(ret.Expr, row)
		if err != nil {
			return nil, err
		}
		return []interface{}{util.UnwrapColumnValue(val)}, nil
	}
}