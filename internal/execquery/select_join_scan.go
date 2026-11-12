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

	rows, maps, handled := e.selectRowidSeekRows(&bs, tableEntry, colDefs, tree, nil)
	if handled {
		return rows, maps, true, nil
	}
	rows, maps, handled, err := e.selectIndexSeekRows(&bs, tableEntry, dbCtx, colDefs, tree, nil)
	if err != nil {
		return nil, nil, true, err
	}
	if handled {
		return rows, maps, true, nil
	}
	// No seek survived: the ORDER-BY-serving index walk is the remaining
	// candidate (scanLoopForQuery's loopNone fallback — where.c prices the
	// sorter into every non-delivering loop, so a walk that delivers the
	// order wins over the scan+sort shape).
	return e.joinBaseOrderIndexWalk(s, bs, where, tableEntry, dbCtx, colDefs, tree)
}

// joinBaseOrderIndexWalk runs the ORDER-BY-serving unconstrained index walk
// for the join statement's FROM table (C's "SCAN t USING INDEX i" loop: no
// constraint on the table, the whole index walked, each entry's row fetched
// through the deferred table seek). C's unconstrained index loop sets
// OPFLAG_FULLSCAN_STEP (wherecode.c: no WHERE_CONSTRAINT), so its OP_Next
// advances count n-1 — recorded here; the per-candidate fetch movetos were
// already counted by the fetch path's search counter.
func (e *SelectEngine) joinBaseOrderIndexWalk(s *sql.SelectStmt, bs sql.SelectStmt, where sql.Expr, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, tree *btree.BTree) ([][]interface{}, []RowMap, bool, error) {
	// The loop pick keeps the ORDER BY (the walk must be the one the ORDER
	// BY serving analysis chooses) but plans constraints from the FROM-only
	// conjuncts only.
	ls := bs
	ls.OrderBy = s.OrderBy
	loop := e.scanLoopForQuery(queryTableFromRef(s.From), &ls, false)
	if loop.kind != loopIndex || loop.seek || loop.token == "PRIMARY KEY" || loop.groupDistinct {
		return nil, nil, false, nil
	}
	plan := &indexSeekPlan{index: loop.token}
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
