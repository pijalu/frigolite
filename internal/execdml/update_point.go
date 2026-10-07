package execdml

// Rowid-pinned single-row UPDATE fast path (the OLTP workhorse
// "UPDATE t SET c=c+1 WHERE id=?"). The generic pipeline collects changes
// through the seek plan, then re-walks them through the bulk apply machinery
// (dedupe maps, rowid sets, per-step re-lookups, and — when the new cell size
// differs — an O(table) DeleteCellsWhere sweep). When the statement is the
// plain point shape, one seek reads the row, the SET expressions rewrite the
// value slots positionally, the record encodes once into the executor's
// reusable buffer, and the write takes the loc==0 in-place overwrite or a
// seek delete + re-insert. Every gate mirrors runUpdatePipeline's routing:
// anything the fast path does not handle exactly returns handled=false and
// the statement runs the unmodified generic pipeline.

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// pointUpdateEligible reports the statement shape the fast path rewrites
// exactly (the clause-level shape plus the target-table gates).
func (e *DMLExecutor) pointUpdateEligible(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	return pointUpdateStatementShapeOK(s) &&
		e.pointUpdateTargetOK(s, tableEntry, colDefs)
}

// withoutRowidCached is tableIsWithoutRowid over one memoized slot: the
// point-UPDATE path resolves the flag for the same table 3-4 times per
// statement (the shape gate, the conflict gate's layout skip, the
// constrained-def walk, the CHECK walk) and each resolve scanned the CREATE
// TABLE tail (strip + LastIndex + fold-contains). The guard is the ciCache
// pattern: schema fingerprint + entry identity — any DDL replaces the entry
// or moves the fingerprint, so a stale flag cannot survive.
func (e *DMLExecutor) withoutRowidCached(tableEntry *schema.Entry) bool {
	fp := e.schemaFingerprint()
	if e.wrFlagEntry == tableEntry && e.wrFlagFp == fp {
		return e.wrFlagVal
	}
	v := tableIsWithoutRowid(tableEntry.SQL)
	e.wrFlagEntry, e.wrFlagFp, e.wrFlagVal = tableEntry, fp, v
	return v
}

// pointUpdateStatementShapeOK reports the clause-level shape: a plain
// statement (no OR clause, no RETURNING, no ORDER BY/LIMIT) over the target
// table only (no FROM), with the single SET list form.
func pointUpdateStatementShapeOK(s *sql.UpdateStmt) bool {
	if s.OnConflict != "" || s.HasReturning || len(s.OrderBy) > 0 || s.Limit != nil {
		return false
	}
	if s.From.Name != "" || s.From.Subquery != nil || len(s.FromJoins) > 0 {
		return false
	}
	return len(s.SetParenColumns) == 0
}

// pointUpdateTargetOK reports the target-table gates: an ordinary rowid
// table without triggers, FK enforcement, per-constraint ON CONFLICT
// clauses, or generated columns, whose SET targets are ordinary columns.
func (e *DMLExecutor) pointUpdateTargetOK(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) bool {
	if e.withoutRowidCached(tableEntry) {
		return false
	}
	if e.hasTriggersForTable(tableEntry.Name) || e.ctx.ForeignKeys() {
		return false
	}
	if hasColumnConflictClauses(colDefs, tableEntry, e) {
		return false
	}
	for i := range colDefs {
		if colDefs[i].Generated != nil {
			return false
		}
	}
	return e.pointUpdateSetTargetsOK(s, colDefs)
}

// pointUpdateSetTargetsOK reports whether every SET target resolves to an
// ordinary declared column (a rowid/IPK target re-keys the row —
// rowidMoveConflict, delete+reinsert at a new rowid — which the fast path
// does not handle), and the pinned rowid equality is the WHOLE WHERE clause
// (the seek's hit then matches by construction and the clause needs no
// evaluation).
func (e *DMLExecutor) pointUpdateSetTargetsOK(s *sql.UpdateStmt, colDefs []sql.ColumnDef) bool {
	colIndex := e.columnIndexFor(colDefs)
	for _, a := range s.Assignments {
		ci, ok := colIndex[strings.ToLower(a.Column)]
		if !ok || ci < 0 || ci >= len(colDefs) || isIPKRowidAliasCol(colDefs[ci]) {
			return false
		}
	}
	return andTermCount(s.Where) == 1
}

