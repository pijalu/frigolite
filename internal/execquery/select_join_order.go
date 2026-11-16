// Package exec implements query execution.
//
// This file owns the JOIN statement's ORDER BY delivery verdict (where.c's
// wherePathSatisfiesOrderBy over the whole loop path): the loops are walked
// outer→inner, each consuming the ORDER BY terms its executor shape can
// deliver — the equality-bound pre-pass, the driving index's column walk
// with the trailing rowid, and the order-distinct mark-off that retires the
// remaining terms of a well-ordered loop. When every term is consumed the
// join runs with no sorter, and the rows must EMERGE in the delivered
// order: the base loop already does (the recorded pick), and each real-table
// inner operand's materialized rows are permuted into its pick's stored
// index order (C's covering walk / ORDER-BY-serving walk).
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// joinDeliveryLoop is one join loop as the delivery analysis sees it —
// where.c's WhereLoop for one path slot, carrying the executor's actual
// scan shape for the operand.
type joinDeliveryLoop struct {
	operand  int    // the FROM operand this loop drives (0 = the base)
	token    string // the operand's reference token (alias, else name)
	real     string // the real table name for schema lookups ("" when not one)
	kind     int    // loopNone / loopIPK / loopIndex (the executor's loop)
	idx      string // the driving index token (loopIndex)
	seek     bool   // the loop positions (a SEARCH) rather than walking whole
	nEq      int    // the equality-bound prefix length (loopIndex seek)
	oneRow   bool   // at most one row (an INTEGER PRIMARY KEY equality seek)
	orLoop   bool   // an OR-branch row source (WHERE_MULTI_OR never delivers)
	withoutRowid bool // the operand is a WITHOUT ROWID table (no rowid tail)
	backward bool   // OUT: the walk direction this loop's delivery used
}

// joinInnerPick is the delivery pick for one inner join operand: the index
// whose stored key order the operand's rows must emerge in. The executor
// reproduces C's index walk by permuting the materialized operand rows into
// the index's stored rowid order.
type joinInnerPick struct {
	idxName  string
	backward bool
}

// joinDeliveryPlan is the per-statement verdict: whether the join's loops
// deliver the ORDER BY (no sorter runs) and the per-operand picks that make
// the rows emerge in the delivered order.
type joinDeliveryPlan struct {
	delivered bool
	picks     []*joinInnerPick // per join operand (s.Joins order); nil = table order
}

// joinDeliveryOperand describes one FROM operand for the delivery analysis.
type joinDeliveryOperand struct {
	tokens     []string        // reference tokens (alias first, then name)
	real       string          // real table name ("" when the operand is not a real table)
	names      map[string]bool // column names (lower-cased) for unqualified references
	notIndexed bool            // NOT INDEXED: every index plan forbidden
}

// joinOrderTerm is one ORDER BY term resolved for the join delivery walk:
// the single-table orderTerm plus the operand identity and the expression's
// operand usage (the mark-off's mask inputs).
type joinOrderTerm struct {
	orderTerm        // col, desc, collate, nullsFirst/Last, sat
	operand  int            // the operand a bare-column term belongs to
	usage    []bool         // operand usage of the term's expression
	isConst  bool           // the term's expression is constant (no operands, no subquery)
	ref      *sql.ColumnRef // non-nil for bare-column terms
}

// joinPoolEntry is one AND conjunct of the delivery pool (the statement
// WHERE plus every join's ON) with the operands its references resolve to.
type joinPoolEntry struct {
	expr sql.Expr
	refs []bool // operand index usage of the whole conjunct
}

