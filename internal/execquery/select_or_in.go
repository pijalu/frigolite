// Package exec implements query execution.
//
// This file extends the OR-index SELECT path (execdml's planOrIndexScan /
// execSelectWithOrPlan, delegated through the SelectContext) to WHERE clauses
// whose OR arms carry IN value lists — where8-1.12.1's
// `a IN(1, 2, 3) OR a = 5`. SQLite plans such a term as one MULTI-INDEX OR
// branch whose index loop is driven by an ephemeral table of the IN values
// (wherecode.c's WHERE_IN_ABLE loop): per value one index seek, one deferred
// row fetch and one index Next, plus the value loop's own successful Nexts
// (n−1). The OR planner only understands plain equality/range arms, so the
// expansion below rewrites each IN arm into its per-value equality arms for
// PLANNING — the executed statement keeps its original WHERE (the IN arm
// filters identically to the equality arms it expands to) — and the search
// counter is completed with the value loop's OP_Next delta.

package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// orInExpansion is the planning rewrite of one OR clause's IN arms.
type orInExpansion struct {
	// where is the rewritten OR clause: every expanded IN arm became its
	// per-value equality arms (the arm's other content, and every non-IN
	// arm, is carried over unchanged).
	where sql.Expr
	// delta is the search-count contribution of the value loops: +1 per
	// successful OP_Next over each IN arm's ephemeral value table — n−1 for
	// an n-value list (the first value is reached by OP_Rewind, which the
	// counter does not track).
	delta int64
}

// expandOrInArms rewrites the statement WHERE's single top-level OR term's
// IN arms into equality arms, or returns nil when the WHERE carries no
// expandable IN arm. The gates mirror planOrIndexScan's own pre-analysis: the
// top-level AND chain must hold exactly one OR term (collectOrConstraintTerms
// declines anything else, so expanding other shapes could never plan), and
// only a flat OR arm that is a plain `col IN (constants)` expands.
func (e *SelectEngine) expandOrInArms(s *sql.SelectStmt, tableEntry *schema.Entry) *orInExpansion {
	if s == nil || s.Where == nil || tableEntry == nil {
		return nil
	}
	orSeen := false
	var conjuncts []sql.Expr
	for _, conj := range splitAnd(s.Where) {
		conj = unwrapParenExpr(conj)
		bin, ok := conj.(*sql.BinaryOp)
		if !ok || !strings.EqualFold(bin.Operator, "OR") {
			continue
		}
		if orSeen {
			return nil // planOrIndexScan serves exactly one OR term
		}
		orSeen = true
		conjuncts = append(conjuncts, conj)
	}
	if !orSeen {
		return nil
	}
	expanded := e.expandOrInChain(conjuncts[0], tableEntry, s.From.As)
	if expanded == nil {
		return nil
	}
	return expanded
}

// expandOrInChain rewrites one OR chain's arms, rebuilding the chain when at
// least one arm expanded.
func (e *SelectEngine) expandOrInChain(chain sql.Expr, tableEntry *schema.Entry, alias string) *orInExpansion {
	arms := splitOrChain(chain)
	out := make([]sql.Expr, 0, len(arms))
	exp := &orInExpansion{}
	changed := false
	for _, arm := range arms {
		values, in := e.orInArmValues(arm, tableEntry, alias)
		if in == nil {
			out = append(out, arm)
			continue
		}
		for _, val := range values {
			out = append(out, &sql.BinaryOp{Left: in.Operand, Operator: "=", Right: val})
		}
		exp.delta += int64(len(values) - 1)
		changed = true
	}
	if !changed {
		return nil
	}
	exp.where = rebuildOrChain(out)
	return exp
}

// splitOrChain flattens an OR tree into its arms (parenthesis-transparent,
// left-to-right).
func splitOrChain(expr sql.Expr) []sql.Expr {
	expr = unwrapParenExpr(expr)
	if bin, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "OR") {
		return append(splitOrChain(bin.Left), splitOrChain(bin.Right)...)
	}
	return []sql.Expr{expr}
}

// rebuildOrChain rebuilds a right-leaning OR tree from flattened arms (the
// shape the parser produces).
func rebuildOrChain(arms []sql.Expr) sql.Expr {
	result := arms[len(arms)-1]
	for i := len(arms) - 2; i >= 0; i-- {
		result = &sql.BinaryOp{Left: arms[i], Right: result, Operator: "OR"}
	}
	return result
}

// orInArmValues resolves one OR arm's IN value list: the constant operands
// when the arm is a non-negated `col IN (list)` over a plain column of the
// scanned table with every element a literal or bound parameter. Anything
// else (NOT IN, an IN subquery, a qualified foreign column, a non-constant
// element, a unary-plus-wrapped operand — where8-1.12.2's `+a IN(...)`) keeps
// the arm unexpanded.
func (e *SelectEngine) orInArmValues(arm sql.Expr, tableEntry *schema.Entry, alias string) ([]sql.Expr, *sql.InList) {
	in, ok := unwrapParenExpr(arm).(*sql.InList)
	if !ok || in.Negated || len(in.List) == 0 {
		return nil, nil
	}
	if _, ok := e.indexSeekColumnRef(in.Operand, tableEntry, alias); !ok {
		return nil, nil
	}
	for _, elem := range in.List {
		elem = unwrapParenExpr(elem)
		if !(isDMLSearchLiteral(elem) || isParameterExpr(elem)) {
			return nil, nil
		}
	}
	return in.List, in
}
