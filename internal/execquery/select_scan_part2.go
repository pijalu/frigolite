package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// This file owns table scanning and row materialization for SELECT execution:
// iterating b-tree cells, applying lazy decoding and WHERE filtering, and
// building output rows / row maps. Extracted from select.go for file-level SRP.

// distinctRows removes duplicate rows from a result set,
// keeping the corresponding rowMaps in sync. colls holds the collation of
// each result column (nil → BINARY).

func installRowidAliases(row RowMap, colDefs []sql.ColumnDef, rowID int64) {
	if RowHasRowIDColumn(colDefs) {
		return
	}
	rowidCV := &util.ColumnValue{Value: rowID, Affinity: 'I'}
	row["rowid"] = rowidCV
	row["_rowid_"] = rowidCV
	row["oid"] = rowidCV
}

// applyRowMapDefaults applies DEFAULT values for columns added after the record
// was written (fewer record values than column definitions), wrapping each with
// the column's affinity and collation.
func (e *SelectEngine) applyRowMapDefaults(row RowMap, rec *storage.Record, colDefs []sql.ColumnDef) {
	vals := make([]interface{}, len(colDefs))
	copy(vals, rec.Values)
	e.applyColumnDefaults(vals, colDefs, len(rec.Values))
	for i := len(rec.Values); i < len(colDefs); i++ {
		cd := &colDefs[i]
		if cd.Default != nil && !cd.Dropped {
			row[cd.Name] = wrapAffinityCollated(*cd, vals[i])
		}
	}
}

// wrapAffinityCollated wraps a raw value with the column's affinity (and
// declared collation, if non-BINARY) so comparison logic applies SQLite
// affinity and column collation rules. compareValuesWithCollate extracts the
// collation from CollatedValue wrappers.
func wrapAffinityCollated(cd sql.ColumnDef, v interface{}) interface{} {
	cv := &util.ColumnValue{Value: v, Affinity: util.Affinity(cd.Type)}
	if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
		return &CollatedValue{Value: cv, Collation: strings.ToUpper(coll)}
	}
	return cv
}

// affinityPlan precomputes, per table scan, which columns receive
// affinity/collation wrappers and which slots are INTEGER PRIMARY KEY
// rowid-alias fills. Building the plan once per scan replaces the per-row
// needsAffinity map+EqualFold scans, util.Affinity (ToUpper), collation
// EqualFold, and isIPKRowidAliasCol walks with two indexed loops.
type affinityPlan struct {
	wrapIdx  []int    // column indices to wrap with affinity/collation
	wrapAff  []rune   // affinity rune per wrapIdx
	wrapColl []string // uppercased collation per wrapIdx ("" = BINARY)
	ipkIdx   []int    // INTEGER PRIMARY KEY rowid-alias column indices
	ipkAff   []rune   // affinity rune per ipkIdx
	ipkColl  []string // uppercased collation per ipkIdx ("" = BINARY)
}

// columnWrapInfo precomputes the affinity rune and uppercased collation a
// column's values are wrapped with (wrapAffinityCollated semantics).
func columnWrapInfo(cd *sql.ColumnDef) (rune, string) {
	aff := util.Affinity(cd.Type)
	collName := ""
	if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
		collName = strings.ToUpper(coll)
	}
	return aff, collName
}