// buildJoinDeliveryPlan computes the join statement's ORDER BY delivery
// verdict and the inner picks. nil = no claim (the comparator sort stays
// responsible — the pre-P2 behavior).
func (e *SelectEngine) buildJoinDeliveryPlan(s *sql.SelectStmt, baseDefs []sql.ColumnDef) *joinDeliveryPlan {
	if len(s.Joins) == 0 || len(s.OrderBy) == 0 || s.Union != nil ||
		len(s.GroupBy) > 0 || s.Having != nil || s.Distinct ||
		s.From.Subquery != nil || s.From.Name == "" || e.ctx.ReverseUnordered() {
		return nil
	}
	// All-inner plain joins only: outer-join NULL padding and USING/NATURAL
	// merges are outside the walk (the sorter owns those statements). A
	// derived/group operand stays in the loop list — C stops the walk at
	// obDone, so a non-deliverable inner loop only voids the claim when the
	// walk actually reaches it.
	for i := range s.Joins {
		join := &s.Joins[i]
		jt := strings.ToUpper(join.JoinType)
		if strings.Contains(jt, "LEFT") || strings.Contains(jt, "RIGHT") || strings.Contains(jt, "FULL") {
			return nil
		}
		if len(join.Using) > 0 || isNaturalJoinType(join.JoinType) {
			return nil
		}
	}
	ops := e.joinDeliveryOperands(s, baseDefs)
	if ops == nil {
		return nil
	}
	pool := e.joinDeliveryPool(s, ops)
	if pool == nil {
		return nil
	}
	terms := e.joinDeliveryTerms(s, s.OrderBy, ops)
	if terms == nil {
		return nil
	}
	loops := make([]joinDeliveryLoop, 0, len(s.Joins)+1)
	// Loop 0: the recorded base pick (what the executor actually ran); a
	// statement that never reached joinBaseScanRows (empty-base
	// short-circuit shapes) records nothing — the plain table scan shape.
	if e.joinBaseLoop != nil {
		loops = append(loops, *e.joinBaseLoop)
	} else {
		loops = append(loops, joinDeliveryLoop{kind: loopNone, withoutRowid: e.operandIsWithoutRowid(s.From.Name)})
	}
	loops[0].operand = 0
	loops[0].token = strings.ToLower(ops[0].tokens[0])
	if loops[0].real == "" {
		loops[0].real = s.From.Name
	}
	// Inner loops: a real-table operand's delivery pick (the ORDER-BY-
	// serving walk, else the cost covering walk, else the table walk);
	// anything else cannot deliver (its rows have no defined b-tree order).
	picks := make([]*joinInnerPick, len(s.Joins))
	for i := range s.Joins {
		op := &ops[i+1]
		loop := joinDeliveryLoop{operand: i + 1, token: strings.ToLower(op.tokens[0]), real: op.real}
		if op.real == "" {
			// A derived/view/vtab operand's rows have no b-tree order the
			// walk can name: C's loop for it carries no index (u.btree.
			// pIndex == 0) — the analysis gives up when it reaches it.
			loop.orLoop = true
			loops = append(loops, loop)
			continue
		}
		loop.withoutRowid = e.operandIsWithoutRowid(op.real)
		if e.joinOrArmConjunctForClaim(s, i, ops) != nil {
			// The operand's loop is an OR-branch row source (C's OR loop
			// carries no index): the walk gives up when it reaches it.
			loop.orLoop = true
			loops = append(loops, loop)
			continue
		}
		loop.idx, loop.kind = e.joinInnerOrderPick(s, i+1, op, terms, pool, ops)
		if loop.kind == loopIndex {
			picks[i] = &joinInnerPick{idxName: loop.idx}
		}
		loops = append(loops, loop)
	}
	delivered := e.joinWalkOrder(loops, terms, pool, ops)
	if delivered && loops[0].backward {
		// The executor's base loop only runs forward (the seek/walk/scan
		// paths have no reverse machinery): a backward-delivering base
		// keeps the sorter (where.c would flip the scan — carded).
		delivered = false
	}
	if !delivered {
		return nil
	}
	// Delivered: fix each inner pick's direction from its loop's walk (the
	// walk that consumed the terms is the order the rows emerge in; a walk
	// that consumed nothing is C's forward covering walk).
	for i := range loops[1:] {
		if picks[i] != nil {
			picks[i].backward = loops[i+1].backward
		}
	}
	return &joinDeliveryPlan{delivered: true, picks: picks}
}

// joinDeliveryOperands builds the FROM operand list: the base plus one per
// join clause, each with its reference tokens and column-name set. nil when
// an operand cannot be resolved (the claim declines).
func (e *SelectEngine) joinDeliveryOperands(s *sql.SelectStmt, baseDefs []sql.ColumnDef) []joinDeliveryOperand {
	ops := make([]joinDeliveryOperand, 0, len(s.Joins)+1)
	base := joinDeliveryOperand{real: s.From.Name}
	if s.From.As != "" {
		base.tokens = append(base.tokens, strings.ToLower(s.From.As))
	}
	base.tokens = append(base.tokens, strings.ToLower(schemaStrippedName(s.From.Name)))
	base.names = joinOperandColumnNames(baseDefs)
	ops = append(ops, base)
	for i := range s.Joins {
		ref := &s.Joins[i].Table
		op := joinDeliveryOperand{}
		if ref.As != "" {
			op.tokens = append(op.tokens, strings.ToLower(ref.As))
		}
		op.tokens = append(op.tokens, strings.ToLower(schemaStrippedName(ref.Name)))
		if entry, _, err := e.ctx.FindTable(ref.Name); err == nil && entry.RootPage > 0 {
			op.real = entry.Name
			op.names = joinOperandColumnNames(e.ctx.ParseColumnDefs(entry.Name, entry.SQL))
			op.notIndexed = ref.NotIndexed
		}
		if op.names == nil {
			// A non-real operand (view/subquery/vtab): its column set is
			// unknown here — unqualified references cannot resolve past it.
			op.names = map[string]bool{}
		}
		ops = append(ops, op)
	}
	return ops
}

