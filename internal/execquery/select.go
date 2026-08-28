package execquery

import (
	"fmt"
	"math"

	"sort"
	"strconv"
	"strings"

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
	// Handle SELECT without FROM (e.g., SELECT 1, SELECT CASE...)
	if s.From.Name == "" && s.From.Subquery == nil && len(s.From.As) == 0 {
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
	if s.From.IsTabFunc && !isPragmaTableFunc(s.From.Name) {
		if _, isModule := e.ctx.VTables().Find(strings.ToLower(s.From.Name)); !isModule {
			if e.relationExists(s, s.From.Name) {
				return &Result{Error: fmt.Errorf("'%s' is not a function", s.From.Name)}, true
			}
			// Unknown name: fall through for the normal "no such table" path.
		}
	}

	// Handle CTE: check if the from table matches a CTE definition (either
	// declared on this statement or in an enclosing WITH clause).
	if cte, ok := e.findCTE(s, s.From.Name); ok {
		return e.execSelectCTE(s, &cte), true
	}
	if s.From.As != "" {
		if cte, ok := e.findCTE(s, s.From.As); ok {
			return e.execSelectCTE(s, &cte), true
		}
	}

	// Table-valued pragma functions: FROM pragma_table_info('t1')
	if isPragmaTableFunc(s.From.Name) {
		return e.ctx.ExecPragmaTableValued(s), true
	}

	// Table-valued virtual-table function: FROM generate_series(1,256)
	if len(s.From.Args) > 0 {
		// Correlated FROM-TVF inside a subquery: the argument references an
		// outer row's columns (EXISTS(SELECT 1 FROM json_each(t1.json,...))),
		// so evaluate the arguments against that outer row (SQLite runs the
		// vtab filter per outer row).
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
	}
	// Eponymous virtual table: FROM generate_series (no arguments) with
	// hidden-column constraints in WHERE (series.c, tabfunc01-1.1).
	opts := e.vtabScanOptions(s)
	residual := opts.Where
	opts.Residual = &residual
	if defs, rows, rowids, err, handled := e.ctx.TryMaterializeEponymousVtab(s.From, opts); handled {
		if err != nil {
			return &Result{Error: err}, true
		}
		return e.execSelectOverMaterializedRowids(e.withVtabResidualWhere(s, &opts), defs, rows, rowids), true
	}
	// Created virtual tables (CREATE VIRTUAL TABLE ... USING csv etc.):
	// RootPage 0 schema entries whose stored SQL names a registered module.
	opts = e.vtabScanOptions(s)
	createdResidual := opts.Where
	opts.Residual = &createdResidual
	if len(s.Joins) == 0 {
		if defs, rows, rowids, err, ok := e.ctx.MaterializeCreatedVTab(s.From.Name, opts); ok {
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
	}
	return nil, false
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
// generating once the cap is reached).
func (e *SelectEngine) vtabScanOptions(s *sql.SelectStmt) VtabScanOptions {
	opts := VtabScanOptions{Where: s.Where, MaxRows: -1}
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
		e.outerRows = []RowMap{emptyRow}
		e.outerRow = emptyRow
		outRow, err := e.buildOutputRow(s.Columns, colDefs, emptyRow)
		if err != nil {
			return &Result{Error: err}
		}
		e.outerRows = prevOuterRows
		columns := e.buildColumnNames(s.Columns, colDefs, s)
		result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
		return e.finalizeSelectResult(result, s, []RowMap{emptyRow})
	}
	prevOuterRows := e.outerRows
	e.outerRows = allRowMaps
	e.outerRow = allRowMaps[0] // provide first row for non-aggregate column refs
	outRow, err := e.buildOutputRow(s.Columns, colDefs, allRowMaps[0])
	if err != nil {
		return &Result{Error: err}
	}
	e.outerRows = prevOuterRows
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
	// For FTS virtual tables, use full SELECT processing (WHERE, ORDER BY, LIMIT).
	// A single-table FTS SELECT uses ExecFTSSelect (which sets the FTS match
	// context for MATCH evaluation); an FTS table in a JOIN needs the generic
	// join pipeline so the join/WHERE clauses are evaluated over combined rows.
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		// A %_content shadow btree that cannot be navigated fails any query
		// that reads content columns, including a JOIN scan (fts3corrupt4
		// 52.1: SELECT * FROM t1, t2 — SQLite's full scan steps the corrupt
		// content table and reports "database disk image is malformed").
		if len(s.Joins) > 0 && e.ftsReadsContentColumns(s, ftsTable) && e.contentBtreeCorrupt(tableEntry.Name) {
			return &Result{Error: fmt.Errorf("database disk image is malformed")}
		}
		if len(s.Joins) == 0 {
			return e.ctx.ExecFTSSelect(s, tableEntry, ftsTable, colDefs)
		}
		// Materialize the FTS rows (with the docid as rowid) and run the
		// generic join/WHERE/aggregate pipeline over them. The row maps must
		// carry the real docid so MATCH evaluation (which reads the row's
		// rowid) resolves the FTS document being matched.
		allRowMaps := e.ftsJoinRowMaps(ftsTable, colDefs, tableEntry.Name)
		allRows := make([][]interface{}, len(allRowMaps))
		for i, rowMap := range allRowMaps {
			allRows[i] = rowMapToValues(rowMap, colDefs)
		}
		return e.execSelectPostScan(s, allRows, allRowMaps, colDefs)
	}
	// Non-FTS virtual tables: materialize the rows (with an upper-bound
	// hint for bounded tables like wholenumber) and run the full SELECT
	// pipeline (WHERE, ORDER BY, LIMIT, aggregates) over them.
	// A virtual table whose module declares column names (e.g.
	// wholenumber's "value") provides the column definitions even when
	// the CREATE VIRTUAL TABLE has no explicit column list.
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
	v, err := e.ctx.EvalExpr(expr, nil)
	if err != nil {
		return expr, nil
	}
	switch n := util.UnwrapColumnValue(v).(type) {
	case int64:
		return &sql.NumericLit{Value: strconv.FormatInt(n, 10)}, nil
	case int:
		return &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}, nil
	case float64:
		if n == math.Trunc(n) {
			return &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}, nil
		}
		return nil, fmt.Errorf("datatype mismatch")
	case string:
		// SQLite casts the LIMIT expression to integer: LIMIT '4' == LIMIT 4,
		// LIMIT '1.0' == LIMIT 1, LIMIT '1.2' / 'abc' → datatype mismatch.
		if f, perr := strconv.ParseFloat(strings.TrimSpace(n), 64); perr == nil && f == math.Trunc(f) {
			return &sql.NumericLit{Value: strconv.FormatInt(int64(f), 10)}, nil
		}
		return nil, fmt.Errorf("datatype mismatch")
	case nil:
		return nil, fmt.Errorf("datatype mismatch")
	case []byte:
		// A blob LIMIT (LIMIT X'ABCD') cannot be cast to an integer.
		return nil, fmt.Errorf("datatype mismatch")
	}
	return expr, nil
}

