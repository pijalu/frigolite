package execquery

import (
	"fmt"
	"math"

	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// --- SELECT ---

// handleSelectAggregates evaluates aggregates. Returns the result if aggregates
// were processed and a result is available, or nil if no aggregates or empty result.
func (e *SelectEngine) handleSelectAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	// A column that is a correlated-aggregate scalar subquery (e.g. (SELECT
	// max(y)) where y resolves to the outer query) makes the query an
	// aggregate query: the inner aggregate collapses the query to one row per
	// GROUP BY group (window1 76.5). This mirrors SQLite's SF_Aggregate
	// marking when a subquery's aggregate references an outer column.
	hasAggs := e.hasAggregates(s.Columns) || e.hasSubqueryWithCorrelatedAgg(s.Columns)
	if hasAggs {
		if len(s.GroupBy) > 0 {
			// When a covering index exists for every column the aggregate
			// GROUP BY query references, SQLite scans via that index, so the
			// rows arrive in index key order. group_concat() (and other
			// order-sensitive aggregates) then accumulate in that order —
			// e.g. SELECT group_concat(one) FROM b1 GROUP BY (one>4) over a
			// PK on one emits 1,2,3,4 not the insertion order 1,4,3,2
			// (e_select-4.9.2). Reorder the scanned rows to match.
			if idxCols := e.coveringIndexForAggregate(s); len(idxCols) > 0 && len(rowMaps) > 1 {
				reorderMapsByIndex(rowMaps, idxCols)
			}
			result := e.evalAggregatesGroupBy(s, rowMaps, colDefs)
			if result != nil {
				return result
			}
		} else {
			result := e.aggs.EvalAggregates(s, rowMaps, colDefs)
			if result != nil {
				return result
			}
		}
	} else if len(s.GroupBy) > 0 {
		// GROUP BY without aggregates: group rows, build output rows using buildOutputRow
		return e.evalGroupByNoAggs(s, rowMaps, colDefs)
	}
	return nil
}

// reorderMapsByIndex sorts row maps into the given index column order,
// matching the order SQLite's covering-index scan would produce.
func reorderMapsByIndex(maps []RowMap, idxCols []string) {
	sort.SliceStable(maps, func(i, j int) bool {
		return comparePairsByIndex(maps[i], maps[j], idxCols) < 0
	})
}

// sortGroupKeys orders GROUP BY output groups by their evaluated key values,
// matching SQLite's sorted-group output (NULL sorts first).
func (e *SelectEngine) sortGroupKeys(keyOrder []string, keyVals map[string][]interface{}) {
	if len(keyOrder) < 2 {
		return
	}
	sort.SliceStable(keyOrder, func(i, j int) bool {
		a := keyVals[keyOrder[i]]
		b := keyVals[keyOrder[j]]
		n := len(a)
		if len(b) < n {
			n = len(b)
		}
		for k := 0; k < n; k++ {
			c := util.CompareValues(a[k], b[k])
			if c != 0 {
				return c < 0
			}
		}
		return len(a) < len(b)
	})
}

// buildIndexSQL builds the SQL string for creating an index.
func BuildIndexSQL(name, table string, columns []sql.IndexColumn, unique bool, where sql.Expr) string {
	var buf strings.Builder
	buf.WriteString("CREATE ")
	if unique {
		buf.WriteString("UNIQUE ")
	}
	buf.WriteString("INDEX ")
	buf.WriteString(name)
	buf.WriteString(" ON ")
	buf.WriteString(table)
	buf.WriteString("(")
	for i, col := range columns {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(col.Name)
		if col.Desc {
			buf.WriteString(" DESC")
		}
	}
	buf.WriteString(")")
	// Add WHERE clause for partial indexes
	if where != nil {
		buf.WriteString(" WHERE ")
		buf.WriteString(sql.ExprString(where))
	}
	return buf.String()
}

