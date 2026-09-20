// This file holds the REPLACE-conflict machinery of INSERT (OR REPLACE):
// conflict collection over unique/PK constraints, the secondary-conflict
// scan (strict/IGNORE column classes), the loop that deletes each
// conflicting row (findNextReplaceConflict), and WITHOUT ROWID
// probe-covering helpers.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *DMLExecutor) collectReplaceConflicts(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, replaceRowID int64) ([]conflictRow, bool) {
	seen := make(map[string]bool)
	keyer := newConflictKeyer(tableEntry, colDefs)
	var conflicts []conflictRow
	for {
		foundID, foundVals, found := e.findNextReplaceConflict(pg, tableEntry, colDefs, colIndex, values, replaceRowID, seen, keyer)
		if !found {
			break
		}
		seen[keyer.key(foundID, foundVals)] = true
		conflicts = append(conflicts, conflictRow{rowID: foundID, values: foundVals})
	}
	return conflicts, len(conflicts) > 0
}

// replaceSecondaryConflictResult inspects the unique constraints OTHER than
// the one whose REPLACE-resolved error triggered the replace for conflicts
// with the new row. SQLite deletes the REPLACE-conflicting rows and re-runs
// the insert; a conflict on a constraint with a non-REPLACE algorithm then
// fails the re-run and the statement journal undoes the deletes (insert.c).
// This pre-check yields the same net effect before any delete happens:
//   - a conflicting constraint with ON CONFLICT IGNORE skips the row
//     silently (no deletes, no error);
//   - a conflicting constraint with FAIL/ABORT/ROLLBACK, no clause, or a
//     UNIQUE index (always clause-less) reports "UNIQUE constraint failed"
//     and nothing is deleted (tkt-4a03edc4c8: IPK REPLACE + b UNIQUE FAIL
//     leaves both original rows in place and errors on t1.b).
//
// Only the per-constraint path uses this: a statement-level OR REPLACE
// overrides the column clauses (verified against sqlite3) and keeps the
// delete-everything behavior. skip=true tells the caller to drop the row.
func (e *DMLExecutor) replaceSecondaryConflictResult(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, replacedCol string) (*Result, bool) {
	if res := e.replaceSecondaryIndexConflict(tableEntry, colDefs, colIndex, values); res != nil {
		return res, false
	}
	strictCols, ignoreCols := e.classifyReplaceSecondaryCols(colDefs, values, replacedCol)
	if len(strictCols) == 0 && len(ignoreCols) == 0 {
		return nil, false
	}
	return e.scanReplaceSecondaryConflict(tableEntry, colDefs, values, strictCols, ignoreCols)
}

// replaceSecondaryIndexConflict reports a UNIQUE index conflict for the new
// values. UNIQUE indexes never carry a conflict clause: any conflict on one
// fails the insert (CREATE UNIQUE INDEX has no ON CONFLICT grammar).
func (e *DMLExecutor) replaceSecondaryIndexConflict(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) *Result {
	for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
		if _, _, ok := e.findRowByIndexCols(tableEntry, colDefs, values, def); ok {
			return &Result{Error: uniqueIndexConflictError(tableEntry, colIndex, def, def.Cols)}
		}
	}
	return nil
}

// classifyReplaceSecondaryCols partitions the column-level UNIQUE/PRIMARY KEY
// constraints other than the replaced one by their conflict resolution:
// strict (FAIL/ABORT/ROLLBACK/no clause → error) vs IGNORE (skip the row
// silently). REPLACE columns are resolved by the delete pass.
func (e *DMLExecutor) classifyReplaceSecondaryCols(colDefs []sql.ColumnDef, values []interface{}, replacedCol string) (strictCols, ignoreCols map[int]bool) {
	strictCols = make(map[int]bool)
	ignoreCols = make(map[int]bool)
	for i := range colDefs {
		if i >= len(values) || values[i] == nil {
			continue
		}
		if strings.EqualFold(colDefs[i].Name, replacedCol) {
			continue // the replaced constraint itself
		}
		if !colDefs[i].Unique && !colDefs[i].PrimaryKey {
			continue
		}
		switch colDefs[i].OnConflict {
		case "REPLACE":
			continue // resolved by the delete pass
		case "IGNORE":
			ignoreCols[i] = true
		default:
			strictCols[i] = true
		}
	}
	return strictCols, ignoreCols
}

