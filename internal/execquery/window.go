// Package execquery implements query execution.
package execquery

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// This file owns window-function execution: computing per-row window values
// for SELECT columns that contain function calls with OVER clauses. The
// window pass runs after WHERE/JOIN/GROUP BY/HAVING have produced the
// intermediate row set, and before the final output rows are built.

// winRow pairs a source row with its original index in the input row set so
// window results computed on a sorted partition can be written back to the
// correct output position.
type winRow struct {
	row     RowMap
	origIdx int
}

// selectHasWindowFuncs reports whether any select column expression contains a
// window function (a function call with an OVER clause).
func (e *SelectEngine) selectHasWindowFuncs(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if e.exprHasWindowFunc(col.Expr) {
			return true
		}
	}
	return false
}

// exprHasWindowFunc reports whether an expression tree contains a window
// function call.
func (e *SelectEngine) exprHasWindowFunc(expr sql.Expr) bool {
	var found []*sql.FuncCall
	e.collectWindowFuncs(expr, &found)
	return len(found) > 0
}

// collectWindowChildren returns an expression node's child expressions in the
// window-collector traversal order: FuncCall nodes contribute their args,
// aggregate ORDER BY terms and FILTER clause; the composite node kinds follow
// in the same order the dedicated walkers used.
func collectWindowChildren(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.FuncCall:
		kids := make([]sql.Expr, 0, len(v.Args)+len(v.OrderBy)+1)
		kids = append(kids, v.Args...)
		for _, ob := range v.OrderBy {
			kids = append(kids, ob.Expr)
		}
		if v.Filter != nil {
			kids = append(kids, v.Filter)
		}
		return kids
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		left, right := BinaryExprOperands(expr)
		return []sql.Expr{left, right}
	case *sql.UnaryOp, *sql.ParenExpr, *sql.CastExpr, *sql.IsNull, *sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		return []sql.Expr{singleExprOperand(expr)}
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		kids := make([]sql.Expr, 0, len(v.List)+1)
		kids = append(kids, v.Operand)
		kids = append(kids, v.List...)
		return kids
	case *sql.CaseExpr:
		return caseExprChildren(v)
	case *sql.RowValue:
		return v.Values
	}
	return nil
}

// collectWindowFuncs appends every window function call node found in expr to
// out, in a deterministic depth-first order.
func (e *SelectEngine) collectWindowFuncs(expr sql.Expr, out *[]*sql.FuncCall) {
	if fc, ok := expr.(*sql.FuncCall); ok && fc.Over != nil {
		*out = append(*out, fc)
	}
	for _, kid := range collectWindowChildren(expr) {
		e.collectWindowFuncs(kid, out)
	}
}

// execWindowPass computes per-row window values for every window function in
// the select columns and builds the final output rows. It returns nil when the
// query has no window functions.
func (e *SelectEngine) execWindowPass(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if !e.selectHasWindowFuncs(s.Columns) {
		return nil
	}
	nodes := e.collectWindowNodes(s)
	results, err := e.computeWindowNodeResults(nodes, s.Windows, rowMaps)
	if err != nil {
		return &Result{Error: err}
	}
	// When no explicit ORDER BY exists, emit rows ordered by the concatenation
	// of every window's key (PARTITION BY + ORDER BY), in window order — the
	// first window's key is primary. SQLite produces this order by nesting one
	// subquery per distinct window: the innermost is sorted by the last
	// window's key and each outer level re-sorts stably by the previous
	// window's key, so the first window's key ends up primary. We reproduce it
	// by applying each window's stable key sort to a running permutation in
	// reverse window order.
	order := e.windowEmitOrder(nodes, s, rowMaps)
	rows := make([][]interface{}, len(rowMaps))
	for oi, origIdx := range order {
		rows[oi] = e.buildWindowOutputRow(s.Columns, colDefs, rowMaps[origIdx], origIdx, results)
	}
	columns := e.buildColumnNames(s.Columns, colDefs, s)
	// Rebuild row maps from the output rows so join materialization and a
	// trailing ORDER BY resolve result column names (including window
	// function result columns like "count(*) OVER (...)"). Also carry the
	// source row's columns so an ORDER BY term referencing a source column
	// not projected in the output (e.g. ORDER BY d on SELECT ... quote(d))
	// still resolves.
	outMaps := buildResultRowMaps(rows, columns)
	e.mergeSourceColsIntoOutMaps(outMaps, rowMaps, order, s.Columns)
	// Store each ORDER BY window function's computed value under its rendered
	// expression key so a trailing ORDER BY term that repeats the window
	// expression (a distinct AST node, e.g. ORDER BY RANK() OVER w) resolves
	// it without recomputing.
	e.storeOrderByWindowKeys(nodes, results, outMaps, order)
	return &Result{
		Columns: columns,
		Rows:    rows,
		rowMaps: outMaps,
	}
}

