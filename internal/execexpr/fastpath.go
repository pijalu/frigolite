// This file holds the typed fast paths for the hottest expression shapes:
// binary arithmetic over bare int64/float64 operands, same-storage-class
// comparisons, and plain-text concatenation. Each fast path mirrors its
// generic counterpart EXACTLY — same results, same Go result types, same
// NULL and NaN handling — and falls back to the generic dispatch for every
// other shape (wrappers, NULLs, mixed classes, text operands). The generic
// path stays the semantic authority: when a rule changes there, the fast
// path must change with it (each helper names the generic function it
// mirrors).
package execexpr

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// arithFastOps reports whether op is one of the five binary arithmetic
// operators the typed fast paths cover (+ - * / %). Every other operator
// (comparisons, LIKE, ||, AND/OR, bitwise, shifts) keeps the generic
// dispatch.
func arithFastOps(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%":
		return true
	}
	return false
}

// fastNumericBinary evaluates + - * / % when BOTH operands are bare numeric
// values (int64/float64, no ColumnValue/CollatedValue wrapper — a wrapper
// fails the type assertions — and non-NULL, since evalBinaryOp's NULL check
// already ran and a bare numeric is never SQL NULL). Returns ok=false for
// any other operand shape, which falls back to evalBinaryOpValues. The both-
// sides requirement is load-bearing: this runs BEFORE the generic path's
// evalArithmeticOp unwrap, so a wrapper on either side must not reach the
// arithmetic (toFloat of a wrapper is 0, not the operand).
//
// The int64 paths mirror addValues/subValues (addInt64/subInt64) and
// mulValues/divValues/modValues' integer branches; the paths with at least
// one float64 operand mirror the REAL-promoted branches of the same
// functions (SQLite: the result is REAL when an operand is REAL, NaN
// becomes NULL, x/0 and x%0 are NULL).
func fastNumericBinary(op string, left, right interface{}) (interface{}, bool) {
	if !arithFastOps(op) {
		return nil, false
	}
	switch l := left.(type) {
	case int64:
		switch r := right.(type) {
		case int64:
			return fastIntArith(op, l, r), true
		case float64:
			return fastFloatArith(op, float64(l), r), true
		}
	case float64:
		switch r := right.(type) {
		case int64:
			return fastFloatArith(op, l, float64(r)), true
		case float64:
			return fastFloatArith(op, l, r), true
		}
	}
	return nil, false
}

// fastIntArith evaluates one arithmetic operator over two bare int64
// operands with the generic path's exact semantics:
//
//	"+" mirrors addValues → addInt64 (overflow promotes to REAL).
//	"-" mirrors subValues → subInt64 (overflow promotes to REAL).
//	"*" mirrors mulValues' integer branch: both operands convert through
//	    float64 first (toFloat's round trip is load-bearing for values above
//	    2^53 — the result must stay bug-compatible, so the fast path performs
//	    the same conversions rather than a direct l*r).
//	"/" mirrors divValues' integer branch: NULL for a zero divisor (SQLite),
//	    then the same float64-converted integer division.
//	"%" mirrors modValues' integer branch: NULL for a zero divisor, then the
//	    same float64-converted integer modulo (SQLite truncates toward zero).
//
// addInt64/subInt64's error result is statically nil for int64 inputs (the
// error arms only fire for non-numeric operands, unreachable here).
func fastIntArith(op string, l, r int64) interface{} {
	switch op {
	case "+":
		v, _ := addInt64(l, r)
		return v
	case "-":
		v, _ := subInt64(l, r)
		return v
	case "*":
		return int64(float64(l)) * int64(float64(r))
	case "/":
		if r == 0 {
			return nil
		}
		return int64(float64(l)) / int64(float64(r))
	default: // "%" (arithFastOps filtered the operator set)
		if r == 0 {
			return nil
		}
		return int64(float64(l)) % int64(float64(r))
	}
}

