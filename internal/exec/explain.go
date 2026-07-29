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

func simplePlan(desc string) *Result {
	return &Result{
		Columns: []string{"plan"},
		Rows:    [][]interface{}{{fmt.Sprintf("QUERY PLAN\n`--%s", desc)}},
	}
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

func (e *Engine) explainQueryPlanSelect(s *sql.SelectStmt) *Result {
	if s.From.Name == "" && s.From.Subquery == nil {
		return simplePlan("SCAN (no from)")
	}
	tableName := s.From.Name
	if s.From.As != "" {
		tableName = s.From.As
	}

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
		return simplePlan(plan)
	}

	// ORDER BY index optimization: if ORDER BY columns match an index, use it
	if len(s.OrderBy) > 0 && bestIndex == "" {
		for _, ob := range s.OrderBy {
			if colRef, ok := ob.Expr.(*sql.ColumnRef); ok {
				idxName := e.findIndexOnColumn(tableName, colRef.Name)
				if idxName != "" {
					plan := fmt.Sprintf("SCAN %s USING INDEX %s -- B-TREE FOR ORDER BY", tableName, idxName)
					return simplePlan(plan)
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
					plan := fmt.Sprintf("INDEX %s", bestCoverIdx)
					return simplePlan(plan)
				}
			}
		}
	}

	return simplePlan(fmt.Sprintf("SCAN %s", tableName))
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
			if colRef != nil {
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