// schemaStrippedName strips a schema qualifier ("aux.t1" → "t1").
func schemaStrippedName(name string) string {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[dot+1:]
	}
	return name
}

// joinOperandColumnNames returns the lower-cased column names (plus the
// rowid pseudo-columns) an operand's unqualified references may name.
func joinOperandColumnNames(colDefs []sql.ColumnDef) map[string]bool {
	names := make(map[string]bool, len(colDefs)+3)
	for i := range colDefs {
		names[strings.ToLower(colDefs[i].Name)] = true
	}
	names["rowid"] = true
	names["_rowid_"] = true
	names["oid"] = true
	return names
}

// operandIsWithoutRowid reports whether the named table is a WITHOUT ROWID
// table (its b-tree order is the PRIMARY KEY order and has no rowid tail).
func (e *SelectEngine) operandIsWithoutRowid(name string) bool {
	entry, _, err := e.ctx.FindTable(name)
	if err != nil || entry == nil {
		return false
	}
	return e.ctx.TableIsWithoutRowidEntry(entry)
}

// joinDeliveryPool builds the constraint conjunct pool: the statement
// WHERE's top-level AND conjuncts plus every join's ON conjuncts, each with
// its operand usage (the per-loop usability input).
func (e *SelectEngine) joinDeliveryPool(s *sql.SelectStmt, ops []joinDeliveryOperand) []joinPoolEntry {
	pool := make([]joinPoolEntry, 0, 8)
	appendExpr := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		refs, ok := e.joinExprOperandUsage(expr, ops)
		if !ok {
			pool = append(pool, joinPoolEntry{expr: expr, refs: nil})
			return
		}
		pool = append(pool, joinPoolEntry{expr: expr, refs: refs})
	}
	for _, conj := range splitAnd(s.Where) {
		appendExpr(conj)
	}
	for i := range s.Joins {
		for _, conj := range splitAnd(s.Joins[i].On) {
			appendExpr(conj)
		}
	}
	return pool
}

// joinExprOperandUsage resolves every column reference in the expression to
// its FROM operand, reporting the per-operand usage. ok=false when a
// reference resolves to no operand (an unknown table — the claim declines
// for terms built from such expressions).
func (e *SelectEngine) joinExprOperandUsage(expr sql.Expr, ops []joinDeliveryOperand) ([]bool, bool) {
	usage := make([]bool, len(ops))
	ok := true
	WalkExprFull(expr, func(n sql.Expr) {
		if !ok {
			return
		}
		ref, isRef := n.(*sql.ColumnRef)
		if !isRef || ref.Name == "*" {
			return
		}
		idx, found := e.joinRefOperand(ref, ops)
		if !found {
			ok = false
			return
		}
		usage[idx] = true
	})
	if !ok {
		return nil, false
	}
	return usage, true
}

// joinRefOperand resolves one column reference to its FROM operand index:
// a qualified reference by its table token, an unqualified one by the first
// operand whose column set names it (FROM-order resolution).
func (e *SelectEngine) joinRefOperand(ref *sql.ColumnRef, ops []joinDeliveryOperand) (int, bool) {
	if ref.Table != "" {
		tok := strings.ToLower(schemaStrippedName(ref.Table))
		for i := range ops {
			for _, t := range ops[i].tokens {
				if tok == t {
					return i, true
				}
			}
		}
		return 0, false
	}
	name := strings.ToLower(ref.Name)
	for i := range ops {
		if ops[i].names[name] {
			return i, true
		}
	}
	return 0, false
}

// joinResolvedOrderExpr resolves one ORDER BY term's expression for the
// delivery analysis: a numeric ordinal term becomes its result column's
// expression (resolve.c's ordinal resolution; out-of-range ordinals error
// in validation before the claim runs).
func joinResolvedOrderExpr(s *sql.SelectStmt, expr sql.Expr) sql.Expr {
	pos, ok := ordinalPosition(expr, len(s.Columns))
	if !ok {
		return expr
	}
	return s.Columns[pos-1].Expr
}

