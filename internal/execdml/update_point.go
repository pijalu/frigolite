package execdml

// Rowid-pinned single-row UPDATE fast path (the OLTP workhorse
// "UPDATE t SET c=c+1 WHERE id=?"). The generic pipeline collects changes
// through the seek plan, then re-walks them through the bulk apply machinery
// (dedupe maps, rowid sets, per-step re-lookups, and — when the new cell size
// differs — an O(table) DeleteCellsWhere sweep). When the statement is the
// plain point shape, one seek reads the row, the SET expressions rewrite the
// value slots positionally, the record encodes once into the executor's
// reusable buffer, and the write takes the loc==0 in-place overwrite or a
// seek delete + re-insert. Every gate mirrors runUpdatePipeline's routing:
// anything the fast path does not handle exactly returns handled=false and
// the statement runs the unmodified generic pipeline.

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// pointUpdateEligible reports the statement shape the fast path rewrites
// exactly (the clause-level shape plus the target-table gates).
func (e *DMLExecutor) pointUpdateEligible(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	return pointUpdateStatementShapeOK(s) &&
		e.pointUpdateTargetOK(s, tableEntry, colDefs)
}

// pointUpdateStatementShapeOK reports the clause-level shape: a plain
// statement (no OR clause, no RETURNING, no ORDER BY/LIMIT) over the target
// table only (no FROM), with the single SET list form.
func pointUpdateStatementShapeOK(s *sql.UpdateStmt) bool {
	if s.OnConflict != "" || s.HasReturning || len(s.OrderBy) > 0 || s.Limit != nil {
		return false
	}
	if s.From.Name != "" || s.From.Subquery != nil || len(s.FromJoins) > 0 {
		return false
	}
	return len(s.SetParenColumns) == 0
}

// pointUpdateTargetOK reports the target-table gates: an ordinary rowid
// table without triggers, FK enforcement, per-constraint ON CONFLICT
// clauses, or generated columns, whose SET targets are ordinary columns.
func (e *DMLExecutor) pointUpdateTargetOK(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	if tableIsWithoutRowid(tableEntry.SQL) {
		return false
	}
	if e.hasTriggersForTable(tableEntry.Name) || e.ctx.ForeignKeys() {
		return false
	}
	if hasColumnConflictClauses(colDefs, tableEntry, e) {
		return false
	}
	for i := range colDefs {
		if colDefs[i].Generated != nil {
			return false
		}
	}
	return e.pointUpdateSetTargetsOK(s, colDefs)
}

// pointUpdateSetTargetsOK reports whether every SET target resolves to an
// ordinary declared column (a rowid/IPK target re-keys the row —
// rowidMoveConflict, delete+reinsert at a new rowid — which the fast path
// does not handle), and the pinned rowid equality is the WHOLE WHERE clause
// (the seek's hit then matches by construction and the clause needs no
// evaluation).
func (e *DMLExecutor) pointUpdateSetTargetsOK(s *sql.UpdateStmt, colDefs []sql.ColumnDef) bool {
	colIndex := e.columnIndexFor(colDefs)
	for _, a := range s.Assignments {
		ci, ok := colIndex[strings.ToLower(a.Column)]
		if !ok || ci < 0 || ci >= len(colDefs) || isIPKRowidAliasCol(colDefs[ci]) {
			return false
		}
	}
	return andTermCount(s.Where) == 1
}

// applyPointUpdate applies the rowid-pinned single-row UPDATE fast path:
// collect the pinned row, run the same constraint and conflict gates as
// runPlainUpdate over the single change, write it. handled=false means the
// statement does not fit the fast path's exact shape and must run the
// generic pipeline (collect → preCheck → dispatch → apply). One b-tree
// wrapper serves both the row fetch and the write (the seek tree stays
// open — the write primitives save/restore cursors themselves).
func (e *DMLExecutor) applyPointUpdate(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) (*Result, bool) {
	if !e.pointUpdateEligible(s, tableEntry, colDefs) {
		return nil, false
	}
	scanName := updateScanName(s, tableEntry.Name)
	plan := e.planDMLSeek(tableEntry, colDefs, s.Where, scanName, e.currentDMLCtx)
	// plan.index != nil is an indexed-seek plan: candidate narrowing only,
	// the WHERE must still be evaluated per candidate — not this shape.
	if plan == nil || plan.empty || plan.index != nil {
		return nil, false
	}
	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	defer tree.Close() // seek+write tree is function-local
	ch, matched, res := e.collectPointUpdateRow(tree, s, tableEntry, colDefs, plan.rowid)
	if res != nil {
		return res, true
	}
	if !matched {
		// The pinned rowid has no row: the generic pipeline collects zero
		// changes and the statement is a no-op.
		return &Result{}, true
	}
	changes := []updateChange{ch}
	// NOT NULL/CHECK pre-check — preCheckUpdate exactly (the OR-clause and
	// per-column ON CONFLICT dispositions that could drop or substitute the
	// change are excluded by the gates, so the single change passes through).
	changes, pres := e.preCheckUpdate(s, tableEntry, colDefs, changes)
	if pres.Error != nil {
		return pres, true
	}
	if len(changes) != 1 {
		return nil, false
	}
	// UNIQUE/PK conflict gate — checkUpdateConflicts exactly (the P1
	// change-detection gate skips its scans when nothing constrained moved).
	if res := e.checkUpdateConflicts(tableEntry, colDefs, changes); res.Error != nil {
		return res, true
	}
	// FOREIGN KEY parent actions run in runPlainUpdate when enforcement is
	// on; the gates exclude that case.
	if res := e.deleteUpdateIndexEntriesFor(tableEntry, colDefs, changes); res != nil {
		return &Result{Error: res}, true
	}
	if wres := e.writePointUpdateRow(tableEntry.Name, tree, tableEntry.RootPage, changes[0], tableEntry, colDefs); wres.Error != nil {
		return wres, true
	}
	return &Result{Changes: 1}, true
}

