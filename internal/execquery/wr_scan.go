package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// wrStorageOrder returns the WITHOUT ROWID index-leaf storage layout: for
// each storage slot s, the declared column index stored there (PK key
// columns first in key order, then remaining declared columns in declared
// order — the index_xinfo iField layout; oracle bytes for (1,2,3) PK(b,c)
// are [2 3 1]). Returns nil when the table is not WITHOUT ROWID.
func wrStorageOrder(createSQL string, colDefs []sql.ColumnDef) []int {
	up := strings.ToUpper(createSQL)
	stripped := createSQL
	if i := strings.Index(up, " AS SELECT"); i >= 0 {
		stripped, up = createSQL[:i], up[:i]
	}
	idx := strings.LastIndex(stripped, ")")
	if idx < 0 || !strings.Contains(up[idx:], "WITHOUT") {
		return nil
	}
	byName := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		byName[strings.ToLower(cd.Name)] = i
	}
	var pkIdx []int
	for _, name := range wrTableLevelPK(createSQL) {
		if di, ok := byName[strings.ToLower(name)]; ok {
			dup := false
			for _, p := range pkIdx {
				if p == di {
					dup = true
					break
				}
			}
			if !dup {
				pkIdx = append(pkIdx, di)
			}
		}
	}
	if len(pkIdx) == 0 {
		for i, cd := range colDefs {
			if cd.PrimaryKey {
				pkIdx = append(pkIdx, i)
			}
		}
	}
	inPK := make(map[int]bool, len(pkIdx))
	for _, i := range pkIdx {
		inPK[i] = true
	}
	order := make([]int, 0, len(colDefs))
	order = append(order, pkIdx...)
	for i := range colDefs {
		if !inPK[i] {
			order = append(order, i)
		}
	}
	return order
}

// wrTableLevelPK extracts the PRIMARY KEY(...) column list from CREATE TABLE
// SQL (first identifier per element, so a DESC/ASC key does not leak in).
func wrTableLevelPK(createSQL string) []string {
	up := strings.ToUpper(createSQL)
	i := strings.Index(up, "PRIMARY KEY(")
	if i < 0 {
		i = strings.Index(up, "PRIMARY KEY (")
		if i < 0 {
			return nil
		}
	}
	open := strings.Index(up[i:], "(") + i
	depth, end := 0, -1
	for j := open; j < len(createSQL); j++ {
		switch createSQL[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = j
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}
	var cols []string
	for _, f := range strings.Split(createSQL[open+1:end], ",") {
		if fields := strings.Fields(strings.TrimSpace(f)); len(fields) > 0 {
			cols = append(cols, strings.Trim(fields[0], `"'[]`))
		}
	}
	return cols
}

// RemapWRRecordToDeclared permutes a PK-first WITHOUT ROWID storage-order
// record back to declared column order in place (inverse of the PK-first
// write reorder). No-op for rowid tables, dropped-column tables, or identity
// layouts.
func (e *SelectEngine) RemapWRRecordToDeclared(rec *storage.Record, createSQL string, colDefs []sql.ColumnDef) {
	if rec == nil {
		return
	}
	wrRemapToDeclared(rec.Values, wrStorageOrder(createSQL, colDefs), colDefs)
}

// WRStorageOrder exposes the WITHOUT ROWID PK-first storage layout (nil for
// rowid tables) so DML decode sites share one layout computation.
func (e *SelectEngine) WRStorageOrder(createSQL string, colDefs []sql.ColumnDef) []int {
	return wrStorageOrder(createSQL, colDefs)
}

// wrRemapToDeclared permutes storage-order decoded values back to declared
// column order (inverse of the PK-first write reorder). No-op unless the
// table has no dropped columns and the layout is non-identity.
func wrRemapToDeclared(values []interface{}, order []int, colDefs []sql.ColumnDef) {
	if len(order) != len(values) || len(order) != len(colDefs) {
		return
	}
	for _, cd := range colDefs {
		if cd.Dropped {
			return
		}
	}
	identity := true
	for s, di := range order {
		if s != di {
			identity = false
			break
		}
	}
	if identity {
		return
	}
	tmp := make([]interface{}, len(values))
	copy(tmp, values)
	for s, di := range order {
		values[di] = tmp[s]
	}
}

// wrRemapRowMapsToDeclared rebuilds row maps whose values were captured in
// storage order back into declared-order maps. Values are looked up by
// column name so wrapped ColumnValue/collation wrappers survive intact.
func wrRemapRowMapsToDeclared(maps []RowMap, order []int, colDefs []sql.ColumnDef) []RowMap {
	out := make([]RowMap, len(maps))
	for i, m := range maps {
		nm := make(RowMap, len(m))
		for k, v := range m {
			nm[k] = v
		}
		if len(order) == len(colDefs) {
			storageVals := make([]interface{}, len(colDefs))
			for s, di := range order {
				if di < len(colDefs) {
					storageVals[s] = m[colDefs[di].Name]
				}
			}
			wrRemapToDeclared(storageVals, order, colDefs)
			for di, cd := range colDefs {
				if di < len(storageVals) {
					nm[cd.Name] = storageVals[di]
				}
			}
		}
		out[i] = nm
	}
	return out
}
