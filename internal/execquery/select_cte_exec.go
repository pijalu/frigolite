package execquery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// evalRecursiveTerm executes a single recursive term against one row of the
// previous iteration. The CTE reference in the term's FROM is replaced by a
// materialized one-row subquery holding the current row, so the term runs as
// a real SELECT through the engine's join machinery — the same way SQLite
// binds the recursive table to each previous-iteration row. Trailing
// ORDER BY/LIMIT/OFFSET belong to the whole CTE body (the parser attaches
// them to the last compound member), not to one term evaluation, so they are
// stripped here.
func (e *SelectEngine) evalRecursiveTerm(term *sql.SelectStmt, row []interface{}, colDefs []sql.ColumnDef, cte *sql.CTEDef) ([][]interface{}, error) {
	// SQLite rejects using a recursive CTE as a table-valued function
	// ("FROM cte(args)") with "'NAME' is not a function". Such a reference
	// must not be silently replaced by the bound subquery — doing so would
	// recurse forever (e.g. with1 29.1: "WITH RECURSIVE cte1(x,y,z) AS
	// (VALUES(1,2,3) UNION ALL SELECT x,4,5 FROM t1 RIGHT JOIN cte1(x))").
	if e.recursiveTermUsesCTEAsFunction(term, cte.Name) {
		return nil, fmt.Errorf("'%s' is not a function", cte.Name)
	}
	termCopy := *term
	termCopy.Union = nil
	termCopy.OrderBy = nil
	termCopy.Limit = nil
	termCopy.Offset = nil
	// The shallow struct copy aliases the original Joins backing array;
	// replacing the CTE reference below would otherwise write through into
	// the stored CTE body and poison later iterations. Copy the elements.
	termCopy.Joins = make([]sql.JoinClause, len(term.Joins))
	copy(termCopy.Joins, term.Joins)
	bound := boundCTESubquery(row, colDefs)
	if err := replaceCTERef(&termCopy.From, cte.Name, bound); err != nil {
		return nil, err
	}
	for i := range termCopy.Joins {
		if err := replaceCTERef(&termCopy.Joins[i].Table, cte.Name, bound); err != nil {
			return nil, err
		}
	}
	res := e.execSelect(&termCopy)
	if res.Error != nil {
		return nil, res.Error
	}
	return res.Rows, nil
}

// recursiveTermUsesCTEAsFunction reports whether a recursive term references
// the CTE in table-function form ("FROM cte(args)"), which SQLite rejects
// with "'NAME' is not a function".
func (e *SelectEngine) recursiveTermUsesCTEAsFunction(term *sql.SelectStmt, cteName string) bool {
	if tableRefUsesNameAsFunction(&term.From, cteName) {
		return true
	}
	for i := range term.Joins {
		if tableRefUsesNameAsFunction(&term.Joins[i].Table, cteName) {
			return true
		}
	}
	return false
}

func tableRefUsesNameAsFunction(ref *sql.TableRef, name string) bool {
	if ref == nil || ref.Subquery != nil {
		return false
	}
	if len(ref.Args) == 0 {
		return false
	}
	return ref.Name == name || ref.As == name
}

// boundCTESubquery builds the one-row SELECT that stands in for the recursive
// table when evaluating a recursive term against one previous-iteration row:
// SELECT v0 AS col0, v1 AS col1, ... . The caller aliases it with the CTE
// name so qualified references (closure.x) resolve through the join row map.
func boundCTESubquery(row []interface{}, colDefs []sql.ColumnDef) *sql.SelectStmt {
	cols := make([]sql.SelectColumn, len(colDefs))
	for i, cd := range colDefs {
		var v interface{}
		if i < len(row) {
			v = row[i]
		}
		cols[i] = sql.SelectColumn{Expr: valueLiteralExpr(v), As: cd.Name}
	}
	return &sql.SelectStmt{Columns: cols}
}

// valueLiteralExpr wraps a Go value in the matching literal expression node
// for use as a bound subquery column.
func valueLiteralExpr(v interface{}) sql.Expr {
	switch x := util.UnwrapColumnValue(v).(type) {
	case int64:
		n := &sql.NumericLit{}
		n.SetCached(x)
		return n
	case float64:
		n := &sql.NumericLit{}
		n.SetCached(x)
		return n
	case string:
		return &sql.StringLit{Value: x}
	case []byte:
		return &sql.BlobLit{Value: x}
	case function.JSONText:
		// Aggregate functions like json_group_array() return values carrying
		// the JSON subtype; render them as their text form (the literal is
		// re-evaluated as an expression value downstream).
		return &sql.StringLit{Value: string(x)}
	default:
		return &sql.NullLit{}
	}
}

