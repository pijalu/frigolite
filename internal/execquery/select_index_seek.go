// Package exec implements query execution.
//
// This file holds the index-driven single-table scan (where.c's "SEARCH <t>
// USING INDEX <i>" loop): the driving index's b-tree is positioned with a
// lower-bound seek over the loop's equality prefix plus its range bound, the
// matching entries iterate forward in the index's STORED key order, and each
// entry's trailing rowid fetches its table row through a b-tree seek — the
// rowid join. Rows therefore arrive in index-key order natively (the order
// SQLite's index loop produces, including rowid-ascending ties), so no
// order emulation runs, and the statement's full WHERE clause is still
// evaluated on every candidate: the seek only narrows the candidate set, so
// a row the bounds might miss would also fail a conjunct and could never
// match the whole AND.
//
// The driving index comes from scanLoopForQuery — the same decision the
// EXPLAIN QUERY PLAN renderer consumes — so the seek and the plan text
// cannot diverge.

package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// indexSeekTerm is one WHERE conjunct resolved for a seek: the index column
// it constrains, the operator class, and the constant operands (val2 carries
// BETWEEN's upper bound).
type indexSeekTerm struct {
	col  string
	op   string // "=", "<", "<=", ">", ">=", "LIKE", "GLOB", "BETWEEN"
	val  interface{}
	val2 interface{}
	// scanLocalOnly marks a LIKE/complete-GLOB term whose matcher calls the
	// regular scan elides (select_like_opt.go — the sqlite3_like_count
	// contract): when such a term drives the seek's own bounds the plan
	// keeps the regular scan instead.
	scanLocalOnly bool
}

// indexSeekBound is one side of the loop's range bound, in VALUE space (the
// column's sort order is applied when the stored-order position is derived).
type indexSeekBound struct {
	op  string // "<", "<=", ">", ">="
	val interface{}
}

// indexSeekPlan is a resolved index-driven single-table scan: the driving
// index, the equality-prefix constants of its leading key columns, and the
// optional range bounds on the first non-equality key column.
type indexSeekPlan struct {
	index string
	eq    []interface{}
	lo    *indexSeekBound
	hi    *indexSeekBound
	// empty marks a plan whose own constants prove that no row matches: a
	// NULL equality or range constant makes the comparison never true.
	empty bool
	// patternDriven marks a plan whose seek constraints were derived from a
	// LIKE or complete-GLOB term. Such statements keep the regular scan,
	// whose scan-local optimizer elides the provably-decided like()/glob()
	// calls (select_like_opt.go — the sqlite3_like_count contract).
	patternDriven bool
}

// orPlanRoutable reports whether the OR-index planner owns the statement:
// the WHERE's top-level OR plans as an OR-index scan (or as per-value
// equality branches after the IN-arm expansion) — the same routing test
// execSelectOrPlan applies before executing. Used by the seek planner to
// stay out of the OR plan's way.
func (e *SelectEngine) orPlanRoutable(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, dbCtx *DatabaseContext) bool {
	if _, ok := e.ctx.PlanOrIndexScan(s.Where, tableEntry.Name, colDefs, dbCtx); ok {
		return true
	}
	expansion := e.expandOrInArms(s, tableEntry)
	if expansion == nil {
		return false
	}
	_, ok := e.ctx.PlanOrIndexScan(expansion.where, tableEntry.Name, colDefs, dbCtx)
	return ok
}

// exprHasOrSubtree reports whether the expression contains an OR node
// beneath its AND/parenthesis spine — peeling parentheses the way
// whereHasOrConjunct does, so a parenthesized OR conjunct is seen.
func exprHasOrSubtree(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		return exprHasOrSubtree(v.Expr)
	case *sql.BinaryOp:
		if strings.EqualFold(v.Operator, "OR") {
			return true
		}
		if strings.EqualFold(v.Operator, "AND") {
			return exprHasOrSubtree(v.Left) || exprHasOrSubtree(v.Right)
		}
		return false
	}
	return false
}

