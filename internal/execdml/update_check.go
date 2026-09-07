// Package exec implements query execution.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// --- UPDATE constraint checks ---
func (e *DMLExecutor) checkUpdateConflicts(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := uniqueColsForTable(colDefs)
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	wrOrder := e.ctx.WRStorageOrder(tableEntry.SQL, colDefs)
	if len(uniqueCols) == 0 && len(idxColsList) == 0 && len(wrOrder) == 0 {
		return &Result{}
	}

	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	for i := range changes {
		c := changes[i]
		if res := e.checkEarlierChanges(changes, i, c, colDefs, colIndex, uniqueCols, idxColsList, tableEntry.Name); res.Error != nil {
			return res
		}
		if res := e.checkLiveTableConflictsWR(tree, changes[:i], c, colDefs, colIndex, uniqueCols, idxColsList, tableEntry, wrOrder); res.Error != nil {
			return res
		}
	}
	return &Result{}
}

// checkEarlierChanges checks one change's NEW values against the NEW values
// of every change processed before it (j < i): those rows have already been
// written, so their current values are their NEW values.
func (e *DMLExecutor) checkEarlierChanges(changes []updateChange, i int, c updateChange, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, tableName string) *Result {
	for j := 0; j < i; j++ {
		if e.valuesConflict(changes[j].values, c.values, changes[j].rowID, c.rowID, colDefs, colIndex, uniqueCols, idxColsList) {
			return &Result{Error: e.uniqueConflictError(tableName, colDefs, colIndex, changes[j].values, c.values, changes[j].rowID, c.rowID, uniqueCols, idxColsList)}
		}
	}
	return &Result{}
}

// checkLiveTableConflicts scans the table for rows whose ORIGINAL values
// conflict with a change's NEW values, excluding the row being updated and
// rows already processed (j < i), whose current NEW values were checked
// pairwise by checkEarlierChanges.
// checkLiveTableConflicts scans the table for rows whose ORIGINAL values
// conflict with a change's NEW values, excluding the row being updated and
// rows already processed (j < i), whose current NEW values were checked
// pairwise by checkEarlierChanges.
func (e *DMLExecutor) checkLiveTableConflicts(tree *btree.BTree, earlier []updateChange, c updateChange, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, tableName string) *Result {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return &Result{Error: err}
	}
	return e.checkLiveTableConflictsWR(tree, earlier, c, colDefs, colIndex, uniqueCols, idxColsList, entry, e.ctx.WRStorageOrder(entry.SQL, colDefs))
}

func (e *DMLExecutor) checkLiveTableConflictsWR(tree *btree.BTree, earlier []updateChange, c updateChange, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, tableEntry *schema.Entry, wrOrder []int) *Result {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}
	var wrSkip []wrOldKey
	if len(wrOrder) > 0 {
		wrSkip = wrSkipKeys(append(append([]updateChange{}, earlier...), c), tableEntry.SQL, colDefs)
	}
	skip := updateSkipRowIDs(earlier, c.rowID)
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		if res := e.checkCellConflictWR(cell, c, skip, wrSkip, colDefs, colIndex, uniqueCols, idxColsList, tableEntry, wrOrder); res != nil {
			return res
		}
		if cursorExhausted(cursor) {
			break
		}
	}
	return &Result{}
}

// wrOldKey is a declared-order PK projection identifying one WITHOUT ROWID
// row independently of its synthetic RowID 0.
type wrOldKey struct {
	vals []interface{}
}

// wrSkipKeys snapshots the OLD PK projections of the given changes.
func wrSkipKeys(changes []updateChange, createSQL string, colDefs []sql.ColumnDef) []wrOldKey {
	idx := wrPKIndices(createSQL, colDefs)
	if len(idx) == 0 {
		return nil
	}
	out := make([]wrOldKey, 0, len(changes))
	for _, ch := range changes {
		key := make([]interface{}, len(idx))
		for k, ci := range idx {
			if ci < len(ch.oldValues) {
				key[k] = ch.oldValues[ci]
			}
		}
		out = append(out, wrOldKey{vals: key})
	}
	return out
}