// collectWindowNodes gathers the statement's window function nodes: the select
// columns AND the ORDER BY clause, deduplicated by node pointer so a shared
// node is computed once. ORDER BY window expressions are separate AST nodes
// from their SELECT-list twins (e.g. ORDER BY RANK() OVER w on SELECT ...
// RANK() OVER w AS r), so they must be computed too for the trailing sort to
// resolve them.
func (e *SelectEngine) collectWindowNodes(s *sql.SelectStmt) []*sql.FuncCall {
	var nodes []*sql.FuncCall
	seen := make(map[*sql.FuncCall]bool)
	addNodes := func(cols []sql.SelectColumn) {
		for _, col := range cols {
			var colNodes []*sql.FuncCall
			e.collectWindowFuncs(col.Expr, &colNodes)
			for _, fn := range colNodes {
				if !seen[fn] {
					seen[fn] = true
					nodes = append(nodes, fn)
				}
			}
		}
	}
	addNodes(s.Columns)
	var obCols []sql.SelectColumn
	for _, ob := range s.OrderBy {
		obCols = append(obCols, sql.SelectColumn{Expr: ob.Expr})
	}
	addNodes(obCols)
	return nodes
}

// computeWindowNodeResults computes the per-row window values for every window
// function node.
func (e *SelectEngine) computeWindowNodeResults(nodes []*sql.FuncCall, windows []sql.WindowDef, rowMaps []RowMap) (map[*sql.FuncCall][]interface{}, error) {
	results := make(map[*sql.FuncCall][]interface{}, len(nodes))
	for _, fn := range nodes {
		vals, _, err := e.computeWindowFunc(fn, windows, rowMaps)
		if err != nil {
			return nil, err
		}
		results[fn] = vals
	}
	return results, nil
}

// windowEmitOrder returns the output row permutation: identity when an
// explicit ORDER BY (or set operation) drives the final order; otherwise the
// running window-key sort described in execWindowPass.
func (e *SelectEngine) windowEmitOrder(nodes []*sql.FuncCall, s *sql.SelectStmt, rowMaps []RowMap) []int {
	order := make([]int, len(rowMaps))
	for i := range rowMaps {
		order[i] = i
	}
	if len(s.OrderBy) == 0 && s.Union == nil {
		for i := len(nodes) - 1; i >= 0; i-- {
			over := e.resolveWindowDef(nodes[i].Over, s.Windows)
			order = e.sortPermByWindowKey(over, rowMaps, order)
		}
	}
	return order
}

// mergeSourceColsIntoOutMaps overlays the source rows' columns on the output
// row maps: for output columns that are plain column references (e.g. SELECT
// color), the source row value wins (it carries the column's declared
// collation so ORDER BY honors it); other source columns fill gaps so the
// output maps keep source-only columns.
func (e *SelectEngine) mergeSourceColsIntoOutMaps(outMaps []RowMap, rowMaps []RowMap, order []int, columns []sql.SelectColumn) {
	plainCols := make(map[string]bool)
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Table == "" && ref.Name != "*" {
			plainCols[ref.Name] = true
		}
	}
	for oi, origIdx := range order {
		if oi < len(outMaps) && origIdx < len(rowMaps) {
			mergeSourceRowCols(outMaps[oi], rowMaps[origIdx], plainCols)
		}
	}
}

// mergeSourceRowCols overlays one source row's columns on an output row map:
// plain column references take the source value (which carries the column's
// declared collation); other source columns fill gaps.
func mergeSourceRowCols(out, src RowMap, plainCols map[string]bool) {
	for k, v := range src {
		if plainCols[k] {
			out[k] = v
		} else if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
}

// storeOrderByWindowKeys writes each window function's computed value into the
// output row maps under its rendered expression key, in output order.
func (e *SelectEngine) storeOrderByWindowKeys(nodes []*sql.FuncCall, results map[*sql.FuncCall][]interface{}, outMaps []RowMap, order []int) {
	for _, fn := range nodes {
		if fn == nil {
			continue
		}
		key := sql.ExprString(fn)
		vals := results[fn]
		for oi, origIdx := range order {
			if oi < len(outMaps) && origIdx < len(vals) {
				outMaps[oi][key] = vals[origIdx]
			}
		}
	}
}

// buildWindowOutputRow builds one output row, substituting each window
// function's precomputed value (indexed by the row's original position).
func (e *SelectEngine) buildWindowOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row RowMap, rowIdx int, results map[*sql.FuncCall][]interface{}) []interface{} {
	outRow := make([]interface{}, 0, outputColumnCount(columns, colDefs))
	for _, col := range columns {
		ref, isStar := col.Expr.(*sql.ColumnRef)
		if isStar && ref.Name == "*" {
			if ref.Table != "" {
				outRow = appendQualifiedStar(outRow, e, ref.Table, colDefs, row)
			} else {
				outRow = appendUnqualifiedStar(outRow, colDefs, row)
			}
			continue
		}
		sub := e.substituteWindowValues(col.Expr, rowIdx, results)
		// In GROUP BY window mode, non-window output columns (aggregates like
		// sum(b)) carry their precomputed group value in the row map keyed by
		// the column name; reuse it instead of re-aggregating over the single
		// combined row.
		if v, ok := e.windowGroupColumnValue(col.Expr, col.As, row); ok {
			outRow = append(outRow, v)
			continue
		}
		v, err := e.ctx.EvalExpr(sub, row)
		if err != nil {
			outRow = append(outRow, nil)
			continue
		}
		outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(v)))
	}
	return outRow
}

