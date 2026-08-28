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
		oldRowid := int64(0)
		if ridCur != nil {
			oldRowid = ridCur.Rowid()
		}
		row := make([]interface{}, len(colDefs))
		for i := range row {
			v, err := cur.Column(i)
			if err != nil {
				break
			}
			row[i] = v
		}
		rm := vtabRowMap(colDefs, row, oldRowid, ridCur != nil)
		pass, err := e.ctx.RowPassesWhere(s.Where, rm, nil)
		if err != nil {
			return &Result{Error: err}, true
		}
		if !pass {
			continue
		}
		newValues := append([]interface{}(nil), row...)
		newRowid := oldRowid
		for _, a := range s.Assignments {
			if strings.EqualFold(a.Column, "rowid") && rowidCapable {
				v, err := e.ctx.EvalExpr(a.Value, rm)
				if err != nil {
					return &Result{Error: err}, true
				}
				if n, ok := util.UnwrapColumnValue(v).(int64); ok {
					newRowid = n
				}
				continue
			}
			idx := -1
			for i, cd := range colDefs {
				if cd.Name == a.Column {
					idx = i
					break
				}
			}
			if idx < 0 {
				return &Result{Error: fmt.Errorf("no such column: %s", a.Column)}, true
			}
			v, err := e.ctx.EvalExpr(a.Value, rm)
			if err != nil {
				return &Result{Error: err}, true
			}
			uv := util.UnwrapColumnValue(v)
			if uv == nil {
				// Preserve "explicitly assigned NULL" semantics for vtabs
				// (zipfile: SET data=NULL makes the entry a directory).
				uv = vtab.ExplicitNull{}
			}
			newValues[idx] = uv
		}
		updater := vt.(vtab.RowUpdater)
		if rowidCapable {
			// Advance oldRowid BEFORE applying (Next() runs after this
			// body); the cursor's current position defines the row.
			applied, keepPrior, uerr := rowidWriter.UpdateRowWithRowid(row, oldRowid, newValues, newRowid, vtabStatementResolve(s.OnConflict, false))
			if uerr != nil {
				r := &Result{Error: uerr}
				if keepPrior {
					r.SetKeepPriorRowsOnError()
				}
				return r, true
			}
			if !applied {
				continue
			}
			changes++
			continue
		}
		skipped, res := applyVTabUpdateAction(s, row, newValues, updater)
		if res != nil {
			return res, true
		}
		if skipped {
			continue
		}
		changes++
	}
	return &Result{Changes: changes}, true
}

