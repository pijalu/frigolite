// Package exec implements query execution.
//
// This file holds the join-order side of EXPLAIN QUERY PLAN rendering: the
// per-FROM-term reorder-prerequisite masks (sqlite3WhereBegin's mPrereq, with
// 3.54's hasRightCrossJoin rule that a CROSS edge pins every later FROM term
// after the terms before it), the driving-table choice, and the
// dependency-ordered emission of the remaining loops (where2-16.2).
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// planJoin computes one plan node per joined table. The driving table is the
// one with constant predicates and the smallest estimated row count; inner
// tables are placed in dependency order, using an index SEARCH when a join
// column is indexed.
func (e *SelectEngine) planJoin(tables []queryTable, s *sql.SelectStmt) []planNode {
	preds := joinPredicates(s)

	// constPreds counts constant predicates (col = literal) per table; these
	// drive the join order even when the column is not indexed.
	constPreds := make([]int, len(tables))
	joinRefs := make([][]joinRef, len(tables))
	for _, p := range preds {
		e.collectJoinPredicate(p, tables, constPreds, joinRefs)
	}

	// The reorder-prerequisite masks (where.c sqlite3WhereBegin): a CROSS
	// edge pins its right operand — and, once seen, every later FROM term —
	// after the terms before it (where2-16.2).
	prereqs := joinPrereqs(s, len(tables))

	// Outer joins preserve their left operand as the outer loop; the right
	// operand may still use an automatic index driven by the ON equality.
	// SQLite's where.c marks these terms with outer-join provenance and only
	// permits the RHS loop after its LHS dependency.
	driver := e.joinDriver(tables, constPreds, joinRefs, prereqs)
	for _, join := range s.Joins {
		if strings.Contains(strings.ToUpper(join.JoinType), "LEFT") {
			driver = 0
			break
		}
	}

	planned := []string{tables[driver].display}
	plannedIdx := []int{driver}
	remaining := make([]int, 0, len(tables)-1)
	for i := range tables {
		if i != driver {
			remaining = append(remaining, i)
		}
	}

	nodes := e.joinNodeFor(tables[driver], nil, joinRefs[driver], s)
	return e.planJoinRemaining(nodes, tables, remaining, planned, plannedIdx, joinRefs, prereqs, s)
}

// joinPrereqs computes each FROM term's reorder-prerequisite mask the way
// sqlite3WhereBegin does (where.c:4960-5017 in 3.54): a term whose join
// operator to its LEFT is CROSS (or an outer join) carries all earlier
// terms as prerequisites (mPrereq |= mPrior), and once a CROSS edge has
// been seen the per-term reset stops (hasRightCrossJoin) — later
// comma-joined terms inherit the CROSS term's prerequisites. Probe-pinned
// against 3.54: in `FROM t1, t2 CROSS JOIN t3, t4` both t3 and t4 need t1
// and t2 planned first (t4 not t3 — probe A), `t1 CROSS JOIN t2, t3, t4`
// leaves t2..t4 with prereq {t1} (probe J), and a LEFT edge still resets
// for later terms (probes H/M).
func joinPrereqs(s *sql.SelectStmt, n int) []uint64 {
	prereq := make([]uint64, n)
	var mPrior, mPrereq uint64
	crossSeen := false
	firstPastRJ := false
	for i := 0; i < n; i++ {
		jt := ""
		comma := false
		if i > 0 && i-1 < len(s.Joins) {
			jt = strings.ToUpper(s.Joins[i-1].JoinType)
			// Our parser normalizes every join operator through joinTypeOf,
			// whose default is "CROSS" — a comma join carries JoinType
			// "CROSS" PLUS CommaJoin. Only a non-comma CROSS JOIN is C's
			// JT_CROSS edge; a comma join resets like JT_INNER.
			comma = s.Joins[i-1].CommaJoin
		}
		outer := strings.Contains(jt, "LEFT") || strings.Contains(jt, "RIGHT") ||
			strings.Contains(jt, "FULL")
		cross := !comma && strings.Contains(jt, "CROSS")
		if firstPastRJ || outer || cross {
			if cross {
				crossSeen = true
			}
			mPrereq |= mPrior
			firstPastRJ = strings.Contains(jt, "RIGHT")
		} else if !crossSeen {
			mPrereq = 0
		}
		prereq[i] = mPrereq
		mPrior |= 1 << uint(i)
	}
	return prereq
}

