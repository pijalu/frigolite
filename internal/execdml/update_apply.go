// Package exec implements query execution.
package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// --- UPDATE application modes (REPLACE / IGNORE / triggers) ---
func (e *DMLExecutor) applyUpdateWithTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange, s *sql.UpdateStmt) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := uniqueColsForTable(colDefs)
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	rootPage := tableEntry.RootPage
	tableName := tableEntry.Name
	tree := e.tableBTreeForDML(tableEntry, rootPage)
	var changesMade int64

	for i := range changes {
		ch := &changes[i]
		// Evaluate the SET expressions per-row (deferred in the collection
		// phase) so user functions and the changes() counter observe SQLite's
		// row-by-row interleaving (e_changes 5.1.2): row N's AFTER trigger
		// fires before row N+1's SET expressions are evaluated.
		if err := e.materializeChangeValues(ch, s, colIndex, colDefs); err != nil {
			return &Result{Error: err}
		}
		appliedChange, res := e.applyTriggeredUpdateRow(tree, tableName, rootPage, tableEntry, colDefs, colIndex, uniqueCols, idxColsList, *ch)
		if res != nil {
			return res
		}
		if appliedChange {
			changesMade++
			// Fire AFTER UPDATE triggers per-row (immediately after this
			// row's write), matching SQLite: the AFTER trigger for row N runs
			// before row N+1's SET expressions are evaluated. This makes the
			// changes() counter (and user functions like my_changes) observe
			// the row-by-row interleaving (e_changes 5.1.2).
			if e.hasTriggersForTable(tableName) {
				newRowID := ch.rowID
				if ch.newRowID != nil {
					newRowID = *ch.newRowID
				}
				newRow := buildRowMapFromValues(ch.values, colDefs, newRowID)
				oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
				if trigResult := e.fireAfterUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
					return trigResult
				}
			}
		}
	}
	return &Result{Changes: changesMade}
}

// applyTriggeredUpdateRow applies one change under the trigger-per-row path:
// BEFORE UPDATE triggers fire first (RAISE(IGNORE) or a deleted row skips the
// write), UNIQUE/PK conflicts are checked against the live table, FOREIGN KEY
// parent actions run, then the row is written with the merged values.
func (e *DMLExecutor) applyTriggeredUpdateRow(tree *btree.BTree, tableName string, rootPage uint32, tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, ch updateChange) (bool, *Result) {
	skip, res := e.fireUpdateBeforeTriggers(tableName, rootPage, ch, colDefs)
	if res != nil {
		return false, res
	}
	if skip {
		return false, nil
	}
	// Check UNIQUE/PK conflicts against the live table state. The row being
	// updated is not a conflict; every other live row (including rows already
	// written by earlier changes, which now hold their new values) is.
	conflict, err := e.updateRowConflictsWithTable(tree, ch, colDefs, colIndex, uniqueCols, idxColsList)
	if err != nil {
		return false, &Result{Error: err}
	}
	if conflict {
		return false, &Result{Error: e.uniqueConflictError(tableName, colDefs, colIndex, nil, ch.values, uniqueCols, idxColsList)}
	}
	if res := e.enforceUpdateFKActions(tableEntry, colDefs, ch); res != nil {
		return false, res
	}
	// Write the row: delete the old cell and insert the new record. SQLite
	// computes the NEW row values from the SET expressions BEFORE firing
	// BEFORE triggers, then writes only the SET columns after the triggers
	// run — so a BEFORE trigger that modifies a non-SET column (e.g.
	// UPDATE t SET b=1000 WHERE a=old.a) keeps its change, while the outer
	// SET columns get the pre-computed values. Re-read the current
	// (post-trigger) row first, overlay the SET columns' computed values,
	// then replace the row with the merged result.
	finalValues, err := e.mergeTriggerModifiedRow(tableName, rootPage, colDefs, ch)
	if err != nil {
		return false, &Result{Error: err}
	}
	if res := e.writeUpdateCell(tree, tableName, rootPage, ch, updateWriteRowID(ch), finalValues); res.Error != nil {
		return false, res
	}
	return true, nil
}

