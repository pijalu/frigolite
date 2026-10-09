package execdml

// Typed SET fast lane for the rowid-pinned point UPDATE (applyPointUpdate's
// collect). The workhorse shape "UPDATE t SET c = c + 1 WHERE id = ?"
// evaluated its SET expressions through the name-keyed row map: every
// operand boxed into a ColumnValue, every reference a map hash, every
// operator the generic dispatch walk. The lane compiles the statement's
// assignment list once per execution into slot-indexed operations and
// evaluates them directly over the collected record's value slots — the
// same evaluation model the encoder consumes. Every rule mirrors the
// generic path exactly (the semantic authority): NULL propagation, int64
// overflow promotion to REAL, the float64 round-trip conversions of * / %,
// division/modulo by zero → NULL, the target column's affinity applied
// after evaluation, and the all-RHS-see-the-ORIGINAL-row ordering. Any
// statement whose shape or runtime operand types the lane cannot mirror
// exactly falls back before the first slot is written — the fallback
// re-runs the generic applyUpdateAssignments over the row map and produces
// byte-identical results.

import (
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// setLaneOp is one compiled SET assignment: store (the evaluated result)
// into values[target]. The operand encoding covers the lane's shapes —
// a column slot (ref >= 0, reading the PRE-assignment slot value; ref ==
// laneRefRowID reads the row's rowid), a parsed numeric literal (lit), or
// a binary arithmetic op over two such operands.
type setLaneOp struct {
	target int // values slot the result stores into (never the IPK alias)
	kind   setLaneKind
	op     byte // '+', '-', '*', '/', '%' (kind == laneArith)
	refA   int  // laneCol/laneArith first operand: slot index or laneRefRowID
	litA   interface{}
	refB   int // laneArith second operand: slot index or laneRefRowID
	litB   interface{}
}

// setLaneKind enumerates the compiled operation shapes.
type setLaneKind uint8

const (
	laneCol   setLaneKind = iota // copy one slot (with rowid-alias substitution)
	laneLit                      // store a literal
	laneArith                    // binary arithmetic over two operands
)

// laneRefRowID marks an operand reading the row's rowid (the unaliased
// rowid/oid/_rowid_ pseudo-column; only reachable when the table declares
// no shadowing column); laneRefLit marks a literal operand (lit holds the
// value — ref must never default-collide with slot 0).
const laneRefRowID = -2
const laneRefLit = -3

// applyTypedPointUpdateSet compiles s.Assignments and, when the whole SET
// clause fits the lane's exact-rewrite shapes, evaluates it over values and
// stores the results. Returns false (slots untouched) for any shape or
// runtime operand type the generic path must own. values arrives holding
// the collected row's original slots; the lane reads operands BEFORE any
// store (the row map's all-RHS-see-the-original-row ordering) and applies
// each target's declared affinity on store (applyUpdateColumnSet parity).
func (e *DMLExecutor) applyTypedPointUpdateSet(s *sql.UpdateStmt, colDefs []sql.ColumnDef, values []interface{}, rowID int64) bool {
	colIndex := e.columnIndexFor(colDefs)
	ops, ok := e.compileTypedPointUpdateSet(s, colIndex, colDefs)
	if !ok {
		return false
	}
	results := e.laneResults[:len(ops)]
	for i := range ops {
		op := &ops[i]
		switch op.kind {
		case laneLit:
			// A lone operand store: column slot (with the IPK rowid-alias
			// substitution), the rowid pseudo-reference, or the literal.
			results[i] = laneOperandValue(values, colDefs, op.refA, op.litA, rowID)
		default: // laneArith
			l := laneOperandValue(values, colDefs, op.refA, op.litA, rowID)
			r := laneOperandValue(values, colDefs, op.refB, op.litB, rowID)
			v, numeric := evalLaneArith(op.op, l, r)
			if !numeric {
				return false // text/blob operand: generic coercion semantics
			}
			results[i] = v
		}
	}
	for i := range ops {
		op := &ops[i]
		if op.target < len(values) {
			v := results[i]
			if op.target < len(colDefs) {
				v = util.ApplyColumnAffinity(v, colDefs[op.target].Type)
			}
			values[op.target] = v
		}
	}
	return true
}

// compileTypedPointUpdateSet walks the assignment list once, resolving every
// SET target and operand through colIndex. ok=false reports a shape the
// generic evaluation owns (any non-literal/ref/arith expression, a rowid or
// IPK target — a re-keying assignment, the row map's error reporting for an
// unresolvable name, table-qualified references).
func (e *DMLExecutor) compileTypedPointUpdateSet(s *sql.UpdateStmt, colIndex map[string]int, colDefs []sql.ColumnDef) ([]setLaneOp, bool) {
	if len(s.Assignments) == 0 || len(s.Assignments) > len(e.laneOps) {
		return nil, false
	}
	ops := e.laneOps[:len(s.Assignments)]
	for i, a := range s.Assignments {
		op := &ops[i]
		op.kind, op.op, op.refA, op.litA, op.refB, op.litB = laneLit, 0, 0, nil, 0, nil
		// The target: an ordinary declared column. IPK aliases re-key the
		// row and rowid pseudo-names move the cell — both the generic
		// applyOneUpdateAssignment's job; an unresolvable name takes the
		// generic path's "no such column" error.
		ti, ok := colIndex[strings.ToLower(a.Column)]
		if !ok || ti < 0 || ti >= len(colDefs) || isIPKRowidAliasCol(colDefs[ti]) ||
			(execquery.IsRowIDName(a.Column) && !hasRowIDColumnName(colDefs)) {
			return nil, false
		}
		op.target = ti
		if !compileLaneOperandInto(a.Value, op, colIndex, colDefs, i, e.ctx.LengthLimit()) {
			return nil, false
		}
	}
	return ops, true
}

// compileLaneOperandInto compiles one SET value expression into op (kind
// laneLit when the whole expression is one operand, laneArith for a binary
// arithmetic node over two operand-shaped children). limit is the
// connection's SQLITE_LIMIT_LENGTH, threaded down to the literal operands.
func compileLaneOperandInto(expr sql.Expr, op *setLaneOp, colIndex map[string]int, colDefs []sql.ColumnDef, slot, limit int) bool {
	switch v := unwrapLaneParens(expr).(type) {
	case *sql.BinaryOp:
		var ob byte
		switch v.Operator {
		case "+":
			ob = '+'
		case "-":
			ob = '-'
		case "*":
			ob = '*'
		case "/":
			ob = '/'
		case "%":
			ob = '%'
		default:
			return false
		}
		if !compileLaneOperandRef(unwrapLaneParens(v.Left), op, colIndex, colDefs, slot, limit) {
			return false
		}
		refA, litA := op.refA, op.litA
		// The second operand compiles into a scratch op so a right-hand
		// literal lands in litB (compileLaneOperandRef writes litA).
		var scratch setLaneOp
		if !compileLaneOperandRef(unwrapLaneParens(v.Right), &scratch, colIndex, colDefs, slot, limit) {
			return false
		}
		op.kind, op.op, op.refA, op.litA, op.refB, op.litB = laneArith, ob, refA, litA, scratch.refA, scratch.litA
		return true
	default:
		// A lone operand-shaped expression: compile as a direct store.
		if !compileLaneOperandRef(unwrapLaneParens(expr), op, colIndex, colDefs, slot, limit) {
			return false
		}
		return true
	}
}

// compileLaneOperandRef compiles one arithmetic operand (a column reference,
// a rowid pseudo-reference, or a literal) into op: refA the slot index
// (laneRefRowID for the pseudo-column), litA the literal's value.
//
// The literal arm covers the three literal kinds vdbe.c materializes
// directly into a register: OP_Integer/OP_Real (NumericLit), OP_String8
// (StringLit) and OP_Blob (BlobLit) — plus OP_Null (NullLit), which stores
// NULL in the target column like any other literal store. Text and blob
// literals are gated by the same SQLITE_LIMIT_LENGTH check
// evalBoundedLiteral (expression.go:62) runs — an over-limit literal bails
// so the generic path raises "string or blob too big" (sqllimits1-5.17.1).
// Every literal's typed Go value is exactly what EvalExpr returns, so the
// lane's affinity application on store (applyUpdateColumnSet parity) sees
// the identical input.
func compileLaneOperandRef(expr sql.Expr, op *setLaneOp, colIndex map[string]int, colDefs []sql.ColumnDef, slot, limit int) bool {
	op.kind, op.refA, op.litA = laneLit, laneRefLit, nil
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return compileLaneColumnRef(v, op, colIndex, colDefs)
	case *sql.NumericLit:
		lit, ok := laneNumericLit(v)
		if !ok {
			return false
		}
		op.litA = lit
		return true
	case *sql.StringLit:
		if len(v.Value) > limit {
			return false // generic path reports "string or blob too big"
		}
		op.litA = v.Value
		return true
	case *sql.BlobLit:
		if len(v.Value) > limit {
			return false // generic path reports "string or blob too big"
		}
		op.litA = v.Value
		return true
	case *sql.NullLit:
		// refA == laneRefLit with a nil literal: laneOperandValue returns
		// nil, and the store writes NULL (ApplyColumnAffinity(nil) == nil).
		return true
	case *sql.UnaryOp:
		return compileLaneNegatedLit(v, op, colIndex, colDefs, slot)
	}
	return false
}

