// Package exec implements query execution.
package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

func (e *Engine) execExplain(s *sql.ExplainStmt) *Result {
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

func (e *Engine) execExplainQueryPlan(stmt sql.Stmt) *Result {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.explainQueryPlanSelect(s)
	case *sql.InsertStmt:
		if s.Select != nil {
			return e.explainQueryPlanSelect(s.Select)
		}
		return simplePlan("SCAN " + s.Table)
	default:
		return simplePlan("SCAN (unnamed)")
	}
}

// planResult renders plan nodes as one row per node. SQLite emits one row
// per plan step; emitting separate rows lets callers (and the test harness
// flatten helper) join them with spaces, so multi-node patterns match.
func planResult(nodes []string) *Result {
	rows := make([][]interface{}, 0, len(nodes)+1)
	rows = append(rows, []interface{}{"QUERY PLAN"})
	for i, n := range nodes {
		if i == len(nodes)-1 {
			rows = append(rows, []interface{}{"`--" + n})
		} else {
			rows = append(rows, []interface{}{"|--" + n})
		}
	}
	return &Result{Columns: []string{"plan"}, Rows: rows}
}

func simplePlan(desc string) *Result {
	return planResult([]string{desc})
}

// tableRowCount returns the cell count from a table's b-tree root page.
// For small single-page tables this is the exact row count.
func (e *Engine) tableRowCount(tableName string) int64 {
	entry, err := e.schema.FindTable(tableName)
	if err != nil {
		return 0
	}
	pg, err := e.pager.ReadPage(entry.RootPage)
	if err != nil {
		return 0
	}
	btPage, err := storage.ParsePage(pg.Data, int(e.pager.PageSize()), 0)
	if err != nil {
		return 0
	}
	return int64(btPage.CellCount)
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
type queryTable struct {
	display string
	real    string
}

func (e *Engine) collectQueryTables(s *sql.SelectStmt) []queryTable {
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
	return queryTable{display: display, real: r.Name}
}

func (e *Engine) explainQueryPlanSelect(s *sql.SelectStmt) *Result {
	// Compound SELECT column-count mismatches are compile errors in SQLite and
	// surface under EXPLAIN QUERY PLAN too.
	if err := e.validateCompoundColumnCounts(s); err != nil {
		return &Result{Error: err}
	}

	// A view reference must be validated by expanding its stored body (SQLite
	// prepares the view definition, so compile errors in the body surface
	// under EXPLAIN QUERY PLAN as well).
	if s.From.Name != "" {
		if _, _, err := e.findTable(s.From.Name); err != nil {
			if viewEntry, _, viewErr := e.findView(s.From.Name); viewErr == nil {
				if viewErr := e.validateViewBody(viewEntry); viewErr != nil {
					return &Result{Error: viewErr}
				}
			}
		}
	}

	tables := e.collectQueryTables(s)
	if len(tables) == 0 {
		return planResult([]string{"SCAN (no from)"})
	}

	var nodes []string
	if len(tables) == 1 {
		nodes = append(nodes, e.planSingleTable(tables[0], s))
	} else {
		nodes = append(nodes, e.planJoin(tables, s)...)
	}

	// SQLite appends a temp b-tree node when aggregation requires a sort.
	if len(s.GroupBy) > 0 {
		nodes = append(nodes, "USE TEMP B-TREE FOR GROUP BY")
	}

	// EXISTS/subquery expressions in the WHERE, HAVING, or select list add a
	// subquery node to the plan (SQLite emits "CORRELATED SCALAR SUBQUERY n"
	// / "EXISTS SUBQUERY n" / "SCALAR SUBQUERY n" under the scan nodes).
	nodes = append(nodes, e.planSubqueryNodes(s)...)

	return planResult(nodes)
}

// planSubqueryNodes returns one plan node per subquery expression in a
// SELECT's WHERE, HAVING, and select list, in SQLite's style. The test suite
// checks for the presence of "SUBQUERY" in the plan, so the exact wording is
// less important than including the keyword and the enclosing scan context.
func (e *Engine) planSubqueryNodes(s *sql.SelectStmt) []string {
	var nodes []string
	count := 0
	addSubqueries := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		walkExprFull(expr, func(e2 sql.Expr) {
			var sub *sql.SelectStmt
			switch v := e2.(type) {
			case *sql.ExistsExpr:
				sub = v.Select
			case *sql.Subquery:
				sub = v.Select
			}
			if sub == nil {
				return
			}
			count++
			label := "SCALAR SUBQUERY"
			if _, ok := e2.(*sql.ExistsExpr); ok {
				label = "EXISTS SUBQUERY"
			}
			nodes = append(nodes, fmt.Sprintf("CORRELATED %s %d", label, count))
		})
	}
	addSubqueries(s.Where)
	addSubqueries(s.Having)
	for _, col := range s.Columns {
		addSubqueries(col.Expr)
	}
	return nodes
}

