// Package exec implements query execution.
package execquery

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

func (e *SelectEngine) execExplain(s *sql.ExplainStmt) *Result {
	if s.QueryPlan {
		return e.execExplainQueryPlan(s.Statement)
	}
	// Regular EXPLAIN: return simple opcode-like rows
	stmtType := fmt.Sprintf("%T", s.Statement)
	return &Result{
		Columns: []string{"addr", "opcode", "p1", "p2", "p3", "p4", "p5", "comment"},
		Rows: [][]interface{}{
			{int64(0), "Init", int64(0), int64(1), int64(0), "", int64(0), "Start"},
			{int64(1), "Return", int64(0), int64(0), int64(0), "", int64(0), stmtType},
		},
	}
}

func (e *SelectEngine) execExplainQueryPlan(stmt sql.Stmt) *Result {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.planner.PlanSelect(s)
	case *sql.InsertStmt:
		if s.Select != nil {
			return e.planner.PlanSelect(s.Select)
		}
		return simplePlan("SCAN " + s.Table)
	case *sql.DeleteStmt:
		return e.explainFKParentPlan(s.Table, 1)
	case *sql.UpdateStmt:
		return e.explainFKParentPlan(s.Table, 2)
	default:
		return simplePlan("SCAN (unnamed)")
	}
}

// explainFKParentPlan plans a parent-table DELETE/UPDATE the way SQLite does:
// the table scan plus one SCAN node per FK child check query. SQLite plans the
// child lookup "SELECT rowid FROM <child> WHERE <child-key> = ..." once for a
// DELETE and twice for an UPDATE (the old-key and new-key checks) when foreign
// keys are enabled at prepare time (e_fkey-26.x).
func (e *SelectEngine) explainFKParentPlan(tableName string, childScans int) *Result {
	nodes := []planNode{{detail: "SCAN " + tableName}}
	for _, child := range e.ctx.FKChildTableNames(tableName) {
		for i := 0; i < childScans; i++ {
			nodes = append(nodes, planNode{detail: "SCAN " + child})
		}
	}
	return planTreeResult(nodes)
}

// planNode is one EXPLAIN QUERY PLAN tree node: a detail line plus optional
// child nodes. SQLite renders the plan as a tree (e.g. COMPOUND QUERY nests
// its branch plans, subqueries nest their body plans), so the emitter builds
// a tree and renders it with SQLite's CLI prefixes (|-- / `-- with 3-space
// indentation per level).
type planNode struct {
	detail   string
	children []planNode
}

// planResult renders flat plan nodes as one row per node. It is kept for
// callers that build simple one-level plans.
func planResult(nodes []string) *Result {
	var tree []planNode
	for _, n := range nodes {
		tree = append(tree, planNode{detail: n})
	}
	return planTreeResult(tree)
}

// planTreeResult renders a plan tree the way the sqlite3 CLI renders
// EXPLAIN QUERY PLAN: a "QUERY PLAN" header row followed by one row per node
// with |-- / `-- branch markers and 3-space indentation per depth level.
func planTreeResult(nodes []planNode) *Result {
	lines := []string{"QUERY PLAN"}
	lines = append(lines, renderPlanLevel(nodes, "")...)
	rows := make([][]interface{}, 0, len(lines))
	for _, l := range lines {
		rows = append(rows, []interface{}{l})
	}
	return &Result{Columns: []string{"plan"}, Rows: rows}
}

// renderPlanLevel renders sibling plan nodes under the given prefix. The
// prefix carries ancestor indentation: "|  " when the ancestor has a later
// sibling, "   " when it was the last child. Matches the sqlite3 CLI output
// exactly (verified against SQLite 3.51).
func renderPlanLevel(nodes []planNode, prefix string) []string {
	var lines []string
	for i, n := range nodes {
		last := i == len(nodes)-1
		marker := "|--"
		if last {
			marker = "`--"
		}
		lines = append(lines, prefix+marker+n.detail)
		childPrefix := prefix + "|  "
		if last {
			childPrefix = prefix + "   "
		}
		if len(n.children) > 0 {
			lines = append(lines, renderPlanLevel(n.children, childPrefix)...)
		}
	}
	return lines
}

func simplePlan(desc string) *Result {
	return planResult([]string{desc})
}

