// Package exec implements query execution.
//
// This file holds the covering-index half of the index-driven single-table
// scan (SQLite's COVERING INDEX loop): when the driving index supplies every
// value the statement reads, the seek builds rows straight from the index
// entries instead of joining each entry's rowid back to the table b-tree.
// The rowid-join and positioning halves live in select_index_seek_exec.go.

package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// indexSeekCoversQuery reports whether the driving index supplies every
// value the statement reads, so the seek can build rows straight from the
// index entries (SQLite's COVERING INDEX: OP_IdxRowid + OP_Column over the
// index record) instead of joining each entry's rowid back to the table
// b-tree. The covered values are the index's stored key values — the same
// values the table record holds, since both were written with the column's
// affinity — plus the trailing rowid, which serves the rowid pseudo-column
// and an INTEGER PRIMARY KEY alias.
func (e *SelectEngine) indexSeekCoversQuery(s *sql.SelectStmt, colDefs []sql.ColumnDef, cols []string, needMaps bool) bool {
	if len(cols) == 0 || !colDefsCoverable(colDefs) {
		return false
	}
	if needMaps {
		// The map consumers read columns by name, so every column must be a
		// key column (the rowid alone cannot answer them).
		return e.indexCoversAllTableColsFor(colDefs, cols)
	}
	if hasStarColumn(s) {
		return e.indexCoversAllTableColsFor(colDefs, cols)
	}
	for ref := range collectSelectColumnRefs(s) {
		if !indexSeekColumnCovered(ref, colDefs, cols) {
			return false
		}
	}
	return true
}

// hasStarColumn reports whether the projection carries a bare `*`.
func hasStarColumn(s *sql.SelectStmt) bool {
	for i := range s.Columns {
		if ref, ok := s.Columns[i].Expr.(*sql.ColumnRef); ok && ref.Name == "*" && ref.Table == "" {
			return true
		}
	}
	return false
}

// indexSeekColumnCovered reports whether one referenced column name is
// answered by the index: a key column, or the rowid pseudo-column (blocked
// when a declared column shadows it), or the INTEGER PRIMARY KEY alias.
func indexSeekColumnCovered(ref string, colDefs []sql.ColumnDef, cols []string) bool {
	for i := range colDefs {
		if !strings.EqualFold(colDefs[i].Name, ref) {
			continue
		}
		if isIPKRowidAliasCol(colDefs[i]) {
			return true // the rowid fills it
		}
		for _, key := range cols {
			if coveredIndexColumn(key, colDefs[i].Name) {
				return true
			}
		}
		return false
	}
	if IsRowIDName(ref) {
		return !RowHasRowIDColumn(colDefs)
	}
	return false
}

// indexCoversAllTableColsFor reports whether every non-dropped column of the
// table is one of the index's key columns.
func (e *SelectEngine) indexCoversAllTableColsFor(colDefs []sql.ColumnDef, cols []string) bool {
	for i := range colDefs {
		if colDefs[i].Dropped {
			continue
		}
		if isIPKRowidAliasCol(colDefs[i]) {
			continue // the rowid fills it
		}
		if !anyCoveredIndexColumn(cols, colDefs[i].Name) {
			return false
		}
	}
	return true
}

// anyCoveredIndexColumn reports whether any index value column supplies
// colName (a bare column or its unary-plus key form).
func anyCoveredIndexColumn(cols []string, colName string) bool {
	for _, key := range cols {
		if coveredIndexColumn(key, colName) {
			return true
		}
	}
	return false
}

// colDefsCoverable reports whether the table's columns are a plain stored
// layout the covered decode can address positionally: no dropped columns
// (whose slots shift), no ALTER TABLE ADD COLUMN defaults (filled from the
// record's value count) and no generated columns (computed from the row).
func colDefsCoverable(colDefs []sql.ColumnDef) bool {
	for i := range colDefs {
		cd := &colDefs[i]
		if cd.Dropped || cd.Default != nil || cd.Generated != nil {
			return false
		}
	}
	return true
}

