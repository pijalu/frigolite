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

	"github.com/pijalu/frigolite/internal/schema"

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
	// vtabOrderConsumed marks the vtab scan whose ORDER BY the module
	// consumed (orderByConsumed): the rows arrive in the consumed order, so
	// the ORDER BY/DISTINCT temp nodes the consumption satisfies are
	// suppressed (where.c orderByConsumed parity).
	vtabOrderConsumed bool
}

// scanLoopForQuery computes the single-table scan loop: the skip-scan, INTEGER
// PRIMARY KEY seek, selective secondary-index seek, or full index walk that
// planSingleTable renders (mirroring its decision order exactly — where.c
// computes the loop once and both the plan and the ORDER BY analysis consume
// it).
func (e *SelectEngine) scanLoopForQuery(t queryTable, s *sql.SelectStmt, wantConditions bool) scanLoop {
	nRow := e.tableRowCount(t.display)
	if nRow == 0 {
		nRow = defaultPlanRowCount
	}
	loop, _ := e.scanLoopForRowCount(t, s, nRow, wantConditions)
	return loop
}

const defaultPlanRowCount = 1000000

func (e *SelectEngine) scanLoopForRowCount(t queryTable, s *sql.SelectStmt, nRow int64, wantConditions bool) (scanLoop, error) {
	loop := scanLoop{bestEst: float64(nRow), nRow: float64(nRow)}
	if s.Where != nil {
		var err error
		_, _, loop, err = e.whereScanLoop(t, s, loop, nRow, wantConditions)
		if err != nil {
			return scanLoop{}, err
		}
		if loop.kind != loopNone {
			// An ORDER BY led by the rowid is delivered for free by the table
			// scan; a secondary-index loop would need the temp-b-tree sort,
			// which where.c prices into every non-delivering loop — the scan
			// always wins (the IPK loop keeps rowid order itself).
			if (loop.kind == loopIndex || loop.kind == loopSkipScan) && e.orderByRowidLeadingName(s) {
				return scanLoop{kind: loopNone, bestEst: float64(nRow), nRow: float64(nRow)}, nil
			}
			// where.c's wherePathSolver: an ORDER-BY-serving covering index
			// walk is a candidate loop, and the seek path that does not
			// deliver the order pays the sorter — the walk wins whenever its
			// run undercuts the seek+sorter path (where-6.7.x: WHERE b>0
			// ORDER BY a over the analyzed t3 plans the covering t3acb walk,
			// not the t3bc seek plus a temp b-tree).
			if loop.kind == loopIndex && loop.seek && len(s.OrderBy) > 0 &&
				s.Union == nil && len(s.Joins) == 0 && len(s.GroupBy) == 0 && !s.Distinct {
				if _, sat := e.indexLoopOrderSatisfied(t, loop, s); !sat {
					if walk, ok := e.coveringOrderIndexToken(t, s); ok &&
						e.orderServingWalkCheaper(t, s, loop, walk) {
						return scanLoop{kind: loopIndex, token: walk,
							bestEst: loop.bestEst, nRow: loop.nRow}, nil
					}
				}
			}
			return loop, nil
		}
	}
	if loop.kind == loopNone {
		// No seek survived its cost bar (a declined seek included —
		// bestIndex != "" but the loop never materialized): the ORDER-BY-
		// serving index walk is the remaining candidate (where.c prices the
		// sorter into every non-delivering loop, so a covering walk that
		// delivers the order wins over the scan+sort shape).
		e.scanLoopForOrderIndex(t, s, &loop)
	}
	return loop, nil
}

