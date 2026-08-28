package execquery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/schema"
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
func (e *SelectEngine) distinctRows(rows [][]interface{}, rowMaps []RowMap, colls []string, s *sql.SelectStmt) ([][]interface{}, []RowMap) {
	if len(rows) == 0 {
		return rows, rowMaps
	}
	// When a covering index exists for the DISTINCT columns, SQLite satisfies
	// DISTINCT by scanning that index, so the output follows the index key
	// order (not insertion order). Otherwise SQLite materializes DISTINCT via
	// a temp b-tree keyed by the output columns, which also sorts.
	newRows, newMaps := dedupDistinctRows(rows, rowMaps, colls)
	if len(newMaps) != len(newRows) {
		return newRows, newMaps
	}
	if idxCols := e.coveringIndexForDistinct(s); len(idxCols) > 0 {
		reorderByIndexCols(newRows, newMaps, idxCols)
	}
	return newRows, newMaps
}

// sortDistinctRows sorts DISTINCT rows by their result columns (like the temp
// b-tree SQLite uses when no covering index exists).
//
//lint:ignore U1000 retained for callers that need explicit DISTINCT sorting.
func (e *SelectEngine) sortDistinctRows(rows [][]interface{}, maps []RowMap, colls []string) {
	type pair struct {
		row []interface{}
		m   RowMap
	}
	pairs := make([]pair, len(rows))
	for i := range rows {
		pairs[i] = pair{rows[i], maps[i]}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		for k := range colls {
			coll := ""
			if k < len(colls) {
				coll = colls[k]
			}
			vi := pairs[i].row[k]
			vj := pairs[j].row[k]
			if cmp := e.ctx.CompareValuesCollate(util.UnwrapColumnValue(vi), util.UnwrapColumnValue(vj), coll); cmp != 0 {
				return cmp < 0
			}
		}
		return false
	})
	for i := range pairs {
		rows[i] = pairs[i].row
		maps[i] = pairs[i].m
	}
}

// dedupDistinctRows removes duplicate rows (by rowKey over colls), keeping the
// corresponding rowMaps in sync.
func dedupDistinctRows(rows [][]interface{}, rowMaps []RowMap, colls []string) ([][]interface{}, []RowMap) {
	seen := make(map[string]bool)
	var newRows [][]interface{}
	var newMaps []RowMap
	for i, row := range rows {
		key := rowKey(row, colls)
		if seen[key] {
			continue
		}
		seen[key] = true
		newRows = append(newRows, row)
		if i < len(rowMaps) {
			newMaps = append(newMaps, rowMaps[i])
		}
	}
	return newRows, newMaps
}

// reorderByIndexCols sorts deduplicated rows so they follow the covering index
// key order, matching SQLite's index-scan DISTINCT output.
func reorderByIndexCols(rows [][]interface{}, maps []RowMap, idxCols []string) {
	type pair struct {
		row []interface{}
		m   RowMap
	}
	pairs := make([]pair, len(rows))
	for i := range rows {
		pairs[i] = pair{rows[i], maps[i]}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return comparePairsByIndex(pairs[i].m, pairs[j].m, idxCols) < 0
	})
	for i := range pairs {
		rows[i] = pairs[i].row
		maps[i] = pairs[i].m
	}
}

// comparePairsByIndex compares two row maps by successive index columns,
// returning the first non-zero comparison (0 if all equal).
func comparePairsByIndex(a, b RowMap, idxCols []string) int {
	for _, col := range idxCols {
		vi := lookupRowMapValue(a, col)
		vj := lookupRowMapValue(b, col)
		if cmp := util.CompareValues(util.UnwrapColumnValue(vi), util.UnwrapColumnValue(vj)); cmp != 0 {
			return cmp
		}
	}
	return 0
}

// coveringIndexForDistinct returns the column list of an index that fully
// covers the DISTINCT output columns of s (a single-table query), so the
// DISTINCT rows can be emitted in index order like SQLite. Returns nil when
// no such index exists or the query is not a simple single-table scan.
func (e *SelectEngine) coveringIndexForDistinct(s *sql.SelectStmt) []string {
	if !distinctIndexApplicable(s) {
		return nil
	}
	tableName, alias := distinctTableAlias(s)
	need, ok := distinctNeededColumns(s, tableName, alias)
	if !ok {
		return nil
	}
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	return indexColumnsForDistinct(e, entries, tableName, need)
}