// replaceCTERef swaps a FROM/join operand that names the CTE for the bound
// one-row subquery, keeping the alias so qualified column references keep
// resolving (FROM closure -> FROM (SELECT 1 AS x) AS closure). Table-function
// references ("FROM cte(args)") are rejected earlier and never reach here.
func replaceCTERef(ref *sql.TableRef, cteName string, bound *sql.SelectStmt) error {
	if ref == nil || ref.Subquery != nil {
		return nil
	}
	if ref.Name == cteName || ref.As == cteName {
		ref.Subquery = bound
		ref.Name = ""
		if ref.As == "" {
			ref.As = cteName
		}
	}
	return nil
}

// copyCompoundChain shallow-copies the compound chain starting at head and
// ending just before stop (the first recursive term), re-linking the copies
// so the last anchor term's Union is nil. The copies share expression
// subtrees with the original AST, matching execRecursiveCTE's existing
// shallow-copy approach for a single-term anchor.
func copyCompoundChain(head, stop *sql.SelectStmt) *sql.SelectStmt {
	var newHead, prev *sql.SelectStmt
	for t := head; t != nil && t != stop; t = t.Union {
		cp := *t
		cp.Union = nil
		if newHead == nil {
			newHead = &cp
		} else {
			prev.Union = &cp
		}
		prev = &cp
	}
	return newHead
}

// cteCompoundOps collects the compound operators along the anchor-to-recursive
// boundary: the last anchor term's SetOp (connecting to the first recursive
// term) followed by each recursive term's SetOp (connecting to the next).
func cteCompoundOps(anchor *sql.SelectStmt, recursive []*sql.SelectStmt) ([]sql.SetOp, []bool) {
	var ops []sql.SetOp
	var alls []bool
	// Find the last anchor term: the one whose Union is the first recursive term.
	lastAnchor := anchor
	for lastAnchor != nil && lastAnchor.Union != nil && lastAnchor.Union != recursive[0] {
		lastAnchor = lastAnchor.Union
	}
	if lastAnchor != nil && lastAnchor.Union == recursive[0] {
		ops = append(ops, lastAnchor.SetOp)
		alls = append(alls, lastAnchor.UnionAll)
	}
	for i := 0; i+1 < len(recursive); i++ {
		ops = append(ops, recursive[i].SetOp)
		alls = append(alls, recursive[i].UnionAll)
	}
	return ops, alls
}

// recursiveCTEOp checks that the compound operators connecting the anchor to
// the recursive part (the last anchor term's SetOp) and between recursive
// terms are all the same — all UNION or all UNION ALL — as SQLite requires
// (a mix is reported as "circular reference: NAME"). It returns whether the
// recursive part is UNION (deduplicating). The last recursive term's SetOp is
// SetNone because the parser stores each operator on its left member.
func recursiveCTEOp(anchor *sql.SelectStmt, recursive []*sql.SelectStmt, name string) (bool, error) {
	if len(recursive) == 0 {
		return false, nil
	}
	ops, alls := cteCompoundOps(anchor, recursive)
	if len(ops) == 0 {
		return false, nil
	}
	op, all := ops[0], alls[0]
	for i := 1; i < len(ops); i++ {
		if ops[i] != op || alls[i] != all {
			return false, fmt.Errorf("circular reference: %s", name)
		}
	}
	return op == sql.SetUnion && !all, nil
}

// cteBodyLimitOffset returns the trailing ORDER BY, LIMIT and OFFSET of a CTE
// body compound. SQLite attaches trailing ORDER BY/LIMIT/OFFSET to the last
// compound member, and they apply to the whole compound result — for a
// recursive CTE that means the materialized CTE rows, not any single term
// evaluation.
func cteBodyLimitOffset(cte *sql.CTEDef) ([]sql.OrderByTerm, sql.Expr, sql.Expr) {
	if cte == nil || cte.Select == nil {
		return nil, nil, nil
	}
	last := cte.Select
	for last.Union != nil {
		last = last.Union
	}
	return last.OrderBy, last.Limit, last.Offset
}

