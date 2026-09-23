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
	for _, kid := range raiseChildExprs(expr) {
		if err := checkRaiseInExpr(kid, checkSelect); err != nil {
			return err
		}
	}
	return nil
}

// raiseChildExprs returns the immediate child expressions of an expression
// node (empty for leaves), mirroring the expression kinds whose subtrees can
// contain a RAISE() outside a trigger.
func raiseChildExprs(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		return []sql.Expr{v.Expr}
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.FuncCall:
		return v.Args
	case *sql.CastExpr:
		return []sql.Expr{v.Operand}
	case *sql.CaseExpr:
		return caseChildExprs(v)
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		kids := []sql.Expr{v.Operand}
		kids = append(kids, v.List...)
		return kids
	case *sql.RowValue:
		return v.Values
	case *sql.IsNull:
		return []sql.Expr{v.Operand}
	case *sql.IsNotNull:
		return []sql.Expr{v.Operand}
	}
	return nil
}

// caseChildExprs returns the operand, WHEN/THEN pairs, and ELSE of a CASE
// expression in traversal order.
func caseChildExprs(v *sql.CaseExpr) []sql.Expr {
	kids := []sql.Expr{v.Operand, v.Else}
	for _, w := range v.Whens {
		kids = append(kids, w.When, w.Then)
	}
	return kids
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
