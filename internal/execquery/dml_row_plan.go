package execquery

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns the positional row plan for DML row collection: the
// per-statement precomputation that lets UPDATE/DELETE collect candidate rows
// WITHOUT materializing a RowMap per row (the SELECT scan's StructRow model,
// applied to the DML scan loops).
//
// The plan is built once per statement from the expressions the collect loop
// will evaluate against each row (WHERE, SET value expressions, ORDER BY
// terms). It knows, once:
//   - which column slots need affinity/collation wrappers for those
//     expressions (mirroring scanTableAffinityCols for SELECT scans), and
//   - which slots are INTEGER PRIMARY KEY rowid-alias columns whose stored
//     NULL must read back as the rowid.
//
// Rows are then carried as StructRows (reused per scan) or raw declared-order
// value slices, and a name-keyed RowMap is materialized only at the points
// whose contract demands one (trigger OLD/NEW rows, RETURNING, FK actions,
// partial-index predicates).

// DMLRowPlan is the per-statement positional row plan for DML row collection.
// It is immutable after construction and safe to reuse across the rows of one
// statement.
type DMLRowPlan struct {
	colDefs  []sql.ColumnDef
	colIndex map[string]int // declared column name → slot (StructRow.Index)
	plan     *affinityPlan  // wrapper + rowid-alias fill plan (nil = wrap nothing)
	ipkIdx   []int          // INTEGER PRIMARY KEY rowid-alias column indices
}

// NewDMLRowPlan builds the positional row plan for one DML statement: exprs
// are every expression the collect loop evaluates per row (the WHERE clause
// and the SET value expressions), orderBy the statement's ORDER BY terms.
// The returned plan wraps exactly the referenced columns — the same model
// the SELECT scan uses — so WHERE/SET/ORDER BY evaluation sees identical
// affinity and collation behavior.
func (e *SelectEngine) NewDMLRowPlan(colDefs []sql.ColumnDef, exprs []sql.Expr, orderBy []sql.OrderByTerm) *DMLRowPlan {
	a := &affinityCollector{cols: make(map[string]bool)}
	for _, expr := range exprs {
		a.collectExprRefs(expr)
	}
	for _, ob := range orderBy {
		a.collectExpr(ob.Expr)
	}
	colIndex := make(map[string]int, len(colDefs))
	for i := range colDefs {
		colIndex[colDefs[i].Name] = i
	}
	return &DMLRowPlan{
		colDefs:  colDefs,
		colIndex: colIndex,
		plan:     newAffinityPlan(colDefs, a.cols),
		ipkIdx:   ipkAliasIndices(colDefs),
	}
}

// NewRow allocates a StructRow over the plan's column layout. The row's
// Values slice is reusable: FillDMLRow overwrites every slot per row.
func (p *DMLRowPlan) NewRow() *StructRow {
	return &StructRow{Values: make([]interface{}, len(p.colDefs)), Index: p.colIndex}
}

// FillDMLRow fills a reused StructRow from one decoded record's values for
// WHERE/SET/ORDER BY evaluation. values must be declared-order (a
// WITHOUT ROWID record must be remapped first, as the DML scan loops already
// do); valueCount is the stored record's value count so rows written before
// ALTER TABLE ADD COLUMN read their added-column DEFAULTs. The fill mirrors
// buildRowMap for expression evaluation: dropped-column re-alignment,
// added-column DEFAULTs, affinity wrappers on the planned (referenced)
// columns, and the INTEGER PRIMARY KEY rowid-alias substitution for stored
// NULLs.
func (e *SelectEngine) FillDMLRow(p *DMLRowPlan, sr *StructRow, values []interface{}, valueCount int, rowID int64) {
	dst := sr.Values
	for i := range dst {
		dst[i] = nil
	}
	copy(dst, values)
	sr.RowID = rowID
	// A dropped column (ALTER TABLE DROP COLUMN) has no on-disk slot: shift
	// the record's values into the full colDefs layout (buildRowMap's
	// ci-skip semantics) so slot i corresponds to colDefs[i].
	shiftDroppedColumns(dst, p.colDefs)
	// Rows written before ALTER TABLE ADD COLUMN store fewer values than the
	// table declares: read-time DEFAULT (buildRowMap's applyRowMapDefaults).
	if valueCount < len(p.colDefs) {
		e.applyColumnDefaults(dst, p.colDefs, valueCount)
	}
	if p.plan != nil {
		p.plan.apply(dst, rowID)
	}
	for _, i := range p.ipkIdx {
		if dst[i] == nil {
			dst[i] = wrapAffinityCollated(p.colDefs[i], rowID)
		}
	}
}

// DMLRowSnapshot returns the RAW declared-order value slice to retain for one
// collected DML row: dropped-column re-alignment, added-column DEFAULTs, and
// the INTEGER PRIMARY KEY rowid-alias substitution applied as RAW values (no
// affinity wrappers). This matches what the wrapped-row consumers extract
// today (rowMapColumnValues unwraps the collected map's values) and what the
// write path expects in updateChange.values/oldValues. The caller owns the
// returned slice; the input values are not retained.
func (e *SelectEngine) DMLRowSnapshot(p *DMLRowPlan, values []interface{}, valueCount int, rowID int64) []interface{} {
	out := make([]interface{}, len(p.colDefs))
	copy(out, values)
	shiftDroppedColumns(out, p.colDefs)
	if valueCount < len(p.colDefs) {
		e.applyColumnDefaults(out, p.colDefs, valueCount)
	}
	for _, i := range p.ipkIdx {
		if out[i] == nil {
			out[i] = rowID
		}
	}
	return out
}

// RowMapFromDeclared materializes the name-keyed RowMap for one collected
// positional row — the exact point a consumer's contract demands
// name-keyed access (trigger OLD/NEW rows, RETURNING, FK actions, partial
// index predicates). Keys and values mirror buildRowMap on the same row:
// dropped columns have no key, every stored value is wrapped with its
// column's affinity/collation, stored NULL in the INTEGER PRIMARY KEY
// rowid-alias column reads back as the rowid, added-column DEFAULTs apply to
// slots beyond valueCount, and the rowid/_rowid_/oid aliases are installed
// unless the table declares a column of that name. values must be a
// DMLRowSnapshot (declared order, defaults and rowid-alias fill applied).
func (e *SelectEngine) RowMapFromDeclared(p *DMLRowPlan, values []interface{}, valueCount int, rowID int64) RowMap {
	row := make(RowMap, len(p.colDefs)+1)
	for i := range p.colDefs {
		cd := &p.colDefs[i]
		if cd.Dropped {
			continue
		}
		if i < valueCount {
			v := values[i]
			if v == nil && isIPKRowidAliasCol(*cd) {
				// SQLite stores NULL in the rowid-alias column; the value is
				// the rowid (buildRowMap's read-time substitution).
				row[cd.Name] = wrapAffinityCollated(*cd, rowID)
				continue
			}
			row[cd.Name] = wrapAffinityCollated(*cd, v)
			continue
		}
		// Beyond the stored record: the rowid-alias substitution or the
		// added column's DEFAULT (already applied to the snapshot by
		// DMLRowSnapshot); columns with neither keep buildRowMap's
		// key-absent behavior.
		if isIPKRowidAliasCol(*cd) || (cd.Default != nil && !cd.Dropped) {
			row[cd.Name] = wrapAffinityCollated(*cd, values[i])
		}
	}
	installRowidAliases(row, p.colDefs, rowID)
	return row
}