// applyPointUpdate applies the rowid-pinned single-row UPDATE fast path:
// collect the pinned row, run the same constraint and conflict gates as
// runPlainUpdate over the single change, write it. handled=false means the
// statement does not fit the fast path's exact shape and must run the
// generic pipeline (collect → preCheck → dispatch → apply). One b-tree
// wrapper serves both the row fetch and the write (the seek tree stays
// open — the write primitives save/restore cursors themselves).
func (e *DMLExecutor) applyPointUpdate(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) (*Result, bool) {
	if !e.pointUpdateEligible(s, tableEntry, colDefs) {
		return nil, false
	}
	scanName := updateScanName(s, tableEntry.Name)
	plan := e.planDMLSeek(tableEntry, colDefs, s.Where, scanName, e.currentDMLCtx)
	// plan.index != nil is an indexed-seek plan: candidate narrowing only,
	// the WHERE must still be evaluated per candidate — not this shape.
	if plan == nil || plan.empty || plan.index != nil {
		return nil, false
	}
	// The point write path's cached wrapper (insertWriteTree pattern): one
	// b-tree serves the row fetch and the write across statements (the
	// write primitives save/restore cursors themselves; the statement's
	// leaked internal seek cursors are released on return).
	pg := e.dmlPager(tableEntry.Name)
	tree := e.pointWriteTree(&e.updTree, &e.updTreeKey, pg, tableEntry.Name, tableEntry.RootPage)
	defer tree.ReleaseIdleCursors()
	ch, matched, res, pos := e.collectPointUpdateRow(tree, s, tableEntry, colDefs, plan.rowid)
	if res != nil {
		return res, true
	}
	if !matched {
		// The pinned rowid has no row: the generic pipeline collects zero
		// changes and the statement is a no-op.
		return e.emptyResultFor(), true
	}
	changes := []updateChange{ch}
	// NOT NULL/CHECK pre-check — preCheckUpdate exactly (the OR-clause and
	// per-column ON CONFLICT dispositions that could drop or substitute the
	// change are excluded by the gates, so the single change passes through).
	changes, pres := e.preCheckUpdate(s, tableEntry, colDefs, changes)
	if pres.Error != nil {
		return pres, true
	}
	if len(changes) != 1 {
		return nil, false
	}
	// UNIQUE/PK conflict gate — checkUpdateConflicts exactly (the P1
	// change-detection gate skips its scans when nothing constrained moved).
	if res := e.checkUpdateConflicts(tableEntry, colDefs, changes); res.Error != nil {
		return res, true
	}
	// FOREIGN KEY parent actions run in runPlainUpdate when enforcement is
	// on; the gates exclude that case.
	if res := e.deleteUpdateIndexEntriesFor(tableEntry, colDefs, changes); res != nil {
		return &Result{Error: res}, true
	}
	if wres := e.writePointUpdateRow(tableEntry.Name, tree, tableEntry.RootPage, changes[0], tableEntry, colDefs, pos); wres.Error != nil {
		return wres, true
	}
	// A growth split may have moved the root: re-key the cached wrapper and
	// persist the new root (persistTreeRootPage parity with the insert path).
	e.pointWriteTreeSync(&e.updTreeKey, pg, tableEntry.Name, tableEntry.RootPage, tree)
	done := e.emptyResultFor()
	done.Changes = 1
	return done, true
}

// cellPos is the leaf position a seek established for one pinned row
// (Cursor.PageNum/CellIdx), so the write can re-address the row without a
// second descent. The btree write primitives re-validate it and fall back
// to a full seek when it went stale.
type cellPos struct {
	leaf uint32
	idx  int
}

