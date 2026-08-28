package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *SelectEngine) hasAggregates(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if e.exprHasAggregate(col.Expr) {
			return true
		}
	}
	return false
}

func (e *SelectEngine) exprHasAggregate(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return e.exprFuncCallHasAggregate(v)
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		left, right := BinaryExprOperands(v)
		return e.exprHasAggregate(left) || e.exprHasAggregate(right)
	case *sql.UnaryOp, *sql.ParenExpr, *sql.CastExpr, *sql.IsNull, *sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		return e.exprHasAggregate(singleExprOperand(v))
	case *sql.Between:
		return e.exprBetweenHasAggregate(v)
	case *sql.InList:
		return e.exprListHasAggregate(v.Operand, v.List)
	case *sql.CaseExpr:
		return e.exprCaseHasAggregate(v)
	case *sql.Subquery, *sql.ExistsExpr:
		// Aggregates inside a scalar subquery are scoped to that subquery;
		// they do not make the OUTER query an aggregate query
		// (SELECT (SELECT count(*) FROM t) FROM u returns one row per u
		// row, not a single collapsed row).
		return false
	case *sql.RowValue:
		if len(v.Values) == 0 {
			return false
		}
		return e.exprListHasAggregate(v.Values[0], v.Values[1:])
	default:
		return false
	}
}

// binaryExprOperands returns the left and right operands of a binary-like
// expression node, or nil for other node types.
func BinaryExprOperands(expr interface{}) (sql.Expr, sql.Expr) {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return v.Left, v.Right
	case *sql.IsDistinctFrom:
		return v.Left, v.Right
	case *sql.IsNotDistinctFrom:
		return v.Left, v.Right
	}
	return nil, nil
}

// singleExprOperand returns the single operand of a unary-like expression
// node, or nil for other node types.
func singleExprOperand(expr interface{}) sql.Expr {
	switch v := expr.(type) {
	case *sql.UnaryOp:
		return v.Operand
	case *sql.ParenExpr:
		return v.Expr
	case *sql.CastExpr:
		return v.Operand
	case *sql.IsNull:
		return v.Operand
	case *sql.IsNotNull:
		return v.Operand
	case *sql.IsTrue:
		return v.Operand
	case *sql.IsFalse:
		return v.Operand
	}
	return nil
}

// exprBetweenHasAggregate reports whether a BETWEEN expression contains an
// aggregate in its operand or bounds.
func (e *SelectEngine) exprBetweenHasAggregate(v *sql.Between) bool {
	if e.exprHasAggregate(v.Operand) {
		return true
	}
	if e.exprHasAggregate(v.Low) {
		return true
	}
	return e.exprHasAggregate(v.High)
}

// exprFuncCallHasAggregate reports whether a function-call expression contains
// an aggregate (MIN/MAX with two or more arguments are scalar, not aggregate).
func (e *SelectEngine) exprFuncCallHasAggregate(v *sql.FuncCall) bool {
	// A window function does not by itself collapse the query into a single
	// aggregate row, but a plain aggregate nested in its arguments or OVER
	// clause (PARTITION BY / ORDER BY) does: SQLite evaluates the inner
	// aggregate as a regular aggregate and runs the window over the aggregate
	// result rows (e.g. SELECT min(sum(a)) OVER () FROM t → one row).
	if v.Over != nil {
		return e.exprWindowHasNestedAggregate(v)
	}
	if fn, ok := e.ctx.Functions().Find(v.Name); ok && fn.Type == function.TypeAggregate {
		// MIN/MAX are scalar functions when given two or more arguments
		// (SQLite: min(X,Y,...) is scalar; min(X) is aggregate). Without
		// this check a plain per-row min(b,5) collapses the whole query
		// into a single aggregate row.
		if (strings.EqualFold(v.Name, "MIN") || strings.EqualFold(v.Name, "MAX")) && len(v.Args) >= 2 {
			return e.exprListHasAggregate(v.Args[0], v.Args[1:])
		}
		return true
	}
	return e.exprListHasAggregate(nil, v.Args)
}

