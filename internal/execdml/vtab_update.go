package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// vtabRowMap wraps one virtual-table row as a Row for WHERE/SET expression
// evaluation. Hidden columns are part of the map so constraints like
// schema='aux1' re-check correctly. rowid/_rowid_/oid are added when the
// cursor exposes a rowid and the vtab does not declare a column of that
// name (UPDATE t SET rowid=..., DELETE ... WHERE rowid=N).
func vtabRowMap(colDefs []sql.ColumnDef, values []interface{}, rowid int64, hasRowid bool) execquery.RowMap {
	m := make(execquery.RowMap, len(colDefs)+3)
	for i, cd := range colDefs {
		if i < len(values) {
			m[cd.Name] = &util.ColumnValue{Value: values[i]}
		}
	}
	if hasRowid {
		for _, alias := range []string{"rowid", "_rowid_", "oid"} {
			if _, taken := m[alias]; !taken {
				m[alias] = &util.ColumnValue{Value: rowid}
			}
		}
	}
	return m
}

// resolveVTabUpdater resolves the UPDATE/INSERT target to a vtab instance
// implementing vtab.RowUpdater. handled is false when the name is not a
// virtual table (the caller falls through to the b-tree paths).
func (e *DMLExecutor) resolveVTabUpdater(name string) (vtab.VirtualTable, []sql.ColumnDef, *Result, bool) {
	vt, colDefs, ok, err := e.ctx.VTabUpdaterInstance(name)
	if !ok {
		return nil, nil, nil, false
	}
	if err != nil {
		return nil, nil, &Result{Error: err}, true
	}
	updater, ok := vt.(vtab.RowUpdater)
	if !ok {
		return nil, nil, &Result{Error: fmt.Errorf("table %s may not be modified", name)}, true
	}
	_ = updater
	return vt, colDefs, nil, true
}

// rejectUnsafeVTabUse errors when the target is a DIRECTONLY virtual table
// referenced from inside a trigger body: SQLITE_VTAB_DIRECTONLY forbids use
// within triggers/views regardless of PRAGMA trusted_schema (dbpagefault
// 3.x).
func (e *DMLExecutor) rejectUnsafeVTabUse(name string) *Result {
	if e.ctx.TriggerDepth() > 0 && e.ctx.DirectOnlyVTab(name) {
		return &Result{Error: fmt.Errorf("unsafe use of virtual table %q", name)}
	}
	// src/dbpage.c dbpageBeginTrans opens a write transaction on every btree;
	// inside an explicit read transaction that upgrade fails with
	// SQLITE_LOCKED (dbpage-620: "database is locked").
	if e.ctx.InTransaction() && e.ctx.DirectOnlyVTab(name) {
		return &Result{Error: fmt.Errorf("database is locked")}
	}
	return nil
}

// execVTabUpdate runs an UPDATE whose target is an updatable virtual table:
// it materializes the instance rows, applies the WHERE filter, evaluates the
// SET assignments per matching row and hands the full new row to xUpdate
// (src/dbpage.c dbpageUpdate parity). handled is false when the target is
// not a virtual table.
func (e *DMLExecutor) execVTabUpdate(s *sql.UpdateStmt) (*Result, bool) {
	vt, colDefs, res, handled := e.resolveVTabUpdater(s.Table)
	if !handled {
		return nil, false
	}
	if res != nil {
		return res, true
	}
	cur, err := vt.Open()
	if err != nil {
		return &Result{Error: err}, true
	}
	defer cur.Close()
	rowidWriter, rowidCapable := vt.(RowidConflictWriter)
	var ridCur vtab.RowidCursor
	if rowidCapable {
		ridCur, _ = cur.(vtab.RowidCursor)
	}
	var changes int64
	for cur.Next() {
		applied, res := e.vtabUpdateRow(s, cur, vt, colDefs, rowidWriter, rowidCapable, ridCur)
		if res != nil {
			return res, true
		}
		if applied {
			changes++
		}
	}
	return &Result{Changes: changes}, true
}