// pointUpdateValueSlots is updateChangeValueSlots over the executor's pooled
// slot pair (two slices per point UPDATE instead of two allocations),
// copying out of the pooled decode buffer. The returned slices are consumed
// entirely within the calling statement — the preupdate event copies them —
// so the next statement may reuse the storage. The values slice is cleared
// first: slots beyond the stored record keep nil unless an added-column
// DEFAULT fills them (updateChangeValueSlots' fresh make() semantics).
// maxIdx is max(recCount, len(colDefs)) — exactly the map iteration's
// maximum (colIndex holds every column's 0-based slot plus the rowid
// pseudo-entry -1) without walking the map per statement.
func (e *DMLExecutor) pointUpdateValueSlots(recCount int, colDefs []sql.ColumnDef) ([]interface{}, []interface{}) {
	maxIdx := recCount
	if len(colDefs) > maxIdx {
		maxIdx = len(colDefs)
	}
	if cap(e.ptValues) < maxIdx {
		e.ptValues = make([]interface{}, maxIdx)
	}
	values := e.ptValues[:maxIdx]
	clear(values)
	copy(values, e.ptDecode[:recCount])
	if cap(e.ptOldValues) < recCount {
		e.ptOldValues = make([]interface{}, recCount)
	}
	oldValues := e.ptOldValues[:recCount]
	copy(oldValues, e.ptDecode[:recCount])
	return values, oldValues
}

// pointUpdateRowMapFromSlots builds the SET-evaluation row map over the
// executor's pooled map (one allocation ever, then clear + refill per
// statement — buildRowMap allocated a fresh map per row, 8.3% of the
// point-UPDATE profile), fed from the pooled value slots instead of a
// decoded Record. The fill mirrors execquery's buildRowMap exactly for the
// common full-record shapes: dropped-column records and short (pre-ALTER)
// records fall back to the shared builder; extra record values project as
// c<N>. The pooled map is safe here and only here: the single change
// consumes the map within the statement AND the SET expressions contain no
// subquery (a correlated subquery's evaluation RETAINS the row as the
// engine's outer-row scope, which a later statement's clear() would corrupt
// — those statements fall back too).
func (e *DMLExecutor) pointUpdateRowMapFromSlots(values []interface{}, colDefs []sql.ColumnDef, recCount int, rowID int64, assignments []sql.Assignment) RowMap {
	if recCount < len(colDefs) || subqueryInAssignments(assignments) {
		return e.ctx.BuildRowMap(&storage.Record{Values: values[:recCount]}, colDefs, rowID)
	}
	for i := range colDefs {
		if colDefs[i].Dropped {
			return e.ctx.BuildRowMap(&storage.Record{Values: values[:recCount]}, colDefs, rowID)
		}
	}
	row := e.ptRowMap
	if row == nil {
		row = make(RowMap, len(colDefs)+4)
		e.ptRowMap = row
	} else {
		clear(row)
	}
	e.fillPointUpdateRowMapFromSlots(row, values, colDefs, recCount, rowID)
	return row
}

// fillPointUpdateRowMapFromSlots refills the pooled map from the value
// slots (the buildRowMap fill: affinity/collation-wrapped declared columns,
// extra record values projected as c<N>, rowid pseudo-aliases). The caller
// guarantees recCount == len(colDefs) here — shorter records and dropped
// columns take the shared builder.
func (e *DMLExecutor) fillPointUpdateRowMapFromSlots(row RowMap, values []interface{}, colDefs []sql.ColumnDef, recCount int, rowID int64) {
	for i := range colDefs {
		cd := colDefs[i]
		if v := values[i]; v == nil && isIPKRowidAliasCol(cd) {
			// SQLite stores NULL in the rowid-alias slot; the value is the
			// rowid (substituted at read time, buildRowMap parity).
			row[cd.Name] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		} else {
			cv := &util.ColumnValue{Value: v, Affinity: util.Affinity(cd.Type)}
			if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
				row[cd.Name] = &execquery.CollatedValue{Value: cv, Collation: strings.ToUpper(coll)}
			} else {
				row[cd.Name] = cv
			}
		}
	}
	for i := len(colDefs); i < recCount; i++ {
		row[fmt.Sprintf("c%d", i)] = values[i]
	}
	if !execquery.RowHasRowIDColumn(colDefs) {
		rowidCV := &util.ColumnValue{Value: rowID, Affinity: 'I'}
		row["rowid"] = rowidCV
		row["_rowid_"] = rowidCV
		row["oid"] = rowidCV
	}
}

// subqueryInAssignments reports whether any SET expression contains a
// Subquery/ExistsExpr node (the pooled-row-map gate).
func subqueryInAssignments(assigns []sql.Assignment) bool {
	for _, a := range assigns {
		if exprHasSubqueryNode(a.Value) {
			return true
		}
	}
	return false
}

