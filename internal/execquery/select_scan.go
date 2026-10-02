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
			// The direct column read is keyed on declared ordinals for the
			// same reason.
			st.useLazyDecode = false
			st.directCols = nil
			st.directScratch = nil
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

// runScan drives the row iteration loop: the page-batch walker decodes cells
// page-locally when possible (select_scan_batch.go), and the cursor loop
// finishes whatever the batch declined or could not decode.
func (st *scanState) runScan(cursor *btree.Cursor) error {
	saved, err := st.runScanBatch(cursor)
	if err != nil {
		return err
	}
	if saved {
		// A nested write saved the position mid-scan: step off the saved cell
		// exactly like the cursor loop's Next (restore re-seeks; skipNext
		// returns the next-larger entry), then finish on the cursor loop.
		if _, err := cursor.Next(); err != nil {
			return nil // swallowed like the loop's advanceCursor
		}
	}
	for {
		payload, rowID, err := cursor.ReadCellData()
		if err != nil {
			break
		}
		if err := st.scanRow(payload, rowID); err != nil {
			return err
		}
		if ok, err := cursor.Next(); err != nil || !ok {
			break
		}
	}
	return nil
}

// scanRow handles a single scanned cell: decode + filter it, then build
// output when appropriate. Cursor-free so the batch walker and the cursor
// loop share one body; the caller owns advancing. The pure bare-projection
// shape folds decode into output (scanRowBarePassthrough); scanRowDecoded is
// the general pipeline both paths fall back to.
func (st *scanState) scanRow(payload []byte, rowID int64) error {
	if st.barePassthrough {
		return st.scanRowBarePassthrough(payload, rowID)
	}
	return st.scanRowDecoded(payload, rowID)
}

// scanRowDecoded runs the general decode → WHERE → feed/output pipeline for
// one scanned cell.
func (st *scanState) scanRowDecoded(payload []byte, rowID int64) error {
	passesWhere, filtered, err := st.decodeAndFilterRow(payload, rowID)
	if err != nil {
		return err
	}
	if filtered {
		return nil
	}
	if st.hasJoins || passesWhere {
		if st.feed != nil {
			// Simple-aggregate feed: step from the decoded values; no output
			// rows or row maps (execSelectPostScan builds the result).
			if err := st.feed.step(st.reuseSRow.Values, rowID); err != nil {
				return err
			}
		} else if err := st.appendRowOutput(); err != nil {
			return err
		}
	}
	return nil
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
	// directCols, when non-nil, selects the direct column-read decode
	// (decodeRowDirect): the sorted declared slots whose values the scan's
	// consumers read, decoded straight from the cell payload each row via
	// storage.DecodeRecordColumns. Eligibility in select_scan_direct.go.
	directCols    []int
	directScratch []interface{}
	// directWrap/directIPK are the affinity plan's application restricted to
	// the direct slots: every consumer of the reused StructRow reads a DIRECT
	// slot (the eligibility contract), so plan entries for undecoded slots are
	// dead work — an INTEGER PRIMARY KEY rowid-alias fill for a slot nobody
	// reads allocated a wrapper per row. Semantics match affinityPlan.apply on
	// the slots that survive the restriction (wrap skips NULL, fill replaces a
	// stored NULL with the rowid).
	directWrapIdx  []int
	directWrapAff  []rune
	directWrapColl []string
	directIPKIdx   []int
	directIPKAff   []rune
	directIPKColl  []string
	// directSlotCeil is max(directCols)+1: the prefix header walk's returned
	// count is exact exactly when it is below this ceiling; at or above it
	// every requested slot is present in the record.
	directSlotCeil int
	// barePassthrough marks the pure all-bare-refs scan (initBarePassthrough):
	// decode lands straight in the flat output buffer, skipping the reused
	// row's slot round-trip and the output unwrap.
	barePassthrough   bool
	ipkPassthroughPos []int
	// output accumulators
	outValues    []interface{}
	outRowStarts []int
	// flatStride is the per-row slot count of the flat output buffer: the
	// star column count for SELECT * scans, the bare-ref count for
	// all-bare-refs scans (a statement is one or the other — a star column
	// never qualifies as a bare ref); 0 leaves the rows to buildResultRows'
	// active-column default (star scans).
	flatStride  int
	nonStarRows [][]interface{}
	allRowMaps  []RowMap
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
	plan := e.scanDecodePlan(s, colDefs, colIndex, affinityCols, feed, hasJoins)
	aggConsumes := scanConsumedByAggPass(e, s)
	posAgg := e.positionalAggScan(s, feed, allowPosAgg, hasJoins, needMaps, aggConsumes)
	bareSlots := bareOutputSlots(s, colDefs)
	st := &scanState{
		e:                      e,
		s:                      s,
		colDefs:                colDefs,
		hasJoins:               hasJoins,
		whereExpr:              plan.whereExpr,
		affinityCols:           affinityCols,
		affPlan:                newAffinityPlan(colDefs, plan.wrapCols),
		ipkFillIdx:             ipkAliasIndices(colDefs),
		reuseSRow:              &StructRow{Values: make([]interface{}, len(colDefs)), Index: colIndex},
		useLazyDecode:          plan.useLazyDecode,
		whereDecodeIndices:     plan.whereDecodeIndices,
		remainingDecodeIndices: plan.remainingDecodeIndices,
		isSelectStar:           isSelectStar,
		bareOutIdx:             bareSlots,
		activeColCount:         activeColCount,
		needMaps:               needMaps && !posAgg,
		feed:                   feed,
		aggConsumesRows:        aggConsumes,
		posAgg:                 posAgg,
		// Pre-allocate a flat slice for SELECT * to avoid per-row make() calls.
		outValues:    make([]interface{}, 0, 1024*activeColCount),
		outRowStarts: make([]int, 0, 1024),
	}
	// The flat buffer's per-row stride: the star path appends activeColCount
	// values per row, the bare path len(bareSlots). Both mirror the exact
	// shape their append branch checks, and a statement is never both star
	// and bare (a star column is not a bare ref).
	if isSelectStar && !aggConsumes {
		st.flatStride = activeColCount
	} else if bareSlots != nil && !hasJoins && !aggConsumes {
		st.flatStride = len(bareSlots)
	}
	st.initDirectDecode(feed, plan)
	return st
}

