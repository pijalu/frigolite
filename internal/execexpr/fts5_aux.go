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
// highlight(), snippet() and fts5_get_locale(), plus the fts5 test-support
// family — fts5_aux.c's SQLITE_TEST-only fts5_test_* functions (the
// fts5_aux_test_functions of ext/fts5/test/fts5_common.tcl) and the ad-hoc
// create_function registrations of fts5aux.test (inst/colsize/totalsize/
// prevrowid/phrasequery/my_rowid/my_phrasesize/firstcol/fts5_hitcount).
// SQLite binds them through the fts5 module's xFindFunction with the scanned
// table's hidden column as the first argument; the Go engine resolves the
// first argument to a registered fts5 table and evaluates against the
// statement's prepared query (FTS5AuxContext). A first argument that is not
// an fts5 table reference reproduces sqlite3_overload_function's placeholder
// error ("unable to use function %s in the requested context"); a first
// argument that is an ordinary column of the scanned table reproduces the
// raw cursor-id protocol failure ("no such cursor: %lld").

// fts5AuxFuncs reports whether name is an fts5 auxiliary function (the
// bm25/highlight/snippet family or the test-support family).
func fts5AuxFuncs(name string) bool {
	_, ok := fts5AuxDispatch[name]
	return ok
}

// fts5AuxDispatch is the dispatched function family. Every entry shares the
// calling convention: the first argument resolves the fts5 table (the
// hidden-column reference), the row supplies the docid, and the statement's
// AuxQuery carries the prepared query.
var fts5AuxDispatch = map[string]bool{
	"bm25":                      true,
	"highlight":                 true,
	"snippet":                   true,
	"matchinfo":                 true,
	"fts5_get_locale":           true,
	"inst":                      true,
	"colsize":                   true,
	"totalsize":                 true,
	"fts5_test_columnsize":      true,
	"fts5_test_columntext":      true,
	"fts5_test_columnlocale":    true,
	"fts5_test_columntotalsize": true,
	"fts5_test_poslist":         true,
	"fts5_test_poslist2":        true,
	"fts5_test_collist":         true,
	"fts5_test_insttoken":       true,
	"fts5_test_tokenize":        true,
	"fts5_test_rowcount":        true,
	"fts5_test_rowid":           true,
	"fts5_test_all":             true,
	"fts5_test_queryphrase":     true,
	"fts5_test_phrasecount":     true,
	"fts5_columntext":           true,
	"fts5_columnlocale":         true,
	"fts5_queryphrase":          true,
	"fts5_collist":              true,
	"fts5_hitcount":             true,
	"phrasequery":               true,
	"prevrowid":                 true,
	"prevrowid1":                true,
	"my_rowid":                  true,
	"my_phrasesize":             true,
	"firstcol":                  true,
	// "tokenize": the fts5tokenizer.test 6.x aux proc (xColumnText(0) +
	// xTokenize), riding the same fts5_create_function aux machinery the
	// fts5_test_* family natively implements.
	"tokenize": true,
}

// IsFTS5AuxFunc reports whether name is an fts5 auxiliary function (the
// bm25/highlight/snippet family or the test-support family).
func IsFTS5AuxFunc(name string) bool {
	return fts5AuxFuncs(name)
}

// evalFTS5Aux dispatches one fts5 auxiliary function. handled=false lets the
// caller fall back to other registrations (the FTS3 snippet machinery).
func (ev *Evaluator) evalFTS5Aux(name string, f *sql.FuncCall, row Row) (interface{}, bool, error) {
	lower := strings.ToLower(name)
	if !fts5AuxFuncs(lower) {
		return nil, false, nil
	}
	// snippet()/matchinfo() are also FTS3/4 auxiliary functions: when the
	// statement's FTS context or the first argument names an FTS3 table,
	// the FTS3 implementation owns the call.
	if (lower == "snippet" || lower == "matchinfo") && ev.snippetBelongsToFTS3(f) {
		return nil, false, nil
	}
	unusable := fmt.Errorf("unable to use function %s in the requested context", lower)
	if len(f.Args) == 0 {
		return nil, true, unusable
	}
	// Auxiliary overloads do not apply inside aggregate arguments (C's
	// overload rewrite matches TK_COLUMN only; aggregate arguments carry
	// TK_AGG_COLUMN), so the placeholder error fires.
	if ev.ctx.AuxAggArgDepth() > 0 {
		return nil, true, unusable
	}
	ref, isRef := f.Args[0].(*sql.ColumnRef)
	if !isRef {
		// The first argument must be a column reference of the scanned fts5
		// table (fts5FindAuxFunction → aFunc[0] must be TK_COLUMN of the
		// vtab cursor); anything else (a literal, an expression) is C's
		// "unable to use function ... in the requested context".
		return nil, true, unusable
	}
	return ev.evalFTS5AuxResolved(lower, ref, isRef, f, row)
}

