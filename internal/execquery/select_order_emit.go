// Package exec implements query execution.
//
// This file holds the index-satisfied ORDER BY emission contract: when the
// planner consumes the ORDER BY (where.c orderByConsumed), the index loop
// emits rows in the index b-tree's STORED key order and no temp b-tree sort
// runs. The stored order is read from the index b-tree itself — no collation
// comparison is invoked — so a collation redefined mid-session does not
// reorder the output (reindex-2.6/2.7); only REINDEX physically rebuilds it.
package execquery

import (
	"github.com/pijalu/frigolite/internal/btree"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// indexOrderedScanForOrderBy reports the index whose b-tree order satisfies
// the ORDER BY of a plain single-table scan (all terms bare columns matching
// an index prefix; a WITHOUT ROWID table's order is its storage order and
// keeps the legacy path), and whether the scan walks it backward. Forward
// requires every term direction to match the index column's sort order;
// backward requires every direction to be the opposite (where.c
// sqlite3OrderByIsIndexed / pIndex->aSortOrder checks). Each term must also
// agree with the index column's collation (where.c satisfies a sort term
// only under its own collation sequence) and its NULLS FIRST/LAST placement
// with the scan's null order (an ASC b-tree stores NULLs first, a DESC one
// last). ok is false when the ordering needs the temp b-tree sort (the
// comparator path).
func (e *SelectEngine) indexOrderedScanForOrderBy(s *sql.SelectStmt, orderBy []sql.OrderByTerm) (idxName string, backward, ok bool) {
	if !simpleOrderedScanShape(s) {
		return "", false, false
	}
	cols, descs, plain := orderByTermColumnsWithDirs(orderBy)
	if !plain || len(e.withoutRowidPKCols(s.From.Name)) > 0 {
		return "", false, false
	}
	idxName, ok = e.orderScanIndexForCols(s, cols)
	if !ok {
		return "", false, false
	}
	idxDescs, known := e.indexColumnDescFlags(s.From.Name, idxName)
	if !known || len(idxDescs) < len(cols) {
		return "", false, false
	}
	backward, ok = orderByScanDirection(cols, descs, idxDescs)
	if !ok {
		return "", false, false
	}
	if !e.orderByTermsMatchIndexCollations(s.From.Name, idxName, cols, orderBy) {
		return "", false, false
	}
	if !orderByTermsMatchScanNullOrder(orderBy, idxDescs, backward) {
		return "", false, false
	}
	return idxName, backward, true
}

// declaredColumnCollation returns the collation declared on a table column
// in its CREATE TABLE DDL, or "" for BINARY (the default).
func (e *SelectEngine) declaredColumnCollation(tableName, colName string) string {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return ""
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if strings.EqualFold(cd.Name, colName) {
			if cd.Collate == "" {
				return ""
			}
			return strings.ToUpper(cd.Collate)
		}
	}
	return ""
}

// orderByTermsMatchIndexCollations reports whether every ORDER BY term's
// effective collation — the term's explicit COLLATE, else the column's
// declared collation, else BINARY — equals the index column's effective
// collation (where.c sorts a term under its own collation only: an index on
// a COLLATE nocase column cannot satisfy a BINARY ORDER BY term, distinct-9.x).
func (e *SelectEngine) orderByTermsMatchIndexCollations(tableName, idxName string, cols []string, orderBy []sql.OrderByTerm) bool {
	for i, ob := range orderBy {
		want := orderByTermExplicitCollation(ob.Expr)
		if want == "" {
			want = e.declaredColumnCollation(tableName, cols[i])
		}
		got := e.indexColumnCollation(tableName, idxName, cols[i])
		if !strings.EqualFold(want, got) {
			return false
		}
	}
	return true
}

// orderByTermExplicitCollation returns the collation name of a term's
// top-level COLLATE operator, or "" when the term carries none.
func orderByTermExplicitCollation(obExpr sql.Expr) string {
	if b, ok := obExpr.(*sql.BinaryOp); ok && strings.EqualFold(b.Operator, "COLLATE") {
		if lit, isLit := b.Right.(*sql.StringLit); isLit {
			return lit.Value
		}
	}
	return ""
}