// coveringIndexForAggregate returns the column list of an index that fully
// covers every table column referenced by an aggregate GROUP BY query
// (SELECT expressions, GROUP BY expressions, WHERE, HAVING), so the rows can
// be scanned in index key order like SQLite's covering-index aggregate scan.
// Returns nil when the query is not a simple single-table scan or no single
// index covers all referenced columns.
func (e *SelectEngine) coveringIndexForAggregate(s *sql.SelectStmt) []string {
	if !distinctIndexApplicable(s) {
		return nil
	}
	tableName, alias := distinctTableAlias(s)
	need := aggregateNeededColumns(s, tableName, alias)
	if len(need) == 0 {
		return nil
	}
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	return indexColumnsForDistinct(e, entries, tableName, need)
}

// aggregateNeededColumns collects the unqualified table column names
// referenced anywhere in an aggregate GROUP BY query (select list, GROUP BY,
// WHERE, HAVING), stripping the table/alias qualifier. Returns nil when a
// referenced column is qualified by a different table (a join) or the query
// is not over a plain table.
func aggregateNeededColumns(s *sql.SelectStmt, tableName, alias string) []string {
	c := &aggregateColCollector{alias: alias, table: tableName, need: nil, seen: make(map[string]bool)}
	if !c.collectList(s.Columns, func(col sql.SelectColumn) sql.Expr { return col.Expr }) {
		return nil
	}
	if !c.collectExprs(s.GroupBy) {
		return nil
	}
	if !c.collect(s.Where) || !c.collect(s.Having) {
		return nil
	}
	return c.need
}

// aggregateColCollector accumulates the table column names referenced by an
// aggregate GROUP BY query's expressions, validating they belong to the
// scanned table (no foreign-table qualification).
type aggregateColCollector struct {
	alias string
	table string
	need  []string
	seen  map[string]bool
}

// collectList collects column refs from a list of select columns or GROUP BY
// expressions (each item contributes one sql.Expr via exprOf).
func (c *aggregateColCollector) collectList(list []sql.SelectColumn, exprOf func(sql.SelectColumn) sql.Expr) bool {
	for _, item := range list {
		if !c.collect(exprOf(item)) {
			return false
		}
	}
	return true
}

// collectExprs collects column refs from a list of expressions.
func (c *aggregateColCollector) collectExprs(list []sql.Expr) bool {
	for _, expr := range list {
		if !c.collect(expr) {
			return false
		}
	}
	return true
}

// collect walks one expression, recording its table column references.
func (c *aggregateColCollector) collect(expr sql.Expr) bool {
	if expr == nil {
		return true
	}
	ok := true
	WalkExprFull(expr, func(e sql.Expr) {
		ref, isRef := e.(*sql.ColumnRef)
		if !isRef {
			return
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, c.alias) && !strings.EqualFold(ref.Table, c.table) {
			ok = false
			return
		}
		if ref.Name == "" || ref.Name == "*" {
			ok = false
			return
		}
		if !c.seen[strings.ToLower(ref.Name)] {
			c.seen[strings.ToLower(ref.Name)] = true
			c.need = append(c.need, ref.Name)
		}
	})
	return ok
}

// distinctIndexApplicable reports whether the covering-index DISTINCT
// optimization applies: a non-nil single-table FROM with no joins/subquery.
func distinctIndexApplicable(s *sql.SelectStmt) bool {
	return s != nil && s.From.Name != "" && len(s.Joins) == 0 && s.From.Subquery == nil
}

// distinctTableAlias resolves the table name (schema prefix stripped) and the
// effective alias (alias or table name) for a DISTINCT query's FROM clause.
func distinctTableAlias(s *sql.SelectStmt) (tableName, alias string) {
	tableName = s.From.Name
	if dot := strings.Index(tableName, "."); dot >= 0 {
		tableName = tableName[dot+1:]
	}
	alias = s.From.As
	if alias == "" {
		alias = tableName
	}
	return tableName, alias
}