// joinDeliveryTerms resolves the ORDER BY terms: bare-column terms carry
// their operand, expression terms carry their usage mask. nil when any term
// cannot be resolved (the claim declines).
func (e *SelectEngine) joinDeliveryTerms(s *sql.SelectStmt, orderBy []sql.OrderByTerm, ops []joinDeliveryOperand) []joinOrderTerm {
	terms := make([]joinOrderTerm, 0, len(orderBy))
	for _, ob := range orderBy {
		// C's resolve.c rewrites ORDER BY ordinals to their result column's
		// expression before the planner runs; the delivery analysis must
		// see the resolved term (ORDER BY 1 over SELECT t2.a is the t2.a
		// column, not a constant).
		expr := normalizeOrderByExpr(joinResolvedOrderExpr(s, ob.Expr))
		tr := joinOrderTerm{
			orderTerm: orderTerm{
				desc:       ob.Desc,
				collate:    orderByTermExplicitCollation(ob.Expr),
				nullsFirst: ob.NullsFirst,
				nullsLast:  ob.NullsLast,
			},
		}
		if ref, isRef := expr.(*sql.ColumnRef); isRef && ref.Name != "*" {
			idx, found := e.joinRefOperand(ref, ops)
			if !found {
				return nil
			}
			tr.ref = ref
			tr.col = ref.Name
			tr.operand = idx
			// C's mark-off mask input: a column term's usage is its own
			// cursor's bit (sqlite3WhereExprUsage of a TK_COLUMN).
			tr.usage = make([]bool, len(ops))
			tr.usage[idx] = true
			terms = append(terms, tr)
			continue
		}
		usage, ok := e.joinExprOperandUsage(expr, ops)
		if !ok {
			return nil
		}
		tr.usage = usage
		tr.isConst = !boolsAny(usage) && !exprHasSubquery(expr)
		terms = append(terms, tr)
	}
	return terms
}

// boolsAny reports whether any element is set.
func boolsAny(bs []bool) bool {
	for _, b := range bs {
		if b {
			return true
		}
	}
	return false
}

// joinInnerOrderPick picks the inner operand's delivery index: the
// ORDER-BY-serving index walk over the operand's own ORDER BY terms
// (where.c's ORDER-BY-delivering loop), else the cost covering walk (an
// unconstrained operand's cheapest loop — C prices the table walk above a
// covering one), else no pick (the table walk: rowid order). Returns
// (index token, loop kind).
func (e *SelectEngine) joinInnerOrderPick(stmt *sql.SelectStmt, operand int, op *joinDeliveryOperand, terms []joinOrderTerm, pool []joinPoolEntry, ops []joinDeliveryOperand) (string, int) {
	// The ORDER-BY-serving walk: an index whose column walk consumes every
	// ORDER BY term of this operand (the prefix + order-distinct mark-off
	// rule — where.c's wherePathSatisfiesOrderBy for this loop; the
	// expression terms ride the mark-off). Candidates run in schema order.
	// NOT INDEXED forbids every index plan for the operand.
	if !op.notIndexed {
		if tok, backward, ok := e.joinServingIndexCandidate(operand, op, terms, pool, ops); ok && !backward {
			return tok, loopIndex
		}
	}
	// The cost covering walk: the statement's used columns on this operand
	// ride the index's key columns plus the trailing rowid (C's colUsed
	// covering rule; a WITHOUT ROWID operand has no rowid tail and no walk
	// pick — its table b-tree already is the PK order).
	if op.real == "" || e.operandIsWithoutRowid(op.real) {
		return "", loopNone
	}
	used := e.joinOperandUsedColumns(stmt, op)
	if idx := e.joinCostCoveringIndex(op.real, used); idx != "" {
		return idx, loopIndex
	}
	return "", loopNone
}

