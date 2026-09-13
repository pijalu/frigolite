package execexpr

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// This file dispatches the fts5 auxiliary functions (fts5_aux.c): bm25(),
// highlight(), snippet() and fts5_get_locale(). SQLite binds them through the
// fts5 module's xFindFunction with the scanned table's hidden column as the
// first argument; the Go engine resolves the first argument to a registered
// fts5 table and evaluates against the statement's prepared query
// (FTS5AuxContext). A first argument that is not an fts5 table reference
// reproduces sqlite3_overload_function's placeholder error
// ("unable to use function %s in the requested context").

// dbgFTS5Aux enables temporary dispatch tracing.
var dbgFTS5Aux = true

// fts5AuxFuncs reports whether name is an fts5 auxiliary function.
func fts5AuxFuncs(name string) bool {
	switch strings.ToLower(name) {
	case "bm25", "highlight", "snippet", "fts5_get_locale":
		return true
	}
	return false
}

// evalFTS5Aux dispatches one fts5 auxiliary function. handled=false lets the
// caller fall back to other registrations (the FTS3 snippet machinery).
func (ev *Evaluator) evalFTS5Aux(name string, f *sql.FuncCall, row Row) (interface{}, bool, error) {
	lower := strings.ToLower(name)
	if !fts5AuxFuncs(lower) {
		return nil, false, nil
	}
	if dbgFTS5Aux {
		println("evalFTS5Aux:", lower, "args:", len(f.Args))
	}
	// snippet() is also an FTS3/4 auxiliary function: when the statement's
	// FTS context or the first argument names an FTS3 table, FTS3 owns it.
	if lower == "snippet" && ev.snippetBelongsToFTS3(f) {
		if dbgFTS5Aux {
			println("-> fts3 owns snippet")
		}
		return nil, false, nil
	}
	unusable := fmt.Errorf("unable to use function %s in the requested context", lower)
	if len(f.Args) == 0 {
		return nil, true, unusable
	}
	ref, isRef := f.Args[0].(*sql.ColumnRef)
	ctxTable, aq := ev.ctx.FTS5Aux()
	tableName := ctxTable
	if isRef && ref.Name != "" {
		if _, known := ev.ctx.FTS5Tables()[ref.Name]; known {
			tableName = ref.Name
		}
	}
	if tableName == "" {
		return nil, true, unusable
	}
	t5, known := ev.ctx.FTS5Tables()[tableName]
	if !known || (ctxTable != "" && !strings.EqualFold(ctxTable, tableName)) {
		// The referenced fts5 table is not the one being scanned: the first
		// argument resolves in the row scope like any column reference (a
		// missing hidden column fails with "no such column: ..."), then the
		// overload placeholder fires.
		if isRef {
			if _, err := ev.evalExpr(f.Args[0], row); err != nil {
				return nil, true, err
			}
		}
		return nil, true, unusable
	}
	if aq == nil {
		aq = t5.NewScanAux()
	}
	println("-> dispatch to aux func:", lower, "ctxTable:", ctxTable)
	val, err := ev.evalFTS5AuxFunc(lower, t5, aq, f, row)
	println("-> aux result:", lower, val, err)
	return val, true, err
}

// snippetBelongsToFTS3 reports whether the current statement's FTS context is
// an FTS3/4 table (whose snippet() registration takes precedence).
func (ev *Evaluator) snippetBelongsToFTS3(f *sql.FuncCall) bool {
	if len(f.Args) > 0 {
		if ref, ok := f.Args[0].(*sql.ColumnRef); ok && ref.Name != "" {
			if _, isFTS3 := ev.ctx.FTSTables()[ref.Name]; isFTS3 {
				return true
			}
		}
	}
	if cur := ev.ctx.CurrentFTSMatch(); cur != "" {
		if _, isFTS3 := ev.ctx.FTSTables()[cur]; isFTS3 {
			return true
		}
	}
	return false
}

// evalFTS5AuxFunc evaluates one resolved fts5 auxiliary function call.
func (ev *Evaluator) evalFTS5AuxFunc(lower string, t5 *fts5.Table, aq *fts5.AuxQuery, f *sql.FuncCall, row Row) (interface{}, error) {
	args := f.Args[1:]
	switch lower {
	case "bm25":
		weights := make([]float64, len(args))
		for i, a := range args {
			v, err := ev.evalExpr(a, row)
			if err != nil {
				return nil, err
			}
			weights[i] = fts5Double(util.UnwrapColumnValue(v))
		}
		return aq.Bm25(ev.auxRowid(row), weights), nil
	case "highlight":
		if len(args) != 3 {
			return nil, fmt.Errorf("wrong number of arguments to function highlight()")
		}
		vals, err := ev.evalExprs(args, row)
		if err != nil {
			return nil, err
		}
		return aq.Highlight(ev.auxRowid(row), int(ToIntValue(vals[0])),
			valueTextOf(vals[1]), valueTextOf(vals[2]))
	case "snippet":
		if len(args) != 5 {
			return nil, fmt.Errorf("wrong number of arguments to function snippet()")
		}
		vals, err := ev.evalExprs(args, row)
		if err != nil {
			return nil, err
		}
		return aq.Snippet(ev.auxRowid(row), int(ToIntValue(vals[0])),
			valueTextOf(vals[1]), valueTextOf(vals[2]), valueTextOf(vals[3]),
			int(ToIntValue(vals[4])))
	case "fts5_get_locale":
		if len(args) != 1 {
			return nil, fmt.Errorf("wrong number of arguments to function fts5_get_locale()")
		}
		v, err := ev.evalExpr(args[0], row)
		if err != nil {
			return nil, err
		}
		if !fts5IsIntLike(v) {
			return nil, fmt.Errorf("non-integer argument passed to function fts5_get_locale()")
		}
		iCol := int(ToIntValue(util.UnwrapColumnValue(v)))
		if iCol < 0 || iCol >= len(t5.ColumnNames()) {
			return nil, &fts5.ColumnRangeError{}
		}
		return nil, nil // no locale= support: NULL
	}
	return nil, fmt.Errorf("unable to use function %s in the requested context", lower)
}

// auxRowid resolves the current row's rowid (the docid the aux functions
// evaluate against).
func (ev *Evaluator) auxRowid(row Row) int64 {
	if row == nil {
		return 0
	}
	if rv, ok := row.Get("rowid"); ok {
		if dv, ok := util.UnwrapColumnValue(rv).(int64); ok {
			return dv
		}
	}
	return 0
}

// evalExprs evaluates an argument list.
func (ev *Evaluator) evalExprs(args []sql.Expr, row Row) ([]interface{}, error) {
	out := make([]interface{}, len(args))
	for i, a := range args {
		v, err := ev.evalExpr(a, row)
		if err != nil {
			return nil, err
		}
		out[i] = util.UnwrapColumnValue(v)
	}
	return out, nil
}

// valueTextOf coerces an evaluated value to text.
func valueTextOf(v interface{}) string {
	if v == nil {
		return ""
	}
	return function.ValueText(v)
}

// fts5Double coerces a value to double (sqlite3_value_double).
func fts5Double(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(strings.TrimSpace(string(x)), 64)
		return f
	}
	return 0
}

// fts5IsIntLike reports whether the value is integer-typed or coercible
// without loss.
func fts5IsIntLike(v interface{}) bool {
	switch x := util.UnwrapColumnValue(v).(type) {
	case int64, int:
		return true
	case float64:
		return x == float64(int64(x))
	case string:
		_, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return err == nil
	}
	return false
}
