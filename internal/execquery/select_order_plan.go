package execquery

import (
	"strconv"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/sql"
)

// Once-per-sort ORDER BY comparator planning: sortRowsWithMaps resolves the
// terms (resolveOrderByOrdinalTerms), then sorts through lessRows, whose
// comparator re-derived every per-term invariant — positional index, column
// reference, alias resolution, result-column lookup, collation chain — on
// EVERY comparison (O(rows·log rows) times each). obSortPlan precomputes
// those invariants once per sort; the plan fast path in lessRows mirrors
// compareOrderByTerm's decision tree exactly and falls back to it per term
// for anything the plan does not model (non-column terms, unresolved
// ordinals). The plan binds to the sort's own ORDER BY term slice and row
// map slice identities, so any other lessRows caller (recursive CTE queue
// ordering) and any sort after a nested statement rebuilt the plan takes the
// legacy comparator.

// obSortTermKind classifies one planned ORDER BY term.
type obSortTermKind int

const (
	// obTermLegacy keeps the per-comparison comparator (non-column terms,
	// out-of-range ordinals).
	obTermLegacy obSortTermKind = iota
	// obTermBare is a bare unqualified column reference resolved by name
	// (output row first, then the row map's collation-carrying value).
	obTermBare
	// obTermPositional is an in-range numeric ordinal read directly from the
	// output row at its position (orderByPositionalIndex's rewrite, kept off
	// the shared AST).
	obTermPositional
)

// obSortPlan is the precomputed comparator for one sort.
type obSortPlan struct {
	terms      []obSortTerm
	orderByRef *sql.OrderByTerm // &orderBy[0] of the planned sort (identity)
	rowMapsRef *RowMap          // &rowMaps[0] of the sort that initialized the plan
	n          int              // len(orderBy) at build time
	init       bool             // initialized against the sort's result columns
	usable     bool             // false → every term takes the legacy comparator
}

// obSortTerm is one ORDER BY term's comparator invariants.
type obSortTerm struct {
	kind  obSortTermKind
	ob    sql.OrderByTerm // term copy driving direction and NULLS FIRST/LAST
	expr  sql.Expr        // the term expression the comparator sees (collation source)
	exprN sql.Expr        // normalizeOrderByExpr(expr): unary +/- and COLLATE stripped

	// positional term state (kind obTermPositional): output position, plus
	// the comparator's rewritten term for the fallback path. Bare terms
	// reuse pos as the result-column index of refName (-1 when unresolved).
	pos      int
	fallBack sql.OrderByTerm

	// bare term state (kind obTermBare)
	refName       string // the compared reference name
	aliasIsAlias  bool   // refName names a SELECT-list alias
	aliasResolved string // the alias's column name ("" when not a plain column)
	aliasPos      int    // result-column index of refName when it is an alias (-1)

	// collation chain (orderBySortCollation), resolved once per sort
	termColl     string // the term's explicit COLLATE
	aliasColl    string // the alias-inherited collation
	declaredColl string // the schema-declared column collation
}

// planSortOrderBy builds the comparator plan for a sort over orderBy. Called
// from resolveOrderByOrdinalTerms (the one hook every sortRowsWithMaps call
// passes through), so the plan always describes the sort that immediately
// follows.
func (e *SelectEngine) planSortOrderBy(orderBy []sql.OrderByTerm) {
	if len(orderBy) == 0 {
		e.obSortPlan = nil
		return
	}
	p := &obSortPlan{
		orderByRef: &orderBy[0],
		n:          len(orderBy),
		terms:      make([]obSortTerm, len(orderBy)),
	}
	for i := range orderBy {
		p.terms[i] = newSortTerm(orderBy[i])
	}
	e.obSortPlan = p
}

// newSortTerm classifies one term from its AST alone (result-column counts
// are not known yet).
func newSortTerm(ob sql.OrderByTerm) obSortTerm {
	t := obSortTerm{ob: ob, expr: ob.Expr}
	if nl, isLit := ob.Expr.(*sql.NumericLit); isLit {
		if pos, err := strconv.Atoi(nl.Value); err == nil && pos >= 1 {
			t.kind = obTermPositional
			t.pos = pos // 1-based candidate; range-checked at init
			return t
		}
		t.kind = obTermLegacy
		return t
	}
	exprN := normalizeOrderByExpr(ob.Expr)
	ref, isRef := stripCollate(exprN).(*sql.ColumnRef)
	if !isRef || ref.Table != "" || ref.Name == "*" {
		t.kind = obTermLegacy
		return t
	}
	t.kind = obTermBare
	t.exprN = exprN
	t.refName = ref.Name
	t.pos = -1
	t.aliasPos = -1
	return t
}