// joinServingIndexCandidate tests the operand's indexes (schema order) with
// the single-loop delivery walk and returns the first whose walk consumes
// every ORDER BY term of the operand, with the walk's direction. The terms
// are tested on a copy (the candidate walk must not leak sat marks into the
// claim walk).
func (e *SelectEngine) joinServingIndexCandidate(operand int, op *joinDeliveryOperand, terms []joinOrderTerm, pool []joinPoolEntry, ops []joinDeliveryOperand) (string, bool, bool) {
	if op.real == "" {
		return "", false, false
	}
	// A WITHOUT ROWID operand keeps its table walk: its rows are already in
	// PK order, and the secondary-index delivery walk would permute rows
	// into the index's order — whose trailing locator is the PK tail (S33),
	// not a rowid sequence the permutation can walk back into table order
	// (every other WR index-scan pick site is excluded the same way).
	if e.operandIsWithoutRowid(op.real) {
		return "", false, false
	}
	indexes, err := e.ctx.Schema().FindIndexesForTable(op.real)
	if err != nil {
		return "", false, false
	}
	for _, entry := range indexes {
		if len(e.indexColumns(entry.Name)) == 0 || indexHasPartialPredicate(entry) {
			continue
		}
		testTerms := make([]joinOrderTerm, len(terms))
		copy(testTerms, terms)
		loop := joinDeliveryLoop{operand: operand, token: strings.ToLower(op.tokens[0]),
			real: op.real, kind: loopIndex, idx: entry.Name,
			withoutRowid: e.operandIsWithoutRowid(op.real)}
		// The candidate test runs the loop's own walk over the term list
		// with nothing positioned (the pre-pass consumes the eq-bound terms
		// whose other side is a literal or an already-positioned reference).
		st := &joinWalkState{obDone: len(testTerms), isOrderDistinct: true,
			orderDistinctMask: make([]bool, len(ops)), ready: make([]bool, len(ops))}
		e.joinEqPrePass(&loop, testTerms, pool, st, ops)
		e.joinWalkLoopColumns(&loop, testTerms, st)
		if st.isOrderDistinct {
			// The order-distinct mark-off: the loop's remaining own terms
			// add no ordering constraint.
			st.orderDistinctMask[operand] = true
			for k := range testTerms {
				if !testTerms[k].sat && testTerms[k].operand == operand &&
					joinUsageSubset(testTerms[k].usage, st.orderDistinctMask) {
					testTerms[k].sat = true
				}
			}
		}
		if joinTermsOfOperandSatisfied(testTerms, operand) {
			return entry.Name, loop.backward, true
		}
	}
	return "", false, false
}

// joinTermsOfOperandSatisfied reports whether every term belonging to the
// operand is consumed (the mark-off included).
func joinTermsOfOperandSatisfied(terms []joinOrderTerm, operand int) bool {
	for i := range terms {
		if terms[i].operand == operand && !terms[i].sat {
			return false
		}
	}
	return true
}

// joinTokensContain reports whether the token list names tok.
func joinTokensContain(tokens []string, tok string) bool {
	for _, t := range tokens {
		if t == tok {
			return true
		}
	}
	return false
}

// joinOperandUsedColumns collects the column names (lower-cased) the
// statement reads from the operand: the output columns, the WHERE, every
// join's ON, and the ORDER BY (C's colUsed bitmask — every reference the
// loop must be able to supply without the table).
func (e *SelectEngine) joinOperandUsedColumns(s *sql.SelectStmt, op *joinDeliveryOperand) map[string]bool {
	used := make(map[string]bool, 8)
	collect := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		WalkExprFull(expr, func(n sql.Expr) {
			ref, isRef := n.(*sql.ColumnRef)
			if !isRef || ref.Name == "*" {
				return
			}
			if ref.Table != "" {
				tok := strings.ToLower(schemaStrippedName(ref.Table))
				if !joinTokensContain(op.tokens, tok) {
					return
				}
			} else if !op.names[strings.ToLower(ref.Name)] {
				return
			}
			used[strings.ToLower(ref.Name)] = true
		})
	}
	for i := range s.Columns {
		collect(s.Columns[i].Expr)
	}
	collect(s.Where)
	for i := range s.Joins {
		collect(s.Joins[i].On)
	}
	for i := range s.OrderBy {
		collect(s.OrderBy[i].Expr)
	}
	return used
}

// indexHasPartialPredicate reports whether the index is PARTIAL (its
// CREATE INDEX carries a WHERE clause): an unconstrained walk over it
// misses rows outside the predicate — no walk pick may use one.
func indexHasPartialPredicate(entry *schema.Entry) bool {
	return indexWhereRe.MatchString(entry.SQL)
}

