// Package exec implements query execution.
package exec

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// --- Expression evaluation ---

func (e *Engine) evalExpr(expr sql.Expr, row Row) (interface{}, error) {
	if expr == nil {
		return nil, nil
	}
	switch v := expr.(type) {
	case *sql.NumericLit:
		return evalNumericLit(v)
	case *sql.StringLit:
		return v.Value, nil
	case *sql.BlobLit:
		return v.Value, nil
	case *sql.NullLit:
		return nil, nil
	case *sql.ParameterExpr:
		// Unbound parameter placeholders evaluate to NULL.
		return nil, nil
	case *sql.ParenExpr:
		return e.evalExpr(v.Expr, row)
	case *sql.ColumnRef:
		return e.evalColumnRef(v, row)
	case *sql.FuncCall:
		return e.evalFuncCall(v, row)
	case *sql.RowValue:
		// Evaluate a row value (a,b,c) into a structured slice so comparison
		// operators and IN can implement SQLite's per-element lexicographic
		// row-value semantics (with arity checks). A bare row value in a
		// SELECT list projects its first element; that unwrapping happens at
		// the projection sites.
		var values []interface{}
		for _, val := range v.Values {
			ev, err := e.evalExpr(val, row)
			if err != nil {
				return nil, err
			}
			values = append(values, ev)
		}
		return values, nil
	default:
		return e.evalComplexExpr(expr, row)
	}
}

func (e *Engine) evalComplexExpr(expr sql.Expr, row Row) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		return e.evalExpr(v.Expr, row)
	case *sql.BinaryOp:
		return e.evalBinaryOp(v, row)
	case *sql.UnaryOp:
		return e.evalUnaryOp(v, row)
	case *sql.IsNull:
		return e.evalIsNull(v, row)
	case *sql.IsNotNull:
		return e.evalIsNotNull(v, row)
	case *sql.IsTrue:
		return e.evalIsTrue(v, row)
	case *sql.IsFalse:
		return e.evalIsFalse(v, row)
	case *sql.IsDistinctFrom:
		return e.evalIsDistinctFrom(v, row)
	case *sql.IsNotDistinctFrom:
		return e.evalIsNotDistinctFrom(v, row)
	case *sql.Between:
		return e.evalBetween(v, row)
	case *sql.InList:
		return e.evalInList(v, row)
	case *sql.Subquery:
		return e.evalSubquery(v, row)
	case *sql.ExistsExpr:
		return e.evalExists(v, row)
	case *sql.CaseExpr:
		return e.evalCaseExpr(v, row)
	case *sql.CastExpr:
		return e.evalCastExpr(v, row)
	case *sql.RaiseExpr:
		return e.evalRaiseExpr(v, row)
	default:
		return nil, fmt.Errorf("unknown expression type: %T", expr)
	}
}

// errRaiseIgnore is a sentinel error returned when a trigger program executes
// RAISE(IGNORE). The statement that hit it is aborted without error and
// execution continues with the next statement in the trigger program.
var errRaiseIgnore = fmt.Errorf("RAISE(IGNORE)")

// evalRaiseExpr evaluates the RAISE() special function. RAISE() is only valid
// inside a trigger program; outside one it is a syntax/semantic error. Within
// a trigger, RAISE(IGNORE) aborts the current statement (signaled via
// errRaiseIgnore) and the other kinds abort with the given error message.
func (e *Engine) evalRaiseExpr(v *sql.RaiseExpr, row Row) (interface{}, error) {
	if e.triggerDepth == 0 {
		return nil, fmt.Errorf("RAISE() may only be used within a trigger-program")
	}
	if strings.EqualFold(v.Kind, "IGNORE") {
		return nil, errRaiseIgnore
	}
	msg := ""
	if v.Message != nil {
		val, err := e.evalExpr(v.Message, row)
		if err != nil {
			return nil, err
		}
		if val != nil {
			msg = fmt.Sprintf("%v", val)
		}
	}
	return nil, fmt.Errorf("%s", msg)
}

// evalRaiseFuncCall handles RAISE() when it reaches expression evaluation as
// a regular function call (the legacy parser represents RAISE(IGNORE) as a
// FuncCall whose first argument is a column reference). The LALR parser
// produces a *sql.RaiseExpr instead, handled by evalRaiseExpr.
func (e *Engine) evalRaiseFuncCall(f *sql.FuncCall, row Row) (interface{}, error) {
	if e.triggerDepth == 0 {
		return nil, fmt.Errorf("RAISE() may only be used within a trigger-program")
	}
	if len(f.Args) == 0 {
		return nil, fmt.Errorf("RAISE() requires an argument")
	}
	kind := ""
	if col, ok := f.Args[0].(*sql.ColumnRef); ok {
		kind = strings.ToUpper(col.Name)
	}
	if kind == "" {
		if s, ok := f.Args[0].(*sql.StringLit); ok {
			kind = strings.ToUpper(s.Value)
		}
	}
	if kind == "IGNORE" {
		return nil, errRaiseIgnore
	}
	msg := ""
	if len(f.Args) > 1 {
		val, err := e.evalExpr(f.Args[1], row)
		if err != nil {
			return nil, err
		}
		if val != nil {
			msg = fmt.Sprintf("%v", val)
		}
	}
	return nil, fmt.Errorf("%s", msg)
}

func (e *Engine) evalSubquery(v *sql.Subquery, row Row) (interface{}, error) {
	// Save and restore outerRow for correlated subquery support. Push the
	// current outer row onto a stack so nested subqueries can resolve
	// multi-level correlated references (outer → grandparent).
	e.pushOuterRow(row)
	defer e.popOuterRow()

	result := e.execSelect(v.Select)
	if result.Error != nil {
		return nil, result.Error
	}
	if len(result.Rows) == 0 {
		return nil, nil
	}
	// Return first column of first row
	if len(result.Rows[0]) > 0 {
		return result.Rows[0][0], nil
	}
	return nil, nil
}

func (e *Engine) evalExists(v *sql.ExistsExpr, row Row) (interface{}, error) {
	// Propagate outerRow for correlated subquery references
	e.pushOuterRow(row)
	defer e.popOuterRow()

	result := e.execSelect(v.Select)
	if result.Error != nil {
		return nil, result.Error
	}
	exists := len(result.Rows) > 0
	if v.Negated {
		exists = !exists
	}
	return boolToInt(exists), nil
}

func (e *Engine) evalCaseExpr(v *sql.CaseExpr, row Row) (interface{}, error) {
	if v.Operand != nil {
		return e.evalCaseWithOperand(v, row)
	}
	for _, w := range v.Whens {
		when, err := e.evalExpr(w.When, row)
		if err != nil {
			return nil, err
		}
		if toBool(when) {
			return e.evalExpr(w.Then, row)
		}
	}
	return e.evalCaseElse(v, row)
}

func (e *Engine) evalCaseWithOperand(v *sql.CaseExpr, row Row) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	// Simple CASE uses = semantics (SQLite): a NULL operand never matches,
	// even a NULL WHEN (CASE NULL WHEN NULL → ELSE). Non-NULL operands are
	// compared with util.CompareValues.
	if operand != nil {
		for _, w := range v.Whens {
			when, err := e.evalExpr(w.When, row)
			if err != nil {
				return nil, err
			}
			if util.CompareValues(operand, when) == 0 {
				return e.evalExpr(w.Then, row)
			}
		}
	}
	return e.evalCaseElse(v, row)
}

func (e *Engine) evalCaseElse(v *sql.CaseExpr, row Row) (interface{}, error) {
	if v.Else != nil {
		return e.evalExpr(v.Else, row)
	}
	return nil, nil
}

func (e *Engine) evalCastExpr(v *sql.CastExpr, row Row) (result interface{}, err error) {
	val, evalErr := e.evalExpr(v.Operand, row)
	if evalErr != nil {
		return nil, evalErr
	}
	if val == nil {
		return nil, nil
	}
	// Unwrap ColumnValue affinity wrappers so the CAST operates on the raw value.
	val = util.UnwrapColumnValue(val)
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
		switch x := val.(type) {
		case int64:
			return x, nil
		case float64:
			return int64(x), nil
		case string:
			// SQLite: CAST(text AS INTEGER) parses only the leading integer
			// prefix; an exponent or decimal part is ignored. E.g.
			// CAST('123e+5' AS INTEGER) is 123, not 12300000.
			t := strings.TrimSpace(x)
			end := 0
			if end < len(t) && (t[end] == '+' || t[end] == '-') {
				end++
			}
			for end < len(t) && t[end] >= '0' && t[end] <= '9' {
				end++
			}
			if end > 0 {
				if i, err := strconv.ParseInt(t[:end], 10, 64); err == nil {
					return i, nil
				}
				// Out of int64 range: SQLite clamps to the max/min integer.
				if t[0] == '-' {
					return int64(math.MinInt64), nil
				}
				return int64(math.MaxInt64), nil
			}
			return int64(0), nil
		default:
			return int64(0), nil
		}
	case "REAL", "FLOAT", "DOUBLE":
		switch x := val.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case string:
			// SQLite: CAST(text AS REAL) parses the text as a number,
			// accepting a leading numeric prefix and ignoring trailing
			// garbage (sqlite3AtoF). E.g. CAST(' 876xyz' AS REAL) is 876.0.
			if f, ok := parseNumericPrefix(x); ok {
				return f, nil
			}
			return float64(0), nil
		default:
			return float64(0), nil
		}
	case "TEXT":
		// CAST(blob AS TEXT) decodes the blob bytes as UTF-8 text (SQLite
		// does a byte copy; the result is a text value). fmt.Sprintf("%v")
		// on a []byte would render "[104 105]", so convert explicitly.
		if b, ok := val.([]byte); ok {
			return string(b), nil
		}
		return fmt.Sprintf("%v", val), nil
	case "NUMERIC":
		// SQLite: CAST(x AS NUMERIC) coerces text to a number; non-numeric
		// text becomes 0. A float64 input stays float64 (CAST(4.0 AS NUMERIC)
		// is 4.0, not 4). TEXT input that parses to a whole number returns
		// INTEGER (CAST('123e+5' AS NUMERIC) is 12300000).
		switch x := val.(type) {
		case int64:
			return x, nil
		case float64:
			return x, nil
		case string:
			t := strings.TrimSpace(x)
			if i, err := strconv.ParseInt(t, 10, 64); err == nil {
				return i, nil
			}
			if f, err := strconv.ParseFloat(t, 64); err == nil {
				if f == float64(int64(f)) {
					return int64(f), nil
				}
				return f, nil
			}
			return int64(0), nil
		default:
			return int64(0), nil
		}
	default:
		return val, nil
	}
}