// fastFloatArith evaluates one arithmetic operator where at least one
// operand is REAL, mirroring the REAL-result branches of addValues
// (addFloatValues' else arm), subValues (subFloatValues' else arm),
// mulValues, divValues, and modValues: with a float64 operand
// NumericIsInt is false on that side, so every operator reduces to the
// plain float computation with NaN→NULL (NanToNil) and NULL for x/0 and
// x%0. The int64 operand converts through toFloat exactly like the generic
// path's toFloat.
func fastFloatArith(op string, af, bf float64) interface{} {
	switch op {
	case "+":
		return NanToNil(af + bf)
	case "-":
		return NanToNil(af - bf)
	case "*":
		return NanToNil(af * bf)
	case "/":
		if bf == 0 {
			return nil
		}
		return NanToNil(af / bf)
	default: // "%"
		if bf == 0 {
			return nil
		}
		return float64(int64(af) % int64(bf))
	}
}

// cmpInt64Fast is util's compareInt64: the three-way int64 comparison.
func cmpInt64Fast(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// cmpFloat64Fast is util's compareFloat64: the three-way float comparison
// where NaN falls to the default arm (NaN compares equal to NaN and
// unordered against everything else — matching the generic path's result
// class-for-class).
func cmpFloat64Fast(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// compareSameClassFast maps a three-way comparison result to the six
// comparison operators' 0/1 results (the shape of rowValueCompareResult and
// dispatchComparisonValues' generic arms).
func compareSameClassFast(op string, cmp int) interface{} {
	switch op {
	case "=":
		return boolToInt(cmp == 0)
	case "<>", "!=":
		return boolToInt(cmp != 0)
	case "<":
		return boolToInt(cmp < 0)
	case ">":
		return boolToInt(cmp > 0)
	case "<=":
		return boolToInt(cmp <= 0)
	default: // ">="
		return boolToInt(cmp >= 0)
	}
}

// fastSameClassCompare evaluates the six comparison operators when both
// operands are bare values of the SAME storage class (int64, float64, or
// string). The operator gate first: dispatchComparisonValues is entered for
// EVERY value-level operator (LIKE, GLOB, ||, ...) and its generic switch
// filters them, so the fast path must apply the same filter before its
// default arm would misread any non-comparison operator as ">=".
//
// A bare operand carries no ColumnValue affinity and no
// CollatedValue marker (the type assertions reject wrappers), so:
//
//   - typesMatchForEquality passes unconditionally (neither side has TEXT
//     column affinity), so = and <> reduce to the plain three-way compare;
//   - compareValuesWithCollate resolves no collation from either side
//     (collation markers and column-ness both come from wrappers), so TEXT
//     compares BINARY — strings.Compare, identical to util's binaryCompare
//     (memcmp prefix, shorter string first).
//
// The int64/int64 arm is util.compareNumericSameClass's compareInt64 arm,
// the float64/float64 arm its compareFloat64 arm. Returns ok=false for any
// other shape (mixed classes, wrappers, NULL, TextCarriers), which falls
// back to the generic comparison walk.
func fastSameClassCompare(op string, left, right interface{}) (interface{}, bool) {
	switch op {
	case "=", "<>", "!=", "<", ">", "<=", ">=":
	default:
		return nil, false
	}
	switch l := left.(type) {
	case int64:
		if r, ok := right.(int64); ok {
			return compareSameClassFast(op, cmpInt64Fast(l, r)), true
		}
	case float64:
		if r, ok := right.(float64); ok {
			return compareSameClassFast(op, cmpFloat64Fast(l, r)), true
		}
	case string:
		if r, ok := right.(string); ok {
			return compareSameClassFast(op, strings.Compare(l, r)), true
		}
	}
	return nil, false
}

// fastConcatOperand renders one || operand the way ConcatValues'
// renderConcatValue does for plain string and int64 values (string maps to
// itself, int64 to strconv.FormatInt — the exact output of the fmt %v walk
// the generic path performs). The ColumnValue affinity wrapper is unwrapped
// first, matching ConcatValues' own unwrap. Returns ok=false for every
// other value (float64's %!.15g rendering, blobs, zeroblobs, TextCarriers),
// which falls back to ConcatValues.
func fastConcatOperand(v interface{}) (string, bool) {
	switch x := util.UnwrapColumnValue(v).(type) {
	case string:
		return x, true
	case int64:
		return strconv.FormatInt(x, 10), true
	}
	return "", false
}
