// Package exec implements query execution.
//
// This file holds the EXPLAIN QUERY PLAN query planner: choosing the plan
// shape for single-table and multi-table (join) queries, subquery nodes, and
// index-based sort/covering optimizations. Index lookups live in
// explain_index.go.
package execquery

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// planSingleTable computes the plan node for a query over a single table.
func (e *SelectEngine) planSingleTable(t queryTable, s *sql.SelectStmt) string {
	tableName := t.display

	// Get actual row count from table
	nRow := e.tableRowCount(tableName)
	if nRow == 0 {
		nRow = 1000000 // default estimate
	}

	// Collect indexed constraints and conditions for plan output
	bestIndex := ""
	bestEstimate := float64(nRow)
	conditions := "" // formatted as "(col op ? AND col op ?)"
	if s.Where != nil {
		bestIndex, conditions = e.bestIndexForQuery(tableName, s.Where, &bestEstimate)
	}

	// Threshold: if estimated rows is less than ~10% of table, use SEARCH
	threshold := float64(nRow) * 0.10
	if bestIndex != "" && (bestIndex == "PRIMARY KEY" || bestEstimate < threshold) {
		return e.searchPlan(tableName, bestIndex, conditions)
	}

	// ORDER BY / GROUP BY / DISTINCT index optimization: when the sort or
	// dedup columns match an index (covering the output for GROUP BY /
	// DISTINCT), scan the index instead of sorting in a temp b-tree.
	if bestIndex == "" {
		if plan := e.indexScanPlan(t, s); plan != "" {
			return plan
		}
	}

	// Covering index: for COUNT(col) on an indexed column, use the best covering index
	if plan := e.countIndexPlan(t, s); plan != "" {
		return plan
	}

	return fmt.Sprintf("SCAN %s", tableName)
}

// indexScanPlan renders a "SCAN <table> USING [COVERING] INDEX <idx>" node
// for the ORDER BY / GROUP BY / DISTINCT optimization, or "" when no index
// qualifies (a temp b-tree sort is needed instead).
func (e *SelectEngine) indexScanPlan(t queryTable, s *sql.SelectStmt) string {
	if len(s.OrderBy) > 0 {
		if plan := e.orderByIndexPlan(t, s); plan != "" {
			return plan
		}
	}
	if len(s.GroupBy) > 0 || s.Distinct {
		return e.groupDistinctIndexPlan(t, s)
	}
	return ""
}

// searchPlan renders a "SEARCH <table> USING <index> (<conditions>)" node for
// a selective index on a single-table query.
func (e *SelectEngine) searchPlan(tableName, idx, conditions string) string {
	using := e.indexUsingLabel(tableName, idx)
	plan := fmt.Sprintf("SEARCH %s USING %s", tableName, using)
	if conditions != "" {
		plan += " " + conditions
	}
	return plan
}

// orderByIndexPlan renders a "SCAN <table> USING [COVERING] INDEX <idx>" node
// when the ORDER BY columns match an index prefix, or "" when no index
// qualifies (a temp b-tree sort is needed instead). Partial indexes are only
// used when the query WHERE implies the partial-index predicate. When the
// query has WHERE constraints on columns outside the index, the ORDER BY
// index optimisation is skipped (SQLite prefers a full scan + sort).
func (e *SelectEngine) orderByIndexPlan(t queryTable, s *sql.SelectStmt) string {
	obCols := orderByCols(s)
	if len(obCols) == 0 {
		return ""
	}
	idxName := e.findIndexOnColsForQuery(t.display, obCols, s.Where)
	if idxName == "" {
		return ""
	}
	// When the WHERE clause constrains columns that the index does not cover,
	// SQLite does a full table scan + temp sort rather than an index scan
	// with post-filtering.
	if s.Where != nil && e.whereHasNonIndexConstraint(s.Where, t.real, idxName) {
		return ""
	}
	if e.indexCoversCols(idxName, t.real, selectOutputCols(s)) {
		return fmt.Sprintf("SCAN %s USING COVERING INDEX %s", t.display, idxName)
	}
	return fmt.Sprintf("SCAN %s USING INDEX %s", t.display, idxName)
}

