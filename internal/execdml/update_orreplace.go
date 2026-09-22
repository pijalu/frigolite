package execdml

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// --- UPDATE OR REPLACE conflict resolution ---
// The OR REPLACE family resolves UNIQUE/PK conflicts by deleting the
// conflicting rows (firing their DELETE triggers), then writing the change.

// conflictInfo records a row that conflicts with an UPDATE's new values during
// UPDATE OR REPLACE conflict resolution.
type conflictInfo struct {
	rowID  int64
	values []interface{}
}

// collectUpdateConflicts scans the table for rows whose key values conflict
// with an update change's new values under UPDATE OR REPLACE semantics (the
// row being updated itself is excluded). It appends conflicts to the provided
// slice and returns it.
func (e *DMLExecutor) collectUpdateConflicts(tree *btree.BTree, tableEntry *schema.Entry, c updateChange, uniqueCols []int, idxColsList []uniqueIndexDef, colDefs []sql.ColumnDef, colIndex map[string]int, conflicts []conflictInfo) ([]conflictInfo, error) {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return conflicts, err
	}
	// WITHOUT ROWID cells are PK-first storage order: remap to declared
	// order so the positional conflict comparison sees declared columns,
	// and exclude the change's own row by OLD PK key (every cell shares
	// synthetic RowID 0, so the rowid self-exclusion never fires).
	wrOrder := e.ctx.WRStorageOrder(tableEntry.SQL, colDefs)
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		if info, conflict := e.updateConflictFromCell(cell, rec, tableEntry, c, wrOrder, colDefs, colIndex, uniqueCols, idxColsList); conflict {
			conflicts = append(conflicts, info)
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return conflicts, nil
}

// updateConflictFromCell evaluates one cell against a change's new values.
// WITHOUT ROWID cells are remapped to declared order first and the change's
// own row (same OLD PK key) is excluded — every WR cell shares the synthetic
// RowID 0, so the rowid self-exclusion never fires there.
func (e *DMLExecutor) updateConflictFromCell(cell *storage.Cell, rec *storage.Record, tableEntry *schema.Entry, c updateChange, wrOrder []int, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef) (conflictInfo, bool) {
	isSelf := len(wrOrder) == 0 && cell.RowID == c.rowID
	if len(wrOrder) > 0 {
		e.ctx.RemapWRRecordToDeclared(rec, tableEntry.SQL, colDefs)
		isSelf = declaredPKMatches(rec.Values, tableEntry, c.oldValues, colDefs)
	}
	if isSelf || !updateRowConflicts(e, rec.Values, c.values, colDefs, colIndex, uniqueCols, idxColsList, cell.RowID, c.rowID) {
		return conflictInfo{}, false
	}
	return conflictInfo{cell.RowID, rec.Values}, true
}

// declaredPKMatches reports whether a declared-order row holds the change's
// OLD PK key (the WITHOUT ROWID self-row test for UPDATE OR REPLACE).
func declaredPKMatches(declared []interface{}, tableEntry *schema.Entry, oldValues []interface{}, colDefs []sql.ColumnDef) bool {
	pkIdx := WRPKIndices(tableEntry.SQL, colDefs)
	if len(pkIdx) == 0 {
		return false
	}
	key := wrPkKeyFromDeclared(oldValues, pkIdx)
	for k, ci := range pkIdx {
		var have interface{}
		if ci < len(declared) {
			have = declared[ci]
		}
		if !wrValuesEqual(have, key[k], colDefs[ci]) {
			return false
		}
	}
	return true
}

// deleteConflictRows deletes the rows identified as conflicts during UPDATE
// OR REPLACE resolution, firing BEFORE/AFTER DELETE triggers and rolling back
// on an error. It returns nil on success.
//
// The delete triggers fire ONLY when the recursive-triggers flag is set
// (insert.c OE_Replace: GenerateRowDelete fires the row triggers when
// "recursive-triggers flag is set"; otherwise the conflicting rows are
// removed without firing — conflict3.test 13.x observes both dispositions).
func (e *DMLExecutor) deleteConflictRows(tree *btree.BTree, tableEntry *schema.Entry, conflicts []conflictInfo, colDefs []sql.ColumnDef, hasTriggers bool, deletedByConflict map[string]bool) *Result {
	// WITHOUT ROWID tables: delete conflict rows in PRIMARY KEY order (the
	// order SQLite scans its keyed table btree; hook2.test 2.3.5 observes
	// the preupdate DELETE order).
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		sort.SliceStable(conflicts, func(i, j int) bool {
			return e.withoutRowidLessVals(conflicts[i].values, conflicts[j].values, tableEntry.Name, tableEntry.SQL, colDefs)
		})
	}
	fireTriggers := hasTriggers && e.ctx.RecursiveTriggers()
	for _, cf := range conflicts {
		if res := e.deleteOneConflictRow(tableEntry, colDefs, cf, fireTriggers, deletedByConflict); res != nil {
			return res
		}
	}
	return nil
}

