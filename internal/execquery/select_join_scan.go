// Package exec implements query execution.
//
// This file owns the JOIN statement's FROM-table loop (where.c's outermost
// WhereLoop): the FROM-only top-level WHERE conjuncts drive an INTEGER
// PRIMARY KEY seek, a secondary-index seek, or — when no seek survives —
// the ORDER-BY-serving index walk (C's "SCAN t USING INDEX i" unconstrained
// loop). A loop the planner indexes drives its b-tree directly instead of
// the full table scan, so the rows enter the join nesting in the loop's own
// order and the SQLITE_STMTSTATUS_FULLSCAN_STEP counter matches C's
// (constrained loops count 0; an unconstrained index walk counts n-1; a
// table scan counts n-1). The join phase re-checks the FULL WHERE on the
// combined rows, so dropping the cross-table conjuncts from the base seek's
// residual loses nothing — where.c evaluates each term at the first loop
// level whose tables can supply it.
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// joinBaseScanRows runs the FROM table's planner-chosen loop for a join
// statement. handled=true carries the loop's rows and FULL row maps into
// execSelectJoins (the join rebuilds output rows from the maps); false
// keeps the regular table scan.
func (e *SelectEngine) joinBaseScanRows(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, tree *btree.BTree, feed *simpleAggFeed) ([][]interface{}, []RowMap, bool, error) {
	if len(s.Joins) == 0 || s.From.Name == "" || s.From.Subquery != nil ||
		s.From.EmptyName || s.From.NotIndexed || s.From.IndexedBy != "" ||
		IsSchemaTable(tableEntry.Name) || tableEntry.RootPage == 0 ||
		e.ctx.TableIsWithoutRowidEntry(tableEntry) || feed != nil ||
		e.outerRow != nil || len(e.OuterRows()) > 0 || e.ctx.ReverseUnordered() {
		return nil, nil, false, nil
	}
	where := e.joinTableOnlyConjuncts(s, tableEntry, colDefs)
	// The seek executors run on a Joins-stripped copy: their shape gates
	// reject joins, but the loop mechanics are exactly the single-table
	// loop C's outer nesting level runs. The ORDER BY is stripped too —
	// the join's own ORDER BY delivery gate owns the ordering decision;
	// the loop pick for the walk case is taken separately with the ORDER
	// BY kept (joinBaseOrderIndexWalk).
	bs := *s
	bs.Joins = nil
	bs.Where = where
	bs.OrderBy = nil

	// The join pipeline consumes full row maps for every base row.
	prev := e.forceScanRowMaps
	e.forceScanRowMaps = true
	defer func() { e.forceScanRowMaps = prev }()

	// The OR-branch row source (where.c's MULTI-INDEX OR loop): the
	// FROM-only conjunct view's top-level OR plans as per-branch index
	// seeks — every branch must find an index (an unindexed arm voids the
	// plan exactly like C's no-OR-loop rule, where8-3.9's c='IX' arm). The
	// rows come out in branch scan order and the FULLSCAN_STEP counter
	// stays silent (the branch seeks never count). The base OR loop never
	// delivers an ORDER BY (C's u.btree.pIndex==0 rule in
	// wherePathSatisfiesOrderBy) — the sorter stays.
	if !s.Distinct && len(s.GroupBy) == 0 {
		rows, maps, routed, rerr := e.routeJoinBaseOrPlan(&bs, tableEntry, dbCtx, colDefs)
		if rerr != nil {
			return nil, nil, true, rerr
		}
		if routed {
			e.joinBaseLoop = &joinDeliveryLoop{orLoop: true, real: tableEntry.Name,
				withoutRowid: e.operandIsWithoutRowid(tableEntry.Name)}
			return rows, maps, true, nil
		}
	}

	rows, maps, handled := e.selectRowidSeekRows(&bs, tableEntry, colDefs, tree, nil)
	if handled {
		// The point-lookup rowid seek: at most one row (C's WHERE_ONEROW).
		e.joinBaseLoop = &joinDeliveryLoop{kind: loopIPK, oneRow: true, real: tableEntry.Name}
		return rows, maps, true, nil
	}
	rows, maps, handled, err := e.selectIndexSeekRows(&bs, tableEntry, dbCtx, colDefs, tree, nil)
	if err != nil {
		return nil, nil, true, err
	}
	if handled {
		// The index seek: the rows arrive in the seek's index order within
		// the equality/range bounds (C's SEARCH loop).
		e.joinBaseLoop = &joinDeliveryLoop{kind: loopIndex, seek: true,
			real: tableEntry.Name, withoutRowid: e.operandIsWithoutRowid(tableEntry.Name)}
		if loop := e.scanLoopForQuery(queryTableFromRef(s.From), &bs, false); loop.kind == loopIndex {
			e.joinBaseLoop.idx = loop.token
			e.joinBaseLoop.nEq = e.indexEqPrefixLen(queryTableFromRef(s.From), loop.token, where)
		}
		return rows, maps, true, nil
	}
	// No seek survived: the ORDER-BY-serving index walk is the remaining
	// candidate (scanLoopForQuery's loopNone fallback — where.c prices the
	// sorter into every non-delivering loop, so a walk that delivers the
	// order wins over the scan+sort shape).
	rows, maps, handled, walkErr := e.joinBaseOrderIndexWalk(s, bs, where, tableEntry, dbCtx, colDefs, tree)
	if walkErr != nil {
		return nil, nil, true, walkErr
	}
	if handled {
		return rows, maps, true, nil
	}
	// The plain table scan (C's Case 6 loop): rowid order.
	e.joinBaseLoop = &joinDeliveryLoop{kind: loopNone, real: tableEntry.Name,
		withoutRowid: e.operandIsWithoutRowid(tableEntry.Name)}
	return nil, nil, false, nil
}

