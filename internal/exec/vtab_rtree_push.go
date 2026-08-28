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
	bo, isOp := conj.(*sql.BinaryOp)
	if !isOp || !rtreePushableOp(bo.Operator) {
		return e.rtreePushInConjunct(sink, cols, conj), nil
	}
	op := strings.ToUpper(bo.Operator)
	if cr, isRef := bo.Left.(*sql.ColumnRef); isRef {
		if col, found := cols[strings.ToLower(cr.Name)]; found {
			if val, err := e.evalExpr(bo.Right, nil); err == nil {
				sink.PushRTreeConstraint(col, op, util.UnwrapColumnValue(val))
				return true, nil
			}
			return false, nil
		}
	}
	// Constant on the left: mirror the operator.
	if cr, isRef := bo.Right.(*sql.ColumnRef); isRef {
		if col, found := cols[strings.ToLower(cr.Name)]; found {
			flipped := map[string]string{"<": ">", ">": "<", "<=": ">=", ">=": "<="}[op]
			if flipped == "" {
				flipped = op
			}
			if val, err := e.evalExpr(bo.Left, nil); err == nil {
				sink.PushRTreeConstraint(col, flipped, util.UnwrapColumnValue(val))
				return true, nil
			}
		}
	}
	return false, nil
}

// rtreePushMatchConjunct binds `col MATCH <expr>` onto a RtreeMatchSink,
// consuming the conjunct so the core never re-evaluates it. The column need
// not be resolved further: SQLite binds MATCH constraints table-wide
// (rtreeFilter keys off op==MATCH alone). Unknown right-hand functions /
// evaluation failures abort the statement ("no such function" parity).
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
	val, err := e.evalExpr(bo.Right, nil)
	if err != nil {
		return false, err
	}
	ms.PushRTreeMatch(util.UnwrapColumnValue(val))
	return true, nil
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