// whereHasNonIndexConstraint reports whether the WHERE expression contains a
// column-to-constant constraint on a column that is NOT part of the named
// index AND not part of the index's partial-index predicate. When true,
// SQLite prefers a full table scan + sort over an index scan with
// post-filtering, so the ORDER BY index optimisation is suppressed.
func (e *SelectEngine) whereHasNonIndexConstraint(where sql.Expr, tableName, idxName string) bool {
	idxCols := e.indexColumns(idxName)
	if len(idxCols) == 0 {
		return false
	}
	// For partial indexes, WHERE constraints matching the partial-index
	// predicate are satisfied by the index definition (the index only
	// contains those rows), so they don't count as non-index constraints.
	partialCols := e.partialIndexWhereColumns(idxName)
	found := false
	walkExpr(where, func(e2 sql.Expr) {
		if found {
			return
		}
		be, ok := e2.(*sql.BinaryOp)
		if !ok {
			return
		}
		if be.Operator == "AND" || be.Operator == "OR" {
			return
		}
		colRef, constVal := findColAndConst(be)
		if colRef == nil || constVal == nil {
			return
		}
		if containsFold(idxCols, colRef.Name) {
			return
		}
		if containsFold(partialCols, colRef.Name) {
			return
		}
		found = true
	})
	return found
}

// groupDistinctIndexPlan renders a "SCAN <table> USING COVERING INDEX <idx>"
// node when the GROUP BY / DISTINCT columns match an index that also covers
// every output column, or "" when no such index exists.
func (e *SelectEngine) groupDistinctIndexPlan(t queryTable, s *sql.SelectStmt) string {
	var cols []string
	if len(s.GroupBy) > 0 {
		cols = groupByCols(s)
	} else {
		cols = distinctCols(s)
	}
	if len(cols) == 0 {
		return ""
	}
	idxName := e.findIndexOnCols(t.display, cols)
	if idxName == "" || !e.indexCoversCols(idxName, t.real, selectOutputCols(s)) {
		return ""
	}
	return fmt.Sprintf("SCAN %s USING COVERING INDEX %s", t.display, idxName)
}

// countIndexPlan renders an "INDEX <idx>" node for COUNT(col) when a covering
// index on the counted column exists, or "" otherwise.
func (e *SelectEngine) countIndexPlan(t queryTable, s *sql.SelectStmt) string {
	if len(s.Columns) != 1 {
		return ""
	}
	fn, ok := s.Columns[0].Expr.(*sql.FuncCall)
	if !ok || strings.ToUpper(fn.Name) != "COUNT" || len(fn.Args) != 1 {
		return ""
	}
	colRef, ok := fn.Args[0].(*sql.ColumnRef)
	if !ok {
		return ""
	}
	bestCoverIdx := e.findBestCoveringIndex(t.display, colRef.Name)
	if bestCoverIdx != "" {
		return fmt.Sprintf("INDEX %s", bestCoverIdx)
	}
	return ""
}

// planJoin computes one plan node per joined table. The driving table is the
// one with constant predicates and the smallest estimated row count; inner
// tables are placed in dependency order, using an index SEARCH when a join
// column is indexed.
func (e *SelectEngine) planJoin(tables []queryTable, s *sql.SelectStmt) []planNode {
	preds := joinPredicates(s)

	// constPreds counts constant predicates (col = literal) per table; these
	// drive the join order even when the column is not indexed.
	constPreds := make([]int, len(tables))
	joinRefs := make([][]joinRef, len(tables))
	for _, p := range preds {
		e.collectJoinPredicate(p, tables, constPreds, joinRefs)
	}

	// Driving table: among tables with constant predicates, the smallest.
	driver := e.joinDriver(tables, constPreds, joinRefs)

	planned := []string{tables[driver].display}
	remaining := make([]int, 0, len(tables)-1)
	for i := range tables {
		if i != driver {
			remaining = append(remaining, i)
		}
	}

	nodes := e.joinNodeFor(tables[driver], nil, joinRefs[driver], s)
	return e.planJoinRemaining(nodes, tables, remaining, planned, joinRefs, s)
}

// joinPredicates returns the flattened conjuncts of a SELECT's WHERE clause
// and every JOIN's ON clause.
func joinPredicates(s *sql.SelectStmt) []sql.Expr {
	var preds []sql.Expr
	if s.Where != nil {
		preds = append(preds, splitAnd(s.Where)...)
	}
	for _, j := range s.Joins {
		if j.On != nil {
			preds = append(preds, splitAnd(j.On)...)
		}
	}
	return preds
}

