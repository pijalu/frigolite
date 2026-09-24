// Package exec implements query execution.
package execquery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// This file owns aggregate evaluation: evaluating aggregate functions across
// rows (with and without GROUP BY), distinct aggregates, and the correlated
// outer-row aggregate paths used by subqueries. The functions were extracted
// from select.go (task CX-03) and each reduced to ≤15 gocognit / ≤12 gocyclo.
// GROUP BY key partitioning lives in select_agg_group.go.

// evalAggCallArgs evaluates the arguments of an aggregate function call for a
// single row, unwrapping column values and substituting nil on error. Each
// argument evaluates inside an aggregate-argument marker (C resolves
// aggregate arguments to TK_AGG_COLUMN, which the fts5 aux overload rewrite
// does not match), so aux calls inside aggregate arguments fail with the
// placeholder error.
func (e *SelectEngine) evalAggCallArgs(fn *sql.FuncCall, row RowMap) []interface{} {
	args := make([]interface{}, len(fn.Args))
	for i, arg := range fn.Args {
		restore := e.ctx.EnterAuxAggArg()
		v, err := e.ctx.EvalExpr(arg, row)
		restore()
		if err != nil {
			// An fts5 aux failure inside an aggregate argument aborts the
			// statement (the overload placeholder error propagates; C's aux
			// callback returns SQLITE_ERROR from inside the aggregate step).
			if fc, isFC := arg.(*sql.FuncCall); isFC && execexpr.IsFTS5AuxFunc(fc.Name) {
				e.aggPendingErr = err
			}
			args[i] = nil
		} else {
			// Mirror evalFuncArgs: peel both ColumnValue affinity wrappers
			// and CollatedValue collation markers so aggregates receive the
			// raw scalar (a COLLATE'd argument like c1 COLLATE nocase must
			// not leak the marker into the aggregate's input).
			args[i] = unwrapCollatedValue(util.UnwrapColumnValue(v))
		}
	}
	return args
}

// orderByExprs projects a slice of ORDER BY terms to their expression slice.
func orderByExprs(terms []sql.OrderByTerm) []sql.Expr {
	exprs := make([]sql.Expr, len(terms))
	for i, t := range terms {
		exprs[i] = t.Expr
	}
	return exprs
}

// aggExprListHasSubquery reports whether any expression in exprs contains a
// subquery (such aggregates need inner rows for the subquery to evaluate).
func aggExprListHasSubquery(exprs []sql.Expr) bool {
	for _, expr := range exprs {
		if exprContainsSubquery(expr) {
			return true
		}
	}
	return false
}

// scanAggExprRefs scans exprs for column references. innerMatch is set when any
// expression references a column in innerColNames; hasColRefs is set when any
// expression has at least one column reference.
func (e *SelectEngine) scanAggExprRefs(exprs []sql.Expr, innerColNames map[string]bool) (innerMatch, hasColRefs bool) {
	for _, expr := range exprs {
		if e.exprHasColumnRef(expr) {
			hasColRefs = true
			if innerColNames != nil && exprHasColRefInMap(expr, innerColNames) {
				innerMatch = true
			}
		}
	}
	return
}

// aggRowPassesFilter reports whether row satisfies the aggregate's optional
// FILTER clause. A nil FILTER passes every row.
func (e *SelectEngine) aggRowPassesFilter(v *sql.FuncCall, row RowMap) bool {
	if v.Filter == nil {
		return true
	}
	filterVal, err := e.ctx.EvalExpr(v.Filter, row)
	return err == nil && execexpr.ToBool(filterVal)
}

// findAggNestedAggregates returns the name of the first nested aggregate found
// in v's arguments or ORDER BY terms, or "" if none (nested aggregates are
// prohibited by SQLite).
func (e *SelectEngine) findAggNestedAggregates(v *sql.FuncCall) string {
	for _, arg := range v.Args {
		if nested := findNestedAggregate(arg, e.ctx.Functions()); nested != "" {
			return nested
		}
	}
	for _, ob := range v.OrderBy {
		if nested := findNestedAggregate(ob.Expr, e.ctx.Functions()); nested != "" {
			return nested
		}
	}
	return ""
}

// compareCollatedOrderBy returns the first non-zero comparison of two rows by
// the given ORDER BY terms, honouring COLLATE clauses. A negative result means
// a sorts before b. ORDER BY terms whose evaluation errors are skipped.
func (e *SelectEngine) compareCollatedOrderBy(orderBy []sql.OrderByTerm, a, b RowMap) int {
	for _, ob := range orderBy {
		coll := orderByTermCollation(ob.Expr)
		obExpr := stripCollate(ob.Expr)
		vi, errI := e.ctx.EvalExpr(obExpr, a)
		vj, errJ := e.ctx.EvalExpr(obExpr, b)
		if errI != nil || errJ != nil {
			continue
		}
		// NULLS FIRST / NULLS LAST override the direction default (NULLs sort
		// first for ASC, last for DESC when unspecified).
		if cmp, isNullCmp := orderByNullPlacement(ob, execexpr.IsSQLNull(vi), execexpr.IsSQLNull(vj)); isNullCmp {
			return cmp
		}
		// A value wrapped in a collated column carries its declared collation
		// (e.g. a COLLATE NOCASE column); prefer it over the ORDER BY term's
		// own collation when the term has none.
		viRaw, collI := execexpr.ExtractValue(vi)
		vjRaw, collJ := execexpr.ExtractValue(vj)
		if coll == "" {
			coll = collI
		}
		if coll == "" {
			coll = collJ
		}
		cmp := e.ctx.CompareValuesCollate(viRaw, vjRaw, coll)
		if cmp != 0 {
			if ob.Desc {
				return -cmp
			}
			return cmp
		}
	}
	return 0
}

