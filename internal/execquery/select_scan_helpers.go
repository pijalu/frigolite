package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
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
	if len(s.Joins) > 0 || len(s.OrderBy) > 0 || s.Distinct || s.Union != nil {
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
	return s.Where != nil && exprHasSubquery(s.Where)
}

// parseRecordSerialTypesInto parses a b-tree record payload header, appending
// the serial types to the caller-owned buffer and returning it (grown in
// place) plus the byte offset where the data section begins. The header size
// must lie within the payload (vdbe.c OP_Column's op_column_corrupt check) —
// a header extending past the fetched bytes is a corrupt record and must
// error, not spin appending serial types. The result is consumed within the
// row's decode; no callee retains it, so one buffer serves a whole scan.
func parseRecordSerialTypesInto(payload []byte, serialTypes []uint64) ([]uint64, int, error) {
	pos := 0
	hdrSize, n := util.GetVarint(payload[pos:])
	pos += n
	hdrEnd := int(hdrSize)
	if hdrEnd < pos || hdrEnd > len(payload) {
		return nil, 0, fmt.Errorf("database disk image is malformed")
	}
	for pos < hdrEnd {
		st, n2 := util.GetVarint(payload[pos:])
		pos += n2
		serialTypes = append(serialTypes, st)
	}
	return serialTypes, pos, nil
}