// orderByTermsMatchScanNullOrder reports whether each term's NULLS FIRST /
// NULLS LAST requirement (default: ASC puts NULLs first, DESC last) agrees
// with the scan's null placement: a forward scan over an ASC index column
// emits NULLs first, a forward scan over a DESC column last; a backward scan
// reverses both (where.c only consumes an ORDER BY term whose null ordering
// the index provides — otherwise a sorter runs, nulls1-4.3).
func orderByTermsMatchScanNullOrder(orderBy []sql.OrderByTerm, idxDescs []bool, backward bool) bool {
	for i := range orderBy {
		ob := orderBy[i]
		nullsFirst := idxDescs[i] == backward
		requested := !ob.Desc
		if ob.NullsFirst {
			requested = true
		} else if ob.NullsLast {
			requested = false
		}
		if requested != nullsFirst {
			return false
		}
	}
	return true
}

// simpleOrderedScanShape reports whether the select is a plain single-table
// scan eligible for index-order emission (no compound, no joins, FROM present).
func simpleOrderedScanShape(s *sql.SelectStmt) bool {
	return s != nil && s.Union == nil && len(s.Joins) == 0 && s.From.Name != ""
}

// orderScanIndexForCols finds an index over cols for the select's FROM table
// that the WHERE clause does not defeat with non-indexable constraints.
func (e *SelectEngine) orderScanIndexForCols(s *sql.SelectStmt, cols []string) (string, bool) {
	idxName := e.findIndexOnColsForQuery(s.From.Name, cols, s.Where)
	if idxName == "" {
		return "", false
	}
	if s.Where != nil && e.whereHasNonIndexConstraint(s.Where, s.From.Name, idxName) {
		return "", false
	}
	return idxName, true
}

// orderByScanDirection matches each ORDER BY term's direction against the
// index column's sort order: forward requires every direction to match the
// index column's, backward requires every direction to be the opposite
// (where.c sqlite3OrderByIsIndexed / pIndex->aSortOrder checks). ok is false
// when neither direction satisfies the whole term list.
func orderByScanDirection(cols []string, descs, idxDescs []bool) (backward, ok bool) {
	forward := true
	backward = true
	for i := range cols {
		if descs[i] != idxDescs[i] {
			forward = false
		}
		if descs[i] == idxDescs[i] {
			backward = false
		}
	}
	return backward, forward || backward
}

// orderByTermColumnsWithDirs extracts the bare column names of an ORDER BY
// list with each term's direction. plain is false when any term is not an
// unqualified column reference (explicit COLLATE wrappers included — the
// legacy comparator path owns those).
func orderByTermColumnsWithDirs(orderBy []sql.OrderByTerm) (cols []string, descs []bool, plain bool) {
	for _, ob := range orderBy {
		ref, ok := normalizeOrderByExpr(ob.Expr).(*sql.ColumnRef)
		if !ok || ref.Table != "" || ref.Name == "*" {
			return nil, nil, false
		}
		cols = append(cols, ref.Name)
		descs = append(descs, ob.Desc)
	}
	return cols, descs, len(cols) > 0
}

// indexColumnDescFlags returns the per-key-column sort order of the named
// index (true = DESC). Autoindexes are always ascending. ok is false when the
// index has no schema entry.
func (e *SelectEngine) indexColumnDescFlags(tableName, idxName string) ([]bool, bool) {
	entry := e.schemaIndexEntry(indexSchemaName(idxName))
	if entry == nil {
		return nil, false
	}
	cols := e.indexEntryColumns(entry)
	if entry.SQL == "" {
		return make([]bool, len(cols)), true
	}
	colText := indexColumnListText(entry.SQL)
	if colText == "" {
		return nil, false
	}
	parts := splitIndexCols(colText)
	descs := make([]bool, len(parts))
	for i, part := range parts {
		upper := strings.ToUpper(strings.TrimSpace(part))
		// Trailing sort-order keyword (the same stripping parseIndexKeyCols
		// applies); " DESC" with the leading space cannot occur inside a
		// plain column name.
		descs[i] = strings.Contains(upper, " DESC")
	}
	return descs, true
}

// schemaIndexEntry finds an index's schema entry by name.
func (e *SelectEngine) schemaIndexEntry(idxName string) *schema.Entry {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.Name == idxName {
			return entry
		}
	}
	return nil
}