// enforceUpdateFKActions enforces FOREIGN KEY parent actions for a change:
// children referencing the old key values are restricted (error) or
// cascaded/updated. This runs for the plain UPDATE path too (SQLite fires FK
// actions regardless of whether the table has triggers).
func (e *DMLExecutor) enforceUpdateFKActions(tableEntry *schema.Entry, colDefs []sql.ColumnDef, ch updateChange) *Result {
	if !e.ctx.ForeignKeys() {
		return nil
	}
	oldFKRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
	newFKRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
	if res := e.ctx.FkParentUpdate(tableEntry, colDefs, oldFKRow, newFKRow, ch.rowID); res.Error != nil {
		return res
	}
	return nil
}

// updateWriteRowID returns the rowid a change should be written at: the new
// rowid when the UPDATE sets rowid/_rowid_/oid, otherwise the original.
func updateWriteRowID(ch updateChange) int64 {
	if ch.newRowID != nil {
		return *ch.newRowID
	}
	return ch.rowID
}

// uniqueColsForTable returns the column indices declared UNIQUE or PRIMARY
// KEY in the table's column definitions.
func uniqueColsForTable(colDefs []sql.ColumnDef) []int {
	var uniqueCols []int
	for i, cd := range colDefs {
		if cd.Unique || cd.PrimaryKey {
			uniqueCols = append(uniqueCols, i)
		}
	}
	return uniqueCols
}

// fireUpdateBeforeTriggers fires BEFORE UPDATE triggers for one change and
// reports whether the row's update should be skipped: RAISE(IGNORE) skips
// this row, and a BEFORE trigger that deleted the row also skips the write.
func (e *DMLExecutor) fireUpdateBeforeTriggers(tableName string, rootPage uint32, ch updateChange, colDefs []sql.ColumnDef) (bool, *Result) {
	newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
	oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
	if trigResult := e.fireBeforeUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
		// RAISE(IGNORE) in a BEFORE UPDATE trigger skips this row's update
		// (no error); other rows continue.
		if trigResult.Error == errRaiseIgnore {
			return true, nil
		}
		return false, trigResult
	}
	// If a BEFORE trigger deleted the row being updated, skip the write.
	stillExists, err := e.rowExists(tableName, rootPage, ch.rowID)
	if err != nil {
		return false, &Result{Error: err}
	}
	if !stillExists {
		return true, nil
	}
	return false, nil
}

// updateRowConflictsWithTable scans the live table for rows whose values
// conflict with a change's new values on a UNIQUE/PK column or UNIQUE index.
// The row being updated is not a conflict.
// updateRowConflictsWithTable scans the live table for rows whose values
// conflict with a change's new values on a UNIQUE/PK column or UNIQUE index.
// The row being updated is not a conflict.
func (e *DMLExecutor) updateRowConflictsWithTable(tree *btree.BTree, ch updateChange, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef) (bool, error) {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false, err
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return false, nil
		}
		if cell.RowID == ch.rowID {
			if cursorExhausted(cursor) {
				return false, nil
			}
			continue
		}
		conflict, stop := e.cellConflicts(cell, ch, colDefs, colIndex, uniqueCols, idxColsList)
		if stop || conflict {
			return conflict, nil
		}
		if cursorExhausted(cursor) {
			return false, nil
		}
	}
}

// cellConflicts reports whether one table cell's values conflict with a
// change's new values; stop is true when the scan should end (the record
// could not be decoded).
func (e *DMLExecutor) cellConflicts(cell *storage.Cell, ch updateChange, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef) (conflict, stop bool) {
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return false, true
	}
	if e.valuesConflict(rec.Values, ch.values, colDefs, colIndex, uniqueCols, idxColsList) {
		return true, true
	}
	return false, false
}