func evalNumericLit(v *sql.NumericLit) (interface{}, error) {
	if v.Cached() != nil {
		return v.Cached(), nil
	}
	// Hex literals: SQLite rejects any whose unsigned magnitude exceeds
	// MaxInt64 (0x7fff...), reporting "hex literal too big: <literal>" with
	// the sign folded in reversed by the parser (e.g. "-0x08000000000000000"
	// is magnitude 2^63 and is rejected even though -2^63 fits in int64).
	if isHexLiteral(v.Value) && v.Value != "" {
		mag := v.Value
		if mag[0] == '+' || mag[0] == '-' {
			mag = mag[1:]
		}
		if u, err := strconv.ParseUint(mag, 0, 64); err == nil {
			if u > math.MaxInt64 {
				return nil, fmt.Errorf("hex literal too big: %s", v.Value)
			}
			i := int64(u)
			if v.Value[0] == '-' {
				i = -i
			}
			v.SetCached(i)
			return i, nil
		}
		return nil, fmt.Errorf("hex literal too big: %s", v.Value)
	}
	// Try base 0 first (auto-detect for hex literals like 0x...)
	if i, err := strconv.ParseInt(v.Value, 0, 64); err == nil {
		v.SetCached(i)
		return i, nil
	}
	if f, err := strconv.ParseFloat(v.Value, 64); err == nil {
		v.SetCached(f)
		return f, nil
	}
	v.SetCached(v.Value)
	return v.Value, nil
}

// isHexLiteral reports whether s is a hexadecimal integer literal
// (optionally with a leading + or - sign), e.g. "0x1A" or "-0xFF".
func isHexLiteral(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	return i+2 <= len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') && i+2 < len(s)
}

func (e *Engine) evalColumnRef(v *sql.ColumnRef, row Row) (interface{}, error) {
	// TRUE/FALSE keywords are boolean literals (1/0), not column references.
	// The parser represents them as ColumnRef{Name:"TRUE"} / {Name:"FALSE"}.
	if v.Table == "" && strings.EqualFold(v.Name, "TRUE") {
		return int64(1), nil
	}
	if v.Table == "" && strings.EqualFold(v.Name, "FALSE") {
		return int64(0), nil
	}
	if v.Name == "*" {
		// RETURNING rejects table-qualified wildcards ("t1.*").
		if e.returningStrict && v.Table != "" {
			return nil, fmt.Errorf("RETURNING may not use \"%s.*\" wildcards", v.Table)
		}
		return "*", nil
	}
	// Qualified column reference: check qualified name first
	if v.Table != "" {
		// Strip a schema prefix (main./temp./aux1.) from the qualifier so
		// "main.txx.a" resolves against the "txx.a" key in the row map, and
		// try both the full qualifier ("main.t4.a") and the stripped form
		// ("t4.a") since join row maps may store either.
		fullQual := v.Table
		tableQual := v.Table
		if dot := strings.Index(tableQual, "."); dot >= 0 {
			tableQual = tableQual[dot+1:]
		}
		// Try table-qualified key in the current row (e.g., "t1.a")
		if row != nil {
			if val, ok := row.Get(fullQual + "." + v.Name); ok {
				return val, nil
			}
			if val, ok := row.Get(tableQual + "." + v.Name); ok {
				return val, nil
			}
			// Qualified rowid alias (t1.rowid, t1._rowid_, t1.oid) resolves
			// against the qualified rowid key stored for joined rows.
			if isRowIDName(v.Name) {
				if val, ok := row.Get(fullQual + ".rowid"); ok {
					return val, nil
				}
				if val, ok := row.Get(tableQual + ".rowid"); ok {
					return val, nil
				}
			}
			// If the qualified key is not found and the qualifier matches the
			// table currently being scanned, resolve via unqualified name.
			// Row maps store unqualified column names, so "t1.a" in a query
			// scanning table t1 resolves to row["a"]. Join result maps store
			// qualified keys (t1.a), so the fallback must NOT apply there:
			// t4.id in a joined row must resolve exactly, not to the merged
			// id column.
			if e.currentScanTable != "" && strings.EqualFold(tableQual, e.currentScanTable) && !rowHasQualifiedKeys(row) {
				if val, ok := row.Get(v.Name); ok {
					return val, nil
				}
			}
			// Same for DML (INSERT/UPDATE) rows: a table-qualified reference in
			// a CHECK/default expression (e.g. CHECK (5 IN (false.false)))
			// resolves against the row's unqualified column keys.
			if e.currentDMLTable != "" && strings.EqualFold(tableQual, e.currentDMLTable) {
				if val, ok := row.Get(v.Name); ok {
					return val, nil
				}
			}
		}
		// Check trigger NEW/OLD rows
		if strings.EqualFold(v.Table, "new") && e.triggerNewRow != nil {
			if val, ok := e.triggerNewRow.Get(v.Name); ok {
				return val, nil
			}
		}
		if strings.EqualFold(v.Table, "old") && e.triggerOldRow != nil {
			if val, ok := e.triggerOldRow.Get(v.Name); ok {
				return val, nil
			}
		}
		// Fallback to outer rows for correlated references (qualified)
		for _, outer := range e.outerRowsForResolution() {
			if outer == nil {
				continue
			}
			// Try qualified name first (e.g., "t1.a")
			if val, ok := outer.Get(v.Table + "." + v.Name); ok {
				return val, nil
			}
			// If not found, try unqualified name (the outer row may store
			// column values without table prefix, e.g., "a" instead of "t1.a")
			if val, ok := outer.Get(v.Name); ok {
				return val, nil
			}
		}
		// RETURNING evaluates expressions against the statement's row with strict
		// column resolution: unknown columns are errors ("no such column"), and a
		// qualifier must name the modified table (not NEW/OLD, an alias, or a
		// different table). Inside a subquery scan (currentScanTable != "") the
		// scan's own row resolution already succeeded or fell through, so strict
		// mode only applies at the RETURNING row level.
		if e.returningStrict && e.currentScanTable == "" {
			if strings.EqualFold(v.Table, e.returningTable) {
				if row != nil {
					if val, ok := row.Get(v.Name); ok {
						return val, nil
					}
					if isRowIDName(v.Name) {
						if val, ok := row.Get("rowid"); ok {
							return val, nil
						}
					}
				}
			}
			return nil, fmt.Errorf("no such column: %s.%s", v.Table, v.Name)
		}
		return nil, nil
	}
	// Unqualified: check short name
	if row != nil {
		if val, ok := row.Get(v.Name); ok {
			return val, nil
		}
		// Unqualified rowid/_rowid_/oid resolve to the scanned row's rowid.
		if isRowIDName(v.Name) {
			if val, ok := row.Get("rowid"); ok {
				return val, nil
			}
		}
	}
	// Fallback to outer rows for correlated references (unqualified)
	for _, outer := range e.outerRowsForResolution() {
		if outer == nil {
			continue
		}
		if val, ok := outer.Get(v.Name); ok {
			return val, nil
		}
	}
	// Output-column aliases: when the name is not a table column, SQLite
	// resolves an unqualified reference to the SELECT-list alias expression
	// (e.g. "SELECT a AS x ... WHERE x>3" → WHERE evaluates the expression a).
	// A guard prevents infinite recursion when an alias expression refers back
	// to its own name (e.g. "SELECT x AS x" with no table column x).
	if !v.Quoted {
		if expr, ok := e.resolveAliasRef(v.Name); ok {
			name := strings.ToLower(v.Name)
			if e.aliasResolving == nil {
				e.aliasResolving = make(map[string]bool)
			}
			if e.aliasResolving[name] {
				return nil, nil
			}
			e.aliasResolving[name] = true
			val, err := e.evalExpr(expr, row)
			delete(e.aliasResolving, name)
			return val, err
		}
	}
	// RETURNING strict resolution: an unqualified reference must name a column
	// of the modified table (or rowid/oid/_rowid_). Unknown columns are errors.
	if e.returningStrict && e.currentScanTable == "" {
		if isRowIDName(v.Name) && row != nil {
			if val, ok := row.Get("rowid"); ok {
				return val, nil
			}
		}
		return nil, fmt.Errorf("no such column: %s", v.Name)
	}
	// SQLite double-quoted-string (DQS) resolution: if an unqualified
	// double-quoted identifier does not match any column, and DQS is enabled
	// for DML, it becomes a string literal. (CREATE "foo" CHECK (c!="bar")
	// stores the raw SQL verbatim; at DML evaluation the "bar" resolves to a
	// string.) When DQS_DML is off, the unresolved reference is an error.
	if v.Quoted {
		if e.dqsDML {
			return v.Name, nil
		}
		// DQS disabled for DML: an unresolved double-quoted identifier is an
		// error naming the identifier and hinting at single-quoted strings
		// (matches SQLite's resolve.c message).
		return nil, fmt.Errorf("no such column: \"%s\" - should this be a string literal in single-quotes?", v.Name)
	}
	return nil, nil
}

