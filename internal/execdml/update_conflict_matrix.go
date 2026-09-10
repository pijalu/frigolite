package execdml

import (
	"strings"

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
		// FOREIGN KEY parent action for this row, before the write (a
		// mid-statement FK error with row-by-row processing keeps the rows
		// written so far).
		if e.ctx.ForeignKeys() {
			oldRow := buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
			newRow := buildRowMapFromValues(c.values, colDefs, c.rowID)
			if res := e.ctx.FkParentUpdate(tableEntry, colDefs, oldRow, newRow, c.rowID); res.Error != nil {
				e.ctx.RestorePager(e.ctx.Pager(), snap)
				return res
			}
		}
		// UNIQUE/PRIMARY KEY conflict check against the current table state,
		// excluding the row being updated.
		colIndexLocal := buildColumnIndex(colDefs)
		uniqueCols := uniqueColsForTable(colDefs)
		idxColsList := e.uniqueIndexColumns(tableEntry.Name)
		wrOrder := e.ctx.WRStorageOrder(tableEntry.SQL, colDefs)
		tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
		var conflictErr error
		{
			if res := e.checkEarlierChanges(nil, 0, c, colDefs, colIndexLocal, uniqueCols, idxColsList, tableEntry.Name); res.Error != nil {
				conflictErr = res.Error
			}
			if conflictErr == nil {
				if res := e.checkLiveTableConflictsWR(tree, nil, c, colDefs, colIndexLocal, uniqueCols, idxColsList, tableEntry, wrOrder); res.Error != nil {
					conflictErr = res.Error
				}
			}
		}
		if conflictErr != nil {
			// The violated constraint's own resolution applies.
			msg := conflictErr.Error()
			if !strings.Contains(msg, "UNIQUE constraint failed: ") {
				e.ctx.RestorePager(e.ctx.Pager(), snap)
				return &Result{Error: conflictErr}
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
				continue
			case "REPLACE":
				// Delete every conflicting row, then apply the change.
				if rres := e.replaceDeleteConflicts(e.ctx.Pager(), tableEntry, colDefs, c.values, c.rowID); rres.Error != nil {
					e.ctx.RestorePager(e.ctx.Pager(), snap)
					return rres
				}
				if ares := e.applyUpdateChanges(tableEntry.Name, tableEntry.RootPage, []updateChange{c}); ares.Error != nil {
					e.ctx.RestorePager(e.ctx.Pager(), snap)
					return ares
				}
				applied++
				continue
			case "FAIL":
				// Rows written before the conflict survive; the statement
				// fails (no snapshot restore).
				out := &Result{Error: conflictErr}
				out.SetKeepPriorRowsOnError()
				return out
			case "ROLLBACK":
				// The statement's changes back out AND the whole transaction
				// rolls back.
				e.ctx.RestorePager(e.ctx.Pager(), snap)
				out := &Result{Error: conflictErr}
				out.SetRollbackTxOnError()
				return out
			default:
				// ABORT: back out the statement's applied rows and fail.
				e.ctx.RestorePager(e.ctx.Pager(), snap)
				return &Result{Error: conflictErr}
			}
		}
		if ares := e.applyUpdateChanges(tableEntry.Name, tableEntry.RootPage, []updateChange{c}); ares.Error != nil {
			e.ctx.RestorePager(e.ctx.Pager(), snap)
			return ares
		}
		applied++
	}
	return &Result{Changes: applied}
}