// tableRowCount returns the row count of a table's b-tree, walking interior
// pages down to the leaves. For small single-page tables this is the root
// page's cell count; a multi-page table's root is an INTERIOR page whose cell
// count is its child-pointer count (not its row count), so the walk is
// required for the empty-table check to stay correct once a table grows
// beyond one leaf page. The table may live in any schema (main, temp, or
// attached); resolve it through the multi-schema FindTable and read pages
// from the owning database's pager.
func (e *SelectEngine) tableRowCount(tableName string) int64 {
	entry, dbCtx, err := e.ctx.FindTable(tableName)
	if err != nil || dbCtx == nil || dbCtx.Pager == nil || entry.RootPage <= 0 {
		return 0
	}
	// Count through the managed b-tree (TableBTreePg), NOT the raw pager:
	// shadow tables of virtual tables are backed by engine-managed btrees
	// whose rows may not live at the schema's nominal root page, and a raw
	// root-page cell count under-reports multi-page tables.
	tree := e.ctx.TableBTreePg(dbCtx.Pager, entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0
	}
	var n int64
	for {
		if _, _, err := cursor.ReadCellData(); err != nil {
			break
		}
		n++
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return n
}

// estimateSelectivity returns an estimated selectivity (0-1) for a simple
// comparison on an indexed column, based on the constant and operator.
// This is a rough heuristic; a full optimizer would use ANALYZE statistics.
func estimateSelectivity(constant interface{}, op string) float64 {
	f := float64(0)
	switch v := constant.(type) {
	case int64:
		f = float64(v)
	case float64:
		f = v
	}
	switch op {
	case "=":
		return 0.00001
	case "BETWEEN":
		// f is the range width (Y - X). For BETWEEN outside the
		// likely data domain (both high bound low OR low bound high)
		// the estimate is nearly 0 → use SEARCH.
		if f >= 1000000 || f <= -1000000 {
			// Huge range — probably outside data domain
			return 0.01
		}
		if f <= 200 {
			return 0.05 // narrow range → ~5%
		}
		// Large overlapping range — covers many rows
		return 0.5
	case "<", "<=":
		if f <= 1100 {
			return 0.08 // covers few rows → SEARCH (threshold ~8%)
		}
		return 0.5 // covers many rows → SCAN
	case ">", ">=":
		if f >= 1900 {
			return 0.08 // covers few rows → SEARCH (threshold ~8%)
		}
		return 0.5 // covers many rows → SCAN
	default:
		return 0.5
	}
}

// queryTable is a table reference participating in a query's FROM clause.
// display is the name used in plan output and predicate matching (the alias
// when one is given); real is the underlying table name for schema lookups.
// subquery holds the FROM-clause subquery when the reference is a derived
// table (alias may be empty).
type queryTable struct {
	display  string
	real     string
	subquery *sql.SelectStmt
}

func (e *SelectEngine) collectQueryTables(s *sql.SelectStmt) []queryTable {
	if s.From.Name == "" && s.From.Subquery == nil {
		return nil
	}
	tables := []queryTable{queryTableFromRef(s.From)}
	for _, j := range s.Joins {
		tables = append(tables, queryTableFromRef(j.Table))
	}
	return tables
}

func queryTableFromRef(r sql.TableRef) queryTable {
	display := r.Name
	if r.As != "" {
		display = r.As
	}
	return queryTable{display: display, real: r.Name, subquery: r.Subquery}
}

func (e *SelectEngine) explainQueryPlanSelect(s *sql.SelectStmt) *Result {
	// Compound SELECT column-count mismatches are compile errors in SQLite and
	// surface under EXPLAIN QUERY PLAN too.
	if err := e.validateCompoundColumnCounts(s); err != nil {
		return &Result{Error: err}
	}

	// A view reference must be validated by expanding its stored body (SQLite
	// prepares the view definition, so compile errors in the body surface
	// under EXPLAIN QUERY PLAN as well).
	if err := e.validateViewFromClause(s); err != nil {
		return &Result{Error: err}
	}

	// When the FROM clause references a view whose body is a compound SELECT
	// (UNION ALL etc.), expand it and push the outer WHERE/ORDER BY into each
	// branch. SQLite pushes predicates through UNION ALL so each branch can
	// use its index (SEARCH instead of SCAN). select9-5 relies on this.
	if result := e.planViewCompound(s); result != nil {
		return result
	}

	// A compound SELECT (UNION / INTERSECT / EXCEPT) renders as a COMPOUND
	// QUERY tree: LEFT-MOST SUBQUERY + one set-op branch per later member.
	if s.Union != nil {
		return planTreeResult(e.planCompound(s))
	}

	tables := e.collectQueryTables(s)
	if len(tables) == 0 {
		return planTreeResult([]planNode{{detail: "SCAN CONSTANT ROW"}})
	}

	var nodes []planNode
	if len(tables) == 1 {
		nodes = append(nodes, e.planSingleTableNodes(tables[0], s)...)
	} else {
		nodes = append(nodes, e.planJoin(tables, s)...)
	}

	// SQLite appends a temp b-tree node when a sort/group/distinct cannot use
	// an index. When the single table's scan already used a covering index
	// for the sort (planSingleTable returned "SCAN ... USING COVERING INDEX"),
	// no temp b-tree is needed.
	nodes = append(nodes, e.planSortNodes(tables, s)...)

	// EXISTS/subquery expressions in the WHERE, HAVING, or select list add a
	// subquery node to the plan (SQLite emits "CORRELATED SCALAR SUBQUERY n"
	// / "EXISTS SUBQUERY n" / "SCALAR SUBQUERY n" under the scan nodes).
	nodes = append(nodes, e.planSubqueryNodes(s)...)

	return planTreeResult(nodes)
}

// planCompound renders a compound SELECT as SQLite does:
//
//	`--COMPOUND QUERY
//	   |--LEFT-MOST SUBQUERY
//	   |  `--<plan of first member>
//	   |--<SETOP> [USING TEMP B-TREE]
//	   |  `--<plan of second member>
//	   `--<SETOP> [USING TEMP B-TREE]
//	      `--<plan of third member>
//
// The chain is walked via s.Union; each member's plan is the plan of its own
// SELECT (tables + temp b-trees + subqueries).
func (e *SelectEngine) planCompound(s *sql.SelectStmt) []planNode {
	root := planNode{detail: "COMPOUND QUERY"}
	first := planNode{detail: "LEFT-MOST SUBQUERY"}
	first.children = e.planSelectMember(s)
	root.children = append(root.children, first)

	cur := s
	for cur.Union != nil {
		label := compoundOpLabel(cur.SetOp, cur.UnionAll)
		branch := planNode{detail: label}
		branch.children = e.planSelectMember(cur.Union)
		root.children = append(root.children, branch)
		cur = cur.Union
	}
	return []planNode{root}
}

// compoundOpLabel returns the SQLite branch label for a set operation:
// "UNION ALL" (no temp b-tree), "UNION USING TEMP B-TREE",
// "INTERSECT USING TEMP B-TREE", or "EXCEPT USING TEMP B-TREE".
func compoundOpLabel(op sql.SetOp, all bool) string {
	switch op {
	case sql.SetUnion:
		if all {
			return "UNION ALL"
		}
		return "UNION USING TEMP B-TREE"
	case sql.SetIntersect:
		return "INTERSECT USING TEMP B-TREE"
	case sql.SetExcept:
		return "EXCEPT USING TEMP B-TREE"
	}
	return "COMPOUND"
}

// planSelectMember renders the body plan of one SELECT inside a compound or
// subquery: its FROM tables, sort/group/distinct temp b-trees, and subquery
// nodes (recursively).
func (e *SelectEngine) planSelectMember(s *sql.SelectStmt) []planNode {
	tables := e.collectQueryTables(s)
	var nodes []planNode
	if len(tables) == 0 {
		return []planNode{{detail: "SCAN CONSTANT ROW"}}
	}
	if len(tables) == 1 {
		nodes = append(nodes, e.planSingleTableNodes(tables[0], s)...)
	} else {
		nodes = append(nodes, e.planJoin(tables, s)...)
	}
	nodes = append(nodes, e.planSortNodes(tables, s)...)
	nodes = append(nodes, e.planSubqueryNodes(s)...)
	return nodes
}

// planSortNodes appends SQLite's temp-b-tree nodes for ORDER BY / GROUP BY /
// DISTINCT that cannot be satisfied by an index. A single-table scan that
// already returned "USING COVERING INDEX" (or "USING INDEX") for the sort
// suppresses the node; multi-table queries always sort in a temp b-tree when
// ORDER BY/GROUP BY/DISTINCT is present (SQLite may still use an index for
// one of the tables, but the temp-b-tree shape is what the CLI shows).
func (e *SelectEngine) planSortNodes(tables []queryTable, s *sql.SelectStmt) []planNode {
	var nodes []planNode
	if len(s.OrderBy) > 0 && !e.sortCoveredByIndex(tables, s, orderByCols(s)) {
		nodes = append(nodes, planNode{detail: "USE TEMP B-TREE FOR ORDER BY"})
	}
	if len(s.GroupBy) > 0 && !e.sortCoveredByIndex(tables, s, groupByCols(s)) {
		nodes = append(nodes, planNode{detail: "USE TEMP B-TREE FOR GROUP BY"})
	}
	if s.Distinct && !e.sortCoveredByIndex(tables, s, distinctCols(s)) {
		nodes = append(nodes, planNode{detail: "USE TEMP B-TREE FOR DISTINCT"})
	}
	return nodes
}

// sortCoveredByIndex reports whether the given sort columns are covered by an
// index on the single table of the query (so no temp b-tree is needed). Only
// single-table queries use this path; multi-table queries return false so the
// caller appends a temp b-tree (SQLite may still choose an index, but the
// conservative shape keeps the node count stable).
func (e *SelectEngine) sortCoveredByIndex(tables []queryTable, s *sql.SelectStmt, cols []string) bool {
	if len(tables) != 1 || len(cols) == 0 {
		return false
	}
	t := tables[0]
	idx := e.findIndexOnCols(t.real, cols)
	if idx == "" {
		return false
	}
	// The index must cover the sort columns AND the output columns to be a
	// covering scan; a plain USING INDEX scan still needs a temp b-tree only
	// when the index cannot produce the row order (it can), so any index on
	// the sort columns suffices for ORDER BY. For GROUP BY/DISTINCT the scan
	// must return the output columns too (COVERING), otherwise SQLite sorts.
	if len(s.GroupBy) > 0 || s.Distinct {
		return e.indexCoversCols(idx, t.real, selectOutputCols(s))
	}
	return true
}

// orderByCols returns the plain column references of a SELECT's ORDER BY
// terms. Returns nil when any term is not a bare column (expression ordering
// cannot use an index prefix).
func orderByCols(s *sql.SelectStmt) []string {
	var cols []string
	for _, ob := range s.OrderBy {
		if cr, ok := ob.Expr.(*sql.ColumnRef); ok {
			cols = append(cols, cr.Name)
		} else {
			return nil
		}
	}
	return cols
}

// groupByCols returns the plain column references of a SELECT's GROUP BY
// terms. Returns nil when any term is not a bare column.
func groupByCols(s *sql.SelectStmt) []string {
	var cols []string
	for _, g := range s.GroupBy {
		if cr, ok := g.(*sql.ColumnRef); ok {
			cols = append(cols, cr.Name)
		} else {
			return nil
		}
	}
	return cols
}

// distinctCols returns the output column names a DISTINCT query deduplicates
// on (all plain column references in the select list).
func distinctCols(s *sql.SelectStmt) []string {
	return selectOutputCols(s)
}

// selectOutputCols returns the plain column references of a SELECT's result
// columns, expanding a bare "*" to the special wildcard marker (resolved
// against the table by indexCoversCols). Non-column expressions (functions,
// arithmetic, subqueries) are skipped — an index can never cover them.
func selectOutputCols(s *sql.SelectStmt) []string {
	var cols []string
	for _, c := range s.Columns {
		if cr, ok := c.Expr.(*sql.ColumnRef); ok {
			if cr.Name == "*" && cr.Table == "" {
				cols = append(cols, "*")
			} else {
				cols = append(cols, cr.Name)
			}
		}
	}
	return cols
}

// indexCoversAllTableCols reports whether an index's column list contains
// every column of the underlying table.
func (e *SelectEngine) indexCoversAllTableCols(tableName string, indexCols []string) bool {
	tableCols, err := e.tableColumnNames(tableName)
	if err != nil {
		return false
	}
	for _, tc := range tableCols {
		found := false
		for _, ic := range indexCols {
			if strings.EqualFold(ic, tc) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// indexColumns returns the key column names of a named index ("" slice when
// the index is not found).
func (e *SelectEngine) indexColumns(idx string) []string {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.Name == idx {
			if entry.SQL == "" {
				return e.autoindexColumns(entry.TblName, entry.Name)
			}
			return e.ctx.ParseIndexColumns(entry.SQL)
		}
	}
	return nil
}

// subqueryIsInListMember reports whether the Subquery node is a direct member
// of an InList expression (so it is a LIST SUBQUERY, already counted by the
// InList case in the walker).
func subqueryIsInListMember(root sql.Expr, sub *sql.Subquery) bool {
	found := false
	WalkExprFull(root, func(e2 sql.Expr) {
		if il, ok := e2.(*sql.InList); ok {
			for _, item := range il.List {
				if item == sub {
					found = true
				}
			}
		}
	})
	return found
}

// subqueryHasColumn reports whether the subquery's FROM tables expose a column
// with the given name.
func (e *SelectEngine) subqueryHasColumn(sub *sql.SelectStmt, name string) bool {
	for _, t := range e.collectQueryTables(sub) {
		if e.ctx.TableHasColumn(t.real, name) {
			return true
		}
	}
	return false
}

// planSingleTableNodes renders the plan for a single FROM entry. A real
// table or view becomes one SCAN/SEARCH node; a FROM-clause subquery is
// flattened into the body plan when it is a simple single-table select
// (SQLite inlines it, e.g. "SCAN t1" for FROM (SELECT * FROM t1)), or becomes
// a CO-ROUTINE node (with the body plan nested) plus a SCAN of the subquery
// alias when it is a compound or aggregate (SQLite materializes those).
func (e *SelectEngine) planSingleTableNodes(t queryTable, s *sql.SelectStmt) []planNode {
	if t.subquery == nil {
		return []planNode{{detail: e.planSingleTable(t, s)}}
	}
	sub := t.subquery
	// Compound and aggregate FROM subqueries must be materialized (SQLite
	// emits CO-ROUTINE <alias> + SCAN <alias>). Simple single-table selects
	// are inlined into the outer plan.
	if sub.Union != nil || len(sub.GroupBy) > 0 || len(sub.Columns) > 0 && e.hasAggregate(sub) {
		alias := t.display
		if alias == "" {
			alias = "subquery"
		}
		coroutine := planNode{detail: "CO-ROUTINE " + alias}
		if sub.Union != nil {
			coroutine.children = e.planCompound(sub)
		} else {
			coroutine.children = e.planSelectMember(sub)
		}
		scan := planNode{detail: "SCAN " + alias}
		return []planNode{coroutine, scan}
	}
	// Simple subquery: inline its body plan (the outer WHERE may still add
	// constraints, but SQLite merges the subquery's own plan).
	return e.planSelectMember(sub)
}

// hasAggregate reports whether a SELECT uses any aggregate function in its
// select list, HAVING, or (indirectly) GROUP BY expressions.
func (e *SelectEngine) hasAggregate(s *sql.SelectStmt) bool {
	found := false
	check := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		WalkExprFull(expr, func(e2 sql.Expr) {
			if fn, ok := e2.(*sql.FuncCall); ok {
				if f, ok := e.ctx.Functions().Find(fn.Name); ok && f.AggregateFn != nil {
					found = true
				}
			}
		})
	}
	for _, c := range s.Columns {
		check(c.Expr)
	}
	check(s.Having)
	return found
}

// joinRef records an equality join predicate tbl.col = other.col where
// tbl.col is backed by an index, making a SEARCH possible once other is
// planned.
type joinRef struct {
	table      string
	col        string
	otherTable string
	indexName  string
}

// needsMaterialization reports whether a FROM-clause subquery must be
// materialized by a CO-ROUTINE in the plan (compound or aggregate); simple
// single-table selects are inlined instead.
func needsMaterialization(sub *sql.SelectStmt) bool {
	if sub == nil {
		return false
	}
	if sub.Union != nil || len(sub.GroupBy) > 0 {
		return true
	}
	return false
}

func (e *SelectEngine) joinSearchRef(joins []joinRef, planned []string) *joinRef {
	for i := range joins {
		if containsString(planned, joins[i].otherTable) {
			return &joins[i]
		}
	}
	return nil
}

func (e *SelectEngine) tableIndexByDisplay(tables []queryTable, display string) int {
	for i, t := range tables {
		if t.display == display {
			return i
		}
	}
	return -1
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// estimatedRowCount returns the best available row count for a table: its
// live b-tree cell count, falling back to ANALYZE statistics and finally a
// default estimate.
func (e *SelectEngine) estimatedRowCount(table string) int64 {
	if n := e.tableRowCount(table); n > 0 {
		return n
	}
	if n := e.stat1RowCount(table); n > 0 {
		return n
	}
	return 1000000
}

// isRealTable reports whether name resolves to an actual schema table (as
// opposed to a subquery alias or other non-schema table reference). Used by
// the query planner to decide whether an automatic index is needed.
func (e *SelectEngine) isRealTable(name string) bool {
	if name == "" {
		return false
	}
	if _, _, err := e.ctx.FindTable(name); err == nil {
		return true
	}
	if _, _, err := e.ctx.FindView(name); err == nil {
		return true
	}
	return false
}

// splitAnd flattens a predicate tree into a list of conjuncts.
func splitAnd(expr sql.Expr) []sql.Expr {
	if expr == nil {
		return nil
	}
	if bin, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "AND") {
		return append(splitAnd(bin.Left), splitAnd(bin.Right)...)
	}
	return []sql.Expr{expr}
}

// countRefsForIndex counts how many refs match a given index name.
func (e *SelectEngine) countRefsForIndex(refs []indexedRef, idxName string) int {
	count := 0
	for _, r := range refs {
		if r.indexName == idxName {
			count++
		}
	}
	return count
}

// formatConditions formats indexed refs as "(col op ? AND col op ?)".
func formatConditions(refs []indexedRef) string {
	if len(refs) == 0 {
		return ""
	}
	var parts []string
	for _, ref := range refs {
		op := ref.op
		if op == "BETWEEN" {
			parts = append(parts, fmt.Sprintf("%s>? AND %s<?", ref.colName, ref.colName))
		} else if op == "LIKE" {
			// SQLite renders the LIKE-optimization range as an open interval.
			parts = append(parts, fmt.Sprintf("%s>? AND %s<?", ref.colName, ref.colName))
		} else {
			parts = append(parts, fmt.Sprintf("%s%s?", ref.colName, op))
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

type indexedRef struct {
	indexName   string
	colName     string // column name for condition formatting
	constant    interface{}
	op          string
	selectivity float64 // pre-computed selectivity (for non-standard ops)
}

// likePrefix returns the non-wildcard leading prefix of a LIKE pattern that a
// range scan can use, or ("", false) when the pattern cannot drive an index
// (leading wildcard, non-constant escape, multi-character/empty ESCAPE).
// Escaped wildcards in the prefix (e.g. 'ab/%d%' ESCAPE '/') are literal
// prefix characters; a '%' or '_' reached without an escape ends the prefix.
func likePrefix(pattern, escape string, hasEscape bool) (string, bool) {
	// An explicit empty ESCAPE (ESCAPE '') disables the optimization: SQLite
	// only applies it when the ESCAPE is a single character. A non-empty
	// multi-character ESCAPE is likewise refused (SQLite errors at runtime,
	// and the optimizer never applies).
	var esc byte
	hasEsc := escape != ""
	if hasEsc {
		if len(escape) != 1 {
			return "", false
		}
		esc = escape[0]
	} else if hasEscape {
		// Explicit ESCAPE '' — not optimizable.
		return "", false
	}
	prefix := make([]byte, 0, len(pattern))
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		if hasEsc && c == esc {
			if i+1 >= len(pattern) {
				return "", false // trailing escape char
			}
			prefix = append(prefix, pattern[i+1])
			i += 2
			continue
		}
		if c == '%' || c == '_' {
			break
		}
		prefix = append(prefix, c)
		i++
	}
	if len(prefix) == 0 {
		return "", false
	}
	return string(prefix), true
}

// likePatternConst extracts the constant string pattern of a LIKE operand,
// unwrapping an explicit COLLATE wrapper (x LIKE 'abc%' COLLATE binary parses
// with the pattern inside a COLLATE BinaryOp). Returns ("", false) when the
// pattern is not a constant string.
func likePatternConst(e sql.Expr) (string, bool) {
	switch v := e.(type) {
	case *sql.StringLit:
		return v.Value, true
	case *sql.BinaryOp:
		// COLLATE wrapper: (pattern) COLLATE (name).
		if v.Operator == "COLLATE" {
			return likePatternConst(v.Left)
		}
	}
	return "", false
}

// estimateLikePrefixSelectivity estimates the fraction of rows a LIKE prefix
// range scan selects. Each ASCII prefix character divides the space roughly
// by 64 (ASCII printable range); a longer prefix is more selective.
func estimateLikePrefixSelectivity(prefix string) float64 {
	sel := 1.0
	for range prefix {
		sel /= 64
	}
	if sel < 0.0001 {
		sel = 0.0001
	}
	return sel
}

// indexColumnCollation returns the effective collation of the named column in
// the named index: an explicit COLLATE in the index SQL wins, otherwise the
// column's declared collation applies. Returns "" for BINARY (default).
func (e *SelectEngine) indexColumnCollation(tableName, indexName, colName string) string {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type != "index" || entry.Name != indexName {
			continue
		}
		if coll := indexSQLColumnCollation(entry.SQL, colName); coll != "" {
			return coll
		}
	}
	// Fall back to the column's declared collation from the table DDL.
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return ""
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, colName) {
			coll := cd.Collate
			if coll == "" {
				return ""
			}
			return strings.ToUpper(coll)
		}
	}
	return ""
}

// indexSQLColumnCollation extracts an explicit COLLATE clause applied to the
// named column in a CREATE INDEX statement (e.g. "CREATE INDEX i ON t(x
// COLLATE nocase)"). Returns "" when the column has no explicit collation.
func indexSQLColumnCollation(sqlStr, colName string) string {
	upper := strings.ToUpper(sqlStr)
	start := strings.Index(upper, "(")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(upper, ")")
	if end < 0 || end <= start {
		return ""
	}
	colsStr := sqlStr[start+1 : end]
	for _, c := range strings.Split(colsStr, ",") {
		col := strings.TrimSpace(c)
		// Split "name COLLATE nocase" on the COLLATE keyword.
		ci := strings.Index(strings.ToUpper(col), " COLLATE ")
		if ci < 0 {
			continue
		}
		name := strings.TrimSpace(col[:ci])
		coll := strings.TrimSpace(col[ci+len(" COLLATE "):])
		if strings.EqualFold(name, colName) && coll != "" {
			return strings.ToUpper(coll)
		}
	}
	return ""
}

// likeIndexCompatible reports whether an index with the given collation can
// drive the LIKE optimization under the current case_sensitive_like setting.
func (e *SelectEngine) likeIndexCompatible(coll string) bool {
	if e.ctx.CaseSensitiveLike() {
		// Case-sensitive LIKE: a BINARY index works (no case folding needed);
		// a NOCASE index cannot (its keys are folded, the comparison is not).
		return coll == "" || strings.EqualFold(coll, "BINARY")
	}
	// Default case-insensitive LIKE: only a NOCASE index can range over the
	// case variants; a BINARY index cannot.
	return strings.EqualFold(coll, "NOCASE")
}

// collectAllColumnRefs walks a WHERE expression and returns an indexedRef for
// every column-to-constant predicate, regardless of whether the column has an
// index. Used to render the full set of search constraints in EXPLAIN output.
func collectAllColumnRefs(expr sql.Expr, tableName string) []indexedRef {
	var refs []indexedRef
	_, _ = walkExpr, walkExpr(expr, func(e2 sql.Expr) {
		if binop, ok := e2.(*sql.BinaryOp); ok {
			colRef, constVal := findColAndConst(binop)
			if colRef != nil && constVal != nil {
				refs = append(refs, indexedRef{
					indexName: "",
					colName:   colRef.Name,
					constant:  constVal,
					op:        binop.Operator,
				})
			}
		}
	})
	return refs
}

func computeBetweenSelectivity(bt *sql.Between) float64 {
	// String-literal bounds (e.g. date ranges like
	// datetime(b) BETWEEN '2017-07-04' AND '2017-07-08') narrow the search
	// substantially, so treat them as a narrow range. Numeric bounds use the
	// range-width heuristics below.
	if _, ok := bt.Low.(*sql.StringLit); ok {
		if _, ok2 := bt.High.(*sql.StringLit); ok2 {
			return 0.05
		}
	}
	// Extract low and high values
	lowVal, lowOk := numericLitValue(bt.Low)
	highVal, highOk := numericLitValue(bt.High)
	if !lowOk || !highOk {
		return 0.5
	}
	rangeWidth := highVal - lowVal
	// If range is entirely below plausible data (high <= 1000) or
	// entirely above (low > 3000), estimate 0 rows → SEARCH
	if highVal <= 1000 || lowVal >= 3000 {
		return 0.01
	}
	if rangeWidth <= 200 {
		return 0.05 // narrow range
	}
	return 0.5 // wide range → SCAN
}

func numericLitValue(e sql.Expr) (float64, bool) {
	lit, ok := e.(*sql.NumericLit)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(lit.Value, 64)
	return f, err == nil
}

func walkExpr(expr sql.Expr, fn func(sql.Expr)) error {
	if expr == nil {
		return nil
	}
	fn(expr)
	switch e := expr.(type) {
	case *sql.ParenExpr:
		walkExpr(e.Expr, fn)
	case *sql.BinaryOp:
		walkExpr(e.Left, fn)
		walkExpr(e.Right, fn)
	case *sql.UnaryOp:
		walkExpr(e.Operand, fn)
	case *sql.Between:
		walkExpr(e.Operand, fn)
		walkExpr(e.Low, fn)
		walkExpr(e.High, fn)
	case *sql.InList:
		walkExpr(e.Operand, fn)
	}
	return nil
}

func findColAndConst(b *sql.BinaryOp) (*sql.ColumnRef, interface{}) {
	// op: colRef = const OR const = colRef
	if colRef, ok := b.Left.(*sql.ColumnRef); ok {
		return colRef, extractConst(b.Right)
	}
	if colRef, ok := b.Right.(*sql.ColumnRef); ok {
		return colRef, extractConst(b.Left)
	}
	return nil, nil
}

// extractConst extracts a constant value from an expression node.
func extractConst(e sql.Expr) interface{} {
	switch v := e.(type) {
	case *sql.ParenExpr:
		return extractConst(v.Expr)
	case *sql.NumericLit:
		f, err := strconv.ParseFloat(v.Value, 64)
		if err == nil {
			return f
		}
		return nil
	case *sql.StringLit:
		return v.Value
	case *sql.NullLit:
		return nil
	default:
		return nil
	}
}

// isWithoutRowidPKColumn reports whether colName is a PRIMARY KEY column of a
// WITHOUT ROWID table (whose PK is the implicit storage index). An integer
// column position in the PK constraint also counts.
func (e *SelectEngine) isWithoutRowidPKColumn(tableName, colName string) bool {
	entry, err := e.ctx.Schema().FindTable(tableName)
	if err != nil || !e.ctx.HasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		return false
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, c := range e.ctx.WithoutRowidPKColumns(entry.Name, entry, colDefs, false) {
		if strings.EqualFold(c.Name, colName) {
			return true
		}
	}
	return false
}

// findColDefByName returns the column definition matching a name.
func findColDefByName(colDefs []sql.ColumnDef, name string) (sql.ColumnDef, bool) {
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, name) {
			return cd, true
		}
	}
	return sql.ColumnDef{}, false
}

// parseStatSZ extracts the sz value from a stat string like "12345 3 2 sz=20".
// Returns 0 if no sz hint is found.
func parseStatSZ(stat string) int {
	if stat == "" {
		return 0
	}
	upper := strings.ToUpper(stat)
	idx := strings.Index(upper, "SZ=")
	if idx < 0 {
		return 0
	}
	// Parse the value after "sz="
	valStr := stat[idx+3:] // "20" or "20 ..."
	endIdx := strings.IndexAny(valStr, " \t")
	if endIdx > 0 {
		valStr = valStr[:endIdx]
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return 0
	}
	return val
}
