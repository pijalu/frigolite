// Package execdml implements DML execution.
package execdml

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- DELETE ---

// internalShadowWrite is set while the engine performs bookkeeping deletes on
// FTS shadow tables (docid re-keying, segment maintenance). A DELETE issued by
// such internals is not user corruption of the index; only a user-issued
// DELETE FROM <fts>_content makes the index diverge from its content.
var internalShadowWrite bool

// BeginInternalShadowWrite marks engine-internal shadow-table deletes.
func BeginInternalShadowWrite() { internalShadowWrite = true }

// EndInternalShadowWrite clears the marker.
func EndInternalShadowWrite() { internalShadowWrite = false }

func (e *DMLExecutor) execDelete(s *sql.DeleteStmt) *Result {
	// Echo virtual tables write through to their source table.
	if srcName, ok := e.ctx.EchoVTabSource(s.Table); ok {
		s.Table = srcName
	}
	if res := e.rejectUnsafeVTabUse(s.Table); res != nil {
		return res
	}
	if res, handled := e.execVTabDelete(s); handled {
		return res
	}
	if err := e.ctx.Authorize(auth.ActionDelete, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, dbCtx, colDefs, tree, res, prevDMLCtx := e.deleteTableContext(s)
	if res != nil {
		return res
	}
	e.currentDMLCtx = dbCtx
	defer func() { e.currentDMLCtx = prevDMLCtx }()

	// Direct modification of sqlite_sequence changes AUTOINCREMENT sequences;
	// clear the in-memory cache so the next INSERT reads the real table fresh.
	if isSQLiteSequenceName(tableEntry.Name) {
		defer e.ctx.ResetAutoIncSeq()
	}

	// Collect the rows that match the WHERE clause (needed for trigger firing
	// and RETURNING) before deleting them. Set the current scan table so
	// table-qualified column references ("t6.x") resolve to the row map.
	prevScan := e.ctx.CurrentScanTable()
	e.ctx.SetCurrentScanTable(tableEntry.Name)
	deletedRows, derr := e.collectDeleteRows(tree, s, colDefs)
	e.ctx.SetCurrentScanTable(prevScan)
	if derr != nil {
		return &Result{Error: derr}
	}
	// Deleting a row from an FTS table's %_content shadow table corrupts the
	// full-text index: a later read of that document fails with "database
	// disk image is malformed" (fts3cov 16.2; fts3.c fts3Column reads the
	// content row for every output column). Record the deleted docids.
	if !internalShadowWrite && strings.HasSuffix(strings.ToLower(tableEntry.Name), "_content") {
		baseName := tableEntry.Name[:len(tableEntry.Name)-len("_content")]
		if ft, ok := e.ctx.FTSTables()[baseName]; ok && ft.ContentTable() == "" && !ft.Contentless() {
			for _, rm := range deletedRows {
				if rid, ok := util.UnwrapColumnValue(rm["rowid"]).(int64); ok {
					ft.RecordCorruptContentDocID(rid)
				}
			}
		}
	}

	// Apply DELETE ... ORDER BY ... LIMIT (a SQLite extension): sort the
	// matching rows by the ORDER BY expressions, then keep only the LIMIT
	// window. Without ORDER BY the rows are processed in rowid order; LIMIT
	// alone applies to that natural order.
	if len(s.OrderBy) > 0 {
		e.sortDeleteRows(deletedRows, s.OrderBy)
	}
	if s.Limit != nil {
		var lerr error
		deletedRows, lerr = e.limitDeleteRows(deletedRows, s)
		if lerr != nil {
			return &Result{Error: lerr}
		}
	}
	// The ORDER BY only selects WHICH rows fall within the LIMIT; the rows
	// are then deleted in rowid order (SQLite R-07548-13422: "the order in
	// which rows are deleted is arbitrary and is not influenced by the ORDER
	// BY clause. In practice, rows are always deleted in rowid order.").
	// Re-sort by rowid so trigger logging (OLD.a order) matches SQLite.
	if len(s.OrderBy) > 0 {
		sort.SliceStable(deletedRows, func(i, j int) bool {
			ri, _ := util.UnwrapColumnValue(deletedRows[i]["rowid"]).(int64)
			rj, _ := util.UnwrapColumnValue(deletedRows[j]["rowid"]).(int64)
			return ri < rj
		})
	}

	// Fire BEFORE DELETE triggers, delete the row, evaluate RETURNING
	// against the post-delete state, and fire AFTER DELETE triggers — one
	// row at a time (SQLite semantics; RETURNING subqueries must observe
	// the table without the current row).
	// Without RETURNING, all BEFORE triggers fire first, rows are deleted
	// in a single pass (O(n), whereas per-row delete is O(n²)), then all
	// AFTER triggers fire.
	if !s.HasReturning {
		return e.execDeleteBulk(tableEntry, dbCtx, tree, colDefs, deletedRows)
	}

	// RETURNING path: process one row at a time so RETURNING subqueries
	// observe the table with the current row already removed.
	return e.execDeleteReturning(s, tableEntry, tree, colDefs, deletedRows)
}

// rowMapColumnValues extracts a row's column values in colDefs order
// (unwrapping any collation wrappers), for preupdate-hook old/new reporting.
func (e *DMLExecutor) rowMapColumnValues(row RowMap, colDefs []sql.ColumnDef) []interface{} {
	vals := make([]interface{}, 0, len(colDefs))
	for _, cd := range colDefs {
		if v, ok := row[cd.Name]; ok {
			vals = append(vals, util.UnwrapColumnValue(v))
		} else {
			vals = append(vals, nil)
		}
	}
	return vals
}

// withoutRowidLess orders two WITHOUT ROWID rows by their PRIMARY KEY columns
// (the order SQLite's table btree stores and scans them). The PK column
// indices come from the table constraints.
func (e *DMLExecutor) withoutRowidLess(a, b RowMap, tableName, createSQL string, colDefs []sql.ColumnDef) bool {
	pkIdx := e.withoutRowidPKIdx(tableName, createSQL, colDefs)
	if len(pkIdx) == 0 {
		return false
	}
	for _, idx := range pkIdx {
		if idx >= len(colDefs) {
			continue
		}
		av, aok := a[colDefs[idx].Name]
		bv, bok := b[colDefs[idx].Name]
		if !aok || !bok {
			continue
		}
		c := e.ctx.CompareValuesCollate(util.UnwrapColumnValue(av), util.UnwrapColumnValue(bv), colDefs[idx].Collate)
		if c != 0 {
			return c < 0
		}
	}
	return false
}

// withoutRowidLessVals orders two WITHOUT ROWID value slices by their PRIMARY
// KEY columns (the order SQLite's table btree stores and scans them).
func (e *DMLExecutor) withoutRowidLessVals(a, b []interface{}, tableName, createSQL string, colDefs []sql.ColumnDef) bool {
	pkIdx := e.withoutRowidPKIdx(tableName, createSQL, colDefs)
	if len(pkIdx) == 0 {
		return false
	}
	for _, idx := range pkIdx {
		if idx >= len(a) || idx >= len(b) || idx >= len(colDefs) {
			continue
		}
		av, bv := a[idx], b[idx]
		if av == nil || bv == nil {
			continue
		}
		c := e.ctx.CompareValuesCollate(util.UnwrapColumnValue(av), util.UnwrapColumnValue(bv), colDefs[idx].Collate)
		if c != 0 {
			return c < 0
		}
	}
	return false
}

// withoutRowidPKIdx returns the PRIMARY KEY column indices for a WITHOUT ROWID
// table (single-column PK flags or the composite PK constraint's column order).
func (e *DMLExecutor) withoutRowidPKIdx(tableName, createSQL string, colDefs []sql.ColumnDef) []int {
	colIndex := buildColumnIndex(colDefs)
	var pkIdx []int
	for i, cd := range colDefs {
		if cd.PrimaryKey {
			pkIdx = append(pkIdx, i)
		}
	}
	if len(pkIdx) == 0 {
		constraints := e.ctx.TableConstraints(tableName, createSQL)
		for _, tc := range constraints {
			if tc.Type == sql.ConstraintPrimaryKey {
				for _, ic := range tc.Columns {
					if idx, ok := colIndex[ic.Name]; ok && idx >= 0 {
						pkIdx = append(pkIdx, idx)
					}
				}
				break
			}
		}
	}
	return pkIdx
}

// deleteTableContext resolves the DELETE's target table (routing views through
// INSTEAD OF triggers and FTS tables through their delete path), guards against
// modification of protected tables, validates RETURNING, and returns the
// table entry, db context, column defs, b-tree, plus any error result (or view
// route). It also returns the prior DMLCtx for trigger-scope restoration.
func (e *DMLExecutor) deleteTableContext(s *sql.DeleteStmt) (*schema.Entry, *DatabaseContext, []sql.ColumnDef, *btree.BTree, *Result, *DatabaseContext) {
	tableEntry, dbCtx, err := e.ctx.FindTable(s.Table)
	if err != nil {
		// Not a table — route through INSTEAD OF DELETE triggers on a view.
		viewEntry, _, viewErr := e.ctx.FindView(s.Table)
		if viewErr == nil {
			return nil, nil, nil, nil, e.execDeleteView(s, viewEntry), nil
		}
		return nil, nil, nil, nil, &Result{Error: err}, nil
	}
	if e.ctx.IsNonModifiableTable(tableEntry) {
		return nil, nil, nil, nil, &Result{Error: fmt.Errorf("table %s may not be modified", tableEntry.Name)}, nil
	}
	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	if s.HasReturning {
		if err := e.validateReturning(s.Returning, colDefs, tableEntry.Name); err != nil {
			return nil, nil, nil, nil, &Result{Error: err}, nil
		}
	}
	prevDMLCtx := e.currentDMLCtx
	// Route FTS virtual table deletes
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		return nil, nil, nil, nil, e.ctx.ExecFTSDelete(tableEntry.Name, ftsTable, colDefs, s), prevDMLCtx
	}
	tree := e.ctx.TableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)
	return tableEntry, dbCtx, colDefs, tree, nil, prevDMLCtx
}