// collectPointUpdateRow reads the pinned row by rowid and builds its change:
// the raw value slots with defaults applied, the SET expressions applied
// against a positional row (the scan loop's evaluation model), generated
// columns recomputed. matched=false reports the rowid is absent; res non-nil
// is a real statement error. A fetch anomaly returns matched=false with a
// nil res and the caller must fall back to the generic pipeline.
func (e *DMLExecutor) collectPointUpdateRow(tree *btree.BTree, s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, rowID int64) (ch updateChange, matched bool, res *Result) {
	// SQLITE_TEST interrupt countdown: one op per row examined
	// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count).
	if err := e.ctx.CheckProgress(); err != nil {
		return updateChange{}, false, &Result{Error: err}
	}
	// The very next act is an explicit rowid seek, which re-descends from
	// the root: skip OpenCursor's leftmost-leaf descent.
	cursor, err := tree.OpenCursorAtRoot()
	if err != nil {
		return updateChange{}, false, nil // anomaly: generic pipeline
	}
	found, serr := cursor.SeekToRowID(rowID)
	if serr != nil {
		return updateChange{}, false, nil // anomaly: generic pipeline
	}
	if !found {
		return updateChange{}, false, &Result{}
	}
	cell, rerr := cursor.ReadCell()
	if rerr != nil {
		return updateChange{}, false, nil // anomaly: generic pipeline
	}
	rec, derr := storage.DecodeRecord(cell.Payload)
	if derr != nil || rec == nil {
		return updateChange{}, false, nil // anomaly: generic pipeline
	}
	e.ctx.RemapWRRecordToDeclared(rec, tableEntry.SQL, colDefs)

	values, oldValues := updateChangeValueSlots(rec, e.columnIndexFor(colDefs))
	// Rows written before ALTER TABLE ADD COLUMN read their added-column
	// DEFAULTs (buildUpdateChange parity).
	e.applyUpdateColumnDefaults(values, colDefs, len(rec.Values))
	e.applyUpdateColumnDefaults(oldValues, colDefs, len(rec.Values))

	// SET evaluation against the collected row map — the exact evaluation
	// row the small-candidate seek collect built before the positional
	// tranche (the positional plan amortizes only across candidate rows; a
	// single pinned row keeps the map, which is both cheaper and original).
	row := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
	newRowID, aerr := e.applyUpdateAssignments(s, row, e.columnIndexFor(colDefs), colDefs, values)
	if aerr != nil {
		return updateChange{}, false, &Result{Error: aerr}
	}
	if newRowID != nil {
		return updateChange{}, false, nil // re-key: generic pipeline
	}
	if gerr := e.recomputeUpdateGenerated(colDefs, values); gerr != nil {
		return updateChange{}, false, &Result{Error: gerr}
	}
	return updateChange{rowID: cell.RowID, values: values, oldValues: oldValues}, true, nil
}

// writePointUpdateRow writes one same-rowid change: the loc==0 in-place
// overwrite when the new cell is the same size (btree.OverwriteCellByRowID),
// a seek delete + re-insert otherwise. Index maintenance, the rowid-cache
// bump/invalidate and the preupdate hook fire exactly as applyUpdateChanges'
// single-change bulk run does.
func (e *DMLExecutor) writePointUpdateRow(tableName string, tree *btree.BTree, rootPage uint32, ch updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	record, err := e.appendEncodedRecord(ch.values)
	if err != nil {
		return &Result{Error: err}
	}
	cellData := storage.EncodeCell(&storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   ch.rowID,
		Payload: record,
	})
	done, oerr := tree.OverwriteCellByRowID(ch.rowID, cellData)
	if oerr != nil {
		return &Result{Error: oerr}
	}
	if !done {
		// Size changed or a guard declined: dropCell + insertCell (sqlite3
		// sqlite3BtreeInsert's non-fast loc==0 branch), with the seek delete
		// instead of the bulk sweep.
		if _, derr := tree.DeleteCellByRowID(ch.rowID); derr != nil {
			return &Result{Error: derr}
		}
		if icerr := tree.InsertCell(&storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   ch.rowID,
			Payload: record,
		}); icerr != nil {
			return &Result{Error: icerr}
		}
	}
	// The NEW row's entries into every index the change touches (the insert
	// phase; the OLD entries were removed before the write).
	if ierr := e.writeUpdateIndexEntriesFor(tableEntry, colDefs, ch.oldValues, ch.rowID, ch.values, ch.rowID); ierr != nil {
		return &Result{Error: ierr}
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableName), rootPage, ch.rowID)
	if res := e.fireUpdatePreupdate(tableName, ch); res != nil {
		return res
	}
	// applyUpdateChanges invalidates the rowid cache after the re-insert loop
	// (SQLite recomputes the rowid counter after any DELETE/UPDATE).
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableName), rootPage)
	return &Result{}
}

// appendEncodedRecord encodes values into the executor's reusable record
// buffer. The btree write paths copy the payload bytes into pages (or
// overflow chains) synchronously and never retain the slice, so the buffer
// can be reused by the next statement.
func (e *DMLExecutor) appendEncodedRecord(values []interface{}) ([]byte, error) {
	buf, err := storage.AppendEncodeRecord(e.encBuf[:0], values)
	if err != nil {
		return nil, err
	}
	e.encBuf = buf
	return buf, nil
}