// exprHasSubqueryNode walks e for a Subquery/ExistsExpr node. The single-
// child kinds dispatch here; the multi-child containers (function calls,
// CASE, BETWEEN, IN lists, row values, DISTINCT pairs) recurse through
// exprHasSubqueryMulti so each walker stays under the complexity gate.
func exprHasSubqueryNode(e sql.Expr) bool {
	switch e.(type) {
	case *sql.Subquery, *sql.ExistsExpr:
		return true
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		return exprHasSubqueryPair(e)
	case *sql.FuncCall, *sql.CaseExpr, *sql.Between, *sql.InList, *sql.RowValue:
		return exprHasSubqueryMulti(e)
	default:
		return exprHasSubquerySingle(e)
	}
}

// exprHasSubqueryPair walks the two-child expression nodes.
func exprHasSubqueryPair(e sql.Expr) bool {
	var left, right sql.Expr
	switch v := e.(type) {
	case *sql.BinaryOp:
		left, right = v.Left, v.Right
	case *sql.IsDistinctFrom:
		left, right = v.Left, v.Right
	case *sql.IsNotDistinctFrom:
		left, right = v.Left, v.Right
	}
	return exprHasSubqueryNode(left) || exprHasSubqueryNode(right)
}

// exprHasSubquerySingle walks the single-child expression nodes.
func exprHasSubquerySingle(e sql.Expr) bool {
	switch v := e.(type) {
	case *sql.UnaryOp:
		return exprHasSubqueryNode(v.Operand)
	case *sql.ParenExpr:
		return exprHasSubqueryNode(v.Expr)
	case *sql.CastExpr:
		return exprHasSubqueryNode(v.Operand)
	case *sql.IsNull:
		return exprHasSubqueryNode(v.Operand)
	case *sql.IsNotNull:
		return exprHasSubqueryNode(v.Operand)
	case *sql.IsTrue:
		return exprHasSubqueryNode(v.Operand)
	case *sql.IsFalse:
		return exprHasSubqueryNode(v.Operand)
	}
	return false
}

// exprHasSubqueryMulti walks the multi-child expression containers for a
// Subquery/ExistsExpr node (exprHasSubqueryNode's tail dispatch).
func exprHasSubqueryMulti(e sql.Expr) bool {
	switch v := e.(type) {
	case *sql.FuncCall:
		return exprListHasSubquery(v.Args) || exprHasSubqueryNode(v.Filter)
	case *sql.CaseExpr:
		return caseExprHasSubquery(v)
	case *sql.Between:
		return exprHasSubqueryNode(v.Operand) || exprHasSubqueryNode(v.Low) || exprHasSubqueryNode(v.High)
	case *sql.InList:
		return exprListHasSubquery(append([]sql.Expr{v.Operand}, v.List...))
	case *sql.RowValue:
		return exprListHasSubquery(v.Values)
	}
	return false
}

// caseExprHasSubquery walks a CASE expression's operand, WHEN arms and ELSE.
func caseExprHasSubquery(v *sql.CaseExpr) bool {
	if exprHasSubqueryNode(v.Operand) || exprHasSubqueryNode(v.Else) {
		return true
	}
	for i := range v.Whens {
		if exprHasSubqueryNode(v.Whens[i].When) || exprHasSubqueryNode(v.Whens[i].Then) {
			return true
		}
	}
	return false
}

// exprListHasSubquery reports whether any list expression holds a
// Subquery/ExistsExpr node.
func exprListHasSubquery(list []sql.Expr) bool {
	for _, item := range list {
		if exprHasSubqueryNode(item) {
			return true
		}
	}
	return false
}

