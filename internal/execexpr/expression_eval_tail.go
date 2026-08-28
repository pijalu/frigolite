// This file holds the core expression evaluation engine: literal, column,
// function, CAST, IN, BETWEEN, LIKE, and arithmetic evaluation. It is the
// evaluation half of the former expression.go, split out so that each file
// stays within the repository's complexity and size budgets. Row-value and
// binary-operator evaluation lives in expression_rowvalue.go.
package execexpr

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func validateFuncArgs(fn *function.Func, f *sql.FuncCall, args []interface{}) error {
	if len(args) < fn.MinArgs || (fn.MaxArgs > 0 && len(args) > fn.MaxArgs) {
		if fn.WrongArgMsg {
			return fmt.Errorf("wrong number of arguments to function %s()", f.Name)
		}
		return fmt.Errorf("function %s expects %d-%d arguments, got %d", f.Name, fn.MinArgs, fn.MaxArgs, len(args))
	}
	return nil
}

// evalRenameQuotefix routes sqlite_rename_quotefix through the engine for
// schema access (it resolves double-quoted tokens against table columns).
func (ev *Evaluator) evalRenameQuotefix(args []interface{}) (interface{}, error) {
	if sqlStr, ok := args[1].(string); ok {
		schemaName := ""
		if len(args) >= 1 && args[0] != nil {
			schemaName, _ = args[0].(string)
		}
		return ev.ctx.QuoteFixWithSchema(schemaName, sqlStr), nil
	}
	return "", nil
}

// evalBaseX evaluates base64/base85 with the engine's SQLITE_LIMIT_LENGTH
// check (basexx.c base64()/base85()). The implementation lives in the
// function package; the evaluator supplies the current limit from the engine.
func (ev *Evaluator) evalBaseX(name string, args []interface{}) (interface{}, error) {
	return function.EvalBaseX(name, args[0], ev.ctx.LengthLimit())
}

// evalSQLFunc implements eval(SQL[,SEP]) — run SQL text and return the joined
// result cells (ext/misc/eval.c sqlEvalFunc). NULL SQL returns NULL; the
// default separator is a single space.
func (ev *Evaluator) evalSQLFunc(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	sqlStr := function.ValueText(args[0])
	if sqlStr == "" {
		return "", nil
	}
	sep := " "
	if len(args) >= 2 && args[1] != nil {
		sep = function.ValueText(args[1])
	}
	return ev.ctx.EvalExecSQL(sqlStr, sep)
}

// evalScalarMinMax evaluates scalar MIN()/MAX() with two or more arguments:
// NULL propagates (unlike the aggregate forms, which ignore NULLs).
func evalScalarMinMax(upper string, args []interface{}) interface{} {
	for _, a := range args {
		if a == nil {
			return nil
		}
	}
	best := args[0]
	for _, a := range args[1:] {
		if (upper == "MIN" && util.CompareValues(a, best) < 0) ||
			(upper == "MAX" && util.CompareValues(a, best) > 0) {
			best = a
		}
	}
	return best
}

func (ev *Evaluator) evalBetween(v *sql.Between, row Row) (interface{}, error) {
	operand, err := ev.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if isSQLNull(operand) {
		return nil, nil
	}
	low, err := ev.evalExpr(v.Low, row)
	if err != nil {
		return nil, err
	}
	high, err := ev.evalExpr(v.High, row)
	if err != nil {
		return nil, err
	}
	// Row-value BETWEEN: (a,b) BETWEEN (x,y) AND (u,v) compares lexicographically
	// element-wise. A row value mixed with a scalar operand (or row values of
	// different arity) is "row value misused" (SQLite).
	opRow, opIsRow := operand.([]interface{})
	loRow, loIsRow := low.([]interface{})
	hiRow, hiIsRow := high.([]interface{})
	// A subquery operand in a row-value BETWEEN evaluates to its full first
	// row (evalExpr reduced it to the first column) ONLY when it is a
	// genuinely multi-column (row-value) subquery, or a sibling operand is
	// already a row value. A scalar subquery between scalar bounds stays a
	// scalar (SQLite: (select min(d) from t1) between f and t1.c is a
	// scalar BETWEEN — randexpr regression).
	var err2 error
	opRow, opIsRow, loRow, loIsRow, hiRow, hiIsRow, err2 = ev.betweenResolveRows(v, row, opRow, opIsRow, loRow, loIsRow, hiRow, hiIsRow)
	if err2 != nil {
		return nil, err2
	}
	if opIsRow || loIsRow || hiIsRow {
		return ev.evalBetweenRowValue(v, opRow, loRow, hiRow, opIsRow, loIsRow, hiIsRow)
	}
	if isSQLNull(low) || isSQLNull(high) {
		// SQLite: any NULL bound makes the whole BETWEEN NULL.
		return nil, nil
	}
	return ev.evalBetweenScalar(v, operand, low, high), nil
}