// exprWindowHasNestedAggregate reports whether a window function expression
// contains a plain aggregate in its arguments or its OVER clause (PARTITION BY
// or ORDER BY). Such a query is an aggregate query in SQLite: the nested
// aggregate is evaluated as a regular aggregate over the input rows and the
// window function runs over the resulting rows.
func (e *SelectEngine) exprWindowHasNestedAggregate(v *sql.FuncCall) bool {
	if e.exprListHasAggregate(nil, v.Args) {
		return true
	}
	if v.Over == nil {
		return false
	}
	if e.exprListHasAggregate(nil, v.Over.Partitions) {
		return true
	}
	for _, ob := range v.Over.OrderBy {
		if e.exprHasAggregate(ob.Expr) {
			return true
		}
	}
	return false
}

// exprListHasAggregate reports whether any of the given expressions contains an
// aggregate.
func (e *SelectEngine) exprListHasAggregate(first sql.Expr, rest []sql.Expr) bool {
	if first != nil && e.exprHasAggregate(first) {
		return true
	}
	for _, item := range rest {
		if e.exprHasAggregate(item) {
			return true
		}
	}
	return false
}

// exprCaseHasAggregate reports whether a CASE expression contains an aggregate
// in its operand, whens, or else.
func (e *SelectEngine) exprCaseHasAggregate(v *sql.CaseExpr) bool {
	if v.Operand != nil && e.exprHasAggregate(v.Operand) {
		return true
	}
	for _, w := range v.Whens {
		if e.exprHasAggregate(w.When) {
			return true
		}
		if e.exprHasAggregate(w.Then) {
			return true
		}
	}
	if v.Else != nil {
		return e.exprHasAggregate(v.Else)
	}
	return false
}

// minMaxAggregate describes a single-argument MIN or MAX aggregate function
// call. It is used to resolve the source row for bare columns in aggregate
// queries: SQLite evaluates bare columns against the input row that produced
// the last min/max aggregate in the result set.
type minMaxAggregate struct {
	name string // "MIN" or "MAX" (uppercased)
	arg  sql.Expr
}

// lastMinMaxAggregate returns the last (rightmost) single-argument MIN/MAX
// aggregate function call found in the SELECT columns, scanning left to right
// and descending into nested expressions. Returns nil when the result set has
// no min/max aggregate.
func (e *SelectEngine) lastMinMaxAggregate(columns []sql.SelectColumn) *minMaxAggregate {
	var last *minMaxAggregate
	for _, col := range columns {
		if mm := lastMinMaxInExpr(col.Expr, e.ctx.Functions()); mm != nil {
			last = mm
		}
	}
	return last
}

// minMaxSourceRow evaluates a single-argument MIN/MAX aggregate's argument
// over the given rows and returns the index of the row that produced the
// extreme value (the first row on ties). When every argument is NULL the
// aggregate yields NULL and bare columns take the last row, matching SQLite.
// Returns -1 when rows is empty.
func (e *SelectEngine) minMaxSourceRow(mm *minMaxAggregate, rowMaps []RowMap) int {
	if len(rowMaps) == 0 {
		return -1
	}
	bestIdx := -1
	var bestVal interface{}
	for i, row := range rowMaps {
		val, err := e.ctx.EvalExpr(mm.arg, row)
		if err != nil || val == nil {
			continue
		}
		val = util.UnwrapColumnValue(val)
		if bestIdx < 0 {
			bestIdx = i
			bestVal = val
			continue
		}
		cmp := util.CompareValues(val, bestVal)
		if (mm.name == "MIN" && cmp < 0) || (mm.name == "MAX" && cmp > 0) {
			bestIdx = i
			bestVal = val
		}
	}
	if bestIdx < 0 {
		// All arguments NULL (or empty rows): bare columns come from the
		// last row.
		return len(rowMaps) - 1
	}
	return bestIdx
}

