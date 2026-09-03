// Package exec implements query execution.
package execdml

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- UPDATE ---

type updateChange struct {
	rowID     int64
	newRowID  *int64 // non-nil when the UPDATE sets rowid/_rowid_/oid to a new value
	values    []interface{}
	oldValues []interface{}
	// rowMap is the original row (as a RowMap) used to re-evaluate the SET
	// expressions per-row. It is only populated when SET evaluation is
	// deferred (the trigger-per-row path), so the changes() counter and user
	// functions observe SQLite's row-by-row interleaving (e_changes 5.1.2).
	rowMap RowMap
}

func buildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	colIndex := make(map[string]int)
	for i, cd := range colDefs {
		// SQLite column names are case-insensitive: SET Test=... and
		// SET test=... must resolve to the same column. Key the index by
		// the lowercased name (rowvalue 27.10's UPDATE items SET
		// test='ok' where the column is declared Test).
		colIndex[strings.ToLower(cd.Name)] = i
	}
	// rowid/_rowid_/oid map to the pseudo-rowid unless the table declares a
	// column with one of those names (which shadows the alias).
	if !execquery.RowHasRowIDColumn(colDefs) {
		colIndex["rowid"] = -1
	}
	return colIndex
}

// cdIndex returns the column index for name, or -1 if not found.
func cdIndex(colDefs []sql.ColumnDef, name string) int {
	for i, cd := range colDefs {
		if strings.EqualFold(cd.Name, name) {
			return i
		}
	}
	return -1
}

// originalColumnName returns the column name as spelled in the CREATE TABLE
// SQL (preserving original case for error messages), or the uppercased name
// if it cannot be found.
func (e *DMLExecutor) originalColumnName(createSQL, colName string) string {
	start := strings.IndexByte(createSQL, '(')
	end := strings.LastIndexByte(createSQL, ')')
	if start < 0 || end <= start {
		return colName
	}
	body := createSQL[start+1 : end]
	for _, part := range splitColumnDefs(body) {
		fields := strings.FieldsFunc(part, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r'
		})
		if len(fields) == 0 {
			continue
		}
		first := strings.Trim(fields[0], "`\"[]")
		if strings.EqualFold(first, colName) {
			return first
		}
	}
	return colName
}

// splitColumnDefs splits a CREATE TABLE column list on top-level commas
// (ignoring commas inside parentheses such as CHECK(...) or DEFAULT(...)).
func splitColumnDefs(body string) []string {
	var parts []string
	depth := 0
	last := 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, body[last:])
	return parts
}

// primaryKeyColIndices returns the set of column indices that are PRIMARY KEY
// columns: column-level PRIMARY KEY declarations plus table-level PRIMARY KEY
// constraints (honoring integer column positions).
func (e *DMLExecutor) primaryKeyColIndices(tableName, createSQL string, colDefs []sql.ColumnDef) map[int]bool {
	idx := make(map[int]bool)
	colIndex := buildColumnIndex(colDefs)
	for i, cd := range colDefs {
		if cd.PrimaryKey {
			idx[i] = true
		}
	}
	for _, tc := range e.ctx.TableConstraints(tableName, createSQL) {
		if tc.Type != sql.ConstraintPrimaryKey {
			continue
		}
		for _, ic := range tc.Columns {
			if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
				idx[n-1] = true
				continue
			}
			if i, ok := colIndex[strings.ToLower(ic.Name)]; ok {
				idx[i] = true
			}
		}
	}
	return idx
}

// sortUpdateChanges sorts updateChange entries by the ORDER BY expressions
// evaluated against each row's original values.
func (e *DMLExecutor) sortUpdateChanges(changes []updateChange, rowMaps []RowMap, orderBy []sql.OrderByTerm) {
	if len(changes) <= 1 {
		return
	}
	type pair struct {
		ch  updateChange
		row RowMap
	}
	pairs := make([]pair, len(changes))
	for i := range changes {
		pairs[i] = pair{ch: changes[i], row: rowMaps[i]}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		for _, ob := range orderBy {
			left, _ := e.ctx.EvalExpr(ob.Expr, pairs[i].row)
			right, _ := e.ctx.EvalExpr(ob.Expr, pairs[j].row)
			cmp := e.ctx.CompareOrderByValues(left, right, ob)
			if cmp < 0 {
				return true
			} else if cmp > 0 {
				return false
			}
		}
		return false
	})
	for i := range pairs {
		changes[i] = pairs[i].ch
	}
}