// collectDeleteRows scans a table b-tree and returns the rows matching the
// DELETE's WHERE clause (in rowid order), for trigger firing and RETURNING.
func (e *DMLExecutor) collectDeleteRows(tree *btree.BTree, s *sql.DeleteStmt, colDefs []sql.ColumnDef) ([]RowMap, error) {
	var deletedRows []RowMap
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil
	}
	for {
		// SQLITE_TEST interrupt countdown: one op per row examined
		// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count).
		if err := e.ctx.CheckProgress(); err != nil {
			return deletedRows, err
		}
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}
		row := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
		match, err := e.rowMatchesWhere(s.Where, row)
		if err != nil {
			return deletedRows, err
		}
		if match {
			deletedRows = append(deletedRows, row)
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return deletedRows, nil
}

// execDeleteBulk executes a DELETE without RETURNING. SQLite's delete.c
// (GenRowDel) processes each row as: fire BEFORE triggers, delete the row, fire
// AFTER triggers (delete.c:807-872). The engine mirrors this per-row order so
// BEFORE/AFTER trigger side effects interleave as SQLite's do.
func (e *DMLExecutor) execDeleteBulk(tableEntry *schema.Entry, dbCtx *DatabaseContext, tree *btree.BTree, colDefs []sql.ColumnDef, deletedRows []RowMap) *Result {
	// Snapshot for the FK-failure rollback below. Skip it for the FTS flush's
	// internal shadow-table deletes: they are part of the enclosing statement's
	// rollback scope, and copying the whole pager per deleted %_segdir row made
	// the automerge's segment cleanup O(n^2) (deleteFTSSegdirIdx was ~20% of
	// the fts4merge4 profile; fts4merge4 2.2.x).
	var snap *pager.PagerState
	if !e.ctx.InFTSFlush() {
		snap = dbCtx.Pager.Snapshot()
	}
	deleted := int64(0)
	rowsToKeep := make([]RowMap, 0, len(deletedRows))
	// WITHOUT ROWID tables store rows keyed by a synthetic rowid, so the
	// btree scan returns insertion order, not PRIMARY KEY order. SQLite
	// iterates the WITHOUT ROWID table btree in PK order (the preupdate
	// hook and DELETE triggers observe that order, hook2.test 2.2.2), so
	// sort the deleted rows by their PRIMARY KEY values.
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		sort.SliceStable(deletedRows, func(i, j int) bool {
			return e.withoutRowidLess(deletedRows[i], deletedRows[j], tableEntry.Name, tableEntry.SQL, colDefs)
		})
	}
	if !e.hasTriggersForTable(tableEntry.Name) {
		// No delete triggers: batch the btree delete into ONE pass (the
		// per-row DeleteCellsWhere re-walked the whole tree for every row —
		// O(rows × tree), which made DELETE FROM %_segments (thousands of
		// 4KB blob rows) take ~40s; fts4merge4's between-scenario DELETE).
		rowIDs := make(map[int64]bool, len(deletedRows))
		for _, row := range deletedRows {
			if rowID, ok := util.UnwrapColumnValue(row["rowid"]).(int64); ok {
				rowIDs[rowID] = true
			}
		}
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return rowIDs[cell.RowID]
		}); err != nil {
			return &Result{Error: err}
		}
		for _, row := range deletedRows {
			rowID, _ := util.UnwrapColumnValue(row["rowid"]).(int64)
			oldVals := e.rowMapColumnValues(row, colDefs)
			delRowID := rowID
			if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
				delRowID = 0
			}
			if res := e.ctx.FirePreupdate(PreupdateEvent{
				Type:  "DELETE",
				DB:    e.schemaNameForPager(dbCtx.Pager),
				Table: tableEntry.Name,
				RowID: delRowID, RowID2: delRowID,
				RowidTable: !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)),
				Old:        oldVals,
				New:        nil,
			}); res != nil {
				return res
			}
			deleted++
			rowsToKeep = append(rowsToKeep, row)
		}
	} else {
		for _, row := range deletedRows {
			rowID, _ := util.UnwrapColumnValue(row["rowid"]).(int64)
			if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, execquery.UnwrapRowMap(row)); trigResult.Error != nil {
				if trigResult.Error == errRaiseIgnore {
					continue
				}
				return trigResult
			}
			if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
				return cell.RowID == rowID
			}); err != nil {
				return &Result{Error: err}
			}
			// Fire the preupdate hook with the deleted row's values.
			oldVals := e.rowMapColumnValues(row, colDefs)
			delRowID := rowID
			if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
				delRowID = 0
			}
			if res := e.ctx.FirePreupdate(PreupdateEvent{
				Type:  "DELETE",
				DB:    e.schemaNameForPager(dbCtx.Pager),
				Table: tableEntry.Name,
				RowID: delRowID, RowID2: delRowID,
				RowidTable: !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)),
				Old:        oldVals,
				New:        nil,
			}); res != nil {
				return res
			}
			deleted++
			rowsToKeep = append(rowsToKeep, row)
			if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, execquery.UnwrapRowMap(row)); trigResult.Error != nil {
				return trigResult
			}
		}
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	// Enforce FOREIGN KEY actions against the post-trigger state. On a
	// RESTRICT/NO ACTION error the whole statement is rolled back. Only rows
	// that were actually deleted (survived BEFORE triggers) get FK actions.
	if e.ctx.ForeignKeys() {
		for _, row := range rowsToKeep {
			if res := e.ctx.FkParentDelete(tableEntry, colDefs, row); res.Error != nil {
				e.ctx.RestorePager(dbCtx.Pager, snap)
				e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
				return res
			}
		}
	}
	return &Result{Changes: deleted}
}