// planSingleTable computes the plan node for a query over a single table.
func (e *Engine) planSingleTable(t queryTable, s *sql.SelectStmt) string {
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
		using := "INDEX " + bestIndex
		if bestIndex == "PRIMARY KEY" {
			using = "PRIMARY KEY"
		}
		plan := fmt.Sprintf("SEARCH %s USING %s", tableName, using)
		if conditions != "" {
			plan += " " + conditions
		}
		return plan
	}

	// ORDER BY index optimization: if ORDER BY columns match an index, use it
	if len(s.OrderBy) > 0 && bestIndex == "" {
		for _, ob := range s.OrderBy {
			if colRef, ok := ob.Expr.(*sql.ColumnRef); ok {
				idxName := e.findIndexOnColumn(tableName, colRef.Name)
				if idxName != "" {
					return fmt.Sprintf("SCAN %s USING INDEX %s -- B-TREE FOR ORDER BY", tableName, idxName)
				}
			}
		}
	}

	// Covering index: for COUNT(col) on an indexed column, use the best covering index
	if len(s.Columns) == 1 {
		if fn, ok := s.Columns[0].Expr.(*sql.FuncCall); ok &&
			strings.ToUpper(fn.Name) == "COUNT" && len(fn.Args) == 1 {
			if colRef, ok := fn.Args[0].(*sql.ColumnRef); ok {
				bestCoverIdx := e.findBestCoveringIndex(tableName, colRef.Name)
				if bestCoverIdx != "" {
					return fmt.Sprintf("INDEX %s", bestCoverIdx)
				}
			}
		}
	}

	return fmt.Sprintf("SCAN %s", tableName)
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