// compileLaneColumnRef resolves one column-reference operand: a declared
// column slot, or the rowid pseudo-reference when nothing shadows the name.
func compileLaneColumnRef(v *sql.ColumnRef, op *setLaneOp, colIndex map[string]int, colDefs []sql.ColumnDef) bool {
	if v.Table != "" {
		return false // qualified reference: the row map's resolution
	}
	ci, ok := colIndex[strings.ToLower(v.Name)]
	if ok && ci >= 0 {
		op.refA = ci
		return true
	}
	// An unqualified rowid/oid/_rowid_ reads the row's rowid — only
	// when no declared column shadows the name (the row map's rule).
	if !hasRowIDColumnName(colDefs) && execquery.IsRowIDName(v.Name) {
		op.refA = laneRefRowID
		return true
	}
	return false // unresolvable name: generic path reports the error
}

// laneNumericLit parses one numeric literal operand: plain decimal integers
// and simple decimals only — hex, sign-folded magnitudes and exponents keep
// the generic evaluator (its evalNumericLit semantics own those forms).
func laneNumericLit(v *sql.NumericLit) (interface{}, bool) {
	if lit := v.Cached(); lit != nil {
		switch lit.(type) {
		case int64, float64:
			return lit, true
		}
		return nil, false
	}
	if i, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
		return i, true
	}
	if f, err := strconv.ParseFloat(v.Value, 64); err == nil && !strings.ContainsAny(v.Value, "xXeEpP") {
		return f, true
	}
	return nil, false
}

