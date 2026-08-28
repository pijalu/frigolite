package execexpr

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// isRtreeGeometry reports whether v is an r-tree MATCH geometry marker.
func isRtreeGeometry(v interface{}) bool {
	_, ok := v.(*vtab.RtreeGeometry)
	return ok
}

func (ev *Evaluator) rowHasQualifiedKeys(row Row) bool {
	return ev.ctx.RowHasQualifiedKeys(row)
}

// extractCollatedValues extracts raw values from CollatedValue wrappers
// for operators that don't need collation propagation.
// Comparison operators keep the CollatedValue for compareValuesWithCollate.
// || keeps CollatedValue for evalConcat to propagate collation.
func extractCollatedValues(op string, left, right interface{}) (interface{}, interface{}) {
	if op == "=" || op == "<>" || op == "!=" || op == "<" || op == ">" || op == "<=" || op == ">=" || op == "||" {
		return left, right
	}
	l, _ := extractValue(left)
	r, _ := extractValue(right)
	return l, r
}

// isComparisonOperator reports whether op is a comparison operator that
// supports row-value semantics (subquery-subquery row comparison).
func isComparisonOperator(op string) bool {
	switch op {
	case "=", "==", "<>", "!=", "<", ">", "<=", ">=", "IS", "IS NOT":
		return true
	}
	return false
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
	// TEXT vs numeric → check if the string can be converted to a number.
	// If it can (e.g., '1' vs 1), allow the comparison so that
	// compareValuesWithCollate handles the numeric conversion normally.
	// If it cannot (e.g., 'abc' vs 1), treat as not a type match.
	if stringNumericMix(lv, rv) && !numericString(lv, rv) {
		return false // non-numeric string vs numeric → not a match
	}
	return true
}

// stringNumericMix reports whether one value is TEXT and the other is numeric
// (INTEGER or REAL).
func stringNumericMix(lv, rv interface{}) bool {
	_, lStr := lv.(string)
	_, rStr := rv.(string)
	_, lInt := lv.(int64)
	_, rInt := rv.(int64)
	_, lFloat := lv.(float64)
	_, rFloat := rv.(float64)
	return (lStr && (rInt || rFloat)) || (rStr && (lInt || lFloat))
}

// numericString reports whether the TEXT value among lv/rv parses as a number
// (after trimming surrounding whitespace).
func numericString(lv, rv interface{}) bool {
	var str string
	if s, ok := lv.(string); ok {
		str = s
	} else {
		str = rv.(string)
	}
	str = strings.TrimSpace(str)
	_, err := strconv.ParseFloat(str, 64)
	return err == nil
}

func globValues(str, pattern interface{}) bool {
	s := util.SQLiteValueString(unwrapCollatedValue(str))
	p := util.SQLiteValueString(unwrapCollatedValue(pattern))
	return function.GlobMatch(s, p)
}

func regexpValues(str, pattern interface{}) (bool, error) {
	s := util.SQLiteValueString(unwrapCollatedValue(str))
	p := util.SQLiteValueString(unwrapCollatedValue(pattern))
	re, err := util.CompileRegexp(p)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
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
	lv, _ := extractValue(left)
	rv, _ := extractValue(right)

	if lv == nil || rv == nil {
		return nil, nil
	}
	// An r-tree geometry marker may only be consumed by a MATCH constraint
	// (rtree9-4.3: CAST(cube(...) || blob AS blob) fails the statement).
	if isRtreeGeometry(lv) || isRtreeGeometry(rv) {
		return nil, fmt.Errorf("SQL logic error")
	}
	result, err := ConcatValues(lv, rv)
	if err != nil {
		return nil, err
	}
	// SQLite's || operator returns a value with BINARY collation regardless
	// of its operands' collations (datatype3.html: "the || operator...
	// result has no collation sequence"). Propagating a column's COLLATE
	// NOCASE through concatenation made comparisons like
	// (a||'')=(b||'') use nocase when SQLite compares them case-sensitively.
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
	// A column whose stored value is NULL wraps as ColumnValue{Value: nil};
	// NULL is not false, so unwrap before the boolean test (SQLite treats
	// NULL as neither true nor false).
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	if v == nil {
		return false
	}
	return !ToBool(v)
}

func isTrue(v interface{}) bool {
	if v == nil {
		return false
	}
	return ToBool(v)
}