// whereScanLoop resolves the WHERE-driven loop: a skip-scan, an INTEGER
// PRIMARY KEY seek, or a selective secondary-index seek (the seek threshold:
// without a sqlite_stat1 row SQLite's default cost model prices an index
// range seek at one tenth of a full scan, so the seek always wins — intpkey-2.5).
// Returns the best-index rendering inputs alongside; a loopNone result falls
// through to the ORDER BY / GROUP BY index walks. rowCount is the loop's
// planning row count (the caller's scale).
func (e *SelectEngine) whereScanLoop(t queryTable, s *sql.SelectStmt, loop scanLoop, rowCount int64, wantConditions bool) (string, string, scanLoop, error) {
	// The schema lookups resolve the REAL table name: an aliased FROM term
	// (t2 AS inner) renders as the alias but its indexes live under the
	// base name (where8-3.15's correlated subquery plans the i3 seek).
	ret := loop
	bestIndex, conditions, err := e.bestIndexForRowCount(t.real, s.Where, &ret.bestEst, rowCount, wantConditions)
	if err != nil {
		return "", "", ret, err
	}
	if ss := e.trySkipScanPlan(t.display, s.Where, ret.bestEst); ss != nil {
		ret.kind, ret.token, ret.seek, ret.conditions = loopSkipScan, ss.indexName, true, ss.conditions
		return bestIndex, conditions, ret, nil
	}
	if detail, eq, ok := e.rowidSeekPlanDetail(t, s); ok {
		ret.kind, ret.oneRow, ret.conditions = loopIPK, eq, detail
		return bestIndex, conditions, ret, nil
	}
	if bestIndex != "" {
		ret.kind, ret.token, ret.seek, ret.conditions = loopIndex, bestIndex, true, conditions
		if e.indexSeekChosen(t, s, bestIndex, ret.bestEst, ret.nRow) {
			return bestIndex, conditions, e.preferOrderByServingIndex(t, s, ret, wantConditions), nil
		}
		// The seek failed its cost bar: back to the plain-scan loop (the
		// ORDER-BY-serving walk remains a candidate in
		// scanLoopForRowCount's loopNone fallback).
		ret = loop
	}
	return bestIndex, conditions, ret, nil
}

// indexEqPrefixLen counts how many of an index's LEADING columns the WHERE
// clause fixes by equality — index-independent (collectIndexedRefs only
// carries refs for the term's best index, so a candidate index's prefix
// cannot be counted through it). A range-bound trailing column does not
// count (its rows still emerge in index order).
func (e *SelectEngine) indexEqPrefixLen(t queryTable, token string, where sql.Expr) int {
	cols := e.seekRefColumns(t.real, token)
	if len(cols) == 0 || where == nil {
		return 0
	}
	n := 0
	for n < len(cols) {
		tr := &orderTerm{col: cols[n]}
		if !e.termEqBoundByWhere(t, where, tr) {
			break
		}
		n++
	}
	return n
}

// preferOrderByServingIndex swaps a chosen secondary-index seek for another
// candidate index that serves the same equality prefix AND satisfies the
// ORDER BY — where.c's sort-cost rule: every loop that does not deliver the
// ORDER BY pays the temp-b-tree sort, so among loops of equal row estimate
// the one that delivers the order wins (where.test 6.9: `WHERE a=1 ORDER BY
// c` plans SEARCH via (a,c,b), not the cheaper-per-row (a) index followed by
// a sort). The candidate must drive a seek with at least as many leading
// equality-bound columns, which pins its selectivity to the chosen loop's.
func (e *SelectEngine) preferOrderByServingIndex(t queryTable, s *sql.SelectStmt, loop scanLoop, wantConditions bool) scanLoop {
	if s == nil || len(s.OrderBy) == 0 || s.Union != nil || len(s.Joins) > 0 ||
		len(s.GroupBy) > 0 || s.Distinct || s.Where == nil {
		return loop
	}
	if len(e.withoutRowidPKCols(t.real)) > 0 {
		return loop
	}
	if _, ok := e.indexLoopOrderSatisfied(t, loop, s); ok {
		// The chosen index already delivers the order — but C keeps
		// pricing every candidate and the solver picks the cheapest loop:
		// a proper-superset index loop (whereH: t1abc's (a=?,b=?,c>?) over
		// t1bc's (b=?,c>=?)) undercuts it on both the row estimate and the
		// run cost via whereLoopInsert's subset-cost adjustment. Swap when
		// one does; keep the chosen otherwise.
		return e.elevateSupersetLoop(t, s, loop, wantConditions)
	}
	nEqBest := e.indexEqPrefixLen(t, loop.token, s.Where)
	if nEqBest == 0 {
		return loop // tranche rule: only equality-pinned substitutions
	}
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return loop
	}
	if cand, ok := e.firstOrderServingSwap(t, s, loop, wantConditions, nEqBest, entries); ok {
		return cand
	}
	return loop
}

