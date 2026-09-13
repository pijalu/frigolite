package fts5

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// MATCH evaluation glue: whole-query evaluation to a rowid set, plus the
// per-row entry point the engine's MATCH expression path calls.

// matchCacheKey identifies one evaluated query (the table's index version
// invalidates it after any write).
type matchCacheKey struct {
	query   string
	col     string
	version uint64
}

// MatchRowids evaluates a MATCH query and returns the matching rowids. col
// restricts the match to one user column (-1 for the whole table).
func (t *Table) MatchRowids(query string, col int) (map[int64]bool, error) {
	if strings.HasPrefix(query, "*") {
		// A special query ('*reads'/'*id'): one row carrying the special
		// value as its rowid (fts5SpecialMatch).
		value, err := t.SpecialQueryValue(query)
		if err != nil {
			return nil, err
		}
		return map[int64]bool{value: true}, nil
	}
	node, err := parseQuery(t, query)
	if err != nil {
		return nil, err
	}
	if col >= 0 {
		node = applyColset(node, []int{col})
	}
	return node.eval(t)
}

// MatchQueryColumn evaluates a MATCH query for one document (the engine's
// expression evaluator calls this per row). column restricts the match to a
// user column when non-empty; rank/unknown names match the whole table
// (fts5's rank column carries no per-column restriction). langid is unused by
// fts5 (no languageid option); the variadic signature mirrors the FTS3 table
// method so both satisfy the evaluator's ftsMatchTable interface.
func (t *Table) MatchQueryColumn(rowid int64, query, column string, langid ...int64) (bool, error) {
	set, err := t.matchSet(query, column)
	if err != nil {
		return false, err
	}
	return set[rowid], nil
}

// matchSet evaluates a query to a rowid set with a one-entry memo: the
// generic scan pipeline evaluates the WHERE clause per row, so caching the
// last query's result keeps the whole-scan cost linear in documents.
func (t *Table) matchSet(query, column string) (map[int64]bool, error) {
	key := matchCacheKey{query: query, col: column, version: t.version}
	if t.cache != nil && t.cache.key == key {
		return t.cache.set, t.cache.err
	}
	col := -1
	if column != "" {
		col = t.ColumnIndex(column)
	}
	set, err := t.MatchRowids(query, col)
	t.cache = &matchCacheEntry{key: key, set: set, err: err}
	return set, err
}

// matchCacheEntry is the memoized result of one MATCH query.
type matchCacheEntry struct {
	key matchCacheKey
	set map[int64]bool
	err error
}

// bumpVersion invalidates the match cache after any index mutation.
func (t *Table) bumpVersion() { t.version++ }

// MatchUniverse evaluates the WHERE's top-level MATCH conjuncts against the
// index and returns the intersection of their rowid sets (the index-driven
// scan universe of fts5's xFilter). evalExpr resolves the MATCH right-hand
// sides (column-restricted by the left operand's user column when one is
// named). A nil result means no MATCH conjunct applies and the caller falls
// back to a full document scan.
func (t *Table) MatchUniverse(where sql.Expr, evalExpr func(sql.Expr) (interface{}, error)) (map[int64]bool, error) {
	var result map[int64]bool
	for _, conjunct := range topAndConjuncts(where) {
		bop, ok := conjunct.(*sql.BinaryOp)
		if !ok || bop.Operator != "MATCH" {
			continue
		}
		col, applies := matchConstraintColumn(bop.Left, t)
		if !applies {
			continue
		}
		qv, err := evalExpr(bop.Right)
		if err != nil {
			return nil, err
		}
		q, ok := util.UnwrapColumnValue(qv).(string)
		if !ok {
			continue
		}
		set, merr := t.MatchRowids(q, col)
		if merr != nil {
			return nil, merr
		}
		if result == nil {
			result = make(map[int64]bool, len(set))
			for rowid := range set {
				result[rowid] = true
			}
			continue
		}
		for rowid := range result {
			if !set[rowid] {
				delete(result, rowid)
			}
		}
	}
	return result, nil
}

// topAndConjuncts splits an expression into its top-level AND operands.
func topAndConjuncts(where sql.Expr) []sql.Expr {
	if where == nil {
		return nil
	}
	if bop, ok := where.(*sql.BinaryOp); ok && bop.Operator == "AND" {
		return append(topAndConjuncts(bop.Left), topAndConjuncts(bop.Right)...)
	}
	return []sql.Expr{where}
}

// matchConstraintColumn resolves a MATCH left operand to a column index of
// the given table: -1 for a whole-table match. applies=false when the operand
// references a different table.
func matchConstraintColumn(left sql.Expr, t *Table) (col int, applies bool) {
	ref, ok := left.(*sql.ColumnRef)
	if !ok {
		return -1, false
	}
	switch {
	case ref.Table != "":
		if strings.EqualFold(ref.Table, t.cfg.Name) {
			if idx := t.ColumnIndex(ref.Name); idx >= 0 {
				return idx, true
			}
			return -1, true
		}
		return -1, false
	case strings.EqualFold(ref.Name, t.cfg.Name):
		return -1, true // t1 MATCH — whole table
	default:
		if idx := t.ColumnIndex(ref.Name); idx >= 0 {
			return idx, true // a MATCH — column restricted
		}
		return -1, false
	}
}