// compileLaneNegatedLit folds a negated numeric literal ("-1", the parser's
// separate-sign form) into one literal operand.
func compileLaneNegatedLit(v *sql.UnaryOp, op *setLaneOp, colIndex map[string]int, colDefs []sql.ColumnDef, slot int) bool {
	// Only numeric negation is folded (-1); every other unary shape (NOT,
	// ~, a negated string/blob literal) goes to the generic evaluator.
	if v.Operator != "-" {
		return false
	}
	sub, ok := unwrapLaneParens(v.Operand).(*sql.NumericLit)
	if !ok {
		return false
	}
	lit, ok := laneNumericLit(sub)
	if !ok {
		return false
	}
	switch n := lit.(type) {
	case int64:
		op.litA = -n
	case float64:
		op.litA = -n
	}
	return true
}

// laneOperandValue reads one compiled operand's value: a slot's raw value
// (with the IPK rowid-alias NULL→rowid substitution), the rowid, or the
// literal.
func laneOperandValue(values []interface{}, colDefs []sql.ColumnDef, ref int, lit interface{}, rowID int64) interface{} {
	if ref == laneRefRowID {
		return rowID
	}
	if ref >= 0 {
		return laneSlotValue(values, colDefs, ref, rowID)
	}
	return lit // laneRefLit: the compiled literal
}

// laneSlotValue reads one slot's raw value, substituting the rowid for a
// NULL INTEGER PRIMARY KEY slot (fillPointUpdateRowMap's rule).
func laneSlotValue(values []interface{}, colDefs []sql.ColumnDef, idx int, rowID int64) interface{} {
	v := values[idx]
	if v == nil && idx < len(colDefs) && isIPKRowidAliasCol(colDefs[idx]) {
		return rowID
	}
	return v
}

