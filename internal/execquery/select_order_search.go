// Package exec implements query execution.
//
// This file holds the SEARCH-plan ORDER BY contract (where.c
// wherePathSatisfiesOrderBy): a single-table loop whose seek prefix and
// residual index columns deliver the full ORDER BY emits rows in the index
// b-tree's stored key order, so the planner omits the temp b-tree sorter
// (orderByConsumed) and the executor skips the comparator sort. The loop
// itself is described by scanLoop, computed once by scanLoopForQuery and
// consumed by both the EXPLAIN QUERY PLAN renderer and the row emitter.
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// scanLoop kinds. loopNone covers plain table scans and the plans whose
// ordering behavior the legacy sortCoveredByIndex heuristic keeps owning
// (skip-scans, virtual tables, COUNT covering scans).
const (
	loopNone = iota
	loopVtab
	loopSkipScan
	loopIPK
	loopIndex
)

// scanLoop describes the single-table scan loop the planner chose, mirroring
// where.c's WhereLoop for the ordering analysis: the driving index (token),
// whether it positions (a SEARCH) or is walked whole, and whether it emits at
// most one row (an INTEGER PRIMARY KEY equality seek). conditions/bestEst/
// nRow carry the plan rendering inputs so planSingleTable renders straight
// from the loop.
type scanLoop struct {
	kind          int
	token         string // planner index token (loopIndex)
	seek          bool   // SEARCH positioning (vs a full index walk)
	oneRow        bool   // at most one row (IPK equality seek)
	groupDistinct bool   // full index walk chosen for GROUP BY / DISTINCT
	conditions    string // constraint text (loopIndex) or the full SEARCH node (loopIPK)
	bestEst       float64
	nRow          float64
}

// scanLoopForQuery computes the single-table scan loop: the skip-scan, INTEGER
// PRIMARY KEY seek, selective secondary-index seek, or full index walk that
// planSingleTable renders (mirroring its decision order exactly — where.c
// computes the loop once and both the plan and the ORDER BY analysis consume
// it).
func (e *SelectEngine) scanLoopForQuery(t queryTable, s *sql.SelectStmt) scanLoop {
	nRow := e.tableRowCount(t.display)
	if nRow == 0 {
		nRow = defaultPlanRowCount // default estimate
	}
	return e.scanLoopForRowCount(t, s, nRow)
}

// defaultPlanRowCount is the planner's default row-count estimate for a table
// whose b-tree holds no entry (where.c's default 1e6).
const defaultPlanRowCount = 1000000

// scanLoopForRowCount is scanLoopForQuery with a caller-supplied row count.
// Every candidate ref's estimate scales linearly with it (refEstimate), so a
// SCALED row count yields the same index choice, seek threshold and
// tie-breaks while skipping the planner's per-statement b-tree page walk —
// the seek path re-derives the loop for every statement it serves.
func (e *SelectEngine) scanLoopForRowCount(t queryTable, s *sql.SelectStmt, nRow int64) scanLoop {
	loop := scanLoop{bestEst: float64(nRow), nRow: float64(nRow)}
	bestIndex := ""
	if s.Where != nil {
		bestIndex, _, loop = e.whereScanLoop(t, s, loop, nRow)
		if loop.kind != loopNone {
			return loop
		}
	}
	if bestIndex == "" {
		e.scanLoopForOrderIndex(t, s, &loop)
	}
	return loop
}

// whereScanLoop resolves the WHERE-driven loop: a skip-scan, an INTEGER
// PRIMARY KEY seek, or a selective secondary-index seek (the seek threshold:
// without a sqlite_stat1 row SQLite's default cost model prices an index
// range seek at one tenth of a full scan, so the seek always wins — intpkey-2.5).
// Returns the best-index rendering inputs alongside; a loopNone result falls
// through to the ORDER BY / GROUP BY index walks. rowCount is the loop's
// planning row count (the caller's scale).
func (e *SelectEngine) whereScanLoop(t queryTable, s *sql.SelectStmt, loop scanLoop, rowCount int64) (string, string, scanLoop) {
	ret := loop
	bestIndex, conditions := e.bestIndexForRowCount(t.display, s.Where, &ret.bestEst, rowCount)
	if ss := e.trySkipScanPlan(t.display, s.Where, ret.bestEst); ss != nil {
		ret.kind, ret.token, ret.seek, ret.conditions = loopSkipScan, ss.indexName, true, ss.conditions
		return bestIndex, conditions, ret
	}
	if detail, eq, ok := e.rowidSeekPlanDetail(t, s); ok {
		ret.kind, ret.oneRow, ret.conditions = loopIPK, eq, detail
		return bestIndex, conditions, ret
	}
	if bestIndex != "" && (bestIndex == "PRIMARY KEY" || e.indexSeekChosen(bestIndex, ret.bestEst, ret.nRow)) {
		ret.kind, ret.token, ret.seek, ret.conditions = loopIndex, bestIndex, true, conditions
		return bestIndex, conditions, ret
	}
	return bestIndex, conditions, ret
}