// isRowIDName reports whether a column name is one of the rowid aliases.
func isRowIDName(name string) bool {
	return strings.EqualFold(name, "rowid") || strings.EqualFold(name, "_rowid_") || strings.EqualFold(name, "oid")
}

// rowHasQualifiedKeys reports whether a row map contains any table-qualified
// keys ("t1.a"), indicating it came from a join result rather than a bare
// single-table scan.
func rowHasQualifiedKeys(row Row) bool {
	if row == nil {
		return false
	}
	if rm, ok := row.(RowMap); ok {
		for k := range rm {
			if strings.Contains(k, ".") {
				return true
			}
		}
	}
	if sr, ok := row.(*structRow); ok && sr.index != nil {
		for k := range sr.index {
			if strings.Contains(k, ".") {
				return true
			}
		}
	}
	return false
}

// outerRowsForResolution returns the correlated-scope rows visible to the
// current evaluation, innermost first: the current outerRow followed by the
// stack of enclosing outer rows (for multi-level correlated subqueries).
func (e *Engine) outerRowsForResolution() []Row {
	rows := make([]Row, 0, len(e.outerRowStack)+1)
	if e.outerRow != nil {
		rows = append(rows, e.outerRow)
	}
	for i := len(e.outerRowStack) - 1; i >= 0; i-- {
		rows = append(rows, e.outerRowStack[i])
	}
	return rows
}

// pushOuterRow sets row as the current outer scope for correlated subquery
// resolution, preserving the previous scope on a stack.
func (e *Engine) pushOuterRow(row Row) {
	e.outerRowStack = append(e.outerRowStack, e.outerRow)
	e.outerRow = row
}

// popOuterRow restores the outer scope saved by pushOuterRow.
func (e *Engine) popOuterRow() {
	n := len(e.outerRowStack)
	if n == 0 {
		e.outerRow = nil
		return
	}
	e.outerRow = e.outerRowStack[n-1]
	e.outerRowStack = e.outerRowStack[:n-1]
}

func (e *Engine) evalBinaryOp(v *sql.BinaryOp, row Row) (interface{}, error) {
	// Handle MATCH and NOT MATCH for FTS virtual tables
	if v.Operator == "MATCH" || v.Operator == "NOT MATCH" {
		return e.evalMatchOp(v, row)
	}

	left, err := e.evalExpr(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := e.evalExpr(v.Right, row)
	if err != nil {
		return nil, err
	}

	// Row-value vs subquery: (a,b,c) OP (SELECT x,y,z ...). The subquery's
	// result row (all columns) forms the right row value; evalExpr above
	// returned only its first column, so re-evaluate the subquery in full
	// when the other side is a row value.
	if _, lIsRow := left.([]interface{}); lIsRow {
		if subq, ok := v.Right.(*sql.Subquery); ok {
			rows, err := e.evalSubqueryRows(subq, row)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				right = nil
			} else {
				right = rows[0]
			}
		}
	}
	if _, rIsRow := right.([]interface{}); rIsRow {
		if subq, ok := v.Left.(*sql.Subquery); ok {
			rows, err := e.evalSubqueryRows(subq, row)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				left = nil
			} else {
				left = rows[0]
			}
		}
	}

	// Most operators return NULL when either operand is NULL.
	// AND/OR need Kleene logic (handled in evalArithmeticOp).
	// IS / IS NOT are NULL-safe: NULL IS NULL is true.
	if v.Operator != "AND" && v.Operator != "OR" && v.Operator != "IS" && v.Operator != "IS NOT" {
		if isSQLNull(left) || isSQLNull(right) {
			return nil, nil
		}
	}
	if v.Operator == "LIKE" && v.Escape != "" {
		return likeValuesWithEscape(left, right, v.Escape), nil
	}
	if v.Operator == "IS" {
		// Row-value IS: NULL-safe element-wise equality, e.g.
		// (a,b,c) IS (x,y,z). Delegate when either side is a row value.
		if _, lok := left.([]interface{}); lok {
			return evalRowValueIs(v.Operator, left, right)
		}
		if _, rok := right.([]interface{}); rok {
			return evalRowValueIs(v.Operator, left, right)
		}
		// Unwrap ColumnValue wrappers so IS NULL works on joined values.
		left = util.UnwrapColumnValue(left)
		right = util.UnwrapColumnValue(right)
		if left == nil && right == nil {
			return int64(1), nil
		}
		if left == nil || right == nil {
			return int64(0), nil
		}
		return boolToInt(compareValuesWithCollate(left, right) == 0), nil
	}
	if v.Operator == "IS NOT" {
		// Row-value IS NOT: NULL-safe element-wise inequality.
		if _, lok := left.([]interface{}); lok {
			return evalRowValueIs(v.Operator, left, right)
		}
		if _, rok := right.([]interface{}); rok {
			return evalRowValueIs(v.Operator, left, right)
		}
		left = util.UnwrapColumnValue(left)
		right = util.UnwrapColumnValue(right)
		if left == nil && right == nil {
			return int64(0), nil
		}
		if left == nil || right == nil {
			return int64(1), nil
		}
		return boolToInt(compareValuesWithCollate(left, right) != 0), nil
	}
	return evalBinaryOpValues(v.Operator, left, right)
}

// evalRowValueIs implements NULL-safe row-value IS / IS NOT comparison.
// Each element pair is compared with IS semantics (NULL IS NULL is true);
// the whole row value is equal when every element is IS-equal.
func evalRowValueIs(op string, left, right interface{}) (interface{}, error) {
	lv, lok := left.([]interface{})
	rv, rok := right.([]interface{})
	if !lok || !rok {
		return nil, fmt.Errorf("row value misused")
	}
	if len(lv) != len(rv) {
		return nil, fmt.Errorf("row value misused")
	}
	equal := true
	for i := range lv {
		lv0, _ := extractValue(lv[i])
		rv0, _ := extractValue(rv[i])
		l := util.UnwrapColumnValue(lv0)
		r := util.UnwrapColumnValue(rv0)
		if l == nil && r == nil {
			continue // IS-equal
		}
		if l == nil || r == nil {
			equal = false
			break
		}
		if compareValuesWithCollate(lv[i], rv[i]) != 0 {
			equal = false
			break
		}
	}
	if op == "IS NOT" {
		equal = !equal
	}
	return boolToInt(equal), nil
}

// evalMatchOp evaluates a MATCH or NOT MATCH expression for FTS virtual tables.
func (e *Engine) evalMatchOp(v *sql.BinaryOp, row Row) (interface{}, error) {
	// Evaluate the right side (query string) normally
	right, err := e.evalExpr(v.Right, row)
	if err != nil {
		return nil, err
	}
	if right == nil {
		return nil, nil
	}
	queryStr, ok := right.(string)
	if !ok {
		return int64(0), nil
	}

	// Look up the FTS table context
	tableName := e.currentFTSMatch
	if tableName == "" {
		// Try to infer the table from the left side ColumnRef
		if colRef, ok := v.Left.(*sql.ColumnRef); ok && colRef.Table != "" {
			tableName = colRef.Table
		}
		if tableName == "" {
			return int64(0), nil
		}
	}

	ftsTable, ok := e.ftsTables[tableName]
	if !ok {
		return int64(0), nil
	}

	// Get the rowid from the current row
	rowidVal := getRowID(row)
	if rowidVal == 0 {
		// Try to get rowid from row
		if v, ok := row.Get("rowid"); ok {
			if r, ok := util.UnwrapColumnValue(v).(int64); ok {
				rowidVal = r
			}
		}
	}

	if rowidVal <= 0 {
		return int64(0), nil
	}

	// Evaluate the match against the FTS index
	matched, err := ftsTable.MatchQuery(rowidVal, queryStr)
	if err != nil {
		// If query parsing fails, treat as no match (matches SQLite behavior)
		return int64(0), nil
	}

	if v.Operator == "NOT MATCH" {
		return boolToInt(!matched), nil
	}
	return boolToInt(matched), nil
}

// getRowID extracts the rowid from a Row value.
func getRowID(row Row) int64 {
	if row == nil {
		return 0
	}
	if v, ok := row.Get("rowid"); ok {
		if r, ok := util.UnwrapColumnValue(v).(int64); ok {
			return r
		}
	}
	return 0
}

// collatedValue wraps a value with a collation name for COLLATE support.
type collatedValue struct {
	value     interface{}
	collation string
	// explicit is true when the collation came from an explicit COLLATE
	// operator in the SQL text (expr COLLATE name), as opposed to a column's
	// declared COLLATE clause. SQLite's collation resolution gives an
	// explicit COLLATE on either operand precedence over any column
	// collation.
	explicit bool
}

// extractValue extracts the raw value and collation from a potentially collated value.
func extractValue(v interface{}) (interface{}, string) {
	if cv, ok := v.(*collatedValue); ok {
		return cv.value, cv.collation
	}
	return v, ""
}

// unwrapCollatedValue extracts the raw value from a collatedValue wrapper.
// Used when a value flows to a result column, where the collation marker
// (a *collatedValue pointer) must not leak into the output. Since a COLLATE
// expression wraps its operand (which may itself be a *ColumnValue), this
// also unwraps the inner ColumnValue to produce the raw scalar. A top-level
// ColumnValue (e.g. the affinity wrapper on a CAST result) is unwrapped too.
func unwrapCollatedValue(v interface{}) interface{} {
	if cv, ok := v.(*collatedValue); ok {
		return util.UnwrapColumnValue(cv.value)
	}
	return util.UnwrapColumnValue(v)
}

