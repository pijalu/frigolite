// Package exec implements query execution.
//
// This file holds the execution half of the index-driven single-table scan:
// opening the driving index's b-tree under the KeyInfo its keys were written
// with, positioning the cursor on the loop's seek range, iterating the
// matching entries in stored key order, and joining each entry's trailing
// rowid back to its table row. The planning half lives in
// select_index_seek.go.

package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// indexSeekRun is one resolved index seek: the b-tree comparator the index's
// keys were ordered under, the probes derived from the plan, and the
// stored-order start/stop the iteration walks between.
type indexSeekRun struct {
	plan *indexSeekPlan
	ki   *btree.KeyInfo
	// desc is the range column's stored sort order: a DESC key reverses the
	// value-space bounds' meaning in stored order, so the plan's lower bound
	// becomes the stop and its upper bound the start.
	desc        bool
	cols        []string                // the index's key columns (declaration order)
	eqProbe     *btree.UnpackedIndexKey // equality prefix (nil when nEq == 0)
	startProbe  *btree.UnpackedIndexKey // nil = start at the prefix's first entry
	startStrict bool
	stopProbe   *btree.UnpackedIndexKey // nil = no stored-order stop
	stopStrict  bool
}

// selectIndexSeekRows resolves an index-driven SELECT through the driving
// index's b-tree. handled=false keeps the regular scan: any gate miss, seek
// anomaly or decode error (the scan re-evaluates and surfaces it
// identically).
func (e *SelectEngine) selectIndexSeekRows(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, tree *btree.BTree, feed *simpleAggFeed) (allRows [][]interface{}, allRowMaps []RowMap, handled bool) {
	plan := e.selectIndexSeekPlanFor(s, tableEntry, colDefs)
	if plan == nil {
		return nil, nil, false
	}
	if plan.empty {
		return [][]interface{}{}, nil, true
	}
	run, idxTree, ok := e.openIndexSeekRun(plan, tableEntry, dbCtx, colDefs)
	if !ok {
		return nil, nil, false
	}
	defer idxTree.Close() // the seek's index tree is statement-local
	cursor, ok := run.position(idxTree)
	if !ok {
		return [][]interface{}{}, nil, true
	}
	needMaps := e.selectNeedsRowMapsCached(s, tableEntry.Name)
	if e.indexSeekCoversQuery(s, colDefs, run.cols, needMaps) {
		return e.indexSeekCoveredRows(s, colDefs, run, cursor, needMaps, feed)
	}
	if e.indexSeekReversed(s) {
		return e.indexSeekReverseRows(s, tableEntry, colDefs, tree, run, cursor, needMaps, feed)
	}
	return e.indexSeekForwardRows(s, tableEntry, colDefs, tree, run, cursor, needMaps, feed)
}

// indexSeekReversed reports whether the loop runs backwards: PRAGMA
// reverse_unordered_selects reverses the scan direction of a top-level
// SELECT without ORDER BY (the same condition the table scan's shouldReverse
// applies).
func (e *SelectEngine) indexSeekReversed(s *sql.SelectStmt) bool {
	return e.ctx.ReverseUnordered() && len(s.OrderBy) == 0 && e.selectDepth == 1 && len(s.Joins) == 0
}

// openIndexSeekRun opens the driving index's b-tree at its schema root page
// (an index walk must use the index's own root, not the table's tracked
// root) and precomputes the probes.
func (e *SelectEngine) openIndexSeekRun(plan *indexSeekPlan, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef) (*indexSeekRun, *btree.BTree, bool) {
	if dbCtx == nil || dbCtx.Pager == nil {
		return nil, nil, false
	}
	entry := e.schemaIndexEntry(indexSchemaName(plan.index))
	if entry == nil || entry.RootPage == 0 {
		return nil, nil, false
	}
	ki := e.indexSeekKeyInfo(tableEntry.Name, plan.index, colDefs)
	if ki == nil {
		return nil, nil, false
	}
	tree := btree.NewBTree(dbCtx.Pager, entry.RootPage, false)
	switch tree.RootPageType() {
	case storage.PageTypeLeafIndex, storage.PageTypeInteriorIndex:
	default:
		// Not an index b-tree (a shadow table's nominal root, a stale schema
		// entry): the regular scan owns the statement.
		tree.Close()
		return nil, nil, false
	}
	tree.SetIndexKeyInfo(ki, e.indexSeekCollationLookup())
	run := &indexSeekRun{plan: plan, ki: ki, cols: e.indexColumns(plan.index), desc: e.indexSeekRangeDesc(tableEntry.Name, plan)}
	if len(plan.eq) > 0 {
		run.eqProbe = btree.NewUnpackedIndexKey(ki, plan.eq)
	}
	run.resolveBounds()
	return run, tree, true
}