// indexSeekCoveredRows emits the seek's candidates from the index entries
// themselves (a covering index scan).
func (e *SelectEngine) indexSeekCoveredRows(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, run *indexSeekRun, cursor *btree.Cursor, needMaps bool, feed *simpleAggFeed) ([][]interface{}, []RowMap, bool) {
	var allRows [][]interface{}
	var allRowMaps []RowMap
	for {
		cell, inRange, ok := indexSeekCurrentCell(run, cursor)
		if !ok {
			return nil, nil, false
		}
		if !inRange {
			break
		}
		// sqlite_offset's sources: this entry's cell in the driving index
		// b-tree. For a WITHOUT ROWID table the driving "index" IS the
		// table b-tree (the PK b-tree holds the full row), so the same
		// offset serves both sources and every column is index-provided.
		off, offOK := cursor.PayloadOffset()
		if e.ctx.TableIsWithoutRowidEntry(tableEntry) && run.plan.index == "PRIMARY KEY" {
			e.ctx.SetScanRowOffsets(off, offOK, off, offOK, indexOffsetAllCols(colDefs))
		} else {
			e.ctx.SetScanRowOffsets(0, false, off, offOK, indexOffsetCols(run.cols, true))
		}
		rows, maps, ok := e.indexSeekCoveredOutput(s, colDefs, run.cols, cell, needMaps, feed, e.ctx.TableIsWithoutRowidEntry(tableEntry))
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
		// SQLITE_TEST's sqlite3_search_count: +1 per successful OP_Next
		// positioning (vdbe.c next_tail). A covering loop opens no table
		// cursor, so there is no deferred moveto to count. A unique
		// fully-bound equality scan has NO Next in C's plan.
		if !run.uniqueScan {
			e.ctx.IncSearchCount(1)
		}
	}
	return allRows, allRowMaps, true
}

// indexSeekCoveredOutput builds one row from an index entry: each column
// takes its key value (an INTEGER PRIMARY KEY alias takes the rowid), the
// statement's WHERE is evaluated on it, and the output is built exactly like
// the rowid-join path's. The entry's rowid is its record's trailing element
// (an index cell's Cell.RowID is not set — the key carries it).
func (e *SelectEngine) indexSeekCoveredOutput(s *sql.SelectStmt, colDefs []sql.ColumnDef, cols []string, cell *storage.Cell, needMaps bool, feed *simpleAggFeed, withoutRowid bool) ([][]interface{}, []RowMap, bool) {
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return nil, nil, false
	}
	rowid, ok := coveredRecordRowID(rec, len(cols), withoutRowid)
	if !ok {
		return nil, nil, false
	}
	values := e.seekRowScratchFor(len(colDefs))
	fillCoveredRecordValues(values, colDefs, cols, rec.Values)
	srow := e.seekSRowScratchFor()
	srow.Index = e.seekColIndexFor(colDefs)
	affinityCols := e.scanTableAffinityCols(s, colDefs, needMaps)
	e.fillSeekRowPhaseOne(values, len(colDefs), srow, colDefs, rowid,
		affinityWrapIndices(colDefs, affinityCols), ipkAliasIndices(colDefs), true)
	pass, err := e.RowPassesWhere(s.Where, srow, nil)
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

// coveredRecordRowID extracts the locator of one index record: a rowid table's
// final field, or the synthetic zero of a WITHOUT ROWID secondary index whose
// final field is a PK-tail value.
func coveredRecordRowID(rec *storage.Record, colCount int, withoutRowid bool) (int64, bool) {
	minFields := colCount
	if !withoutRowid {
		minFields++
	}
	if len(rec.Values) < minFields {
		return 0, false
	}
	if withoutRowid {
		return 0, true
	}
	rowid, ok := util.UnwrapColumnValue(rec.Values[len(rec.Values)-1]).(int64)
	return rowid, ok
}

// fillCoveredRecordValues maps each declared column onto its stored index
// field. A column absent from the key is left unset: the coverage gate proved
// no consumer reads it (except an IPK alias, filled from the rowid below).
func fillCoveredRecordValues(values []interface{}, colDefs []sql.ColumnDef, cols []string, stored []interface{}) {
	for i := range colDefs {
		if isIPKRowidAliasCol(colDefs[i]) {
			continue
		}
		pos := indexColPos(cols, colDefs[i].Name)
		if pos >= len(cols) {
			for j, key := range cols {
				if coveredIndexColumn(key, colDefs[i].Name) {
					pos = j
					break
				}
			}
		}
		if pos >= len(cols) || pos >= len(stored) {
			continue
		}
		values[i] = stored[pos]
	}
}

// indexSeekCurrentCell reads the cursor's current entry, reporting whether it
// is still inside the seek's range and whether the read succeeded. The caller
// owns advancing the cursor.
func indexSeekCurrentCell(run *indexSeekRun, cursor *btree.Cursor) (*storage.Cell, bool, bool) {
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		return nil, false, false
	}
	inRange, err := run.entryInRange(cell.Payload)
	if err != nil {
		return nil, false, false
	}
	if !inRange {
		return nil, false, true
	}
	return cell, true, true
}