// scanDecodePlan is the per-scan decode/wrap configuration newScanState
// derives from the statement shape: the LIKE-optimized WHERE expression, the
// lazy-decode phase-1/phase-2 index sets, and the affinity wrap set.
type scanDecodePlan struct {
	whereExpr              sql.Expr
	wrapCols               map[string]bool
	useLazyDecode          bool
	whereDecodeIndices     map[int]bool
	remainingDecodeIndices map[int]bool
}

// scanDecodePlan computes the scan's decode/wrap configuration. Lazy decode
// only decodes WHERE-referenced columns first; a WHERE containing subqueries
// (EXISTS, scalar) may reference any column of the outer row, so those scans
// decode all columns upfront. Feed mode runs the lazy two-phase pipeline even
// without a WHERE clause — its phase-1 set is the statement's referenced
// columns (the feed's compiled argument slots among them) and the feed branch
// skips the phase-2 refill, so the record-wide full decode was pure waste.
// Feed mode wraps only WHERE-referenced columns (the WHERE evaluation consumes
// the wrappers; the feed steps raw values) PLUS the INTEGER PRIMARY KEY
// rowid-alias columns: the affinity plan performs their stored-NULL → rowid
// substitution, a value fill, not a comparison wrapper — dropping it would
// feed NULL to the aggregates.
func (e *SelectEngine) scanDecodePlan(s *sql.SelectStmt, colDefs []sql.ColumnDef, colIndex map[string]int, affinityCols map[string]bool, feed *simpleAggFeed, hasJoins bool) scanDecodePlan {
	whereHasSubquery := s.Where != nil && exprHasSubquery(s.Where)
	plan := scanDecodePlan{whereExpr: s.Where, wrapCols: affinityCols}
	plan.useLazyDecode = (s.Where != nil || feed != nil) && !hasJoins && !whereHasSubquery
	if plan.useLazyDecode {
		plan.whereDecodeIndices, plan.remainingDecodeIndices = scanLazyDecodeIndices(colDefs, colIndex, affinityCols)
		// A grouped feed reads its key-term and aggregate-argument slots
		// regardless of the affinity walk's exemptions (a bare GROUP BY term
		// over a no-collation column is exempt from wrapping, not from
		// decoding).
		if feed != nil && feed.group != nil {
			feed.group.unionDecodeSlots(plan.whereDecodeIndices)
		}
	}
	// LIKE-optimization range synthesis (whereexpr.c exprAnalyze): decorate
	// index-usable LIKE/GLOB conjuncts for this scan only (s.Where itself is
	// left untouched for EXPLAIN and other consumers).
	if !hasJoins && plan.whereExpr != nil {
		plan.whereExpr = e.likeOptimizedScanWhere(s, colDefs, plan.whereExpr)
	}
	if feed != nil {
		plan.wrapCols = e.aggFeedWrapCols(s.Where, colDefs)
	}
	return plan
}

