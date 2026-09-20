package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// filterSystemTables removes rows that correspond to internal system tables
// from query results. This is applied when reading from sqlite_master/sqlite_schema.
func (e *SelectEngine) filterSystemTables(allRows [][]interface{}, allRowMaps []RowMap, colDefs []sql.ColumnDef) ([][]interface{}, []RowMap) {
	if nameIndex := systemTableNameIndex(colDefs); nameIndex < 0 {
		return allRows, allRowMaps
	}

	var filteredRows [][]interface{}
	var filteredMaps []RowMap
	for i, rowMap := range allRowMaps {
		if rowMapIsHiddenSystemTable(rowMap) {
			continue // skip system tables
		}
		if i < len(allRows) {
			filteredRows = append(filteredRows, allRows[i])
		}
		filteredMaps = append(filteredMaps, rowMap)
	}
	return filteredRows, filteredMaps
}

// systemTableNameIndex returns the column index of a "name"/"tbl_name" column,
// or -1 when neither is present (meaning system-table filtering does not apply).
func systemTableNameIndex(colDefs []sql.ColumnDef) int {
	for i, cd := range colDefs {
		if strings.EqualFold(cd.Name, "name") || strings.EqualFold(cd.Name, "tbl_name") {
			return i
		}
	}
	return -1
}

// rowMapIsHiddenSystemTable reports whether the row's "name" value names an
// internal system table that should be hidden from query results.
func rowMapIsHiddenSystemTable(rowMap RowMap) bool {
	nameVal, ok := rowMap["name"]
	if !ok {
		return false
	}
	nameStr := util.UnwrapColumnValue(nameVal)
	if nameStr == nil {
		return false
	}
	name, ok := nameStr.(string)
	return ok && isHiddenSystemTable(name)
}

// buildRowMap builds a column-name-to-value map from a record.
func (e *SelectEngine) buildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) RowMap {
	row := make(RowMap)
	// Record values map to the NON-dropped columns in order. A dropped column
	// (ALTER TABLE DROP COLUMN) has no on-disk slot: a VIRTUAL generated
	// column was never stored (the record skips it), and a STORED/plain
	// column's slot was removed by the drop's record rewrite.
	ci := 0
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		if ci < len(rec.Values) {
			// Wrap all column values with their affinity/collation so comparison
			// logic correctly applies SQLite affinity and column collation rules.
			row[cd.Name] = wrapAffinityCollated(cd, rec.Values[ci])
		}
		ci++
	}
	for i := ci; i < len(rec.Values); i++ {
		row[fmt.Sprintf("c%d", i)] = rec.Values[i]
	}
	installRowidAliases(row, colDefs, rowID)
	// SQLite writes NULL into the record for an INTEGER PRIMARY KEY rowid
	// alias column; the value is the rowid. Substitute it at read time
	// regardless of whether the query references the column (btree.c
	// record decoding: the alias column has no storage of its own).
	for i := range colDefs {
		cd := &colDefs[i]
		if isIPKRowidAliasCol(*cd) && util.UnwrapColumnValue(row[cd.Name]) == nil {
			row[cd.Name] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		}
	}
	// Rows written before ALTER TABLE ADD COLUMN have fewer record values
	// than column definitions; apply the added column's DEFAULT at read time
	// (with column affinity), matching SQLite semantics.
	if len(rec.Values) < len(colDefs) {
		e.applyRowMapDefaults(row, rec, colDefs)
	}
	return row
}

// installRowidAliases installs the pseudo-rowid aliases (rowid/_rowid_/oid)
// unless the table declares a column shadowing them, in which case the column
// value already set above takes precedence.