// scanReplaceSecondaryConflict walks the table once looking for a row that
// conflicts with the new values on a tracked secondary constraint: strict
// columns produce the UNIQUE error before any delete happens; IGNORE columns
// skip the row silently (skip=true).
func (e *DMLExecutor) scanReplaceSecondaryConflict(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, strictCols, ignoreCols map[int]bool) (*Result, bool) {
	keyer := newConflictKeyer(tableEntry, colDefs)
	tree := e.uniqueScanTree(tableEntry.Name, tableEntry.RootPage)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, false
	}
	for {
		cell, cerr := cursor.ReadCell()
		if cerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		// WITHOUT ROWID cells are PK-first storage order; the scan compares
		// declared positions, so remap first (as findNextReplaceConflict does).
		if keyer.wr {
			e.ctx.RemapWRRecordToDeclared(rec, tableEntry.SQL, colDefs)
		}
		if res, skip := replaceSecondaryRowConflict(tableEntry, rec.Values, values, colDefs, strictCols, ignoreCols); res != nil || skip {
			return res, skip
		}
		if okN, nerr := cursor.Next(); nerr != nil || !okN {
			break
		}
	}
	return nil, false
}

// replaceSecondaryRowConflict tests one existing row against the tracked
// secondary constraint columns: strict → UNIQUE error result; IGNORE → skip.
func replaceSecondaryRowConflict(tableEntry *schema.Entry, rowVals, values []interface{}, colDefs []sql.ColumnDef, strictCols, ignoreCols map[int]bool) (*Result, bool) {
	for i := range strictCols {
		if hasConflictAt(rowVals, []int{i}, values, colDefs) >= 0 {
			return &Result{Error: fmt.Errorf("UNIQUE constraint failed: %s.%s", tableEntry.Name, colDefs[i].Name)}, false
		}
	}
	for i := range ignoreCols {
		if hasConflictAt(rowVals, []int{i}, values, colDefs) >= 0 {
			return nil, true
		}
	}
	return nil, false
}

// findNextReplaceConflict locates one not-yet-seen row conflicting with the
// new values, checking the explicit rowid, UNIQUE columns, then UNIQUE indexes.
// UNIQUE columns are checked PER-COLUMN: INSERT OR REPLACE INTO t(a UNIQUE,
// b UNIQUE) VALUES('one','two') must delete BOTH the row with a='one' and the
// row with b='two' (each unique column independently).
func (e *DMLExecutor) findNextReplaceConflict(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, replaceRowID int64, seen map[string]bool, keyer conflictKeyer) (int64, []interface{}, bool) {
	// An explicit rowid (rowid/oid/_rowid_ in the INSERT list) conflicts
	// with the existing row at that rowid (SQLite OP_Delete on the rowid).
	if rid, rv, ok := e.replaceConflictAtRowID(pg, tableEntry, replaceRowID, seen); ok {
		return rid, rv, true
	}
	// UNIQUE indexes (CREATE UNIQUE INDEX ... ON t(c1, c2)): SQLite resolves
	// these conflicts first in a REPLACE (a row matched by a UNIQUE index is
	// deleted before a composite-PK conflict; hook2.test 2.1.5 expects the
	// index-conflict row's DELETE preupdate before the PK-conflict row's).
	for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
		if rid, rv, ok := e.findRowByIndexCols(tableEntry, colDefs, values, def); ok && !seen[keyer.key(rid, rv)] {
			return rid, rv, true
		}
	}
	// Scan the table once, collecting the first conflicting row for each
	// UNIQUE/PK column. scanAllUniqueConflicts uses the DML target's context
	// pager (dmlTableBTree); the explicit-pager variant below reuses the
	// same scan tree so an ATTACHed table (currentDMLCtx pager) is scanned.
	uniqueCols := collectUniqueColsWithPK(colDefs, colIndex, values)
	if !keyer.wr {
		// The rowid probe above already answers the IPK-alias column for a
		// rowid table: rec.Values[ipk] == values[ipk] ⟺ the row's rowid ==
		// replaceRowID (the alias column IS the rowid, and replaceRowID was
		// derived from the same INSERT value — pkRowIDOrZero /
		// replaceRowIDAndDelete). Keeping the column would full-scan the
		// table once per inserted row: quadratic INSERT..SELECT into
		// rowid-keyed shadow tables (rtree %_rowid/%_node).
		uniqueCols = dropIPKProbeCoveredCol(uniqueCols, colDefs, values, replaceRowID)
	}
	if len(uniqueCols) > 0 {
		if rid, rv, ok := e.findReplaceColumnConflict(tableEntry, colDefs, values, uniqueCols, seen, keyer); ok {
			return rid, rv, true
		}
	}
	// Composite PRIMARY KEY / UNIQUE groups (e.g. PRIMARY KEY(b,c)): scan
	// for a row where ALL group columns match the new values (statement
	// REPLACE must delete it; per-column scans miss composite keys).
	for _, group := range e.compositeUniqueGroups(tableEntry.Name, tableEntry.SQL, colDefs) {
		if cell, rec, err := e.scanTableForMatch(tableEntry, func(rec *storage.Record, cell *storage.Cell) bool {
			return !seen[keyer.key(cell.RowID, rec.Values)] && e.allMatch(colDefs, rec.Values, group, values)
		}); err == nil && cell != nil {
			return cell.RowID, rec.Values, true
		}
	}
	return 0, nil, false
}

