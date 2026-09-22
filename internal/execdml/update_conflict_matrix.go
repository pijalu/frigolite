package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// hasColumnConflictClauses reports whether any column (or table-level
// constraint) of the table carries its own ON CONFLICT resolution clause.
// Only such tables need the per-row disposition matrix; plain updates keep
// the validate-all-then-apply path (ABORT semantics).
func hasColumnConflictClauses(colDefs []sql.ColumnDef, tableEntry *schema.Entry, e *DMLExecutor) bool {
	for i := range colDefs {
		if colDefs[i].OnConflict != "" {
			return true
		}
	}
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey || tc.Type == sql.ConstraintCheck) && tc.OnConflict != "" {
			return true
		}
	}
	return false
}

// runPlainUpdatePerRow applies a plain UPDATE row-by-row so each row's
// UNIQUE/PRIMARY KEY conflict resolves under the VIOLATED constraint's own
// ON CONFLICT clause (SQLite evaluate-one-row-at-a-time semantics;
// conflict-9.3..9.25 mix IGNORE/FAIL/REPLACE/ABORT/ROLLBACK columns in one
// table). ABORT/ROLLBACK back out the statement's applied rows via the
// pager snapshot; FAIL keeps them; IGNORE skips the row; REPLACE deletes the
// conflicting rows. FOREIGN KEY parent actions are checked per row before
// the write.
func (e *DMLExecutor) runPlainUpdatePerRow(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	snap := e.ctx.Pager().Snapshot()
	applied := int64(0)
	for _, c := range changes {
		written, res := e.applyPerRowUpdateChange(c, tableEntry, colDefs, snap)
		if res != nil {
			return res
		}
		if written {
			applied++
		}
	}
	return &Result{Changes: applied}
}

// applyPerRowUpdateChange writes one change under per-row conflict
// resolution: FOREIGN KEY parent action, UNIQUE/PK conflict check, then the
// row's own ON CONFLICT disposition. written=false with a nil Result skips
// the row (IGNORE) or reports it already applied (REPLACE).
func (e *DMLExecutor) applyPerRowUpdateChange(c updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef, snap *pager.PagerState) (bool, *Result) {
	// FOREIGN KEY parent action for this row, before the write (a
	// mid-statement FK error with row-by-row processing keeps the rows
	// written so far).
	if e.ctx.ForeignKeys() {
		oldRow := buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
		newRow := buildRowMapFromValues(c.values, colDefs, c.rowID)
		if res := e.ctx.FkParentUpdate(tableEntry, colDefs, oldRow, newRow, c.rowID); res.Error != nil {
			e.ctx.RestorePager(e.ctx.Pager(), snap)
			return false, res
		}
	}
	conflictErr := e.perRowConflictError(c, tableEntry, colDefs)
	if conflictErr != nil {
		return e.resolvePerRowConflict(conflictErr, c, tableEntry, colDefs, snap)
	}
	if ares := e.applyUpdateChanges(tableEntry.Name, tableEntry.RootPage, []updateChange{c}); ares.Error != nil {
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		return false, ares
	}
	return true, nil
}

// perRowConflictError checks one change for UNIQUE/PK conflicts against the
// current table state (excluding the row being updated), returning the
// violated constraint's error or nil.
func (e *DMLExecutor) perRowConflictError(c updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef) error {
	colIndexLocal := buildColumnIndex(colDefs)
	uniqueCols := uniqueColsForTable(colDefs)
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	wrOrder := e.ctx.WRStorageOrder(tableEntry.SQL, colDefs)
	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	if res := e.checkEarlierChanges(nil, 0, c, colDefs, colIndexLocal, uniqueCols, idxColsList, tableEntry.Name); res.Error != nil {
		return res.Error
	}
	if res := e.checkLiveTableConflictsWR(tree, nil, c, colDefs, colIndexLocal, uniqueCols, idxColsList, tableEntry, wrOrder); res.Error != nil {
		return res.Error
	}
	return nil
}

// resolvePerRowConflict disposes one UNIQUE conflict under the violated
// column's own ON CONFLICT clause: IGNORE skips the row, REPLACE deletes the
// conflicting rows and applies the change, FAIL keeps prior rows, ROLLBACK
// also rolls back the transaction, ABORT backs out the applied rows. A
// non-UNIQUE error stands. written=false + nil Result: the row was skipped.
func (e *DMLExecutor) resolvePerRowConflict(conflictErr error, c updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef, snap *pager.PagerState) (bool, *Result) {
	// The violated constraint's own resolution applies.
	msg := conflictErr.Error()
	if !strings.Contains(msg, "UNIQUE constraint failed: ") {
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		return false, &Result{Error: conflictErr}
	}
	last := msg[strings.LastIndex(msg, ".")+1:]
	clause := "ABORT"
	for i := range colDefs {
		if strings.EqualFold(colDefs[i].Name, last) && colDefs[i].OnConflict != "" {
			clause = colDefs[i].OnConflict
		}
	}
	switch strings.ToUpper(clause) {
	case "IGNORE":
		// Skip this row's update; the row keeps its old values.
		return false, nil
	case "REPLACE":
		// Delete every conflicting row, then apply the change.
		if rres := e.replaceDeleteConflicts(e.ctx.Pager(), tableEntry, colDefs, c.values, c.rowID); rres.Error != nil {
			e.ctx.RestorePager(e.ctx.Pager(), snap)
			return false, rres
		}
		if ares := e.applyUpdateChanges(tableEntry.Name, tableEntry.RootPage, []updateChange{c}); ares.Error != nil {
			e.ctx.RestorePager(e.ctx.Pager(), snap)
			return false, ares
		}
		return true, nil
	case "FAIL":
		// Rows written before the conflict survive; the statement
		// fails (no snapshot restore).
		out := &Result{Error: conflictErr}
		out.SetKeepPriorRowsOnError()
		return false, out
	case "ROLLBACK":
		// The statement's changes back out AND the whole transaction
		// rolls back.
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		out := &Result{Error: conflictErr}
		out.SetRollbackTxOnError()
		return false, out
	default:
		// ABORT: back out the statement's applied rows and fail.
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		return false, &Result{Error: conflictErr}
	}
}