// limitUpdateChanges applies UPDATE ... LIMIT n [OFFSET m] to the change list,
// keeping the first n entries after skipping m (SQLite semantics for UPDATE
// LIMIT: the first N rows matched by the scan order are updated).
func (e *DMLExecutor) limitUpdateChanges(changes []updateChange, s *sql.UpdateStmt) []updateChange {
	limit, err := e.evalConstInt(s.Limit)
	if err != nil || limit < 0 {
		return changes
	}
	offset := int64(0)
	if s.Offset != nil {
		if v, err := e.evalConstInt(s.Offset); err == nil && v > 0 {
			offset = v
		}
	}
	if offset >= int64(len(changes)) {
		return nil
	}
	end := offset + limit
	if end > int64(len(changes)) {
		end = int64(len(changes))
	}
	return changes[offset:end]
}

// evalConstInt evaluates an expression that must be a constant integer,
// returning -1 on error or non-integer values.
func (e *DMLExecutor) evalConstInt(expr sql.Expr) (int64, error) {
	v, err := e.ctx.EvalExpr(expr, nil)
	if err != nil {
		return -1, err
	}
	v = util.UnwrapColumnValue(v)
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		// SQLite accepts integer-valued floats (LIMIT 1.0 == LIMIT 1) but
		// rejects non-integral floats (LIMIT 1.2 → datatype mismatch).
		if n == math.Trunc(n) {
			return int64(n), nil
		}
	case string:
		// SQLite casts the LIMIT expression to integer: LIMIT '4' == LIMIT 4,
		// LIMIT '1.0' == LIMIT 1, LIMIT '1.2' and LIMIT 'abc' → datatype
		// mismatch. Only integral-valued strings are accepted.
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil && f == math.Trunc(f) {
			return int64(f), nil
		}
	}
	return -1, fmt.Errorf("datatype mismatch")
}

// dedupeUpdateChanges removes duplicate-rowID entries from an update
// change list, keeping the LAST entry per rowid (sequential-overwrite
// semantics). A change scan over a table holding a physical duplicate
// rowid (corrupt or legacy file) reports the row twice; re-inserting it
// twice would recreate the duplicate.
func dedupeUpdateChanges(changes []updateChange) []updateChange {
	if len(changes) < 2 {
		return changes
	}
	last := make(map[int64]int, len(changes))
	for i, c := range changes {
		last[c.rowID] = i
	}
	out := make([]updateChange, 0, len(last))
	seen := make(map[int64]bool, len(last))
	for i, c := range changes {
		if last[c.rowID] == i && !seen[c.rowID] {
			out = append(out, c)
			seen[c.rowID] = true
		}
	}
	return out
}

func (e *DMLExecutor) rowMatchesWhere(where sql.Expr, row Row) (bool, error) {
	if where == nil {
		return true, nil
	}
	match, err := e.ctx.EvalBool(where, row)
	if err != nil {
		return false, err
	}
	return match, nil
}