// reorderRowsForMinMax moves the row that produced the last min/max aggregate
// to the front of rowMaps so bare columns in an aggregate query evaluate from
// the correct source row. The min/max aggregate is searched in the SELECT
// columns and the HAVING clause (a bare output column paired with
// "HAVING max(x) ..." takes the row that produced the max, matching SQLite).
// Returns the reordered slice (a copy is not made unless reordering is needed).
func (e *SelectEngine) reorderRowsForMinMax(s *sql.SelectStmt, rowMaps []RowMap) []RowMap {
	mm := e.lastMinMaxAggregate(s.Columns)
	if mm == nil && s.Having != nil {
		mm = lastMinMaxInExpr(s.Having, e.ctx.Functions())
	}
	if mm == nil || len(rowMaps) <= 1 {
		return rowMaps
	}
	idx := e.minMaxSourceRow(mm, rowMaps)
	if idx <= 0 {
		return rowMaps
	}
	rows := make([]RowMap, len(rowMaps))
	copy(rows, rowMaps)
	rows[0], rows[idx] = rows[idx], rows[0]
	return rows
}

// aggregateName returns the name of the first aggregate function found in the
// expression, or "?" if none is found.
func (e *SelectEngine) aggregateName(expr sql.Expr) string {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if fn, ok := e.ctx.Functions().Find(v.Name); ok && fn.Type == function.TypeAggregate {
			// MIN/MAX with two or more arguments are scalar functions, not
			// aggregates (SQLite: min(X,Y,...) is scalar; min(X) is aggregate).
			if (strings.EqualFold(v.Name, "MIN") || strings.EqualFold(v.Name, "MAX")) && len(v.Args) >= 2 {
				return "?"
			}
			return v.Name
		}
		return "?"
	case *sql.BinaryOp:
		if n := e.aggregateName(v.Left); n != "?" {
			return n
		}
		return e.aggregateName(v.Right)
	case *sql.UnaryOp:
		return e.aggregateName(v.Operand)
	default:
		return "?"
	}
}

