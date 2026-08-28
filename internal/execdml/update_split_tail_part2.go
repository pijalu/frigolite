// Package exec implements query execution.
package execdml

import (
	"fmt"
	"math"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *DMLExecutor) updateFromTree(entry *schema.Entry, fromCtx *DatabaseContext) *btree.BTree {
	if fromCtx != nil && fromCtx.Pager != nil {
		return e.ctx.TableBTreePg(fromCtx.Pager, entry.Name, entry.RootPage, true)
	}
	return e.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
}

// qualifyUpdateRow adds qualified keys (alias.col) for a FROM table row so
// SET/WHERE can reference them by table name. It snapshots the original keys
// first: iterating the live map while inserting would cascade (adding
// alias.alias.col for already-qualified keys), which wastes memory and pollutes
// the row map.
func qualifyUpdateRow(fm RowMap, alias string) {
	keys := make([]string, 0, len(fm))
	for k := range fm {
		keys = append(keys, k)
	}
	for _, k := range keys {
		// The rowid alias must be qualified too (ft.rowid) so WHERE/SET can
		// reference the target row's rowid explicitly (fts4upfrom 1.9: WITH
		// x1(o,n) ... UPDATE ft SET docid=n FROM x1 WHERE ft.rowid = o).
		if k == "rowid" {
			fm[alias+".rowid"] = fm[k]
			continue
		}
		fm[alias+"."+k] = fm[k]
	}
}

// crossJoinUpdateRows cross-products an accumulator of row maps with the rows
// of every FROM table. An empty FROM table yields no combined rows.
func crossJoinUpdateRows(acc []RowMap, tables [][]RowMap) []RowMap {
	result := acc
	for _, tblRows := range tables {
		if len(tblRows) == 0 {
			return nil
		}
		var next []RowMap
		for _, base := range result {
			for _, fr := range tblRows {
				next = append(next, mergeRowMaps(base, fr))
			}
		}
		result = next
	}
	return result
}

// mergeRowMaps combines two row maps; keys present in both keep the base
// (target-row) value.
func mergeRowMaps(base, fr RowMap) RowMap {
	merged := make(RowMap, len(base)+len(fr))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range fr {
		if _, ok := merged[k]; !ok {
			merged[k] = v
		}
	}
	return merged
}

func (e *DMLExecutor) buildUpdateChange(cell *storage.Cell, rec *storage.Record, colIndex map[string]int, colDefs []sql.ColumnDef, s *sql.UpdateStmt, row Row, deferSetEval bool) (*updateChange, error) {
	values, oldValues := updateChangeValueSlots(rec, colIndex)
	ch := &updateChange{rowID: cell.RowID, oldValues: oldValues}
	if deferSetEval {
		// Defer SET evaluation to the apply loop (per-row interleaving). The
		// row map is retained so the SET expressions can be re-evaluated
		// against the original row values.
		rm, ok := row.(RowMap)
		if ok {
			ch.rowMap = rm
		}
		ch.values = values
		return ch, nil
	}

	// Detect an explicit rowid assignment (SET rowid = N / _rowid_ / oid):
	// the row is deleted at the old rowid and re-inserted at the new one.
	var newRowID *int64

	for _, a := range s.Assignments {
		colIdx := -1
		if ci, ok := colIndex[strings.ToLower(a.Column)]; ok {
			colIdx = ci
		}
		isIPKAssign := colIdx >= 0 && colIdx < len(colDefs) && isIPKRowidAliasCol(colDefs[colIdx])
		if isIPKAssign {
			// SET <ipk-column> (an INTEGER PRIMARY KEY rowid alias, e.g. b in
			// t11(a, b INTEGER PRIMARY KEY)): validate the value like any IPK
			// assignment (NULL/REAL '4.1'/TEXT 'hello'/BLOB → "datatype
			// mismatch", e_createtable-5.9.2), set the column value, and move
			// the row to the new rowid (e_createtable-5.7.2.4: UPDATE t11 SET
			// b = 8 moves rowid to 8).
			nrid, err := e.applyIPKRowidAssignment(a, row, colIndex, colDefs, values)
			if err != nil {
				return nil, err
			}
			if nrid != nil {
				newRowID = nrid
			}
			continue
		}
		if execquery.IsRowIDName(a.Column) && !execquery.RowHasRowIDColumn(colDefs) {
			// SET rowid = N changes the cell's rowid, not a column value (only
			// when the table has no column named rowid/oid/_rowid_; a declared
			// rowid column is a normal column assignment).
			rid, err := e.evalRowIDAssignment(a, row, colDefs, values)
			if err != nil {
				return nil, err
			}
			if rid != nil {
				newRowID = rid
			}
			continue
		}
		if err := e.applyUpdateColumnSet(a, row, colIndex, colDefs, values); err != nil {
			return nil, err
		}
	}
	// Recompute generated columns (b AS(expr)) after the SET assignments
	// change base columns, matching SQLite (UPDATE recomputes generated
	// columns from the new base values). Clear them first so the recompute
	// always runs (they hold the pre-update values).
	if err := e.recomputeUpdateGenerated(colDefs, values); err != nil {
		return nil, err
	}
	return &updateChange{rowID: cell.RowID, newRowID: newRowID, values: values, oldValues: oldValues}, nil
}

