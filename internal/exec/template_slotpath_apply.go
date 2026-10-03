package exec

import (
	"math"

	"github.com/pijalu/frigolite/internal/sql"
)

// Slot-path substitution: value gates and in-place literal writes on a
// template entry's live per-depth clone (see template_slotpath.go for the
// path table and its correctness contract).

// validateValues reports whether the hit's values can serve this slot table:
// the count matches and every value passes its slot's kind gate (the exact
// gates the COW walkers apply — a false here falls back to the COW path,
// which refuses identically).
func (ts *templateSlots) validateValues(values []interface{}) bool {
	if len(values) != len(ts.paths) {
		return false
	}
	for i, c := range ts.classes {
		switch v := values[i].(type) {
		case int64:
			if c == slotReal {
				return false
			}
		case float64:
			if c == slotInt || c == slotStr {
				return false
			}
			// REAL gates mirror numeric(): finite, not the shape-ambiguous
			// 2^63 double. The INSERT family (slotInsAny) keeps insertValue's
			// unchecked rendering — parity with the COW walk by construction.
			if c == slotReal && (math.IsInf(v, 0) || math.IsNaN(v) || v == twoPow63) {
				return false
			}
		case string:
			// A quoted value serves any slot (the fresh node kind follows
			// the value).
		default:
			return false
		}
	}
	return true
}

// apply rewrites the literal leaves of the live clone along the precomputed
// paths. validateValues MUST have passed: apply assumes every gate holds.
// It returns false when a path cannot be followed (unreachable for a
// collector-produced table — belt and braces: the caller falls back to the
// COW clone rather than serve a stale literal).
func (ts *templateSlots) apply(stmt sql.Stmt, values []interface{}) bool {
	for i, p := range ts.paths {
		var cur any = stmt
		for si := 0; si < len(p.steps)-1; si++ {
			next, ok := slotStepNode(cur, p.steps[si], nil, false)
			if !ok {
				return false
			}
			cur = next
		}
		if !ts.writeSlot(cur, p.steps[len(p.steps)-1], values[i]) {
			return false
		}
	}
	return true
}

// writeSlot rewrites one literal slot through its parent node and terminal
// selector. The target node kind follows the VALUE (what a fresh parse of
// the statement text produces): int64/float64 yield NumericLit, string
// yields StringLit — regardless of the slot's current node kind (an INSERT
// tuple slot first seen with a quoted literal serves a numeric statement and
// vice versa). The node is rewritten in place when the kind matches (the
// common OLTP case), dropping the NumericLit parsed-value cache — execution
// caches the parsed literal on first evaluation, and a stale cache would
// serve the PREVIOUS statement's value for the rewritten text (the COW form
// never faces this: its literal nodes are always fresh). A kind switch
// writes a fresh literal node.
func (ts *templateSlots) writeSlot(cur any, st slotStep, val interface{}) bool {
	node, ok := slotStepNode(cur, st, nil, false)
	if !ok {
		return false
	}
	switch v := val.(type) {
	case int64:
		if n, isNum := node.(*sql.NumericLit); isNum {
			n.Value = int64Text(v)
			n.SetCached(nil)
			return true
		}
		return slotSet(cur, st, &sql.NumericLit{Value: int64Text(v)})
	case float64:
		if n, isNum := node.(*sql.NumericLit); isNum {
			n.Value = floatSlotText(v)
			n.SetCached(nil)
			return true
		}
		return slotSet(cur, st, &sql.NumericLit{Value: floatSlotText(v)})
	case string:
		if n, isStr := node.(*sql.StringLit); isStr {
			n.Value = v
			return true
		}
		return slotSet(cur, st, &sql.StringLit{Value: v})
	}
	return false
}

// slotSet writes v through the terminal selector of cur.
func slotSet(cur any, st slotStep, v sql.Expr) bool {
	_, ok := slotStepNode(cur, st, v, true)
	return ok
}

// joinArgIdx packs (outer index, item index) for the two-level slice fields
// (join-table args, window partitions/terms).
func joinArgIdx(outer, item int) int { return outer<<8 | item }
func joinArgOuter(idx int) int       { return idx >> 8 }
func joinArgItem(idx int) int        { return idx & 0xff }

