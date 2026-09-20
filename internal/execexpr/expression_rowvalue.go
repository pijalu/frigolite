// This file holds row-value and binary-operator evaluation: row-value
// comparisons (IS, =, <, BETWEEN, IN), arithmetic and bitwise operators, the
// binary-operator dispatcher, affinity resolution, and numeric/rowid
// helpers. It is the row-value half of the former expression.go, split out so
// that each file stays within the repository's complexity and size budgets.
// Core expression evaluation lives in expression_eval.go.
package execexpr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// evalRowValueCaseWhens compares a row-value CASE operand against each WHEN
// (CASE (a,b) WHEN (1,2) THEN ...) element-wise with row-value = semantics.
func (ev *Evaluator) evalRowValueCaseWhens(v *sql.CaseExpr, opRow []interface{}, row Row) (interface{}, error) {
	for _, w := range v.Whens {
		when, err := ev.evalExpr(w.When, row)
		if err != nil {
			return nil, err
		}
		whenRow, ok := when.([]interface{})
		if !ok || len(whenRow) != len(opRow) {
			continue
		}
		if rowValuesEqual(ev, opRow, whenRow) {
			return ev.evalExpr(w.Then, row)
		}
	}
	return ev.evalCaseElse(v, row)
}

// rowValuesEqual compares two row values element-wise with SQLite row-value =
// semantics: a NULL element at any position makes the rows unequal, and each
// element compares with the left operand's collation.
func rowValuesEqual(ev *Evaluator, opRow, whenRow []interface{}) bool {
	for i := range opRow {
		l, lColl := extractValue(opRow[i])
		r, _ := extractValue(whenRow[i])
		if util.UnwrapColumnValue(l) == nil || util.UnwrapColumnValue(r) == nil {
			return false
		}
		if lColl != "" {
			if ev.ctx.CompareValuesCollate(util.UnwrapColumnValue(l), util.UnwrapColumnValue(r), lColl) != 0 {
				return false
			}
		} else if util.CompareValues(util.UnwrapColumnValue(l), util.UnwrapColumnValue(r)) != 0 {
			return false
		}
	}
	return true
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
		return evalHexLiteral(v)
	}
	// Decimal integer (SQLite's only non-hex integer form): leading zeros
	// are NOT octal (unlike Go's base-0 scan) — SELECT typeof(08) is
	// integer in SQLite, so parse base 10 explicitly. Hex was handled by
	// evalHexLiteral above; 0b/0o prefixes are not SQLite literals.
	if i, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
		v.SetCached(i)
		return i, nil
	}
	if f, err := strconv.ParseFloat(v.Value, 64); err == nil || (errors.Is(err, strconv.ErrRange) && math.IsInf(f, 0)) {
		// ParseFloat returns ErrRange for literals that overflow/underflow
		// float64 (1e400 -> +Inf, 1e-400 -> 0.0). SQLite treats these as
		// REAL overflow results: 1e400 is +Inf, 1e-400 is 0.0. Accept the
		// returned value in that case instead of falling through to the
		// string cache.
		v.SetCached(f)
		return f, nil
	}
	v.SetCached(v.Value)
	return v.Value, nil
}