// exprHasColumnRef recursively checks if an expression tree contains a ColumnRef node.
// This does NOT recurse into Subquery expressions — correlated aggregate detection
// is handled separately at the SELECT level.
func (e *SelectEngine) exprHasColumnRef(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		// A bare "*" (count(*), SELECT *) is not a column reference: it
		// means "all rows of the FROM table", so it must not count as an
		// outer reference in correlated-aggregate detection.
		return v.Name != "*"
	case *sql.BinaryOp:
		return e.exprHasColumnRef(v.Left) || e.exprHasColumnRef(v.Right)
	case *sql.UnaryOp:
		return e.exprHasColumnRef(v.Operand)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if e.exprHasColumnRef(arg) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// exprHasColRefInMap checks if an expression tree contains a ColumnRef whose
// name matches an entry in the provided column name map.
func exprHasColRefInMap(expr sql.Expr, colNames map[string]bool) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return colNames[v.Name]
	case *sql.BinaryOp:
		return exprHasColRefInMap(v.Left, colNames) || exprHasColRefInMap(v.Right, colNames)
	case *sql.UnaryOp:
		return exprHasColRefInMap(v.Operand, colNames)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if exprHasColRefInMap(arg, colNames) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// exprHasCorrelatedSubquery checks if an expression tree contains a subquery
// that has a correlated aggregate.
//
//lint:ignore U1000  Planned for query optimization
func (e *SelectEngine) exprHasCorrelatedSubquery(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.Subquery:
		return e.selectHasCorrelatedAggSubquery(v.Select)
	case *sql.BinaryOp:
		return e.exprHasCorrelatedSubquery(v.Left) || e.exprHasCorrelatedSubquery(v.Right)
	case *sql.UnaryOp:
		return e.exprHasCorrelatedSubquery(v.Operand)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if e.exprHasCorrelatedSubquery(arg) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// hasSubqueryWithCorrelatedAgg checks if any SELECT column contains a subquery
// that has a correlated aggregate at any nesting depth.
func (e *SelectEngine) hasSubqueryWithCorrelatedAgg(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		found := false
		WalkExprFull(col.Expr, func(en sql.Expr) {
			if found {
				return
			}
			if subq, ok := en.(*sql.Subquery); ok && subq.Select != nil {
				if e.selectHasCorrelatedAggSubquery(subq.Select) {
					found = true
				}
			}
		})
		if found {
			return true
		}
	}
	return false
}

// resolveGroupByOrdinals maps numeric GROUP BY terms (e.g., GROUP BY 2) to
// the corresponding SELECT column expression (1-based ordinal). When the
// ordinal's SELECT column is a bare star (*), the ordinal groups by the first
// output column of the result (SQLite resolves GROUP BY N against result
// columns positionally), so it resolves to a reference to that column.
func resolveGroupByOrdinals(s *sql.SelectStmt, colDefs []sql.ColumnDef) []sql.Expr {
	if len(s.GroupBy) == 0 {
		return nil
	}
	resolved := make([]sql.Expr, len(s.GroupBy))
	for i, g := range s.GroupBy {
		if num, ok := g.(*sql.NumericLit); ok {
			var ord int64
			fmt.Sscanf(num.Value, "%d", &ord)
			if ord >= 1 && int(ord) <= len(s.Columns) {
				col := s.Columns[ord-1]
				if ref, isStar := col.Expr.(*sql.ColumnRef); isStar && ref.Name == "*" && ref.Table == "" && len(colDefs) > 0 {
					// GROUP BY 1 on SELECT * groups by the first result column.
					resolved[i] = &sql.ColumnRef{Name: colDefs[0].Name}
				} else {
					resolved[i] = col.Expr
				}
				continue
			}
		}
		resolved[i] = g
	}
	return resolved
}

// matchGroupByExpr returns the index of the GROUP BY term that matches the
// given output-column expression (same AST node or structurally identical),
// or -1 when no term matches. SQLite reuses the grouping value for output
// columns that are GROUP BY expressions, so non-deterministic functions
// (random()) and formatting stay consistent.
func matchGroupByExpr(groupBy []sql.Expr, col sql.Expr) int {
	for i, g := range groupBy {
		if g == col {
			return i
		}
		if exprStructurallyEqual(g, col) {
			return i
		}
	}
	return -1
}

// computeGroupByKeyValues evaluates each GROUP BY expression for a row,
// returning a serialized string key and the raw evaluated values (used to
// sort the output groups, matching SQLite's key-order GROUP BY output). The
// key honors the expression's collation so values equal under that collation
// (e.g. 'abc'/'aBC' under NOCASE) group together.
func (e *SelectEngine) computeGroupByKeyValues(groupBy []sql.Expr, row Row) (string, []interface{}) {
	parts := make([]string, len(groupBy))
	values := make([]interface{}, len(groupBy))
	for i, expr := range groupBy {
		v, err := e.ctx.EvalExpr(expr, row)
		if err != nil || v == nil {
			parts[i] = "\x00"
			values[i] = nil
		} else {
			coll := groupByExprCollation(v)
			uv := unwrapGroupByValue(v)
			parts[i] = collationGroupKey(uv, coll)
			values[i] = uv
		}
	}
	return strings.Join(parts, "\x00"), values
}

// groupByExprCollation extracts the collation marker from a GROUP BY
// expression's evaluated value. A CollatedValue carries the column's declared
// collation (e.g. COLLATE nocase); an explicit COLLATE operator propagates via
// execexpr.ExprCollation.
func groupByExprCollation(v interface{}) string {
	if cv, ok := v.(*execexpr.CollatedValue); ok {
		return cv.Collation
	}
	return ""
}

// unwrapGroupByValue strips the CollatedValue and ColumnValue wrappers from a
// GROUP BY expression's evaluated value, returning the raw value.
func unwrapGroupByValue(v interface{}) interface{} {
	if cv, ok := v.(*execexpr.CollatedValue); ok {
		return unwrapGroupByValue(cv.Value)
	}
	return util.UnwrapColumnValue(v)
}

// collationGroupKey serializes a GROUP BY value into a key that groups values
// equal under the expression's collation. For the built-in case-folding
// collations this folds the text; BINARY and unknown collations keep the raw
// value (so the key stays lossless).
func collationGroupKey(v interface{}, coll string) string {
	s := fmt.Sprintf("%v", v)
	switch strings.ToUpper(coll) {
	case "NOCASE":
		return strings.ToLower(s)
	case "RTRIM":
		return strings.TrimRight(s, " ")
	default:
		return s
	}
}

// evalHaving evaluates a HAVING expression by treating aggregate function
// calls as group-aware (evaluating over all rows in the group).
func (e *SelectEngine) evalHaving(expr sql.Expr, groupRows []RowMap) (bool, error) {
	v, err := e.evalHavingExpr(expr, groupRows)
	if err != nil {
		return false, err
	}
	return execexpr.ToBool(v), nil
}

// evalHavingExpr recursively evaluates an expression, handling aggregate
// functions across all groupRows.
func (e *SelectEngine) evalHavingExpr(expr sql.Expr, groupRows []RowMap) (interface{}, error) {
	if expr == nil {
		return nil, nil
	}
	switch v := expr.(type) {
	case *sql.FuncCall:
		return e.evalHavingFuncCall(v, groupRows)
	case *sql.BinaryOp:
		return e.evalHavingBinaryOp(v, groupRows)
	case *sql.UnaryOp:
		return e.evalHavingUnary(v, groupRows)
	case *sql.IsNull:
		return e.evalHavingIsNull(v, groupRows)
	case *sql.IsNotNull:
		return e.evalHavingIsNotNull(v, groupRows)
	case *sql.IsDistinctFrom:
		return e.evalHavingIsDistinctFrom(v, groupRows)
	case *sql.IsNotDistinctFrom:
		return e.evalHavingIsNotDistinctFrom(v, groupRows)
	case *sql.Subquery:
		return e.evalHavingSubquery(v, groupRows)
	default:
		return e.evalHavingDefault(expr, groupRows)
	}
}

// evalHavingBinaryOp evaluates a binary operator in a HAVING clause, applying
// SQLite's NULL propagation for non-AND/OR operators. IS / IS NOT are
// NULL-safe (NULL IS NULL is true), so they skip the propagation.
func (e *SelectEngine) evalHavingBinaryOp(v *sql.BinaryOp, groupRows []RowMap) (interface{}, error) {
	left, err := e.evalHavingExpr(v.Left, groupRows)
	if err != nil {
		return nil, err
	}
	right, err := e.evalHavingExpr(v.Right, groupRows)
	if err != nil {
		return nil, err
	}
	// NULL propagation for non-AND/OR ops (IS / IS NOT are NULL-safe).
	if v.Operator != "AND" && v.Operator != "OR" && v.Operator != "IS" && v.Operator != "IS NOT" {
		if left == nil || right == nil {
			return nil, nil
		}
	}
	// IS / IS NOT are NULL-safe and are not dispatched by EvalBinaryOpValues
	// (the scalar binary-op path only handles comparison/arithmetic). Evaluate
	// them directly.
	if v.Operator == "IS" {
		return execexpr.BoolToInt(e.ctx.Expr().IsEqualNullSafe(left, right)), nil
	}
	if v.Operator == "IS NOT" {
		return execexpr.BoolToInt(!e.ctx.Expr().IsEqualNullSafe(left, right)), nil
	}
	return e.ctx.Expr().EvalBinaryOpValues(v.Operator, left, right)
}

// evalHavingIsNull evaluates an IS NULL expression in a HAVING clause.
func (e *SelectEngine) evalHavingIsNull(v *sql.IsNull, groupRows []RowMap) (interface{}, error) {
	operand, err := e.evalHavingExpr(v.Operand, groupRows)
	if err != nil {
		return nil, err
	}
	if len(groupRows) > 0 {
	}
	operand = util.UnwrapColumnValue(operand)
	return execexpr.BoolToInt(operand == nil), nil
}
func (e *SelectEngine) evalHavingFuncCall(v *sql.FuncCall, groupRows []RowMap) (interface{}, error) {
	fn, ok := e.ctx.Functions().Find(v.Name)
	if ok && fn.Type == function.TypeAggregate {
		if v.Distinct {
			return e.evalDistinctAggregate(v, groupRows), nil
		}
		return e.evalAggFuncCall(v, groupRows)
	}
	if len(groupRows) > 0 {
		return e.ctx.EvalFuncCall(v, groupRows[0])
	}
	return nil, nil
}

func (e *SelectEngine) evalHavingUnary(v *sql.UnaryOp, groupRows []RowMap) (interface{}, error) {
	operand, err := e.evalHavingExpr(v.Operand, groupRows)
	if err != nil {
		return nil, err
	}
	switch v.Operator {
	case "NOT":
		if operand == nil {
			return nil, nil
		}
		return execexpr.BoolToInt(!execexpr.ToBool(operand)), nil
	case "-":
		return execexpr.NegateValue(operand)
	default:
		return nil, nil
	}
}

func (e *SelectEngine) evalHavingIsNotNull(v *sql.IsNotNull, groupRows []RowMap) (interface{}, error) {
	operand, err := e.evalHavingExpr(v.Operand, groupRows)
	if err != nil {
		return nil, err
	}
	operand = util.UnwrapColumnValue(operand)
	return execexpr.BoolToInt(operand != nil), nil
}

func (e *SelectEngine) evalHavingIsDistinctFrom(v *sql.IsDistinctFrom, groupRows []RowMap) (interface{}, error) {
	left, err := e.evalHavingExpr(v.Left, groupRows)
	if err != nil {
		return nil, err
	}
	right, err := e.evalHavingExpr(v.Right, groupRows)
	if err != nil {
		return nil, err
	}
	if left == nil && right == nil {
		return int64(0), nil
	}
	if left == nil || right == nil {
		return int64(1), nil
	}
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(0), nil
	}
	return int64(1), nil
}