// vtabUpdateRow processes the cursor's current row: the WHERE filter gates
// the SET application and the xUpdate write. applied=false skips the row
// (filter mismatch or conflict-skip); a non-nil Result carries the error.
func (e *DMLExecutor) vtabUpdateRow(s *sql.UpdateStmt, cur vtab.Cursor, vt vtab.VirtualTable, colDefs []sql.ColumnDef, rowidWriter RowidConflictWriter, rowidCapable bool, ridCur vtab.RowidCursor) (bool, *Result) {
	oldRowid := int64(0)
	if ridCur != nil {
		oldRowid = ridCur.Rowid()
	}
	row := vtabCursorRow(cur, len(colDefs))
	rm := vtabRowMap(colDefs, row, oldRowid, ridCur != nil)
	pass, err := e.ctx.RowPassesWhere(s.Where, rm, nil)
	if err != nil {
		return false, &Result{Error: err}
	}
	if !pass {
		return false, nil
	}
	newValues, newRowid, res := e.vtabApplyAssignments(s, colDefs, row, rm, rowidCapable, oldRowid)
	if res != nil {
		return false, res
	}
	updater := vt.(vtab.RowUpdater)
	if rowidCapable {
		r, applied := vtabApplyRowidUpdate(rowidWriter, row, newValues, oldRowid, newRowid, vtabStatementResolve(s.OnConflict, false))
		if r != nil {
			return false, r
		}
		return applied, nil
	}
	skipped, res := applyVTabUpdateAction(s, row, newValues, updater)
	if res != nil {
		return false, res
	}
	return !skipped, nil
}

// vtabCursorRow materializes the cursor's current row; a Column error
// truncates the row (matching the cursor's short read).
func vtabCursorRow(cur vtab.Cursor, n int) []interface{} {
	row := make([]interface{}, n)
	for i := range row {
		v, err := cur.Column(i)
		if err != nil {
			break
		}
		row[i] = v
	}
	return row
}

// vtabApplyAssignments evaluates the UPDATE's SET assignments against the
// current row: a rowid assignment (rowid-capable tables) changes newRowid; a
// column assignment writes the evaluated slot, preserving "explicitly
// assigned NULL" semantics for vtabs via vtab.ExplicitNull (zipfile:
// SET data=NULL makes the entry a directory).
func (e *DMLExecutor) vtabApplyAssignments(s *sql.UpdateStmt, colDefs []sql.ColumnDef, row []interface{}, rm execquery.RowMap, rowidCapable bool, oldRowid int64) (newValues []interface{}, newRowid int64, res *Result) {
	newValues = append([]interface{}(nil), row...)
	newRowid = oldRowid
	for _, a := range s.Assignments {
		if strings.EqualFold(a.Column, "rowid") && rowidCapable {
			if res := e.vtabSetRowid(a, rm, &newRowid); res != nil {
				return nil, 0, res
			}
			continue
		}
		if res := e.vtabSetColumn(colDefs, a, rm, newValues); res != nil {
			return nil, 0, res
		}
	}
	return newValues, newRowid, nil
}

// vtabSetRowid evaluates a SET rowid assignment.
func (e *DMLExecutor) vtabSetRowid(a sql.Assignment, rm execquery.RowMap, newRowid *int64) *Result {
	v, err := e.ctx.EvalExpr(a.Value, rm)
	if err != nil {
		return &Result{Error: err}
	}
	if n, ok := util.UnwrapColumnValue(v).(int64); ok {
		*newRowid = n
	}
	return nil
}

// vtabSetColumn evaluates one SET column assignment into the new row's slot.
func (e *DMLExecutor) vtabSetColumn(colDefs []sql.ColumnDef, a sql.Assignment, rm execquery.RowMap, newValues []interface{}) *Result {
	idx := -1
	for i, cd := range colDefs {
		if cd.Name == a.Column {
			idx = i
			break
		}
	}
	if idx < 0 {
		return &Result{Error: fmt.Errorf("no such column: %s", a.Column)}
	}
	v, err := e.ctx.EvalExpr(a.Value, rm)
	if err != nil {
		return &Result{Error: err}
	}
	uv := util.UnwrapColumnValue(v)
	if uv == nil {
		uv = vtab.ExplicitNull{}
	}
	newValues[idx] = uv
	return nil
}