// execSelectFrom dispatches the non-real-table FROM forms: SELECT without
// FROM, a subquery in FROM, a CTE, a table-valued pragma function, and a
// table-valued virtual-table function. Returns handled=true when the result
// is complete.
func (e *SelectEngine) execSelectFrom(s *sql.SelectStmt) (*Result, bool) {
	if isNoFromTerm(s) {
		return e.execSelectNoFrom(s), true
	}

	// Handle subquery in FROM: (SELECT ...) AS t
	if s.From.Subquery != nil {
		return e.execSelectFromSubquery(s), true
	}

	// A FROM term written with function-call syntax that resolves to an
	// ordinary CTE, table or view is SQLite resolve.c's "'%s' is not a
	// function" (tabfunc01-1.21/1.23/1.25/1.26). Registered vtab modules and
	// pragma table functions are genuine table-valued functions and proceed.
	// An fts5 table is table-valued too: FROM t1('query') is the MATCH TVF
	// form (fts5_main.c), so it is consumed before the not-a-function check.
	if fromIsTabFuncWithArgs(s) {
		if res, handled := e.execFTS5TableFunc(s.From, s); handled {
			return res, true
		}
	}
	if res, handled := e.execFTS4TabFuncForm(s); handled {
		return res, true
	}
	if res, handled, consumed := e.execCreatedVTabFuncForm(s); handled {
		return res, consumed
	}
	if res, handled := e.notATabFuncError(s); handled {
		return res, true
	}

	// Handle CTE: check if the from table matches a CTE definition (either
	// declared on this statement or in an enclosing WITH clause).
	if res, handled := e.execCTEFromForm(s); handled {
		return res, true
	}

	// Table-valued pragma functions: FROM pragma_table_info('t1')
	if isPragmaTableFunc(s.From.Name) {
		return e.ctx.ExecPragmaTableValued(s), true
	}

	if res, handled := e.execVTabTableFuncForm(s); handled {
		return res, true
	}
	if res, handled := e.execEponymousVTabForm(s); handled {
		return res, true
	}
	return e.execCreatedVTabForm(s)
}

// isNoFromTerm reports whether the FROM term is absent (SELECT without FROM).
// EmptyName marks the quoted empty table name ("FROM \"\""), which is a real
// FROM term naming the zero-length table (tkt-78e04e52ea).
func isNoFromTerm(s *sql.SelectStmt) bool {
	return s.From.Name == "" && !s.From.EmptyName && s.From.Subquery == nil && len(s.From.As) == 0
}

// fromIsTabFuncWithArgs reports whether the FROM term uses table-valued
// function syntax with actual arguments.
func fromIsTabFuncWithArgs(s *sql.SelectStmt) bool {
	return s.From.IsTabFunc && len(s.From.Args) > 0
}

// execFTS4TabFuncForm handles an FTS3/4 table in TVF form (FROM t1('query')):
// the argument binds as a MATCH constraint on the table's hidden column —
// xBestIndex sees it like any MATCH. The TVF form is stripped and the
// constraint ANDed into the WHERE so the statement flows through the dedicated
// FTS scan (fts4content 12.1.3/12.2.3: self-referential content sources fail
// the read with "SQL logic error" in either form). handled=false leaves the
// statement untouched for the next FROM form.
func (e *SelectEngine) execFTS4TabFuncForm(s *sql.SelectStmt) (*Result, bool) {
	if !s.From.IsTabFunc || len(s.From.Args) == 0 {
		return nil, false
	}
	entry, _, terr := e.ctx.FindTable(s.From.Name)
	if terr != nil || entry == nil || entry.RootPage != 0 {
		return nil, false
	}
	if _, isFTS := e.ctx.FTSTables()[entry.Name]; !isFTS {
		return nil, false
	}
	if lit, ok := s.From.Args[0].(*sql.StringLit); ok {
		match := &sql.BinaryOp{Operator: "MATCH", Left: &sql.ColumnRef{Name: entry.Name}, Right: lit}
		if s.Where == nil {
			s.Where = match
		} else {
			s.Where = &sql.BinaryOp{Operator: "AND", Left: s.Where, Right: match}
		}
	}
	s.From.IsTabFunc = false
	s.From.Args = nil
	return nil, false
}

// execCreatedVTabFuncForm handles a FROM term naming a CREATED virtual table
// (CREATE VIRTUAL TABLE entry): the arguments bind to the leftmost HIDDEN
// columns as equality constraints (SQLite's vtab TVF form, e.g.
// FROM fts5tokenize-t('text')). consumed reports whether the statement was
// handled (true) or stripped in place (false).
func (e *SelectEngine) execCreatedVTabFuncForm(s *sql.SelectStmt) (res *Result, handled, consumed bool) {
	if !s.From.IsTabFunc || len(s.From.Args) == 0 {
		return nil, false, false
	}
	opts := e.vtabScanOptions(s)
	residual := opts.Where
	opts.Residual = &residual
	defs, rows, rowids, err, ok := e.ctx.MaterializeCreatedVTabFunc(s.From, opts)
	if !ok {
		return nil, false, false
	}
	if err != nil {
		return &Result{Error: err}, true, true
	}
	return e.execSelectOverMaterializedRowids(e.withVtabResidualWhere(s, &opts), defs, rows, rowids), true, true
}