// cteOuterIsPassThrough reports whether the outer SELECT is a simple
// "SELECT cols FROM cte" shape that passes CTE rows through unchanged — no
// joins, WHERE, GROUP BY, DISTINCT, ORDER BY, UNION, or aggregates. Only in
// that shape can the outer LIMIT be pushed into the recursive iteration loop
// as a row cap; any filtering or reshaping must see all CTE rows first.
func (e *SelectEngine) cteOuterIsPassThrough(s *sql.SelectStmt) bool {
	return len(s.Joins) == 0 && s.Where == nil && len(s.GroupBy) == 0 &&
		!s.Distinct && len(s.OrderBy) == 0 && s.Union == nil &&
		!e.hasAggregates(s.Columns)
}

// evalLimitCount evaluates a LIMIT/OFFSET expression as a non-negative count;
// nil or unevaluable expressions return fallback.
func evalLimitCount(expr sql.Expr, fallback int64) int64 {
	if expr == nil {
		return fallback
	}
	if n, ok := sql.EvalNumber(expr); ok && n >= 0 {
		return n
	}
	return fallback
}

// outerPushdownCap returns the outer query's limit cap for a pass-through
// SELECT (body offset + outer OFFSET + outer LIMIT), or -1 when the outer
// cannot be pushed down — any filtering or reshaping must see all CTE rows
// first.
func (e *SelectEngine) outerPushdownCap(s *sql.SelectStmt, bodyM int64) int64 {
	if !e.cteOuterIsPassThrough(s) || s.Limit == nil {
		return -1
	}
	lExpr, lErr := e.evalLimitExpr(s.Limit)
	if lErr != nil {
		return -1
	}
	l, ok := sql.EvalNumber(lExpr)
	if !ok || l < 0 {
		return -1
	}
	o := int64(0)
	if s.Offset != nil {
		oExpr, oErr := e.evalLimitExpr(s.Offset)
		if oErr != nil {
			return -1
		}
		if m, ok2 := sql.EvalNumber(oExpr); ok2 && m > 0 {
			o = m
		}
	}
	return bodyM + o + l
}

// cteRowBudget computes the maximum number of rows the recursive iteration may
// produce before stopping (-1 means no cap). The CTE body's trailing
// LIMIT/OFFSET is the primary cap (SQLite applies it to the materialized CTE
// rows); for a simple pass-through outer query the outer LIMIT/OFFSET is also
// pushed down. With a body OFFSET the outer cap applies to the body's output
// rows, so the iteration must produce the body offset plus the outer window.
func (e *SelectEngine) cteRowBudget(s *sql.SelectStmt, cte *sql.CTEDef) (budget int64, bodyLimit, bodyOffset sql.Expr) {
	_, bodyLimit, bodyOffset = cteBodyLimitOffset(cte)
	// The body's LIMIT/OFFSET are applied inline during the queue-based
	// iteration (SQLite's computeLimitRegisters on the CTE body compound), so
	// the budget here is only the outer pass-through LIMIT/OFFSET cap — the
	// number of rows the outer query needs from the CTE.
	budget = -1
	bodyM := evalLimitCount(bodyOffset, 0)
	if cap := e.outerPushdownCap(s, bodyM); cap >= 0 {
		budget = cap
	}
	return budget, bodyLimit, bodyOffset
}

// recursiveRowQueue is the queue used by the recursive CTE iteration,
// mirroring SQLite's generateWithRecursiveQuery: a FIFO when the CTE body has
// no ORDER BY, and a priority queue (rows kept sorted by the ORDER BY terms,
// smallest first) when it does. Dequeue order is therefore the CTE output
// order: iteration order for FIFO, ORDER BY order for the priority queue.
type recursiveRowQueue struct {
	rows       [][]interface{}
	orderBy    []sql.OrderByTerm
	colDefs    []sql.ColumnDef
	resultCols []string // compound result column names (anchor output names)
	e          *SelectEngine
}

func newRecursiveRowQueue(anchorRows [][]interface{}, colDefs []sql.ColumnDef, resultCols []string, orderBy []sql.OrderByTerm, e *SelectEngine) *recursiveRowQueue {
	q := &recursiveRowQueue{orderBy: orderBy, colDefs: colDefs, resultCols: resultCols, e: e}
	if len(orderBy) == 0 {
		q.rows = append(q.rows, anchorRows...)
		return q
	}
	for _, r := range anchorRows {
		q.enqueue(r)
	}
	return q
}

