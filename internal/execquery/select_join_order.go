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
	tokens []string        // reference tokens (alias first, then name)
	real   string          // real table name ("" when the operand is not a real table)
	names  map[string]bool // column names (lower-cased) for unqualified references
}

// joinOrderTerm is one ORDER BY term resolved for the join delivery walk:
// the single-table orderTerm plus the operand identity and the expression's
// operand usage (the mark-off's mask inputs).
type joinOrderTerm struct {
	orderTerm        // col, desc, collate, nullsFirst/Last, sat
	operand  int     // the operand a bare-column term belongs to
	usage    []bool  // operand usage of the term's expression
	isConst  bool    // the term's expression is constant (no operands, no subquery)
	col      *sql.ColumnRef // non-nil for bare-column terms
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
		s.From.Subquery != nil || s.From.Name == "" || s.From.NotIndexed ||
		s.From.IndexedBy != "" || e.ctx.ReverseUnordered() {
		return nil
	}
	// All-inner plain joins only: outer-join NULL padding, USING/NATURAL
	// merges, and parenthesized groups are outside the walk (the sorter
	// owns those statements).
	for i := range s.Joins {
		join := &s.Joins[i]
		jt := strings.ToUpper(join.JoinType)
		if strings.Contains(jt, "LEFT") || strings.Contains(jt, "RIGHT") || strings.Contains(jt, "FULL") {
			return nil
		}
		if len(join.Using) > 0 || isNaturalJoinType(join.JoinType) {
			return nil
		}
		if join.Table.Subquery != nil || join.Table.JoinGroup || join.Table.NotIndexed {
			return nil
		}
	}
	ops := e.joinDeliveryOperands(s, baseDefs)
	if ops == nil {
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
	// Inner loops: a real-table operand's delivery pick (the ORDER-BY-
	// serving walk, else the cost covering walk, else the table walk);
	// anything else cannot deliver (its rows have no defined b-tree order).
	picks := make([]*joinInnerPick, len(s.Joins))
	for i := range s.Joins {
		join := &s.Joins[i]
		op := &ops[i+1]
		loop := joinDeliveryLoop{operand: i + 1, token: strings.ToLower(op.tokens[0]), real: op.real}
		if op.real == "" {
			loops = append(loops, loop)
			continue
		}
		loop.withoutRowid = e.operandIsWithoutRowid(op.real)
		loop.idx, loop.kind = e.joinInnerOrderPick(op, s)
		if loop.kind == loopIndex {
			picks[i] = &joinInnerPick{idxName: loop.idx}
		}
		loops = append(loops, loop)
	}
	pool := e.joinDeliveryPool(s, ops)
	if pool == nil {
		return nil
	}
	terms := e.joinDeliveryTerms(s.OrderBy, ops)
	if terms == nil {
		return nil
	}
	if !e.joinWalkOrder(loops, terms, pool) {
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

// joinDeliveryTerms resolves the ORDER BY terms: bare-column terms carry
// their operand, expression terms carry their usage mask. nil when any term
// cannot be resolved (the claim declines).
func (e *SelectEngine) joinDeliveryTerms(orderBy []sql.OrderByTerm, ops []joinDeliveryOperand) []joinOrderTerm {
	terms := make([]joinOrderTerm, 0, len(orderBy))
	for _, ob := range orderBy {
		expr := normalizeOrderByExpr(ob.Expr)
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
func (e *SelectEngine) joinInnerOrderPick(op *joinDeliveryOperand, s *sql.SelectStmt) (string, int) {
	t := e.queryTableForOperand(op)
	terms := e.joinOperandOrderTerms(s.OrderBy, op)
	// The ORDER-BY-serving walk: the operand's terms in ORDER BY order are
	// delivered by an index whose walk orders them (the rowid tail included).
	if len(terms) > 0 {
		synth := &sql.SelectStmt{From: sql.TableRef{Name: op.real}, OrderBy: terms}
		loop := e.scanLoopForQuery(t, synth, false)
		if loop.kind == loopIndex && !loop.seek && loop.token != "PRIMARY KEY" && !loop.groupDistinct {
			return loop.token, loopIndex
		}
	}
	// The cost covering walk: the statement's used columns on this operand
	// ride the index's key columns plus the trailing rowid (C's colUsed
	// covering rule; a WITHOUT ROWID operand has no rowid tail and no walk
	// pick — its table b-tree already is the PK order).
	if op.real == "" || e.operandIsWithoutRowid(op.real) {
		return "", loopNone
	}
	used := e.joinOperandUsedColumns(s, op)
	if idx := e.joinCostCoveringIndex(op.real, used); idx != "" {
		return idx, loopIndex
	}
	return "", loopNone
}

// queryTableForOperand builds the queryTable a single-table planner call
// needs for one join operand.
func (e *SelectEngine) queryTableForOperand(op *joinDeliveryOperand) queryTable {
	return queryTable{display: op.tokens[0], real: op.real}
}

// joinOperandOrderTerms returns the ORDER BY terms (in ORDER BY order)
// whose bare columns belong to the given operand.
func (e *SelectEngine) joinOperandOrderTerms(orderBy []sql.OrderByTerm, op *joinDeliveryOperand) []sql.OrderByTerm {
	var out []sql.OrderByTerm
	for _, ob := range orderBy {
		ref, ok := normalizeOrderByExpr(ob.Expr).(*sql.ColumnRef)
		if !ok || ref.Name == "*" {
			continue
		}
		if ref.Table != "" {
			tok := strings.ToLower(schemaStrippedName(ref.Table))
			if !joinTokensContain(op.tokens, tok) {
				continue
			}
		} else if !op.names[strings.ToLower(ref.Name)] {
			continue
		}
		out = append(out, ob)
	}
	return out
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

// joinCostCoveringIndex returns the first index (schema order) whose key
// columns plus the trailing rowid cover every used column of the operand.
func (e *SelectEngine) joinCostCoveringIndex(realTable string, used map[string]bool) string {
	indexes, err := e.ctx.Schema().FindIndexesForTable(realTable)
	if err != nil {
		return ""
	}
	ipk := e.ipkAliasColumn(realTable)
	for _, entry := range indexes {
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
