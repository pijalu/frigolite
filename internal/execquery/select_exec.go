// Package exec implements query execution.
package execquery

import (
	"fmt"
	"os"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns SELECT execution orchestration: the top-level execSelect
// dispatch, no-FROM/CTE/recursive/VIEW/materialized execution paths, and
// result finalization. Extracted from select.go for file-level SRP.

// execSelectScanPhase runs the table scan, WITHOUT ROWID PK ordering, and
// system-table filtering, returning the scanned rows, row maps, and any error.
func (e *SelectEngine) execSelectScanPhase(s *sql.SelectStmt, cursor *btree.Cursor, colDefs []sql.ColumnDef, tableEntry *schema.Entry) ([][]interface{}, []RowMap, error) {
	needMaps, withoutRowidPKCols := e.prepareScanOutputs(s, tableEntry, colDefs)
	allRows, allRowMaps, err := e.scanTableRowsWithSQL(cursor, s, colDefs, needMaps, tableEntry.SQL)
	if err != nil {
		return nil, nil, err
	}
	if len(withoutRowidPKCols) > 0 && len(allRowMaps) > 0 {
		sortRowMapsByPKNames(allRowMaps, withoutRowidPKCols)
		for i := range allRows {
			row, err := e.buildOutputRow(s.Columns, colDefs, allRowMaps[i])
			if err != nil {
				return nil, nil, err
			}
			allRows[i] = row
		}
	}
	if IsSchemaTable(tableEntry.Name) && len(allRowMaps) > 0 {
		allRows, allRowMaps = e.filterSystemTables(allRows, allRowMaps, colDefs)
	}
	return allRows, allRowMaps, nil
}

// prepareScanOutputs computes whether the scan must build row maps and the
// WITHOUT ROWID PRIMARY KEY column list that dictates scan output order. The
// index-order reorder sorts by the index key through the scan's row maps, so
// they must be built even for star outputs when it applies.
func (e *SelectEngine) prepareScanOutputs(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) (needMaps bool, withoutRowidPKCols []string) {
	needMaps = SelectNeedsRowMaps(e, s, tableEntry.Name)
	if e.indexScanOrderIndex(s) != "" {
		needMaps = true
	}
	if len(s.Joins) > 0 || len(s.OrderBy) > 0 {
		return needMaps, nil
	}
	if !e.ctx.HasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return needMaps, nil
	}
	withoutRowidPKCols = PKColumnNames(tableEntry.SQL, colDefs)
	if len(withoutRowidPKCols) > 0 {
		needMaps = true
	}
	return needMaps, withoutRowidPKCols
}