// parseRecordSerialTypesOffsetsInto parses the header exactly like
// parseRecordSerialTypesInto and additionally records each serial entry's
// value span relative to the data section: entry i's value bytes are
// payload[dataStart+offs[i] : dataStart+offs[i+1]] (offs has len(types)+1
// entries, the last marking the end of the final value). This is the typed
// aggregate lane's O(1) slot address book: a call reads its slot's span
// instead of re-walking the preceding serial types' lengths per row.
//
// Corruption parity with resolveSlot (the walk this replaces):
//   - a corrupt serial type (10/11) has no length: its slot and every later
//     slot record -1, and the consumer's resolve reports not-ok — the same
//     "leave the slot NULL" outcome the generic walk's SerialTypeLength
//     error produces;
//   - a truncated body is NOT a parse error: offsets are prefix sums, so a
//     short body pushes the end past len(payload) and the consumer's bounds
//     check reports it per slot (identical to the walk's pos+n check).
//
// offs returns nil when the offsets cannot be represented (a payload over
// 2GB cannot index in int32); the caller falls back to the generic walk.
func parseRecordSerialTypesOffsetsInto(payload []byte, serialTypes []uint64, offs []int32) ([]uint64, []int32, int, error) {
	if len(payload) > 1<<31-1 {
		st, dataStart, err := parseRecordSerialTypesInto(payload, serialTypes)
		return st, nil, dataStart, err
	}
	if len(payload) == 0 {
		// GetVarint's empty-input contract returns (0, 1); hdrEnd 0 < pos 1
		// is the generic parse's malformed verdict.
		return nil, nil, 0, fmt.Errorf("database disk image is malformed")
	}
	pos := 0
	var hdrSize uint64
	var n int
	if b := payload[0]; b < 0x80 {
		hdrSize, n = uint64(b), 1
	} else {
		hdrSize, n = util.GetVarint(payload)
	}
	pos += n
	hdrEnd := int(hdrSize)
	if hdrEnd < pos || hdrEnd > len(payload) {
		return nil, nil, 0, fmt.Errorf("database disk image is malformed")
	}
	rel := int32(0)
	for pos < hdrEnd {
		var st uint64
		if b := payload[pos]; b < 0x80 {
			st, n = uint64(b), 1
		} else {
			st, n = util.GetVarint(payload[pos:])
		}
		pos += n
		serialTypes = append(serialTypes, st)
		offs = append(offs, rel)
		if rel < 0 {
			// Poisoned by a corrupt type upstream: this slot's span stays
			// unreachable (start -1), like the generic walk's error.
			continue
		}
		switch {
		case st == storage.SerialNull || st == storage.SerialZero || st == storage.SerialOne:
			// zero-length value (0, 8, 9)
		case st <= storage.SerialInt32: // 1..4 → len st
			rel += int32(st)
		case st == storage.SerialInt48:
			rel += 6
		case st <= storage.SerialFloat: // int64 (6) and float (7) → 8
			rel += 8
		case st < storage.SerialMin:
			// 10/11 are reserved: no length exists, so this slot's END and
			// every later span become unreachable (the generic walk errors
			// resolveSlot the same way).
			rel = -1
		default:
			rel += int32((st - 12) >> 1)
		}
	}
	offs = append(offs, rel)
	return serialTypes, offs, pos, nil
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

// boolDecodeSet materializes an index set as a by-position bool slice sized n
// (the storage decode loop's colIndices[i] membership test becomes a slice
// index; keys outside [0,n) keep the map lookup's not-set answer).
func boolDecodeSet(m map[int]bool, n int) []bool {
	cols := make([]bool, n)
	for i := range m {
		if i >= 0 && i < n {
			cols[i] = true
		}
	}
	return cols
}

func (e *SelectEngine) rowPassesWhere(where sql.Expr, row Row, cursor *btree.Cursor) (bool, error) {
	if where == nil {
		return true, nil
	}
	// Fast paths: simple comparison / literal-bounded BETWEEN / AND chains
	// thereof (see fastEvalWhere). A fall-through evaluates generically.
	if row != nil {
		if result, ok := e.fastEvalWhere(where, row); ok {
			return result, nil
		}
	}
	match, err := e.ctx.EvalBool(where, row)
	if err != nil {
		return false, err
	}
	return match, nil
}

// fastEvalWhere attempts the row-loop WHERE fast paths without the general
// expression evaluator: a simple comparison (ColumnRef OP Literal), a
// BETWEEN with a column operand and literal bounds, or an AND chain whose
// every leaf is one of those. ok=false falls through to the general
// evaluator (the fast paths are strict subsets of its semantics).
func (e *SelectEngine) fastEvalWhere(where sql.Expr, row Row) (bool, bool) {
	switch v := where.(type) {
	case *sql.BinaryOp:
		if v.Operator == "AND" {
			return e.fastEvalAndChain(v, row)
		}
		return e.fastEvalComparison(v, row)
	case *sql.Between:
		return e.fastEvalBetween(v, row)
	}
	return false, false
}

// fastEvalAndChain evaluates an AND of ANDs whose leaves are all fast
// comparisons or BETWEENs. ok=false reports a leaf the fast paths decline.
func (e *SelectEngine) fastEvalAndChain(bop *sql.BinaryOp, row Row) (bool, bool) {
	left, ok := e.fastEvalWhereLeaf(bop.Left, row)
	if !ok {
		return false, false
	}
	right, ok := e.fastEvalWhereLeaf(bop.Right, row)
	if !ok {
		return false, false
	}
	return left && right, true
}

// fastEvalWhereLeaf evaluates one AND-chain leaf: a nested AND chain or a
// fast comparison/BETWEEN.
func (e *SelectEngine) fastEvalWhereLeaf(expr sql.Expr, row Row) (bool, bool) {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		if v.Operator == "AND" {
			return e.fastEvalAndChain(v, row)
		}
		return e.fastEvalComparison(v, row)
	case *sql.Between:
		return e.fastEvalBetween(v, row)
	}
	return false, false
}