// wrKeyMatchesCell reports whether a storage-order cell payload projects to
// one of the OLD PK keys (declared order, per-column collation).
func wrKeyMatchesCell(cell *storage.Cell, keys []wrOldKey, createSQL string, colDefs []sql.ColumnDef) bool {
	if len(keys) == 0 {
		return false
	}
	order := withoutRowidStorageOrder(createSQL, colDefs)
	idx := wrPKIndices(createSQL, colDefs)
	if len(order) != len(colDefs) || len(idx) == 0 {
		return false
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return false
	}
	decl := reorderToDeclared(rec.Values, order)
	for _, key := range keys {
		if len(key.vals) != len(idx) {
			continue
		}
		match := true
		for k, ci := range idx {
			var have interface{}
			if ci < len(decl) {
				have = decl[ci]
			}
			if !wrValuesEqual(have, key.vals[k], colDefs[ci]) {
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

// checkCellConflictWR is checkCellConflict with WITHOUT ROWID awareness:
// PK-first records are remapped to declared order, and rows in wrSkip
// (matched by OLD PK key) are ignored instead of every RowID-0 row.
func (e *DMLExecutor) checkCellConflictWR(cell *storage.Cell, c updateChange, skip map[int64]bool, wrSkip []wrOldKey, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, tableEntry *schema.Entry, wrOrder []int) *Result {
	if len(wrOrder) > 0 {
		if wrKeyMatchesCell(cell, wrSkip, tableEntry.SQL, colDefs) {
			return nil
		}
	} else if skip[cell.RowID] {
		return nil
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return &Result{}
	}
	e.ctx.RemapWRRecordToDeclared(rec, tableEntry.SQL, colDefs)
	if e.valuesConflict(rec.Values, c.values, cell.RowID, c.rowID, colDefs, colIndex, uniqueCols, idxColsList) {
		return &Result{Error: e.uniqueConflictError(tableEntry.Name, colDefs, colIndex, rec.Values, c.values, cell.RowID, c.rowID, uniqueCols, idxColsList)}
	}
	return nil
}

// updateSkipRowIDs returns the set of rowIDs the conflict scan must skip: the
// row being updated plus every row already processed by an earlier change.
func updateSkipRowIDs(earlier []updateChange, rowID int64) map[int64]bool {
	skip := make(map[int64]bool, len(earlier)+1)
	skip[rowID] = true
	for _, ch := range earlier {
		skip[ch.rowID] = true
	}
	return skip
}

// checkCellConflict reports a conflict for one table cell, or nil to continue
// scanning. Rows in the skip set are ignored. An empty result stops the scan
// (a record that could not be decoded is treated as no conflict).
func (e *DMLExecutor) checkCellConflict(cell *storage.Cell, c updateChange, skip map[int64]bool, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, tableName string) *Result {
	if skip[cell.RowID] {
		return nil
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return &Result{}
	}
	// A later change (j > i) still holds its ORIGINAL values (not yet
	// written), which the table scan sees.
	if e.valuesConflict(rec.Values, c.values, cell.RowID, c.rowID, colDefs, colIndex, uniqueCols, idxColsList) {
		return &Result{Error: e.uniqueConflictError(tableName, colDefs, colIndex, rec.Values, c.values, cell.RowID, c.rowID, uniqueCols, idxColsList)}
	}
	return nil
}

// checkUpdateConstraints validates NOT NULL and CHECK constraints on the new
// values of an UPDATE. SQLite checks these per-row during UPDATE (before
// applying); a violation aborts the whole statement with no partial writes.
// UNIQUE/PRIMARY KEY conflicts are handled separately by checkUpdateConflicts.
func (e *DMLExecutor) checkUpdateConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if !hasNotNullOrCheckConstraint(colDefs) && len(e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL)) == 0 {
		return &Result{}
	}
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	var pkCols map[int]bool
	if withoutRowid {
		pkCols = e.primaryKeyColIndices(tableEntry.Name, tableEntry.SQL, colDefs)
	}
	// Set the DML table context so table-qualified column references inside
	// CHECK expressions (e.g. CHECK(Table0.Col0 NOT NULL)) resolve against
	// the row's unqualified column keys.
	prevDML := e.currentDMLTable
	e.currentDMLTable = tableEntry.Name
	defer func() { e.currentDMLTable = prevDML }()
	for _, ch := range changes {
		row := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
		if res := e.checkRowUpdateConstraints(ch.values, row, tableEntry, colDefs, withoutRowid, pkCols); res.Error != nil {
			return res
		}
	}
	return &Result{}
}

// hasNotNullOrCheckConstraint reports whether any column declares a NOT NULL
// or CHECK constraint.
func hasNotNullOrCheckConstraint(colDefs []sql.ColumnDef) bool {
	for _, cd := range colDefs {
		if cd.NotNull || cd.Check != nil {
			return true
		}
	}
	return false
}

// checkRowUpdateConstraints validates NOT NULL and CHECK constraints (column
// and table-level) for one updated row's NEW values.
// checkRowUpdateConstraints validates NOT NULL and CHECK constraints (column
// and table-level) for one updated row's NEW values.
func (e *DMLExecutor) checkRowUpdateConstraints(values []interface{}, row RowMap, tableEntry *schema.Entry, colDefs []sql.ColumnDef, withoutRowid bool, pkCols map[int]bool) *Result {
	for _, cd := range colDefs {
		if res := e.checkUpdateColumn(values, row, tableEntry, colDefs, withoutRowid, pkCols, cd); res != nil {
			return res
		}
	}
	return e.checkTableUpdateChecks(row, tableEntry)
}

// checkUpdateColumn validates one column's NOT NULL and CHECK constraints for
// an updated row's NEW value.
func (e *DMLExecutor) checkUpdateColumn(values []interface{}, row RowMap, tableEntry *schema.Entry, colDefs []sql.ColumnDef, withoutRowid bool, pkCols map[int]bool, cd sql.ColumnDef) *Result {
	val := columnValue(values, colDefs, cd.Name)
	// NOT NULL constraint — skip for INTEGER PRIMARY KEY columns of rowid
	// tables (their value derives from the rowid, which is unchanged by an
	// UPDATE).
	pkAutoRowID := isIPKRowidAliasCol(cd) && !withoutRowid
	implicitNotNull := cd.NotNull || (withoutRowid && pkCols[cdIndex(colDefs, cd.Name)])
	if implicitNotNull && val == nil && !pkAutoRowID {
		return &Result{Error: fmt.Errorf("NOT NULL constraint failed: %s.%s", tableEntry.Name, e.originalColumnName(tableEntry.SQL, cd.Name))}
	}
	// CHECK constraint: only fails when the result is explicitly false.
	// PRAGMA ignore_check_constraints=ON skips enforcement.
	if cd.Check != nil && !e.ctx.IgnoreCheckConstraints() {
		checkVal, err := e.ctx.EvalExpr(cd.Check, row)
		if err == nil && checkVal != nil && !execexpr.ToBool(checkVal) {
			checkText := e.checkConstraintText(tableEntry.SQL, cd.Name, cd.Check)
			return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", checkText)}
		}
	}
	return nil
}

// checkTableUpdateChecks validates table-level CHECK constraints for an
// updated row's NEW values.
func (e *DMLExecutor) checkTableUpdateChecks(row RowMap, tableEntry *schema.Entry) *Result {
	tcs := e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL)
	for ti, tc := range tcs {
		if tc.Type != sql.ConstraintCheck || tc.Expr == nil {
			continue
		}
		if e.ctx.IgnoreCheckConstraints() {
			continue
		}
		checkVal, err := e.ctx.EvalExpr(tc.Expr, row)
		if err == nil && checkVal != nil && !execexpr.ToBool(checkVal) {
			name := tc.Name
			if name == "" {
				name = e.tableCheckConstraintText(tableEntry.SQL, ti, tcs)
			}
			return &Result{Error: fmt.Errorf("CHECK constraint failed: %s", name)}
		}
	}
	return &Result{}
}
