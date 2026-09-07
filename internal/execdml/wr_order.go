package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// WithoutRowidStorageOrder returns, for each storage slot s of a WITHOUT
// ROWID table's index-leaf record, the declared column index stored there.
// SQLite stores WR rows PK-columns-first (index_xinfo iField layout: PK key
// columns in key order, then remaining declared columns in declared order),
// e.g. (a,b,c) PK(b,c) stores [b c a]. Mirrors recover.tableEntry.iField.
func WithoutRowidStorageOrder(createSQL string, colDefs []sql.ColumnDef) []int {
	byName := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		byName[strings.ToLower(cd.Name)] = i
	}
	var pkIdx []int
	for _, name := range tableLevelPKColumns(createSQL) {
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

// tableLevelPKColumns extracts the PRIMARY KEY(...) column list from CREATE
// TABLE SQL (first identifier of each comma-separated element, so a trailing
// DESC/ASC sort key does not leak into the name).
func tableLevelPKColumns(createSQL string) []string {
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

// reorderToStorage returns values reordered from declared order to WR
// storage order (PK-first). The input slice is never mutated.
func ReorderToStorage(values []interface{}, order []int) []interface{} {
	out := make([]interface{}, len(order))
	for s, di := range order {
		if di < len(values) {
			out[s] = values[di]
		}
	}
	return out
}

// wrPKSlotCount returns the number of leading PK slots in the WR storage
// record (length of the PK prefix of withoutRowidStorageOrder).
func WRPKSlotCount(createSQL string, colDefs []sql.ColumnDef) int {
	order := WithoutRowidStorageOrder(createSQL, colDefs)
	if len(order) != len(colDefs) {
		return 0
	}
	byName := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		byName[strings.ToLower(cd.Name)] = i
	}
	n := 0
	for _, name := range tableLevelPKColumns(createSQL) {
		if _, ok := byName[strings.ToLower(name)]; ok {
			n++
		} else {
			break
		}
	}
	if n == 0 {
		for _, cd := range colDefs {
			if cd.PrimaryKey {
				n++
			}
		}
	}
	if n > len(order) {
		n = len(order)
	}
	return n
}

// wrRecordComparator returns a comparator over WR storage-order record bytes
// that orders by the leading npk PK slots using per-column affinity, then by
// record length with a raw-bytes tiebreak (SQLite's unpacked-record
// comparison over the PK prefix). Decoded records are memoized per comparator
// instance (one instance per insert/delete call): leaf splits sort with
// bubble sort, so a naive decode-per-comparison is O(n^2) DecodeRecord calls
// per split.
func WRRecordComparator(npk int, colDefs []sql.ColumnDef, order []int) func(a, b []byte) int {
	affs := make([]rune, npk)
	for s := 0; s < npk && s < len(order); s++ {
		if di := order[s]; di < len(colDefs) {
			affs[s] = util.Affinity(colDefs[di].Type)
		}
	}
	cache := make(map[string]*storage.Record)
	decode := func(b []byte) *storage.Record {
		if r, ok := cache[string(b)]; ok {
			return r
		}
		r, err := storage.DecodeRecord(b)
		if err != nil || r == nil {
			return nil
		}
		cache[string(b)] = r
		return r
	}
	return func(a, b []byte) int {
		ra := decode(a)
		rb := decode(b)
		if ra == nil || rb == nil {
			return util.CompareValues(a, b)
		}
		for s := 0; s < npk && s < len(ra.Values) && s < len(rb.Values); s++ {
			va, vb := ra.Values[s], rb.Values[s]
			if aff := affs[s]; aff != 0 {
				va = &util.ColumnValue{Value: util.UnwrapColumnValue(va), Affinity: aff}
				vb = &util.ColumnValue{Value: util.UnwrapColumnValue(vb), Affinity: aff}
			}
			if c := util.CompareValues(va, vb); c != 0 {
				return c
			}
		}
		if len(ra.Values) != len(rb.Values) {
			if len(ra.Values) < len(rb.Values) {
				return -1
			}
			return 1
		}
		return util.CompareValues(a, b)
	}
}

// reorderToDeclared returns storage-order values mapped back to declared
// order (inverse of reorderToStorage).
func ReorderToDeclared(stored []interface{}, order []int) []interface{} {
	out := make([]interface{}, len(order))
	for s, di := range order {
		if s < len(stored) && di < len(out) {
			out[di] = stored[s]
		}
	}
	return out
}

// wrTableBTree builds the storage btree for a table entry: index btree with
// a PK-aware comparator for WITHOUT ROWID tables, plain table btree
// otherwise. Central choke point for DML read/delete paths so every caller
// addresses WR rows by PK order (SQLite's index-btree layout).
func (e *DMLExecutor) wrTableBTree(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *btree.BTree {
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	tree := e.ctx.TableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, !withoutRowid)
	if withoutRowid {
		if order := WithoutRowidStorageOrder(tableEntry.SQL, colDefs); len(order) == len(colDefs) {
			if npk := WRPKSlotCount(tableEntry.SQL, colDefs); npk > 0 {
				tree.SetKeyCompare(WRRecordComparator(npk, colDefs, order))
			}
		}
	}
	return tree
}

// wrPKIndices returns the declared column indices of a WITHOUT ROWID
// table's PRIMARY KEY in key order (table-level PRIMARY KEY(...) first,
// then column-level PRIMARY KEY flags). Empty when the table has no PK.
func WRPKIndices(createSQL string, colDefs []sql.ColumnDef) []int {
	order := WithoutRowidStorageOrder(createSQL, colDefs)
	if len(order) != len(colDefs) {
		return nil
	}
	npk := WRPKSlotCount(createSQL, colDefs)
	if npk <= 0 {
		return nil
	}
	idx := make([]int, 0, npk)
	for s := 0; s < npk && s < len(order); s++ {
		idx = append(idx, order[s])
	}
	return idx
}

// wrCellMatchesOldKey reports whether an index-leaf cell holds one of the
// OLD PK keys (declared-order PK projections) collected for an UPDATE's
// delete phase. The cell payload is PK-first storage order; project it to
// the declared PK slots and compare with per-column affinity/collation.
func wrCellMatchesOldKey(cell *storage.Cell, oldKeys [][]interface{}, tableEntry *schema.Entry, ctx DMLContext) bool {
	if len(oldKeys) == 0 || tableEntry == nil || ctx == nil {
		return false
	}
	colDefs := ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	order := WithoutRowidStorageOrder(tableEntry.SQL, colDefs)
	return WRCellMatchesPKKeys(cell, oldKeys, order, WRPKIndices(tableEntry.SQL, colDefs), colDefs)
}

// WRCellMatchesPKKeys reports whether an index-leaf cell holds one of the
// given PK keys (each key is a declared-order PK projection aligned with
// pkIdx). Context-free core of wrCellMatchesOldKey, shared with the DDL
// row-rebuild delete phase.
func WRCellMatchesPKKeys(cell *storage.Cell, keys [][]interface{}, order []int, pkIdx []int, colDefs []sql.ColumnDef) bool {
	if len(keys) == 0 || len(order) != len(colDefs) || len(pkIdx) == 0 {
		return false
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return false
	}
	decl := ReorderToDeclared(rec.Values, order)
	for _, key := range keys {
		if len(key) != len(pkIdx) {
			continue
		}
		match := true
		for k, ci := range pkIdx {
			var have interface{}
			if ci < len(decl) {
				have = decl[ci]
			}
			if !wrValuesEqual(have, key[k], colDefs[ci]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// wrValuesEqual compares two column values with the column's declared
// collation via the engine comparator (mirrors allMatch's per-column
// comparison for PK deletes; PK slots are NOT NULL so NULL never matches).
func wrValuesEqual(have, want interface{}, cd sql.ColumnDef) bool {
	hv := util.UnwrapColumnValue(have)
	wv := util.UnwrapColumnValue(want)
	if hv == nil || wv == nil {
		return hv == nil && wv == nil
	}
	return util.CompareValuesCollate(hv, wv, cd.Collate) == 0
}

// wrPkKeyFromDeclared projects a declared-order row's PRIMARY KEY values
// (aligned with pkIdx, the declared PK column indices in key order).
func wrPkKeyFromDeclared(declaredValues []interface{}, pkIdx []int) []interface{} {
	key := make([]interface{}, len(pkIdx))
	for k, ci := range pkIdx {
		if ci < len(declaredValues) {
			key[k] = declaredValues[ci]
		}
	}
	return key
}

// deleteRowCells deletes the single row identified by (rowID, declaredValues):
// rowid equality for ordinary tables; OLD-PK key match for WITHOUT ROWID
// tables, whose index cells all share the synthetic RowID 0 so rowid equality
// would delete (or miss) the whole table.
func (e *DMLExecutor) deleteRowCells(tableEntry *schema.Entry, colDefs []sql.ColumnDef, rowID int64, declaredValues []interface{}) (int64, error) {
	return e.deleteRowsByIdentity(tableEntry, colDefs, nil, [][]interface{}{declaredValues}, []int64{rowID})
}

// deleteRowsByIdentity batch-deletes rows in one DeleteCellsWhere pass:
// OLD-PK key match (from each row's declared values) for WITHOUT ROWID
// tables; rowid set membership (rowIDs, or the singleRowIDs list) for
// ordinary tables. Returns the number of cells deleted.
func (e *DMLExecutor) deleteRowsByIdentity(tableEntry *schema.Entry, colDefs []sql.ColumnDef, rowIDs map[int64]bool, declaredRowValues [][]interface{}, singleRowIDs []int64) (int64, error) {
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	tree := e.wrTableBTree(e.dmlPager(tableEntry.Name), tableEntry, colDefs)
	if withoutRowid {
		order := WithoutRowidStorageOrder(tableEntry.SQL, colDefs)
		pkIdx := WRPKIndices(tableEntry.SQL, colDefs)
		if len(order) != len(colDefs) || len(pkIdx) == 0 {
			return 0, fmt.Errorf("without rowid layout undecodable for %s", tableEntry.Name)
		}
		keys := make([][]interface{}, 0, len(declaredRowValues))
		for _, vals := range declaredRowValues {
			keys = append(keys, wrPkKeyFromDeclared(vals, pkIdx))
		}
		keys = dropNilKeys(keys)
		return tree.DeleteCellsWhere(func(c *storage.Cell) bool {
			return WRCellMatchesPKKeys(c, keys, order, pkIdx, colDefs)
		})
	}
	if rowIDs == nil {
		rowIDs = make(map[int64]bool, len(singleRowIDs))
		for _, id := range singleRowIDs {
			rowIDs[id] = true
		}
	}
	return tree.DeleteCellsWhere(func(c *storage.Cell) bool {
		return rowIDs[c.RowID]
	})
}

// dropNilKeys filters out PK keys containing nil slots (a row whose record
// predates a PK column can never match a full cell payload).
func dropNilKeys(keys [][]interface{}) [][]interface{} {
	out := keys[:0]
	for _, key := range keys {
		bad := false
		for _, v := range key {
			if v == nil {
				bad = true
				break
			}
		}
		if !bad {
			out = append(out, key)
		}
	}
	return out
}
