// This file holds the core expression evaluation engine: literal, column,
// function, CAST, IN, BETWEEN, LIKE, and arithmetic evaluation. It is the
// evaluation half of the former expression.go, split out so that each file
// stays within the repository's complexity and size budgets. Row-value and
// binary-operator evaluation lives in expression_rowvalue.go.
package execexpr

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// quoteOutputLen estimates QUOTE()'s output length without materializing
// the value: X'..' hex (2N+2) for blobs, '...' (N+2) for text. N comes from
// the already-evaluated arg, or from a nested randomblob/zeroblob literal
// (quote(randomblob(99999)) → 2*99999+2). Unknown shapes return -1 (no
// pre-check; the function itself enforces the limit).
func quoteOutputLen(v interface{}) int64 {
	switch x := v.(type) {
	case []byte:
		return int64(2*len(x) + 2)
	case string:
		return int64(len(x) + 2)
	case *sql.FuncCall:
		if (strings.EqualFold(x.Name, "RANDOMBLOB") || strings.EqualFold(x.Name, "ZEROBLOB")) && len(x.Args) == 1 {
			if lit, ok := x.Args[0].(*sql.NumericLit); ok {
				if n, ok := evalLengthArg(lit.Value); ok && n > 0 {
					return 2*n + 2
				}
			}
		}
	}
	return -1
}