// indexSeekKeyInfo builds the KeyInfo of the driving index: one collation and
// sort flag per key column, exactly what the insert side ordered the tree
// with, so the seek's probes compare in the tree's stored order.
func (e *SelectEngine) indexSeekKeyInfo(tableName, token string, colDefs []sql.ColumnDef) *btree.KeyInfo {
	cols := e.indexColumns(token)
	if len(cols) == 0 {
		return nil
	}
	descs, known := e.indexColumnDescFlags(tableName, token)
	if !known || len(descs) < len(cols) {
		return nil
	}
	colls := make([]string, len(cols))
	flags := make([]byte, len(cols))
	for i, col := range cols {
		colls[i] = strings.ToUpper(e.indexColumnCollation(tableName, token, col))
		if descs[i] {
			flags[i] = btree.KeyInfoOrderDesc
		}
	}
	return btree.NewKeyInfo(len(cols), colls, flags)
}

// indexSeekCollationLookup resolves a key collation name for the b-tree
// comparator through the engine's collation registry (built-ins and custom
// sequences alike), so a probe orders exactly like the stored keys.
func (e *SelectEngine) indexSeekCollationLookup() btree.CollationLookup {
	return func(name string) (util.CollationFunc, bool) {
		if name == "" {
			return nil, false
		}
		return func(a, b string) int { return e.ctx.CompareValuesCollate(a, b, name) }, true
	}
}

// indexSeekRangeDesc reports the stored sort order of the plan's range column
// (the first key column without an equality bound). Equality-only plans have
// no range column: false (the flag is then unused).
func (e *SelectEngine) indexSeekRangeDesc(tableName string, plan *indexSeekPlan) bool {
	if plan.lo == nil && plan.hi == nil {
		return false
	}
	descs, known := e.indexColumnDescFlags(tableName, plan.index)
	if !known || len(descs) <= len(plan.eq) {
		return false
	}
	return descs[len(plan.eq)]
}

// resolveBounds derives the stored-order start and stop from the plan's
// value-space bounds and the range column's sort order. A DESC key reverses
// the two: the plan's lower bound becomes the stored-order stop and its upper
// bound the start.
func (r *indexSeekRun) resolveBounds() {
	startBound, stopBound := r.plan.lo, r.plan.hi
	if r.desc {
		startBound, stopBound = r.plan.hi, r.plan.lo
	}
	if startBound != nil {
		r.startProbe = r.boundProbe(startBound.val)
		r.startStrict = startBound.op == ">" || startBound.op == "<"
	}
	if stopBound != nil {
		r.stopProbe = r.boundProbe(stopBound.val)
		r.stopStrict = stopBound.op == "<=" || stopBound.op == ">="
	}
}

// boundProbe builds the equality prefix extended by one range bound value.
func (r *indexSeekRun) boundProbe(val interface{}) *btree.UnpackedIndexKey {
	vals := make([]interface{}, 0, len(r.plan.eq)+1)
	vals = append(vals, r.plan.eq...)
	vals = append(vals, val)
	return btree.NewUnpackedIndexKey(r.ki, vals)
}

// position opens a cursor on the seek's first candidate entry. ok=false with
// a nil error means the tree holds no candidate at all (an empty result, not
// a fallback).
func (r *indexSeekRun) position(tree *btree.BTree) (*btree.Cursor, bool) {
	if r.startProbe != nil {
		return r.seekLowerBound(tree, r.startProbe, r.startStrict)
	}
	if r.eqProbe != nil {
		cursor, err := tree.OpenCursorAtRoot()
		if err != nil {
			return nil, false
		}
		found, err := cursor.SeekIndexKey(r.eqProbe)
		if err != nil || !found {
			return nil, false
		}
		return cursor, true
	}
	// No bound at all: the range starts at the index's first entry (a
	// prefix-less upper-bounded range, e.g. "b < ?").
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, false
	}
	return cursor, true
}

// seekLowerBound positions a fresh cursor at the first entry whose probed
// key fields are >= the probe (or > it when strict).
func (r *indexSeekRun) seekLowerBound(tree *btree.BTree, probe *btree.UnpackedIndexKey, strict bool) (*btree.Cursor, bool) {
	cursor, err := tree.OpenCursorAtRoot()
	if err != nil {
		return nil, false
	}
	found, err := cursor.SeekIndexLowerBound(probe, strict)
	if err != nil || !found {
		return nil, false
	}
	return cursor, true
}