// slotStepNode follows (set=false) or writes (set=true) one selector. It
// returns the selected child for descent; set=true's first result is
// meaningless. A false result means the selector does not fit the current
// node (unreachable for a collector-produced path).
func slotStepNode(cur any, st slotStep, v sql.Expr, set bool) (any, bool) {
	switch node := cur.(type) {
	case *sql.SelectStmt:
		return slotSelectNode(node, st, v, set)
	case *sql.UpdateStmt:
		return slotUpdateNode(node, st, v, set)
	case *sql.DeleteStmt:
		return slotDeleteNode(node, st, v, set)
	case *sql.InsertStmt:
		return slotInsertNode(node, st, v, set)
	case *sql.OnConflictClause:
		return slotConflictNode(node, st, v, set)
	case *sql.BinaryOp:
		switch st.field {
		case sfBinLeft:
			return exprFieldNode(&node.Left, v, set)
		case sfBinRight:
			return exprFieldNode(&node.Right, v, set)
		}
	case *sql.UnaryOp:
		if st.field == sfUnary {
			return exprFieldNode(&node.Operand, v, set)
		}
	case *sql.ParenExpr:
		if st.field == sfParen {
			return exprFieldNode(&node.Expr, v, set)
		}
	case *sql.Between:
		switch st.field {
		case sfBetweenOp:
			return exprFieldNode(&node.Operand, v, set)
		case sfBetweenLow:
			return exprFieldNode(&node.Low, v, set)
		case sfBetweenHigh:
			return exprFieldNode(&node.High, v, set)
		}
	case *sql.InList:
		switch st.field {
		case sfInOperand:
			return exprFieldNode(&node.Operand, v, set)
		case sfInItem:
			return exprSliceNode(&node.List, st.idx, v, set)
		}
	case *sql.RowValue:
		if st.field == sfRowVal {
			return exprSliceNode(&node.Values, st.idx, v, set)
		}
	case *sql.CastExpr:
		if st.field == sfCast {
			return exprFieldNode(&node.Operand, v, set)
		}
	case *sql.IsNull:
		if st.field == sfIsNull {
			return exprFieldNode(&node.Operand, v, set)
		}
	case *sql.IsNotNull:
		if st.field == sfIsNotNull {
			return exprFieldNode(&node.Operand, v, set)
		}
	case *sql.IsDistinctFrom:
		switch st.field {
		case sfDistL:
			return exprFieldNode(&node.Left, v, set)
		case sfDistR:
			return exprFieldNode(&node.Right, v, set)
		}
	case *sql.IsNotDistinctFrom:
		switch st.field {
		case sfNotDistL:
			return exprFieldNode(&node.Left, v, set)
		case sfNotDistR:
			return exprFieldNode(&node.Right, v, set)
		}
	case *sql.IsTrue:
		if st.field == sfTrue {
			return exprFieldNode(&node.Operand, v, set)
		}
	case *sql.IsFalse:
		if st.field == sfFalse {
			return exprFieldNode(&node.Operand, v, set)
		}
	case *sql.FuncCall:
		switch st.field {
		case sfFuncArg:
			return exprSliceNode(&node.Args, st.idx, v, set)
		case sfFuncFilter:
			return exprFieldNode(&node.Filter, v, set)
		case sfFuncOrd:
			return orderByTermNode(&node.OrderBy, st.idx, v, set)
		}
	case *sql.CaseExpr:
		switch st.field {
		case sfCaseOp:
			return exprFieldNode(&node.Operand, v, set)
		case sfCaseWhenWhen:
			if st.idx < len(node.Whens) {
				return exprFieldNode(&node.Whens[st.idx].When, v, set)
			}
		case sfCaseWhenThen:
			if st.idx < len(node.Whens) {
				return exprFieldNode(&node.Whens[st.idx].Then, v, set)
			}
		case sfCaseElse:
			return exprFieldNode(&node.Else, v, set)
		}
	}
	return nil, false
}