// distinctNeededColumns collects the table column names referenced by the
// DISTINCT output columns. Returns (need, false) if any output column is not a
// simple qualified table column ref (e.g. a star or a wrong-table ref).
func distinctNeededColumns(s *sql.SelectStmt, tableName, alias string) ([]string, bool) {
	var need []string
	for _, col := range s.Columns {
		ref, ok := col.Expr.(*sql.ColumnRef)
		if !ok || ref.Name == "*" {
			return nil, false
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, alias) && !strings.EqualFold(ref.Table, tableName) {
			return nil, false
		}
		need = append(need, ref.Name)
	}
	return need, true
}

// indexColumnsForDistinct scans index schema entries for the given table and
// returns the leading index columns restricted to the DISTINCT output columns,
// or nil when no usable index exists.
func indexColumnsForDistinct(e *SelectEngine, entries []*schema.Entry, tableName string, need []string) []string {
	needSet := make(map[string]bool, len(need))
	for _, n := range need {
		needSet[strings.ToLower(n)] = true
	}
	// A table entry for the target table, used to derive PRIMARY KEY
	// columns for autoindexes (their CREATE INDEX SQL is empty: SQLite
	// stores no SQL for sqlite_autoindex_* entries).
	var tableEntry *schema.Entry
	for _, entry := range entries {
		if entry.Type == "table" && strings.EqualFold(entry.Name, tableName) {
			tableEntry = entry
			break
		}
	}
	for _, entry := range entries {
		if entry.Type != "index" || !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		if order := distinctIndexOrder(e, entry, tableEntry, needSet); len(order) > 0 {
			return order
		}
	}
	return nil
}

// distinctIndexOrder returns the index's leading columns (from its CREATE INDEX
// SQL, or the table's PRIMARY KEY declaration for an autoindex whose SQL is
// empty) restricted to the DISTINCT output column set, or nil if none match.
func distinctIndexOrder(e *SelectEngine, entry *schema.Entry, tableEntry *schema.Entry, needSet map[string]bool) []string {
	cols := e.ctx.ParseIndexColumns(entry.SQL)
	if len(cols) == 0 && tableEntry != nil && strings.HasPrefix(strings.ToUpper(entry.Name), "SQLITE_AUTOINDEX_") {
		cols = PKColumnNames(tableEntry.SQL, e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL))
	}
	if len(cols) == 0 {
		return nil
	}
	// The index must cover EVERY needed column (a covering-index scan). If
	// any referenced column is not in the index, the scan cannot produce the
	// full row data, so the index order does not apply (e.g. b1(one PRIMARY
	// KEY, two): SELECT ... GROUP BY (one==2 OR two=='o') references two,
	// which the one-only PK index does not cover — SQLite uses a table scan
	// there, keeping insertion order).
	covered := make(map[string]bool, len(cols))
	for _, c := range cols {
		covered[strings.ToLower(strings.TrimSpace(c))] = true
	}
	for n := range needSet {
		if !covered[n] {
			return nil
		}
	}
	var order []string
	for _, c := range cols {
		name := strings.ToLower(strings.TrimSpace(c))
		if needSet[name] {
			order = append(order, strings.TrimSpace(c))
		}
	}
	return order
}

// scanTableRows iterates over all cells, applies WHERE, builds output rows.
func (e *SelectEngine) scanTableRows(cursor *btree.Cursor, s *sql.SelectStmt, colDefs []sql.ColumnDef, needMaps bool) ([][]interface{}, []RowMap, error) {

	st := newScanState(e, s, colDefs, needMaps)
	if err := st.runScan(cursor); err != nil {
		return nil, nil, err
	}
	allRows := st.buildResultRows()
	// PRAGMA reverse_unordered_selects: reverse the scan order of the
	// top-level SELECT when it has no ORDER BY (SQLite's behavior).
	if st.shouldReverse() {
		reverseInterfaces(allRows)
		reverseRowMaps(st.allRowMaps)
	}
	return allRows, st.allRowMaps, nil
}

// runScan drives the row iteration loop: read a cell, decode + filter it, and
// accumulate output rows/maps for rows that pass (or all rows for joins).
func (st *scanState) runScan(cursor *btree.Cursor) error {
	for {
		payload, rowID, err := cursor.ReadCellData()
		if err != nil {
			break
		}
		if cont, err := st.processRow(cursor, payload, rowID); err != nil {
			return err
		} else if !cont {
			break
		}
	}
	return nil
}