// initFor finalizes the plan against the sorting result columns: ordinal
// range checks, result-column positions, alias resolution, and the
// collation chain (the sort's obCollationResolver and alias stack are active
// by the first comparison). It runs once per plan and binds the plan to the
// calling sort's row map slice identity.
func (p *obSortPlan) initFor(e *SelectEngine, resultCols []string, rowMaps []RowMap) {
	p.init = true
	p.usable = true
	if len(rowMaps) > 0 {
		p.rowMapsRef = &rowMaps[0]
	} else {
		p.rowMapsRef = nil
	}
	for i := range p.terms {
		t := &p.terms[i]
		if !t.initTerm(e, resultCols) {
			continue // legacy term: the per-comparison comparator owns it
		}
		t.initCollations(e)
	}
}

// initTerm resolves one term's kind-specific invariants against the sorting
// result columns. false leaves the term on the legacy comparator.
func (t *obSortTerm) initTerm(e *SelectEngine, resultCols []string) bool {
	switch t.kind {
	case obTermPositional:
		return t.initPositional(resultCols)
	case obTermBare:
		return t.initBare(e, resultCols)
	default:
		return false
	}
}

// initPositional finalizes an in-range ordinal: the comparator reads the
// output row at the ordinal's position through a fresh column reference (the
// positional rewrite, kept off the shared AST).
func (t *obSortTerm) initPositional(resultCols []string) bool {
	if t.pos > len(resultCols) {
		// Out-of-range ordinal: the comparator never rewrites the term, the
		// expression stays a NumericLit, and the term takes the legacy path.
		t.kind = obTermLegacy
		return false
	}
	t.pos--
	ref := &sql.ColumnRef{Name: resultCols[t.pos]}
	t.expr = ref
	t.exprN = ref
	t.fallBack = sql.OrderByTerm{Expr: ref, Desc: t.ob.Desc, NullsFirst: t.ob.NullsFirst, NullsLast: t.ob.NullsLast}
	return true
}

// initBare finalizes a bare unqualified column term: result-column position
// and resolveOrderByRowValues' alias resolution.
func (t *obSortTerm) initBare(e *SelectEngine, resultCols []string) bool {
	t.exprN = normalizeOrderByExpr(t.expr)
	ref, isRef := stripCollate(t.exprN).(*sql.ColumnRef)
	if !isRef || ref.Table != "" || ref.Name == "*" {
		t.kind = obTermLegacy
		return false
	}
	t.refName = ref.Name
	t.pos = resultColumnIndex(resultCols, ref.Name)
	t.fallBack = t.ob
	// resolveOrderByRowValues' alias resolution.
	resolvedName, isAlias := e.orderByAliasColumnName(ref.Name)
	t.aliasIsAlias = isAlias
	t.aliasResolved = resolvedName
	if isAlias && resolvedName != "" {
		t.aliasPos = resultColumnIndex(resultCols, ref.Name)
	}
	return true
}

// initCollations resolves the term's collation chain prefix — the parts of
// orderBySortCollation that do not depend on the compared values.
func (t *obSortTerm) initCollations(e *SelectEngine) {
	t.termColl = orderByTermCollation(t.expr)
	t.aliasColl = e.aliasOrderByCollation(t.expr)
	t.declaredColl = e.declaredOrderByCollation(t.expr)
}

// planMatches reports whether the plan describes THIS lessRows call: same
// ORDER BY term slice (identity, set at build) and — once initialized — the
// same row map slice the plan was initialized against. Anything else —
// another sort, a recursive CTE queue comparison, a sort after a nested
// statement rebuilt the plan — takes the legacy comparator.
func (p *obSortPlan) planMatches(orderBy []sql.OrderByTerm, rowMaps []RowMap) bool {
	if len(orderBy) != p.n || p.orderByRef != &orderBy[0] {
		return false
	}
	if !p.init {
		return true // caller initializes and binds before comparing
	}
	if p.rowMapsRef == nil {
		return len(rowMaps) == 0
	}
	return len(rowMaps) > 0 && p.rowMapsRef == &rowMaps[0]
}

// lessRowsPlan is lessRows over the planned terms. Legacy terms delegate to
// compareOrderByTerm exactly as before.
func (e *SelectEngine) lessRowsPlan(p *obSortPlan, orderBy []sql.OrderByTerm, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) bool {
	for ti := range p.terms {
		t := &p.terms[ti]
		var cmp int
		switch t.kind {
		case obTermBare:
			cmp = e.planCompareBare(t, orderBy[ti], rowMaps, rows, resultCols, i, j)
		case obTermPositional:
			cmp = e.planComparePositional(t, rowMaps, rows, resultCols, i, j)
		default:
			cmp = e.compareOrderByTerm(orderBy[ti], rowMaps, rows, resultCols, i, j)
		}
		if cmp < 0 {
			return true
		}
		if cmp > 0 {
			return false
		}
	}
	return false
}