// execSelectPostScan processes scanned rows: outer-row aggregates, correlated
// aggregates, joins, regular aggregates, and result construction + finalization.
func (e *SelectEngine) execSelectPostScan(s *sql.SelectStmt, allRows [][]interface{}, allRowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if len(e.outerRows) > 0 && e.hasAggregates(s.Columns) {
		if result := e.execSelectOuterAgg(s, allRowMaps, colDefs); result != nil {
			return result
		}
	}
	if result := e.execSelectCorrelatedAgg(s, allRowMaps, colDefs); result != nil {
		return result
	}
	var err error
	if len(s.Joins) > 0 {
		if allRows, allRowMaps, colDefs, err = e.execSelectJoins(s, allRowMaps, colDefs); err != nil {
			return &Result{Error: err}
		}
	}
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	// Window-function pass: runs over the post-WHERE/JOIN/GROUP-BY row set
	// when the query has window functions and no plain aggregates. (Window
	// queries with plain aggregates/GROUP BY are handled inside
	// handleSelectAggregates.)
	if e.selectHasWindowFuncs(s.Columns) {
		winResult := e.execWindowPass(s, allRowMaps, colDefs)
		return e.finalizeSelectResult(winResult, s, winResult.rowMaps)
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: allRows}
	if len(s.Joins) > 0 && selectProjectsPlainColumns(s.Columns) {
		result.rowMaps = allRowMaps
	}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// validateOrderGroupByTerms enforces SQLITE_LIMIT_COLUMN on the ORDER BY and
// GROUP BY clause sizes: more terms than the limit errors "too many terms in
// ORDER BY clause" / "too many terms in GROUP BY clause" (resolve.c
// sqlite3ResolveOrderGroupBy, checked during name resolution before rows are
// processed — sqllimits1-8.x).
func (e *SelectEngine) validateOrderGroupByTerms(s *sql.SelectStmt) error {
	limit := e.ctx.ColumnLimit()
	if limit != 0 {
		if len(s.GroupBy) > limit {
			return fmt.Errorf("too many terms in GROUP BY clause")
		}
		if len(s.OrderBy) > limit {
			return fmt.Errorf("too many terms in ORDER BY clause")
		}
	}
	return nil
}

func (e *SelectEngine) execSelect(s *sql.SelectStmt) *Result {
	e.selectDepth++
	if e.selectDepth == 1 {
		e.resultTooWide = false // per-statement state
	}
	e.aggPendingErr = nil // per-statement state: a prior aborted SELECT must not leak its aggregate error
	defer func() { e.selectDepth-- }()
	// SQLite resolves TVF arguments against every FROM term of the query:
	// a top-level table-valued function whose arguments reference columns of
	// another FROM item (tabfunc01-3.1: FROM generate_series(1,x), t1)
	// executes as a correlated nested loop with the referenced table as the
	// outer loop. Promote that table to head position and demote the function
	// to a correlated comma-join operand before any dispatch/validation.
	if ns, ok := e.promoteCorrelatedTVFFrom(s); ok {
		s = ns
	}
	// SQLite resolves TVF arguments against the function's own FROM scope
	// only, and forbids references past an OUTER join (tabfunc01-1410/1420,
	// carray01-201).
	if err := e.validateTVFArgScope(s); err != nil {
		return &Result{Error: err}
	}
	if res := e.enterCTEScopes(s); res != nil {
		return res
	}
	if len(s.CTEs) > 0 {
		defer func() { e.cteScopes = e.cteScopes[:len(e.cteScopes)-1] }()
	}
	if err := e.validateSelectPreDispatch(s); err != nil {
		return &Result{Error: err}
	}
	if res := e.indexedByOnViewError(s); res != nil {
		return res
	}
	if aliasMap := selectAliasMap(s); len(aliasMap) > 0 {
		e.aliasStack = append(e.aliasStack, aliasMap)
		defer func() { e.aliasStack = e.aliasStack[:len(e.aliasStack)-1] }()
	}
	if result, handled := e.joins.ExecFrom(s); handled {
		return result
	}
	if result, handled := e.execSelectFrom(s); handled {
		return result
	}
	return e.execRealTableSelect(s)
}

// validateSelectPreDispatch runs the statement-level validations before FROM
// dispatch: ORDER BY/GROUP BY term shape, expression validity, compound
// column counts, and compound FROM table resolution. SQLite resolves compound
// member FROM tables right-to-left, so a missing table in the LAST member is
// reported before an earlier member's (with3 1.0: "SELECT 5 FROM t0 UNION
// SELECT 8 FROM m" errors "no such table: m", not t0).
func (e *SelectEngine) validateSelectPreDispatch(s *sql.SelectStmt) error {
	if err := e.validateOrderGroupByTerms(s); err != nil {
		return err
	}
	if err := e.validate.ValidateExprs(s); err != nil {
		return err
	}
	if err := e.validateCompoundColumnCounts(s); err != nil {
		return err
	}
	return e.validateCompoundFromTables(s)
}

// indexedByOnViewError rejects INDEXED BY against a FROM term that resolves
// to a VIEW: a view has no indexes, so any INDEXED BY name on it reports
// "no such index" (indexedby-6.4: SELECT * FROM v1 INDEXED BY i1 where v1 is
// a view).
func (e *SelectEngine) indexedByOnViewError(s *sql.SelectStmt) *Result {
	if s.From.IndexedBy == "" || s.From.Name == "" || s.From.EmptyName || s.From.Subquery != nil {
		return nil
	}
	if _, _, terr := e.ctx.FindTable(s.From.Name); terr != nil {
		if _, _, verr := e.ctx.FindView(s.From.Name); verr == nil {
			return &Result{Error: fmt.Errorf("no such index: %s", s.From.IndexedBy)}
		}
	}
	return nil
}

// enterCTEScopes validates the WITH clause's names, assigns each CTE its
// scope depth, and pushes the CTE list onto the scope stack (the caller pops
// one level when CTEs were present).
func (e *SelectEngine) enterCTEScopes(s *sql.SelectStmt) *Result {
	if len(s.CTEs) == 0 {
		return nil
	}
	if dup := duplicateCTEName(s.CTEs); dup != "" {
		return &Result{Error: fmt.Errorf("duplicate WITH table name: %s", dup)}
	}
	depth := len(e.cteScopes)
	for i := range s.CTEs {
		s.CTEs[i].ScopeDepth = depth
	}
	if os.Getenv("DBG_CTE") != "" {
		for _, c := range s.CTEs {
			fmt.Fprintf(os.Stderr, "DBG push %s depth=%d\n", c.Name, depth)
		}
	}
	e.cteScopes = append(e.cteScopes, s.CTEs)
	return nil
}

// execRealTableSelect executes a SELECT over a real (RootPage != 0) table:
// resolve + prevalidate + scan + post-scan pipeline. The scan runs with the
// FROM term's alias as the current scan table so correlated references
// resolve under it.
func (e *SelectEngine) execRealTableSelect(s *sql.SelectStmt) *Result {
	tableEntry, dbCtx, result := e.resolveFromTable(s)
	if result != nil {
		return result
	}
	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	if result, cd := e.execSelectPrevalidate(s, tableEntry, dbCtx, colDefs); result != nil {
		return result
	} else {
		colDefs = cd
	}
	tree := e.ctx.TableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}
	prevScanTable := e.currentScanTable
	e.currentScanTable = tableEntry.Name
	if s.From.As != "" {
		e.currentScanTable = s.From.As
	}
	defer func() { e.currentScanTable = prevScanTable }()
	// Point-lookup short circuit (src/where.c SEARCH rowid=?): a WHERE that
	// pins the rowid to a literal reads the single candidate row by seek.
	if rows, rowMaps, handled := e.selectRowidSeekRows(s, tableEntry, colDefs, tree); handled {
		return e.execSelectPostScan(s, rows, rowMaps, colDefs)
	}
	allRows, allRowMaps, scanErr := e.scan.ScanTable(s, tableEntry, colDefs, cursor)
	if scanErr != nil {
		return &Result{Error: scanErr}
	}
	return e.execSelectPostScan(s, allRows, allRowMaps, colDefs)
}

