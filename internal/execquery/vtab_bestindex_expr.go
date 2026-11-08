package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// Expression-level helpers for the xBestIndex planning port
// (vtab_bestindex.go): conjunct value resolution and the small AST
// classifiers the WHERE/ORDER BY enumeration needs.

// VtabConjunctValue returns the value-side expression of constraint c's
// conjunct (the RHS for col <op> expr, LHS for expr <op> col; for the
// LIMIT/OFFSET aux constraints it returns the LIMIT/OFFSET expression from
// opts). ok is false for constraints whose value side is not a plain
// expression: IS NULL / IS NOT NULL carry no value, and an IN constraint's
// per-element argv is driven by its IsIn flag rather than a single
// expression.
func VtabConjunctValue(opts *VtabScanOptions, conjunct sql.Expr, op vtab.IndexConstraintOp) (sql.Expr, bool) {
	if conjunct == nil {
		return vtabLimitConjunctValue(opts, op)
	}
	switch e := conjunct.(type) {
	case *sql.BinaryOp:
		return vtabBinaryConjunctValue(e)
	case *sql.Between:
		// One BETWEEN conjunct expands to Ge (low bound) + Le (high bound)
		// sharing the conjunct, so the operator selects the bound.
		return vtabBetweenConjunctValue(e, op)
	case *sql.FuncCall:
		return vtabFuncConjunctValue(e, op)
	}
	return nil, false
}

// vtabLimitConjunctValue resolves the LIMIT/OFFSET auxiliary constraints'
// value from the statement's clauses (BuildVtabIndexInfo stores the
// expression itself, but the documented nil conjunct entry is honored too).
func vtabLimitConjunctValue(opts *VtabScanOptions, op vtab.IndexConstraintOp) (sql.Expr, bool) {
	switch op {
	case vtab.IndexConstraintLimit:
		return opts.Limit, opts.Limit != nil
	case vtab.IndexConstraintOffset:
		return opts.Offset, opts.Offset != nil
	}
	return nil, false
}

// vtabBinaryConjunctValue returns a binary conjunct's non-column operand:
// the constraint was anchored on the first operand resolving to a vtab
// column (with COLLATE/wrapper-skipping), so the other side is the value.
func vtabBinaryConjunctValue(bo *sql.BinaryOp) (sql.Expr, bool) {
	if isVtabColumnish(bo.Left) {
		return bo.Right, true
	}
	if isVtabColumnish(bo.Right) {
		return bo.Left, true
	}
	return nil, false
}

// vtabBetweenConjunctValue selects the BETWEEN bound matching the expanded
// constraint operator.
func vtabBetweenConjunctValue(e *sql.Between, op vtab.IndexConstraintOp) (sql.Expr, bool) {
	switch op {
	case vtab.IndexConstraintGe:
		return e.Low, true
	case vtab.IndexConstraintLe:
		return e.High, true
	}
	return nil, false
}

// vtabFuncConjunctValue returns the function-call conjunct's value operand:
// the first argument for the builtin match/glob/like/regexp form (whose
// column is the SECOND argument) and the second argument for overloaded
// functions (whose column is the FIRST argument).
func vtabFuncConjunctValue(fn *sql.FuncCall, op vtab.IndexConstraintOp) (sql.Expr, bool) {
	if len(fn.Args) != 2 {
		return nil, false
	}
	if _, isAux := vtabAuxOpForName(fn.Name); isAux {
		return fn.Args[0], true
	}
	if op >= vtab.IndexConstraintFunction {
		return fn.Args[1], true
	}
	return nil, false
}

// vtabColumnIndex maps lowercased declared column names to their declared
// order index.
type vtabColumnIndex map[string]int

// newVtabColumnIndex builds the declared-column lookup (first occurrence of
// a duplicate name wins).
func newVtabColumnIndex(names []string) vtabColumnIndex {
	m := make(vtabColumnIndex, len(names))
	for i, n := range names {
		key := strings.ToLower(n)
		if _, dup := m[key]; !dup {
			m[key] = i
		}
	}
	return m
}

// resolve reports whether expr is a column reference of this vtab
// (sqlite3ExprSkipCollateAndLikely + ExprIsVtab parity): the returned index
// is -1 for the rowid. A qualified reference qualifies only when its Table
// matches tableName (an empty tableName accepts unqualified references
// only — a qualifier naming any other table is an outer reference).
func (m vtabColumnIndex) resolve(expr sql.Expr, tableName string) (int, bool) {
	cr, ok := skipVtabCollate(expr).(*sql.ColumnRef)
	if !ok {
		return 0, false
	}
	if cr.Table != "" && (tableName == "" || !strings.EqualFold(cr.Table, tableName)) {
		return 0, false
	}
	name := strings.ToLower(cr.Name)
	if isRowIDName(name) {
		return -1, true
	}
	idx, ok := m[name]
	return idx, ok
}

// skipVtabCollate unwraps parentheses, COLLATE wrappers, and the
// likely()/unlikely()/likelihood() no-op calls around an expression
// (sqlite3ExprSkipCollateAndLikely parity) so a constraint's column operand
// is recognized through them.
func skipVtabCollate(expr sql.Expr) sql.Expr {
	for {
		switch e := expr.(type) {
		case *sql.ParenExpr:
			expr = e.Expr
		case *sql.BinaryOp:
			if !strings.EqualFold(e.Operator, "COLLATE") {
				return expr
			}
			expr = e.Left
		case *sql.FuncCall:
			if len(e.Args) == 1 && vtabIsLikelyFunc(e.Name) {
				expr = e.Args[0]
				continue
			}
			return expr
		default:
			return expr
		}
	}
}