// vtabApplyRowidUpdate routes one UPDATE row through the rowid-aware write.
// applied=false means the row was skipped under the statement's resolution;
// a non-nil Result carries the error (with keep-prior-rows set under the
// OR FAIL keep-prior-rows error contract).
func vtabApplyRowidUpdate(w RowidConflictWriter, row, newValues []interface{}, oldRowid, newRowid int64, resolve string) (*Result, bool) {
	// oldRowid is read BEFORE applying (Next() runs after this body); the
	// cursor's current position defines the row.
	applied, keepPrior, uerr := w.UpdateRowWithRowid(row, oldRowid, newValues, newRowid, resolve)
	if uerr != nil {
		r := &Result{Error: uerr}
		if keepPrior {
			r.SetKeepPriorRowsOnError()
		}
		return r, false
	}
	return nil, applied
}

// execVTabInsert runs an INSERT ... VALUES against an updatable virtual
// table (dbpage's truncate-on-NULL INSERT semantics live in the module's
// InsertRow). handled is false when the target is not a virtual table.
func (e *DMLExecutor) execVTabInsert(s *sql.InsertStmt) (*Result, bool) {
	if len(s.Values) == 0 && s.Select == nil {
		return e.execVTabInsertDefault(s)
	}
	if s.Select != nil {
		return e.execVTabInsertSelect(s)
	}
	return e.execVTabInsertValues(s)
}

// execVTabInsertDefault runs INSERT ... DEFAULT VALUES: one row of all NULLs
// through xUpdate (zipfile.test 15.x: INSERT INTO t1 DEFAULT VALUES).
func (e *DMLExecutor) execVTabInsertDefault(s *sql.InsertStmt) (*Result, bool) {
	vt, colDefs, res, handled := e.resolveVTabUpdater(s.Table)
	if !handled || res != nil {
		return res, handled
	}
	updater := vt.(vtab.RowUpdater)
	nulls := make([]interface{}, len(colDefs))
	for i := range nulls {
		nulls[i] = nil
	}
	if _, err := e.insertVTabRow(updater, nulls, s.OrConflict); err != nil {
		return &Result{Error: err}, true
	}
	return &Result{Changes: 1}, true
}

// execVTabInsertSelect runs INSERT INTO <vtab> SELECT ...: materialize the
// source rows and feed each through xUpdate (zipfile.test 13.10 REPLACE INTO
// t1 SELECT * FROM t0).
func (e *DMLExecutor) execVTabInsertSelect(s *sql.InsertStmt) (*Result, bool) {
	if len(s.CTEs) > 0 {
		e.ctx.PushCTEScope(s.CTEs)
	}
	selectResult := e.ctx.ExecSelect(s.Select)
	if len(s.CTEs) > 0 {
		e.ctx.PopCTEScope()
	}
	if selectResult.Error != nil {
		return &Result{Error: selectResult.Error}, true
	}
	vt, colDefs, res, handled := e.resolveVTabUpdater(s.Table)
	if !handled || res != nil {
		return res, handled
	}
	updater := vt.(vtab.RowUpdater)
	rowidWriter, rowidCapable := vt.(RowidConflictWriter)
	resolve := vtabStatementResolve(s.OrConflict, s.IsReplace)
	rowidColIdx := vtabRowidColumnIndex(s.Columns, rowidCapable)
	var changes int64
	for _, row := range selectResult.Rows {
		values, explicitRowid := vtabSelectRowValues(s, row, colDefs, rowidColIdx)
		rowid, applied, res := e.vtabInsertSelectRow(rowidWriter, updater, values, explicitRowid, resolve, rowidColIdx >= 0, changes)
		if res != nil {
			return res, true
		}
		if applied {
			changes++
			_ = rowid
		}
	}
	return &Result{Changes: changes}, true
}