// findReplaceColumnConflict scans the table once for the first row whose
// value on any not-yet-seen UNIQUE/PK column matches the new values.
// scanAllUniqueConflicts uses the DML target's context pager
// (dmlTableBTree); this explicit-tree variant reuses the same scan tree so
// an ATTACHed table (currentDMLCtx pager) is scanned.
func (e *DMLExecutor) findReplaceColumnConflict(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, uniqueCols []int, seen map[string]bool, keyer conflictKeyer) (int64, []interface{}, bool) {
	tree := e.uniqueScanTree(tableEntry.Name, tableEntry.RootPage)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, nil, false
	}
	for {
		cell, rec, ok := e.replaceScanCell(tableEntry, colDefs, cursor, keyer.wr)
		if !ok {
			return 0, nil, false
		}
		if replaceCellColumnConflict(cell, rec, values, uniqueCols, seen, keyer) {
			return cell.RowID, rec.Values, true
		}
		hasNext, nerr := cursor.Next()
		if nerr != nil || !hasNext {
			return 0, nil, false
		}
	}
}

// replaceScanCell reads and decodes the cursor's next cell. WITHOUT ROWID
// cells are PK-first storage order; the scan compares declared positions, so
// remap first. Rowid tables skip the call: the remap is a no-op whose DDL
// sniff would otherwise re-parse the CREATE per cell.
func (e *DMLExecutor) replaceScanCell(tableEntry *schema.Entry, colDefs []sql.ColumnDef, cursor *btree.Cursor, wr bool) (*storage.Cell, *storage.Record, bool) {
	cell, cerr := cursor.ReadCell()
	if cerr != nil || cell == nil {
		return nil, nil, false
	}
	rec, derr := storage.DecodeRecord(cell.Payload)
	if derr != nil || rec == nil {
		return nil, nil, false
	}
	if wr {
		e.ctx.RemapWRRecordToDeclared(rec, tableEntry.SQL, colDefs)
	}
	return cell, rec, true
}

// replaceCellColumnConflict reports whether the cell's value matches the new
// values on any not-yet-seen UNIQUE column (UNIQUE columns are checked
// PER-COLUMN: INSERT OR REPLACE INTO t(a UNIQUE, b UNIQUE) VALUES('one',
// 'two') must delete BOTH the row with a='one' and the row with b='two').
func replaceCellColumnConflict(cell *storage.Cell, rec *storage.Record, values []interface{}, uniqueCols []int, seen map[string]bool, keyer conflictKeyer) bool {
	for _, idx := range uniqueCols {
		if seen[keyer.key(cell.RowID, rec.Values)] {
			continue
		}
		if idx >= len(rec.Values) || idx >= len(values) {
			continue
		}
		if rec.Values[idx] == nil || values[idx] == nil {
			continue
		}
		if util.CompareValues(rec.Values[idx], values[idx]) == 0 {
			return true
		}
	}
	return false
}

// conflictKeyer precomputes a REPLACE pass's WITHOUT-ROWID classification
// and PK index projection once, so the per-cell seen-key cost is O(pk)
// instead of a DDL re-parse per scanned cell (quadratic for INSERT..SELECT
// into rowid shadows — rtree's %_rowid/%_node tables).
type conflictKeyer struct {
	wr bool
	pk []int
}

func newConflictKeyer(tableEntry *schema.Entry, colDefs []sql.ColumnDef) conflictKeyer {
	wr := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	var pk []int
	if wr {
		pk = WRPKIndices(tableEntry.SQL, colDefs)
	}
	return conflictKeyer{wr: wr, pk: pk}
}

// key is conflictSeenKey with the DDL classification precomputed.
func (k conflictKeyer) key(rowID int64, vals []interface{}) string {
	if !k.wr {
		return fmt.Sprintf("r%d", rowID)
	}
	var b strings.Builder
	for _, ci := range k.pk {
		var v interface{}
		if ci < len(vals) {
			v = vals[ci]
		}
		fmt.Fprintf(&b, "%v\x00", v)
	}
	return b.String()
}

// conflictSeenKey identifies a conflict row across the REPLACE find passes:
// the rowid for ordinary tables; the declared PK values for WITHOUT ROWID
// tables, whose cells all share the synthetic RowID 0. Cold-path wrapper —
// per-cell callers must hoist newConflictKeyer instead.
func conflictSeenKey(tableEntry *schema.Entry, colDefs []sql.ColumnDef, rowID int64, vals []interface{}) string {
	return newConflictKeyer(tableEntry, colDefs).key(rowID, vals)
}