// joinBaseOrderIndexWalk runs the ORDER-BY-serving unconstrained index walk
// for the join statement's FROM table (C's "SCAN t USING INDEX i" loop: no
// constraint on the table, the whole index walked, each entry's row fetched
// through the deferred table seek). C's unconstrained index loop sets
// OPFLAG_FULLSCAN_STEP (wherecode.c: no WHERE_CONSTRAINT), so its OP_Next
// advances count n-1 — recorded here; the per-candidate fetch movetos were
// already counted by the fetch path's search counter.
func (e *SelectEngine) joinBaseOrderIndexWalk(s *sql.SelectStmt, bs sql.SelectStmt, where sql.Expr, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, tree *btree.BTree) ([][]interface{}, []RowMap, bool, error) {
	// The loop pick: an index whose unconstrained walk consumes every ORDER
	// BY term of the FROM table (the prefix + order-distinct mark-off rule —
	// where.c's ORDER-BY-delivering loop for the outermost slot; expression
	// terms ride the mark-off). Candidates run in schema order.
	tok, ok := e.joinBaseWalkCandidate(s, where, tableEntry, colDefs)
	if !ok {
		return nil, nil, false, nil
	}
	// The walk ran: record its pick for the ORDER BY delivery analysis (the
	// rows emerge in this index's stored key order).
	e.joinBaseLoop = &joinDeliveryLoop{kind: loopIndex, idx: tok,
		real: tableEntry.Name, withoutRowid: e.operandIsWithoutRowid(tableEntry.Name)}
	plan := &indexSeekPlan{index: tok}
	run, idxTree, ok := e.openIndexSeekRun(plan, tableEntry, dbCtx, colDefs)
	if !ok {
		return nil, nil, false, nil
	}
	defer idxTree.Close()
	entries := countIndexEntries(run, idxTree)
	cursor, ok := run.position(idxTree)
	if !ok {
		return [][]interface{}{}, nil, true, nil
	}
	rows, maps, done := e.indexSeekForwardRows(&bs, tableEntry, colDefs, tree, run, cursor, true, nil)
	if !done {
		return nil, nil, false, nil
	}
	if entries > 1 {
		// SQLITE_STMTSTATUS_FULLSCAN_STEP: the unconstrained index loop's
		// OP_Next advances. indexSeekForwardRows already counted the Nexts
		// in the search counter; the fullscan counter mirrors them here.
		e.ctx.RecordFullScanSteps(entries - 1)
	}
	return rows, maps, true, nil
}