// planCompareBare mirrors compareOrderByTerm for a bare unqualified column
// term: output-row values first, overridden by the row map's values (which
// carry the column's declared collation), then the fallback when either side
// is unresolved.
func (e *SelectEngine) planCompareBare(t *obSortTerm, ob sql.OrderByTerm, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) int {
	left, right, lok, rok := t.bareOutputValues(rows, i, j)
	if !t.aliasKeepsOutputValues() {
		if decided, l, r := t.aliasPositionValues(rows, i, j, left, right); decided {
			return e.planCompareValues(t, l, r)
		}
		left, right = t.bareRowMapValues(rowMaps, i, j, left, right)
	}
	if !lok || !rok {
		return e.compareOrderByFallback(t.fallBack, t.exprN, rowMaps, rows, resultCols, i, j)
	}
	return e.planCompareValues(t, left, right)
}

// bareOutputValues reads the term's output-row values (resolveOrderByValue):
// the result-column position resolved at plan time, with per-row bounds
// checks. found=false means the comparator's fallback runs.
func (t *obSortTerm) bareOutputValues(rows [][]interface{}, i, j int) (left, right interface{}, lok, rok bool) {
	if t.pos < 0 {
		return
	}
	if t.pos < len(rows[i]) {
		left, lok = rows[i][t.pos], true
	}
	if t.pos < len(rows[j]) {
		right, rok = rows[j][t.pos], true
	}
	return
}

// aliasKeepsOutputValues mirrors resolveOrderByRowValues' early return: an
// alias whose expression is not a plain column reference keeps the
// output-row values (the row map would grab a same-named source column).
func (t *obSortTerm) aliasKeepsOutputValues() bool {
	return t.aliasIsAlias && t.aliasResolved == ""
}

// aliasPositionValues implements the alias-resolves-to-output-column branch:
// when the alias's output position exists in rows[i], both sides read that
// position (rows[j] out of bounds keeps the prior right, exactly like the
// comparator) and the comparison is decided.
func (t *obSortTerm) aliasPositionValues(rows [][]interface{}, i, j int, left, right interface{}) (decided bool, l, r interface{}) {
	if t.aliasResolved == "" || t.aliasPos < 0 || t.aliasPos >= len(rows[i]) {
		return false, left, right
	}
	left = rows[i][t.aliasPos]
	if t.aliasPos < len(rows[j]) {
		right = rows[j][t.aliasPos]
	}
	return true, left, right
}

// bareRowMapValues applies the row-map override: the value under the term's
// (alias-resolved) row name carries the column's declared collation.
func (t *obSortTerm) bareRowMapValues(rowMaps []RowMap, i, j int, left, right interface{}) (interface{}, interface{}) {
	rowName := t.refName
	if t.aliasResolved != "" {
		rowName = t.aliasResolved
	}
	if lm, ok := rowMaps[i].Get(rowName); ok {
		left = lm
	}
	if rm, ok := rowMaps[j].Get(rowName); ok {
		right = rm
	}
	return left, right
}

// planComparePositional mirrors compareOrderByTerm's positional branch: the
// output row is read at the ordinal's position with no row-map preference;
// out-of-bounds positions fall back exactly like the comparator.
func (e *SelectEngine) planComparePositional(t *obSortTerm, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) int {
	if t.pos < len(rows[i]) && t.pos < len(rows[j]) {
		return e.planCompareValues(t, rows[i][t.pos], rows[j][t.pos])
	}
	return e.compareOrderByFallback(t.fallBack, t.exprN, rowMaps, rows, resultCols, i, j)
}

// planCompareValues mirrors compareOrderByValues with the term's precomputed
// collation chain: NULL placement, then the term's explicit COLLATE, the
// alias-inherited collation, a collation marker carried by the compared
// values, and finally the schema-declared collation.
func (e *SelectEngine) planCompareValues(t *obSortTerm, left, right interface{}) int {
	if cmp, isNull := nullOrderByCmp(execexpr.IsSQLNull(left), execexpr.IsSQLNull(right), t.ob); isNull {
		return cmp
	}
	coll := t.termColl
	if coll == "" {
		coll = t.aliasColl
	}
	if coll == "" {
		if _, c := extractValue(left); c != "" {
			coll = c
		} else if _, c := extractValue(right); c != "" {
			coll = c
		}
	}
	if coll == "" {
		coll = t.declaredColl
	}
	if coll != "" {
		// An explicit COLLATE in the ORDER BY term (or one inherited from a
		// SELECT-list alias) overrides the column's declared collation (see
		// compareOrderByValues).
		lc, _ := extractValue(left)
		rc, _ := extractValue(right)
		cmp := e.ctx.CompareValuesCollate(lc, rc, coll)
		if t.ob.Desc {
			cmp = -cmp
		}
		return cmp
	}
	cmp := e.ctx.CompareValuesWithCollate(left, right)
	if t.ob.Desc {
		cmp = -cmp
	}
	return cmp
}