// windowGroupColumnValue returns the precomputed GROUP BY output value for a
// non-window column when windowGroupOutputs is set, or ok=false. The output
// column is matched by its rendered expression text OR its alias.
func (e *SelectEngine) windowGroupColumnValue(expr sql.Expr, alias string, row RowMap) (interface{}, bool) {
	if e.windowGroupOutputs == nil || e.exprHasWindowFunc(expr) {
		return nil, false
	}
	if v, ok := windowGroupLookup(alias, e.windowGroupOutputs, row); ok {
		return v, true
	}
	name := sql.ExprString(expr)
	if v, ok := windowGroupLookup(name, e.windowGroupOutputs, row); ok {
		return v, true
	}
	// Resolve by the SELECT-list alias (e.g. sum(y) AS s: the partition or
	// window expression sum(y) resolves to the s output column).
	if v, ok := e.windowGroupAliasLookup(name, row); ok {
		return v, true
	}
	// Unaliased expression columns are named by their RAW SQL SPAN
	// (exprResultName: tight symbol operators, select1-6.5 "f1+F2"), so the
	// lookup must also try that rendering — sql.ExprString injects spaces
	// ("b = count(*)" vs the stored "b=count(*)") and the mismatch drops into
	// a re-evaluation that loses the source value's column affinity
	// (window9-4.1.2: b=count(*) compared the group's TEXT value against the
	// INTEGER aggregate with no affinity and returned 0 for every group).
	if span := exprResultName(expr); !strings.EqualFold(span, name) {
		return windowGroupLookup(span, e.windowGroupOutputs, row)
	}
	return nil, false
}

// windowGroupLookup returns the row value stored under the GROUP BY output
// column whose name matches want (case-insensitive), or ok=false.
func windowGroupLookup(want string, outputs []string, row RowMap) (interface{}, bool) {
	if want == "" {
		return nil, false
	}
	for _, cn := range outputs {
		if strings.EqualFold(want, cn) {
			if v, exists := row.Get(cn); exists {
				return unwrapCollatedValue(util.UnwrapColumnValue(v)), true
			}
		}
	}
	return nil, false
}

// windowGroupAliasLookup resolves a group output by the SELECT-list alias
// (e.g. sum(y) AS s: the expression sum(y) resolves to the s output column).
func (e *SelectEngine) windowGroupAliasLookup(name string, row RowMap) (interface{}, bool) {
	for _, sc := range e.windowGroupCols {
		if sc.As != "" && strings.EqualFold(sql.ExprString(sc.Expr), name) {
			if v, exists := row.Get(sc.As); exists {
				return unwrapCollatedValue(util.UnwrapColumnValue(v)), true
			}
		}
	}
	return nil, false
}

// substituteWindowValues returns a copy of expr with every window function
// call replaced by a literal holding that function's precomputed value for
// rowIdx.
func (e *SelectEngine) substituteWindowValues(expr sql.Expr, rowIdx int, results map[*sql.FuncCall][]interface{}) sql.Expr {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return e.substituteWindowFuncCall(v, rowIdx, results)
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		return e.substituteWindowPairNode(expr, rowIdx, results)
	case *sql.UnaryOp, *sql.ParenExpr, *sql.CastExpr, *sql.IsNull, *sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		return e.substituteWindowUnaryNode(expr, rowIdx, results)
	case *sql.Between:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		clone.Low = e.substituteWindowValues(v.Low, rowIdx, results)
		clone.High = e.substituteWindowValues(v.High, rowIdx, results)
		return &clone
	case *sql.InList:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		clone.List = e.substituteWindowExprList(v.List, rowIdx, results)
		return &clone
	case *sql.CaseExpr:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		var whens []sql.WhenClause
		for _, w := range v.Whens {
			whens = append(whens, sql.WhenClause{
				When: e.substituteWindowValues(w.When, rowIdx, results),
				Then: e.substituteWindowValues(w.Then, rowIdx, results),
			})
		}
		clone.Whens = whens
		if v.Else != nil {
			clone.Else = e.substituteWindowValues(v.Else, rowIdx, results)
		}
		return &clone
	case *sql.RowValue:
		clone := *v
		clone.Values = e.substituteWindowExprList(v.Values, rowIdx, results)
		return &clone
	default:
		return expr
	}
}