// joinBaseWalkCandidate tests the FROM table's indexes (schema order) with
// the base loop's delivery walk and returns the first whose walk consumes
// every ORDER BY term of the base operand, forward only (the executor's
// walk is forward; a backward delivery keeps the sorter — carded).
func (e *SelectEngine) joinBaseWalkCandidate(s *sql.SelectStmt, where sql.Expr, tableEntry *schema.Entry, colDefs []sql.ColumnDef) (string, bool) {
	if len(s.OrderBy) == 0 {
		return "", false
	}
	ops := e.joinDeliveryOperands(s, colDefs)
	if ops == nil {
		return "", false
	}
	terms := e.joinDeliveryTerms(s, s.OrderBy, ops)
	if terms == nil {
		return "", false
	}
	// The base loop's usable pool: the FROM-only conjuncts (where.c's
	// prereqRight ⊆ ready with nothing positioned — a conjunct referencing
	// another operand is never usable at loop 0).
	pool := make([]joinPoolEntry, 0, 4)
	for _, conj := range splitAnd(where) {
		refs, ok := e.joinExprOperandUsage(conj, ops)
		if !ok {
			pool = append(pool, joinPoolEntry{expr: conj})
			continue
		}
		pool = append(pool, joinPoolEntry{expr: conj, refs: refs})
	}
	indexes, err := e.ctx.Schema().FindIndexesForTable(tableEntry.Name)
	if err != nil {
		return "", false
	}
	for _, entry := range indexes {
		if len(e.indexColumns(entry.Name)) == 0 || indexHasPartialPredicate(entry) {
			continue
		}
		testTerms := make([]joinOrderTerm, len(terms))
		copy(testTerms, terms)
		loop := joinDeliveryLoop{operand: 0, token: strings.ToLower(ops[0].tokens[0]),
			real: tableEntry.Name, kind: loopIndex, idx: entry.Name,
			withoutRowid: e.operandIsWithoutRowid(tableEntry.Name)}
		st := &joinWalkState{obDone: len(testTerms), isOrderDistinct: true,
			orderDistinctMask: make([]bool, len(ops)), ready: make([]bool, len(ops))}
		e.joinEqPrePass(&loop, testTerms, pool, st, ops)
		e.joinWalkLoopColumns(&loop, testTerms, st)
		if st.isOrderDistinct {
			st.orderDistinctMask[0] = true
			for k := range testTerms {
				if !testTerms[k].sat && testTerms[k].operand == 0 &&
					joinUsageSubset(testTerms[k].usage, st.orderDistinctMask) {
					testTerms[k].sat = true
				}
			}
		}
		if !loop.backward && joinTermsOfOperandSatisfied(testTerms, 0) {
			return entry.Name, true
		}
	}
	return "", false
}

// countIndexEntries walks the run's index b-tree once to count its entries
// (the unconstrained loop visits every entry; each visit is one OP_Next
// advance after the first).
func countIndexEntries(run *indexSeekRun, tree *btree.BTree) int {
	cursor, ok := run.position(tree)
	if !ok {
		return 0
	}
	n := 0
	for {
		_, inRange, ok := indexSeekCurrentRowid(run, cursor)
		if !ok || !inRange {
			break
		}
		n++
		more, err := cursor.Next()
		if err != nil || !more {
			break
		}
	}
	return n
}

// joinTableOnlyConjuncts returns the AND of the WHERE's top-level conjuncts
// whose column references all resolve to the FROM table (C's per-loop
// constraint analysis: a term is usable by loop 1 only when every table it
// references is already positioned). A conjunct carrying a subquery is
// dropped: its correlation may reach tables the base row map cannot supply
// (the join phase evaluates it against the combined row). nil when no
// conjunct qualifies.
func (e *SelectEngine) joinTableOnlyConjuncts(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) sql.Expr {
	if s.Where == nil {
		return nil
	}
	fromTokens := joinFromTokens(s)
	colNames := joinBaseColumnNames(colDefs)
	var kept []sql.Expr
	for _, conj := range splitAnd(s.Where) {
		if exprHasSubquery(conj) {
			continue
		}
		if e.exprReferencesOnlyTables(conj, fromTokens, colNames) {
			kept = append(kept, conj)
		}
	}
	return andChainExprs(kept)
}

