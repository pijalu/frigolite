// WITHOUT ROWID secondary-index record tails: the C payload format (S33).
// C stores a WR table's index record as [indexed cols..., PRIMARY KEY
// cols...] — build.c's "Append the table key" (sqlite3CreateIndex,
// build.c:4269) appends pPk's key columns after the index's own key
// columns, skipping any PK column the key already carries (isDupColumn),
// with the PK column's collation and sort order on every appended field.
// The tail is the row locator every consumer resolves: the write-path
// recheck seeks the WR table btree by it (where-25.5's OP_NotExists), and
// the delete side removes the entry by the same bytes.

package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// wrTailPlan describes the PRIMARY KEY tail of one WITHOUT ROWID table's
// index records: the tail columns in PK key order minus the PK columns the
// index's own key already carries (C's isDupColumn elision), with each
// column's collation and DESC flag (the tail orders under the PK
// definition's collations and sort directions — oracle wr5.db: a
// PRIMARY KEY(b DESC) tail sorts 'z' before 'x').
type wrTailPlan struct {
	cols  []int    // declared column index of each tail column (PK key order, elided)
	colls []string // per tail column: the column's collation ("" = BINARY)
	flags []byte   // per tail column: KeyInfoOrderDesc when the PK column is DESC
}

// valid reports whether the tail layout could be resolved. An invalid plan
// (no PK, unresolvable storage order) keeps the caller's legacy behavior.
func (p wrTailPlan) valid() bool { return len(p.cols) > 0 }

// wrIndexTailPlan resolves one index's WR tail layout. indexCols are the
// index's key columns as written (a plain column name or an expression);
// colIndex resolves plain names to declared indices (lowercased keys, -1
// for pseudo-columns — buildColumnIndex's map). An expression key never
// matches a PK column (C's isDupColumn compares resolved column indices),
// so expressions never elide a tail column.
func wrIndexTailPlan(createSQL string, colDefs []sql.ColumnDef, indexCols []string, colIndex map[string]int) wrTailPlan {
	pkIdx := WRPKIndices(createSQL, colDefs)
	if len(pkIdx) == 0 {
		return wrTailPlan{}
	}
	present := make(map[int]bool, len(indexCols))
	for _, cn := range indexCols {
		if ci, ok := colIndex[strings.ToLower(cn)]; ok && ci >= 0 && ci < len(colDefs) {
			present[ci] = true
		}
	}
	desc := wrPKDescFlags(createSQL, colDefs, pkIdx)
	plan := wrTailPlan{}
	for k, ci := range pkIdx {
		if present[ci] {
			continue // C isDupColumn: the key already carries this PK column
		}
		coll := ""
		if ci < len(colDefs) {
			coll = strings.ToUpper(colDefs[ci].Collate)
		}
		var flag byte
		if k < len(desc) && desc[k] {
			flag = btree.KeyInfoOrderDesc
		}
		plan.cols = append(plan.cols, ci)
		plan.colls = append(plan.colls, coll)
		plan.flags = append(plan.flags, flag)
	}
	return plan
}

// wrPKDescFlags reports, per PK column in key order, whether that column's
// PRIMARY KEY declaration carries DESC (a table-level PRIMARY KEY(col DESC)
// list first — the promoted column flags carry only the column-level
// spelling).
func wrPKDescFlags(createSQL string, colDefs []sql.ColumnDef, pkIdx []int) []bool {
	out := make([]bool, len(pkIdx))
	terms := wrTableLevelPKTerms(createSQL)
	if len(terms) > 0 {
		byName := make(map[string]bool, len(terms))
		for _, t := range terms {
			byName[strings.ToLower(t.name)] = t.desc
		}
		for k, ci := range pkIdx {
			if ci < len(colDefs) {
				out[k] = byName[strings.ToLower(colDefs[ci].Name)]
			}
		}
		return out
	}
	for k, ci := range pkIdx {
		if ci < len(colDefs) {
			out[k] = colDefs[ci].PKDesc
		}
	}
	return out
}

// wrPKTerm is one table-level PRIMARY KEY element: its column name and
// whether the element declares DESC.
type wrPKTerm struct {
	name string
	desc bool
}