// execSelectPrevalidate runs the pre-scan validation for a real-table SELECT:
// INDEXED BY, WHERE collations, WITHOUT ROWID rowid references, column
// references, RAISE-in-select, and the OR-index optimization. It also handles
// the virtual-table branch. Returns a non-nil Result when execution completed
// early, otherwise the (possibly module-augmented) column defs.
// prevalidateSelectChecks runs the INDEXED BY, WHERE collation, WITHOUT ROWID
// rowid-ref, column-reference, and RAISE validations. Returns an error Result
// when a check fails, nil to continue.
func (e *SelectEngine) prevalidateSelectChecks(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	if err := e.prevalidateIndexCollation(s, tableEntry, colDefs); err != nil {
		return &Result{Error: err}
	}
	if err := e.prevalidateRowIDAndRefs(s, tableEntry, colDefs); err != nil {
		return &Result{Error: err}
	}
	if err := e.prevalidateSchemaFunctionSafety(s, colDefs); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// prevalidateSchemaFunctionSafety rejects SELECTs over tables whose generated
// columns use functions unsafe under PRAGMA trusted_schema=OFF
// (trustschema1-1.140: SELECT a,b,c over t1 with c AS (f2(a+2)) errors
// "unsafe use of f2()"). Only generated columns the statement actually
// references are checked (1.130: SELECT a,b over the same table succeeds
// because b uses the innocuous f1).
func (e *SelectEngine) prevalidateSchemaFunctionSafety(s *sql.SelectStmt, colDefs []sql.ColumnDef) error {
	refs := collectSelectColumnRefs(s)
	for _, cd := range colDefs {
		if cd.Generated == nil {
			continue
		}
		if !refs[strings.ToLower(cd.Name)] {
			continue
		}
		var unsafe string
		WalkExprFull(cd.Generated, func(en sql.Expr) {
			if unsafe != "" {
				return
			}
			if fc, ok := en.(*sql.FuncCall); ok && !e.ctx.SchemaFunctionSafe(fc.Name) {
				unsafe = fc.Name
			}
		})
		if unsafe != "" {
			return fmt.Errorf("unsafe use of %s()", unsafe)
		}
	}
	return nil
}

// collectSelectColumnRefs returns the lower-cased column names referenced by a
// SELECT's result columns, WHERE, GROUP BY, HAVING, and ORDER BY.
func collectSelectColumnRefs(s *sql.SelectStmt) map[string]bool {
	refs := make(map[string]bool)
	collect := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		WalkExprFull(expr, func(en sql.Expr) {
			if ref, ok := en.(*sql.ColumnRef); ok {
				refs[strings.ToLower(ref.Name)] = true
			}
		})
	}
	for _, col := range s.Columns {
		collect(col.Expr)
	}
	collect(s.Where)
	for _, g := range s.GroupBy {
		collect(g)
	}
	collect(s.Having)
	for _, ob := range s.OrderBy {
		collect(ob.Expr)
	}
	return refs
}