// collectJoinPredicate classifies one equality predicate: a constant
// predicate increments the table's constPreds counter; a column-to-column
// predicate records the join refs (when either side is indexed).
func (e *SelectEngine) collectJoinPredicate(p sql.Expr, tables []queryTable, constPreds []int, joinRefs [][]joinRef) {
	bin, ok := p.(*sql.BinaryOp)
	if !ok || !strings.EqualFold(bin.Operator, "=") {
		return
	}
	if colRef, constVal := findColAndConst(bin); colRef != nil && constVal != nil {
		if ti := e.resolveJoinTable(tables, colRef); ti >= 0 {
			constPreds[ti]++
		}
		return
	}
	e.collectJoinColumnPair(bin, tables, joinRefs)
}

// collectJoinColumnPair records the join refs for an equality predicate
// between two columns of different tables.
func (e *SelectEngine) collectJoinColumnPair(bin *sql.BinaryOp, tables []queryTable, joinRefs [][]joinRef) {
	left, okL := bin.Left.(*sql.ColumnRef)
	right, okR := bin.Right.(*sql.ColumnRef)
	if !okL || !okR {
		return
	}
	li := e.resolveJoinTable(tables, left)
	ri := e.resolveJoinTable(tables, right)
	if li < 0 || ri < 0 || li == ri {
		return
	}
	e.addJoinRefPair(tables, joinRefs, li, ri, left, right)
}

// addJoinRefPair records the join refs for both sides of a column-to-column
// equality predicate.
func (e *SelectEngine) addJoinRefPair(tables []queryTable, joinRefs [][]joinRef, li, ri int, left, right *sql.ColumnRef) {
	e.maybeAddJoinRef(tables, joinRefs, li, left.Name, ri, right.Name)
	e.maybeAddJoinRef(tables, joinRefs, ri, right.Name, li, left.Name)
}

// maybeAddJoinRef records a joinRef for table ti when its column colName is
// indexed (SEARCH once other is planned), or, when ti's column has no index
// and the other table is not a real schema table (a subquery/derived table),
// records an automatic-index joinRef on the other side.
func (e *SelectEngine) maybeAddJoinRef(tables []queryTable, joinRefs [][]joinRef, ti int, colName string, otherIdx int, otherCol string) {
	if idx := e.findIndexOnColumn(tables[ti].real, colName); idx != "" {
		joinRefs[ti] = append(joinRefs[ti], joinRef{table: tables[ti].display, col: colName, otherTable: tables[otherIdx].display, indexName: idx})
		return
	}
	if !e.isRealTable(tables[otherIdx].real) {
		// The other side is a subquery/derived table: SQLite creates an
		// automatic index on its join column.
		joinRefs[otherIdx] = append(joinRefs[otherIdx], joinRef{table: tables[otherIdx].display, col: otherCol, otherTable: tables[ti].display, indexName: ""})
	}
}

// resolveJoinTable maps a column reference to its table index in the join.
// A qualified ref (t.col) resolves directly; an unqualified ref resolves by
// matching the column name against each table's columns (-1 when ambiguous).
func (e *SelectEngine) resolveJoinTable(tables []queryTable, ref *sql.ColumnRef) int {
	if ref.Table != "" {
		return e.tableIndexByDisplay(tables, ref.Table)
	}
	found := -1
	for i := range tables {
		if e.ctx.TableHasColumn(tables[i].real, ref.Name) {
			if found >= 0 {
				return -1 // ambiguous
			}
			found = i
		}
	}
	return found
}

// joinDriver chooses the driving table of a join: among tables with constant
// predicates the one with the smallest estimated row count; with no constant
// predicates, the table with the fewest indexed join connections (so tables
// with useful join indexes become SEARCHed inner tables). Matches SQLite's
// choice for e.g. "FROM t2, t1 WHERE a=z AND c=x" where t2's index covers
// both predicates: scan t1, search t2.
func (e *SelectEngine) joinDriver(tables []queryTable, constPreds []int, joinRefs [][]joinRef) int {
	driver := 0
	bestCnt := int64(0)
	found := false
	for i := range tables {
		if constPreds[i] == 0 {
			continue
		}
		cnt := e.estimatedRowCount(tables[i].real)
		if !found || cnt < bestCnt {
			driver, bestCnt, found = i, cnt, true
		}
	}
	if !found {
		return leastJoinRefs(joinRefs)
	}
	return driver
}