// joinFromTokens lists the FROM table's reference tokens: its alias (when
// present) and its name, schema prefix stripped.
func joinFromTokens(s *sql.SelectStmt) []string {
	var tokens []string
	if s.From.As != "" {
		tokens = append(tokens, s.From.As)
	}
	name := s.From.Name
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:]
	}
	if name != "" {
		tokens = append(tokens, name)
	}
	return tokens
}

// joinBaseColumnNames returns the lower-cased column names (plus the rowid
// pseudo-columns) the FROM table's unqualified references may name.
func joinBaseColumnNames(colDefs []sql.ColumnDef) map[string]bool {
	names := make(map[string]bool, len(colDefs)+3)
	for i := range colDefs {
		names[strings.ToLower(colDefs[i].Name)] = true
	}
	names["rowid"] = true
	names["_rowid_"] = true
	names["oid"] = true
	return names
}

// exprReferencesOnlyTables reports whether every column reference in the
// expression names one of the given tables: a qualified reference by its
// table token, an unqualified one by membership in the FROM table's column
// set (an unqualified name outside the set belongs to another operand —
// the join phase resolves it).
func (e *SelectEngine) exprReferencesOnlyTables(expr sql.Expr, fromTokens []string, colNames map[string]bool) bool {
	ok := true
	WalkExprFull(expr, func(n sql.Expr) {
		if !ok {
			return
		}
		ref, isRef := n.(*sql.ColumnRef)
		if !isRef || ref.Name == "*" {
			return
		}
		if ref.Table != "" {
			if !refTableMatchesTokens(ref.Table, fromTokens) {
				ok = false
			}
			return
		}
		if !colNames[strings.ToLower(ref.Name)] {
			ok = false
		}
	})
	return ok
}

// refTableMatchesTokens reports whether a qualified reference's table token
// (schema prefix stripped) matches one of the FROM table's tokens.
func refTableMatchesTokens(table string, tokens []string) bool {
	if dot := strings.LastIndex(table, "."); dot >= 0 {
		table = table[dot+1:]
	}
	for _, tok := range tokens {
		if strings.EqualFold(table, tok) {
			return true
		}
	}
	return false
}

// routeJoinBaseOrPlan routes the base loop to the OR-branch row source when
// the FROM-only conjunct view plans as a MULTI-INDEX OR (the S21 branch
// machinery; an IN-valued arm expands to per-value equality branches first,
// execSelectOrPlan's routing). The executed statement keeps its original
// WHERE — the join's full-WHERE filter re-checks every cross-table term.
func (e *SelectEngine) routeJoinBaseOrPlan(bs *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef) ([][]interface{}, []RowMap, bool, error) {
	if bs.Where == nil {
		return nil, nil, false, nil
	}
	branches, ok := e.ctx.PlanOrIndexScan(bs.Where, tableEntry.Name, colDefs, dbCtx)
	delta := int64(0)
	if !ok {
		expansion := e.expandOrInArms(bs, tableEntry)
		if expansion == nil {
			return nil, nil, false, nil
		}
		branches, ok = e.ctx.PlanOrIndexScan(expansion.where, tableEntry.Name, colDefs, dbCtx)
		if !ok {
			return nil, nil, false, nil
		}
		delta = expansion.delta
	}
	rows, maps, err := e.ctx.ExecSelectWithOrPlanRows(bs, tableEntry, dbCtx, colDefs, branches)
	if err != nil {
		return nil, nil, true, err
	}
	if delta != 0 {
		e.ctx.IncSearchCount(delta)
	}
	return rows, maps, true, nil
}