func (e *SelectEngine) substituteWindowExprList(exprs []sql.Expr, rowIdx int, results map[*sql.FuncCall][]interface{}) []sql.Expr {
	out := make([]sql.Expr, len(exprs))
	for i, x := range exprs {
		out[i] = e.substituteWindowValues(x, rowIdx, results)
	}
	return out
}

func (e *SelectEngine) substituteWindowOrderBy(terms []sql.OrderByTerm, rowIdx int, results map[*sql.FuncCall][]interface{}) []sql.OrderByTerm {
	out := make([]sql.OrderByTerm, len(terms))
	for i, t := range terms {
		t.Expr = e.substituteWindowValues(t.Expr, rowIdx, results)
		out[i] = t
	}
	return out
}

// substituteWindowFuncCall substitutes one function-call node: a window call
// becomes its precomputed value literal; a plain call clones with substituted
// args, aggregate ORDER BY terms and FILTER.
func (e *SelectEngine) substituteWindowFuncCall(fc *sql.FuncCall, rowIdx int, results map[*sql.FuncCall][]interface{}) sql.Expr {
	if fc.Over != nil {
		if vals, ok := results[fc]; ok && rowIdx >= 0 && rowIdx < len(vals) {
			return valueLiteralExpr(vals[rowIdx])
		}
		return &sql.NullLit{}
	}
	clone := *fc
	clone.Args = e.substituteWindowExprList(fc.Args, rowIdx, results)
	clone.OrderBy = e.substituteWindowOrderBy(fc.OrderBy, rowIdx, results)
	if fc.Filter != nil {
		clone.Filter = e.substituteWindowValues(fc.Filter, rowIdx, results)
	}
	return &clone
}

// substituteWindowPairNode clones a two-operand node (BinaryOp /
// Is[Not]DistinctFrom) and substitutes both operands' window values.
func (e *SelectEngine) substituteWindowPairNode(expr sql.Expr, rowIdx int, results map[*sql.FuncCall][]interface{}) sql.Expr {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		clone := *v
		clone.Left = e.substituteWindowValues(v.Left, rowIdx, results)
		clone.Right = e.substituteWindowValues(v.Right, rowIdx, results)
		return &clone
	case *sql.IsDistinctFrom:
		clone := *v
		clone.Left = e.substituteWindowValues(v.Left, rowIdx, results)
		clone.Right = e.substituteWindowValues(v.Right, rowIdx, results)
		return &clone
	case *sql.IsNotDistinctFrom:
		clone := *v
		clone.Left = e.substituteWindowValues(v.Left, rowIdx, results)
		clone.Right = e.substituteWindowValues(v.Right, rowIdx, results)
		return &clone
	}
	return expr
}

// substituteWindowUnaryNode clones a single-operand node and substitutes the
// operand's window values.
func (e *SelectEngine) substituteWindowUnaryNode(expr sql.Expr, rowIdx int, results map[*sql.FuncCall][]interface{}) sql.Expr {
	switch v := expr.(type) {
	case *sql.UnaryOp:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		return &clone
	case *sql.ParenExpr:
		clone := *v
		clone.Expr = e.substituteWindowValues(v.Expr, rowIdx, results)
		return &clone
	case *sql.CastExpr:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		return &clone
	case *sql.IsNull:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		return &clone
	case *sql.IsNotNull:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		return &clone
	case *sql.IsTrue:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		return &clone
	case *sql.IsFalse:
		clone := *v
		clone.Operand = e.substituteWindowValues(v.Operand, rowIdx, results)
		return &clone
	}
	return expr
}

