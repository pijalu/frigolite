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
		equal = ev.inListScalarEqual(operand, ival)
	}
	return equal, false, nil
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
		return addInt64(arithIntOperand(a), arithIntOperand(b))
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return addFloatValues(af, bf, a, b)
	}
	return nil, fmt.Errorf("cannot add non-numeric values")
}

// arithIntOperand coerces one arithmetic operand to int64 the way
// sqlite3VdbeIntValue does: numbers pass through, TEXT/BLOB contribute
// their leading numeric prefix (or 0 when there is none).
func arithIntOperand(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	if n, ok := ToIntNumeric(v); ok {
		return n
	}
	return 0
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
	// Output-column aliases: when the name is not a table column, SQLite
	// resolves an unqualified reference to the SELECT-list alias expression
	// (e.g. "SELECT a AS x ... WHERE x>3" → WHERE evaluates the expression a).
	if !v.Quoted {
		if val, ok, err := ev.evalAliasRef(v.Name, row); ok || err != nil {
			return val, err
		}
	}
	// RETURNING strict resolution: an unqualified reference must name a column
	// of the modified table (or rowid/oid/_rowid_). Unknown columns are errors.
	if ev.ctx.ReturningStrict() && ev.ctx.CurrentScanTable() == "" {
		if val, ok := ev.strictReturningUnqualified(v.Name, row); ok {
			return val, nil
		}
		return nil, fmt.Errorf("no such column: %s", v.Name)
	}
	// SQLite double-quoted-string (DQS) resolution: if an unqualified
	// double-quoted identifier does not match any column, and DQS is enabled
	// for DML, it becomes a string literal. When DQS_DML is off, the
	// unresolved reference is an error.
	if v.Quoted {
		if ev.ctx.DQS_DML() {
			return v.Name, nil
		}
		return nil, fmt.Errorf("no such column: \"%s\" - should this be a string literal in single-quotes?", v.Name)
	}
	return nil, nil
}

