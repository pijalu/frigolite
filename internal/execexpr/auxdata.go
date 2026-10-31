// This file implements SQLite's function auxiliary data (P-C6): the
// sqlite3_get_auxdata()/sqlite3_set_auxdata() slots a scalar function can
// attach to a call site, with C's exact lifetime semantics.
//
// C reference (3.54):
//   - Storage: Vdbe.pAuxData, a list of AuxData{iAuxOp, iAuxArg, pAux,
//     xDeleteAux} (vdbeInt.h:374). The key is (OP_Function instruction
//     address, argument index); a negative iAuxArg selects a statement-global
//     cache shared by all function sites (vdbeapi.c:1151-1250).
//   - OP_Function (vdbe.c ~8824): after the function returns with the
//     isError soft flag set (a NEW slot was created) or a real error,
//     sqlite3VdbeDeleteAuxData(db, &p->pAuxData, iOp, pOp->p1) releases
//     every slot at this call site whose argument was NOT compile-time
//     constant (the P1 bitmask: vdbeaux.c:3702 — iAuxArg>31 or the mask bit
//     clear). Constant-argument slots therefore persist across rows of one
//     statement execution; non-constant slots are fresh every call.
//   - Statement teardown (closeAllCursors, vdbeaux.c:2852) destroys every
//     slot with its destructor — a re-executed statement starts empty
//     (func.test 13.8.1-3).
//
// The storage lives on the Evaluator (statement-scoped like stmtAux/
// collMemo: the Evaluator is engine-scoped and cleared at the statement
// boundary, so a later statement cannot observe a freed statement's node
// addresses). The site key is the *sql.FuncCall AST node — the per-row
// evaluation of one call site always passes the same node, and two call
// sites in one statement are distinct nodes, matching C's per-instruction
// site identity.
package execexpr

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// auxKey is one auxdata slot: (call site, argument index). A nil site names
// the statement-global cache (C's negative-iArg behavior); a negative arg
// with a nil site is that cache's key, so every function site converges.
type auxKey struct {
	site interface{}
	arg  int
}

// auxEntry is the stored value plus its destructor (C AuxData.pAux /
// xDeleteAux; destructor may be nil).
type auxEntry struct {
	value   interface{}
	destroy func(interface{})
}

// auxStore implements function.AuxStore over the Evaluator's statement-
// scoped slot table (dependency inversion: the function package defines the
// port, this package implements it).
type auxStore struct {
	ev *Evaluator
}

// GetAuxData implements function.AuxStore.
func (s auxStore) GetAuxData(site interface{}, arg int) (interface{}, bool) {
	if arg < 0 {
		site = nil // statement-global cache: site-independent (vdbeapi.c:1157)
	}
	e, ok := s.ev.siteAux[auxKey{site: site, arg: arg}]
	return e.value, ok
}

// SetAuxData implements function.AuxStore. Replacing an existing slot runs
// its destructor immediately (vdbeapi.c:1223); creating one reports true —
// C's `if (pCtx->isError==0) pCtx->isError = -1` soft flag (vdbeapi.c:1213).
func (s auxStore) SetAuxData(site interface{}, arg int, v interface{}, destroy func(interface{})) bool {
	if arg < 0 {
		site = nil
	}
	if s.ev.siteAux == nil {
		s.ev.siteAux = make(map[auxKey]auxEntry)
	}
	k := auxKey{site: site, arg: arg}
	if prev, ok := s.ev.siteAux[k]; ok {
		if prev.destroy != nil {
			prev.destroy(prev.value)
		}
		s.ev.siteAux[k] = auxEntry{value: v, destroy: destroy}
		return false
	}
	s.ev.siteAux[k] = auxEntry{value: v, destroy: destroy}
	return true
}

// releaseNonConstSiteAux is sqlite3VdbeDeleteAuxData(db, &p->pAuxData, iOp,
// pOp->p1) for one call site: it destroys every slot of this site whose
// argument was NOT compile-time constant. mask[i] mirrors the P1 bit for
// argument i; slots at arguments >= 32 never survive (P1 is a 32-bit mask,
// vdbeaux.c:3702), and the statement-global cache (arg < 0) is untouched by
// a site-scoped release.
func (ev *Evaluator) releaseNonConstSiteAux(site interface{}, mask []bool) {
	for k, e := range ev.siteAux {
		if k.site != site || k.arg < 0 {
			continue
		}
		if k.arg < 32 && k.arg < len(mask) && mask[k.arg] {
			continue // constant argument within the 32-bit P1 mask: persists (func.test 13.3/13.4)
		}
		if e.destroy != nil {
			e.destroy(e.value)
		}
		delete(ev.siteAux, k)
	}
}

// destroyStatementAux runs every stored destructor and drops the table —
// C's closeAllCursors auxdata teardown at statement halt/reset
// (vdbeaux.c:2852). Called from ResetStatementAux at the statement boundary.
func (ev *Evaluator) destroyStatementAux() {
	for _, e := range ev.siteAux {
		if e.destroy != nil {
			e.destroy(e.value)
		}
	}
	ev.siteAux = nil
}