// orderByNullPlacement compares a pair where at least one side is NULL.
// ok=false when both sides share NULL-ness (nothing decided). NULLS FIRST /
// NULLS LAST override the direction default (NULLs sort first for ASC, last
// for DESC when unspecified).
func orderByNullPlacement(ob sql.OrderByTerm, leftNull, rightNull bool) (int, bool) {
	if leftNull == rightNull {
		return 0, false
	}
	nullsFirst := ob.NullsFirst
	if ob.NullsLast {
		nullsFirst = false
	}
	if !ob.NullsFirst && !ob.NullsLast {
		nullsFirst = !ob.Desc
	}
	if leftNull == nullsFirst {
		return -1, true
	}
	return 1, true
}

// sortRowMapsByOrderBy sorts rows by the aggregate's ORDER BY terms (collation
// aware) and returns the sorted copy, or rows unchanged when there is nothing
// to sort.
func (e *SelectEngine) sortRowMapsByOrderBy(orderBy []sql.OrderByTerm, rows []RowMap) []RowMap {
	if len(orderBy) == 0 || len(rows) <= 1 {
		return rows
	}
	sorted := make([]RowMap, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		return e.compareCollatedOrderBy(orderBy, sorted[i], sorted[j]) < 0
	})
	return sorted
}

// findAggOrderBy returns the ORDER BY terms of the first aggregate output
// column that has them, and whether that aggregate is MAX. Returns (nil, false)
// when no aggregate column carries ORDER BY.
func (e *SelectEngine) findAggOrderBy(cols []sql.SelectColumn) ([]sql.OrderByTerm, bool) {
	for _, col := range cols {
		if fn, ok := col.Expr.(*sql.FuncCall); ok && len(fn.OrderBy) > 0 {
			return fn.OrderBy, strings.ToUpper(fn.Name) == "MAX"
		}
	}
	return nil, false
}

// comparePlainOrderBy returns the first non-zero binary comparison of two rows
// by the given ORDER BY terms. A negative result means a sorts before b.
// ORDER BY terms whose evaluation errors are skipped.
func (e *SelectEngine) comparePlainOrderBy(orderBy []sql.OrderByTerm, a, b RowMap) int {
	for _, ob := range orderBy {
		vi, errI := e.ctx.EvalExpr(ob.Expr, a)
		vj, errJ := e.ctx.EvalExpr(ob.Expr, b)
		if errI != nil || errJ != nil {
			continue
		}
		cmp := util.CompareValues(vi, vj)
		if cmp != 0 {
			if ob.Desc {
				return -cmp
			}
			return cmp
		}
	}
	return 0
}

// sortRowMapsForAggOrderBy sorts rowMaps by an aggregate's ORDER BY terms so
// bare columns evaluate from the correct row. For MAX ORDER BY the value comes
// from the last row, which is rotated to the front. rowMaps is returned
// unchanged when there is nothing to sort.
func (e *SelectEngine) sortRowMapsForAggOrderBy(orderBy []sql.OrderByTerm, isMax bool, rowMaps []RowMap) []RowMap {
	if len(orderBy) == 0 || len(rowMaps) <= 1 {
		return rowMaps
	}
	sorted := make([]RowMap, len(rowMaps))
	copy(sorted, rowMaps)
	sort.SliceStable(sorted, func(i, j int) bool {
		return e.comparePlainOrderBy(orderBy, sorted[i], sorted[j]) < 0
	})
	if isMax {
		sorted[0] = sorted[len(sorted)-1]
	}
	return sorted
}

// evalAggOutputRow evaluates each output column as an aggregate expression over
// rowMaps, unwrapping column values for display. Star columns are expanded to
// the underlying table columns, taking their values from the first row in the
// group (SQLite's aggregate semantics for bare columns).
func (e *SelectEngine) evalAggOutputRow(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) ([]interface{}, error) {
	var outRow []interface{}
	for _, col := range s.Columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			if len(rowMaps) > 0 {
				e.appendStarCols(&outRow, ref, colDefs, rowMaps[0])
			}
			continue
		}
		// Window-function columns are computed by the window pass over the
		// collapsed row; leave a placeholder here.
		if e.exprHasWindowFunc(col.Expr) {
			outRow = append(outRow, nil)
			continue
		}
		v, err := e.evalAggregateExpr(col.Expr, rowMaps)
		if err != nil {
			return nil, err
		}
		outRow = append(outRow, util.UnwrapColumnValue(v))
	}
	return outRow, nil
}

// appendEmptyStarCols appends one NULL per expanded star column. It is used
// when an aggregate query produced no rows: the star's underlying columns are
// still present in the result, each with a NULL value (no row exists to read
// real values from).
func (e *SelectEngine) appendEmptyStarCols(outRow *[]interface{}, ref *sql.ColumnRef, colDefs []sql.ColumnDef) {
	if ref.Table != "" {
		names := e.qualifiedStarResolveNames(ref.Table, colDefs, nil)
		for range names {
			*outRow = append(*outRow, nil)
		}
		return
	}
	for _, cd := range colDefs {
		if cd.Dropped || IsHiddenColumnDef(cd) {
			continue
		}
		*outRow = append(*outRow, nil)
	}
}

