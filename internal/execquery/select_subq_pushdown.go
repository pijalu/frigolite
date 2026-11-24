package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Predicate push-down optimization (select.c pushDownWhereTerms,
// tag-select-0420): outer WHERE terms that constrain only a FROM-clause
// subquery's result columns are copied into the subquery's members' WHERE
// clauses (HAVING for aggregate members), each result-column reference
// substituted by the member's own select-list expression — so a compound
// member's planner sees the constraint at xBestIndex time
// (bestindexE-2.2.2: the UNION arms' Delivery/ReturnDelivery scans must see
// the pushed `ID = 1`).
//
// The outer WHERE keeps the terms (C copies them; the outer re-checks), so
// row results are unchanged — the pushdown only widens what the member's
// own planner can observe.

// pushDownWhereTermsIntoSub returns the FROM-clause subquery to execute with
// the outer statement's pushable WHERE conjuncts appended to each compound
// member. termRef is the FROM operand the subquery backs (its name/alias are
// the only qualifiers a pushed term may use); otherRefs are the outer
// statement's remaining operands (their columns shadow unqualified
// references — C's sqlite3ExprIsSingleTableConstraint). sub is returned
// unmodified when no conjunct qualifies (C's nChng == 0 case).
func (e *SelectEngine) pushDownWhereTermsIntoSub(termRef sql.TableRef, otherRefs []sql.TableRef, s *sql.SelectStmt, sub *sql.SelectStmt) *sql.SelectStmt {
	if s.Where == nil || sub == nil || !e.pushDownGuardsAllow(termRef, s, sub) {
		return sub
	}
	members := compoundMemberChain(sub)
	// The head member's result column names are the subquery's output names
	// (C's findLeftmostExprlist for name resolution; compounds share the
	// head's names).
	outputs := e.compoundResultColumnNames(members[0])
	pushed := pushableConjuncts(e, termRef, otherRefs, s.Where, outputs)
	if len(pushed) == 0 {
		return sub
	}
	appendPushedTerms(e, members, termRef, outputs, pushed)
	return sub
}

// pushDownGuardsAllow mirrors pushDownWhereTerms' early rejections:
//
//	(6b) any member carrying window functions blocks the pushdown (C
//	     returns 0 when a member has pWin);
//	(3)  a compound's trailing LIMIT/OFFSET blocks it (the parser attaches
//	     the compound's tail clauses to the last member — C checks
//	     pSubq->pLimit);
//	(8)  a UNION/INTERSECT/EXCEPT compound needs BINARY collations on every
//	     output column (the pushed filter must compare the same way in every
//	     arm) — C resolves sqlite3ExprCollSeq over each member's output
//	     expressions, so a DECLARED column collation (selectA-9.2's
//	     `a COLLATE nocase`) blocks the pushdown. A UNION ALL chain skips
//	     the check (C only audits when some arm is not TK_ALL);
//	(4/10) a FROM operand on the nullable side of an outer join is not a
//	       single-table constraint (join2-7.0); and
//	(2/7/11) VALUES members and CTE readers keep their own row semantics
//	       (with2-1.15.2's recursive CTE, the VALUES anchor).
func (e *SelectEngine) pushDownGuardsAllow(termRef sql.TableRef, s *sql.SelectStmt, sub *sql.SelectStmt) bool {
	if outerJoinOperandBlocks(s, termRef) {
		return false // restrictions (4), (10): nullable-side operand
	}
	members := compoundMemberChain(sub)
	if !e.pushDownMembersAllow(sub, members) {
		return false
	}
	last := members[len(members)-1]
	if last.Limit != nil || last.Offset != nil {
		return false
	}
	return e.compoundCollationsAllow(sub)
}

// pushDownMembersAllow applies the per-arm restrictions (2), (6b), (7), (11).
func (e *SelectEngine) pushDownMembersAllow(sub *sql.SelectStmt, members []*sql.SelectStmt) bool {
	for _, m := range members {
		if e.selectHasWindowFuncs(m.Columns) {
			return false
		}
		if m.ValuesChain || e.memberReadsCTE(sub, m) {
			return false
		}
	}
	return true
}

// compoundCollationsAllow applies restriction (8): a non-UNION-ALL compound
// needs BINARY output collations in every arm.
func (e *SelectEngine) compoundCollationsAllow(sub *sql.SelectStmt) bool {
	if compoundIsUnionAll(sub) {
		return true
	}
	for _, coll := range e.selectOutputCollations(sub) {
		if coll != "" && !strings.EqualFold(coll, "BINARY") {
			return false
		}
	}
	return true
}

// pushableConjuncts returns the outer WHERE's AND conjuncts that constrain
// only the subquery (C's per-term sqlite3ExprIsSingleTableConstraint walk).
func pushableConjuncts(e *SelectEngine, termRef sql.TableRef, otherRefs []sql.TableRef, where sql.Expr, outputs []string) []sql.Expr {
	var pushed []sql.Expr
	for _, conj := range splitAnd(where) {
		if e.singleTableConstraintOnSub(termRef, otherRefs, conj, outputs) {
			pushed = append(pushed, conj)
		}
	}
	return pushed
}

// appendPushedTerms substitutes and appends each pushed term per compound
// member (C's while(pSubq) loop: each member substitutes against its OWN
// select list). Aggregate members receive the terms in HAVING (WHERE would
// run before the aggregation).
func appendPushedTerms(e *SelectEngine, members []*sql.SelectStmt, termRef sql.TableRef, outputs []string, pushed []sql.Expr) {
	headColls := e.selectOutputCollations(members[0])
	for _, m := range members {
		exprs := e.memberPushDownExprs(m, outputs, headColls)
		for _, term := range pushed {
			newTerm := substituteOutputRefs(term, termRef, outputs, exprs)
			if e.hasAggregate(m) {
				m.Having = andIntoExpr(m.Having, newTerm)
			} else {
				m.Where = andIntoExpr(m.Where, newTerm)
			}
		}
	}
}

// memberPushDownExprs builds one resolved replacement expression per output
// column for a compound member. C pushes against the member's RESOLVED select
// list; our AST still carries bare stars, so expand those through the member's
// FROM source. The substituted expression also inherits the leftmost arm's
// output collation (select.c:3983-3995's pCList handling).
func (e *SelectEngine) memberPushDownExprs(m *sql.SelectStmt, outputs, headColls []string) []sql.Expr {
	memberColls := e.selectOutputCollations(m)
	exprs := make([]sql.Expr, len(outputs))
	for i, name := range outputs {
		expr := e.memberPushDownExpr(m, i, name)
		head := collAt(headColls, i)
		own := collAt(memberColls, i)
		exprs[i] = withPushDownCollation(expr, head, own)
	}
	return exprs
}

func collAt(colls []string, i int) string {
	if i >= 0 && i < len(colls) {
		return colls[i]
	}
	return ""
}

// memberPushDownExpr resolves one output position against a member's select
// list. A bare star is expanded from the member's FROM source; an explicit
// expression is reused as written.
func (e *SelectEngine) memberPushDownExpr(m *sql.SelectStmt, i int, name string) sql.Expr {
	if i < 0 || i >= len(m.Columns) {
		return &sql.ColumnRef{Name: name}
	}
	col := m.Columns[i]
	if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
		source := m.From.Name
		if ref.Table != "" {
			source = ref.Table
		}
		if source != "" && len(m.Columns) == 1 {
			if names, err := e.resolveTableColumnNames(m, source); err == nil && i < len(names) {
				return &sql.ColumnRef{Table: pushDownTableName(source), Name: names[i]}
			}
		}
	}
	return col.Expr
}