// vtabInsertSelectRow writes one INSERT ... SELECT source row through the
// rowid-aware or conflict-action path. A non-nil Result carries the error
// with the rows written so far (keepPrior marks the OR FAIL contract).
func (e *DMLExecutor) vtabInsertSelectRow(rowidWriter RowidConflictWriter, updater vtab.RowUpdater, values []interface{}, explicitRowid int64, resolve string, useRowid bool, changes int64) (int64, bool, *Result) {
	var id int64
	var applied, keepPrior bool
	var err error
	if useRowid {
		id, applied, keepPrior, err = insertVTabRowWithRowid(rowidWriter, values, explicitRowid, resolve)
	} else {
		id, applied, keepPrior, err = e.insertVTabConflictAction(resolve, updater, values)
	}
	if err != nil {
		r := &Result{Error: err, Changes: changes}
		if keepPrior {
			r.SetKeepPriorRowsOnError()
		}
		return 0, false, r
	}
	return id, applied, nil
}

// vtabRowidColumnIndex finds the explicit-rowid column in the INSERT column
// list (rowid writes require a RowidConflictWriter table), or -1.
func vtabRowidColumnIndex(columns []string, rowidCapable bool) int {
	for i, c := range columns {
		if strings.EqualFold(c, "rowid") && rowidCapable {
			return i
		}
	}
	return -1
}

// vtabSelectRowValues maps one INSERT ... SELECT source row onto the vtab's
// column slots (unlisted columns insert NULL); an explicit-rowid column's
// value is captured separately.
func vtabSelectRowValues(s *sql.InsertStmt, row []interface{}, colDefs []sql.ColumnDef, rowidColIdx int) ([]interface{}, int64) {
	values := make([]interface{}, len(colDefs))
	for i := range values {
		values[i] = nil
	}
	var explicitRowid int64
	for i, v := range row {
		colIdx, isRowid := vtabSelectColumnIndex(s.Columns, colDefs, len(values), i, rowidColIdx)
		if isRowid {
			if n, ok := util.UnwrapColumnValue(v).(int64); ok {
				explicitRowid = n
			}
			continue
		}
		if colIdx < 0 {
			continue
		}
		values[colIdx] = util.UnwrapColumnValue(v)
	}
	return values, explicitRowid
}

// vtabSelectColumnIndex resolves source column i's target slot: the rowid
// marker, the named column's slot, or positional i past the column list.
// (-1, true) marks the explicit-rowid slot; (-1, false) skips the value.
func vtabSelectColumnIndex(columns []string, colDefs []sql.ColumnDef, nValues, i, rowidColIdx int) (int, bool) {
	if i >= len(columns) {
		if i < nValues {
			return i, false
		}
		return -1, false
	}
	if i == rowidColIdx {
		return -1, true
	}
	return columnIndexByName(columns[i], colDefs), false
}

// columnIndexByName resolves a column name's slot in a vtab column list.
func columnIndexByName(name string, colDefs []sql.ColumnDef) int {
	for j, cd := range colDefs {
		if cd.Name == name {
			return j
		}
	}
	return -1
}

// execVTabInsertValues runs INSERT INTO <vtab> VALUES ...: each tuple is
// mapped onto the column slots and written through xUpdate.
func (e *DMLExecutor) execVTabInsertValues(s *sql.InsertStmt) (*Result, bool) {
	vt, colDefs, res, handled := e.resolveVTabUpdater(s.Table)
	if !handled {
		return nil, false
	}
	if res != nil {
		return res, true
	}
	updater := vt.(vtab.RowUpdater)
	empty := vtabRowMap(colDefs, nil, 0, false)
	resolve := vtabStatementResolve(s.OrConflict, s.IsReplace)
	rowidWriter, rowidCapable := updater.(RowidConflictWriter)
	var changes int64
	var lastID int64
	for _, tuple := range s.Values {
		rowid, applied, res := e.vtabInsertTuple(s, tuple, colDefs, empty, rowidCapable, rowidWriter, updater, resolve)
		if res != nil {
			return res, true
		}
		if applied {
			changes++
			lastID = rowid
		}
	}
	r := &Result{Changes: changes}
	r.LastInsertRowID = lastID
	e.ctx.SetLastRowID(lastID)
	return r, true
}