// collectPointUpdateRow reads the pinned row by rowid and builds its change:
// the raw value slots with defaults applied, the SET expressions applied
// against a positional row (the scan loop's evaluation model), generated
// columns recomputed. matched=false reports the rowid is absent; res non-nil
// is a real statement error. A fetch anomaly returns matched=false with a
// nil res and the caller must fall back to the generic pipeline. The
// seeked row's leaf position comes back in pos for the write.
func (e *DMLExecutor) collectPointUpdateRow(tree *btree.BTree, s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, rowID int64) (ch updateChange, matched bool, res *Result, pos cellPos) {
	// SQLITE_TEST interrupt countdown: one op per row examined
	// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count).
	if err := e.ctx.CheckProgress(); err != nil {
		return updateChange{}, false, &Result{Error: err}, pos
	}
	// The very next act is an explicit rowid seek, which re-descends from
	// the root: skip OpenCursor's leftmost-leaf descent.
	cursor, err := tree.OpenCursorAtRoot()
	if err != nil {
		return updateChange{}, false, nil, pos // anomaly: generic pipeline
	}
	found, serr := cursor.SeekToRowID(rowID)
	if serr != nil {
		return updateChange{}, false, nil, pos // anomaly: generic pipeline
	}
	if !found {
		return updateChange{}, false, e.emptyResultFor(), pos
	}
	pos = cellPos{leaf: cursor.PageNum(), idx: cursor.CellIdx()}
	// ReadCellData (the delete path's fetch): payload + rowid straight off
	// the cached leaf page — no Cell struct, no overflow re-boxing between
	// the page bytes and the decode.
	payload, realRowID, rerr := cursor.ReadCellData()
	if rerr != nil {
		return updateChange{}, false, nil, pos // anomaly: generic pipeline
	}
	// Decode straight into the executor's pooled slot buffer
	// (DecodeRecordValuesInto): no intermediate Record struct, no fresh
	// values slice per row. The gates (pointUpdateTargetOK) exclude WITHOUT
	// ROWID tables, so the record arrives in declared order — the former
	// RemapWRRecordToDeclared call was a structurally-guaranteed no-op here
	// (its PK-first permutation only applies to WITHOUT ROWID layouts; it
	// re-parsed the CREATE TABLE text per row to decide that). A malformed
	// record falls back to the generic pipeline, whose decode reports the
	// error exactly as before.
	recCount, derr := e.decodePointUpdateRecord(payload, colDefs)
	if derr != nil {
		return updateChange{}, false, nil, pos // anomaly: generic pipeline
	}

	// The pooled slot pair is safe here and only here: the single change is
	// fully consumed within this statement (the generic pipeline's changes
	// outlive the collect loop and keep fresh allocations).
	values, oldValues := e.pointUpdateValueSlots(recCount, colDefs)
	// Rows written before ALTER TABLE ADD COLUMN read their added-column
	// DEFAULTs (buildUpdateChange parity).
	e.applyUpdateColumnDefaults(values, colDefs, recCount)
	e.applyUpdateColumnDefaults(oldValues, colDefs, recCount)

	// SET evaluation: the typed fast lane rewrites the common arithmetic/
	// literal SET shape directly over the value slots (no row map, no boxed
	// evaluation); every other shape keeps the row-map path — the exact
	// evaluation row seekUpdateChangesMaps uses for single-candidate
	// collects (the positional plan's fixed per-statement cost only
	// amortizes from three candidates up, so one pinned row keeps the map).
	if e.applyTypedPointUpdateSet(s, colDefs, values, realRowID) {
		if gerr := e.recomputeUpdateGenerated(colDefs, values); gerr != nil {
			return updateChange{}, false, &Result{Error: gerr}, pos
		}
		return updateChange{rowID: realRowID, values: values, oldValues: oldValues}, true, nil, pos
	}
	row := e.pointUpdateRowMapFromSlots(values, colDefs, recCount, realRowID, s.Assignments)
	newRowID, aerr := e.applyUpdateAssignments(s, row, e.columnIndexFor(colDefs), colDefs, values)
	if aerr != nil {
		return updateChange{}, false, &Result{Error: aerr}, pos
	}
	if newRowID != nil {
		return updateChange{}, false, nil, pos // re-key: generic pipeline
	}
	if gerr := e.recomputeUpdateGenerated(colDefs, values); gerr != nil {
		return updateChange{}, false, &Result{Error: gerr}, pos
	}
	return updateChange{rowID: realRowID, values: values, oldValues: oldValues}, true, nil, pos
}