// whereNonOrConjuncts returns the AND of the WHERE's top-level conjuncts
// that contain no OR subtree (where.c: an OR term the OR-optimization
// declines is a plain filter — only the OR-free conjuncts can drive an
// index seek, and a term inside the OR is never a standalone constraint).
// nil when no conjunct qualifies.
func whereNonOrConjuncts(where sql.Expr) sql.Expr {
	var kept []sql.Expr
	for _, conj := range splitAnd(where) {
		if !exprHasOrSubtree(conj) {
			kept = append(kept, conj)
		}
	}
	return andChainExprs(kept)
}

// andChainExprs rebuilds an AND chain over the conjuncts (nil when empty,
// the single conjunct unwrapped).
func andChainExprs(conjuncts []sql.Expr) sql.Expr {
	switch len(conjuncts) {
	case 0:
		return nil
	case 1:
		return conjuncts[0]
	}
	out := conjuncts[0]
	for _, c := range conjuncts[1:] {
		out = &sql.BinaryOp{Left: out, Right: c, Operator: "AND"}
	}
	return out
}

// selectIndexSeekPlanFor resolves the index-driven scan for a single-table
// SELECT, or nil when the statement keeps the regular table scan. The gates
// mirror the shapes whose row order an index loop can serve: one real rowid
// table, no join, no GROUP BY / DISTINCT / compound member, and a WHERE
// whose top-level OR conjunct the OR-index planner did not claim (an OR is
// a union of index ranges, served by execSelectWithOrPlan; when the planner
// declines it, the OR-free conjuncts still drive a seek the way where.c
// keeps the surviving WHERE terms usable — where9-4.2's
// `b>1000 AND (c=31031 OR +d IS NULL)` seeks the t1b range). An ORDER BY
// rides along when the seek loop does not deliver it (the comparator sort
// runs in the post-scan pipeline, matching C's seek+temp-b-tree plan);
// a loop-delivering ORDER BY keeps the regular scan and the S22
// index-order emission. An INDEXED BY clause forces the named index the
// way C's planner does (where9-4.4). The driving index is the planner's
// own loop — the same decision the EXPLAIN QUERY PLAN renderer consumes —
// so a LIKE/GLOB conjunct that merely rides along as a residual filter
// (where8-1.8's `a > 1 AND c LIKE 'I%'` planned as `SEARCH t1 USING INDEX
// i1 (a>?)`) does not disconnect the executor from the plan. Only a plan
// whose own seek constraints were derived from a LIKE/complete-GLOB term
// keeps the regular scan, whose scan-local optimizer elides the
// provably-decided matcher calls (select_like_opt.go — the
// sqlite3_like_count contract).
func (e *SelectEngine) selectIndexSeekPlanFor(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, dbCtx *DatabaseContext) (*indexSeekPlan, error) {
	if !indexSeekShapeApplies(s, tableEntry) || e.ctx.TableIsWithoutRowidEntry(tableEntry) {
		return nil, nil
	}
	if s.From.NotIndexed {
		// NOT INDEXED forbids every index plan for the table (where9-4.6's
		// full table scan): the regular scan owns the statement.
		return nil, nil
	}
	seek := s
	forced := ""
	if idx := s.From.IndexedBy; idx != "" {
		// INDEXED BY forces the named index (build.c: the planner must use
		// it or report "no query solution"): the OR arbitration below is
		// skipped — the OR arms' own indexes are unavailable.
		forced = idx
	} else if whereHasOrConjunct(s.Where) {
		// The OR-index planner owns the statement when it plans the OR; when
		// it declines, only the OR-free top-level conjuncts may drive the
		// seek (a term inside the OR is not a standalone constraint — C
		// never extracts an arm as an index term). The full WHERE stays the
		// residual: every candidate row is re-checked against it.
		if e.orPlanRoutable(s, tableEntry, colDefs, dbCtx) {
			return nil, nil
		}
		where := whereNonOrConjuncts(s.Where)
		if where == nil {
			return nil, nil
		}
		ns := *s
		ns.Where = where
		seek = &ns
	}
	loop, err := e.scanLoopForRowCount(queryTableFromRef(seek.From), seek, indexSeekPlanRowCount, false)
	if err != nil {
		return nil, err
	}
	if forced != "" {
		if e.schemaIndexEntry(forced) == nil {
			return nil, nil
		}
		loop = scanLoop{kind: loopIndex, token: forced, seek: true, bestEst: loop.bestEst, nRow: loop.nRow}
	}
	if loop.kind != loopIndex || !loop.seek || loop.token == "PRIMARY KEY" {
		return nil, nil
	}
	if len(s.OrderBy) > 0 && len(s.GroupBy) == 0 && !s.Distinct {
		// A seek the ORDER BY delivery rides would double-serve the order
		// (the post-scan index-order emission owns those shapes): keep the
		// regular scan there. A seek that does NOT deliver runs ahead of the
		// comparator sort like C's seek+temp-b-tree plan.
		if _, sat := e.indexLoopOrderSatisfied(queryTableFromRef(s.From), loop, s); sat {
			return nil, nil
		}
	}
	plan := e.buildIndexSeekPlan(seek, tableEntry, colDefs, loop.token)
	if plan == nil || plan.patternDriven {
		return nil, nil
	}
	return plan, nil
}