// emptyAggValue returns the value an aggregate produces over zero rows: COUNT
// is 0, TOTAL is 0.0, and others either report a defined empty-input Final or
// NULL.
func (e *SelectEngine) emptyAggValue(f *function.Func) interface{} {
	switch f.Name {
	case "COUNT":
		return int64(0)
	case "TOTAL":
		return float64(0.0)
	default:
		if f.AggregateFn != nil {
			agg := f.AggregateFn()
			if res, err := agg.Final(); err == nil {
				return res
			}
		}
		return nil
	}
}

// appendStarCols expands a bare-star or qualified-star output column for a
// GROUP BY group, appending each expanded value to outRow.
func (e *SelectEngine) appendStarCols(outRow *[]interface{}, ref *sql.ColumnRef, colDefs []sql.ColumnDef, groupRow RowMap) {
	if ref.Table != "" {
		for _, cd := range e.qualifiedStarColNames(ref.Table, colDefs, groupRow) {
			*outRow = append(*outRow, util.UnwrapColumnValue(unwrapCollatedValue(cd.value)))
		}
		return
	}
	for _, cd := range colDefs {
		if cd.Dropped || IsHiddenColumnDef(cd) {
			continue
		}
		if val, exists := groupRow.Get(cd.Name); exists {
			*outRow = append(*outRow, util.UnwrapColumnValue(unwrapCollatedValue(val)))
		}
	}
}

// buildResultRowMaps builds per-result-row RowMaps keyed by column name so a
// trailing ORDER BY can resolve its terms against the result columns.
func buildResultRowMaps(rows [][]interface{}, columns []string) []RowMap {
	maps := make([]RowMap, len(rows))
	for i, row := range rows {
		m := make(RowMap, len(row))
		for j, v := range row {
			if j < len(columns) {
				m[columns[j]] = v
			}
		}
		maps[i] = m
	}
	return maps
}

// buildNoAggGroupRow builds the output row for one GROUP BY group (without
// aggregates), replacing output columns that are themselves GROUP BY
// expressions with the group's key value.
func (e *SelectEngine) buildNoAggGroupRow(s *sql.SelectStmt, colDefs []sql.ColumnDef, groupBy []sql.Expr, row RowMap, groupVals []interface{}) ([]interface{}, error) {
	outRow, err := e.buildOutputRow(s.Columns, colDefs, row)
	if err != nil {
		return nil, err
	}
	for ci := range s.Columns {
		if gi := matchGroupByExpr(groupBy, s.Columns[ci].Expr); gi >= 0 && gi < len(groupVals) {
			if ci < len(outRow) {
				outRow[ci] = groupVals[gi]
			}
		}
	}
	return outRow, nil
}

// buildGroupByAggRow builds the output row for one GROUP BY group with
// aggregates: GROUP BY expressions emit the group's key value, star columns are
// expanded, and other columns are evaluated as aggregate expressions.
func (e *SelectEngine) buildGroupByAggRow(s *sql.SelectStmt, colDefs []sql.ColumnDef, groupBy []sql.Expr, groupVals []interface{}, groupRows []RowMap) ([]interface{}, error) {
	var outRow []interface{}
	for _, col := range s.Columns {
		if gi := matchGroupByExpr(groupBy, col.Expr); gi >= 0 && gi < len(groupVals) {
			outRow = append(outRow, groupVals[gi])
			continue
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			e.appendStarCols(&outRow, ref, colDefs, groupRows[0])
			continue
		}
		// Window-function columns are computed by the window pass over the
		// group output rows; leave a placeholder here (evaluating the window
		// expression per group would hit nested-aggregate rejection).
		if e.exprHasWindowFunc(col.Expr) {
			outRow = append(outRow, nil)
			continue
		}
		v, err := e.evalAggregateExpr(col.Expr, groupRows)
		if err != nil {
			return nil, err
		}
		outRow = append(outRow, util.UnwrapColumnValue(v))
	}
	return outRow, nil
}

// dedupeAggRows filters rowMaps by the aggregate's FILTER clause and removes
// duplicate argument tuples, preserving first-seen order.
func (e *SelectEngine) dedupeAggRows(v *sql.FuncCall, rowMaps []RowMap) []RowMap {
	seen := make(map[string]bool)
	var uniqueRows []RowMap
	for _, row := range rowMaps {
		if !e.aggRowPassesFilter(v, row) {
			continue
		}
		key := distinctKey(e.evalAggCallArgs(v, row))
		if !seen[key] {
			seen[key] = true
			uniqueRows = append(uniqueRows, row)
		}
	}
	return uniqueRows
}

// evalAggOverOuterRows evaluates aggregate functions across all outerRows and
// non-aggregate expressions with a nil row.
func (e *SelectEngine) evalAggOverOuterRows(s *sql.SelectStmt, outerRows []RowMap) []interface{} {
	return e.evalAggOverOuterRowsWithInner(s, outerRows, nil)
}