func (e *SelectEngine) evalHavingIsNotDistinctFrom(v *sql.IsNotDistinctFrom, groupRows []RowMap) (interface{}, error) {
	left, err := e.evalHavingExpr(v.Left, groupRows)
	if err != nil {
		return nil, err
	}
	right, err := e.evalHavingExpr(v.Right, groupRows)
	if err != nil {
		return nil, err
	}
	if left == nil && right == nil {
		return int64(1), nil
	}
	if left == nil || right == nil {
		return int64(0), nil
	}
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *SelectEngine) evalHavingDefault(expr sql.Expr, groupRows []RowMap) (interface{}, error) {
	if len(groupRows) > 0 {
		return e.ctx.EvalExpr(expr, groupRows[0])
	}
	return nil, nil
}

// evalHavingSubquery evaluates a Subquery expression in a HAVING clause.
// It sets outerRows to all group rows so that correlated aggregates within
// the subquery can evaluate over the entire group (not just one row).
func (e *SelectEngine) evalHavingSubquery(v *sql.Subquery, groupRows []RowMap) (interface{}, error) {
	prevOuterRows := e.outerRows
	if len(groupRows) > 0 {
		e.outerRows = groupRows
	}
	result, err := e.ctx.EvalSubquery(v, groupRows[0])
	e.outerRows = prevOuterRows
	return result, err
}