// vtabInsertTuple maps one VALUES tuple onto the column slots and writes it:
// the explicit-rowid form routes through the rowid-aware path, the default
// through the statement's conflict action.
func (e *DMLExecutor) vtabInsertTuple(s *sql.InsertStmt, tuple []sql.Expr, colDefs []sql.ColumnDef, empty execquery.RowMap, rowidCapable bool, rowidWriter RowidConflictWriter, updater vtab.RowUpdater, resolve string) (int64, bool, *Result) {
	values, hasRowid, rowidTupleIdx, res := e.vtabTupleValues(s, tuple, colDefs, empty, rowidCapable)
	if res != nil {
		return 0, false, res
	}
	if hasRowid {
		explicitRowid, rerr := e.vtabTupleRowid(tuple[rowidTupleIdx], empty)
		if rerr != nil {
			return 0, false, rerr
		}
		rowid, applied, keepPrior, err := insertVTabRowWithRowid(rowidWriter, values, explicitRowid, resolve)
		return vtabInsertOutcome(rowid, applied, keepPrior, err)
	}
	rowid, applied, keepPrior, err := e.insertVTabConflictAction(resolve, updater, values)
	return vtabInsertOutcome(rowid, applied, keepPrior, err)
}

// vtabInsertOutcome converts one write attempt's outcome: keepPrior marks
// the OR FAIL keep-prior-rows error contract.
func vtabInsertOutcome(rowid int64, applied, keepPrior bool, err error) (int64, bool, *Result) {
	if err != nil {
		r := &Result{Error: err}
		if keepPrior {
			r.SetKeepPriorRowsOnError()
		}
		return 0, false, r
	}
	return rowid, applied, nil
}

// vtabTupleValues maps one VALUES tuple onto the vtab's column slots
// (unlisted columns insert NULL). An unresolvable column name or an
// out-of-range slot is an error Result. (isRowid, rowidTupleIdx) marks the
// explicit-rowid entry, evaluated by the caller.
func (e *DMLExecutor) vtabTupleValues(s *sql.InsertStmt, tuple []sql.Expr, colDefs []sql.ColumnDef, empty execquery.RowMap, rowidCapable bool) (values []interface{}, hasRowid bool, rowidTupleIdx int, res *Result) {
	values = make([]interface{}, len(colDefs))
	for i := range values {
		values[i] = nil
	}
	for i, expr := range tuple {
		colIdx, isRowid, cres := e.vtabTupleColumnIndex(s, i, colDefs, rowidCapable)
		if cres != nil {
			return nil, false, -1, cres
		}
		if isRowid {
			hasRowid = true
			rowidTupleIdx = i
			continue
		}
		if colIdx < 0 {
			return nil, false, -1, &Result{Error: fmt.Errorf("table %s has no column named %s", s.Table, s.Columns[i])}
		}
		if colIdx >= len(values) {
			return nil, false, -1, &Result{Error: fmt.Errorf("table %s has %d values-supplying columns but column index %d is out of range",
				s.Table, len(values), colIdx)}
		}
		v, err := e.ctx.EvalExpr(expr, empty)
		if err != nil {
			return nil, false, -1, &Result{Error: err}
		}
		values[colIdx] = util.UnwrapColumnValue(v)
	}
	return values, hasRowid, rowidTupleIdx, nil
}