// aggregateHasOnlyOuterRefs checks whether an aggregate function's arguments,
// FILTER clause, and ORDER BY terms reference only outer columns (none from
// the inner table). Returns true only when the aggregate has at least one
// column reference and none of them match the inner column set — resolve.c's
// sqlite3ReferencesSrcList ownership test: the whole aggregate expression
// (args + FILTER + ORDER BY) must reference no inner column for the
// aggregate to belong to the outer aggregate context.
func (e *SelectEngine) aggregateHasOnlyOuterRefs(fn *sql.FuncCall, innerColNames map[string]bool) bool {
	if aggExprListHasSubquery(fn.Args) || aggExprListHasSubquery(orderByExprs(fn.OrderBy)) ||
		(fn.Filter != nil && aggExprListHasSubquery([]sql.Expr{fn.Filter})) {
		return false
	}
	// A FILTER bound to the subquery's own rows keeps the aggregate
	// inner-evaluated: it can never promote to the outer query
	// (filter1-6.1: COUNT(a) FILTER(WHERE x) with x inner stays per-row).
	if fn.Filter != nil {
		if fInner, _ := e.scanAggExprRefs([]sql.Expr{fn.Filter}, innerColNames); fInner {
			return false
		}
	}
	aInner, aHas := e.scanAggExprRefs(fn.Args, innerColNames)
	oInner, oHas := e.scanAggExprRefs(orderByExprs(fn.OrderBy), innerColNames)
	fInner, fHas := e.scanAggExprRefs(filterExprs(fn), innerColNames)
	return !aInner && !oInner && !fInner && (aHas || oHas || fHas)
}

// filterExprs returns the FILTER clause of an aggregate call as a one-element
// slice (nil when absent), so it can share the aggregate-expression scanners.
func filterExprs(fn *sql.FuncCall) []sql.Expr {
	if fn.Filter == nil {
		return nil
	}
	return []sql.Expr{fn.Filter}
}

// isAggregateFuncCallName reports whether the named function is a registered
// aggregate.
func (e *SelectEngine) isAggregateFuncCallName(name string) bool {
	reg, found := e.ctx.Functions().Find(name)
	return found && reg.Type == function.TypeAggregate
}

// evalAggOverOuterRowsWithInner evaluates a fully-correlated subquery's output
// row: aggregate functions across outerRows, window functions over the single
// collapsed row, and non-aggregate expressions against the first inner row (or
// nil when there are no inner rows). This handles both direct aggregate columns
// (count(a)) and aggregate expressions (max(y)+sum(0) OVER ()) — window1 76.5:
// (SELECT max(y)+sum(0) OVER ()) over a GROUP BY group aggregates max(y) over
// the group's rows and runs the window over the one collapsed row.
func (e *SelectEngine) evalAggOverOuterRowsWithInner(s *sql.SelectStmt, outerRows, allRowMaps []RowMap) []interface{} {
	var innerRow RowMap
	if len(allRowMaps) > 0 {
		innerRow = allRowMaps[0]
	}
	// C semantics (resolve.c + select.c): a subquery WITH a FROM clause scans
	// its own tables — the aggregate steps over the INNER rows, its FILTER
	// evaluates on the inner row, and argument names missing from the inner
	// row resolve as outer constants (constant per outer-row evaluation, so
	// the first outer row is the representative). FROM-less correlated
	// aggregates (SELECT (SELECT max(y)) with y outer) keep stepping over the
	// outer rows. filter1-6.1: COUNT(a) FILTER(WHERE x) with a outer and x
	// inner counts the inner rows, not the outer rows.
	haveFrom := s.From.Name != "" || s.From.Subquery != nil || len(s.From.Args) > 0
	stepping := aggSteppingRows(allRowMaps, outerRows, haveFrom)
	innerColNames := collectRowMapKeys(allRowMaps)
	defer func() { e.aggRowMaps = nil }()
	var outRow []interface{}
	for _, col := range s.Columns {
		if e.exprHasWindowFunc(col.Expr) {
			// A window column in a correlated aggregate subquery: evaluate the
			// aggregate parts against the outer rows first, then the window
			// pass fills the single collapsed row (mirrors evalAggregates).
			outRow = append(outRow, nil)
			continue
		}
		e.aggRowMaps = e.chooseAggRowMaps(col.Expr, innerColNames, stepping, outerRows, haveFrom)
		v, err := e.ctx.EvalExpr(col.Expr, innerRow)
		if err != nil {
			outRow = append(outRow, nil)
		} else {
			outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(v)))
		}
	}
	// Window-function columns run the window pass over the single collapsed
	// row; the window's arguments resolve against the outer rows (the
	// correlated aggregate inputs).
	if e.selectHasWindowFuncs(s.Columns) {
		cols := e.buildColumnNames(s.Columns, nil, s)
		outRow = e.runWindowOverCollapsedRow(s, innerRow, outRow, cols, outerRows, nil)
	}
	return outRow
}

// chooseAggRowMaps implements resolve.c:1332 — the first context whose
// SrcList the aggregate expression references owns it. A pure-outer aggregate
// (no inner column in args/FILTER/ORDER BY, aggnested-1.1 string_agg(a1,'x'),
// filter1-6.3 COUNT(a)) belongs to the OUTER aggregate context and steps the
// outer rows; an aggregate touching an inner column (filter1-6.1/6.2) steps
// the inner rows.
func (e *SelectEngine) chooseAggRowMaps(expr sql.Expr, innerColNames map[string]bool, stepping, outerRows []RowMap, haveFrom bool) []RowMap {
	if !haveFrom || len(outerRows) == 0 {
		return outerRows
	}
	if fn, ok := expr.(*sql.FuncCall); ok && e.isAggregateFuncCallName(fn.Name) && e.aggregateHasOnlyOuterRefs(fn, innerColNames) {
		return outerRows
	}
	if stepping != nil {
		return stepping
	}
	return outerRows
}

