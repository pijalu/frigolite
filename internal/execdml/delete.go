// Package execdml implements DML execution.
package execdml

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
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

// execDelete wraps the DELETE pipeline with the echo module's error prefix:
// a DELETE routed through an echo virtual table reports failures from the
// source write as "echo-vtab-error: %s" (test8.c echoError / xUpdate).
func (e *DMLExecutor) execDelete(s *sql.DeleteStmt) *Result {
	if _, ok := e.ctx.EchoVTabSource(s.Table); !ok {
		return e.execDeleteInner(s)
	}
	// The echo module's xBegin runs before the statement's first write
	// (vtab.c sqlite3VtabBegin): a failed module transaction start vetoes
	// the whole statement (test8.c echoBegin, vtab1.10-3).
	if err, ok := e.ctx.EchoVTabBegin(s.Table); ok && err != nil {
		return &Result{Error: err}
	}
	e.echoWriteDepth++
	res := e.execDeleteInner(s)
	if res.Error != nil {
		res.Error = e.wrapEchoWriteError(res.Error)
	}
	e.echoWriteDepth--
	return res
}

// execDeleteInner is execDelete's statement pipeline (the echo write-through
// wrapper above re-routes its errors).
func (e *DMLExecutor) execDeleteInner(s *sql.DeleteStmt) *Result {
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
	if res := e.CheckSameFileWriteConflictRes(dbCtx); res != nil {
		return res
	}
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
	deletedRows, derr := e.collectDeleteRows(tree, s, tableEntry, colDefs)
	e.ctx.SetCurrentScanTable(prevScan)
	if derr != nil {
		return &Result{Error: derr}
	}
	// Deleting a row from an FTS table's %_content shadow table corrupts the
	// full-text index: a later read of that document fails with "database
	// disk image is malformed" (fts3cov 16.2; fts3.c fts3Column reads the
	// content row for every output column). Record the deleted docids.
	e.recordDeletedContentDocs(tableEntry, deletedRows)

	// Apply DELETE ... ORDER BY ... LIMIT (a SQLite extension): sort the
	// matching rows by the ORDER BY expressions, then keep only the LIMIT
	// window. Without ORDER BY the rows are processed in rowid order; LIMIT
	// alone applies to that natural order.
	var lerr *Result
	deletedRows, lerr = e.applyDeleteOrderLimit(s, deletedRows)
	if lerr != nil {
		return lerr
	}
	// The ORDER BY only selects WHICH rows fall within the LIMIT; the rows
	// are then deleted in rowid order (SQLite R-07548-13422: "the order in
	// which rows are deleted is arbitrary and is not influenced by the ORDER
	// BY clause. In practice, rows are always deleted in rowid order.").
	// Re-sort by rowid so trigger logging (OLD.a order) matches SQLite.
	// (applyDeleteOrderLimit performed the re-sort.)

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

// recordDeletedContentDocs records the docids of a user-issued DELETE from an
// FTS table's %_content shadow table: a later read of such a document fails
// with "database disk image is malformed" (fts3cov 16.2; fts3.c fts3Column
// reads the content row for every output column). Engine-internal shadow
// writes are exempt.
func (e *DMLExecutor) recordDeletedContentDocs(tableEntry *schema.Entry, deletedRows []*dmlRow) {
	if internalShadowWrite || !strings.HasSuffix(strings.ToLower(tableEntry.Name), "_content") {
		return
	}
	baseName := tableEntry.Name[:len(tableEntry.Name)-len("_content")]
	ft, ok := e.ctx.FTSTables()[baseName]
	if !ok || ft.ContentTable() != "" || ft.Contentless() {
		return
	}
	for _, rm := range deletedRows {
		ft.RecordCorruptContentDocID(rm.rowID)
	}
}

// applyDeleteOrderLimit applies DELETE ... ORDER BY ... LIMIT (a SQLite
// extension): sort the matching rows by the ORDER BY expressions, keep only
// the LIMIT window, then re-sort by rowid. The ORDER BY only selects WHICH
// rows fall within the LIMIT; the rows are then deleted in rowid order
// (SQLite R-07548-13422: "the order in which rows are deleted is arbitrary
// and is not influenced by the ORDER BY clause. In practice, rows are always
// deleted in rowid order.") so trigger logging (OLD.a order) matches SQLite.
func (e *DMLExecutor) applyDeleteOrderLimit(s *sql.DeleteStmt, deletedRows []*dmlRow) ([]*dmlRow, *Result) {
	if len(s.OrderBy) > 0 {
		e.sortDMLRowsByOrderBy(deletedRows, s.OrderBy)
	}
	if s.Limit != nil {
		rows, err := e.limitDMLRows(deletedRows, s)
		if err != nil {
			return nil, &Result{Error: err}
		}
		deletedRows = rows
	}
	if len(s.OrderBy) > 0 {
		sort.SliceStable(deletedRows, func(i, j int) bool {
			return deletedRows[i].rowID < deletedRows[j].rowID
		})
	}
	return deletedRows, nil
}

// sortDMLRowsByOrderBy sorts collected rows by the ORDER BY expressions
// evaluated against each row's values (DELETE ... ORDER BY ... LIMIT). The
// expressions see the row's affinity/collation-wrapped values exactly as
// sortDeleteRows' map evaluation did; the lazily materialized maps stay
// cached on the rows for the later trigger/FK/index consumers.
func (e *DMLExecutor) sortDMLRowsByOrderBy(rows []*dmlRow, orderBy []sql.OrderByTerm) {
	if len(rows) <= 1 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, ob := range orderBy {
			left, _ := e.ctx.EvalExpr(ob.Expr, rows[i].rowMap(e))
			right, _ := e.ctx.EvalExpr(ob.Expr, rows[j].rowMap(e))
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

// limitDMLRows applies DELETE ... LIMIT n [OFFSET m] to the collected row
// list, keeping the first n entries after skipping m (limitDeleteRows'
// semantics over positional rows).
func (e *DMLExecutor) limitDMLRows(rows []*dmlRow, s *sql.DeleteStmt) ([]*dmlRow, error) {
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

// unwrapDMLValue peels the row-map value wrappers down to the raw scalar:
// a CollatedValue collation marker around a ColumnValue affinity marker
// around the value (the WHERE-comparison wrappers). The delete-identity keys
// and preupdate values must compare against raw stored scalars — a leaked
// wrapper makes the WR PK-key match silently fail and the delete no-ops
// (without_rowid3-12.2.4's NOCASE PK t1 kept every deleted row).
func unwrapDMLValue(v interface{}) interface{} {
	// Wrappers nest at most two deep; the bound just keeps the loop
	// obviously terminating without comparing interface values (a raw
	// []byte payload is not comparable).
	for i := 0; i < 4; i++ {
		switch cv := v.(type) {
		case *execexpr.CollatedValue:
			v = cv.Value
		case *util.ColumnValue:
			v = cv.Value
		default:
			return v
		}
	}
	return v
}

// rowMapColumnValues extracts a row's column values in colDefs order
// (unwrapping any collation wrappers), for preupdate-hook old/new reporting.
func (e *DMLExecutor) rowMapColumnValues(row RowMap, colDefs []sql.ColumnDef) []interface{} {
	vals := make([]interface{}, 0, len(colDefs))
	for _, cd := range colDefs {
		if v, ok := row[cd.Name]; ok {
			vals = append(vals, unwrapDMLValue(v))
		} else {
			vals = append(vals, nil)
		}
	}
	return vals
}

// withoutRowidLessDML orders two positionally collected WITHOUT ROWID rows by
// their PRIMARY KEY columns (the order SQLite's table btree stores and scans
// them). The PK column indices come from the table constraints; the raw
// declared-order snapshots hold the exact values the collected map exposed
// (unwrapDMLValue over its affinity-wrapped values), so the PK-column
// comparison is identical.
// the raw declared-order snapshots hold the exact values the collected map
// exposed (unwrapDMLValue over the map's wrapped values), so the PK-column
// comparison is identical.
func (e *DMLExecutor) withoutRowidLessDML(a, b *dmlRow, tableName, createSQL string, colDefs []sql.ColumnDef) bool {
	pkIdx := e.withoutRowidPKIdx(tableName, createSQL, colDefs)
	if len(pkIdx) == 0 {
		return false
	}
	for _, idx := range pkIdx {
		if idx >= len(colDefs) || idx >= len(a.values) || idx >= len(b.values) {
			continue
		}
		c := e.ctx.CompareValuesCollate(unwrapDMLValue(a.values[idx]), unwrapDMLValue(b.values[idx]), colDefs[idx].Collate)
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
	var pkIdx []int
	for i, cd := range colDefs {
		if cd.PrimaryKey {
			pkIdx = append(pkIdx, i)
		}
	}
	if len(pkIdx) == 0 {
		pkIdx = e.pkIdxFromConstraints(tableName, createSQL, e.columnIndexFor(colDefs))
	}
	return pkIdx
}

// pkIdxFromConstraints returns the composite PRIMARY KEY constraint's column
// indices (declaration order) from the table constraints.
func (e *DMLExecutor) pkIdxFromConstraints(tableName, createSQL string, colIndex map[string]int) []int {
	var pkIdx []int
	for _, tc := range e.ctx.TableConstraints(tableName, createSQL) {
		if tc.Type == sql.ConstraintPrimaryKey {
			for _, ic := range tc.Columns {
				if idx, ok := colIndex[ic.Name]; ok && idx >= 0 {
					pkIdx = append(pkIdx, idx)
				}
			}
			break
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
	// Alias masking for DELETE ("DELETE FROM t1 AS a WHERE t1.x=1").
	if err == nil {
		if res := e.validateDeleteTargetExprs(s, tableEntry); res != nil {
			return nil, nil, nil, nil, res, nil
		}
	}
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
	// A WHERE-clause DELETE is row-by-row and maintains every index on the
	// table, so each index key's collation must resolve at prepare time
	// (build.c sqlite3LocateCollSeq; collate3-3.4). A WHERE-less DELETE
	// truncates the table b-tree and needs no collation (collate3-3.6).
	if s.Where != nil {
		if res := e.validateIndexCollations(tableEntry, colDefs, nil); res != nil {
			return nil, nil, nil, nil, res, nil
		}
	}
	if s.HasReturning {
		if err := e.validateReturning(s.Returning, colDefs, tableEntry.Name); err != nil {
			return nil, nil, nil, nil, &Result{Error: err}, nil
		}
	}
	prevDMLCtx := e.currentDMLCtx
	// Route fts5 virtual table deletes through the fts5 engine.
	if t5, ok := e.ctx.FTS5Tables()[tableEntry.Name]; ok {
		return nil, nil, nil, nil, e.execFTS5Delete(t5, colDefs, s), prevDMLCtx
	}
	// Route FTS virtual table deletes
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		return nil, nil, nil, nil, e.ctx.ExecFTSDelete(tableEntry.Name, ftsTable, colDefs, s), prevDMLCtx
	}
	tree := e.ctx.TableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)
	return tableEntry, dbCtx, colDefs, tree, nil, prevDMLCtx
}

// validateDeleteTargetExprs runs the DELETE target's prepare-time checks:
// alias masking ("DELETE FROM t1 AS a WHERE t1.x=1" must not resolve t1
// through the alias) and resolve.c parity — the WHERE must resolve every
// column and function against the target table.
func (e *DMLExecutor) validateDeleteTargetExprs(s *sql.DeleteStmt, tableEntry *schema.Entry) *Result {
	if res := e.validateDMLAliasQualifier(s.Table, s.Alias, []sql.Expr{s.Where}); res != nil {
		return res
	}
	qualifiers := []string{s.Table}
	if s.Alias != "" {
		qualifiers = append(qualifiers, s.Alias)
	}
	colDefs := e.ctx.ParseColumnDefs(s.Table, tableEntry.SQL)
	return e.validateDMLExprs(qualifiers, colDefs, !tableIsWithoutRowid(tableEntry.SQL), []sql.Expr{s.Where})
}

// trueRowidKey is the reserved RowMap key carrying the row's TRUE btree
// rowid. It is never a SQL-resolvable name (identifiers cannot contain NUL),
// so a table that DECLARES a column named rowid/_rowid_/oid keeps expression
// resolution on its declared column (the shadow rule) while the delete
// machinery still addresses cells by the real rowid (rowid-4.2: DELETE FROM
// t2 with t2(rowid int, ...) must remove every row).
const trueRowidKey = "\x00trueRowid"

// dmlRow is one collected DML row in positional form: the raw declared-order
// value snapshot (dropped-column re-alignment, added-column DEFAULTs and the
// INTEGER PRIMARY KEY rowid-alias substitution applied as raw values) plus
// the row's TRUE btree rowid. A name-keyed RowMap is materialized lazily (at
// most once per row) at the consumers whose contract demands name-keyed
// access: trigger OLD/NEW rows, RETURNING projection, FK parent actions and
// partial-index predicates.
type dmlRow struct {
	plan       *execquery.DMLRowPlan
	values     []interface{} // raw declared-order snapshot
	valueCount int           // stored record's value count (pre-ALTER short records)
	rowID      int64         // true btree rowid
	m          RowMap        // lazily materialized name-keyed map
}

// rowMap materializes (and caches) the row's name-keyed map. Keys and values
// mirror the collected BuildRowMap the scan built before positional
// collection; the reserved trueRowidKey carries the TRUE btree rowid
// (rowTrueRowID reads it) exactly as the collect-time map did.
func (r *dmlRow) rowMap(e *DMLExecutor) RowMap {
	if r.m == nil {
		r.m = e.ctx.RowMapFromDeclared(r.plan, r.values, r.valueCount, r.rowID)
		r.m[trueRowidKey] = r.rowID
	}
	return r.m
}

// rowTrueRowID returns the row's true btree rowid: the reserved key when the
// scan recorded it, else the legacy "rowid" slot.
func rowTrueRowID(row RowMap) (int64, bool) {
	if v, ok := row[trueRowidKey]; ok {
		return util.UnwrapColumnValue(v).(int64), true
	}
	id, ok := util.UnwrapColumnValue(row["rowid"]).(int64)
	return id, ok
}

// collectDeleteRows scans a table b-tree and returns the rows matching the
// DELETE's WHERE clause (in rowid order), for trigger firing and RETURNING.
// WITHOUT ROWID tables store PK-first index cells, so each decoded record is
// remapped to declared order before the WHERE row is evaluated.
//
// Rows are collected positionally (the SELECT scan's StructRow model): the
// WHERE clause evaluates against one reused StructRow whose referenced
// columns carry affinity/collation wrappers, and the matched rows are
// retained as raw value snapshots — a name-keyed RowMap is built lazily only
// when a consumer's contract demands one.
//
// A point-lookup WHERE (rowid = <const> or col = <const> on an index's
// leading column) collects candidates through seeks instead of a full scan
// (see seek.go); the full WHERE is still evaluated per candidate row, so the
// returned row set is identical to the scan's.
func (e *DMLExecutor) collectDeleteRows(tree *btree.BTree, s *sql.DeleteStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) ([]*dmlRow, error) {
	if rows, ok := e.seekDeleteRows(tree, s, tableEntry, colDefs); ok {
		return rows, nil
	}
	var deletedRows []*dmlRow
	plan := e.ctx.NewDMLRowPlan(colDefs, []sql.Expr{s.Where}, s.OrderBy)
	srow := plan.NewRow()
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, err
	}
	for {
		cell, err := e.nextScanCell(cursor)
		if err != nil {
			return deletedRows, err
		}
		if cell == nil {
			break
		}
		row, stop, err := e.decodeDeleteScanRow(plan, srow, cell, s, tableEntry, colDefs)
		if err != nil {
			return deletedRows, err
		}
		if stop {
			break
		}
		if row != nil {
			deletedRows = append(deletedRows, row)
		}
		if !e.advanceDeleteCursor(cursor) {
			break
		}
	}
	return deletedRows, nil
}

// advanceDeleteCursor steps the scan cursor to the next cell; false ends the
// scan (cursor exhausted or a traversal error).
func (e *DMLExecutor) advanceDeleteCursor(cursor *btree.Cursor) bool {
	ok, err := cursor.Next()
	return err == nil && ok
}

// nextScanCell returns the cell at the cursor, honoring the SQLITE_TEST
// progress interrupt (one op per row examined — src/vdbe.c per-opcode
// decrement of sqlite3_interrupt_count). A nil cell (with nil error) ends
// the scan.
func (e *DMLExecutor) nextScanCell(cursor *btree.Cursor) (*storage.Cell, error) {
	// SQLITE_TEST interrupt countdown: one op per row examined
	// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count).
	if err := e.ctx.CheckProgress(); err != nil {
		return nil, err
	}
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return nil, nil
	}
	return cell, nil
}

// decodeDeleteScanRow decodes the cell at the cursor into a WHERE-ready row
// (remapping WITHOUT ROWID records to declared order and recording the true
// btree rowid — a declared rowid-named column shadows the "rowid" name for
// expression resolution, so the delete machinery reads it from the row's
// true-rowid field) and evaluates the DELETE's WHERE clause. stop reports
// the scan must break (a record decode failure); row is non-nil when the row
// matched; err is a WHERE evaluation error that aborts the scan.
func (e *DMLExecutor) decodeDeleteScanRow(plan *execquery.DMLRowPlan, srow *execquery.StructRow, cell *storage.Cell, s *sql.DeleteStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) (*dmlRow, bool, error) {
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil {
		return nil, true, nil
	}
	e.ctx.RemapWRRecordToDeclared(rec, tableEntry.SQL, colDefs)
	e.ctx.FillDMLRow(plan, srow, rec.Values, len(rec.Values), cell.RowID)
	match, err := e.rowMatchesWhere(s.Where, srow)
	if err != nil {
		return nil, false, err
	}
	if !match {
		return nil, false, nil
	}
	return &dmlRow{
		plan:       plan,
		values:     e.ctx.DMLRowSnapshot(plan, rec.Values, len(rec.Values), cell.RowID),
		valueCount: len(rec.Values),
		rowID:      cell.RowID,
	}, false, nil
}

// execDeleteBulk executes a DELETE without RETURNING. SQLite's delete.c
// (GenRowDel) processes each row as: fire BEFORE triggers, delete the row, fire
// AFTER triggers (delete.c:807-872). The engine mirrors this per-row order so
// BEFORE/AFTER trigger side effects interleave as SQLite's do.
func (e *DMLExecutor) execDeleteBulk(tableEntry *schema.Entry, dbCtx *DatabaseContext, tree *btree.BTree, colDefs []sql.ColumnDef, deletedRows []*dmlRow) *Result {
	// Statement journal for the FK-failure rollback below (pager.c
	// sub-journal: before-images captured lazily at first page write, so a
	// statement that matches no rows — or deletes without an FK error — pays
	// O(1) here instead of the O(database) deep copy a PagerState snapshot
	// took). Skip it for the FTS flush's internal shadow-table deletes: they
	// are part of the enclosing statement's rollback scope (deleteFTSSegdirIdx
	// was ~20% of the fts4merge4 profile).
	var stmt *pager.StmtJournal
	if !e.ctx.InFTSFlush() {
		stmt = dbCtx.Pager.BeginStatement()
		defer dbCtx.Pager.EndStatement(stmt)
	}
	// WITHOUT ROWID tables store rows keyed by a synthetic rowid, so the
	// btree scan returns insertion order, not PRIMARY KEY order. SQLite
	// iterates the WITHOUT ROWID table btree in PK order (the preupdate
	// hook and DELETE triggers observe that order, hook2.test 2.2.2), so
	// sort the deleted rows by their PRIMARY KEY values.
	if tableIsWithoutRowid(tableEntry.SQL) {
		sort.SliceStable(deletedRows, func(i, j int) bool {
			return e.withoutRowidLessDML(deletedRows[i], deletedRows[j], tableEntry.Name, tableEntry.SQL, colDefs)
		})
	}
	var deleted int64
	var rowsToKeep []*dmlRow
	var res *Result
	if !e.hasTriggersForTable(tableEntry.Name) {
		// No delete triggers: batch the btree delete into ONE pass (the
		// per-row DeleteCellsWhere re-walked the whole tree for every row —
		// O(rows × tree), which made DELETE FROM %_segments (thousands of
		// 4KB blob rows) take ~40s; fts4merge4's between-scenario DELETE).
		deleted, rowsToKeep, res = e.deleteBulkNoTriggers(tableEntry, dbCtx, colDefs, deletedRows)
	} else {
		deleted, rowsToKeep, res = e.deleteBulkWithTriggers(tableEntry, dbCtx, colDefs, stmt, deletedRows)
	}
	if res != nil {
		return res
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	// Enforce FOREIGN KEY actions for the no-trigger batch path: the rows were
	// deleted in one pass, so their FK actions run after the batch (each row's
	// FK action runs at the row-delete point in the trigger path above — RESTRICT
	// must fire before AFTER triggers can repair the children, e_fkey-42.5).
	// On a RESTRICT/NO ACTION error the whole statement is rolled back.
	if e.ctx.ForeignKeys() && !e.hasTriggersForTable(tableEntry.Name) {
		for _, row := range rowsToKeep {
			if res := e.ctx.FkParentDelete(tableEntry, colDefs, row.rowMap(e)); res.Error != nil {
				e.ctx.RollbackPagerStatement(dbCtx.Pager, stmt)
				e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
				return res
			}
		}
	}
	return &Result{Changes: deleted}
}

// deleteBulkNoTriggers batch-deletes the matching rows in one pass, firing
// the preupdate hook per row, and returns the delete count plus the rows
// that survived (all of them, minus RAISE(IGNORE)-style skips).
func (e *DMLExecutor) deleteBulkNoTriggers(tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, deletedRows []*dmlRow) (int64, []*dmlRow, *Result) {
	deleted := int64(0)
	rowsToKeep := make([]*dmlRow, 0, len(deletedRows))
	// Each identity path consumes only its own key material: the WITHOUT
	// ROWID path matches OLD-PK keys from the declared values, the rowid path
	// matches rowid set membership — so build only the one in use (a point
	// DELETE on a rowid table paid a per-statement map and slice for nothing).
	withoutRowid := tableIsWithoutRowid(tableEntry.SQL)
	var declaredRows [][]interface{}
	var rowIDs map[int64]bool
	if withoutRowid {
		declaredRows = make([][]interface{}, 0, len(deletedRows))
		for _, row := range deletedRows {
			// The raw positional snapshot holds exactly the values
			// rowMapColumnValues extracted from the collected map (the unwrap of
			// its affinity-wrapped values), so the delete-identity keys and the
			// preupdate values are identical.
			declaredRows = append(declaredRows, row.values)
		}
	} else {
		rowIDs = make(map[int64]bool, len(deletedRows))
		for _, row := range deletedRows {
			rowIDs[row.rowID] = true
		}
	}
	// WITHOUT ROWID rows are PK-keyed index cells sharing synthetic
	// RowID 0: match OLD PK keys, not rowids. With no matching rows there is
	// nothing to delete — skip the b-tree pass entirely (DeleteCellsWhere
	// sweeps every leaf; running it for a no-match DELETE made the statement
	// O(table) even when the seek plan had already pinned an empty candidate
	// set, PERF_REPORT 3.4).
	if len(deletedRows) > 0 {
		if _, err := e.deleteRowsByIdentity(tableEntry, colDefs, rowIDs, declaredRows, nil); err != nil {
			return 0, nil, &Result{Error: err}
		}
		// Remove the deleted rows' index entries (SQLite OP_Delete deletes
		// from every index; stale entries pin overflow pages and stall
		// auto-vacuum truncation).
		if err := e.maintainIndexesOnDelete(tableEntry, colDefs, deletedRows); err != nil {
			return 0, nil, &Result{Error: err}
		}
	}
	for _, row := range deletedRows {
		if res := e.fireDeletePreupdate(tableEntry, dbCtx, colDefs, row); res != nil {
			return 0, nil, res
		}
		deleted++
		rowsToKeep = append(rowsToKeep, row)
	}
	return deleted, rowsToKeep, nil
}

// deleteBulkWithTriggers deletes the matching rows one at a time, firing the
// BEFORE triggers, the row delete, the preupdate hook, FK actions (RESTRICT
// must fire before AFTER triggers can repair the children, e_fkey-42.5) and
// the AFTER triggers per row.
func (e *DMLExecutor) deleteBulkWithTriggers(tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, stmt *pager.StmtJournal, deletedRows []*dmlRow) (int64, []*dmlRow, *Result) {
	deleted := int64(0)
	rowsToKeep := make([]*dmlRow, 0, len(deletedRows))
	for _, row := range deletedRows {
		rowID := row.rowID
		// One name-keyed map per row serves every consumer that demands
		// name-keyed access (triggers, FK, index maintenance); it carries the
		// same entries with the same wrapped values as the collected map did.
		rowMap := row.rowMap(e)
		if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, execquery.UnwrapRowMap(rowMap)); trigResult.Error != nil {
			if trigResult.Error == errRaiseIgnore {
				continue
			}
			return 0, nil, trigResult
		}
		if _, err := e.deleteRowCells(tableEntry, colDefs, rowID, row.values); err != nil {
			return 0, nil, &Result{Error: err}
		}
		if err := e.maintainIndexesOnDelete(tableEntry, colDefs, []*dmlRow{row}); err != nil {
			return 0, nil, &Result{Error: err}
		}
		// Fire the preupdate hook with the deleted row's values.
		if res := e.fireDeletePreupdate(tableEntry, dbCtx, colDefs, row); res != nil {
			return 0, nil, res
		}
		deleted++
		rowsToKeep = append(rowsToKeep, row)
		if res := e.finishBulkTriggerRow(tableEntry, dbCtx, colDefs, stmt, rowMap); res != nil {
			return 0, nil, res
		}
	}
	return deleted, rowsToKeep, nil
}

// finishBulkTriggerRow runs the post-delete FK action and AFTER triggers for
// one row (fk.c: the FK action subprogram sits between OP_Delete and the
// after-trigger program). RESTRICT must fire here: an AFTER trigger repairing
// the child rows (e_fkey-42.5: UPDATE child SET c = NULL) must not mask the
// RESTRICT error. On an FK failure the statement rolls back to the snapshot.
// Only a non-nil Error aborts the caller: fireTriggers legitimately returns a
// non-nil Result with a nil Error on success.
func (e *DMLExecutor) finishBulkTriggerRow(tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, stmt *pager.StmtJournal, row RowMap) *Result {
	if e.ctx.ForeignKeys() {
		if res := e.ctx.FkParentDelete(tableEntry, colDefs, row); res.Error != nil {
			e.ctx.RollbackPagerStatement(dbCtx.Pager, stmt)
			e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
			return res
		}
	}
	if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, execquery.UnwrapRowMap(row)); trigResult.Error != nil {
		return trigResult
	}
	return nil
}

// fireDeletePreupdate fires the preupdate hook with a deleted row's values
// (WITHOUT ROWID tables report the synthetic rowid 0 — SQLite uses the key
// columns instead). The positional snapshot holds exactly the values
// rowMapColumnValues extracted from the collected map.
func (e *DMLExecutor) fireDeletePreupdate(tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, row *dmlRow) *Result {
	rowID := row.rowID
	oldVals := row.values
	delRowID := rowID
	if tableIsWithoutRowid(tableEntry.SQL) {
		delRowID = 0
	}
	return e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "DELETE",
		DB:    e.schemaNameForPager(dbCtx.Pager),
		Table: tableEntry.Name,
		RowID: delRowID, RowID2: delRowID,
		RowidTable: !tableIsWithoutRowid(tableEntry.SQL),
		Old:        oldVals,
		New:        nil,
	})
}