// vtabTupleColumnIndex resolves VALUES tuple element i's target slot.
// Unlisted elements map positionally; (isRowid) marks the explicit-rowid
// entry (spellfix1: INSERT INTO t(rowid, word) routes the rowid through
// xUpdate's argv[1] analog, rowid-capable tables only).
func (e *DMLExecutor) vtabTupleColumnIndex(s *sql.InsertStmt, i int, colDefs []sql.ColumnDef, rowidCapable bool) (colIdx int, isRowid bool, res *Result) {
	if i >= len(s.Columns) {
		return i, false, nil
	}
	if strings.EqualFold(s.Columns[i], "rowid") {
		if !rowidCapable {
			return -1, false, &Result{Error: fmt.Errorf("table %s has no column named %s", s.Table, s.Columns[i])}
		}
		return -1, true, nil
	}
	return columnIndexByName(s.Columns[i], colDefs), false, nil
}

// vtabTupleRowid evaluates an explicit-rowid VALUES entry.
func (e *DMLExecutor) vtabTupleRowid(expr sql.Expr, row execquery.RowMap) (int64, *Result) {
	v, err := e.ctx.EvalExpr(expr, row)
	if err != nil {
		return 0, &Result{Error: err}
	}
	if n, ok := util.UnwrapColumnValue(v).(int64); ok {
		return n, nil
	}
	return 0, nil
}

// execVTabDelete runs a DELETE whose target is an updatable virtual table:
// matching rows are materialized, filtered by WHERE and removed via
// RowUpdater.DeleteRow. handled is false when the target is not a vtab.
func (e *DMLExecutor) execVTabDelete(s *sql.DeleteStmt) (*Result, bool) {
	vt, colDefs, res, handled := e.resolveVTabUpdater(s.Table)
	if !handled {
		return nil, false
	}
	if res != nil {
		return res, true
	}
	cur, err := vt.Open()
	if err != nil {
		return &Result{Error: err}, true
	}
	defer cur.Close()
	doomed, res := e.collectVTabDeleteCandidates(s, cur, colDefs)
	if res != nil {
		return res, true
	}
	updater := vt.(vtab.RowUpdater)
	rowidWriter, rowidCapable := vt.(RowidConflictWriter)
	var changes int64
	for _, d := range doomed {
		var err error
		if rowidCapable {
			err = rowidWriter.DeleteRowWithRowid(d.oldValues, d.rowid)
		} else {
			err = updater.DeleteRow(d.oldValues)
		}
		if err != nil {
			return &Result{Error: err}, true
		}
		changes++
	}
	return &Result{Changes: changes}, true
}

// vtabDeleteCandidate is one row queued for deletion; rows are collected
// during the scan and deleted after it completes.
type vtabDeleteCandidate struct {
	oldValues []interface{}
	rowid     int64
}

// collectVTabDeleteCandidates scans the cursor for rows passing the DELETE's
// WHERE filter.
func (e *DMLExecutor) collectVTabDeleteCandidates(s *sql.DeleteStmt, cur vtab.Cursor, colDefs []sql.ColumnDef) ([]vtabDeleteCandidate, *Result) {
	var doomed []vtabDeleteCandidate
	for cur.Next() {
		row := vtabCursorRow(cur, len(colDefs))
		var rowid int64
		hasRowid := false
		if rc, ok := cur.(vtab.RowidCursor); ok {
			rowid = rc.Rowid()
			hasRowid = true
		}
		rm := vtabRowMap(colDefs, row, rowid, hasRowid)
		pass, err := e.ctx.RowPassesWhere(s.Where, rm, nil)
		if err != nil {
			return nil, &Result{Error: err}
		}
		if !pass {
			continue
		}
		doomed = append(doomed, vtabDeleteCandidate{oldValues: row, rowid: rowid})
	}
	return doomed, nil
}

// ConflictAwareInserter is implemented by virtual tables whose xUpdate
// distinguishes statement-level OR conflict actions (zipfile.c: REPLACE
// overwrites a same-name entry, IGNORE skips it silently, ABORT keeps
// rows inserted by earlier tuples of the same statement — non-transactional
// vtabs are not rolled back).
type ConflictAwareInserter interface {
	InsertRowConflict(values []interface{}, resolve string) (int64, error)
}