func (ev *Evaluator) evalUnaryOp(v *sql.UnaryOp, row Row) (interface{}, error) {
	operand, err := ev.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if operand == nil {
		return nil, nil
	}
	// Unwrap the ColumnValue so arithmetic operators see the base value.
	// Unary plus (+) preserves the ColumnValue wrapper so its column
	// collation survives into comparisons (SQLite: +bb >= aa with bb BINARY
	// compares BINARY, not the right side's NOCASE).
	switch v.Operator {
	case "-":
		return NegateValue(util.UnwrapColumnValue(operand))
	case "+":
		// Unary plus is a no-op in SQLite — it returns the operand value
		// unchanged (no numeric conversion). It strips the column AFFINITY
		// ((+x) = a with x BLOB and a TEXT coerces and matches) but keeps the
		// column-ness so COLLATION resolution still sees the operand as a
		// column (+bb >= aa with bb BINARY compares BINARY, not the right
		// side's NOCASE). ColumnValue's Affinity is cleared; the wrapper
		// stays so isColumnValue/compareValuesWithCollate resolve collation.
		if cv, ok := operand.(*util.ColumnValue); ok {
			return &util.ColumnValue{Value: cv.Value, Affinity: 0}, nil
		}
		return operand, nil
	case "NOT":
		return boolToInt(!ToBool(util.UnwrapColumnValue(operand))), nil
	case "~":
		operand = util.UnwrapColumnValue(operand)
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

func (ev *Evaluator) evalIsNull(v *sql.IsNull, row Row) (interface{}, error) {
	operand, err := ev.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	// Unwrap ColumnValue so we check for actual NULL, not just wrapper nil
	operand = util.UnwrapColumnValue(operand)
	return boolToInt(operand == nil), nil
}

func (ev *Evaluator) evalIsNotNull(v *sql.IsNotNull, row Row) (interface{}, error) {
	operand, err := ev.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	// Unwrap ColumnValue so we check for actual NULL, not just wrapper nil
	operand = util.UnwrapColumnValue(operand)
	return boolToInt(operand != nil), nil
}

func (ev *Evaluator) evalIsTrue(v *sql.IsTrue, row Row) (interface{}, error) {
	operand, err := ev.evalExpr(v.Operand, row)
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

func (ev *Evaluator) evalIsFalse(v *sql.IsFalse, row Row) (interface{}, error) {
	operand, err := ev.evalExpr(v.Operand, row)
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

func (ev *Evaluator) evalIsDistinctFrom(v *sql.IsDistinctFrom, row Row) (interface{}, error) {
	left, err := ev.evalExpr(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := ev.evalExpr(v.Right, row)
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

func (ev *Evaluator) evalIsNotDistinctFrom(v *sql.IsNotDistinctFrom, row Row) (interface{}, error) {
	left, err := ev.evalExpr(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := ev.evalExpr(v.Right, row)
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
func (ev *Evaluator) evalInList(v *sql.InList, row Row) (interface{}, error) {
	// A subquery operand (SELECT a,b ...) IN (...) is evaluated as a ROW
	// (row-value IN): the subquery's full first row is the comparison value,
	// not just its first column. evalExpr would reduce it to a scalar.
	if subq, ok := v.Operand.(*sql.Subquery); ok {
		rows, err := ev.evalSubqueryRows(subq, row)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 || len(rows[0]) == 0 {
			return nil, nil
		}
		operand := make([]interface{}, len(rows[0]))
		for i := range rows[0] {
			operand[i] = util.UnwrapColumnValue(rows[0][i])
		}
		return ev.evalInListOperand(v, operand, row)
	}
	operand, err := ev.evalExprWithCollation(v.Operand, row)
	if err != nil {
		return nil, err
	}
	return ev.evalInListOperand(v, operand, row)
}

// evalSubqueryRows executes a subquery and returns all result rows (each row
// is a []interface{} of the row's column values). Used for row-value IN
// subqueries where the full row, not just the first column, must be compared.
func (ev *Evaluator) evalSubqueryRows(subq *sql.Subquery, row Row) ([][]interface{}, error) {
	ev.ctx.PushOuterRow(row)
	defer ev.ctx.PopOuterRow()
	return ev.ctx.ExecSelectRows(subq.Select)
}

// subqueryOutputAffinity returns the affinity of output column i of a
// subquery used as the RHS of an IN operator, matching SQLite's
// exprINAffinity: the affinity of the subquery's first result expression,
// resolved through CTE/table/expression column types.
func (ev *Evaluator) subqueryOutputAffinity(sel *sql.SelectStmt, i int) rune {
	if sel == nil || i >= len(sel.Columns) {
		return 0
	}
	return ev.exprOutputAffinity(sel, sel.Columns[i].Expr, i)
}

// mergeINAffinity mirrors SQLite's sqlite3CompareAffinity as used by
// exprINAffinity for the IN (subquery) operator: when both the subquery
// column and the LHS have affinity, a numeric affinity wins and otherwise the
// result is BLOB (no conversion); when only one side has affinity, that
// affinity is used.
func mergeINAffinity(subqAff, lhsAff rune) rune {
	if subqAff != 0 && lhsAff != 0 {
		if subqAff == 'N' || subqAff == 'R' || subqAff == 'I' || lhsAff == 'N' || lhsAff == 'R' || lhsAff == 'I' {
			return 'N'
		}
		return 'B'
	}
	if subqAff != 0 {
		return subqAff
	}
	return lhsAff
}

// compareWithAffinity compares two values after applying the given affinity
// to both (SQLite applies the IN comparison affinity to both operands). A
// zero affinity means no conversion: values compare by storage class.
func compareWithAffinity(a, b interface{}, aff rune) int {
	if aff != 0 {
		a = &util.ColumnValue{Value: util.UnwrapColumnValue(a), Affinity: aff}
		b = &util.ColumnValue{Value: util.UnwrapColumnValue(b), Affinity: aff}
	}
	return util.CompareValues(a, b)
}

func (ev *Evaluator) evalBool(expr sql.Expr, row Row) (bool, error) {
	v, err := ev.evalExpr(expr, row)
	if err != nil {
		return false, err
	}
	return ToBool(v), nil
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

// BoolToInt converts a boolean to an integer (0 or 1) matching SQLite
// behavior. Exported for the execution engine's row-value predicates.
func BoolToInt(b bool) int64 {
	return boolToInt(b)
}
