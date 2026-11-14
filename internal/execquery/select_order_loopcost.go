// Package exec implements query execution.
//
// This file prices the ORDER-BY-serving index candidates the way where.c
// does, so the superset candidate wins: whereLoopAddBtreeIndex prices one
// loop per (index, nEq equality prefix + one range bound) with the default
// aiRowLogEst ladder (build.c sqlite3DefaultRowEst) or the sqlite_stat1
// tokens, and whereLoopInsert's subset-cost adjustment
// (whereLoopCheaperProperSubset / whereLoopAdjustCost / whereLoopFindLesser)
// forces every loop whose constraint set is a proper superset of another
// loop's strictly cheaper (rRun=min, nOut=min(existing-1)) and every proper
// subset strictly costlier. whereH pins exactly that choice: with indexes
// t1abc(a,b,c) and t1bc(b,c), `WHERE a=? AND b=? AND c>=? ORDER BY c` plans
// t1abc even though t1bc delivers the order too — the t1bc loop's
// constraint set is a proper subset of the t1abc loop's, and the raw ladder
// arithmetic ties at nOut 12, so the subset rule breaks the tie.
package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// pricedLoop is one candidate index seek loop with its where.c costs.
type pricedLoop struct {
	token    string // planner index token
	rRun     int    // LogEst run cost
	nOut     int    // LogEst output row estimate (may go negative)
	terms    []int  // consumed conjunct indexes of the AND split
	covering bool   // WHERE_IDX_ONLY: the index carries every query column
	chosen   bool   // true for the loop the seek already selected
}

// defaultRowEstLadder ports build.c sqlite3DefaultRowEst's aiRowLogEst
// values: a[1]=33 (10 rows), a[2]=32 (9), a[3]=30 (8), a[4]=28 (7), a[5]=26
// (6), then 23 (5) onward; a[0] is the table's nRowLogEst clamped to 99
// (LogEst(1000)); a unique index's last entry is 0 (one row).
func defaultRowEstLadder(nKeyCol int, tableLogEst int, unique bool) []int {
	aVal := []int{33, 32, 30, 28, 26}
	ladder := make([]int, nKeyCol+1)
	x := tableLogEst
	if x < 99 {
		x = 99
	}
	ladder[0] = x
	nCopy := len(aVal)
	if nKeyCol < nCopy {
		nCopy = nKeyCol
	}
	copy(ladder[1:], aVal[:nCopy])
	for i := nCopy + 1; i <= nKeyCol; i++ {
		ladder[i] = 23
	}
	if unique {
		ladder[nKeyCol] = 0
	}
	return ladder
}

// indexRowEstLadder returns the index's aiRowLogEst: the sqlite_stat1
// tokens through the LogEst when the index has a stat1 row (analyze.c
// decodeIntArray), else the default ladder over the table's row estimate
// (200 = LogEst(1048576) without any stat1, else the stat1 table count).
func (e *SelectEngine) indexRowEstLadder(tableName, idxName string) []int {
	cols := e.seekRefColumns(tableName, idxName)
	if len(cols) == 0 {
		return nil
	}
	if tokens := e.stat1Tokens(idxName); len(tokens) > 0 {
		ladder := make([]int, len(cols)+1)
		for i := 0; i <= len(cols) && i < len(tokens); i++ {
			if tokens[i] < 0 {
				tokens[i] = 0
			}
			ladder[i] = sqlite3LogEst(uint64(tokens[i]))
		}
		return ladder
	}
	tableLogEst := 200 // LogEst(1048576): build.c's default nRowLogEst
	if n := e.stat1RowCount(tableName); n > 0 {
		tableLogEst = sqlite3LogEst(uint64(n))
	}
	unique := e.indexIsUnique(idxName)
	return defaultRowEstLadder(len(cols), tableLogEst, unique)
}