// scanLoopForOrderIndex records the full index walk chosen for the ORDER BY
// or the GROUP BY / DISTINCT optimization when no WHERE-driven seek exists.
func (e *SelectEngine) scanLoopForOrderIndex(t queryTable, s *sql.SelectStmt, loop *scanLoop) {
	if tok, ok := e.orderByIndexToken(t, s); ok {
		loop.kind, loop.token = loopIndex, tok
		return
	}
	if len(s.GroupBy) > 0 || s.Distinct {
		if tok, ok := e.groupDistinctIndexToken(t, s); ok {
			loop.kind, loop.token, loop.groupDistinct = loopIndex, tok, true
		}
	}
}

// indexSeekChosen mirrors the seek threshold: without a sqlite_stat1 row for
// the index, SQLite's default cost model prices an index range seek at one
// tenth of a full scan, so the seek always wins; with stats the 10%
// selectivity threshold applies.
func (e *SelectEngine) indexSeekChosen(bestIndex string, bestEstimate, nRow float64) bool {
	if len(e.stat1Tokens(bestIndex)) == 0 {
		return true
	}
	return nRow > 0 && bestEstimate < nRow*0.10
}

// orderTerm is one ORDER BY term resolved for the loop-order walk.
type orderTerm struct {
	col        string // column name ("rowid" pseudo-column included)
	desc       bool
	collate    string // explicit COLLATE name on the term ("" = none)
	nullsFirst bool
	nullsLast  bool
	sat        bool // consumed by the pre-pass or the index walk
}

// orderTermsForWalk resolves the ORDER BY terms to bare columns of the loop's
// table (a term qualified with the FROM table resolves to its column).
// ok is false when any term is an expression — such terms always need the
// sorter (where.c TK_COLUMN check).
func orderTermsForWalk(orderBy []sql.OrderByTerm, t queryTable) ([]orderTerm, bool) {
	var terms []orderTerm
	for _, ob := range orderBy {
		ref, ok := normalizeOrderByExpr(ob.Expr).(*sql.ColumnRef)
		if !ok || ref.Name == "*" {
			return nil, false
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, t.display) && !strings.EqualFold(ref.Table, t.real) {
			return nil, false
		}
		terms = append(terms, orderTerm{
			col:        ref.Name,
			desc:       ob.Desc,
			collate:    orderByTermExplicitCollation(ob.Expr),
			nullsFirst: ob.NullsFirst,
			nullsLast:  ob.NullsLast,
		})
	}
	return terms, len(terms) > 0
}

// prePassEqBoundTerms marks every ORDER BY term whose column an AND conjunct
// of the WHERE clause fixes by equality (where.c's eqOpMask pre-pass: X=? /
// X IS ? / X IS NULL deliver one value per row, so any output order satisfies
// the term; a collation only has to agree for value-carrying equalities).
func (e *SelectEngine) prePassEqBoundTerms(t queryTable, where sql.Expr, terms []orderTerm) {
	if where == nil {
		return
	}
	for i := range terms {
		if terms[i].sat {
			continue
		}
		if e.termEqBoundByWhere(t, where, &terms[i]) {
			terms[i].sat = true
		}
	}
}

// eqConjunctColumnSides splits an equality conjunct into its column operand
// and the other side (either orientation).
func eqConjunctColumnSides(v *sql.BinaryOp) (col, other sql.Expr, ok bool) {
	if ref, isRef := v.Left.(*sql.ColumnRef); isRef {
		return ref, v.Right, true
	}
	if ref, isRef := v.Right.(*sql.ColumnRef); isRef {
		return ref, v.Left, true
	}
	return nil, nil, false
}