// materializeChangeValues evaluates the SET expressions for a change whose
// evaluation was deferred (rowMap set, values still the original record slots).
// This runs per-row in the trigger apply loop so user functions and the
// changes() counter observe SQLite's row-by-row interleaving (update.c
// evaluates SET for row N after row N-1's AFTER trigger).
func (e *DMLExecutor) materializeChangeValues(ch *updateChange, s *sql.UpdateStmt, colIndex map[string]int, colDefs []sql.ColumnDef) error {
	if ch.rowMap == nil || len(s.Assignments) == 0 {
		return nil
	}
	row := RowMap(ch.rowMap)
	var newRowID *int64
	values := ch.values
	for _, a := range s.Assignments {
		colIdx := -1
		if ci, ok := colIndex[strings.ToLower(a.Column)]; ok {
			colIdx = ci
		}
		isIPKAssign := colIdx >= 0 && colIdx < len(colDefs) && isIPKRowidAliasCol(colDefs[colIdx])
		if isIPKAssign {
			nrid, err := e.applyIPKRowidAssignment(a, row, colIndex, colDefs, values)
			if err != nil {
				return err
			}
			if nrid != nil {
				newRowID = nrid
			}
			continue
		}
		if execquery.IsRowIDName(a.Column) && !execquery.RowHasRowIDColumn(colDefs) {
			rid, err := e.evalRowIDAssignment(a, row, colDefs, values)
			if err != nil {
				return err
			}
			if rid != nil {
				newRowID = rid
			}
			continue
		}
		if err := e.applyUpdateColumnSet(a, row, colIndex, colDefs, values); err != nil {
			return err
		}
	}
	if err := e.recomputeUpdateGenerated(colDefs, values); err != nil {
		return err
	}
	ch.newRowID = newRowID
	ch.values = values
	ch.rowMap = nil // values now materialized
	return nil
}

// updateChangeValueSlots allocates the values array for an update change,
// large enough to hold all columns (not just those present in the record),
// plus a copy of the original record values.
func updateChangeValueSlots(rec *storage.Record, colIndex map[string]int) ([]interface{}, []interface{}) {
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
	return values, oldValues
}

// evalRowIDAssignment evaluates a SET rowid/_rowid_/oid assignment, returning
// the new rowid (nil when the value is NULL, meaning no rowid change).
func (e *DMLExecutor) evalRowIDAssignment(a sql.Assignment, row Row, colDefs []sql.ColumnDef, values []interface{}) (*int64, error) {
	v, err := e.ctx.EvalExpr(a.Value, row)
	if err != nil {
		return nil, fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)
	}
	nv := util.UnwrapColumnValue(v)
	if nv == nil {
		return nil, nil
	}
	n, ok := execexpr.ToInt64(nv)
	if !ok {
		return nil, fmt.Errorf("datatype mismatch")
	}
	// An INTEGER PRIMARY KEY column is the rowid: changing the rowid changes
	// the IPK column value too (and fires FK parent actions on it).
	for i, cd := range colDefs {
		if isIPKRowidAliasCol(cd) && i < len(values) {
			values[i] = n
		}
	}
	return &n, nil
}