// processRow handles a single scanned cell. It decodes and filters the row, then
// builds output when appropriate. Returns (continue, error): continue is false
// when the scan should stop (no more cells), true to keep scanning.
func (st *scanState) processRow(cursor *btree.Cursor, payload []byte, rowID int64) (bool, error) {
	passesWhere, filtered, err := st.decodeAndFilterRow(cursor, payload, rowID)
	if err != nil {
		return false, err
	}
	if filtered {
		return advanceCursor(cursor)
	}
	if st.hasJoins || passesWhere {
		if err := st.appendRowOutput(); err != nil {
			return false, err
		}
	}
	return advanceCursor(cursor)
}

// advanceCursor moves to the next cell. Returns (true, nil) if there is another
// cell to read, or (false, nil) if the scan is exhausted.
func advanceCursor(cursor *btree.Cursor) (bool, error) {
	ok, err := cursor.Next()
	if err != nil || !ok {
		return false, nil
	}
	return true, nil
}

// scanState holds the per-scan configuration and output accumulators for
// scanTableRows, keeping the scan loop body small and low-complexity.
type scanState struct {
	e                      *SelectEngine
	s                      *sql.SelectStmt
	colDefs                []sql.ColumnDef
	hasJoins               bool
	affinityCols           map[string]bool
	reuseSRow              *StructRow
	useLazyDecode          bool
	whereDecodeIndices     map[int]bool
	remainingDecodeIndices map[int]bool
	isSelectStar           bool
	activeColCount         int
	needMaps               bool
	// output accumulators
	outValues    []interface{}
	outRowStarts []int
	nonStarRows  [][]interface{}
	allRowMaps   []RowMap
}

// newScanState builds the scan configuration and reusable buffers for a table
// scan. The StructRow and flat output buffers are reused across all rows to
// avoid per-row allocation.
func newScanState(e *SelectEngine, s *sql.SelectStmt, colDefs []sql.ColumnDef, needMaps bool) *scanState {
	hasJoins := len(s.Joins) > 0
	affinityCols := e.scanTableAffinityCols(s, colDefs, needMaps)
	// Build shared column index for StructRow lookups (avoids per-row map allocation).
	colIndex := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	activeColCount := countActiveColumns(colDefs)
	// Lazy decode only decodes WHERE-referenced columns first. But if the WHERE
	// contains subqueries (EXISTS, scalar), the subquery may reference any column
	// of the outer row, so we must decode all columns upfront.
	whereHasSubquery := s.Where != nil && exprHasSubquery(s.Where)
	useLazyDecode := s.Where != nil && !hasJoins && !whereHasSubquery
	var whereDecodeIndices, remainingDecodeIndices map[int]bool
	if useLazyDecode {
		whereDecodeIndices, remainingDecodeIndices = scanLazyDecodeIndices(colDefs, colIndex, affinityCols)
	}
	return &scanState{
		e:                      e,
		s:                      s,
		colDefs:                colDefs,
		hasJoins:               hasJoins,
		affinityCols:           affinityCols,
		reuseSRow:              &StructRow{Values: make([]interface{}, len(colDefs)), Index: colIndex},
		useLazyDecode:          useLazyDecode,
		whereDecodeIndices:     whereDecodeIndices,
		remainingDecodeIndices: remainingDecodeIndices,
		isSelectStar:           isSelectStarQuery(s, hasJoins),
		activeColCount:         activeColCount,
		needMaps:               needMaps,
		// Pre-allocate a flat slice for SELECT * to avoid per-row make() calls.
		outValues:    make([]interface{}, 0, 1024*activeColCount),
		outRowStarts: make([]int, 0, 1024),
	}
}

// decodeAndFilterRow decodes the current row's columns and evaluates WHERE.
// Returns (passesWhere, filtered, err). filtered is true only in the lazy-decode
// path when the row fails WHERE early (remaining columns are not decoded); the
// caller must advance the cursor and continue in that case.
func (st *scanState) decodeAndFilterRow(cursor *btree.Cursor, payload []byte, rowID int64) (passesWhere, filtered bool, err error) {
	// Parse header ONCE per row — parseRecordSerialTypes uses a stack buffer to
	// avoid the heap allocation of ParseRecordHeader (saves ~40% of total alloc bytes).
	serialTypes, dataStart := parseRecordSerialTypes(payload)
	if st.useLazyDecode {
		return st.decodeRowLazy(cursor, payload, dataStart, rowID, serialTypes)
	}
	return st.decodeRowFull(cursor, payload, dataStart, rowID, serialTypes)
}

