// Package exec implements query execution.
//
// This file holds best-index selection for a single table's WHERE clause:
// the seekable-constraint prefix rule (where.c whereLoopAddBtreeIndex),
// candidate scoring, and the SEARCH constraint list rendered by EQP.
package execquery

import (
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// bestIndexForQuery examines the WHERE clause and returns the best index name,
// estimated row count, and formatted column conditions for the plan output.
func (e *SelectEngine) bestIndexForQuery(tableName string, where sql.Expr, estimate *float64) (string, string) {
	// Collect all column references with their operators
	refs := collectIndexedRefs(where, tableName, e)
	if len(refs) == 0 {
		return "", ""
	}
	// where.c builds an index seek from the index's LEADING columns only:
	// whereScanInit scans WHERE terms for index column nEq, equality terms
	// extend the prefix (the recursive nEq++ call), and one range term at the
	// next position ends it. A constraint on a column with an unconstrained
	// leading prefix cannot drive the index at all — index-14.3 "WHERE b=''"
	// over index t6i1(a,b) plans "SCAN t6", not a "(b=?)"-driven SEARCH
	// (skip-scan needs stat1 evidence; skipscan.go owns that path).
	refs = e.seekableRefs(refs, tableName)
	if len(refs) == 0 {
		return "", ""
	}
	// Pick the one with the lowest estimate
	bestName := ""
	bestEst := *estimate
	var bestRefs []indexedRef // all refs matching the best index
	for _, ref := range refs {
		est := refEstimate(ref, e.tableRowCount(tableName))
		if est < bestEst {
			bestEst = est
			bestName = ref.indexName
		} else if est == bestEst && ref.indexName != bestName {
			bestName = e.tiebreakIndex(refs, bestName, ref.indexName)
		}
	}
	// Collect all refs for the best index to build conditions. Only
	// column-to-constant predicates ON THE CHOSEN INDEX'S COLUMNS are listed:
	// SQLite's explainIndexRange renders one constraint per leading index
	// column satisfied by the query, so a constraint on a column outside the
	// index never appears (analyze7-2.3 "SEARCH t1 USING INDEX t1a (a=?)" for
	// "WHERE a=123 AND b=123" — b is not in t1a and is not listed).
	if bestName != "" {
		bestRefs = e.refsForBestIndex(refs, where, tableName, bestName)
	}
	*estimate = bestEst
	return bestName, formatConditions(bestRefs)
}

// seekableRefs filters refs to those that can drive an index seek under the
// leading-prefix rule: for each candidate index, only refs on columns before
// the seek prefix's end survive (index-14.3). Refs on columns the position
// model cannot place (expression-index keys matched by findIndexOnExpr) are
// kept unchanged.
func (e *SelectEngine) seekableRefs(refs []indexedRef, tableName string) []indexedRef {
	type indexGroup struct {
		cols    []string
		refIdxs []int
	}
	groups := map[string]*indexGroup{}
	var order []string
	for i, ref := range refs {
		g, ok := groups[ref.indexName]
		if !ok {
			g = &indexGroup{cols: e.seekRefColumns(tableName, ref.indexName)}
			groups[ref.indexName] = g
			order = append(order, ref.indexName)
		}
		g.refIdxs = append(g.refIdxs, i)
	}
	var out []indexedRef
	for _, name := range order {
		g := groups[name]
		end, ok := seekPrefixEnd(g.cols, refsAt(g.refIdxs, refs))
		for _, i := range g.refIdxs {
			if pos, known := refColumnPos(g.cols, refs[i]); ok && known && pos >= end {
				continue // filter term, not a search constraint
			}
			out = append(out, refs[i])
		}
	}
	return out
}

// seekRefColumns returns the ordered key columns of a planner index token.
func (e *SelectEngine) seekRefColumns(tableName, idx string) []string {
	if idx == "PRIMARY KEY" {
		return e.withoutRowidPKCols(tableName)
	}
	return e.indexColumns(idx)
}

// refsAt gathers the refs named by idxs, in idxs order.
func refsAt(idxs []int, refs []indexedRef) []indexedRef {
	out := make([]indexedRef, len(idxs))
	for k, i := range idxs {
		out[k] = refs[i]
	}
	return out
}

// refColumnPos returns the position of the ref's column in the index's key
// columns. known is false for refs the position model cannot place
// (expression-index keys).
func refColumnPos(cols []string, ref indexedRef) (int, bool) {
	pos := indexColPos(cols, ref.colName)
	if pos >= len(cols) {
		return 0, false
	}
	return pos, true
}

// seekPrefixEnd returns the length of the leading-columns prefix the refs can
// drive: the number of leading columns each bound by an equality ref, plus
// one when the next position carries a range-class ref (where.c: equality
// extends the prefix; a single range term at position nEq ends it). ok is
// false when cols is empty (unknown column list — callers keep the refs
// unchanged).
func seekPrefixEnd(cols []string, idxRefs []indexedRef) (int, bool) {
	if len(cols) == 0 {
		return 0, false
	}
	end := 0
	for end < len(cols) {
		if hasSeekRefAt(cols, idxRefs, end, isEqualitySeekOp) {
			end++
			continue
		}
		if hasSeekRefAt(cols, idxRefs, end, isRangeSeekOp) {
			end++
		}
		break
	}
	return end, true
}

// hasSeekRefAt reports whether a ref sits on index position pos with an
// operator accepted by classify.
func hasSeekRefAt(cols []string, idxRefs []indexedRef, pos int, classify func(string) bool) bool {
	for _, ref := range idxRefs {
		if p, known := refColumnPos(cols, ref); known && p == pos && classify(ref.op) {
			return true
		}
	}
	return false
}

// isEqualitySeekOp reports whether op extends an index seek prefix
// (where.c WO_EQ; WO_IN list expansion is not modeled as a ref here).
func isEqualitySeekOp(op string) bool {
	return op == "=" || op == "=="
}

// isRangeSeekOp reports whether op is a range constraint class at the seek
// prefix's end (where.c WO_GT|WO_GE|WO_LT|WO_LE and the LIKE/GLOB-optimization
// / BETWEEN bounds derived from them).
func isRangeSeekOp(op string) bool {
	switch op {
	case "<", "<=", ">", ">=", "LIKE", "GLOB", "BETWEEN":
		return true
	}
	return false
}

// refEstimate computes the estimated row count for an indexed ref: its
// pre-computed selectivity when present, else the operator heuristic, times
// the table's row count.
func refEstimate(ref indexedRef, rowCount int64) float64 {
	sel := ref.selectivity
	if sel <= 0 {
		sel = estimateSelectivity(ref.constant, ref.op)
	}
	return sel * float64(rowCount)
}

// tiebreakIndex picks between two equally-estimated indexes: the one covering
// more WHERE conditions, then the simpler one (fewer columns). Returns the
// winning index name.
func (e *SelectEngine) tiebreakIndex(refs []indexedRef, bestName, candidateName string) string {
	covCur := e.countRefsForIndex(refs, bestName)
	covNew := e.countRefsForIndex(refs, candidateName)
	if covNew > covCur {
		return candidateName
	}
	if covNew == covCur && e.ctx.IndexColumnCount(indexSchemaName(candidateName)) < e.ctx.IndexColumnCount(indexSchemaName(bestName)) {
		return candidateName
	}
	return bestName
}

// refsForBestIndex returns every indexed ref matching the best index, plus
// column-to-constant predicates on the index's own columns so the plan lists
// the full set of search constraints for that index. For a WITHOUT ROWID
// PRIMARY KEY search, only PRIMARY KEY columns are listed: SQLite's plan for
// a PK lookup shows exactly the PK constraints, not unrelated WHERE
// predicates (see without_rowid1 14.2).
func (e *SelectEngine) refsForBestIndex(refs []indexedRef, where sql.Expr, tableName, bestName string) []indexedRef {
	var bestRefs []indexedRef
	for _, ref := range refs {
		if ref.indexName == bestName {
			bestRefs = append(bestRefs, ref)
		}
	}
	if bestName == "PRIMARY KEY" {
		return bestRefs
	}
	indexCols := e.indexColumns(bestName)
	end, ok := seekPrefixEnd(indexCols, bestRefs)
	for _, ar := range collectAllColumnRefs(where, tableName) {
		if !containsFold(indexCols, ar.colName) {
			continue
		}
		// wherecode.c renders the loop's LTerms — seek constraints only. A
		// predicate on a non-prefix index column is a filter, never listed
		// (index-14.3's sibling "WHERE a=1 AND b=2" over (a,b) with a range
		// a-term lists "(a>?)" alone).
		if pos, known := refColumnPos(indexCols, ar); ok && known && pos >= end {
			continue
		}
		if !bestRefsContain(bestRefs, ar) {
			bestRefs = append(bestRefs, ar)
		}
	}
	// explainIndexRange (wherecode.c) walks the index's columns in INDEX
	// order — a WHERE "a=? AND b=?" on index (b,a) renders "(b=? AND a=?)"
	// (e_fkey-26.4). Sort the refs into index column order, keeping the
	// relative order of same-column refs (multi-operator terms).
	sort.SliceStable(bestRefs, func(i, j int) bool {
		return indexColPos(indexCols, bestRefs[i].colName) < indexColPos(indexCols, bestRefs[j].colName)
	})
	return bestRefs
}

// indexColPos returns the position of colName in indexCols, or len(indexCols)
// when absent (unknown columns sort last; callers pre-filter to index cols).
func indexColPos(indexCols []string, colName string) int {
	for i, c := range indexCols {
		if strings.EqualFold(c, colName) {
			return i
		}
	}
	return len(indexCols)
}

// bestRefsContain reports whether bestRefs already has a ref with the same
// column and operator as ar.
func bestRefsContain(bestRefs []indexedRef, ar indexedRef) bool {
	for _, br := range bestRefs {
		if br.colName == ar.colName && br.op == ar.op {
			return true
		}
	}
	return false
}
