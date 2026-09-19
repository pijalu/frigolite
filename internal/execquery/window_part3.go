// Package execquery implements query execution.
package execquery

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
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
func (e *SelectEngine) storeWindowNestedAggs(dst RowMap, columns []sql.SelectColumn, rows []RowMap) {
	for _, col := range columns {
		e.storeNestedAggsInExpr(dst, col.Expr, rows)
	}
}

// storeNestedAggsInExpr walks one expression, evaluating and storing every
// nested plain aggregate (a FuncCall with no OVER) under its expression-string
// key. Window functions' args and OVER clauses are checked for nested
// aggregates; aggregates inside a subquery are scoped to that subquery and
// skipped.
func (e *SelectEngine) storeNestedAggsInExpr(dst RowMap, expr sql.Expr, rows []RowMap) {
	if fc, ok := expr.(*sql.FuncCall); ok {
		e.storeNestedAggsInFuncCall(dst, fc, rows)
		return
	}
	for _, kid := range nestedAggChildren(expr) {
		e.storeNestedAggsInExpr(dst, kid, rows)
	}
}

// storeNestedAggsInFuncCall handles the FuncCall node of the nested-aggregate
// walk: a window call descends into args/partitions/ORDER BY; a plain
// aggregate is evaluated and stored; a scalar call descends into args and
// aggregate ORDER BY terms.
func (e *SelectEngine) storeNestedAggsInFuncCall(dst RowMap, fc *sql.FuncCall, rows []RowMap) {
	if fc.Over != nil {
		// A window function: check its args and OVER clause for nested
		// plain aggregates, but do not descend into nested windows.
		for _, a := range fc.Args {
			e.storeNestedAggsInExpr(dst, a, rows)
		}
		for _, p := range fc.Over.Partitions {
			e.storeNestedAggsInExpr(dst, p, rows)
		}
		for _, ob := range fc.Over.OrderBy {
			e.storeNestedAggsInExpr(dst, ob.Expr, rows)
		}
		return
	}
	if reg, found := e.ctx.Functions().Find(fc.Name); found && reg.Type == function.TypeAggregate {
		name := sql.ExprString(fc)
		if _, exists := dst.Get(name); !exists {
			if val, err := e.evalAggFuncCall(fc, rows); err == nil {
				dst[name] = val
			}
		}
		return
	}
	e.storeNestedAggsInCallArgs(dst, fc, rows)
}

// storeNestedAggsInCallArgs descends a scalar function call's args and
// aggregate ORDER BY terms in the nested-aggregate walk.
func (e *SelectEngine) storeNestedAggsInCallArgs(dst RowMap, fc *sql.FuncCall, rows []RowMap) {
	for _, a := range fc.Args {
		e.storeNestedAggsInExpr(dst, a, rows)
	}
	for _, ob := range fc.OrderBy {
		e.storeNestedAggsInExpr(dst, ob.Expr, rows)
	}
}