// execDeleteReturning executes a DELETE ... RETURNING one row at a time so
// RETURNING subqueries observe the table with the current row removed.
func (e *DMLExecutor) execDeleteReturning(s *sql.DeleteStmt, tableEntry *schema.Entry, tree *btree.BTree, colDefs []sql.ColumnDef, deletedRows []*dmlRow) *Result {
	var returningRows [][]interface{}
	for _, row := range deletedRows {
		rowID := row.rowID
		rowMap := row.rowMap(e)
		if trigResult := e.fireBeforeDeleteTriggers(tableEntry.Name, execquery.UnwrapRowMap(rowMap)); trigResult.Error != nil {
			if trigResult.Error == errRaiseIgnore {
				continue
			}
			return trigResult
		}
		if _, err := e.deleteRowCells(tableEntry, colDefs, rowID, row.values); err != nil {
			return &Result{Error: err}
		}
		if err := e.maintainIndexesOnDelete(tableEntry, colDefs, []*dmlRow{row}); err != nil {
			return &Result{Error: err}
		}
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		values, err := e.evalReturningStrict(s.Returning, rowMap, colDefs, tableEntry.Name)
		if err != nil {
			return &Result{Error: err}
		}
		returningRows = append(returningRows, values)
		if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, execquery.UnwrapRowMap(rowMap)); trigResult.Error != nil {
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
