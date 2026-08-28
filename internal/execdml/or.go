package execdml

import (
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// orConstraint is one equality constraint usable as an index prefix term:
// column = value. applyAffinity reports whether the comparison applies the
// column's affinity to the literal (a bare column reference). It is false for
// `+col = value`, which compares the raw value with no affinity conversion.

// maxOrTerms caps the number of DNF-flattened OR terms before the OR-index
// optimization is abandoned (SQLite similarly caps OR-term expansion).
const maxOrTerms = 32

// planOrIndexScan builds an OR-index plan for a single-table WHERE clause of
// the form (col1=v1 AND col2=v2) OR (col3=v3 AND col4=v4), where every OR
// term constrains the leading columns of some index on the table. Constant
// equalities from sibling AND terms (e.g. a=(subquery)) can extend each
// branch's index prefix. It returns (branches, true) when the plan applies,
// otherwise (nil, false).
func (e *DMLExecutor) planOrIndexScan(where sql.Expr, tableName string, colDefs []sql.ColumnDef, ctx *DatabaseContext) ([]orBranchPlan, bool) {
	if where == nil {
		return nil, false
	}
	// Split the top-level AND into conjuncts.
	andTerms := splitAndTerms(where)

	// Collect constant equality constraints from non-OR conjuncts (they can
	// extend each OR branch's index prefix, e.g. a=(subquery) AND (b=1 OR c=1)).
	outerCols := make(map[string]bool, len(colDefs))
	for _, cd := range colDefs {
		outerCols[cd.Name] = true
	}
	constConstraints, orTerm, orCount := e.collectOrConstraintTerms(andTerms, outerCols)
	if orCount != 1 || orTerm == nil {
		return nil, false
	}

	// Flatten the OR into DNF terms (distributing AND over nested ORs).
	terms := flattenOrTerms(orTerm)
	if len(terms) == 0 || len(terms) > maxOrTerms {
		return nil, false
	}

	// Load the table's indexes once.
	indexes, err := ctx.Schema.FindIndexesForTable(tableName)
	if err != nil || len(indexes) == 0 {
		return nil, false
	}

	var branches []orBranchPlan
	for _, term := range terms {
		bestK, bestIdx, bestCols, bestPrefix := e.bestIndexForOrBranch(term, outerCols, indexes, constConstraints)
		if bestK == 0 || bestIdx == nil {
			return nil, false
		}
		branches = append(branches, orBranchPlan{
			IndexName: bestIdx.Name,
			IndexCols: bestCols,
			Prefix:    bestPrefix,
		})
	}
	return branches, true
}

// collectOrConstraintTerms splits AND terms into the single OR term and the
// constant equality constraints from the non-OR conjuncts.
func (e *DMLExecutor) collectOrConstraintTerms(andTerms []sql.Expr, outerCols map[string]bool) ([]orConstraint, sql.Expr, int) {
	var constConstraints []orConstraint
	var orTerm sql.Expr
	orCount := 0
	for _, t := range andTerms {
		if isOrExpr(t) {
			orTerm = t
			orCount++
			continue
		}
		if col, val, aff, ok := e.extractEquality(t, outerCols); ok {
			constConstraints = append(constConstraints, orConstraint{Col: col, Val: val, ApplyAffinity: aff})
		}
	}
	return constConstraints, orTerm, orCount
}

// bestIndexForOrBranch selects the index with the longest prefix of columns
// covered by the branch's own constraints or a constant AND-context constraint.
func (e *DMLExecutor) bestIndexForOrBranch(term sql.Expr, outerCols map[string]bool, indexes []*schema.Entry, constConstraints []orConstraint) (int, *schema.Entry, []string, []orConstraint) {
	// Extract this term's own equality constraints.
	branchCols := make(map[string]orConstraint)
	for _, sub := range splitAndTerms(term) {
		if col, val, aff, ok := e.extractEquality(sub, outerCols); ok {
			branchCols[col] = orConstraint{Col: col, Val: val, ApplyAffinity: aff}
		}
	}
	if len(branchCols) == 0 {
		return 0, nil, nil, nil
	}

	bestK := 0
	var bestIdx *schema.Entry
	var bestCols []string
	var bestPrefix []orConstraint
	for _, idx := range indexes {
		cols := indexPlainColumns(idx.SQL)
		if len(cols) == 0 {
			continue
		}
		prefix, k := orBranchPrefix(cols, branchCols, constConstraints)
		if k > bestK {
			bestK = k
			bestIdx = idx
			bestCols = cols
			bestPrefix = prefix
		}
	}
	return bestK, bestIdx, bestCols, bestPrefix
}

// orBranchPrefix builds the constraint prefix for one index column list,
// falling back to a constant equality from a sibling AND term when the branch
// has no constraint for a column. The prefix stops at the first uncovered
// column.
func orBranchPrefix(cols []string, branchCols map[string]orConstraint, constConstraints []orConstraint) ([]orConstraint, int) {
	var prefix []orConstraint
	k := 0
	for _, ic := range cols {
		c, ok := branchCols[ic]
		if !ok {
			found := false
			for _, cc := range constConstraints {
				if cc.Col == ic {
					c = cc
					found = true
					break
				}
			}
			if !found {
				break
			}
		}
		prefix = append(prefix, c)
		k++
	}
	return prefix, k
}

// unwrapParen strips any number of enclosing parentheses.
func unwrapParen(expr sql.Expr) sql.Expr {
	for {
		p, ok := expr.(*sql.ParenExpr)
		if !ok {
			return expr
		}
		expr = p.Expr
	}
}

// isOrExpr reports whether the expression is a top-level OR.
func isOrExpr(expr sql.Expr) bool {
	b, ok := unwrapParen(expr).(*sql.BinaryOp)
	return ok && strings.EqualFold(b.Operator, "OR")
}

// splitAndTerms splits a top-level AND chain into its conjuncts.
func splitAndTerms(expr sql.Expr) []sql.Expr {
	expr = unwrapParen(expr)
	b, ok := expr.(*sql.BinaryOp)
	if !ok {
		return []sql.Expr{expr}
	}
	if strings.EqualFold(b.Operator, "AND") {
		return append(splitAndTerms(b.Left), splitAndTerms(b.Right)...)
	}
	return []sql.Expr{expr}
}

// flattenOrTerms converts an expression into DNF OR terms, distributing AND
// over OR. The result is a flat list where each element is a conjunction.
func flattenOrTerms(expr sql.Expr) []sql.Expr {
	expr = unwrapParen(expr)
	b, ok := expr.(*sql.BinaryOp)
	if !ok {
		return []sql.Expr{expr}
	}
	switch strings.ToUpper(b.Operator) {
	case "OR":
		return append(flattenOrTerms(b.Left), flattenOrTerms(b.Right)...)
	case "AND":
		lefts := flattenOrTerms(b.Left)
		rights := flattenOrTerms(b.Right)
		out := make([]sql.Expr, 0, len(lefts)*len(rights))
		for _, l := range lefts {
			for _, r := range rights {
				out = append(out, &sql.BinaryOp{Operator: "AND", Left: l, Right: r})
			}
		}
		return out
	}
	return []sql.Expr{expr}
}

// extractEquality extracts a col=value equality usable as an index prefix
// term. Returns the column name, the constant value, whether the comparison
// applies the column's affinity, and ok. The value side may be a literal or a
// constant scalar subquery (one that does not reference the outer table).
func (e *DMLExecutor) extractEquality(expr sql.Expr, outerCols map[string]bool) (string, interface{}, bool, bool) {
	expr = unwrapParen(expr)
	b, ok := expr.(*sql.BinaryOp)
	if !ok || b.Operator != "=" {
		return "", nil, false, false
	}
	if col, aff, ok := columnRefSide(b.Left); ok {
		val, ok := e.evalConstExpr(b.Right, outerCols)
		if !ok {
			return "", nil, false, false
		}
		return col, val, aff, true
	}
	if col, aff, ok := columnRefSide(b.Right); ok {
		val, ok := e.evalConstExpr(b.Left, outerCols)
		if !ok {
			return "", nil, false, false
		}
		return col, val, aff, true
	}
	return "", nil, false, false
}

// columnRefSide extracts a column reference from one side of a comparison,
// allowing a unary + wrapper (+col). Returns the column name, whether the
// comparison applies the column's affinity (false for +col), and ok.
func columnRefSide(expr sql.Expr) (string, bool, bool) {
	expr = unwrapParen(expr)
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return v.Name, true, true
	case *sql.UnaryOp:
		if v.Operator == "+" {
			if inner, ok := unwrapParen(v.Operand).(*sql.ColumnRef); ok {
				return inner.Name, false, true
			}
		}
	}
	return "", false, false
}