func (e *SelectEngine) evalAggregateExpr(expr sql.Expr, rowMaps []RowMap) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if v.Distinct {
			return e.evalDistinctAggregate(v, rowMaps), nil
		}
		return e.evalAggFuncCall(v, rowMaps)
	default:
		if len(rowMaps) > 0 {
			val, err := e.ctx.EvalExpr(expr, rowMaps[0])
			return val, err
		}
		return nil, nil
	}
}

// findNestedAggregate checks if an expression tree contains an aggregate function call
// and returns its name. It does NOT descend into subqueries, since subqueries have
// their own evaluation context. Returns "" if no nested aggregate is found.
func findNestedAggregate(expr sql.Expr, funcs *function.Registry) string {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return findNestedAggregateFuncCall(v, funcs)
	case *sql.BinaryOp:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.UnaryOp, *sql.IsNull, *sql.IsNotNull, *sql.CastExpr:
		return findNestedAggregate(aggregateSingleOperand(v), funcs)
	case *sql.IsDistinctFrom:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.IsNotDistinctFrom:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.Between:
		return findNestedAggregateBetween(v, funcs)
	case *sql.InList:
		return findNestedAggregateInList(v, funcs)
	case *sql.CaseExpr:
		return findNestedAggregateCaseExpr(v, funcs)
	case *sql.RowValue:
		return findNestedAggregateRowValue(v, funcs)
	case *sql.Subquery, *sql.ExistsExpr:
		return ""
	default:
		return ""
	}
}

