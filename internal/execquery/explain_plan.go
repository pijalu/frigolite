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
	"github.com/pijalu/frigolite/internal/vtab"
)

// planSingleTable computes the plan node for a query over a single table
// together with the scan loop the planner chose (the loop feeds the ORDER BY
// consumption analysis in planSortNodes).
func (e *SelectEngine) planSingleTable(t queryTable, s *sql.SelectStmt) (string, scanLoop) {
	// Created virtual tables plan through xBestIndex (wherecode.c:205-208
	// "VIRTUAL TABLE INDEX" rendering) before any b-tree index logic; a vtab
	// has no indexes of its own.
	if plan := e.vtabExplainPlan(t, s); plan != "" {
		return plan, scanLoop{kind: loopVtab}
	}
	loop := e.scanLoopForQuery(t, s, true)
	switch loop.kind {
	case loopSkipScan, loopIndex:
		if loop.seek {
			return e.searchPlan(t.display, loop.token, loop.conditions, s), loop
		}
		// Full index walk chosen for the ORDER BY / GROUP BY / DISTINCT
		// optimization: "SCAN <t> USING [COVERING] INDEX <idx>".
		return e.indexScanDetail(t, loop, s), loop
	case loopIPK:
		// INTEGER PRIMARY KEY / rowid seek (equality or literal range):
		// whereScanLoop stored the rendered node (same analysis the seek
		// executor runs), so plan and execution share one decision.
		return loop.conditions, loop
	}
	// Covering index: for COUNT(col) on an indexed column, use the best covering index
	if plan, ok := e.countIndexPlan(t, s); ok {
		return plan, loop
	}
	return fmt.Sprintf("SCAN %s", t.display), loop
}

// vtabExplainPlan renders the "SCAN <name> VIRTUAL TABLE INDEX <idxNum>:<idxStr>"
// node for a created virtual table (wherecode.c:205-208), running the
// prepare-time xBestIndex plan call (BestIndexPlan) on a representative
// instance resolved by the engine. It returns "" when the FROM term is not a
// created vtab (the caller falls through to the regular table planning).
// EQP planning never fails the statement: a rejected plan
// (vtab.ErrVtabConstraint), any BestIndexPlan error, or a re-entrant call
// renders the plain "SCAN <name>" fallback instead. BestIndexPlan is a
// prepare-time contract and must not execute SQL; the explainVtabBusy guard
// degrades any nested EQP request to the fallback should a module misbehave.
func (e *SelectEngine) vtabExplainPlan(t queryTable, s *sql.SelectStmt) string {
	if e.explainVtabBusy {
		return ""
	}
	vt, columns, ok := e.ctx.VtabPlanInstance(t.real)
	if !ok {
		return ""
	}
	opts := e.vtabScanOptions(s)
	// Qualifiers in the statement use the FROM alias when one was written
	// (SQLite rejects the table name once aliased), else the table name.
	qualifier := t.real
	if s.From.As != "" {
		qualifier = s.From.As
	}
	var overloader vtab.FunctionOverloader
	if fo, can := vt.(vtab.FunctionOverloader); can {
		overloader = fo
	}
	ii, _, _ := BuildVtabIndexInfoWithInstance(&opts, qualifier, columns, overloader)
	e.explainVtabBusy = true
	err := vtabBestIndexPlan(ii, vt)
	e.explainVtabBusy = false
	if err != nil {
		return fmt.Sprintf("SCAN %s", t.display)
	}
	if ii.IdxFlags&vtab.IndexScanHex != 0 {
		return fmt.Sprintf("SCAN %s VIRTUAL TABLE INDEX 0x%x:%s", t.display, ii.IdxNum, ii.IdxStr)
	}
	return fmt.Sprintf("SCAN %s VIRTUAL TABLE INDEX %d:%s", t.display, ii.IdxNum, ii.IdxStr)
}

// vtabBestIndexPlan calls the module's BestIndexPlan when the instance
// implements vtab.PlanBestIndexer; instances without it keep the legacy
// full-materialization plan (default IdxNum/IdxStr).
func vtabBestIndexPlan(ii *vtab.IndexInfo, vt vtab.VirtualTable) error {
	if pbi, can := vt.(vtab.PlanBestIndexer); can {
		return pbi.BestIndexPlan(ii)
	}
	return nil
}

// searchPlan renders a "SEARCH <table> USING <index> (<conditions>)" node for
// a selective index on a single-table query.
func (e *SelectEngine) searchPlan(tableName, idx, conditions string, s *sql.SelectStmt) string {
	using := e.indexUsingLabel(tableName, idx, s)
	plan := fmt.Sprintf("SEARCH %s USING %s", tableName, using)
	if conditions != "" {
		plan += " " + conditions
	}
	return plan
}

