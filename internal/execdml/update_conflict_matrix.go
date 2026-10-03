package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
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
	stmt := e.ctx.Pager().BeginStatement()
	defer e.ctx.Pager().EndStatement(stmt)
	applied := int64(0)
	for _, c := range changes {
		written, res := e.applyPerRowUpdateChange(c, tableEntry, colDefs, stmt)
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
func (e *DMLExecutor) applyPerRowUpdateChange(c updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef, stmt *pager.StmtJournal) (bool, *Result) {
	// FOREIGN KEY parent action for this row, before the write (a
	// mid-statement FK error with row-by-row processing keeps the rows
	// written so far).
	if e.ctx.ForeignKeys() {
		oldRow := buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
		newRow := buildRowMapFromValues(c.values, colDefs, c.rowID)
		if res := e.ctx.FkParentUpdate(tableEntry, colDefs, oldRow, newRow, c.rowID); res.Error != nil {
			e.ctx.RollbackPagerStatement(e.ctx.Pager(), stmt)
			return false, res
		}
	}
	conflictErr := e.perRowConflictError(c, tableEntry, colDefs)
	if conflictErr != nil {
		return e.resolvePerRowConflict(conflictErr, c, tableEntry, colDefs, stmt)
	}
	if ares := e.applyUpdateChanges(tableEntry.Name, tableEntry.RootPage, []updateChange{c}); ares.Error != nil {
		e.ctx.RollbackPagerStatement(e.ctx.Pager(), stmt)
		return false, ares
	}
	return true, nil
}

// perRowConflictError checks one change for UNIQUE/PK conflicts against the
// current table state (excluding the row being updated), returning the
// violated constraint's error or nil.
func (e *DMLExecutor) perRowConflictError(c updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef) error {
	colIndexLocal := e.columnIndexFor(colDefs)
	uniqueCols := uniqueColsForTable(colDefs)
	idxColsList := e.updateConstrainedDefs(tableEntry, colDefs)
	wrOrder := e.ctx.WRStorageOrder(tableEntry.SQL, colDefs)
	// Change-detection gate (see checkUpdateConflicts): nothing constrained
	// moved, so no other row can conflict with this change.
	if e.updateConstraintUnchanged(c, colDefs, colIndexLocal, uniqueCols, idxColsList, dmlConstraintRowMapsNeeded(colIndexLocal, idxColsList)) {
		return nil
	}
	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	defer tree.Close() // conflict-scan tree is function-local
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
func (e *DMLExecutor) resolvePerRowConflict(conflictErr error, c updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef, stmt *pager.StmtJournal) (bool, *Result) {
	// The violated constraint's own resolution applies.
	msg := conflictErr.Error()
	if !strings.Contains(msg, "UNIQUE constraint failed: ") {
		e.ctx.RollbackPagerStatement(e.ctx.Pager(), stmt)
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
			e.ctx.RollbackPagerStatement(e.ctx.Pager(), stmt)
			return false, rres
		}
		if ares := e.applyUpdateChanges(tableEntry.Name, tableEntry.RootPage, []updateChange{c}); ares.Error != nil {
			e.ctx.RollbackPagerStatement(e.ctx.Pager(), stmt)
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
		e.ctx.RollbackPagerStatement(e.ctx.Pager(), stmt)
		out := &Result{Error: conflictErr}
		out.SetRollbackTxOnError()
		return false, out
	default:
		// ABORT: back out the statement's applied rows and fail.
		e.ctx.RollbackPagerStatement(e.ctx.Pager(), stmt)
		return false, &Result{Error: conflictErr}
	}
}

// uniqueIndexDefNeedsRowMaps reports whether one UNIQUE index definition's
// conflict check needs a name-keyed row map: a partial index's WHERE
// predicate evaluates against the row, and an expression/qualified key (or a
// rowid alias, which maps to the negative pseudo-slot) evaluates its
// expression against the row. Plain declared-column keys read the positional
// values directly.
func uniqueIndexDefNeedsRowMaps(def uniqueIndexDef, colIndex map[string]int) bool {
	if def.Where != "" {
		return true
	}
	for _, cn := range def.Cols {
		if strings.ContainsAny(cn, "(.") {
			return true
		}
		if idx, ok := colIndex[strings.ToLower(cn)]; !ok || idx < 0 {
			return true
		}
	}
	return false
}

// indexDefsMatch reports whether two value sets agree on the indexed columns
// of any UNIQUE index (full and partial). The per-definition row maps are
// built lazily — only definitions whose predicate or keys evaluate against a
// row ever materialize one.
func indexDefsMatch(e *DMLExecutor, a, b []interface{}, colDefs []sql.ColumnDef, colIndex map[string]int, idxColsList []uniqueIndexDef, aRowID, bRowID int64) bool {
	for _, def := range idxColsList {
		var nrow, orow RowMap
		if uniqueIndexDefNeedsRowMaps(def, colIndex) {
			nrow = buildRowMapFromValues(b, colDefs, bRowID)
			orow = buildRowMapFromValues(a, colDefs, aRowID)
		}
		if inIndex, _ := e.evalIndexWhere(def.Where, nrow); !inIndex {
			continue
		}
		match := true
		for _, cn := range def.Cols {
			rkv, rok := e.indexKeyValue(cn, colDefs, colIndex, a, orow)
			ckv, cok := e.indexKeyValue(cn, colDefs, colIndex, b, nrow)
			if !rok || !cok || util.CompareValues(rkv, ckv) != 0 {
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

// valuesConflict reports whether two value sets conflict on any UNIQUE/PRIMARY
// KEY column or UNIQUE index (partial-index predicates evaluated). rowIDa and
// rowIDb are the owning rows' rowids: SQLite stores NULL in the INTEGER
// PRIMARY KEY (rowid-alias) column of each record, so uniqueColsMatch
// substitutes the rowid for a stored NULL — two different rows must not both
// substitute the same placeholder (update.test: UPDATE of a 2-row table with
// a rowid-alias PK used to self-conflict with rowID 0==0).
func (e *DMLExecutor) valuesConflict(a, b []interface{}, rowIDa, rowIDb int64, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef) bool {
	if uniqueColsMatch(a, b, colDefs, rowIDa, rowIDb, uniqueCols) {
		return true
	}
	return indexDefsMatch(e, a, b, colDefs, colIndex, idxColsList, rowIDa, rowIDb)
}

// uniqueConflictError builds a SQLite-style UNIQUE constraint error for the
// first conflicting column.
func (e *DMLExecutor) uniqueConflictError(tableName string, colDefs []sql.ColumnDef, colIndex map[string]int, a, b []interface{}, aRowID, bRowID int64, uniqueCols []int, idxColsList []uniqueIndexDef) error {
	if idx := firstConflictColIdx(colDefs, a, b, aRowID, bRowID, uniqueCols); idx >= 0 {
		return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableName, colDefs[idx].Name)
	}
	for _, def := range idxColsList {
		if e.valuesConflict(a, b, aRowID, bRowID, colDefs, colIndex, nil, []uniqueIndexDef{def}) {
			return uniqueIndexColsConflictError(tableName, def)
		}
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", tableName)
}

// firstConflictColIdx returns the index (into colDefs) of the first
// UNIQUE/PRIMARY KEY column on which the two value sets agree, -1 when none.
func firstConflictColIdx(colDefs []sql.ColumnDef, a, b []interface{}, aRowID, bRowID int64, uniqueCols []int) int {
	for _, idx := range uniqueCols {
		// Rowid-alias convention: the INTEGER PRIMARY KEY column reads
		// back NULL from the stored record — substitute the owning
		// row's rowid (uniqueColsMatch parity), else the PK-conflict
		// message degrades to the column-less form ("t5" not "t5.a",
		// conflict-12.3).
		if uniqueColValuesMatch(a, b, colDefs, aRowID, bRowID, idx) {
			return idx
		}
	}
	return -1
}

// uniqueIndexColsConflictError builds the error naming every column of the
// violated UNIQUE index ("UNIQUE constraint failed: t.a, t.b").
func uniqueIndexColsConflictError(tableName string, def uniqueIndexDef) error {
	parts := make([]string, len(def.Cols))
	for i, cn := range def.Cols {
		parts[i] = tableName + "." + cn
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
}