// aggSteppingRows builds the combined outer-fallback + inner stepping row maps
// for aggregate evaluation: each inner row overlaid on the first outer row
// (inner columns shadow the outer fallback). nil when the shape does not apply.
func aggSteppingRows(allRowMaps, outerRows []RowMap, haveFrom bool) []RowMap {
	if len(allRowMaps) == 0 || len(outerRows) == 0 || !haveFrom {
		return nil
	}
	fallback := outerRows[0]
	stepping := make([]RowMap, len(allRowMaps))
	for i, inner := range allRowMaps {
		m := make(RowMap, len(inner)+len(fallback))
		for k, v := range fallback {
			m[k] = v
		}
		for k, v := range inner {
			m[k] = v // inner columns shadow the outer fallback
		}
		stepping[i] = m
	}
	return stepping
}

// collectRowMapKeys collects the union of the row maps' keys.
func collectRowMapKeys(rows []RowMap) map[string]bool {
	keys := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			keys[k] = true
		}
	}
	return keys
}

// runWindowOverCollapsedRow runs the window pass over the single collapsed
// aggregate row: the row gains the aggregate outputs under the output column
// names, nested aggregates are precomputed over srcRows, and the window's
// arguments resolve against those rows.
func (e *SelectEngine) runWindowOverCollapsedRow(s *sql.SelectStmt, collapsed RowMap, outRow []interface{}, columns []string, srcRows []RowMap, colDefs []sql.ColumnDef) []interface{} {
	m := make(RowMap, len(collapsed))
	for k, v := range collapsed {
		m[k] = v
	}
	for j, col := range columns {
		if j < len(outRow) {
			m[col] = outRow[j]
		}
	}
	// Nested aggregates inside window columns (e.g. sum(a) in
	// min(sum(a)) OVER ()) are computed as regular aggregates over the
	// full input and stored for the window pass to resolve.
	e.storeWindowNestedAggs(m, s.Columns, srcRows)
	e.windowGroupOutputs = columns
	e.windowGroupCols = s.Columns
	defer func() { e.windowGroupOutputs = nil; e.windowGroupCols = nil }()
	if winResult := e.execWindowPass(s, []RowMap{m}, colDefs); winResult != nil && len(winResult.Rows) > 0 {
		return winResult.Rows[0]
	}
	return outRow
}

// evalAggregates evaluates aggregate functions across all row maps (no GROUP
// BY).
func (e *SelectEngine) evalAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if len(rowMaps) == 0 {
		return e.evalAggregatesEmpty(s, colDefs)
	}

	// Nested aggregate functions inside wrapper expressions (e.g.
	// round(avg(x),2)) resolve through aggRowMaps instead of per-row.
	e.aggRowMaps = rowMaps
	defer func() { e.aggRowMaps = nil }()

	orderBy, isMax := e.findAggOrderBy(s.Columns)
	if orderBy != nil {
		rowMaps = e.sortRowMapsForAggOrderBy(orderBy, isMax, rowMaps)
	}

	// Bare columns take their values from the row that produced the last
	// min/max aggregate (SQLite semantics), not an arbitrary first row.
	firstSource := rowMaps[0]
	rowMaps = e.reorderRowsForMinMax(s, rowMaps)

	columns := e.buildColumnNames(s.Columns, colDefs, s)
	outRow, err := e.evalAggOutputRow(s, rowMaps, colDefs)
	if err != nil {
		return &Result{Error: err}
	}
	// A window function in a no-GROUP-BY aggregate query runs over the single
	// collapsed row; its arguments resolve against the ORIGINAL first source
	// row (e.g. SELECT sum(a), max(b) OVER () FROM t: max(b) OVER () is the
	// first source row's b, before any min/max reordering).
	if e.selectHasWindowFuncs(s.Columns) {
		outRow = e.runWindowOverCollapsedRow(s, firstSource, outRow, columns, rowMaps, colDefs)
	}
	// A HAVING clause without GROUP BY still filters the single aggregate
	// row (SQLite resolves it as a one-group aggregate query — select3-3.1:
	// "SELECT log, count(*) FROM t1 HAVING log>=4" emits no row when the
	// predicate fails on the group's representative row).
	if s.Having != nil {
		match, herr := e.evalHaving(s.Having, rowMaps)
		if herr != nil {
			return &Result{Error: herr}
		}
		if !match {
			return &Result{Columns: columns, Rows: nil}
		}
	}
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: [][]interface{}{outRow}}, s, nil)
}

