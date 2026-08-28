package exec

import (
	"math"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// Generic virtual-table WHERE-clause rowid handling: modules implementing
// vtab.RowidRangeConsumer (unionvtab) learn the rowid interval the scan will
// cover so they can pick which source tables to read.

// isFilterLiteral reports whether x is a literal expression (number,
// string, NULL, blob, or negated number) whose value is available before
// the scan starts. C's xBestIndex hands the module only directly usable RHS
// values — a column reference (`cc.rowid>c4.rowid`, unionvtab.test 5.3) is
// NOT usable and the conjunct must stay a plain filter.
func isFilterLiteral(x sql.Expr) bool {
	switch v := x.(type) {
	case *sql.NumericLit, *sql.StringLit, *sql.NullLit, *sql.BlobLit:
		return true
	case *sql.UnaryOp:
		if v.Operator == "-" {
			_, ok := v.Operand.(*sql.NumericLit)
			return ok
		}
	}
	return false
}

// consumed marks conjuncts the module omits (BinaryOp comparisons and
// BETWEEN forms).

// consumeVTabRowidRange feeds the WHERE-clause rowid interval to modules
// implementing RowidRangeConsumer (unionvtab source selection). The
// constraint STAYS in the residual WHERE — chosen sources are fully
// scanned and the core re-filters values (unionvtab.test 3.4/3.5).
func (e *Engine) consumeVTabRowidRange(vt vtab.VirtualTable, where sql.Expr) sql.Expr {
	rc, ok := vt.(vtab.RowidRangeConsumer)
	if !ok || rc == nil || where == nil {
		return nil
	}
	// The rowid aliases: the implicit rowid names, plus the declared
	// INTEGER-PK column when the module declares one (unionvtab pTab->iPK —
	// xBestIndex consumes constraints on that column like rowid ones;
	// unionvtab.test 3.10 filters on the IPK column, 5.1 on a non-PK one).
	rowidNames := map[string]bool{"rowid": true, "_rowid_": true, "oid": true}
	var cols []string
	if ci, ok := vt.(vtab.ColumnInfo); ok {
		cols = ci.Columns()
	}
	if rn, ok := vt.(vtab.RowidColumner); ok {
		if i := rn.RowidColumn(); i >= 0 && i < len(cols) {
			rowidNames[strings.ToLower(cols[i])] = true
		}
	}
	consumed := map[sql.Expr]bool{}
	var lo, hi *int64
	var loIncl, hiIncl bool
	apply := func(v int64, op string) {
		switch op {
		case "<":
			if v == math.MinInt64 {
				// Nothing is < MinInt64: force an empty selection
				// (hi below every possible source min).
				zero := int64(0)
				mone := int64(-1)
				lo, hi = &zero, &mone
				break
			}
			nv := v - 1
			if hi == nil || nv < *hi {
				hi = &nv
			}
			hiIncl = true
		case "<=":
			nv := v
			if hi == nil || nv < *hi {
				hi = &nv
			}
			hiIncl = true
		case ">":
			if v == math.MaxInt64 {
				// Nothing is > MaxInt64: force an empty selection.
				zero := int64(0)
				mone := int64(-1)
				lo, hi = &zero, &mone
				break
			}
			nv := v + 1
			if lo == nil || nv > *lo {
				lo = &nv
			}
			loIncl = true
		case ">=":
			nv := v
			if lo == nil || nv > *lo {
				lo = &nv
			}
			loIncl = true
		case "=":
			nv := v
			lo, hi = &nv, &nv
			loIncl, hiIncl = true, true
		}
	}
	for _, conj := range splitAndConjuncts(where) {
		if bt, isBt := conj.(*sql.Between); isBt {
			e.consumeRowidBetween(bt, rowidNames, apply, consumed)
			continue
		}
		bo, isOp := conj.(*sql.BinaryOp)
		if !isOp {
			continue
		}
		op := strings.ToUpper(bo.Operator)
		if op != "<" && op != "<=" && op != ">" && op != ">=" && op != "=" {
			continue
		}
		e.consumeRowidComparison(bo, op, rowidNames, apply, consumed)
	}
	// Arm the range on EVERY call — with no consumed conjunct lo/hi stay nil
	// (the unconstrained full range, C idxNum==0). The consumed range lives
	// on the per-table cached instance, so skipping the arm would leak the
	// previous statement's selection into this scan.
	rc.ConsumeRowidRange(lo, loIncl, hi, hiIncl)
	if len(consumed) == 0 {
		return nil
	}
	return removeConsumed(where, consumed)
}

// consumeRowidComparison feeds one `rowid <op> literal` (either orientation)
// conjunct to the interval builder; consumed marks it for omission.
func (e *Engine) consumeRowidComparison(
	bo *sql.BinaryOp, op string, rowidNames map[string]bool,
	apply func(int64, string), consumed map[sql.Expr]bool,
) {
	lref, lok := bo.Left.(*sql.ColumnRef)
	if lok && rowidNames[strings.ToLower(lref.Name)] && isFilterLiteral(bo.Right) {
		if v, err := e.evalExpr(bo.Right, nil); err == nil {
			if n, ok := vtab.AsVtabInt64(util.UnwrapColumnValue(v)); ok {
				apply(n, op)
				consumed[bo] = true
			}
		}
		return
	}
	rref, rok := bo.Right.(*sql.ColumnRef)
	if rok && rowidNames[strings.ToLower(rref.Name)] && isFilterLiteral(bo.Left) {
		mirror := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<=", "=": "="}[op]
		if v, err := e.evalExpr(bo.Left, nil); err == nil {
			if n, ok := vtab.AsVtabInt64(util.UnwrapColumnValue(v)); ok {
				apply(n, mirror)
				consumed[bo] = true
			}
		}
	}
}

// consumeRowidBetween feeds a `rowid BETWEEN lo AND hi` conjunct to the
// interval builder as a >= / <= pair. xBestIndex sees BETWEEN as two usable
// GE/LE constraints (both omitted), so the conjunct is consumed when both
// bounds are scan-start literals. NOT BETWEEN has a disjoint range and
// stays a plain filter.
func (e *Engine) consumeRowidBetween(
	bt *sql.Between, rowidNames map[string]bool,
	apply func(int64, string), consumed map[sql.Expr]bool,
) {
	if bt.Negated {
		return
	}
	oref, ok := bt.Operand.(*sql.ColumnRef)
	if !ok || !rowidNames[strings.ToLower(oref.Name)] {
		return
	}
	if !isFilterLiteral(bt.Low) || !isFilterLiteral(bt.High) {
		return
	}
	lv, lerr := e.evalExpr(bt.Low, nil)
	hv, herr := e.evalExpr(bt.High, nil)
	if lerr != nil || herr != nil {
		return
	}
	ln, lok := vtab.AsVtabInt64(util.UnwrapColumnValue(lv))
	hn, hok := vtab.AsVtabInt64(util.UnwrapColumnValue(hv))
	if !lok || !hok {
		return
	}
	apply(ln, ">=")
	apply(hn, "<=")
	consumed[bt] = true
}

// removeConsumed rebuilds the WHERE tree skipping consumed operators.
func removeConsumed(where sql.Expr, consumed map[sql.Expr]bool) sql.Expr {
	switch w := where.(type) {
	case *sql.BinaryOp:
		if strings.EqualFold(w.Operator, "AND") {
			l := removeConsumed(w.Left, consumed)
			r := removeConsumed(w.Right, consumed)
			switch {
			case l == nil:
				return r
			case r == nil:
				return l
			default:
				return &sql.BinaryOp{Operator: "AND", Left: l, Right: r}
			}
		}
		if consumed[w] {
			return nil
		}
	case *sql.Between:
		if consumed[w] {
			return nil
		}
	}
	return where
}

// dropRowidRangeConjuncts removes rowid/IPK comparison conjuncts from the
// WHERE expression (unionvtab omits them once it has chosen source scans).
// Only conjuncts whose other side is a scan-start literal are dropped —
// a column-vs-column comparison must stay a filter.
func dropRowidRangeConjuncts(where sql.Expr, rowidNames map[string]bool) sql.Expr {
	if where == nil {
		return nil
	}
	switch w := where.(type) {
	case *sql.BinaryOp:
		if strings.EqualFold(w.Operator, "AND") {
			l := dropRowidRangeConjuncts(w.Left, rowidNames)
			r := dropRowidRangeConjuncts(w.Right, rowidNames)
			switch {
			case l == nil:
				return r
			case r == nil:
				return l
			default:
				return &sql.BinaryOp{Operator: "AND", Left: l, Right: r}
			}
		}
		op := strings.ToUpper(w.Operator)
		if op == "<" || op == "<=" || op == ">" || op == ">=" || op == "=" {
			if lr, ok := w.Left.(*sql.ColumnRef); ok && rowidNames[strings.ToLower(lr.Name)] && isFilterLiteral(w.Right) {
				return nil
			}
			if rr, ok := w.Right.(*sql.ColumnRef); ok && rowidNames[strings.ToLower(rr.Name)] && isFilterLiteral(w.Left) {
				return nil
			}
		}
	}
	return where
}

// dropVTabRowidConjuncts strips rowid/IPK range conjuncts for modules that
// consume them via RowidRangeConsumer; returns nil when nothing changed.
func (e *Engine) dropVTabRowidConjuncts(vt vtab.VirtualTable, where sql.Expr) (sql.Expr, bool) {
	rc, ok := vt.(vtab.RowidRangeConsumer)
	if !ok || rc == nil || where == nil {
		return nil, false
	}
	names := map[string]bool{"rowid": true, "_rowid_": true, "oid": true}
	var cols []string
	if ci, ok := vt.(vtab.ColumnInfo); ok {
		cols = ci.Columns()
	}
	if rn, ok := vt.(vtab.RowidColumner); ok {
		if i := rn.RowidColumn(); i >= 0 && i < len(cols) {
			names[strings.ToLower(cols[i])] = true
		}
	}
	cleaned := dropRowidRangeConjuncts(where, names)
	return cleaned, true
}