// nestedAggChildren returns an expression node's child expressions for the
// nested-aggregate walk (the unary family via aggregateSingleOperand).
func nestedAggChildren(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		left, right := BinaryExprOperands(expr)
		return []sql.Expr{left, right}
	case *sql.UnaryOp, *sql.IsNull, *sql.IsNotNull, *sql.CastExpr:
		return []sql.Expr{aggregateSingleOperand(expr)}
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

// dedupeAggRowsWindow returns the distinct (by argument values) rows in
// [start, end) after the FILTER clause, preserving order.
func (e *SelectEngine) dedupeAggRowsWindow(fn *sql.FuncCall, part []winRow, start, end int) []RowMap {
	seen := make(map[string]bool)
	var out []RowMap
	for j := start; j < end; j++ {
		if e.windowExcludesRow(nil, 0, j, part) {
			continue
		}
		if !e.aggRowPassesFilter(fn, part[j].row) {
			continue
		}
		key := distinctKey(e.evalAggCallArgs(fn, part[j].row))
		if !seen[key] {
			seen[key] = true
			out = append(out, part[j].row)
		}
	}
	return out
}

// windowExcludesRow reports whether frame index j must be excluded from the
// frame of row i per the window's EXCLUDE clause. A nil over (or no EXCLUDE)
// keeps every row.
func (e *SelectEngine) windowExcludesRow(over *sql.WindowDef, i, j int, part []winRow) bool {
	if over == nil || over.Frame == nil || over.Frame.Exclude == "" {
		return false
	}
	excl := over.Frame.Exclude
	switch excl {
	case "NO OTHERS":
		return false
	case "CURRENT ROW":
		return i == j
	case "GROUP":
		return e.winRowsArePeers(over.OrderBy, part[i], part[j])
	case "TIES":
		// Exclude peer rows (which share the ORDER BY values with row i),
		// but keep the current row itself.
		return i != j && e.winRowsArePeers(over.OrderBy, part[i], part[j])
	}
	return false
}

// computeRankingWindow fills window results for the ranking functions.
func (e *SelectEngine) computeRankingWindow(fn *sql.FuncCall, name string, over *sql.WindowDef, part []winRow, results []interface{}) error {
	n := len(part)
	if name == "NTILE" {
		return e.computeNtileResults(fn, part, results)
	}

	// RANK/DENSE_RANK/PERCENT_RANK/CUME_DIST need peer groups; ROW_NUMBER is
	// just the position.
	if name == "ROW_NUMBER" {
		for i, wr := range part {
			results[wr.origIdx] = int64(i + 1)
		}
		return nil
	}

	groupStart, groupEnd := e.computePeerGroups(over.OrderBy, part)
	_ = groupEnd
	for i, wr := range part {
		switch name {
		case "RANK":
			// rank = 1 + number of rows before this peer group.
			results[wr.origIdx] = int64(groupStart[i] + 1)
		case "DENSE_RANK":
			// dense_rank = number of distinct peer groups before this one + 1.
			results[wr.origIdx] = int64(e.denseRankOf(groupStart, i) + 1)
		case "PERCENT_RANK":
			if n <= 1 {
				results[wr.origIdx] = float64(0)
			} else {
				rank := float64(groupStart[i] + 1)
				results[wr.origIdx] = (rank - 1) / float64(n-1)
			}
		case "CUME_DIST":
			// cume_dist = (number of rows <= current peer group's last row) / n.
			cnt := groupEnd[i] + 1
			results[wr.origIdx] = float64(cnt) / float64(n)
		}
	}
	return nil
}

// computeNtileResults fills window results for ntile(N): the partition is
// divided into N buckets as evenly as possible and each row gets its bucket
// number.
func (e *SelectEngine) computeNtileResults(fn *sql.FuncCall, part []winRow, results []interface{}) error {
	n := len(part)
	buckets, err := e.ntileBucketCount(fn, part)
	if err != nil {
		return err
	}
	if buckets <= 0 {
		return fmt.Errorf("argument of ntile must be a positive integer")
	}
	// Compute per-row bucket numbers.
	rowsPer := n / buckets
	remainder := n % buckets
	bucket := 1
	counted := 0
	for i := 0; i < n; i++ {
		results[part[i].origIdx] = int64(bucket)
		counted++
		limit := rowsPer
		if bucket <= remainder {
			limit++
		}
		if counted >= limit && bucket < buckets {
			bucket++
			counted = 0
		}
	}
	return nil
}

// denseRankOf returns the number of distinct peer groups strictly before the
// group of row index i. All rows in the same peer group share the same
// dense_rank.
func (e *SelectEngine) denseRankOf(groupStart []int, i int) int {
	if i >= len(groupStart) {
		return 0
	}
	myGroup := groupStart[i]
	distinct := 0
	prev := -1
	for k := 0; k < myGroup; k++ {
		if groupStart[k] != prev {
			distinct++
			prev = groupStart[k]
		}
	}
	return distinct
}

// ntileBucketCount evaluates ntile's argument (a positive integer constant).
func (e *SelectEngine) ntileBucketCount(fn *sql.FuncCall, part []winRow) (int, error) {
	if len(fn.Args) != 1 {
		return 0, fmt.Errorf("wrong number of arguments to function ntile()")
	}
	v, err := e.ctx.EvalExpr(fn.Args[0], part[0].row)
	if err != nil {
		return 0, err
	}
	v = util.UnwrapColumnValue(unwrapCollatedValue(v))
	switch x := v.(type) {
	case int64:
		return int(x), nil
	case float64:
		if x != float64(int64(x)) {
			return 0, fmt.Errorf("argument of ntile must be a positive integer")
		}
		return int(x), nil
	default:
		return 0, fmt.Errorf("argument of ntile must be a positive integer")
	}
}

// ntileConstArg returns ntile's argument value when it is a constant integer
// (evaluated against an empty row), or ok=false otherwise.
func (e *SelectEngine) ntileConstArg(fn *sql.FuncCall) (int64, bool) {
	if len(fn.Args) != 1 {
		return 0, false
	}
	v, err := e.ctx.EvalExpr(fn.Args[0], RowMap{})
	if err != nil {
		return 0, false
	}
	switch x := util.UnwrapColumnValue(unwrapCollatedValue(v)).(type) {
	case int64:
		return x, true
	case float64:
		if x == float64(int64(x)) {
			return int64(x), true
		}
	}
	return 0, false
}

// computeLeadLagWindow fills window results for LEAD/LAG: value functions
// that access a row at a per-row offset (default 1) from the current row
// within the partition (SQLite lead/lag ignore the frame; they use partition
// order). Returns the DEFAULT when the target is out of range.
func (e *SelectEngine) computeLeadLagWindow(fn *sql.FuncCall, name string, over *sql.WindowDef, part []winRow, results []interface{}) error {
	if len(fn.Args) < 1 || len(fn.Args) > 3 {
		return fmt.Errorf("wrong number of arguments to function %s()", strings.ToLower(name))
	}
	var def interface{}
	if len(fn.Args) >= 3 {
		def = e.evalWindowExprValue(fn.Args[2], part[0].row)
	}
	for i, wr := range part {
		offset, err := e.leadLagOffset(fn, wr)
		if err != nil {
			return err
		}
		target := i + int(offset)
		if name == "LAG" {
			target = i - int(offset)
		}
		if target < 0 || target >= len(part) {
			results[wr.origIdx] = def
			continue
		}
		results[wr.origIdx] = e.evalWindowExprValue(fn.Args[0], part[target].row)
	}
	return nil
}

// leadLagOffset evaluates a LEAD/LAG call's per-row offset argument (default
// 1). SQLite allows negative offsets: lead(b, -1) is lag(b, 1). The direction
// arithmetic in the caller handles the sign naturally.
func (e *SelectEngine) leadLagOffset(fn *sql.FuncCall, wr winRow) (int64, error) {
	if len(fn.Args) < 2 {
		return 1, nil
	}
	v, err := e.ctx.EvalExpr(fn.Args[1], wr.row)
	if err != nil {
		return 0, err
	}
	offset := int64(1)
	switch x := util.UnwrapColumnValue(unwrapCollatedValue(v)).(type) {
	case int64:
		offset = x
	case float64:
		offset = int64(x)
	}
	return offset, nil
}

// computeValueWindow fills window results for FIRST_VALUE/LAST_VALUE/NTH_VALUE:
// the first/last/nth row's value within the current row's frame.
func (e *SelectEngine) computeValueWindow(fn *sql.FuncCall, name string, over *sql.WindowDef, part []winRow, results []interface{}) error {
	for i, wr := range part {
		start, end, err := e.frameBounds(over, part, i)
		if err != nil {
			return err
		}

		if start >= end {
			results[wr.origIdx] = nil
			continue
		}
		target, err := e.valueWindowTarget(fn, name, over, part, i, wr, start, end)
		if err != nil {
			return err
		}
		if target < 0 {
			results[wr.origIdx] = nil
			continue
		}
		results[wr.origIdx] = e.evalWindowExprValue(fn.Args[0], part[target].row)
	}
	return nil
}

// valueWindowTarget resolves the frame row a value window function picks for
// the current row: the first/last non-excluded row (FIRST/LAST_VALUE) or the
// nth non-excluded row (NTH_VALUE). -1 means "no such row" (NULL result).
func (e *SelectEngine) valueWindowTarget(fn *sql.FuncCall, name string, over *sql.WindowDef, part []winRow, i int, wr winRow, start, end int) (int, error) {
	switch name {
	case "FIRST_VALUE":
		// Apply the frame's EXCLUDE clause: the picked row must not be an
		// excluded peer/current row (SQLite first/last/nth_value honour
		// EXCLUDE CURRENT ROW / GROUP / TIES).
		return e.pickNonExcluded(over, part, i, start, end, true), nil
	case "LAST_VALUE":
		return e.pickNonExcluded(over, part, i, start, end, false), nil
	case "NTH_VALUE":
		return e.nthValueTarget(fn, over, part, i, wr, start, end)
	}
	return -1, nil
}

// pickNonExcluded returns the frame's first (fromStart) or last non-excluded
// row index, or -1 when every frame row is excluded.
func (e *SelectEngine) pickNonExcluded(over *sql.WindowDef, part []winRow, i, start, end int, fromStart bool) int {
	if fromStart {
		for t := start; t < end; t++ {
			if !e.windowExcludesRow(over, i, t, part) {
				return t
			}
		}
		return -1
	}
	for t := end - 1; t >= start; t-- {
		if !e.windowExcludesRow(over, i, t, part) {
			return t
		}
	}
	return -1
}

// nthValueTarget resolves nth_value(X, N): the Nth row in the frame (1-based).
// With an EXCLUDE clause the Nth NON-EXCLUDED row is picked: the frame's
// excluded rows (current/peers/ties) do not count toward N.
func (e *SelectEngine) nthValueTarget(fn *sql.FuncCall, over *sql.WindowDef, part []winRow, i int, wr winRow, start, end int) (int, error) {
	if len(fn.Args) != 2 {
		return -1, fmt.Errorf("wrong number of arguments to function nth_value()")
	}
	v, err := e.ctx.EvalExpr(fn.Args[1], wr.row)
	if err != nil {
		return -1, err
	}
	n := nthValueRowNumber(v)
	if n < 1 {
		return -1, nil
	}
	target := -1
	count := int64(0)
	for t := start; t < end; t++ {
		if e.windowExcludesRow(over, i, t, part) {
			continue
		}
		count++
		if count == n {
			target = t
			break
		}
	}
	return target, nil
}

// nthValueRowNumber converts nth_value's N argument ('2' and '2.0' are the
// integer 2, per SQLite's numeric conversion of the string form).
func nthValueRowNumber(v interface{}) int64 {
	switch x := util.UnwrapColumnValue(unwrapCollatedValue(v)).(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		fv, convErr := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if convErr != nil {
			return 0
		}
		return int64(fv)
	}
	return 0
}

// evalWindowExprValue evaluates an expression against a row, unwrapping the
// value (nil on error).
func (e *SelectEngine) evalWindowExprValue(expr sql.Expr, row RowMap) interface{} {
	v, err := e.ctx.EvalExpr(expr, row)
	if err != nil {
		return nil
	}
	return util.UnwrapColumnValue(unwrapCollatedValue(v))
}

// windowFuncNames are the built-in window functions SQLite recognizes as
// window-only (usable only with an OVER clause).
var windowOnlyFuncs = map[string]bool{
	"ROW_NUMBER": true, "RANK": true, "DENSE_RANK": true,
	"PERCENT_RANK": true, "CUME_DIST": true, "NTILE": true,
	"LEAD": true, "LAG": true,
	"FIRST_VALUE": true, "LAST_VALUE": true, "NTH_VALUE": true,
}

// validateWindowFunctions enforces SQLite's window-function usage rules:
//   - window functions may not appear in WHERE, GROUP BY, HAVING, or FROM
//   - FILTER is only allowed with aggregate window functions
//   - built-in window functions require an OVER clause (misuse otherwise)
//   - non-window functions may not be used with OVER (e.g. trim() OVER ...)
//   - named windows must exist in the statement's WINDOW clause
//   - window function argument counts are validated
func (e *SelectEngine) validateWindowFunctions(s *sql.SelectStmt) error {
	if err := e.validateWindowMisusePlacement(s); err != nil {
		return err
	}
	// Validate each window function call in the select columns.
	var nodes []*sql.FuncCall
	e.collectWindowFuncsInColumns(s.Columns, &nodes)
	if err := e.validateWindowFuncNodes(nodes, s.Windows); err != nil {
		return err
	}
	// Validate the WINDOW clause definitions themselves: a definition that
	// references another named window with added clauses must satisfy the
	// same override rules (e.g. "win2 AS (win1 ORDER BY b)" when win1 has a
	// frame).
	if err := e.validateWindowDefinitions(s.Windows); err != nil {
		return err
	}
	// Validate subqueries inside window ORDER BY / PARTITION BY terms so
	// their errors surface (window1 67.1: a nested (SELECT 1 FROM v1) in a
	// window's ORDER BY must raise "no such table: v1").
	for i := range s.Windows {
		if err := e.validateWindowDefSubqueries(&s.Windows[i]); err != nil {
			return err
		}
	}
	return e.validateWindowNodeSubqueries(nodes, s.Windows)
}

// validateWindowMisusePlacement enforces where window functions may not
// appear: without OVER in the select list, and anywhere in WHERE, LIMIT,
// OFFSET, GROUP BY, or HAVING.
func (e *SelectEngine) validateWindowMisusePlacement(s *sql.SelectStmt) error {
	// A window function used without an OVER clause (e.g. row_number() in a
	// plain SELECT list) is a misuse error.
	for _, col := range s.Columns {
		if err := e.checkWindowFuncWithoutOver(col.Expr); err != nil {
			return err
		}
	}
	// Window functions in WHERE / GROUP BY / HAVING are misuse errors.
	if s.Where != nil {
		if name := e.windowFuncInExpr(s.Where); name != "" {
			return fmt.Errorf("misuse of window function %s()", name)
		}
	}
	// Window functions in LIMIT / OFFSET are misuse errors: the LIMIT
	// expression is evaluated without table-column context, so a window
	// function reference like LIMIT nth_value(x, 1) OVER () fails with
	// "no such column: x" in SQLite.
	if err := e.validateWindowLimitOffset(s); err != nil {
		return err
	}
	resolvedGroupBy, gbErr := resolveGroupByOrdinals(s, nil)
	if gbErr != nil {
		return gbErr
	}
	for _, g := range resolvedGroupBy {
		if name := e.windowFuncInExpr(g); name != "" {
			return fmt.Errorf("misuse of window function %s()", name)
		}
	}
	if s.Having != nil {
		if name := e.windowFuncInExpr(s.Having); name != "" {
			return fmt.Errorf("misuse of window function %s()", name)
		}
	}
	return nil
}

// validateWindowLimitOffset rejects window functions in LIMIT/OFFSET with
// SQLite's "no such column" error (the LIMIT expression is evaluated without
// table-column context).
func (e *SelectEngine) validateWindowLimitOffset(s *sql.SelectStmt) error {
	if s.Limit != nil {
		if name := e.windowFuncInExpr(s.Limit); name != "" {
			return fmt.Errorf("no such column: %s", windowLimitColRef(s.Limit))
		}
	}
	if s.Offset != nil {
		if name := e.windowFuncInExpr(s.Offset); name != "" {
			return fmt.Errorf("no such column: %s", windowLimitColRef(s.Offset))
		}
	}
	return nil
}

// validateWindowFuncNodes validates every collected window function call.
func (e *SelectEngine) validateWindowFuncNodes(nodes []*sql.FuncCall, windows []sql.WindowDef) error {
	for _, fn := range nodes {
		if err := e.validateWindowFuncCall(fn, windows); err != nil {
			return err
		}
	}
	return nil
}

// validateWindowNodeSubqueries re-validates each window call and its OVER
// definition's subqueries after the WINDOW clause definitions passed.
func (e *SelectEngine) validateWindowNodeSubqueries(nodes []*sql.FuncCall, windows []sql.WindowDef) error {
	for _, fn := range nodes {
		if err := e.validateWindowFuncCall(fn, windows); err != nil {
			return err
		}
		if fn.Over != nil {
			if err := e.validateWindowDefSubqueries(fn.Over); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateWindowDefSubqueries validates scalar subqueries inside a window
// definition's PARTITION BY and ORDER BY terms.
func (e *SelectEngine) validateWindowDefSubqueries(w *sql.WindowDef) error {
	for _, p := range w.Partitions {
		if err := e.validateExprSubqueries(p); err != nil {
			return err
		}
	}
	for _, ob := range w.OrderBy {
		if err := e.validateExprSubqueries(ob.Expr); err != nil {
			return err
		}
	}
	return nil
}

// validateWindowDefinitions validates that each WINDOW clause definition's
// reference to another named window obeys the override rules. A definition
// like "win2 AS (win1 ORDER BY b)" references base window win1 (BaseName) with
// an added ORDER BY; SQLite rejects it when the base already defines the
// clause or the combination is invalid.
func (e *SelectEngine) validateWindowDefinitions(windows []sql.WindowDef) error {
	// Name resolution walks WINDOW-clause expressions too: a function used
	// with OVER inside a window definition's PARTITION BY / ORDER BY is
	// validated (windowE-2.1: PARTITION BY x_count(x) OVER w1).
	if err := e.validateWindowDefOverFuncs(windows); err != nil {
		return err
	}
	return e.validateWindowOverrides(windows)
}

// validateWindowDefOverFuncs validates OVER function usage inside every
// WINDOW-clause definition's PARTITION BY / ORDER BY expressions.
func (e *SelectEngine) validateWindowDefOverFuncs(windows []sql.WindowDef) error {
	for i := range windows {
		for _, p := range windows[i].Partitions {
			if err := e.windowDefOverFuncError(p); err != nil {
				return err
			}
		}
		for _, ob := range windows[i].OrderBy {
			if err := e.windowDefOverFuncError(ob.Expr); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateWindowOverrides checks each WINDOW definition that references a
// base window against SQLite's override rules.
func (e *SelectEngine) validateWindowOverrides(windows []sql.WindowDef) error {
	for i := range windows {
		if err := e.validateOneWindowOverride(&windows[i], windows); err != nil {
			return err
		}
	}
	return nil
}

// validateOneWindowOverride checks one WINDOW definition's reference to its
// base window: the base must exist and no already-defined base clause may be
// overridden.
func (e *SelectEngine) validateOneWindowOverride(w *sql.WindowDef, windows []sql.WindowDef) error {
	if w.BaseName == "" {
		return nil
	}
	base, ok := e.findNamedWindow(w.BaseName, windows)
	if !ok {
		return fmt.Errorf("no such window: %s", w.BaseName)
	}
	if w.Frame != nil && base.Frame != nil {
		return fmt.Errorf("cannot override frame specification of window: %s", w.BaseName)
	}
	if len(w.Partitions) > 0 {
		return fmt.Errorf("cannot override PARTITION clause of window: %s", w.BaseName)
	}
	if len(w.OrderBy) > 0 && len(base.OrderBy) > 0 {
		return fmt.Errorf("cannot override ORDER BY clause of window: %s", w.BaseName)
	}
	if len(w.OrderBy) > 0 && base.Frame != nil {
		return fmt.Errorf("cannot override frame specification of window: %s", w.BaseName)
	}
	return nil
}

// windowDefOverFuncError validates one WINDOW-clause expression: any
// function call carrying an OVER clause inside a window definition is checked
// against the registry (windowFuncOverError).
func (e *SelectEngine) windowDefOverFuncError(expr sql.Expr) error {
	if expr == nil {
		return nil
	}
	var err error
	WalkExprFull(expr, func(n sql.Expr) {
		if err != nil {
			return
		}
		if fc, ok := n.(*sql.FuncCall); ok && fc.Over != nil {
			err = e.windowFuncOverError(fc.Name)
		}
	})
	return err
}

// checkWindowFuncWithoutOver reports a misuse error when a built-in window
// function appears without an OVER clause.
func (e *SelectEngine) checkWindowFuncWithoutOver(expr sql.Expr) error {
	name := e.windowFuncWithoutOver(expr)
	if name != "" {
		return fmt.Errorf("misuse of window function %s()", name)
	}
	return nil
}

// windowInExprChildren returns the child expressions the nested-window
// detection walkers (windowFuncWithoutOver, windowFuncInExpr) visit for a
// node. FuncCall FILTER subtrees and RowValue nodes are not visited, matching
// the original walkers' scope.
func windowInExprChildren(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.FuncCall:
		kids := make([]sql.Expr, 0, len(v.Args)+len(v.OrderBy))
		kids = append(kids, v.Args...)
		for _, ob := range v.OrderBy {
			kids = append(kids, ob.Expr)
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
	}
	return nil
}

// windowFuncWithoutOver returns the name of the first built-in window function
// found in expr that has no OVER clause, or "".
func (e *SelectEngine) windowFuncWithoutOver(expr sql.Expr) string {
	if fc, ok := expr.(*sql.FuncCall); ok {
		if fc.Over == nil && windowOnlyFuncs[strings.ToUpper(fc.Name)] {
			// A registered scalar function shadows the built-in window name:
			// SQLite resolves rank(...) without OVER to the scalar function
			// when one is registered (the test build registers rank() via
			// install_fts3_rank_function — src/test_func.c). Only flag the
			// misuse when no scalar of that name exists.
			if fn, ok := e.ctx.Functions().Find(fc.Name); !ok || fn.Type != function.TypeScalar {
				return fc.Name
			}
		}
	}
	for _, kid := range windowInExprChildren(expr) {
		if name := e.windowFuncWithoutOver(kid); name != "" {
			return name
		}
	}
	return ""
}

// windowFuncInExpr returns the name of the first window function (with OVER)
// found in expr, or "".
func (e *SelectEngine) windowFuncInExpr(expr sql.Expr) string {
	if fc, ok := expr.(*sql.FuncCall); ok && fc.Over != nil {
		return fc.Name
	}
	for _, kid := range windowInExprChildren(expr) {
		if name := e.windowFuncInExpr(kid); name != "" {
			return name
		}
	}
	return ""
}

// windowLimitColRef returns the first column reference found inside a window
// function expression in a LIMIT/OFFSET clause (used for the "no such column:
// X" error SQLite reports).
func windowLimitColRef(expr sql.Expr) string {
	found := ""
	WalkExprFull(expr, func(en sql.Expr) {
		if found != "" {
			return
		}
		if ref, ok := en.(*sql.ColumnRef); ok && ref.Name != "*" {
			found = ref.Name
		}
	})
	if found == "" {
		found = "(unknown)"
	}
	return found
}

// collectWindowFuncsInColumns collects window function nodes from select
// columns (not descending into subqueries).
func (e *SelectEngine) collectWindowFuncsInColumns(columns []sql.SelectColumn, out *[]*sql.FuncCall) {
	for _, col := range columns {
		e.collectWindowFuncsNoSubquery(col.Expr, out)
	}
}

// collectWindowFuncsNoSubquery collects window function nodes from expr
// without descending into subqueries (window functions in subqueries are
// validated in the subquery's own scope). The traversal mirrors
// windowInExprChildren: no FILTER subtrees, no RowValue, no subqueries.
func (e *SelectEngine) collectWindowFuncsNoSubquery(expr sql.Expr, out *[]*sql.FuncCall) {
	if fc, ok := expr.(*sql.FuncCall); ok && fc.Over != nil {
		*out = append(*out, fc)
	}
	for _, kid := range windowInExprChildren(expr) {
		e.collectWindowFuncsNoSubquery(kid, out)
	}
}

// validateWindowFrame rejects frame specifications SQLite reports as
// "unsupported frame specification": a frame whose start is CURRENT ROW with
// a PRECEDING end, or whose start is FOLLOWING with a PRECEDING/CURRENT ROW
// end. A shorthand (non-BETWEEN) frame whose single bound is FOLLOWING
// implies an end of CURRENT ROW, so it is unsupported too (ROWS 4 FOLLOWING).
func (e *SelectEngine) validateWindowFrame(over *sql.WindowDef) error {
	if over == nil || over.Frame == nil {
		return nil
	}
	f := over.Frame
	startKind := f.Start.Kind
	endKind := f.End.Kind
	if !f.Between {
		// Shorthand frame: a single bound sets the start; the end is
		// CURRENT ROW (PRECEDING/UNBOUNDED PRECEDING/CURRENT ROW) or
		// CURRENT ROW for FOLLOWING.
		if startKind == "FOLLOWING" {
			return fmt.Errorf("unsupported frame specification")
		}
		return nil
	}
	if startKind == "CURRENT ROW" && endKind == "PRECEDING" {
		return fmt.Errorf("unsupported frame specification")
	}
	if startKind == "FOLLOWING" && (endKind == "PRECEDING" || endKind == "CURRENT ROW") {
		return fmt.Errorf("unsupported frame specification")
	}
	// ROWS/GROUPS frame offsets must be constant non-negative integers: a
	// column-reference offset (e.g. ROWS BETWEEN d FOLLOWING AND ...) is
	// rejected at prepare time (SQLite: "frame starting/ending offset must be
	// a non-negative integer").
	return e.validateFrameOffsetConstness(f)
}

// validateFrameOffsetConstness rejects a ROWS/GROUPS frame bound whose offset
// is a column reference.
func (e *SelectEngine) validateFrameOffsetConstness(f *sql.WindowFrame) error {
	if f.Type != "ROWS" && f.Type != "GROUPS" {
		return nil
	}
	bounds := []sql.FrameBound{f.Start, f.End}
	for i, b := range bounds {
		if (b.Kind == "PRECEDING" || b.Kind == "FOLLOWING") && e.exprHasColumnRef(b.Expr) {
			msg := "frame ending offset must be a non-negative integer"
			if i == 0 {
				msg = "frame starting offset must be a non-negative integer"
			}
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}

// validateWindowFuncCall validates one window function call.
func (e *SelectEngine) validateWindowFuncCall(fn *sql.FuncCall, windows []sql.WindowDef) error {
	// A named window (referenced by Name for "OVER name" or by BaseName for
	// "OVER (name ORDER BY ...)") must exist; when it does, check that the
	// OVER clause does not override a clause the base window already defines
	// (SQLite: "cannot override ... of window: NAME").
	if err := e.checkWindowRefOverride(fn.Over, windows); err != nil {
		return err
	}
	name := strings.ToUpper(fn.Name)
	// DISTINCT is not supported for window functions (SQLite: "DISTINCT is
	// not supported for window functions").
	if fn.Distinct {
		return fmt.Errorf("DISTINCT is not supported for window functions")
	}
	if err := e.validateWindowSubqueryArgs(fn); err != nil {
		return err
	}
	// Unsupported frame specifications (SQLite sqlite3WindowAlloc): a frame
	// whose start is CURRENT ROW with a PRECEDING end, or whose start is
	// FOLLOWING with a PRECEDING/CURRENT ROW end. A shorthand frame whose
	// single bound is FOLLOWING implies an end of CURRENT ROW.
	if err := e.validateWindowFrame(fn.Over); err != nil {
		return err
	}
	// FILTER is only valid with aggregate window functions.
	if fn.Filter != nil && !e.isAggregateWindowFunc(fn) {
		return fmt.Errorf("FILTER clause may only be used with aggregate window functions")
	}
	// Built-in window functions: validate argument counts.
	if err := e.validateWindowFuncArity(fn, name); err != nil {
		return err
	}
	// A non-window scalar function used with OVER is an error, as is a
	// classic application-registered aggregate without a window-value
	// callback (resolve.c: pDef->xValue==0 && pWin → "%s() may not be used
	// as a window function", windowE-2.1's x_count()).
	if fn.Over != nil && !windowOnlyFuncs[name] {
		if err := e.windowFuncOverError(fn.Name); err != nil {
			return err
		}
	}
	return nil
}

// validateWindowSubqueryArgs rejects a scalar subquery argument containing a
// correlated aggregate (window1 59.1: ntile((SELECT sum(x))) OVER ... →
// "misuse of aggregate: sum()").
func (e *SelectEngine) validateWindowSubqueryArgs(fn *sql.FuncCall) error {
	for _, arg := range fn.Args {
		if sub, ok := arg.(*sql.Subquery); ok && sub.Select != nil {
			if name := e.subqueryOuterAggRef(sub.Select); name != "" {
				return fmt.Errorf("misuse of aggregate: %s()", name)
			}
		}
	}
	return nil
}

// validateWindowFuncArity validates the built-in window functions' argument
// counts (and nth_value's constant second argument).
func (e *SelectEngine) validateWindowFuncArity(fn *sql.FuncCall, name string) error {
	switch name {
	case "ROW_NUMBER", "RANK", "DENSE_RANK", "PERCENT_RANK", "CUME_DIST", "NTILE":
		return e.validateRankingFuncArity(fn, name)
	case "LEAD", "LAG", "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE":
		return e.validateValueFuncArity(fn, name)
	}
	return nil
}

// validateRankingFuncArity validates the ranking window functions' argument
// counts (ROW_NUMBER family and ntile).
func (e *SelectEngine) validateRankingFuncArity(fn *sql.FuncCall, name string) error {
	switch name {
	case "ROW_NUMBER", "RANK", "DENSE_RANK", "PERCENT_RANK", "CUME_DIST":
		if len(fn.Args) != 0 {
			return fmt.Errorf("wrong number of arguments to function %s()", strings.ToLower(name))
		}
	case "NTILE":
		if len(fn.Args) != 1 {
			return fmt.Errorf("wrong number of arguments to function ntile()")
		}
		if n, ok := e.ntileConstArg(fn); ok && n <= 0 {
			return fmt.Errorf("argument of ntile must be a positive integer")
		}
	}
	return nil
}

// validateValueFuncArity validates LEAD/LAG and the value window functions'
// argument counts.
func (e *SelectEngine) validateValueFuncArity(fn *sql.FuncCall, name string) error {
	switch name {
	case "LEAD", "LAG":
		if len(fn.Args) < 1 || len(fn.Args) > 3 {
			return fmt.Errorf("wrong number of arguments to function %s()", strings.ToLower(name))
		}
	case "FIRST_VALUE", "LAST_VALUE":
		if len(fn.Args) != 1 {
			return fmt.Errorf("wrong number of arguments to function %s()", strings.ToLower(name))
		}
	case "NTH_VALUE":
		if len(fn.Args) != 2 {
			return fmt.Errorf("wrong number of arguments to function nth_value()")
		}
		if err := e.validateNthValueArg(fn.Args[1]); err != nil {
			return err
		}
	}
	return nil
}

// checkWindowRefOverride validates an OVER clause's named-window reference:
// the base must exist and must not be illegally overridden. An inline OVER
// (no reference name) passes.
func (e *SelectEngine) checkWindowRefOverride(over *sql.WindowDef, windows []sql.WindowDef) error {
	refName := over.Name
	if refName == "" {
		refName = over.BaseName
	}
	if refName == "" {
		return nil
	}
	return e.checkWindowOverride(over, refName, windows)
}

// windowFuncOverError returns the resolve.c "may not be used as a window
// function" error when name is not a window-capable aggregate (unknown
// functions and classic application aggregates both reject OVER); nil when
// the function may be used with OVER.
func (e *SelectEngine) windowFuncOverError(name string) error {
	reg, found := e.ctx.Functions().Find(name)
	if !found || reg.Type != function.TypeAggregate || reg.ClassicAggregate {
		return fmt.Errorf("%s() may not be used as a window function", strings.ToLower(name))
	}
	return nil
}

// validateNthValueArg validates the second argument of nth_value(): it must
// be a positive integer. A constant argument that is not a positive integer
// (0, negative, a non-numeric string, NULL, or a non-integral real) is
// rejected at prepare time; non-constant arguments are checked per row at
// execution.
func (e *SelectEngine) validateNthValueArg(arg sql.Expr) error {
	constErr := func() error {
		return fmt.Errorf("second argument to nth_value must be a positive integer")
	}
	v, err := e.ctx.EvalExpr(arg, RowMap{})
	if err != nil || e.exprHasColumnRef(arg) {
		// Non-constant expression (needs a row, e.g. nth_value(b, c) or
		// nth_value(b, b+1)): the runtime check handles it per row.
		return nil
	}
	nv := util.UnwrapColumnValue(unwrapCollatedValue(v))
	switch x := nv.(type) {
	case int64:
		if x < 1 {
			return constErr()
		}
	case float64:
		if x < 1 || math.Trunc(x) != x {
			return constErr()
		}
	case string:
		// SQLite converts the string to a number first ('2' and '2.0' are
		// both the integer 2; '4ab' is not a number).
		fv, convErr := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if convErr != nil || fv < 1 || math.Trunc(fv) != fv {
			return constErr()
		}
	default:
		// NULL (or an unhandled constant type) is not a positive integer.
		return constErr()
	}
	return nil
}

// checkWindowOverride validates that an OVER clause referencing base window
// refName does not illegally override the base's clauses.
func (e *SelectEngine) checkWindowOverride(over *sql.WindowDef, refName string, windows []sql.WindowDef) error {
	base, ok := e.findNamedWindow(refName, windows)
	if !ok {
		return fmt.Errorf("no such window: %s", refName)
	}
	if over.Frame != nil && base.Frame != nil {
		return fmt.Errorf("cannot override frame specification of window: %s", refName)
	}
	// SQLite rejects adding a PARTITION BY clause to ANY named window
	// reference, even when the base window has no partitions (window.c
	// sqlite3WindowRewrite).
	if len(over.Partitions) > 0 {
		return fmt.Errorf("cannot override PARTITION clause of window: %s", refName)
	}
	if len(over.OrderBy) > 0 && len(base.OrderBy) > 0 {
		return fmt.Errorf("cannot override ORDER BY clause of window: %s", refName)
	}
	// Adding an ORDER BY to a base window that already has a frame is
	// rejected: the frame's ORDER BY dependency cannot be re-established
	// (window.c sqlite3WindowAssemble).
	if len(over.OrderBy) > 0 && base.Frame != nil {
		return fmt.Errorf("cannot override frame specification of window: %s", refName)
	}
	return nil
}

// findNamedWindow returns the WINDOW-clause definition for name, if present.
func (e *SelectEngine) findNamedWindow(name string, windows []sql.WindowDef) (sql.WindowDef, bool) {
	for _, w := range windows {
		if w.Name == name {
			return w, true
		}
	}
	return sql.WindowDef{}, false
}

// isAggregateWindowFunc reports whether fn is an aggregate function used as a
// window (e.g. sum() OVER ...).