// notATabFuncError reports resolve.c's "'%s' is not a function" when a TVF-
// form FROM term names a non-table-valued relation. An unknown name falls
// through for the normal "no such table" path.
func (e *SelectEngine) notATabFuncError(s *sql.SelectStmt) (*Result, bool) {
	if !s.From.IsTabFunc || isPragmaTableFunc(s.From.Name) {
		return nil, false
	}
	if _, isModule := e.ctx.VTables().Find(strings.ToLower(s.From.Name)); isModule {
		return nil, false
	}
	if e.relationExists(s, s.From.Name) {
		return &Result{Error: fmt.Errorf("'%s' is not a function", s.From.Name)}, true
	}
	return nil, false
}

// execCTEFromForm dispatches a FROM term that matches a CTE definition,
// directly or through its alias.
func (e *SelectEngine) execCTEFromForm(s *sql.SelectStmt) (*Result, bool) {
	if cte, ok := e.findCTE(s, s.From.Name); ok {
		return e.execSelectCTE(s, &cte), true
	}
	if s.From.As != "" {
		if cte, ok := e.findCTE(s, s.From.As); ok {
			return e.execSelectCTE(s, &cte), true
		}
	}
	return nil, false
}

// execVTabTableFuncForm handles the table-valued virtual-table function form
// (FROM generate_series(1,256)), including the correlated FROM-TVF case
// inside a subquery: the argument references an outer row's columns
// (EXISTS(SELECT 1 FROM json_each(t1.json,...))), so the arguments evaluate
// against that outer row (SQLite runs the vtab filter per outer row).
func (e *SelectEngine) execVTabTableFuncForm(s *sql.SelectStmt) (*Result, bool) {
	if len(s.From.Args) == 0 {
		return nil, false
	}
	if e.outerRow != nil && pragmaArgsCorrelated(s.From) {
		if colDefs, rows, err := e.ctx.MaterializeVtabTableFuncInRow(s.From, e.outerRow); err == nil {
			return e.execSelectOverMaterialized(s, colDefs, rows), true
		} else if !isNoSuchVtabErr(err) {
			return &Result{Error: err}, true
		}
	}
	opts := e.vtabScanOptions(s)
	residual := opts.Where
	opts.Residual = &residual
	if colDefs, rows, rowids, err := e.ctx.MaterializeVtabTableFunc(s.From, opts); err == nil {
		return e.execSelectOverMaterializedRowids(e.withVtabResidualWhere(s, &opts), colDefs, rows, rowids), true
	} else if !isNoSuchVtabErr(err) {
		return &Result{Error: err}, true
	}
	return nil, false
}

// execEponymousVTabForm handles an eponymous virtual table (FROM
// generate_series with no arguments) with hidden-column constraints in WHERE
// (series.c, tabfunc01-1.1).
func (e *SelectEngine) execEponymousVTabForm(s *sql.SelectStmt) (*Result, bool) {
	opts := e.vtabScanOptions(s)
	residual := opts.Where
	opts.Residual = &residual
	defs, rows, rowids, err, handled := e.ctx.TryMaterializeEponymousVtab(s.From, opts)
	if !handled {
		return nil, false
	}
	if err != nil {
		return &Result{Error: err}, true
	}
	return e.execSelectOverMaterializedRowids(e.withVtabResidualWhere(s, &opts), defs, rows, rowids), true
}

// execCreatedVTabForm handles created virtual tables (CREATE VIRTUAL TABLE
// ... USING csv etc.): RootPage 0 schema entries whose stored SQL names a
// registered module.
func (e *SelectEngine) execCreatedVTabForm(s *sql.SelectStmt) (*Result, bool) {
	opts := e.vtabScanOptions(s)
	createdResidual := opts.Where
	opts.Residual = &createdResidual
	if len(s.Joins) != 0 {
		return nil, false
	}
	defs, rows, rowids, err, ok := e.ctx.MaterializeCreatedVTab(s.From.Name, opts)
	if !ok {
		return nil, false
	}
	if err != nil {
		return &Result{Error: err}, true
	}
	// A WITHOUT ROWID declared schema rejects rowid references
	// (csv01 3.2) — checked only after the claim succeeds so real
	// WITHOUT ROWID tables keep their normal path.
	if e.ctx.WithoutRowidVTab(s.From.Name) && selectReferencesRowID(s) {
		return &Result{Error: fmt.Errorf("no such column: rowid")}, true
	}
	return e.execSelectOverMaterializedRowids(e.withVtabResidualWhere(s, &opts), defs, rows, rowids), true
}