// wrTableLevelPKTerms parses a table-level PRIMARY KEY(...) column list with
// each element's sort direction (tableLevelPKColumns' parse plus the DESC
// flag the name extraction strips).
func wrTableLevelPKTerms(createSQL string) []wrPKTerm {
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
	var terms []wrPKTerm
	for _, f := range strings.Split(createSQL[open+1:end], ",") {
		fields := strings.Fields(strings.TrimSpace(f))
		if len(fields) == 0 {
			continue
		}
		t := wrPKTerm{name: strings.Trim(fields[0], `"'[]`)}
		if len(fields) > 1 && strings.EqualFold(fields[1], "DESC") {
			t.desc = true
		}
		terms = append(terms, t)
	}
	return terms
}

// wrIndexTailValues returns the tail values one row contributes to its
// WITHOUT ROWID index records: the row's PRIMARY KEY column values (raw;
// the write path's indexStorageValues unwraps wrappers). values is the
// row's declared-order value slice.
func wrIndexTailValues(plan wrTailPlan, values []interface{}) []interface{} {
	tail := make([]interface{}, len(plan.cols))
	for k, ci := range plan.cols {
		if ci < len(values) {
			tail[k] = values[ci]
		}
	}
	return tail
}

// wrTailPlanMemo is wrIndexTailPlan through the write path's schema-keyed
// memo slot (the ikiStamp pattern): the tail layout is a pure function of
// the schema, so one resolution per index serves every index-cell write.
// withoutRowid=false short-circuits to the empty plan (the caller keeps the
// rowid locator).
func (e *DMLExecutor) wrTailPlanMemo(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, def indexDef) wrTailPlan {
	if !tableIsWithoutRowid(tableEntry.SQL) {
		return wrTailPlan{}
	}
	stamp := e.databasesSchemaStamp()
	if e.wrPlanDone && e.wrPlanStamp == stamp && e.wrPlanCtx == def.Ctx &&
		e.wrPlanTable == tableEntry.Name && e.wrPlanIndex == def.Name {
		return e.wrPlanVal
	}
	plan := wrIndexTailPlan(tableEntry.SQL, colDefs, def.Cols, colIndex)
	e.wrPlanStamp, e.wrPlanCtx, e.wrPlanTable, e.wrPlanIndex = stamp, def.Ctx, tableEntry.Name, def.Name
	e.wrPlanDone, e.wrPlanVal = true, plan
	return plan
}

// extendIndexKeyInfoForWR copies a base index KeyInfo with the tail's
// collations and sort flags appended (C's index KeyInfo covers every record
// field — nKeyField == nColumn for WR — so the packed-vs-packed comparator
// that orders the tree and drives the delete descent compares the tail
// under the PK collations). The base KeyInfo is never mutated: it is the
// write path's memoized instance.
func extendIndexKeyInfoForWR(ki *btree.KeyInfo, plan wrTailPlan) *btree.KeyInfo {
	if ki == nil || !plan.valid() {
		return ki
	}
	n := ki.NKeyField + len(plan.cols)
	colls := make([]string, 0, n)
	colls = append(colls, ki.Collations...)
	colls = append(colls, plan.colls...)
	flags := make([]byte, 0, n)
	flags = append(flags, ki.SortFlags...)
	flags = append(flags, plan.flags...)
	return btree.NewKeyInfo(n, colls, flags)
}

// wrEntryFullPK rebuilds a found index entry's row's full PRIMARY KEY from
// the entry's decoded record ([key fields..., tail...]): the elided PK
// columns' values ride the record's key fields (at the key position where
// the PK column appears among the index's key columns), the rest ride the
// tail in elided order. This is the 25.5 recheck's seek key — the tail
// alone is a PARTIAL PK when the index key covers part of it (oracle
// wr4.db: i1 on (c,a) with PK(a,b) stores tail [b] only; the row's a rides
// the key). The ENTRY's stored values are used (C's OP_NotExists re-seeks
// from the index cursor's content), not the probe's — a collation-equal
// prefix (NOCASE) can differ textually.
func wrEntryFullPK(tableSQL string, colDefs []sql.ColumnDef, indexCols []string, colIndex map[string]int, plan wrTailPlan, recValues []interface{}) ([]interface{}, bool) {
	pkIdx := WRPKIndices(tableSQL, colDefs)
	if len(pkIdx) == 0 || !plan.valid() {
		return nil, false
	}
	nKey := len(indexCols)
	if nKey > len(recValues) {
		return nil, false
	}
	tail := recValues[nKey:]
	// Where each elided PK column sits among the index's key columns.
	keyPos := make(map[int]int, nKey)
	for i, cn := range indexCols {
		if ci, ok := colIndex[strings.ToLower(cn)]; ok && ci >= 0 && ci < len(colDefs) {
			if _, dup := keyPos[ci]; !dup {
				keyPos[ci] = i
			}
		}
	}
	pk := make([]interface{}, 0, len(pkIdx))
	ti := 0
	for _, ci := range pkIdx {
		if p, elided := keyPos[ci]; elided {
			pk = append(pk, recValues[p])
			continue
		}
		var v interface{}
		if ti < len(tail) {
			v = tail[ti]
		}
		pk = append(pk, v)
		ti++
	}
	if ti != len(plan.cols) {
		return nil, false // tail layout disagrees with the plan
	}
	return pk, true
}