// indexScanDetail renders a "SCAN <table> USING [COVERING] INDEX <idx>" node
// for the loop's full index walk. A GROUP BY / DISTINCT walk is always
// COVERING (its token selection required coverage); an ORDER BY walk is
// COVERING when the index also covers the output columns.
func (e *SelectEngine) indexScanDetail(t queryTable, loop scanLoop, s *sql.SelectStmt) string {
	if loop.groupDistinct || e.indexCoversCols(loop.token, t.real, selectOutputCols(s)) {
		return fmt.Sprintf("SCAN %s USING COVERING INDEX %s", t.display, indexSchemaName(loop.token))
	}
	return fmt.Sprintf("SCAN %s USING INDEX %s", t.display, indexSchemaName(loop.token))
}

// orderByIndexToken resolves the index whose leading columns match the ORDER
// BY columns (a temp-b-tree-free ORDER BY walk), or ok=false when no index
// qualifies. Partial indexes are only used when the query WHERE implies the
// partial-index predicate. When the query has WHERE constraints on columns
// outside the index, SQLite prefers a full scan + temp sort rather than an
// index scan with post-filtering. Collation-aware: each term's effective
// collation must equal the index column's declared collation by name
// (findIndexForOrderTerms) — terms the matcher cannot represent fall back
// to the name-only path.
func (e *SelectEngine) orderByIndexToken(t queryTable, s *sql.SelectStmt) (string, bool) {
	var idxName string
	if terms := orderByTermColls(s); terms != nil {
		idxName = e.findIndexForOrderTerms(t.display, terms, s.Where)
	} else {
		obCols := orderByCols(s)
		if len(obCols) == 0 {
			return "", false
		}
		idxName = e.findIndexOnColsForQuery(t.display, obCols, s.Where)
	}
	if idxName == "" {
		return "", false
	}
	if s.Where != nil && e.whereHasNonIndexConstraint(s.Where, t.real, idxName) {
		// where.c prices the temp-b-tree sort into every loop that does not
		// deliver the ORDER BY. A serving index defeated by a WHERE column
		// outside it still loses only to plans cheaper than walk+filter —
		// and a COVERING serving index walks without a single table lookup:
		// the filter reads the index row and the walk delivers the order.
		// C picks it over the seek+sorter whenever the seek fails its own
		// cost bar (where-6.7.x: WHERE b>0 ORDER BY a over t3 plans
		// "SCAN t3 USING COVERING INDEX t3acb", not the t3bc seek plus a
		// sorter).
		return e.coveringOrderIndexToken(t, s)
	}
	return idxName, true
}

// coveringOrderIndexToken resolves the first ORDER-BY-serving index that
// also covers the query: every WHERE column of the table and every output
// column sits in the index key (or is the rowid tail an index entry carries),
// so the walk evaluates the filter and the projection from the index alone —
// where.c's WHERE_IDX_ONLY loop. ok is false when no serving candidate
// covers the query (the legacy full-scan + temp-sort shape stands).
func (e *SelectEngine) coveringOrderIndexToken(t queryTable, s *sql.SelectStmt) (string, bool) {
	terms := orderByTermColls(s)
	if terms == nil {
		return "", false
	}
	var found string
	e.forEachOrderServingIndex(t.display, terms, s.Where, func(token string) bool {
		if e.whereHasNonIndexConstraint(s.Where, t.real, token) {
			return false // a WHERE column outside the index: the walk pays table lookups
		}
		if !e.indexCoversQuery(t, s, token) {
			return false
		}
		found = token
		return true
	})
	return found, found != ""
}