// cursorExhausted advances the cursor and reports whether the scan is done.
func cursorExhausted(cursor *btree.Cursor) bool {
	ok, err := cursor.Next()
	return err != nil || !ok
}

// writeUpdateCell replaces the row at ch.rowID with a new record at
// writeRowID (delete old cell, insert new), maintaining the rowid caches.
func (e *DMLExecutor) writeUpdateCell(tree *btree.BTree, tableName string, rootPage uint32, ch updateChange, writeRowID int64, finalValues []interface{}) *Result {
	if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == ch.rowID
	}); err != nil {
		return &Result{Error: err}
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableName), rootPage)
	newRecord, err := storage.EncodeRecord(finalValues)
	if err != nil {
		return &Result{Error: err}
	}
	newCell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   writeRowID,
		Payload: newRecord,
	}
	if err := tree.InsertCell(newCell); err != nil {
		return &Result{Error: err}
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableName), rootPage, writeRowID)

	// Fire the preupdate hook with the old and new row values. WITHOUT ROWID
	// tables report rowid 0 (SQLite uses the key columns instead); rowid
	// tables report the rowid (old for UPDATE, per the preupdate contract).
	if entry, _, err := e.ctx.FindTable(tableName); err == nil {
		rowID := ch.rowID
		if hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
			rowID = 0
		}
		if res := e.ctx.FirePreupdate(PreupdateEvent{
			Type:  "UPDATE",
			DB:    e.schemaNameForPager(e.dmlPager(tableName)),
			Table: tableName,
			RowID: rowID, RowID2: rowID,
			RowidTable: !hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)),
			Old:        append([]interface{}(nil), ch.oldValues...),
			New:        append([]interface{}(nil), finalValues...),
		}); res != nil {
			return res
		}
	}
	return &Result{}
}

// mergeTriggerModifiedRow reads the current (post-BEFORE-trigger) values of
// the row being updated and overlays the UPDATE statement's SET columns with
// their pre-computed values. SQLite computes NEW values from the SET
// expressions before BEFORE triggers run, then after the triggers the outer
// UPDATE writes only the SET columns — so a BEFORE trigger's changes to
// non-SET columns survive, while the SET columns get the computed values.
// Returns the full column-value slice to encode for the final row.
func (e *DMLExecutor) mergeTriggerModifiedRow(tableName string, rootPage uint32, colDefs []sql.ColumnDef, ch updateChange) ([]interface{}, error) {
	// Re-read the current row (post-trigger state) from the btree.
	cursor, err := e.updateRowTree(tableName, rootPage).OpenCursor()
	if err != nil {
		return nil, err
	}
	current, found := readCurrentRowValues(cursor, ch.rowID)
	if !found {
		// The row vanished during trigger execution (a BEFORE trigger deleted
		// it); the caller skips the write when rowExists reports false, so this
		// path should not normally be reached. Fall back to the computed NEW
		// values.
		return ch.values, nil
	}
	// Overlay the SET columns with the pre-computed values. Columns not in
	// the SET clause keep the post-trigger (possibly trigger-modified) values.
	if len(e.updateSetColumns) == 0 {
		return ch.values, nil
	}
	return e.overlaySetColumns(current, colDefs, ch), nil
}

// updateRowTree builds the btree for the table being updated, using the
// modified table's context pager when known.
func (e *DMLExecutor) updateRowTree(tableName string, rootPage uint32) *btree.BTree {
	return e.dmlTableBTree(tableName, rootPage)
}

// readCurrentRowValues scans the table for the row with the given rowID and
// returns its current record values.
// readCurrentRowValues scans the table for the row with the given rowID and
// returns its current record values.
func readCurrentRowValues(cursor *btree.Cursor, rowID int64) ([]interface{}, bool) {
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		if cell.RowID == rowID {
			rec, derr := storage.DecodeRecord(cell.Payload)
			if derr != nil || rec == nil {
				break
			}
			return rec.Values, true
		}
		if cursorExhausted(cursor) {
			break
		}
	}
	return nil, false
}

