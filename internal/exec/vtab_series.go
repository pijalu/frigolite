package exec

import (
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// seriesValueBounds is the narrowed [min,max] range for the "value" column
// plus which constraint kinds were seen (series.c xFilter idxNum bits).
type seriesValueBounds struct {
	min, max *int64
	lower    bool // equality or >= / > constraint seen (0x0080/0x0100/0x0200)
	upper    bool // equality or <= / < constraint seen (0x0080/0x1000/0x2000)
}

// seriesValueRange extracts [min,max] bounds for the virtual table's "value"
// column from a WHERE clause (series.c xBestIndex value-constraint flags):
// value = X, value >/>= X, value </<= X, reversed operand orders, and
// BETWEEN. ANDed conjuncts combine. ok is false when no bound was found.
func seriesValueRange(where sql.Expr) (b seriesValueBounds, ok bool) {
	var s seriesBoundState
	for _, conj := range splitAndConjuncts(where) {
		s.applyConjunct(conj)
	}
	if s.empty {
		// Provably unsatisfiable: an inverted range yields zero rows
		// (NarrowValueRange/init detect start>stop).
		lo, hi := int64(1), int64(0)
		return seriesValueBounds{min: &lo, max: &hi}, true
	}
	return s.b, s.b.min != nil || s.b.max != nil
}

// seriesBoundState accumulates the narrowed value-column bounds while the
// WHERE conjuncts are classified. empty marks a provably unsatisfiable
// constraint combination (further conjuncts are ignored, series.c parity).
type seriesBoundState struct {
	b     seriesValueBounds
	empty bool
}

// applyConjunct applies one AND conjunct to the bounds; non value-column
// comparisons are ignored.
func (s *seriesBoundState) applyConjunct(conj sql.Expr) {
	switch c := conj.(type) {
	case *sql.Between:
		if !strings.EqualFold(colName(c.Operand), "value") || c.Negated {
			return
		}
		s.apply(">=", c.Low)
		s.apply("<=", c.High)
	case *sql.BinaryOp:
		op, valExpr := seriesBinaryOpBound(c)
		if valExpr == nil {
			return
		}
		s.apply(op, valExpr)
	}
}

// apply applies one `value <op> <const>` comparison to the bounds. Integer
// bounds are used exactly (float precision loss near 2^63); other constants
// take the float path. Unrecognized constants are ignored.
func (s *seriesBoundState) apply(op string, valExpr sql.Expr) {
	if s.empty {
		return
	}
	if n, iok := constInt64(valExpr); iok {
		s.applyIntBound(op, n)
		return
	}
	r, konst := constNum(valExpr)
	if !konst {
		return
	}
	s.applyFloatBound(op, r)
}

// applyIntBound narrows the bounds for an integer-bound comparison
// (series.c xFilter integer branches).
func (s *seriesBoundState) applyIntBound(op string, n int64) {
	switch op {
	case "=", "==":
		s.b.min = setMin(s.b.min, n)
		s.b.max = setMax(s.b.max, n)
		s.b.lower, s.b.upper = true, true
	case ">":
		if n == math.MaxInt64 {
			s.empty = true
		} else {
			s.b.min = setMin(s.b.min, n+1)
			s.b.lower = true
		}
	case ">=":
		s.b.min = setMin(s.b.min, n)
		s.b.lower = true
	case "<":
		if n == math.MinInt64 {
			s.empty = true
		} else {
			s.b.max = setMax(s.b.max, n-1)
			s.b.upper = true
		}
	case "<=":
		s.b.max = setMax(s.b.max, n)
		s.b.upper = true
	}
}

// applyFloatBound narrows the bounds for one REAL-bound comparison and
// records the constraint kinds (series.c xFilter idxNum bits).
func (s *seriesBoundState) applyFloatBound(op string, r float64) {
	var fop bool
	s.b.min, s.b.max, fop = applySeriesFloatBound(s.b.min, s.b.max, op, r)
	if !fop {
		s.empty = true
		return
	}
	switch op {
	case "=", "==", ">", ">=":
		s.b.lower = true
	}
	switch op {
	case "=", "==", "<", "<=":
		s.b.upper = true
	}
}

// seriesBinaryOpBound classifies a value-column binary comparison: it returns
// the normalized operator (flipped when the value column sits on the right)
// and the constant-bound expression, or a nil expression when the operator is
// not a comparison or neither operand is the value column.
func seriesBinaryOpBound(c *sql.BinaryOp) (string, sql.Expr) {
	op := strings.ToUpper(c.Operator)
	switch op {
	case "=", "==", ">", ">=", "<", "<=":
	default:
		return "", nil
	}
	var valExpr sql.Expr
	var flipped bool
	if strings.EqualFold(colName(c.Left), "value") {
		valExpr = c.Right
	} else if strings.EqualFold(colName(c.Right), "value") {
		valExpr, flipped = c.Left, true
	}
	if valExpr == nil {
		return "", nil
	}
	if flipped {
		op = flipSeriesComparison(op)
	}
	return op, valExpr
}

// flipSeriesComparison mirrors a comparison operator for a value-on-the-right
// operand order (value >= X reads as X <= value).
func flipSeriesComparison(op string) string {
	switch op {
	case "<":
		return ">="
	case "<=":
		return ">"
	case ">":
		return "<="
	case ">=":
		return "<"
	}
	return op
}

// seriesTwo63 is 2^63 as a double. (double)LARGEST_INT64 rounds up to exactly
// this value and (double)SMALLEST_INT64 equals its negation.
var seriesTwo63 = float64(1 << 63)

// seriesRealToI64 ports trunk series.c's seriesRealToI64: doubles beyond
// ±(2^63-1024) saturate instead of relying on the platform-defined C cast.
func seriesRealToI64(r float64) int64 {
	const edge = float64(9223372036854774784) // 2^63-1024
	if r < -edge {
		return math.MinInt64
	}
	if r > edge {
		return math.MaxInt64
	}
	return int64(r)
}

// applySeriesFloatBound narrows [min,max] for one REAL-bound comparison on
// the value column, porting trunk series.c xFilter's float branches:
// bounds at/beyond ±2^63 saturate rather than vanish, and strict operators
// adjust by +/-1 in INTEGER space after the conversion (the old ceil(r±1.0)
// form lost the +1 to double rounding). Returns ok=false when no integer row
// can satisfy the constraint.
func applySeriesFloatBound(min, max *int64, op string, r float64) (*int64, *int64, bool) {
	switch op {
	case "=", "==":
		return applySeriesFloatEq(min, max, r)
	case ">", ">=":
		return applySeriesFloatLower(min, max, op, r)
	case "<", "<=":
		return applySeriesFloatUpper(min, max, op, r)
	}
	return min, max, true
}

// applySeriesFloatEq handles `value = <real>`: the bound binds only when the
// double is an exact integer within ±2^63.
func applySeriesFloatEq(min, max *int64, r float64) (*int64, *int64, bool) {
	ce := math.Ceil(r)
	if r != ce || r < -seriesTwo63 || r > seriesTwo63 {
		return min, max, false
	}
	v := seriesRealToI64(r)
	return setMin(min, v), setMax(max, v), true
}

// applySeriesFloatLower handles `value > <real>` / `value >= <real>` (series.c
// float lower-bound branches).
func applySeriesFloatLower(min, max *int64, op string, r float64) (*int64, *int64, bool) {
	if r <= -seriesTwo63 {
		return setMin(min, math.MinInt64), max, true
	}
	if r > seriesTwo63 {
		return min, max, false
	}
	m := seriesRealToI64(math.Ceil(r))
	if op == ">" && r == math.Ceil(r) {
		if m == math.MaxInt64 {
			return min, max, false
		}
		m++
	}
	return setMin(min, m), max, true
}

// applySeriesFloatUpper handles `value < <real>` / `value <= <real>` (series.c
// float upper-bound branches).
func applySeriesFloatUpper(min, max *int64, op string, r float64) (*int64, *int64, bool) {
	if r >= seriesTwo63 {
		return min, setMax(max, math.MaxInt64), true
	}
	if r <= -seriesTwo63 {
		return min, max, false
	}
	m := seriesRealToI64(math.Floor(r))
	if op == "<" && r == math.Floor(r) {
		if m == math.MinInt64 {
			return min, max, false
		}
		m--
	}
	return min, setMax(max, m), true
}

// residualSeriesWhere returns where minus the value-column comparisons the
// series narrowing consumed. seriesBestIndex marks those constraints omit
// (argvConsumed), so SQLite's core never re-checks them at run time; the
// narrowed range already encodes them, including the saturation semantics at
// ±2^63 where re-checking would wrongly drop rows (tabfunc01-1504).
func residualSeriesWhere(vt vtab.VirtualTable, where sql.Expr) (sql.Expr, bool) {
	if _, ok := vt.(vtab.ValueRangeNarrower); !ok {
		return where, false
	}
	if _, has := seriesValueRange(where); !has {
		return where, false
	}
	var kept []sql.Expr
	stripped := false
	for _, conj := range splitAndConjuncts(where) {
		if isSeriesValueConstraint(conj) {
			stripped = true
			continue
		}
		kept = append(kept, conj)
	}
	if !stripped {
		return where, false
	}
	return joinConjuncts(kept), true
}

// isSeriesValueConstraint reports whether expr is a value-column comparison
// with a constant bound — exactly the conjunct shape seriesValueRange
// consumes.
func isSeriesValueConstraint(expr sql.Expr) bool {
	if bt, ok := expr.(*sql.Between); ok {
		if bt.Negated || !strings.EqualFold(colName(bt.Operand), "value") {
			return false
		}
		return seriesConstBound(bt.Low) && seriesConstBound(bt.High)
	}
	b, ok := expr.(*sql.BinaryOp)
	if !ok {
		return false
	}
	switch strings.ToUpper(b.Operator) {
	case "=", "==", ">", ">=", "<", "<=":
	default:
		return false
	}
	var valExpr sql.Expr
	if strings.EqualFold(colName(b.Left), "value") {
		valExpr = b.Right
	} else if strings.EqualFold(colName(b.Right), "value") {
		valExpr = b.Left
	}
	return valExpr != nil && seriesConstBound(valExpr)
}

// seriesConstBound reports whether e is a constant integer or real literal.
func seriesConstBound(e sql.Expr) bool {
	if _, ok := constInt64(e); ok {
		return true
	}
	_, ok := constNum(e)
	return ok
}

// colName returns the referenced column name of an expression, or "" when the
// expression is not a plain column reference.
func colName(e sql.Expr) string {
	if cr, ok := e.(*sql.ColumnRef); ok {
		return cr.Name
	}
	return ""
}

// constInt64 evaluates a constant integer expression (literal or -literal).
func constInt64(expr sql.Expr) (int64, bool) {
	switch v := expr.(type) {
	case *sql.NumericLit:
		if n, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
			return n, true
		}
	case *sql.UnaryOp:
		if v.Operator == "-" {
			if inner, ok := v.Operand.(*sql.NumericLit); ok {
				if n, err := strconv.ParseInt("-"+inner.Value, 10, 64); err == nil {
					return n, true
				}
			}
		}
	}
	return 0, false
}

func setMin(cur *int64, n int64) *int64 {
	if cur == nil || n > *cur {
		v := n
		return &v
	}
	return cur
}

func setMax(cur *int64, n int64) *int64 {
	if cur == nil || n < *cur {
		v := n
		return &v
	}
	return cur
}

// constNum evaluates a constant numeric expression.
func constNum(expr sql.Expr) (float64, bool) {
	switch v := expr.(type) {
	case *sql.NumericLit:
		f, err := strconv.ParseFloat(strings.TrimPrefix(v.Value, "+"), 64)
		return f, err == nil
	case *sql.UnaryOp:
		if v.Operator == "-" {
			if f, ok := constNum(v.Operand); ok {
				return -f, true
			}
		}
	}
	return 0, false
}