// indexSeekShapeApplies reports whether the statement is a single-table scan
// whose row order an index loop can serve: one real FROM table, no join, no
// ORDER BY / GROUP BY / DISTINCT / compound member, and no system table.
// INDEXED BY / NOT INDEXED clauses are resolved in selectIndexSeekPlanFor
// (an INDEXED BY name forces the named index; NOT INDEXED forbids the seek).
func indexSeekShapeApplies(s *sql.SelectStmt, tableEntry *schema.Entry) bool {
	if s == nil || s.Where == nil || tableEntry == nil {
		return false
	}
	if !indexSeekPlainScanShape(s) {
		return false
	}
	return !IsSchemaTable(tableEntry.Name)
}

// indexSeekPlainScanShape reports whether the statement is a plain
// single-table scan: a real FROM table, no join, and no GROUP BY /
// DISTINCT / compound member. An ORDER BY is allowed — the plan-for gate
// declines the seek when the loop delivers it (the post-scan index-order
// emission owns those shapes) and runs the seek ahead of the comparator
// sort when it does not (C's seek + temp-b-tree shape, where9-4.2).
func indexSeekPlainScanShape(s *sql.SelectStmt) bool {
	if len(s.Joins) > 0 || len(s.GroupBy) > 0 || s.Distinct || s.Union != nil {
		return false
	}
	return s.From.Name != "" && s.From.Subquery == nil && !s.From.EmptyName
}

// indexSeekPlanRowCount is the row-count scale the seek path plans with. Every
// candidate ref's estimate is its selectivity times the row count, so the
// chosen index, the seek threshold and the tie-breaks are scale-invariant:
// this constant yields the same loop the EXPLAIN QUERY PLAN renderer picks
// from the table's real count, without paying that count's per-statement
// b-tree page walk on the hot lookup path.
const indexSeekPlanRowCount = defaultPlanRowCount

// indexSeekRefSet returns the (column, operator) pairs of the planner's own
// seekable refs for the driving index. A conjunct is only used as a seek
// constraint when the planner collected the same pair, so the seek inherits
// the planner's gates (LIKE collation compatibility, partial-index
// implication, the leading-prefix rule) instead of re-deriving them.
//
// The analysis runs through the statement-shape memo (whereref_cache.go);
// an all-plain shape's per-index set is published once and shared read-only,
// so the hot lookup path re-allocates nothing.
func (e *SelectEngine) indexSeekRefSet(s *sql.SelectStmt, tableEntry *schema.Entry, token string) map[string]bool {
	refs, sk, cached := e.whererefCollect(s.Where, tableEntry.Name)
	if cached {
		if sk.likeFree {
			return sk.seekSet(token)
		}
		refs = e.seekableRefs(refs, tableEntry.Name)
	} else {
		refs = e.seekableRefs(collectIndexedRefs(s.Where, tableEntry.Name, e), tableEntry.Name)
	}
	allowed := map[string]bool{}
	for _, ref := range refs {
		if ref.indexName == token {
			allowed[indexSeekRefKey(ref.colName, ref.op)] = true
		}
	}
	return allowed
}