// withPushDownCollation wraps a substituted expression with the head arm's
// output collation when the arm's own natural collation differs (select.c's
// substExpr pCList rule). An explicit COLLATE keeps compound output semantics
// identical across arms — collate5-5.1's `b='BbB'` must stay NOCASE when b
// comes from the BINARY `d` of the second UNION ALL arm.
func withPushDownCollation(expr sql.Expr, head, own string) sql.Expr {
	if head == "" || (own != "" && strings.EqualFold(head, own)) {
		return expr
	}
	return &sql.BinaryOp{Left: expr, Operator: "COLLATE", Right: &sql.StringLit{Value: head}}
}

// memberReadsCTE reports whether a compound member's FROM scope reads a CTE.
// C refuses push-down into materialized and recursive CTEs (restrictions 2,
// 7, 11); a `main.t4` qualifier is normalized before the scope lookup.
func (e *SelectEngine) memberReadsCTE(root, m *sql.SelectStmt) bool {
	if m == nil {
		return false
	}
	refs := make([]sql.TableRef, 0, len(m.Joins)+1)
	refs = append(refs, m.From)
	for i := range m.Joins {
		refs = append(refs, m.Joins[i].Table)
	}
	for _, ref := range refs {
		if ref.Subquery != nil {
			if e.subqueryReadsCTE(root, ref.Subquery) {
				return true
			}
			continue
		}
		if ref.Name != "" {
			if _, ok := e.findCTE(root, pushDownTableName(ref.Name)); ok {
				return true
			}
		}
	}
	return false
}

