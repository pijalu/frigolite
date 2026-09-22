package exec

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// ---- rtree coordinate-constraint pushdown helpers (xBestIndex/xFilter) ----

// declaredColumnLower maps an instance's declared column names (lowercased)
// to their indexes.
func declaredColumnLower(vt vtab.VirtualTable) map[string]int {
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil
	}
	m := make(map[string]int, len(ci.Columns()))
	for i, c := range ci.Columns() {
		m[strings.ToLower(c)] = i
	}
	return m
}

// rtreePushableOp reports whether op is one of the comparison operators the
// r-tree filter consumes.
func rtreePushableOp(op string) bool {
	switch strings.ToUpper(op) {
	case "=", "<", "<=", ">", ">=", "<>", "!=":
		return true
	}
	return false
}

// rtreePushConjunct pushes one AND-conjunct onto sink when it constrains a
// declared vtab column with a constant operand. Reports whether consumed.
// Column-on-the-right comparisons (`5 < c1`) are normalized by flipping.
// A MATCH conjunct evaluated against a registered geometry function is
// consumed after binding the marker (rtreePushMatchConjunct); an evaluation
// error surfaces to the caller so the statement fails like SQLite's prepare.
func (e *Engine) rtreePushConjunct(sink vtab.ConstraintSink, cols map[string]int, conj sql.Expr) (bool, error) {
	if bo, isOp := conj.(*sql.BinaryOp); isOp && strings.EqualFold(bo.Operator, "MATCH") {
		return e.rtreePushMatchConjunct(sink, cols, bo)
	}
	// geopoly-family overloaded-function constraints (geopoly.c
	// geopolyBestIndex idxNum 2/3): geopoly_overlap(_shape, X) and
	// geopoly_within(_shape, X) narrow the scan to X's bounding-box candidate
	// set via the sink, but are NOT consumed — C leaves
	// aConstraintUsage[].omit = 0 so the core re-checks the true polygon
	// predicate per candidate row.
	if e.rtreePushGeopolyFunc(sink, cols, conj) {
		return false, nil
	}
	bo, isOp := conj.(*sql.BinaryOp)
	if !isOp || !rtreePushableOp(bo.Operator) {
		return e.rtreePushInConjunct(sink, cols, conj), nil
	}
	return rtreePushComparison(e, sink, cols, bo, strings.ToUpper(bo.Operator))
}

// rtreePushGeopolyFunc handles a 2-argument overloaded geopoly function
// conjunct (geopoly_overlap/_within) whose first argument is the declared
// shape column: it pushes the second argument's constant value onto the
// GeopolyFuncSink and reports true (the conjunct is then left unconsumed by
// the caller). Non-function conjuncts and non-geopoly sinks report false and
// fall through to the regular comparison handling.
func (e *Engine) rtreePushGeopolyFunc(sink vtab.ConstraintSink, cols map[string]int, conj sql.Expr) bool {
	fc, isFunc := conj.(*sql.FuncCall)
	if !isFunc || len(fc.Args) != 2 {
		return false
	}
	gs, can := sink.(vtab.GeopolyFuncSink)
	if !can {
		return false
	}
	if cr, isRef := fc.Args[0].(*sql.ColumnRef); isRef {
		if col, found := cols[strings.ToLower(cr.Name)]; found && !exprHasColumnRef(fc.Args[1]) {
			if val, err := e.evalExpr(fc.Args[1], nil); err == nil {
				gs.PushGeopolyFunc(fc.Name, col, util.UnwrapColumnValue(val))
			}
		}
	}
	return true
}

// rtreePushComparison pushes a column-vs-constant comparison onto the sink
// (column on either side; the operator is mirrored for constant-on-left).
// Reports whether the conjunct is consumed.
func rtreePushComparison(e *Engine, sink vtab.ConstraintSink, cols map[string]int, bo *sql.BinaryOp, op string) (bool, error) {
	if pushed := rtreePushColumnLeft(e, sink, cols, bo, op); pushed {
		return true, nil
	}
	// Constant on the left: mirror the operator.
	if pushed := rtreePushColumnRightMirrored(e, sink, cols, bo, op); pushed {
		return true, nil
	}
	return false, nil
}