// firstOrderServingSwap scans the table's ordered indexes for the first
// candidate whose equality prefix is at least the chosen loop's and that
// delivers the ORDER BY (the pre-elevation tranche rule: the chosen loop
// does not deliver the order, and every loop that does not deliver it pays
// the temp-b-tree sort).
func (e *SelectEngine) firstOrderServingSwap(t queryTable, s *sql.SelectStmt, loop scanLoop, wantConditions bool, nEqBest int, entries []*schema.Entry) (scanLoop, bool) {
	for _, entry := range entries {
		if entry.Type != "index" || entry.TblName != t.display || e.indexIsUnordered(entry.Name) {
			continue
		}
		token := indexLookupToken(entry.Name)
		if token == loop.token || e.indexEqPrefixLen(t, token, s.Where) < nEqBest {
			continue
		}
		cand := scanLoop{kind: loopIndex, token: token, seek: true,
			bestEst: loop.bestEst, nRow: loop.nRow}
		if _, ok := e.indexLoopOrderSatisfied(t, cand, s); !ok {
			continue
		}
		if wantConditions {
			cand.conditions = e.eqPrefixConditions(t.real, token, s.Where)
		} else {
			cand.conditions = loop.conditions
		}
		return cand, true
	}
	return scanLoop{}, false
}