// compareValuesWithCollate compares two values using the collation from either side.
func compareValuesWithCollate(left, right interface{}) int {
	lv, lc := extractValue(left)
	rv, rc := extractValue(right)
	// SQLite collation resolution for a binary comparison (datatype3.html):
	// 1. An explicit COLLATE clause on either operand (the COLLATE operator)
	//    wins over any column collation: `a = 'ABC' COLLATE BINARY` compares
	//    BINARY even when column a is declared COLLATE NOCASE, and
	//    `a COLLATE BINARY = 'ABC'` likewise. If both sides are explicit,
	//    the left one wins (matching sqlite3ExprCollSeq).
	// 2. Otherwise, if the LEFT operand is a column, its column collation is
	//    used — defaulting to BINARY when the column has no COLLATE (a plain
	//    ColumnValue wrapper). A column on the left masks a collation on
	//    the right: `t2.y > t1.b` (b COLLATE NOCASE) compares BINARY
	//    because t2.y is a column without collation.
	// 3. Only when the left operand is NOT a column (literal/expression)
	//    does the right operand's column collation apply, e.g. `'abc' > b`.
	if le, ok := left.(*collatedValue); ok && le.explicit {
		return util.CompareValuesCollate(lv, rv, le.collation)
	}
	if re, ok := right.(*collatedValue); ok && re.explicit {
		return util.CompareValuesCollate(lv, rv, re.collation)
	}
	leftIsColumn := isColumnValue(left)
	if leftIsColumn {
		return util.CompareValuesCollate(lv, rv, lc)
	}
	collation := lc
	if collation == "" {
		collation = rc
	}
	return util.CompareValuesCollate(lv, rv, collation)
}

// isColumnValue reports whether v is a column value (a *util.ColumnValue
// wrapper, possibly wrapped in a collatedValue marker). Used by
// compareValuesWithCollate to apply SQLite's left-operand collation rule.
func isColumnValue(v interface{}) bool {
	if cv, ok := v.(*collatedValue); ok {
		_, isCol := cv.value.(*util.ColumnValue)
		return isCol
	}
	_, isCol := v.(*util.ColumnValue)
	return isCol
}

// extractCollatedValues extracts raw values from collatedValue wrappers
// for operators that don't need collation propagation.
// Comparison operators keep the collatedValue for compareValuesWithCollate.
// || keeps collatedValue for evalConcat to propagate collation.
func extractCollatedValues(op string, left, right interface{}) (interface{}, interface{}) {
	if op == "=" || op == "<>" || op == "!=" || op == "<" || op == ">" || op == "<=" || op == ">=" || op == "||" {
		return left, right
	}
	l, _ := extractValue(left)
	r, _ := extractValue(right)
	return l, r
}

// evalRowValueCompare implements SQLite row-value comparison:
// (a,b,c) OP (x,y,z) compares element-wise lexicographically. One side is a
// row value ([]interface{}); the other must be a row value of the same arity,
// otherwise an error is raised (SQLite "row value misused" for scalar vs row,
// and "row value comparison with different number of terms" for arity
// mismatch). NULL elements propagate NULL like scalar comparisons: if the
// elements compared so far are equal and one is NULL, the result is NULL.
func evalRowValueCompare(op string, lv []interface{}, right interface{}) (interface{}, error) {
	rv, ok := right.([]interface{})
	if !ok {
		return nil, fmt.Errorf("row value misused")
	}
	if len(lv) != len(rv) {
		return nil, fmt.Errorf("row value misused")
	}
	cmp := 0
	for i := range lv {
		// Pass the element values as-is to compareValuesWithCollate, which
		// extracts column collations from collatedValue wrappers (a column
		// declared COLLATE NOCASE must apply to its row-value elements).
		l := lv[i]
		r := rv[i]
		lv0, _ := extractValue(l)
		rv0, _ := extractValue(r)
		if util.UnwrapColumnValue(lv0) == nil || util.UnwrapColumnValue(rv0) == nil {
			// SQLite: NULL only matters at the first position where the
			// comparison is still undecided (all previous elements equal).
			// If an earlier element already ordered the pair, the result is
			// decided and NULL is ignored.
			if cmp == 0 {
				return nil, nil
			}
			break
		}
		cmp = compareValuesWithCollate(l, r)
		if cmp != 0 {
			break
		}
	}
	switch op {
	case "=", "==":
		return boolToInt(cmp == 0), nil
	case "<>", "!=":
		return boolToInt(cmp != 0), nil
	case "<":
		return boolToInt(cmp < 0), nil
	case ">":
		return boolToInt(cmp > 0), nil
	case "<=":
		return boolToInt(cmp <= 0), nil
	case ">=":
		return boolToInt(cmp >= 0), nil
	default:
		return nil, fmt.Errorf("row value misused")
	}
}

func evalBinaryOpValues(op string, left, right interface{}) (interface{}, error) {
	// Row-value comparison: (a,b,c) OP (x,y,z) compares element-wise
	// lexicographically with SQLite's row-value semantics. A row value
	// compared with a scalar (or a row value of different arity) is an
	// error ("row value misused" / "row value comparison with different
	// number of terms"). Note: a row value is only valid in comparison and
	// IN contexts; comparing via =, <>, <, >, <=, >= is allowed.
	if lv, lok := left.([]interface{}); lok {
		return evalRowValueCompare(op, lv, right)
	}
	if rv, rok := right.([]interface{}); rok {
		return evalRowValueCompare(op, rv, left)
	}
	// Extract collation-wrapped values for non-comparison operators.
	// Comparison operators use compareValuesWithCollate which handles this internally.
	// For || (concatenation), we preserve collation through evalConcat.
	left, right = extractCollatedValues(op, left, right)
	switch op {
	case "=":
		// When comparing a TEXT column value with a value that has no
		// column affinity (expression result, view value) and the actual
		// types differ (TEXT vs numeric), SQLite treats them as not equal.
		// The standard comparison path's NONE string comparison would
		// incorrectly report them as equal after converting numeric to text.
		if !typesMatchForEquality(left, right) {
			return int64(0), nil
		}
		return boolToInt(compareValuesWithCollate(left, right) == 0), nil
	case "<>", "!=":
		if !typesMatchForEquality(left, right) {
			return int64(1), nil
		}
		return boolToInt(compareValuesWithCollate(left, right) != 0), nil
	case "<":
		return boolToInt(compareValuesWithCollate(left, right) < 0), nil
	case ">":
		return boolToInt(compareValuesWithCollate(left, right) > 0), nil
	case "<=":
		return boolToInt(compareValuesWithCollate(left, right) <= 0), nil
	case ">=":
		return boolToInt(compareValuesWithCollate(left, right) >= 0), nil
	case "LIKE":
		return boolToInt(likeValues(left, right)), nil
	case "GLOB":
		return boolToInt(globValues(left, right)), nil
	case "REGEXP":
		b, err := regexpValues(left, right)
		if err != nil {
			return nil, err
		}
		return boolToInt(b), nil
	case "NOT LIKE":
		return boolToInt(!likeValues(left, right)), nil
	case "NOT GLOB":
		return boolToInt(!globValues(left, right)), nil
	case "NOT REGEXP":
		b, err := regexpValues(left, right)
		if err != nil {
			return nil, err
		}
		return boolToInt(!b), nil
	case "MATCH":
		// FTS not supported — MATCH always returns 0
		return int64(0), nil
	case "NOT MATCH":
		// FTS not supported — NOT MATCH always returns 1
		return int64(1), nil
	case "->", "->>":
		// JSON extract operators — not supported, return NULL
		return nil, nil
	case "COLLATE":
		// COLLATE operator — returns the left value but marks it with
		// the collation name. Comparison operators check for this
		// marker and apply the correct collation. The explicit flag
		// distinguishes this operator from a column's declared COLLATE
		// clause, so an explicit COLLATE on either operand wins.
		if rightStr, ok := right.(string); ok {
			switch strings.ToUpper(rightStr) {
			case "", "BINARY", "NOCASE", "RTRIM":
				return &collatedValue{value: left, collation: rightStr, explicit: true}, nil
			default:
				return nil, fmt.Errorf("no such collation sequence: %s", rightStr)
			}
		}
		return left, nil
	default:
		return evalArithmeticOp(op, left, right)
	}
}

// boolToInt converts a boolean to an integer (0 or 1) matching SQLite behavior.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// typesMatchForEquality checks whether two values should be considered for
// equality comparison. Returns false when one value has TEXT column affinity
// and the other has no column affinity, with different actual storage types
// (TEXT vs numeric). In this case SQLite treats them as not equal for = and !=.
func typesMatchForEquality(left, right interface{}) bool {
	lAff := util.ColumnAffinity(left)
	rAff := util.ColumnAffinity(right)
	// Only applies when one side has TEXT affinity and the other has none
	if !((lAff == 'T' && rAff == 0) || (rAff == 'T' && lAff == 0)) {
		return true
	}
	lv := util.UnwrapColumnValue(left)
	rv := util.UnwrapColumnValue(right)
	// Check if one is TEXT and the other is numeric (INTEGER or REAL)
	_, lStr := lv.(string)
	_, rStr := rv.(string)
	_, lInt := lv.(int64)
	_, rInt := rv.(int64)
	_, lFloat := lv.(float64)
	_, rFloat := rv.(float64)
	lNum := lInt || lFloat
	rNum := rInt || rFloat
	// TEXT vs numeric → check if the string can be converted to a number.
	// If it can (e.g., '1' vs 1), allow the comparison so that
	// compareValuesWithCollate handles the numeric conversion normally.
	// If it cannot (e.g., 'abc' vs 1), treat as not a type match.
	if (lStr && rNum) || (rStr && lNum) {
		// Try to parse the string as a number.
		var str string
		if lStr {
			str = lv.(string)
		} else {
			str = rv.(string)
		}
		str = strings.TrimSpace(str)
		if _, err := strconv.ParseFloat(str, 64); err != nil {
			return false // non-numeric string vs numeric → not a match
		}
		// Numeric string → allow comparison
	}
	return true
}