// computeWindowFunc computes the per-row values for one window function call
// across the input row set. Named windows (OVER win) are resolved from the
// statement's WINDOW clause. It returns the per-input-row values and the
// output order (the sequence of original row indices in partition-major,
// ORDER BY order).
func (e *SelectEngine) computeWindowFunc(fn *sql.FuncCall, windows []sql.WindowDef, rowMaps []RowMap) ([]interface{}, []int, error) {
	over := e.resolveWindowDef(fn.Over, windows)
	results := make([]interface{}, len(rowMaps))
	if len(rowMaps) == 0 {
		return results, nil, nil
	}

	// Evaluate partition keys and group the rows.
	partitions, err := e.windowPartitions(over, rowMaps)
	if err != nil {
		return nil, nil, err
	}

	var outputOrder []int
	for _, part := range partitions {
		// Sort the partition by the window ORDER BY.
		part = e.sortWinRows(over.OrderBy, part)
		for _, wr := range part {
			outputOrder = append(outputOrder, wr.origIdx)
		}
		if err := e.computePartitionWindowValues(fn, over, part, results); err != nil {
			return nil, nil, err
		}
	}
	return results, outputOrder, nil
}

// ComputeWindowValues computes a single window function's value for every row
// in rowMaps (in rowMaps order). Exported for the DML executor's UPDATE ...
// SET window-function support: a SET expression like nth_value(15,2) OVER()
// must be evaluated over the UPDATE's matched rows (window1 73.4).
func (e *SelectEngine) ComputeWindowValues(fn *sql.FuncCall, windows []sql.WindowDef, rowMaps []RowMap) ([]interface{}, error) {
	vals, _, err := e.computeWindowFunc(fn, windows, rowMaps)
	return vals, err
}

// resolveWindowDef returns the effective window definition for a function's
// OVER clause: the inline definition, or the named WINDOW clause definition.
func (e *SelectEngine) resolveWindowDef(over *sql.WindowDef, windows []sql.WindowDef) *sql.WindowDef {
	if over == nil {
		return &sql.WindowDef{}
	}
	// Resolve a named-window reference: by Name ("OVER name") or by BaseName
	// ("OVER (name ORDER BY ...)"). Merge the base window's clauses with the
	// OVER's explicit additions.
	if merged, ok := e.resolveNamedWindow(over, windows); ok {
		return merged
	}
	return over
}

// resolveNamedWindow resolves a named-window reference against the WINDOW
// clause and overlays the OVER's explicit additions. ok=false when the OVER
// is inline (no reference name) or the name has no WINDOW entry.
func (e *SelectEngine) resolveNamedWindow(over *sql.WindowDef, windows []sql.WindowDef) (*sql.WindowDef, bool) {
	refName := over.Name
	if refName == "" {
		refName = over.BaseName
	}
	if refName == "" {
		return over, false
	}
	for i := range windows {
		if windows[i].Name != refName {
			continue
		}
		merged := e.mergeWindowChain(windows[i], windows)
		// The OVER's explicit clauses override the base window's.
		if len(over.Partitions) > 0 {
			merged.Partitions = over.Partitions
		}
		if len(over.OrderBy) > 0 {
			merged.OrderBy = over.OrderBy
		}
		if over.Frame != nil {
			merged.Frame = over.Frame
			merged.FrameSpec = over.FrameSpec
		}
		return &merged, true
	}
	return over, false
}

// mergeWindowChain follows a chain of named-window references (win2 AS (win1
// ORDER BY b)): the effective definition is the base's clauses merged with
// the intermediate definitions' additions.
func (e *SelectEngine) mergeWindowChain(base sql.WindowDef, windows []sql.WindowDef) sql.WindowDef {
	for base.BaseName != "" && base.Name != base.BaseName {
		parent, ok := e.findNamedWindow(base.BaseName, windows)
		if !ok {
			break
		}
		base = mergeWindowDefs(parent, base)
	}
	return base
}

// mergeWindowDefs merges a derived window definition (derived, which may
// reference a base via BaseName) over its base definition. The merged result
// represents the base window's clauses extended by the derived window's
// explicit additions; it keeps the BASE window's Name/BaseName so a longer
// reference chain terminates (win2 AS (win1 ...), win3 AS (win2 ...)).
func mergeWindowDefs(base, derived sql.WindowDef) sql.WindowDef {
	merged := base
	if len(derived.Partitions) > 0 {
		merged.Partitions = derived.Partitions
	}
	if len(derived.OrderBy) > 0 {
		merged.OrderBy = derived.OrderBy
	}
	if derived.Frame != nil {
		merged.Frame = derived.Frame
		merged.FrameSpec = derived.FrameSpec
	}
	return merged
}