// evalHexLiteral evaluates a hexadecimal numeric literal, rejecting any whose
// unsigned magnitude exceeds MaxInt64.
func evalHexLiteral(v *sql.NumericLit) (interface{}, error) {
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

// resolveRowValueSubqueries re-evaluates a subquery operand in full when the
// other side of a comparison is a row value (SQLite: (a,b,c) OP (SELECT x,y,z)).
// For subquery-vs-subquery comparisons, both result rows are compared.
func (ev *Evaluator) resolveRowValueSubqueries(v *sql.BinaryOp, row Row, left, right interface{}) (interface{}, interface{}, interface{}, error) {
	// Row-value vs subquery: the subquery's result row forms the row value;
	// evalExpr above returned only its first column.
	var err error
	if _, lIsRow := left.([]interface{}); lIsRow {
		right, err = ev.subqueryRowValue(v.Right, row, right)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if _, rIsRow := right.([]interface{}); rIsRow {
		left, err = ev.subqueryRowValue(v.Left, row, left)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	// A subquery compared against a subquery compares their full result rows
	// (SQLite: (SELECT 1,2,3) == (SELECT 1,2,3)) — comparison operators only.
	// Arithmetic on scalar subqueries ((SELECT 5)*(SELECT 6) = 30) keeps the
	// scalar values evalExpr produced; re-evaluating them as row values here
	// would wrongly raise "row value misused" (randexpr regression).
	if !isComparisonOperator(v.Operator) {
		return nil, left, right, nil
	}
	_, lok := v.Left.(*sql.Subquery)
	_, rok := v.Right.(*sql.Subquery)
	if !lok || !rok {
		return nil, left, right, nil
	}
	return ev.compareSubquerySubquery(v, row)
}

// subqueryRowValue re-evaluates a subquery operand in full when the other side
// of a comparison is a row value: the subquery's full first row is the
// comparison value (with affinity applied). A zero-row subquery compares as
// NULL.
func (ev *Evaluator) subqueryRowValue(expr sql.Expr, row Row, cur interface{}) (interface{}, error) {
	subq, ok := expr.(*sql.Subquery)
	if !ok {
		return cur, nil
	}
	rows, err := ev.evalSubqueryRows(subq, row)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return ev.subqueryRowWithAffinity(subq, rows[0]), nil
}

// compareSubquerySubquery compares two subquery operands as row values (both
// result rows element-wise). Returns (doneValue, left, right, err): a non-nil
// doneValue is the comparison result; otherwise left/right are passed through.
func (ev *Evaluator) compareSubquerySubquery(v *sql.BinaryOp, row Row) (interface{}, interface{}, interface{}, error) {
	lrows, err := ev.evalSubqueryRows(v.Left.(*sql.Subquery), row)
	if err != nil {
		return nil, nil, nil, err
	}
	rrows, err := ev.evalSubqueryRows(v.Right.(*sql.Subquery), row)
	if err != nil {
		return nil, nil, nil, err
	}
	var lv, rv []interface{}
	if len(lrows) > 0 {
		lv = ev.subqueryRowWithAffinity(v.Left.(*sql.Subquery), lrows[0])
	}
	if len(rrows) > 0 {
		rv = ev.subqueryRowWithAffinity(v.Right.(*sql.Subquery), rrows[0])
	}
	// Zero-row subqueries compare as NULL (no row to compare).
	if lv == nil || rv == nil {
		return nil, nil, nil, nil
	}
	if len(lv) != len(rv) {
		return nil, nil, nil, fmt.Errorf("row value misused")
	}
	if v.Operator == "IS" || v.Operator == "IS NOT" {
		val, err := ev.evalRowValueIs(v.Operator, lv, rv)
		return val, nil, nil, err
	}
	val, err := ev.evalRowValueCompare(v.Operator, lv, rv)
	return val, nil, nil, err
}

// evalBinaryOpIs handles IS / IS NOT operators (NULL-safe comparisons),
// including row-value IS.
func (ev *Evaluator) evalBinaryOpIs(op string, left, right interface{}) (interface{}, error) {
	// Row-value IS: delegate to evalRowValueIs when either operand is a row.
	if ev.isRowValueOperand(left) || ev.isRowValueOperand(right) {
		return ev.evalRowValueIs(op, left, right)
	}
	if op == "IS" {
		return boolToInt(ev.isEqualNullSafe(left, right)), nil
	}
	// IS NOT
	left = util.UnwrapColumnValue(left)
	right = util.UnwrapColumnValue(right)
	return boolToInt(!ev.isEqualNullSafe(left, right)), nil
}

// isRowValueOperand reports whether the operand is a row value ([]interface{}).
func (ev *Evaluator) isRowValueOperand(v interface{}) bool {
	_, ok := v.([]interface{})
	return ok
}

// isEqualNullSafe reports whether two values are IS-equal: both NULL is true,
// one NULL is false, otherwise a collated comparison for equality.
func (ev *Evaluator) isEqualNullSafe(left, right interface{}) bool {
	if util.UnwrapColumnValue(left) == nil && util.UnwrapColumnValue(right) == nil {
		return true
	}
	if util.UnwrapColumnValue(left) == nil || util.UnwrapColumnValue(right) == nil {
		return false
	}
	return ev.ctx.CompareValuesWithCollate(left, right) == 0
}

// IsEqualNullSafe reports whether two values are IS-equal (both NULL is true,
// one NULL is false, otherwise a collated equality comparison). Exported for
// the SELECT engine's HAVING evaluation.
func (ev *Evaluator) IsEqualNullSafe(left, right interface{}) bool {
	return ev.isEqualNullSafe(left, right)
}

func (ev *Evaluator) evalBinaryOp(v *sql.BinaryOp, row Row) (interface{}, error) {
	if val, done, err := ev.evalBinaryOpMatchPreamble(v, row); done {
		return val, err
	}

	left, err := ev.evalExprWithCollation(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := ev.evalExprWithCollation(v.Right, row)
	if err != nil {
		return nil, err
	}

	// Row-value vs subquery: (a,b,c) OP (SELECT x,y,z ...). The subquery's
	// result row forms the row value; evalExpr above returned only its first
	// column, so re-evaluate the subquery in full when the other side is a
	// row value.
	var doneVal interface{}
	doneVal, left, right, err = ev.resolveRowValueSubqueries(v, row, left, right)
	if err != nil {
		return nil, err
	}
	if doneVal != nil {
		return doneVal, nil
	}
	// Most operators return NULL when either operand is NULL. AND/OR need
	// Kleene logic (handled in evalArithmeticOp). IS / IS NOT are NULL-safe.
	if binaryOpNeedsNullCheck(v.Operator) && (isSQLNull(left) || isSQLNull(right)) {
		return nil, nil
	}
	return ev.evalBinaryOpDispatched(v, left, right)
}

// binaryOpNeedsNullCheck reports whether the operator returns NULL when either
// operand is NULL (AND/OR need Kleene logic, IS/IS NOT are NULL-safe).
func binaryOpNeedsNullCheck(op string) bool {
	return op != "AND" && op != "OR" && op != "IS" && op != "IS NOT"
}

// evalBinaryOpDispatched evaluates a binary operator after both operands are
// resolved and the NULL pre-check has passed: LIKE-with-ESCAPE, IS/IS NOT, and
// the remaining operators via evalBinaryOpValues.
func (ev *Evaluator) evalBinaryOpDispatched(v *sql.BinaryOp, left, right interface{}) (interface{}, error) {
	// LIKE/GLOB term with a planner-synthesized prefix range: the bounds
	// gate the matcher (and elide it where they decide the row).
	if v.LikeRange != nil {
		return ev.evalLikeRangeOp(v, left, right)
	}
	if (v.Operator == "LIKE" || v.Operator == "NOT LIKE") && (v.Escape != "" || v.HasEscape) {
		return ev.evalLikeWithEscape(v, left, right)
	}
	if v.Operator == "IS" || v.Operator == "IS NOT" {
		return ev.evalBinaryOpIs(v.Operator, left, right)
	}
	return ev.evalBinaryOpValues(v.Operator, left, right)
}

// evalLikeWithEscape evaluates a LIKE (or negated NOT LIKE) expression with
// an ESCAPE clause. SQLite requires the ESCAPE expression to be a single
// character: ESCAPE ” and multi-character ESCAPE are runtime errors. An
// absent ESCAPE clause uses the default matcher.
func (ev *Evaluator) evalLikeWithEscape(v *sql.BinaryOp, left, right interface{}) (interface{}, error) {
	if v.HasEscape && len([]rune(v.Escape)) != 1 {
		return nil, fmt.Errorf("ESCAPE expression must be a single character")
	}
	bumpLikeCallCount()
	var result bool
	if ev.ctx.CaseSensitiveLike() {
		result = likeValuesWithEscapeCS(left, right, v.Escape)
	} else {
		result = likeValuesWithEscape(left, right, v.Escape)
	}
	if v.Operator == "NOT LIKE" {
		result = !result
	}
	if result && v.Operator != "NOT LIKE" {
		// Operator-overload probing (vtab.OperatorOverloadCounter scans):
		// invoke the user's like(pattern, value) once per TRUE evaluation.
		ev.probeOperatorOverload("LIKE", right, left)
	}
	return boolToInt(result), nil
}

// evalRowValueIs implements NULL-safe row-value IS / IS NOT comparison.
// Each element pair is compared with IS semantics (NULL IS NULL is true);
// the whole row value is equal when every element is IS-equal.
func (ev *Evaluator) evalRowValueIs(op string, left, right interface{}) (interface{}, error) {
	lv, lok := left.([]interface{})
	rv, rok := right.([]interface{})
	if !lok || !rok {
		return nil, fmt.Errorf("row value misused")
	}
	if len(lv) != len(rv) {
		return nil, fmt.Errorf("row value misused")
	}
	equal, err := ev.rowValueIsEqual(lv, rv)
	if err != nil {
		return nil, err
	}
	if op == "IS NOT" {
		equal = !equal
	}
	return boolToInt(equal), nil
}

// rowValueIsEqual reports whether two row values are IS-equal element-wise:
// NULL IS NULL is true, and a nested row value whose arity mismatches its
// counterpart is "row value misused".
func (ev *Evaluator) rowValueIsEqual(lv, rv []interface{}) (bool, error) {
	equal := true
	for i := range lv {
		// SQLite rejects nested row values in a comparison: (2,(2,0)) IS
		// (2,(20)) is "row value misused" (the element arity differs). A
		// row-value element compared against a non-row element is likewise
		// misused.
		if err := checkRowValueNesting(lv[i], rv[i]); err != nil {
			return false, err
		}
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
		if ev.ctx.CompareValuesWithCollate(lv[i], rv[i]) != 0 {
			equal = false
			break
		}
	}
	return equal, nil
}

// ftsMatchTable is the MATCH-evaluation contract an FTS table (FTS3/4 or
// fts5) must satisfy: per-document query evaluation plus column resolution.
// evalRowValueCompare implements SQLite row-value comparison:
// (a,b,c) OP (x,y,z) compares element-wise lexicographically. One side is a
// row value ([]interface{}); the other must be a row value of the same arity,
// otherwise an error is raised (SQLite "row value misused" for scalar vs row,
// and "row value comparison with different number of terms" for arity
// mismatch). NULL elements propagate NULL like scalar comparisons: if the
// elements compared so far are equal and one is NULL, the result is NULL.
func (ev *Evaluator) evalRowValueCompare(op string, lv []interface{}, right interface{}) (interface{}, error) {
	rv, ok := right.([]interface{})
	if !ok {
		return nil, fmt.Errorf("row value misused")
	}
	if len(lv) != len(rv) {
		return nil, fmt.Errorf("row value misused")
	}
	// isOrdering reports whether op is an ordering comparison (<, >, <=, >=).
	isOrdering := op == "<" || op == ">" || op == "<=" || op == ">="
	cmp, sawNull, err := ev.rowValueLexCompare(lv, rv, isOrdering)
	if err != nil {
		return nil, err
	}
	// Equality/inequality: a NULL at an undecided position (all compared
	// elements equal) makes the whole comparison NULL unless a later element
	// decided it.
	if sawNull {
		return nil, nil
	}
	return rowValueCompareResult(op, cmp)
}

// rowValueLexCompare compares two row values element-wise lexicographically,
// returning the first non-zero element comparison (or 0 when all equal),
// whether a NULL at an undecided position made the result unknown, and any
// nested-row-value misuse error.
func (ev *Evaluator) rowValueLexCompare(lv, rv []interface{}, isOrdering bool) (int, bool, error) {
	cmp := 0
	eqSawNull := false
	for i := range lv {
		// SQLite rejects nested row values in a comparison: (2,(2,0)) IS
		// (2,(20)) is "row value misused" (the element arity differs).
		if err := checkRowValueNesting(lv[i], rv[i]); err != nil {
			return 0, false, err
		}
		l := lv[i]
		r := rv[i]
		lv0, _ := extractValue(l)
		rv0, _ := extractValue(r)
		if util.UnwrapColumnValue(lv0) == nil || util.UnwrapColumnValue(rv0) == nil {
			// SQLite row-value semantics: a NULL element while the comparison
			// is still undecided makes the result NULL. For ORDERING
			// operators a NULL at an undecided position is final — later
			// elements cannot decide it. For equality/inequality a later
			// differing element DOES decide the result, so keep scanning.
			if isOrdering {
				return 0, true, nil
			}
			eqSawNull = true
			continue
		}
		c := ev.ctx.CompareValuesWithCollate(l, r)
		if c != 0 {
			cmp = c
			// A differing element decides the result regardless of earlier
			// NULLs for equality/inequality ((1,NULL,1) == (1,1,2) is 0).
			eqSawNull = false
			break
		}
	}
	return cmp, eqSawNull, nil
}

// checkRowValueNesting rejects nested row values in a comparison: (2,(2,0))
// IS (2,(20)) is "row value misused" (the element arity differs), and a
// row-value element compared against a non-row element is likewise misused.
func checkRowValueNesting(l, r interface{}) error {
	lRow, lIsRow := l.([]interface{})
	rRow, rIsRow := r.([]interface{})
	if !lIsRow && !rIsRow {
		return nil
	}
	if !lIsRow || !rIsRow || len(lRow) != len(rRow) {
		return fmt.Errorf("row value misused")
	}
	return nil
}

// rowValueCompareResult maps a lexicographic comparison result to the row-
// value comparison operator's boolean result, or an error for an unsupported
// operator.
func rowValueCompareResult(op string, cmp int) (interface{}, error) {
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

func (ev *Evaluator) evalBinaryOpValues(op string, left, right interface{}) (interface{}, error) {
	// Row-value comparison: (a,b,c) OP (x,y,z) compares element-wise
	// lexicographically with SQLite's row-value semantics. A row value
	// compared with a scalar (or a row value of different arity) is an
	// error ("row value misused" / "row value comparison with different
	// number of terms"). Note: a row value is only valid in comparison and
	// IN contexts; comparing via =, <>, <, >, <=, >= is allowed.
	if lv, lok := left.([]interface{}); lok {
		return ev.evalRowValueCompare(op, lv, right)
	}
	if rv, rok := right.([]interface{}); rok {
		return ev.evalRowValueCompare(op, rv, left)
	}
	// Extract collation-wrapped values for non-comparison operators.
	// Comparison operators use compareValuesWithCollate which handles this
	// internally. For || (concatenation), we preserve collation through
	// evalConcat.
	left, right = extractCollatedValues(op, left, right)
	if op == "||" {
		// vdbe.c OP_Concat: output longer than SQLITE_LIMIT_LENGTH
		// fails with "string or blob too big" (sqllimits1-5.17.3/5.21).
		res, err := evalConcat(left, right)
		if err != nil {
			return nil, err
		}
		if s, ok := res.(string); ok {
			if int64(len(s)) > int64(ev.ctx.LengthLimit()) {
				return nil, fmt.Errorf("string or blob too big")
			}
		}
		return res, nil
	}
	if fn, ok := binaryOpDispatch[op]; ok {
		return fn(ev, left, right)
	}
	return evalArithmeticOp(op, left, right)
}

// binaryOpFn evaluates a binary operator over two scalar values.
type binaryOpFn func(ev *Evaluator, left, right interface{}) (interface{}, error)

// binaryOpDispatch maps scalar binary operators (comparison, LIKE/GLOB/REGEXP,
// COLLATE, and the unsupported MATCH/JSON operators) to their evaluators.
var binaryOpDispatch = map[string]binaryOpFn{
	"=":  func(ev *Evaluator, l, r interface{}) (interface{}, error) { return ev.evalEqualityOp(l, r), nil },
	"<>": func(ev *Evaluator, l, r interface{}) (interface{}, error) { return ev.evalInequalityOp(l, r), nil },
	"!=": func(ev *Evaluator, l, r interface{}) (interface{}, error) { return ev.evalInequalityOp(l, r), nil },
	"<": func(ev *Evaluator, l, r interface{}) (interface{}, error) {
		return boolToInt(ev.ctx.CompareValuesWithCollate(l, r) < 0), nil
	},
	">": func(ev *Evaluator, l, r interface{}) (interface{}, error) {
		return boolToInt(ev.ctx.CompareValuesWithCollate(l, r) > 0), nil
	},
	"<=": func(ev *Evaluator, l, r interface{}) (interface{}, error) {
		return boolToInt(ev.ctx.CompareValuesWithCollate(l, r) <= 0), nil
	},
	">=": func(ev *Evaluator, l, r interface{}) (interface{}, error) {
		return boolToInt(ev.ctx.CompareValuesWithCollate(l, r) >= 0), nil
	},
	"LIKE":     func(ev *Evaluator, l, r interface{}) (interface{}, error) { return ev.evalLikeOp(l, r, false), nil },
	"NOT LIKE": func(ev *Evaluator, l, r interface{}) (interface{}, error) { return ev.evalLikeOp(l, r, true), nil },
	"GLOB": func(ev *Evaluator, l, r interface{}) (interface{}, error) {
		bumpLikeCallCount()
		res := globValues(l, r)
		if res {
			ev.probeOperatorOverload("GLOB", r, l)
		}
		return boolToInt(res), nil
	},
	"NOT GLOB": func(ev *Evaluator, l, r interface{}) (interface{}, error) {
		bumpLikeCallCount()
		return boolToInt(!globValues(l, r)), nil
	},
	"REGEXP": func(ev *Evaluator, l, r interface{}) (interface{}, error) {
		res, err := ev.evalRegexpOp(l, r, false)
		if err == nil && res == int64(1) {
			ev.probeOperatorOverload("REGEXP", r, l)
		}
		return res, err
	},
	"NOT REGEXP": func(ev *Evaluator, l, r interface{}) (interface{}, error) { return ev.evalRegexpOp(l, r, true) },
	"MATCH":      nilBinaryFn(0),
	"NOT MATCH":  nilBinaryFn(1),
	"->":         func(ev *Evaluator, l, r interface{}) (interface{}, error) { return function.JSONArrowExtract(l, r) },
	"->>":        func(ev *Evaluator, l, r interface{}) (interface{}, error) { return function.JSONArrowExtractSQL(l, r) },
	"COLLATE":    func(ev *Evaluator, l, r interface{}) (interface{}, error) { return ev.evalCollateOp(l, r) },
	"||": func(ev *Evaluator, l, r interface{}) (interface{}, error) {
		res, err := evalConcat(l, r)
		if err != nil {
			return nil, err
		}
		// vdbe.c OP_Concat: output longer than SQLITE_LIMIT_LENGTH fails
		// with "string or blob too big" (sqllimits1-5.17.3/5.21).
		if s, ok := res.(string); ok {
			if int64(len(s)) > int64(ev.ctx.LengthLimit()) {
				return nil, fmt.Errorf("string or blob too big")
			}
		}
		return res, nil
	},
}

// nilBinaryFn returns a fixed integer result for an operator.
func nilBinaryFn(v int64) binaryOpFn {
	return func(ev *Evaluator, l, r interface{}) (interface{}, error) { return v, nil }
}

// evalEqualityOp evaluates = with SQLite's type-matching rule: when a TEXT
// column value is compared with a value that has no column affinity and the
// actual types differ (TEXT vs numeric), they are not equal.
func (ev *Evaluator) evalEqualityOp(left, right interface{}) interface{} {
	if !typesMatchForEquality(left, right) {
		return int64(0)
	}
	return boolToInt(ev.ctx.CompareValuesWithCollate(left, right) == 0)
}

// evalInequalityOp evaluates <> / != with SQLite's type-matching rule.
func (ev *Evaluator) evalInequalityOp(left, right interface{}) interface{} {
	if !typesMatchForEquality(left, right) {
		return int64(1)
	}
	return boolToInt(ev.ctx.CompareValuesWithCollate(left, right) != 0)
}

// evalLikeOp evaluates LIKE / NOT LIKE with the engine's case-sensitivity
// setting.
func (ev *Evaluator) evalLikeOp(left, right interface{}, negated bool) interface{} {
	bumpLikeCallCount()
	var result bool
	if ev.ctx.CaseSensitiveLike() {
		result = likeValuesCaseSensitive(left, right)
	} else {
		result = likeValues(left, right)
	}
	if negated {
		result = !result
	}
	if result && !negated {
		// Operator-overload probing (vtab.OperatorOverloadCounter scans):
		// invoke the user's like(pattern, value) once per TRUE evaluation.
		ev.probeOperatorOverload("LIKE", right, left)
	}
	return boolToInt(result)
}

// probeOperatorOverload invokes a user-registered override of like/glob/
// regexp for its side effects when an opted-in vtab scan feeds the current
// statement; the override's result never influences filtering.
func (ev *Evaluator) probeOperatorOverload(op string, pattern, value interface{}) {
	if !ev.ctx.OverloadProbe() {
		return
	}
	fn, ok := ev.ctx.Functions().Find(op)
	if !ok || fn.ScalarFn == nil {
		return
	}
	_, _ = fn.ScalarFn([]interface{}{fmt.Sprintf("%v", util.UnwrapColumnValue(pattern)), fmt.Sprintf("%v", util.UnwrapColumnValue(value))})
}

// evalLikeFunction evaluates the LIKE(X, Y [, Z]) function-call form:
// X is the pattern and Y the string, with optional escape character Z (the
// operator form is string LIKE pattern; the function form is pattern, string).
// The escape must be a single character (SQLite runtime error otherwise).
func (ev *Evaluator) evalLikeFunction(args []interface{}) (interface{}, error) {
	bumpLikeCallCount()
	if len(args) == 3 && args[2] != nil {
		esc, ok := util.UnwrapColumnValue(args[2]).(string)
		if !ok || len([]rune(esc)) != 1 {
			return nil, fmt.Errorf("ESCAPE expression must be a single character")
		}
		if ev.ctx.CaseSensitiveLike() {
			return boolToInt(likeValuesWithEscapeCS(args[1], args[0], esc)), nil
		}
		return boolToInt(likeValuesWithEscape(args[1], args[0], esc)), nil
	}
	return ev.evalLikeOp(args[1], args[0], false), nil
}

// evalRegexpOp evaluates REGEXP / NOT REGEXP.
func (ev *Evaluator) evalRegexpOp(left, right interface{}, negated bool) (interface{}, error) {
	b, err := regexpValues(left, right)
	if err != nil {
		return nil, err
	}
	if negated {
		b = !b
	}
	return boolToInt(b), nil
}

// evalCollateOp evaluates the COLLATE operator: returns the left value marked
// with the collation name so comparison operators apply it. An explicit
// COLLATE on either operand wins over a column's declared COLLATE clause.
func (ev *Evaluator) evalCollateOp(left, right interface{}) (interface{}, error) {
	rightStr, ok := right.(string)
	if !ok {
		return left, nil
	}
	switch strings.ToUpper(rightStr) {
	case "", "BINARY", "NOCASE", "RTRIM":
		return &CollatedValue{Value: left, Collation: rightStr, Explicit: true}, nil
	default:
		if ev.ctx.LookupCollation(rightStr) != nil {
			return &CollatedValue{Value: left, Collation: rightStr, Explicit: true}, nil
		}
		return nil, fmt.Errorf("no such collation sequence: %s", rightStr)
	}
}

func evalArithmeticOp(op string, left, right interface{}) (interface{}, error) {
	// Unwrap BlobColumnValue so arithmetic functions see the base value.
	left = util.UnwrapColumnValue(left)
	right = util.UnwrapColumnValue(right)
	if fn, ok := binaryArithOps[op]; ok {
		return fn(left, right)
	}
	switch op {
	case "+":
		return evalAdd(left, right)
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

// binaryArithOps maps binary arithmetic operators to their evaluation
// functions (each returns NULL when either operand is NULL).
var binaryArithOps = map[string]binaryArithFn{
	"-":  nilCheckBinaryFn(subValues),
	"*":  nilCheckBinaryFn(mulValues),
	"/":  nilCheckBinaryFn(divValues),
	"%":  nilCheckBinaryFn(modValues),
	"&":  nilCheckBinaryFn(bitwiseAnd),
	"|":  nilCheckBinaryFn(bitwiseOr),
	"<<": nilCheckBinaryFn(shiftLeft),
	">>": nilCheckBinaryFn(shiftRight),
}

// binaryArithFn is a binary arithmetic evaluation function.
type binaryArithFn func(a, b interface{}) (interface{}, error)

// nilCheckBinaryFn wraps a binary arithmetic function so it returns NULL when
// either operand is NULL.
func nilCheckBinaryFn(fn binaryArithFn) binaryArithFn {
	return func(a, b interface{}) (interface{}, error) {
		if a == nil || b == nil {
			return nil, nil
		}
		return fn(a, b)
	}
}

// exprOutputAffinity resolves the affinity of a subquery output column
// expression. Column references resolve through the FROM source (CTE, table,
// view, or derived-table subquery); numeric/string/cast expressions contribute
// their expression affinity; anything else (functions, arithmetic) has none.
func (ev *Evaluator) exprOutputAffinity(sel *sql.SelectStmt, expr sql.Expr, i int) rune {
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return ev.columnRefOutputAffinity(sel, v, i)
	case *sql.CastExpr:
		return util.Affinity(v.AsType)
	case *sql.NumericLit:
		return 'N'
	case *sql.StringLit:
		return 'T'
	case *sql.BlobLit:
		return 'B'
	case *sql.NullLit:
		return 'B'
	default:
		return 0
	}
}

// columnRefOutputAffinity resolves the affinity of a column-reference output
// expression through the FROM source's column definitions (CTE, table, view,
// or derived-table subquery).
func (ev *Evaluator) columnRefOutputAffinity(sel *sql.SelectStmt, v *sql.ColumnRef, i int) rune {
	defs := ev.ctx.FromSourceColumnDefs(sel.From, nil)
	if v.Name == "*" {
		// A bare * expands to the first column of the FROM source.
		if len(defs) > 0 {
			return util.Affinity(defs[0].Type)
		}
		if cte, ok := ev.ctx.FindCTE(sel, sel.From.Name); ok {
			return ev.ctx.CompoundColumnAffinity(cte.Select, 0)
		}
		return 0
	}
	// A named column reference: match by column name (and optional table
	// qualifier) against the FROM source's column definitions.
	for _, cd := range defs {
		if cd.Name == v.Name || (v.Table != "" && cd.Name == v.Table+"."+v.Name) {
			return util.Affinity(cd.Type)
		}
	}
	if cte, ok := ev.ctx.FindCTE(sel, sel.From.Name); ok {
		if i < len(cte.Columns) && len(cte.Columns) > 0 && cte.Columns[i] == v.Name {
			return ev.ctx.CompoundColumnAffinity(cte.Select, i)
		}
	}
	return 0
}