// joinPredicates returns the flattened conjuncts of a SELECT's WHERE clause
// and every JOIN's ON clause.
func joinPredicates(s *sql.SelectStmt) []sql.Expr {
	var preds []sql.Expr
	if s.Where != nil {
		preds = append(preds, splitAnd(s.Where)...)
	}
	for _, j := range s.Joins {
		if j.On != nil {
			preds = append(preds, splitAnd(j.On)...)
		}
	}
	return preds
}

// collectJoinPredicate classifies one equality predicate: a constant
// predicate increments the table's constPreds counter; a column-to-column
// predicate records the join refs (when either side is indexed).
func (e *SelectEngine) collectJoinPredicate(p sql.Expr, tables []queryTable, constPreds []int, joinRefs [][]joinRef) {
	bin, ok := p.(*sql.BinaryOp)
	if !ok || !strings.EqualFold(bin.Operator, "=") {
		return
	}
	if colRef, constVal := findColAndConst(bin); colRef != nil && constVal != nil {
		if ti := e.resolveJoinTable(tables, colRef); ti >= 0 {
			constPreds[ti]++
		}
		return
	}
	e.collectJoinColumnPair(bin, tables, joinRefs)
}

// collectJoinColumnPair records the join refs for an equality predicate
// between two columns of different tables.
func (e *SelectEngine) collectJoinColumnPair(bin *sql.BinaryOp, tables []queryTable, joinRefs [][]joinRef) {
	left, okL := bin.Left.(*sql.ColumnRef)
	right, okR := bin.Right.(*sql.ColumnRef)
	if !okL || !okR {
		return
	}
	li := e.resolveJoinTable(tables, left)
	ri := e.resolveJoinTable(tables, right)
	if li < 0 || ri < 0 || li == ri {
		return
	}
	e.addJoinRefPair(tables, joinRefs, li, ri, left, right)
}

// addJoinRefPair records the join refs for both sides of a column-to-column
// equality predicate.
func (e *SelectEngine) addJoinRefPair(tables []queryTable, joinRefs [][]joinRef, li, ri int, left, right *sql.ColumnRef) {
	e.maybeAddJoinRef(tables, joinRefs, li, left.Name, ri, right.Name)
	e.maybeAddJoinRef(tables, joinRefs, ri, right.Name, li, left.Name)
}

// maybeAddJoinRef records a joinRef for table ti when its column colName is
// indexed (SEARCH once other is planned), or, when ti's column has no index
// and the other table is not a real schema table (a subquery/derived table),
// records an automatic-index joinRef on the other side.
func (e *SelectEngine) maybeAddJoinRef(tables []queryTable, joinRefs [][]joinRef, ti int, colName string, otherIdx int, otherCol string) {
	if idx := e.findIndexOnColumn(tables[ti].real, colName); idx != "" {
		joinRefs[ti] = append(joinRefs[ti], joinRef{table: tables[ti].display, col: colName, otherTable: tables[otherIdx].display, indexName: idx})
		return
	}
	// When the opposite operand has a usable index, its indexed lookup is
	// the inner-loop candidate; do not add a competing automatic-index loop.
	if e.isRealTable(tables[ti].real) && e.findIndexOnColumn(tables[otherIdx].real, otherCol) != "" {
		return
	}
	// Otherwise SQLite may build an automatic covering index for this
	// unindexed operand when selected as an inner loop.
	joinRefs[ti] = append(joinRefs[ti], joinRef{table: tables[ti].display, col: colName, otherTable: tables[otherIdx].display, indexName: ""})
}

// resolveJoinTable maps a column reference to its table index in the join.
// A qualified ref (t.col) resolves directly; an unqualified ref resolves by
// matching the column name against each table's columns (-1 when ambiguous).
func (e *SelectEngine) resolveJoinTable(tables []queryTable, ref *sql.ColumnRef) int {
	if ref.Table != "" {
		return e.tableIndexByDisplay(tables, ref.Table)
	}
	found := -1
	for i := range tables {
		hasColumn := e.ctx.TableHasColumn(tables[i].real, ref.Name)
		if !hasColumn {
			if view, _, err := e.ctx.FindView(tables[i].real); err == nil {
				if names, nameErr := e.viewSelectColumnNames(view); nameErr == nil {
					hasColumn = containsFold(names, ref.Name)
				}
			}
		}
		if hasColumn {
			if found >= 0 {
				return -1 // ambiguous
			}
			found = i
		}
	}
	return found
}