// windowPartitions groups the row set by the window's PARTITION BY keys,
// preserving input order within each partition.
func (e *SelectEngine) windowPartitions(over *sql.WindowDef, rowMaps []RowMap) ([][]winRow, error) {
	// resolve.c window resolution: PARTITION BY expressions of a window
	// definition must resolve against the FROM rows — an unknown bare column
	// errors "no such column: NAME" instead of silently evaluating to NULL
	// (windowB-19.x: PARTITION BY fake_column). See validateWindowPartitionCols.
	if err := e.validateWindowPartitionCols(over, rowMaps); err != nil {
		return nil, err
	}
	if len(over.Partitions) == 0 {
		return singleWindowPartition(rowMaps), nil
	}
	return e.groupWindowPartitions(over.Partitions, rowMaps)
}

// validateWindowPartitionCols checks the window's PARTITION BY expressions
// against the first FROM row (all rows share the same column space). Window
// ORDER BY terms are NOT validated here: they may be correlated references to
// an outer query's columns (window1-55.x: row_number() OVER (ORDER BY t1_id)
// inside an IN-subquery over t3), which do not exist in the local row. The
// same holds for PARTITION BY in a correlated subquery (window1-44.x:
// d IN (SELECT sum(c) OVER (PARTITION BY d ...) FROM t3) FROM (SELECT *
// FROM t2) — d resolves to the outer t2 column), so the validation only
// runs for statements evaluated without an outer row scope.
func (e *SelectEngine) validateWindowPartitionCols(over *sql.WindowDef, rowMaps []RowMap) error {
	if len(rowMaps) == 0 || e.outerRow != nil || len(e.outerRows) != 0 {
		return nil
	}
	for _, ex := range over.Partitions {
		if err := validateWindowExprColumns(ex, rowMaps[0]); err != nil {
			return err
		}
	}
	return nil
}

// singleWindowPartition returns the whole row set as one partition (a window
// with no PARTITION BY).
func singleWindowPartition(rowMaps []RowMap) [][]winRow {
	part := make([]winRow, len(rowMaps))
	for i, row := range rowMaps {
		part[i] = winRow{row: row, origIdx: i}
	}
	return [][]winRow{part}
}

// groupWindowPartitions groups rows by the PARTITION BY key, emitting the
// partitions in SQLite's key order.
func (e *SelectEngine) groupWindowPartitions(partitions []sql.Expr, rowMaps []RowMap) ([][]winRow, error) {
	groups := make(map[string][]winRow)
	keyVals := make(map[string][]interface{})
	var order []string
	for i, row := range rowMaps {
		key, vals, err := e.windowPartitionKey(partitions, row)
		if err != nil {
			return nil, err
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
			keyVals[key] = vals
		}
		groups[key] = append(groups[key], winRow{row: row, origIdx: i})
	}
	// SQLite emits window partitions sorted by the PARTITION BY key values
	// (ascending, NULLs first), not by first-seen order.
	sort.SliceStable(order, func(i, j int) bool {
		a := keyVals[order[i]]
		b := keyVals[order[j]]
		n := len(a)
		if len(b) < n {
			n = len(b)
		}
		for k := 0; k < n; k++ {
			c := util.CompareValues(a[k], b[k])
			if c != 0 {
				return c < 0
			}
		}
		return len(a) < len(b)
	})
	out := make([][]winRow, 0, len(order))
	for _, k := range order {
		out = append(out, groups[k])
	}
	return out, nil
}

// windowPartitionKey evaluates the PARTITION BY expressions for one row into a
// deterministic string key and the evaluated key values.
func (e *SelectEngine) windowPartitionKey(partitions []sql.Expr, row RowMap) (string, []interface{}, error) {
	var sb strings.Builder
	vals := make([]interface{}, 0, len(partitions))
	for i, p := range partitions {
		v, err := e.ctx.EvalExpr(p, row)
		if err != nil {
			return "", nil, err
		}
		// In GROUP BY window mode, a partition expression that is itself a
		// GROUP BY aggregate (e.g. PARTITION BY sum(y)) resolves from the
		// output column value.
		if e.windowGroupOutputs != nil {
			if ov, ok := e.windowGroupColumnValue(p, "", row); ok {
				v = ov
			}
		}
		// Apply the partition expression's collation (a COLLATE-wrapped value
		// carries it; e.g. PARTITION BY name on a COLLATE NOCASE column groups
		// 'apple' and 'APPLE' together).
		raw, coll := execexpr.ExtractValue(v)
		_ = coll
		raw = util.UnwrapColumnValue(raw)
		vals = append(vals, raw)
		if i > 0 {
			sb.WriteByte(0x1f)
		}
		sb.WriteString(windowValueKeyCollated(raw, coll))
	}
	return sb.String(), vals, nil
}

// windowValueKeyCollated renders a partition key component honouring the
// value's collation: TEXT values under a NOCASE collation fold to lowercase.
func windowValueKeyCollated(v interface{}, coll string) string {
	if s, ok := v.(string); ok && strings.EqualFold(coll, "NOCASE") {
		return windowValueKey(strings.ToLower(s))
	}
	return windowValueKey(v)
}