// overlaySetColumns overlays the SET columns with the pre-computed values
// from the change; columns not in the SET clause keep the post-trigger
// (possibly trigger-modified) values.
func (e *DMLExecutor) overlaySetColumns(current []interface{}, colDefs []sql.ColumnDef, ch updateChange) []interface{} {
	merged := make([]interface{}, len(current))
	copy(merged, current)
	for i, cd := range colDefs {
		for _, setCol := range e.updateSetColumns {
			if strings.EqualFold(cd.Name, setCol) && i < len(ch.values) {
				merged[i] = ch.values[i]
				break
			}
		}
	}
	return merged
}

// applyUpdateIgnore implements UPDATE OR IGNORE: each change is applied only
// if its new values do not conflict with a UNIQUE/PK constraint or UNIQUE
// index. Conflicting rows are skipped without error. BEFORE/AFTER UPDATE
// triggers fire per row for the rows that are written.
func (e *DMLExecutor) applyUpdateIgnore(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := uniqueColsForTable(colDefs)
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	rootPage := tableEntry.RootPage
	tableName := tableEntry.Name
	tree := e.tableBTreeForDML(tableEntry, rootPage)
	hasTriggers := e.hasTriggersForTable(tableName)
	var changesMade int64

	for _, ch := range changes {
		skip, res := e.applyIgnoredUpdateRow(tree, tableName, rootPage, tableEntry, colDefs, colIndex, uniqueCols, idxColsList, hasTriggers, ch)
		if res != nil {
			return res
		}
		if skip {
			continue
		}
		changesMade++
	}
	return &Result{Changes: changesMade}
}

// applyIgnoredUpdateRow applies one change under UPDATE OR IGNORE: rows whose
// new values conflict with a UNIQUE/PK constraint, UNIQUE index, NOT NULL,
// CHECK, or FOREIGN KEY constraint are skipped without error. BEFORE/AFTER
// UPDATE triggers fire per row for the rows that are written.
func (e *DMLExecutor) applyIgnoredUpdateRow(tree *btree.BTree, tableName string, rootPage uint32, tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, hasTriggers bool, ch updateChange) (bool, *Result) {
	if hasTriggers {
		s, r := e.fireUpdateBeforeTriggers(tableName, rootPage, ch, colDefs)
		if r != nil {
			return false, r
		}
		if s {
			return true, nil
		}
	}
	// Check conflicts against the live table state; skip on conflict.
	conflict, err := e.updateRowConflictsWithTable(tree, ch, colDefs, colIndex, uniqueCols, idxColsList)
	if err != nil {
		return false, &Result{Error: err}
	}
	if conflict {
		return true, nil
	}
	// OR IGNORE also skips rows whose NEW values violate a NOT NULL, CHECK,
	// or FOREIGN KEY constraint (SQLite's OR IGNORE applies to every
	// constraint type, so a violating row is silently left unchanged).
	if e.skipIgnoreChange(tableEntry, colDefs, ch) {
		return true, nil
	}
	// Write the row (UPDATE OR IGNORE keeps the original rowid even when
	// the SET clause assigns rowid — the row is re-inserted in place).
	if res := e.writeUpdateCell(tree, tableName, rootPage, ch, ch.rowID, ch.values); res.Error != nil {
		return false, res
	}
	if hasTriggers {
		newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
		oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
		if trigResult := e.fireAfterUpdateTriggers(tableName, newRow, oldRow); trigResult.Error != nil {
			return false, trigResult
		}
	}
	return false, nil
}