func (e *SelectEngine) subqueryReadsCTE(root, sub *sql.SelectStmt) bool {
	for _, m := range compoundMemberChain(sub) {
		if e.memberReadsCTE(root, m) {
			return true
		}
	}
	return false
}

// outerJoinOperandBlocks implements sqlite3ExprIsSingleTableConstraint's rule
// (3) and pushDownWhereTerms' restriction (10): a term on the nullable side
// of an outer join is not a single-table constraint for push-down (join2-7.0's
// LEFT JOIN derived table must keep the post-join `c IS NULL` filter).
func outerJoinOperandBlocks(s *sql.SelectStmt, termRef sql.TableRef) bool {
	if s == nil {
		return false
	}
	for _, j := range s.Joins {
		if joinTypeHas(j.JoinType, "RIGHT") || joinTypeHas(j.JoinType, "FULL") {
			return true
		}
		if samePushDownOperand(termRef, j.Table) &&
			(joinTypeHas(j.JoinType, "LEFT") || joinTypeHas(j.JoinType, "RIGHT") || joinTypeHas(j.JoinType, "FULL")) {
			return true
		}
	}
	return false
}

func samePushDownOperand(a, b sql.TableRef) bool {
	if a.Subquery != nil && b.Subquery != nil {
		return a.Subquery == b.Subquery
	}
	return a.Subquery == nil && b.Subquery == nil &&
		strings.EqualFold(a.Name, b.Name) && strings.EqualFold(a.As, b.As)
}

func pushDownTableName(name string) string {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[dot+1:]
	}
	return name
}

// compoundMemberChain returns the compound member chain starting at sub (the
// head plus each s.Union link, VALUES chains collapsed like the executor's
// walk).
func compoundMemberChain(sub *sql.SelectStmt) []*sql.SelectStmt {
	members := []*sql.SelectStmt{sub}
	cur := sub
	for cur.Union != nil {
		members = append(members, cur.Union)
		cur = cur.Union
	}
	return members
}

// compoundIsUnionAll reports whether every set-op link is UNION ALL (C's
// notUnionAll audit gate).
func compoundIsUnionAll(sub *sql.SelectStmt) bool {
	cur := sub
	for cur.Union != nil {
		if cur.SetOp != sql.SetUnion || !cur.UnionAll {
			return false
		}
		cur = cur.Union
	}
	return true
}