// fastEvalBetween attempts to evaluate a BETWEEN whose operand is a plain
// column reference and whose bounds are literals without the general
// expression evaluator. Returns (result, true) on the fast path, or
// (false, false) to fall through (non-column operand, NULL operand, or a
// bound the literal fast path cannot evaluate).
func (e *SelectEngine) fastEvalBetween(bt *sql.Between, row Row) (bool, bool) {
	ref, ok := bt.Operand.(*sql.ColumnRef)
	if !ok {
		return false, false
	}
	colVal, exists := fastEvalColRef(ref, row)
	if !exists || execexpr.IsSQLNull(colVal) {
		return false, false // let the slow path apply NULL semantics
	}
	low, ok := e.evalLiteralFast(bt.Low)
	if !ok || low == nil {
		return false, false
	}
	high, ok := e.evalLiteralFast(bt.High)
	if !ok || high == nil {
		return false, false
	}
	inRange := e.compareColumnToLiteral(">=", colVal, low, false) &&
		e.compareColumnToLiteral("<=", colVal, high, false)
	if bt.Negated {
		return !inRange, true
	}
	return inRange, true
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
	// Exact-length EqualFold comparisons: this runs per looked-up table on
	// the DML floor, where a ToUpper was a heap hit per statement.
	switch len(name) {
	case 13:
		return strings.EqualFold(name, "sqlite_master") || strings.EqualFold(name, "sqlite_schema")
	case 18:
		return strings.EqualFold(name, "main.sqlite_master") || strings.EqualFold(name, "main.sqlite_schema")
	}
	return false
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
// ipkIdx holds the precomputed INTEGER PRIMARY KEY rowid-alias column indices
// (from scanState.ipkFillIdx) so the refill below skips the per-row
// isIPKRowidAliasCol walk over all column definitions.
func (e *SelectEngine) fillStructRowRemainingFromTypes(sr *StructRow, payload []byte, dataStart int, colDefs []sql.ColumnDef, serialTypes []uint64, indices []bool, ipkIdx []int) {
	storage.DecodeRecordValuesFromTypesCols(payload, dataStart, sr.Values, serialTypes, indices)
	// Same missing-column default handling as fillStructRowFromTypes: rows
	// written before ALTER TABLE ADD COLUMN need the added column's DEFAULT.
	e.applyColumnDefaults(sr.Values, colDefs, len(serialTypes))
	// Re-apply the INTEGER PRIMARY KEY rowid-alias substitution AFTER the
	// second decode: phase 1 (the scan's affinity plan) fills the alias
	// column with the rowid, but the remaining-columns decode here re-reads
	// the stored NULL from the record and would overwrite it ("SELECT * WHERE
	// c>1" showed NULL for the alias column — filtered-scan class,
	// regexp1/indexexpr1/tableopts/whereA).
	for _, i := range ipkIdx {
		if sr.Values[i] == nil {
			sr.Values[i] = wrapAffinityCollated(colDefs[i], sr.RowID)
		}
	}
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

// StructRowToMap converts a StructRow to a RowMap. The map must not observe
// later overwrites of the StructRow's reused value slots: the slot INTERFACE
// for this row is copied into the map, so the next fillStructRow* (which
// REPLACES slot contents, never mutating them in place) leaves the map
// holding this row's values. Fresh affinity wrappers (wrapPrecomputed) and
// freshly decoded values are exclusive to this row and shared as-is; only a
// []byte payload is deep-copied, so a consumer writing through the map's
// blob cannot corrupt other references to the same buffer.
func StructRowToMap(sr *StructRow) RowMap {
	m := make(RowMap, len(sr.Index)+1)
	m["rowid"] = &util.ColumnValue{Value: sr.RowID, Affinity: 'I'}
	for name, idx := range sr.Index {
		if idx < len(sr.Values) {
			m[name] = rowMapValue(sr.Values[idx])
		}
	}
	return m
}

// rowMapValue prepares a StructRow slot value for retention in a RowMap:
// immutable payloads (int64, float64, string, nil) and fresh wrapper pointers
// are shared; []byte payloads are copied (through any wrapper chain).
func rowMapValue(v interface{}) interface{} {
	switch t := v.(type) {
	case *util.ColumnValue:
		if _, isBlob := t.Value.([]byte); isBlob {
			return cloneRowValue(t)
		}
		return v
	case *CollatedValue:
		if cv, ok := t.Value.(*util.ColumnValue); ok {
			if _, isBlob := cv.Value.([]byte); isBlob {
				cp := *t
				cp.Value = cloneRowValue(cv)
				return &cp
			}
		}
		return v
	case []byte:
		b := make([]byte, len(t))
		copy(b, t)
		return b
	default:
		return v
	}
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

// scanConsumedByAggPass reports whether execSelectPostScan rebuilds the
// statement's output from the scanned row maps (aggregate / GROUP BY /
// correlated-aggregate / window passes) instead of using the per-row rows the
// scan built. When true, appendRowOutput skips the per-row output-row build —
// for a 100k-row GROUP BY that work was pure discard (the aggregate passes
// evaluate output expressions per group from the maps). The gate must stay a
// subset of execSelectPostScan's aggregate dispatch so the plain path (which
// does consume the scanned rows) is never taken with rows missing.
func scanConsumedByAggPass(e *SelectEngine, s *sql.SelectStmt) bool {
	if len(s.GroupBy) > 0 {
		return true
	}
	if e.hasAggregates(s.Columns) || e.hasSubqueryWithCorrelatedAgg(s.Columns) {
		return true
	}
	return e.selectHasWindowFuncs(s.Columns)
}