// joinCostCoveringIndex returns the first index (schema order) whose key
// columns plus the trailing rowid cover every used column of the operand.
func (e *SelectEngine) joinCostCoveringIndex(realTable string, used map[string]bool) string {
	indexes, err := e.ctx.Schema().FindIndexesForTable(realTable)
	if err != nil {
		return ""
	}
	ipk := e.ipkAliasColumn(realTable)
	for _, entry := range indexes {
		if indexHasPartialPredicate(entry) {
			continue
		}
		cols := e.indexColumns(entry.Name)
		if len(cols) == 0 {
			continue
		}
		covered := true
		for col := range used {
			if isRowIDName(col) || (ipk != "" && strings.EqualFold(col, ipk)) {
				continue
			}
			found := false
			for _, c := range cols {
				if strings.EqualFold(c, col) {
					found = true
					break
				}
			}
			if !found {
				covered = false
				break
			}
		}
		if covered {
			return entry.Name
		}
	}
	return ""
}

// joinWalkState carries the delivery walk's cross-loop state (where.c's
// obSat / obDone / orderDistinctMask / isOrderDistinct).
type joinWalkState struct {
	obSat             int
	obDone            int
	isOrderDistinct   bool
	orderDistinctMask []bool
	ready             []bool
}

// joinLoopWalk carries one loop's column-walk state (C's rev/revSet/
// distinctColumns, reset per loop).
type joinLoopWalk struct {
	orderDistinct   bool
	distinctColumns bool
	backward        bool
	revSet          bool
}

// joinWalkOrder walks the loops outer→inner and reports whether every ORDER
// BY term is consumed (where.c returns nOrderBy only at obSat==obDone).
func (e *SelectEngine) joinWalkOrder(loops []joinDeliveryLoop, terms []joinOrderTerm, pool []joinPoolEntry, ops []joinDeliveryOperand) bool {
	st := &joinWalkState{
		obDone:            len(terms),
		isOrderDistinct:   true,
		orderDistinctMask: make([]bool, len(ops)),
		ready:             make([]bool, len(ops)),
	}
	for i := range loops {
		if !st.isOrderDistinct || st.obSat >= st.obDone {
			break // C's loop condition: the walk stops at obDone / a void
		}
		loop := &loops[i]
		if i > 0 {
			// ready accumulates the loops BEFORE this one (C: ready |=
			// pLoop->maskSelf at the head of every iteration after the first).
			st.ready[loops[i-1].operand] = true
		}
		if loop.orLoop {
			// where.c's OR loop carries no index: the analysis gives up.
			return false
		}
		e.joinEqPrePass(loop, terms, pool, st, ops)
		if !loop.oneRow {
			e.joinWalkLoopColumns(loop, terms, st)
		}
		if st.isOrderDistinct {
			// Mark off any remaining ORDER BY term whose operand usage the
			// well-ordered loops cover (C 5333-5347); constants always go.
			st.orderDistinctMask[loop.operand] = true
			for k := range terms {
				if terms[k].sat {
					continue
				}
				if terms[k].isConst || joinUsageSubset(terms[k].usage, st.orderDistinctMask) {
					terms[k].sat = true
					st.obSat++
				}
			}
		}
	}
	return st.obSat == st.obDone
}

// joinUsageSubset reports whether the term's operand usage is contained in
// the accumulated order-distinct mask.
func joinUsageSubset(usage, mask []bool) bool {
	for i := range usage {
		if usage[i] && (i >= len(mask) || !mask[i]) {
			return false
		}
	}
	return true
}

// joinEqPrePass marks the ORDER BY terms the current loop's level fixes by
// equality (where.c 5116-5150: X IS NULL / X=? whose other side references
// only already-positioned loops, collation-agreeing for value equalities).
func (e *SelectEngine) joinEqPrePass(loop *joinDeliveryLoop, terms []joinOrderTerm, pool []joinPoolEntry, st *joinWalkState, ops []joinDeliveryOperand) {
	for k := range terms {
		tr := &terms[k]
		if tr.sat || tr.ref == nil {
			continue // bare-column terms only (C's TK_COLUMN check)
		}
		if tr.operand != loop.operand {
			continue
		}
		for p := range pool {
			if pool[p].refs == nil {
				continue // an unresolvable conjunct is never usable
			}
			if e.joinPoolBinds(pool[p].expr, loop, tr, st, ops) {
				tr.sat = true
				st.obSat++
				break
			}
		}
	}
}