// slotSelectNode handles one selector against a SELECT statement (including
// the selector kinds reused by UPDATE/DELETE/INSERT for their CTE lists).
func slotSelectNode(node *sql.SelectStmt, st slotStep, v sql.Expr, set bool) (any, bool) {
	switch st.field {
	case sfSelCTE:
		if st.idx < len(node.CTEs) {
			return node.CTEs[st.idx].Select, true
		}
	case sfSelCol:
		if st.idx < len(node.Columns) {
			return exprFieldNode(&node.Columns[st.idx].Expr, v, set)
		}
	case sfSelFromSub:
		if node.From.Subquery != nil {
			return node.From.Subquery, true
		}
	case sfSelFromArg:
		return exprSliceNode(&node.From.Args, st.idx, v, set)
	case sfSelJoinOn:
		if st.idx < len(node.Joins) {
			return exprFieldNode(&node.Joins[st.idx].On, v, set)
		}
	case sfSelJoinSub:
		if joinArgOuter(st.idx) < len(node.Joins) && node.Joins[joinArgOuter(st.idx)].Table.Subquery != nil {
			return node.Joins[joinArgOuter(st.idx)].Table.Subquery, true
		}
	case sfSelJoinArg:
		if joinArgOuter(st.idx) < len(node.Joins) {
			return exprSliceNode(&node.Joins[joinArgOuter(st.idx)].Table.Args, joinArgItem(st.idx), v, set)
		}
	case sfSelWhere:
		return exprFieldNode(&node.Where, v, set)
	case sfSelGroup:
		return exprSliceNode(&node.GroupBy, st.idx, v, set)
	case sfSelHaving:
		return exprFieldNode(&node.Having, v, set)
	case sfSelWinPart:
		if joinArgOuter(st.idx) < len(node.Windows) {
			return exprSliceNode(&node.Windows[joinArgOuter(st.idx)].Partitions, joinArgItem(st.idx), v, set)
		}
	case sfSelWinOrd:
		if joinArgOuter(st.idx) < len(node.Windows) {
			return orderByTermNode(&node.Windows[joinArgOuter(st.idx)].OrderBy, joinArgItem(st.idx), v, set)
		}
	case sfSelOrder:
		return orderByTermNode(&node.OrderBy, st.idx, v, set)
	case sfSelLimit:
		return exprFieldNode(&node.Limit, v, set)
	case sfSelOffset:
		return exprFieldNode(&node.Offset, v, set)
	case sfSelUnion:
		if node.Union != nil {
			return node.Union, true
		}
	}
	return nil, false
}

// slotUpdateNode handles one selector against an UPDATE statement.
func slotUpdateNode(node *sql.UpdateStmt, st slotStep, v sql.Expr, set bool) (any, bool) {
	switch st.field {
	case sfSelCTE:
		if st.idx < len(node.CTEs) {
			return node.CTEs[st.idx].Select, true
		}
	case sfUpdAssign:
		if st.idx < len(node.Assignments) {
			return exprFieldNode(&node.Assignments[st.idx].Value, v, set)
		}
	case sfUpdFromSub:
		if node.From.Subquery != nil {
			return node.From.Subquery, true
		}
	case sfUpdFromArg:
		return exprSliceNode(&node.From.Args, st.idx, v, set)
	case sfUpdFJOn:
		if st.idx < len(node.FromJoins) {
			return exprFieldNode(&node.FromJoins[st.idx].On, v, set)
		}
	case sfUpdFJSub:
		if joinArgOuter(st.idx) < len(node.FromJoins) && node.FromJoins[joinArgOuter(st.idx)].Table.Subquery != nil {
			return node.FromJoins[joinArgOuter(st.idx)].Table.Subquery, true
		}
	case sfUpdFJArg:
		if joinArgOuter(st.idx) < len(node.FromJoins) {
			return exprSliceNode(&node.FromJoins[joinArgOuter(st.idx)].Table.Args, joinArgItem(st.idx), v, set)
		}
	case sfUpdWhere:
		return exprFieldNode(&node.Where, v, set)
	case sfUpdOrder:
		return orderByTermNode(&node.OrderBy, st.idx, v, set)
	case sfUpdLimit:
		return exprFieldNode(&node.Limit, v, set)
	case sfUpdOffset:
		return exprFieldNode(&node.Offset, v, set)
	case sfUpdReturning:
		return exprFieldNode(&node.Returning.Expr, v, set)
	}
	return nil, false
}