// decodeRowLazy is the two-phase lazy decode: decode only WHERE-referenced
// columns (phase 1), evaluate WHERE, and if filtered return early so the
// remaining (expensive) columns are never decoded. Otherwise decode the rest
// (phase 2) using the cached serial types.
func (st *scanState) decodeRowLazy(cursor *btree.Cursor, payload []byte, dataStart int, rowID int64, serialTypes []uint64) (bool, bool, error) {
	st.e.fillStructRowFromTypes(st.reuseSRow, payload, dataStart, st.colDefs, rowID, st.affinityCols, serialTypes, st.whereDecodeIndices)
	passesWhere, err := st.evalRowWhere(cursor)
	if err != nil {
		return false, false, err
	}
	if !passesWhere {
		return false, true, nil // filtered — skip decoding remaining columns
	}
	st.e.fillStructRowRemainingFromTypes(st.reuseSRow, payload, dataStart, st.colDefs, serialTypes, st.remainingDecodeIndices)
	return true, false, nil
}

// decodeRowFull decodes all columns at once, then evaluates WHERE.
func (st *scanState) decodeRowFull(cursor *btree.Cursor, payload []byte, dataStart int, rowID int64, serialTypes []uint64) (bool, bool, error) {
	st.e.fillStructRowFromTypes(st.reuseSRow, payload, dataStart, st.colDefs, rowID, st.affinityCols, serialTypes, nil)
	passesWhere, err := st.evalRowWhere(cursor)
	return passesWhere, false, err
}

// evalRowWhere evaluates the WHERE predicate against the current row. Returns
// true (pass) when there is no WHERE clause to evaluate here (joins defer WHERE
// to later join processing).
func (st *scanState) evalRowWhere(cursor *btree.Cursor) (bool, error) {
	if st.hasJoins || st.s.Where == nil {
		return true, nil
	}
	return st.e.rowPassesWhere(st.s.Where, st.reuseSRow, cursor)
}

// appendRowOutput builds the output for the current row. For SELECT * it copies
// values into the pre-allocated flat slice (fast path); otherwise it allocates a
// row via buildOutputRow. Row maps are accumulated when needed. In a JOIN, the
// scan produces only the first table's columns — output rows are rebuilt from
// the full joined row maps by execJoins afterwards, so skip the (potentially
// error-raising) per-row expression evaluation here to avoid evaluating
// expressions against a row missing the joined tables' columns.
func (st *scanState) appendRowOutput() error {
	if st.isSelectStar {
		st.outRowStarts = append(st.outRowStarts, len(st.outValues))
		st.outValues = appendScanStarValues(st.outValues, st.colDefs, st.reuseSRow.Values, st.affinityCols != nil)
	} else if !st.hasJoins {
		row, err := st.e.buildOutputRow(st.s.Columns, st.colDefs, st.reuseSRow)
		if err != nil {
			return err
		}
		st.nonStarRows = append(st.nonStarRows, row)
	}
	if st.needMaps {
		st.allRowMaps = append(st.allRowMaps, StructRowToMap(st.reuseSRow))
	}
	return nil
}

// buildResultRows assembles the final row slice: SELECT * rows from the flat
// buffer first, then any individually-allocated (non-star) rows.
func (st *scanState) buildResultRows() [][]interface{} {
	totalStarRows := len(st.outRowStarts)
	allRows := make([][]interface{}, totalStarRows+len(st.nonStarRows))
	for i, start := range st.outRowStarts {
		allRows[i] = st.outValues[start : start+st.activeColCount : start+st.activeColCount]
	}
	copy(allRows[totalStarRows:], st.nonStarRows)
	return allRows
}

// shouldReverse reports whether the top-level, no-ORDER-BY scan should be
// reversed (PRAGMA reverse_unordered_selects, SQLite behavior).
func (st *scanState) shouldReverse() bool {
	return st.e.ctx.ReverseUnordered() && len(st.s.OrderBy) == 0 && st.e.selectDepth == 1 && !st.hasJoins
}

// isSelectStarQuery reports whether s is a simple "SELECT *" (single star
// column, no joins), eligible for the fast output path.
func isSelectStarQuery(s *sql.SelectStmt, hasJoins bool) bool {
	if hasJoins || len(s.Columns) != 1 {
		return false
	}
	ref, ok := s.Columns[0].Expr.(*sql.ColumnRef)
	return ok && ref.Name == "*"
}