// termColumnMatches reports whether a WHERE operand's column reference names
// the ORDER BY term's column on the loop's table.
func termColumnMatches(ref *sql.ColumnRef, t queryTable, col string) bool {
	if !strings.EqualFold(ref.Name, col) {
		return false
	}
	return ref.Table == "" || strings.EqualFold(ref.Table, t.display) || strings.EqualFold(ref.Table, t.real)
}

// termEqBoundByWhere reports whether one ORDER BY term's column is fixed by
// an equality conjunct of the WHERE clause whose collation agrees with the
// term's.
func (e *SelectEngine) termEqBoundByWhere(t queryTable, where sql.Expr, tr *orderTerm) bool {
	for _, conj := range splitAnd(where) {
		if e.termEqBoundByConjunct(t, conj, tr) {
			return true
		}
	}
	return false
}

// termEqBoundByConjunct reports whether one WHERE conjunct fixes the term's
// column: X IS NULL outright, or an equality whose collation agrees.
func (e *SelectEngine) termEqBoundByConjunct(t queryTable, conj sql.Expr, tr *orderTerm) bool {
	switch v := conj.(type) {
	case *sql.IsNull:
		ref, ok := v.Operand.(*sql.ColumnRef)
		return ok && termColumnMatches(ref, t, tr.col)
	case *sql.BinaryOp:
		return e.eqConjunctBound(t, v, tr)
	}
	return false
}

// eqConjunctBound reports whether one equality conjunct (X = c, X == c, or
// X IS c) fixes the term's column under an agreeing collation.
func (e *SelectEngine) eqConjunctBound(t queryTable, v *sql.BinaryOp, tr *orderTerm) bool {
	if v.Operator != "=" && v.Operator != "==" && v.Operator != "IS" {
		return false
	}
	colExpr, other, ok := eqConjunctColumnSides(v)
	if !ok {
		return false
	}
	ref, isRef := colExpr.(*sql.ColumnRef)
	if !isRef || !termColumnMatches(ref, t, tr.col) || !isEqBoundOperand(other) {
		return false
	}
	return e.eqCollationsAgree(t, ref, tr)
}

// isEqBoundOperand reports whether the non-column side of an equality conjunct
// fixes one value: a literal or a bind parameter.
func isEqBoundOperand(expr sql.Expr) bool {
	return isDMLSearchLiteral(expr) || isParameterExpr(expr)
}

// eqCollationsAgree reports whether the equality conjunct's column-side
// collation (an explicit COLLATE, else the column's declared collation)
// equals the ORDER BY term's effective collation (where.c compares the two
// before marking a term satisfied: ORDER BY a COLLATE nocase is not delivered
// by a binary a=? constraint).
func (e *SelectEngine) eqCollationsAgree(t queryTable, ref *sql.ColumnRef, tr *orderTerm) bool {
	whereColl := ""
	if c := collateWrapperName(ref); c != "" {
		whereColl = c
	} else {
		whereColl = e.declaredColumnCollation(t.real, tr.col)
	}
	termColl := tr.collate
	if termColl == "" {
		termColl = e.declaredColumnCollation(t.real, tr.col)
	}
	return strings.EqualFold(whereColl, termColl)
}

// collateWrapperName returns the collation name when expr is a top-level
// COLLATE operator, or "".
func collateWrapperName(expr sql.Expr) string {
	if b, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(b.Operator, "COLLATE") {
		if lit, isLit := b.Right.(*sql.StringLit); isLit {
			return lit.Value
		}
	}
	return ""
}

// indexLoopOrderSatisfied reports whether the loop's index delivers the full
// ORDER BY, and the scan direction that does it. ok is false when the sorter
// is required. This is wherePathSatisfiesOrderBy for a single loop: the
// equality-bound pre-pass, the in-order index-column walk over the
// unconstrained columns (one direction for all of them), and the
// order-distinct mark-off for the remaining terms.
func (e *SelectEngine) indexLoopOrderSatisfied(t queryTable, loop scanLoop, s *sql.SelectStmt) (backward, ok bool) {
	idxCols := e.indexColumns(loop.token)
	if len(idxCols) == 0 || s == nil {
		return false, false
	}
	terms, walkable := orderTermsForWalk(s.OrderBy, t)
	if !walkable {
		return false, false
	}
	e.prePassEqBoundTerms(t, s.Where, terms)
	nEq := 0
	if loop.seek {
		nEq = e.eqSeekRun(t.real, loop.token, s.Where)
	}
	backward, allSat := e.walkIndexOrderTerms(t, loop.token, idxCols, nEq, terms)
	return backward, allSat
}