// funcArgConstMask computes the P1 constant-argument bitmask of a call site
// as a per-argument bool slice: mask[i] reports whether argument i is
// compile-time constant (expr.c TK_FUNCTION: bit i set iff
// sqlite3ExprIsConstant(arg_i) with the eCode=1 walker). The mask is
// computed per invocation for context-aware calls only, so no statement-
// scoped memoization is needed.
func (ev *Evaluator) funcArgConstMask(f *sql.FuncCall) []bool {
	mask := make([]bool, len(f.Args))
	for i, arg := range f.Args {
		mask[i] = ev.exprIsConstant(arg)
	}
	return mask
}

// exprIsConstant is SQLite's sqlite3ExprIsConstant (eCode=1 walker,
// expr.c:2534-2634) over the frigolite AST: literals and bound parameters
// are constant; column references (including DQS double-quoted identifiers,
// "considered a variable") and subqueries are not; operators are constant
// iff all their operands are; a nested function call is constant only when
// the resolved function is deterministic (SQLITE_FUNC_CONSTANT,
// exprNodeIsConstantFunction) with constant arguments.
func (ev *Evaluator) exprIsConstant(e sql.Expr) bool {
	switch v := e.(type) {
	case *sql.NumericLit, *sql.StringLit, *sql.BlobLit, *sql.NullLit:
		return true
	case *sql.ParameterExpr:
		// TK_VARIABLE: bound parameters count as constant for the P1 mask
		// (exprNodeIsConstant default branch; func.test 13.7/13.8.4-6).
		return true
	case *sql.ColumnRef:
		return false
	case *sql.Subquery, *sql.ExistsExpr:
		// sqlite3SelectWalkFail aborts the walk on any embedded SELECT.
		return false
	case *sql.RaiseExpr:
		// TK_RAISE sets eCode=0 (expr.c:2616).
		return false
	case *sql.FuncCall:
		return ev.constFuncCall(v)
	}
	// Operator shapes: constant iff every operand is (the walker's default
	// branch keeps walking the children). An unrecognized shape cannot
	// happen today (every sql.Expr implementation is handled) and is
	// conservatively non-constant.
	children := constOperandExprs(e)
	if children == nil {
		return false
	}
	for _, child := range children {
		if !ev.exprIsConstant(child) {
			return false
		}
	}
	return true
}

// constFuncCall is the TK_FUNCTION case of the constant walk: constant only
// when the resolved FuncDef carries SQLITE_FUNC_CONSTANT
// (exprNodeIsConstantFunction) with constant arguments. The registry's
// Deterministic flag is that flag for application functions; the builtin
// registrations do not carry it today, so a nested builtin call is
// conservatively non-constant (no suite-visible divergence: the auxdata
// fixture UDFs take literal/parameter/column arguments).
func (ev *Evaluator) constFuncCall(v *sql.FuncCall) bool {
	fn, ok := ev.ctx.Functions().Find(v.Name)
	if !ok || !fn.Deterministic {
		return false
	}
	for _, arg := range v.Args {
		if !ev.exprIsConstant(arg) {
			return false
		}
	}
	return true
}

// constOperandExprs lists the child expressions a non-leaf operator node
// walks in the constant check, or nil for an unrecognized shape. Grouped by
// operand arity to keep each helper within the complexity gate.
func constOperandExprs(e sql.Expr) []sql.Expr {
	if l, r, ok := constBinaryOperands(e); ok {
		return []sql.Expr{l, r}
	}
	if o, ok := constUnaryOperand(e); ok {
		return []sql.Expr{o}
	}
	return constVariadicOperands(e)
}

// constBinaryOperands extracts the two operands of two-sided operator
// shapes.
func constBinaryOperands(e sql.Expr) (l, r sql.Expr, ok bool) {
	switch v := e.(type) {
	case *sql.BinaryOp:
		return v.Left, v.Right, true
	case *sql.IsDistinctFrom:
		return v.Left, v.Right, true
	case *sql.IsNotDistinctFrom:
		return v.Left, v.Right, true
	}
	return nil, nil, false
}

// constUnaryOperand extracts the single operand of one-sided operator
// shapes.
func constUnaryOperand(e sql.Expr) (sql.Expr, bool) {
	switch v := e.(type) {
	case *sql.ParenExpr:
		return v.Expr, true
	case *sql.UnaryOp:
		return v.Operand, true
	case *sql.IsNull:
		return v.Operand, true
	case *sql.IsNotNull:
		return v.Operand, true
	case *sql.IsTrue:
		return v.Operand, true
	case *sql.IsFalse:
		return v.Operand, true
	case *sql.CastExpr:
		return v.Operand, true
	}
	return nil, false
}

// constVariadicOperands extracts the operand lists of n-ary shapes: BETWEEN
// bounds, CASE branches, IN-list items, and row values.
func constVariadicOperands(e sql.Expr) []sql.Expr {
	switch v := e.(type) {
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		// A list of constants is constant; a subquery item fails through
		// the Subquery case (TK_IN with a SELECT: SelectWalkFail).
		items := make([]sql.Expr, 0, 1+len(v.List))
		items = append(items, v.Operand)
		return append(items, v.List...)
	case *sql.CaseExpr:
		items := make([]sql.Expr, 0, 1+2*len(v.Whens)+1)
		if v.Operand != nil {
			items = append(items, v.Operand)
		}
		for _, w := range v.Whens {
			items = append(items, w.When, w.Then)
		}
		if v.Else != nil {
			items = append(items, v.Else)
		}
		return items
	case *sql.RowValue:
		return v.Values
	}
	return nil
}