func (e *DMLExecutor) applyUpdateChanges(tableName string, rootPage uint32, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	// A pre-existing physical duplicate rowid (corrupt/legacy file) makes the
	// scan collect the row twice; SQLite updates rows one at a time in place,
	// so write each distinct rowid exactly once — last write wins.
	changes = dedupeUpdateChanges(changes)

	// Build a set of rowIDs to update
	toUpdate := make(map[int64]bool, len(changes))
	for _, c := range changes {
		toUpdate[c.rowID] = true
	}

	tree := e.dmlTableBTree(tableName, rootPage)

	// Step 1: Delete all existing rows in a single pass
	_, delErr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return toUpdate[cell.RowID]
	})
	if delErr != nil {
		return &Result{Error: delErr}
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableName), rootPage)

	// Step 2: Insert all new rows, firing the preupdate hook per row.
	for _, c := range changes {
		if err := e.writeUpdatedCell(tableName, tree, rootPage, c); err != nil {
			return &Result{Error: err}
		}
		if res := e.fireUpdatePreupdate(tableName, c); res != nil {
			return res
		}
	}

	// The re-insert loop above bumps the rowid cache with each re-inserted
	// row's rowid. When the UPDATE touched only a subset of rows (e.g. an FTS
	// segment truncate), that leaves the cached max LOWER than the table's
	// real maximum (a concurrent insert's higher rowid is forgotten), so the
	// next auto-rowid insert COLLIDES and overwrites the missing row
	// (fts4merge 5.7: the L2 output insert at rowid 1067 is clobbered by the
	// next level-0 segment because the truncate's re-insert bumped the cache
	// only to 1066). SQLite recomputes the rowid counter after any
	// DELETE/UPDATE, so drop the cache again and let the next allocation
	// rescan the true max.
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableName), rootPage)

	return &Result{Changes: int64(len(changes))}
}

// writeUpdatedCell re-inserts one updated row (encode, insert at the
// possibly re-keyed rowid, bump the rowid cache).
func (e *DMLExecutor) writeUpdatedCell(tableName string, tree *btree.BTree, rootPage uint32, c updateChange) error {
	newRecord, err := storage.EncodeRecord(c.values)
	if err != nil {
		return err
	}
	writeRowID := c.rowID
	if c.newRowID != nil {
		writeRowID = *c.newRowID
	}
	newCell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   writeRowID,
		Payload: newRecord,
	}
	if err := tree.InsertCell(newCell); err != nil {
		return err
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableName), rootPage, writeRowID)
	return nil
}

// fireUpdatePreupdate fires the preupdate hook with the old and new row
// values for one updated row.
func (e *DMLExecutor) fireUpdatePreupdate(tableName string, c updateChange) *Result {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return nil
	}
	rowidTable := !hasWithoutRowidKeyword(strings.ToUpper(entry.SQL))
	rowID := c.rowID
	if !rowidTable {
		rowID = 0
	}
	return e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "UPDATE",
		DB:    e.schemaNameForPager(e.dmlPager(tableName)),
		Table: tableName,
		RowID: rowID, RowID2: rowID,
		RowidTable: rowidTable,
		Old:        append([]interface{}(nil), c.oldValues...),
		New:        append([]interface{}(nil), c.values...),
	})
}

// applyUpdateWithTriggers processes a plain UPDATE with triggers, matching
// SQLite's ordering: BEFORE UPDATE triggers fire per-row before the row is
// written, and a BEFORE trigger may delete the row being updated — SQLite then
// skips writing rows that no longer exist and checks UNIQUE/PK constraints
// against the live table state. AFTER UPDATE triggers fire phase-based after
// the writes (matching the engine's existing behavior for non-trigger rows).
// tableBTreeForDML builds the btree for a table being modified, using the
// modified table's context pager when known (a table in an ATTACHed database
// lives on the attached pager even when a same-named table exists in main).
func (e *DMLExecutor) tableBTreeForDML(tableEntry *schema.Entry, rootPage uint32) *btree.BTree {
	return e.dmlTableBTree(tableEntry.Name, rootPage)
}