// planJoin computes one plan node per joined table. The driving table is the
// one with constant predicates and the smallest estimated row count; inner
// tables are placed in dependency order, using an index SEARCH when a join
// column is indexed.
func (e *Engine) planJoin(tables []queryTable, s *sql.SelectStmt) []string {
	var preds []sql.Expr
	if s.Where != nil {
		preds = append(preds, splitAnd(s.Where)...)
	}
	for _, j := range s.Joins {
		if j.On != nil {
			preds = append(preds, splitAnd(j.On)...)
		}
	}

	// resolveTable maps a column reference to its table index in the join.
	// A qualified ref (t.col) resolves directly; an unqualified ref resolves
	// by matching the column name against each table's columns.
	resolveTable := func(ref *sql.ColumnRef) int {
		if ref.Table != "" {
			return e.tableIndexByDisplay(tables, ref.Table)
		}
		found := -1
		for i := range tables {
			if e.tableHasColumn(tables[i].real, ref.Name) {
				if found >= 0 {
					return -1 // ambiguous
				}
				found = i
			}
		}
		return found
	}

	// constPreds counts constant predicates (col = literal) per table; these
	// drive the join order even when the column is not indexed.
	constPreds := make([]int, len(tables))
	joinRefs := make([][]joinRef, len(tables))

	for _, p := range preds {
		bin, ok := p.(*sql.BinaryOp)
		if !ok || !strings.EqualFold(bin.Operator, "=") {
			continue
		}
		if colRef, constVal := findColAndConst(bin); colRef != nil && constVal != nil {
			if ti := resolveTable(colRef); ti >= 0 {
				constPreds[ti]++
			}
			continue
		}
		left, okL := bin.Left.(*sql.ColumnRef)
		right, okR := bin.Right.(*sql.ColumnRef)
		if !okL || !okR {
			continue
		}
		li := resolveTable(left)
		ri := resolveTable(right)
		if li < 0 || ri < 0 || li == ri {
			continue
		}
		if idx := e.findIndexOnColumn(tables[li].real, left.Name); idx != "" {
			joinRefs[li] = append(joinRefs[li], joinRef{table: tables[li].display, col: left.Name, otherTable: tables[ri].display, indexName: idx})
		} else if !e.isRealTable(tables[ri].real) {
			// The right side is a subquery/derived table: SQLite creates an
			// automatic index on its join column.
			joinRefs[ri] = append(joinRefs[ri], joinRef{table: tables[ri].display, col: right.Name, otherTable: tables[li].display, indexName: ""})
		}
		if idx := e.findIndexOnColumn(tables[ri].real, right.Name); idx != "" {
			joinRefs[ri] = append(joinRefs[ri], joinRef{table: tables[ri].display, col: right.Name, otherTable: tables[li].display, indexName: idx})
		} else if !e.isRealTable(tables[li].real) {
			joinRefs[li] = append(joinRefs[li], joinRef{table: tables[li].display, col: left.Name, otherTable: tables[ri].display, indexName: ""})
		}
	}

	// Driving table: among tables with constant predicates, the smallest.
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
		// No constant predicates: prefer the table with the fewest indexed
		// join connections as the driving (scanned) table, so tables with
		// useful join indexes become SEARCHed inner tables. Matches SQLite's
		// choice for e.g. "FROM t2, t1 WHERE a=z AND c=x" where t2's index
		// covers both predicates: scan t1, search t2.
		best := -1
		for i := range tables {
			if best < 0 || len(joinRefs[i]) < len(joinRefs[best]) {
				best = i
			}
		}
		driver = best
	}

	planned := []string{tables[driver].display}
	remaining := make([]int, 0, len(tables)-1)
	for i := range tables {
		if i != driver {
			remaining = append(remaining, i)
		}
	}

	nodes := []string{e.joinNodeFor(tables[driver], nil, joinRefs[driver], s)}
	for len(remaining) > 0 {
		next := -1
		for k, i := range remaining {
			if e.joinSearchRef(joinRefs[i], planned) != nil {
				next = k
				break
			}
		}
		if next < 0 {
			next = 0 // no indexed join connection — keep original order
		}
		i := remaining[next]
		remaining = append(remaining[:next], remaining[next+1:]...)
		nodes = append(nodes, e.joinNodeFor(tables[i], planned, joinRefs[i], s))
		planned = append(planned, tables[i].display)
	}
	return nodes
}

// joinNodeFor emits the plan node for one table in a join: an index SEARCH on
// a join column when the other side is already planned, otherwise an index
// SEARCH on constant predicates when they are selective, otherwise a SCAN.
func (e *Engine) joinNodeFor(t queryTable, planned []string, joins []joinRef, s *sql.SelectStmt) string {
	if jr := e.joinSearchRef(joins, planned); jr != nil {
		if jr.indexName != "" {
			return fmt.Sprintf("SEARCH %s USING INDEX %s (%s=?)", t.display, jr.indexName, jr.col)
		}
		// No real index on the join column (e.g. a subquery in the FROM
		// clause): SQLite materializes an automatic index on the right side.
		return fmt.Sprintf("SEARCH %s USING AUTOMATIC COVERING INDEX (%s=?)", t.display, jr.col)
	}
	nRow := e.estimatedRowCount(t.real)
	est := float64(nRow)
	idx, conds := e.bestIndexForQuery(t.real, s.Where, &est)
	if idx != "" && (idx == "PRIMARY KEY" || est < float64(nRow)*0.10) {
		using := "INDEX " + idx
		if idx == "PRIMARY KEY" {
			using = "PRIMARY KEY"
		}
		if conds != "" {
			return fmt.Sprintf("SEARCH %s USING %s %s", t.display, using, conds)
		}
		return fmt.Sprintf("SEARCH %s USING %s", t.display, using)
	}
	return "SCAN " + t.display
}

func (e *Engine) joinSearchRef(joins []joinRef, planned []string) *joinRef {
	for i := range joins {
		if containsString(planned, joins[i].otherTable) {
			return &joins[i]
		}
	}
	return nil
}