// replaceConflictAtRowID returns the row at replaceRowID when it is a not-yet-
// seen conflict for a REPLACE insert.

// replaceConflictAtRowID returns the row at replaceRowID when it is a not-yet-
// seen conflict for a REPLACE insert.

// replaceConflictAtRowID returns the row at replaceRowID when it is a not-yet-
// seen conflict for a REPLACE insert.
// replaceConflictAtRowID returns the row at replaceRowID when it is a not-yet-
// seen conflict for a REPLACE insert.
func (e *DMLExecutor) replaceConflictAtRowID(pg *pager.Pager, tableEntry *schema.Entry, replaceRowID int64, seen map[string]bool) (int64, []interface{}, bool) {
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return 0, nil, false
	}
	tree := e.ctx.TableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, true)
	cell, cerr := e.ctx.ReadCellByRowID(tree, replaceRowID)
	if cerr != nil || cell == nil {
		return 0, nil, false
	}
	rec, derr := storage.DecodeRecord(cell.Payload)
	if derr != nil || rec == nil {
		return 0, nil, false
	}
	if seen[conflictSeenKey(tableEntry, nil, cell.RowID, rec.Values)] {
		return 0, nil, false
	}
	return cell.RowID, rec.Values, true
}

// deleteReplaceConflictRow fires BEFORE/AFTER DELETE triggers, deletes the
// row, and applies foreign-key actions for one REPLACE conflict.

// deleteReplaceConflictRow fires BEFORE/AFTER DELETE triggers, deletes the
// row, and applies foreign-key actions for one REPLACE conflict.

// deleteReplaceConflictRow fires BEFORE/AFTER DELETE triggers, deletes the
// row, and applies foreign-key actions for one REPLACE conflict.
// deleteReplaceConflictRow fires BEFORE/AFTER DELETE triggers, deletes the
// row, and applies foreign-key actions for one REPLACE conflict.
func (e *DMLExecutor) deleteReplaceConflictRow(tree *btree.BTree, tableEntry *schema.Entry, colDefs []sql.ColumnDef, conflictRowID int64, conflictValues []interface{}, hasTriggers bool) *Result {
	// Read the row for trigger OLD values.
	oldRow := buildRowMapFromValues(conflictValues, colDefs, conflictRowID)
	if hasTriggers {
		if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
			// RAISE(IGNORE) in a BEFORE DELETE trigger skips this row's delete.
			if trigResult.Error == errRaiseIgnore {
				return nil
			}
			return trigResult
		}
	}
	// WITHOUT ROWID rows are PK-keyed index cells sharing synthetic RowID 0:
	// match the conflicting row's OLD PK instead of the rowid.
	if _, err := e.deleteRowCells(tableEntry, colDefs, conflictRowID, conflictValues); err != nil {
		return &Result{Error: err}
	}
	// Remove the conflicting row's index entries (REPLACE deletes the old
	// row; its index entries must go with it).
	if err := e.maintainIndexesOnDelete(tableEntry, colDefs, []RowMap{oldRow}); err != nil {
		return &Result{Error: err}
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	// Fire the preupdate hook for the deleted conflicting row (REPLACE
	// deletes the old row, then the INSERT fires for the new one).
	delRowID := conflictRowID
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		delRowID = 0
	}
	if res := e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "DELETE",
		DB:    e.schemaNameForPager(e.dmlPager(tableEntry.Name)),
		Table: tableEntry.Name,
		RowID: delRowID, RowID2: delRowID,
		RowidTable:   !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)),
		NoUpdateHook: true,
		Old:          conflictValues,
		New:          nil,
	}); res != nil {
		return res
	}
	if hasTriggers {
		if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	// Foreign key actions for the deleted conflicting row: CASCADE children
	// are deleted, SET NULL / SET DEFAULT children update their FK column.
	// NO ACTION / RESTRICT are deferred to after the new row is written
	// (SQLite: the REPLACE may re-insert the same key).
	if e.ctx.ForeignKeys() {
		if fkResult := e.ctx.FkParentDeleteReplace(tableEntry, colDefs, oldRow); fkResult.Error != nil {
			return fkResult
		}
	}
	return nil
}

// buildRowMapFromValues creates a column-name-to-value map from a values slice.

// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.

// buildRowMapFromValues creates a column-name-to-value map from a values slice.
// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.

// validateOnConflictTarget validates every ON CONFLICT clause in the chain
// (statement order) against the table's PRIMARY KEY / UNIQUE constraints and
// indexes. The target must name existing columns whose set exactly matches a
// UNIQUE index or PK/UNIQUE constraint (order-insensitive, matching SQLite's
// upsert target analysis), or be an expression whose text exactly matches an
// expression index key. COLLATE and partial-index WHERE predicates must also
// match. Missing columns raise "no such column"; a target that matches no
// constraint raises the SQLite "does not match" error.