// eqConjunctIndexOf returns the index (into conjuncts) of the first
// conjunct that fixes col by equality with agreeing collation, or -1.
func (e *SelectEngine) eqConjunctIndexOf(t queryTable, conjuncts []sql.Expr, col string) int {
	for i, conj := range conjuncts {
		if e.termEqBoundByConjunct(t, conj, &orderTerm{col: col}) {
			return i
		}
	}
	return -1
}

// rangeBoundsOf classifies one conjunct's seekable range bounds on the named
// column: btm (a lower bound) and top (an upper bound). BETWEEN counts both.
// Only seekable operands (constants/parameters — what the seek executor can
// drive) are counted; a non-constant bound stays residual, unlike C which
// prices every range term. LIKE/GLOB count a lower bound only (C's
// LIKEOPT pair is priced through its own est path).
func rangeBoundsOf(t queryTable, conj sql.Expr, col string) (btm, top bool, ok bool) {
	switch v := unwrapParenExpr(conj).(type) {
	case *sql.Between:
		ref, isRef := v.Operand.(*sql.ColumnRef)
		if !isRef || !termColumnMatches(ref, t, col) {
			return false, false, false
		}
		if isSeekConstraintOperand(v.Low) && isSeekConstraintOperand(v.High) {
			return true, true, true
		}
		return false, false, false
	case *sql.BinaryOp:
		ref, isRef := v.Left.(*sql.ColumnRef)
		if !isRef || !termColumnMatches(ref, t, col) || !isSeekConstraintOperand(v.Right) {
			return false, false, false
		}
		switch v.Operator {
		case ">", ">=", "LIKE", "GLOB":
			return true, false, true
		case "<", "<=":
			return false, true, true
		}
	}
	return false, false, false
}

// priceOrderLoop prices one candidate index seek loop for the single-table
// ORDER BY decision. It mirrors whereLoopAddBtreeIndex: nOut starts at
// ladder[nEq] (the equality prefix), one range bound at position nEq runs
// whereRangeScanEst's arithmetic (−20 per bound with the default truthProb
// of 1, −20 more for both bounds, −1 per bound, floor 10); rRun is priced
// from the PRE-adjustment nOut exactly where C prices it; then
// whereLoopOutputAdjust (single table WITH an ORDER BY — C's early-out
// otherwise) subtracts one per unconsumed conjunct with the iReduce cap
// (10 for an unused `col = small int`, else 20) against ladder[0]. terms
// carries the consumed conjunct indexes for the subset relation. ok is
// false when the token is not a seekable secondary-index candidate.
func (e *SelectEngine) priceOrderLoop(t queryTable, s *sql.SelectStmt, token string, conjuncts []sql.Expr) (pricedLoop, bool) {
	cols := e.seekRefColumns(t.real, token)
	if len(cols) == 0 {
		return pricedLoop{}, false
	}
	lp := pricedLoop{token: token}
	// The equality prefix and its consumed conjuncts (whereScanInit's
	// leading-columns rule; one equality conjunct per position).
	for pos := 0; pos < len(cols); pos++ {
		ci := e.eqConjunctIndexOf(t, conjuncts, cols[pos])
		if ci < 0 {
			break
		}
		lp.terms = append(lp.terms, ci)
	}
	if len(lp.terms) == 0 {
		return pricedLoop{}, false // not a seek: no equality-bound leading column
	}
	ladder := e.indexRowEstLadder(t.real, token)
	if len(ladder) <= len(lp.terms) {
		return pricedLoop{}, false
	}
	nOut := e.applyOrderLoopRangeBound(t, conjuncts, cols, &lp, ladder[len(lp.terms)])
	lp.rRun = e.orderLoopRunCost(t, s, token, ladder, nOut, &lp)
	lp.nOut = outputAdjustNOut(conjuncts, &lp, ladder[0], nOut)
	return lp, true
}