// leastJoinRefs returns the table index with the fewest indexed join
// connections.
func leastJoinRefs(joinRefs [][]joinRef) int {
	best := -1
	for i := range joinRefs {
		if best < 0 || len(joinRefs[i]) < len(joinRefs[best]) {
			best = i
		}
	}
	return best
}

// planJoinRemaining orders the non-driving tables by indexed join
// connectivity, emitting each table's node once its join partner is planned.
func (e *SelectEngine) planJoinRemaining(nodes []planNode, tables []queryTable, remaining []int, planned []string, joinRefs [][]joinRef, s *sql.SelectStmt) []planNode {
	for len(remaining) > 0 {
		next := e.nextJoinTable(remaining, joinRefs, planned)
		i := remaining[next]
		remaining = append(remaining[:next], remaining[next+1:]...)
		nodes = append(nodes, e.joinNodeFor(tables[i], planned, joinRefs[i], s)...)
		planned = append(planned, tables[i].display)
	}
	return nodes
}

// nextJoinTable returns the index (into remaining) of the next table to plan:
// the first with an indexed join connection to an already-planned table, or 0
// (original order) when none exists.
func (e *SelectEngine) nextJoinTable(remaining []int, joinRefs [][]joinRef, planned []string) int {
	for k, i := range remaining {
		if e.joinSearchRef(joinRefs[i], planned) != nil {
			return k
		}
	}
	return 0 // no indexed join connection — keep original order
}

// bestIndexForQuery examines the WHERE clause and returns the best index name,
// estimated row count, and formatted column conditions for the plan output.
func (e *SelectEngine) bestIndexForQuery(tableName string, where sql.Expr, estimate *float64) (string, string) {
	// Collect all column references with their operators
	refs := collectIndexedRefs(where, tableName, e)
	if len(refs) == 0 {
		return "", ""
	}
	// Pick the one with the lowest estimate
	bestName := ""
	bestEst := *estimate
	var bestRefs []indexedRef // all refs matching the best index
	for _, ref := range refs {
		est := refEstimate(ref, e.tableRowCount(tableName))
		if est < bestEst {
			bestEst = est
			bestName = ref.indexName
		} else if est == bestEst && ref.indexName != bestName {
			bestName = e.tiebreakIndex(refs, bestName, ref.indexName)
		}
	}
	// Collect all refs for the best index to build conditions. Also include
	// column-to-constant predicates on columns without an index: SQLite's
	// older plans (and the without_rowid1 14.2 test) list every WHERE
	// constraint that narrows the search, e.g. SEARCH ... (a=? AND b=?).
	if bestName != "" {
		bestRefs = refsForBestIndex(refs, where, tableName, bestName)
	}
	*estimate = bestEst
	return bestName, formatConditions(bestRefs)
}

// refEstimate computes the estimated row count for an indexed ref: its
// pre-computed selectivity when present, else the operator heuristic, times
// the table's row count.
func refEstimate(ref indexedRef, rowCount int64) float64 {
	sel := ref.selectivity
	if sel <= 0 {
		sel = estimateSelectivity(ref.constant, ref.op)
	}
	return sel * float64(rowCount)
}

// tiebreakIndex picks between two equally-estimated indexes: the one covering
// more WHERE conditions, then the simpler one (fewer columns). Returns the
// winning index name.
func (e *SelectEngine) tiebreakIndex(refs []indexedRef, bestName, candidateName string) string {
	covCur := e.countRefsForIndex(refs, bestName)
	covNew := e.countRefsForIndex(refs, candidateName)
	if covNew > covCur {
		return candidateName
	}
	if covNew == covCur && e.ctx.IndexColumnCount(candidateName) < e.ctx.IndexColumnCount(bestName) {
		return candidateName
	}
	return bestName
}