// aggregateSingleOperand returns the single operand of a unary-like aggregate
// expression node, or nil for other node types.
func aggregateSingleOperand(expr interface{}) sql.Expr {
	switch v := expr.(type) {
	case *sql.UnaryOp:
		return v.Operand
	case *sql.IsNull:
		return v.Operand
	case *sql.IsNotNull:
		return v.Operand
	case *sql.CastExpr:
		return v.Operand
	}
	return nil
}

func findNestedAggregateFuncCall(v *sql.FuncCall, funcs *function.Registry) string {
	if fn, ok := funcs.Find(v.Name); ok && fn.Type == function.TypeAggregate {
		// MIN/MAX are scalar functions when given two or more arguments
		// (SQLite: min(X,Y,...) is scalar; min(X) is aggregate). A scalar
		// min(a,b) nested inside an aggregate is not a nested aggregate.
		if (strings.EqualFold(v.Name, "MIN") || strings.EqualFold(v.Name, "MAX")) && len(v.Args) >= 2 {
			// fall through to check the arguments for a real nested aggregate
		} else {
			return v.Name
		}
	}
	for _, arg := range v.Args {
		if nested := findNestedAggregate(arg, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

func findNestedAggregateBinary(left, right sql.Expr, funcs *function.Registry) string {
	if nested := findNestedAggregate(left, funcs); nested != "" {
		return nested
	}
	return findNestedAggregate(right, funcs)
}

func findNestedAggregateBetween(v *sql.Between, funcs *function.Registry) string {
	if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
		return nested
	}
	if nested := findNestedAggregate(v.Low, funcs); nested != "" {
		return nested
	}
	return findNestedAggregate(v.High, funcs)
}

func findNestedAggregateInList(v *sql.InList, funcs *function.Registry) string {
	if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
		return nested
	}
	for _, item := range v.List {
		if nested := findNestedAggregate(item, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

func findNestedAggregateCaseExpr(v *sql.CaseExpr, funcs *function.Registry) string {
	if v.Operand != nil {
		if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
			return nested
		}
	}
	for _, w := range v.Whens {
		if nested := findNestedAggregate(w.When, funcs); nested != "" {
			return nested
		}
		if nested := findNestedAggregate(w.Then, funcs); nested != "" {
			return nested
		}
	}
	if v.Else != nil {
		return findNestedAggregate(v.Else, funcs)
	}
	return ""
}

func findNestedAggregateRowValue(v *sql.RowValue, funcs *function.Registry) string {
	for _, val := range v.Values {
		if nested := findNestedAggregate(val, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

// validateRowValueInList validates an IN-list expression's row-value usage,
// including row-value IN subquery arity.