// evalInListOperand evaluates an IN list against a pre-evaluated operand.
func (ev *Evaluator) evalInListOperand(v *sql.InList, operand interface{}, row Row) (interface{}, error) {
	// An empty IN list has no elements to match: the result is FALSE for IN
	// and TRUE for NOT IN regardless of the operand (even NULL). This must be
	// checked before the NULL-operand short-circuit below.
	if len(v.List) == 0 {
		return inListEmptyResult(v.Negated), nil
	}
	if operand == nil {
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
	// Engine-specific functions that need engine state
	if val, handled, err := ev.evalEngineFunc(f, row); handled {
		return val, err
	}
	upper := strings.ToUpper(f.Name)
	fn, ok := ev.ctx.Functions().Find(f.Name)
	if !ok {
		return nil, fmt.Errorf("no such function: %s", f.Name)
	}

	// Nested aggregate inside a wrapper expression of an aggregate query
	// (e.g. round(avg(x),2)): evaluate over the aggregate row set rather
	// than the single per-row context. MIN/MAX with two or more arguments
	// are scalar functions even in an aggregate context (SQLite evaluates
	// group_concat(substr(...,1+min(iter/7,4),1)) per row — with1
	// 8.1-mandelbrot), so they take the scalar path below.
	if fn.Type == function.TypeAggregate && ev.ctx.AggRowMaps() != nil {
		if !(len(f.Args) >= 2 && (strings.EqualFold(f.Name, "MIN") || strings.EqualFold(f.Name, "MAX"))) {
			return ev.ctx.EvalAggFuncCall(f, ev.ctx.AggRowMaps())
		}
	}

	// ORDER BY is only allowed for aggregate functions
	if len(f.OrderBy) > 0 && fn.Type != function.TypeAggregate {
		return nil, fmt.Errorf("ORDER BY may not be used with non-aggregate %s()", f.Name)
	}

	args, err := ev.evalFuncArgs(f, row)
	if err != nil {
		return nil, err
	}
	if err := validateFuncArgs(fn, f, args); err != nil {
		return nil, err
	}

	return ev.evalFuncCallDispatched(fn, f, upper, args)
}

// evalFuncCallDispatched evaluates a function call after argument evaluation:
// scalar functions, scalar MIN/MAX, and aggregate step/final.
func (ev *Evaluator) evalFuncCallDispatched(fn *function.Func, f *sql.FuncCall, upper string, args []interface{}) (interface{}, error) {
	if fn.Type == function.TypeScalar {
		// sqlite_rename_quotefix needs schema access (it resolves double-quoted
		// tokens against table columns); route it through the engine.
		if strings.EqualFold(f.Name, "sqlite_rename_quotefix") && len(args) >= 2 {
			return ev.evalRenameQuotefix(args)
		}
		// LIKE/GLOB/REGEXP as function calls (e.g. LIKE('a%', x)) evaluate
		// through the operator implementations, which honor the engine's
		// case-sensitivity setting and the optional ESCAPE argument.
		if strings.EqualFold(f.Name, "LIKE") && (len(args) == 2 || len(args) == 3) {
			return ev.evalLikeFunction(args)
		}
		if strings.EqualFold(f.Name, "GLOB") && len(args) == 2 {
			return boolToInt(globValues(args[0], args[1])), nil
		}
		if strings.EqualFold(f.Name, "REGEXP") && len(args) == 2 {
			return ev.evalRegexpOp(args[0], args[1], false)
		}
		// base64/base85 enforce SQLITE_LIMIT_LENGTH on their output ("blob
		// expanded to base64/base85 too big", basexx.c base64()/base85()).
		// Route them through the engine context for the current limit.
		if strings.EqualFold(f.Name, "BASE64") {
			return ev.evalBaseX("base64", args)
		}
		if strings.EqualFold(f.Name, "BASE85") {
			return ev.evalBaseX("base85", args)
		}
		// eval(SQL[,SEP]) runs SQL text recursively (ext/misc/eval.c); route
		// through the engine for statement execution.
		if strings.EqualFold(f.Name, "EVAL") {
			return ev.evalSQLFunc(args)
		}
		return fn.ScalarFn(args)
	}
	// Scalar min/max: with two or more arguments, MIN()/MAX() are scalar
	// functions. SQLite semantics: if any argument is NULL the result is
	// NULL (unlike the aggregate forms, which ignore NULLs).
	if fn.Type == function.TypeAggregate && len(args) >= 2 && (upper == "MIN" || upper == "MAX") {
		return evalScalarMinMax(upper, args), nil
	}
	// For aggregate functions, evaluate step by step if row is provided
	if fn.Type == function.TypeAggregate {
		return evalAggregateFunc(fn, args)
	}
	return nil, fmt.Errorf("aggregate function %s not supported in this context", f.Name)
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

// evalEngineFunc evaluates engine-state functions (CHANGES,
// LAST_INSERT_ROWID, RAISE, AFFINITY, COUNTER, NONDETER). Returns whether the
// call was handled.
func (ev *Evaluator) evalEngineFunc(f *sql.FuncCall, row Row) (interface{}, bool, error) {
	switch strings.ToUpper(f.Name) {
	case "CHANGES":
		return ev.ctx.LastChanges(), true, nil
	case "TOTAL_CHANGES":
		return ev.ctx.TotalChanges(), true, nil
	case "LAST_INSERT_ROWID":
		return ev.ctx.LastRowID(), true, nil
	case "RAISE":
		val, err := ev.evalRaiseFuncCall(f, row)
		return val, true, err
	case "AFFINITY":
		// Test-only affinity(X): reports the affinity of the column X refers
		// to (SQLite's column-affinity reports). The ColumnValue wrapper
		// carries the declared column affinity; evaluate WITHOUT unwrapping so
		// the function can see it. A non-column argument falls back to the
		// value's storage class.
		if len(f.Args) == 1 {
			v, err := ev.evalExpr(f.Args[0], row)
			if err != nil {
				return nil, true, err
			}
			return affinityOfValue(v), true, nil
		}
		return nil, true, fmt.Errorf("function affinity expects 1 argument, got %d", len(f.Args))
	case "COUNTER":
		// Test-only counter(N) function (SQLite test1.c selectH_counter):
		// increments the engine counter by N and returns the new value.
		// The counter resets at the start of each statement (see Exec).
		amt := int64(1)
		if len(f.Args) > 0 {
			if v, err := ev.evalExpr(f.Args[0], row); err == nil {
				amt = ToIntValue(util.UnwrapColumnValue(v))
			}
		}
		ev.ctx.SetCounterVal(ev.ctx.CounterVal() + amt)
		return ev.ctx.CounterVal(), true, nil
	case "NONDETER":
		// Test-only nondeter() function (SQLite having.test): a non-
		// deterministic function that increments a counter per call and
		// returns counter%2. The counter resets at statement start so each
		// query begins from 0, matching the TCL harness's
		// `set ::nondeter_ret 0` before each query.
		ev.ctx.SetNondeterVal(ev.ctx.NondeterVal() + 1)
		return ev.ctx.NondeterVal() % 2, true, nil
	case "FTS3_TOKENIZER":
		// fts3_tokenizer(name [, module]) — the tokenizer registry interface
		// (fts3_tokenizer.c). One argument resolves the name (error "unknown
		// tokenizer" when unregistered); two arguments register name → a
		// Go mirror of the fts3_test.c test tokenizer under that name and
		// return the module value non-NULL. A NULL second argument deletes
		// the registration.
		if len(f.Args) < 1 || len(f.Args) > 2 {
			return nil, true, fmt.Errorf("wrong number of arguments to function fts3_tokenizer()")
		}
		nameVal, err := ev.evalExpr(f.Args[0], row)
		if err != nil {
			return nil, true, err
		}
		name, _ := util.UnwrapColumnValue(nameVal).(string)
		if len(f.Args) == 1 {
			if !fts.HasTokenizer(name) {
				return nil, true, fmt.Errorf("unknown tokenizer: %s", name)
			}
			return []byte(name), true, nil
		}
		modVal, err := ev.evalExpr(f.Args[1], row)
		if err != nil {
			return nil, true, err
		}
		mod := util.UnwrapColumnValue(modVal)
		if mod == nil {
			fts.UnregisterCustomTokenizer(name)
			return nil, true, nil
		}
		fts.RegisterCustomTokenizer(strings.ToLower(name), func() fts.Tokenizer { return fts.NewTestTokenizer() })
		return mod, true, nil
	case "MATCHINFO", "OFFSETS", "SNIPPET", "OPTIMIZE":
		// FTS3 auxiliary functions: matchinfo(TABLE[, fmt]) returns a blob of
		// per-row match statistics; offsets(TABLE) returns the byte spans of
		// query-token occurrences; snippet(TABLE, ...) extracts a text
		// fragment around the matches; optimize(TABLE) merges segments
		// (fts3_snippet.c / fts3.c fts3OptimizeFunc).
		val, err := ev.evalFTSAux(f.Name, f, row)
		return val, true, err
	}
	return nil, false, nil
}

// evalFTSAux dispatches the FTS3 auxiliary functions by name.
func (ev *Evaluator) evalFTSAux(name string, f *sql.FuncCall, row Row) (interface{}, error) {
	// fts3.c fts3FunctionArg L3690-3699: argv[0] must be the hidden fts3
	// cursor of the CURRENTLY MATCHING fts table; anything else fails with
	// "illegal first argument to <func>". Arity violations use SQLite's
	// generic wrong-num-args text (snippet >6 has its own message,
	// fts3.c L3724-3727).
	if err := ev.validateFTSAuxArgs(name, f); err != nil {
		return nil, err
	}
	switch strings.ToUpper(name) {
	case "MATCHINFO":
		return ev.evalMatchinfoFunc(f, row)
	case "OFFSETS":
		return ev.evalFTSSnippetAux("offsets", f, row)
	case "SNIPPET":
		return ev.evalFTSSnippetAux("snippet", f, row)
	case "OPTIMIZE":
		return ev.evalFTSOptimize(f)
	}
	return nil, nil
}

// validateFTSAuxArgs reproduces the argument checks SQLite applies before an
// FTS3 auxiliary function runs: arity limits and the first-argument-must-be-
// the-matching-fts-table rule.
func (ev *Evaluator) validateFTSAuxArgs(name string, f *sql.FuncCall) error {
	lower := strings.ToLower(name)
	switch strings.ToUpper(name) {
	case "MATCHINFO":
		if len(f.Args) < 1 || len(f.Args) > 2 {
			return fmt.Errorf("wrong number of arguments to function %s()", lower)
		}
	case "SNIPPET":
		if len(f.Args) > 6 {
			return fmt.Errorf("wrong number of arguments to function snippet()")
		}
	case "OFFSETS":
		if len(f.Args) != 1 {
			return fmt.Errorf("wrong number of arguments to function %s()", lower)
		}
	}
	// First argument must reference the fts table of the active MATCH query.
	if len(f.Args) == 0 {
		return fmt.Errorf("wrong number of arguments to function %s()", lower)
	}
	colRef, ok := f.Args[0].(*sql.ColumnRef)
	if !ok || colRef.Name == "" {
		return fmt.Errorf("illegal first argument to %s", lower)
	}
	current := ev.ctx.CurrentFTSMatch()
	if current == "" {
		// No active MATCH-cursor context (JOIN/derived-table queries): the
		// legacy fallback resolves the first argument as an FTS table name;
		// unknown tables stay illegal.
		if _, ok := ev.ctx.FTSTables()[colRef.Name]; ok {
			return nil
		}
		return fmt.Errorf("illegal first argument to %s", lower)
	}
	if !strings.EqualFold(colRef.Name, current) {
		return fmt.Errorf("illegal first argument to %s", lower)
	}
	if _, ok := ev.ctx.FTSTables()[current]; !ok {
		return fmt.Errorf("illegal first argument to %s", lower)
	}
	return nil
}

// evalMatchinfoFunc implements the FTS3 matchinfo() function (fts3_snippet.c
// fts3GetMatchinfo / fts3MatchinfoValues): it returns a blob of little-endian
// uint32 values in format-string order. The format letters are:
//
//	'p' number of phrases in the MATCH query (1 value)
//	'c' number of columns (1 value)
//	'n' number of documents (1 value, FTS4 only)
//	'a' average token count per column (nCol values, FTS4 only)
//	'l' per-column token count of the current row (nCol values, docsize)
//	'x' per phrase and column: local hits, global occurrences, global rows
//	'y' per phrase and column: local hit counts
//	'b' per phrase: bitmask of columns with local hits
//
// The phrase structure comes from the current FTS SELECT's MATCH constraint
// (SetFTSMatchInfo); without a MATCH the function returns an empty blob.
func (ev *Evaluator) evalMatchinfoFunc(f *sql.FuncCall, row Row) (interface{}, error) {
	tableName := ev.ctx.CurrentFTSMatch()
	if tableName == "" {
		// matchinfo(TABLE) — the first argument names the FTS table.
		if len(f.Args) > 0 {
			if colRef, ok := f.Args[0].(*sql.ColumnRef); ok && colRef.Name != "" {
				tableName = colRef.Name
			}
		}
	}
	ftsTable, ok := ev.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return []byte{}, nil
	}
	format := "pcx"
	if len(f.Args) > 1 {
		v, err := ev.evalExpr(f.Args[1], row)
		if err != nil {
			return nil, err
		}
		if s, ok := util.UnwrapColumnValue(v).(string); ok {
			format = s
		}
	}
	// Validate the format string against the table's capabilities
	// (fts3_snippet.c fts3MatchinfoCheck): 'p'/'c' are always recognized;
	// 'n' and 'a' require an FTS4 table; 'l' requires the %_docsize table
	// (absent for FTS3 and for FTS4 created with matchinfo=fts3).
	if err := validateMatchinfoFormat(ftsTable, format); err != nil {
		return nil, err
	}

	// The matchinfo query context: the FTS table's MATCH phrases (set by
	// execFTSSelect). Without a MATCH constraint the blob is empty
	// (fts3matchinfo 7.2/7.3: typeof=blob, length=0).
	ctxTable, hasMatch, phrases := ev.ctx.FTSMatchInfo()
	if !strings.EqualFold(ctxTable, tableName) || !hasMatch {
		return []byte{}, nil
	}

	// Current row's docid (the FTS row map stores it under "rowid").
	docID := rowDocID(row)

	nCol := int64(len(ftsTable.ColumnNames()))
	nPhrase := int64(len(phrases))
	var out []byte
	if err := ev.writeMatchinfoFormat(ftsTable, format, phrases, docID, nCol, nPhrase, func(v uint32) {
		out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// validateMatchinfoFormat rejects format letters the table does not support
// (fts3_snippet.c fts3MatchinfoCheck): 'n'/'a' need an FTS4 table, 'l' needs
// the %_docsize table.
func validateMatchinfoFormat(ftsTable *fts.FTS3Table, format string) error {
	hasDocsize := ftsTable.IsFTS4() && !ftsTable.NoDocsize()
	for i := 0; i < len(format); i++ {
		switch format[i] {
		case 'p', 'c', 'x', 'y', 'b':
			// always recognized
		case 'n', 'a':
			if !ftsTable.IsFTS4() {
				return fmt.Errorf("unrecognized matchinfo request: %c", format[i])
			}
		case 'l':
			if !hasDocsize {
				return fmt.Errorf("unrecognized matchinfo request: l")
			}
		default:
			// fts3_snippet.c fts3MatchinfoCheck L1005: any other letter
			// fails the statement ("unrecognized matchinfo request: d").
			return fmt.Errorf("unrecognized matchinfo request: %c", format[i])
		}
	}
	return nil
}

// rowDocID returns the FTS row map's docid (the row map stores it under
// "rowid").
func rowDocID(row Row) int64 {
	var docID int64
	if rv, ok := row.Get("rowid"); ok {
		if dv, ok := util.UnwrapColumnValue(rv).(int64); ok {
			docID = dv
		}
	}
	return docID
}

// writeMatchinfoFormat appends the matchinfo values for one format string to
// the output via writeU32 (fts3_snippet.c fts3MatchinfoValues). The 'n' and
// 'a' formats read the FTS4 %_stat doctotal blob and 'l' reads the %_docsize
// blob for the current row (sqlite3Fts3SelectDoctotal / fts3SelectDocsize); a
// corrupt or missing blob errors "database disk image is malformed"
// (FTS_CORRUPT_VTAB).
func (ev *Evaluator) writeMatchinfoFormat(ftsTable *fts.FTS3Table, format string, phrases []fts.MatchPhrase, docID int64, nCol, nPhrase int64, writeU32 func(uint32)) error {
	for i := 0; i < len(format); i++ {
		switch format[i] {
		case 'p':
			writeU32(uint32(nPhrase))
		case 'c':
			writeU32(uint32(nCol))
		case 'n':
			n, err := ev.matchinfoDoctotal(ftsTable, nCol)
			if err != nil {
				return err
			}
			writeU32(uint32(n))
		case 'a':
			vals, err := ev.matchinfoAverages(ftsTable, nCol)
			if err != nil {
				return err
			}
			for _, v := range vals {
				writeU32(v)
			}
		case 'l':
			vals, err := ev.matchinfoLengths(ftsTable, nCol, docID)
			if err != nil {
				return err
			}
			for _, v := range vals {
				writeU32(v)
			}
		case 'x':
			writeMatchinfoHits(ftsTable, phrases, docID, func(mp fts.MatchPhrase) []uint32 {
				return ftsTable.MatchInfoX(mp.Node, mp.Scope, mp.Side, mp.Gate, docID)
			}, writeU32)
		case 'y':
			writeMatchinfoHits(ftsTable, phrases, docID, func(mp fts.MatchPhrase) []uint32 {
				return ftsTable.MatchInfoY(mp.Node, mp.Scope, mp.Side, mp.Gate, docID)
			}, writeU32)
		case 'b':
			// nPhrase * ceil(nCol/32) values: per phrase, a bitmask of
			// columns with at least one local hit.
			bmWords := (nCol + 31) / 32
			for _, mp := range phrases {
				y := ftsTable.MatchInfoY(mp.Node, mp.Scope, mp.Side, mp.Gate, docID)
				for w := int64(0); w < bmWords; w++ {
					writeU32(matchinfoBitmask(y, nCol, w))
				}
			}
		default:
			// Unrecognized flags are ignored by SQLite's matchinfo
			// (fts3MatchinfoCheck errors, but the engine's compat suite
			// does not exercise error paths here).
		}
	}
	return nil
}

// matchinfoDoctotal reads the FTS4 %_stat doctotal blob and returns its first
// varint, the document count (fts3_snippet.c fts3MatchinfoSelectDoctotal:
// the first varint is nDoc; nDoc<=0 or an overrun is FTS_CORRUPT_VTAB).
func (ev *Evaluator) matchinfoDoctotal(ftsTable *fts.FTS3Table, nCol int64) (uint32, error) {
	blob, err := ev.ctx.FTSShadowBlob(ftsTable.Name(), "doctotal", 0)
	if err != nil {
		return 0, err
	}
	nDoc, consumed := fts.GetFTS3Varint(blob)
	if consumed == 0 || nDoc <= 0 {
		return 0, fmt.Errorf("database disk image is malformed")
	}
	return uint32(nDoc), nil
}

// matchinfoAverages computes the 'a' average-token-count values
// ((total + nDoc/2) / nDoc) from the %_stat doctotal blob (fts3_snippet.c
// FTS3_MATCHINFO_AVGLENGTH: the blob is nDoc followed by one per-column
// token-total varint; an overrun is FTS_CORRUPT_VTAB).
func (ev *Evaluator) matchinfoAverages(ftsTable *fts.FTS3Table, nCol int64) ([]uint32, error) {
	blob, err := ev.ctx.FTSShadowBlob(ftsTable.Name(), "doctotal", 0)
	if err != nil {
		return nil, err
	}
	nDoc, consumed := fts.GetFTS3Varint(blob)
	if consumed == 0 || nDoc <= 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	pos := consumed
	out := make([]uint32, nCol)
	for i := int64(0); i < nCol; i++ {
		total, n := fts.GetFTS3Varint(blob[pos:])
		if n == 0 || pos+n > len(blob) {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		pos += n
		out[i] = uint32((uint64(total) + nDoc/2) / nDoc)
	}
	return out, nil
}

// matchinfoLengths reads the %_docsize blob for the current row and decodes
// nCol per-column token-count varints (fts3_snippet.c FTS3_MATCHINFO_LENGTH:
// the blob is one FTS3 varint per column; an overrun is FTS_CORRUPT_VTAB).
func (ev *Evaluator) matchinfoLengths(ftsTable *fts.FTS3Table, nCol int64, docID int64) ([]uint32, error) {
	blob, err := ev.ctx.FTSShadowBlob(ftsTable.Name(), "docsize", docID)
	if err != nil {
		return nil, err
	}
	pos := 0
	out := make([]uint32, nCol)
	for i := int64(0); i < nCol; i++ {
		v, n := fts.GetFTS3Varint(blob[pos:])
		if n == 0 || pos+n > len(blob) {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		pos += n
		out[i] = uint32(v)
	}
	return out, nil
}

// writeMatchinfoHits writes one phrase's per-column values for the 'x' and
// 'y' formats (per phrase, per column, in phrase order).
func writeMatchinfoHits(ftsTable *fts.FTS3Table, phrases []fts.MatchPhrase, docID int64, values func(fts.MatchPhrase) []uint32, writeU32 func(uint32)) {
	for _, mp := range phrases {
		for _, v := range values(mp) {
			writeU32(v)
		}
	}
}

// matchinfoBitmask computes one 32-bit word of the 'b' format bitmap for a
// phrase's per-column hit counts: word w holds bits for columns w*32..w*32+31
// that have at least one local hit (fts3_snippet.c fts3ExprLHits).
func matchinfoBitmask(y []uint32, nCol, w int64) uint32 {
	var mask uint32
	for c := int64(0); c < 32; c++ {
		col := w*32 + c
		if col < nCol && y[col] > 0 {
			mask |= 1 << uint(c)
		}
	}
	return mask
}

// evalFTSSnippetAux evaluates the FTS3 offsets() and snippet() auxiliary
// functions. Both need the current FTS SELECT's MATCH phrases (from the
// matchinfo context) and the current row's docid; without a MATCH they
// return an empty string (fts3_snippet.c: pCsr->pExpr NULL → "").
func (ev *Evaluator) evalFTSSnippetAux(name string, f *sql.FuncCall, row Row) (interface{}, error) {
	tableName := ev.ctx.CurrentFTSMatch()
	if tableName == "" && len(f.Args) > 0 {
		if colRef, ok := f.Args[0].(*sql.ColumnRef); ok && colRef.Name != "" {
			tableName = colRef.Name
		}
	}
	ftsTable, ok := ev.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return "", nil
	}
	ctxTable, hasMatch, phrases := ev.ctx.FTSMatchInfo()
	if !strings.EqualFold(ctxTable, tableName) || !hasMatch {
		return "", nil
	}
	var docID int64
	if rv, ok := row.Get("rowid"); ok {
		if dv, ok := util.UnwrapColumnValue(rv).(int64); ok {
			docID = dv
		}
	}
	if name == "offsets" {
		s, err := ftsTable.Offsets(docID, phrases, ev.ftsContentOverride(ftsTable, row))
		if err != nil {
			return "", err
		}
		return s, nil
	}
	zStart, zEnd, zEllipsis, iCol, nToken := ev.snippetArgs(f, row)
	return ftsTable.Snippet(docID, phrases, zStart, zEnd, zEllipsis, iCol, nToken, ev.ftsContentOverride(ftsTable, row)), nil
}

// ftsContentOverride returns the content-row column values for an FTS4
// content=<table> table from the current row map (keyed by column name), or
// nil for a normal FTS table. SQLite reads the content table for
// offsets()/snippet() column text (fts3_snippet.c reads the row via the
// content table); the row map built by ftsContentTableRowMapsForDocIDs
// carries those values, so a document whose content row was updated after
// indexing shows the NEW text (fts4content 2.4.3/2.5.x).
func (ev *Evaluator) ftsContentOverride(ftsTable *fts.FTS3Table, row Row) []interface{} {
	if ftsTable.ContentTable() == "" {
		return nil
	}
	names := ftsTable.ColumnNames()
	out := make([]interface{}, len(names))
	for i, cn := range names {
		if v, ok := row.Get(cn); ok {
			out[i] = util.UnwrapColumnValue(v)
		}
	}
	return out
}

// snippetArgs evaluates the optional snippet(TABLE, zStart, zEnd, zEllipsis,
// iCol, nToken) arguments, returning the defaults when absent.
func (ev *Evaluator) snippetArgs(f *sql.FuncCall, row Row) (zStart, zEnd, zEllipsis string, iCol, nToken int) {
	zStart, zEnd, zEllipsis = "<b>", "</b>", "<b>...</b>"
	iCol, nToken = -1, 15
	if len(f.Args) > 1 {
		if v, err := ev.evalExpr(f.Args[1], row); err == nil {
			if s, ok := util.UnwrapColumnValue(v).(string); ok {
				zStart = s
			}
		}
	}
	if len(f.Args) > 2 {
		if v, err := ev.evalExpr(f.Args[2], row); err == nil {
			if s, ok := util.UnwrapColumnValue(v).(string); ok {
				zEnd = s
			}
		}
	}
	if len(f.Args) > 3 {
		if v, err := ev.evalExpr(f.Args[3], row); err == nil {
			if s, ok := util.UnwrapColumnValue(v).(string); ok {
				zEllipsis = s
			}
		}
	}
	if len(f.Args) > 4 {
		if v, err := ev.evalExpr(f.Args[4], row); err == nil {
			iCol = int(ToIntValue(util.UnwrapColumnValue(v)))
		}
	}
	if len(f.Args) > 5 {
		if v, err := ev.evalExpr(f.Args[5], row); err == nil {
			nToken = int(ToIntValue(util.UnwrapColumnValue(v)))
		}
	}
	return zStart, zEnd, zEllipsis, iCol, nToken
}

// evalFTSOptimize implements the FTS3 optimize(TABLE) auxiliary function
// (fts3.c fts3OptimizeFunc): it merges the table's segments and returns
// "Index optimized" (or "Index already optimal" when no merge was needed).
func (ev *Evaluator) evalFTSOptimize(f *sql.FuncCall) (interface{}, error) {
	tableName := ev.ctx.CurrentFTSMatch()
	if tableName == "" && len(f.Args) > 0 {
		if colRef, ok := f.Args[0].(*sql.ColumnRef); ok && colRef.Name != "" {
			tableName = colRef.Name
		}
	}
	ftsTable, ok := ev.ctx.FTSTables()[tableName]
	if !ok || ftsTable == nil {
		return "", nil
	}
	_ = ftsTable
	// fts3.c fts3DoOptimize: flush the pending terms FIRST (an unflushed
	// batch becomes its own segment), then merge. SQLITE_DONE ("Index
	// already optimal") is returned only when the post-flush index is a
	// single segment — i.e. nothing needed merging (fts3f 1.3: the first
	// optimize() flushes the open transaction's pending docs, creating a
	// second segment, so it reports "Index optimized"; every later call
	// sees one segment and reports "Index already optimal").
	before, cerr := ev.ctx.EvalExecSQL("SELECT count(*) FROM "+tableName+"_segdir", "")
	hasPending := false
	if t, ok := ev.ctx.FTSTables()[tableName]; ok && t != nil {
		hasPending = len(t.PendingSnapshot()) > 0
	}
	if cerr == nil {
		if n, perr := strconv.Atoi(strings.TrimSpace(before)); perr == nil && n <= 1 && !hasPending {
			return "Index already optimal", nil
		}
	}
	if _, exErr := ev.ctx.EvalExecSQL("INSERT INTO "+tableName+"("+tableName+") VALUES('optimize')", ""); exErr != nil {
		return nil, exErr
	}
	return "Index optimized", nil
}

// evalFuncArgs evaluates a function call's argument expressions, unwrapping
// BlobColumnValue, ColumnValue and CollatedValue wrappers so functions receive
// the raw scalar. For UTF-16 encoding, odd-length blobs are truncated (ignore
// the last byte) to ensure valid UTF-16 byte sequences (SQLite ticket
// 9eda2697f5cc1aba).
func (ev *Evaluator) evalFuncArgs(f *sql.FuncCall, row Row) ([]interface{}, error) {
	args := make([]interface{}, len(f.Args))
	for i, arg := range f.Args {
		v, err := ev.evalExpr(arg, row)
		if err != nil {
			return nil, err
		}
		v = util.UnwrapColumnValue(v)
		v = unwrapCollatedValue(v)
		if b, ok := v.([]byte); ok && len(b)%2 == 1 {
			if strings.HasPrefix(ev.ctx.TextEncoding(), "UTF-16") {
				v = b[:len(b)-1]
			}
		}
		args[i] = v
	}
	return args, nil
}

// validateFuncArgs checks a function call's argument count against the
// function definition.
