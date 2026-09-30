package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// UPDATE constraint-check plumbing: the constrained-column set an UPDATE
// conflict scan runs against, and the per-change change-detection gate that
// skips the O(table) scans when no constrained value moved (update.c emits
// uniqueness checks only for constrained columns the statement changes —
// indexColumnIsBeingUpdated / chngRowid — so unchanged constrained columns
// never pay for a check).

// updateConstrainedDefs returns the unique-constraint definitions an UPDATE
// conflict scan runs against: the table's UNIQUE indexes and autoindexes
// (uniqueIndexColumns) plus, for a WITHOUT ROWID table, a synthesized
// definition for its PRIMARY KEY when no schema index entry already covers
// the PK columns in key order. The engine stores WITHOUT ROWID rows in a
// PK-ordered table b-tree and creates no sqlite_autoindex schema row for
// them, so uniqueIndexColumns finds nothing — yet the PK is still a
// uniqueness constraint every UPDATE must enforce (SQLite checks it through
// the table's own index b-tree; the INSERT path enforces it via
// WRPKIndices directly).
func (e *DMLExecutor) updateConstrainedDefs(tableEntry *schema.Entry, colDefs []sql.ColumnDef) []uniqueIndexDef {
	defs := e.uniqueIndexColumns(tableEntry.Name)
	if !tableIsWithoutRowid(tableEntry.SQL) {
		return defs
	}
	pkIdx := WRPKIndices(tableEntry.SQL, colDefs)
	if len(pkIdx) == 0 {
		return defs
	}
	for _, def := range defs {
		if wrPKDefCoversPK(def, pkIdx, colDefs) {
			return defs
		}
	}
	cols := make([]string, len(pkIdx))
	for k, ci := range pkIdx {
		cols[k] = colDefs[ci].Name
	}
	// Appended last: SQLite's index list is newest-first for error naming,
	// and the implicit PK index is the oldest constraint on the table.
	out := make([]uniqueIndexDef, 0, len(defs)+1)
	out = append(out, defs...)
	out = append(out, uniqueIndexDef{Name: wrPKSynthIndexName(tableEntry.Name), Cols: cols})
	return out
}

// wrPKDefCoversPK reports whether def's key columns are exactly the table's
// PRIMARY KEY columns in key order (declared indices pkIdx).
func wrPKDefCoversPK(def uniqueIndexDef, pkIdx []int, colDefs []sql.ColumnDef) bool {
	if len(def.Cols) != len(pkIdx) {
		return false
	}
	for k, ci := range pkIdx {
		if ci >= len(colDefs) || !strings.EqualFold(def.Cols[k], colDefs[ci].Name) {
			return false
		}
	}
	return true
}

// wrPKSynthIndexName names the synthesized WITHOUT ROWID PK definition. The
// prefix keeps it distinct from every real schema index name.
func wrPKSynthIndexName(tableName string) string {
	return "pk:" + tableName
}

// dmlConstraintRowMapsNeeded reports whether any UNIQUE index definition in
// idxColsList needs a name-keyed row map during the change-detection gate
// (uniqueIndexDefNeedsRowMaps). When the result is false the per-change gate
// builds no row maps (a per-statement O(changes × 2) map saving).
func dmlConstraintRowMapsNeeded(colIndex map[string]int, idxColsList []uniqueIndexDef) bool {
	for _, def := range idxColsList {
		if uniqueIndexDefNeedsRowMaps(def, colIndex) {
			return true
		}
	}
	return false
}

// updateConstraintUnchanged reports whether a change's NEW values agree with
// its OLD values on every constrained slot — each UNIQUE/PRIMARY KEY column,
// and every UNIQUE index's key columns with unchanged partial-index
// membership. Nothing constrained moved means no other row can conflict with
// the new values: the old values already coexisted with every other row and
// with every other change's old values, so both the pairwise and the
// live-table conflict scans are skippable for that change (the value-level
// form of update.c emitting constraint checks only for columns the statement
// changes). needRowMaps precomputes whether any definition's check reads a
// name-keyed row map (dmlConstraintRowMapsNeeded); when false, no maps are
// built for the change.
func (e *DMLExecutor) updateConstraintUnchanged(c updateChange, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, needRowMaps bool) bool {
	if len(uniqueCols) == 0 && len(idxColsList) == 0 {
		return true
	}
	// A re-keyed row (SET rowid=... / SET <ipk> when the alias slot is
	// NULL) changes its rowid identity; rowidMoveConflict validates the
	// target, but the change keeps today's full checks.
	if c.newRowID != nil && *c.newRowID != c.rowID {
		return false
	}
	var oldRow, newRow RowMap
	if needRowMaps {
		oldRow = buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
		newRow = buildRowMapFromValues(c.values, colDefs, c.rowID)
	}
	for _, idx := range uniqueCols {
		if !uniqueColValuesMatch(c.oldValues, c.values, colDefs, c.rowID, c.rowID, idx) {
			return false
		}
	}
	for _, def := range idxColsList {
		if !e.uniqueIndexKeyUnchanged(def, c.oldValues, c.values, colDefs, colIndex, oldRow, newRow) {
			return false
		}
	}
	return true
}

// uniqueIndexKeyUnchanged reports whether one UNIQUE index's key is unchanged
// between a change's old and new values. Anything the comparison cannot
// settle — expression or qualified keys, unresolvable column names, a NULL
// key slot, changed partial-index membership — reports false so the checks
// still run (the conservative direction). A key that is in the index on both
// sides and compares slot-equal the way indexDefsMatch compares is unchanged.
func (e *DMLExecutor) uniqueIndexKeyUnchanged(def uniqueIndexDef, oldValues, newValues []interface{}, colDefs []sql.ColumnDef, colIndex map[string]int, oldRow, newRow RowMap) bool {
	if def.Where != "" {
		inOld, _ := e.evalIndexWhere(def.Where, oldRow)
		inNew, _ := e.evalIndexWhere(def.Where, newRow)
		if inOld != inNew {
			return false
		}
		if !inOld {
			return true // outside the index on both sides: cannot conflict
		}
	}
	for _, cn := range def.Cols {
		// Expression or qualified keys, and names that do not resolve to a
		// declared column, keep the full checks.
		if strings.ContainsAny(cn, "(.") {
			return false
		}
		if _, ok := colIndex[strings.ToLower(cn)]; !ok {
			return false
		}
		rkv, rok := e.indexKeyValue(cn, colDefs, colIndex, oldValues, oldRow)
		ckv, cok := e.indexKeyValue(cn, colDefs, colIndex, newValues, newRow)
		if !rok || !cok || util.CompareValues(rkv, ckv) != 0 {
			return false
		}
	}
	return true
}
