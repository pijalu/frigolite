// Package exec implements query execution.
//
// This file owns the MIN/MAX bare-column source machinery: SQLite evaluates
// an aggregate query's bare columns against the input row that produced the
// last min/max aggregate, so the aggregate paths reorder each group (or the
// whole input) to move that row to the front before evaluating outputs.
package execquery

import (
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// minMaxAggregate describes a single-argument MIN or MAX aggregate function
// call. It is used to resolve the source row for bare columns in aggregate
// queries: SQLite evaluates bare columns against the input row that produced
// the last min/max aggregate in the result set.
type minMaxAggregate struct {
	name   string // "MIN" or "MAX" (uppercased)
	arg    sql.Expr
	filter sql.Expr // FILTER (WHERE ...) clause; nil when absent
}

// lastMinMaxAggregate returns the last (rightmost) single-argument MIN/MAX
// aggregate function call found in the SELECT columns, scanning left to right
// and descending into nested expressions. Returns nil when the result set has
// no min/max aggregate.
func (e *SelectEngine) lastMinMaxAggregate(columns []sql.SelectColumn) *minMaxAggregate {
	var last, lastFiltered *minMaxAggregate
	for _, col := range columns {
		if mm := lastMinMaxInExpr(col.Expr, e.ctx.Functions()); mm != nil {
			if mm.filter == nil {
				last = mm
			} else {
				lastFiltered = mm
			}
		}
	}
	// An UNFILTERED min/max takes priority: its row is the source row for
	// bare columns even when a FILTERED min/max appears later in the list
	// (filter1-7.1: max(a), max(a) FILTER (WHERE b<12345), b — the bare b
	// comes from the unfiltered max row, not the filtered aggregate's
	// empty-row fallback). The last filtered aggregate is the fallback only
	// when no unfiltered one exists (filter1-3.3).
	if last != nil {
		return last
	}
	return lastFiltered
}

// minMaxSourceRow evaluates a single-argument MIN/MAX aggregate's argument
// over the given rows and returns the index of the row that produced the
// extreme value (the first row on ties). When every argument is NULL the
// aggregate yields NULL and bare columns take the last row, matching SQLite.
// Returns -1 when rows is empty.
func (e *SelectEngine) minMaxSourceRow(mm *minMaxAggregate, rowMaps []RowMap) int {
	if len(rowMaps) == 0 {
		return -1
	}
	bestIdx := -1
	var bestVal interface{}
	for i, row := range rowMaps {
		// FILTER (WHERE ...) excludes rows from the aggregate entirely:
		// a filtered-out row can never be the row that produced the
		// extreme value (filter1-3.3).
		if !e.minMaxRowPassesFilter(mm, row) {
			continue
		}
		val := e.minMaxRowValue(mm, row)
		if val == nil {
			continue
		}
		if bestIdx < 0 || minMaxBeats(mm.name, val, bestVal) {
			bestIdx = i
			bestVal = val
		}
	}
	if bestIdx < 0 {
		// No row produced a value. With a FILTER excluding every row the
		// aggregate yields NULL and bare columns take the group's FIRST row
		// (filter1-3.3: max(b) FILTER (WHERE c='x') -> c of the first row);
		// the unfiltered all-NULL case keeps the last-row behavior.
		if mm.filter != nil {
			return 0
		}
		return len(rowMaps) - 1
	}
	return bestIdx
}

// minMaxRowPassesFilter evaluates the aggregate's FILTER clause.
func (e *SelectEngine) minMaxRowPassesFilter(mm *minMaxAggregate, row RowMap) bool {
	if mm.filter == nil {
		return true
	}
	fv, ferr := e.ctx.EvalExpr(mm.filter, row)
	return ferr == nil && fv != nil && execexpr.ToBool(fv)
}

// minMaxRowValue evaluates the aggregate argument for one row, unwrapped
// (nil on error or NULL).
func (e *SelectEngine) minMaxRowValue(mm *minMaxAggregate, row RowMap) interface{} {
	val, err := e.ctx.EvalExpr(mm.arg, row)
	if err != nil || val == nil {
		return nil
	}
	return util.UnwrapColumnValue(val)
}

// minMaxBeats reports whether val replaces bestVal for a MIN/MAX aggregate.
func minMaxBeats(name string, val, bestVal interface{}) bool {
	cmp := util.CompareValues(val, bestVal)
	return (name == "MIN" && cmp < 0) || (name == "MAX" && cmp > 0)
}

// lastMinMaxAggregateFor resolves the statement's bare-column source aggregate
// (the last min/max in the SELECT columns, else the HAVING clause), or nil.
func (e *SelectEngine) lastMinMaxAggregateFor(s *sql.SelectStmt) *minMaxAggregate {
	mm := e.lastMinMaxAggregate(s.Columns)
	if mm == nil && s.Having != nil {
		mm = lastMinMaxInExpr(s.Having, e.ctx.Functions())
	}
	return mm
}

// reorderRowsForMinMaxSource is reorderRowsForMinMax with the statement's
// min/max aggregate pre-resolved (per-group callers hoist the resolution out
// of their loop).
func (e *SelectEngine) reorderRowsForMinMaxSource(mm *minMaxAggregate, rowMaps []RowMap) []RowMap {
	if mm == nil || len(rowMaps) <= 1 {
		return rowMaps
	}
	idx := e.minMaxSourceRow(mm, rowMaps)
	if idx <= 0 {
		return rowMaps
	}
	rows := make([]RowMap, len(rowMaps))
	copy(rows, rowMaps)
	rows[0], rows[idx] = rows[idx], rows[0]
	return rows
}

// reorderRowsForMinMax moves the row that produced the last min/max aggregate
// to the front of rowMaps so bare columns in an aggregate query evaluate from
// the correct source row. The min/max aggregate is searched in the SELECT
// columns and the HAVING clause (a bare output column paired with
// "HAVING max(x) ..." takes the row that produced the max, matching SQLite).
// Returns the reordered slice (a copy is not made unless reordering is needed).
func (e *SelectEngine) reorderRowsForMinMax(s *sql.SelectStmt, rowMaps []RowMap) []RowMap {
	return e.reorderRowsForMinMaxSource(e.lastMinMaxAggregateFor(s), rowMaps)
}

// aggregateName returns the name of the first aggregate function found in the