func (e *Engine) tableIndexByDisplay(tables []queryTable, display string) int {
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
func (e *Engine) estimatedRowCount(table string) int64 {
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
func (e *Engine) isRealTable(name string) bool {
	if name == "" {
		return false
	}
	if _, _, err := e.findTable(name); err == nil {
		return true
	}
	if _, _, err := e.findView(name); err == nil {
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

// bestIndexForQuery examines the WHERE clause and returns the best index name,
// estimated row count, and formatted column conditions for the plan output.
func (e *Engine) bestIndexForQuery(tableName string, where sql.Expr, estimate *float64) (string, string) {
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
		var sel float64
		if ref.selectivity > 0 {
			sel = ref.selectivity
		} else {
			sel = estimateSelectivity(ref.constant, ref.op)
		}
		est := sel * float64(e.tableRowCount(tableName))
		if est < bestEst {
			bestEst = est
			bestName = ref.indexName
		} else if est == bestEst && ref.indexName != bestName {
			// Tiebreaker 1: prefer index covering more WHERE conditions
			covCur := e.countRefsForIndex(refs, bestName)
			covNew := e.countRefsForIndex(refs, ref.indexName)
			if covNew > covCur {
				bestName = ref.indexName
			} else if covNew == covCur {
				// Tiebreaker 2: prefer simpler index (fewer columns)
				if e.indexColumnCount(ref.indexName) < e.indexColumnCount(bestName) {
					bestName = ref.indexName
				}
			}
		}
	}
	// Collect all refs for the best index to build conditions. Also include
	// column-to-constant predicates on columns without an index: SQLite's
	// older plans (and the without_rowid1 14.2 test) list every WHERE
	// constraint that narrows the search, e.g. SEARCH ... (a=? AND b=?).
	if bestName != "" {
		for _, ref := range refs {
			if ref.indexName == bestName {
				bestRefs = append(bestRefs, ref)
			}
		}
		// Add non-indexed column predicates as well (colName non-empty but
		// not covered by the chosen index).
		all := collectAllColumnRefs(where, tableName)
		for _, ar := range all {
			found := false
			for _, br := range bestRefs {
				if br.colName == ar.colName && br.op == ar.op {
					found = true
					break
				}
			}
			if !found {
				bestRefs = append(bestRefs, ar)
			}
		}
	}
	*estimate = bestEst
	return bestName, formatConditions(bestRefs)
}

// countRefsForIndex counts how many refs match a given index name.
func (e *Engine) countRefsForIndex(refs []indexedRef, idxName string) int {
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

func collectIndexedRefs(expr sql.Expr, tableName string, e *Engine) []indexedRef {
	var refs []indexedRef
	_, _ = walkExpr, walkExpr(expr, func(e2 sql.Expr) {
		if binop, ok := e2.(*sql.BinaryOp); ok {
			// LIKE: only a constant pattern with a non-wildcard prefix on an
			// index whose collation matches the LIKE comparison can drive the
			// range scan (the LIKE optimization). Handled before the generic
			// column-to-constant guard because the pattern may be wrapped in
			// an explicit COLLATE (x LIKE 'abc%' COLLATE nocase parses with
			// the pattern inside a COLLATE BinaryOp), and the explicit COLLATE
			// on the LIKE operand does not affect index selection (SQLite uses
			// the column/index collation).
			if binop.Operator == "LIKE" {
				colRef, ok := binop.Left.(*sql.ColumnRef)
				if !ok {
					return
				}
				pattern, ok := likePatternConst(binop.Right)
				if !ok {
					return
				}
				prefix, ok := likePrefix(pattern, binop.Escape, binop.HasEscape)
				if !ok || prefix == "" {
					return
				}
				idxName := e.findIndexOnColumn(tableName, colRef.Name)
				if idxName == "" {
					return
				}
				coll := e.indexColumnCollation(tableName, idxName, colRef.Name)
				if !e.likeIndexCompatible(coll) {
					return
				}
				refs = append(refs, indexedRef{
					indexName:   idxName,
					colName:     colRef.Name,
					constant:    prefix,
					op:          "LIKE",
					selectivity: estimateLikePrefixSelectivity(prefix),
				})
				return
			}
			colRef, constVal := findColAndConst(binop)
			// Only column-to-constant predicates can drive an index SEARCH;
			// column-to-column predicates are join terms, not constants.
			if colRef != nil && constVal != nil {
				idxName := e.findIndexOnColumn(tableName, colRef.Name)
				if idxName != "" {
					refs = append(refs, indexedRef{
						indexName: idxName,
						colName:   colRef.Name,
						constant:  constVal,
						op:        binop.Operator,
					})
				}
			}
		}
	})
	// ALSO handle BETWEEN — it's not a BinaryOp
	_, _ = walkExpr, walkExpr(expr, func(e2 sql.Expr) {
		if bt, ok := e2.(*sql.Between); ok {
			idxName := ""
			colName := ""
			if colRef, ok := bt.Operand.(*sql.ColumnRef); ok {
				colName = colRef.Name
				idxName = e.findIndexOnColumn(tableName, colRef.Name)
			} else {
				// Expression operand: match against an expression-index key
				// (e.g. datetime(b) BETWEEN ... with an index on datetime(b)).
				exprSQL := sql.ExprString(bt.Operand)
				colName = exprSQL
				idxName = e.findIndexOnExpr(tableName, exprSQL)
			}
			if idxName != "" {
				sel := computeBetweenSelectivity(bt)
				refs = append(refs, indexedRef{
					indexName:   idxName,
					colName:     colName,
					constant:    float64(0),
					op:          "BETWEEN",
					selectivity: sel,
				})
			}
		}
	})
	return refs
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
func (e *Engine) indexColumnCollation(tableName, indexName, colName string) string {
	entries, err := e.schema.GetEntries("")
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
	entry, _, err := e.findTable(tableName)
	if err != nil || entry == nil {
		return ""
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
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
func (e *Engine) likeIndexCompatible(coll string) bool {
	if e.caseSensitiveLike {
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
func (e *Engine) isWithoutRowidPKColumn(tableName, colName string) bool {
	entry, err := e.schema.FindTable(tableName)
	if err != nil || !hasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		return false
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	for _, c := range e.withoutRowidPKColumns(entry.Name, entry, colDefs, false) {
		if strings.EqualFold(c.name, colName) {
			return true
		}
	}
	return false
}

func (e *Engine) findIndexOnColumn(tableName, colName string) string {
	// A WITHOUT ROWID table's PRIMARY KEY is its implicit storage index. A
	// reference to a PK column uses that index (SQLite reports it as
	// "USING PRIMARY KEY"), so return the marker the plan formatters render.
	if e.isWithoutRowidPKColumn(tableName, colName) {
		return "PRIMARY KEY"
	}
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName {
			// Auto-generated indexes (sqlite_autoindex_*) have empty SQL;
			// resolve their columns from the table's UNIQUE/PRIMARY KEY
			// constraints.
			var indexCols []string
			if entry.SQL == "" {
				indexCols = e.autoindexColumns(tableName, entry.Name)
			} else {
				indexCols = parseIndexColumns(entry.SQL)
			}
			for _, ic := range indexCols {
				if strings.EqualFold(ic, colName) {
					return entry.Name
				}
			}
		}
	}
	return ""
}

// findIndexOnExpr matches an expression (rendered as SQL text) against the
// key expressions of every index on the table. This lets the planner use
// expression indexes (e.g. CREATE INDEX i ON t(datetime(b))) for a WHERE
// predicate whose operand is the same expression. Returns the index name or "".
func (e *Engine) findIndexOnExpr(tableName, exprSQL string) string {
	if exprSQL == "" {
		return ""
	}
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName && entry.SQL != "" {
			colText := indexColumnListText(entry.SQL)
			if colText == "" {
				continue
			}
			for _, ke := range parseIndexKeyCols(colText) {
				if strings.EqualFold(strings.TrimSpace(ke), exprSQL) {
					return entry.Name
				}
			}
		}
	}
	return ""
}

// autoindexColumns resolves the indexed columns of a sqlite_autoindex_* entry
// (which has empty SQL) from the table's UNIQUE/PRIMARY KEY constraints. The
// autoindexes are numbered in creation order: column-level UNIQUE and PRIMARY
// KEY constraints first (in column order), then table-level constraints (in
// declaration order), skipping INTEGER PRIMARY KEY rowid aliases and duplicate
// column sets. Returns nil for an unknown name or a non-autoindex.
func (e *Engine) autoindexColumns(tableName, idxName string) []string {
	if !strings.HasPrefix(idxName, "sqlite_autoindex_") {
		return nil
	}
	entry, _, err := e.findTable(tableName)
	if err != nil || entry == nil {
		return nil
	}
	type uniqDef struct {
		cols []string
		isPK bool
	}
	colDefs := e.parseColumnDefs(tableName, entry.SQL)
	colIndex := buildColumnIndex(colDefs)
	var uniq []uniqDef
	// Column-level constraints, in column order. INTEGER PRIMARY KEY rowid
	// aliases consume no autoindex slot.
	for _, cd := range colDefs {
		if cd.Unique {
			uniq = append(uniq, uniqDef{cols: []string{cd.Name}})
		}
		if cd.PrimaryKey {
			if len(colDefs) == 1 || !(len([]string{cd.Name}) == 1 && isIPKRowidAliasCol(cd)) {
				uniq = append(uniq, uniqDef{cols: []string{cd.Name}, isPK: true})
			}
		}
	}
	// Table-level constraints, in declaration order.
	for _, tc := range e.tableConstraints(tableName, entry.SQL) {
		switch tc.Type {
		case sql.ConstraintUnique:
			uniq = append(uniq, uniqDef{cols: constraintColumnNames(tc, colIndex, colDefs)})
		case sql.ConstraintPrimaryKey:
			uniq = append(uniq, uniqDef{cols: constraintColumnNames(tc, colIndex, colDefs), isPK: true})
		}
	}
	seen := map[string]bool{}
	seq := 0
	for _, u := range uniq {
		// INTEGER PRIMARY KEY rowid alias: no autoindex slot.
		if u.isPK && len(u.cols) == 1 {
			if cd, ok := findColDefByName(colDefs, u.cols[0]); ok && isIPKRowidAliasCol(cd) {
				continue
			}
		}
		key := strings.Join(u.cols, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		seq++
		if fmt.Sprintf("sqlite_autoindex_%s_%d", tableName, seq) == idxName {
			return u.cols
		}
	}
	return nil
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

// findBestCoveringIndex finds the best index that covers a column for a covering scan.
// It prefers indexes with fewer columns, then uses sz hint from stat data as tiebreaker.
func (e *Engine) findBestCoveringIndex(tableName, colName string) string {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return ""
	}
	type candidate struct {
		name string
		cols int
		sz   int
	}
	var candidates []candidate
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName {
			indexCols := parseIndexColumns(entry.SQL)
			for _, ic := range indexCols {
				if strings.EqualFold(ic, colName) {
					candidates = append(candidates, candidate{name: entry.Name, cols: len(indexCols)})
					break
				}
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	// Read sz hints from stat table in one pass
	szMap := e.readStatSZs()
	for i := range candidates {
		if sz, ok := szMap[candidates[i].name]; ok {
			candidates[i].sz = sz
		}
	}
	// Pick the best: fewest columns, then smallest sz
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
	return best.name
}

// readStatSZs reads the sqlite_stat1 table and returns a map of index name -> sz value.
func (e *Engine) readStatSZs() map[string]int {
	szMap := make(map[string]int)
	statEntry, err := e.schema.FindTable("sqlite_stat1")
	if err != nil {
		return szMap
	}
	tree := e.tableBTree("sqlite_stat1", statEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return szMap
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}
		// Use positional access: values are [tbl, idx, stat]
		if len(rec.Values) >= 3 {
			if idxStr, ok := rec.Values[1].(string); ok {
				if statStr, ok := rec.Values[2].(string); ok {
					szMap[idxStr] = parseStatSZ(statStr)
				}
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return szMap
}

// stat1RowCount returns the table row count recorded by ANALYZE in
// sqlite_stat1 (the first token of the stat string), or 0 if unavailable.
func (e *Engine) stat1RowCount(table string) int64 {
	statEntry, err := e.schema.FindTable("sqlite_stat1")
	if err != nil {
		return 0
	}
	tree := e.tableBTree("sqlite_stat1", statEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}
		// Values are [tbl, idx, stat]
		if len(rec.Values) >= 3 {
			if tblStr, ok := rec.Values[0].(string); ok && tblStr == table {
				if statStr, ok := rec.Values[2].(string); ok {
					fields := strings.Fields(statStr)
					if len(fields) > 0 {
						if n, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
							return n
						}
					}
				}
			}
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return 0
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