// newAffinityPlan builds the plan for a scan's colDefs against the collected
// affinity column names. Returns nil when affinityCols is nil AND the table
// has no INTEGER PRIMARY KEY rowid-alias column (the alias fill is a VALUE
// substitution, not a wrapper, and it rides on the plan: without it a
// wrapper-free scan would leave the alias slot at its stored NULL —
// "SELECT id FROM ipk" must still emit the rowid). A nil-map-with-alias
// table yields a fill-only plan (empty wrap set, IPK fill computed).
func newAffinityPlan(colDefs []sql.ColumnDef, affinityCols map[string]bool) *affinityPlan {
	if affinityCols == nil {
		if !hasIPKRowidAliasCol(colDefs) {
			return nil
		}
		affinityCols = map[string]bool{}
	}
	p := &affinityPlan{}
	for i := range colDefs {
		cd := &colDefs[i]
		if needsAffinity(affinityCols, cd.Name) {
			aff, coll := columnWrapInfo(cd)
			p.wrapIdx = append(p.wrapIdx, i)
			p.wrapAff = append(p.wrapAff, aff)
			p.wrapColl = append(p.wrapColl, coll)
		}
		if isIPKRowidAliasCol(*cd) {
			aff, coll := columnWrapInfo(cd)
			p.ipkIdx = append(p.ipkIdx, i)
			p.ipkAff = append(p.ipkAff, aff)
			p.ipkColl = append(p.ipkColl, coll)
		}
	}
	return p
}

// wrapPrecomputed wraps a raw value with a precomputed affinity/collation
// pair, producing the same wrapper chain wrapAffinityCollated builds.
func wrapPrecomputed(aff rune, collName string, v interface{}) interface{} {
	cv := &util.ColumnValue{Value: v, Affinity: aff}
	if collName != "" {
		return &CollatedValue{Value: cv, Collation: collName}
	}
	return cv
}

// apply wraps the planned columns' values in place, then fills rowid-alias
// columns that SQLite stores as NULL (INTEGER PRIMARY KEY) with the rowid.
// Order matches the previous applyStructRowAffinity: wrap first, then the
// rowid-alias fill.
func (p *affinityPlan) apply(values []interface{}, rowID int64) {
	for k, i := range p.wrapIdx {
		if v := values[i]; v != nil {
			values[i] = wrapPrecomputed(p.wrapAff[k], p.wrapColl[k], v)
		}
	}
	for k, i := range p.ipkIdx {
		if values[i] == nil {
			values[i] = wrapPrecomputed(p.ipkAff[k], p.ipkColl[k], rowID)
		}
	}
}

// ipkAliasIndices returns the indices of INTEGER PRIMARY KEY rowid-alias
// columns in colDefs. Unlike the affinity plan (which is only built when
// affinity columns are referenced), this list is computed for every scan so
// the lazy-decode phase-2 refill can skip its per-row isIPKRowidAliasCol walk.
func ipkAliasIndices(colDefs []sql.ColumnDef) []int {
	var idx []int
	for i := range colDefs {
		if isIPKRowidAliasCol(colDefs[i]) {
			idx = append(idx, i)
		}
	}
	return idx
}

// hasIPKRowidAliasCol reports whether any column of the table is an INTEGER
// PRIMARY KEY rowid-alias column (allocation-free existence check for the
// affinity plan's fill-only fallback).
func hasIPKRowidAliasCol(colDefs []sql.ColumnDef) bool {
	for i := range colDefs {
		if isIPKRowidAliasCol(colDefs[i]) {
			return true
		}
	}
	return false
}