// rtreePushColumnLeft pushes `col <op> value` when the comparison's LEFT
// side is a declared column with a column-free right operand. Reports
// whether the constraint was pushed.
func rtreePushColumnLeft(e *Engine, sink vtab.ConstraintSink, cols map[string]int, bo *sql.BinaryOp, op string) bool {
	cr, isRef := bo.Left.(*sql.ColumnRef)
	if !isRef {
		return false
	}
	col, found := cols[strings.ToLower(cr.Name)]
	if !found {
		return false
	}
	// Only a column-free value operand may be pushed and consumed:
	// a column reference (join/CTE/outer column — `rt0.b = v0.x`,
	// `id = r.x`) evaluates per joined row, so binding it here with
	// no current row would push col=NULL and drop the conjunct from
	// the residual WHERE (where.c marks such terms usable=false and
	// the core keeps them).
	if exprHasColumnRef(bo.Right) {
		return false
	}
	val, err := e.evalExpr(bo.Right, nil)
	if err != nil {
		return false
	}
	sink.PushRTreeConstraint(col, op, util.UnwrapColumnValue(val))
	return true
}

// rtreePushColumnRightMirrored pushes `value <flipped-op> col` when the
// comparison's RIGHT side is a declared column with a column-free left
// operand ("<" mirrors to ">", "=" mirrors to itself). Reports whether the
// constraint was pushed.
func rtreePushColumnRightMirrored(e *Engine, sink vtab.ConstraintSink, cols map[string]int, bo *sql.BinaryOp, op string) bool {
	cr, isRef := bo.Right.(*sql.ColumnRef)
	if !isRef {
		return false
	}
	col, found := cols[strings.ToLower(cr.Name)]
	if !found {
		return false
	}
	if exprHasColumnRef(bo.Left) {
		return false
	}
	flipped := map[string]string{"<": ">", ">": "<", "<=": ">=", ">=": "<="}[op]
	if flipped == "" {
		flipped = op
	}
	val, err := e.evalExpr(bo.Left, nil)
	if err != nil {
		return false
	}
	sink.PushRTreeConstraint(col, flipped, util.UnwrapColumnValue(val))
	return true
}

// exprHasColumnRef reports whether the expression tree contains any column
// reference (qualifier-qualified or bare).
func exprHasColumnRef(expr sql.Expr) bool {
	switch t := expr.(type) {
	case *sql.ColumnRef:
		return true
	case *sql.BinaryOp:
		return exprHasColumnRef(t.Left) || exprHasColumnRef(t.Right)
	case *sql.UnaryOp:
		return exprHasColumnRef(t.Operand)
	case *sql.FuncCall:
		return exprAnyColumnRef(t.Args)
	case *sql.Between:
		return exprHasColumnRef(t.Operand) || exprHasColumnRef(t.Low) || exprHasColumnRef(t.High)
	case *sql.InList:
		return exprHasColumnRef(t.Operand) || exprAnyColumnRef(t.List)
	case *sql.ParenExpr:
		return exprHasColumnRef(t.Expr)
	}
	return false
}

// exprAnyColumnRef reports whether any expression in the list contains a
// column reference.
func exprAnyColumnRef(exprs []sql.Expr) bool {
	for _, e := range exprs {
		if exprHasColumnRef(e) {
			return true
		}
	}
	return false
}

