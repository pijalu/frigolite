package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// Aggregate function-call reduction (split from select_agg.go for file-size
// hygiene): dispatching one aggregate call to its registry implementation,
// the single-argument MIN/MAX collation-aware reduction, and DISTINCT
// aggregate handling. Statement-level evaluation lives in select_agg.go.

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

// minMaxReduction carries one single-argument MIN/MAX reduction's state: the
// best value so far, its comparison direction, and the collation donated by
// the first argument value carrying a CollatedValue marker.
type minMaxReduction struct {
	isMax     bool
	best      interface{}
	collation string
}

// evalMinMaxAggregate reduces a single-argument MIN/MAX under the argument's
// collation: the first evaluated argument value carrying a CollatedValue
// marker donates the collation (explicit COLLATE operator or the column's
// declared COLLATE clause — SQLite's sqlite3ExprCollSeq of the argument).
// NULLs are skipped; the first extreme on ties wins (minmaxStep keeps the
// earliest row's value).
func (e *SelectEngine) evalMinMaxAggregate(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error) {
	r := &minMaxReduction{isMax: strings.EqualFold(v.Name, "MAX")}
	for _, row := range rowMaps {
		if !e.aggRowPassesFilter(v, row) {
			continue
		}
		if err := r.stepArg(e, v.Args[0], row); err != nil {
			return nil, err
		}
	}
	return r.best, nil
}

// stepArg evaluates the MIN/MAX argument for one row and folds the value
// into the reduction.
func (r *minMaxReduction) stepArg(e *SelectEngine, arg sql.Expr, row RowMap) error {
	restore := e.ctx.EnterAuxAggArg()
	raw, err := e.ctx.EvalExpr(arg, row)
	restore()
	if err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	val := util.UnwrapColumnValue(raw)
	if cv, ok := raw.(*execexpr.CollatedValue); ok {
		val = util.UnwrapColumnValue(cv.Value)
		if r.collation == "" && cv.Collation != "" {
			r.collation = cv.Collation
		}
	}
	if val == nil {
		return nil
	}
	if r.best == nil {
		r.best = val
		return nil
	}
	cmp := e.ctx.CompareValuesCollate(val, r.best, r.collation)
	if (r.isMax && cmp > 0) || (!r.isMax && cmp < 0) {
		r.best = val
	}
	return nil
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

// EvalAggFuncCall evaluates an aggregate function call over the given row
// maps. Exported for the expression evaluator's function-call dispatch.
func (e *SelectEngine) EvalAggFuncCall(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error) {
	return e.evalAggFuncCall(v, rowMaps)
}
