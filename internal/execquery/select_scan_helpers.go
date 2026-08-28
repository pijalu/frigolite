package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func lookupRowMapValue(m RowMap, col string) interface{} {
	if m == nil {
		return nil
	}
	if v, ok := m[col]; ok {
		return v
	}
	// Try unqualified key (row maps may store "a" instead of "t1.a").
	for k, v := range m {
		if strings.HasSuffix(strings.ToLower(k), "."+strings.ToLower(col)) {
			return v
		}
	}
	return nil
}

// selectNeedsRowMaps returns true if the query requires per-row RowMap
// allocations for expression evaluation, sorting, filtering, or combining.
func SelectNeedsRowMaps(e *SelectEngine, s *sql.SelectStmt, tableName string) bool {
	// RowMaps are only needed for operations that require looking up values
	// by name in a map: JOINs evaluate expressions across row maps, ORDER BY
	// and DISTINCT need map-based comparison, UNIONS combine results, aggregates
	// group rows by map, and schema tables need filtering by name.
	// A simple WHERE clause without the above works fine with the StructRow's
	// index-based lookup and doesn't need per-row map allocation.
	if len(s.Joins) > 0 {
		return true
	}
	if len(s.OrderBy) > 0 {
		return true
	}
	if s.Distinct {
		return true
	}
	if s.Union != nil {
		return true
	}
	if IsSchemaTable(tableName) {
		return true
	}
	if e.hasAggregates(s.Columns) || e.hasSubqueryWithCorrelatedAgg(s.Columns) {
		return true
	}
	if e.selectHasWindowFuncs(s.Columns) {
		return true
	}
	if len(s.GroupBy) > 0 || s.Having != nil {
		return true
	}
	// WHERE clauses with subqueries (EXISTS, scalar subqueries) need row maps
	// because the subquery evaluation passes the row as outerRow for correlated
	// references, and StructRow's lazy decode may not have all columns available.
	if s.Where != nil && exprHasSubquery(s.Where) {
		return true
	}
	return false
}

// parseRecordSerialTypes parses a b-tree record payload header, returning the
// serial types and the byte offset where the data section begins.
func parseRecordSerialTypes(payload []byte) ([]uint64, int) {
	var stackSerialTypes [16]uint64
	serialTypes := stackSerialTypes[:0]
	pos := 0
	hdrSize, n := util.GetVarint(payload[pos:])
	pos += n
	hdrEnd := int(hdrSize)
	for pos < hdrEnd {
		st, n2 := util.GetVarint(payload[pos:])
		pos += n2
		serialTypes = append(serialTypes, st)
	}
	return serialTypes, pos
}

// appendScanStarValues appends the active (non-dropped) column values of a
// decoded SELECT * row to the flat output slice. When affinity is active, the
// ColumnValue/CollatedValue wrappers are unwrapped so internal comparison
// metadata never leaks into the output.
func appendScanStarValues(outValues []interface{}, colDefs []sql.ColumnDef, values []interface{}, affinity bool) []interface{} {
	if affinity {
		for i, cd := range colDefs {
			if cd.Dropped {
				continue
			}
			// (fillStructRowFromTypes wraps a column that has both an
			// affinity and a declared collation as CollatedValue around
			// a ColumnValue; UnwrapColumnValue alone would leave the
			// CollatedValue pointer visible.)
			outValues = append(outValues, util.UnwrapColumnValue(unwrapCollatedValue(values[i])))
		}
		return outValues
	}
	// No affinity wrappers — values are already raw
	for i, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		outValues = append(outValues, values[i])
	}
	return outValues
}

// scanLazyDecodeIndices computes the WHERE-referenced column indices (phase 1)
// and their complement (phase 2) for lazy two-phase decoding.
func scanLazyDecodeIndices(colDefs []sql.ColumnDef, colIndex map[string]int, affinityCols map[string]bool) (map[int]bool, map[int]bool) {
	whereDecodeIndices := make(map[int]bool, len(affinityCols))
	for name := range affinityCols {
		if idx, ok := colIndex[name]; ok {
			whereDecodeIndices[idx] = true
			continue
		}
		// Case-insensitive fallback: the WHERE reference may use a
		// different case than the declared column name (SQLite column
		// names are case-insensitive).
		for k, idx := range colIndex {
			if strings.EqualFold(k, name) && idx >= 0 {
				whereDecodeIndices[idx] = true
				break
			}
		}
	}
	// Pre-compute the complement set for phase 2 decoding (avoids per-row map allocation)
	remainingDecodeIndices := make(map[int]bool, len(colDefs)-len(whereDecodeIndices))
	for i := range colDefs {
		if !whereDecodeIndices[i] {
			remainingDecodeIndices[i] = true
		}
	}
	return whereDecodeIndices, remainingDecodeIndices
}