// entryInRange reports whether the entry at the cursor's position still lies
// inside the seek's range: the equality prefix must still match, and the
// stored-order stop bound (if any) must not have been passed. Values that
// sort before a bound but fail the predicate (NULL against a range bound) are
// left to the statement's own WHERE re-evaluation — they never END the scan,
// because the entries after them can still match.
func (r *indexSeekRun) entryInRange(payload []byte) (bool, error) {
	if r.eqProbe != nil {
		cmp, err := btree.IndexRecordCompare(payload, r.eqProbe)
		if err != nil {
			return false, err
		}
		if cmp != 0 {
			return false, nil
		}
	}
	if r.stopProbe == nil {
		return true, nil
	}
	cmp, err := btree.IndexRecordCompare(payload, r.stopProbe)
	if err != nil {
		return false, err
	}
	if r.stopStrict {
		return cmp <= 0, nil
	}
	return cmp < 0, nil
}

// indexSeekForwardRows emits the seek's candidates in the index's stored key
// order (SQLite's index loop order).
func (e *SelectEngine) indexSeekForwardRows(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, tree *btree.BTree, run *indexSeekRun, cursor *btree.Cursor, needMaps bool, feed *simpleAggFeed) ([][]interface{}, []RowMap, bool) {
	var allRows [][]interface{}
	var allRowMaps []RowMap
	for {
		rowid, inRange, ok := indexSeekCurrentRowid(run, cursor)
		if !ok {
			return nil, nil, false
		}
		if !inRange {
			break
		}
		rows, maps, ok := e.indexSeekRowOutput(s, tree, colDefs, rowid, needMaps, feed)
		if !ok {
			return nil, nil, false
		}
		allRows = append(allRows, rows...)
		allRowMaps = append(allRowMaps, maps...)
		more, err := cursor.Next()
		if err != nil {
			return nil, nil, false
		}
		if !more {
			break
		}
	}
	return allRows, allRowMaps, true
}

// indexSeekReverseRows materializes the seek's candidates and emits them in
// reverse stored order: PRAGMA reverse_unordered_selects reverses the loop's
// scan direction, so the whole range is walked backwards.
func (e *SelectEngine) indexSeekReverseRows(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, tree *btree.BTree, run *indexSeekRun, cursor *btree.Cursor, needMaps bool, feed *simpleAggFeed) ([][]interface{}, []RowMap, bool) {
	var rowids []int64
	for {
		rowid, inRange, ok := indexSeekCurrentRowid(run, cursor)
		if !ok {
			return nil, nil, false
		}
		if !inRange {
			break
		}
		rowids = append(rowids, rowid)
		more, err := cursor.Next()
		if err != nil {
			return nil, nil, false
		}
		if !more {
			break
		}
	}
	var allRows [][]interface{}
	var allRowMaps []RowMap
	for i := len(rowids) - 1; i >= 0; i-- {
		rows, maps, ok := e.indexSeekRowOutput(s, tree, colDefs, rowids[i], needMaps, feed)
		if !ok {
			return nil, nil, false
		}
		allRows = append(allRows, rows...)
		allRowMaps = append(allRowMaps, maps...)
	}
	return allRows, allRowMaps, true
}

// indexSeekCurrentRowid reads the cursor's current entry: the trailing rowid,
// whether the entry is still inside the seek's range (false ends the walk),
// and whether the read succeeded at all (false answers with the regular
// scan). The caller owns advancing the cursor.
func indexSeekCurrentRowid(run *indexSeekRun, cursor *btree.Cursor) (rowid int64, inRange, ok bool) {
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return 0, false, false
	}
	inRange, err = run.entryInRange(cell.Payload)
	if err != nil {
		return 0, false, false
	}
	if !inRange {
		return 0, false, true
	}
	rowid, ok = indexEntryRowid(cell)
	if !ok {
		return 0, false, false
	}
	return rowid, true, true
}

// indexSeekRowOutput joins one index entry's rowid back to its table row (a
// b-tree seek), evaluates the statement's full WHERE clause on it, and builds
// the row's output (or steps the statement's aggregate feed).
func (e *SelectEngine) indexSeekRowOutput(s *sql.SelectStmt, tree *btree.BTree, colDefs []sql.ColumnDef, rowid int64, needMaps bool, feed *simpleAggFeed) ([][]interface{}, []RowMap, bool) {
	cursor, srow, found, ok := e.fetchSeekStructRow(s, tree, rowid, colDefs, needMaps, false, feed != nil)
	if !ok {
		return nil, nil, false
	}
	if !found {
		// The index entry names a row the table b-tree no longer holds: the
		// regular scan's table walk would not see it either.
		return nil, nil, true
	}
	pass, err := e.RowPassesWhere(s.Where, srow, cursor)
	if err != nil {
		return nil, nil, false
	}
	if !pass {
		return nil, nil, true
	}
	if feed != nil {
		if err := feed.step(srow.Values, srow.RowID); err != nil {
			return nil, nil, false
		}
		return nil, nil, true
	}
	rows, maps, ok := e.seekRowOutput(s, colDefs, srow, true, needMaps)
	return rows, maps, ok
}