// singleTableConstraintOnSub reports whether every column reference in term
// resolves to the subquery's output columns (C's
// sqlite3ExprIsSingleTableConstraint: the term constrains only this FROM
// term). An unqualified name that also matches another outer operand's
// column is ambiguous there and rejects the term.
func (e *SelectEngine) singleTableConstraintOnSub(termRef sql.TableRef, otherRefs []sql.TableRef, term sql.Expr, outputs []string) bool {
	ok := true
	WalkExprFull(term, func(node sql.Expr) {
		if !ok {
			return
		}
		switch v := node.(type) {
		case *sql.ColumnRef:
			if !subOutputRefMatch(v, termRef, outputs) {
				ok = false
				return
			}
			if v.Table == "" && e.otherOperandHasColumn(otherRefs, v.Name) {
				ok = false
			}
		case *sql.Subquery, *sql.ExistsExpr:
			// Conservative subset: a nested subquery's scope interactions
			// (correlated outer references) are not analyzed here — the term
			// stays unpushed (C pushes it; we keep the outer-side filter).
			ok = false
		}
	})
	return ok
}

// subOutputRefMatch reports whether ref designates a subquery output column:
// a qualified reference must qualify the subquery's own FROM operand (its
// name or alias); the bare name must be an output column.
func subOutputRefMatch(ref *sql.ColumnRef, termRef sql.TableRef, outputs []string) bool {
	if ref.Table != "" && !strings.EqualFold(ref.Table, termRef.Name) && !strings.EqualFold(ref.Table, termRef.As) {
		return false
	}
	return outputNameIndex(outputs, ref.Name) >= 0
}

// outputNameIndex finds a case-insensitive output column position.
func outputNameIndex(outputs []string, name string) int {
	for i, out := range outputs {
		if out != "" && strings.EqualFold(out, name) {
			return i
		}
	}
	return -1
}

// otherOperandHasColumn reports whether any OTHER outer operand exposes a
// column of that name (the resolution would bind the unqualified reference
// there, making the term not a single-table constraint on the subquery).
func (e *SelectEngine) otherOperandHasColumn(otherRefs []sql.TableRef, name string) bool {
	for _, ref := range otherRefs {
		if ref.Name == "" {
			continue
		}
		if e.ctx.TableHasColumn(ref.Name, name) {
			return true
		}
	}
	return false
}

// substitutedOutputRef renders one output-column reference's replacement: the
// member's resolved output expression (C's substExpr). ok=false keeps the
// reference as written.
func substitutedOutputRef(v *sql.ColumnRef, termRef sql.TableRef, outputs []string, exprs []sql.Expr) (sql.Expr, bool) {
	if v.Table != "" && !strings.EqualFold(v.Table, termRef.Name) && !strings.EqualFold(v.Table, termRef.As) {
		return v, false
	}
	idx := outputNameIndex(outputs, v.Name)
	if idx < 0 || idx >= len(exprs) {
		return v, false
	}
	return exprs[idx], true
}

// substituteOutputRefs rebuilds term with each output-column reference
// replaced by the member's resolved output expression (C's substExpr). Leaf
// nodes that do not match are shared, not copied — expression nodes are
// read-only during execution.
func substituteOutputRefs(term sql.Expr, termRef sql.TableRef, outputs []string, exprs []sql.Expr) sql.Expr {
	subst := func(e sql.Expr) sql.Expr {
		return substituteOutputRefs(e, termRef, outputs, exprs)
	}
	if ref, ok := term.(*sql.ColumnRef); ok {
		if replaced, hit := substitutedOutputRef(ref, termRef, outputs, exprs); hit {
			return replaced
		}
		return ref
	}
	if out, ok := substituteOutputRefsUnary(term, subst); ok {
		return out
	}
	return substituteOutputRefsMulti(term, subst)
}

// substituteOutputRefsUnary handles the single-operand node family; ok=false
// when term is not one of them.
func substituteOutputRefsUnary(term sql.Expr, subst func(sql.Expr) sql.Expr) (sql.Expr, bool) {
	switch v := term.(type) {
	case *sql.ParenExpr:
		return &sql.ParenExpr{Expr: subst(v.Expr)}, true
	case *sql.UnaryOp:
		return &sql.UnaryOp{Operand: subst(v.Operand), Operator: v.Operator}, true
	case *sql.CastExpr:
		return &sql.CastExpr{Operand: subst(v.Operand), AsType: v.AsType}, true
	case *sql.IsNull:
		return &sql.IsNull{Operand: subst(v.Operand)}, true
	case *sql.IsNotNull:
		return &sql.IsNotNull{Operand: subst(v.Operand)}, true
	case *sql.IsTrue:
		return &sql.IsTrue{Operand: subst(v.Operand), Negated: v.Negated}, true
	case *sql.IsFalse:
		return &sql.IsFalse{Operand: subst(v.Operand), Negated: v.Negated}, true
	}
	return term, false
}