// withVtabResidualWhere returns a shallow statement copy whose WHERE is the
// residual clause left after the materializer consumed (omitted) vtab
// constraints; s itself when nothing was consumed.
func (e *SelectEngine) withVtabResidualWhere(s *sql.SelectStmt, opts *VtabScanOptions) *sql.SelectStmt {
	if opts.Residual == nil || *opts.Residual == s.Where {
		return s
	}
	ns := *s
	ns.Where = *opts.Residual
	return &ns
}

// vtabScanOptions builds the materialization options for a FROM-clause
// virtual-table reference: WHERE pushdown plus the LIMIT/OFFSET row cap
// (series.c consumes LIMIT via xBestIndex; an eager materializer must stop
// generating once the cap is reached). The planning fields (OrderBy..)
// populate unconditionally so prepare-time xBestIndex planning
// (BuildVtabIndexInfo — where.c allocateIndexInfo parity) sees the whole
// statement.
func (e *SelectEngine) vtabScanOptions(s *sql.SelectStmt) VtabScanOptions {
	opts := VtabScanOptions{
		Where:          s.Where,
		MaxRows:        -1,
		OrderBy:        s.OrderBy,
		GroupBy:        s.GroupBy,
		Having:         s.Having,
		Distinct:       s.Distinct,
		HasAggregate:   e.hasAggregate(s),
		Limit:          s.Limit,
		Offset:         s.Offset,
		RefColumnNames: collectVtabRefCols(s),
		RefAllColumns:  selectStarCoversVtab(s),
	}
	// The row cap is only safe for unbounded generator modules (series,
	// wholenumber) whose ValueRangeNarrower bounds the scan; for every other
	// virtual table the LIMIT must apply AFTER residual WHERE filtering and
	// ORDER BY, otherwise pre-filter rows are counted first
	// (amatch1-1.1: fts4aux LIMIT 5 with term>'b' returned pre-filter rows).
	pushdown := false
	if m, ok := e.ctx.VTables().Find(strings.ToLower(s.From.Name)); ok {
		// Only modules declaring unbounded output keep the pushdown; all
		// others (and created-vtab entries) materialize fully so residual
		// WHERE / ORDER BY still apply.
		if lp, declares := m.(vtab.LimitPushdown); declares && lp.NeedsLimitPushdown() {
			pushdown = true
		}
	}
	if !pushdown {
		return opts
	}
	limit, limOK := e.constIntExpr(s.Limit)
	if !limOK || limit < 0 {
		return opts
	}
	opts.MaxRows = limit
	if off, offOK := e.constIntExpr(s.Offset); offOK && off > 0 {
		opts.MaxRows += off
	}
	return opts
}

// collectVtabRefCols lists every column name the statement references for
// its single FROM term: refs whose Table qualifier is empty or names the
// FROM table/alias (case-insensitive). Names are collected from the select
// list, WHERE, ORDER BY, GROUP BY, and HAVING so the planner can intersect
// them with the vtab's declared columns to build colUsed; unresolved names
// are included (they simply never intersect).
func collectVtabRefCols(s *sql.SelectStmt) []string {
	var names []string
	seen := make(map[string]bool)
	add := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		WalkExprFull(expr, func(e2 sql.Expr) {
			cr, ok := e2.(*sql.ColumnRef)
			if !ok || !vtabRefQualifierMatches(cr, s) || cr.Name == "*" {
				return
			}
			key := strings.ToLower(cr.Name)
			if seen[key] {
				return
			}
			seen[key] = true
			names = append(names, cr.Name)
		})
	}
	for _, col := range s.Columns {
		add(col.Expr)
	}
	add(s.Where)
	for _, ob := range s.OrderBy {
		add(ob.Expr)
	}
	for _, gb := range s.GroupBy {
		add(gb)
	}
	add(s.Having)
	return names
}

// vtabRefQualifierMatches reports whether a column reference's Table
// qualifier names the statement's FROM term (or is unqualified).
func vtabRefQualifierMatches(cr *sql.ColumnRef, s *sql.SelectStmt) bool {
	if cr.Table == "" {
		return true
	}
	return strings.EqualFold(cr.Table, s.From.Name) || strings.EqualFold(cr.Table, s.From.As)
}