// slotDeleteNode handles one selector against a DELETE statement.
func slotDeleteNode(node *sql.DeleteStmt, st slotStep, v sql.Expr, set bool) (any, bool) {
	switch st.field {
	case sfSelCTE:
		if st.idx < len(node.CTEs) {
			return node.CTEs[st.idx].Select, true
		}
	case sfDelWhere:
		return exprFieldNode(&node.Where, v, set)
	case sfDelOrder:
		return orderByTermNode(&node.OrderBy, st.idx, v, set)
	case sfDelLimit:
		return exprFieldNode(&node.Limit, v, set)
	case sfDelOffset:
		return exprFieldNode(&node.Offset, v, set)
	case sfDelReturning:
		return exprFieldNode(&node.Returning.Expr, v, set)
	}
	return nil, false
}

// slotInsertNode handles one selector against an INSERT statement.
func slotInsertNode(node *sql.InsertStmt, st slotStep, v sql.Expr, set bool) (any, bool) {
	switch st.field {
	case sfSelCTE:
		if st.idx < len(node.CTEs) {
			return node.CTEs[st.idx].Select, true
		}
	case sfInsTuple:
		tuple, item := insTupleOf(st.idx), insItemOf(st.idx)
		if tuple < len(node.Values) && item < len(node.Values[tuple]) {
			return exprSliceNode(&node.Values[tuple], item, v, set)
		}
	case sfInsSelect:
		if node.Select != nil {
			return node.Select, true
		}
	case sfInsConflict:
		if node.OnConflict != nil {
			return node.OnConflict, true
		}
	}
	return nil, false
}

// slotConflictNode handles one selector against an ON CONFLICT clause.
func slotConflictNode(node *sql.OnConflictClause, st slotStep, v sql.Expr, set bool) (any, bool) {
	switch st.field {
	case sfOCTgtWhere:
		return exprFieldNode(&node.TargetWhere, v, set)
	case sfOCAssign:
		if st.idx < len(node.Assignments) {
			return exprFieldNode(&node.Assignments[st.idx].Value, v, set)
		}
	case sfOCWhere:
		return exprFieldNode(&node.Where, v, set)
	case sfOCNext:
		if node.Next != nil {
			return node.Next, true
		}
	}
	return nil, false
}

// exprFieldNode reads (set=false) or writes (set=true) one Expr field via
// its address.
func exprFieldNode(f *sql.Expr, v sql.Expr, set bool) (any, bool) {
	if set {
		*f = v
		return nil, true
	}
	if *f == nil {
		return nil, false
	}
	return *f, true
}

// exprSliceNode reads or writes one item of an expression slice.
func exprSliceNode(s *[]sql.Expr, i int, v sql.Expr, set bool) (any, bool) {
	if i >= len(*s) {
		return nil, false
	}
	if set {
		(*s)[i] = v
		return nil, true
	}
	if (*s)[i] == nil {
		return nil, false
	}
	return (*s)[i], true
}

// orderByTermNode reads or writes one ORDER BY term's expression.
func orderByTermNode(s *[]sql.OrderByTerm, i int, v sql.Expr, set bool) (any, bool) {
	if i >= len(*s) {
		return nil, false
	}
	if set {
		(*s)[i].Expr = v
		return nil, true
	}
	if (*s)[i].Expr == nil {
		return nil, false
	}
	return (*s)[i].Expr, true
}