// execDeleteReturning executes a DELETE ... RETURNING one row at a time so
// RETURNING subqueries observe the table with the current row removed.
func (e *DMLExecutor) execDeleteReturning(s *sql.DeleteStmt, tableEntry *schema.Entry, tree *btree.BTree, colDefs []sql.ColumnDef, deletedRows []RowMap) *Result {
	var returningRows [][]interface{}
	for _, row := range deletedRows {
		rowID, _ := util.UnwrapColumnValue(row["rowid"]).(int64)
		if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, execquery.UnwrapRowMap(row)); trigResult.Error != nil {
			if trigResult.Error == errRaiseIgnore {
				continue
			}
			return trigResult
		}
		if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == rowID
		}); err != nil {
			return &Result{Error: err}
		}
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		values, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
		if err != nil {
			return &Result{Error: err}
		}
		returningRows = append(returningRows, values)
		if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, execquery.UnwrapRowMap(row)); trigResult.Error != nil {
			return trigResult
		}
	}
	columns := e.ctx.BuildColumnNames([]sql.SelectColumn{s.Returning}, colDefs, nil)
	return &Result{Columns: columns, Rows: returningRows}
}

// execDeleteView routes DELETE on a view through INSTEAD OF DELETE triggers.
// The view's SELECT is executed (with the DELETE's WHERE applied) to find
// matching rows; for each, the trigger fires with OLD.* values.
func (e *DMLExecutor) execDeleteView(s *sql.DeleteStmt, viewEntry *schema.Entry) *Result {
	if !e.hasTriggersForTable(viewEntry.Name) {
		return &Result{Error: fmt.Errorf("cannot modify %s because it is a view", viewEntry.Name)}
	}
	// Qualified view column references (main.v5.x, v5.b) must resolve against
	// the view row during WHERE evaluation.
	prevDML := e.currentDMLTable
	e.currentDMLTable = viewEntry.Name
	defer func() { e.currentDMLTable = prevDML }()
	viewResult := e.ctx.ExecSelectView(viewEntry)
	if viewResult.Error != nil {
		return viewResult
	}
	viewCols := viewResult.Columns
	// Apply the view's declared column list (CREATE VIEW v(a,b) AS ...) so
	// INSTEAD OF trigger OLD/NEW rows are keyed by the declared names even
	// when the SELECT produces expression columns without names.
	if decl := e.viewDeclaredColumns(viewEntry); len(decl) > 0 {
		viewCols = decl
	}
	// Materialize matching rows first so DELETE ... ORDER BY ... LIMIT applies
	// to view deletes too (SQLite applies the ORDER BY/LIMIT to the set of
	// rows the INSTEAD OF trigger processes).
	matched := e.matchViewDeleteRows(viewResult.Rows, viewCols, s.Where)
	if len(s.OrderBy) > 0 {
		e.sortDeleteRows(matched, s.OrderBy)
	}
	if s.Limit != nil {
		var lerr error
		matched, lerr = e.limitDeleteRows(matched, s)
		if lerr != nil {
			return &Result{Error: lerr}
		}
	}
	for _, oldRow := range matched {
		if res := e.fireTriggers(viewEntry.Name, "DELETE", "INSTEAD", nil, oldRow); res != nil && res.Error != nil {
			return res
		}
	}
	// The view delete itself counts 0 changes (SQLite: INSTEAD OF trigger
	// interception is not counted); the trigger body's DML counts via its
	// own Exec.
	return &Result{}
}