// fillStructRowFromTypes fills a StructRow using pre-parsed serial types.
// It clears all values and decodes only the columns in colIndices.
// Unlike fillStructRow, it does not re-parse the record header.
func (e *SelectEngine) fillStructRowFromTypes(sr *StructRow, payload []byte, dataStart int, colDefs []sql.ColumnDef, rowID int64, plan *affinityPlan, serialTypes []uint64, colIndices map[int]bool, wrOrder []int) {
	values := sr.Values
	for i := range values {
		values[i] = nil
	}
	sr.RowID = rowID

	storage.DecodeRecordValuesFromTypes(payload, dataStart, values, serialTypes, colIndices)

	// WITHOUT ROWID index-leaf records are PK-first storage order: permute
	// to declared order BEFORE defaults/affinity so every later consumer
	// (defaults, per-column affinity wrappers, WHERE) sees declared slots.
	if len(wrOrder) == len(values) {
		permuteDeclared(values, wrOrder)
	}

	// A dropped column (ALTER TABLE DROP COLUMN) has no on-disk slot: a VIRTUAL
	// generated column was never stored, so the record's values sit at the
	// non-dropped positions; shift them into the full colDefs layout so
	// values[i] still corresponds to colDefs[i]. A STORED/plain column's slot
	// was removed by the drop's record rewrite (same shift).
	shiftDroppedColumns(values, colDefs)

	// Rows written before ALTER TABLE ADD COLUMN have fewer record values
	// than column definitions; SQLite applies the added column's DEFAULT at
	// read time. Only columns beyond the record's value count get the default.
	e.applyColumnDefaults(values, colDefs, len(serialTypes))

	// Apply affinity wrappers for the columns planned at scan setup. Match
	// buildRowMap: wrap ALL columns (including INTEGER/REAL) with their
	// affinity so comparison logic applies the same SQLite affinity rules on
	// both the fast StructRow path and the map path.
	if plan != nil {
		plan.apply(values, rowID)
	}
}

// permuteDeclared permutes storage-order decoded values to declared column
// order in place (values[order[s]] = old values[s]).
func permuteDeclared(values []interface{}, order []int) {
	tmp := make([]interface{}, len(values))
	copy(tmp, values)
	for s, di := range order {
		if di < len(values) {
			values[di] = tmp[s]
		}
	}
}

// shiftDroppedColumns re-aligns decoded values so values[i] corresponds to
// colDefs[i] when the table has dropped (ALTER TABLE DROP COLUMN) columns that
// have no on-disk slot. No-op when there are no dropped columns.
func shiftDroppedColumns(values []interface{}, colDefs []sql.ColumnDef) {
	hasDropped := false
	for _, cd := range colDefs {
		if cd.Dropped {
			hasDropped = true
			break
		}
	}
	if !hasDropped {
		return
	}
	shifted := make([]interface{}, len(colDefs))
	ci := 0
	for i := range colDefs {
		if colDefs[i].Dropped {
			continue
		}
		if ci < len(values) {
			shifted[i] = values[ci]
		}
		ci++
	}
	copy(values, shifted)
}

// needsAffinity reports whether a column name is referenced in affinityCols,
// matching case-insensitively.
func needsAffinity(affinityCols map[string]bool, colName string) bool {
	if affinityCols[colName] {
		return true
	}
	for name := range affinityCols {
		if strings.EqualFold(name, colName) {
			return true
		}
	}
	return false
}

// buildOutputRow builds the output row from the SELECT columns. An output
// expression that raises an error (e.g. base64 of a blob exceeding
// SQLITE_LIMIT_LENGTH) aborts the query, matching SQLite's propagation of
// function errors from result columns.
func (e *SelectEngine) buildOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) ([]interface{}, error) {
	outRow := make([]interface{}, 0, outputColumnCount(columns, colDefs))
	var err error
	for _, col := range columns {
		ref, isStar := col.Expr.(*sql.ColumnRef)
		if isStar && ref.Name == "*" {
			if ref.Table != "" {
				outRow = appendQualifiedStar(outRow, e, ref.Table, colDefs, row)
			} else {
				outRow = appendUnqualifiedStar(outRow, colDefs, row)
			}
			continue
		}
		// A window function column is computed by the window pass over the
		// full row set; evaluating it here per-row would fail for
		// unregistered window-only functions (lead/lag/nth_value). Emit a
		// placeholder the window pass replaces.
		if e.exprHasWindowFunc(col.Expr) {
			outRow = append(outRow, nil)
			continue
		}
		outRow, err = appendOutputExpr(outRow, e, col.Expr, row)
		if err != nil {
			return nil, err
		}
	}
	return outRow, nil
}