// selectStarCoversVtab reports whether the select list projects a wildcard
// for the FROM term: a bare `*` or a `t.*` whose qualifier matches the FROM
// table/alias (both parse to ColumnRef{Name:"*"} — parse rules 103/104).
func selectStarCoversVtab(s *sql.SelectStmt) bool {
	for _, col := range s.Columns {
		cr, ok := col.Expr.(*sql.ColumnRef)
		if ok && cr.Name == "*" && vtabRefQualifierMatches(cr, s) {
			return true
		}
	}
	return false
}

// constIntExpr evaluates expr as a constant integer; ok is false for nil or
// non-numeric expressions.
func (e *SelectEngine) constIntExpr(expr sql.Expr) (int64, bool) {
	if expr == nil {
		return 0, false
	}
	v, err := e.ctx.EvalExpr(expr, nil)
	if err != nil {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// resolveFromTable looks up the FROM table; when it is actually a view,
// executes the view (with circular-reference tracking). Returns a non-nil
// Result when the view path completed.
func (e *SelectEngine) resolveFromTable(s *sql.SelectStmt) (*schema.Entry, *DatabaseContext, *Result) {
	tableEntry, dbCtx, err := e.ctx.FindTable(s.From.Name)
	if err == nil {
		return tableEntry, dbCtx, nil
	}
	// select.c selectExpander (ticket d58ccbb3f1b): every FROM-term
	// reference increments the view's Table.nTabRef; a view expanded more
	// than 65535 times in one statement aborts name resolution with
	// "too many references to \"%s\": max 65535" (view3 1.1). The count is
	// LIVE references: released when the view's own expansion finishes.
	if e.viewRefCounts == nil {
		e.viewRefCounts = make(map[string]int)
	}
	viewRefKey := strings.ToUpper(s.From.Name)
	if e.viewRefCounts[viewRefKey] >= 0xffff {
		return nil, nil, &Result{Error: fmt.Errorf("too many references to %q: max 65535", s.From.Name)}
	}
	e.viewRefCounts[viewRefKey]++
	viewEntry, viewCtx, viewErr := e.ctx.FindView(s.From.Name)
	if viewErr != nil {
		// SQLite prefixes a missing table in a main-schema view's body
		// with "main." ("no such table: main.txx", alterlegacy-3.1.2b);
		// temp-schema views use the bare name (alterlegacy-3.3.1).
		if s.From.Name != "" && !strings.HasPrefix(err.Error(), "no such table: main.") {
			if e.expandingView && !e.expandingTempView {
				return nil, nil, &Result{Error: fmt.Errorf("no such table: main.%s", s.From.Name)}
			}
		}
		return nil, nil, &Result{Error: err}
	}
	// Check for circular view reference
	if e.resolvingViews[s.From.Name] {
		return nil, nil, &Result{Error: fmt.Errorf("view %s is circularly defined", s.From.Name)}
	}
	if e.resolvingViews == nil {
		e.resolvingViews = make(map[string]bool)
	}
	e.resolvingViews[s.From.Name] = true
	result := e.execSelectViewWithOuter(s, viewEntry, viewCtx)
	delete(e.resolvingViews, s.From.Name)
	return nil, nil, result
}

// execSelectCorrelatedAgg re-evaluates the SELECT columns with outerRows set
// to all rowMaps when a column contains a subquery with a correlated
// aggregate. Returns nil when normal handling should continue.
func (e *SelectEngine) execSelectCorrelatedAgg(s *sql.SelectStmt, allRowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if !e.hasSubqueryWithCorrelatedAgg(s.Columns) {
		return nil
	}
	// A GROUP BY query with a correlated-aggregate subquery column is an
	// aggregate query: the grouping happens in evalAggregatesGroupBy (per
	// group), not by collapsing the whole query to one row (window1 76.5:
	// SELECT (SELECT max(y)) FROM t LEFT JOIN u ON x=y GROUP BY x → one
	// output row per group).
	if len(s.GroupBy) > 0 {
		return nil
	}
	if len(allRowMaps) == 0 {
		// A FROM-less correlated aggregate in a column collapses the outer
		// query to ONE row even when the scan is empty (SQLite aggregates
		// over the empty input produce NULL / the window over the single
		// collapsed row). window1 44.3.2: SELECT (0,0) IN(SELECT MIN(c0),
		// NTILE(1) OVER()) FROM t0 with t0 empty → one row 0.
		emptyRow := RowMap{}
		prevOuterRows := e.outerRows
		prevOuterRow := e.outerRow
		e.outerRows = []RowMap{emptyRow}
		e.outerRow = emptyRow
		outRow, err := e.buildOutputRow(s.Columns, colDefs, emptyRow)
		if err != nil {
			return &Result{Error: err}
		}
		e.outerRows = prevOuterRows
		e.outerRow = prevOuterRow
		columns := e.buildColumnNames(s.Columns, colDefs, s)
		result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
		return e.finalizeSelectResult(result, s, []RowMap{emptyRow})
	}
	prevOuterRows := e.outerRows
	prevOuterRow := e.outerRow
	e.outerRows = allRowMaps
	e.outerRow = allRowMaps[0] // provide first row for non-aggregate column refs
	outRow, err := e.buildOutputRow(s.Columns, colDefs, allRowMaps[0])
	if err != nil {
		return &Result{Error: err}
	}
	e.outerRows = prevOuterRows
	e.outerRow = prevOuterRow
	columns := e.buildColumnNames(s.Columns, colDefs, s)
	result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// execSelectJoins processes the nested-loop joins for a SELECT: validates
// ambiguous column references, runs execJoins, applies the statement-level
// WHERE filter, and rebuilds the output rows. Returns the updated rows, row
// maps, and column defs (or the error).
func (e *SelectEngine) execSelectJoins(s *sql.SelectStmt, allRowMaps []RowMap, colDefs []sql.ColumnDef) ([][]interface{}, []RowMap, []sql.ColumnDef, error) {
	// SQLite rejects unqualified column references that are ambiguous
	// across the joined tables at prepare time (e.g. "SELECT rowid FROM
	// t2, t3" → "ambiguous column name: rowid").
	if err := e.validateAmbiguousColumnRefs(s); err != nil {
		return nil, nil, nil, err
	}
	var err error
	allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
	if err != nil {
		return nil, nil, nil, err
	}
	// Apply the WHERE filter to the joined result. execJoins only applies
	// per-join ON conditions; the statement-level WHERE must be applied
	// after the full join is built.
	if s.Where != nil {
		filtered := allRowMaps[:0]
		for _, rowMap := range allRowMaps {
			pass, err := e.rowPassesWhere(s.Where, rowMap, nil)
			if err != nil {
				return nil, nil, nil, err
			}
			if pass {
				filtered = append(filtered, rowMap)
			}
		}
		allRowMaps = filtered
	}
	// Rebuild allRows from combined row maps using SELECT columns
	allRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		row, err := e.buildOutputRow(s.Columns, colDefs, rowMap)
		if err != nil {
			return nil, nil, nil, err
		}
		allRows[i] = row
	}
	return allRows, allRowMaps, colDefs, nil
}

// execSelectVtab executes a SELECT whose FROM table is a virtual table
// (RootPage == 0): FTS tables use the full SELECT pipeline; other virtual
// tables materialize their rows (with an upper-bound hint for bounded tables)
// and run the full SELECT pipeline over them.
func (e *SelectEngine) execSelectVtab(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	// fts5 tables take the dedicated materialized scan: documents come from
	// the fts5 engine and the generic pipeline applies WHERE/ORDER/LIMIT.
	if t5, ok := e.ctx.FTS5Tables()[tableEntry.Name]; ok {
		return e.execFTS5VtabSelect(s, t5, colDefs)
	}
	// For FTS virtual tables, use full SELECT processing (WHERE, ORDER BY, LIMIT).
	// A single-table FTS SELECT uses ExecFTSSelect (which sets the FTS match
	// context for MATCH evaluation); an FTS table in a JOIN needs the generic
	// join pipeline so the join/WHERE clauses are evaluated over combined rows.
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		return e.execFTSVtabSelect(s, tableEntry, ftsTable, colDefs)
	}
	// Non-FTS virtual tables: materialize the rows (with an upper-bound
	// hint for bounded tables like wholenumber) and run the full SELECT
	// pipeline (WHERE, ORDER BY, LIMIT, aggregates) over them.
	return e.execGenericVtabSelect(s, tableEntry, colDefs)
}

// execFTS5VtabSelect runs the fts5 FROM path. A single-table scan uses the
// dedicated fts5 pipeline; a join materializes the fts5 documents (rowid-
// backed row maps) and runs the generic join pipeline over them. No rank
// projection: a joined fts5 scan has no single rank function (C's
// "unable to use function MATCH in the requested context" class).
func (e *SelectEngine) execFTS5VtabSelect(s *sql.SelectStmt, t5 *fts5.Table, colDefs []sql.ColumnDef) *Result {
	if len(s.Joins) == 0 {
		return e.execFTS5Select(s, t5, colDefs)
	}
	if err := fts5LeftJoinUnusableMatch(s, t5); err != nil {
		return &Result{Error: err}
	}
	rowids, allRows, err := fts5ScanRows(t5, colDefs, nil)
	if err != nil {
		return &Result{Error: err}
	}
	allRowMaps := buildMaterializedRowMaps(s, colDefs, allRows, rowids)
	return e.execSelectPostScan(s, allRows, allRowMaps, colDefs)
}

// execFTSVtabSelect runs the FTS3/4 FROM path. A %_content shadow btree that
// cannot be navigated fails any query that reads content columns, including a
// JOIN scan (fts3corrupt4 52.1: SELECT * FROM t1, t2 — SQLite's full scan
// steps the corrupt content table and reports "database disk image is
// malformed"). SQLite's FTS3 xBestIndex binds at most one MATCH constraint per
// table; a second MATCH on the same table is an unusable constraint reported
// at prepare (e_fts3 7.3.1/7.3.2). The generic pipeline validates this for
// real tables; the dedicated FTS scan paths run it here. A joined FTS scan
// materializes the rows (with the docid as rowid) and runs the generic
// join/WHERE/aggregate pipeline over them; the row maps must carry the real
// docid so MATCH evaluation (which reads the row's rowid) resolves the FTS
// document being matched.
func (e *SelectEngine) execFTSVtabSelect(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) *Result {
	if len(s.Joins) > 0 && e.ftsReadsContentColumns(s, ftsTable) && e.contentBtreeCorrupt(tableEntry.Name) {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}
	if err := e.validateMultipleFTSMatch(s); err != nil {
		return &Result{Error: err}
	}
	if len(s.Joins) == 0 {
		return e.ctx.ExecFTSSelect(s, tableEntry, ftsTable, colDefs)
	}
	allRowMaps := e.ftsJoinRowMaps(ftsTable, colDefs, tableEntry.Name)
	allRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		allRows[i] = rowMapToValues(rowMap, colDefs)
	}
	return e.execSelectPostScan(s, allRows, allRowMaps, colDefs)
}