// indexSeekRefKey keys the (column, operator) pair set case-insensitively on
// the column (SQL identifiers compare case-insensitively).
func indexSeekRefKey(col, op string) string {
	return strings.ToLower(col) + "\x00" + op
}

// buildIndexSeekPlan walks the driving index's key columns and collects the
// seek prefix: leading columns bound by equality refs, then at most one range
// column (where.c's nEq / nRange split). The prefix stops at the first column
// without a usable constraint; a plan with no constraint at all is declined.
func (e *SelectEngine) buildIndexSeekPlan(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, token string) *indexSeekPlan {
	cols := e.indexColumns(token)
	if len(cols) == 0 {
		return nil
	}
	allowed := e.indexSeekRefSet(s, tableEntry, token)
	if len(allowed) == 0 {
		return nil
	}
	terms := e.indexSeekTerms(s, tableEntry)
	plan := &indexSeekPlan{index: token}
	for pos := 0; pos < len(cols) && e.indexSeekPlanStep(plan, terms, allowed, colDefs, cols[pos]); pos++ {
	}
	if len(plan.eq) == 0 && plan.lo == nil && plan.hi == nil {
		return nil
	}
	return plan
}

// indexSeekPlanStep extends the plan with one index position and reports
// whether the prefix continues: an equality term appends its constant (a NULL
// constant proves the plan empty), a range term sets the bounds (and ends the
// prefix), and a position without a usable constraint ends it too.
func (e *SelectEngine) indexSeekPlanStep(plan *indexSeekPlan, terms []indexSeekTerm, allowed map[string]bool, colDefs []sql.ColumnDef, col string) bool {
	if plan.lo != nil || plan.hi != nil || !isPlainIndexColumn(col) {
		return false // the range column ends the seek prefix
	}
	if term, ok := indexSeekEqualityTerm(terms, allowed, col); ok {
		if term.val == nil {
			plan.empty = true
			return false
		}
		plan.eq = append(plan.eq, e.indexSeekAffinityValue(colDefs, col, term.val))
		return true
	}
	lo, hi, ok := e.indexSeekRangeTerms(plan, terms, allowed, colDefs, col)
	if !ok {
		return false
	}
	if (lo != nil && lo.val == nil) || (hi != nil && hi.val == nil) {
		plan.empty = true
		return false
	}
	plan.lo, plan.hi = lo, hi
	return true
}

// indexSeekEqualityTerm returns the equality term constraining col when the
// planner collected one.
func indexSeekEqualityTerm(terms []indexSeekTerm, allowed map[string]bool, col string) (indexSeekTerm, bool) {
	for _, t := range terms {
		if !strings.EqualFold(t.col, col) {
			continue
		}
		if t.op != "=" && t.op != "==" {
			continue
		}
		if allowed[indexSeekRefKey(t.col, t.op)] {
			return t, true
		}
	}
	return indexSeekTerm{}, false
}

// indexSeekRangeTerms returns the range bounds constraining col: a
// comparison bound, a BETWEEN pair, or the LIKE/GLOB prefix's lower bound
// (which walks the prefix run to the index's end, the WHERE filter making
// the candidate set exact). A contributing LIKE/complete-GLOB term marks the
// plan patternDriven — its statement keeps the regular scan (the
// scan-local optimizer's exact matcher-call elision).
func (e *SelectEngine) indexSeekRangeTerms(plan *indexSeekPlan, terms []indexSeekTerm, allowed map[string]bool, colDefs []sql.ColumnDef, col string) (*indexSeekBound, *indexSeekBound, bool) {
	var lo, hi *indexSeekBound
	for _, t := range terms {
		if !strings.EqualFold(t.col, col) || !allowed[indexSeekRefKey(t.col, t.op)] {
			continue
		}
		lo, hi = e.extendIndexSeekRange(plan, t, lo, hi, colDefs, col)
	}
	if lo == nil && hi == nil {
		return nil, nil, false
	}
	if lo != nil {
		lo.val = e.indexSeekAffinityValue(colDefs, col, lo.val)
	}
	if hi != nil {
		hi.val = e.indexSeekAffinityValue(colDefs, col, hi.val)
	}
	return lo, hi, true
}