// wrPKProbeValues unwraps a probe value list into raw storage values for a
// probe record (wrPKTailProbe's role for any locator slice).
func wrPKProbeValues(vals []interface{}) []interface{} {
	out := make([]interface{}, len(vals))
	for i, v := range vals {
		out[i] = util.UnwrapColumnValue(v)
	}
	return out
}

// wrSeekRowByPK seeks a WITHOUT ROWID table's PK-keyed btree for the row
// with the given full PRIMARY KEY (PK key order — the storage record's
// leading slots). exists=false is definitive for a decodable layout and a
// BINARY-collated PK (wrPKSeekApplies' binary-window rule: a
// collation-equal row can sort outside the seek's landing window, and a
// false corrupt verdict on a healthy database is worse than no recheck, so
// seekable=false sends the caller back to its scan-based verdict).
func (e *DMLExecutor) wrSeekRowByPK(tableName, createSQL string, rootPage uint32, colDefs []sql.ColumnDef, pkKey []interface{}) (exists, seekable bool) {
	order := WithoutRowidStorageOrder(createSQL, colDefs)
	pkIdx := WRPKIndices(createSQL, colDefs)
	if len(order) != len(colDefs) || len(pkIdx) != len(pkKey) {
		return false, false
	}
	probe := make([]interface{}, len(pkKey))
	for s, v := range pkKey {
		if util.UnwrapColumnValue(v) == nil {
			// PK columns are implicitly NOT NULL: a NULL locator names no
			// row (the entry is stale — C's OP_NotExists would miss too).
			return false, true
		}
		probe[s] = util.UnwrapColumnValue(v)
	}
	payload, err := storage.EncodeRecord(probe)
	if err != nil {
		return false, false
	}
	tree := e.uniqueScanTreeFresh(tableName, rootPage)
	defer tree.Close() // probe tree is function-local
	tree.SetKeyCompare(WRRecordComparator(len(pkIdx), colDefs, order))
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false, false
	}
	if _, err := cursor.SeekToKey(payload); err != nil {
		return false, false
	}
	cell, err := cursor.ReadCell()
	if err != nil || cell == nil {
		// No entry at the lower bound: no row with that PK.
		return false, true
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil {
		return false, false
	}
	e.ctx.RemapWRRecordToDeclared(rec, createSQL, colDefs)
	for k, ci := range pkIdx {
		var have interface{}
		if ci < len(rec.Values) {
			have = rec.Values[ci]
		}
		var want interface{}
		if k < len(pkKey) {
			// pkKey is PK key order; pkIdx is the same order.
			want = pkKey[k]
		}
		if !wrValuesEqual(have, want, colDefs[ci]) {
			return false, true
		}
	}
	return true, true
}

// wrRecheckRowExists is the 25.5 recheck for one found WR index entry: it
// rebuilds the entry's full PK from the entry's record and seeks the table
// btree for it. ok=false means the recheck could not run (undecodable
// layout/seek failure) — the caller keeps its scan-based verdict.
func (e *DMLExecutor) wrRecheckRowExists(tableEntry *schema.Entry, colDefs []sql.ColumnDef, def indexDef, colIndex map[string]int, plan wrTailPlan, recValues []interface{}) (exists, ok bool) {
	pk, valid := wrEntryFullPK(tableEntry.SQL, colDefs, def.Cols, colIndex, plan, recValues)
	if !valid {
		return false, false
	}
	exists, seekable := e.wrSeekRowByPK(tableEntry.Name, tableEntry.SQL, tableEntry.RootPage, colDefs, pk)
	if !seekable {
		return false, false
	}
	return exists, true
}