// execGenericVtabSelect materializes a non-FTS virtual table's rows and runs
// the generic pipeline over them. A virtual table whose module declares
// column names (e.g. wholenumber's "value") provides the column definitions
// even when the CREATE VIRTUAL TABLE has no explicit column list.
func (e *SelectEngine) execGenericVtabSelect(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	if len(colDefs) == 0 {
		colDefs = e.vtabModuleColDefs(tableEntry, colDefs)
	}
	var bound int64
	if b, ok := vtabUpperBound(s.Where); ok {
		bound = b
	}
	input, hasInput := vtabInputConstraint(s.Where)
	rows, err := e.ctx.VirtualTableRows(tableEntry, bound, input, hasInput)
	if err != nil {
		return &Result{Error: err}
	}
	return e.execSelectOverMaterialized(s, colDefs, rows)
}

// vtabModuleColDefs augments empty column defs with the virtual-table module's
// declared column names (when the module provides ColumnInfo).
func (e *SelectEngine) vtabModuleColDefs(tableEntry *schema.Entry, colDefs []sql.ColumnDef) []sql.ColumnDef {
	moduleName, args, perr := parseVTabSQL(tableEntry.SQL)
	if perr != nil {
		return colDefs
	}
	module, found := e.ctx.VTables().Find(moduleName)
	if !found {
		return colDefs
	}
	inst, cerr := module.Connect(args)
	if cerr != nil {
		return colDefs
	}
	ci, ok := inst.(vtab.ColumnInfo)
	if !ok {
		return colDefs
	}
	var types []string
	if cti, ok := inst.(vtab.ColumnTypeInfo); ok {
		types = cti.ColumnTypes()
	}
	var hidden map[int]bool
	if hc, ok := inst.(vtab.HiddenColumnInfo); ok {
		hidden = hc.HiddenColumns()
	}
	for i, name := range ci.Columns() {
		if hidden[i] {
			continue
		}
		typ := ""
		if i < len(types) {
			typ = types[i]
		}
		colDefs = append(colDefs, sql.ColumnDef{Name: name, Type: typ})
	}
	return colDefs
}