func globValues(str, pattern interface{}) bool {
	s := util.SQLiteValueString(util.UnwrapColumnValue(str))
	p := util.SQLiteValueString(util.UnwrapColumnValue(pattern))
	return function.GlobMatch(s, p)
}

func regexpValues(str, pattern interface{}) (bool, error) {
	s := util.SQLiteValueString(util.UnwrapColumnValue(str))
	p := util.SQLiteValueString(util.UnwrapColumnValue(pattern))
	re, err := util.CompileRegexp(p)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
}

func evalArithmeticOp(op string, left, right interface{}) (interface{}, error) {
	// Unwrap BlobColumnValue so arithmetic functions see the base value.
	left = util.UnwrapColumnValue(left)
	right = util.UnwrapColumnValue(right)
	switch op {
	case "+":
		return evalAdd(left, right)
	case "-":
		if left == nil || right == nil {
			return nil, nil
		}
		return subValues(left, right)
	case "*":
		if left == nil || right == nil {
			return nil, nil
		}
		return mulValues(left, right)
	case "/":
		if left == nil || right == nil {
			return nil, nil
		}
		return divValues(left, right)
	case "%":
		if left == nil || right == nil {
			return nil, nil
		}
		return modValues(left, right)
	case "&":
		if left == nil || right == nil {
			return nil, nil
		}
		return bitwiseAnd(left, right)
	case "|":
		if left == nil || right == nil {
			return nil, nil
		}
		return bitwiseOr(left, right)
	case "<<":
		if left == nil || right == nil {
			return nil, nil
		}
		return shiftLeft(left, right)
	case ">>":
		if left == nil || right == nil {
			return nil, nil
		}
		return shiftRight(left, right)
	case "||":
		return evalConcat(left, right)
	case "AND":
		return kleeneAnd(left, right), nil
	case "OR":
		return kleeneOr(left, right), nil
	default:
		return nil, fmt.Errorf("unknown operator: %s", op)
	}
}

// kleeneAnd implements Kleene AND logic:
//
//	true  AND true  → true
//	false AND any   → false
//	any   AND false → false
//	true  AND NULL  → NULL
//	NULL  AND true  → NULL
//	NULL  AND NULL  → NULL
func evalAdd(left, right interface{}) (interface{}, error) {
	if left == nil || right == nil {
		return nil, nil
	}
	return addValues(left, right)
}

func evalConcat(left, right interface{}) (interface{}, error) {
	// Extract collation info from any collatedValue operands
	lv, lc := extractValue(left)
	rv, rc := extractValue(right)
	collation := lc
	if collation == "" {
		collation = rc
	}

	if lv == nil || rv == nil {
		return nil, nil
	}
	result, err := concatValues(lv, rv)
	if err != nil {
		return nil, err
	}
	// If either operand had a collation, wrap the result so comparison
	// operators can apply the collation correctly.
	if collation != "" {
		return &collatedValue{value: result, collation: collation}, nil
	}
	return result, nil
}

func kleeneAnd(left, right interface{}) interface{} {
	if isFalse(left) || isFalse(right) {
		return boolToInt(false)
	}
	if left == nil || right == nil {
		return nil
	}
	return boolToInt(true)
}

// kleeneOr implements Kleene OR logic:
//
//	true  OR any   → true
//	any   OR true  → true
//	false OR NULL  → NULL
//	NULL  OR false → NULL
//	false OR false → false
//	NULL  OR NULL  → NULL
func kleeneOr(left, right interface{}) interface{} {
	if isTrue(left) || isTrue(right) {
		return boolToInt(true)
	}
	if left == nil || right == nil {
		return nil
	}
	return boolToInt(false)
}

func isFalse(v interface{}) bool {
	if v == nil {
		return false
	}
	return !toBool(v)
}

func isTrue(v interface{}) bool {
	if v == nil {
		return false
	}
	return toBool(v)
}

func (e *Engine) evalUnaryOp(v *sql.UnaryOp, row Row) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if operand == nil {
		return nil, nil
	}
	// Unwrap BlobColumnValue so arithmetic operators see the base value.
	operand = util.UnwrapColumnValue(operand)
	switch v.Operator {
	case "-":
		return negateValue(operand)
	case "+":
		// Unary plus is a no-op in SQLite — it returns the operand value
		// unchanged (no numeric conversion). This is used in ORDER BY +col
		// to bypass index-based sorting while preserving the original value
		// and type.
		return operand, nil
	case "NOT":
		return boolToInt(!toBool(operand)), nil
	case "~":
		// Bitwise NOT: ~x = ^(int64(x))
		switch v := operand.(type) {
		case int64:
			return ^v, nil
		case float64:
			return ^int64(v), nil
		default:
			// Try numeric conversion (SQLite: ~'text' = ~0 = -1)
			return ^int64(0), nil
		}
	default:
		return nil, nil
	}
}

func (e *Engine) evalIsNull(v *sql.IsNull, row Row) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	// Unwrap ColumnValue so we check for actual NULL, not just wrapper nil
	operand = util.UnwrapColumnValue(operand)
	return boolToInt(operand == nil), nil
}

func (e *Engine) evalIsNotNull(v *sql.IsNotNull, row Row) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	// Unwrap ColumnValue so we check for actual NULL, not just wrapper nil
	operand = util.UnwrapColumnValue(operand)
	return boolToInt(operand != nil), nil
}

func (e *Engine) evalIsTrue(v *sql.IsTrue, row Row) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	result := isTrue(operand)
	if v.Negated {
		result = !result
	}
	if result {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *Engine) evalIsFalse(v *sql.IsFalse, row Row) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	result := isFalse(operand)
	if v.Negated {
		result = !result
	}
	if result {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *Engine) evalIsDistinctFrom(v *sql.IsDistinctFrom, row Row) (interface{}, error) {
	left, err := e.evalExpr(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := e.evalExpr(v.Right, row)
	if err != nil {
		return nil, err
	}
	// IS DISTINCT FROM: 0 if equal (including NULL==NULL), 1 otherwise
	lNull := isSQLNull(left)
	rNull := isSQLNull(right)
	if lNull && rNull {
		return int64(0), nil
	}
	if lNull || rNull {
		return int64(1), nil
	}
	left = util.UnwrapColumnValue(left)
	right = util.UnwrapColumnValue(right)
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(0), nil
	}
	return int64(1), nil
}

func (e *Engine) evalIsNotDistinctFrom(v *sql.IsNotDistinctFrom, row Row) (interface{}, error) {
	left, err := e.evalExpr(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := e.evalExpr(v.Right, row)
	if err != nil {
		return nil, err
	}
	// IS NOT DISTINCT FROM: 1 if equal (including NULL==NULL), 0 otherwise
	lNull := isSQLNull(left)
	rNull := isSQLNull(right)
	if lNull && rNull {
		return int64(1), nil
	}
	if lNull || rNull {
		return int64(0), nil
	}
	left = util.UnwrapColumnValue(left)
	right = util.UnwrapColumnValue(right)
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *Engine) evalBetween(v *sql.Between, row Row) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if operand == nil {
		return nil, nil
	}
	low, err := e.evalExpr(v.Low, row)
	if err != nil {
		return nil, err
	}
	high, err := e.evalExpr(v.High, row)
	if err != nil {
		return nil, err
	}
	if low == nil || high == nil {
		// SQLite: any NULL bound makes the whole BETWEEN NULL.
		return nil, nil
	}
	result := compareValuesWithCollate(operand, low) >= 0 && compareValuesWithCollate(operand, high) <= 0
	if v.Negated {
		result = !result
	}
	return boolToInt(result), nil
}