// prevalidateIndexCollation validates the INDEXED BY clause and WHERE-column
// collations for a single-table SELECT (both only apply without joins).
func (e *SelectEngine) prevalidateIndexCollation(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) error {
	if s.From.IndexedBy != "" && len(s.Joins) == 0 {
		if err := e.ctx.ValidateIndexedBy(tableEntry, s.From.IndexedBy, s); err != nil {
			return err
		}
	}
	if len(s.Joins) == 0 && s.Where != nil {
		if err := e.checkWhereCollations(s.Where, colDefs, s.From); err != nil {
			return err
		}
	}
	return nil
}

// prevalidateRowIDAndRefs validates WITHOUT-ROWID rowid references, unqualified
// column references, and RAISE usage outside triggers.
func (e *SelectEngine) prevalidateRowIDAndRefs(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) error {
	if e.ctx.HasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) && !tableHasRealRowIDCol(colDefs) {
		if ref := e.findRowIDRef(s, tableEntry.Name, s.From.As, len(s.Joins) > 0); ref != "" {
			return fmt.Errorf("no such column: %s", ref)
		}
	}
	if len(s.Joins) == 0 && e.outerRow == nil {
		if err := e.validateSelectColumnRefs(s, colDefs, tableEntry.Name, s.From.As, true); err != nil {
			return err
		}
	}
	if e.ctx.TriggerDepth() == 0 {
		if err := e.ctx.ValidateNoRaiseOutsideTrigger(s); err != nil {
			return err
		}
	}
	return nil
}

func (e *SelectEngine) execSelectPrevalidate(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef) (*Result, []sql.ColumnDef) {
	if result := e.prevalidateSelectChecks(s, tableEntry, colDefs); result != nil {
		return result, colDefs
	}
	if tableEntry.RootPage == 0 {
		return e.execSelectVtab(s, tableEntry, colDefs), colDefs
	}
	// OR-index optimization.
	if len(s.Joins) == 0 && s.Where != nil && e.outerRow == nil && len(e.outerRows) == 0 && !e.ctx.ReverseUnordered() {
		if branches, ok := e.ctx.PlanOrIndexScan(s.Where, tableEntry.Name, colDefs, dbCtx); ok {
			return e.ctx.ExecSelectWithOrPlan(s, tableEntry, dbCtx, colDefs, branches), colDefs
		}
	}
	return nil, colDefs
}

// finalizeSelectResult applies DISTINCT, ORDER BY, LIMIT, and UNION.
func (e *SelectEngine) finalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result {
	// A pending aggregate Step failure (e.g. zipfile's "out of memory"
	// raised inside a wrapping scalar expression whose plumbing drops the
	// per-expression error) outranks any computed rows: surface it now.
	if e.aggPendingErr != nil {
		err := e.aggPendingErr
		e.aggPendingErr = nil
		return &Result{Error: err}
	}
	// select.c sqlite3SelectCallback: a result set wider than
	// SQLITE_LIMIT_COLUMN errors "too many columns in result set"
	// (sqllimits1-17.0: nested SELECT *,*,* expansion). The flag is set by
	// buildColumnNames for any SELECT level (subqueries included).
	if e.resultTooWide {
		e.resultTooWide = false
		return &Result{Error: fmt.Errorf("too many columns in result set")}
	}
	// The collation of each result column of a compound query comes from the
	// leftmost SELECT member (SQLite's compound column collation rule).
	colls := e.selectOutputCollations(s)
	if s.Distinct {
		result.Rows, rowMaps = e.distinctRows(result.Rows, rowMaps, colls, s)
	}
	// Handle UNION before ORDER BY (ORDER BY on compound SELECT applies to the merged result).
	orderBy := s.OrderBy
	limit := s.Limit
	offset := s.Offset
	if s.Union != nil {
		var mergeErr error
		result.Rows, orderBy, limit, offset, mergeErr = e.mergeCompoundChain(result.Rows, s, colls, len(result.Columns))
		if mergeErr != nil {
			return &Result{Error: mergeErr}
		}
		// The head's rowMaps only cover its own rows; rebuild them from the
		// merged result so ORDER BY can resolve columns across all members.
		rowMaps = rebuildRowMapsFromRows(result.Rows, result.Columns)
	}
	if len(orderBy) > 0 {
		resolved, rerr := e.resolveFinalOrderBy(s, orderBy, result.Columns, colls)
		if rerr != nil {
			return &Result{Error: rerr}
		}
		if serr := e.sortRowsWithMaps(result, resolved, rowMaps, s); serr != nil {
			return &Result{Error: serr}
		}
	}
	lExpr, oExpr, lerr := e.evalLimitOffsetExprs(limit, offset)
	if lerr != nil {
		return &Result{Error: lerr}
	}
	result.Rows = applyLimitOffset(result.Rows, lExpr, oExpr)
	return result
}