// rowExists reports whether a table contains a cell with the given rowID.
func (e *DMLExecutor) rowExists(tableName string, rootPage uint32, rowID int64) (bool, error) {
	// The table may share its short name with a table in another schema
	// (main.t1 vs aux.t1); use the modified table's context pager when known
	// AND the name carries a schema prefix (an unqualified name resolves
	// temp/main-first, which the fallback handles correctly).
	var pg *pager.Pager
	if e.currentDMLCtx != nil && e.currentDMLCtx.Pager != nil && e.currentDMLCtx != e.ctx.MainDB() {
		pg = e.currentDMLCtx.Pager
	} else {
		pg = e.dmlPager(tableName)
	}
	tree := e.ctx.TableBTreePg(pg, tableName, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false, err
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			return false, nil
		}
		if cell.RowID == rowID {
			return true, nil
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return false, nil
		}
	}
}

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
func (e *DMLExecutor) collectUpdateConflicts(tree *btree.BTree, c updateChange, uniqueCols []int, idxColsList []uniqueIndexDef, colDefs []sql.ColumnDef, colIndex map[string]int, conflicts []conflictInfo) ([]conflictInfo, error) {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return conflicts, err
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		if cell.RowID != c.rowID && updateRowConflicts(e, rec.Values, c.values, colDefs, colIndex, uniqueCols, idxColsList, cell.RowID, c.rowID) {
			conflicts = append(conflicts, conflictInfo{cell.RowID, rec.Values})
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return conflicts, nil
}

// deleteConflictRows deletes the rows identified as conflicts during UPDATE
// OR REPLACE resolution, firing BEFORE/AFTER DELETE triggers and rolling back
// on an error. It returns nil on success.
func (e *DMLExecutor) deleteConflictRows(tree *btree.BTree, tableEntry *schema.Entry, conflicts []conflictInfo, colDefs []sql.ColumnDef, hasTriggers bool, deletedByConflict map[int64]bool) *Result {
	// WITHOUT ROWID tables: delete conflict rows in PRIMARY KEY order (the
	// order SQLite scans its keyed table btree; hook2.test 2.3.5 observes
	// the preupdate DELETE order).
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		sort.SliceStable(conflicts, func(i, j int) bool {
			return e.withoutRowidLessVals(conflicts[i].values, conflicts[j].values, tableEntry.Name, tableEntry.SQL, colDefs)
		})
	}
	for _, cf := range conflicts {
		oldRow := buildRowMapFromValues(cf.values, colDefs, cf.rowID)
		if hasTriggers {
			if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
				return trigResult
			}
		}
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == cf.rowID
		}); err != nil {
			return &Result{Error: err}
		}
		deletedByConflict[cf.rowID] = true
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		// Fire the preupdate hook for the deleted conflicting row.
		delRowID := cf.rowID
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
			Old:          cf.values,
			New:          nil,
		}); res != nil {
			return res
		}
		if hasTriggers {
			if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, oldRow); trigResult.Error != nil {
				return trigResult
			}
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
func (e *DMLExecutor) updateRowInPlace(tree *btree.BTree, tableEntry *schema.Entry, c updateChange, deletedByConflict map[int64]bool, snap *pager.PagerState) (bool, *Result) {
	// If a conflict-resolution delete's trigger removed the row being
	// updated too (e.g. a recursive DELETE FROM t0 inside an AFTER DELETE
	// trigger), SQLite aborts the statement with the generic "constraint
	// failed" error and rolls it back. A row deleted by a PRIOR change's
	// conflict resolution is skipped, and a row deleted by THIS change's own
	// conflict resolution is also skipped.
	if deletedByConflict[c.rowID] {
		return false, nil
	}
	if !e.rowIDExists(tableEntry.Name, tableEntry.RootPage, c.rowID) {
		e.ctx.RestorePager(e.ctx.Pager(), snap)
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		return false, &Result{Error: fmt.Errorf("constraint failed")}
	}
	if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == c.rowID
	}); err != nil {
		return false, &Result{Error: err}
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	newRecord, err := storage.EncodeRecord(c.values)
	if err != nil {
		return false, &Result{Error: err}
	}
	// Write at the NEW rowid when the UPDATE re-keys the row (SET rowid=N);
	// writing the record (whose PK column holds the new id) at the old rowid
	// desyncs the btree key from the record (writeUpdateCell parity).
	writeRowID := updateWriteRowID(c)
	newCell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   writeRowID,
		Payload: newRecord,
	}
	if err := tree.InsertCell(newCell); err != nil {
		return false, &Result{Error: err}
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage, writeRowID)
	// Fire the preupdate hook with the old and new row values (UPDATE OR
	// REPLACE's in-place update write).
	rowID := c.rowID
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		rowID = 0
	}
	if res := e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "UPDATE",
		DB:    e.schemaNameForPager(e.dmlPager(tableEntry.Name)),
		Table: tableEntry.Name,
		RowID: rowID, RowID2: rowID,
		RowidTable: !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)),
		Old:        append([]interface{}(nil), c.oldValues...),
		New:        append([]interface{}(nil), c.values...),
	}); res != nil {
		return false, res
	}
	return true, nil
}