// refsForBestIndex returns every indexed ref matching the best index, plus
// every column-to-constant predicate (indexed or not) so the plan lists the
// full set of search constraints. For a WITHOUT ROWID PRIMARY KEY search,
// only PRIMARY KEY columns are listed: SQLite's plan for a PK lookup shows
// exactly the PK constraints, not unrelated WHERE predicates (see
// without_rowid1 14.2).
func refsForBestIndex(refs []indexedRef, where sql.Expr, tableName, bestName string) []indexedRef {
	var bestRefs []indexedRef
	for _, ref := range refs {
		if ref.indexName == bestName {
			bestRefs = append(bestRefs, ref)
		}
	}
	if bestName == "PRIMARY KEY" {
		return bestRefs
	}
	for _, ar := range collectAllColumnRefs(where, tableName) {
		if !bestRefsContain(bestRefs, ar) {
			bestRefs = append(bestRefs, ar)
		}
	}
	return bestRefs
}

// bestRefsContain reports whether bestRefs already has a ref with the same
// column and operator as ar.
func bestRefsContain(bestRefs []indexedRef, ar indexedRef) bool {
	for _, br := range bestRefs {
		if br.colName == ar.colName && br.op == ar.op {
			return true
		}
	}
	return false
}

// planSubqueryNodes returns one plan node per subquery expression in a
// SELECT's WHERE, HAVING, and select list, in SQLite's style: SCALAR SUBQUERY
// n for scalar/EXISTS subqueries (with a CORRELATED prefix when the subquery
// references outer columns), LIST SUBQUERY n for IN, and the subquery's own
// plan nested beneath the node.
//
// EXISTS terms that are top-level WHERE conjuncts and satisfy SQLite's
// EXISTS-to-JOIN conditions (single real table, no aggregate/limit/compound)
// are rendered as join loops — "SEARCH <tbl> EXISTS USING <index> (<col>=?)"
// or "SCAN <tbl> EXISTS" — instead of a SUBQUERY node, matching SQLite's
// select.c existsToJoin() transformation.
func (e *SelectEngine) planSubqueryNodes(s *sql.SelectStmt) []planNode {
	var nodes []planNode
	count := 0
	addSubquery := func(sub *sql.SelectStmt, label string) {
		if sub == nil {
			return
		}
		count++
		if e.subqueryReferencesOuter(sub, s) {
			label = "CORRELATED " + label
		}
		node := planNode{detail: fmt.Sprintf("%s %d", label, count)}
		node.children = e.planSelectMember(sub)
		nodes = append(nodes, node)
	}
	collect := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		WalkExprFull(expr, func(e2 sql.Expr) {
			subqueryNodesFor(e2, expr, addSubquery)
		})
	}
	// The WHERE clause is walked conjunct-by-conjunct (mirroring SQLite's
	// existsToJoin AND recursion): a qualified EXISTS becomes an EXISTS loop
	// and its subtree is pruned; everything else is collected as before.
	e.planWhereSubqueries(s.Where, s, &nodes, addSubquery)
	collect(s.Having)
	for _, col := range s.Columns {
		collect(col.Expr)
	}
	return nodes
}

// planWhereSubqueries walks a WHERE clause conjunct-by-conjunct, rendering
// qualified EXISTS terms as EXISTS join loops (nodes appended directly) and
// collecting every other subquery through add. Unqualified EXISTS and
// non-EXISTS conjuncts keep their subquery nodes.
func (e *SelectEngine) planWhereSubqueries(expr sql.Expr, outer *sql.SelectStmt, nodes *[]planNode, add func(sub *sql.SelectStmt, label string)) {
	if expr == nil {
		return
	}
	if bin, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "AND") {
		e.planWhereSubqueries(bin.Left, outer, nodes, add)
		e.planWhereSubqueries(bin.Right, outer, nodes, add)
		return
	}
	if paren, ok := expr.(*sql.ParenExpr); ok {
		e.planWhereSubqueries(paren.Expr, outer, nodes, add)
		return
	}
	if ex, ok := expr.(*sql.ExistsExpr); ok {
		// A correlated EXISTS stays a subquery node (SQLite reports
		// "CORRELATED SCALAR SUBQUERY n"); only a non-correlated flat EXISTS
		// may become an EXISTS join loop.
		if !e.subqueryReferencesOuter(ex.Select, outer) {
			if exNodes, ok2 := e.existsJoinNode(ex.Select); ok2 {
				*nodes = append(*nodes, exNodes...)
				return // pruned: rendered as an EXISTS loop, no SUBQUERY node
			}
		}
	}
	WalkExprFull(expr, func(e2 sql.Expr) {
		subqueryNodesFor(e2, expr, add)
	})
}