// resolveFinalOrderBy validates and resolves a result's ORDER BY terms.
// A compound ORDER BY inherits the result column's collation (SQLite: the
// first member with a defined collation wins — with1 10.8.4.x): an ordinal
// or bare term without its own COLLATE must sort with the compound column's
// collation, so it is wrapped in COLLATE when the column defines one.
// resultCols carries the compound's output column names (used to resolve bare
// term names to their result positions).
func (e *SelectEngine) resolveFinalOrderBy(s *sql.SelectStmt, orderBy []sql.OrderByTerm, resultCols []string, colls []string) ([]sql.OrderByTerm, error) {
	if err := validateOrderBy(orderBy, len(resultCols)); err != nil {
		return nil, err
	}
	if s.Union == nil {
		return orderBy, nil
	}
	if err := e.validateCompoundOrderBy(s, orderBy); err != nil {
		return nil, err
	}
	orderBy = e.resolveCompoundOrderByTerms(s, orderBy)
	return e.applyCompoundOrderByCollations(orderBy, resultCols, colls), nil
}

// execSelectViewWithOuter executes a view and applies the outer SELECT's
// column expressions, aggregates, ORDER BY, etc. on the view's result.
// execViewBody pins the view's schema context, clears outer CTE scopes, and
// executes the view body SELECT. The circular-reference guard is cleared after
// execution so JOINs through other views can re-reference this view.
func (e *SelectEngine) execViewBody(viewEntry *schema.Entry, viewCtx *DatabaseContext) *Result {
	prevPin := e.schemaPin
	prevTempView := e.expandingTempView
	prevExpandingView := e.expandingView
	e.expandingView = true
	if viewCtx != nil && !viewCtx.IsTemp {
		e.schemaPin = viewCtx
	} else if viewCtx != nil && viewCtx.IsTemp {
		e.expandingTempView = true
	}
	prevCTEScopes := e.cteScopes
	e.cteScopes = nil
	viewResult := e.execSelectView(viewEntry)
	e.cteScopes = prevCTEScopes
	e.schemaPin = prevPin
	e.expandingTempView = prevTempView
	e.expandingView = prevExpandingView
	if viewResult.Error != nil {
		return viewResult
	}
	delete(e.resolvingViews, viewEntry.Name)
	return viewResult
}

func (e *SelectEngine) execSelectViewWithOuter(s *sql.SelectStmt, viewEntry *schema.Entry, viewCtx *DatabaseContext) *Result {
	e.nestDepth++
	defer func() { e.nestDepth-- }()
	if e.nestDepth >= e.ctx.ExprDepthLimit() {
		return &Result{Error: fmt.Errorf("VIEWs and/or subqueries nested too deep")}
	}
	// Offer the outer statement to the view-body expansion for the
	// omit-unused-subquery-column optimization (select.c
	// disableUnusedSubqueryResultColumns applies to view materialization
	// like any FROM-clause subquery). The freshly parsed body consumes and
	// clears it, so a body expanding further views does not inherit stale
	// usage.
	prevOuterStmt := e.viewOuterStmt
	prevOuterQuals := e.viewOuterQuals
	e.viewOuterStmt = s
	quals := make([]string, 0, 3)
	if viewEntry.Name != "" {
		quals = append(quals, viewEntry.Name)
	}
	if s.From.Name != "" {
		quals = append(quals, s.From.Name)
	}
	if s.From.As != "" {
		quals = append(quals, s.From.As)
	}
	e.viewOuterQuals = quals
	defer func() {
		e.viewOuterStmt = prevOuterStmt
		e.viewOuterQuals = prevOuterQuals
	}()
	viewResult := e.execViewBody(viewEntry, viewCtx)
	if viewResult.Error != nil {
		return viewResult
	}
	viewColDefs, colErr := e.viewColDefsFromResult(viewEntry, viewResult)
	if colErr != nil {
		return &Result{Error: colErr}
	}
	viewQual := s.From.Name
	if s.From.As != "" {
		viewQual = s.From.As
	}
	rowMaps := viewRowMapsFromResult(viewResult.Rows, viewColDefs, viewQual)
	rowMaps, viewColDefs, jerr := e.joinAndViewRowMaps(s, rowMaps, viewColDefs)
	if jerr != nil {
		return &Result{Error: jerr}
	}
	if aggResult := e.handleSelectAggregates(s, rowMaps, viewColDefs); aggResult != nil {
		return aggResult
	}
	// Window-function pass: runs over the post-WHERE/JOIN/GROUP-BY row set
	// when the query has window functions and no plain aggregates (mirrors
	// execSelectPostScan).
	if e.selectHasWindowFuncs(s.Columns) {
		winResult := e.execWindowPass(s, rowMaps, viewColDefs)
		return e.finalizeSelectResult(winResult, s, winResult.rowMaps)
	}
	allRows, err := buildOutputRowsFromMaps(e, s.Columns, viewColDefs, rowMaps)
	if err != nil {
		return &Result{Error: err}
	}
	result := &Result{
		Columns: e.buildColumnNames(s.Columns, viewColDefs, s),
		Rows:    allRows,
	}
	if len(s.Joins) > 0 && selectProjectsPlainColumns(s.Columns) {
		result.rowMaps = rowMaps
	}
	return e.finalizeSelectResult(result, s, rowMaps)
}