// evalFTS5AuxResolved resolves the aux call's target table against the
// statement's scanned fts5 table and evaluates the call.
func (ev *Evaluator) evalFTS5AuxResolved(lower string, ref *sql.ColumnRef, isRef bool, f *sql.FuncCall, row Row) (interface{}, bool, error) {
	unusable := fmt.Errorf("unable to use function %s in the requested context", lower)
	ctxTable, aq := ev.ctx.FTS5Aux()
	tableName := ctxTable
	if ref.Name != "" {
		if _, known := ev.ctx.FTS5Tables()[ref.Name]; known {
			tableName = ref.Name
		}
	}
	if tableName == "" {
		return nil, true, unusable
	}
	t5, known := ev.ctx.FTS5Tables()[tableName]
	if !known || (ctxTable != "" && !strings.EqualFold(ctxTable, tableName)) {
		return ev.fts5AuxForeignTable(lower, ref, isRef, f, row)
	}
	// A first argument that is an ORDINARY column of the scanned table is
	// not the cursor reference: C's callback reads its value as a cursor id
	// and fails with "no such cursor: <int64(value)>" (fts5_main.c
	// fts5ApiCallback; fts5aux.test 6.1/6.2).
	if isRef && t5.ColumnIndex(ref.Name) >= 0 {
		return ev.fts5AuxOrdinaryColumn(ref, f, row)
	}
	if aq == nil {
		aq = t5.NewScanAux()
	}
	if aq.IsSpecial() {
		// A special-query cursor ('*id'/'*reads'): C's fts5ApiInvoke fails
		// every aux call with "no such cursor: <hidden-column value>"
		// (fts5_main.c: ePlan==FTS5_PLAN_SPECIAL). The stored value is the
		// special query's result.
		return nil, true, fmt.Errorf("no such cursor: %d", aq.SpecialValue())
	}
	val, err := ev.evalFTS5AuxFunc(lower, t5, aq, f, row)
	return val, true, err
}

// fts5AuxForeignTable handles an aux call whose first argument names an fts5
// table that is not the one being scanned: the argument resolves in the row
// scope like any column reference (a missing hidden column fails with
// "no such column: ..."), then the overload placeholder fires.
func (ev *Evaluator) fts5AuxForeignTable(lower string, ref *sql.ColumnRef, isRef bool, f *sql.FuncCall, row Row) (interface{}, bool, error) {
	if isRef {
		if _, err := ev.evalExpr(f.Args[0], row); err != nil {
			return nil, true, err
		}
	}
	return nil, true, fmt.Errorf("unable to use function %s in the requested context", lower)
}

// fts5AuxOrdinaryColumn handles an aux call whose first argument is an
// ordinary column of the scanned table: C's callback reads its value as a
// cursor id and fails with "no such cursor: <int64(value)>" (fts5_main.c
// fts5ApiCallback; fts5aux.test 6.1/6.2).
func (ev *Evaluator) fts5AuxOrdinaryColumn(ref *sql.ColumnRef, f *sql.FuncCall, row Row) (interface{}, bool, error) {
	v, err := ev.evalExpr(f.Args[0], row)
	if err != nil {
		return nil, true, err
	}
	return nil, true, fmt.Errorf("no such cursor: %d", ToIntValue(util.UnwrapColumnValue(v)))
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
		return ev.fts5Bm25(aq, args, row)
	case "highlight":
		return ev.fts5Highlight(aq, args, row)
	case "snippet":
		return ev.fts5Snippet(aq, args, row)
	case "fts5_get_locale":
		return ev.fts5GetLocale(t5, args, row)
	}
	return ev.evalFTS5TestFunc(lower, t5, aq, args, row)
}

// fts5Bm25 evaluates bm25(weight, ...).
func (ev *Evaluator) fts5Bm25(aq *fts5.AuxQuery, args []sql.Expr, row Row) (interface{}, error) {
	weights := make([]float64, len(args))
	for i, a := range args {
		v, err := ev.evalExpr(a, row)
		if err != nil {
			return nil, err
		}
		weights[i] = fts5Double(util.UnwrapColumnValue(v))
	}
	return aq.Bm25(ev.auxRowid(row), weights), nil
}

// fts5Highlight evaluates highlight(iCol, open, close).
func (ev *Evaluator) fts5Highlight(aq *fts5.AuxQuery, args []sql.Expr, row Row) (interface{}, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("wrong number of arguments to function highlight()")
	}
	vals, err := ev.evalExprs(args, row)
	if err != nil {
		return nil, err
	}
	return aq.Highlight(ev.auxRowid(row), int(ToIntValue(vals[0])),
		valueTextOf(vals[1]), valueTextOf(vals[2]))
}

// fts5Snippet evaluates snippet(iCol, open, close, ellipses, nToken).
func (ev *Evaluator) fts5Snippet(aq *fts5.AuxQuery, args []sql.Expr, row Row) (interface{}, error) {
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
}

// fts5GetLocale evaluates fts5_get_locale(iCol): no locale= support — NULL,
// with the column-range check the C implementation performs.
func (ev *Evaluator) fts5GetLocale(t5 *fts5.Table, args []sql.Expr, row Row) (interface{}, error) {
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

// renderTclList renders a string list in TCL list form: elements containing
// spaces (or empty elements) are braced.
func renderTclList(elems []string) string {
	parts := make([]string, len(elems))
	for i, e := range elems {
		if e == "" || strings.ContainsAny(e, " \t\n") {
			parts[i] = "{" + e + "}"
		} else {
			parts[i] = e
		}
	}
	return strings.Join(parts, " ")
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