// indexCoversQuery reports whether the named index covers every column the
// statement references on the FROM table: the output columns plus the WHERE
// clause's column refs. Rowid references ride the index entry's trailing
// rowid (the pseudo-names and the table's INTEGER PRIMARY KEY alias alike)
// and never require a table lookup.
func (e *SelectEngine) indexCoversQuery(t queryTable, s *sql.SelectStmt, token string) bool {
	cols := make([]string, 0, len(s.Columns))
	for _, c := range s.Columns {
		ref, ok := c.Expr.(*sql.ColumnRef)
		if !ok {
			return false // an expression output needs the table row unless the index carries it
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, t.display) && !strings.EqualFold(ref.Table, t.real) {
			continue
		}
		cols = append(cols, ref.Name)
	}
	refs, _, cached := e.whererefCollect(s.Where, t.real)
	if !cached {
		refs = collectIndexedRefs(s.Where, t.real, e)
	}
	for _, ref := range refs {
		cols = append(cols, ref.colName)
	}
	// The rowid tail: pseudo-names and the IPK alias are covered implicitly.
	ipk := e.ipkAliasColumn(t.real)
	out := cols[:0]
	for _, c := range cols {
		if isRowIDName(c) || (ipk != "" && strings.EqualFold(c, ipk)) {
			continue
		}
		out = append(out, c)
	}
	return e.indexCoversCols(token, t.real, out)
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

// groupDistinctIndexToken resolves the index whose leading columns match the
// GROUP BY / DISTINCT columns and which covers every output column (the
// temp-b-tree-free grouping/dedup walk), or ok=false when none qualifies.
func (e *SelectEngine) groupDistinctIndexToken(t queryTable, s *sql.SelectStmt) (string, bool) {
	var cols []string
	if len(s.GroupBy) > 0 {
		cols = groupByCols(s)
	} else {
		cols = distinctCols(s)
	}
	if len(cols) == 0 {
		return "", false
	}
	idxName := e.findIndexOnCols(t.display, cols)
	if idxName == "" || !e.indexCoversCols(idxName, t.real, selectOutputCols(s)) {
		return "", false
	}
	return idxName, true
}

// countIndexPlan renders an "INDEX <idx>" node for COUNT(col) when a covering
// index on the counted column exists, or ok=false otherwise.
func (e *SelectEngine) countIndexPlan(t queryTable, s *sql.SelectStmt) (string, bool) {
	if len(s.Columns) != 1 {
		return "", false
	}
	fn, ok := s.Columns[0].Expr.(*sql.FuncCall)
	if !ok || strings.ToUpper(fn.Name) != "COUNT" || len(fn.Args) != 1 {
		return "", false
	}
	colRef, ok := fn.Args[0].(*sql.ColumnRef)
	if !ok {
		return "", false
	}
	bestCoverIdx := e.findBestCoveringIndex(t.display, colRef.Name)
	if bestCoverIdx != "" {
		return fmt.Sprintf("INDEX %s", indexSchemaName(bestCoverIdx)), true
	}
	return "", false
}

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
	e.planWhereSubqueries(s.Where, s, &nodes, addSubquery, s)
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
func (e *SelectEngine) planWhereSubqueries(expr sql.Expr, outer *sql.SelectStmt, nodes *[]planNode, add func(sub *sql.SelectStmt, label string), sel *sql.SelectStmt) {
	if expr == nil {
		return
	}
	if bin, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "AND") {
		e.planWhereSubqueries(bin.Left, outer, nodes, add, sel)
		e.planWhereSubqueries(bin.Right, outer, nodes, add, sel)
		return
	}
	if paren, ok := expr.(*sql.ParenExpr); ok {
		e.planWhereSubqueries(paren.Expr, outer, nodes, add, sel)
		return
	}
	if ex, ok := expr.(*sql.ExistsExpr); ok {
		// SQLite's existsToJoin transforms ANY top-level EXISTS conjunct
		// meeting the structural conditions into a CROSS join of the inner
		// table with the subquery's WHERE ANDed into the outer query —
		// correlated or not (existsexpr-1.3.x: the plan shows
		// "SEARCH x1 EXISTS ..." with no SUBQUERY line). NOT EXISTS is not a
		// bare TK_EXISTS conjunct and stays a subquery.
		if !ex.Negated {
			if exNodes, ok2 := e.existsJoinNode(ex.Select, sel); ok2 {
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
func (e *SelectEngine) existsJoinNode(sub *sql.SelectStmt, s *sql.SelectStmt) ([]planNode, bool) {
	if sub == nil || sub.From.Name == "" || sub.From.Subquery != nil || len(sub.Joins) > 0 {
		return nil, false
	}
	if sub.Union != nil || sub.Limit != nil || sub.GroupBy != nil || e.hasAggregate(sub) {
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
		// Without a real index SQLite builds an automatic partial covering
		// index on the join key for the EXISTS probe (existsexpr-1.3.2
		// "SEARCH x1 EXISTS USING AUTOMATIC PARTIAL COVERING INDEX (b=?);").
		return []planNode{{detail: fmt.Sprintf("SEARCH %s EXISTS USING AUTOMATIC PARTIAL COVERING INDEX (%s=?)", tableName, col)}}, true
	}
	using := e.indexUsingLabel(tableName, idx, s)
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

// AutoindexColumnsForAnalyze exposes autoindexColumns for callers in other
// packages (e.g. internal/exec pragma_analyze). Returns nil when idxName is
// not a sqlite_autoindex_* entry.
func (e *SelectEngine) AutoindexColumnsForAnalyze(tableName, idxName string) []string {
	return e.autoindexColumns(tableName, idxName)
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
		if entry.Type == "index" && entry.TblName == tableName && !e.indexIsUnordered(entry.Name) {
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
	return indexLookupToken(bestCoveringCandidate(candidates).name)
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