// eqSeekRun counts the loop index's leading columns bound by equality refs
// (where.c nEq: the equality prefix; a trailing range column is NOT counted —
// its rows still emerge in index order).
func (e *SelectEngine) eqSeekRun(tableName, token string, where sql.Expr) int {
	cols := e.seekRefColumns(tableName, token)
	if len(cols) == 0 {
		return 0
	}
	var idxRefs []indexedRef
	for _, ref := range collectIndexedRefs(where, tableName, e) {
		if ref.indexName == token {
			idxRefs = append(idxRefs, ref)
		}
	}
	n := 0
	for n < len(cols) && hasSeekRefAt(cols, idxRefs, n, isEqualitySeekOp) {
		n++
	}
	return n
}

// walkCtx carries the per-walk state of one index-order analysis: the loop's
// table and index, and the scan direction fixed by the first free match.
type walkCtx struct {
	t        queryTable
	token    string
	ipkAlias string
	backward bool
	revSet   bool
}

// walkIndexOrderTerms walks the index's columns in key order (the trailing
// implicit rowid of a rowid table included) and marks the terms each free
// column satisfies: the first unsatisfied term must name the column, under
// the column's collation, with one scan direction shared by every free
// match. orderDistinct tracks whether the loop's output is provably free of
// duplicate key rows (a unique index whose walk consumed every key column),
// which lets any remaining term of the table be marked off afterwards.
// Returns the scan direction and whether every term is satisfied.
func (e *SelectEngine) walkIndexOrderTerms(t queryTable, token string, idxCols []string, nEq int, terms []orderTerm) (backward, allSat bool) {
	idxDescs, known := e.indexColumnDescFlags(t.real, token)
	if !known || len(idxDescs) < len(idxCols) {
		return false, false
	}
	ctx := &walkCtx{t: t, token: token, ipkAlias: e.ipkAliasColumn(t.real)}
	orderDistinct := e.indexIsUnique(token)
	for j := 0; j <= len(idxCols); j++ {
		if j < nEq {
			continue // equality-bound prefix: its terms were marked by the pre-pass
		}
		var keepGoing bool
		orderDistinct, keepGoing = e.walkColumnStep(ctx, idxCols, idxDescs, j, terms, orderDistinct)
		if !keepGoing {
			break
		}
	}
	if orderDistinct {
		// Mark-off pass: rows emerge distinct under the consumed columns, so
		// any remaining term of the loop's table adds no ordering constraint.
		markAllTermsSatisfied(terms)
	}
	return ctx.backward, allTermsSatisfied(terms)
}

// walkColumnStep processes one walk position j: it maintains the loop's
// order-distinct state and marks the matched term satisfied. keepGoing is
// false when the walk must stop (no term matches this column — where.c's
// no-match break).
func (e *SelectEngine) walkColumnStep(ctx *walkCtx, idxCols []string, idxDescs []bool, j int, terms []orderTerm, orderDistinct bool) (bool, bool) {
	isRowidCol := j == len(idxCols)
	col, colDesc := walkColumn(idxCols, idxDescs, j)
	if orderDistinct && !isRowidCol && !e.columnNotNull(ctx.t.real, col) {
		// An unconstrained nullable column can hold duplicate NULLs
		// (where.c tag-20210426-1) — the loop is not order-distinct.
		orderDistinct = false
	}
	tr := firstUnsatisfiedTerm(terms)
	rev, matched := e.matchWalkColumn(ctx, col, colDesc, isRowidCol, tr)
	if !matched {
		// A break at or before the last key column also voids distinctness.
		if j == 0 || j < len(idxCols) {
			orderDistinct = false
		}
		return orderDistinct, false
	}
	if !ctx.revSet {
		ctx.backward, ctx.revSet = rev, true
	}
	tr.sat = true
	if isRowidCol {
		// A rowid match makes the loop's rows pairwise distinct.
		orderDistinct = true
	}
	return orderDistinct, true
}