// existsJoinNode renders the EQP node for an EXISTS subquery that SQLite's
// EXISTS-to-JOIN optimization (select.c existsToJoin) would transform into a
// join loop: the inner table becomes a "SEARCH <tbl> EXISTS USING <index>
// (<col>=?)" (or "SCAN <tbl> EXISTS" when no index applies) sibling of the
// outer scans, with no SUBQUERY label. Returns (nil, false) when the subquery
// does not qualify (SQLite keeps it as a subquery).
func (e *SelectEngine) existsJoinNode(sub *sql.SelectStmt) ([]planNode, bool) {
	if sub == nil || sub.From.Name == "" || sub.From.Subquery != nil || len(sub.Joins) > 0 {
		return nil, false
	}
	if sub.Union != nil || sub.Limit != nil || e.hasAggregate(sub) {
		return nil, false
	}
	tableName := sub.From.Name
	col, isRowid, ok := e.existsJoinSearchColumn(sub, tableName)
	if !ok {
		return []planNode{{detail: "SCAN " + tableName + " EXISTS"}}, true
	}
	if isRowid {
		return []planNode{{detail: fmt.Sprintf("SEARCH %s EXISTS USING INTEGER PRIMARY KEY (rowid=?)", tableName)}}, true
	}
	idx := e.findIndexOnColumn(tableName, col, sub.Where)
	if idx == "" {
		return []planNode{{detail: "SCAN " + tableName + " EXISTS"}}, true
	}
	using := e.indexUsingLabel(tableName, idx)
	return []planNode{{detail: fmt.Sprintf("SEARCH %s EXISTS USING %s (%s=?)", tableName, using, col)}}, true
}

// existsJoinSearchColumn finds the indexed inner-table column of the EXISTS
// subquery's WHERE clause that becomes the join key: an equality predicate
// col = <expr> (or rowid = <expr>) where col belongs to the inner table and
// <expr> references an outer row or is a constant. Returns the column name,
// whether it is the rowid, and whether such a predicate exists.
func (e *SelectEngine) existsJoinSearchColumn(sub *sql.SelectStmt, tableName string) (string, bool, bool) {
	if sub.Where == nil {
		return "", false, false
	}
	isInnerRef := e.innerRefChecker(tableName)
	for _, p := range splitAnd(sub.Where) {
		if col, isRowid, ok := existsJoinKeyPredicate(p, tableName, isInnerRef); ok {
			return col, isRowid, true
		}
	}
	return "", false, false
}

// innerRefChecker builds a predicate that reports whether a column reference
// names a column of the inner EXISTS table (or its rowid).
func (e *SelectEngine) innerRefChecker(tableName string) func(*sql.ColumnRef) bool {
	innerCols := map[string]bool{}
	if entry, err := e.ctx.Schema().FindTable(tableName); err == nil {
		for _, c := range e.ctx.ParseColumnDefs(tableName, entry.SQL) {
			innerCols[strings.ToLower(c.Name)] = true
		}
	}
	return func(cr *sql.ColumnRef) bool {
		name := strings.ToLower(cr.Name)
		if isRowIDName(name) {
			return true
		}
		if cr.Table != "" {
			return strings.EqualFold(cr.Table, tableName)
		}
		return innerCols[name]
	}
}

// existsJoinKeyPredicate checks one WHERE conjunct of an EXISTS subquery: an
// equality predicate "innerCol = <expr>" (either side) where the other side is
// not itself an inner column reference (i.e. an outer reference or constant)
// is a valid join key. Returns the inner column name and whether it is the
// rowid.
func existsJoinKeyPredicate(p sql.Expr, tableName string, isInnerRef func(*sql.ColumnRef) bool) (string, bool, bool) {
	bin, ok := p.(*sql.BinaryOp)
	if !ok || !strings.EqualFold(bin.Operator, "=") {
		return "", false, false
	}
	left, lok := bin.Left.(*sql.ColumnRef)
	right, rok := bin.Right.(*sql.ColumnRef)
	if lok && isInnerRef(left) && (!rok || !isInnerRef(right)) {
		return left.Name, isRowIDName(strings.ToLower(left.Name)), true
	}
	if rok && isInnerRef(right) && (!lok || !isInnerRef(left)) {
		return right.Name, isRowIDName(strings.ToLower(right.Name)), true
	}
	return "", false, false
}