// evalBetweenScalar evaluates a scalar BETWEEN: operand >= low AND operand <=
// high, with NOT BETWEEN negation.
func (ev *Evaluator) evalBetweenScalar(v *sql.Between, operand, low, high interface{}) interface{} {
	result := ev.ctx.CompareValuesWithCollate(operand, low) >= 0 && ev.ctx.CompareValuesWithCollate(operand, high) <= 0
	if v.Negated {
		result = !result
	}
	return boolToInt(result)
}

// betweenResolveRows resolves subquery operands/bounds of a BETWEEN to their
// full first rows (row-value semantics) when they are genuinely multi-column
// subqueries or a sibling operand is already a row value. Non-subquery row
// values (from the initial type assertions) are preserved unchanged.
func (ev *Evaluator) betweenResolveRows(v *sql.Between, row Row, opRow []interface{}, opIsRow bool, loRow []interface{}, loIsRow bool, hiRow []interface{}, hiIsRow bool) ([]interface{}, bool, []interface{}, bool, []interface{}, bool, error) {
	var err error
	opRow, opIsRow, err = ev.betweenSubqueryRow(v.Operand, opRow, row, opIsRow, loIsRow, hiIsRow)
	if err != nil {
		return nil, false, nil, false, nil, false, err
	}
	loRow, loIsRow, err = ev.betweenSubqueryRow(v.Low, loRow, row, loIsRow, opIsRow, hiIsRow)
	if err != nil {
		return nil, false, nil, false, nil, false, err
	}
	hiRow, hiIsRow, err = ev.betweenSubqueryRow(v.High, hiRow, row, hiIsRow, opIsRow, loIsRow)
	if err != nil {
		return nil, false, nil, false, nil, false, err
	}
	return opRow, opIsRow, loRow, loIsRow, hiRow, hiIsRow, nil
}

// evalBetweenRowValue evaluates a row-value BETWEEN: (a,b) BETWEEN (x,y) AND
// (u,v) compares lexicographically element-wise. A row value mixed with a
// scalar operand (or row values of different arity) is "row value misused".
func (ev *Evaluator) evalBetweenRowValue(v *sql.Between, opRow, loRow, hiRow []interface{}, opIsRow, loIsRow, hiIsRow bool) (interface{}, error) {
	if !opIsRow || !loIsRow || !hiIsRow {
		return nil, fmt.Errorf("row value misused")
	}
	if len(opRow) != len(loRow) || len(opRow) != len(hiRow) {
		return nil, fmt.Errorf("row value misused")
	}
	// Compare element-wise: result is the lexicographic BETWEEN. A NULL
	// element leaves the comparison undecided unless a later element decides
	// it (SQLite row-value semantics).
	geLow, geLowNull := ev.rowValueGE(opRow, loRow)
	leHigh, leHighNull := ev.rowValueGE(hiRow, opRow)
	if geLowNull || leHighNull {
		return nil, nil
	}
	result := geLow && leHigh
	if v.Negated {
		result = !result
	}
	return boolToInt(result), nil
}

// betweenSubqueryRow resolves a BETWEEN subquery bound to its full first row
// (row-value semantics) when it is a genuinely multi-column subquery or a
// sibling operand is already a row value. A scalar subquery between scalar
// bounds stays a scalar. Returns the (possibly resolved) row, whether it is
// now a row value, and any error. The current row value is preserved when no
// subquery resolution applies.
func (ev *Evaluator) betweenSubqueryRow(expr sql.Expr, curRow []interface{}, row Row, isRow bool, siblings ...bool) ([]interface{}, bool, error) {
	subq, ok := expr.(*sql.Subquery)
	if !ok || isRow {
		return curRow, isRow, nil
	}
	multi := false
	for _, s := range siblings {
		if s {
			multi = true
			break
		}
	}
	if ev.ctx.SubqueryColumnCount(subq.Select) <= 1 && !multi {
		return curRow, isRow, nil
	}
	rows, err := ev.evalSubqueryRows(subq, row)
	if err != nil {
		return curRow, isRow, err
	}
	if len(rows) > 0 {
		return rows[0], true, nil
	}
	return curRow, isRow, nil
}

