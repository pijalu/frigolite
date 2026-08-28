package execexpr

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// exprCollation computes the compile-time collation of an expression node,
// mirroring SQLite's sqlite3ExprCollSeq. It returns the collation name, or ""
// for BINARY/no collation. The second return value reports whether the
// collation is "explicit" (comes from a COLLATE operator, or propagates up
// from an explicit COLLATE through a function call / CASE / ||), which makes
// it win over a column collation on the other side of a comparison.
//
// SQLite propagation rules (expr.c sqlite3ExprCollSeq):
//   - COLLATE operator: its collation (explicit).
//   - Function call: the first argument with a known collation.
//   - CASE: the first THEN branch with a known collation, else ELSE.
//   - || (concat): the right operand's collation if explicit, else the left's.
//   - Column reference: the column's declared collation (not explicit).
func exprCollation(e sql.Expr) (string, bool) {
	switch v := e.(type) {
	case *sql.BinaryOp:
		return binaryOpCollation(v)
	case *sql.FuncCall:
		return firstNonEmptyCollation(v.Args)
	case *sql.CaseExpr:
		return caseCollation(v)
	case *sql.UnaryOp:
		// UPLUS propagates the operand's collation (SQLite TK_UPLUS).
		return exprCollation(v.Operand)
	}
	// CastExpr drops the operand's collation; ColumnRef collation is resolved
	// at runtime via CollatedValue markers; literals have none.
	return "", false
}

// ExprCollation computes the compile-time collation of an expression node.
// Exported for the execution engine's set-operation collation analysis.
func ExprCollation(e sql.Expr) (string, bool) {
	return exprCollation(e)
}

// binaryOpCollation resolves the collation of a binary operator. COLLATE yields
// its explicit collation; "||" (concat) takes the right operand's collation
// when explicit, else the left's; all other operators have no collation.
func binaryOpCollation(v *sql.BinaryOp) (string, bool) {
	if strings.EqualFold(v.Operator, "COLLATE") {
		return collateOpCollation(v.Right)
	}
	if strings.EqualFold(v.Operator, "||") {
		return concatCollation(v.Left, v.Right)
	}
	return "", false
}

// collateOpCollation returns the explicit collation named by a COLLATE
// operator's right operand (a string literal), uppercased.
func collateOpCollation(right sql.Expr) (string, bool) {
	lit, ok := right.(*sql.StringLit)
	if !ok {
		return "", false
	}
	return strings.ToUpper(lit.Value), true
}

// concatCollation resolves the collation of a "||" concatenation: the right
// operand's collation when explicit, otherwise the left operand's (if any).
func concatCollation(left, right sql.Expr) (string, bool) {
	if rc, rx := exprCollation(right); rx {
		return rc, true
	}
	if lc, lx := exprCollation(left); lc != "" {
		return lc, lx
	}
	return "", false
}

// caseCollation resolves the collation of a CASE expression: the first THEN
// branch with a known collation, else the ELSE branch's collation.
func caseCollation(v *sql.CaseExpr) (string, bool) {
	for _, w := range v.Whens {
		if c, x := exprCollation(w.Then); c != "" {
			return c, x
		}
	}
	if v.Else != nil {
		return exprCollation(v.Else)
	}
	return "", false
}

// firstNonEmptyCollation returns the first non-empty collation among exprs
// (used for function arguments, which inherit the first collation-bearing arg).
func firstNonEmptyCollation(exprs []sql.Expr) (string, bool) {
	for _, ex := range exprs {
		if c, x := exprCollation(ex); c != "" {
			return c, x
		}
	}
	return "", false
}