// windowValueKey renders a value as a stable partition key component. NULLs
// are a distinct partition key (SQLite: NULLs in PARTITION BY form their own
// group).
func windowValueKey(v interface{}) string {
	if v == nil {
		return "\x00null"
	}
	switch x := v.(type) {
	case int64:
		return "\x01i" + strconv.FormatInt(x, 10)
	case float64:
		return "\x02f" + strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "\x03s" + x
	case []byte:
		return "\x04b" + string(x)
	default:
		return "\x05x" + fmt.Sprintf("%v", v)
	}
}

// sortWinRows stably sorts a partition slice by the window ORDER BY terms.
func (e *SelectEngine) sortWinRows(orderBy []sql.OrderByTerm, rows []winRow) []winRow {
	if len(orderBy) == 0 || len(rows) <= 1 {
		return rows
	}
	sorted := make([]winRow, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		return e.compareCollatedOrderBy(orderBy, sorted[i].row, sorted[j].row) < 0
	})
	return sorted
}

// sortPermByWindowKey stably sorts a row-index permutation by the window's
// combined key (PARTITION BY expressions, then ORDER BY expressions), matching
// the order SQLite uses for the final output of a multi-window query.
func (e *SelectEngine) sortPermByWindowKey(over *sql.WindowDef, rowMaps []RowMap, perm []int) []int {
	if len(perm) <= 1 {
		return perm
	}
	out := make([]int, len(perm))
	copy(out, perm)
	sort.SliceStable(out, func(i, j int) bool {
		return e.windowKeyCompare(over, rowMaps[out[i]], rowMaps[out[j]]) < 0
	})
	return out
}

// windowKeyCompare returns <0 if row a sorts before row b under the window's
// combined key: PARTITION BY expressions first (ascending, collation aware),
// then ORDER BY expressions.
func (e *SelectEngine) windowKeyCompare(over *sql.WindowDef, a, b RowMap) int {
	for _, p := range over.Partitions {
		if cmp := e.windowPartitionTermCompare(p, a, b); cmp != 0 {
			return cmp
		}
	}
	return e.compareCollatedOrderBy(over.OrderBy, a, b)
}

// windowPartitionTermCompare compares two rows under one PARTITION BY term
// (collation aware). Unevaluable terms compare equal (0).
func (e *SelectEngine) windowPartitionTermCompare(p sql.Expr, a, b RowMap) int {
	coll, _ := execexpr.ExprCollation(p)
	pe := stripCollate(p)
	vi, errI := e.ctx.EvalExpr(pe, a)
	vj, errJ := e.ctx.EvalExpr(pe, b)
	// In GROUP BY window mode, a partition expression that is itself a
	// GROUP BY aggregate (e.g. PARTITION BY sum(y)) resolves from the
	// output column value (matching windowPartitionKey).
	if e.windowGroupOutputs != nil {
		if ov, ok := e.windowGroupColumnValue(p, "", a); ok {
			vi = ov
		}
		if ov, ok := e.windowGroupColumnValue(p, "", b); ok {
			vj = ov
		}
	}
	if errI != nil || errJ != nil {
		return 0
	}
	viRaw, collI := execexpr.ExtractValue(vi)
	vjRaw, collJ := execexpr.ExtractValue(vj)
	if coll == "" {
		coll = collI
	}
	if coll == "" {
		coll = collJ
	}
	return e.ctx.CompareValuesCollate(viRaw, vjRaw, coll)
}

// computePartitionWindowValues fills results[origIdx] for every row in the
// (sorted) partition based on the window function type.
func (e *SelectEngine) computePartitionWindowValues(fn *sql.FuncCall, over *sql.WindowDef, part []winRow, results []interface{}) error {
	name := strings.ToUpper(fn.Name)
	switch name {
	case "ROW_NUMBER", "RANK", "DENSE_RANK", "PERCENT_RANK", "CUME_DIST", "NTILE":
		return e.computeRankingWindow(fn, name, over, part, results)
	case "LEAD", "LAG":
		return e.computeLeadLagWindow(fn, name, over, part, results)
	case "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE":
		return e.computeValueWindow(fn, name, over, part, results)
	default:
		return e.computeAggregateWindow(fn, over, part, results)
	}
}