func (e *SelectEngine) rowPassesWhere(where sql.Expr, row Row, cursor *btree.Cursor) (bool, error) {
	if where == nil {
		return true, nil
	}
	// Fast path: simple comparison ColumnRef OP Literal
	if bop, ok := where.(*sql.BinaryOp); ok && row != nil {
		if result, ok := e.fastEvalComparison(bop, row); ok {
			return result, nil
		}
	}
	match, err := e.ctx.EvalBool(where, row)
	if err != nil {
		return false, err
	}
	return match, nil
}

// fastEvalColRef resolves a column reference against a row for the fast
// comparison path, honoring the qualifier: a qualified ref (t1.a) looks up
// the qualified key first, falling back to the unqualified key only when the
// qualifier matches the row's table or the row has no qualified keys.
func fastEvalColRef(cr *sql.ColumnRef, row Row) (interface{}, bool) {
	if cr.Table != "" {
		// Strip a schema prefix (main.t4.a) and try the qualified key.
		tableQual := cr.Table
		if dot := strings.Index(tableQual, "."); dot >= 0 {
			tableQual = tableQual[dot+1:]
		}
		if val, ok := row.Get(tableQual + "." + cr.Name); ok {
			return val, true
		}
		if val, ok := row.Get(cr.Table + "." + cr.Name); ok {
			return val, true
		}
		// No qualified key: fall back to the unqualified name only when the
		// row is NOT a join result (join maps store qualified keys).
		if !rowHasQualifiedKeys(row) {
			if val, ok := row.Get(cr.Name); ok {
				return val, true
			}
		}
		return nil, false
	}
	val, ok := row.Get(cr.Name)
	return val, ok
}

// evalLiteralFast evaluates a literal expression (NumericLit, StringLit, etc.)
// without error handling overhead.
func (e *SelectEngine) evalLiteralFast(expr sql.Expr) (interface{}, bool) {
	switch v := expr.(type) {
	case *sql.NumericLit:
		if v.Cached() != nil {
			return v.Cached(), true
		}
		// Fall through to full eval for uncached (complex) numbers
		return nil, false
	case *sql.StringLit:
		return v.Value, true
	case *sql.ParenExpr:
		return e.evalLiteralFast(v.Expr)
	default:
		return nil, false
	}
}

// applyComparisonOp maps a comparison result to a boolean for the given operator.
func applyComparisonOp(op string, cmp int) bool {
	switch op {
	case ">":
		return cmp > 0
	case "<":
		return cmp < 0
	case ">=":
		return cmp >= 0
	case "<=":
		return cmp <= 0
	case "=":
		return cmp == 0
	case "<>", "!=":
		return cmp != 0
	default:
		return false
	}
}

// applyIntComparison evaluates a comparison operator directly on two int64
// values, avoiding the overhead of CompareValuesCollate for the common case
// of integer column vs integer literal comparisons.
func applyIntComparison(op string, a, b int64) bool {
	switch op {
	case ">":
		return a > b
	case "<":
		return a < b
	case ">=":
		return a >= b
	case "<=":
		return a <= b
	case "=":
		return a == b
	case "<>", "!=":
		return a != b
	default:
		return false
	}
}

// isSchemaTable returns true if the given table name is the sqlite_master/sqlite_schema table.
func IsSchemaTable(name string) bool {
	upper := strings.ToUpper(name)
	return upper == "SQLITE_MASTER" || upper == "SQLITE_SCHEMA" ||
		upper == "MAIN.SQLITE_MASTER" || upper == "MAIN.SQLITE_SCHEMA"
}

// isSQLiteSequence reports whether name refers to the sqlite_sequence system
// table (case-insensitive, with or without a main. prefix). Unqualified
// references always resolve to the MAIN schema's sqlite_sequence, never the
// temp schema's synthetic fallback.
func IsSQLiteSequence(name string) bool {
	upper := strings.ToUpper(name)
	return upper == "SQLITE_SEQUENCE" || upper == "MAIN.SQLITE_SEQUENCE"
}

// isHiddenSystemTable returns true if the table name is an internal system table
// that should not appear in sqlite_master queries. SQLite exposes sqlite_stat1
// and sqlite_stat4 as ordinary entries in sqlite_schema (they can be read and
// queried like any table), so they are NOT hidden here.
func isHiddenSystemTable(name string) bool {
	return false
}