// subqueryNodesFor dispatches one walked expression node to the add callback
// with the SQLite subquery label: LIST SUBQUERY for an IN-list subquery,
// SCALAR SUBQUERY for an EXISTS or bare scalar subquery (unless it is a
// direct IN-list member, which walkExprFull visits via the InList node first).
func subqueryNodesFor(e2, root sql.Expr, add func(sub *sql.SelectStmt, label string)) {
	switch v := e2.(type) {
	case *sql.InList:
		// x IN (SELECT ...) — the Subquery is the IN-list operand and is
		// labelled LIST SUBQUERY (a constant list has no plan).
		for _, item := range v.List {
			if sub, ok := item.(*sql.Subquery); ok {
				add(sub.Select, "LIST SUBQUERY")
			}
		}
	case *sql.ExistsExpr:
		add(v.Select, "SCALAR SUBQUERY")
	case *sql.Subquery:
		// A bare (SELECT ...) in an expression is a scalar subquery;
		// IN-list operands were already handled above (walkExprFull visits
		// the InList first, and the inner Subquery node must not be
		// double-counted, so skip Subquery nodes that are direct InList
		// members).
		if !subqueryIsInListMember(root, v) {
			add(v.Select, "SCALAR SUBQUERY")
		}
	}
}

// subqueryReferencesOuter reports whether a subquery's FROM/WHERE/columns
// reference a table of the enclosing SELECT (making it correlated).
func (e *SelectEngine) subqueryReferencesOuter(sub, outer *sql.SelectStmt) bool {
	outerTables := map[string]bool{}
	for _, t := range e.collectQueryTables(outer) {
		outerTables[strings.ToLower(t.display)] = true
		outerTables[strings.ToLower(t.real)] = true
	}
	if len(outerTables) == 0 {
		return false
	}
	correlated := false
	checkCols := func(expr sql.Expr) {
		if expr == nil || correlated {
			return
		}
		WalkExprFull(expr, func(e2 sql.Expr) {
			if refsOuterTable(e2, sub, outerTables, e) {
				correlated = true
			}
		})
	}
	checkCols(sub.Where)
	checkCols(sub.Having)
	for _, c := range sub.Columns {
		checkCols(c.Expr)
	}
	return correlated
}

// refsOuterTable reports whether one walked expression node references an
// outer table: a qualified reference whose table is in outerTables, or an
// unqualified reference whose column no local table exposes.
func refsOuterTable(e2 sql.Expr, sub *sql.SelectStmt, outerTables map[string]bool, e *SelectEngine) bool {
	cr, ok := e2.(*sql.ColumnRef)
	if !ok {
		return false
	}
	// A qualified reference (sub.t.col) can only be outer; an unqualified
	// reference is outer when no local table has the column.
	if cr.Table != "" {
		return outerTables[strings.ToLower(cr.Table)]
	}
	return !e.subqueryHasColumn(sub, cr.Name)
}

// autoindexColumns resolves the indexed columns of a sqlite_autoindex_* entry
// (which has empty SQL) from the table's UNIQUE/PRIMARY KEY constraints. The
// autoindexes are numbered in creation order: column-level UNIQUE and PRIMARY
// KEY constraints first (in column order), then table-level constraints (in
// declaration order), skipping INTEGER PRIMARY KEY rowid aliases and duplicate
// column sets. Returns nil for an unknown name or a non-autoindex.
func (e *SelectEngine) autoindexColumns(tableName, idxName string) []string {
	if !strings.HasPrefix(idxName, "sqlite_autoindex_") {
		return nil
	}
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return nil
	}
	colDefs := e.ctx.ParseColumnDefs(tableName, entry.SQL)
	colIndex := buildColumnIndex(colDefs)
	uniq := collectAutoindexDefs(colDefs, colIndex, e, tableName, entry.SQL)
	seen := map[string]bool{}
	seq := 0
	for _, u := range uniq {
		if skipAutoindexDef(u, colDefs) {
			continue
		}
		key := strings.Join(u.Cols, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		seq++
		if fmt.Sprintf("sqlite_autoindex_%s_%d", tableName, seq) == idxName {
			return u.Cols
		}
	}
	return nil
}