// exprHasLiteral reports whether any NumericLit/StringLit sits anywhere
// below e, including inside nested subqueries (the INSERT tuple contract's
// value-count divergence check).
func exprHasLiteral(e sql.Expr) bool {
	switch v := e.(type) {
	case nil:
		return false
	case *sql.NumericLit, *sql.StringLit:
		return true
	case *sql.FuncCall:
		return exprListHasLiteral(v.Args) || exprHasLiteral(v.Filter) || orderByHasLiteral(v.OrderBy)
	case *sql.CaseExpr:
		if exprHasLiteral(v.Operand) || exprHasLiteral(v.Else) {
			return true
		}
		for i := range v.Whens {
			if exprHasLiteral(v.Whens[i].When) || exprHasLiteral(v.Whens[i].Then) {
				return true
			}
		}
		return false
	case *sql.BinaryOp:
		return exprHasLiteral(v.Left) || exprHasLiteral(v.Right)
	case *sql.UnaryOp:
		return exprHasLiteral(v.Operand)
	case *sql.ParenExpr:
		return exprHasLiteral(v.Expr)
	case *sql.Between:
		return exprHasLiteral(v.Operand) || exprHasLiteral(v.Low) || exprHasLiteral(v.High)
	case *sql.InList:
		return exprHasLiteral(v.Operand) || exprListHasLiteral(v.List)
	case *sql.RowValue:
		return exprListHasLiteral(v.Values)
	case *sql.CastExpr:
		return exprHasLiteral(v.Operand)
	case *sql.IsNull:
		return exprHasLiteral(v.Operand)
	case *sql.IsNotNull:
		return exprHasLiteral(v.Operand)
	case *sql.IsDistinctFrom:
		return exprHasLiteral(v.Left) || exprHasLiteral(v.Right)
	case *sql.IsNotDistinctFrom:
		return exprHasLiteral(v.Left) || exprHasLiteral(v.Right)
	case *sql.IsTrue:
		return exprHasLiteral(v.Operand)
	case *sql.IsFalse:
		return exprHasLiteral(v.Operand)
	case *sql.Subquery:
		return selectHasLiteral(v.Select)
	case *sql.ExistsExpr:
		return selectHasLiteral(v.Select)
	default:
		// BlobLit/RaiseExpr and unknown kinds carry no substitutable literal,
		// but the collector refuses them anyway — this arm is unreachable in
		// the divergence check's callers.
		return false
	}
}

// exprListHasLiteral reports whether any list item holds a literal.
func exprListHasLiteral(list []sql.Expr) bool {
	for _, e := range list {
		if exprHasLiteral(e) {
			return true
		}
	}
	return false
}

// orderByHasLiteral reports whether any ORDER BY term holds a literal.
func orderByHasLiteral(terms []sql.OrderByTerm) bool {
	for i := range terms {
		if exprHasLiteral(terms[i].Expr) {
			return true
		}
	}
	return false
}

// selectHasLiteral reports whether any literal sits anywhere below a SELECT
// (subquery descent for the INSERT divergence check).
func selectHasLiteral(sel *sql.SelectStmt) bool {
	if sel == nil {
		return false
	}
	for i := range sel.CTEs {
		if selectHasLiteral(sel.CTEs[i].Select) {
			return true
		}
	}
	for i := range sel.Columns {
		if exprHasLiteral(sel.Columns[i].Expr) {
			return true
		}
	}
	if sel.From.Subquery != nil && selectHasLiteral(sel.From.Subquery) {
		return true
	}
	if exprListHasLiteral(sel.From.Args) {
		return true
	}
	for i := range sel.Joins {
		if exprHasLiteral(sel.Joins[i].On) || selectHasLiteral(sel.Joins[i].Table.Subquery) ||
			exprListHasLiteral(sel.Joins[i].Table.Args) {
			return true
		}
	}
	if exprHasLiteral(sel.Where) || exprListHasLiteral(sel.GroupBy) || exprHasLiteral(sel.Having) {
		return true
	}
	for i := range sel.Windows {
		if exprListHasLiteral(sel.Windows[i].Partitions) || orderByHasLiteral(sel.Windows[i].OrderBy) {
			return true
		}
	}
	if orderByHasLiteral(sel.OrderBy) || exprHasLiteral(sel.Limit) || exprHasLiteral(sel.Offset) {
		return true
	}
	return selectHasLiteral(sel.Union)
}