// skipIgnoreChange reports whether UPDATE OR IGNORE should skip a change
// because its NEW values violate a NOT NULL, CHECK, or FOREIGN KEY
// constraint.
func (e *DMLExecutor) skipIgnoreChange(tableEntry *schema.Entry, colDefs []sql.ColumnDef, ch updateChange) bool {
	if res := e.checkUpdateConstraints(tableEntry, colDefs, []updateChange{ch}); res.Error != nil {
		return true
	}
	if !e.ctx.ForeignKeys() {
		return false
	}
	// Child direction: the new child value has no parent.
	if res := e.ctx.CheckForeignKeyViolations(tableEntry, colDefs, ch.values, ch.rowID); res.Error != nil {
		return true
	}
	// Parent direction: a child references the old key value.
	oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
	newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
	if res := e.ctx.FkParentUpdate(tableEntry, colDefs, oldRow, newRow, ch.rowID); res.Error != nil {
		return true
	}
	return false
}

// applyUpdateReplace implements UPDATE OR REPLACE by processing each change
// incrementally (matching SQLite's row-by-row semantics): for each row, delete
// other rows whose values conflict with the row's NEW values on a UNIQUE/
// PRIMARY KEY column or UNIQUE index (firing BEFORE/AFTER DELETE triggers),
// then delete the row itself and insert its new version. Processing in order
// lets later changes see earlier applied rows, so conflicts between updated
// rows are resolved too (e.g. UPDATE OR REPLACE SET x=1 on two NULL rows).
func (e *DMLExecutor) applyUpdateReplace(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := uniqueColsForTable(colDefs)
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	hasTriggers := e.hasTriggersForTable(tableEntry.Name)
	changesMade := int64(0)
	// Snapshot so a FOREIGN KEY violation mid-statement rolls back any
	// conflict rows already deleted.
	snap := e.ctx.Pager().Snapshot()
	// RowIDs deleted by an earlier change's conflict resolution. A later
	// change targeting one of these rows must be skipped (the row is gone),
	// not aborted — SQLite processes the remaining changes against the live
	// table (tkt2832: UPDATE OR REPLACE SET a=1 over PK rows 2,1,3).
	deletedByConflict := map[int64]bool{}

	for _, c := range changes {
		updated, res := e.replaceUpdateRow(tree, tableEntry, colDefs, colIndex, uniqueCols, idxColsList, hasTriggers, snap, deletedByConflict, c)
		if res != nil {
			return res
		}
		if updated {
			changesMade++
		}
	}
	return &Result{Changes: changesMade}
}

// replaceUpdateRow applies one change under UPDATE OR REPLACE: delete other
// rows whose values conflict with the row's NEW values (firing BEFORE/AFTER
// DELETE triggers), then delete the row itself and insert its new version.
func (e *DMLExecutor) replaceUpdateRow(tree *btree.BTree, tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, hasTriggers bool, snap *pager.PagerState, deletedByConflict map[int64]bool, c updateChange) (bool, *Result) {
	// If this change's row was deleted by an earlier change's conflict
	// resolution, the row is gone — skip it (SQLite processes the live
	// table; the deleted row no longer needs updating).
	if deletedByConflict[c.rowID] {
		return false, nil
	}
	conflicts, err := e.collectUpdateConflicts(tree, c, uniqueCols, idxColsList, colDefs, colIndex, nil)
	if err != nil {
		return false, &Result{Error: err}
	}
	trigRes := e.deleteConflictRows(tree, tableEntry, conflicts, colDefs, hasTriggers, deletedByConflict)
	if trigRes != nil {
		if trigRes.Error == errRaiseIgnore {
			return false, nil
		}
		return false, trigRes
	}
	// UPDATE OR REPLACE still enforces FOREIGN KEY constraints: a new
	// value that orphans a child (or a child value with no parent) is an
	// error, matching SQLite.
	if res := e.enforceUpdateForeignKey(tableEntry, colDefs, c, snap); res != nil {
		return false, res
	}
	// Delete the row being updated (no DELETE trigger: this is the UPDATE
	// itself, not a conflict-replacement) and insert its new version.
	return e.updateRowInPlace(tree, tableEntry, c, deletedByConflict, snap)
}
