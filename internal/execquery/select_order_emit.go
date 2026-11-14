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
	"strconv"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/pager"
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
	// Collation-aware first: the terms' explicit COLLATE names participate
	// in the match (where.c compares the term's collation with the index's
	// azColl array). Terms the term-matcher cannot represent fall back to
	// the name-only path.
	idxName := ""
	if terms := orderByTermColls(s); terms != nil {
		idxName = e.findIndexForOrderTerms(s.From.Name, terms, s.Where)
	} else {
		idxName = e.findIndexOnColsForQuery(s.From.Name, cols, s.Where)
	}
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
	entry := e.schemaIndexEntryForTable(tableName, indexSchemaName(idxName))
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
// indexEntryAndPager resolves an index entry in the OWNING schema of the
// named FROM table (possibly schema-qualified: an attached database's
// same-named index is a separate b-tree over its own pager) plus that
// context's pager. The engine's default context serves when the table does
// not resolve.
func (e *SelectEngine) indexEntryAndPager(tableName, idxName string) (*schema.Entry, *pager.Pager) {
	if _, tblCtx, err := e.ctx.FindTable(tableName); err == nil && tblCtx != nil {
		entries, gerr := tblCtx.Schema.GetEntries("")
		if gerr == nil {
			for _, entry := range entries {
				if entry.Type == "index" && entry.Name == idxName {
					return entry, tblCtx.Pager
				}
			}
			return nil, nil
		}
	}
	return e.schemaIndexEntry(idxName), e.ctx.Pager()
}

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

