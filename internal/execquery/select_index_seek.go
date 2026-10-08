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
}

// selectIndexSeekPlanFor resolves the index-driven scan for a single-table
// SELECT, or nil when the statement keeps the regular table scan. The gates
// mirror the shapes whose row order an index loop can serve: one real rowid
// table, no join, no ORDER BY / GROUP BY / DISTINCT / compound member, and a
// WHERE without a top-level OR (an OR is a union of index ranges, served by
// execSelectWithOrPlan).
func (e *SelectEngine) selectIndexSeekPlanFor(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *indexSeekPlan {
	if !indexSeekShapeApplies(s, tableEntry) || e.ctx.TableIsWithoutRowidEntry(tableEntry) {
		return nil
	}
	loop := e.scanLoopForRowCount(queryTableFromRef(s.From), s, indexSeekPlanRowCount)
	if loop.kind != loopIndex || !loop.seek || loop.token == "PRIMARY KEY" {
		return nil
	}
	return e.buildIndexSeekPlan(s, tableEntry, colDefs, loop.token)
}

// indexSeekShapeApplies reports whether the statement is a single-table scan
// whose row order an index loop can serve: one real FROM table, no join, no
// ORDER BY / GROUP BY / DISTINCT / compound member, a WHERE without a
// top-level OR (an OR is a union of index ranges, served by
// execSelectWithOrPlan), and no system table.
func indexSeekShapeApplies(s *sql.SelectStmt, tableEntry *schema.Entry) bool {
	if s == nil || s.Where == nil || tableEntry == nil {
		return false
	}
	if !indexSeekPlainScanShape(s) {
		return false
	}
	return !IsSchemaTable(tableEntry.Name) && !whereHasOrConjunct(s.Where)
}

// indexSeekPlainScanShape reports whether the statement is a plain
// single-table scan: a real FROM table, no join, and no ORDER BY / GROUP BY /
// DISTINCT / compound member.
func indexSeekPlainScanShape(s *sql.SelectStmt) bool {
	if len(s.Joins) > 0 || len(s.OrderBy) > 0 || len(s.GroupBy) > 0 || s.Distinct || s.Union != nil {
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
func (e *SelectEngine) indexSeekRefSet(s *sql.SelectStmt, tableEntry *schema.Entry, token string) map[string]bool {
	allowed := map[string]bool{}
	for _, ref := range e.seekableRefs(collectIndexedRefs(s.Where, tableEntry.Name, e), tableEntry.Name) {
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
	lo, hi, ok := e.indexSeekRangeTerms(terms, allowed, colDefs, col)
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
// the candidate set exact).
func (e *SelectEngine) indexSeekRangeTerms(terms []indexSeekTerm, allowed map[string]bool, colDefs []sql.ColumnDef, col string) (*indexSeekBound, *indexSeekBound, bool) {
	var lo, hi *indexSeekBound
	for _, t := range terms {
		if !strings.EqualFold(t.col, col) || !allowed[indexSeekRefKey(t.col, t.op)] {
			continue
		}
		l, h := e.indexSeekBoundFor(t, colDefs, col)
		if l != nil {
			lo = l
		}
		if h != nil {
			hi = h
		}
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
			continue
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
	hi, ok2 := e.seekConstValue(v.High)
	if !ok1 || !ok2 {
		return indexSeekTerm{}, false
	}
	return indexSeekTerm{col: col, op: "BETWEEN", val: lo, val2: hi}, true
}

// indexSeekLikeTerm resolves a LIKE/GLOB conjunct to its literal-prefix lower
// bound. A pattern without a literal prefix cannot position the scan and
// keeps the regular one (SQLite likewise only range-scans a non-empty
// prefix).
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
	return indexSeekTerm{col: col, op: v.Operator, val: prefix}, true
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