// frameBounds computes the [start, end) row indices for the current row's
// frame within the sorted partition, following SQLite window semantics.
// ROWS/GROUPS frames use physical positions; RANGE frames use ORDER BY value
// peers. Returns an error for invalid frame offsets.
func (e *SelectEngine) frameBounds(over *sql.WindowDef, part []winRow, current int) (int, int, error) {
	// Default frame: no ORDER BY → whole partition; ORDER BY → RANGE
	// UNBOUNDED PRECEDING to CURRENT ROW (peers of current).
	if over.Frame == nil {
		if len(over.OrderBy) == 0 {
			return 0, len(part), nil
		}
		start := 0
		end := current + 1
		for end < len(part) && e.winRowsArePeers(over.OrderBy, part[current], part[end]) {
			end++
		}
		return start, end, nil
	}
	f := over.Frame
	switch f.Type {
	case "ROWS":
		start, end, err := e.rowsFrameBounds(f, part, current)
		return start, end, err
	case "RANGE":
		return e.rangeFrameBounds(over, f, part, current)
	case "GROUPS":
		return e.groupsFrameBounds(over, f, part, current)
	}
	return 0, len(part), nil
}

// rowsFrameBounds computes a ROWS frame's [start, end) indices.
func (e *SelectEngine) rowsFrameBounds(f *sql.WindowFrame, part []winRow, current int) (int, int, error) {
	n := len(part)
	if !f.Between {
		// Shorthand "ROWS N PRECEDING" == BETWEEN N PRECEDING AND CURRENT ROW.
		start, err := e.frameOffset(f.Start, part[current].row, true)
		if err != nil {
			return 0, 0, err
		}
		return e.clampFrame(current-start, current+1, n)
	}
	start, err := e.rowsBoundIndex(f.Start, part, current, true)
	if err != nil {
		return 0, 0, err
	}
	end, err := e.rowsBoundIndex(f.End, part, current, false)
	if err != nil {
		return 0, 0, err
	}
	return e.clampFrame(start, end+1, n)
}

// rowsBoundIndex resolves one ROWS frame bound to a row index.
func (e *SelectEngine) rowsBoundIndex(b sql.FrameBound, part []winRow, current int, isStart bool) (int, error) {
	n := len(part)
	switch b.Kind {
	case "UNBOUNDED PRECEDING":
		return 0, nil
	case "UNBOUNDED FOLLOWING":
		return n - 1, nil
	case "CURRENT ROW":
		return current, nil
	case "PRECEDING":
		off, err := e.frameOffset(b, part[current].row, isStart)
		if err != nil {
			return 0, err
		}
		return current - off, nil
	case "FOLLOWING":
		off, err := e.frameOffset(b, part[current].row, isStart)
		if err != nil {
			return 0, err
		}
		return current + off, nil
	}
	return 0, nil
}

// frameOffset evaluates a PRECEDING/FOLLOWING bound's offset expression for
// the current row and validates it is a non-negative integer. isStart selects
// the "starting"/"ending" offset error message.

// validateWindowExprColumns checks a window PARTITION BY / ORDER BY
// expression's bare column references against the row's column space,
// erroring "no such column: NAME" for the first unknown one. The walk does
// not descend into subqueries (not permitted in window expressions anyway).
func validateWindowExprColumns(expr sql.Expr, row RowMap) error {
	if expr == nil {
		return nil
	}
	if bad := windowUnresolvedColRef(expr, row); bad != "" {
		return fmt.Errorf("no such column: %s", bad)
	}
	return nil
}

// windowUnresolvedColRef returns the first unresolvable column reference name
// in expr (""): rowid aliases are always accepted, qualified references may
// use "table.column" keys, and subquery subtrees are skipped.
func windowUnresolvedColRef(expr sql.Expr, row RowMap) string {
	var bad string
	WalkExprFull(expr, func(n sql.Expr) {
		if bad != "" {
			return
		}
		if name := windowUnresolvedRefName(n, row); name != "" {
			bad = name
		}
	})
	return bad
}

// windowUnresolvedRefName returns n's unresolved column name, or "" when n is
// not a column reference, is a rowid alias, is inside a skipped subtree, or
// resolves against row.
func windowUnresolvedRefName(n sql.Expr, row RowMap) string {
	switch n.(type) {
	case *sql.Subquery, *sql.ExistsExpr:
		return ""
	}
	ref, ok := n.(*sql.ColumnRef)
	if !ok {
		return ""
	}
	lower := strings.ToLower(ref.Name)
	if lower == "rowid" || lower == "_rowid_" || lower == "oid" {
		return ""
	}
	if ref.Table != "" {
		// Qualified refs may be stored as "table.column" keys.
		if _, exists := row.Get(ref.Table + "." + ref.Name); !exists {
			if _, exists2 := row.Get(ref.Name); !exists2 {
				return ref.Table + "." + ref.Name
			}
		}
		return ""
	}
	if _, exists := row.Get(ref.Name); !exists {
		return ref.Name
	}
	return ""
}