func (e *Engine) evalInList(v *sql.InList, row Row) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if operand == nil {
		return nil, nil
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
	found := false
	sawNull := false // an unmatched list item evaluated to NULL
	for _, item := range v.List {
		// A subquery item in an IN list produces a set of rows. With a
		// row-value operand each row's full column set is the comparison
		// value; the subquery's column count must match the operand arity.
		if subq, ok := item.(*sql.Subquery); ok {
			res, err := e.evalSubqueryRows(subq, row)
			if err != nil {
				continue
			}
			for _, subRow := range res {
				if opIsRow {
					if len(subRow) != opArity {
						return nil, fmt.Errorf("sub-select returns %d columns - expected %d", len(subRow), opArity)
					}
					equal := true
					sawRowNull := false
					for i := range opRow {
						l, _ := extractValue(opRow[i])
						if util.UnwrapColumnValue(l) == nil || util.UnwrapColumnValue(subRow[i]) == nil {
							sawRowNull = true
							continue
						}
						if util.CompareValues(util.UnwrapColumnValue(l), util.UnwrapColumnValue(subRow[i])) != 0 {
							equal = false
							break
						}
					}
					if equal && sawRowNull {
						sawNull = true
					} else if equal {
						found = true
					}
				} else {
					// A scalar operand with a multi-column subquery is an error
					// (SQLite: "sub-select returns N columns - expected 1").
					if len(subRow) > 1 {
						return nil, fmt.Errorf("sub-select returns %d columns - expected 1", len(subRow))
					}
					if len(subRow) > 0 {
						if util.UnwrapColumnValue(subRow[0]) == nil {
							sawNull = true
						} else if util.CompareValues(operand, subRow[0]) == 0 {
							found = true
						}
					}
				}
			}
			continue
		}
		ival, err := e.evalExpr(item, row)
		if err != nil {
			continue
		}
		if ival == nil {
			// A NULL list item can only make the result unknown (NULL),
			// never a match. Track it for the final decision.
			sawNull = true
			continue
		}
		ivRow, ivIsRow := ival.([]interface{})
		if opIsRow && !ivIsRow {
			// A scalar item in a row-value IN list is treated as a 1-term
			// row value by SQLite and reports the arity mismatch (unless the
			// operand is also 1-term, in which case it compares as a scalar).
			if opArity != 1 {
				return nil, fmt.Errorf("IN(...) element has 1 term - expected %d", opArity)
			}
			ivIsRow = true
			ivRow = []interface{}{ival}
		}
		if !opIsRow && ivIsRow {
			return nil, fmt.Errorf("row value misused")
		}
		if opIsRow && ivIsRow && len(ivRow) != opArity {
			return nil, fmt.Errorf("IN(...) element has %d terms - expected %d", len(ivRow), opArity)
		}
		// Element-wise equality for row values; scalar CompareValues otherwise.
		// SQLite validates the arity of EVERY element (above) even after a
		// match is found, so the arity check is not short-circuited here.
		equal := false
		if opIsRow && ivIsRow {
			equal = true
			for i := range opRow {
				l, _ := extractValue(opRow[i])
				r, _ := extractValue(ivRow[i])
				if util.CompareValues(util.UnwrapColumnValue(l), util.UnwrapColumnValue(r)) != 0 {
					equal = false
					break
				}
			}
		} else {
			equal = util.CompareValues(operand, ival) == 0
		}
		if equal {
			found = true
			// Continue validating arity of remaining elements; the match is
			// remembered but later arity violations still raise errors.
			if opIsRow {
				continue
			}
			break
		}
	}
	if found {
		if v.Negated {
			return int64(0), nil
		}
		return int64(1), nil
	}
	// Not found: a NULL list item makes the result unknown (NULL);
	// otherwise the result is FALSE for IN and TRUE for NOT IN.
	if sawNull {
		return nil, nil
	}
	if v.Negated {
		return int64(1), nil
	}
	return int64(0), nil
}

// evalSubqueryRows executes a subquery and returns all result rows (each row
// is a []interface{} of the row's column values). Used for row-value IN
// subqueries where the full row, not just the first column, must be compared.
func (e *Engine) evalSubqueryRows(subq *sql.Subquery, row Row) ([][]interface{}, error) {
	e.pushOuterRow(row)
	defer e.popOuterRow()
	result := e.execSelect(subq.Select)
	if result.Error != nil {
		return nil, result.Error
	}
	return result.Rows, nil
}

func (e *Engine) evalBool(expr sql.Expr, row Row) (bool, error) {
	v, err := e.evalExpr(expr, row)
	if err != nil {
		return false, err
	}
	return toBool(v), nil
}

// affinityOfValue reports the affinity of a value for the test-only affinity()
// function: a ColumnValue wrapper carries the declared column affinity (from a
// table scan or materialized subquery/CTE column), while a bare value reports
// its storage class (the fallback for literals).
func affinityOfValue(v interface{}) string {
	if v == nil {
		return "none"
	}
	if cv, ok := v.(*util.ColumnValue); ok {
		switch cv.Affinity {
		case 'I':
			return "integer"
		case 'R':
			return "real"
		case 'T':
			return "text"
		case 'N':
			return "numeric"
		default:
			return "blob"
		}
	}
	switch v.(type) {
	case int64:
		return "integer"
	case float64:
		return "real"
	case string:
		return "text"
	case []byte:
		return "blob"
	default:
		return "none"
	}
}

func (e *Engine) evalFuncCall(f *sql.FuncCall, row Row) (interface{}, error) {
	// Engine-specific functions that need engine state
	upper := strings.ToUpper(f.Name)
	switch upper {
	case "CHANGES":
		return e.lastChanges, nil
	case "LAST_INSERT_ROWID":
		return e.lastRowID, nil
	case "RAISE":
		return e.evalRaiseFuncCall(f, row)
	case "AFFINITY":
		// Test-only affinity(X): reports the affinity of the column X refers
		// to (SQLite's column-affinity reports). The ColumnValue wrapper
		// carries the declared column affinity; evaluate WITHOUT unwrapping so
		// the function can see it. A non-column argument falls back to the
		// value's storage class.
		if len(f.Args) == 1 {
			v, err := e.evalExpr(f.Args[0], row)
			if err != nil {
				return nil, err
			}
			return affinityOfValue(v), nil
		}
		return nil, fmt.Errorf("function affinity expects 1 argument, got %d", len(f.Args))
	case "COUNTER":
		// Test-only counter(N) function (SQLite test1.c selectH_counter):
		// increments the engine counter by N and returns the new value.
		// The counter resets at the start of each statement (see Exec).
		amt := int64(1)
		if len(f.Args) > 0 {
			if v, err := e.evalExpr(f.Args[0], row); err == nil {
				amt = toIntValue(util.UnwrapColumnValue(v))
			}
		}
		e.counterVal += amt
		return e.counterVal, nil
	case "NONDETER":
		// Test-only nondeter() function (SQLite having.test): a non-
		// deterministic function that increments a counter per call and
		// returns counter%2. The counter resets at statement start so each
		// query begins from 0, matching the TCL harness's
		// `set ::nondeter_ret 0` before each query.
		e.nondeterVal++
		return e.nondeterVal % 2, nil
	}

	fn, ok := e.funcs.Find(f.Name)
	if !ok {
		return nil, fmt.Errorf("no such function: %s", f.Name)
	}

	// Nested aggregate inside a wrapper expression of an aggregate query
	// (e.g. round(avg(x),2)): evaluate over the aggregate row set rather
	// than the single per-row context.
	if fn.Type == function.TypeAggregate && e.aggRowMaps != nil {
		return e.evalAggFuncCall(f, e.aggRowMaps)
	}

	// ORDER BY is only allowed for aggregate functions
	if len(f.OrderBy) > 0 && fn.Type != function.TypeAggregate {
		return nil, fmt.Errorf("ORDER BY may not be used with non-aggregate %s()", f.Name)
	}

	args := make([]interface{}, len(f.Args))
	for i, arg := range f.Args {
		v, err := e.evalExpr(arg, row)
		if err != nil {
			return nil, err
		}
		// Unwrap BlobColumnValue so functions see the raw value.
		v = util.UnwrapColumnValue(v)
		// Unwrap collatedValue wrappers (column values with a collation) so
		// functions receive the raw scalar, not the collation marker.
		if cv, ok := v.(*collatedValue); ok {
			v = util.UnwrapColumnValue(cv.value)
		}
		// For UTF-16 encoding, truncate odd-length blobs (ignore last byte)
		// to ensure valid UTF-16 byte sequences. (SQLite ticket 9eda2697f5cc1aba)
		if b, ok := v.([]byte); ok && len(b)%2 == 1 {
			if strings.HasPrefix(e.encoding, "UTF-16") {
				v = b[:len(b)-1]
			}
		}
		args[i] = v
	}

	if len(args) < fn.MinArgs || (fn.MaxArgs > 0 && len(args) > fn.MaxArgs) {
		return nil, fmt.Errorf("function %s expects %d-%d arguments, got %d", f.Name, fn.MinArgs, fn.MaxArgs, len(args))
	}

	if fn.Type == function.TypeScalar {
		// sqlite_rename_quotefix needs schema access (it resolves double-quoted
		// tokens against table columns); route it through the engine.
		if strings.EqualFold(f.Name, "sqlite_rename_quotefix") && len(args) >= 2 {
			if sqlStr, ok := args[1].(string); ok {
				schemaName := ""
				if len(args) >= 1 && args[0] != nil {
					schemaName, _ = args[0].(string)
				}
				return e.quoteFixWithSchema(schemaName, sqlStr), nil
			}
			return "", nil
		}
		return fn.ScalarFn(args)
	}

	// Scalar min/max: with two or more arguments, MIN()/MAX() are scalar
	// functions. SQLite semantics: if any argument is NULL the result is
	// NULL (unlike the aggregate forms, which ignore NULLs).
	if fn.Type == function.TypeAggregate && len(args) >= 2 && (upper == "MIN" || upper == "MAX") {
		for _, a := range args {
			if a == nil {
				return nil, nil
			}
		}
		best := args[0]
		for _, a := range args[1:] {
			if (upper == "MIN" && util.CompareValues(a, best) < 0) ||
				(upper == "MAX" && util.CompareValues(a, best) > 0) {
				best = a
			}
		}
		return best, nil
	}

	// For aggregate functions, evaluate step by step if row is provided
	if fn.Type == function.TypeAggregate {
		agg := fn.AggregateFn()
		if err := agg.Step(args); err != nil {
			return nil, err
		}
		return agg.Final()
	}

	return nil, fmt.Errorf("aggregate function %s not supported in this context", f.Name)
}