// rowHasRowIDColumn reports whether the column definitions include a column
// named rowid, _rowid_, or oid. SQLite lets tables declare such columns; the
// column then shadows the pseudo-rowid alias for unqualified name resolution.
func RowHasRowIDColumn(colDefs []sql.ColumnDef) bool {
	for _, cd := range colDefs {
		n := strings.ToLower(cd.Name)
		if n == "rowid" || n == "_rowid_" || n == "oid" {
			return true
		}
	}
	return false
}

// unwrapRowMap returns a copy of row with all affinity ColumnValue wrappers
// replaced by their raw values. Trigger bodies and RETURNING projections must
// receive raw values: the wrappers are only for WHERE-clause affinity
// comparison and would otherwise leak into trigger logs and result sets.
func UnwrapRowMap(row RowMap) RowMap {
	out := make(RowMap, len(row))
	for k, v := range row {
		out[k] = util.UnwrapColumnValue(v)
	}
	return out
}

// fillStructRowRemainingFromTypes decodes remaining columns using pre-parsed
// serial types. This is the second phase of lazy decoding: after WHERE
// evaluation passes, decode the columns that were skipped in the first phase.
// Only columns in affinityCols get ColumnValue wrappers (these are the WHERE-referenced
// columns — already decoded in phase 1). Remaining columns are left raw.
func (e *SelectEngine) fillStructRowRemainingFromTypes(sr *StructRow, payload []byte, dataStart int, colDefs []sql.ColumnDef, serialTypes []uint64, indices map[int]bool) {
	storage.DecodeRecordValuesFromTypes(payload, dataStart, sr.Values, serialTypes, indices)
	// Same missing-column default handling as fillStructRowFromTypes: rows
	// written before ALTER TABLE ADD COLUMN need the added column's DEFAULT.
	e.applyColumnDefaults(sr.Values, colDefs, len(serialTypes))
}

// applyColumnDefaults fills in DEFAULT values for columns that are absent
// from the stored record (e.g., rows written before ALTER TABLE ADD COLUMN).
// Only columns beyond the record's value count get the default: a column
// present in the record — even as NULL — keeps its stored value. The default
// expression is evaluated with an empty row (it cannot reference other
// columns) and the column's declared affinity is applied, matching SQLite's
// ALTER TABLE ADD COLUMN semantics (e.g. TEXT column with DEFAULT -123.0
// yields the text value "-123.0").
func (e *SelectEngine) applyColumnDefaults(values []interface{}, colDefs []sql.ColumnDef, recordValueCount int) {
	for i := recordValueCount; i < len(colDefs); i++ {
		cd := &colDefs[i]
		if cd.Default != nil && !cd.Dropped {
			if dv, err := e.ctx.EvalExpr(cd.Default, nil); err == nil {
				values[i] = util.ApplyColumnAffinity(dv, cd.Type)
			}
		}
	}
}

// StructRowToMap converts a StructRow to a RowMap, deep-copying mutable
// values (ColumnValue wrappers, []byte) so the map does not share the
// reused StructRow value slots that the next decoded row overwrites.
func StructRowToMap(sr *StructRow) RowMap {
	m := make(RowMap, len(sr.Index)+1)
	m["rowid"] = &util.ColumnValue{Value: sr.RowID, Affinity: 'I'}
	for name, idx := range sr.Index {
		if idx < len(sr.Values) {
			m[name] = cloneRowValue(sr.Values[idx])
		}
	}
	return m
}

// cloneRowValue deep-copies a mutable value so RowMaps do not share the
// reused StructRow value slots (which are overwritten by the next decoded
// row). Immutable values (int64, float64, nil) are returned as-is; pointers
// (ColumnValue, []byte, string) are copied.
func cloneRowValue(v interface{}) interface{} {
	switch t := v.(type) {
	case *util.ColumnValue:
		cp := *t
		switch inner := t.Value.(type) {
		case []byte:
			b := make([]byte, len(inner))
			copy(b, inner)
			cp.Value = b
		case string:
			cp.Value = inner
		}
		return &cp
	case []byte:
		b := make([]byte, len(t))
		copy(b, t)
		return b
	default:
		return v
	}
}

// qualifiedStarColNames resolves a qualified star (t.* / alias.*) against a
// joined row map. It returns the table's column name+value pairs in column
// order, resolving each value via the qualified key (alias.col) first, then
// the short key (col) when the qualified key is absent.