// outputColumnCount estimates the number of result columns for pre-allocation.
func outputColumnCount(columns []sql.SelectColumn, colDefs []sql.ColumnDef) int {
	colCount := 0
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			for _, cd := range colDefs {
				if !cd.Dropped && !IsHiddenColumnDef(cd) {
					colCount++
				}
			}
		} else {
			colCount++
		}
	}
	return colCount
}

// appendQualifiedStar appends a qualified star's (t.*) columns to outRow,
// resolved by the table's real column names (aliases allowed).
func appendQualifiedStar(outRow []interface{}, e *SelectEngine, table string, colDefs []sql.ColumnDef, row Row) []interface{} {
	for _, cd := range e.qualifiedStarColNames(table, colDefs, row) {
		outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(cd.value)))
	}
	return outRow
}

// appendUnqualifiedStar appends an unqualified star's (*) columns to outRow.
// Duplicate-named columns (e.g. a view aliasing several columns) cannot be
// distinguished by name, so a positional slice is used when the colDef index
// matches.
func appendUnqualifiedStar(outRow []interface{}, colDefs []sql.ColumnDef, row Row) []interface{} {
	posIdx := 0
	for _, cd := range colDefs {
		if cd.Dropped || IsHiddenColumnDef(cd) {
			continue
		}
		if pos, ok := row.Get(positionalRowKey); ok {
			if pv, ok := pos.([]interface{}); ok && posIdx < len(pv) {
				outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(pv[posIdx])))
				posIdx++
				continue
			}
		}
		if val, exists := row.Get(cd.Name); exists {
			outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(val)))
		}
		posIdx++
	}
	return outRow
}

// appendOutputExpr appends an evaluated expression column to outRow. Returns
// (row, nil) on success and (row, err) when the expression raised an error
// (the error aborts the query — SQLite propagates function errors from result
// columns rather than silently substituting NULL).
func appendOutputExpr(outRow []interface{}, e *SelectEngine, expr sql.Expr, row Row) ([]interface{}, error) {
	v, err := e.ctx.EvalExpr(expr, row)
	if err != nil {
		return append(outRow, nil), err
	}
	return append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(v))), nil
}

// filterRowMapsByWhere filters rowMaps by a WHERE expression, returning the
// subset that passes. Returns nil error on success.
func filterRowMapsByWhere(e *SelectEngine, where sql.Expr, rowMaps []RowMap) ([]RowMap, error) {
	if where == nil {
		return rowMaps, nil
	}
	filtered := rowMaps[:0]
	for _, rowMap := range rowMaps {
		pass, err := e.rowPassesWhere(where, rowMap, nil)
		if err != nil {
			return nil, err
		}
		if pass {
			filtered = append(filtered, rowMap)
		}
	}
	return filtered, nil
}

// buildOutputRowsFromMaps evaluates the SELECT column list against each row map,
// producing the output row slice.
func buildOutputRowsFromMaps(e *SelectEngine, columns []sql.SelectColumn, colDefs []sql.ColumnDef, rowMaps []RowMap) ([][]interface{}, error) {
	rows := make([][]interface{}, len(rowMaps))
	for i, rowMap := range rowMaps {
		row, err := e.buildOutputRow(columns, colDefs, rowMap)
		if err != nil {
			return nil, err
		}
		rows[i] = row
	}
	return rows, nil
}

// rebuildRowMapsFromRows creates RowMaps from output rows keyed by result column
// names, used to rebuild maps after a compound merge so ORDER BY resolves columns
// across all members.
func rebuildRowMapsFromRows(rows [][]interface{}, columns []string) []RowMap {
	rowMaps := make([]RowMap, len(rows))
	for i, row := range rows {
		m := make(RowMap)
		for j, v := range row {
			if j < len(columns) {
				m[columns[j]] = v
			}
		}
		rowMaps[i] = m
	}
	return rowMaps
}

// materializedAlias returns the qualified-name alias for a materialized table
// (derived-table alias, pragma table-valued function alias, or vtab function
// alias), or "" when none applies.