// joinAndViewRowMaps applies the outer statement's JOINs (with the ambiguous
// reference pre-check) and WHERE filter over the view's row maps.
func (e *SelectEngine) joinAndViewRowMaps(s *sql.SelectStmt, rowMaps []RowMap, viewColDefs []sql.ColumnDef) ([]RowMap, []sql.ColumnDef, error) {
	if len(s.Joins) > 0 {
		if err := e.validateAmbiguousColumnRefs(s); err != nil {
			return nil, nil, err
		}
		var err error
		rowMaps, viewColDefs, err = e.execJoins(s, rowMaps, viewColDefs)
		if err != nil {
			return nil, nil, err
		}
	}
	rowMaps, ferr := filterRowMapsByWhere(e, s.Where, rowMaps)
	return rowMaps, viewColDefs, ferr
}

// execSelectNoFrom handles SELECT without FROM clause.
// validateNoFromSelect runs the pre-execution validation for a FROM-less
// SELECT: column references, RAISE, VALUES column naming, ORDER BY, WHERE.
// Returns the (possibly adjusted) column names and any error.
func (e *SelectEngine) validateNoFromSelect(s *sql.SelectStmt, columns []string) ([]string, error) {
	if err := e.validateNoFromRefsAndRaise(s); err != nil {
		return nil, err
	}
	if s.ValuesChain {
		for i := range columns {
			columns[i] = fmt.Sprintf("column%d", i+1)
		}
	}
	if len(s.OrderBy) > 0 {
		if err := validateOrderBy(s.OrderBy, len(columns)); err != nil {
			return nil, err
		}
	}
	if s.Where != nil {
		pass, err := e.rowPassesWhere(s.Where, nil, nil)
		if err != nil {
			return nil, err
		}
		if !pass {
			return columns, errNoFromWhereFalse
		}
	}
	return columns, nil
}

// validateNoFromRefsAndRaise validates column references (when not inside an
// outer query) and RAISE usage outside triggers for a FROM-less SELECT.
// Trigger NEW./OLD. references stay exempt through checkNoFromRef's
// isTriggerRowRef (resolved against the trigger row's columns, triggerB-2.2:
// an unrecognized name inside a trigger body still errors "no such column").
func (e *SelectEngine) validateNoFromRefsAndRaise(s *sql.SelectStmt) error {
	if e.outerRow == nil && len(e.outerRows) == 0 {
		if err := e.validateNoFromColumnRefs(s); err != nil {
			return err
		}
	}
	if e.ctx.TriggerDepth() == 0 {
		if err := e.ctx.ValidateNoRaiseOutsideTrigger(s); err != nil {
			return err
		}
	}
	return nil
}

// errNoFromWhereFalse signals that the WHERE filter eliminated all rows in a
// FROM-less SELECT (caller returns an empty result).
var errNoFromWhereFalse = fmt.Errorf("__nofrom_where_false__")

// errUndeterminedCTEWidth is returned by cteAnchorColumnCount when a star in
// the anchor expands through a mutual CTE recursion cycle whose width cannot
// be determined statically (with2 3.2: i→j→k→i). Callers skip the declared-
// column width check and let the execution-time circular reference fire.
var errUndeterminedCTEWidth = fmt.Errorf("__undetermined_cte_width__")