// walkColumn returns the name and stored sort order of the index column at
// walk position j; the position len(idxCols) is the implicit rowid, always
// ascending.
func walkColumn(idxCols []string, idxDescs []bool, j int) (string, bool) {
	if j == len(idxCols) {
		return "rowid", false
	}
	return idxCols[j], idxDescs[j]
}

// markAllTermsSatisfied marks every term consumed.
func markAllTermsSatisfied(terms []orderTerm) {
	for i := range terms {
		terms[i].sat = true
	}
}

// matchWalkColumn matches the candidate term against the walk column: the
// column name, the column's collation, the one shared scan direction, and
// the NULLS placement must all agree (where.c isMatch conditions).
// matched=false leaves the walk — the sorter takes over.
func (e *SelectEngine) matchWalkColumn(ctx *walkCtx, col string, colDesc, isRowidCol bool, tr *orderTerm) (rev, matched bool) {
	if tr == nil || !walkColumnNamesTerm(col, isRowidCol, tr.col, ctx.ipkAlias) {
		return false, false
	}
	if !isRowidCol && !e.walkCollationAgree(ctx.t, ctx.token, col, tr) {
		return false, false
	}
	rev = colDesc != tr.desc
	if ctx.revSet && rev != ctx.backward {
		return false, false
	}
	if !walkNullsAgree(colDesc, rev, tr) {
		return false, false
	}
	return rev, true
}

// walkColumnNamesTerm reports whether the walk column is the term's column:
// a key column by name, the trailing implicit rowid by the rowid
// pseudo-name or the table's INTEGER PRIMARY KEY alias (ipkAlias, resolved
// once per walk; "" when the table has none).
func walkColumnNamesTerm(col string, isRowidCol bool, termCol, ipkAlias string) bool {
	if !isRowidCol {
		return strings.EqualFold(col, termCol)
	}
	return isRowIDName(termCol) || (ipkAlias != "" && strings.EqualFold(termCol, ipkAlias))
}

// ipkAliasColumn returns the table's INTEGER PRIMARY KEY rowid-alias column
// name, or "" when the table has none.
func (e *SelectEngine) ipkAliasColumn(tableName string) string {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return ""
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if isIPKRowidAliasCol(cd) {
			return cd.Name
		}
	}
	return ""
}

// walkCollationAgree reports whether the ORDER BY term's effective collation
// equals the index column's collation (where.c compares the term's collation
// with the index's azColl before matching: a BINARY term is not delivered by
// a NOCASE index key).
func (e *SelectEngine) walkCollationAgree(t queryTable, token string, col string, tr *orderTerm) bool {
	termColl := tr.collate
	if termColl == "" {
		termColl = e.declaredColumnCollation(t.real, tr.col)
	}
	return strings.EqualFold(termColl, e.indexColumnCollation(t.real, token, col))
}

// walkNullsAgree applies the t33 NULLS placement rule to one matched term: a
// forward scan over an ascending column emits NULLs first (a backward or
// descending one last), and an explicit NULLS FIRST/LAST must agree.
// Default placements always agree once the direction matched.
func walkNullsAgree(colDesc, backward bool, tr *orderTerm) bool {
	scanNullsFirst := colDesc == backward
	requested := !tr.desc
	if tr.nullsFirst {
		requested = true
	} else if tr.nullsLast {
		requested = false
	}
	return requested == scanNullsFirst
}

// allTermsSatisfied reports whether every term was marked.
func allTermsSatisfied(terms []orderTerm) bool {
	for i := range terms {
		if !terms[i].sat {
			return false
		}
	}
	return true
}

// firstUnsatisfiedTerm returns the first unconsumed term in ORDER BY order.
func firstUnsatisfiedTerm(terms []orderTerm) *orderTerm {
	for i := range terms {
		if !terms[i].sat {
			return &terms[i]
		}
	}
	return nil
}

// columnNotNull reports whether the table column is declared NOT NULL (or is
// the INTEGER PRIMARY KEY rowid alias, which can never hold NULL).
func (e *SelectEngine) columnNotNull(tableName, colName string) bool {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return false
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if strings.EqualFold(cd.Name, colName) {
			return cd.NotNull || isIPKRowidAliasCol(cd)
		}
	}
	return false
}