// emitRowsInIndexOrder reorders the scan's output into the index b-tree's
// stored key order: the index tree is walked (no collation comparison — the
// stored order is the b-tree order, so a redefined collation cannot affect
// the emission), the trailing rowid of each index record keys the walk, and
// the scan rows are permuted to match. Rows without a rowid in the scan
// (none for a rowid-table scan) keep their relative order at the end. The
// bool result is false when the walk is unavailable — the caller falls back
// to the comparator sort.
func (e *SelectEngine) emitRowsInIndexOrder(result *Result, rowMaps []RowMap, tableName, idxName string, backward bool) bool {
	if len(result.Rows) == 0 {
		return true
	}
	if len(result.Rows) < 2 || len(rowMaps) != len(result.Rows) {
			return false
	}
	rowids, ok := e.indexStoredRowidOrder(tableName, idxName)
	if !ok || len(rowids) == 0 {
		return false
	}
	if backward {
		// A backward scan reads the index b-tree in the opposite direction.
		reverseRowids(rowids)
	}
	positions := scanRowidPositions(rowMaps)
	if len(positions) == 0 {
		// No scan row carries a rowid — aggregate (GROUP BY) outputs are the
		// case: each group's row map is its representative row, and the
		// index walk cannot key it. The identity permutation such a walk
		// would produce leaks the ASC group-key order as a "consumed" ORDER
		// BY (tkt-ba7cbfaedc 1.x.4: GROUP BY x,y ORDER BY x DESC, y DESC
		// must reverse-sort); the temp-sort comparator owns the ordering.
		return false
	}
	perm := indexOrderPermutation(rowids, positions, len(result.Rows))
	e.permuteScanResults(result.Rows, rowMaps, perm)
	return true
}

// reverseRowids reverses a stored rowid sequence in place.
func reverseRowids(rowids []int64) {
	for i, j := 0, len(rowids)-1; i < j; i, j = i+1, j-1 {
		rowids[i], rowids[j] = rowids[j], rowids[i]
	}
}

// scanRowidPositions maps each scan row's rowid to its position in the
// result (rows without a rowid — none for a rowid-table scan — are never
// keyed).
func scanRowidPositions(rowMaps []RowMap) map[int64]int {
	posByRowid := make(map[int64]int, len(rowMaps))
	for i, m := range rowMaps {
		if v := lookupRowMapValue(m, "rowid"); v != nil {
			if rid, isInt := util.UnwrapColumnValue(v).(int64); isInt {
				posByRowid[rid] = i
			}
		}
	}
	return posByRowid
}

// indexOrderPermutation permutes scan positions into the index's stored (or
// reversed) key order; positions never keyed by the walk keep their relative
// order at the end.
func indexOrderPermutation(rowids []int64, posByRowid map[int64]int, n int) []int {
	used := make([]bool, n)
	perm := make([]int, 0, n)
	for _, rid := range rowids {
		if p, hit := posByRowid[rid]; hit && !used[p] {
			used[p] = true
			perm = append(perm, p)
		}
	}
	for p, wasUsed := range used {
		if !wasUsed {
			perm = append(perm, p)
		}
	}
	return perm
}

// indexStoredRowidOrder walks the index b-tree and returns every entry's
// trailing rowid in stored key order (index records carry the rowid as their
// last element).
func (e *SelectEngine) indexStoredRowidOrder(tableName, idxName string) ([]int64, bool) {
	entry := e.schemaIndexEntry(indexSchemaName(idxName))
	if entry == nil || entry.RootPage == 0 {
		return nil, false
	}
	// The index b-tree is built directly over the context's pager at the
	// schema entry's root page: the TableBTree helpers resolve the root
	// through the TABLE's tracked-root map, which would substitute the
	// table's root and misread the tree (an index walk must use the index's
	// own rootpage).
	tree := btree.NewBTree(e.ctx.Pager(), entry.RootPage, false)
	defer tree.Close() // tree and its scan cursor are function-local
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, false
	}
	var rowids []int64
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return nil, false
		}
		rid, ok := indexEntryRowid(cell)
		if !ok {
			return nil, false
		}
		rowids = append(rowids, rid)
		more, err := cursor.Next()
		if err != nil || !more {
			break
		}
	}
	return rowids, true
}

// indexEntryRowid decodes an index record's trailing rowid (index records
// carry the rowid as their last element). ok is false on any decode failure
// (the caller aborts the walk).
func indexEntryRowid(cell *storage.Cell) (int64, bool) {
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil || rec == nil || len(rec.Values) == 0 {
		return 0, false
	}
	rid, isInt := util.UnwrapColumnValue(rec.Values[len(rec.Values)-1]).(int64)
	return rid, isInt
}