// conflictInfo records a row that conflicts with an UPDATE's new values during

// updateRowConflicts reports whether an existing row's values conflict with a
// change's new values on any UNIQUE/PRIMARY KEY column or UNIQUE index.
func updateRowConflicts(e *DMLExecutor, rowValues, newValues []interface{}, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, rowID, newRowID int64) bool {
	if uniqueColsMatch(rowValues, newValues, colDefs, rowID, newRowID, uniqueCols) {
		return true
	}
	return indexDefsMatch(e, rowValues, newValues, colDefs, colIndex, idxColsList, rowID, newRowID)
}

// uniqueColsMatch reports whether two value sets agree on any UNIQUE/PRIMARY
// KEY column index. colDefs and rowids perform the INTEGER PRIMARY KEY
// rowid-alias substitution (a stored NULL becomes the rowid before comparison).
func uniqueColsMatch(a, b []interface{}, colDefs []sql.ColumnDef, rowIDa, rowIDb int64, uniqueCols []int) bool {
	for _, idx := range uniqueCols {
		if idx < len(a) && idx < len(b) && idx < len(colDefs) {
			av, bv := a[idx], b[idx]
			if isIPKRowidAliasCol(colDefs[idx]) {
				if av == nil {
					av = rowIDa
				}
				if bv == nil {
					bv = rowIDb
				}
			}
			if av == nil || bv == nil {
				continue
			}
			if util.CompareValues(av, bv) == 0 {
				return true
			}
		}
	}
	return false
}

// indexDefsMatch reports whether two value sets agree on the indexed columns
// of any UNIQUE index (full and partial).
func indexDefsMatch(e *DMLExecutor, a, b []interface{}, colDefs []sql.ColumnDef, colIndex map[string]int, idxColsList []uniqueIndexDef, aRowID, bRowID int64) bool {
	for _, def := range idxColsList {
		nrow := buildRowMapFromValues(b, colDefs, bRowID)
		if inIndex, _ := e.evalIndexWhere(def.Where, nrow); !inIndex {
			continue
		}
		orow := buildRowMapFromValues(a, colDefs, aRowID)
		if inIndex, _ := e.evalIndexWhere(def.Where, orow); !inIndex {
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
// KEY column or UNIQUE index (partial-index predicates evaluated).
func (e *DMLExecutor) valuesConflict(a, b []interface{}, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef) bool {
	if uniqueColsMatch(a, b, colDefs, 0, 0, uniqueCols) {
		return true
	}
	return indexDefsMatch(e, a, b, colDefs, colIndex, idxColsList, 0, 0)
}

// uniqueConflictError builds a SQLite-style UNIQUE constraint error for the
// first conflicting column.
func (e *DMLExecutor) uniqueConflictError(tableName string, colDefs []sql.ColumnDef, colIndex map[string]int, a, b []interface{}, uniqueCols []int, idxColsList []uniqueIndexDef) error {
	for _, idx := range uniqueCols {
		if idx < len(a) && idx < len(b) && a[idx] != nil && b[idx] != nil && util.CompareValues(a[idx], b[idx]) == 0 {
			return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableName, colDefs[idx].Name)
		}
	}
	for _, def := range idxColsList {
		if e.valuesConflict(a, b, colDefs, colIndex, nil, []uniqueIndexDef{def}) {
			parts := make([]string, len(def.Cols))
			for i, cn := range def.Cols {
				parts[i] = tableName + "." + cn
			}
			return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(parts, ", "))
		}
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", tableName)
}
