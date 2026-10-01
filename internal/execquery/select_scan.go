package execquery

import (
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
	} else if idxCols := e.partialCoverIndexForDistinct(s); len(idxCols) > 0 {
		// An index whose LEADING columns are a prefix of the DISTINCT
		// columns lets SQLite scan the index for those columns and fetch
		// the rest, so the output follows the index-prefix order
		// (distinct-2.3: i1(a,b) delivers (A,B,C) before (a,b,c)).
		reorderByIndexCols(newRows, newMaps, idxCols)
	}
	// With no usable index SQLite keeps DISTINCT as a FILTER over the chosen
	// scan: rows emerge in scan (first-occurrence) order — DISTINCT does not
	// sort by itself (oracle 3.54: SELECT DISTINCT x FROM h1, h2 ON (x=b)
	// returns One, Four).
	return newRows, newMaps
}

// partialCoverIndexForDistinct returns the leading columns of an explicitly
// created index that form a PREFIX, in order, of the DISTINCT output columns
// (single-table scan). Autoindexes are skipped: their ordering behavior is
// not observable the same way, and reordering by them changes long-green
// results. nil when no index qualifies.
func (e *SelectEngine) partialCoverIndexForDistinct(s *sql.SelectStmt) []string {
	if !distinctIndexApplicable(s) {
		return nil
	}
	tableName, alias := distinctTableAlias(s)
	need, ok := distinctNeededColumns(s, tableName, alias)
	if !ok || len(need) == 0 {
		return nil
	}
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	needLower := make([]string, len(need))
	for i, n := range need {
		needLower[i] = strings.ToLower(n)
	}
	return e.scanDistinctCoveringIndex(entries, tableName, needLower)
}

// scanDistinctCoveringIndex returns the leading index column prefix that
// covers the DISTINCT-needed columns: the first index on the table whose
// leading columns match the needed column list in order (implicit
// autoindexes excluded).
func (e *SelectEngine) scanDistinctCoveringIndex(entries []*schema.Entry, tableName string, needLower []string) []string {
	for _, entry := range entries {
		if entry.Type != "index" || !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(entry.Name), "SQLITE_AUTOINDEX_") {
			continue
		}
		cols := e.ctx.ParseIndexColumns(entry.SQL)
		if prefix := leadingColumnPrefix(cols, needLower); len(prefix) > 0 {
			return prefix
		}
	}
	return nil
}

// leadingColumnPrefix returns the index columns matching needLower in order
// from the start (original-cased), or nil when none match.
func leadingColumnPrefix(cols, needLower []string) []string {
	prefix := make([]string, 0, len(cols))
	for i, c := range cols {
		name := strings.ToLower(strings.TrimSpace(c))
		if i >= len(needLower) || name != needLower[i] {
			break
		}
		prefix = append(prefix, strings.TrimSpace(c))
	}
	return prefix
}

// sortDistinctRows sorts DISTINCT rows by their result columns.
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