// execVTabInsert runs an INSERT ... VALUES against an updatable virtual
// table (dbpage's truncate-on-NULL INSERT semantics live in the module's
// InsertRow). handled is false when the target is not a virtual table.
func (e *DMLExecutor) execVTabInsert(s *sql.InsertStmt) (*Result, bool) {
	if len(s.Values) == 0 && s.Select == nil {
		// INSERT ... DEFAULT VALUES: one row of all NULLs through xUpdate
		// (zipfile.test 15.x: INSERT INTO t1 DEFAULT VALUES).
		vt0, colDefs0, res0, handled0 := e.resolveVTabUpdater(s.Table)
		if !handled0 || res0 != nil {
			return res0, handled0
		}
		updater0 := vt0.(vtab.RowUpdater)
		nulls := make([]interface{}, len(colDefs0))
		for i := range nulls {
			nulls[i] = nil
		}
		if _, err := e.insertVTabRow(updater0, nulls, s.OrConflict); err != nil {
			return &Result{Error: err}, true
		}
		return &Result{Changes: 1}, true
	}
	if s.Select != nil {
		// INSERT INTO <vtab> SELECT ... : materialize the source rows and
		// feed each through xUpdate (zipfile.test 13.10 REPLACE INTO t1
		// SELECT * FROM t0).
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
		vt2, colDefs2, res2, handled2 := e.resolveVTabUpdater(s.Table)
		if !handled2 || res2 != nil {
			return res2, handled2
		}
		updater2 := vt2.(vtab.RowUpdater)
		rowidWriter2, rowidCapable2 := vt2.(RowidConflictWriter)
		resolve2 := vtabStatementResolve(s.OrConflict, s.IsReplace)
		rowidColIdx := -1
		for i, c := range s.Columns {
			if strings.EqualFold(c, "rowid") && rowidCapable2 {
				rowidColIdx = i
				break
			}
		}
		var changes int64
		for _, row := range selectResult.Rows {
			values := make([]interface{}, len(colDefs2))
			for i := range values {
				values[i] = nil
			}
			var explicitRowid int64
			for i, v := range row {
				colIdx := -1
				if i < len(s.Columns) {
					if i == rowidColIdx {
						if n, ok := util.UnwrapColumnValue(v).(int64); ok {
							explicitRowid = n
						}
						continue
					}
					for j, cd := range colDefs2 {
						if cd.Name == s.Columns[i] {
							colIdx = j
							break
						}
					}
				} else if i < len(values) {
					colIdx = i
				}
				if colIdx < 0 {
					continue
				}
				values[colIdx] = util.UnwrapColumnValue(v)
			}
			var applied, keepPrior bool
			var err error
			var id int64
			if rowidColIdx >= 0 {
				id, applied, keepPrior, err = insertVTabRowWithRowid(rowidWriter2, values, explicitRowid, resolve2)
			} else {
				id, applied, keepPrior, err = e.insertVTabConflictAction(resolve2, updater2, values)
			}
			if err != nil {
				r := &Result{Error: err, Changes: changes}
				if keepPrior {
					r.SetKeepPriorRowsOnError()
				}
				return r, true
			}
			if applied {
				changes++
				_ = id
			}
		}
		return &Result{Changes: changes}, true
	}
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
		values := make([]interface{}, len(colDefs))
		for i := range values {
			values[i] = nil
		}
		explicitRowid := int64(0)
		hasRowid := false
		rowidTupleIdx := -1
		for i, expr := range tuple {
			colIdx := i
			if i < len(s.Columns) {
				colIdx = -1
				if strings.EqualFold(s.Columns[i], "rowid") {
					// spellfix1: INSERT INTO t(rowid, word) routes the
					// rowid through xUpdate's argv[1] analog.
					if rowidCapable {
						hasRowid = true
						rowidTupleIdx = i
						continue
					}
					return &Result{Error: fmt.Errorf("table %s has no column named %s", s.Table, s.Columns[i])}, true
				}
				for j, cd := range colDefs {
					if cd.Name == s.Columns[i] {
						colIdx = j
						break
					}
				}
			}
			if colIdx < 0 {
				return &Result{Error: fmt.Errorf("table %s has no column named %s", s.Table, s.Columns[i])}, true
			}
			if colIdx >= len(values) {
				return &Result{Error: fmt.Errorf("table %s has %d values-supplying columns but column index %d is out of range",
					s.Table, len(values), colIdx)}, true
			}
			v, err := e.ctx.EvalExpr(expr, empty)
			if err != nil {
				return &Result{Error: err}, true
			}
			values[colIdx] = util.UnwrapColumnValue(v)
		}
		var rowid int64
		var applied, keepPrior bool
		var err error
		switch {
		case hasRowid:
			v, verr := e.ctx.EvalExpr(tuple[rowidTupleIdx], empty)
			if verr != nil {
				return &Result{Error: verr}, true
			}
			if n, ok := util.UnwrapColumnValue(v).(int64); ok {
				explicitRowid = n
			}
			rowid, applied, keepPrior, err = insertVTabRowWithRowid(rowidWriter, values, explicitRowid, resolve)
		default:
			rowid, applied, keepPrior, err = e.insertVTabConflictAction(resolve, updater, values)
		}
		if err != nil {
			r := &Result{Error: err}
			if keepPrior {
				r.SetKeepPriorRowsOnError()
			}
			return r, true
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
	var changes int64
	type pending struct {
		oldValues []interface{}
		rowid     int64
	}
	var doomed []pending
	for cur.Next() {
		row := make([]interface{}, len(colDefs))
		for i := range row {
			v, err := cur.Column(i)
			if err != nil {
				break
			}
			row[i] = v
		}
		var rowid int64
		hasRowid := false
		if rc, ok := cur.(vtab.RowidCursor); ok {
			rowid = rc.Rowid()
			hasRowid = true
		}
		rm := vtabRowMap(colDefs, row, rowid, hasRowid)
		pass, err := e.ctx.RowPassesWhere(s.Where, rm, nil)
		if err != nil {
			return &Result{Error: err}, true
		}
		if !pass {
			continue
		}
		doomed = append(doomed, pending{oldValues: row, rowid: rowid})
	}
	updater := vt.(vtab.RowUpdater)
	rowidWriter, rowidCapable := vt.(RowidConflictWriter)
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
		err := cu.UpdateRowConflict(oldValues, newValues, resolve)
		if err == nil {
			return false, nil
		}
		return false, &Result{Error: err}
	}
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