func (q *recursiveRowQueue) empty() bool { return len(q.rows) == 0 }

func (q *recursiveRowQueue) dequeue() []interface{} {
	r := q.rows[0]
	q.rows = q.rows[1:]
	return r
}

func (q *recursiveRowQueue) enqueue(row []interface{}) {
	if len(q.orderBy) == 0 {
		q.rows = append(q.rows, row)
		return
	}
	idx := sort.Search(len(q.rows), func(i int) bool {
		return q.e.recursiveRowLess(q.orderBy, q.colDefs, q.resultCols, row, q.rows[i])
	})
	q.rows = append(q.rows, nil)
	copy(q.rows[idx+1:], q.rows[idx:])
	q.rows[idx] = row
}

// enqueueDeduped appends produced rows, skipping duplicates when dedup is set.
func (q *recursiveRowQueue) enqueueDeduped(rows [][]interface{}, dedup bool, seen map[string]bool) {
	for _, pr := range rows {
		if dedup {
			k := rowKey(pr, nil)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		q.enqueue(pr)
	}
}

// recursiveRowLess reports whether row a sorts before row b per the ORDER BY
// terms, using the same comparison machinery as a normal ORDER BY sort. The
// ORDER BY resolves against the compound's result column names (resultCols,
// from the anchor output) — not the declared CTE column names — matching
// SQLite, which rejects a body ORDER BY that names a declared CTE column that
// is not a result column (with1 10.7.1).
func (e *SelectEngine) recursiveRowLess(orderBy []sql.OrderByTerm, colDefs []sql.ColumnDef, resultCols []string, a, b []interface{}) bool {
	rows := [][]interface{}{a, b}
	maps := []RowMap{
		buildRowMapFromValues(a, colDefs, 1),
		buildRowMapFromValues(b, colDefs, 1),
	}
	if len(resultCols) == 0 {
		resultCols = make([]string, len(colDefs))
		for i := range colDefs {
			resultCols[i] = colDefs[i].Name
		}
	}
	return e.lessRows(orderBy, maps, rows, resultCols, 0, 1)
}

// iterateRecursiveCTE runs the recursive CTE iteration loop using SQLite's
// queue algorithm (generateWithRecursiveQuery): rows are dequeued from a queue
// (FIFO, or priority-ordered when the body has an ORDER BY), output with the
// body's LIMIT/OFFSET applied inline during dequeue, and expanded through the
// recursive terms back into the queue. A UNION recursive part (dedup) skips
// rows already seen (which also guarantees termination once the row set
// stabilizes). The loop stops when the queue empties, the body LIMIT is
// reached, or the outer pushdown budget (rowLimit, counting dequeued rows) is
// hit. Rows are returned in dequeue order — for an ORDER BY body that is
// already the sorted result.
func (e *SelectEngine) iterateRecursiveCTE(anchorRows [][]interface{}, colDefs []sql.ColumnDef, resultCols []string, cte *sql.CTEDef, recursiveTerms []*sql.SelectStmt, dedup bool, rowLimit int64, bodyOrderBy []sql.OrderByTerm, bodyLimit, bodyOffset sql.Expr) ([][]interface{}, error) {
	// For a UNION recursive part (dedup), the anchor rows are also deduplicated
	// against each other (SQLite's iDistinct covers the setup rows too — with1
	// 26.2's anchor "SELECT * FROM t" over two identical rows collapses to
	// one).
	if dedup {
		anchorRows = dedupRecursiveRows(anchorRows)
	}
	queue := newRecursiveRowQueue(anchorRows, colDefs, resultCols, bodyOrderBy, e)
	seen := make(map[string]bool, len(anchorRows))
	if dedup {
		for _, r := range anchorRows {
			seen[rowKey(r, nil)] = true
		}
	}
	maxIter := e.ctx.RecursiveCTELimit()
	if maxIter <= 0 {
		maxIter = 100000
	}
	offsetSkip := evalLimitCount(bodyOffset, 0)
	limitLeft := evalLimitCount(bodyLimit, -1)
	var allRows [][]interface{}
	dequeued := int64(0)
	// Fast path: a single recursive term shaped "FROM <base> JOIN <cte> ON
	// base.col = cte.col" (or reversed) degenerates into one full base scan
	// per dequeued row through the generic path — O(rows x scan) on large
	// tables. Pre-hash the base table once and probe it per dequeued row
	// instead (SQLite uses the base table's index for the same effect).
	if len(recursiveTerms) == 1 {
		if fp := e.newRecursiveJoinFastPath(recursiveTerms[0], colDefs, cte.Name); fp != nil {
			for iter := 0; iter < maxIter && !queue.empty(); iter++ {
				if err := e.ctx.CheckProgress(); err != nil {
					return nil, err
				}
				row := queue.dequeue()
				dequeued++
				if offsetSkip > 0 {
					offsetSkip--
				} else {
					allRows = append(allRows, row)
					if limitLeft >= 0 {
						limitLeft--
						if limitLeft == 0 {
							return allRows, nil
						}
					}
				}
				if rowLimit >= 0 && dequeued >= rowLimit {
					return allRows, nil
				}
				produced, err := fp(row)
				if err != nil {
					return nil, err
				}
				queue.enqueueDeduped(produced, dedup, seen)
			}
			return allRows, nil
		}
	}
	for iter := 0; iter < maxIter && !queue.empty(); iter++ {
		if err := e.ctx.CheckProgress(); err != nil {
			return nil, err
		}
		row := queue.dequeue()
		dequeued++
		if offsetSkip > 0 {
			offsetSkip--
		} else {
			allRows = append(allRows, row)
			if limitLeft >= 0 {
				limitLeft--
				if limitLeft == 0 {
					// The body LIMIT is reached: stop without expanding the last
					// output row (SQLite jumps to break before the recursive
					// step when the LIMIT counter hits zero).
					return allRows, nil
				}
			}
		}
		if rowLimit >= 0 && dequeued >= rowLimit {
			return allRows, nil
		}
		produced, err := e.evalRecursiveTerms(recursiveTerms, row, colDefs, cte)
		if err != nil {
			return nil, err
		}
		queue.enqueueDeduped(produced, dedup, seen)
	}
	return allRows, nil
}

// dedupRecursiveRows removes duplicate rows from a recursive CTE anchor (used
// for a UNION recursive part).
func dedupRecursiveRows(rows [][]interface{}) [][]interface{} {
	seen := make(map[string]bool, len(rows))
	deduped := make([][]interface{}, 0, len(rows))
	for _, r := range rows {
		k := rowKey(r, nil)
		if seen[k] {
			continue
		}
		seen[k] = true
		deduped = append(deduped, r)
	}
	return deduped
}

// recJoinFastPath evaluates one recursive-term iteration against a pre-hashed
// base table. Produced rows match the term's column order exactly.
type recJoinFastPath func(row []interface{}) ([][]interface{}, error)

// newRecursiveJoinFastPath detects the dominant recursive-CTE shape
//
//	SELECT <exprs> FROM <base> JOIN <cte> ON <base>.<a> = <cte>.<b>
//	-- or with the operands reversed --
//
// (single base table, single join, plain equality, no WHERE/LIMIT/GROUP BY)
// and returns a closure that produces the term's rows for one dequeued CTE
// row by probing a hash index built over the base table once. nil is returned
// when the term does not fit the pattern; callers fall back to the generic
// per-row execSelect path.
func (e *SelectEngine) newRecursiveJoinFastPath(term *sql.SelectStmt, colDefs []sql.ColumnDef, cteName string) recJoinFastPath {
	if term == nil || term.Where != nil || term.Limit != nil || term.Offset != nil ||
		len(term.GroupBy) > 0 || len(term.Joins) != 1 {
		return nil
	}
	base := term.From
	jc := term.Joins[0]
	cteRef := jc.Table
	if base.Subquery != nil || len(base.Args) > 0 || base.Name == "" {
		if base.Name == cteName {
			// Reversed roles: the CTE is the FROM operand.
			base, cteRef = cteRef, base
			if base.Subquery != nil || len(base.Args) > 0 || base.Name == "" {
				return nil
			}
		} else {
			return nil
		}
	} else if cteRef.Name != cteName && cteRef.As != cteName {
		return nil
	}
	if cteRef.Subquery != nil {
		return nil
	}
	eq, ok := jc.On.(*sql.BinaryOp)
	if !ok || eq.Operator != "=" {
		return nil
	}
	lhs, lok := eq.Left.(*sql.ColumnRef)
	rhs, rok := eq.Right.(*sql.ColumnRef)
	if !lok || !rok {
		return nil
	}
	baseAlias := strings.ToLower(base.As)
	if baseAlias == "" {
		baseAlias = strings.ToLower(base.Name)
	}
	cteAlias := strings.ToLower(cteRef.As)
	if cteAlias == "" {
		cteAlias = cteName
	}
	// Classify which side names the base table and which the CTE.
	var baseCol, cteCol string
	classify := func(cr *sql.ColumnRef) (side string, col string) {
		tbl := ""
		name := cr.Name
		if cr.Table != "" {
			tbl = strings.ToLower(cr.Table)
			if dot := strings.LastIndex(tbl, "."); dot >= 0 {
				tbl = tbl[dot+1:]
			}
		}
		if tbl == "" {
			// Unqualified: decide by membership in the CTE's columns.
			for _, cd := range colDefs {
				if strings.EqualFold(cd.Name, name) {
					return "cte", name
				}
			}
			return "base", name
		}
		if tbl == cteAlias || tbl == strings.ToLower(cteName) {
			return "cte", name
		}
		if tbl == baseAlias {
			return "base", name
		}
		return "", ""
	}
	s1, c1 := classify(lhs)
	s2, c2 := classify(rhs)
	if s1 == "" || s2 == "" || s1 == s2 {
		return nil
	}
	if s1 == "base" {
		baseCol, cteCol = c1, c2
	} else {
		baseCol, cteCol = c2, c1
	}

	// Materialize the base table once and build the probe hash.
	baseSel := &sql.SelectStmt{
		Columns: []sql.SelectColumn{{Expr: &sql.ColumnRef{Name: "*"}}},
		From:    sql.TableRef{Name: base.Name},
	}
	res := e.execSelect(baseSel)
	if res.Error != nil {
		return nil
	}
	baseMaps := res.rowMaps
	hash := make(map[interface{}][]RowMap, len(baseMaps))
	for _, rm := range baseMaps {
		cv, found := lookupRowMapCol(rm, baseCol, baseAlias)
		if !found {
			continue
		}
		key := joinIndexKey(cv)
		hash[key] = append(hash[key], rm)
	}
	// The expression list to evaluate per produced combination.
	exprs := make([]sql.Expr, 0, len(term.Columns))
	for _, col := range term.Columns {
		exprs = append(exprs, col.Expr)
	}
	return func(row []interface{}) ([][]interface{}, error) {
		cteMap := make(RowMap, len(colDefs)*2)
		for i, cd := range colDefs {
			var v interface{}
			if i < len(row) {
				v = row[i]
			}
			wrapped := &util.ColumnValue{Value: v}
			cteMap[strings.ToLower(cd.Name)] = wrapped
			cteMap[strings.ToLower(cteName)+"."+strings.ToLower(cd.Name)] = wrapped
			if cteAlias != strings.ToLower(cteName) {
				cteMap[cteAlias+"."+strings.ToLower(cd.Name)] = wrapped
			}
		}
		var probeKey interface{}
		if cv, found := lookupRowMapCol(cteMap, cteCol, ""); found {
			probeKey = joinIndexKey(cv)
		} else if cv, found := cteMap[strings.ToLower(cteCol)]; found {
			probeKey = joinIndexKey(cv)
		} else {
			return nil, nil
		}
		matches := hash[probeKey]
		outRows := make([][]interface{}, 0, len(matches))
		for _, brm := range matches {
			combined := make(RowMap, len(brm)+len(cteMap))
			for k, v := range brm {
				combined[k] = v
				if baseAlias != "" && !strings.Contains(k, ".") {
					combined[baseAlias+"."+k] = v
				}
			}
			for k, v := range cteMap {
				combined[k] = v
			}
			out := make([]interface{}, len(exprs))
			for i, ex := range exprs {
				v, err := e.ctx.EvalExpr(ex, combined)
				if err != nil {
					return nil, err
				}
				out[i] = util.UnwrapColumnValue(v)
			}
			outRows = append(outRows, out)
		}
		return outRows, nil
	}
}

// lookupRowMapCol fetches a column value trying the bare name and the
// alias-qualified form. Returns the raw stored value (any type).
func lookupRowMapCol(rm RowMap, col, alias string) (interface{}, bool) {
	if cv, ok := rm[col]; ok {
		return cv, true
	}
	if alias != "" {
		if cv, ok := rm[alias+"."+col]; ok {
			return cv, true
		}
	}
	return nil, false
}