// applyOrderLoopRangeBound consumes the one range bound at the prefix's end
// (where.c: the range term at position nEq ends the seek prefix) and returns
// the bound-adjusted nOut — whereRangeScanEst without stat4. The consumed
// conjuncts join lp.terms.
func (e *SelectEngine) applyOrderLoopRangeBound(t queryTable, conjuncts []sql.Expr, cols []string, lp *pricedLoop, nOut int) int {
	nEq := len(lp.terms)
	if nEq >= len(cols) {
		return nOut
	}
	btmCi, topCi := rangeConjunctBounds(t, conjuncts, cols[nEq])
	nBnd := 0
	if btmCi >= 0 {
		lp.terms = append(lp.terms, btmCi)
		nBnd++
	}
	if topCi >= 0 {
		lp.terms = append(lp.terms, topCi)
		nBnd++
	}
	if nBnd == 0 {
		return nOut
	}
	// whereRangeScanEst without stat4: whereRangeAdjust −20 per bound
	// (truthProb defaults to 1), −20 more for both bounds.
	nNew := nOut - 20*nBnd
	if btmCi >= 0 && topCi >= 0 {
		nNew -= 20
	}
	if nNew < 10 {
		nNew = 10
	}
	if adj := nOut - nBnd; adj < nNew {
		nNew = adj
	}
	return nNew
}

// rangeConjunctBounds returns the conjunct indexes of the first lower- and
// upper-bound range constraints on the named column.
func rangeConjunctBounds(t queryTable, conjuncts []sql.Expr, col string) (btmCi, topCi int) {
	btmCi, topCi = -1, -1
	for i, conj := range conjuncts {
		btm, top, ok := rangeBoundsOf(t, conj, col)
		if !ok {
			continue
		}
		if btm && btmCi < 0 {
			btmCi = i
		}
		if top && topCi < 0 {
			topCi = i
		}
	}
	return btmCi, topCi
}

// orderLoopRunCost prices the loop's rRun the way whereLoopAddBtreeIndex
// does: rCostIdx = LogEstAdd(estLog(ladder[0]), nOut+1+(15*szIdxRow)/szTabRow),
// plus the table-lookup term (nOut+16) when the index does not cover the
// query. nOut is the PRE-output-adjustment estimate — C prices rRun before
// calling whereLoopOutputAdjust.
func (e *SelectEngine) orderLoopRunCost(t queryTable, s *sql.SelectStmt, token string, ladder []int, nOut int, lp *pricedLoop) int {
	szIdx := e.indexRowWidthLogEst(t.real, token)
	szTab := e.tableRowWidthLogEst(t.real)
	if szTab <= 0 {
		szTab = 1
	}
	lp.covering = e.indexCoversQuery(t, s, token)
	rCostIdx := logEstAdd(estLog(ladder[0]), nOut+1+(15*szIdx)/szTab)
	if lp.covering {
		return rCostIdx
	}
	// Not WHERE_IDX_ONLY: every surviving row pays a table lookup.
	return logEstAdd(rCostIdx, nOut+16)
}

// outputAdjustNOut applies whereLoopOutputAdjust to a priced loop: single
// table and an ORDER BY present (the caller guarantees both), so one per
// unconsumed conjunct; unused plain equalities set the iReduce cap (10 for
// an integer literal in [-1,1], else 20) against the index's row count.
func outputAdjustNOut(conjuncts []sql.Expr, lp *pricedLoop, ladderTop int, nOut int) int {
	if len(conjuncts) <= len(lp.terms) {
		return nOut
	}
	iReduce := 0
	for i, conj := range conjuncts {
		if lp.consumes(i) {
			continue
		}
		nOut--
		if k := unusedEqIReduce(conj); k > iReduce {
			iReduce = k
		}
	}
	if capVal := ladderTop - iReduce; nOut > capVal {
		nOut = capVal
	}
	return nOut
}

// consumes reports whether conjunct i is part of the loop's constraint set.
func (lp *pricedLoop) consumes(i int) bool {
	for _, t := range lp.terms {
		if t == i {
			return true
		}
	}
	return false
}