// execSelectOuterAgg handles the outer-rows aggregate collapse: when outerRows
// is set (from a parent collapse) and this query has aggregates referencing
// only outer columns, evaluate them over all outer rows while using the first
// inner row for non-aggregate columns. Returns nil when normal handling should
// continue.
func (e *SelectEngine) execSelectOuterAgg(s *sql.SelectStmt, allRowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	// Build set of inner column names from the scanned rows
	innerColNames := make(map[string]bool)
	for _, cd := range colDefs {
		innerColNames[cd.Name] = true
	}
	// Check if any aggregate references inner columns — if so, fall through to normal handling
	allOuterRefs := true
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if reg, found := e.ctx.Functions().Find(fn.Name); found && reg.Type == function.TypeAggregate {
				if !e.aggregateHasOnlyOuterRefs(fn, innerColNames) {
					allOuterRefs = false
					break
				}
			}
		}
	}
	if !allOuterRefs {
		return nil
	}
	columns := e.buildColumnNames(s.Columns, colDefs, s)
	outRow := e.evalAggOverOuterRowsWithInner(s, e.outerRows, allRowMaps)
	result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// evalLimitOffsetExprs evaluates a statement's LIMIT and OFFSET expressions
// (either may be nil) via evalLimitExpr.
func (e *SelectEngine) evalLimitOffsetExprs(limit, offset sql.Expr) (sql.Expr, sql.Expr, error) {
	lExpr, lErr := e.evalLimitExpr(limit)
	if lErr != nil {
		return nil, nil, lErr
	}
	oExpr, oErr := e.evalLimitExpr(offset)
	if oErr != nil {
		return nil, nil, oErr
	}
	return lExpr, oExpr, nil
}