// extendIndexSeekRange folds one term's value-space bounds into the running
// bound pair, marking the plan patternDriven when the term is a
// scan-local-only (LIKE/complete-GLOB) constraint.
func (e *SelectEngine) extendIndexSeekRange(plan *indexSeekPlan, t indexSeekTerm, lo, hi *indexSeekBound, colDefs []sql.ColumnDef, col string) (*indexSeekBound, *indexSeekBound) {
	l, h := e.indexSeekBoundFor(t, colDefs, col)
	if l != nil {
		lo = l
	}
	if h != nil {
		hi = h
	}
	if t.scanLocalOnly && (l != nil || h != nil) {
		plan.patternDriven = true
	}
	return lo, hi
}

// indexSeekBoundFor maps one range term to its value-space bounds: a
// comparison bound on either side, a BETWEEN pair, or the LIKE/GLOB prefix's
// lower bound.
func (e *SelectEngine) indexSeekBoundFor(t indexSeekTerm, colDefs []sql.ColumnDef, col string) (*indexSeekBound, *indexSeekBound) {
	switch t.op {
	case ">", ">=", "<", "<=":
		b := &indexSeekBound{op: t.op, val: t.val}
		if t.op == ">" || t.op == ">=" {
			return b, nil
		}
		return nil, b
	case "BETWEEN":
		return &indexSeekBound{op: ">=", val: t.val}, &indexSeekBound{op: "<=", val: t.val2}
	case "LIKE", "GLOB":
		// The prefix range is only sound on a TEXT-affinity column: SQLite's
		// LIKE/GLOB optimization requires TEXT affinity (where.c), because a
		// numeric key sorts BEFORE every text key in the index order while
		// its text rendering can still match the pattern — a seek from the
		// prefix's lower bound would skip it (like3-5.122's "x LIKE '-2%'"
		// over -234). A non-TEXT column keeps the regular scan, exactly as
		// SQLite plans it.
		if util.Affinity(e.indexSeekColumnType(colDefs, col)) != 'T' {
			return nil, nil
		}
		if t.op == "GLOB" && t.val2 != nil {
			return &indexSeekBound{op: ">=", val: t.val}, &indexSeekBound{op: "<", val: t.val2}
		}
		return &indexSeekBound{op: ">=", val: t.val}, nil
	}
	return nil, nil
}

// indexSeekTerms resolves every WHERE conjunct that can drive a seek into its
// (column, operator, constant) form. The constant side must be a literal or a
// bound parameter — anything else keeps the regular scan (the planner may
// still have chosen the index for another conjunct).
func (e *SelectEngine) indexSeekTerms(s *sql.SelectStmt, tableEntry *schema.Entry) []indexSeekTerm {
	var terms []indexSeekTerm
	alias := s.From.As
	for _, conj := range splitAnd(s.Where) {
		switch v := unwrapParenExpr(conj).(type) {
		case *sql.BinaryOp:
			if t, ok := e.indexSeekBinaryTerm(v, tableEntry, alias); ok {
				terms = append(terms, t)
			}
		case *sql.Between:
			if t, ok := e.indexSeekBetweenTerm(v, tableEntry, alias); ok {
				terms = append(terms, t)
			}
		}
	}
	return terms
}

// indexSeekBinaryTerm resolves one comparison conjunct: a column-to-constant
// comparison (either operand order, with the operator flipped when the column
// is the right operand) or a LIKE/GLOB whose constant pattern has a literal
// prefix.
func (e *SelectEngine) indexSeekBinaryTerm(v *sql.BinaryOp, tableEntry *schema.Entry, alias string) (indexSeekTerm, bool) {
	switch v.Operator {
	case "=", "==", "<", "<=", ">", ">=":
	case "LIKE", "GLOB":
		return e.indexSeekLikeTerm(v, tableEntry, alias)
	default:
		return indexSeekTerm{}, false
	}
	for _, sides := range [2][2]sql.Expr{{v.Left, v.Right}, {v.Right, v.Left}} {
		col, ok := e.indexSeekColumnRef(sides[0], tableEntry, alias)
		if !ok {
			continue
		}
		val, ok := e.seekConstValue(sides[1])
		if !ok {
			// A correlated bound: C compiles the subquery's seek with the
			// outer loop's value in the bound register — the comparison's
			// other side is a column reference that resolves outside this
			// statement (where8-3.14's `e = b` seeks i4 per outer row).
			val, ok = e.seekOuterBoundValue(sides[1], tableEntry, alias)
			if !ok {
				continue
			}
		}
		op := v.Operator
		if sides[0] == v.Right {
			op = flipComparisonOp(op)
		}
		return indexSeekTerm{col: col, op: op, val: val}, true
	}
	return indexSeekTerm{}, false
}