// indexIsUnique reports whether the loop index is a UNIQUE index (an explicit
// CREATE UNIQUE INDEX, or an automatic UNIQUE/PK constraint index whose
// schema SQL is empty). Uniqueness under the constrained prefix is what makes
// the loop's output order-distinct.
func (e *SelectEngine) indexIsUnique(token string) bool {
	entry := e.schemaIndexEntry(indexSchemaName(token))
	if entry == nil {
		return false
	}
	if entry.SQL == "" {
		return true // sqlite_autoindex_: built for UNIQUE / PRIMARY KEY
	}
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(entry.SQL)), "CREATE UNIQUE INDEX")
}

// orderByConsumedByLoop decides the EXPLAIN QUERY PLAN "USE TEMP B-TREE FOR
// ORDER BY" node for one SELECT (where.c orderByConsumed): the chosen
// single-table loop satisfying the full ORDER BY omits the node. Decisions
// the loop cannot describe — joins, subquery scans, WITHOUT ROWID storage
// order, GROUP BY / DISTINCT grouping order, skip-scans, virtual tables —
// keep the legacy sortCoveredByIndex heuristic.
func (e *SelectEngine) orderByConsumedByLoop(tables []queryTable, s *sql.SelectStmt, loop scanLoop) bool {
	if len(tables) != 1 {
		return false
	}
	t := tables[0]
	if t.subquery != nil {
		return false
	}
	if len(s.GroupBy) > 0 || s.Distinct {
		return e.sortCoveredByIndex(tables, s, orderByCols(s))
	}
	if len(e.withoutRowidPKCols(t.real)) > 0 {
		return e.sortCoveredByIndex(tables, s, orderByCols(s))
	}
	switch loop.kind {
	case loopIPK:
		// An INTEGER PRIMARY KEY equality seek emits at most one row: every
		// ORDER BY term is trivially delivered (where.c WHERE_ONEROW).
		return loop.oneRow
	case loopIndex:
		_, ok := e.indexLoopOrderSatisfied(t, loop, s)
		return ok
	}
	return e.sortCoveredByIndex(tables, s, orderByCols(s))
}

// loopOrderedEmission reports the index whose b-tree order the executor must
// emit a SEARCH plan's rows in, and the direction: the same loop-order gate
// the planner used to drop the sorter (so the EQP shape and the emitted row
// order never disagree). ok is false when the comparator sort stays
// responsible for the ordering.
func (e *SelectEngine) loopOrderedEmission(s *sql.SelectStmt) (idxName string, backward, ok bool) {
	if s == nil || s.Where == nil || s.Union != nil || len(s.Joins) > 0 ||
		s.From.Name == "" || s.From.Subquery != nil || len(s.GroupBy) > 0 || s.Distinct {
		return "", false, false
	}
	if len(e.withoutRowidPKCols(s.From.Name)) > 0 {
		return "", false, false
	}
	loop := e.scanLoopForQuery(queryTableFromRef(s.From), s)
	if loop.kind != loopIndex {
		return "", false, false
	}
	backward, satisfied := e.indexLoopOrderSatisfied(queryTableFromRef(s.From), loop, s)
	if !satisfied {
		return "", false, false
	}
	return loop.token, backward, true
}

// emitIndexOrderedRows applies the two index-order emission gates: the
// plain-scan gate (all terms bare, index-leading-prefix order) and the
// SEARCH gate (loopOrderedEmission: seek prefix + residual index columns —
// the gate the planner used to omit the sorter, so the EQP shape and the
// emitted row order never disagree). When either gate accepts, the result
// rows are permuted into the index b-tree's stored key order: the seek
// prefix fixes the leading values and the walk orders the rest, including
// the reverse direction and its rowid-descending ties. Returns false when
// the comparator sort stays responsible.
func (e *SelectEngine) emitIndexOrderedRows(result *Result, rowMaps []RowMap, s *sql.SelectStmt, orderBy []sql.OrderByTerm) bool {
	if idxName, backward, ok := e.indexOrderedScanForOrderBy(s, orderBy); ok &&
		e.emitRowsInIndexOrder(result, rowMaps, s.From.Name, idxName, backward) {
		return true
	}
	if idxName, backward, ok := e.loopOrderedEmission(s); ok &&
		e.emitRowsInIndexOrder(result, rowMaps, s.From.Name, idxName, backward) {
		return true
	}
	return false
}