// joinPoolBinds reports whether one pool conjunct is an equality that fixes
// the term's column on the current loop, with the other side's references
// already positioned (C's prereqRight ⊆ ready).
func (e *SelectEngine) joinPoolBinds(expr sql.Expr, loop *joinDeliveryLoop, tr *joinOrderTerm, st *joinWalkState, ops []joinDeliveryOperand) bool {
	switch v := expr.(type) {
	case *sql.IsNull:
		ref, ok := v.Operand.(*sql.ColumnRef)
		return ok && e.joinRefMatchesLoop(ref, loop, tr)
	case *sql.BinaryOp:
		switch v.Operator {
		case "=", "==":
		case "IS":
			if _, isNull := v.Right.(*sql.NullLit); isNull {
				// `c IS NULL` parses as IS over a NULL literal: the ISNULL
				// shape (no collation applies to a NULL comparison).
				ref, ok := v.Left.(*sql.ColumnRef)
				return ok && e.joinRefMatchesLoop(ref, loop, tr)
			}
		default:
			return false
		}
		// C's exprAnalyze normalization: the written-left column is the
		// term's LHS; a non-column left swaps the column side in.
		if ref, isRef := v.Left.(*sql.ColumnRef); isRef {
			if !e.joinRefMatchesLoop(ref, loop, tr) {
				return false
			}
			return e.joinEqSideUsable(v.Right, st.ready, ops) && e.joinEqCollationsAgree(v, tr, loop)
		}
		ref, isRef := v.Right.(*sql.ColumnRef)
		if !isRef || !e.joinRefMatchesLoop(ref, loop, tr) {
			return false
		}
		return e.joinEqSideUsable(v.Left, st.ready, ops) && e.joinEqCollationsAgree(v, tr, loop)
	}
	return false
}

// joinRefMatchesLoop reports whether the reference names the ORDER BY term's
// column on the current loop's operand.
func (e *SelectEngine) joinRefMatchesLoop(ref *sql.ColumnRef, loop *joinDeliveryLoop, tr *joinOrderTerm) bool {
	if !strings.EqualFold(ref.Name, tr.col) {
		return false
	}
	if ref.Table != "" {
		tok := strings.ToLower(schemaStrippedName(ref.Table))
		return tok == loop.token
	}
	// Unqualified: the term's operand was resolved at term build; an
	// unqualified reference matches when it belongs to this loop's operand
	// (ambiguous references error out before the claim runs).
	return tr.operand == loop.operand
}

// joinEqSideUsable reports whether the equality's other side references
// only already-positioned operands (no subqueries — their binding is the
// seek planner's business, not the pre-pass's).
func (e *SelectEngine) joinEqSideUsable(side sql.Expr, ready []bool, ops []joinDeliveryOperand) bool {
	usable := true
	WalkExprFull(side, func(n sql.Expr) {
		if !usable {
			return
		}
		if _, isSub := n.(*sql.Subquery); isSub {
			usable = false
			return
		}
		if isParameterExpr(n) {
			return
		}
		ref, isRef := n.(*sql.ColumnRef)
		if !isRef {
			return
		}
		idx, found := e.joinRefOperand(ref, ops)
		if !found || idx >= len(ready) || !ready[idx] {
			usable = false
		}
	})
	return usable
}

// joinEqCollationsAgree reports whether the equality conjunct's collation
// (an explicit COLLATE, else the column side's declared collation) equals
// the ORDER BY term's effective collation (where.c 5139-5148).
func (e *SelectEngine) joinEqCollationsAgree(v *sql.BinaryOp, tr *joinOrderTerm, loop *joinDeliveryLoop) bool {
	whereColl := collateWrapperName(v.Left)
	if whereColl == "" {
		whereColl = collateWrapperName(v.Right)
	}
	if whereColl == "" {
		if ref, isRef := v.Left.(*sql.ColumnRef); isRef {
			whereColl = e.declaredColumnCollation(loop.real, ref.Name)
		}
	}
	termColl := tr.collate
	if termColl == "" {
		termColl = e.declaredColumnCollation(loop.real, tr.col)
	}
	return strings.EqualFold(whereColl, termColl)
}

