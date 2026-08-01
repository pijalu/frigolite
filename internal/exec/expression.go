// Package exec implements query execution.
package exec

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/function"
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
	case *sql.ParenExpr:
		return e.evalExpr(v.Expr, row)
	case *sql.ColumnRef:
		return e.evalColumnRef(v, row)
	case *sql.FuncCall:
		return e.evalFuncCall(v, row)
	case *sql.RowValue:
		var parts []string
		for _, val := range v.Values {
			ev, err := e.evalExpr(val, row)
			if err != nil {
				return nil, err
			}
			if ev == nil {
				parts = append(parts, "{}")
			} else {
				parts = append(parts, fmt.Sprintf("%v", ev))
			}
		}
		return strings.Join(parts, " "), nil
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
	default:
		return nil, fmt.Errorf("unknown expression type: %T", expr)
	}
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
	for _, w := range v.Whens {
		when, err := e.evalExpr(w.When, row)
		if err != nil {
			return nil, err
		}
		if util.CompareValues(operand, when) == 0 {
			return e.evalExpr(w.Then, row)
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

func (e *Engine) evalCastExpr(v *sql.CastExpr, row Row) (interface{}, error) {
	val, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	// Unwrap ColumnValue affinity wrappers so the CAST operates on the raw value.
	val = util.UnwrapColumnValue(val)
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
			// SQLite: CAST(text AS REAL) parses the text as a number;
			// non-numeric text coerces to 0.0.
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return f, nil
			}
			return float64(0), nil
		default:
			return float64(0), nil
		}
	case "TEXT":
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
		return "*", nil
	}
	// Qualified column reference: check qualified name first
	if v.Table != "" {
		// Try table-qualified key in the current row (e.g., "t1.a")
		if row != nil {
			if val, ok := row.Get(v.Table + "." + v.Name); ok {
				return val, nil
			}
			// If the qualified key is not found and the qualifier matches the
			// table currently being scanned, resolve via unqualified name.
			// Row maps store unqualified column names, so "t1.a" in a query
			// scanning table t1 resolves to row["a"].
			if e.currentScanTable != "" && strings.EqualFold(v.Table, e.currentScanTable) {
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
		return nil, nil
	}
	// Unqualified: check short name
	if row != nil {
		if val, ok := row.Get(v.Name); ok {
			return val, nil
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
	return nil, nil
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

	// Most operators return NULL when either operand is NULL.
	// AND/OR need Kleene logic (handled in evalArithmeticOp).
	// IS / IS NOT are NULL-safe: NULL IS NULL is true.
	if v.Operator != "AND" && v.Operator != "OR" && v.Operator != "IS" && v.Operator != "IS NOT" {
		if left == nil || right == nil {
			return nil, nil
		}
	}
	if v.Operator == "LIKE" && v.Escape != "" {
		return likeValuesWithEscape(left, right, v.Escape), nil
	}
	if v.Operator == "IS" {
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
}

// extractValue extracts the raw value and collation from a potentially collated value.
func extractValue(v interface{}) (interface{}, string) {
	if cv, ok := v.(*collatedValue); ok {
		return cv.value, cv.collation
	}
	return v, ""
}

// compareValuesWithCollate compares two values using the collation from either side.
func compareValuesWithCollate(left, right interface{}) int {
	lv, lc := extractValue(left)
	rv, rc := extractValue(right)
	// Use the first non-empty collation found
	collation := lc
	if collation == "" {
		collation = rc
	}
	return util.CompareValuesCollate(lv, rv, collation)
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

func evalBinaryOpValues(op string, left, right interface{}) (interface{}, error) {
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
		return boolToInt(regexpValues(left, right)), nil
	case "NOT LIKE":
		return boolToInt(!likeValues(left, right)), nil
	case "NOT GLOB":
		return boolToInt(!globValues(left, right)), nil
	case "NOT REGEXP":
		return boolToInt(!regexpValues(left, right)), nil
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
		// marker and apply the correct collation.
		if rightStr, ok := right.(string); ok {
			switch strings.ToUpper(rightStr) {
			case "", "BINARY", "NOCASE", "RTRIM":
				return &collatedValue{value: left, collation: rightStr}, nil
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
	s := fmt.Sprintf("%v", util.UnwrapColumnValue(str))
	p := fmt.Sprintf("%v", util.UnwrapColumnValue(pattern))
	return function.GlobMatch(s, p)
}

func regexpValues(str, pattern interface{}) bool {
	s := fmt.Sprintf("%v", util.UnwrapColumnValue(str))
	p := fmt.Sprintf("%v", util.UnwrapColumnValue(pattern))
	re, err := regexp.Compile(p)
	if err != nil {
		return false
	}
	return re.MatchString(s)
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
		// Unary plus: convert to numeric (like SQLite's + operator)
		return numericValue(operand)
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
	result := util.CompareValues(operand, low) >= 0 && util.CompareValues(operand, high) <= 0
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
	found := false
	for _, item := range v.List {
		ival, err := e.evalExpr(item, row)
		if err != nil {
			continue
		}
		if util.CompareValues(operand, ival) == 0 {
			found = true
			break
		}
	}
	if v.Negated {
		found = !found
	}
	return boolToInt(found), nil
}

func (e *Engine) evalBool(expr sql.Expr, row Row) (bool, error) {
	v, err := e.evalExpr(expr, row)
	if err != nil {
		return false, err
	}
	return toBool(v), nil
}

func (e *Engine) evalFuncCall(f *sql.FuncCall, row Row) (interface{}, error) {
	// Engine-specific functions that need engine state
	upper := strings.ToUpper(f.Name)
	switch upper {
	case "CHANGES":
		return e.lastChanges, nil
	case "LAST_INSERT_ROWID":
		return e.lastRowID, nil
	}

	fn, ok := e.funcs.Find(f.Name)
	if !ok {
		return nil, fmt.Errorf("unknown function: %s", f.Name)
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
		return fn.ScalarFn(args)
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
	return maxID + 1
}

// bumpRowIDCache records a row with the given rowid as present in the table.
// The cache always holds the largest rowid seen so far; explicit-rowid inserts
// must bump it so later auto-rowid inserts do not collide.
func (e *Engine) bumpRowIDCache(rootPage uint32, rowID int64) {
	if cur, ok := e.nextRowIDCache[rootPage]; !ok || rowID > cur {
		e.nextRowIDCache[rootPage] = rowID
	}
}

// invalidateRowIDCache drops the cached largest rowid for a table. Called after
// any DELETE (or rowid-changing UPDATE) because the largest rowid may have been
// removed; the next findNextRowID recomputes it by scanning.
func (e *Engine) invalidateRowIDCache(rootPage uint32) {
	delete(e.nextRowIDCache, rootPage)
}

func (e *Engine) parseColumnDefs(tableName, createSQL string) []sql.ColumnDef {
	// Check cache first
	if cached, ok := e.colCache[tableName]; ok {
		return cached
	}
	// Fall back to re-parsing
	parser := sql.NewParser(createSQL)
	stmts := parser.Parse()
	if len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if ok && ct != nil {
		// Cache for future use
		e.colCache[tableName] = ct.Columns
		return ct.Columns
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
	parser := sql.NewParser(createSQL)
	stmts := parser.Parse()
	if len(stmts) == 0 {
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
		return x != ""
	case []byte:
		return len(x) > 0
	default:
		return true
	}
}

func addValues(a, b interface{}) (interface{}, error) {
	// Empty/whitespace/dot strings are integer 0 in SQLite arithmetic.
	if isZeroString(a) || isZeroString(b) {
		ai := toIntValue(a)
		bi := toIntValue(b)
		return ai + bi, nil
	}
	// Integer arithmetic must not round-trip through float64 (precision loss
	// for large int64 values).
	if ia, ok := a.(int64); ok {
		if ib, ok := b.(int64); ok {
			return ia + ib, nil
		}
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if isInt(a) && isInt(b) {
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
		if isInt(a) && isInt(b) {
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

func mulValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if isInt(a) && isInt(b) {
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
		if isInt(a) && isInt(b) {
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
		if isInt(a) && isInt(b) {
			return int64(af) % int64(bf), nil
		}
		// For floating point modulo, convert to int64 equivalent
		return int64(af) % int64(bf), nil
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

func concatValues(a, b interface{}) (interface{}, error) {
	if a == nil || b == nil {
		return nil, nil
	}
	return fmt.Sprintf("%v%v", a, b), nil
}

func negateValue(v interface{}) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	// Try numeric negation first
	switch val := v.(type) {
	case int64:
		return -val, nil
	case float64:
		// Handle negative zero: return int64 0 for -0.0
		if val == 0 {
			return int64(0), nil
		}
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
	s := fmt.Sprintf("%v", util.UnwrapColumnValue(str))
	p := fmt.Sprintf("%v", util.UnwrapColumnValue(pattern))
	return likeMatch(s, p)
}

// likeValuesWithEscape performs LIKE matching with an escape character.
func likeValuesWithEscape(str, pattern interface{}, escape string) bool {
	s := fmt.Sprintf("%v", util.UnwrapColumnValue(str))
	p := fmt.Sprintf("%v", util.UnwrapColumnValue(pattern))
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
		// SQLite treats empty/whitespace-only strings as numeric 0 in
		// arithmetic contexts (e.g. '' - 5 == -5). A lone '.' is also 0.
		if strings.TrimSpace(x) == "" || x == "." || x == "+." || x == "-." {
			return 0, true
		}
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return f, true
		}
		return 0, false
	default:
		return 0, false
	}
}

func isInt(v interface{}) bool {
	_, ok := v.(int64)
	return ok
}
