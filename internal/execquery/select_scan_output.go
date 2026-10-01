package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// This file owns the scan's per-row output construction: the bare-refs fast
// path, the retained positional-aggregate row clone, and their helpers.
// Split from select_scan.go for file-size hygiene.

// appendRowOutput builds the output for the current row. For SELECT * it copies
// values into the pre-allocated flat slice (fast path); otherwise it allocates a
// row via buildOutputRow. Row maps are accumulated when needed. In a JOIN, the
// scan produces only the first table's columns — output rows are rebuilt from
// the full joined row maps by execJoins afterwards, so skip the (potentially
// error-raising) per-row expression evaluation here to avoid evaluating
// expressions against a row missing the joined tables' columns.
func (st *scanState) appendRowOutput() error {
	if st.isSelectStar {
		if !st.aggConsumesRows {
			st.outRowStarts = append(st.outRowStarts, len(st.outValues))
			st.outValues = appendScanStarValues(st.outValues, st.colDefs, st.reuseSRow.Values, st.affinityCols != nil)
		}
	} else if st.bareOutIdx != nil && !st.hasJoins && !st.aggConsumesRows {
		// All-bare-refs SELECT: peel the reused StructRow's slots directly
		// (evalColumnRef's in-row hit returns the same slot value, and both
		// appendOutputExpr and the unwrap here peel the identical wrapper
		// chain — the fast row is byte-identical to buildOutputRow's).
		values := st.reuseSRow.Values
		row := make([]interface{}, len(st.bareOutIdx))
		for i, slot := range st.bareOutIdx {
			row[i] = unwrapCollatedValue(util.UnwrapColumnValue(values[slot]))
		}
		st.nonStarRows = append(st.nonStarRows, row)
	} else if !st.hasJoins && !st.aggConsumesRows {
		row, err := st.e.buildOutputRow(st.s.Columns, st.colDefs, st.reuseSRow)
		if err != nil {
			return err
		}
		st.nonStarRows = append(st.nonStarRows, row)
	}
	if st.posAgg {
		st.aggRows = append(st.aggRows, st.cloneReuseSRow())
		return nil
	}
	if st.needMaps {
		st.allRowMaps = append(st.allRowMaps, StructRowToMap(st.reuseSRow))
	}
	return nil
}

// cloneReuseSRow retains the current row as a StructRow clone: the value
// slice is copied out of the reused row buffer (the next fill REPLACES slot
// contents, never mutates them, but the backing array is shared), with blob
// payloads deep-copied under exactly the map path's retention discipline
// (rowMapValue). Value slices are carved out of arena chunks so retention
// costs one small allocation per row (the StructRow header).
func (st *scanState) cloneReuseSRow() *StructRow {
	n := len(st.reuseSRow.Values)
	if cap(st.aggArena)-len(st.aggArena) < n {
		st.aggArena = make([]interface{}, 0, 512*n)
	}
	start := len(st.aggArena)
	st.aggArena = append(st.aggArena, st.reuseSRow.Values...)
	vals := st.aggArena[start : start+n : start+n]
	// Blob payloads are deep-copied under exactly the map path's retention
	// discipline (rowMapValue); every other payload (scalars, fresh wrapper
	// pointers) is exclusive to this row and shared as-is.
	for i, v := range vals {
		switch t := v.(type) {
		case []byte:
			b := make([]byte, len(t))
			copy(b, t)
			vals[i] = b
		case *util.ColumnValue:
			if _, isBlob := t.Value.([]byte); isBlob {
				vals[i] = rowMapValue(v)
			}
		case *CollatedValue:
			if cv, ok := t.Value.(*util.ColumnValue); ok {
				if _, isBlob := cv.Value.([]byte); isBlob {
					vals[i] = rowMapValue(v)
				}
			}
		}
	}
	return &StructRow{Values: vals, Index: st.reuseSRow.Index, RowID: st.reuseSRow.RowID}
}

// bareOutputSlots lists the reused StructRow slot of every output column of
// an all-bare-refs SELECT, or nil when the fast output path does not apply.
// A column disqualifies the whole statement when it is not a plain
// unqualified, non-star, non-keyword column reference resolving to a
// non-generated stored slot (exact or case-variant), or when the statement
// declares any SELECT alias — an alias can shadow a later output column of
// the same name and change evalColumnRef's resolution order.
func bareOutputSlots(s *sql.SelectStmt, colDefs []sql.ColumnDef) []int {
	if len(s.Columns) == 0 || len(selectAliasMap(s)) > 0 {
		return nil
	}
	slots := make([]int, 0, len(s.Columns))
	for _, col := range s.Columns {
		ref, ok := unwrapParenExpr(col.Expr).(*sql.ColumnRef)
		if !ok || ref.Name == "*" || ref.Table != "" || groupByKeywordName(ref.Name) {
			return nil
		}
		slot, ok := storedSlotForName(ref.Name, colDefs)
		if !ok {
			return nil
		}
		slots = append(slots, slot)
	}
	return slots
}

// storedSlotForName resolves a column name to its stored (non-generated,
// non-dropped) colDefs slot, exact first, then case-insensitively
// (evalColumnRef's case-variant fallback resolves identically).
func storedSlotForName(name string, colDefs []sql.ColumnDef) (int, bool) {
	for i := range colDefs {
		if colDefs[i].Name == name && colDefs[i].Generated == nil && !colDefs[i].Dropped {
			return i, true
		}
	}
	for i := range colDefs {
		if strings.EqualFold(colDefs[i].Name, name) && colDefs[i].Generated == nil && !colDefs[i].Dropped {
			return i, true
		}
	}
	return 0, false
}

// buildResultRows assembles the final row slice: SELECT * rows from the flat
// buffer first, then any individually-allocated (non-star) rows.