// scanTableRowsWithSQL scans with the table's CREATE SQL so WITHOUT ROWID
// index-leaf records (PK-first) can be remapped to declared order. Empty
// createSQL disables the remap (legacy callers without schema context).
// allowPosAgg is the caller's positional-aggregate permission
// (allowPositionalAggRows); the returned aggRows slice is non-nil exactly
// when the scan ran in positional-aggregate mode (its rowMaps stay empty in
// that mode — the aggregate passes consume aggRows instead).
func (e *SelectEngine) scanTableRowsWithSQL(cursor *btree.Cursor, s *sql.SelectStmt, colDefs []sql.ColumnDef, needMaps bool, createSQL string, feed *simpleAggFeed, allowPosAgg bool) ([][]interface{}, []RowMap, []Row, error) {

	st := newScanState(e, s, colDefs, needMaps, feed, allowPosAgg)
	// WITHOUT ROWID tables live in an index btree; the root is an index-leaf
	// (0x0a) while small, and an interior index page (0x02) once the table
	// exceeds one leaf. Both store PK-first records.
	if createSQL != "" {
		switch cursor.RootPageType() {
		case storage.PageTypeLeafIndex, storage.PageTypeInteriorIndex:
			st.wrOrder = wrStorageOrder(createSQL, colDefs)
			// Lazy decode indexes positional slots; a permutation would decode
			// the wrong columns in phase 1, so force full decode under remap.
			st.useLazyDecode = false
		}
	}
	if err := st.runScan(cursor); err != nil {
		return nil, nil, nil, err
	}
	allRows := st.buildResultRows()
	// PRAGMA reverse_unordered_selects: reverse the scan order of the
	// top-level SELECT when it has no ORDER BY (SQLite's behavior).
	if st.shouldReverse() {
		reverseInterfaces(allRows)
		reverseRowMaps(st.allRowMaps)
		reverseRows(st.aggRows)
	}
	// A WHERE-driven index scan emits rows in index-key order: SQLite drives
	// the scan loop from the index (where.c), so the WHERE survivors arrive
	// sorted by the index key (NULLs first, rowid ties) even though the
	// engine filters a table scan (intpkey-2.3.2 "WHERE b<'second'" over
	// index i1(b) emits (hello world, one two), not insertion order).
	// Positional-aggregate scans decline this reorder up front
	// (allowPositionalAggRows), so the map consumer below never sees one.
	if idx := e.indexScanOrderIndex(s); idx != "" {
		e.sortScanRowsIndexOrder(allRows, st.allRowMaps, s.From.Name, idx)
	}
	return allRows, st.allRowMaps, st.aggRows, nil
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
		if st.feed != nil {
			// Simple-aggregate feed: step from the decoded values; no output
			// rows or row maps (execSelectPostScan builds the result).
			if err := st.feed.step(st.reuseSRow.Values, rowID); err != nil {
				return false, err
			}
		} else if err := st.appendRowOutput(); err != nil {
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
	e            *SelectEngine
	s            *sql.SelectStmt
	colDefs      []sql.ColumnDef
	hasJoins     bool
	affinityCols map[string]bool
	// affPlan precomputes the per-column affinity/collation wrapping for the
	// scan (nil exactly when affinityCols is nil, preserving the old gate).
	affPlan *affinityPlan
	// ipkFillIdx holds the INTEGER PRIMARY KEY rowid-alias column indices
	// (always computed; drives the phase-2 rowid refill in lazy decode).
	ipkFillIdx []int
	reuseSRow  *StructRow
	// wrOrder permutes PK-first index-leaf records back to declared order
	// (nil for rowid tables and legacy table-leaf WR roots). Set by
	// execSelectScanPhase via initWROrder; decodeRowFull applies it.
	wrOrder []int
	// whereExpr is the WHERE clause this scan evaluates: s.Where with
	// index-usable LIKE/GLOB terms decorated with their synthesized prefix
	// range (select_like_opt.go). Identical to s.Where when no term qualifies.
	whereExpr              sql.Expr
	useLazyDecode          bool
	whereDecodeIndices     map[int]bool
	remainingDecodeIndices map[int]bool
	isSelectStar           bool
	activeColCount         int
	needMaps               bool
	// bareOutIdx, when non-nil, lists the StructRow slot of every output
	// column of an all-bare-refs SELECT ("SELECT a, b FROM t"): the output
	// row is built by peeling the reused StructRow's slots directly,
	// skipping the per-column EvalExpr dispatch (appendRowOutput).
	bareOutIdx []int
	// feed, when non-nil, is the statement's simple-aggregate feed: surviving
	// rows step it (phase-1 decoded values only) and no output rows or row
	// maps are materialized; execSelectPostScan builds the aggregate result.
	feed *simpleAggFeed
	// aggConsumesRows marks statements whose output is rebuilt from the
	// scanned row maps by the aggregate / GROUP BY / window passes, so a
	// per-row output row built during the scan would be discarded work.
	aggConsumesRows bool
	// posAgg marks scans running in positional-aggregate mode: surviving rows
	// are retained as StructRow clones in aggRows (index-addressed, shared
	// column index) instead of per-row RowMaps. The aggregate passes read
	// them through the Row interface; the consumers that demand name-keyed
	// maps (window pass, nested aggregates, correlated outer sets) materialize
	// them lazily. Gated to exactly the shapes whose scan output only the
	// aggregate passes consume (see allowPositionalAggRows).
	posAgg bool
	// aggRows accumulates the positional rows of a posAgg scan; aggArena
	// chunks the per-row value slices (cloneReuseSRow) so retention costs one
	// small allocation per row.
	aggRows  []Row
	aggArena []interface{}
	// serialTypesBuf is the scan's reusable record-header type buffer
	// (parseRecordSerialTypesInto), reused across all rows of the scan.
	serialTypesBuf []uint64
	// output accumulators
	outValues    []interface{}
	outRowStarts []int
	nonStarRows  [][]interface{}
	allRowMaps   []RowMap
}

// allowPositionalAggRows reports whether the caller (execSelectScanPhase)
// permits positional-aggregate retention for this scan: no WITHOUT ROWID PK
// reorder (it sorts the scan's row maps), no schema-table post-filter (it
// consumes the maps), no WHERE-driven index-order reorder (same), no join
// (the join pass rebuilds maps), and no outer/correlated aggregate context
// (execSelectOuterAgg / execSelectCorrelatedAgg consume the maps first).
func (e *SelectEngine) allowPositionalAggRows(s *sql.SelectStmt, tableEntry *schema.Entry, withoutRowidPKCols []string) bool {
	if len(withoutRowidPKCols) > 0 || IsSchemaTable(tableEntry.Name) {
		return false
	}
	if len(s.Joins) > 0 || s.From.Subquery != nil {
		return false
	}
	if e.indexScanOrderIndex(s) != "" {
		return false
	}
	return e.outerRow == nil && len(e.OuterRows()) == 0
}

// newScanState builds the scan configuration and reusable buffers for a table
// scan. The StructRow and flat output buffers are reused across all rows to
// avoid per-row allocation. allowPosAgg is the caller's positional-retention
// permission (allowPositionalAggRows); the scan adds its own statement-shape
// gates on top.
func newScanState(e *SelectEngine, s *sql.SelectStmt, colDefs []sql.ColumnDef, needMaps bool, feed *simpleAggFeed, allowPosAgg bool) *scanState {
	hasJoins := len(s.Joins) > 0
	if feed != nil {
		// Feed mode materializes no rows or row maps: the map-driven
		// all-columns affinity fallback can never apply.
		needMaps = false
	}
	affinityCols := e.scanTableAffinityCols(s, colDefs, needMaps)
	// Build shared column index for StructRow lookups (avoids per-row map allocation).
	colIndex := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	activeColCount := countActiveColumns(colDefs)
	isSelectStar := isSelectStarQuery(s, hasJoins)
	// Lazy decode only decodes WHERE-referenced columns first. But if the WHERE
	// contains subqueries (EXISTS, scalar), the subquery may reference any column
	// of the outer row, so we must decode all columns upfront.
	whereHasSubquery := s.Where != nil && exprHasSubquery(s.Where)
	useLazyDecode := s.Where != nil && !hasJoins && !whereHasSubquery
	var whereDecodeIndices, remainingDecodeIndices map[int]bool
	if useLazyDecode {
		whereDecodeIndices, remainingDecodeIndices = scanLazyDecodeIndices(colDefs, colIndex, affinityCols)
	}
	// LIKE-optimization range synthesis (whereexpr.c exprAnalyze): decorate
	// index-usable LIKE/GLOB conjuncts for this scan only (s.Where itself is
	// left untouched for EXPLAIN and other consumers).
	whereExpr := s.Where
	if !hasJoins && whereExpr != nil {
		whereExpr = e.likeOptimizedScanWhere(s, colDefs, whereExpr)
	}
	// Feed mode wraps only WHERE-referenced columns (the WHERE evaluation
	// consumes the wrappers; the feed steps raw values) PLUS the INTEGER
	// PRIMARY KEY rowid-alias columns: the affinity plan performs their
	// stored-NULL → rowid substitution, which is a value fill, not a
	// comparison wrapper — dropping it would feed NULL to the aggregates.
	wrapCols := affinityCols
	if feed != nil {
		wrapCols = e.aggFeedWrapCols(s.Where, colDefs)
	}
	aggConsumes := scanConsumedByAggPass(e, s)
	// Plain window scans (window functions, no GROUP BY / aggregates) fall
	// through to execWindowPass over the scanned row maps: they keep the map
	// path. Window-over-GROUP-BY shapes stay positional — the group passes
	// materialize their maps per group for the window run. A correlated-
	// aggregate subquery column re-evaluates the columns over the scanned
	// row maps first (execSelectCorrelatedAgg): the feed excludes that shape
	// for the same reason.
	posAgg := allowPosAgg && feed == nil && !hasJoins && needMaps && aggConsumes &&
		!e.selectHasWindowFuncs(s.Columns) && !e.hasSubqueryWithCorrelatedAgg(s.Columns)
	return &scanState{
		e:                      e,
		s:                      s,
		colDefs:                colDefs,
		hasJoins:               hasJoins,
		whereExpr:              whereExpr,
		affinityCols:           affinityCols,
		affPlan:                newAffinityPlan(colDefs, wrapCols),
		ipkFillIdx:             ipkAliasIndices(colDefs),
		reuseSRow:              &StructRow{Values: make([]interface{}, len(colDefs)), Index: colIndex},
		useLazyDecode:          useLazyDecode,
		whereDecodeIndices:     whereDecodeIndices,
		remainingDecodeIndices: remainingDecodeIndices,
		isSelectStar:           isSelectStar,
		bareOutIdx:             bareOutputSlots(s, colDefs),
		activeColCount:         activeColCount,
		needMaps:               needMaps && !posAgg,
		feed:                   feed,
		aggConsumesRows:        aggConsumes,
		posAgg:                 posAgg,
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
	// Parse header ONCE per row into the scan's reusable type buffer — the
	// types are consumed within this row's decode (fill + WHERE + refill),
	// never retained, so one buffer serves the whole scan.
	var dataStart int
	st.serialTypesBuf, dataStart, err = parseRecordSerialTypesInto(payload, st.serialTypesBuf[:0])
	if err != nil {
		return false, false, err
	}
	if st.useLazyDecode {
		return st.decodeRowLazy(cursor, payload, dataStart, rowID, st.serialTypesBuf)
	}
	return st.decodeRowFull(cursor, payload, dataStart, rowID, st.serialTypesBuf)
}

// decodeRowLazy is the two-phase lazy decode: decode only WHERE-referenced
// columns (phase 1), evaluate WHERE, and if filtered return early so the
// remaining (expensive) columns are never decoded. Otherwise decode the rest
// (phase 2) using the cached serial types.
func (st *scanState) decodeRowLazy(cursor *btree.Cursor, payload []byte, dataStart int, rowID int64, serialTypes []uint64) (bool, bool, error) {
	st.e.fillStructRowFromTypes(st.reuseSRow, payload, dataStart, st.colDefs, rowID, st.affPlan, serialTypes, st.whereDecodeIndices, nil)
	passesWhere, err := st.evalRowWhere(cursor)
	if err != nil {
		return false, false, err
	}
	if !passesWhere {
		return false, true, nil // filtered — skip decoding remaining columns
	}
	// Feed mode consumes only the phase-1-decoded columns (every statement
	// reference is in the WHERE-referenced index set): skip the refill.
	if st.feed != nil {
		return true, false, nil
	}
	st.e.fillStructRowRemainingFromTypes(st.reuseSRow, payload, dataStart, st.colDefs, serialTypes, st.remainingDecodeIndices, st.ipkFillIdx)
	return true, false, nil
}

// decodeRowFull decodes all columns at once, then evaluates WHERE.
func (st *scanState) decodeRowFull(cursor *btree.Cursor, payload []byte, dataStart int, rowID int64, serialTypes []uint64) (bool, bool, error) {
	// wrOrder drives the PK-first → declared permutation inside the fill
	// (before affinity/defaults), so the row is fully declared-order here.
	st.e.fillStructRowFromTypes(st.reuseSRow, payload, dataStart, st.colDefs, rowID, st.affPlan, serialTypes, nil, st.wrOrder)
	passesWhere, err := st.evalRowWhere(cursor)
	return passesWhere, false, err
}

// evalRowWhere evaluates the WHERE predicate against the current row. Returns
// true (pass) when there is no WHERE clause to evaluate here (joins defer WHERE
// to later join processing).
func (st *scanState) evalRowWhere(cursor *btree.Cursor) (bool, error) {
	if st.hasJoins || st.whereExpr == nil {
		return true, nil
	}
	return st.e.rowPassesWhere(st.whereExpr, st.reuseSRow, cursor)
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
		if !st.aggConsumesRows {
			st.outRowStarts = append(st.outRowStarts, len(st.outValues))
			st.outValues = appendScanStarValues(st.outValues, st.colDefs, st.reuseSRow.Values, st.affinityCols != nil)
		}
	} else if st.bareOutIdx != nil && !st.hasJoins && !st.aggConsumesRows {
		// All-bare-refs SELECT: peel the reused StructRow's slots directly
		// (evalColumnRef's in-row hit returns the same slot value, and both
		// appendOutputExpr and the unwrap here peel the identical wrapper
		// chain — the fast row is byte-identical to buildOutputRow's).
		values := st.reuseSRow.Values
		row := make([]interface{}, len(st.bareOutIdx))
		for i, slot := range st.bareOutIdx {
			row[i] = unwrapCollatedValue(util.UnwrapColumnValue(values[slot]))
		}
		st.nonStarRows = append(st.nonStarRows, row)
	} else if !st.hasJoins && !st.aggConsumesRows {
		row, err := st.e.buildOutputRow(st.s.Columns, st.colDefs, st.reuseSRow)
		if err != nil {
			return err
		}
		st.nonStarRows = append(st.nonStarRows, row)
	}
	if st.posAgg {
		st.aggRows = append(st.aggRows, st.cloneReuseSRow())
		return nil
	}
	if st.needMaps {
		st.allRowMaps = append(st.allRowMaps, StructRowToMap(st.reuseSRow))
	}
	return nil
}

// cloneReuseSRow retains the current row as a StructRow clone: the value
// slice is copied out of the reused row buffer (the next fill REPLACES slot
// contents, never mutates them, but the backing array is shared), with blob
// payloads deep-copied under exactly the map path's retention discipline
// (rowMapValue). Value slices are carved out of arena chunks so retention
// costs one small allocation per row (the StructRow header).
func (st *scanState) cloneReuseSRow() *StructRow {
	n := len(st.reuseSRow.Values)
	if cap(st.aggArena)-len(st.aggArena) < n {
		st.aggArena = make([]interface{}, 0, 512*n)
	}
	start := len(st.aggArena)
	st.aggArena = append(st.aggArena, st.reuseSRow.Values...)
	vals := st.aggArena[start : start+n : start+n]
	// Blob payloads are deep-copied under exactly the map path's retention
	// discipline (rowMapValue); every other payload (scalars, fresh wrapper
	// pointers) is exclusive to this row and shared as-is.
	for i, v := range vals {
		switch t := v.(type) {
		case []byte:
			b := make([]byte, len(t))
			copy(b, t)
			vals[i] = b
		case *util.ColumnValue:
			if _, isBlob := t.Value.([]byte); isBlob {
				vals[i] = rowMapValue(v)
			}
		case *CollatedValue:
			if cv, ok := t.Value.(*util.ColumnValue); ok {
				if _, isBlob := cv.Value.([]byte); isBlob {
					vals[i] = rowMapValue(v)
				}
			}
		}
	}
	return &StructRow{Values: vals, Index: st.reuseSRow.Index, RowID: st.reuseSRow.RowID}
}

// bareOutputSlots lists the reused StructRow slot of every output column of
// an all-bare-refs SELECT, or nil when the fast output path does not apply.
// A column disqualifies the whole statement when it is not a plain
// unqualified, non-star, non-keyword column reference resolving to a
// non-generated stored slot (exact or case-variant), or when the statement
// declares any SELECT alias — an alias can shadow a later output column of
// the same name and change evalColumnRef's resolution order.
func bareOutputSlots(s *sql.SelectStmt, colDefs []sql.ColumnDef) []int {
	if len(s.Columns) == 0 || len(selectAliasMap(s)) > 0 {
		return nil
	}
	slots := make([]int, 0, len(s.Columns))
	for _, col := range s.Columns {
		ref, ok := unwrapParenExpr(col.Expr).(*sql.ColumnRef)
		if !ok || ref.Name == "*" || ref.Table != "" || groupByKeywordName(ref.Name) {
			return nil
		}
		slot := -1
		for i := range colDefs {
			if colDefs[i].Name == ref.Name && colDefs[i].Generated == nil && !colDefs[i].Dropped {
				slot = i
				break
			}
		}
		if slot < 0 {
			// Case-variant reference: evalColumnRef falls back to a
			// case-insensitive index scan, so the resolved slot evaluates
			// identically.
			for i := range colDefs {
				if strings.EqualFold(colDefs[i].Name, ref.Name) && colDefs[i].Generated == nil && !colDefs[i].Dropped {
					slot = i
					break
				}
			}
		}
		if slot < 0 {
			return nil
		}
		slots = append(slots, slot)
	}
	return slots
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
	// affinity of xt even when xt is not referenced in WHERE/ORDER BY. A bare
	// output column reference (SELECT c) is exempt: every output builder peels
	// the wrappers off its slot value, so the wrapper never reaches a
	// comparison — skipping it saves one allocation per row on the
	// full-scan shapes. The declared-collation exception keeps the
	// CollatedValue marker alive for consumers that read the marker off the
	// row value (the GROUP BY key computation groups 'abc'/'aBC' together
	// through a NOCASE column's declared collation).
	for _, col := range s.Columns {
		if !skipBareSelectRef(col.Expr, colDefs) {
			a.collectExpr(col.Expr)
		}
	}
	for _, ob := range s.OrderBy {
		a.collectExpr(ob.Expr)
	}
	// GROUP BY expressions need affinity/collation wrappers too: grouping a
	// NOCASE column must compare values under that collation (b3's
	// 'abc'/'aBC' group together). A bare term over a no-collation column is
	// exempt — the key computation unwraps the evaluated value and reads the
	// collation marker off it, so only a declared collation needs the
	// wrapper to survive (same exception as the SELECT columns above).
	for _, gb := range s.GroupBy {
		if !skipBareSelectRef(gb, colDefs) {
			a.collectExpr(gb)
		}
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

// skipBareSelectRef reports whether a SELECT output column's affinity
// collection can be skipped: the expression is a bare, unqualified, non-star,
// non-keyword column reference resolving (case-insensitively) to a column
// with no non-BINARY declared collation. Only that shape's wrapper never
// reaches a comparison — every output builder peels the wrappers off its slot
// value — so collecting it costs a per-row wrapper allocation for nothing.
// All other expressions keep the historical collect-everything behavior.
func skipBareSelectRef(expr sql.Expr, colDefs []sql.ColumnDef) bool {
	ref, ok := unwrapParenExpr(expr).(*sql.ColumnRef)
	if !ok || ref.Table != "" || groupByKeywordName(ref.Name) {
		return false
	}
	if ref.Name == "*" {
		// A star keeps its historical collection: the collector's seen flag
		// drives appendScanStarValues's output unwrap (the IPK rowid-alias
		// fill leaves a wrapper in the star's slots).
		return false
	}
	for i := range colDefs {
		if strings.EqualFold(colDefs[i].Name, ref.Name) {
			coll := colDefs[i].Collate
			return coll == "" || strings.EqualFold(coll, "BINARY")
		}
	}
	return false // unresolved name: keep the historical wrapper
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
	// A consumed fts5 rank override (rank = '...' — the xFilter 'r'
	// constraint) never filters rows (execexpr's evalBinaryOp parity for
	// this fast path).
	if (bop.Operator == "=" || bop.Operator == "==") && e.consumesFTS5RankEq(bop) {
		return true, true
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

// consumesFTS5RankEq reports whether the comparison consumes the fts5 rank
// pseudo-column override (rank = '...' with an active fts5 aux context):
// the constraint belongs to the scan's xFilter, not the row filter.
func (e *SelectEngine) consumesFTS5RankEq(bop *sql.BinaryOp) bool {
	ref, ok := bop.Left.(*sql.ColumnRef)
	if !ok || !strings.EqualFold(ref.Name, "rank") {
		return false
	}
	ctxTable, _ := e.ctx.FTS5Aux()
	return ctxTable != ""
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