// evalConstExpr evaluates an expression expected to be constant: a literal, a
// simple unary-op expression, or a scalar subquery that does not reference the
// outer table's columns (a correlated subquery is not constant).
func (e *DMLExecutor) evalConstExpr(expr sql.Expr, outerCols map[string]bool) (interface{}, bool) {
	expr = unwrapParen(expr)
	switch v := expr.(type) {
	case *sql.NumericLit:
		if c := v.Cached(); c != nil {
			return c, true
		}
		val, err := e.ctx.EvalExpr(v, nil)
		if err != nil {
			return nil, false
		}
		return val, true
	case *sql.StringLit:
		return v.Value, true
	case *sql.BlobLit:
		return v.Value, true
	case *sql.NullLit:
		return nil, true
	case *sql.UnaryOp:
		val, err := e.ctx.EvalExpr(v, nil)
		if err != nil {
			return nil, false
		}
		return val, true
	case *sql.Subquery:
		if e.selectRefsOuterTable(v.Select, outerCols) {
			return nil, false
		}
		val, err := e.ctx.EvalExpr(v, nil)
		if err != nil {
			return nil, false
		}
		return val, true
	}
	return nil, false
}

// selectRefsOuterTable reports whether any column reference inside the SELECT
// (select list, WHERE, GROUP BY, HAVING, ORDER BY) resolves to one of
// outerCols — i.e. the subquery is correlated to the outer table.
func (e *DMLExecutor) selectRefsOuterTable(s *sql.SelectStmt, outerCols map[string]bool) bool {
	var refs []string
	collect := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		execquery.WalkExprFull(expr, func(sub sql.Expr) {
			if cr, ok := sub.(*sql.ColumnRef); ok {
				refs = append(refs, cr.Name)
			}
		})
	}
	for _, col := range s.Columns {
		collect(col.Expr)
	}
	collect(s.Where)
	for _, g := range s.GroupBy {
		collect(g)
	}
	collect(s.Having)
	for _, ob := range s.OrderBy {
		collect(ob.Expr)
	}
	for _, r := range refs {
		name := r
		if dot := strings.Index(r, "."); dot >= 0 {
			name = r[dot+1:]
		}
		if outerCols[name] {
			return true
		}
	}
	return false
}