// ConflictAwareUpdater lets virtual tables implement SQLite's xUpdate
// conflict policy directly when their uniqueness key is not a rowid.
type ConflictAwareUpdater interface {
	UpdateRowConflict(oldValues, newValues []interface{}, resolve string) error
}

// RowidConflictWriter is implemented by virtual tables whose rows are keyed
// by rowid alone and whose xUpdate contract receives the rowid explicitly
// (spellfix1: INSERT INTO t(rowid, word), UPDATE t SET rowid=N, DELETE FROM
// t where the shadow id is the rowid). resolve is the statement-level OR
// action ("" when none); the implementation owns the conflict policy.
//
// UpdateRowWithRowid returns applied=false when the row was skipped under
// the statement's resolution; keepPrior=true with a non-nil error marks the
// OR FAIL keep-prior-rows error contract.
type RowidConflictWriter interface {
	InsertRowWithRowid(values []interface{}, rowid int64, resolve string) (int64, error)
	UpdateRowWithRowid(oldValues []interface{}, oldRowid int64, newValues []interface{}, newRowid int64, resolve string) (applied bool, keepPrior bool, err error)
	DeleteRowWithRowid(oldValues []interface{}, rowid int64) error
}

// insertVTabRow routes one INSERT row through the conflict-aware path when
// the module supports it and an OR action is present.
func (e *DMLExecutor) insertVTabRow(updater vtab.RowUpdater, values []interface{}, orConflict string) (int64, error) {
	if orConflict != "" {
		if ci, ok := updater.(ConflictAwareInserter); ok {
			return ci.InsertRowConflict(values, orConflict)
		}
	}
	return updater.InsertRow(values)
}

func (e *DMLExecutor) insertVTabConflictAction(resolve string, updater vtab.RowUpdater, values []interface{}) (int64, bool, bool, error) {
	if ci, ok := updater.(ConflictAwareInserter); ok && resolve != "" {
		id, err := ci.InsertRowConflict(values, resolve)
		if err == nil {
			return id, true, false, nil
		}
		if strings.EqualFold(resolve, "IGNORE") {
			return 0, false, false, nil
		}
		return 0, false, strings.EqualFold(resolve, "FAIL"), err
	}
	return e.applyVTabConflictAction(resolve, updater, values, func() (int64, error) {
		return updater.InsertRow(values)
	})
}

// applyVTabConflictAction runs one row write (insert or update, supplied as
// fn) honoring the statement-level OR <action> resolution against a plain
// vtab.RowUpdater whose only failure vocabulary is
// vtab.UniqueConstraintError (rtree family). Semantics mirror sqlite3 xUpdate
// handling in rtree.c:
//
//	IGNORE  – conflicting row skipped silently (applied=false)
//	REPLACE – existing row deleted, write retried once
//	FAIL    – error returned with keepPrior=true (rows written so far survive;
//	          the engine's statement rollback honors the flag)
//	other   – error propagates (default/ABORT undo the statement via the
//	          pager-snapshot restore; ROLLBACK adds a transaction rollback at
//	          the engine layer)
func (e *DMLExecutor) applyVTabConflictAction(resolve string, updater vtab.RowUpdater, values []interface{}, fn func() (int64, error)) (rowid int64, applied, keepPrior bool, err error) {
	resolve = strings.ToUpper(resolve)
	rowid, err = fn()
	if err == nil {
		return rowid, true, false, nil
	}
	uce, isUnique := vtab.AsUniqueConstraintError(err)
	if !isUnique {
		// OR IGNORE also drops rows violating the r-tree geometry predicate
		// ("rtree constraint failed") — any SQLITE_CONSTRAINT-class xUpdate
		// rejection is a skippable conflict for this resolution.
		if resolve == "IGNORE" && isRtreeConstraintFailure(err) {
			return 0, false, false, nil
		}
		return 0, false, false, err
	}
	switch resolve {
	case "REPLACE":
		// REPLACE INTO also reaches here via s.IsReplace.
		if derr := updater.DeleteRow([]interface{}{uce.RowID}); derr != nil {
			return 0, false, false, derr
		}
		rowid, err = fn()
		if err != nil {
			return 0, false, false, err
		}
		return rowid, true, false, nil
	case "IGNORE":
		return 0, false, false, nil
	case "FAIL":
		return 0, false, true, err
	default:
		return 0, false, false, err
	}
}