// evalAggregatesEmpty handles an aggregate query over zero rows: the query
// still emits one row, with aggregates at their empty-input values and bare
// expressions evaluated against an all-NULL synthetic row. Star columns expand
// to NULL values for each underlying column.
func (e *SelectEngine) evalAggregatesEmpty(s *sql.SelectStmt, colDefs []sql.ColumnDef) *Result {
	columns := e.buildColumnNames(s.Columns, colDefs, s)
	var outRow []interface{}
	emptyRow := RowMap{}
	// Nested aggregate functions inside wrapper expressions (e.g.
	// round(avg(x),2)) resolve through aggRowMaps instead of per-row. Over an
	// empty input set the aggregate must see zero rows (e.g. avg(a) -> NULL),
	// so set a non-nil empty slice; leaving it nil would make the scalar path
	// evaluate the aggregate argument against emptyRow, which can leak a
	// stale outer-row value (returning1 20.2: DELETE ... RETURNING with a
	// subquery aggregate over the emptied table returned the previous row's
	// aggregate instead of NULL).
	e.aggRowMaps = []RowMap{}
	defer func() { e.aggRowMaps = nil }()
	for _, col := range s.Columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			e.appendEmptyStarCols(&outRow, ref, colDefs)
			continue
		}
		if appended := e.appendEmptyAggValue(col.Expr, &outRow); appended {
			continue
		}
		v, err := e.ctx.EvalExpr(col.Expr, emptyRow)
		if err != nil {
			outRow = append(outRow, nil)
		} else {
			outRow = append(outRow, util.UnwrapColumnValue(v))
		}
	}
	if outRow != nil {
		// A HAVING clause without GROUP BY still applies to the single
		// (empty) group an aggregate query over zero rows forms — count-2.9a:
		// "SELECT count(*) FROM t2 HAVING count(*)>1" emits no row because
		// the empty-input aggregate (count(*)=0) fails the predicate.
		if res := e.applyEmptyGroupHaving(s, columns); res != nil {
			return res
		}
		// Route through finalizeSelectResult so a compound (UNION/INTERSECT/
		// EXCEPT) head with an empty row set still merges its members (e.g.
		// "SELECT count(*) FROM t1 WHERE 0 UNION ALL SELECT count(*) FROM
		// t2" must return both counts, not just the head's).
		return e.finalizeSelectResult(&Result{Columns: columns, Rows: [][]interface{}{outRow}}, s, nil)
	}
	return nil
}

// evalAggregatesGroupBy partitions rows by GROUP BY key, evaluates aggregates
// per group, applies HAVING, and emits groups in key order.
func (e *SelectEngine) evalAggregatesGroupBy(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if len(rowMaps) == 0 {
		return nil
	}

	groupBy, gbErr := resolveGroupByOrdinals(s, colDefs)
	if gbErr != nil {
		return &Result{Error: gbErr}
	}
	groups, keyVals, keyOrder := e.partitionByGroupKey(groupBy, rowMaps)
	e.sortGroupKeys(keyOrder, keyVals)

	columns := e.buildColumnNames(s.Columns, colDefs, s)
	var outRows [][]interface{}
	var outMaps []RowMap
	var groupRowsList [][]RowMap

	for _, key := range keyOrder {
		groupRows := groups[key]
		groupRows = e.reorderRowsForMinMax(s, groupRows)
		outRow, first, keep, err := e.evalGroupRow(s, colDefs, groupBy, keyVals[key], groupRows)
		if err != nil {
			return &Result{Error: err}
		}
		if !keep {
			continue
		}
		outRows = append(outRows, outRow)
		groupRowsList = append(groupRowsList, groupRows)
		if first != nil {
			outMaps = append(outMaps, first)
		}
	}

	if len(outRows) == 0 {
		return e.finalizeSelectResult(&Result{Columns: columns, Rows: [][]interface{}{}}, s, nil)
	}
	// Window functions in a GROUP BY query operate over the GROUP OUTPUT rows;
	// see groupWindowPass.
	if e.selectHasWindowFuncs(s.Columns) {
		outRows = e.groupWindowPass(s, outRows, outMaps, columns, groupRowsList, colDefs)
	}
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: outRows}, s, outMaps)
}

// evalGroupRow evaluates one group's aggregate output row, applying HAVING.
// keep=false when the group is filtered out or its HAVING errors (SQLite
// treats a HAVING evaluation failure as a skipped group here). first is the
// group's representative row for outMaps.
func (e *SelectEngine) evalGroupRow(s *sql.SelectStmt, colDefs []sql.ColumnDef, groupBy []sql.Expr, groupVals []interface{}, groupRows []RowMap) (outRow []interface{}, first RowMap, keep bool, err error) {
	e.aggRowMaps = groupRows
	// Set outerRows to the group's rows so a correlated scalar subquery
	// column (SELECT max(y) FROM-less) aggregates over the WHOLE group,
	// not just the first row (window1 76.5: (SELECT max(y)+sum(0) OVER ())
	// with GROUP BY x → per-group max over the group's joined rows).
	prevOuterRows := e.outerRows
	e.outerRows = groupRows
	outRow, err = e.buildGroupByAggRow(s, colDefs, groupBy, groupVals, groupRows)
	e.outerRows = prevOuterRows
	e.aggRowMaps = nil
	if err != nil {
		return nil, nil, false, err
	}
	if s.Having != nil {
		match, herr := e.evalHaving(s.Having, groupRows)
		if herr != nil || !match {
			return nil, nil, false, nil
		}
	}
	if len(groupRows) > 0 {
		first = groupRows[0]
	}
	return outRow, first, true, nil
}