// indexPlainColumns extracts the plain column names of an index from its SQL.
// Expression columns (containing parens) or non-identifier text cause the
// index to be skipped (returning an empty slice), because the engine does not
// build correct values for them.
func indexPlainColumns(sqlStr string) []string {
	cols := parseIndexColumns(sqlStr)
	if len(cols) == 0 {
		return nil
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		c = strings.TrimSpace(c)
		// Strip a trailing COLLATE clause.
		if i := strings.Index(strings.ToUpper(c), " COLLATE "); i >= 0 {
			c = strings.TrimSpace(c[:i])
		}
		// Strip a trailing ASC / DESC marker.
		upper := strings.ToUpper(c)
		if strings.HasSuffix(upper, " DESC") {
			c = strings.TrimSpace(c[:len(c)-len(" DESC")])
		} else if strings.HasSuffix(upper, " ASC") {
			c = strings.TrimSpace(c[:len(c)-len(" ASC")])
		}
		c = strings.TrimSpace(strings.Trim(c, "`\"[]"))
		if c == "" || strings.ContainsAny(c, "()+-*/%<>= ") {
			return nil // expression or compound column — not usable as a plain prefix
		}
		out = append(out, c)
	}
	return out
}

// execSelectWithOrPlan executes a single-table SELECT whose WHERE was planned
// by planOrIndexScan. Rows are produced in SQLite's OR-index union order:
// for each OR term in order, the matching rows in that term's index scan
// order (deduplicated against rows already emitted).
func (e *DMLExecutor) execSelectWithOrPlan(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, branches []orBranchPlan) *Result {
	prevScanTable := e.ctx.CurrentScanTable()
	e.ctx.SetCurrentScanTable(tableEntry.Name)
	if s.From.As != "" {
		e.ctx.SetCurrentScanTable(s.From.As)
	}
	defer func() { e.ctx.SetCurrentScanTable(prevScanTable) }()

	tree := e.ctx.TableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}

	colIndex := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}

	// Pass 1: scan the table once, collecting for every branch the rowids of
	// rows whose prefix columns match, along with the index key values used
	// to reproduce the index scan order.
	matches := e.collectOrBranchMatches(cursor, branches, colDefs, colIndex)

	// Union the rowids in branch scan order, deduplicating.
	ordered := unionBranchRowIDs(matches)

	// Pass 2: fetch the matching rows in order and apply the full WHERE as a
	// safety filter (the index prefixes cover only part of the predicate).
	needMaps := e.ctx.SelectNeedsRowMaps(s, tableEntry.Name)
	allRows, allRowMaps, res := e.fetchOrPlanRows(cursor, ordered, s, tableEntry, colDefs, needMaps)
	if res != nil {
		return res
	}

	// Aggregate and finalize exactly like the regular scan path.
	if result := e.ctx.HandleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	result := &Result{Columns: e.ctx.BuildColumnNames(s.Columns, colDefs, s), Rows: allRows}
	return e.ctx.FinalizeSelectResult(result, s, allRowMaps)
}