// evalNoFromRow evaluates the output row for a FROM-less SELECT: aggregates
// over outer rows when applicable, or each column expression individually.
func (e *SelectEngine) evalNoFromRow(s *sql.SelectStmt) ([]interface{}, error) {
	if len(e.outerRows) > 0 && e.hasAggregates(s.Columns) && e.aggHasColumnRef(s.Columns) {
		return e.evalAggOverOuterRows(s, e.outerRows), nil
	}
	var outRow []interface{}
	for _, col := range s.Columns {
		// Window-function columns are computed by the window pass over the
		// full row set (the no-FROM path materializes one row, which the
		// window pass then fills).
		if e.exprHasWindowFunc(col.Expr) {
			outRow = append(outRow, nil)
			continue
		}
		v, err := e.ctx.EvalExpr(col.Expr, e.outerRow)
		if err != nil {
			return nil, err
		}
		outRow = append(outRow, unwrapCollatedValue(v))
	}
	return outRow, nil
}

func (e *SelectEngine) execSelectNoFrom(s *sql.SelectStmt) *Result {
	columns := e.buildColumnNames(s.Columns, nil, s)
	cols, verr := e.validateNoFromSelect(s, columns)
	if verr != nil {
		if verr == errNoFromWhereFalse {
			// WHERE eliminated the single implicit row, but an aggregate
			// query still emits one row with empty-input aggregate values
			// (SELECT count(*) WHERE 0 → 0, SELECT sum(x) WHERE 0 → NULL).
			// Only a non-aggregate FROM-less SELECT yields no rows.
			if e.hasAggregates(s.Columns) {
				return e.evalAggregatesEmpty(s, nil)
			}
			return &Result{Columns: columns, Rows: nil}
		}
		return &Result{Error: verr}
	}
	columns = cols
	outRow, evalErr := e.evalNoFromRow(s)
	if evalErr != nil {
		return &Result{Error: evalErr}
	}

	// Handle UNION / INTERSECT / EXCEPT for no-FROM selects.
	if s.Union != nil {
		return e.execNoFromCompound(s, columns, outRow)
	}

	result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
	result = e.applyNoFromWindowPass(result, s, columns)
	if len(s.OrderBy) > 0 {
		rowMaps := e.buildNoFromRowMaps(result.Rows, columns)
		if serr := e.sortRowsWithMaps(result, s.OrderBy, rowMaps, s); serr != nil {
			return &Result{Error: serr}
		}
	}
	if s.Limit != nil || s.Offset != nil {
		lExpr, oExpr, lerr := e.evalLimitOffsetExprs(s.Limit, s.Offset)
		if lerr != nil {
			return &Result{Error: lerr}
		}
		result.Rows = applyLimitOffset(result.Rows, lExpr, oExpr)
	}
	return result
}

// execNoFromCompound merges the no-FROM head row through the compound chain.
// The head row may contain a window function (e.g. VALUES(count(*)OVER())
// UNION ...); it is filled before merging.
func (e *SelectEngine) execNoFromCompound(s *sql.SelectStmt, columns []string, outRow []interface{}) *Result {
	if e.selectHasWindowFuncs(s.Columns) {
		headResult := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
		win := e.execWindowPass(s, e.buildNoFromRowMaps(headResult.Rows, columns), nil)
		if win != nil && len(win.Rows) > 0 {
			outRow = win.Rows[0]
		}
	}
	rows, _, _, _, mergeErr := e.mergeCompoundChain([][]interface{}{outRow}, s, e.selectOutputCollations(s), len(columns))
	if mergeErr != nil {
		return &Result{Error: mergeErr}
	}
	result := &Result{Columns: columns, Rows: rows}
	if ferr := e.finalizeNoFromSelect(result, s); ferr != nil {
		return &Result{Error: ferr}
	}
	return result
}

// applyNoFromWindowPass runs the window pass over the single implicit row.
// A window function whose argument/OVER clause contains a plain aggregate
// (e.g. min(max((SELECT x FROM v1))) OVER ()) is an aggregate query:
// precompute the inner aggregate over the implicit row and let the window
// pass resolve it (matching evalAggregates).
func (e *SelectEngine) applyNoFromWindowPass(result *Result, s *sql.SelectStmt, columns []string) *Result {
	if !e.selectHasWindowFuncs(s.Columns) {
		return result
	}
	outRow := result.Rows[0]
	rowMaps := e.buildNoFromRowMaps(result.Rows, columns)
	if len(rowMaps) > 0 && e.hasAggregates(s.Columns) {
		e.storeWindowNestedAggs(rowMaps[0], s.Columns, rowMaps)
		e.windowGroupOutputs = columns
		e.windowGroupCols = s.Columns
		defer func() { e.windowGroupOutputs = nil; e.windowGroupCols = nil }()
	}
	result = e.execWindowPass(s, rowMaps, nil)
	if result == nil {
		result = &Result{Columns: columns, Rows: [][]interface{}{outRow}}
	}
	return result
}