// collectAutoindexDefs collects a table's UNIQUE/PRIMARY KEY constraints in
// autoindex slot order: column-level constraints in column order (INTEGER
// PRIMARY KEY rowid aliases consume no slot), then table-level constraints in
// declaration order.
func collectAutoindexDefs(colDefs []sql.ColumnDef, colIndex map[string]int, e *SelectEngine, tableName, tableSQL string) []UniqDef {
	var uniq []UniqDef
	// Column-level constraints, in column order. INTEGER PRIMARY KEY rowid
	// aliases consume no autoindex slot.
	for _, cd := range colDefs {
		if cd.Unique {
			uniq = append(uniq, UniqDef{Cols: []string{cd.Name}})
		}
		if cd.PrimaryKey && !isIPKRowidAliasCol(cd) {
			uniq = append(uniq, UniqDef{Cols: []string{cd.Name}, IsPK: true})
		}
	}
	// Table-level constraints, in declaration order.
	for _, tc := range e.ctx.TableConstraints(tableName, tableSQL) {
		switch tc.Type {
		case sql.ConstraintUnique:
			uniq = append(uniq, UniqDef{Cols: constraintColumnNames(tc, colIndex, colDefs)})
		case sql.ConstraintPrimaryKey:
			uniq = append(uniq, UniqDef{Cols: constraintColumnNames(tc, colIndex, colDefs), IsPK: true})
		}
	}
	return uniq
}

// skipAutoindexDef reports whether a UNIQUE/PRIMARY KEY def consumes no
// autoindex slot: an INTEGER PRIMARY KEY rowid alias.
func skipAutoindexDef(u UniqDef, colDefs []sql.ColumnDef) bool {
	if !u.IsPK || len(u.Cols) != 1 {
		return false
	}
	cd, ok := findColDefByName(colDefs, u.Cols[0])
	return ok && isIPKRowidAliasCol(cd)
}

// coveringCandidate is one candidate covering index: its name, key-column
// count, and sz hint from ANALYZE statistics.
type coveringCandidate struct {
	name string
	cols int
	sz   int
}

// findBestCoveringIndex finds the best index that covers a column for a covering scan.
// It prefers indexes with fewer columns, then uses sz hint from stat data as tiebreaker.
func (e *SelectEngine) findBestCoveringIndex(tableName, colName string) string {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return ""
	}
	var candidates []coveringCandidate
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName {
			if cols := e.ctx.ParseIndexColumns(entry.SQL); containsFold(cols, colName) {
				candidates = append(candidates, coveringCandidate{name: entry.Name, cols: len(cols)})
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	// Read sz hints from stat table in one pass
	szMap := e.readStatSZs()
	for i := range candidates {
		candidates[i].sz = szMap[candidates[i].name]
	}
	return bestCoveringCandidate(candidates).name
}

// bestCoveringCandidate picks the best covering index: fewest columns, then
// smallest sz hint (a zero sz means no hint and loses to any positive hint).
func bestCoveringCandidate(candidates []coveringCandidate) coveringCandidate {
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.cols < best.cols {
			best = c
		} else if c.cols == best.cols {
			if best.sz == 0 && c.sz > 0 {
				best = c
			} else if best.sz > 0 && c.sz > 0 && c.sz < best.sz {
				best = c
			}
		}
	}
	return best
}

// stat1RowCount returns the table row count recorded by ANALYZE in
// sqlite_stat1 (the first token of the stat string), or 0 if unavailable.
func (e *SelectEngine) stat1RowCount(table string) int64 {
	var count int64
	e.forEachStat1Row(func(rec *storage.Record) bool {
		if n, ok := stat1RowCountForTable(rec, table); ok {
			count = n
			return false
		}
		return true
	})
	return count
}

// stat1RowCountForTable extracts the row count for table from one sqlite_stat1
// row: values are [tbl, idx, stat] and the count is the first token of stat.
func stat1RowCountForTable(rec *storage.Record, table string) (int64, bool) {
	if len(rec.Values) < 3 {
		return 0, false
	}
	tblStr, ok := rec.Values[0].(string)
	if !ok || tblStr != table {
		return 0, false
	}
	statStr, ok := rec.Values[2].(string)
	if !ok {
		return 0, false
	}
	fields := strings.Fields(statStr)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	return n, err == nil
}