// positionalAggScan reports whether the scan retains its rows positionally
// for the aggregate passes. Plain window scans (window functions, no
// GROUP BY / aggregates) fall through to execWindowPass over the scanned
// row maps: they keep the map path. Window-over-GROUP-BY shapes stay
// positional — the group passes materialize their maps per group for the
// window run. A correlated-aggregate subquery column re-evaluates the
// columns over the scanned row maps first (execSelectCorrelatedAgg): the
// feed excludes that shape for the same reason.
func (e *SelectEngine) positionalAggScan(s *sql.SelectStmt, feed *simpleAggFeed, allowPosAgg, hasJoins, needMaps, aggConsumes bool) bool {
	return allowPosAgg && feed == nil && !hasJoins && needMaps && aggConsumes &&
		!e.selectHasWindowFuncs(s.Columns) && !e.hasSubqueryWithCorrelatedAgg(s.Columns)
}

// decodeAndFilterRow decodes the current row's columns and evaluates WHERE.
// Returns (passesWhere, filtered, err). filtered is true only in the lazy-decode
// path when the row fails WHERE early (remaining columns are not decoded); the
// caller advances to the next cell in that case.
func (st *scanState) decodeAndFilterRow(payload []byte, rowID int64) (passesWhere, filtered bool, err error) {
	// Direct column read: decode only the scan's referenced slots straight
	// from the payload (no record-wide boxing). A corrupt payload falls back
	// to the historical decode below, which reproduces its exact
	// silent-truncation semantics on crafted pages.
	if st.directCols != nil {
		if err := st.decodeRowDirect(payload, rowID); err == nil {
			passes, err := st.evalRowWhere()
			return passes, false, err
		}
	}
	// Parse header ONCE per row into the scan's reusable type buffer — the
	// types are consumed within this row's decode (fill + WHERE + refill),
	// never retained, so one buffer serves the whole scan.
	var dataStart int
	st.serialTypesBuf, dataStart, err = parseRecordSerialTypesInto(payload, st.serialTypesBuf[:0])
	if err != nil {
		return false, false, err
	}
	if st.useLazyDecode {
		return st.decodeRowLazy(payload, dataStart, rowID, st.serialTypesBuf)
	}
	return st.decodeRowFull(payload, dataStart, rowID, st.serialTypesBuf)
}

// decodeRowLazy is the two-phase lazy decode: decode only WHERE-referenced
// columns (phase 1), evaluate WHERE, and if filtered return early so the
// remaining (expensive) columns are never decoded. Otherwise decode the rest
// (phase 2) using the cached serial types.
func (st *scanState) decodeRowLazy(payload []byte, dataStart int, rowID int64, serialTypes []uint64) (bool, bool, error) {
	st.e.fillStructRowFromTypes(st.reuseSRow, payload, dataStart, st.colDefs, rowID, st.affPlan, serialTypes, st.whereDecodeIndices, nil)
	passesWhere, err := st.evalRowWhere()
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
func (st *scanState) decodeRowFull(payload []byte, dataStart int, rowID int64, serialTypes []uint64) (bool, bool, error) {
	// wrOrder drives the PK-first → declared permutation inside the fill
	// (before affinity/defaults), so the row is fully declared-order here.
	st.e.fillStructRowFromTypes(st.reuseSRow, payload, dataStart, st.colDefs, rowID, st.affPlan, serialTypes, nil, st.wrOrder)
	passesWhere, err := st.evalRowWhere()
	return passesWhere, false, err
}

// evalRowWhere evaluates the WHERE predicate against the current row. Returns
// true (pass) when there is no WHERE clause to evaluate here (joins defer WHERE
// to later join processing).
func (st *scanState) evalRowWhere() (bool, error) {
	if st.hasJoins || st.whereExpr == nil {
		return true, nil
	}
	return st.e.rowPassesWhere(st.whereExpr, st.reuseSRow, nil)
}

func (st *scanState) buildResultRows() [][]interface{} {
	// flat rows (SELECT * and all-bare-refs scans) carve out of outValues at
	// the scan's per-row slot stride; individually-built rows follow.
	stride := st.flatStride
	if stride == 0 {
		stride = st.activeColCount
	}
	totalFlatRows := len(st.outRowStarts)
	allRows := make([][]interface{}, totalFlatRows+len(st.nonStarRows))
	for i, start := range st.outRowStarts {
		allRows[i] = st.outValues[start : start+stride : start+stride]
	}
	copy(allRows[totalFlatRows:], st.nonStarRows)
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