// branchMatch records one row that matched an OR branch's prefix, together
// with the index key values used to reproduce the index scan order.
type branchMatch struct {
	rowID int64
	key   []interface{}
}

// collectOrBranchMatches scans the table once, collecting for every branch the
// rowids of rows whose prefix columns match, plus their index key values.
func (e *DMLExecutor) collectOrBranchMatches(cursor *btree.Cursor, branches []orBranchPlan, colDefs []sql.ColumnDef, colIndex map[string]int) [][]branchMatch {
	matches := make([][]branchMatch, len(branches))
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		e.matchRowAgainstBranches(rec, cell.RowID, branches, colDefs, colIndex, matches)

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return matches
}

// matchRowAgainstBranches checks one decoded record against every OR branch
// and appends a branchMatch for each branch whose prefix it satisfies.
func (e *DMLExecutor) matchRowAgainstBranches(rec *storage.Record, rowID int64, branches []orBranchPlan, colDefs []sql.ColumnDef, colIndex map[string]int, matches [][]branchMatch) {
	row := e.ctx.BuildRowMap(rec, colDefs, rowID)
	for bi, br := range branches {
		if !e.orRowMatchesPrefix(row, br.Prefix) {
			continue
		}
		matches[bi] = append(matches[bi], branchMatch{rowID: rowID, key: branchKeyValues(rec, br, colIndex, rowID)})
	}
}

// branchKeyValues builds the index key value list for one branch match,
// appending the rowid as the final key component.
func branchKeyValues(rec *storage.Record, br orBranchPlan, colIndex map[string]int, rowID int64) []interface{} {
	keyVals := make([]interface{}, 0, len(br.IndexCols)+1)
	for _, ic := range br.IndexCols {
		idx, ok := colIndex[ic]
		if !ok || idx < 0 || idx >= len(rec.Values) {
			keyVals = append(keyVals, nil)
			continue
		}
		keyVals = append(keyVals, rec.Values[idx])
	}
	keyVals = append(keyVals, rowID)
	return keyVals
}