// groupWindowPass rewrites outRows for a GROUP BY query's window functions.
// Window functions operate over the GROUP OUTPUT rows (e.g. SELECT count(*),
// max(a) OVER () FROM t GROUP BY c: the window max(a) OVER () is 2 for every
// group). The window pass needs rows that carry BOTH the output column values
// (for window-column substitution) and the source columns (for window
// arguments like max(a)): each combined row merges the group's source map
// with its output row values.
func (e *SelectEngine) groupWindowPass(s *sql.SelectStmt, outRows [][]interface{}, outMaps []RowMap, columns []string, groupRowsList [][]RowMap, colDefs []sql.ColumnDef) [][]interface{} {
	if winResult := e.runGroupWindowPass(s, outRows, outMaps, columns, groupRowsList, colDefs); winResult != nil {
		return winResult.Rows
	}
	return outRows
}

// buildGroupWindowRow merges a group's source map with its output row values
// and precomputes the nested aggregates over the group's rows.
func (e *SelectEngine) buildGroupWindowRow(s *sql.SelectStmt, src RowMap, outRow []interface{}, columns []string, groupRows []RowMap) RowMap {
	m := make(RowMap)
	for k, v := range src {
		m[k] = v
	}
	for j, col := range columns {
		if j < len(outRow) {
			m[col] = outRow[j]
		}
	}
	// Nested aggregates inside window columns (e.g. sum(a) in
	// min(sum(a)) OVER ()) are computed per group and stored for the
	// window pass to resolve.
	e.storeWindowNestedAggs(m, s.Columns, groupRows)
	return m
}

// evalAggFuncCall evaluates a single aggregate function call across rowMaps,
// applying its FILTER clause and ORDER BY ordering. Returns (nil, nil) for a
// non-aggregate function over no rows.
func (e *SelectEngine) evalAggFuncCall(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error) {
	fn, ok := e.ctx.Functions().Find(v.Name)
	if !ok || fn.Type != function.TypeAggregate {
		if len(rowMaps) > 0 {
			val, _ := e.ctx.EvalExpr(v, rowMaps[0])
			return val, nil
		}
		return nil, nil
	}
	if nested := e.findAggNestedAggregates(v); nested != "" {
		return nil, fmt.Errorf("misuse of aggregate function %s()", nested)
	}
	// Single-argument MIN/MAX compares its argument values under the
	// argument's collation (func.c minmaxStep: pColl =
	// sqlite3GetFuncCollSeq — the collation of the first argument):
	// x COLLATE nocase and x declared COLLATE nocase both order the
	// reduction by nocase (minmax3-4.x). Reduced here (not in the
	// function's Step) because the collation is statement context the
	// registry's collation-free Step signature cannot carry.
	if (strings.EqualFold(v.Name, "MIN") || strings.EqualFold(v.Name, "MAX")) && len(v.Args) == 1 {
		return e.evalMinMaxAggregate(v, rowMaps)
	}
	agg := fn.AggregateFn()
	rows := e.sortRowMapsByOrderBy(v.OrderBy, rowMaps)
	for _, row := range rows {
		if !e.aggRowPassesFilter(v, row) {
			continue
		}
		if err := agg.Step(e.evalAggCallArgs(v, row)); err != nil {
			e.aggPendingErr = err
			return nil, err
		}
	}
	// sumFinalize raises "integer overflow" from Final when an int64
	// overflow was never absorbed by a later non-integer input (func-37.x):
	// a Final error must propagate like a Step error, not collapse to NULL.
	result, ferr := agg.Final()
	if ferr != nil {
		e.aggPendingErr = ferr
		return nil, ferr
	}
	return result, nil
}

// evalMinMaxAggregate reduces a single-argument MIN/MAX under the argument's
// collation: the first evaluated argument value carrying a CollatedValue
// marker donates the collation (explicit COLLATE operator or the column's
// declared COLLATE clause — SQLite's sqlite3ExprCollSeq of the argument).
// NULLs are skipped; the first extreme on ties wins (minmaxStep keeps the
// earliest row's value).
func (e *SelectEngine) evalMinMaxAggregate(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error) {
	isMax := strings.EqualFold(v.Name, "MAX")
	var best interface{}
	collation := ""
	for _, row := range rowMaps {
		if !e.aggRowPassesFilter(v, row) {
			continue
		}
		restore := e.ctx.EnterAuxAggArg()
		raw, err := e.ctx.EvalExpr(v.Args[0], row)
		restore()
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		val := util.UnwrapColumnValue(raw)
		if cv, ok := raw.(*execexpr.CollatedValue); ok {
			val = util.UnwrapColumnValue(cv.Value)
			if collation == "" && cv.Collation != "" {
				collation = cv.Collation
			}
		}
		if val == nil {
			continue
		}
		if best == nil {
			best = val
			continue
		}
		cmp := e.ctx.CompareValuesCollate(val, best, collation)
		if (isMax && cmp > 0) || (!isMax && cmp < 0) {
			best = val
		}
	}
	return best, nil
}

// evalDistinctAggregate evaluates an aggregate with DISTINCT over the distinct
// argument tuples (after applying the FILTER clause and ORDER BY ordering).
func (e *SelectEngine) evalDistinctAggregate(v *sql.FuncCall, rowMaps []RowMap) interface{} {
	fn, ok := e.ctx.Functions().Find(v.Name)
	if !ok || fn.Type != function.TypeAggregate {
		return nil
	}
	agg := fn.AggregateFn()
	uniqueRows := e.dedupeAggRows(v, rowMaps)
	uniqueRows = e.sortRowMapsByOrderBy(v.OrderBy, uniqueRows)
	for _, row := range uniqueRows {
		if err := agg.Step(e.evalAggCallArgs(v, row)); err != nil {
			e.aggPendingErr = err
			return nil
		}
	}
	result, ferr := agg.Final()
	if ferr != nil {
		e.aggPendingErr = ferr
		return nil
	}
	return result
}