// finalizeNoFromSelect applies ORDER BY and LIMIT/OFFSET to a no-FROM SELECT result.
func (e *SelectEngine) finalizeNoFromSelect(result *Result, s *sql.SelectStmt) error {
	// A trailing ORDER BY / LIMIT / OFFSET on a compound lives on its LAST
	// member (SQLite attaches it there); hoist it here for no-FROM selects.
	orderBy, limit, offset := hoistCompoundNoFromClauses(s)
	if err := e.sortNoFromCompound(result, s, orderBy); err != nil {
		return err
	}
	// Apply LIMIT/OFFSET
	if limit != nil || offset != nil {
		lExpr, oExpr, lerr := e.evalLimitOffsetExprs(limit, offset)
		if lerr != nil {
			result.Error = lerr
			return lerr
		}
		result.Rows = applyLimitOffset(result.Rows, lExpr, oExpr)
	}
	return nil
}

// sortNoFromCompound validates and applies a no-FROM (possibly compound)
// result's ORDER BY. Compound queries restrict ORDER BY terms to result
// column names or ordinals (SQLite: "Nth ORDER BY term does not match any
// column in the result set"). The no-FROM path merges compounds too (e.g.
// VALUES(2) EXCEPT SELECT ” ORDER BY abc), so validate the same way
// finalizeSelectResult does.
func (e *SelectEngine) sortNoFromCompound(result *Result, s *sql.SelectStmt, orderBy []sql.OrderByTerm) error {
	if len(orderBy) == 0 {
		return nil
	}
	if err := validateOrderBy(orderBy, len(result.Columns)); err != nil {
		result.Error = err
		return err
	}
	if s.Union != nil {
		if err := e.validateCompoundOrderBy(s, orderBy); err != nil {
			result.Error = err
			return err
		}
	}
	rowMaps := e.buildNoFromRowMaps(result.Rows, result.Columns)
	if serr := e.sortRowsWithMaps(result, orderBy, rowMaps, s); serr != nil {
		result.Error = serr
		return serr
	}
	return nil
}

// hoistCompoundNoFromClauses returns the ORDER BY / LIMIT / OFFSET that apply
// to a no-FROM compound, taking them from the compound's last member when the
// statement has a UNION chain (SQLite attaches trailing clauses there).
func hoistCompoundNoFromClauses(s *sql.SelectStmt) ([]sql.OrderByTerm, sql.Expr, sql.Expr) {
	orderBy := s.OrderBy
	limit := s.Limit
	offset := s.Offset
	if s.Union != nil {
		last := s
		for last.Union != nil {
			last = last.Union
		}
		if len(last.OrderBy) > 0 {
			orderBy = last.OrderBy
		}
		if last.Limit != nil {
			limit = last.Limit
		}
		if last.Offset != nil {
			offset = last.Offset
		}
	}
	return orderBy, limit, offset
}

// execSelectOverMaterialized runs the outer SELECT pipeline (WHERE, JOINs,
// aggregates, projection, DISTINCT, ORDER BY, LIMIT/OFFSET, UNION) over a
// materialized set of rows with known column definitions. It is shared by
// subquery-in-FROM execution and table-valued pragma functions.
// joinAndFilterRowMaps validates ambiguous refs, executes joins, then applies
// the statement-level WHERE filter. Returns the joined+filtered row maps.
func (e *SelectEngine) joinAndFilterRowMaps(s *sql.SelectStmt, allRowMaps []RowMap, colDefs []sql.ColumnDef) ([]RowMap, []sql.ColumnDef, error) {
	if err := e.validateAmbiguousColumnRefs(s); err != nil {
		return nil, nil, err
	}
	var err error
	allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
	if err != nil {
		return nil, nil, err
	}
	filtered, ferr := filterRowMapsByWhere(e, s.Where, allRowMaps)
	return filtered, colDefs, ferr
}

// joinOrFilterRowMaps applies joins when the query has any, otherwise it
// filters the materialized rows with the WHERE clause. It returns the updated
// row maps (and column definitions when joins added columns).
func (e *SelectEngine) joinOrFilterRowMaps(s *sql.SelectStmt, allRows [][]interface{}, allRowMaps []RowMap, colDefs []sql.ColumnDef) ([]RowMap, []sql.ColumnDef, error) {
	if len(s.Joins) == 0 {
		_, maps, err := e.filterSubqueryRows(allRows, allRowMaps, s.Where)
		return maps, colDefs, err
	}
	return e.joinAndFilterRowMaps(s, allRowMaps, colDefs)
}

// execCTEPostProcess runs the outer query over a CTE's materialized rows:
// joins, WHERE filtering, aggregates, output-row building and finalization.