// joinDriver chooses the driving table of a join: among tables with constant
// predicates the one with the smallest estimated row count; with no constant
// predicates, the table with the fewest indexed join connections (so tables
// with useful join indexes become SEARCHed inner tables). Matches SQLite's
// choice for e.g. "FROM t2, t1 WHERE a=z AND c=x" where t2's index covers
// both predicates: scan t1, search t2. Tables with reorder prerequisites
// (joinPrereqs — a CROSS edge or its inheritance) cannot drive: the driver
// is the plan's first loop, and C's solver only starts a path with a
// zero-prereq loop (where2-16.2: t4's g=1 const pred does not make it the
// outer loop of `t1, t2 CROSS JOIN t3, t4`).
func (e *SelectEngine) joinDriver(tables []queryTable, constPreds []int, joinRefs [][]joinRef, prereqs []uint64) int {
	driver := 0
	bestCnt := int64(0)
	found := false
	for i := range tables {
		if constPreds[i] == 0 || prereqs[i] != 0 {
			continue
		}
		cnt := e.estimatedRowCount(tables[i].real)
		if !found || cnt < bestCnt {
			driver, bestCnt, found = i, cnt, true
		}
	}
	if !found {
		return leastJoinRefs(joinRefs, prereqs)
	}
	return driver
}

// leastJoinRefs returns the table index with the fewest indexed join
// connections among the zero-prereq tables (joinPrereqs; the first FROM
// term always qualifies).
func leastJoinRefs(joinRefs [][]joinRef, prereqs []uint64) int {
	best := -1
	for i := range joinRefs {
		if prereqs[i] != 0 {
			continue
		}
		if best < 0 || len(joinRefs[i]) < len(joinRefs[best]) {
			best = i
		}
	}
	if best < 0 {
		best = 0
	}
	return best
}

// planJoinRemaining orders the non-driving tables by indexed join
// connectivity, emitting each table's node once its join partner is planned
// and the table's reorder prerequisites are covered.
func (e *SelectEngine) planJoinRemaining(nodes []planNode, tables []queryTable, remaining []int, planned []string, plannedIdx []int, joinRefs [][]joinRef, prereqs []uint64, s *sql.SelectStmt) []planNode {
	for len(remaining) > 0 {
		next := e.nextJoinTable(remaining, joinRefs, planned, plannedIdx, prereqs)
		i := remaining[next]
		remaining = append(remaining[:next], remaining[next+1:]...)
		nodes = append(nodes, e.joinNodeFor(tables[i], planned, joinRefs[i], s)...)
		planned = append(planned, tables[i].display)
		plannedIdx = append(plannedIdx, i)
	}
	return nodes
}

// nextJoinTable returns the index (into remaining) of the next table to plan:
// the first with an indexed join connection to an already-planned table, or
// the first prereq-covered table (original order) when none exists. A table
// whose reorder prerequisites are not yet covered — a CROSS edge's right
// operand, or a term inheriting them — cannot be planned
// (sqlite3WhereBegin's mPrereq; where2-16.2).
func (e *SelectEngine) nextJoinTable(remaining []int, joinRefs [][]joinRef, planned []string, plannedIdx []int, prereqs []uint64) int {
	var mask uint64
	for _, i := range plannedIdx {
		mask |= 1 << uint(i)
	}
	for k, i := range remaining {
		if prereqs[i]&^mask != 0 {
			continue
		}
		if e.joinSearchRef(joinRefs[i], planned) != nil {
			return k
		}
	}
	for k, i := range remaining {
		if prereqs[i]&^mask == 0 {
			return k
		}
	}
	return 0 // unreachable: the first FROM term carries no prereq
}

// planSubqueryNodes returns one plan node per subquery expression in a
// SELECT's WHERE, HAVING, and select list, in SQLite's style: SCALAR SUBQUERY
// n for scalar/EXISTS subqueries (with a CORRELATED prefix when the subquery
// references outer columns), LIST SUBQUERY n for IN, and the subquery's own
// plan nested beneath the node.
//
// EXISTS terms that are top-level WHERE conjuncts and satisfy SQLite's
// EXISTS-to-JOIN conditions (single real table, no aggregate/limit/compound)
// are rendered as join loops — "SEARCH <tbl> EXISTS USING <index> (<col>=?)"
// or "SCAN <tbl> EXISTS" — instead of a SUBQUERY node, matching SQLite's
// select.c existsToJoin() transformation.