// flipComparisonOp mirrors a comparison operator when the column operand is
// on the right (5 > b means b < 5).
func flipComparisonOp(op string) string {
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

// indexSeekBetweenTerm resolves a non-negated BETWEEN conjunct with
// literal/parameter bounds into its inclusive lower/upper pair.
func (e *SelectEngine) indexSeekBetweenTerm(v *sql.Between, tableEntry *schema.Entry, alias string) (indexSeekTerm, bool) {
	if v.Negated {
		return indexSeekTerm{}, false
	}
	col, ok := e.indexSeekColumnRef(v.Operand, tableEntry, alias)
	if !ok {
		return indexSeekTerm{}, false
	}
	lo, ok1 := e.seekConstValue(v.Low)
	if !ok1 {
		lo, ok1 = e.seekOuterBoundValue(v.Low, tableEntry, alias)
	}
	hi, ok2 := e.seekConstValue(v.High)
	if !ok2 {
		hi, ok2 = e.seekOuterBoundValue(v.High, tableEntry, alias)
	}
	if !ok1 || !ok2 {
		return indexSeekTerm{}, false
	}
	return indexSeekTerm{col: col, op: "BETWEEN", val: lo, val2: hi}, true
}

// indexSeekLikeTerm resolves a LIKE/GLOB conjunct to its literal-prefix lower
// bound. A pattern without a literal prefix cannot position the scan and
// keeps the regular one (SQLite likewise only range-scans a non-empty
// prefix). A LIKE pattern, and a GLOB pattern whose prefix run provably
// decides the match (prefix + one trailing match-all wildcard), are marked
// scanLocalOnly: the regular scan's optimizer elides their matcher calls, so
// they must not drive an index seek.
func (e *SelectEngine) indexSeekLikeTerm(v *sql.BinaryOp, tableEntry *schema.Entry, alias string) (indexSeekTerm, bool) {
	col, ok := e.indexSeekColumnRef(v.Left, tableEntry, alias)
	if !ok {
		return indexSeekTerm{}, false
	}
	pattern, ok := likePatternConst(v.Right)
	if !ok {
		return indexSeekTerm{}, false
	}
	prefix := ""
	if v.Operator == "LIKE" {
		prefix, ok = likePrefix(pattern, v.Escape, v.HasEscape)
	} else {
		prefix = globLiteralPrefix(pattern)
	}
	if !ok || prefix == "" {
		return indexSeekTerm{}, false
	}
	term := indexSeekTerm{col: col, op: v.Operator, val: prefix}
	if v.Operator == "GLOB" {
		_, term.val2, _ = likeRangeBounds(prefix, false, false)
		_, complete, usable := likePrefixRange(pattern, v.Escape, v.HasEscape, '*', '?', '[')
		term.scanLocalOnly = usable && complete
	} else {
		term.scanLocalOnly = true
	}
	return term, true
}

// globLiteralPrefix returns the literal leading characters of a GLOB pattern
// (everything before the first wildcard). Every value the pattern matches
// starts with that prefix, so it is a sound lower bound under any collation;
// an empty prefix (a leading wildcard) yields "" and the scan stays regular.
func globLiteralPrefix(pattern string) string {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*', '?', '[':
			return pattern[:i]
		}
	}
	return pattern
}