func (e *Engine) findNextRowID(tableName string, rootPage uint32) int64 {
	// AUTOINCREMENT tables use a persistent sequence: the largest rowid ever
	// used is remembered (like SQLite's sqlite_sequence), so after DELETE the
	// next rowid still continues from the old maximum.
	isAutoInc := e.tableHasAutoIncrement(tableName)
	if isAutoInc {
		if seq, ok := e.autoIncSeq[rootPage]; ok {
			return seq + 1
		}
	}

	// Use the cached largest rowid when available (SQLite keeps the largest
	// rowid seen so far and recomputes it only after a DELETE or when the
	// cache is empty). This avoids a full-table scan per insert, which is
	// O(n²) for bulk auto-rowid inserts (e.g. selectG inserts 100k rows).
	if cached, ok := e.nextRowIDCache[rootPage]; ok {
		return cached + 1
	}
	tree := btree.NewBTree(e.pager, e.rootPage(tableName, rootPage), true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 1
	}
	var maxID int64
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		if cell.RowID > maxID {
			maxID = cell.RowID
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	e.nextRowIDCache[rootPage] = maxID
	// AUTOINCREMENT never reuses rowid 1 after the sequence starts; the
	// sequence itself is recorded by bumpRowIDCache on the successful insert.
	if isAutoInc && maxID < 1 {
		return 1
	}
	return maxID + 1
}

// tableHasAutoIncrement reports whether the table declares an AUTOINCREMENT
// column (an INTEGER PRIMARY KEY AUTOINCREMENT column in a rowid table).
func (e *Engine) tableHasAutoIncrement(tableName string) bool {
	colDefs, ok := e.colCache[tableName]
	if !ok {
		return false
	}
	for _, cd := range colDefs {
		if cd.AutoInc {
			return true
		}
	}
	return false
}

// bumpRowIDCache records a row with the given rowid as present in the table.
// The cache always holds the largest rowid seen so far; explicit-rowid inserts
// must bump it so later auto-rowid inserts do not collide.
func (e *Engine) bumpRowIDCache(rootPage uint32, rowID int64) {
	if cur, ok := e.nextRowIDCache[rootPage]; !ok || rowID > cur {
		e.nextRowIDCache[rootPage] = rowID
	}
	if cur, ok := e.autoIncSeq[rootPage]; !ok || rowID > cur {
		e.autoIncSeq[rootPage] = rowID
	}
}

// invalidateRowIDCache drops the cached largest rowid for a table. Called after
// any DELETE (or rowid-changing UPDATE) because the largest rowid may have been
// removed; the next findNextRowID recomputes it by scanning. The AUTOINCREMENT
// sequence is deliberately kept: DELETE does not rewind sqlite_sequence.
func (e *Engine) invalidateRowIDCache(rootPage uint32) {
	delete(e.nextRowIDCache, rootPage)
}

func (e *Engine) parseColumnDefs(tableName, createSQL string) []sql.ColumnDef {
	// Check cache first
	if cached, ok := e.colCache[tableName]; ok {
		return cached
	}
	// Fall back to re-parsing
	stmts, perr := parse.ParseSQL(createSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if ok && ct != nil && len(ct.Columns) > 0 {
		// Cache for future use
		e.colCache[tableName] = ct.Columns
		return ct.Columns
	}
	// CREATE VIRTUAL TABLE t1 USING module(a, b, c): the module arguments are
	// the virtual table's column names.
	if vt, ok := stmts[0].(*sql.CreateVirtualTableStmt); ok && vt != nil {
		colDefs := make([]sql.ColumnDef, len(vt.Args))
		for i, arg := range vt.Args {
			colDefs[i] = sql.ColumnDef{Name: arg, Type: ""}
		}
		e.colCache[tableName] = colDefs
		return colDefs
	}
	// For virtual tables, check if we have an FTS table registered
	if ftsTable, ok := e.ftsTables[tableName]; ok {
		colDefs := make([]sql.ColumnDef, len(ftsTable.ColumnNames()))
		for i, name := range ftsTable.ColumnNames() {
			colDefs[i] = sql.ColumnDef{Name: name, Type: ""}
		}
		e.colCache[tableName] = colDefs
		return colDefs
	}
	return nil
}

// tableConstraints returns the table-level constraints (CHECK, UNIQUE, etc.)
// parsed from the stored CREATE TABLE SQL. Results are cached per table.
func (e *Engine) tableConstraints(tableName, createSQL string) []sql.TableConstraint {
	if e.tcCache == nil {
		e.tcCache = make(map[string][]sql.TableConstraint)
	}
	if cached, ok := e.tcCache[tableName]; ok {
		return cached
	}
	stmts, perr := parse.ParseSQL(createSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if !ok || ct == nil {
		return nil
	}
	e.tcCache[tableName] = ct.Constraints
	return ct.Constraints
}

// --- Value arithmetic helpers ---

func toBool(v interface{}) bool {
	if v == nil {
		return false
	}
	// Unwrap ColumnValue so HAVING, WHERE, and boolean filters
	// correctly evaluate scalar values from the database.
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		// SQLite's sqlite3VdbeBooleanValue converts TEXT/BLOB to REAL and
		// checks != 0.0: 'a' and '0.0' are FALSE, '5' and '5x' are TRUE.
		return sqliteRealValue(x) != 0.0
	case []byte:
		return sqliteRealValue(string(x)) != 0.0
	default:
		return true
	}
}

// sqliteRealValue converts a text value to REAL like SQLite's
// sqlite3VdbeRealValue: the numeric prefix is parsed (an empty or
// non-numeric string yields 0.0).
func sqliteRealValue(s string) float64 {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0.0
	}
	// Parse the leading numeric prefix; stop at the first non-numeric char.
	i := 0
	if i < len(trimmed) && (trimmed[i] == '+' || trimmed[i] == '-') {
		i++
	}
	dot := false
	exp := false
	for ; i < len(trimmed); i++ {
		c := trimmed[i]
		if c >= '0' && c <= '9' {
			continue
		}
		if c == '.' && !dot && !exp {
			dot = true
			continue
		}
		if (c == 'e' || c == 'E') && !exp {
			exp = true
			dot = true
			// Optional sign after e/E.
			if i+1 < len(trimmed) && (trimmed[i+1] == '+' || trimmed[i+1] == '-') {
				i++
			}
			continue
		}
		break
	}
	prefix := trimmed[:i]
	if prefix == "" || prefix == "+" || prefix == "-" || prefix == "." || prefix == "+." || prefix == "-." {
		return 0.0
	}
	f, err := strconv.ParseFloat(prefix, 64)
	if err != nil {
		return 0.0
	}
	return f
}

func addValues(a, b interface{}) (interface{}, error) {
	// Empty/whitespace/dot strings are integer 0 in SQLite arithmetic.
	if isZeroString(a) || isZeroString(b) {
		ai := toIntValue(a)
		bi := toIntValue(b)
		return ai + bi, nil
	}
	// Integer arithmetic must not round-trip through float64 (precision loss
	// for large int64 values) unless the result overflows int64, in which
	// case SQLite promotes the result to REAL (e.g. 9223372036854775807+1
	// is 9.22337203685478e+18, not the wrapped MinInt64).
	if ia, ok := a.(int64); ok {
		if ib, ok := b.(int64); ok {
			sum := ia + ib
			// Detect int64 overflow: the sum of two same-sign values must
			// keep that sign (or be zero for -1 + 1, which cannot overflow).
			if (ib > 0 && sum < ia) || (ib < 0 && sum > ia) {
				return float64(ia) + float64(ib), nil
			}
			return sum, nil
		}
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if numericIsInt(a) && numericIsInt(b) {
			return int64(af) + int64(bf), nil
		}
		return af + bf, nil
	}
	return nil, fmt.Errorf("cannot add non-numeric values")
}

func isZeroString(v interface{}) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(s)
	return trimmed == "" || trimmed == "." || trimmed == "+." || trimmed == "-."
}

// numericIsInt reports whether a value should be treated as an integer in
// arithmetic. int64 values are integers; text that parses to a whole number
// (SQLite's text→integer rule: '12' is 12, '1x' is 1, '12.5' is not) is also
// integer-valued so that '12' + 3 yields the integer 15, matching SQLite.
// A string with no numeric prefix at all ("abc") is integer 0.
func numericIsInt(v interface{}) bool {
	if _, ok := v.(int64); ok {
		return true
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	f, ok := parseNumericPrefix(s)
	if !ok {
		return true
	}
	return f == float64(int64(f))
}

func subValues(a, b interface{}) (interface{}, error) {
	// Empty/whitespace/dot strings are integer 0 in SQLite arithmetic.
	if isZeroString(a) || isZeroString(b) {
		ai := toIntValue(a)
		bi := toIntValue(b)
		return ai - bi, nil
	}
	// Integer arithmetic must not round-trip through float64 (precision loss
	// for large int64 values). Use int64 subtraction directly.
	if ia, ok := a.(int64); ok {
		if ib, ok := b.(int64); ok {
			return ia - ib, nil
		}
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if numericIsInt(a) && numericIsInt(b) {
			return int64(af) - int64(bf), nil
		}
		return af - bf, nil
	}
	return nil, fmt.Errorf("cannot subtract non-numeric values")
}

func toIntValue(v interface{}) int64 {
	if isZeroString(v) {
		return 0
	}
	if i, ok := v.(int64); ok {
		return i
	}
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

// toInt64 converts a value to int64 with an ok flag, matching SQLite's
// integer conversion for bitwise operators: int64 stays, float64 truncates
// toward zero, numeric strings parse, everything else coerces to 0 (SQLite
// applies NUMERIC affinity to text/blob operands, so 'a' → 0, x'00' → 0).
func toInt64(v interface{}) (int64, bool) {
	switch x := util.UnwrapColumnValue(v).(type) {
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case string:
		s := strings.TrimSpace(x)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
		return 0, true
	case []byte:
		s := strings.TrimSpace(string(x))
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
		return 0, true
	case nil:
		return 0, false // NULL propagates NULL
	default:
		return 0, true
	}
}

func mulValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if numericIsInt(a) && numericIsInt(b) {
			return int64(af) * int64(bf), nil
		}
		return af * bf, nil
	}
	return nil, fmt.Errorf("cannot multiply non-numeric values")
}

func divValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if bf == 0 {
			return nil, nil
		}
		if numericIsInt(a) && numericIsInt(b) {
			return int64(af) / int64(bf), nil
		}
		return af / bf, nil
	}
	return nil, fmt.Errorf("cannot divide non-numeric values")
}

func modValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if bf == 0 {
			return nil, nil
		}
		// SQLite's % truncates both operands to integers then applies integer
		// modulo (5.5 % 2 is 1, not 1.5). The result type is REAL when either
		// operand was REAL, INTEGER otherwise (5 % 2 → 1, 5.0 % 2 → 1.0).
		r := int64(af) % int64(bf)
		if numericIsInt(a) && numericIsInt(b) {
			return r, nil
		}
		return float64(r), nil
	}
	return nil, fmt.Errorf("cannot mod non-numeric values")
}

func bitwiseAnd(a, b interface{}) (interface{}, error) {
	ai, aok := a.(int64)
	bi, bok := b.(int64)
	if aok && bok {
		return ai & bi, nil
	}
	return nil, fmt.Errorf("cannot bitwise-AND non-integer values")
}

func bitwiseOr(a, b interface{}) (interface{}, error) {
	ai, aok := toInt64(a)
	bi, bok := toInt64(b)
	if aok && bok {
		return ai | bi, nil
	}
	return nil, fmt.Errorf("cannot bitwise-OR non-integer values")
}

func shiftLeft(a, b interface{}) (interface{}, error) {
	ai, aok := toInt64(a)
	bi, bok := toInt64(b)
	if aok && bok {
		if bi < 0 {
			return shiftRight(a, int64(-bi))
		}
		if bi >= 64 {
			return int64(0), nil
		}
		return ai << uint(bi), nil
	}
	return nil, fmt.Errorf("cannot shift non-integer values")
}

func shiftRight(a, b interface{}) (interface{}, error) {
	ai, aok := toInt64(a)
	bi, bok := toInt64(b)
	if aok && bok {
		if bi < 0 {
			return shiftLeft(a, int64(-bi))
		}
		if bi >= 64 {
			if ai < 0 {
				return int64(-1), nil
			}
			return int64(0), nil
		}
		return ai >> uint(bi), nil
	}
	return nil, fmt.Errorf("cannot shift non-integer values")
}

func concatValues(a, b interface{}) (interface{}, error) {
	if a == nil || b == nil {
		return nil, nil
	}
	// Unwrap column-affinity wrappers so blobs concatenate as raw bytes.
	a = util.UnwrapColumnValue(a)
	b = util.UnwrapColumnValue(b)
	ab, aIsBlob := a.([]byte)
	bb, bIsBlob := b.([]byte)
	if aIsBlob || bIsBlob {
		// Concatenate raw bytes; SQLite yields a TEXT value (not a blob).
		var buf []byte
		if aIsBlob {
			buf = append(buf, ab...)
		} else {
			buf = append(buf, renderConcatValue(a)...)
		}
		if bIsBlob {
			buf = append(buf, bb...)
		} else {
			buf = append(buf, renderConcatValue(b)...)
		}
		return string(buf), nil
	}
	return renderConcatValue(a) + renderConcatValue(b), nil
}

// renderConcatValue converts a value to its TEXT form for the || operator,
// matching SQLite's sqlite3_value_text rendering (REALs use the 15-digit
// %!.15g format with a trailing .0 for whole numbers, e.g. 11.0).
func renderConcatValue(v interface{}) string {
	switch x := v.(type) {
	case float64:
		return util.FormatSQLiteReal(x)
	case []byte:
		return string(x)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

func negateValue(v interface{}) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	// Try numeric negation first
	switch val := v.(type) {
	case int64:
		// Negating math.MinInt64 overflows int64 (Go wraps to MinInt64).
		// SQLite promotes the result to REAL: -(-9223372036854775808)
		// evaluates to 9.22337203685478e+18 (2^63 as a float).
		if val == math.MinInt64 {
			return -float64(val), nil
		}
		return -val, nil
	case float64:
		// SQLite keeps -0.0 as a REAL -0.0 (typeof(-0.0)='real'); do not
		// coerce it to an integer here.
		return -val, nil
	}
	// Try string as number
	if isZeroString(v) {
		// SQLite: -'.' == 0 (integer), -'' == 0.
		return int64(0), nil
	}
	f, ok := toFloat(v)
	if ok {
		return -f, nil
	}
	// Non-numeric values: return 0 (SQLite behavior, e.g. -'abc' = 0, -x'ce' = 0)
	return int64(0), nil
}

// numericValue converts a value to a number (used by unary + operator).
// Non-numeric values are converted to 0, matching SQLite behavior.
func numericValue(v interface{}) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	if i, ok := v.(int64); ok {
		return i, nil
	}
	if f, ok := v.(float64); ok {
		return f, nil
	}
	// Try string conversion
	if s, ok := v.(string); ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			// Return int64 if it's a whole number
			if f == float64(int64(f)) {
				return int64(f), nil
			}
			return f, nil
		}
		// Try integer parsing
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		}
	}
	// Blob or other non-numeric: return 0
	return int64(0), nil
}

func likeValues(str, pattern interface{}) bool {
	s := util.SQLiteValueString(util.UnwrapColumnValue(str))
	p := util.SQLiteValueString(util.UnwrapColumnValue(pattern))
	return likeMatch(s, p)
}

// likeValuesWithEscape performs LIKE matching with an escape character.
func likeValuesWithEscape(str, pattern interface{}, escape string) bool {
	s := util.SQLiteValueString(util.UnwrapColumnValue(str))
	p := util.SQLiteValueString(util.UnwrapColumnValue(pattern))
	return likeMatchEscaped(s, p, escape)
}

func likeMatch(s, pattern string) bool {
	return likeMatchRecursiveEscaped(s, pattern, 0, 0, 0)
}

func likeMatchEscaped(s, pattern, escape string) bool {
	if escape == "" {
		return likeMatch(s, pattern)
	}
	// Process the pattern, treating escape char + next char as literal
	return likeMatchRecursiveEscaped(s, pattern, 0, 0, escape[0])
}

func likeMatchRecursiveEscaped(s, pattern string, idx, patIdx int, escape byte) bool {
	for patIdx < len(pattern) {
		c := pattern[patIdx]
		if c == escape && patIdx+1 < len(pattern) {
			// Escape char followed by another char: treat the next char as literal
			nextChar := pattern[patIdx+1]
			if idx >= len(s) || !strings.EqualFold(string(s[idx]), string(nextChar)) {
				return false
			}
			idx++
			patIdx += 2
			continue
		}
		switch c {
		case '%':
			return likeMatchPercentEscaped(s, pattern, idx, patIdx, escape)
		case '_':
			if idx >= len(s) {
				return false
			}
			idx++
			patIdx++
		default:
			if idx >= len(s) || !strings.EqualFold(string(s[idx]), string(c)) {
				return false
			}
			idx++
			patIdx++
		}
	}
	return idx >= len(s)
}

func likeMatchPercentEscaped(s, pattern string, idx, patIdx int, escape byte) bool {
	patIdx++
	if patIdx >= len(pattern) {
		return true
	}
	for idx < len(s) {
		if likeMatchRecursiveEscaped(s, pattern, idx, patIdx, escape) {
			return true
		}
		idx++
	}
	return false
}

func toFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case string:
		// SQLite text→numeric conversion in arithmetic uses the leading
		// numeric prefix: '1x' → 1, 'x1' → 0, ' 12.5foo' → 12.5, and
		// empty/whitespace/dot strings are 0. (sqlite3AtoF semantics.)
		// A string with no numeric prefix at all ("abc") is numeric 0.
		if f, ok := parseNumericPrefix(x); ok {
			return f, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// parseNumericPrefix parses the leading numeric prefix of a string the way
// SQLite's sqlite3AtoF does: optional whitespace, sign, digits, optional
// fraction, optional exponent, stopping at the first non-numeric character.
// It returns false when the string has no numeric prefix at all (e.g. "x1").
func parseNumericPrefix(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	// Empty/whitespace-only strings and a lone '.' are numeric 0.
	if t == "" || t == "." || t == "+." || t == "-." {
		return 0, true
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f, true
	}
	// Numeric prefix parse: [sign] digits [.digits] [eE [sign] digits]
	i := 0
	if i < len(t) && (t[i] == '+' || t[i] == '-') {
		i++
	}
	digits := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
		digits++
	}
	if i < len(t) && t[i] == '.' {
		i++
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
			digits++
		}
	}
	if digits > 0 && i < len(t) && (t[i] == 'e' || t[i] == 'E') {
		j := i + 1
		if j < len(t) && (t[j] == '+' || t[j] == '-') {
			j++
		}
		if j < len(t) && t[j] >= '0' && t[j] <= '9' {
			i = j
			for i < len(t) && t[i] >= '0' && t[i] <= '9' {
				i++
			}
		}
	}
	if digits > 0 {
		if f, err := strconv.ParseFloat(t[:i], 64); err == nil {
			return f, true
		}
		// Overflow: SQLite returns +/-Inf.
		if t[0] == '-' {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	return 0, false
}

func isInt(v interface{}) bool {
	_, ok := v.(int64)
	return ok
}