// joinWalkLoopColumns runs one loop's index-column walk (C 5187-5330): the
// equality-bound prefix skipped, one direction shared by the loop's
// matches, the trailing rowid of a rowid-table index included, and the
// order-distinct tracking (unique init, nullable void, rowid restore).
func (e *SelectEngine) joinWalkLoopColumns(loop *joinDeliveryLoop, terms []joinOrderTerm, st *joinWalkState) {
	var idxCols []string
	var idxDescs []bool
	hasTail := false
	switch loop.kind {
	case loopIndex:
		cols := e.indexColumns(loop.idx)
		descs, known := e.indexColumnDescFlags(loop.real, loop.idx)
		if !known || len(descs) < len(cols) {
			st.isOrderDistinct = false
			return
		}
		// C's index-loop init: isOrderDistinct = IsUniqueIndex && !skipscan.
		st.isOrderDistinct = e.indexIsUnique(loop.idx)
		idxCols, idxDescs = cols, descs
		hasTail = !loop.withoutRowid
	case loopNone:
		if loop.withoutRowid {
			// The WITHOUT ROWID table scan is the PRIMARY KEY b-tree walk.
			idxCols = e.withoutRowidPKCols(loop.real)
			idxDescs = make([]bool, len(idxCols))
		} else {
			// A rowid table's walk: the rowid tail only.
			hasTail = true
		}
	default:
		hasTail = !loop.withoutRowid
	}
	w := &joinLoopWalk{orderDistinct: st.isOrderDistinct}
	for j := 0; j <= len(idxCols); j++ {
		if j == len(idxCols) && !hasTail {
			break // a WITHOUT ROWID index has no trailing rowid
		}
		if j < loop.nEq {
			continue // the equality-bound prefix: the pre-pass consumed it
		}
		if !e.joinWalkColumnStep(loop, w, st, idxCols, idxDescs, j, terms) {
			break
		}
	}
	if w.distinctColumns {
		// A rowid match makes the loop's rows pairwise distinct (C's
		// distinctColumns restore — even for a voided non-unique loop).
		st.isOrderDistinct = true
	} else {
		st.isOrderDistinct = w.orderDistinct
	}
	loop.backward = w.backward
}

// joinWalkColumnStep processes one walk position: the nullable-unconstrained
// void, the first-unsatisfied-term match, and the rowid distinctness. Returns
// false when the walk must stop (C's no-match break).
func (e *SelectEngine) joinWalkColumnStep(loop *joinDeliveryLoop, w *joinLoopWalk, st *joinWalkState, idxCols []string, idxDescs []bool, j int, terms []joinOrderTerm) bool {
	isRowidCol := j == len(idxCols)
	col, colDesc := walkColumn(idxCols, idxDescs, j)
	if w.orderDistinct && !isRowidCol && !e.columnNotNull(loop.real, col) {
		// An unconstrained nullable column can hold duplicate NULLs
		// (where.c tag-20210426-1) — the loop is not order-distinct.
		w.orderDistinct = false
	}
	tr := firstUnsatisfiedJoinTerm(terms)
	rev, matched := false, false
	if tr != nil && tr.operand == loop.operand {
		rev, matched = e.matchWalkColumn(&walkCtx{
			t:        queryTable{display: loop.real, real: loop.real},
			token:    loop.idx,
			ipkAlias: e.ipkAliasColumn(loop.real),
		}, col, colDesc, isRowidCol, &tr.orderTerm)
	}
	if !matched {
		// A break at or before the last key column also voids distinctness.
		if j == 0 || j < len(idxCols) {
			w.orderDistinct = false
		}
		return false
	}
	if !w.revSet {
		w.backward, w.revSet = rev, true
	}
	tr.sat = true
	st.obSat++
	if isRowidCol {
		w.distinctColumns = true
	}
	return true
}

// firstUnsatisfiedJoinTerm returns the first unconsumed term in ORDER BY
// order (nil when all are consumed).
func firstUnsatisfiedJoinTerm(terms []joinOrderTerm) *joinOrderTerm {
	for i := range terms {
		if !terms[i].sat {
			return &terms[i]
		}
	}
	return nil
}

// permuteJoinRightToIndexOrder permutes the materialized operand rows into
// the pick index's stored key order (C's index-walk emission: the b-tree is
// walked for the rowid sequence, reversed for a backward walk).
func (e *SelectEngine) permuteJoinRightToIndexOrder(maps []RowMap, realTable string, pick *joinInnerPick) {
	if len(maps) <= 1 {
		return
	}
	rowids, ok := e.indexStoredRowidOrder(realTable, pick.idxName)
	if !ok || len(rowids) == 0 {
		return
	}
	if pick.backward {
		reverseRowids(rowids)
	}
	positions := scanRowidPositions(maps)
	if len(positions) == 0 {
		return
	}
	perm := indexOrderPermutation(rowids, positions, len(maps))
	sortedMaps := make([]RowMap, len(maps))
	for i, from := range perm {
		sortedMaps[i] = maps[from]
	}
	copy(maps, sortedMaps)
}