// vtabStatementResolve maps an INSERT/UPDATE statement's clause to the OR
// action string ("", "IGNORE", ...).
func vtabStatementResolve(orConflict string, isReplace bool) string {
	if orConflict == "" && isReplace {
		return "REPLACE"
	}
	return orConflict
}

// isRtreeConstraintFailure reports the rtree.c geometry rejection text.
func isRtreeConstraintFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "rtree constraint failed:")
}

// insertVTabRowWithRowid routes one explicit-rowid INSERT row through the
// rowid-aware conflict path (spellfix1 xUpdate argv[0]=NULL, argv[1]=rowid).
func insertVTabRowWithRowid(w RowidConflictWriter, values []interface{}, rowid int64, resolve string) (int64, bool, bool, error) {
	id, err := w.InsertRowWithRowid(values, rowid, resolve)
	if err != nil {
		if strings.EqualFold(resolve, "IGNORE") && isVtabConstraintSkipped(err) {
			return 0, false, false, nil
		}
		return 0, false, strings.EqualFold(resolve, "FAIL"), err
	}
	return id, true, false, nil
}

// isVtabConstraintSkipped reports whether err is the generic vtab constraint
// rejection that OR IGNORE may silently drop (spellfix shadow "constraint
// failed").
func isVtabConstraintSkipped(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

// applyVTabUpdateAction runs one UPDATE row through the OR-conflict
// resolution (see applyVTabConflictAction): skipped=true means the
// conflicting row was dropped under OR IGNORE; a non-nil res carries the
// statement error (with keep-prior-rows set under OR FAIL).
func applyVTabUpdateAction(s *sql.UpdateStmt, oldValues, newValues []interface{}, updater vtab.RowUpdater) (bool, *Result) {
	resolve := strings.ToUpper(vtabStatementResolve(s.OnConflict, false))
	if cu, ok := updater.(ConflictAwareUpdater); ok && resolve != "" {
		if err := cu.UpdateRowConflict(oldValues, newValues, resolve); err != nil {
			return false, &Result{Error: err}
		}
		return false, nil
	}
	return applyPlainVTabUpdateAction(resolve, updater, oldValues, newValues)
}

// applyPlainVTabUpdateAction is the conflict resolution for a plain
// vtab.RowUpdater (rtree family semantics, see applyVTabConflictAction).
func applyPlainVTabUpdateAction(resolve string, updater vtab.RowUpdater, oldValues, newValues []interface{}) (bool, *Result) {
	err := updater.UpdateRow(oldValues, newValues)
	if err == nil {
		return false, nil
	}
	uce, isUnique := vtab.AsUniqueConstraintError(err)
	if !isUnique {
		if resolve == "IGNORE" && isRtreeConstraintFailure(err) {
			return true, nil
		}
		return false, &Result{Error: err}
	}
	switch resolve {
	case "REPLACE":
		if derr := updater.DeleteRow([]interface{}{uce.RowID}); derr != nil {
			return false, &Result{Error: derr}
		}
		if err2 := updater.UpdateRow(oldValues, newValues); err2 != nil {
			return false, &Result{Error: err2}
		}
		return false, nil
	case "IGNORE":
		return true, nil
	default:
		r := &Result{Error: err}
		if resolve == "FAIL" {
			r.SetKeepPriorRowsOnError()
		}
		return false, r
	}
}