// unusedEqIReduce mirrors whereLoopOutputAdjust's heuristic 2: an unused
// `col = <literal>` conjunct caps the output estimate at 1/4 of the table
// (iReduce 20), or 1/2 when the literal is an integer in [-1,1] (iReduce
// 10); non-equality conjuncts contribute nothing to the cap.
func unusedEqIReduce(conj sql.Expr) int {
	bin, ok := unwrapParenExpr(conj).(*sql.BinaryOp)
	if !ok {
		return 0
	}
	if bin.Operator != "=" && bin.Operator != "==" {
		return 0
	}
	lit := numericOperand(bin.Right)
	if lit == "" {
		lit = numericOperand(bin.Left)
	}
	if lit == "" {
		return 20
	}
	if v, err := strconv.ParseInt(lit, 10, 64); err == nil && v >= -1 && v <= 1 {
		return 10
	}
	return 20
}

// numericOperand returns the literal text of a numeric leaf (a plain
// NumericLit or a negated one), or "" when the operand is not one.
func numericOperand(x sql.Expr) string {
	switch v := x.(type) {
	case *sql.NumericLit:
		return strings.TrimSpace(v.Value)
	case *sql.UnaryOp:
		if inner, isNum := v.Operand.(*sql.NumericLit); isNum && (v.Operator == "-" || v.Operator == "+") {
			return v.Operator + strings.TrimSpace(inner.Value)
		}
	}
	return ""
}

