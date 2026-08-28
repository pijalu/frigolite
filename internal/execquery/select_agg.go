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

// partitionByGroupKey partitions rowMaps by their GROUP BY key, preserving
// first-seen order in keyOrder. It returns the per-key row slices, the per-key
// evaluated key values, and the ordered list of keys.
func (e *SelectEngine) partitionByGroupKey(groupBy []sql.Expr, rowMaps []RowMap) (map[string][]RowMap, map[string][]interface{}, []string) {
	groups := make(map[string][]RowMap)
	keyVals := make(map[string][]interface{})
	var keyOrder []string
	for _, row := range rowMaps {
		key, vals := e.computeGroupByKeyValues(groupBy, row)
		if _, exists := groups[key]; !exists {
			keyOrder = append(keyOrder, key)
			keyVals[key] = vals
		}
		groups[key] = append(groups[key], row)
	}
	return groups, keyVals, keyOrder
}

// evalAggCallArgs evaluates the arguments of an aggregate function call for a
// single row, unwrapping column values and substituting nil on error.
func (e *SelectEngine) evalAggCallArgs(fn *sql.FuncCall, row RowMap) []interface{} {
	args := make([]interface{}, len(fn.Args))
	for i, arg := range fn.Args {
		v, err := e.ctx.EvalExpr(arg, row)
		if err != nil {
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

// distinctKey builds a deduplication key for already-evaluated aggregate args.
func distinctKey(args []interface{}) string {
	var key string
	for _, a := range args {
		if a == nil {
			key += "\x00"
		} else {
			key += fmt.Sprintf("%v", a) + "\x00"
		}
	}
	return key
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
		leftNull := execexpr.IsSQLNull(vi)
		rightNull := execexpr.IsSQLNull(vj)
		if leftNull != rightNull {
			nullsFirst := ob.NullsFirst
			if ob.NullsLast {
				nullsFirst = false
			}
			if !ob.NullsFirst && !ob.NullsLast {
				nullsFirst = !ob.Desc
			}
			if leftNull {
				if nullsFirst {
					return -1
				}
				return 1
			}
			if nullsFirst {
				return 1
			}
			return -1
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

// aggregateHasOnlyOuterRefs checks whether an aggregate function's arguments
// and ORDER BY terms reference only outer columns (none from the inner table).
// Returns true only when the aggregate has at least one column reference and
// none of them match the inner column set.
func (e *SelectEngine) aggregateHasOnlyOuterRefs(fn *sql.FuncCall, innerColNames map[string]bool) bool {
	if aggExprListHasSubquery(fn.Args) || aggExprListHasSubquery(orderByExprs(fn.OrderBy)) {
		return false
	}
	aInner, aHas := e.scanAggExprRefs(fn.Args, innerColNames)
	oInner, oHas := e.scanAggExprRefs(orderByExprs(fn.OrderBy), innerColNames)
	return !aInner && !oInner && (aHas || oHas)
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
	e.aggRowMaps = outerRows
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
		m := RowMap{}
		for k, v := range innerRow {
			m[k] = v
		}
		cols := e.buildColumnNames(s.Columns, nil, s)
		for j, col := range cols {
			if j < len(outRow) {
				m[col] = outRow[j]
			}
		}
		e.storeWindowNestedAggs(m, s.Columns, outerRows)
		e.windowGroupOutputs = cols
		e.windowGroupCols = s.Columns
		defer func() { e.windowGroupOutputs = nil; e.windowGroupCols = nil }()
		if winResult := e.execWindowPass(s, []RowMap{m}, nil); winResult != nil && len(winResult.Rows) > 0 {
			outRow = winResult.Rows[0]
		}
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
		m := make(RowMap)
		for k, v := range firstSource {
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
		e.storeWindowNestedAggs(m, s.Columns, rowMaps)
		e.windowGroupOutputs = columns
		e.windowGroupCols = s.Columns
		defer func() { e.windowGroupOutputs = nil; e.windowGroupCols = nil }()
		winResult := e.execWindowPass(s, []RowMap{m}, colDefs)
		if winResult != nil && len(winResult.Rows) > 0 {
			outRow = winResult.Rows[0]
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
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if f, found := e.ctx.Functions().Find(fn.Name); found && f.Type == function.TypeAggregate {
				outRow = append(outRow, e.emptyAggValue(f))
				continue
			}
		}
		v, err := e.ctx.EvalExpr(col.Expr, emptyRow)
		if err != nil {
			outRow = append(outRow, nil)
		} else {
			outRow = append(outRow, util.UnwrapColumnValue(v))
		}
	}
	if outRow != nil {
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

	groupBy := resolveGroupByOrdinals(s, colDefs)
	groups, keyVals, keyOrder := e.partitionByGroupKey(groupBy, rowMaps)
	e.sortGroupKeys(keyOrder, keyVals)

	columns := e.buildColumnNames(s.Columns, colDefs, s)
	var outRows [][]interface{}
	var outMaps []RowMap
	var groupRowsList [][]RowMap

	for _, key := range keyOrder {
		groupRows := groups[key]
		groupVals := keyVals[key]
		groupRows = e.reorderRowsForMinMax(s, groupRows)

		e.aggRowMaps = groupRows
		// Set outerRows to the group's rows so a correlated scalar subquery
		// column (SELECT max(y) FROM-less) aggregates over the WHOLE group,
		// not just the first row (window1 76.5: (SELECT max(y)+sum(0) OVER ())
		// with GROUP BY x → per-group max over the group's joined rows).
		prevOuterRows := e.outerRows
		e.outerRows = groupRows
		outRow, err := e.buildGroupByAggRow(s, colDefs, groupBy, groupVals, groupRows)
		e.outerRows = prevOuterRows
		e.aggRowMaps = nil
		if err != nil {
			return &Result{Error: err}
		}
		if s.Having != nil {
			match, err := e.evalHaving(s.Having, groupRows)
			if err != nil || !match {
				continue
			}
		}
		outRows = append(outRows, outRow)
		groupRowsList = append(groupRowsList, groupRows)
		if len(groupRows) > 0 {
			outMaps = append(outMaps, groupRows[0])
		}
	}

	if len(outRows) == 0 {
		return e.finalizeSelectResult(&Result{Columns: columns, Rows: [][]interface{}{}}, s, nil)
	}
	// Window functions in a GROUP BY query operate over the GROUP OUTPUT rows
	// (e.g. SELECT count(*), max(a) OVER () FROM t GROUP BY c: the window
	// max(a) OVER () is 2 for every group). The window pass needs rows that
	// carry BOTH the output column values (for window-column substitution)
	// and the source columns (for window arguments like max(a)). Combine the
	// source group map with the output row values.
	if e.selectHasWindowFuncs(s.Columns) {
		combined := make([]RowMap, len(outRows))
		for i := range outRows {
			m := make(RowMap)
			if i < len(outMaps) {
				for k, v := range outMaps[i] {
					m[k] = v
				}
			}
			for j, col := range columns {
				if j < len(outRows[i]) {
					m[col] = outRows[i][j]
				}
			}
			// Nested aggregates inside window columns (e.g. sum(a) in
			// min(sum(a)) OVER ()) are computed per group and stored for the
			// window pass to resolve.
			if i < len(groupRowsList) {
				e.storeWindowNestedAggs(m, s.Columns, groupRowsList[i])
			}
			combined[i] = m
		}
		e.windowGroupOutputs = columns
		e.windowGroupCols = s.Columns
		defer func() { e.windowGroupOutputs = nil; e.windowGroupCols = nil }()
		winResult := e.execWindowPass(s, combined, colDefs)
		if winResult != nil {
			outRows = winResult.Rows
		}
	}
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: outRows}, s, outMaps)
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
	result, _ := agg.Final()
	return result, nil
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
		agg.Step(e.evalAggCallArgs(v, row))
	}
	result, _ := agg.Final()
	return result
}

// evalGroupByNoAggs handles GROUP BY without aggregate functions: groups rows
// by key and builds output rows using buildOutputRow, emitting groups in key
// order and applying HAVING.
func (e *SelectEngine) evalGroupByNoAggs(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if len(rowMaps) == 0 {
		return nil
	}

	groupBy := resolveGroupByOrdinals(s, colDefs)
	groups, keyVals, keyOrder := e.partitionByGroupKey(groupBy, rowMaps)
	e.sortGroupKeys(keyOrder, keyVals)

	var outRows [][]interface{}
	var outMaps []RowMap
	var groupRowsList [][]RowMap
	for _, key := range keyOrder {
		groupRows := groups[key]
		groupVals := keyVals[key]
		if s.Having != nil {
			match, err := e.evalHaving(s.Having, groupRows)
			if err != nil || !match {
				continue
			}
		}
		// A HAVING min/max aggregate determines the source row for bare
		// output columns (SQLite: "SELECT x FROM t GROUP BY g HAVING max(y)"
		// evaluates x on the row that produced the max).
		groupRows = e.reorderRowsForMinMax(s, groupRows)
		outRow, err := e.buildNoAggGroupRow(s, colDefs, groupBy, groupRows[0], groupVals)
		if err != nil {
			return &Result{Error: err}
		}
		outRows = append(outRows, outRow)
		groupRowsList = append(groupRowsList, groupRows)
		if len(groupRows) > 0 {
			outMaps = append(outMaps, groupRows[0])
		}
	}

	columns := e.buildColumnNames(s.Columns, colDefs, s)
	// A GROUP BY query whose output contains a window function (e.g. SELECT
	// max(b) OVER (ORDER BY b) FROM t GROUP BY b) runs the window pass over
	// the group output rows, matching evalAggregatesGroupBy.
	if e.selectHasWindowFuncs(s.Columns) {
		combined := make([]RowMap, len(outRows))
		for i := range outRows {
			m := make(RowMap)
			if i < len(outMaps) {
				for k, v := range outMaps[i] {
					m[k] = v
				}
			}
			for j, col := range columns {
				if j < len(outRows[i]) {
					m[col] = outRows[i][j]
				}
			}
			if i < len(groupRowsList) {
				e.storeWindowNestedAggs(m, s.Columns, groupRowsList[i])
			}
			combined[i] = m
		}
		e.windowGroupOutputs = columns
		e.windowGroupCols = s.Columns
		defer func() { e.windowGroupOutputs = nil; e.windowGroupCols = nil }()
		winResult := e.execWindowPass(s, combined, colDefs)
		if winResult != nil {
			return e.finalizeSelectResult(winResult, s, winResult.rowMaps)
		}
	}
	outMaps = buildResultRowMaps(outRows, columns)
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: outRows}, s, outMaps)
}

// EvalAggFuncCall evaluates an aggregate function call over the given row
// maps. Exported for the expression evaluator's function-call dispatch.
func (e *SelectEngine) EvalAggFuncCall(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error) {
	return e.evalAggFuncCall(v, rowMaps)
}