// decodePointUpdateRecord decodes the seeked cell's record into the pooled
// ptDecode buffer and returns the record's stored-column count. The buffer
// keeps its backing storage across statements; undecoded slots are cleared
// first, so a short record's trailing slots read as NULL until
// applyUpdateColumnDefaults fills them. error reports a malformed record
// header (the generic pipeline's fallback re-reads and reports it exactly).
func (e *DMLExecutor) decodePointUpdateRecord(payload []byte, colDefs []sql.ColumnDef) (int, error) {
	size := len(colDefs)
	if size < 8 {
		size = 8
	}
	if cap(e.ptDecode) < size {
		e.ptDecode = make([]interface{}, size)
	}
	target := e.ptDecode[:size]
	clear(target)
	count, err := storage.DecodeRecordValuesInto(payload, target, nil)
	if err != nil {
		return 0, err
	}
	if count > len(target) {
		// The record stores more values than the declared column list
		// (pre-ALTER columns projected as c<N>): grow once and re-decode —
		// the rare shape pays the second parse, the common one never runs.
		e.ptDecode = make([]interface{}, count)
		count, err = storage.DecodeRecordValuesInto(payload, e.ptDecode, nil)
		if err != nil {
			return 0, err
		}
	}
	return count, nil
}

// writePointUpdateRow writes one same-rowid change: the in-place overwrite
// when the new cell is the same size (btree.OverwriteCellByRowIDAt through
// the collect seek's position — verified there and re-seeked on staleness),
// a seek delete + re-insert otherwise. Index maintenance, the rowid-cache
// bump/invalidate and the preupdate hook fire exactly as applyUpdateChanges'
// single-change bulk run does.
func (e *DMLExecutor) writePointUpdateRow(tableName string, tree *btree.BTree, rootPage uint32, ch updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef, pos cellPos) *Result {
	record, err := e.appendEncodedRecord(ch.values)
	if err != nil {
		return &Result{Error: err}
	}
	cellData, cerr := e.appendEncodedCell(ch.rowID, record)
	if cerr != nil {
		return &Result{Error: cerr}
	}
	done, oerr := tree.OverwriteCellByRowIDAt(ch.rowID, cellData, pos.leaf, pos.idx)
	if oerr != nil {
		return &Result{Error: oerr}
	}
	if !done {
		// Size changed or a guard declined: dropCell + insertCell (sqlite3
		// sqlite3BtreeInsert's non-fast loc==0 branch), with the seek delete
		// instead of the bulk sweep.
		if _, derr := tree.DeleteCellByRowID(ch.rowID); derr != nil {
			return &Result{Error: derr}
		}
		if icerr := tree.InsertCell(&storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   ch.rowID,
			Payload: record,
		}); icerr != nil {
			return &Result{Error: icerr}
		}
	}
	// The NEW row's entries into every index the change touches (the insert
	// phase; the OLD entries were removed before the write).
	if ierr := e.writeUpdateIndexEntriesFor(tableEntry, colDefs, ch.oldValues, ch.rowID, ch.values, ch.rowID); ierr != nil {
		return &Result{Error: ierr}
	}
	// applyUpdateChanges invalidates the rowid cache after the re-insert loop
	// (SQLite recomputes the rowid counter after any DELETE/UPDATE) — and the
	// point path's write below is followed by that same invalidate, so the
	// bump the bulk path performs is skipped here: a BumpRowIDCache
	// immediately followed by InvalidateRowIDCache is two map writes where
	// the invalidate alone has the same effect.
	if res := e.fireUpdatePreupdateEntry(tableEntry, ch); res != nil {
		return res
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableName), rootPage)
	return e.emptyResultFor()
}

// appendEncodedCell encodes a table-leaf cell image (rowid + record payload)
// into the executor's reusable cell buffer. Like appendEncodedRecord, the
// bytes are consumed synchronously by the btree write paths (copied into
// pages or split redistributions within the call), so the buffer can be
// reused by the next statement.
func (e *DMLExecutor) appendEncodedCell(rowID int64, record []byte) ([]byte, error) {
	c := storage.Cell{Type: storage.CellTableLeaf, RowID: rowID, Payload: record}
	n := storage.CellWireLen(&c)
	if cap(e.cellBuf) < n {
		e.cellBuf = make([]byte, n+64)
	}
	buf := e.cellBuf[:n]
	storage.EncodeCellInto(&c, buf)
	return buf, nil
}

// appendEncodedRecord encodes values into the executor's reusable record
// buffer. The btree write paths copy the payload bytes into pages (or
// overflow chains) synchronously and never retain the slice, so the buffer
// can be reused by the next statement.
func (e *DMLExecutor) appendEncodedRecord(values []interface{}) ([]byte, error) {
	buf, err := storage.AppendEncodeRecord(e.encBuf[:0], values)
	if err != nil {
		return nil, err
	}
	e.encBuf = buf
	return buf, nil
}