// evalLimitExpr evaluates a LIMIT/OFFSET expression (which may be a scalar
// subquery) to a numeric literal so applyLimitOffset can consume it. When
// evaluation fails or the value is not numeric (e.g. a correlated expression),
// the raw expression is returned unchanged. A value that cannot be cast to an
// integer (non-numeric text, non-integral float, NULL, blob) is an error
// ("datatype mismatch"), matching SQLite.
func (e *SelectEngine) evalLimitExpr(expr sql.Expr) (sql.Expr, error) {
	if expr == nil {
		return nil, nil
	}
	// resolve.c resolves LIMIT/OFFSET expressions at prepare time. They are
	// evaluated without a row, so ANY column reference is "no such column"
	// (even a real FROM column) and functions must exist with matching
	// arity (limit-12.1: LIMIT replace(1); LIMIT x; OFFSET x).
	if err := e.validateLimitExpr(expr); err != nil {
		return nil, err
	}
	v, err := e.ctx.EvalExpr(expr, nil)
	if err != nil {
		// A LIMIT expression containing a subquery resolves at prepare:
		// its evaluation failures are schema errors ("no such table: blah",
		// misc5-6.1) and must surface, not degrade to an unlimited scan.
		if exprContainsSubquery(expr) {
			return nil, err
		}
		return expr, nil
	}

	switch n := util.UnwrapColumnValue(v).(type) {
	case int64:
		return &sql.NumericLit{Value: strconv.FormatInt(n, 10)}, nil
	case int:
		return &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}, nil
	case float64:
		return limitFloatToInt(n)
	case string:
		// SQLite casts the LIMIT expression to integer: LIMIT '4' == LIMIT 4,
		// LIMIT '1.0' == LIMIT 1, LIMIT '1.2' / 'abc' → datatype mismatch.
		return limitTextToInt(n)
	case nil:
		return nil, fmt.Errorf("datatype mismatch")
	case []byte:
		// A blob LIMIT (LIMIT X'ABCD') cannot be cast to an integer.
		return nil, fmt.Errorf("datatype mismatch")
	}
	return expr, nil
}

// limitFloatToInt casts a LIMIT float value to an integer literal; a
// non-integral value is a "datatype mismatch".
func limitFloatToInt(n float64) (sql.Expr, error) {
	if n == math.Trunc(n) {
		return &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}, nil
	}
	return nil, fmt.Errorf("datatype mismatch")
}

// limitTextToInt casts a LIMIT text value to an integer literal; non-numeric
// or non-integral text is a "datatype mismatch".
func limitTextToInt(n string) (sql.Expr, error) {
	if f, perr := strconv.ParseFloat(strings.TrimSpace(n), 64); perr == nil && f == math.Trunc(f) {
		return &sql.NumericLit{Value: strconv.FormatInt(int64(f), 10)}, nil
	}
	return nil, fmt.Errorf("datatype mismatch")
}