func (e *SelectEngine) validateRowValueInList(v *sql.InList) error {
	if err := e.validateRowValueUse(v.Operand, false); err != nil {
		return err
	}
	for _, item := range v.List {
		if err := e.validateRowValueUse(item, false); err != nil {
			return err
		}
	}
	// Row-value IN subquery arity: (a,b) IN (SELECT * FROM t) requires
	// the subquery to return exactly len(operand) columns.
	if isRowValueExpr(v.Operand) && len(v.List) == 1 && isSubqueryExpr(v.List[0]) {
		arity := rowValueArity(v.Operand)
		if err := e.validateSubqueryArity(v.List[0], arity); err != nil {
			return err
		}
	}
	return nil
}

// isRowValueExpr reports whether expr is a row value (or a parenthesized row
// value).
func isRowValueExpr(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.RowValue:
		return true
	case *sql.ParenExpr:
		return isRowValueExpr(v.Expr)
	}
	return false
}

// rowValueArity returns the number of elements in a row value expression, or
// -1 if expr is not a row value.
func rowValueArity(expr sql.Expr) int {
	switch v := expr.(type) {
	case *sql.RowValue:
		return len(v.Values)
	case *sql.ParenExpr:
		return rowValueArity(v.Expr)
	}
	return -1
}

// isSubqueryExpr reports whether expr is a subquery (possibly parenthesized).
func isSubqueryExpr(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.Subquery:
		return true
	case *sql.ParenExpr:
		return isSubqueryExpr(v.Expr)
	}
	return false
}