// unionBranchRowIDs unions the rowids across all branches in branch scan
// order, deduplicating. Within a branch, rows are sorted by the index key
// values (SQLite compares index keys value-wise, not by their serial-type
// byte encoding, so int 1 vs int 2 must order numerically).
func unionBranchRowIDs(matches [][]branchMatch) []int64 {
	seen := make(map[int64]bool)
	ordered := make([]int64, 0)
	for _, ms := range matches {
		sort.Slice(ms, func(i, j int) bool { return compareValueLists(ms[i].key, ms[j].key) < 0 })
		for _, m := range ms {
			if !seen[m.rowID] {
				seen[m.rowID] = true
				ordered = append(ordered, m.rowID)
			}
		}
	}
	return ordered
}

// fetchOrPlanRows fetches the matching rows in index order and applies the
// full WHERE predicate as a safety filter, building output rows and maps.
func (e *DMLExecutor) fetchOrPlanRows(cursor *btree.Cursor, ordered []int64, s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, needMaps bool) ([][]interface{}, []RowMap, *Result) {
	var allRows [][]interface{}
	var allRowMaps []RowMap
	for _, rid := range ordered {
		row, ok, res := e.fetchOrPlanRow(cursor, rid, s, colDefs)
		if res != nil {
			return nil, nil, res
		}
		if !ok {
			continue
		}
		allRows = append(allRows, e.ctx.BuildOutputRow(s.Columns, colDefs, row))
		if needMaps {
			allRowMaps = append(allRowMaps, row)
		}
	}
	return allRows, allRowMaps, nil
}

// fetchOrPlanRow seeks to a rowid, decodes the record, and applies the full
// WHERE predicate as a safety filter. Returns ok=false when the rowid is not
// found or the WHERE rejects the row.
func (e *DMLExecutor) fetchOrPlanRow(cursor *btree.Cursor, rid int64, s *sql.SelectStmt, colDefs []sql.ColumnDef) (RowMap, bool, *Result) {
	found, err := cursor.SeekToRowID(rid)
	if err != nil || !found {
		return nil, false, nil
	}
	payload, rowID, err := cursor.ReadCellData()
	if err != nil {
		return nil, false, nil
	}
	rec, err := storage.DecodeRecord(payload)
	if err != nil || rec == nil {
		return nil, false, nil
	}
	row := e.ctx.BuildRowMap(rec, colDefs, rowID)
	if s.Where != nil {
		pass, err := e.ctx.RowPassesWhere(s.Where, row, cursor)
		if err != nil {
			return nil, false, &Result{Error: err}
		}
		if !pass {
			return nil, false, nil
		}
	}
	return row, true, nil
}

// orRowMatchesPrefix reports whether the row satisfies every prefix
// constraint of an OR branch (all equalities; NULL never matches).
func (e *DMLExecutor) orRowMatchesPrefix(row RowMap, prefix []orConstraint) bool {
	for _, pc := range prefix {
		colVal, exists := row[pc.Col]
		if !exists || colVal == nil {
			return false
		}
		raw := util.UnwrapColumnValue(execexpr.UnwrapCollatedValue(colVal))
		if raw == nil || pc.Val == nil {
			return false
		}
		var cmp int
		if pc.ApplyAffinity {
			cmp = e.ctx.CompareValuesWithCollate(colVal, pc.Val)
		} else {
			cmp = e.ctx.CompareValuesWithCollate(raw, pc.Val)
		}
		if cmp != 0 {
			return false
		}
	}
	return true
}

// compareValueLists compares two index key value lists element-wise using
// SQLite's value comparison (NULL < INTEGER/REAL < TEXT < BLOB; INTEGER and
// REAL compare numerically). This reproduces the order a correctly-built
// index b-tree would produce, without relying on the serial-type byte
// encoding (which is not value-ordered across int 0/1 and other small ints).
func compareValueLists(a, b []interface{}) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := util.CompareValues(a[i], b[i]); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}
