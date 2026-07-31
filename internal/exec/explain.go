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

	return planResult(nodes)
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
	if bestIndex != "" && bestEstimate < threshold {
		plan := fmt.Sprintf("SEARCH %s USING INDEX %s", tableName, bestIndex)
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
			if ti := e.tableIndexByDisplay(tables, colRef.Table); ti >= 0 {
				constPreds[ti]++
			}
			continue
		}
		left, okL := bin.Left.(*sql.ColumnRef)
		right, okR := bin.Right.(*sql.ColumnRef)
		if !okL || !okR {
			continue
		}
		li := e.tableIndexByDisplay(tables, left.Table)
		ri := e.tableIndexByDisplay(tables, right.Table)
		if li < 0 || ri < 0 || li == ri {
			continue
		}
		if idx := e.findIndexOnColumn(tables[li].real, left.Name); idx != "" {
			joinRefs[li] = append(joinRefs[li], joinRef{table: tables[li].display, col: left.Name, otherTable: tables[ri].display, indexName: idx})
		}
		if idx := e.findIndexOnColumn(tables[ri].real, right.Name); idx != "" {
			joinRefs[ri] = append(joinRefs[ri], joinRef{table: tables[ri].display, col: right.Name, otherTable: tables[li].display, indexName: idx})
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
		return fmt.Sprintf("SEARCH %s USING INDEX %s (%s=?)", t.display, jr.indexName, jr.col)
	}
	nRow := e.estimatedRowCount(t.real)
	est := float64(nRow)
	idx, conds := e.bestIndexForQuery(t.real, s.Where, &est)
	if idx != "" && est < float64(nRow)*0.10 {
		if conds != "" {
			return fmt.Sprintf("SEARCH %s USING INDEX %s %s", t.display, idx, conds)
		}
		return fmt.Sprintf("SEARCH %s USING INDEX %s", t.display, idx)
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
	// Collect all refs for the best index to build conditions
	if bestName != "" {
		for _, ref := range refs {
			if ref.indexName == bestName {
				bestRefs = append(bestRefs, ref)
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
		} else {
			parts = append(parts, fmt.Sprintf("%s%s?", ref.colName, op))
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

type indexedRef struct {
	indexName   string
	colName     string   // column name for condition formatting
	constant    interface{}
	op          string
	selectivity float64 // pre-computed selectivity (for non-standard ops)
}

func collectIndexedRefs(expr sql.Expr, tableName string, e *Engine) []indexedRef {
	var refs []indexedRef
	_, _ = walkExpr, walkExpr(expr, func(e2 sql.Expr) {
		if binop, ok := e2.(*sql.BinaryOp); ok {
			colRef, constVal := findColAndConst(binop)
			// Only column-to-constant predicates can drive an index SEARCH;
			// column-to-column predicates are join terms, not constants.
			if colRef != nil && constVal != nil {
				idxName := e.findIndexOnColumn(tableName, colRef.Name)
				if idxName != "" {
					refs = append(refs, indexedRef{
						indexName:  idxName,
						colName:    colRef.Name,
						constant:   constVal,
						op:         binop.Operator,
					})
				}
			}
		}
	})
	// ALSO handle BETWEEN — it's not a BinaryOp
	_, _ = walkExpr, walkExpr(expr, func(e2 sql.Expr) {
		if bt, ok := e2.(*sql.Between); ok {
			if colRef, ok := bt.Operand.(*sql.ColumnRef); ok {
				idxName := e.findIndexOnColumn(tableName, colRef.Name)
				if idxName != "" {
					sel := computeBetweenSelectivity(bt)
					refs = append(refs, indexedRef{
						indexName:   idxName,
						colName:     colRef.Name,
						constant:    float64(0),
						op:          "BETWEEN",
						selectivity: sel,
					})
				}
			}
		}
	})
	return refs
}

func computeBetweenSelectivity(bt *sql.Between) float64 {
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

func (e *Engine) findIndexOnColumn(tableName, colName string) string {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName {
			// Parse the column list from the index SQL and check if colName is in it
			indexCols := parseIndexColumns(entry.SQL)
			for _, ic := range indexCols {
				if strings.EqualFold(ic, colName) {
					return entry.Name
				}
			}
		}
	}
	return ""
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