// rowValueGE reports whether opRow is lexicographically >= boundRow (element-
// wise, SQLite row-value ordering), tracking whether a NULL at an undecided
// position left the result unknown.
func (ev *Evaluator) rowValueGE(opRow, boundRow []interface{}) (bool, bool) {
	ge := true
	geNull := false
	for i := range opRow {
		lv0, _ := extractValue(opRow[i])
		rv0, _ := extractValue(boundRow[i])
		if util.UnwrapColumnValue(lv0) == nil || util.UnwrapColumnValue(rv0) == nil {
			geNull = true
			continue
		}
		c := ev.ctx.CompareValuesWithCollate(opRow[i], boundRow[i])
		if c < 0 {
			ge = false
			geNull = false
			break
		}
		if c > 0 {
			geNull = false
			break
		}
	}
	return ge, geNull
}

// evalQualifiedColumnRef resolves a table-qualified column reference against
// the row, trigger NEW/OLD rows, and outer rows (correlated references).
func (ev *Evaluator) evalQualifiedColumnRef(v *sql.ColumnRef, row Row) (interface{}, error) {
	// Strip a schema prefix (main./temp./aux1.) from the qualifier so
	// "main.txx.a" resolves against the "txx.a" key in the row map, and
	// try both the full qualifier ("main.t4.a") and the stripped form
	// ("t4.a") since join row maps may store either.
	fullQual := v.Table
	tableQual := v.Table
	if dot := strings.Index(tableQual, "."); dot >= 0 {
		tableQual = tableQual[dot+1:]
	}
	// Try table-qualified key in the current row, then trigger NEW/OLD rows.
	if val, ok := ev.qualifiedRowLookup(v, row, fullQual, tableQual); ok {
		return val, nil
	}
	if val, ok := ev.triggerRowLookup(v); ok {
		return val, nil
	}
	// Fallback to outer rows for correlated references (qualified)
	for _, outer := range ev.ctx.OuterRowsForResolution() {
		if outer == nil {
			continue
		}
		// Try qualified name first (e.g., "t1.a"), then unqualified name
		// (the outer row may store column values without a table prefix).
		if val, ok := outer.Get(v.Table + "." + v.Name); ok {
			return val, nil
		}
		if val, ok := outer.Get(v.Name); ok {
			return val, nil
		}
	}
	// RETURNING evaluates expressions against the statement's row with strict
	// column resolution: unknown columns are errors ("no such column"), and a
	// qualifier must name the modified table. Inside a subquery scan
	// (currentScanTable != "") the scan's own row resolution already
	// succeeded or fell through, so strict mode only applies at the RETURNING
	// row level.
	if ev.ctx.ReturningStrict() && ev.ctx.CurrentScanTable() == "" {
		if val, ok := ev.strictReturningQualified(v, row); ok {
			return val, nil
		}
		return nil, fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
	}
	return nil, nil
}

// qualifiedRowLookup resolves a table-qualified column reference against the
// current row: the full qualifier ("main.t4.a"), the stripped qualifier
// ("t4.a"), qualified rowid aliases, and unqualified-name fallbacks for the
// table currently being scanned or a DML row.
func (ev *Evaluator) qualifiedRowLookup(v *sql.ColumnRef, row Row, fullQual, tableQual string) (interface{}, bool) {
	if row == nil {
		return nil, false
	}
	if val, ok := row.Get(fullQual + "." + v.Name); ok {
		return val, true
	}
	if val, ok := row.Get(tableQual + "." + v.Name); ok {
		return val, true
	}
	// Qualified rowid alias (t1.rowid, t1._rowid_, t1.oid) resolves against
	// the qualified rowid key stored for joined rows.
	if isRowIDName(v.Name) {
		if val, ok := row.Get(fullQual + ".rowid"); ok {
			return val, true
		}
		if val, ok := row.Get(tableQual + ".rowid"); ok {
			return val, true
		}
	}
	// If the qualified key is not found and the qualifier matches the table
	// currently being scanned or a DML row, resolve via unqualified name.
	if val, ok := ev.qualifiedUnqualifiedFallback(v, row, tableQual); ok {
		return val, true
	}
	return nil, false
}