// rtreePushMatchConjunct binds `col MATCH <expr>` onto a RtreeMatchSink,
// consuming the conjunct so the core never re-evaluates it. The column need
// not be resolved further: SQLite binds MATCH constraints table-wide
// (rtreeFilter keys off op==MATCH alone). Unknown right-hand functions /
// evaluation failures abort the statement ("no such function" parity).
//
// When the right operand is a registered r-tree geometry/query function call,
// the marker is rebuilt directly from the call (rtree.c deserializeGeometry
// analogue): the SQL function itself returns NULL — sqlite3_result_pointer
// reads as NULL in every ordinary context — so the pointer payload is only
// recoverable through this name-keyed path.
func (e *Engine) rtreePushMatchConjunct(sink vtab.ConstraintSink, cols map[string]int, bo *sql.BinaryOp) (bool, error) {
	ms, ok := sink.(vtab.RtreeMatchSink)
	if !ok {
		return false, nil // non-rtree module: leave for FTS handling
	}
	cr, isRef := bo.Left.(*sql.ColumnRef)
	if !isRef {
		return false, nil
	}
	if _, found := cols[strings.ToLower(cr.Name)]; !found {
		return false, nil
	}
	if fc, isFunc := bo.Right.(*sql.FuncCall); isFunc {
		if marker := e.buildRTreeGeometryMarker(fc); marker != nil {
			ms.PushRTreeMatch(marker)
			return true, nil
		}
	}
	val, err := e.evalExpr(bo.Right, nil)
	if err != nil {
		return false, err
	}
	ms.PushRTreeMatch(util.UnwrapColumnValue(val))
	return true, nil
}

// buildRTreeGeometryMarker evaluates a geometry-function call's arguments and
// constructs its opaque marker, or nil when the callee is not a registered
// r-tree geometry/query function.
func (e *Engine) buildRTreeGeometryMarker(fc *sql.FuncCall) *vtab.RtreeGeometry {
	if _, ok := vtab.RtreeGeometryForFunc(fc.Name, nil); !ok {
		return nil
	}
	args := make([]interface{}, 0, len(fc.Args))
	for _, a := range fc.Args {
		v, err := e.evalExpr(a, nil)
		if err != nil {
			return nil
		}
		args = append(args, util.UnwrapColumnValue(v))
	}
	marker, _ := vtab.RtreeGeometryForFunc(fc.Name, args)
	return marker
}

// rtreePushInConjunct pushes `id IN (<ints>)` membership restrictions.
func (e *Engine) rtreePushInConjunct(sink vtab.ConstraintSink, cols map[string]int, conj sql.Expr) bool {
	inop, isIn := conj.(*sql.InList)
	if !isIn || inop.Negated {
		return false
	}
	cr, isRef := inop.Operand.(*sql.ColumnRef)
	if !isRef {
		return false
	}
	col, found := cols[strings.ToLower(cr.Name)]
	if !found || col != 0 || len(inop.List) == 0 {
		return false
	}
	ids := make([]int64, 0, len(inop.List))
	for _, item := range inop.List {
		v, err := e.evalExpr(item, nil)
		if err != nil {
			return false
		}
		switch n := util.UnwrapColumnValue(v).(type) {
		case int64:
			ids = append(ids, n)
		case float64:
			ids = append(ids, int64(n))
		default:
			return false
		}
	}
	sink.PushRTreeRowids(ids)
	return true
}

// dropConsumedConjuncts rebuilds where without the identity-matched consumed
// expressions. Returns nil unchanged sentinel when nothing dropped.
func dropConsumedConjuncts(where sql.Expr, consumed []sql.Expr) sql.Expr {
	dropped := false
	var rebuild func(x sql.Expr) sql.Expr
	rebuild = func(x sql.Expr) sql.Expr {
		if bin, ok := x.(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "AND") {
			l := rebuild(bin.Left)
			r := rebuild(bin.Right)
			if l == nil {
				return r
			}
			if r == nil {
				return l
			}
			bin.Left, bin.Right = l, r
			return bin
		}
		for _, c := range consumed {
			if sameExprIdentity(c, x) {
				dropped = true
				return nil
			}
		}
		return x
	}
	out := rebuild(where)
	if !dropped {
		return where
	}
	return out
}

// sameExprIdentity compares pointer identity; falls back to rendered text for
// cloned trees.
func sameExprIdentity(a, b sql.Expr) bool {
	if a == b {
		return true
	}
	return false // pointer mismatch: treated as distinct (safe — no drop)
}
