package exec

// RAISE() placement validation: SQLite rejects RAISE() outside a
// trigger-program at prepare time (resolve.c check for trigger-parsed
// statements). Split from engine_core.go for file-size hygiene.

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// errRaiseOutsideTrigger is reported when RAISE() appears outside a trigger
// program (SQLite: "RAISE() may only be used within a trigger-program").
var errRaiseOutsideTrigger = fmt.Errorf("RAISE() may only be used within a trigger-program")

// isRaiseExpr reports whether expr is a RAISE() expression, whether parsed as
// a RaiseExpr node or as a raise() function call.
func isRaiseExpr(expr sql.Expr) bool {
	if _, ok := expr.(*sql.RaiseExpr); ok {
		return true
	}
	if f, ok := expr.(*sql.FuncCall); ok && strings.EqualFold(f.Name, "raise") {
		return true
	}
	return false
}

// checkRaiseInExpr walks an expression tree and rejects RAISE() expressions.
// Subquery/EXISTS selects are delegated to checkSelect so the walk covers
// GROUP BY/HAVING of nested selects.
func checkRaiseInExpr(expr sql.Expr, checkSelect func(*sql.SelectStmt) error) error {
	if expr == nil {
		return nil
	}
	if isRaiseExpr(expr) {
		return errRaiseOutsideTrigger
	}
	switch v := expr.(type) {
	case *sql.Subquery:
		return checkSelect(v.Select)
	case *sql.ExistsExpr:
		return checkSelect(v.Select)
	}
	// One closure per walk (not per node): the recursion re-runs through
	// forEachRaiseChild with the walker itself as the child callback, so the
	// per-statement validation never materializes child slices.
	var rec func(sql.Expr) error
	rec = func(kid sql.Expr) error {
		return checkRaiseInExprCb(kid, checkSelect, rec)
	}
	return checkRaiseInExprCb(expr, checkSelect, rec)
}

// checkRaiseInExprCb is checkRaiseInExpr with the child walk injected (the
// recursion re-enters through next so the walk allocates nothing per node).
func checkRaiseInExprCb(expr sql.Expr, checkSelect func(*sql.SelectStmt) error, next func(sql.Expr) error) error {
	if expr == nil {
		return nil
	}
	if isRaiseExpr(expr) {
		return errRaiseOutsideTrigger
	}
	switch v := expr.(type) {
	case *sql.Subquery:
		return checkSelect(v.Select)
	case *sql.ExistsExpr:
		return checkSelect(v.Select)
	}
	var err error
	forEachRaiseChild(expr, func(kid sql.Expr) {
		if err == nil {
			err = next(kid)
		}
	})
	return err
}

// forEachRaiseChild calls fn for each immediate child expression of an
// expression node in raiseChildExprs' traversal order (empty for leaves) —
// the allocation-free form the per-statement RAISE walks use. It mirrors the
// expression kinds whose subtrees can contain a RAISE() outside a trigger.
func forEachRaiseChild(expr sql.Expr, fn func(sql.Expr)) {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		fn(v.Expr)
	case *sql.BinaryOp:
		fn(v.Left)
		fn(v.Right)
	case *sql.UnaryOp:
		fn(v.Operand)
	case *sql.FuncCall:
		for _, a := range v.Args {
			fn(a)
		}
	case *sql.CastExpr:
		fn(v.Operand)
	case *sql.CaseExpr:
		// The operand, ELSE, then the WHEN/THEN pairs (caseChildExprs order).
		fn(v.Operand)
		fn(v.Else)
		for _, w := range v.Whens {
			fn(w.When)
			fn(w.Then)
		}
	case *sql.Between:
		fn(v.Operand)
		fn(v.Low)
		fn(v.High)
	case *sql.InList:
		fn(v.Operand)
		for _, item := range v.List {
			fn(item)
		}
	case *sql.RowValue:
		for _, item := range v.Values {
			fn(item)
		}
	case *sql.IsNull:
		fn(v.Operand)
	case *sql.IsNotNull:
		fn(v.Operand)
	}
}

// validateNoRaiseOutsideTrigger walks a statement's expression trees and
// rejects RAISE() expressions when not inside a trigger program (SQLite's
// "RAISE() may only be used within a trigger-program"). This is a compile-
// time check: the runtime evaluation in evalRaiseExpr would miss RAISE()
// inside expressions that never execute (e.g. GROUP BY/HAVING over an empty
// table).
func (e *Engine) validateNoRaiseOutsideTrigger(stmt sql.Stmt) error {
	checkSelect := func(s *sql.SelectStmt) error { return e.checkSelectRaise(s) }
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.checkSelectRaise(s)
	case *sql.InsertStmt:
		for _, tuple := range s.Values {
			if err := checkRaiseInExprs(tuple, checkSelect); err != nil {
				return err
			}
		}
		if s.Select != nil {
			return e.checkSelectRaise(s.Select)
		}
	case *sql.UpdateStmt:
		for _, a := range s.Assignments {
			if err := checkRaiseInExpr(a.Value, checkSelect); err != nil {
				return err
			}
		}
		return checkRaiseInExpr(s.Where, checkSelect)
	case *sql.DeleteStmt:
		return checkRaiseInExpr(s.Where, checkSelect)
	}
	return nil
}

// checkRaiseInExprs walks a list of expressions, rejecting RAISE() anywhere.
func checkRaiseInExprs(exprs []sql.Expr, checkSelect func(*sql.SelectStmt) error) error {
	for _, expr := range exprs {
		if err := checkRaiseInExpr(expr, checkSelect); err != nil {
			return err
		}
	}
	return nil
}

// checkSelectRaise walks a SELECT's GROUP BY, HAVING, and compound tails for
// RAISE() expressions. Only GROUP BY and HAVING need the early check: SQLite
// validates ORDER BY column matching first ("1st ORDER BY term does not match
// any column", triggerC-16.1), and SELECT-column/WHERE RAISE() is caught by
// runtime evaluation when rows exist. GROUP BY/HAVING over an empty table
// never evaluates, hiding the error (triggerC-16.2).
func (e *Engine) checkSelectRaise(s *sql.SelectStmt) error {
	if s == nil {
		return nil
	}
	checkSelect := func(sel *sql.SelectStmt) error { return e.checkSelectRaise(sel) }
	for _, g := range s.GroupBy {
		if err := checkRaiseInExpr(g, checkSelect); err != nil {
			return err
		}
	}
	if err := checkRaiseInExpr(s.Having, checkSelect); err != nil {
		return err
	}
	if s.Union != nil {
		return e.checkSelectRaise(s.Union)
	}
	return nil
}