// countActiveColumns counts the non-dropped column definitions.
func countActiveColumns(colDefs []sql.ColumnDef) int {
	n := 0
	for _, cd := range colDefs {
		if !cd.Dropped {
			n++
		}
	}
	return n
}

// reverseInterfaces reverses a slice of interface rows in place.
func reverseInterfaces(rows [][]interface{}) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}

// reverseRowMaps reverses a slice of row maps in place.
func reverseRowMaps(maps []RowMap) {
	for i, j := 0, len(maps)-1; i < j; i, j = i+1, j-1 {
		maps[i], maps[j] = maps[j], maps[i]
	}
}

// scanTableAffinityCols collects the column names that need affinity wrappers
// from the WHERE clause, SELECT columns, ORDER BY, and join ON/USING/NATURAL
// references (columns compared with affinity must wrap their values).
func (e *SelectEngine) scanTableAffinityCols(s *sql.SelectStmt, colDefs []sql.ColumnDef, needMaps bool) map[string]bool {
	a := &affinityCollector{cols: make(map[string]bool)}
	// Collect column references from the WHERE clause.
	a.collectExprRefs(s.Where)
	// Also collect from SELECT columns: expressions like "xt==+xi" need the
	// affinity of xt even when xt is not referenced in WHERE/ORDER BY.
	for _, col := range s.Columns {
		a.collectExpr(col.Expr)
	}
	for _, ob := range s.OrderBy {
		a.collectExpr(ob.Expr)
	}
	// GROUP BY expressions need affinity/collation wrappers too: grouping a
	// NOCASE column must compare values under that collation (b3's
	// 'abc'/'aBC' group together).
	for _, gb := range s.GroupBy {
		a.collectExpr(gb)
	}
	if s.Having != nil {
		a.collectExpr(s.Having)
	}
	// JOIN ON/USING/NATURAL clauses reference columns that need affinity
	// wrappers for the join comparison.
	for i := range s.Joins {
		e.collectJoinAffinity(a, &s.Joins[i], s.From.Name)
	}
	return a.result(colDefs, needMaps)
}

// affinityCollector accumulates column names that need affinity wrappers.
type affinityCollector struct {
	cols map[string]bool
	seen bool // true once any column was collected
}

// collectExpr collects column references from a single expression,
// descending into subquery SELECT bodies (their WHERE and output columns)
// so outer scans wrap the columns a correlated subquery references. This
// mirrors the original collectExprRefs helper the engine used before the
// query extraction.
func (a *affinityCollector) collectExpr(expr sql.Expr) {
	if expr == nil {
		return
	}
	WalkExprFull(expr, func(e sql.Expr) {
		if cr, ok := e.(*sql.ColumnRef); ok {
			a.cols[cr.Name] = true
			a.seen = true
		}
		a.collectSubqueryCols(e)
	})
}

// collectSubqueryCols descends into subquery and EXISTS bodies, collecting
// column references from their WHERE and output columns.
func (a *affinityCollector) collectSubqueryCols(e sql.Expr) {
	if sub, ok := e.(*sql.Subquery); ok && sub.Select != nil {
		a.collectSelectBodyCols(sub.Select)
	}
	if ex, ok := e.(*sql.ExistsExpr); ok && ex.Select != nil {
		a.collectSelectBodyCols(ex.Select)
	}
}

// collectSelectBodyCols collects affinity columns from a subquery's WHERE
// and result columns.
func (a *affinityCollector) collectSelectBodyCols(sel *sql.SelectStmt) {
	if sel.Where != nil {
		a.collectExpr(sel.Where)
	}
	for _, col := range sel.Columns {
		a.collectExpr(col.Expr)
	}
}

// collectExprRefs collects column references from one or more expressions.
func (a *affinityCollector) collectExprRefs(expr sql.Expr) {
	if expr != nil {
		a.collectExpr(expr)
	}
}

// add marks a column name as needing affinity.
func (a *affinityCollector) add(name string) {
	a.cols[name] = true
	a.seen = true
}

// addAll marks all names as needing affinity.
func (a *affinityCollector) addAll(names []string) {
	for _, n := range names {
		a.add(n)
	}
}