// properLoopSubset ports whereLoopCheaperProperSubset's case 2: X's
// constraint set uses strictly fewer conjuncts than Y's and every one of
// X's conjuncts is also used by Y (terms are conjunct indexes, so the same
// WHERE term serving two loops is shared identity, like C's WhereTerm
// pointers), with case 2e's guard: a covering X does not force a
// non-covering Y. Case 1 (same index, shorter nEq) is subsumed — both
// loops on one index have the same covering status and the shallower
// prefix's conjuncts are a strict subset of the deeper loop's.
func properLoopSubset(x, y *pricedLoop) bool {
	if len(x.terms) >= len(y.terms) {
		return false
	}
	for _, xt := range x.terms {
		found := false
		for _, yt := range y.terms {
			if yt == xt {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if x.covering && !y.covering {
		return false
	}
	return true
}

// adjustLoopCosts runs whereLoopAdjustCost: the template is forced strictly
// cheaper than every existing loop it is a proper superset of, and strictly
// costlier than every existing loop it is a proper subset of.
func adjustLoopCosts(list []pricedLoop, tpl *pricedLoop) {
	for i := range list {
		p := &list[i]
		if properLoopSubset(p, tpl) {
			forceLoopCheaper(p, tpl)
		} else if properLoopSubset(tpl, p) {
			forceLoopCostlier(p, tpl)
		}
	}
}

// forceLoopCheaper is whereLoopAdjustCost's downward branch: the superset
// template undercuts its subset on both rRun (min) and nOut (min(existing-1)
// — strictly cheaper).
func forceLoopCheaper(p, tpl *pricedLoop) {
	if p.rRun < tpl.rRun {
		tpl.rRun = p.rRun
	}
	if p.nOut-1 < tpl.nOut {
		tpl.nOut = p.nOut - 1
	}
}

// forceLoopCostlier is whereLoopAdjustCost's upward branch: the subset
// template is priced above its superset on both rRun (max) and nOut
// (max(existing+1) — strictly costlier).
func forceLoopCostlier(p, tpl *pricedLoop) {
	if p.rRun > tpl.rRun {
		tpl.rRun = p.rRun
	}
	if p.nOut+1 > tpl.nOut {
		tpl.nOut = p.nOut + 1
	}
}

// adjustLoopSubset runs whereLoopInsert's entry protocol for one template
// against the list built so far (in C's probe order): the subset-cost
// adjustment, then whereLoopFindLesser's discard when an existing loop
// Pareto-dominates the template. Returns the (possibly extended) list;
// kept is false when the template was discarded.
func adjustLoopSubset(list []pricedLoop, tpl pricedLoop) ([]pricedLoop, bool) {
	adjustLoopCosts(list, &tpl)
	for i := range list {
		if list[i].rRun <= tpl.rRun && list[i].nOut <= tpl.nOut {
			return list, false // an existing loop is at least as good: discard
		}
	}
	return append(list, tpl), true
}

// elevateSupersetLoop is the whereH rule: the chosen seek already delivers
// the ORDER BY, but C still prices every candidate and the solver picks
// the cheapest — a proper-superset index loop (one that consumes MORE of
// the WHERE clause's terms on an earlier index position) undercuts it on
// both the row estimate and the run cost. Price the chosen and every
// ordered secondary-index candidate in C's probe order (the index chain:
// most recently created first), run the subset adjustment, and swap when
// a delivering candidate strictly undercuts the chosen on (rRun, nOut).
func (e *SelectEngine) elevateSupersetLoop(t queryTable, s *sql.SelectStmt, loop scanLoop, wantConditions bool) scanLoop {
	if s.Where == nil || len(e.indexColumns(loop.token)) == 0 {
		return loop // not a named secondary index (e.g. a PRIMARY KEY token)
	}
	conjuncts := splitAnd(s.Where)
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return loop
	}
	list, ok := e.priceChosenLoop(t, s, loop, conjuncts)
	if !ok {
		return loop // the chosen loop is not priceable here; keep it
	}
	list = e.priceOrderServingCandidates(t, s, loop.token, conjuncts, entries, list)
	best := cheapestOtherLoop(list)
	if best < 0 {
		return loop
	}
	cand := scanLoop{kind: loopIndex, token: list[best].token, seek: true,
		bestEst: loop.bestEst, nRow: loop.nRow}
	if wantConditions {
		cand.conditions = e.eqPrefixConditions(t.real, list[best].token, s.Where)
	} else {
		cand.conditions = loop.conditions
	}
	return cand
}

// priceChosenLoop prices the chosen seek and seeds the candidate list with
// it; ok is false when the chosen loop cannot be priced as a seekable
// secondary-index loop that delivers the ORDER BY.
func (e *SelectEngine) priceChosenLoop(t queryTable, s *sql.SelectStmt, loop scanLoop, conjuncts []sql.Expr) ([]pricedLoop, bool) {
	lp, ok := e.priceOrderLoop(t, s, loop.token, conjuncts)
	if !ok {
		return nil, false
	}
	if _, sat := e.indexLoopOrderSatisfied(t, loop, s); !sat {
		return nil, false
	}
	lp.chosen = true
	return []pricedLoop{lp}, true
}

// priceOrderServingCandidates prices every ordered secondary index of the
// table (C's probe order — the prepend chain, most recently created first;
// GetEntries serves creation order) and folds the ORDER-BY-delivering ones
// through the subset adjustment.
func (e *SelectEngine) priceOrderServingCandidates(t queryTable, s *sql.SelectStmt, chosenToken string, conjuncts []sql.Expr, entries []*schema.Entry, list []pricedLoop) []pricedLoop {
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Type != "index" || entry.TblName != t.display || e.indexIsUnordered(entry.Name) {
			continue
		}
		token := indexLookupToken(entry.Name)
		if token == chosenToken {
			continue
		}
		lp, ok := e.priceOrderLoop(t, s, token, conjuncts)
		if !ok {
			continue
		}
		candLoop := scanLoop{kind: loopIndex, token: token, seek: true}
		if _, sat := e.indexLoopOrderSatisfied(t, candLoop, s); !sat {
			continue // C's solver prices the sorter into this one; skip
		}
		list, _ = adjustLoopSubset(list, lp)
	}
	return list
}

// cheapestOtherLoop returns the list index of the cheapest non-chosen loop:
// wherePathSolver's comparison — rCost (the delivering loops' rUnsort =
// rRun) first, then nRow — or -1 when no challenger exists.
func cheapestOtherLoop(list []pricedLoop) int {
	best := -1
	for i := range list {
		if list[i].chosen {
			continue
		}
		if best < 0 || list[i].rRun < list[best].rRun ||
			(list[i].rRun == list[best].rRun && list[i].nOut < list[best].nOut) {
			best = i
		}
	}
	return best
}