// indexSeekColumnRef resolves a comparison operand to the name of a plain
// table column (unqualified, or qualified by the table name / FROM alias).
func (e *SelectEngine) indexSeekColumnRef(expr sql.Expr, tableEntry *schema.Entry, alias string) (string, bool) {
	ref, ok := unwrapParenExpr(expr).(*sql.ColumnRef)
	if !ok || ref.Name == "*" || groupByKeywordName(ref.Name) {
		return "", false
	}
	if ref.Table != "" && !strings.EqualFold(ref.Table, tableEntry.Name) &&
		(alias == "" || !strings.EqualFold(ref.Table, alias)) {
		return "", false
	}
	if isRowIDName(strings.ToLower(ref.Name)) || strings.EqualFold(ref.Name, "rowid") {
		return "", false
	}
	return ref.Name, true
}

// seekConstValue evaluates a comparison's constant operand: a literal or a
// bound parameter, unwrapped to the plain SQL value. ok is false for anything
// else (an expression, a column reference, a subquery).
func (e *SelectEngine) seekConstValue(expr sql.Expr) (interface{}, bool) {
	if expr == nil || !(isDMLSearchLiteral(expr) || isParameterExpr(expr)) {
		return nil, false
	}
	val, err := e.ctx.EvalExpr(expr, nil)
	if err != nil {
		return nil, false
	}
	return execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(val)), true
}

// seekOuterBoundValue binds a correlated comparison bound: a column
// reference that does not resolve to this statement's own table resolves
// through the enclosing outer rows (the subquery re-executes per outer row,
// so the bound is the outer loop's value — where.c compiles the seek with
// the outer register). ok is false when no outer row supplies the column.
func (e *SelectEngine) seekOuterBoundValue(expr sql.Expr, tableEntry *schema.Entry, alias string) (interface{}, bool) {
	if e.outerRow == nil && len(e.OuterRows()) == 0 {
		return nil, false
	}
	ref, isRef := unwrapParenExpr(expr).(*sql.ColumnRef)
	if !isRef || ref.Name == "*" {
		return nil, false
	}
	// The reference must not resolve inside this statement: a qualified one
	// must not name the FROM table's token, an unqualified one must not name
	// one of the table's columns.
	tok := strings.ToLower(schemaStrippedName(ref.Table))
	ownToken := strings.ToLower(schemaStrippedName(alias))
	if ownToken == "" {
		ownToken = strings.ToLower(schemaStrippedName(tableEntry.Name))
	}
	if ref.Table != "" && tok == ownToken {
		return nil, false
	}
	if ref.Table == "" {
		for _, cd := range e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL) {
			if strings.EqualFold(cd.Name, ref.Name) {
				return nil, false
			}
		}
	}
	qual := ref.Name
	if ref.Table != "" {
		qual = ref.Table + "." + ref.Name
	}
	for _, outer := range e.OuterRowsForResolution() {
		if outer == nil {
			continue
		}
		if val, ok := outer.Get(qual); ok {
			return execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(val)), true
		}
		if val, ok := outer.Get(ref.Name); ok {
			return execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(val)), true
		}
	}
	return nil, false
}

// indexSeekColumnType returns the declared type text of a column ("" when
// the column is not declared, which is BLOB affinity).
func (e *SelectEngine) indexSeekColumnType(colDefs []sql.ColumnDef, col string) string {
	for i := range colDefs {
		if strings.EqualFold(colDefs[i].Name, col) {
			return colDefs[i].Type
		}
	}
	return ""
}

// indexSeekAffinityValue applies the key column's declared affinity to a seek
// constant: the index stores affinity-converted keys, so the probe must carry
// the same conversion (vdbe.c's OP_Affinity before the seek).
func (e *SelectEngine) indexSeekAffinityValue(colDefs []sql.ColumnDef, col string, val interface{}) interface{} {
	if val == nil {
		return nil
	}
	for i := range colDefs {
		if strings.EqualFold(colDefs[i].Name, col) {
			return util.ApplyColumnAffinity(val, colDefs[i].Type)
		}
	}
	return val
}

// isPlainIndexColumn reports whether an index key column is a plain column
// name a WHERE reference can be matched against (no expression key, no
// residual COLLATE/ASC/DESC text).
func isPlainIndexColumn(col string) bool {
	col = strings.TrimSpace(col)
	return col != "" && !strings.ContainsAny(col, "( )")
}