// substituteOutputRefsMulti handles the multi-child node family.
func substituteOutputRefsMulti(term sql.Expr, subst func(sql.Expr) sql.Expr) sql.Expr {
	switch v := term.(type) {
	case *sql.BinaryOp:
		return &sql.BinaryOp{Left: subst(v.Left), Right: subst(v.Right), Operator: v.Operator}
	case *sql.IsDistinctFrom:
		return &sql.IsDistinctFrom{Left: subst(v.Left), Right: subst(v.Right)}
	case *sql.IsNotDistinctFrom:
		return &sql.IsNotDistinctFrom{Left: subst(v.Left), Right: subst(v.Right)}
	case *sql.Between:
		return &sql.Between{Operand: subst(v.Operand), Low: subst(v.Low), High: subst(v.High), Negated: v.Negated}
	case *sql.FuncCall:
		args := make([]sql.Expr, len(v.Args))
		for i, a := range v.Args {
			args[i] = subst(a)
		}
		return &sql.FuncCall{Name: v.Name, Args: args, Distinct: v.Distinct, OrderBy: v.OrderBy, Filter: v.Filter, Over: v.Over}
	case *sql.InList:
		items := make([]sql.Expr, len(v.List))
		for i, item := range v.List {
			items[i] = subst(item)
		}
		return &sql.InList{Operand: subst(v.Operand), List: items, Negated: v.Negated}
	case *sql.RowValue:
		values := make([]sql.Expr, len(v.Values))
		for i, item := range v.Values {
			values[i] = subst(item)
		}
		return &sql.RowValue{Values: values}
	case *sql.CaseExpr:
		return substituteCaseExpr(v, subst)
	default:
		return v
	}
}

// substituteCaseExpr rebuilds a CASE with substituted operand/branches.
func substituteCaseExpr(v *sql.CaseExpr, subst func(sql.Expr) sql.Expr) sql.Expr {
	whens := make([]sql.WhenClause, len(v.Whens))
	for i, w := range v.Whens {
		whens[i] = sql.WhenClause{When: subst(w.When), Then: subst(w.Then)}
	}
	ce := &sql.CaseExpr{Whens: whens}
	if v.Operand != nil {
		ce.Operand = subst(v.Operand)
	}
	if v.Else != nil {
		ce.Else = subst(v.Else)
	}
	return ce
}

// andIntoExpr ANDs newTerm into an existing clause (sqlite3ExprAnd parity:
// a nil clause yields the term itself).
func andIntoExpr(clause sql.Expr, newTerm sql.Expr) sql.Expr {
	if clause == nil {
		return newTerm
	}
	return &sql.BinaryOp{Left: clause, Right: newTerm, Operator: "AND"}
}

// joinOperandRefs lists the outer statement's FROM operands, skipping the
// operand the subquery backs (matched by TableRef identity: the base ref
// pointer, or — for a join operand — the shared Subquery pointer, since
// JoinClause values are copied while the Subquery node is not).
func joinOperandRefs(s *sql.SelectStmt, skip *sql.TableRef) []sql.TableRef {
	refs := make([]sql.TableRef, 0, len(s.Joins)+1)
	if &s.From != skip && !(skip.Subquery != nil && s.From.Subquery == skip.Subquery) {
		refs = append(refs, s.From)
	}
	for i := range s.Joins {
		jt := s.Joins[i].Table
		if skip.Subquery != nil && jt.Subquery == skip.Subquery {
			continue
		}
		refs = append(refs, jt)
	}
	return refs
}