// matchViewDeleteRows materializes the view result rows that satisfy the
// DELETE WHERE clause (ORDER BY/LIMIT apply afterwards).
func (e *DMLExecutor) matchViewDeleteRows(rows [][]interface{}, viewCols []string, where sql.Expr) []RowMap {
	var matched []RowMap
	for _, rowVals := range rows {
		oldRow := viewDeleteRow(rowVals, viewCols)
		if where != nil {
			pass, err := e.ctx.EvalBool(where, oldRow)
			if err != nil || !pass {
				continue
			}
		}
		matched = append(matched, oldRow)
	}
	return matched
}

// viewDeleteRow builds the OLD row map for a view DELETE from one result row.
func viewDeleteRow(rowVals []interface{}, viewCols []string) RowMap {
	oldRow := make(RowMap)
	for i, v := range rowVals {
		if i < len(viewCols) {
			oldRow[viewCols[i]] = v
		}
	}
	oldRow["rowid"] = nil
	return oldRow
}

// sortDeleteRows sorts the rows to delete by the ORDER BY expressions evaluated
// against each row's values (SQLite DELETE ... ORDER BY ... LIMIT).
func (e *DMLExecutor) sortDeleteRows(rows []RowMap, orderBy []sql.OrderByTerm) {
	if len(rows) <= 1 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, ob := range orderBy {
			left, _ := e.ctx.EvalExpr(ob.Expr, rows[i])
			right, _ := e.ctx.EvalExpr(ob.Expr, rows[j])
			cmp := e.ctx.CompareOrderByValues(left, right, ob)
			if cmp < 0 {
				return true
			} else if cmp > 0 {
				return false
			}
		}
		return false
	})
}

// limitDeleteRows applies DELETE ... LIMIT n [OFFSET m] to the row list, keeping
// the first n entries after skipping m (SQLite semantics for DELETE LIMIT: the
// first N rows matched by the scan order are deleted). A LIMIT expression that
// cannot be cast to an integer is an error ("datatype mismatch"), matching
// SQLite.
func (e *DMLExecutor) limitDeleteRows(rows []RowMap, s *sql.DeleteStmt) ([]RowMap, error) {
	limit, err := e.evalConstInt(s.Limit)
	if err != nil {
		return rows, err
	}
	if limit < 0 {
		return rows, nil
	}
	offset := int64(0)
	if s.Offset != nil {
		if v, err := e.evalConstInt(s.Offset); err != nil {
			return rows, err
		} else if v > 0 {
			offset = v
		}
	}
	if offset >= int64(len(rows)) {
		return nil, nil
	}
	end := offset + limit
	if end > int64(len(rows)) {
		end = int64(len(rows))
	}
	return rows[offset:end], nil
}