// applyIPKRowidAssignment applies a SET clause on an INTEGER PRIMARY KEY
// rowid-alias column: validates the value like any IPK assignment (NULL / non-
// integer REAL / TEXT / BLOB → "datatype mismatch"), stores the column value,
// and returns the new rowid (nil when the value is NULL).
func (e *DMLExecutor) applyIPKRowidAssignment(a sql.Assignment, row Row, colIndex map[string]int, colDefs []sql.ColumnDef, values []interface{}) (*int64, error) {
	if err := e.applyUpdateColumnSet(a, row, colIndex, colDefs, values); err != nil {
		return nil, err
	}
	av, aerr := e.ctx.EvalExpr(a.Value, row)
	if aerr != nil {
		return nil, nil
	}
	nv := util.UnwrapColumnValue(av)
	if nv == nil {
		return nil, nil
	}
	n, ok := execexpr.ToInt64(nv)
	if !ok {
		return nil, nil
	}
	return &n, nil
}

// applyUpdateColumnSet evaluates and stores one normal SET assignment (a
// non-rowid column), applying the column's type affinity.
func (e *DMLExecutor) applyUpdateColumnSet(a sql.Assignment, row Row, colIndex map[string]int, colDefs []sql.ColumnDef, values []interface{}) error {
	idx, ok := colIndex[strings.ToLower(a.Column)]
	if !ok {
		// Column not in schema - this happens when SQLite tests dynamically
		// add columns via PRAGMA writable_schema. Extend values array.
		idx = len(values)
		values = append(values, nil)
		colIndex[strings.ToLower(a.Column)] = idx
	}
	v, err := e.ctx.EvalExpr(a.Value, row)
	if err != nil {
		return fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)
	}
	// Unwrap ColumnValue to avoid storing internal wrapper types
	// in the record — only raw values should be serialized.
	v = util.UnwrapColumnValue(v)
	// An INTEGER PRIMARY KEY column (rowid alias) accepts only integer
	// values; assigning a non-integer (REAL 4.1, TEXT 'hello', BLOB, NULL)
	// raises "datatype mismatch" (SQLite, e_createtable-5.9.2). An
	// integer-valued REAL or numeric string ('-15.0') is accepted.
	if idx >= 0 && idx < len(colDefs) && isIPKRowidAliasCol(colDefs[idx]) {
		if !e.ipkAssignValueOK(v) {
			return fmt.Errorf("datatype mismatch")
		}
	}
	// Apply the column's type affinity (e.g. a REAL column stores 1 as 1.0).
	if idx >= 0 && idx < len(values) {
		if idx < len(colDefs) {
			v = util.ApplyColumnAffinity(v, colDefs[idx].Type)
		}
		values[idx] = v
	}
	return nil
}

// ipkAssignValueOK reports whether v can be assigned to an INTEGER PRIMARY
// KEY (rowid alias) column: an integer, an integer-valued real (4.0), or a
// numeric string that converts to an integer ('-15.0', '12').
func (e *DMLExecutor) ipkAssignValueOK(v interface{}) bool {
	switch n := util.ApplyColumnAffinity(v, "NUMERIC").(type) {
	case int64:
		return true
	case float64:
		return n == math.Trunc(n) && !math.IsInf(n, 0) && !math.IsNaN(n)
	case string:
		// A non-numeric string under NUMERIC affinity stays a string.
		return false
	case []byte:
		return false
	default:
		return false
	}
}

// recomputeUpdateGenerated clears and recomputes generated columns after the
// SET assignments change base columns.
func (e *DMLExecutor) recomputeUpdateGenerated(colDefs []sql.ColumnDef, values []interface{}) error {
	for gi, gcd := range colDefs {
		if gcd.Generated != nil && gi < len(values) {
			values[gi] = nil
		}
	}
	return e.computeGeneratedValues(colDefs, values)
}