// vtabIsLikelyFunc reports whether name is one of SQLite's likelihood
// no-op wrappers that sqlite3ExprSkipCollateAndLikely steps through.
func vtabIsLikelyFunc(name string) bool {
	switch strings.ToLower(name) {
	case "likely", "unlikely", "likelihood":
		return true
	}
	return false
}

// isVtabColumnish reports whether expr is (possibly COLLATE-wrapped) a bare
// column reference — the anchor side of an offered constraint.
func isVtabColumnish(expr sql.Expr) bool {
	_, ok := skipVtabCollate(expr).(*sql.ColumnRef)
	return ok
}

// vtabWhereConjuncts flattens the top-level AND conjuncts of a WHERE clause
// (sqlite3WhereSplit parity: it walks the AND tree after skipping COLLATE /
// likely wrappers and parentheses, since the parser flattens AND groups).
func vtabWhereConjuncts(where sql.Expr) []sql.Expr {
	if where == nil {
		return nil
	}
	if bin, ok := skipVtabCollate(where).(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "AND") {
		return append(vtabWhereConjuncts(bin.Left), vtabWhereConjuncts(bin.Right)...)
	}
	return []sql.Expr{where}
}

// vtabUsable reports whether a constraint's value side can be evaluated
// before the scan starts: none of its expressions contains a column
// reference at all (constant, parameter, or uncorrelated subquery —
// WalkExprFull does not descend into Subquery). Values referencing another
// table's column in a join are still enumerated, with Usable=false (where.c
// prereqRight handling); the consumer keeps them out of xFilter argv and
// they remain in the residual WHERE.
func vtabUsable(exprs ...sql.Expr) bool {
	for _, expr := range exprs {
		if expr == nil {
			continue
		}
		usable := true
		WalkExprFull(expr, func(e2 sql.Expr) {
			if _, ok := e2.(*sql.ColumnRef); ok {
				usable = false
			}
		})
		if !usable {
			return false
		}
	}
	return true
}

// isVtabConstExpr reports whether expr is constant for ORDER BY purposes
// (sqlite3ExprIsConstant parity): it contains no column reference.
func isVtabConstExpr(expr sql.Expr) bool {
	constant := true
	WalkExprFull(expr, func(e2 sql.Expr) {
		if _, ok := e2.(*sql.ColumnRef); ok {
			constant = false
		}
	})
	return constant
}

// vtabConstIntOk reports whether expr is a constant integer usable as a
// LIMIT/OFFSET constraint value (whereAddLimitExpr's sqlite3ExprIsInteger
// check — an integer literal, its unary negation, or a parenthesized form;
// non-constant values stay Usable=false).
func vtabConstIntOk(expr sql.Expr) bool {
	_, ok := vtabConstInt(expr)
	return ok
}

// vtabConstInt evaluates expr as a constant integer syntactically.
func vtabConstInt(expr sql.Expr) (int64, bool) {
	switch e := expr.(type) {
	case *sql.NumericLit:
		if n, err := strconv.ParseInt(e.Value, 10, 64); err == nil {
			return n, true
		}
	case *sql.UnaryOp:
		if e.Operator == "-" {
			if n, ok := vtabConstInt(e.Operand); ok {
				return -n, true
			}
		}
	case *sql.ParenExpr:
		return vtabConstInt(e.Expr)
	}
	return 0, false
}

// vtabAuxOpForName maps a MATCH/GLOB/LIKE/REGEXP operator or function name
// (case-insensitive) to its auxiliary constraint code (whereexpr.c aOp[]).
func vtabAuxOpForName(name string) (vtab.IndexConstraintOp, bool) {
	switch strings.ToLower(name) {
	case "match":
		return vtab.IndexConstraintMatch, true
	case "glob":
		return vtab.IndexConstraintGlob, true
	case "like":
		return vtab.IndexConstraintLike, true
	case "regexp":
		return vtab.IndexConstraintRegexp, true
	}
	return 0, false
}

// vtabRangeOp maps a range operator string to its constraint code
// (WO_/SQLITE_INDEX_CONSTRAINT_ identity — where.c allocateIndexInfo
// assigns the codes directly).
func vtabRangeOp(op string) vtab.IndexConstraintOp {
	switch op {
	case "<":
		return vtab.IndexConstraintLt
	case "<=":
		return vtab.IndexConstraintLe
	case ">":
		return vtab.IndexConstraintGt
	case ">=":
		return vtab.IndexConstraintGe
	}
	return 0
}

// vtabMirrorRangeOp mirrors a range operator for a commuted term
// (5 < col ⇔ col > 5).
func vtabMirrorRangeOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

// vtabOrderColumn classifies one ORDER BY term expression: a plain column
// reference (collateName "") or a COLLATE-wrapped one (the wrapper's
// collation name; parse rule 187 builds COLLATE as a BinaryOp over a
// StringLit). ok is false for any other shape.
func vtabOrderColumn(expr sql.Expr) (col sql.Expr, collateName string, ok bool) {
	if bo, isBin := expr.(*sql.BinaryOp); isBin && strings.EqualFold(bo.Operator, "COLLATE") {
		lit, isLit := bo.Right.(*sql.StringLit)
		if !isLit {
			return nil, "", false
		}
		return bo.Left, lit.Value, true
	}
	return expr, "", true
}

// vtabBigNullOrdering reports a NULLS-ordering override that makes NULL
// sort larger than every value (KEYINFO_ORDER_BIGNULL), which virtual tables
// cannot honor.
func vtabBigNullOrdering(term sql.OrderByTerm) bool {
	if term.NullsLast && !term.Desc {
		return true
	}
	return term.NullsFirst && term.Desc
}