// result returns the accumulated affinity set, or nil when nothing was
// collected and needMaps is false. When needMaps is true but nothing was
// collected, all columns need affinity (maps may be used downstream).
func (a *affinityCollector) result(colDefs []sql.ColumnDef, needMaps bool) map[string]bool {
	if a.seen {
		return a.cols
	}
	if !needMaps {
		return nil
	}
	for _, cd := range colDefs {
		a.cols[cd.Name] = true
	}
	return a.cols
}

// collectJoinAffinity collects affinity-requiring columns from a join's ON,
// USING, and (for NATURAL joins) the common columns of both tables.
func (e *SelectEngine) collectJoinAffinity(a *affinityCollector, j *sql.JoinClause, fromTable string) {
	if j.On != nil {
		a.collectExpr(j.On)
	}
	for _, uc := range j.Using {
		a.add(uc)
	}
	if !isNaturalJoinType(j.JoinType) {
		return
	}
	// NATURAL joins compare all common columns; mark the join table's columns
	// and, conservatively, the base FROM table's columns with the same names.
	if names, err := e.tableColumnNames(j.Table.Name); err == nil {
		a.addAll(names)
	}
	if fromTable != "" {
		if names, err := e.tableColumnNames(fromTable); err == nil {
			a.addAll(names)
		}
	}
}

// fastEvalComparison attempts to evaluate a simple BinaryOp comparison
// (ColumnRef OP Literal or Literal OP ColumnRef) without going through the
// full evalExpr → evalComplexExpr → evalBinaryOp chain. Returns (result, true)
// if the fast path was taken, or (false, false) to fall through to the slow path.
func (e *SelectEngine) fastEvalComparison(bop *sql.BinaryOp, row Row) (bool, bool) {
	if !isSimpleComparisonOp(bop.Operator) {
		return false, false
	}

	// Try ColumnRef OP Literal
	if colRef, ok := bop.Left.(*sql.ColumnRef); ok {
		colVal, litVal, ok := e.resolveColRefAndLiteral(colRef, bop.Right, row)
		if !ok {
			return false, false
		}
		return e.compareColumnToLiteral(bop.Operator, colVal, litVal, false), true
	}

	// Try Literal OP ColumnRef
	if colRef, ok := bop.Right.(*sql.ColumnRef); ok {
		colVal, litVal, ok := e.resolveColRefAndLiteral(colRef, bop.Left, row)
		if !ok {
			return false, false
		}
		return e.compareColumnToLiteral(bop.Operator, colVal, litVal, true), true
	}

	return false, false
}

// isSimpleComparisonOp reports whether op is a comparison handled by the fast path.
func isSimpleComparisonOp(op string) bool {
	switch op {
	case ">", "<", ">=", "<=", "=", "<>", "!=":
		return true
	}
	return false
}

// resolveColRefAndLiteral resolves a column reference and a literal operand for
// the fast comparison path. Returns (colVal, litVal, true) when both are usable
// (non-NULL, literal parseable); otherwise (nil, nil, false) to fall through.
func (e *SelectEngine) resolveColRefAndLiteral(colRef *sql.ColumnRef, litExpr sql.Expr, row Row) (interface{}, interface{}, bool) {
	val, exists := fastEvalColRef(colRef, row)
	if !exists || execexpr.IsSQLNull(val) {
		return nil, nil, false // let slow path handle NULL
	}
	litVal, ok := e.evalLiteralFast(litExpr)
	if !ok || litVal == nil {
		return nil, nil, false
	}
	return val, litVal, true
}

// compareColumnToLiteral compares a column value against a literal for the fast
// path, applying int fast-path when both are int64. swapped is true when the
// column is the right operand (Literal OP ColumnRef), reversing operand order.
func (e *SelectEngine) compareColumnToLiteral(op string, colVal, litVal interface{}, swapped bool) bool {
	// Fast path: both int64 — direct comparison without CompareValuesCollate.
	if a, ok := util.UnwrapColumnValue(colVal).(int64); ok {
		if b, ok := litVal.(int64); ok {
			if swapped {
				return applyIntComparison(op, b, a)
			}
			return applyIntComparison(op, a, b)
		}
	}
	if swapped {
		return applyComparisonOp(op, e.ctx.CompareValuesWithCollate(litVal, colVal))
	}
	return applyComparisonOp(op, e.ctx.CompareValuesWithCollate(colVal, litVal))
}

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