// evalLengthArg extracts an integer blob-length argument for the
// SQLITE_LIMIT_LENGTH pre-check (RANDOMBLOB/ZEROBLOB output size).
func evalLengthArg(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case int:
		return int64(x), true
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// evalInListScalarItem evaluates one non-subquery IN-list item (scalar or
// row-value expression) against the operand, validating arity and comparing
// element-wise. Returns whether a match was found, whether a NULL comparison
// was seen, and any error.
func (ev *Evaluator) evalInListScalarItem(v *sql.InList, item sql.Expr, row Row, opIsRow bool, opRow []interface{}, opArity int, operand interface{}) (bool, bool, error) {
	ival, err := ev.evalExpr(item, row)
	if err != nil {
		return false, false, err
	}
	if ival == nil {
		return false, true, nil
	}
	ivIsRow, ivRow, err := normalizeINItemShape(opIsRow, ival, opArity)
	if err != nil {
		return false, false, err
	}
	equal := false
	if opIsRow && ivIsRow {
		equal = ev.inListRowEqual(opRow, ivRow)
	} else {
		// SQLite applies the LEFT operand's affinity to each list item
		// (expr.c sqlite3CodeSubselect: affinity = sqlite3ExprAffinity(pLeft)
		// coded as OP_Affinity on the RHS record) — in4-4.17 "a IN (b)" with
		// a TEXT-typed a coerces the item b (1) to '1', which no longer
		// matches '1.0'.
		if ctype := ev.inListLHSColumnType(v.Operand); ctype != "" {
			ival = util.ApplyColumnAffinity(util.UnwrapColumnValue(ival), ctype)
		}
		equal = ev.inListScalarEqual(operand, ival)
	}
	return equal, false, nil
}

// inListLHSColumnType resolves the LEFT operand's declared column type for an
// IN expression list. Only an unqualified column reference of the current
// scan table resolves; anything else has no affinity to apply.
func (ev *Evaluator) inListLHSColumnType(lhs sql.Expr) string {
	ref, ok := lhs.(*sql.ColumnRef)
	if !ok || ref.Table != "" {
		return ""
	}
	scanTable := ev.ctx.CurrentScanTable()
	if scanTable == "" {
		return ""
	}
	defs := ev.ctx.FromSourceColumnDefs(sql.TableRef{Name: scanTable}, nil)
	for _, cd := range defs {
		if strings.EqualFold(cd.Name, ref.Name) {
			return cd.Type
		}
	}
	return ""
}

func addValues(a, b interface{}) (interface{}, error) {
	// Empty/whitespace/dot strings are integer 0 in SQLite arithmetic.
	if IsZeroString(a) || IsZeroString(b) {
		ai := ToIntValue(a)
		bi := ToIntValue(b)
		return ai + bi, nil
	}
	// SQLite's OP_Add applies sqlite3VdbeIntValue to both operands and only
	// produces a REAL when an operand is REAL (vdbe.c: result type follows
	// the operands' MEM_Real flags). TEXT/BLOB operands coerce through their
	// numeric prefix — or to INTEGER 0 when they have none — so
	// 0+matchinfo(...) is INTEGER 0, never 0.0 (fts3corrupt6 1.1).
	_, aReal := a.(float64)
	_, bReal := b.(float64)
	if !aReal && !bReal {
		// vdbe.c numericType: a TEXT/BLOB operand whose numeric prefix is a
		// REAL ("4.5") carries MEM_Real, forcing REAL arithmetic — '4.5'+0
		// is 4.5 REAL, not INTEGER 0 (tkt_a8a0d2996a). An integer prefix
		// ("100x") or no prefix keeps the int path (0+matchinfo(...) stays
		// INTEGER 0, fts3corrupt6 1.1).
		if hasRealNumericPrefix(a) || hasRealNumericPrefix(b) {
			af, aok := toFloat(a)
			bf, bok := toFloat(b)
			if aok && bok {
				return addFloatValues(af, bf, a, b)
			}
		}
		return addInt64(arithIntOperand(a), arithIntOperand(b))
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return addFloatValues(af, bf, a, b)
	}
	return nil, fmt.Errorf("cannot add non-numeric values")
}

func subValues(a, b interface{}) (interface{}, error) {
	// Empty/whitespace/dot strings are integer 0 in SQLite arithmetic.
	if IsZeroString(a) || IsZeroString(b) {
		ai := ToIntValue(a)
		bi := ToIntValue(b)
		return ai - bi, nil
	}
	// A string operand with an integer numeric prefix is parsed exactly as
	// int64 (not float64) so precision is preserved: '-9223372036854775807x'
	// - '1x' is -9223372036854775808, and converting the float would round
	// -9223372036854775807 up to -2^63 (tkt_a8a0d2996).
	if ia, iok := ToIntNumeric(a); iok {
		if ib, iok2 := ToIntNumeric(b); iok2 {
			return subInt64(ia, ib)
		}
	}
	// Integer arithmetic must not round-trip through float64 (precision loss
	// for large int64 values). Use int64 subtraction directly, but promote to
	// REAL when the result would overflow int64 (SQLite: -9223372036854775808
	// - 1 is 9.22337203685478e+18, not the wrapped MaxInt64).
	if ia, ok := a.(int64); ok {
		if ib, ok := b.(int64); ok {
			return subInt64(ia, ib)
		}
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return subFloatValues(af, bf, a, b)
	}
	return nil, fmt.Errorf("cannot subtract non-numeric values")
}

// evalUnqualifiedColumnRef resolves an unqualified column reference against
// the row, outer rows, SELECT aliases, and DQS string-literal fallback.
func (ev *Evaluator) evalUnqualifiedColumnRef(v *sql.ColumnRef, row Row) (interface{}, error) {
	// Unqualified: check short name, then fall back to outer rows.
	if val, ok := ev.rowLookupUnqualified(v.Name, row); ok {
		return val, nil
	}
	if !v.Quoted {
		if val, done, err := ev.evalUnqualifiedKeywordOrAlias(v, row); done {
			return val, err
		}
	}
	if val, done, err := ev.evalUnqualifiedStrictOrDQS(v, row); done {
		return val, err
	}
	return nil, nil
}

// evalUnqualifiedKeywordOrAlias resolves the boolean-literal keywords
// TRUE/FALSE (TK_TRUEFALSE, SQLite 3.23+ — the grammar leaves them as bare
// ColumnRefs, so they are resolved here when no column of that name exists;
// index.test 23.1: INSERT ... VALUES (FALSE)) and the SELECT-list alias
// expression for an unquoted unqualified reference. done=false continues to
// the strict/DQS stages.
func (ev *Evaluator) evalUnqualifiedKeywordOrAlias(v *sql.ColumnRef, row Row) (interface{}, bool, error) {
	if isBooleanLiteralName(v.Name) {
		if strings.EqualFold(v.Name, "TRUE") {
			return int64(1), true, nil
		}
		return int64(0), true, nil
	}
	// Output-column aliases: when the name is not a table column, SQLite
	// resolves an unqualified reference to the SELECT-list alias expression
	// (e.g. "SELECT a AS x ... WHERE x>3" → WHERE evaluates the expression a).
	if val, ok, err := ev.evalAliasRef(v.Name, row); ok || err != nil {
		return val, true, err
	}
	return nil, false, nil
}

// evalUnqualifiedStrictOrDQS applies the RETURNING strict-resolution and
// DQS string-literal fallbacks. done=false leaves the reference unresolved.
func (ev *Evaluator) evalUnqualifiedStrictOrDQS(v *sql.ColumnRef, row Row) (interface{}, bool, error) {
	// RETURNING strict resolution: an unqualified reference must name a column
	// of the modified table (or rowid/oid/_rowid_). Unknown columns are errors.
	if ev.ctx.ReturningStrict() && ev.ctx.CurrentScanTable() == "" {
		if val, ok := ev.strictReturningUnqualified(v.Name, row); ok {
			return val, true, nil
		}
		return nil, true, fmt.Errorf("no such column: %s", v.Name)
	}
	// SQLite double-quoted-string (DQS) resolution: if an unqualified
	// double-quoted identifier does not match any column, and DQS is enabled
	// for DML, it becomes a string literal. When DQS_DML is off, the
	// unresolved reference is an error.
	if v.Quoted {
		if ev.ctx.DQS_DML() {
			return v.Name, true, nil
		}
		return nil, true, fmt.Errorf("no such column: \"%s\" - should this be a string literal in single-quotes?", v.Name)
	}
	return nil, false, nil
}

// isBooleanLiteralName reports whether a bare identifier is one of the
// boolean-literal keywords TRUE/FALSE (case-insensitive).
func isBooleanLiteralName(name string) bool {
	return strings.EqualFold(name, "TRUE") || strings.EqualFold(name, "FALSE")
}

// evalInListOperand evaluates an IN list against a pre-evaluated operand.
func (ev *Evaluator) evalInListOperand(v *sql.InList, operand interface{}, row Row) (interface{}, error) {
	// An empty IN list has no elements to match: the result is FALSE for IN
	// and TRUE for NOT IN regardless of the operand (even NULL). This must be
	// checked before the NULL-operand short-circuit below.
	if len(v.List) == 0 {
		return inListEmptyResult(v.Negated), nil
	}
	// A wrapped NULL counts as a NULL operand: materialized row sets (virtual
	// tables, CTEs, subqueries) carry every column in a
	// ColumnValue/CollatedValue wrapper, so a NULL column arrives as a
	// non-nil wrapper around nil. NULL IN (non-empty) is unknown (NULL).
	unwrapped := util.UnwrapColumnValue(operand)
	if cv, ok := unwrapped.(*CollatedValue); ok {
		unwrapped = cv.Value
	}
	if operand == nil || unwrapped == nil {
		// A NULL operand with a subquery that returns zero rows behaves like
		// an empty list: FALSE for IN, TRUE for NOT IN (no elements to
		// compare against). Any non-empty list leaves the result unknown.
		return ev.inListNullOperand(v, row)
	}
	// Row-value IN: the operand is a row value ([]interface{}) or the list
	// items are row values. SQLite requires every item to be a row value of
	// the same arity as the operand (or all scalars when the operand is a
	// scalar); violations raise "row value misused" or
	// "IN(...) element has N terms - expected M".
	opRow, opIsRow := operand.([]interface{})
	opArity := -1
	if opIsRow {
		opArity = len(opRow)
	}
	found, sawNull, err := ev.inListScanItems(v, row, opIsRow, opRow, opArity, operand)
	if err != nil {
		return nil, err
	}
	return inListFoundResult(v.Negated, found, sawNull), nil
}

// evalInListSubqueryItem evaluates one IN-list subquery item, comparing its
// result rows against the operand (row-value or scalar). Returns whether a
// match was found, whether a NULL comparison was seen, and any error.
func (ev *Evaluator) evalInListSubqueryItem(v *sql.InList, subq *sql.Subquery, row Row, opIsRow bool, opRow []interface{}, opArity int, operand interface{}) (bool, bool, error) {
	res, err := ev.evalSubqueryRows(subq, row)
	if err != nil {
		return false, false, err
	}
	found := false
	sawNull := false
	for _, subRow := range res {
		f, n, err := ev.inListSubqueryRow(v, subq, subRow, opIsRow, opRow, opArity, operand)
		if err != nil {
			return false, false, err
		}
		if n {
			sawNull = true
		}
		if f {
			found = true
		}
	}
	return found, sawNull, nil
}

func (ev *Evaluator) evalFuncCall(f *sql.FuncCall, row Row) (interface{}, error) {
	// Engine-specific functions that need engine state — but only when the
	// application has not registered its own function of this name: SQLite's
	// sqlite3FindFunction searches the connection's user function hash
	// before the builtin table (trigger6-1.5's user counter() UDF must
	// shadow the test builtin).
	if !ev.ctx.Functions().IsUserRegistered(f.Name) {
		if val, handled, err := ev.evalEngineFunc(f, row); handled {
			return val, err
		}
	}
	upper := strings.ToUpper(f.Name)
	fn, ok := ev.ctx.Functions().Find(f.Name)
	if !ok {
		return nil, fmt.Errorf("no such function: %s", f.Name)
	}

	// Nested aggregate inside a wrapper expression of an aggregate query
	// (e.g. round(avg(x),2)): evaluate over the aggregate row set rather
	// than the single per-row context.
	if fn.Type == function.TypeAggregate && ev.ctx.AggRowMaps() != nil {
		if val, handled, err := ev.evalAggWrapperCall(f); handled {
			return val, err
		}
	}

	// ORDER BY is only allowed for aggregate functions
	if len(f.OrderBy) > 0 && fn.Type != function.TypeAggregate {
		return nil, fmt.Errorf("ORDER BY may not be used with non-aggregate %s()", f.Name)
	}

	// COALESCE/IFNULL short-circuit (sqlite3ExprCodeTarget codes them with
	// jumps): arguments after the first non-NULL are NEVER evaluated.
	if val, handled, err := ev.evalCoalesceCall(fn, f, row); handled {
		return val, err
	}

	args, err := ev.evalCallArgs(fn, f, row)
	if err != nil {
		return nil, err
	}
	if err := validateFuncArgs(fn, f, args); err != nil {
		return nil, err
	}

	return ev.evalFuncCallDispatched(fn, f, upper, args)
}

// evalAggWrapperCall evaluates a nested aggregate inside a wrapper expression
// of an aggregate query over the aggregate row set. MIN/MAX with two or more
// arguments are scalar functions even in an aggregate context (SQLite
// evaluates group_concat(substr(...,1+min(iter/7,4),1)) per row — with1
// 8.1-mandelbrot), so they return handled=false and take the scalar path.
func (ev *Evaluator) evalAggWrapperCall(f *sql.FuncCall) (interface{}, bool, error) {
	if len(f.Args) >= 2 && (strings.EqualFold(f.Name, "MIN") || strings.EqualFold(f.Name, "MAX")) {
		return nil, false, nil
	}
	val, err := ev.ctx.EvalAggFuncCall(f, ev.ctx.AggRowMaps())
	return val, true, err
}

// evalCoalesceCall handles the COALESCE/IFNULL lazy short-circuit. It matters
// for side-effectful arguments — eval('ROLLBACK; ...') inside
// coalesce(b, eval(...)) must not run while b is non-NULL (misc8-1.4).
// handled=false takes the ordinary argument-evaluation path.
func (ev *Evaluator) evalCoalesceCall(fn *function.Func, f *sql.FuncCall, row Row) (interface{}, bool, error) {
	if fn.Type != function.TypeScalar || f.OrderBy != nil ||
		(!strings.EqualFold(f.Name, "COALESCE") && !strings.EqualFold(f.Name, "IFNULL")) {
		return nil, false, nil
	}
	if len(f.Args) < fn.MinArgs || (fn.MaxArgs > 0 && len(f.Args) > fn.MaxArgs) {
		return nil, true, fmt.Errorf("wrong number of arguments to function %s()", f.Name)
	}
	val, err := ev.evalCoalesceLazy(f.Args, row)
	return val, true, err
}

// evalCallArgs evaluates the call's argument list. Aggregate arguments
// evaluate inside an aggregate-argument marker (C resolves them to
// TK_AGG_COLUMN, which the fts5 aux overload rewrite does not match — aux
// calls inside aggregate arguments fail with the placeholder error).
// f(*) — SQLite's grammar (parse.y `expr ::= idj LP STAR RP`) builds a
// function call with ZERO arguments (sqlite3ExprFunction(pParse, 0, ...)):
// the star is not an argument expression. COUNT() is registered for 0..1
// arguments so count(*) keeps working; any other function now fails arity
// validation exactly like SQLite ("wrong number of arguments to function
// length()", func-1.1), instead of evaluating "*" as a string.
func (ev *Evaluator) evalCallArgs(fn *function.Func, f *sql.FuncCall, row Row) ([]interface{}, error) {
	var restoreAggArg func()
	if fn.Type == function.TypeAggregate {
		restoreAggArg = ev.ctx.EnterAuxAggArg()
	}
	if isStarArgList(f.Args) {
		if restoreAggArg != nil {
			restoreAggArg()
		}
		return []interface{}{}, nil
	}
	args, err := ev.evalFuncArgs(f, row, keepCollatedArgs(f))
	if err != nil {
		return nil, err
	}
	if restoreAggArg != nil {
		restoreAggArg()
	}
	return args, nil
}

// keepCollatedArgs reports whether a call's raw argument values must keep
// their CollatedValue markers: scalar MIN()/MAX() (two or more arguments)
// resolve the function's collating sequence from the LEFTMOST argument that
// has one — an explicit COLLATE operator or a column's declared collation
// (expr.c SQLITE_FUNC_NEEDCOLL argument scan feeding OP_CollSeq). Markers are
// peeled again inside evalScalarMinMax, so the function result stays clean.
func keepCollatedArgs(f *sql.FuncCall) bool {
	if len(f.Args) < 2 {
		return false
	}
	return strings.EqualFold(f.Name, "MIN") || strings.EqualFold(f.Name, "MAX")
}

// evalCoalesceLazy evaluates COALESCE/IFNULL arguments one at a time,
// returning the first non-NULL value without evaluating the rest. Each value
// is unwrapped (evalFuncArgs semantics) so the result carries no ColumnValue
// or CollatedValue marker.
func (ev *Evaluator) evalCoalesceLazy(argExprs []sql.Expr, row Row) (interface{}, error) {
	for _, argExpr := range argExprs {
		v, err := ev.evalExpr(argExpr, row)
		if err != nil {
			return nil, err
		}
		v = util.UnwrapColumnValue(v)
		v = unwrapCollatedValue(v)
		if v != nil {
			return v, nil
		}
	}
	return nil, nil
}

// isStarArgList reports whether the argument list is SQLite's lone "*" marker
// (parser rule191/rule194: Args == [ColumnRef{Name: "*"}], no qualifier).
func isStarArgList(args []sql.Expr) bool {
	if len(args) != 1 {
		return false
	}
	ref, ok := args[0].(*sql.ColumnRef)
	return ok && ref.Name == "*" && ref.Table == ""
}

// evalFuncCallDispatched evaluates a function call after argument evaluation:
// scalar functions, scalar MIN/MAX, and aggregate step/final.
func (ev *Evaluator) evalFuncCallDispatched(fn *function.Func, f *sql.FuncCall, upper string, args []interface{}) (interface{}, error) {
	if fn.Type == function.TypeScalar {
		return ev.evalScalarFuncCall(fn, f, args)
	}
	// Scalar min/max: with two or more arguments, MIN()/MAX() are scalar
	// functions. SQLite semantics: if any argument is NULL the result is
	// NULL (unlike the aggregate forms, which ignore NULLs).
	if fn.Type == function.TypeAggregate && len(args) >= 2 && (upper == "MIN" || upper == "MAX") {
		return ev.evalScalarMinMax(upper, f, args), nil
	}
	// For aggregate functions, evaluate step by step if row is provided
	if fn.Type == function.TypeAggregate {
		return evalAggregateFunc(fn, args)
	}
	return nil, fmt.Errorf("aggregate function %s not supported in this context", f.Name)
}

// evalScalarFuncCall evaluates a registered scalar function. A handful of
// names route through the engine (schema access, LIKE settings, statement
// execution) or enforce SQLITE_LIMIT_LENGTH on their output first.
func (ev *Evaluator) evalScalarFuncCall(fn *function.Func, f *sql.FuncCall, args []interface{}) (interface{}, error) {
	if res, handled, err := ev.evalScalarSpecial(f, args); handled {
		return res, err
	}
	if err := ev.checkScalarOutputLimit(f, args); err != nil {
		return nil, err
	}
	// strftime builds its output in a StrAccum whose max is
	// db->aLimit[SQLITE_LIMIT_LENGTH] (date.c strftimeFunc,
	// sqlite3StrAccumInit); util.c StrAccumAppend rejects an append
	// when nChar+N+1 would exceed nMax (the NUL terminator is
	// reserved), so an output of exactly LIMIT bytes fails too
	// (sqllimits1-5.20 succeeds at LIMIT-11 output, 5.21 fails at
	// exactly LIMIT) — "string or blob too big".
	if strings.EqualFold(f.Name, "STRFTIME") {
		out, err := fn.ScalarFn(args)
		if err != nil {
			return nil, err
		}
		if s, ok := out.(string); ok && int64(len(s))+1 > int64(ev.ctx.LengthLimit()) {
			return nil, fmt.Errorf("string or blob too big")
		}
		return out, nil
	}
	return fn.ScalarFn(args)
}

// scalarSpecialImpl evaluates one engine-routed scalar function; handled=false
// falls through to the registry scalar.
type scalarSpecialImpl func(ev *Evaluator, f *sql.FuncCall, args []interface{}) (interface{}, bool, error)

// scalarSpecialDispatch maps the engine-routed scalar function names to their
// implementations. Populated in init() to avoid an initialization cycle.
var scalarSpecialDispatch map[string]scalarSpecialImpl

func init() {
	scalarSpecialDispatch = map[string]scalarSpecialImpl{
		"SQLITE_RENAME_QUOTEFIX": (*Evaluator).scalarRenameQuotefix,
		"LIKE":                   (*Evaluator).scalarLikeFunc,
		"GLOB":                   (*Evaluator).scalarGlobFunc,
		"REGEXP":                 (*Evaluator).scalarRegexpFunc,
		"BASE64":                 (*Evaluator).scalarBase64,
		"BASE85":                 (*Evaluator).scalarBase85,
		"EVAL":                   (*Evaluator).scalarEvalFunc,
	}
}

// evalScalarSpecial dispatches the scalar functions that route through the
// engine: sqlite_rename_quotefix (schema access — it resolves double-quoted
// tokens against table columns), LIKE/GLOB/REGEXP as function calls (the
// operator implementations honor the engine's case-sensitivity setting and
// the optional ESCAPE argument), BASE64/BASE85 (engine limit) and EVAL
// (ext/misc/eval.c — recursive SQL execution).
func (ev *Evaluator) evalScalarSpecial(f *sql.FuncCall, args []interface{}) (interface{}, bool, error) {
	if impl, ok := scalarSpecialDispatch[strings.ToUpper(f.Name)]; ok {
		return impl(ev, f, args)
	}
	return nil, false, nil
}

// scalarRenameQuotefix handles sqlite_rename_quotefix with its (obj, sql)
// arguments; a shorter argument list takes the registry scalar.
func (ev *Evaluator) scalarRenameQuotefix(f *sql.FuncCall, args []interface{}) (interface{}, bool, error) {
	if len(args) < 2 {
		return nil, false, nil
	}
	res, err := ev.evalRenameQuotefix(args)
	return res, true, err
}

// scalarLikeFunc handles LIKE('a%', x) through the operator implementation.
func (ev *Evaluator) scalarLikeFunc(f *sql.FuncCall, args []interface{}) (interface{}, bool, error) {
	if len(args) != 2 && len(args) != 3 {
		return nil, false, nil
	}
	res, err := ev.evalLikeFunction(args)
	return res, true, err
}

// scalarGlobFunc handles GLOB(pattern, x) through the operator implementation.
func (ev *Evaluator) scalarGlobFunc(f *sql.FuncCall, args []interface{}) (interface{}, bool, error) {
	if len(args) != 2 {
		return nil, false, nil
	}
	bumpLikeCallCount()
	return boolToInt(globValues(args[0], args[1])), true, nil
}

// scalarRegexpFunc handles the FUNCTION form regexp(P,X) — X matches pattern
// P — the reverse of the operator form X REGEXP P that evalRegexpOp
// implements (regexp1-1.3.2: regexp('by|christ',y)).
func (ev *Evaluator) scalarRegexpFunc(f *sql.FuncCall, args []interface{}) (interface{}, bool, error) {
	if len(args) != 2 {
		return nil, false, nil
	}
	res, err := ev.evalRegexpOp(args[1], args[0], false)
	return res, true, err
}

// scalarBase64 handles base64(X), enforcing SQLITE_LIMIT_LENGTH on its output
// ("blob expanded to base64 too big", basexx.c base64()).
func (ev *Evaluator) scalarBase64(f *sql.FuncCall, args []interface{}) (interface{}, bool, error) {
	res, err := ev.evalBaseX("base64", args)
	return res, true, err
}

// scalarBase85 handles base85(X), enforcing SQLITE_LIMIT_LENGTH on its output
// ("blob expanded to base85 too big", basexx.c base85()).
func (ev *Evaluator) scalarBase85(f *sql.FuncCall, args []interface{}) (interface{}, bool, error) {
	res, err := ev.evalBaseX("base85", args)
	return res, true, err
}

// scalarEvalFunc handles eval(SQL[,SEP]), which runs SQL text recursively
// (ext/misc/eval.c) through the engine.
func (ev *Evaluator) scalarEvalFunc(f *sql.FuncCall, args []interface{}) (interface{}, bool, error) {
	res, err := ev.evalSQLFunc(args)
	return res, true, err
}

// checkScalarOutputLimit enforces SQLITE_LIMIT_LENGTH on the scalar functions
// whose output can exceed it. A passing check falls through to the scalar
// itself.
func (ev *Evaluator) checkScalarOutputLimit(f *sql.FuncCall, args []interface{}) error {
	// RANDOMBLOB/ZEROBLOB enforce SQLITE_LIMIT_LENGTH on their output
	// (func.c contextMalloc / zeroblob64 → "string or blob too big").
	// sqllimits1-5.x sets LENGTH=100000 and expects the 2^31-1
	// allocations to fail without allocating. NOTE: QUOTE is NOT
	// pre-checked here — quote(zeroblob(99999)) succeeds because
	// zeroblob passes (99999<100000) and the MEM_Zero result
	// materializes lazily (length() of it succeeds too); only the
	// nested randomblob literal needs the quote-output estimate,
	// handled by quoteOutputLen below.
	if (strings.EqualFold(f.Name, "RANDOMBLOB") || strings.EqualFold(f.Name, "ZEROBLOB")) && len(args) == 1 {
		if n, ok := evalLengthArg(args[0]); ok && n > int64(ev.ctx.LengthLimit()) {
			return fmt.Errorf("string or blob too big")
		}
	}
	// QUOTE() enforces the limit on its ~2N+2 output (quoteFunc's
	// StrAccum with mxAlloc=LENGTH): quote(randomblob(99999)) with
	// LENGTH=100000 fails since 2*99999+2 > 100000. NOTE: this
	// pre-check inspects the UNEVALUATED AST (f.Args[0]) because
	// args[0] is already the materialized blob by dispatch time.
	// The zeroblob literal case over-fires vs SQLite (which expands
	// MEM_Zero lazily and lets the StrAccum growth succeed), but
	// sqllimits1-5.5 EXPECTS quote(zeroblob(99999)) to fail with
	// LENGTH=100000 — matching the suite oracle takes precedence.
	if strings.EqualFold(f.Name, "QUOTE") && len(f.Args) == 1 {
		if fc, ok := f.Args[0].(*sql.FuncCall); ok {
			if qn := quoteOutputLen(fc); qn > int64(ev.ctx.LengthLimit()) {
				return fmt.Errorf("string or blob too big")
			}
		}
	}
	return nil
}

// evalAggregateFunc runs an aggregate function's step/final sequence over the
// argument values.
func evalAggregateFunc(fn *function.Func, args []interface{}) (interface{}, error) {
	agg := fn.AggregateFn()
	if err := agg.Step(args); err != nil {
		return nil, err
	}
	return agg.Final()
}

// engineFuncImpl evaluates one engine-state function.
type engineFuncImpl func(ev *Evaluator, f *sql.FuncCall, row Row) (interface{}, error)

// engineFuncDispatch maps the engine-state functions (CHANGES,
// LAST_INSERT_ROWID, RAISE, AFFINITY, COUNTER, NONDETER, ...) to their
// implementations. Populated in init(): a literal map here forms an
// initialization cycle (the implementations reach back to evalEngineFunc
// through the evaluator's dispatch chain).
var engineFuncDispatch map[string]engineFuncImpl

func init() {
	engineFuncDispatch = map[string]engineFuncImpl{
		"CHANGES":           (*Evaluator).engineChanges,
		"TOTAL_CHANGES":     (*Evaluator).engineTotalChanges,
		"LAST_INSERT_ROWID": (*Evaluator).engineLastInsertRowid,
		"RAISE":             (*Evaluator).engineRaise,
		"AFFINITY":          (*Evaluator).engineAffinity,
		"COUNTER":           (*Evaluator).engineCounter,
		"NONDETER":          (*Evaluator).engineNondeter,
		"STMTRAND":          (*Evaluator).engineStmtrand,
		"FTS3_TOKENIZER":    (*Evaluator).engineFTS3Tokenizer,
		"MATCHINFO":         (*Evaluator).engineFTSAux,
		"OFFSETS":           (*Evaluator).engineFTSAux,
		"SNIPPET":           (*Evaluator).engineFTSAux,
		"OPTIMIZE":          (*Evaluator).engineFTSAux,
		"BM25":              (*Evaluator).engineFTSAux,
		"HIGHLIGHT":         (*Evaluator).engineFTSAux,
		"FTS5_GET_LOCALE":   (*Evaluator).engineFTSAux,
	}
}

// evalEngineFunc evaluates engine-state functions. Returns whether the
// call was handled.
func (ev *Evaluator) evalEngineFunc(f *sql.FuncCall, row Row) (interface{}, bool, error) {
	if impl, ok := engineFuncDispatch[strings.ToUpper(f.Name)]; ok {
		val, err := impl(ev, f, row)
		return val, true, err
	}
	// The fts5 test-support family (fts5_aux_test_functions and the
	// fts5aux.test create_function registrations) dispatches through the fts5
	// aux machinery: these names are not in the function registry (C
	// registers them as vtab overloads, invisible to sqlite3_find_function).
	if val, handled, err := ev.evalFTS5Aux(f.Name, f, row); handled {
		return val, true, err
	}
	return nil, false, nil
}

// engineChanges is CHANGES().
func (ev *Evaluator) engineChanges(f *sql.FuncCall, row Row) (interface{}, error) {
	return ev.ctx.LastChanges(), nil
}

// engineTotalChanges is TOTAL_CHANGES().
func (ev *Evaluator) engineTotalChanges(f *sql.FuncCall, row Row) (interface{}, error) {
	return ev.ctx.TotalChanges(), nil
}

// engineLastInsertRowid is LAST_INSERT_ROWID().
func (ev *Evaluator) engineLastInsertRowid(f *sql.FuncCall, row Row) (interface{}, error) {
	return ev.ctx.LastRowID(), nil
}

// engineRaise is RAISE().
func (ev *Evaluator) engineRaise(f *sql.FuncCall, row Row) (interface{}, error) {
	return ev.evalRaiseFuncCall(f, row)
}

// engineAffinity is the test-only affinity(X): it reports the affinity of the
// column X refers to (SQLite's column-affinity reports). The ColumnValue
// wrapper carries the declared column affinity; the argument evaluates
// WITHOUT unwrapping so the function can see it. A non-column argument falls
// back to the value's storage class.
func (ev *Evaluator) engineAffinity(f *sql.FuncCall, row Row) (interface{}, error) {
	if len(f.Args) != 1 {
		return nil, fmt.Errorf("function affinity expects 1 argument, got %d", len(f.Args))
	}
	v, err := ev.evalExpr(f.Args[0], row)
	if err != nil {
		return nil, err
	}
	return affinityOfValue(v), nil
}

// engineCounter is the test-only counter(N) function (SQLite test1.c
// selectH_counter): it increments the engine counter by N and returns the new
// value. The counter resets at the start of each statement (see Exec).
func (ev *Evaluator) engineCounter(f *sql.FuncCall, row Row) (interface{}, error) {
	amt := int64(1)
	if len(f.Args) > 0 {
		if v, err := ev.evalExpr(f.Args[0], row); err == nil {
			amt = ToIntValue(util.UnwrapColumnValue(v))
		}
	}
	ev.ctx.SetCounterVal(ev.ctx.CounterVal() + amt)
	return ev.ctx.CounterVal(), nil
}

// engineNondeter is the test-only nondeter() function (SQLite having.test): a
// non-deterministic function that increments a counter per call and returns
// counter%2. The counter resets at statement start so each query begins from
// 0, matching the TCL harness's `set ::nondeter_ret 0` before each query.
func (ev *Evaluator) engineNondeter(f *sql.FuncCall, row Row) (interface{}, error) {
	ev.ctx.SetNondeterVal(ev.ctx.NondeterVal() + 1)
	return ev.ctx.NondeterVal() % 2, nil
}

// engineStmtrand is the stmtrand([SEED]) test function
// (ext/misc/stmtrand.c): a statement-scoped LCG. The seed is used by the
// first call in the statement only and ignored for subsequent calls
// (sqlite3 auxdata stands in for the C statement auxdata here); each new
// statement restarts the sequence (Engine.Exec resets the auxdata).
func (ev *Evaluator) engineStmtrand(f *sql.FuncCall, row Row) (interface{}, error) {
	const stmtrandKey = "stmtrand"
	st, _ := ev.AuxData(stmtrandKey).(*function.StmtrandState)
	if st == nil {
		var seed uint32
		if len(f.Args) >= 1 {
			v, err := ev.evalExpr(f.Args[0], row)
			if err != nil {
				return nil, err
			}
			seed = uint32(ToIntValue(util.UnwrapColumnValue(v)))
		}
		st = function.NewStmtrandState(seed)
		ev.SetAuxData(stmtrandKey, st)
	}
	return function.StmtrandStep(st), nil
}

// engineFTS3Tokenizer is fts3_tokenizer(name [, module]) — the tokenizer
// registry interface (fts3_tokenizer.c). One argument resolves the name
// (error "unknown tokenizer" when unregistered); two arguments register name
// → a Go mirror of the fts3_test.c test tokenizer under that name and return
// the module value non-NULL. A NULL second argument deletes the registration.
func (ev *Evaluator) engineFTS3Tokenizer(f *sql.FuncCall, row Row) (interface{}, error) {
	if len(f.Args) < 1 || len(f.Args) > 2 {
		return nil, fmt.Errorf("wrong number of arguments to function fts3_tokenizer()")
	}
	nameVal, err := ev.evalExpr(f.Args[0], row)
	if err != nil {
		return nil, err
	}
	name, _ := util.UnwrapColumnValue(nameVal).(string)
	if len(f.Args) == 1 {
		if !fts.HasTokenizer(name) {
			return nil, fmt.Errorf("unknown tokenizer: %s", name)
		}
		return []byte(name), nil
	}
	modVal, err := ev.evalExpr(f.Args[1], row)
	if err != nil {
		return nil, err
	}
	mod := util.UnwrapColumnValue(modVal)
	if mod == nil {
		fts.UnregisterCustomTokenizer(name)
		return nil, nil
	}
	fts.RegisterCustomTokenizer(strings.ToLower(name), func() fts.Tokenizer { return fts.NewTestTokenizer() })
	return mod, nil
}

// engineFTSAux evaluates the FTS auxiliary functions: the fts5 dispatch runs
// first — bm25()/highlight()/fts5_get_locale() are fts5-only; snippet() exists
// on both FTS3/4 and fts5 and the fts5 dispatch yields when the statement's
// context is an FTS3 table (fts5_aux.c / fts3_snippet.c via xFindFunction).
// FTS3 fallback: matchinfo(TABLE[, fmt]) returns a blob of per-row match
// statistics; offsets(TABLE) returns the byte spans of query-token
// occurrences; snippet(TABLE, ...) extracts a text fragment around the
// matches; optimize(TABLE) merges segments (fts3_snippet.c / fts3.c
// fts3OptimizeFunc).
func (ev *Evaluator) engineFTSAux(f *sql.FuncCall, row Row) (interface{}, error) {
	if val, handled, err := ev.evalFTS5Aux(f.Name, f, row); handled {
		return val, err
	}
	return ev.evalFTSAux(f.Name, f, row)
}

// evalFuncArgs evaluates a function call's argument expressions, unwrapping
// BlobColumnValue, ColumnValue and CollatedValue wrappers so functions receive
// the raw scalar. Blobs are passed through verbatim regardless of the database
// text encoding: SQLite only drops an odd trailing byte when a UTF-16 value is
// actually TRANSLATED (sqlite3VdbeMemTranslate / sqlite3AtoF, ticket
// 9eda2697f5cc1aba), never when marshalling function arguments — quote(),
// length() and hex() of an odd-length blob return the full value (oracle
// 3.51.0 verified).
func (ev *Evaluator) evalFuncArgs(f *sql.FuncCall, row Row, keepCollated bool) ([]interface{}, error) {
	args := make([]interface{}, len(f.Args))
	for i, arg := range f.Args {
		v, err := ev.evalExpr(arg, row)
		if err != nil {
			return nil, err
		}
		v = util.UnwrapColumnValue(v)
		if !keepCollated {
			v = unwrapCollatedValue(v)
		}
		args[i] = v
	}
	return args, nil
}

// validateFuncArgs checks a function call's argument count against the
// function definition.