// schemaIndexEntryForTable resolves an index entry within the FROM table's
// owning schema (falling back to the engine's default context).
func (e *SelectEngine) schemaIndexEntryForTable(tableName, idxName string) *schema.Entry {
	entry, _ := e.indexEntryAndPager(tableName, idxName)
	return entry
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
	// One row needs no permutation: the order is trivially delivered (a
	// sorter must not run for it — SQLite emits no OP_Sort for an
	// index-ordered plan regardless of the row count).
	if len(result.Rows) == 1 {
		return true
	}
	if len(rowMaps) != len(result.Rows) {
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

// permuteScanResults applies a row permutation to the scan's output rows and
// row maps in lockstep. When the scan produced no output rows (aggregate-
// consumed statements skip the per-row build) only the maps are permuted.
func (e *SelectEngine) permuteScanResults(rows [][]interface{}, maps []RowMap, perm []int) {
	sortedMaps := make([]RowMap, len(maps))
	for i, from := range perm {
		sortedMaps[i] = maps[from]
	}
	copy(maps, sortedMaps)
	if len(rows) != len(perm) {
		return
	}
	sortedRows := make([][]interface{}, len(rows))
	for i, from := range perm {
		sortedRows[i] = rows[from]
	}
	copy(rows, sortedRows)
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
	entry, pager := e.indexEntryAndPager(tableName, indexSchemaName(idxName))
	if entry == nil || entry.RootPage == 0 || pager == nil {
		return nil, false
	}
	// The index b-tree is built directly over the context's pager at the
	// schema entry's root page: the TableBTree helpers resolve the root
	// through the TABLE's tracked-root map, which would substitute the
	// table's root and misread the tree (an index walk must use the index's
	// own rootpage).
	tree := btree.NewBTree(pager, entry.RootPage, false)
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
// rowsInRowidOrder reports whether the statement's rows are already in
// rowid order and the ORDER BY asks for exactly that order: a plain
// single-table scan whose FIRST ORDER BY term is the rowid pseudo-column or
// the table's INTEGER PRIMARY KEY alias. The rowid is unique, so any terms
// after the first add no ordering constraint (where.c: a unique leading term
// retires the rest of the sort). backward reports a rowid DESC order, which
// a reverse table scan delivers.
//
// SQLite's rule (where.c): a full table scan emits its rows in rowid order,
// so an ORDER BY on the rowid needs no temp b-tree. A skip-scan, an index
// seek and the OR-index plan all deliver a different order and are excluded.
func (e *SelectEngine) rowsInRowidOrder(result *Result, s *sql.SelectStmt, orderBy []sql.OrderByTerm) (backward, ok bool) {
	if result == nil || result.RowsInBranchOrder || s == nil || len(orderBy) == 0 {
		return false, false
	}
	if len(s.Joins) > 0 || s.Union != nil || len(s.GroupBy) > 0 || s.Distinct ||
		s.From.Subquery != nil || s.From.Name == "" {
		return false, false
	}
	// The scan must be a plain table scan: an index loop (seek, skip-scan or
	// a covering walk) delivers its own order. An INTEGER PRIMARY KEY loop is
	// rowid-ordered too — an equality seek emits one row and a range seek
	// (rowid BETWEEN…) walks the table b-tree itself.
	//
	// A WHERE with a top-level OR is the exception: the OR-index plan
	// decides the order, and when it is not applicable (a branch that no
	// index can serve) the executor falls back to a plain table scan, whose
	// rows are in rowid order. The OR plan itself marks its result
	// RowsInBranchOrder, so the sort path never reaches this check for it.
	loop, err := e.scanLoopForRowCount(queryTableFromRef(s.From), s, indexSeekPlanRowCount, false)
	if err != nil {
		return false, false
	}
	if loop.kind != loopNone && loop.kind != loopIPK && !whereHasOrConjunct(s.Where) {
		return false, false
	}
	alias := e.rowidAliasColumnName(s.From.Name)
	// The FIRST term decides (the rowid is unique, so later terms cannot
	// break its order — where.c retires the remaining terms behind a unique
	// leading term). It must name the rowid: the pseudo-columns
	// rowid/_rowid_/oid work on every rowid table, the alias only where the
	// table declares an INTEGER PRIMARY KEY.
	first := orderBy[0]
	isRowid := false
	if ref, isRef := first.Expr.(*sql.ColumnRef); isRef {
		isRowid = (alias != "" && strings.EqualFold(ref.Name, alias)) || isRowidPseudoColumnName(ref.Name)
	} else if lit, isLit := first.Expr.(*sql.NumericLit); isLit {
		// An ORDER BY ordinal names an output column: "ORDER BY 1" is the
		// first column of the result.
		if n, convErr := strconv.Atoi(strings.TrimSpace(lit.Value)); convErr == nil &&
			n >= 1 && n <= len(result.Columns) {
			name := result.Columns[n-1]
			isRowid = (alias != "" && strings.EqualFold(name, alias)) || isRowidPseudoColumnName(name)
		}
	}
	if !isRowid {
		return false, false
	}
	return first.Desc, true
}

// rowidAliasColumnName returns the table's INTEGER PRIMARY KEY alias column
// name ("" when the table has none, is a WITHOUT ROWID table, or declares a
// column that shadows the rowid pseudo-column).
func (e *SelectEngine) rowidAliasColumnName(tableName string) string {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Name != tableName {
			continue
		}
		if e.ctx.TableIsWithoutRowidEntry(entry) {
			return ""
		}
		colDefs := e.ctx.ParseColumnDefs(tableName, entry.SQL)
		if len(colDefs) > 0 && isRowidShadowColumn(colDefs) {
			return ""
		}
		for i := range colDefs {
			if isIPKRowidAliasCol(colDefs[i]) {
				return colDefs[i].Name
			}
		}
		return ""
	}
	return ""
}

// isRowidShadowColumn reports whether a declared column shadows the rowid
// pseudo-column ("rowid"/"_rowid_"/"oid").
func isRowidShadowColumn(colDefs []sql.ColumnDef) bool {
	for i := range colDefs {
		if isRowidPseudoColumnName(colDefs[i].Name) {
			return true
		}
	}
	return false
}

// isRowidPseudoColumnName reports whether a column name is one of the rowid
// pseudo-column spellings.
func isRowidPseudoColumnName(name string) bool {
	switch strings.ToLower(name) {
	case "rowid", "_rowid_", "oid":
		return true
	}
	return false
}

// loopOrderedEmission reports the index whose b-tree order the executor must
// emit a SEARCH plan's rows in, and the direction: the same loop-order gate
// the planner used to drop the sorter (so the EQP shape and the emitted row
// order never disagree). A no-WHERE query's full index walk (the ORDER BY-
// serving index scanLoopForOrderIndex resolves) is served the same way —
// the walk consumes the terms, the rowid tail of a rowid-table index
// included (where-12.2's ORDER BY b, a). ok is false when the comparator
// sort stays responsible for the ordering.
func (e *SelectEngine) loopOrderedEmission(s *sql.SelectStmt) (idxName string, backward, ok bool) {
	if s == nil || s.Union != nil || len(s.Joins) > 0 ||
		s.From.Name == "" || s.From.Subquery != nil || len(s.GroupBy) > 0 || s.Distinct {
		return "", false, false
	}
	if len(e.withoutRowidPKCols(s.From.Name)) > 0 {
		return "", false, false
	}
	loop := e.scanLoopForQuery(queryTableFromRef(s.From), s, false)
	if loop.kind != loopIndex {
		return "", false, false
	}
	backward, satisfied := e.indexLoopOrderSatisfied(queryTableFromRef(s.From), loop, s)
	if !satisfied {
		return "", false, false
	}
	return loop.token, backward, true
}

// emitIndexOrderedRows applies the two index-order emission gates: the
// plain-scan gate (all terms bare, index-leading-prefix order) and the
// SEARCH gate (loopOrderedEmission: seek prefix + residual index columns —
// the gate the planner used to omit the sorter, so the EQP shape and the
// emitted row order never disagree). When either gate accepts, the result
// rows are permuted into the index b-tree's stored key order: the seek
// prefix fixes the leading values and the walk orders the rest, including
// the reverse direction and its rowid-descending ties. Returns false when
// the comparator sort stays responsible.
// reverseResultRows flips the emitted row order in place — the reverse of a
// table scan (OP_Last/OP_Prev) delivers a rowid-DESC order with no sorter.
func reverseResultRows(result *Result) {
	for i, j := 0, len(result.Rows)-1; i < j; i, j = i+1, j-1 {
		result.Rows[i], result.Rows[j] = result.Rows[j], result.Rows[i]
	}
}

// orderByRowidLeadingName reports whether the statement's first ORDER BY
// term directly names the rowid (pseudo-column or INTEGER PRIMARY KEY
// alias) by column name — the shape whose delivery the planner secures by
// choosing the plain table scan over a secondary-index loop (where.c prices
// the temp-b-tree sort into every loop that does not deliver the order, and
// rowid order is free). Ordinal terms are excluded: resolving them needs
// the result columns, which the planner does not have.
func (e *SelectEngine) orderByRowidLeadingName(s *sql.SelectStmt) bool {
	if s == nil || len(s.OrderBy) == 0 || s.Union != nil || len(s.Joins) > 0 ||
		len(s.GroupBy) > 0 || s.Distinct || s.From.Subquery != nil || s.From.Name == "" {
		return false
	}
	if len(e.withoutRowidPKCols(s.From.Name)) > 0 {
		return false
	}
	ref, ok := normalizeOrderByExpr(s.OrderBy[0].Expr).(*sql.ColumnRef)
	if !ok {
		return false
	}
	if isRowidPseudoColumnName(ref.Name) {
		return true
	}
	alias := e.rowidAliasColumnName(s.From.Name)
	return alias != "" && strings.EqualFold(ref.Name, alias)
}

func (e *SelectEngine) emitIndexOrderedRows(result *Result, rowMaps []RowMap, s *sql.SelectStmt, orderBy []sql.OrderByTerm) bool {
	// JOIN delivery: the claim (wherePathSatisfiesOrderBy over the whole
	// loop path) held — the base rows entered the nesting in the base
	// loop's order and every picked operand materialized in its index's
	// order, so the combined rows already emerge in the delivered order
	// and no sorter runs. Without the claim the comparator sort stays
	// responsible (the pre-claim behavior).
	if len(s.Joins) > 0 {
		return e.joinDelivery != nil && e.joinDelivery.delivered
	}
	// A plain table scan yields its rows in rowid order, so an ORDER BY whose
	// FIRST term is the rowid (or its INTEGER PRIMARY KEY alias) is already
	// satisfied and no sorter runs (SQLite: where.c's "the loop delivers the
	// ordering" for a full table scan; the rowid's uniqueness retires later
	// terms). A DESC first term is delivered by reversing the emitted rows —
	// SQLite's OP_Last/OP_Prev reverse table scan. An OR-index plan is
	// excluded: its rows are in branch order, not rowid order.
	if backward, ok := e.rowsInRowidOrder(result, s, orderBy); ok {
		if backward {
			reverseResultRows(result)
		}
		return true
	}

	if idxName, backward, ok := e.indexOrderedScanForOrderBy(s, orderBy); ok {
		if e.emitRowsInIndexOrder(result, rowMaps, s.From.Name, idxName, backward) {
			return true
		}
	}
	if idxName, backward, ok := e.loopOrderedEmission(s); ok {
		if e.emitRowsInIndexOrder(result, rowMaps, s.From.Name, idxName, backward) {
			return true
		}
	}
	return false
}