// getCollationName extracts the collation name from a COLLATE expression's
// right operand (a StringLit or ColumnRef).
func getCollationName(expr sql.Expr) string {
	switch v := expr.(type) {
	case *sql.StringLit:
		return v.Value
	case *sql.ColumnRef:
		return v.Name
	}
	return ""
}

// validateSubqueryArity checks that a subquery returns exactly wantCols
// columns, raising "sub-select returns N columns - expected M" otherwise
// (SQLite: `(a,b) IN (SELECT x, y, z ...)` with a 3-column subquery). A
// `SELECT *` column is resolved to the table's column count via the schema.
func (e *SelectEngine) validateSubqueryArity(expr sql.Expr, wantCols int) error {
	sub := expr
	for {
		if p, ok := sub.(*sql.ParenExpr); ok {
			sub = p.Expr
			continue
		}
		break
	}
	sq, ok := sub.(*sql.Subquery)
	if !ok {
		return nil
	}
	n := e.subqueryColumnCount(sq.Select)
	if n != wantCols {
		return fmt.Errorf("sub-select returns %d columns - expected %d", n, wantCols)
	}
	return nil
}

// subqueryColumnCount returns the number of result columns a SELECT produces,
// resolving a single `SELECT *` column to the FROM table's column count.
func (e *SelectEngine) subqueryColumnCount(s *sql.SelectStmt) int {
	if len(s.Columns) != 1 {
		return len(s.Columns)
	}
	ref, ok := s.Columns[0].Expr.(*sql.ColumnRef)
	if !ok || ref.Name != "*" {
		return len(s.Columns)
	}
	// SELECT * FROM (subquery): star expands to the subquery's columns.
	// A nil/empty From (no FROM clause) contributes no columns.
	if s.From.Subquery != nil {
		return e.subqueryColumnCount(s.From.Subquery)
	}
	if s.From.Name == "" {
		return 0
	}
	entry, _, err := e.ctx.FindTable(s.From.Name)
	if err != nil {
		return len(s.Columns)
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	count := 0
	for _, cd := range colDefs {
		if !cd.Dropped {
			count++
		}
	}
	return count
}

// validateSelectColumnRefs checks that every column reference in a SELECT
// (select list, WHERE, GROUP BY, HAVING, ORDER BY) resolves to a column of
// the scanned table. SQLite reports unknown columns at prepare time; without
// this check an unknown column would silently evaluate to NULL.
//
// selectAliasMap builds the output-column alias map for a SELECT statement:
// alias name → select-list expression (e.g. "SELECT a AS x" maps x → a). The
// map is used at evaluation time so WHERE/GROUP BY/HAVING can reference an
// alias when the name is not a table column (SQLite resolves the reference to
// the alias's expression). Returns nil when the SELECT has no aliases.
func selectAliasMap(s *sql.SelectStmt) map[string]sql.Expr {
	if s == nil {
		return nil
	}
	var m map[string]sql.Expr
	for _, col := range s.Columns {
		if col.As != "" {
			if m == nil {
				m = make(map[string]sql.Expr)
			}
			m[strings.ToLower(col.As)] = col.Expr
		}
	}
	return m
}

// resolveAliasRef looks up an unqualified column reference in the enclosing
// SELECTs' output-column alias maps (innermost first). It returns the alias
// expression and true when found.
func (e *SelectEngine) resolveAliasRef(name string) (sql.Expr, bool) {
	for i := len(e.aliasStack) - 1; i >= 0; i-- {
		if expr, ok := e.aliasStack[i][strings.ToLower(name)]; ok {
			return expr, true
		}
	}
	return nil, false
}

// aliasStackTop reports whether name is an output-column alias in the
// innermost SELECT's alias map.
func (e *SelectEngine) aliasStackTop(name string) (sql.Expr, bool) {
	if len(e.aliasStack) == 0 {
		return nil, false
	}
	expr, ok := e.aliasStack[len(e.aliasStack)-1][strings.ToLower(name)]
	return expr, ok
}

// validateUnionSubqueryNoAggs checks that a subquery used in FROM does not
// contain aggregates inside a UNION ALL. SQLite prohibits this pattern:
// SELECT * FROM (SELECT 1 UNION ALL SELECT sum(x) FROM t) -- invalid
func validateUnionSubqueryNoAggs(s *sql.SelectStmt) error {
	if s.Union != nil {
		// SQLite rejects an aggregate in a UNION member only when the member
		// is not an aggregate query itself (no GROUP BY and no aggregate
		// context). A grouped SELECT (SELECT a, sum(b) FROM t GROUP BY a)
		// legitimately uses aggregates inside a UNION.
		checkMember := func(m *sql.SelectStmt) error {
			if len(m.GroupBy) == 0 && !hasAggregateInColumns(m.Columns) {
				if nested := findAggregateInSelect(m); nested != "" {
					return fmt.Errorf("misuse of aggregate: %s()", nested)
				}
			}
			return nil
		}
		if err := checkMember(s); err != nil {
			return err
		}
		if err := checkMember(s.Union); err != nil {
			return err
		}
	}
	// Recurse into nested FROM subqueries
	if s.From.Subquery != nil {
		return validateUnionSubqueryNoAggs(s.From.Subquery)
	}
	return nil
}

// hasAggregateInColumns reports whether any column expression is an aggregate
// function call (used to recognize aggregate queries without GROUP BY).
func hasAggregateInColumns(cols []sql.SelectColumn) bool {
	for _, c := range cols {
		if FindAggregateInExpr(c.Expr) != "" {
			return true
		}
	}
	return false
}

// findAggregateInSelect checks if a SELECT statement directly contains an aggregate function.
func findAggregateInSelect(s *sql.SelectStmt) string {
	for _, col := range s.Columns {
		if nested := FindAggregateInExpr(col.Expr); nested != "" {
			return nested
		}
	}
	return ""
}
func applyLimitOffset(rows [][]interface{}, limit, offset sql.Expr) [][]interface{} {
	if limit == nil {
		return rows
	}
	l, ok := sql.EvalNumber(limit)
	if !ok || l < 0 {
		// Can't evaluate or negative limit → no upper bound
		l = int64(len(rows))
	}
	o := int64(0)
	if offset != nil {
		o, _ = sql.EvalNumber(offset)
	}
	if o < 0 {
		o = 0
	}
	if o > int64(len(rows)) {
		return [][]interface{}{}
	}
	if l == 0 {
		return [][]interface{}{}
	}
	end := o + l
	if end > int64(len(rows)) {
		end = int64(len(rows))
	}
	return rows[o:end]
}

// lookupRowMapValue fetches a column value from a RowMap, trying both the
// qualified (alias.col / table.col) and unqualified forms.

// SubqueryColumnCount returns the number of output columns of a subquery
// SELECT. Exported for the expression evaluator's BETWEEN subquery arity.
func (e *SelectEngine) SubqueryColumnCount(s *sql.SelectStmt) int {
	return e.subqueryColumnCount(s)
}

// ResolveAliasRef looks up a SELECT output-column alias by name. Exported
// for the expression evaluator's alias resolution.
func (e *SelectEngine) ResolveAliasRef(name string) (sql.Expr, bool) {
	return e.resolveAliasRef(name)
}

// selectReferencesRowID reports whether the statement's output columns or
// WHERE clause contain an unqualified rowid/_rowid_/oid reference.
func selectReferencesRowID(s *sql.SelectStmt) bool {
	if s == nil {
		return false
	}
	check := func(ex sql.Expr) bool {
		found := false
		WalkExprFull(ex, func(e2 sql.Expr) {
			if cr, ok := e2.(*sql.ColumnRef); ok && cr.Table == "" && isRowIDName(cr.Name) {
				found = true
			}
		})
		return found
	}
	for _, col := range s.Columns {
		if check(col.Expr) {
			return true
		}
	}
	return check(s.Where)
}