// evalGroupByNoAggs handles GROUP BY without aggregate functions: groups rows
// by key and builds output rows using buildOutputRow, emitting groups in key
// order and applying HAVING.
func (e *SelectEngine) evalGroupByNoAggs(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if len(rowMaps) == 0 {
		return nil
	}

	groupBy, gbErr := resolveGroupByOrdinals(s, colDefs)
	if gbErr != nil {
		return &Result{Error: gbErr}
	}
	groups, keyVals, keyOrder := e.partitionByGroupKey(groupBy, rowMaps)
	e.sortGroupKeys(keyOrder, keyVals)

	outRows, outMaps, groupRowsList, gerr := e.collectNoAggGroups(s, colDefs, groupBy, groups, keyVals, keyOrder)
	if gerr != nil {
		return &Result{Error: gerr}
	}

	columns := e.buildColumnNames(s.Columns, colDefs, s)
	// A GROUP BY query whose output contains a window function (e.g. SELECT
	// max(b) OVER (ORDER BY b) FROM t GROUP BY b) runs the window pass over
	// the group output rows, matching evalAggregatesGroupBy.
	if e.selectHasWindowFuncs(s.Columns) {
		if winResult := e.runGroupWindowPass(s, outRows, outMaps, columns, groupRowsList, colDefs); winResult != nil {
			return e.finalizeSelectResult(winResult, s, winResult.rowMaps)
		}
	}
	outMaps = buildResultRowMaps(outRows, columns)
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: outRows}, s, outMaps)
}

// collectNoAggGroups builds the non-aggregate GROUP BY output rows: HAVING is
// applied per group, and groups emit in key order.
func (e *SelectEngine) collectNoAggGroups(s *sql.SelectStmt, colDefs []sql.ColumnDef, groupBy []sql.Expr, groups map[string][]RowMap, keyVals map[string][]interface{}, keyOrder []string) (outRows [][]interface{}, outMaps []RowMap, groupRowsList [][]RowMap, err error) {
	for _, key := range keyOrder {
		groupRows := groups[key]
		if s.Having != nil {
			match, herr := e.evalHaving(s.Having, groupRows)
			if herr != nil || !match {
				continue
			}
		}
		outRow, first, keep, gerr := e.evalNoAggGroupRow(s, colDefs, groupBy, keyVals[key], groupRows)
		if gerr != nil {
			return nil, nil, nil, gerr
		}
		if !keep {
			continue
		}
		outRows = append(outRows, outRow)
		groupRowsList = append(groupRowsList, groupRows)
		if first != nil {
			outMaps = append(outMaps, first)
		}
	}
	return outRows, outMaps, groupRowsList, nil
}

// runGroupWindowPass builds the combined group rows (source map + output
// values + precomputed nested aggregates) and runs the window pass over them.
// Returns nil when the pass produced no result (the caller keeps outRows).
func (e *SelectEngine) runGroupWindowPass(s *sql.SelectStmt, outRows [][]interface{}, outMaps []RowMap, columns []string, groupRowsList [][]RowMap, colDefs []sql.ColumnDef) *Result {
	combined := make([]RowMap, len(outRows))
	for i := range outRows {
		var src RowMap
		if i < len(outMaps) {
			src = outMaps[i]
		}
		var groupRows []RowMap
		if i < len(groupRowsList) {
			groupRows = groupRowsList[i]
		}
		combined[i] = e.buildGroupWindowRow(s, src, outRows[i], columns, groupRows)
	}
	e.windowGroupOutputs = columns
	e.windowGroupCols = s.Columns
	defer func() { e.windowGroupOutputs = nil; e.windowGroupCols = nil }()
	return e.execWindowPass(s, combined, colDefs)
}

// evalNoAggGroupRow builds one GROUP BY group's output row without aggregates.
// A HAVING min/max aggregate determines the source row for bare output columns
// (SQLite: "SELECT x FROM t GROUP BY g HAVING max(y)" evaluates x on the row
// that produced the max), hence the min/max reorder before row selection.
func (e *SelectEngine) evalNoAggGroupRow(s *sql.SelectStmt, colDefs []sql.ColumnDef, groupBy []sql.Expr, groupVals []interface{}, groupRows []RowMap) (outRow []interface{}, first RowMap, keep bool, err error) {
	// (see comment above)
	groupRows = e.reorderRowsForMinMax(s, groupRows)
	outRow, err = e.buildNoAggGroupRow(s, colDefs, groupBy, groupRows[0], groupVals)
	if err != nil {
		return nil, nil, false, err
	}
	if len(groupRows) > 0 {
		first = groupRows[0]
	}
	return outRow, first, true, nil
}

// EvalAggFuncCall evaluates an aggregate function call over the given row
// maps. Exported for the expression evaluator's function-call dispatch.
func (e *SelectEngine) EvalAggFuncCall(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error) {
	return e.evalAggFuncCall(v, rowMaps)
}