// qualifiedUnqualifiedFallback resolves a table-qualified reference via the
// row's unqualified column keys when the qualifier names the table currently
// being scanned (not a join result) or the current DML row.
func (ev *Evaluator) qualifiedUnqualifiedFallback(v *sql.ColumnRef, row Row, tableQual string) (interface{}, bool) {
	// Row maps store unqualified column names, so "t1.a" in a query scanning
	// table t1 resolves to row["a"]. Join result maps store qualified keys
	// (t1.a), so the fallback must NOT apply there: t4.id in a joined row
	// must resolve exactly, not to the merged id column.
	if ev.ctx.CurrentScanTable() != "" && strings.EqualFold(tableQual, ev.ctx.CurrentScanTable()) && !ev.rowHasQualifiedKeys(row) {
		if val, ok := row.Get(v.Name); ok {
			return val, true
		}
	}
	// Same for DML (INSERT/UPDATE) rows: a table-qualified reference in a
	// CHECK/default expression resolves against the row's unqualified keys.
	if ev.ctx.CurrentDMLTable() != "" && strings.EqualFold(tableQual, ev.ctx.CurrentDMLTable()) {
		if val, ok := row.Get(v.Name); ok {
			return val, true
		}
	}
	return nil, false
}

// triggerRowLookup resolves a NEW/OLD trigger-row column reference.
func (ev *Evaluator) triggerRowLookup(v *sql.ColumnRef) (interface{}, bool) {
	if strings.EqualFold(v.Table, "new") && ev.ctx.TriggerNewRow() != nil {
		if val, ok := ev.ctx.TriggerNewRow().Get(v.Name); ok {
			return val, true
		}
	}
	if strings.EqualFold(v.Table, "old") && ev.ctx.TriggerOldRow() != nil {
		if val, ok := ev.ctx.TriggerOldRow().Get(v.Name); ok {
			return val, true
		}
	}
	return nil, false
}

// strictReturningQualified resolves a table-qualified column reference against
// the RETURNING row in strict mode: the qualifier must name the modified
// table and the column (or rowid alias) must be present.
func (ev *Evaluator) strictReturningQualified(v *sql.ColumnRef, row Row) (interface{}, bool) {
	if !strings.EqualFold(v.Table, ev.ctx.ReturningTable()) || row == nil {
		return nil, false
	}
	if val, ok := row.Get(v.Name); ok {
		return val, true
	}
	if isRowIDName(v.Name) {
		if val, ok := row.Get("rowid"); ok {
			return val, true
		}
	}
	return nil, false
}

func (ev *Evaluator) evalCastExpr(v *sql.CastExpr, row Row) (result interface{}, err error) {
	val, evalErr := ev.evalExpr(v.Operand, row)
	if evalErr != nil {
		return nil, evalErr
	}
	if val == nil {
		return nil, nil
	}
	// An r-tree geometry marker cannot be coerced to any storage class
	// (rtree9-4.3: CAST(cube(...) || X'...' AS blob) errors the statement).
	if isRtreeGeometry(val) {
		return nil, fmt.Errorf("SQL logic error")
	}
	// Unwrap ColumnValue affinity wrappers and CollatedValue collation markers
	// so the CAST operates on the raw value.
	val = unwrapCollatedValue(val)
	// The CAST result carries the affinity of its target type for comparison
	// purposes (sqlite3ExprAffinity): CAST(x AS NUMERIC) compares its other
	// operand with NUMERIC affinity, CAST(x AS TEXT) with TEXT affinity, etc.
	// Output paths unwrap the ColumnValue (unwrapCollatedValue), so the
	// wrapper only affects comparisons.
	defer func() {
		if result != nil {
			result = &util.ColumnValue{Value: result, Affinity: util.Affinity(v.AsType)}
		}
	}()
	switch strings.ToUpper(v.AsType) {
	case "INTEGER", "INT":
		return castToInteger(val)
	case "REAL", "FLOAT", "DOUBLE":
		return castToReal(val)
	case "TEXT":
		return castToText(val)
	case "BLOB":
		return castToBlob(val)
	case "NUMERIC":
		return castToNumeric(val)
	default:
		// Any other TEXT-affinity target (VARCHAR, CHARACTER, CLOB, ...):
		// CAST(1 AS VARCHAR(50)) is '1' (text), like CAST(... AS TEXT)
		// (SQLite affinity rules; tkt3527).
		if isTextAffinityType(v.AsType) {
			return castToText(val)
		}
		return val, nil
	}
}