// evalLaneArith evaluates one arithmetic operator over two already-unwrapped
// operand values. numeric=false reports an operand type the generic
// evaluator's coercion semantics own (text, blob, wrappers) — the caller
// falls back with the slots untouched. The integer rules mirror
// execexpr's addInt64/subInt64 (overflow promotes to REAL) and
// fastIntArith/fastFloatArith (* / / % convert through float64; x/0 and
// x%0 are NULL; % casts the divisor to int64 first, minInt64 % -1 wraps
// to 0); the float rules mirror fastFloatArith (NaN → NULL, REAL result).
// NULL propagates for every operator.
func evalLaneArith(op byte, l, r interface{}) (interface{}, bool) {
	if l == nil || r == nil {
		return nil, true
	}
	switch li := l.(type) {
	case int64:
		switch ri := r.(type) {
		case int64:
			return laneIntArith(op, li, ri), true
		case float64:
			return laneFloatArith(op, float64(li), ri), true
		}
	case float64:
		switch ri := r.(type) {
		case int64:
			return laneFloatArith(op, li, float64(ri)), true
		case float64:
			return laneFloatArith(op, li, ri), true
		}
	}
	return nil, false
}

// laneIntArith is the two-integer arm (the fastIntArith semantics, with
// + and - inlined from addInt64/subInt64: the error result there is
// statically nil for int64 inputs).
func laneIntArith(op byte, l, r int64) interface{} {
	switch op {
	case '+':
		sum := l + r
		if (r > 0 && sum < l) || (r < 0 && sum > l) {
			return float64(l) + float64(r)
		}
		return sum
	case '-':
		diff := l - r
		if (r > 0 && diff > l) || (r < 0 && diff < l) {
			return float64(l) - float64(r)
		}
		return diff
	case '*':
		return int64(float64(l)) * int64(float64(r))
	default: // '/' and '%' (laneIntDivMod)
		return laneIntDivMod(op, l, r)
	}
}

// laneIntDivMod is the integer division/modulo arm: NULL for a zero divisor
// (SQLite), the same float64-converted integer arithmetic (fastIntArith
// parity), and % casting the divisor to int64 first with the minInt64 % -1
// wrap to 0.
func laneIntDivMod(op byte, l, r int64) interface{} {
	if op == '/' {
		if r == 0 {
			return nil
		}
		return int64(float64(l)) / int64(float64(r))
	}
	ri := int64(float64(r))
	if ri == 0 {
		return nil
	}
	if ri == -1 {
		return int64(0) // minInt64 % -1 overflows int64 in Go; SQLite wraps to 0
	}
	return int64(float64(l)) % ri
}

// laneFloatArith is the at-least-one-REAL arm (fastFloatArith's semantics:
// NaN → NULL, x/0 and x%0 NULL, % casts through int64 with the -1 wrap).
func laneFloatArith(op byte, af, bf float64) interface{} {
	switch op {
	case '+':
		return laneNanToNil(af + bf)
	case '-':
		return laneNanToNil(af - bf)
	case '*':
		return laneNanToNil(af * bf)
	case '/':
		if bf == 0 {
			return nil
		}
		return laneNanToNil(af / bf)
	default: // '%'
		bi := int64(bf)
		if bi == 0 {
			return nil
		}
		if bi == -1 {
			return float64(0)
		}
		return float64(int64(af) % bi)
	}
}

// laneNanToNil maps a NaN arithmetic result to SQL NULL (NanToNil parity).
func laneNanToNil(v float64) interface{} {
	if math.IsNaN(v) {
		return nil
	}
	return v
}

// unwrapLaneParens strips parenthesized wrappers (the expression the parser
// hands back for "c = (c + 1)").
func unwrapLaneParens(e sql.Expr) sql.Expr {
	for {
		p, ok := e.(*sql.ParenExpr)
		if !ok {
			return e
		}
		e = p.Expr
	}
}

// hasRowIDColumnName reports whether any declared column is literally named
// rowid/oid/_rowid_ (execquery.RowHasRowIDColumn's fold rule).
func hasRowIDColumnName(colDefs []sql.ColumnDef) bool {
	for i := range colDefs {
		n := colDefs[i].Name
		if strings.EqualFold(n, "rowid") || strings.EqualFold(n, "_rowid_") || strings.EqualFold(n, "oid") {
			return true
		}
	}
	return false
}