// deleteOneConflictRow deletes one OR REPLACE conflict row: BEFORE/AFTER
// DELETE triggers (only under recursive-triggers), the row delete, the
// preupdate DELETE hook, and the deleted-by-conflict registry entry.
func (e *DMLExecutor) deleteOneConflictRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, cf conflictInfo, fireTriggers bool, deletedByConflict map[string]bool) *Result {
	oldRow := buildRowMapFromValues(cf.values, colDefs, cf.rowID)
	if fireTriggers {
		if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	if _, err := e.deleteRowCells(tableEntry, colDefs, cf.rowID, cf.values); err != nil {
		return &Result{Error: err}
	}
	deletedByConflict[conflictSeenKey(tableEntry, colDefs, cf.rowID, cf.values)] = true
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	// Fire the preupdate hook for the deleted conflicting row.
	if res := e.fireConflictDeletePreupdate(tableEntry, cf.rowID, cf.values); res != nil {
		return res
	}
	if fireTriggers {
		if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	return nil
}

func (e *DMLExecutor) enforceUpdateForeignKey(tableEntry *schema.Entry, colDefs []sql.ColumnDef, c updateChange, snap *pager.PagerState) *Result {
	if !e.ctx.ForeignKeys() {
		return nil
	}
	if res := e.ctx.CheckForeignKeyViolations(tableEntry, colDefs, c.values, c.rowID); res.Error != nil {
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		return res
	}
	oldRow := buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
	newRow := buildRowMapFromValues(c.values, colDefs, c.rowID)
	if res := e.ctx.FkParentUpdate(tableEntry, colDefs, oldRow, newRow, c.rowID); res.Error != nil {
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		return res
	}
	return nil
}

// updateRowInPlace replaces the row being updated with its new value. It
// skips rows deleted by an earlier change's conflict resolution, aborts (with
// rollback) when the row vanished during trigger firing, and returns whether
// the row was actually updated.
func (e *DMLExecutor) updateRowInPlace(tree *btree.BTree, tableEntry *schema.Entry, colDefs []sql.ColumnDef, c updateChange, deletedByConflict map[string]bool, snap *pager.PagerState) (bool, *Result) {
	// If a conflict-resolution delete's trigger removed the row being
	// updated too (e.g. a recursive DELETE FROM t0 inside an AFTER DELETE
	// trigger), SQLite aborts the statement with the generic "constraint
	// failed" error and rolls it back. A row deleted by a PRIOR change's
	// conflict resolution is skipped, and a row deleted by THIS change's own
	// conflict resolution is also skipped.
	if deletedByConflict[conflictSeenKey(tableEntry, colDefs, c.rowID, c.oldValues)] {
		return false, nil
	}
	// WITHOUT ROWID rows have no rowid: the OLD-PK delete below is the
	// existence check (a vanished row deletes nothing and the re-insert
	// surfaces any anomaly), so the rowid probe runs for rowid tables only.
	withoutRowidKw := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	if !withoutRowidKw && !e.rowIDExists(tableEntry.Name, tableEntry.RootPage, c.rowID) {
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		return false, &Result{Error: fmt.Errorf("constraint failed")}
	}
	// Index maintenance (update.c UXF): remove the OLD row's entries from the
	// indexes this change touches before the cell delete.
	if err := e.deleteUpdateIndexEntries(tableEntry, colDefs, c, updateWriteRowID(c)); err != nil {
		return false, &Result{Error: err}
	}
	deletedCells, err := e.deleteRowCells(tableEntry, colDefs, c.rowID, c.oldValues)
	if err != nil {
		return false, &Result{Error: err}
	}
	// WITHOUT ROWID rows have no rowid to probe above: the OLD-PK delete IS
	// the existence check. Zero cells deleted means the row vanished while
	// this change's conflict resolution fired its delete triggers (the
	// trigger's DELETE FROM t2 removed the row being updated) — SQLite aborts
	// the statement with the generic "constraint failed" error, like the
	// rowid-table probe above (conflict3.test 13.2).
	if withoutRowidKw && deletedCells == 0 {
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		return false, &Result{Error: fmt.Errorf("constraint failed")}
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	if res := e.rewriteUpdatedRow(tree, tableEntry, colDefs, c, withoutRowidKw); res != nil {
		return false, res
	}
	// Fire the preupdate hook with the old and new row values (UPDATE OR
	// REPLACE's in-place update write).
	rowID := c.rowID
	if withoutRowidKw {
		rowID = 0
	}
	if res := e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "UPDATE",
		DB:    e.schemaNameForPager(e.dmlPager(tableEntry.Name)),
		Table: tableEntry.Name,
		RowID: rowID, RowID2: rowID,
		RowidTable: !withoutRowidKw,
		Old:        append([]interface{}(nil), c.oldValues...),
		New:        append([]interface{}(nil), c.values...),
	}); res != nil {
		return false, res
	}
	return true, nil
}

// rewriteUpdatedRow rewrites one change's row in place: encode (WITHOUT
// ROWID rows re-encode PK-first through the WR storage tree), insert at the
// NEW rowid when the UPDATE re-keys the row (SET rowid=N — writing the
// record, whose PK column holds the new id, at the old rowid desyncs the
// btree key from the record, writeUpdateCell parity), bump the rowid cache,
// and write the NEW entries into the indexes this change touches.
func (e *DMLExecutor) rewriteUpdatedRow(tree *btree.BTree, tableEntry *schema.Entry, colDefs []sql.ColumnDef, c updateChange, withoutRowidKw bool) *Result {
	writeRowID := updateWriteRowID(c)
	record, cellType, tree, err := e.encodeUpdatedRecord(tree, tableEntry.Name, tableEntry, colDefs, c.values, withoutRowidKw)
	if err != nil {
		return &Result{Error: err}
	}
	newCell := &storage.Cell{
		Type:    cellType,
		RowID:   writeRowID,
		Payload: record,
	}
	if err := tree.InsertCell(newCell); err != nil {
		return &Result{Error: err}
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage, writeRowID)
	// Write the NEW row's entries into the indexes this change touches (the
	// insert phase; the OLD entries were removed above).
	if err := e.writeUpdateIndexEntries(tableEntry, colDefs, c, writeRowID); err != nil {
		return &Result{Error: err}
	}
	return nil
}