// eqPrefixConditions renders the SEARCH constraint text of an index's
// equality-bound leading columns plus one range term on the next column
// ("(a=?)", "(a=? AND c>?)"), the way the refs-driven formatConditions does
// for the chosen index. A candidate index has no refs in
// collectIndexedRefs's pool, so its text is rebuilt from the WHERE conjuncts.
func (e *SelectEngine) eqPrefixConditions(tableName, token string, where sql.Expr) string {
	cols := e.seekRefColumns(tableName, token)
	t := queryTable{real: tableName, display: tableName}
	var parts []string
	n := 0
	for n < len(cols) {
		if !e.termEqBoundByWhere(t, where, &orderTerm{col: cols[n]}) {
			break
		}
		parts = append(parts, cols[n]+"=?")
		n++
	}
	if n < len(cols) {
		if part, ok := indexRangeConjunctText(splitAndInto(nil, where), cols[n]); ok {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// indexRangeConjunctText finds the first AND conjunct constraining the named
// column with a seekable range/equality operator and renders it as SQLite's
// SEARCH text does (BETWEEN expands to both bounds).
func indexRangeConjunctText(conjuncts []sql.Expr, col string) (string, bool) {
	for _, conj := range conjuncts {
		b, ok := unwrapParenExpr(conj).(*sql.BinaryOp)
		if !ok {
			continue
		}
		ref, isRef := b.Left.(*sql.ColumnRef)
		if !isRef || !strings.EqualFold(ref.Name, col) || !isSeekConstraintOperand(b.Right) {
			continue
		}
		switch b.Operator {
		case "=", "==":
			return col + "=?", true
		case ">", ">=", "<", "<=":
			return col + b.Operator + "?", true
		case "BETWEEN":
			return col + ">? AND " + col + "<?", true
		}
	}
	return "", false
}

// isSeekConstraintOperand reports whether an expression can serve as the
// constant side of a SEARCH constraint rendering (literals and parameters;
// anything else keeps the term out of the constraint text).
func isSeekConstraintOperand(x sql.Expr) bool {
	switch x.(type) {
	case *sql.NumericLit, *sql.StringLit, *sql.NullLit, *sql.ParameterExpr:
		return true
	}
	return false
}

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

// indexSeekChosen is where.c's seek-vs-scan cost comparison in the LogEst
// arithmetic whereLoopAddBtree prices loops with: the seek reads
// LogEstAdd(rLogSize, nOut + 1 + (15*szIdxRow)/szTabRow) rows' worth of
// b-tree work, plus the table-lookup term (nOut + 16) when the index does
// not cover the query; the scan costs rSize + 16. The seek wins when its
// run undercuts the scan. Without a sqlite_stat1 row the default cost
// model prices the seek at one tenth of a full scan, so it always wins.
// Evidence: SEARCH at ~12% over stat1 "100 8 1" (where-7.x) and at 50%
// over "1000 500 1"; the tie "1000 1000 1" and the stat4-informed wide
// ranges (analyzeE) stay scans.
func (e *SelectEngine) indexSeekChosen(t queryTable, s *sql.SelectStmt, bestIndex string, bestEstimate, nRow float64) bool {
	if len(e.stat1Tokens(bestIndex)) == 0 {
		return true
	}
	if nRow <= 0 {
		return false
	}
	est := bestEstimate
	if est < 1 {
		est = 1
	}
	if est >= nRow {
		return false
	}
	rSize := sqlite3LogEst(uint64(nRow))
	rOut := sqlite3LogEst(uint64(est))
	szTab := e.tableRowWidthLogEst(t.real)
	szIdx := e.indexRowWidthLogEst(t.real, bestIndex)
	if szTab <= 0 {
		szTab = 1
	}
	seekRun := logEstAdd(rSize, rOut+1+(15*szIdx)/szTab)
	if !e.indexCoversQuery(t, s, bestIndex) {
		// Not WHERE_IDX_ONLY: every surviving row pays a table lookup.
		seekRun = logEstAdd(seekRun, rOut+16)
	}
	return seekRun < rSize+16
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

// eqPrefixMayHoldDistinctNulls reports whether any equality-bound prefix
// column of the loop is fixed by an `X IS NULL` constraint while the column
// itself is nullable: UNIQUE-index semantics make each row's NULL key a
// distinct entry, so the loop can emit several rows sharing the visible key.
func (e *SelectEngine) eqPrefixMayHoldDistinctNulls(t queryTable, where sql.Expr, prefixCols []string) bool {
	if where == nil {
		return false
	}
	for _, col := range prefixCols {
		for _, conj := range splitAnd(where) {
			isNullConjunct := false
			if isn, ok := conj.(*sql.IsNull); ok {
				if ref, isRef := isn.Operand.(*sql.ColumnRef); isRef && termColumnMatches(ref, t, col) {
					isNullConjunct = true
				}
			} else if b, ok := conj.(*sql.BinaryOp); ok && (b.Operator == "IS" || b.Operator == "IS NOT") {
				// `c IS NULL` parses as IS over a NULL literal in some
				// shapes (the eq-bound pre-pass's IS handling treats it
				// as an equality).
				if ref, isRef := b.Left.(*sql.ColumnRef); isRef && termColumnMatches(ref, t, col) {
					if _, isNull := b.Right.(*sql.NullLit); isNull {
						isNullConjunct = true
					}
				}
			}
			if isNullConjunct && !e.columnNotNull(t.real, col) {
				return true
			}
		}
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
		nEq = e.indexEqPrefixLen(t, loop.token, s.Where)
	}
	backward, allSat := e.walkIndexOrderTerms(t, loop.token, idxCols, nEq, terms, s.Where)
	return backward, allSat
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
func (e *SelectEngine) walkIndexOrderTerms(t queryTable, token string, idxCols []string, nEq int, terms []orderTerm, where sql.Expr) (backward, allSat bool) {
	idxDescs, known := e.indexColumnDescFlags(t.real, token)
	if !known || len(idxDescs) < len(idxCols) {
		return false, false
	}
	ctx := &walkCtx{t: t, token: token, ipkAlias: e.ipkAliasColumn(t.real)}
	orderDistinct := e.indexIsUnique(token)
	if orderDistinct && nEq > 0 && e.eqPrefixMayHoldDistinctNulls(t, where, idxCols[:nEq]) {
		// A UNIQUE index treats NULL keys as distinct entries: an
		// `X IS NULL` equality over a nullable prefix column can match
		// several rows sharing the visible key, so the loop's output is
		// NOT provably duplicate-free (tkt3824 2.x: b=2 AND c IS NULL
		// over UNIQUE(b,c) yields two (2,NULL) rows).
		orderDistinct = false
	}
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
	idxColl := e.indexColumnCollation(t.real, token, col)
	return strings.EqualFold(termColl, idxColl)
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
	// Constant ORDER BY terms are satisfied without the loop's help (where.c
	// marks constant terms ordered in the obSat walk — where.c:5342); when
	// EVERY term is constant the sorter is never built (where2-2.6's
	// ORDER BY abs(5) vs 2.5's random(), oracle-verified).
	if e.allOrderTermsConstant(s.OrderBy, len(s.Columns)) {
		return true
	}
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
	case loopNone:
		// A plain table scan emits rowid order, so an ORDER BY led by the
		// rowid is consumed with no temp b-tree (where.c: the scan delivers
		// the ordering; the rowid's uniqueness retires the later terms).
		return e.orderByRowidLeadingName(s)
	}
	return e.sortCoveredByIndex(tables, s, orderByCols(s))
}








