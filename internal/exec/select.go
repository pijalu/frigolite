// Package exec implements query execution.
package exec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// --- SELECT ---

// handleSelectAggregates evaluates aggregates. Returns the result if aggregates
// were processed and a result is available, or nil if no aggregates or empty result.
func (e *Engine) handleSelectAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	hasAggs := e.hasAggregates(s.Columns)
	if hasAggs {
		if len(s.GroupBy) > 0 {
			result := e.evalAggregatesGroupBy(s, rowMaps, colDefs)
			if result != nil {
				return result
			}
		} else {
			result := e.evalAggregates(s, rowMaps)
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

// evalGroupByNoAggs handles GROUP BY without aggregate functions.
// It groups the row maps by the GROUP BY key, then for each group uses
// buildOutputRow to build the output row (properly handling * expansion).
func (e *Engine) evalGroupByNoAggs(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if len(rowMaps) == 0 {
		return nil
	}

	// Partition rows by GROUP BY key, keeping each key's representative
	// values so the groups can be emitted in key order (SQLite sorts groups).
	groups := make(map[string][]RowMap)
	keyVals := make(map[string][]interface{})
	var keyOrder []string

	groupBy := resolveGroupByOrdinals(s, colDefs)
	for _, row := range rowMaps {
		key, vals := e.computeGroupByKeyValues(groupBy, row)
		if _, exists := groups[key]; !exists {
			keyOrder = append(keyOrder, key)
			keyVals[key] = vals
		}
		groups[key] = append(groups[key], row)
	}
	e.sortGroupKeys(keyOrder, keyVals)

	var outRows [][]interface{}
	for _, key := range keyOrder {
		groupRows := groups[key]
		groupVals := keyVals[key]
		// Apply HAVING filter
		if s.Having != nil {
			match, err := e.evalHaving(s.Having, groupRows)
			if err != nil || !match {
				continue
			}
		}
		// Use the first row of the group as the representative for non-aggregated
		// columns. Output columns that are themselves GROUP BY expressions use
		// the group's key value (re-evaluating would break non-deterministic
		// expressions like random() and float formatting).
		row := groupRows[0]
		outRow := e.buildOutputRow(s.Columns, colDefs, row)
		for ci := range s.Columns {
			if gi := matchGroupByExpr(groupBy, s.Columns[ci].Expr); gi >= 0 && gi < len(groupVals) {
				if ci < len(outRow) {
					outRow[ci] = groupVals[gi]
				}
			}
		}
		outRows = append(outRows, outRow)
	}

	columns := e.buildColumnNames(s.Columns, colDefs, s)
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: outRows}, s, nil)
}

// sortGroupKeys orders GROUP BY output groups by their evaluated key values,
// matching SQLite's sorted-group output (NULL sorts first).
func (e *Engine) sortGroupKeys(keyOrder []string, keyVals map[string][]interface{}) {
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
func buildIndexSQL(name, table string, columns []sql.IndexColumn, unique bool, where sql.Expr) string {
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

func (e *Engine) execSelect(s *sql.SelectStmt) *Result {
	// Track SELECT nesting depth so PRAGMA reverse_unordered_selects only
	// reverses the top-level SELECT, never subqueries or join members.
	e.selectDepth++
	defer func() { e.selectDepth-- }()

	// Validate expressions before executing: check for invalid ORDER BY usage and
	// aggregates inside UNION ALL in subqueries.
	if err := e.validateSelectExprs(s); err != nil {
		return &Result{Error: err}
	}

	// Push this statement's WITH (CTE) definitions so nested subqueries can
	// resolve them by name (SQLite's name resolver consults enclosing WITH
	// clauses). Pop on return regardless of path. This must happen before
	// compound column-count validation so a CTE reference in a union member
	// (SELECT * FROM VVV UNION ALL ...) can resolve its column count.
	if len(s.CTEs) > 0 {
		if dup := duplicateCTEName(s.CTEs); dup != "" {
			return &Result{Error: fmt.Errorf("duplicate WITH table name: %s", dup)}
		}
		e.cteScopes = append(e.cteScopes, s.CTEs)
		defer func() { e.cteScopes = e.cteScopes[:len(e.cteScopes)-1] }()
	}

	// Validate compound SELECT column-count consistency (SQLite does this at
	// prepare time, before any rows are produced).
	if err := e.validateCompoundColumnCounts(s); err != nil {
		return &Result{Error: err}
	}

	// Push this statement's output-column aliases (SELECT expr AS x) so the
	// WHERE/GROUP BY/HAVING clauses can reference them when the name is not a
	// table column (SQLite resolves such references to the alias expression).
	// Aliases are pushed before sub-query dispatch so every execution path
	// (scan, JOIN, materialized, no-FROM) sees them during WHERE evaluation.
	if aliasMap := selectAliasMap(s); len(aliasMap) > 0 {
		e.aliasStack = append(e.aliasStack, aliasMap)
		defer func() { e.aliasStack = e.aliasStack[:len(e.aliasStack)-1] }()
	}

	// Handle SELECT without FROM (e.g., SELECT 1, SELECT CASE...)
	if s.From.Name == "" && s.From.Subquery == nil && len(s.From.As) == 0 {
		return e.execSelectNoFrom(s)
	}

	// Handle subquery in FROM: (SELECT ...) AS t
	if s.From.Subquery != nil {
		return e.execSelectFromSubquery(s)
	}

	// Handle CTE: check if the from table matches a CTE definition (either
	// declared on this statement or in an enclosing WITH clause).
	if cte, ok := e.findCTE(s, s.From.Name); ok {
		return e.execSelectCTE(s, &cte)
	}
	if s.From.As != "" {
		if cte, ok := e.findCTE(s, s.From.As); ok {
			return e.execSelectCTE(s, &cte)
		}
	}

	// Table-valued pragma functions: FROM pragma_table_info('t1')
	if isPragmaTableFunc(s.From.Name) {
		return e.execPragmaTableValued(s)
	}

	// Table-valued virtual-table function: FROM generate_series(1,256)
	if len(s.From.Args) > 0 {
		if colDefs, rows, err := e.materializeVtabTableFunc(s.From); err == nil {
			return e.execSelectOverMaterialized(s, colDefs, rows)
		} else if !isNoSuchVtabErr(err) {
			return &Result{Error: err}
		}
	}

	tableEntry, dbCtx, err := e.findTable(s.From.Name)
	if err != nil {
		viewEntry, viewCtx, viewErr := e.findView(s.From.Name)
		if viewErr != nil {
			// SQLite prefixes a missing table in a main-schema view's body
			// with "main." ("no such table: main.txx", alterlegacy-3.1.2b);
			// temp-schema views use the bare name (alterlegacy-3.3.1).
			if s.From.Name != "" && !strings.HasPrefix(err.Error(), "no such table: main.") {
				if e.expandingView && !e.expandingTempView {
					return &Result{Error: fmt.Errorf("no such table: main.%s", s.From.Name)}
				}
			}
			return &Result{Error: err}
		}
		// Check for circular view reference
		if e.resolvingViews[s.From.Name] {
			return &Result{Error: fmt.Errorf("view %s is circularly defined", s.From.Name)}
		}
		if e.resolvingViews == nil {
			e.resolvingViews = make(map[string]bool)
		}
		e.resolvingViews[s.From.Name] = true
		result := e.execSelectViewWithOuter(s, viewEntry, viewCtx)
		delete(e.resolvingViews, s.From.Name)
		return result
	}

	// INDEXED BY clause: validate the named index exists and can serve the
	// query. SQLite raises "no such index" for a missing index and "no
	// query solution" when the forced index cannot be used (e.g. a partial
	// index whose predicate is not implied by the query's WHERE clause).
	// The engine still scans the table for correct results; this check only
	// enforces the INDEXED BY contract.
	if s.From.IndexedBy != "" && len(s.Joins) == 0 {
		if err := e.validateIndexedBy(tableEntry, s.From.IndexedBy, s); err != nil {
			return &Result{Error: err}
		}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// Resolve collations used by the WHERE clause. SQLite raises
	// "no such collation sequence: X" at compile time when a comparison
	// references a column whose declared collation is unknown. Only the
	// single-table (no JOIN) case is checked here; joins resolve collations
	// per-table elsewhere.
	if len(s.Joins) == 0 && s.Where != nil {
		if err := e.checkWhereCollations(s.Where, colDefs, s.From); err != nil {
			return &Result{Error: err}
		}
	}

	// WITHOUT ROWID tables have no rowid/_rowid_/oid columns. SQLite rejects
	// any reference to them with "no such column".
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		if ref := e.findRowIDRef(s, tableEntry.Name, s.From.As, len(s.Joins) > 0); ref != "" {
			return &Result{Error: fmt.Errorf("no such column: %s", ref)}
		}
	}

	// Check if this is a virtual table (RootPage = 0)
	if tableEntry.RootPage == 0 {
		// For FTS virtual tables, use full SELECT processing (WHERE, ORDER BY, LIMIT)
		if ftsTable, ok := e.ftsTables[tableEntry.Name]; ok {
			return e.execFTSSelect(s, tableEntry, ftsTable, colDefs)
		}
		// Non-FTS virtual tables: materialize the rows (with an upper-bound
		// hint for bounded tables like wholenumber) and run the full SELECT
		// pipeline (WHERE, ORDER BY, LIMIT, aggregates) over them.
		// A virtual table whose module declares column names (e.g.
		// wholenumber's "value") provides the column definitions even when
		// the CREATE VIRTUAL TABLE has no explicit column list.
		if len(colDefs) == 0 {
			if moduleName, _, perr := parseVTabSQL(tableEntry.SQL); perr == nil {
				if module, found := e.vtabs.Find(moduleName); found {
					if inst, cerr := module.Connect(nil); cerr == nil {
						if ci, ok := inst.(vtab.ColumnInfo); ok {
							for _, name := range ci.Columns() {
								colDefs = append(colDefs, sql.ColumnDef{Name: name, Type: ""})
							}
						}
					}
				}
			}
		}
		var bound int64
		if b, ok := vtabUpperBound(s.Where); ok {
			bound = b
		}
		rows, err := e.virtualTableRows(tableEntry, bound)
		if err != nil {
			return &Result{Error: err}
		}
		return e.execSelectOverMaterialized(s, colDefs, rows)
	}

	// Validate that every column reference in the SELECT (select list, WHERE,
	// GROUP BY, HAVING, ORDER BY) resolves to a column of the scanned table.
	// SQLite reports unknown columns at prepare time; without this check an
	// unknown column silently evaluates to NULL. (Virtual tables are handled
	// above where the table-name pseudo-column is legal, e.g. FTS MATCH.)
	if len(s.Joins) == 0 && e.outerRow == nil {
		if err := e.validateSelectColumnRefs(s, colDefs, tableEntry.Name, s.From.As); err != nil {
			return &Result{Error: err}
		}
	}

	// OR-index optimization: WHERE of the form (a=1 AND b=2) OR (c=3 AND d=4)
	// where every OR term constrains the leading columns of some index.
	// SQLite unions the matching rowids in index scan order (deduplicating),
	// which determines the output order when there is no ORDER BY. The plan is
	// skipped when correlated outer rows are active or reverse_unordered_selects
	// is on (which forces a plain table scan order).
	if len(s.Joins) == 0 && s.Where != nil && e.outerRow == nil && len(e.outerRows) == 0 && !e.reverseUnordered {
		if branches, ok := e.planOrIndexScan(s.Where, tableEntry.Name, colDefs, dbCtx); ok {
			return e.execSelectWithOrPlan(s, tableEntry, dbCtx, colDefs, branches)
		}
	}

	tree := e.tableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}

	// Track the table being scanned for qualified column resolution.
	// Save/restore for nested subqueries. Use the alias if present (e.g.,
	// FROM t1 AS x → currentScanTable = "x"), otherwise use the table name.
	prevScanTable := e.currentScanTable
	e.currentScanTable = tableEntry.Name
	if s.From.As != "" {
		e.currentScanTable = s.From.As
	}
	defer func() { e.currentScanTable = prevScanTable }()

	// Determine if row maps are needed for later processing stages.
	// The fast path (needMaps=false) avoids per-row RowMap allocation
	// when no expression evaluation, sorting, filtering, or combining is required.
	needMaps := selectNeedsRowMaps(e, s, tableEntry.Name)

	// WITHOUT ROWID tables need PK-ordered output. We force needMaps to get
	// row maps with all column data for sorting.
	isWithoutRowidTable := len(s.Joins) == 0 && len(s.OrderBy) == 0 &&
		hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	var withoutRowidPKCols []string
	if isWithoutRowidTable {
		withoutRowidPKCols = pkColumnNames(tableEntry.SQL, colDefs)
		if len(withoutRowidPKCols) > 0 {
			needMaps = true
		}
	}

	allRows, allRowMaps, err := e.scanTableRows(cursor, s, colDefs, needMaps)
	if err != nil {
		return &Result{Error: err}
	}

	// WITHOUT ROWID tables store data in PK order. Since Frigolite uses
	// rowid-based storage for all tables, we sort the results by PK columns
	// to emulate WITHOUT ROWID ordering when there is no explicit ORDER BY
	// and no JOINs.
	if len(withoutRowidPKCols) > 0 && len(allRowMaps) > 0 {
		sortRowMapsByPKNames(allRowMaps, withoutRowidPKCols)
		// Re-project rows from sorted row maps
		for i := range allRows {
			allRows[i] = e.buildOutputRow(s.Columns, colDefs, allRowMaps[i])
		}
	}

	// Filter out internal system tables when querying sqlite_master/sqlite_schema.
	// SQLite hides sqlite_stat1, sqlite_stat4, and similar internal tables from
	// direct schema queries while still allowing direct table access by name.
	if isSchemaTable(tableEntry.Name) && len(allRowMaps) > 0 {
		allRows, allRowMaps = e.filterSystemTables(allRows, allRowMaps, colDefs)
	}

	// If outerRows is set (from a parent collapse) and this query has aggregates
	// referencing only outer columns, evaluate them over all outer rows while
	// using the first inner row for non-aggregate columns.
	if len(e.outerRows) > 0 && e.hasAggregates(s.Columns) {
		// Build set of inner column names from the scanned rows
		innerColNames := make(map[string]bool)
		for _, cd := range colDefs {
			innerColNames[cd.Name] = true
		}
		// Check if any aggregate references inner columns — if so, fall through to normal handling
		allOuterRefs := true
		for _, col := range s.Columns {
			if fn, ok := col.Expr.(*sql.FuncCall); ok {
				if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
					if !e.aggregateHasOnlyOuterRefs(fn, innerColNames) {
						allOuterRefs = false
						break
					}
				}
			}
		}
		if allOuterRefs {
			columns := e.buildColumnNames(s.Columns, colDefs, s)
			outRow := e.evalAggOverOuterRowsWithInner(s, e.outerRows, allRowMaps)
			result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
			return e.finalizeSelectResult(result, s, allRowMaps)
		}
	}

	// If any SELECT column contains a subquery with a correlated aggregate,
	// re-evaluate the SELECT columns with outerRows set to all rowMaps.
	// This allows the aggregate to evaluate over all outer rows (collapsing to 1).
	if len(allRowMaps) > 0 && e.hasSubqueryWithCorrelatedAgg(s.Columns) {
		prevOuterRows := e.outerRows
		e.outerRows = allRowMaps
		e.outerRow = allRowMaps[0] // provide first row for non-aggregate column refs
		outRow := e.buildOutputRow(s.Columns, colDefs, allRowMaps[0])
		e.outerRows = prevOuterRows
		columns := e.buildColumnNames(s.Columns, colDefs, s)
		result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
		return e.finalizeSelectResult(result, s, allRowMaps)
	}

	// If there are JOINs, process them (nested-loop join)
	if len(s.Joins) > 0 {
		// SQLite rejects unqualified column references that are ambiguous
		// across the joined tables at prepare time (e.g. "SELECT rowid FROM
		// t2, t3" → "ambiguous column name: rowid").
		if err := e.validateAmbiguousColumnRefs(s); err != nil {
			return &Result{Error: err}
		}
		var err error
		allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
		if err != nil {
			return &Result{Error: err}
		}
		// Apply the WHERE filter to the joined result. execJoins only applies
		// per-join ON conditions; the statement-level WHERE must be applied
		// after the full join is built.
		if s.Where != nil {
			filtered := allRowMaps[:0]
			for _, rowMap := range allRowMaps {
				pass, err := e.rowPassesWhere(s.Where, rowMap, nil)
				if err != nil {
					return &Result{Error: err}
				}
				if pass {
					filtered = append(filtered, rowMap)
				}
			}
			allRowMaps = filtered
		}
		// Rebuild allRows from combined row maps using SELECT columns
		allRows = make([][]interface{}, len(allRowMaps))
		for i, rowMap := range allRowMaps {
			allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
		}
	}

	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: allRows}
	// Preserve the joined row maps (with qualified keys like t4.a) for
	// derived-table materialization when the query joins tables. Only do this
	// when the SELECT projects plain columns (the join maps align with the
	// output); computed expressions (coalesce(b,3) AS b2) are NOT in the join
	// maps, so reusing them would lose the projected column.
	if len(s.Joins) > 0 && selectProjectsPlainColumns(s.Columns) {
		result.rowMaps = allRowMaps
	}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// finalizeSelectResult applies DISTINCT, ORDER BY, LIMIT, and UNION.
func (e *Engine) finalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result {
	// The collation of each result column of a compound query comes from the
	// leftmost SELECT member (SQLite's compound column collation rule).
	colls := e.selectOutputCollations(s)
	if s.Distinct {
		result.Rows, rowMaps = e.distinctRows(result.Rows, rowMaps, colls, s)
	}
	// Handle UNION before ORDER BY (ORDER BY on compound SELECT applies to the merged result).
	// The parser attaches a trailing ORDER BY / LIMIT / OFFSET to the LAST member
	// of the compound chain (SQLite does the same); find it and apply it here.
	orderBy := s.OrderBy
	limit := s.Limit
	offset := s.Offset
	if s.Union != nil {
		// Merge the FULL compound chain, one member at a time. A member may
		// itself be a multi-tuple VALUES (chain of UNION ALL members), so
		// evaluate each member's own rows (Union detached) and apply the set
		// operator carried on the link.
		cur := s
		for cur.Union != nil {
			member := cur.Union
			if member.ValuesChain {
				// A VALUES member is a single operand: evaluate its full
				// tuple list (the internal UNION ALL chain of one node per
				// tuple) as one set before the link's operator applies.
				memberResult := e.execValuesGroup(member)
				if memberResult.Error != nil {
					return memberResult
				}
				result.Rows = e.applySetOp(result.Rows, memberResult.Rows, cur.SetOp, cur.UnionAll, colls)
				// Skip the VALUES group's internal tuple nodes (links that
				// are UNION ALL); stop at the next real compound member.
				for member.Union != nil && member.SetOp == sql.SetUnion && member.UnionAll {
					member = member.Union
				}
				cur = member
				continue
			}
			memberCopy := *member
			memberCopy.Union = nil
			prevCompound := e.inCompoundMember
			e.inCompoundMember = true
			memberResult := e.execSelect(&memberCopy)
			e.inCompoundMember = prevCompound
			if memberResult.Error != nil {
				return memberResult
			}
			if len(memberResult.Columns) != len(result.Columns) {
				return &Result{Error: fmt.Errorf("SELECTs to the left and right of %s do not have the same number of result columns", setOpName(cur.SetOp, cur.UnionAll))}
			}
			result.Rows = e.applySetOp(result.Rows, memberResult.Rows, cur.SetOp, cur.UnionAll, colls)
			cur = member
		}
		last := cur
		if len(last.OrderBy) > 0 {
			orderBy = last.OrderBy
		}
		if last.Limit != nil {
			limit = last.Limit
		}
		if last.Offset != nil {
			offset = last.Offset
		}
		// The head's rowMaps only cover its own rows; rebuild them from the
		// merged result so ORDER BY can resolve columns across all members.
		rowMaps = make([]RowMap, len(result.Rows))
		for i, row := range result.Rows {
			m := make(RowMap)
			for j, v := range row {
				if j < len(result.Columns) {
					m[result.Columns[j]] = v
				}
			}
			rowMaps[i] = m
		}
	}
	if len(orderBy) > 0 {
		if err := validateOrderBy(orderBy, len(result.Columns)); err != nil {
			return &Result{Error: err}
		}
		// Compound queries restrict ORDER BY terms to result column names or
		// ordinals (SQLite: expressions from underlying tables are rejected
		// with "Nth ORDER BY term does not match any column in the result
		// set" unless they match a result column).
		if s.Union != nil {
			if err := e.validateCompoundOrderBy(s, orderBy); err != nil {
				return &Result{Error: err}
			}
		}
		e.sortRowsWithMaps(result, orderBy, rowMaps)
	}
	result.Rows = applyLimitOffset(result.Rows, e.evalLimitExpr(limit), e.evalLimitExpr(offset))
	return result
}

// evalLimitExpr evaluates a LIMIT/OFFSET expression (which may be a scalar
// subquery) to a numeric literal so applyLimitOffset can consume it. When
// evaluation fails or the value is not numeric (e.g. a correlated expression),
// the raw expression is returned unchanged.
func (e *Engine) evalLimitExpr(expr sql.Expr) sql.Expr {
	if expr == nil {
		return nil
	}
	v, err := e.evalExpr(expr, nil)
	if err != nil {
		return expr
	}
	switch n := v.(type) {
	case int64:
		return &sql.NumericLit{Value: strconv.FormatInt(n, 10)}
	case float64:
		return &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}
	}
	return expr
}

func (e *Engine) mergeUnionRows(rows [][]interface{}, union *sql.SelectStmt, op sql.SetOp, unionAll bool) [][]interface{} {
	unionResult := e.execSelect(union)
	if unionResult.Error != nil {
		return rows
	}
	return e.applySetOp(rows, unionResult.Rows, op, unionAll, nil)
}

// applySetOp combines left and right row sets with a compound set operator.
// colls holds the collation of each result column (from the leftmost member
// of the compound query), used to deduplicate/intersect with SQLite's column
// collation semantics; nil means BINARY for all columns.
func (e *Engine) applySetOp(rows, rightRows [][]interface{}, op sql.SetOp, unionAll bool, colls []string) [][]interface{} {
	switch op {
	case sql.SetUnion:
		if unionAll {
			// UNION ALL: concatenate without dedup
			return append(rows, rightRows...)
		}
		// UNION: deduplicate combined rows; SQLite's temp b-tree emits
		// the unique rows in sorted key order.
		return e.sortSetOpRows(e.dedupeRows(append(rows, rightRows...), colls), colls)
	case sql.SetIntersect:
		// INTERSECT: rows that appear in both sets
		return e.sortSetOpRows(e.intersectRows(rows, rightRows, colls), colls)
	case sql.SetExcept:
		// EXCEPT: rows in left but not in right
		return e.sortSetOpRows(e.exceptRows(rows, rightRows, colls), colls)
	default:
		return append(rows, rightRows...)
	}
}

// sortSetOpRows sorts compound SELECT (UNION/INTERSECT/EXCEPT) output rows
// by their result values, matching SQLite's ephemeral b-tree ordering
// (NULL first, then INTEGER/REAL, then TEXT, then BLOB, with the leftmost
// result column's collation applied to text comparisons).
func (e *Engine) sortSetOpRows(rows [][]interface{}, colls []string) [][]interface{} {
	if len(rows) < 2 {
		return rows
	}
	out := make([][]interface{}, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		n := len(a)
		if len(b) < n {
			n = len(b)
		}
		for k := 0; k < n; k++ {
			coll := ""
			if colls != nil && k < len(colls) {
				coll = colls[k]
			}
			cmp := e.compareValuesCollate(util.UnwrapColumnValue(a[k]), util.UnwrapColumnValue(b[k]), coll)
			if cmp != 0 {
				return cmp < 0
			}
		}
		return false
	})
	return out
}

// compoundSelectColCount returns the declared output column count of a
// single SELECT member of a compound query, expanding "*" / "t.*" through
// the schema. It is used to validate that all members of a compound query
// have the same number of result columns (SQLite reports this error at
// prepare time, including under EXPLAIN QUERY PLAN).
// resolveTableColumnNames returns the column names for a FROM/join table
// reference, resolving CTE names before falling back to real tables/views.
// Used by compound column-count validation, which runs before the CTE scope
// is pushed onto e.cteScopes (execSelect pushes CTEs after validation).
func (e *Engine) resolveTableColumnNames(s *sql.SelectStmt, name string) ([]string, error) {
	if cte, ok := e.findCTE(s, name); ok && cte.Select != nil {
		// The CTE's output column names come from its SELECT body (a VALUES
		// body exposes column1..columnN).
		res := e.execSelect(cte.Select)
		if res.Error != nil {
			return nil, res.Error
		}
		cols := make([]string, len(res.Columns))
		copy(cols, res.Columns)
		if len(cte.Columns) > 0 {
			for i := 0; i < len(cols) && i < len(cte.Columns); i++ {
				cols[i] = cte.Columns[i]
			}
		}
		return cols, nil
	}
	return e.tableColumnNames(name)
}

func (e *Engine) compoundSelectColCount(s *sql.SelectStmt) (int, error) {
	count := 0
	for _, col := range s.Columns {
		ref, ok := col.Expr.(*sql.ColumnRef)
		if ok && ref.Name == "*" {
			// Star expansion: count the columns of the referenced table or
			// subquery. When the FROM clause joins multiple tables, count
			// every joined table's columns (SQLite expands * across all
			// tables; USING/NATURAL merges happen at execution time, and the
			// merged column still appears once from the left table).
			var n int
			if ref.Table != "" {
				cols, err := e.resolveTableColumnNames(s, ref.Table)
				if err != nil {
					return 0, err
				}
				n = len(cols)
			} else if s.From.Subquery != nil {
				subCols, err := e.compoundSelectColCount(s.From.Subquery)
				if err != nil {
					return 0, err
				}
				n = subCols
			} else if s.From.Name != "" {
				cols, err := e.resolveTableColumnNames(s, s.From.Name)
				if err != nil {
					return 0, err
				}
				n = len(cols)
				leftColNames := cols
				for _, j := range s.Joins {
					var jcols []string
					if j.Table.Subquery != nil {
						// Derive the subquery's real column names when possible
						// (so USING/NATURAL merge detection works); fall back to
						// generic names for the count.
						subCols, err := e.compoundSelectColCount(j.Table.Subquery)
						if err != nil {
							return 0, err
						}
						for _, col := range j.Table.Subquery.Columns {
							if col.As != "" {
								jcols = append(jcols, col.As)
							} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
								jcols = append(jcols, ref.Name)
							} else {
								jcols = append(jcols, "")
							}
						}
						// If the subquery is a bare * (e.g. SELECT * FROM t13), expand
						// through all its FROM sources (joins included).
						var expanded []string
						for _, n := range jcols {
							if n == "*" {
								for _, cd := range e.viewColumnDefsFromSelect(j.Table.Subquery) {
									expanded = append(expanded, cd.Name)
								}
							} else {
								expanded = append(expanded, n)
							}
						}
						if len(expanded) > 0 {
							jcols = expanded
						} else {
							for i := 0; i < subCols; i++ {
								jcols = append(jcols, fmt.Sprintf("_c%d", i))
							}
						}
					} else {
						jcols, err = e.resolveTableColumnNames(s, j.Table.Name)
						if err != nil {
							return 0, err
						}
					}
					n += len(jcols)
					// USING/NATURAL joins merge their columns: the output
					// excludes the merged column from the right side.
					if len(j.Using) > 0 || isNaturalJoinType(j.JoinType) {
						leftNames := map[string]bool{}
						for _, c := range leftColNames {
							leftNames[c] = true
						}
						for _, c := range jcols {
							if leftNames[c] {
								n--
							}
						}
					}
					leftColNames = append(leftColNames, jcols...)
				}
			} else {
				return 0, fmt.Errorf("no tables specified")
			}
			count += n
		} else {
			count++
		}
	}
	return count, nil
}

// validateCompoundColumnCounts checks that all members of a compound SELECT
// chain produce the same number of result columns, matching SQLite's
// "SELECTs to the left and right of <OP> do not have the same number of
// result columns" error.
func (e *Engine) validateCompoundColumnCounts(s *sql.SelectStmt) error {
	if s.Union == nil {
		return nil
	}
	headCount, err := e.compoundSelectColCount(s)
	if err != nil {
		return err
	}
	cur := s
	for cur.Union != nil {
		member := cur.Union
		if member.ValuesChain {
			// VALUES members contribute one column per expression.
			for member.Union != nil && member.SetOp == sql.SetUnion && member.UnionAll {
				member = member.Union
			}
			cur = member
			continue
		}
		memberCount, err := e.compoundSelectColCount(member)
		if err != nil {
			return err
		}
		if memberCount != headCount {
			return fmt.Errorf("SELECTs to the left and right of %s do not have the same number of result columns", setOpName(cur.SetOp, cur.UnionAll))
		}
		cur = member
	}
	return nil
}

// isHiddenColumnDef reports whether a column definition is hidden (a HIDDEN
// virtual-table column or an internal __hidden__-prefixed column). Hidden
// columns are excluded from bare * expansion and PRAGMA table_info but remain
// readable by explicit column references.
func isHiddenColumnDef(cd sql.ColumnDef) bool {
	return cd.Hidden || strings.HasPrefix(cd.Name, "__hidden__")
}

// tableColumnNames returns the column names of a table (or view), resolving
// schema entries by name. For a view, columns whose alias starts with
// "__hidden__" are excluded from a bare * expansion (SQLite's hidden-column
// feature: they remain usable by qualified/trigger references but do not
// appear in SELECT *).
func (e *Engine) tableColumnNames(tableName string) ([]string, error) {
	entry, _, err := e.findTable(tableName)
	if err != nil {
		// Views are resolved through a different path; try the main schema.
		if v, verr := e.mainDB.Schema.FindView(tableName); verr == nil && v != nil {
			// Prefer the view's declared column list (CREATE VIEW v(c0) AS ...);
			// otherwise derive names from the SELECT body.
			if declared := viewDeclaredColumns(v.SQL); len(declared) > 0 {
				return declared, nil
			}
			names, _ := e.viewSelectColumnNames(v)
			var visible []string
			for _, n := range names {
				if !strings.HasPrefix(n, "__hidden__") {
					visible = append(visible, n)
				}
			}
			return visible, nil
		}
		// A missing table referenced by a main-schema view's body is reported
		// with the "main." prefix (SQLite); temp views and direct queries use
		// the bare name.
		if e.expandingView && !e.expandingTempView {
			return nil, fmt.Errorf("no such table: main.%s", tableName)
		}
		return nil, fmt.Errorf("no such table: %s", tableName)
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	names := make([]string, 0, len(colDefs))
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		names = append(names, cd.Name)
	}
	return names, nil
}

// viewSelectColumnNames returns the result column names of a view by parsing
// its stored SELECT body and deriving column names.
func (e *Engine) viewSelectColumnNames(entry *schema.Entry) ([]string, error) {
	sqlStr := entry.SQL
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS")
	if idx < 0 {
		return nil, fmt.Errorf("exec: invalid view SQL: %s", sqlStr)
	}
	selectSQL := strings.TrimSpace(sqlStr[idx+3:])
	stmts, err := parse.ParseSQL(selectSQL)
	if err != nil || len(stmts) == 0 {
		return nil, fmt.Errorf("exec: view parse error: %v", err)
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok {
		// Prefer the declared column list, then typed defs (which expand
		// stars); fall back to derived names.
		if declared := viewDeclaredColumns(entry.SQL); len(declared) > 0 {
			return declared, nil
		}
		if defs := e.viewColumnDefsFromSelect(sel); len(defs) > 0 {
			names := make([]string, len(defs))
			for i, cd := range defs {
				names[i] = cd.Name
			}
			return names, nil
		}
		return e.viewColumnNames(sel), nil
	}
	return nil, fmt.Errorf("exec: view does not contain SELECT")
}

// validateViewBody parses a view's stored SELECT body and runs the same
// compile-time validations a real query would (compound column counts,
// expression checks). It returns the first error found, or nil.
func (e *Engine) validateViewBody(entry *schema.Entry) error {
	sqlStr := entry.SQL
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS")
	if idx < 0 {
		return fmt.Errorf("exec: invalid view SQL: %s", sqlStr)
	}
	selectSQL := strings.TrimSpace(sqlStr[idx+3:])
	stmts, err := parse.ParseSQL(selectSQL)
	if err != nil || len(stmts) == 0 {
		return fmt.Errorf("exec: view parse error: %v", err)
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok {
		if err := e.validateCompoundColumnCounts(sel); err != nil {
			return err
		}
		if err := e.validateSelectExprs(sel); err != nil {
			return err
		}
	}
	return nil
}

// selectOutputCollations returns the collation of each output column of a
// SELECT, based on the column references in the select list and the FROM
// table's declared column collations. SQLite applies the leftmost member's
// column collations to a compound query result, so this is called on the
// head SELECT. Returns nil when collations cannot be determined (BINARY).
func (e *Engine) selectOutputCollations(s *sql.SelectStmt) []string {
	if s == nil || s.From.Name == "" {
		return nil
	}
	entry, _, err := e.findTable(s.From.Name)
	if err != nil {
		return nil
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	colByName := make(map[string]string, len(colDefs))
	for _, cd := range colDefs {
		if cd.Collate != "" && !strings.EqualFold(cd.Collate, "BINARY") {
			colByName[strings.ToLower(cd.Name)] = strings.ToUpper(cd.Collate)
		}
	}
	colls := make([]string, 0, len(s.Columns))
	for _, col := range s.Columns {
		coll := ""
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name != "*" {
			coll = colByName[strings.ToLower(ref.Name)]
		}
		// An explicit COLLATE in the select expression takes precedence over
		// the column's declared collation (e.g. "SELECT DISTINCT b COLLATE
		// nocase FROM t1"). COLLATE is a BinaryOp{Operator:"COLLATE"}.
		if exprColl, _ := exprCollation(col.Expr); exprColl != "" {
			coll = exprColl
		}
		colls = append(colls, coll)
	}
	return colls
}

// orderByTermCollation returns the COLLATE name applied to an ORDER BY term
// expression (e.g. "ORDER BY x COLLATE nocase"), or "" for BINARY.
func orderByTermCollation(e sql.Expr) string {
	c, _ := exprCollation(e)
	return c
}

// exprCollation computes the compile-time collation of an expression node,
// mirroring SQLite's sqlite3ExprCollSeq. It returns the collation name, or ""
// for BINARY/no collation. The second return value reports whether the
// collation is "explicit" (comes from a COLLATE operator, or propagates up
// from an explicit COLLATE through a function call / CASE / ||), which makes
// it win over a column collation on the other side of a comparison.
//
// SQLite propagation rules (expr.c sqlite3ExprCollSeq):
//   - COLLATE operator: its collation (explicit).
//   - Function call: the first argument with a known collation.
//   - CASE: the first THEN branch with a known collation, else ELSE.
//   - || (concat): the right operand's collation if explicit, else the left's.
//   - Column reference: the column's declared collation (not explicit).
func exprCollation(e sql.Expr) (string, bool) {
	switch v := e.(type) {
	case *sql.BinaryOp:
		if strings.EqualFold(v.Operator, "COLLATE") {
			if lit, ok := v.Right.(*sql.StringLit); ok {
				return strings.ToUpper(lit.Value), true
			}
			return "", false
		}
		if strings.EqualFold(v.Operator, "||") {
			if rc, rx := exprCollation(v.Right); rx {
				return rc, true
			}
			if lc, lx := exprCollation(v.Left); lc != "" {
				return lc, lx
			}
			return "", false
		}
		return "", false
	case *sql.FuncCall:
		for _, a := range v.Args {
			if c, x := exprCollation(a); c != "" {
				return c, x
			}
		}
		return "", false
	case *sql.CaseExpr:
		for _, w := range v.Whens {
			if c, x := exprCollation(w.Then); c != "" {
				return c, x
			}
		}
		if v.Else != nil {
			if c, x := exprCollation(v.Else); c != "" {
				return c, x
			}
		}
		return "", false
	case *sql.UnaryOp:
		// UPLUS propagates the operand's collation (SQLite TK_UPLUS).
		return exprCollation(v.Operand)
	case *sql.CastExpr:
		// CAST drops the operand's collation (SQLite TK_CAST returns the
		// operand's collation only when the type has no affinity; Frigolite
		// approximates by not propagating — matches common usage).
		return "", false
	case *sql.ColumnRef:
		// The column's declared collation is resolved at evaluation time by
		// the collatedValue marker; the compile-time name is unknown here
		// (no schema access), so report no collation. Runtime markers handle
		// column collations.
		return "", false
	default:
		return "", false
	}
}

// exprHasExplicitCollate reports whether the expression tree contains an
// explicit COLLATE operator anywhere (SQLite's EP_Collate flag semantics).
// Used to decide whether a propagated collation should win over a column
// collation on the other side of a comparison.
func exprHasExplicitCollate(e sql.Expr) bool {
	switch v := e.(type) {
	case *sql.BinaryOp:
		if strings.EqualFold(v.Operator, "COLLATE") {
			return true
		}
		return exprHasExplicitCollate(v.Left) || exprHasExplicitCollate(v.Right)
	case *sql.FuncCall:
		for _, a := range v.Args {
			if exprHasExplicitCollate(a) {
				return true
			}
		}
		return false
	case *sql.CaseExpr:
		for _, w := range v.Whens {
			if exprHasExplicitCollate(w.When) || exprHasExplicitCollate(w.Then) {
				return true
			}
		}
		if v.Else != nil {
			return exprHasExplicitCollate(v.Else)
		}
		return false
	case *sql.UnaryOp:
		return exprHasExplicitCollate(v.Operand)
	case *sql.CastExpr:
		return exprHasExplicitCollate(v.Operand)
	default:
		return false
	}
}

// stripCollate removes a top-level COLLATE operator from an expression,
// returning the underlying operand (the value to evaluate).
func stripCollate(e sql.Expr) sql.Expr {
	if b, ok := e.(*sql.BinaryOp); ok && strings.EqualFold(b.Operator, "COLLATE") {
		return b.Left
	}
	return e
}

// setOpName returns the SQL keyword for a compound set operator, used in
// error messages about mismatched result column counts.
func setOpName(op sql.SetOp, unionAll bool) string {
	switch op {
	case sql.SetUnion:
		if unionAll {
			return "UNION ALL"
		}
		return "UNION"
	case sql.SetIntersect:
		return "INTERSECT"
	case sql.SetExcept:
		return "EXCEPT"
	default:
		return "UNION"
	}
}

// execValuesGroup evaluates a VALUES-select head together with its internal
// tuple chain (one UNION ALL node per tuple) as a single row set.
func (e *Engine) execValuesGroup(head *sql.SelectStmt) *Result {
	memberCopy := *head
	memberCopy.Union = nil
	res := e.execSelect(&memberCopy)
	if res.Error != nil {
		return res
	}
	cur := head
	for cur.Union != nil && cur.SetOp == sql.SetUnion && cur.UnionAll {
		next := cur.Union
		nextCopy := *next
		nextCopy.Union = nil
		nres := e.execSelect(&nextCopy)
		if nres.Error != nil {
			return nres
		}
		res.Rows = append(res.Rows, nres.Rows...)
		cur = next
	}
	return res
}

// dedupeRows removes duplicate rows using CompareValues-based keys.
// colls holds the collation of each column (nil → BINARY).
func (e *Engine) dedupeRows(rows [][]interface{}, colls []string) [][]interface{} {
	if len(rows) == 0 {
		return rows
	}
	seen := make(map[string]bool)
	var result [][]interface{}
	for _, row := range rows {
		key := rowKey(row, colls)
		if !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
}

// intersectRows returns rows that exist in both a and b (INTERSECT).
// colls holds the collation of each column (nil → BINARY).
func (e *Engine) intersectRows(a, b [][]interface{}, colls []string) [][]interface{} {
	if len(a) == 0 || len(b) == 0 {
		return [][]interface{}{}
	}
	// Build set of b rows
	bSet := make(map[string]bool)
	for _, row := range b {
		bSet[rowKey(row, colls)] = true
	}
	// Find a rows that are also in b
	var result [][]interface{}
	seen := make(map[string]bool)
	for _, row := range a {
		key := rowKey(row, colls)
		if bSet[key] && !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
}

// exceptRows returns rows in a that are not in b (EXCEPT).
// colls holds the collation of each column (nil → BINARY).
func (e *Engine) exceptRows(a, b [][]interface{}, colls []string) [][]interface{} {
	if len(a) == 0 {
		return [][]interface{}{}
	}
	bSet := make(map[string]bool)
	for _, row := range b {
		bSet[rowKey(row, colls)] = true
	}
	var result [][]interface{}
	seen := make(map[string]bool)
	for _, row := range a {
		key := rowKey(row, colls)
		if !bSet[key] && !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
}

// rowKey creates a deduplication key for a row using CompareValues-based
// serialization. This is more robust than fmt.Sprintf because it handles
// type equivalence (int64(1) == float64(1.0) per SQLite affinity).
// colls holds the collation of each column (nil → BINARY); string keys are
// normalized by their column's collation so compound set operators and
// DISTINCT compare with the column's declared collation, matching SQLite.
func rowKey(row []interface{}, colls []string) string {
	parts := make([]string, len(row))
	for i, v := range row {
		if v == nil {
			parts[i] = "\x00"
			continue
		}
		raw, coll := extractValue(v)
		if raw == nil {
			parts[i] = "\x00"
			continue
		}
		if coll == "" && colls != nil && i < len(colls) {
			coll = colls[i]
		}
		switch x := raw.(type) {
		case int64:
			parts[i] = "n:" + strconv.FormatInt(x, 10)
		case float64:
			// Numeric keys unify INTEGER and REAL so 1 and 1.0 deduplicate
			// (SQLite compares them as equal); an integral float formats
			// without a decimal point to match the int64 key of the same
			// value, while a fractional float keeps its distinct form.
			if x == float64(int64(x)) {
				parts[i] = "n:" + strconv.FormatInt(int64(x), 10)
			} else {
				parts[i] = "n:" + strconv.FormatFloat(x, 'g', -1, 64)
			}
		case string:
			parts[i] = "s:" + normalizeForKey(x, coll)
		case []byte:
			parts[i] = "b:" + string(x)
		default:
			parts[i] = "o:" + fmt.Sprintf("%v", util.UnwrapColumnValue(raw))
		}
	}
	return strings.Join(parts, "\x00")
}

// normalizeForKey applies a collation's normalization to a string for use as
// a deduplication/set-operator key.
func normalizeForKey(s, collation string) string {
	switch strings.ToUpper(collation) {
	case "NOCASE":
		return strings.ToUpper(s)
	case "RTRIM":
		return strings.TrimRight(s, " ")
	default:
		return s
	}
}

// viewDeclaredColumns extracts the optional declared column list from a
// stored view SQL string ("CREATE VIEW v(c0, c1) AS SELECT ...").
// Returns nil when the view has no declared column list.
func viewDeclaredColumns(viewSQL string) []string {
	upper := strings.ToUpper(viewSQL)
	viewIdx := strings.Index(upper, "VIEW ")
	if viewIdx < 0 {
		return nil
	}
	after := viewSQL[viewIdx+5:]
	// The view name ends at the next space or '('.
	nameEnd := strings.IndexAny(after, " (")
	if nameEnd < 0 {
		return nil
	}
	rest := after[nameEnd:]
	if !strings.HasPrefix(rest, "(") {
		return nil
	}
	closeIdx := strings.Index(rest, ")")
	if closeIdx < 0 {
		return nil
	}
	inner := rest[1:closeIdx]
	var cols []string
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			cols = append(cols, part)
		}
	}
	if len(cols) == 0 {
		return nil
	}
	return cols
}

// execSelectView executes a SELECT on a view by expanding its stored definition.
func (e *Engine) execSelectView(entry *schema.Entry) *Result {
	// entry.SQL contains "CREATE VIEW name AS SELECT ..."
	sqlStr := entry.SQL
	// Find " AS " after "CREATE VIEW name"
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS")
	if idx < 0 {
		return &Result{Error: fmt.Errorf("exec: invalid view SQL: %s", sqlStr)}
	}
	selectSQL := strings.TrimSpace(sqlStr[idx+3:])
	trimmedUpper := strings.ToUpper(strings.TrimSpace(selectSQL))
	// Allow SELECT or WITH (CTE) as the start of the view body
	if !strings.HasPrefix(trimmedUpper, "SELECT") && !strings.HasPrefix(trimmedUpper, "WITH") && !strings.HasPrefix(trimmedUpper, "VALUES") {
		return &Result{Error: fmt.Errorf("exec: view does not contain SELECT: %s", sqlStr)}
	}
	// Circular references are detected at expansion time by the
	// resolvingViews guard in execSelect (a body reference to the same view
	// re-enters while its name is marked in-use). A name-based static check
	// here would wrongly flag a view that shadows a same-named table in
	// another schema (e.g. "CREATE TEMP VIEW t1 AS SELECT ... FROM t1"
	// where t1 is a main table), so it must not be used as a pre-check.
	stmts, err := parse.ParseSQL(selectSQL)
	if err != nil || len(stmts) == 0 {
		return &Result{Error: fmt.Errorf("exec: view parse error: %v", err)}
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok {
		return e.execSelect(sel)
	}
	return &Result{Error: fmt.Errorf("exec: view does not contain SELECT")}
}

// execSelectViewWithOuter executes a view and applies the outer SELECT's
// column expressions, aggregates, ORDER BY, etc. on the view's result.
func (e *Engine) execSelectViewWithOuter(s *sql.SelectStmt, viewEntry *schema.Entry, viewCtx *DatabaseContext) *Result {
	// Enforce the expression-depth limit for nested views/subqueries
	// (SQLITE_LIMIT_EXPR_DEPTH). Each view expansion counts as one level.
	e.nestDepth++
	defer func() { e.nestDepth-- }()
	if e.nestDepth >= e.exprDepthLimit {
		return &Result{Error: fmt.Errorf("VIEWs and/or subqueries nested too deep")}
	}
	// Pin unqualified name resolution to the view's own schema while the body
	// runs (SQLite sqlite3FixSrcList semantics). Non-temp views cannot see
	// temp objects; temp view bodies use the normal temp-then-main search.
	prevPin := e.schemaPin
	prevTempView := e.expandingTempView
	prevExpandingView := e.expandingView
	e.expandingView = true
	if viewCtx != nil && !viewCtx.IsTemp {
		e.schemaPin = viewCtx
	} else if viewCtx != nil && viewCtx.IsTemp {
		e.expandingTempView = true
	}
	// A view body has its own scope: CTEs defined in the outer query must NOT
	// be visible inside the view (SQLite resolves the view's FROM names
	// against the base objects at CREATE time). Save and clear the outer CTE
	// scopes so a CTE with the same name as a base table cannot shadow it.
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
	// The view body is fully materialized now; clear its circular-reference
	// guard so a JOIN referencing the same view through ANOTHER view (e.g.
	// FROM v1 JOIN (v2 ...) where v2 = SELECT ... FROM v1) does not report
	// v1 as circular. Only a view referencing ITSELF while being expanded is
	// truly circular.
	delete(e.resolvingViews, viewEntry.Name)
	// Build colDefs from view result's column names, preferring the view's
	// declared column list (CREATE VIEW v(c0, c1) AS ...) when present. Use
	// typed defs (viewColumnDefs) so row values carry their affinity for
	// WHERE/ON comparisons.
	var viewColDefs []sql.ColumnDef
	if typed, terr := e.viewColumnDefs(viewEntry); terr == nil && len(typed) > 0 {
		viewColDefs = typed
		if declared := viewDeclaredColumns(viewEntry.SQL); len(declared) > 0 {
			// Override names with the declared column list, keeping types.
			for i, colName := range declared {
				if i < len(viewColDefs) {
					viewColDefs[i].Name = colName
				} else {
					viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
				}
			}
		}
	} else if terr != nil {
		// Column-resolution errors (e.g. declared column count mismatch) are
		// fatal: SQLite reports them when the view is used.
		return &Result{Error: terr}
	} else if declared := viewDeclaredColumns(viewEntry.SQL); len(declared) > 0 {
		for _, colName := range declared {
			viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
		}
	} else {
		for _, colName := range viewResult.Columns {
			viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
		}
	}
	// Build rowMaps from view result rows for expression evaluation, wrapping
	// each value with its column affinity (matching buildRowMap). Also add
	// table-qualified keys (view.c0) so qualified references resolve, matching
	// the derived-table alias handling in the subquery path.
	viewQual := s.From.Name
	if s.From.As != "" {
		viewQual = s.From.As
	}
	var rowMaps []RowMap
	for _, row := range viewResult.Rows {
		rowMap := make(RowMap)
		for i, val := range row {
			if i < len(viewColDefs) {
				cd := viewColDefs[i]
				aff := util.Affinity(cd.Type)
				cv := &util.ColumnValue{Value: val, Affinity: aff}
				var mapped interface{} = cv
				if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
					mapped = &collatedValue{value: cv, collation: strings.ToUpper(coll)}
				}
				rowMap[cd.Name] = mapped
				rowMap[viewQual+"."+cd.Name] = mapped
			}
		}
		// Retain the positional row so SELECT * can output duplicate-named
		// view columns (e.g. three columns aliased '') in order; a name-keyed
		// map cannot distinguish them.
		rowMap[positionalRowKey] = row
		rowMaps = append(rowMaps, rowMap)
	}

	// Handle outer JOINs: the outer query may join the view against other
	// tables/views (e.g. FROM v1 RIGHT JOIN t ON v1.x=t.x).
	if len(s.Joins) > 0 {
		// SQLite rejects unqualified column references that are ambiguous
		// across the joined tables at prepare time.
		if err := e.validateAmbiguousColumnRefs(s); err != nil {
			return &Result{Error: err}
		}
		var err error
		rowMaps, viewColDefs, err = e.execJoins(s, rowMaps, viewColDefs)
		if err != nil {
			return &Result{Error: err}
		}
	}
	// Apply the outer WHERE to the view's rows. This must happen for the
	// no-join case too (FROM view WHERE ...), not just joined views.
	if s.Where != nil {
		filtered := rowMaps[:0]
		for _, rowMap := range rowMaps {
			pass, err := e.rowPassesWhere(s.Where, rowMap, nil)
			if err != nil {
				return &Result{Error: err}
			}
			if pass {
				filtered = append(filtered, rowMap)
			}
		}
		rowMaps = filtered
	}
	// Handle aggregates in outer SELECT
	if aggResult := e.handleSelectAggregates(s, rowMaps, viewColDefs); aggResult != nil {
		return aggResult
	}
	// Build output from outer SELECT expressions (e.g., val/100)
	allRows := make([][]interface{}, len(rowMaps))
	for i, rowMap := range rowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, viewColDefs, rowMap)
	}
	result := &Result{
		Columns: e.buildColumnNames(s.Columns, viewColDefs, s),
		Rows:    allRows,
	}
	// Preserve joined row maps (qualified keys) for derived-table reuse.
	if len(s.Joins) > 0 && selectProjectsPlainColumns(s.Columns) {
		result.rowMaps = rowMaps
	}
	return e.finalizeSelectResult(result, s, rowMaps)
}

// execSelectNoFrom handles SELECT without FROM clause.
func (e *Engine) execSelectNoFrom(s *sql.SelectStmt) *Result {
	columns := e.buildColumnNames(s.Columns, nil, s)

	// A FROM-less SELECT cannot resolve any column reference — SQLite raises
	// "no such column" at prepare time (SELECT false.false → no such column:
	// false.false). Validate the select list and WHERE before evaluating.
	if e.outerRow == nil && len(e.outerRows) == 0 {
		if err := e.validateNoFromColumnRefs(s); err != nil {
			return &Result{Error: err}
		}
	}

	if s.ValuesChain {
		// A VALUES statement exposes its result columns as column1..columnN
		// (SQLite naming). Without real names, materializing the rows into
		// row maps collapses distinct columns onto the same key.
		for i := range columns {
			columns[i] = fmt.Sprintf("column%d", i+1)
		}
	}

	// Validate positional ORDER BY terms against the number of result columns.
	if len(s.OrderBy) > 0 {
		if err := validateOrderBy(s.OrderBy, len(columns)); err != nil {
			return &Result{Error: err}
		}
	}

	// Apply WHERE filter for FROM-less SELECT
	if s.Where != nil {
		// Use nil row since there are no columns to reference
		pass, err := e.rowPassesWhere(s.Where, nil, nil)
		if err != nil {
			return &Result{Error: err}
		}
		if !pass {
			return &Result{Columns: columns, Rows: nil}
		}
	}

	// Check for correlated aggregates with outerRows: if this FROM-less SELECT
	// has aggregates that reference columns, evaluate them over all outer rows.
	var outRow []interface{}
	if len(e.outerRows) > 0 && e.hasAggregates(s.Columns) && e.aggHasColumnRef(s.Columns) {
		outRow = e.evalAggOverOuterRows(s, e.outerRows)
	} else {
		for _, col := range s.Columns {
			// Pass outerRow as the evaluation context so subqueries inside
			// FROM-less SELECTs can resolve correlated column references.
			evalRow := e.outerRow
			v, err := e.evalExpr(col.Expr, evalRow)
			if err != nil {
				return &Result{Error: err}
			}
			outRow = append(outRow, unwrapCollatedValue(v))
		}
	}

	// Handle UNION / INTERSECT / EXCEPT for no-FROM selects — merge the FULL
	// compound chain (a VALUES member's internal tuple chain is one operand).
	if s.Union != nil {
		rows := [][]interface{}{outRow}
		cur := s
		for cur.Union != nil {
			member := cur.Union
			if member.ValuesChain {
				memberResult := e.execValuesGroup(member)
				if memberResult.Error != nil {
					return memberResult
				}
				rows = e.applySetOp(rows, memberResult.Rows, cur.SetOp, cur.UnionAll, nil)
				for member.Union != nil && member.SetOp == sql.SetUnion && member.UnionAll {
					member = member.Union
				}
				cur = member
				continue
			}
			memberCopy := *member
			memberCopy.Union = nil
			memberResult := e.execSelect(&memberCopy)
			if memberResult.Error != nil {
				return memberResult
			}
			rows = e.applySetOp(rows, memberResult.Rows, cur.SetOp, cur.UnionAll, nil)
			cur = member
		}
		result := &Result{Columns: columns, Rows: rows}
		e.finalizeNoFromSelect(result, s)
		return result
	}

	// No UNION: apply ORDER BY and LIMIT/OFFSET
	result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
	// Apply ORDER BY
	if len(s.OrderBy) > 0 {
		rowMaps := e.buildNoFromRowMaps(result.Rows, columns)
		e.sortRowsWithMaps(result, s.OrderBy, rowMaps)
	}
	// Evaluate LIMIT/OFFSET expressions
	limitExpr, offsetExpr := s.Limit, s.Offset
	if s.Limit != nil {
		if v, err := e.evalExpr(s.Limit, nil); err == nil {
			switch n := v.(type) {
			case int64:
				limitExpr = &sql.NumericLit{Value: strconv.FormatInt(n, 10)}
			case float64:
				limitExpr = &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}
			}
		}
	}
	if s.Offset != nil {
		if v, err := e.evalExpr(s.Offset, nil); err == nil {
			switch n := v.(type) {
			case int64:
				offsetExpr = &sql.NumericLit{Value: strconv.FormatInt(n, 10)}
			case float64:
				offsetExpr = &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}
			}
		}
	}
	// Apply LIMIT/OFFSET
	if s.Limit != nil || s.Offset != nil {
		result.Rows = applyLimitOffset(result.Rows, limitExpr, offsetExpr)
	}
	return result
}

// finalizeNoFromSelect applies ORDER BY and LIMIT/OFFSET to a no-FROM SELECT result.
func (e *Engine) finalizeNoFromSelect(result *Result, s *sql.SelectStmt) {
	// A trailing ORDER BY / LIMIT / OFFSET on a compound lives on its LAST
	// member (SQLite attaches it there); hoist it here for no-FROM selects.
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
	if len(orderBy) > 0 {
		// Compound queries restrict ORDER BY terms to result column names or
		// ordinals (SQLite: "Nth ORDER BY term does not match any column in
		// the result set"). The no-FROM path merges compounds too (e.g.
		// VALUES(2) EXCEPT SELECT '' ORDER BY abc), so validate the same way
		// finalizeSelectResult does.
		if err := validateOrderBy(orderBy, len(result.Columns)); err != nil {
			result.Error = err
			return
		}
		if s.Union != nil {
			if err := e.validateCompoundOrderBy(s, orderBy); err != nil {
				result.Error = err
				return
			}
		}
		rowMaps := e.buildNoFromRowMaps(result.Rows, result.Columns)
		e.sortRowsWithMaps(result, orderBy, rowMaps)
	}
	// Apply LIMIT/OFFSET
	limitExpr, offsetExpr := limit, offset
	if limit != nil {
		if v, err := e.evalExpr(limit, nil); err == nil {
			switch n := v.(type) {
			case int64:
				limitExpr = &sql.NumericLit{Value: strconv.FormatInt(n, 10)}
			case float64:
				limitExpr = &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}
			}
		}
	}
	if offset != nil {
		if v, err := e.evalExpr(offset, nil); err == nil {
			switch n := v.(type) {
			case int64:
				offsetExpr = &sql.NumericLit{Value: strconv.FormatInt(n, 10)}
			case float64:
				offsetExpr = &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}
			}
		}
	}
	if limit != nil || offset != nil {
		result.Rows = applyLimitOffset(result.Rows, limitExpr, offsetExpr)
	}
}

// buildNoFromRowMaps creates RowMaps for no-FROM SELECT results (used for ORDER BY).
func (e *Engine) buildNoFromRowMaps(rows [][]interface{}, columns []string) []RowMap {
	rowMaps := make([]RowMap, len(rows))
	for i, row := range rows {
		rm := make(RowMap)
		for j, val := range row {
			if j < len(columns) {
				rm[columns[j]] = val
			}
		}
		rowMaps[i] = rm
	}
	return rowMaps
}

// execSelectFromSubquery executes an outer SELECT whose FROM is a subquery.
func (e *Engine) execSelectFromSubquery(s *sql.SelectStmt) *Result {
	// Execute the subquery
	subqResult := e.execSelect(s.From.Subquery)
	if subqResult.Error != nil {
		return subqResult
	}

	// Build colDefs from subquery column names, carrying the subquery's
	// expression affinities so row-map values wrap correctly (e.g. CAST(...
	// AS REAL) produces a REAL-affinity column; a table column reference
	// inherits the table column's declared type; a function call has no
	// affinity, matching SQLite sqlite3ExprAffinity).
	colDefs := make([]sql.ColumnDef, len(subqResult.Columns))
	subqDefs := e.viewColumnDefsFromSelect(s.From.Subquery)
	subqAff := subqueryColumnAffinities(s.From.Subquery)
	for i, col := range subqResult.Columns {
		colDefs[i] = sql.ColumnDef{Name: col}
		if i < len(subqDefs) {
			colDefs[i].Type = subqDefs[i].Type
			colDefs[i].Collate = subqDefs[i].Collate
		} else if i < len(subqAff) {
			if subqAff[i] != 0 {
				colDefs[i].Type = affinityTypeName(subqAff[i])
			} else {
				// No affinity (expression result such as a function call):
				// carry the NONE sentinel so row values wrap with affinity 0
				// (SQLite sqlite3ExprAffinity returns NONE for functions).
				colDefs[i].Type = util.AffinityNone
			}
		}
	}

	return e.execSelectOverMaterialized(s, colDefs, subqResult.Rows)
}

// affinityTypeName returns a type name whose util.Affinity equals aff, used to
// carry a computed expression affinity through column definitions.
func affinityTypeName(aff rune) string {
	switch aff {
	case 'T':
		return "TEXT"
	case 'I':
		return "INTEGER"
	case 'R':
		return "REAL"
	case 'N':
		return "NUMERIC"
	default:
		return "BLOB"
	}
}

// execSelectOverMaterialized runs the outer SELECT pipeline (WHERE, JOINs,
// aggregates, projection, DISTINCT, ORDER BY, LIMIT/OFFSET, UNION) over a
// materialized set of rows with known column definitions. It is shared by
// subquery-in-FROM execution and table-valued pragma functions.
func (e *Engine) execSelectOverMaterialized(s *sql.SelectStmt, colDefs []sql.ColumnDef, rows [][]interface{}) *Result {
	// Build rowMaps from result rows
	allRows := rows
	if len(allRows) == 0 {
		// An aggregate query over an empty input still yields one row (e.g.
		// SELECT avg(b) FROM (empty) → NULL). Let the aggregate handler
		// produce that row; only short-circuit when there are no aggregates.
		if !e.hasAggregates(s.Columns) && len(s.GroupBy) == 0 {
			return &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: [][]interface{}{}}
		}
	}
	allRowMaps := make([]RowMap, len(allRows))
	subqAff := subqueryColumnAffinities(s)
	// A derived-table alias (FROM (SELECT ...) AS d) makes qualified
	// references like d.a resolvable; add the qualified keys alongside the
	// unqualified ones. Table-valued pragma functions and virtual-table
	// functions with aliases (FROM pragma_table_list() AS t) get the same
	// qualified keys.
	alias := ""
	if s.From.Subquery != nil && s.From.As != "" {
		alias = s.From.As
	}
	if alias == "" && isPragmaTableFunc(s.From.Name) && s.From.As != "" {
		alias = s.From.As
	}
	if alias == "" && s.From.As != "" && len(s.From.Args) > 0 {
		alias = s.From.As
	}
	for i, row := range allRows {
		rowMap := make(RowMap)
		for j, val := range row {
			if j < len(colDefs) {
				aff := 'B'
				if j < len(subqAff) && subqAff[j] != 0 {
					aff = subqAff[j]
				} else {
					aff = util.Affinity(colDefs[j].Type)
				}
				cv := &util.ColumnValue{Value: val, Affinity: aff}
				var mapped interface{} = cv
				// Carry the derived column's declared collation (e.g. a
				// subquery column that inherits a COLLATE NOCASE base column)
				// so outer comparisons use it (SQLite column collation
				// rules). BINARY needs no marker.
				if coll := colDefs[j].Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
					mapped = &collatedValue{value: cv, collation: strings.ToUpper(coll)}
				}
				rowMap[colDefs[j].Name] = mapped
				if alias != "" {
					rowMap[alias+"."+colDefs[j].Name] = mapped
				}
			}
		}
		// Materialized tables (subquery-in-FROM, pragma table-valued
		// functions) have implicit rowids 1..N, like SQLite. When the
		// materialized table itself declares a column named "rowid" (e.g.
		// pragma_foreign_key_check's child-rowid column), that column wins
		// and no implicit rowid is added.
		if _, hasRowIDCol := rowMap["rowid"]; !hasRowIDCol {
			rowMap["rowid"] = int64(i + 1)
			if alias != "" {
				rowMap[alias+".rowid"] = int64(i + 1)
			}
		}
		allRowMaps[i] = rowMap
	}

	// Apply WHERE filter. When the query has JOINs, the WHERE may reference
	// joined tables (e.g. t2.a=t3.a), so it cannot be applied before the
	// join — the post-join filter below handles it.
	if len(s.Joins) == 0 {
		var err error
		_, allRowMaps, err = e.filterSubqueryRows(allRows, allRowMaps, s.Where)
		if err != nil {
			return &Result{Error: err}
		}
	}

	// Handle outer JOINs: the outer query may join the subquery against other
	// tables/views (e.g. FROM (SELECT ...) v RIGHT JOIN t ON v.x=t.x).
	if len(s.Joins) > 0 {
		// SQLite rejects unqualified column references that are ambiguous
		// across the joined tables at prepare time.
		if err := e.validateAmbiguousColumnRefs(s); err != nil {
			return &Result{Error: err}
		}
		var err error
		allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
		if err != nil {
			return &Result{Error: err}
		}
		if s.Where != nil {
			filtered := allRowMaps[:0]
			for _, rowMap := range allRowMaps {
				pass, err := e.rowPassesWhere(s.Where, rowMap, nil)
				if err != nil {
					return &Result{Error: err}
				}
				if pass {
					filtered = append(filtered, rowMap)
				}
			}
			allRowMaps = filtered
		}
	}

	// Handle aggregate functions
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}

	// Build output rows by evaluating outer SELECT expressions against each row map
	allRows = make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}

	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: allRows}

	// Apply DISTINCT
	if s.Distinct {
		result.Rows, allRowMaps = e.distinctRows(result.Rows, allRowMaps, e.selectOutputCollations(s), s)
	}

	// Apply ORDER BY
	if len(s.OrderBy) > 0 {
		if err := validateOrderBy(s.OrderBy, len(result.Columns)); err != nil {
			return &Result{Error: err}
		}
		e.sortRowsWithMaps(result, s.OrderBy, allRowMaps)
	}

	// Apply LIMIT / OFFSET
	result.Rows = applyLimitOffset(result.Rows, e.evalLimitExpr(s.Limit), e.evalLimitExpr(s.Offset))

	// Handle UNION / INTERSECT / EXCEPT
	if s.Union != nil {
		result.Rows = e.mergeUnionRows(result.Rows, s.Union, s.SetOp, s.UnionAll)
	}

	return result
}

// execSelectCTE executes a query that references a CTE definition.
func (e *Engine) execSelectCTE(s *sql.SelectStmt, cte *sql.CTEDef) *Result {
	// Detect circular CTE references (e.g. WITH a AS (SELECT * FROM b),
	// b AS (SELECT * FROM a)). SQLite reports "circular reference: NAME".
	if e.resolvingCTEs == nil {
		e.resolvingCTEs = make(map[string]bool)
	}
	if e.resolvingCTEs[cte.Name] {
		return &Result{Error: fmt.Errorf("circular reference: %s", cte.Name)}
	}
	e.resolvingCTEs[cte.Name] = true
	defer delete(e.resolvingCTEs, cte.Name)

	// Handle recursive CTE. A CTE is recursive when declared WITH RECURSIVE
	// or when its body references the CTE name in a FROM position (SQLite
	// accepts self-referencing CTEs regardless of the keyword, e.g.
	// "WITH s(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM s WHERE i<10)").
	if cte.Select != nil && (cte.Recursive || cteBodyReferencesSelf(cte)) {
		// SQLite requires the recursive (self-referencing) term to be the
		// LAST term of the compound chain. If the body self-references but
		// the final term does not, the self-reference is not a valid
		// recursion and is reported as a circular reference (e.g. "WITH
		// s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<3 UNION ALL
		// SELECT 4)"). Multiple self-referencing terms are allowed as long
		// as the last one references the CTE.
		if !recursiveSelfRefIsLast(cte) {
			return &Result{Error: fmt.Errorf("circular reference: %s", cte.Name)}
		}
		return e.execRecursiveCTE(s, cte)
	}
	// Non-recursive CTE: execute the CTE's SELECT directly
	cteResult := e.execSelect(cte.Select)
	if cteResult.Error != nil {
		return cteResult
	}
	// SQLite validates a CTE's declared column count against its body's
	// output width at prepare time ("table i has N values for M columns").
	if len(cte.Columns) > 0 && len(cteResult.Columns) != len(cte.Columns) {
		return &Result{Error: fmt.Errorf("table %s has %d values for %d columns", cte.Name, len(cteResult.Columns), len(cte.Columns))}
	}
	colDefs := make([]sql.ColumnDef, len(cteResult.Columns))
	for i, colName := range cteResult.Columns {
		colDefs[i] = sql.ColumnDef{Name: colName}
		// Carry the CTE body's output column affinity so values read through
		// the CTE keep the compound column's affinity (SQLite's
		// selectAddColumnTypeAndCollation: the CTE column's type comes from
		// its SELECT body, which for a compound is the merged member type).
		if aff := e.compoundColumnAffinity(cte.Select, i); aff != 0 {
			colDefs[i].Type = affinityTypeName(aff)
		}
	}
	if len(cte.Columns) > 0 {
		for i := 0; i < len(colDefs) && i < len(cte.Columns); i++ {
			colDefs[i].Name = cte.Columns[i]
		}
	}
	allRowMaps := make([]RowMap, len(cteResult.Rows))
	for i, row := range cteResult.Rows {
		allRowMaps[i] = buildRowMapFromValues(row, colDefs, int64(i+1))
	}

	// If the outer query joins the CTE with other tables (or references the
	// CTE twice, e.g. "FROM x one, x two"), process the joins here. The join
	// path resolves right-side CTE references via findCTE.
	if len(s.Joins) > 0 {
		var err error
		allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
		if err != nil {
			return &Result{Error: err}
		}
	}
	// Apply the outer query's WHERE to the CTE rows (both the no-join and
	// joined cases; a CTE reference with a WHERE but no JOIN must still be
	// filtered, e.g. "SELECT x+1 FROM c1 WHERE x<1").
	if s.Where != nil {
		filtered := allRowMaps[:0]
		for _, rowMap := range allRowMaps {
			pass, err := e.rowPassesWhere(s.Where, rowMap, nil)
			if err != nil {
				return &Result{Error: err}
			}
			if pass {
				filtered = append(filtered, rowMap)
			}
		}
		allRowMaps = filtered
	}

	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	allRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: allRows}
	// The CTE is fully materialized; clear the resolving guard so the outer
	// query's compound merge (which may reference the CTE again in a later
	// term) can re-read the CTE without reporting a circular reference.
	delete(e.resolvingCTEs, cte.Name)
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// cteBodyReferencesSelf reports whether a CTE body references the CTE name in
// any FROM position (the base FROM, join tables, union members, or nested
// subqueries). A body that does is recursive even when the WITH clause omits
// the RECURSIVE keyword — SQLite accepts self-referencing CTEs regardless.
func cteBodyReferencesSelf(cte *sql.CTEDef) bool {
	if cte == nil || cte.Select == nil {
		return false
	}
	return selectFromRefersTo(cte.Select, cte.Name)
}

// selectFromRefersTo walks a SELECT's FROM positions (base table, joins,
// union members, nested subqueries) looking for a reference to name.
func selectFromRefersTo(s *sql.SelectStmt, name string) bool {
	if s == nil {
		return false
	}
	if s.From.Name == name || s.From.As == name {
		return true
	}
	if s.From.Subquery != nil && selectFromRefersTo(s.From.Subquery, name) {
		return true
	}
	for i := range s.Joins {
		j := &s.Joins[i]
		if j.Table.Name == name || j.Table.As == name {
			return true
		}
		if j.Table.Subquery != nil && selectFromRefersTo(j.Table.Subquery, name) {
			return true
		}
	}
	return s.Union != nil && selectFromRefersTo(s.Union, name)
}

// recursiveSelfRefIsLast reports whether a self-referencing CTE body has its
// recursive (self-referencing) term as the LAST term of the compound chain.
// SQLite requires the recursive term to be final; a self-reference anywhere
// else is an error ("circular reference: NAME"). Multiple self-referencing
// terms are fine as long as the last one references the CTE.
func recursiveSelfRefIsLast(cte *sql.CTEDef) bool {
	if cte == nil || cte.Select == nil {
		return true
	}
	// Find the last term of the chain.
	last := cte.Select
	for last.Union != nil {
		last = last.Union
	}
	// The last term must reference the CTE.
	return termFromRefersTo(last, cte.Name)
}

// termFromRefersTo reports whether a single compound term's FROM sources (base
// table, joins, nested subqueries — but NOT its Union chain) reference name.
func termFromRefersTo(s *sql.SelectStmt, name string) bool {
	if s == nil {
		return false
	}
	if s.From.Name == name || s.From.As == name {
		return true
	}
	if s.From.Subquery != nil && selectFromRefersTo(s.From.Subquery, name) {
		return true
	}
	for i := range s.Joins {
		j := &s.Joins[i]
		if j.Table.Name == name || j.Table.As == name {
			return true
		}
		if j.Table.Subquery != nil && selectFromRefersTo(j.Table.Subquery, name) {
			return true
		}
	}
	return false
}

// findCTE returns the CTE definition whose name matches the given table
// reference. It first checks the CTEs declared directly on the statement,
// then consults enclosing WITH clauses (innermost scope first), matching
// SQLite's name-resolution order for nested queries.
func (e *Engine) findCTE(s *sql.SelectStmt, name string) (sql.CTEDef, bool) {
	if s == nil {
		return sql.CTEDef{}, false
	}
	for _, cte := range s.CTEs {
		if cte.Name == name {
			return cte, true
		}
	}
	for i := len(e.cteScopes) - 1; i >= 0; i-- {
		for _, cte := range e.cteScopes[i] {
			if cte.Name == name {
				return cte, true
			}
		}
	}
	return sql.CTEDef{}, false
}

// materializeCTEForJoin executes a CTE body and builds column definitions and
// row maps suitable for use as a join operand.
func (e *Engine) materializeCTEForJoin(cte *sql.CTEDef) ([]sql.ColumnDef, []RowMap, error) {
	cteResult := e.execSelect(cte.Select)
	if cteResult.Error != nil {
		return nil, nil, cteResult.Error
	}
	colDefs := make([]sql.ColumnDef, len(cteResult.Columns))
	for i, colName := range cteResult.Columns {
		colDefs[i] = sql.ColumnDef{Name: colName}
	}
	if len(cte.Columns) > 0 {
		for i := 0; i < len(colDefs) && i < len(cte.Columns); i++ {
			colDefs[i].Name = cte.Columns[i]
		}
	}
	rowMaps := make([]RowMap, len(cteResult.Rows))
	for i, row := range cteResult.Rows {
		rowMaps[i] = buildRowMapFromValues(row, colDefs, int64(i+1))
	}
	return colDefs, rowMaps, nil
}

// execRecursiveCTE executes a recursive CTE (WITH RECURSIVE ...).
// The CTE definition is a compound SELECT: the first term is the anchor
// (seed) and every following term is a recursive term (SQLite allows several
// recursive terms; each is evaluated per iteration against the rows produced
// in the previous iteration).
// cteBodyColumnCount returns the output column width of a CTE body, verifying
// that all compound members agree on the width (SQLite's compound arity check:
// "SELECTs to the left and right of OP do not have the same number of result
// columns"). The width is derived statically from the anchor member's select
// list (executing the body would re-enter the CTE and report a false circular
// reference).
func (e *Engine) cteBodyColumnCount(cte *sql.CTEDef) (int, error) {
	if cte == nil || cte.Select == nil {
		return 0, nil
	}
	// Validate compound member widths first (matches execSelect's
	// validateCompoundColumnCounts, which a CTE body may not reach directly).
	if err := e.validateCompoundColumnCounts(cte.Select); err != nil {
		return 0, err
	}
	return e.cteAnchorColumnCount(cte.Select)
}

// cteAnchorColumnCount counts the anchor (first) member's output columns
// without executing it: a plain expression counts 1, a star expands via the
// FROM table's columns (or the CTE's own declared columns when self-joined).
func (e *Engine) cteAnchorColumnCount(sel *sql.SelectStmt) (int, error) {
	if sel == nil {
		return 0, nil
	}
	count := 0
	for _, col := range sel.Columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			if ref.Table != "" {
				cols, err := e.tableColumnNames(ref.Table)
				if err != nil {
					return 0, err
				}
				count += len(cols)
			} else if sel.From.Name != "" {
				cols, err := e.resolveTableColumnNames(sel, sel.From.Name)
				if err != nil {
					return 0, err
				}
				count += len(cols)
			}
		} else {
			count++
		}
	}
	return count, nil
}

func (e *Engine) execRecursiveCTE(s *sql.SelectStmt, cte *sql.CTEDef) *Result {
	// SQLite validates a recursive CTE's declared column count against its
	// body width at prepare time, and requires all compound members to agree
	// on the width:
	//   WITH i(x) AS (VALUES(1,2))           -> "table i has 2 values for 1 columns"
	//   WITH i(x) AS (SELECT 1 UNION ALL SELECT 1,2) -> compound arity error
	if bodyCols, err := e.cteBodyColumnCount(cte); err != nil {
		return &Result{Error: err}
	} else if len(cte.Columns) > 0 && bodyCols != len(cte.Columns) {
		return &Result{Error: fmt.Errorf("table %s has %d values for %d columns", cte.Name, bodyCols, len(cte.Columns))}
	}

	// Build column definitions from CTE column names
	colDefs := make([]sql.ColumnDef, len(cte.Columns))
	for i, name := range cte.Columns {
		colDefs[i] = sql.ColumnDef{Name: name}
	}
	if len(colDefs) == 0 {
		colDefs = []sql.ColumnDef{{Name: "x"}}
	}

	// Execute the anchor part (the first term before UNION)
	anchorSelect := *cte.Select
	anchorSelect.Union = nil
	anchorResult := e.execSelect(&anchorSelect)
	if anchorResult.Error != nil {
		return anchorResult
	}

	// Collect all rows (anchor + recursive iterations)
	allRows := append([][]interface{}{}, anchorResult.Rows...)

	// Recursive terms: the chain after the anchor. SQLite evaluates each
	// term per iteration against the rows from the previous iteration.
	var recursiveTerms []*sql.SelectStmt
	for term := cte.Select.Union; term != nil; term = term.Union {
		recursiveTerms = append(recursiveTerms, term)
	}

	currentRows := anchorResult.Rows
	maxIter := e.recursiveCTELimit
	if maxIter <= 0 {
		maxIter = 100000 // SQLite test builds default
	}
	for iter := 0; iter < maxIter; iter++ {
		if err := e.checkProgress(); err != nil {
			return &Result{Error: err}
		}
		var newRows [][]interface{}
		for _, row := range currentRows {
			rowMap := buildRowMapFromValues(row, colDefs, int64(len(allRows)+1))
			for _, term := range recursiveTerms {
				// Evaluate WHERE clause if present
				if term.Where != nil {
					pass, err := e.rowPassesWhere(term.Where, rowMap, nil)
					if err != nil {
						return &Result{Error: err}
					}
					if !pass {
						continue
					}
				}

				// Evaluate column expressions
				outRow := make([]interface{}, len(term.Columns))
				for i, col := range term.Columns {
					val, err := e.evalExpr(col.Expr, rowMap)
					if err != nil {
						return &Result{Error: err}
					}
					outRow[i] = unwrapCollatedValue(val)
				}
				newRows = append(newRows, outRow)
			}
		}
		if len(newRows) == 0 {
			break
		}
		allRows = append(allRows, newRows...)
		currentRows = newRows
	}

	// Build row maps for ordering/aggregation
	allRowMaps := make([]RowMap, len(allRows))
	for i, row := range allRows {
		allRowMaps[i] = buildRowMapFromValues(row, colDefs, int64(i+1))
	}
	// The CTE is fully materialized; clear the resolving guard so join
	// operands (or later compound terms) can re-read the CTE without
	// reporting a circular reference.
	delete(e.resolvingCTEs, cte.Name)
	// Process outer JOINs (e.g. "FROM c1 AS x1, (subquery) AS x2") against
	// the recursive CTE rows, same as the non-recursive CTE path.
	if len(s.Joins) > 0 {
		var err error
		allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
		if err != nil {
			return &Result{Error: err}
		}
	}
	// Apply the outer query's WHERE to the recursive CTE rows (a CTE
	// reference with a WHERE, e.g. "SELECT x+1 FROM c1 WHERE x<1", must be
	// filtered even without a JOIN).
	if s.Where != nil {
		filtered := allRowMaps[:0]
		for _, rowMap := range allRowMaps {
			pass, err := e.rowPassesWhere(s.Where, rowMap, nil)
			if err != nil {
				return &Result{Error: err}
			}
			if pass {
				filtered = append(filtered, rowMap)
			}
		}
		allRowMaps = filtered
	}
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}

	// Build output rows
	outRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		outRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: outRows}
	// The CTE is fully materialized; clear the resolving guard so the outer
	// query's compound merge (which may reference the CTE again in a later
	// term, e.g. "SELECT x+1 FROM c1 ... UNION ALL SELECT 1+x FROM c1") can
	// re-read the CTE without reporting a circular reference.
	delete(e.resolvingCTEs, cte.Name)
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// the base table rows and each joined table. Returns combined rowMaps and
// colDefs.

// filterSubqueryRows applies a WHERE expression to filter rows from a subquery result.
func (e *Engine) filterSubqueryRows(allRows [][]interface{}, allRowMaps []RowMap, where sql.Expr) ([][]interface{}, []RowMap, error) {
	if where == nil {
		return allRows, allRowMaps, nil
	}
	var filteredRows [][]interface{}
	var filteredMaps []RowMap
	for i, rowMap := range allRowMaps {
		pass, err := e.rowPassesWhere(where, rowMap, nil)
		if err != nil {
			return nil, nil, err
		}
		if pass {
			filteredRows = append(filteredRows, allRows[i])
			filteredMaps = append(filteredMaps, rowMap)
		}
	}
	return filteredRows, filteredMaps, nil
}

// isNaturalJoinType reports whether a join type string includes NATURAL
// (e.g. "NATURAL", "NATURAL LEFT", "NATURAL RIGHT").
func isNaturalJoinType(joinType string) bool {
	return strings.Contains(joinType, "NATURAL")
}

// joinTypeHas reports whether a join type string includes the given type
// keyword (e.g. "NATURAL LEFT" has LEFT).
func joinTypeHas(joinType, typ string) bool {
	return strings.Contains(joinType, typ)
}

func (e *Engine) execJoins(s *sql.SelectStmt, baseMaps []RowMap, baseDefs []sql.ColumnDef) ([]RowMap, []sql.ColumnDef, error) {
	if err := e.validateJoinOnClauses(s); err != nil {
		return nil, nil, err
	}
	plainNames := map[string]bool{}
	// SQLite allows output column aliases (e.g., "SELECT (+a)b ... ON z=b")
	// to be referenced in ON clauses. Collect them so validation passes.
	for _, col := range s.Columns {
		if col.As != "" {
			plainNames[col.As] = true
		}
	}
	for _, d := range baseDefs {
		plainNames[d.Name] = true
	}
	// The left table's qualified-name prefix in combined row maps must use
	// the table's alias when present (e.g. "c.id" for "FROM customer c"),
	// so JOIN ON conditions referencing the alias resolve correctly.
	leftName := s.From.Name
	if s.From.As != "" {
		leftName = s.From.As
	}
	currentMaps := baseMaps
	currentDefs := baseDefs
	lastTableName := leftName // immediate left table of the next join

	for _, join := range s.Joins {
		var rightMaps []RowMap
		var rightDefs []sql.ColumnDef
		var corrLeftIdx []int
		var tableName string

		// Handle derived table (subquery) in JOIN: JOIN (SELECT ...) AS t
		if join.Table.Subquery != nil {
			// A derived table cannot reference tables outside its own FROM
			// (no correlation). Validate its refs resolve within its scope.
			if bad := derivedTableBadColumnRef(join.Table.Subquery); bad != "" {
				return nil, nil, fmt.Errorf("no such column: %s", bad)
			}
			subqResult := e.execSelect(join.Table.Subquery)
			if subqResult.Error != nil {
				return nil, nil, subqResult.Error
			}
			// Build column defs from subquery result columns
			for _, colName := range subqResult.Columns {
				rightDefs = append(rightDefs, sql.ColumnDef{Name: colName})
			}
			tableName = join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}
			// An unaliased derived table (JOIN (SELECT ...) USING(id)) has no
			// name for qualified column keys. Give it a synthetic name so its
			// columns are stored under distinct keys and the USING ON clause
			// (id = synth.id) can match the two sides.
			synthetic := tableName == ""
			if synthetic {
				e.subqSeq++
				tableName = fmt.Sprintf("_subq%d", e.subqSeq)
			}
			// Build row maps from subquery result rows. When the subquery itself
			// joined tables (a parenthesized group), reuse its qualified row
			// maps so outer references like t4.a resolve; otherwise build
			// plain maps from the projected rows, wrapping each value with the
			// column affinity (matching buildRowMap) so ON/WHERE comparisons
			// apply SQLite affinity rules.
			if len(subqResult.rowMaps) > 0 && len(subqResult.rowMaps) == len(subqResult.Rows) {
				rightMaps = subqResult.rowMaps
			} else {
				subqAff := subqueryColumnAffinities(join.Table.Subquery)
				for _, row := range subqResult.Rows {
					rightRowMap := make(RowMap)
					for i, val := range row {
						if i < len(rightDefs) {
							cd := rightDefs[i]
							aff := 'B'
							if i < len(subqAff) && subqAff[i] != 0 {
								aff = subqAff[i]
							} else {
								aff = util.Affinity(cd.Type)
							}
							cv := &util.ColumnValue{Value: val, Affinity: aff}
							rightRowMap[rightDefs[i].Name] = cv
							if synthetic {
								// Also store under the synthetic qualified key so the
								// USING ON clause (id = _subq.id) can match the right
								// side independently of the merged plain column.
								rightRowMap[tableName+"."+rightDefs[i].Name] = val
							}
						}
					}
					rightMaps = append(rightMaps, rightRowMap)
				}
			}
		} else if isPragmaTableFunc(join.Table.Name) {
			// Table-valued pragma function in a JOIN: pragma_table_info('t1') AS t
			// When an argument references an outer (left-side) column, the pragma
			// is materialized per left row with that row as evaluation context
			// (SQLite correlation, e.g. FROM sqlite_schema,
			// pragma_foreign_key_check(name)).
			if pragmaArgsCorrelated(join.Table) {
				var merr error
				rightDefs, rightMaps, corrLeftIdx, merr = e.materializeCorrelatedPragma(join.Table, currentMaps)
				if merr != nil {
					return nil, nil, merr
				}
				tableName = join.Table.Name
				if join.Table.As != "" {
					tableName = join.Table.As
				}
			} else {
				defs, rows, err := e.materializePragmaTable(join.Table)
				if err != nil {
					return nil, nil, err
				}
				rightDefs = defs
				tableName = join.Table.Name
				if join.Table.As != "" {
					tableName = join.Table.As
				}
				for _, row := range rows {
					rightRowMap := make(RowMap)
					for i, val := range row {
						if i < len(rightDefs) {
							rightRowMap[rightDefs[i].Name] = val
						}
					}
					rightMaps = append(rightMaps, rightRowMap)
				}
			}
		} else if cteDef, ok := e.findCTE(s, join.Table.Name); ok {
			// A CTE referenced in a JOIN (e.g. "FROM t LEFT JOIN VVV" where
			// VVV is a WITH definition). Materialize the CTE body once.
			var merr error
			rightDefs, rightMaps, merr = e.materializeCTEForJoin(&cteDef)
			if merr != nil {
				return nil, nil, merr
			}
			tableName = join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}
		} else if tableEntry, _, tableErr := e.findTable(join.Table.Name); tableErr != nil {
			viewEntry, _, viewErr := e.findView(join.Table.Name)
			if viewErr != nil {
				return nil, nil, tableErr
			}
			// Execute the view to get its columns and rows
			viewResult := e.execSelectView(viewEntry)
			if viewResult.Error != nil {
				return nil, nil, viewResult.Error
			}
			// Build column defs from the view's declared column list when
			// present, otherwise from the view result column names. Use typed
			// defs (viewColumnDefs) so row values carry their affinity; when the
			// view declares explicit column names (CREATE VIEW v(c0) AS ...)
			// those names override the expression-derived names.
			viewDefs, vdErr := e.viewColumnDefs(viewEntry)
			if vdErr != nil {
				return nil, nil, vdErr
			}
			declared := viewDeclaredColumns(viewEntry.SQL)
			if len(declared) > 0 {
				// Rebuild with declared names, keeping the computed types.
				named := make([]sql.ColumnDef, 0, len(declared))
				for i, colName := range declared {
					cd := sql.ColumnDef{Name: colName}
					if i < len(viewDefs) {
						cd.Type = viewDefs[i].Type
						cd.Collate = viewDefs[i].Collate
					}
					named = append(named, cd)
				}
				rightDefs = named
			} else if len(viewDefs) > 0 {
				rightDefs = viewDefs
			} else {
				for _, colName := range viewResult.Columns {
					rightDefs = append(rightDefs, sql.ColumnDef{Name: colName})
				}
			}
			tableName = join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}
			// Build row maps from view result rows, wrapping each value with
			// its column affinity (matching buildRowMap for real tables) so
			// ON/WHERE comparisons apply SQLite affinity rules. Also add
			// table-qualified keys (view.c0) so qualified references resolve.
			for _, row := range viewResult.Rows {
				rightRowMap := make(RowMap)
				for i, val := range row {
					if i < len(rightDefs) {
						cd := rightDefs[i]
						aff := util.Affinity(cd.Type)
						cv := &util.ColumnValue{Value: val, Affinity: aff}
						var mapped interface{} = cv
						if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
							mapped = &collatedValue{value: cv, collation: strings.ToUpper(coll)}
						}
						rightRowMap[cd.Name] = mapped
						rightRowMap[tableName+"."+cd.Name] = mapped
					}
				}
				rightMaps = append(rightMaps, rightRowMap)
			}
		} else {
			rightDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
			tableName = join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}

			if tableEntry.RootPage == 0 {
				// Virtual table on the right side of a join: materialize its
				// rows through the virtual-table machinery (echo proxies its
				// source, wholenumber/generate_series generate values).
				rows, err := e.virtualTableRows(tableEntry, 0)
				if err != nil {
					return nil, nil, err
				}
				for _, row := range rows {
					rightRowMap := make(RowMap)
					for i, val := range row {
						if i < len(rightDefs) {
							cd := rightDefs[i]
							aff := util.Affinity(cd.Type)
							cv := &util.ColumnValue{Value: val, Affinity: aff}
							rightRowMap[cd.Name] = cv
							rightRowMap[tableName+"."+cd.Name] = cv
						}
					}
					rightMaps = append(rightMaps, rightRowMap)
				}
				goto joinDone
			}

			// Scan all rows from the right table. Use the qualified name (e.g.
			// "aux1.t4") so tablePager resolves the attached database's pager
			// rather than falling back to the main pager via the short name.
			tree := e.tableBTreeForName(join.Table.Name, tableEntry.RootPage, true)
			cursor, err := tree.OpenCursor()
			if err != nil {
				return nil, nil, err
			}
			for {
				cell, err := cursor.ReadCell()
				if err != nil {
					break
				}
				rec, err := storage.DecodeRecord(cell.Payload)
				if err != nil {
					break
				}
				rightRowMap := e.buildRowMap(rec, rightDefs, cell.RowID)
				rightMaps = append(rightMaps, rightRowMap)
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
			}
		}
	joinDone:

		// NATURAL JOIN: auto-generate USING conditions for all common columns.
		effectiveOn := join.On
		var naturalCols map[string]bool
		if isNaturalJoinType(join.JoinType) {
			naturalCols = naturalJoinCommonCols(currentDefs, rightDefs)
			effectiveOn = e.generateNaturalJoinOn(currentDefs, rightDefs, leftName, tableName)
		}
		// USING(col1, col2): generate ON left.col = right.col for each column.
		// The LEFT side is the table most recently joined (not necessarily the
		// FROM clause's first table), so the ON compares the immediate left
		// table with the current right table.
		if len(join.Using) > 0 && effectiveOn == nil {
			effectiveOn = e.generateUsingJoinOn(join.Using, lastTableName, tableName)
		}

		// SQLite forbids an ON clause from referencing tables to its right;
		// unqualified column references must resolve among the joined tables.
		for _, d := range rightDefs {
			plainNames[d.Name] = true
		}
		if joinTypeHas(join.JoinType, "LEFT") || joinTypeHas(join.JoinType, "RIGHT") {
			if err := validateOnColumnRefs(effectiveOn, plainNames); err != nil {
				return nil, nil, err
			}
		}

		// Build ephemeral hash index for equi-join optimization.
		// Detect simple "left.col = right.col" patterns in the ON clause
		// and create a temporary index on the right table's column.
		// lastTableName is the immediate left table of THIS join (for a
		// chained join it is the previous join's right table, not the
		// first FROM table).
		var autoIndex map[interface{}][]joinIndexEntry
		_, rightColName := extractEquiJoinCols(effectiveOn, lastTableName, tableName)
		// Only build the autoindex when the extracted right column actually
		// exists in the right operand's defs. extractEquiJoinCols falls back
		// to assuming unqualified x=y means x-left/y-right, but both may be
		// LEFT columns (SELECT * FROM t3 JOIN t2 ON x=y where t2 has no y);
		// an empty index would wrongly short-circuit the join to zero rows.
		if rightColName != "" && len(rightMaps) > 0 && rightDefHasColumn(rightDefs, rightColName) {
			autoIndex = make(map[interface{}][]joinIndexEntry)
			for ri, rm := range rightMaps {
				if val, ok := rm[rightColName]; ok {
					// Unwrap ColumnValue (and collatedValue) wrappers so the map
					// key compares by value, not by pointer identity. Normalize
					// numeric text (e.g. '0' vs 0) so affinity-aware equality
					// matches.
					key := joinIndexKey(val)
					autoIndex[key] = append(autoIndex[key], joinIndexEntry{row: rm, idx: ri})
				}
			}
		}
		// Track autoindex usage for EXPLAIN QUERY PLAN output
		e.usingAutoIndex = autoIndex != nil

		// Pre-filter the right table when the ON references ONLY right-table
		// columns (e.g. LEFT JOIN t2 ON t2.x>0): the condition is independent
		// of the left row, so filter rightMaps once instead of per left row.
		// RIGHT/FULL joins keep the original right rows for unmatched tracking.
		if effectiveOn != nil && !joinTypeHas(join.JoinType, "RIGHT") && !joinTypeHas(join.JoinType, "FULL") &&
			joinONReferencesOnlyRight(effectiveOn, tableName) {
			var filtered []RowMap
			for _, rm := range rightMaps {
				// Evaluate against a map that also exposes the right table's
				// qualified keys (t2.x) for view/subquery operands whose rows
				// are keyed unqualified.
				probe := make(RowMap, len(rm)+2)
				for k, v := range rm {
					probe[k] = v
					if tableName != "" && !strings.Contains(k, ".") {
						probe[tableName+"."+k] = v
					}
				}
				if e.evalOnCondition(effectiveOn, probe) {
					filtered = append(filtered, rm)
				}
			}
			rightMaps = filtered
			effectiveOn = nil
		}

		// Nested-loop join (for both table and view)
		var combinedMaps []RowMap
		// For USING clause, exclude the merged columns from the right table's
		// column definitions so that SELECT * expansion does not duplicate them.
		// Only a USING/NATURAL join merges columns; a regular ON clause must
		// keep both sides' columns even when they share names (A.ID = B.ID
		// leaves B.ID in the output).
		usingJoin := len(join.Using) > 0 || len(naturalCols) > 0
		filteredRightDefs := e.filterUsingColumns(rightDefs, effectiveOn, naturalCols, usingJoin)
		// Prefix remaining right-table column names when they conflict with
		// existing left-table column names, so * expansion resolves values
		// from the combined row map using qualified keys (table.col).
		rightDefsNamed := e.prefixRightColDefs(filteredRightDefs, currentDefs, tableName)
		combinedDefs := append(append([]sql.ColumnDef{}, currentDefs...), rightDefsNamed...)

		// Track which right rows were matched — needed for RIGHT/FULL JOIN
		// to find unmatched right rows that must be included with NULL left side.
		isRightOrFull := joinTypeHas(join.JoinType, "RIGHT") || joinTypeHas(join.JoinType, "FULL")
		matchedRight := make([]bool, len(rightMaps))

		for leftIdx, leftMap := range currentMaps {
			effectiveJoin := join
			if effectiveOn != join.On {
				effectiveJoin.On = effectiveOn
			}
			// For a correlated pragma the right rows were materialized per left
			// row; pair each left row only with its own right rows.
			rowRightMaps := rightMaps
			rowCorrLeft := corrLeftIdx
			if corrLeftIdx != nil {
				rowRightMaps = nil
				rowCorrLeft = nil
				for ri, li := range corrLeftIdx {
					if li == leftIdx {
						rowRightMaps = append(rowRightMaps, rightMaps[ri])
						rowCorrLeft = append(rowCorrLeft, 0) // placeholders; no further filtering
					}
				}
			}
			matched := e.processJoinRowTrackingRight(
				leftMap, rowRightMaps, &combinedMaps, lastTableName, tableName, effectiveJoin,
				rightDefs, autoIndex,
				isRightOrFull, matchedRight, leftIdx, rowCorrLeft)
			if !matched && (joinTypeHas(join.JoinType, "LEFT") || joinTypeHas(join.JoinType, "FULL")) {
				combinedMaps = append(combinedMaps, e.buildLeftJoinRow(leftMap, rightDefs, tableName, leftName))
			}
		}

		// For RIGHT and FULL JOIN: add unmatched right rows with NULL-padded left
		if isRightOrFull {
			for ri, rm := range rightMaps {
				if !matchedRight[ri] {
					combinedMaps = append(combinedMaps, e.buildRightJoinUnmatched(rm, currentDefs, rightDefsNamed, tableName, leftName))
				}
			}
		}

		currentMaps = combinedMaps
		currentDefs = combinedDefs
		lastTableName = tableName
	}

	return currentMaps, currentDefs, nil
}

// joinIndexEntry pairs a right row with its index in rightMaps, used by the
// ephemeral equi-join hash index so RIGHT/FULL joins can track which right
// rows matched.
type joinIndexEntry struct {
	row RowMap
	idx int
}

// processJoinRowTrackingRight processes a single left row against all right rows
// for a JOIN, optionally tracking which right rows were matched (for RIGHT/FULL JOIN).
// When trackMatchedRight is true, disables the autoIndex hash optimization so that
// right-row indices are available for tracking. leftIdx is the index of the left row
// (unused but kept for API symmetry).
// Returns true if at least one match was found (for the ON condition).
func (e *Engine) processJoinRowTrackingRight(
	leftMap RowMap, rightMaps []RowMap, combinedMaps *[]RowMap,
	leftTableName, tableName string, join sql.JoinClause,
	rightDefs []sql.ColumnDef, autoIndex map[interface{}][]joinIndexEntry,
	trackMatchedRight bool, matchedRight []bool, leftIdx int, corrLeftIdx []int,
) bool {
	matched := false
	if autoIndex != nil {
		leftColName, _ := extractEquiJoinCols(join.On, leftTableName, tableName)
		// The left row's column may be keyed unqualified (base table) or
		// qualified (a chained join's combined map stores each side under
		// table.col keys). Prefer the QUALIFIED key: in a chained join the
		// unqualified name resolves to the first table's column, which is
		// NOT the immediate-left table we are joining against.
		leftColVal, leftOK := leftMap[leftTableName+"."+leftColName]
		if !leftOK {
			leftColVal, leftOK = leftMap[leftColName]
		}
		// If the extracted left column isn't present (e.g., unqualified
		// columns guessed the wrong side), fall back to the nested-loop.
		if leftOK {
			uv := joinIndexKey(leftColVal)
			if rightRows, ok := autoIndex[uv]; ok {
				for _, entry := range rightRows {
					combinedMap := e.buildCombinedRowMap(leftMap, entry.row, tableName, leftTableName)
					if e.evalOnCondition(join.On, combinedMap) {
						matched = true
						*combinedMaps = append(*combinedMaps, combinedMap)
						if trackMatchedRight {
							matchedRight[entry.idx] = true
						}
					}
				}
			}
			// The hash index is authoritative for the equi-condition (keys are
			// affinity- and collation-normalized, so a miss means no
			// equi-match): skip the nested loop that would only re-scan right
			// rows finding none. Only when the extracted column was absent
			// (leftOK false) do we fall back to the nested loop.
			return matched
		}
		if !matched {
			// Fall through to the nested-loop when the hash lookup produced no
			// matches (wrong-side extraction, or NULL keys not in the index).
			for ri, rightMap := range rightMaps {
				combinedMap := e.buildCombinedRowMap(leftMap, rightMap, tableName, leftTableName)
				onPass := e.evalOnCondition(join.On, combinedMap)
				if onPass {
					matched = true
					*combinedMaps = append(*combinedMaps, combinedMap)
					if trackMatchedRight {
						matchedRight[ri] = true
					}
				}
			}
		}
	} else {
		for ri, rightMap := range rightMaps {
			combinedMap := e.buildCombinedRowMap(leftMap, rightMap, tableName, leftTableName)
			onPass := e.evalOnCondition(join.On, combinedMap)
			if onPass {
				matched = true
				*combinedMaps = append(*combinedMaps, combinedMap)
				if trackMatchedRight {
					matchedRight[ri] = true
				}
			}
		}
	}
	// CROSS JOIN: always produces a match. A NATURAL CROSS JOIN still applies
	// its generated equality condition, so it must not take this fallback.
	if !matched && join.JoinType == "CROSS" {
		for ri, rightMap := range rightMaps {
			*combinedMaps = append(*combinedMaps, e.buildCombinedRowMap(leftMap, rightMap, tableName, leftTableName))
			if trackMatchedRight {
				matchedRight[ri] = true
			}
		}
		matched = true
	}
	return matched
}

// buildRightJoinUnmatched creates a combined row for an unmatched right row
// in a RIGHT or FULL JOIN. The left side columns are set to NULL.
func (e *Engine) buildRightJoinUnmatched(rightMap RowMap, leftDefs, rightDefs []sql.ColumnDef, tableName, leftTableName string) RowMap {
	combined := make(RowMap)
	// Set all left-side columns to NULL (both qualified and unqualified)
	for _, cd := range leftDefs {
		combined[cd.Name] = nil
		if leftTableName != "" {
			combined[leftTableName+"."+cd.Name] = nil
		}
	}
	// Set right-side columns from the right row
	for _, cd := range rightDefs {
		// cd.Name may be prefixed (table.col) due to prefixRightColDefs
		colName := cd.Name
		// Also store under the unprefixed name for qualified lookups
		baseName := cd.Name
		if idx := strings.Index(colName, "."); idx >= 0 {
			baseName = colName[idx+1:]
		}
		if val, ok := rightMap[baseName]; ok {
			combined[colName] = val
			if _, exists := combined[baseName]; !exists {
				combined[baseName] = val
			}
		}
	}
	// Merged USING/NATURAL columns (filtered out of rightDefs) still carry
	// the right row's value for unmatched right rows: an unmatched right row
	// of t1 FULL JOIN t2 USING(id) has a real id from t2. Only overwrite
	// columns that were MERGED (present in the left defs but absent from the
	// right defs) — same-named columns of a plain ON join (t1.c vs t3.c)
	// must stay separate.
	rightDefNames := make(map[string]bool)
	for _, cd := range rightDefs {
		base := cd.Name
		if idx := strings.Index(base, "."); idx >= 0 {
			base = base[idx+1:]
		}
		rightDefNames[base] = true
	}
	for k, v := range rightMap {
		if _, isLeft := combined[k]; isLeft && k != "rowid" && !rightDefNames[k] {
			combined[k] = v
		}
	}
	// Copy the right row's qualified keys as-is (a derived table's row map
	// carries keys like t3.a and t4.a that must survive re-materialization),
	// and add the table-name prefix for unqualified keys.
	for k, v := range rightMap {
		if k == "rowid" {
			continue
		}
		if strings.Contains(k, ".") {
			if _, exists := combined[k]; !exists {
				combined[k] = v
			}
		} else if tableName != "" {
			combined[tableName+"."+k] = v
		}
	}
	return combined
}

// joinIndexKey unwraps a row value's ColumnValue and collatedValue wrappers
// and normalizes it for use as an ephemeral equi-join hash key. A NOCASE
// collation lowercases the key so 'ABC' and 'abc' match.
func joinIndexKey(v interface{}) interface{} {
	coll := ""
	if cv, ok := v.(*collatedValue); ok {
		coll = cv.collation
		v = cv.value
	}
	key := normalizeJoinKey(util.UnwrapColumnValue(v))
	if coll == "NOCASE" {
		if s, ok := key.(string); ok {
			return strings.ToLower(s)
		}
		if b, ok := key.([]byte); ok {
			return strings.ToLower(string(b))
		}
	}
	return key
}

// normalizeJoinKey converts a value to a canonical key for the ephemeral
// equi-join hash index: numeric text (e.g. "0", "1.5") becomes its numeric
// value so a text '0' right key matches an integer 0 left key (SQLite's
// affinity-aware equality). Non-numeric values are returned unchanged.
func normalizeJoinKey(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
		return t
	case []byte:
		return normalizeJoinKey(string(t))
	default:
		return v
	}
}

// joinONReferencesOnlyRight reports whether every column reference in a join
// ON expression is qualified to the RIGHT table (none unqualified, none to
// the left or other tables). Such a condition is independent of the left row
// and can be pre-filtered once on the right rows.
func joinONReferencesOnlyRight(on sql.Expr, rightName string) bool {
	if on == nil {
		return false
	}
	onlyRight := true
	walkJoinOnExpr(on, func(e sql.Expr) {
		cr, ok := e.(*sql.ColumnRef)
		if !ok {
			return
		}
		if cr.Table == "" || !strings.EqualFold(cr.Table, rightName) {
			onlyRight = false
		}
	})
	return onlyRight
}

// naturalJoinCommonCols returns the set of column names common to the left
// and right table definitions (used to merge columns in NATURAL JOIN output).
func naturalJoinCommonCols(leftDefs, rightDefs []sql.ColumnDef) map[string]bool {
	rightNames := make(map[string]bool)
	for _, cd := range rightDefs {
		rightNames[cd.Name] = true
	}
	common := make(map[string]bool)
	for _, cd := range leftDefs {
		if rightNames[cd.Name] {
			common[cd.Name] = true
		}
	}
	return common
}

// generateNaturalJoinOn creates an ON expression for a NATURAL JOIN by finding
// all common column names between left and right table definitions and creating
// equality conditions: col = col AND col2 = col2 ...
// If no common columns exist, NATURAL JOIN behaves as a CROSS JOIN (nil ON).
func (e *Engine) generateNaturalJoinOn(leftDefs, rightDefs []sql.ColumnDef, leftName, rightName string) sql.Expr {
	rightNames := make(map[string]bool)
	for _, cd := range rightDefs {
		rightNames[cd.Name] = true
	}
	var onExpr sql.Expr
	for _, cd := range leftDefs {
		if rightNames[cd.Name] {
			// Generate an equality whose LEFT side is UNQUALIFIED: in a chained
			// natural join the merged column (stored unqualified in the combined
			// row map) is the value from whichever side supplied it, while the
			// qualified left-table name can be NULL for rows that only matched
			// on a deeper table (t1 FULL JOIN t2 FULL JOIN t3 must match
			// t2-only rows of the first join against t3 on the merged id).
			eq := &sql.BinaryOp{
				Left:     &sql.ColumnRef{Name: cd.Name},
				Right:    &sql.ColumnRef{Table: rightName, Name: cd.Name},
				Operator: "=",
			}
			if onExpr == nil {
				onExpr = eq
			} else {
				onExpr = &sql.BinaryOp{Left: onExpr, Right: eq, Operator: "AND"}
			}
		}
	}
	return onExpr
}

// generateUsingJoinOn builds ON col = col for each USING column. The refs are
// unqualified (like NATURAL JOIN) so filterUsingColumns/collectUsingColumns can
// recognize and merge them (the column resolution uses currentScanTable).
func (e *Engine) generateUsingJoinOn(cols []string, leftName, rightName string) sql.Expr {
	var onExpr sql.Expr
	for _, col := range cols {
		// The LEFT side is UNQUALIFIED: in a chained join the immediate left
		// "table" may be a JOIN result whose merged column is stored under
		// the plain name, while the qualified name (e.g. dual.id for
		// t3 JOIN dual FULL JOIN t4 USING(id)) may not exist. The merged
		// column always resolves via the unqualified key. When the right side
		// is a derived table without an alias (rightName == ""), its column
		// is also unqualified.
		right := &sql.ColumnRef{Table: rightName, Name: col}
		if rightName == "" {
			right = &sql.ColumnRef{Name: col}
		}
		eq := &sql.BinaryOp{
			Left:     &sql.ColumnRef{Name: col},
			Right:    right,
			Operator: "=",
		}
		if onExpr == nil {
			onExpr = eq
		} else {
			onExpr = &sql.BinaryOp{Left: onExpr, Right: eq, Operator: "AND"}
		}
	}
	return onExpr
}

// extractEquiJoinCols examines a join ON expression looking for a simple
// equality comparison (col = col) where one column belongs to the left table
// and the other to the right table. Returns (leftCol, rightCol) or ("", "").
// The leftCol belongs to the join's left (base) table and rightCol to the
// right (joined) table, identified by the tableName parameter.
func extractEquiJoinCols(on sql.Expr, leftTableName, rightTableName string) (string, string) {
	if on == nil {
		return "", ""
	}
	// IS NOT DISTINCT FROM is a NULL-safe equality: usable as an equi-join
	// index key (NULL keys are handled by the index).
	if ndf, ok := on.(*sql.IsNotDistinctFrom); ok {
		return extractEquiJoinCols(
			&sql.BinaryOp{Left: ndf.Left, Right: ndf.Right, Operator: "="},
			leftTableName, rightTableName)
	}
	bop, ok := on.(*sql.BinaryOp)
	if !ok {
		return "", ""
	}
	if bop.Operator == "AND" {
		// Search an AND chain for a usable equality (e.g. ON t1.d=t4.d AND
		// t4.z>0): the equality drives the hash index, the rest is still
		// evaluated per row by evalOnCondition.
		if l, r := extractEquiJoinCols(bop.Left, leftTableName, rightTableName); l != "" {
			return l, r
		}
		return extractEquiJoinCols(bop.Right, leftTableName, rightTableName)
	}
	if bop.Operator != "=" && bop.Operator != "IS NOT DISTINCT FROM" && bop.Operator != "IS" {
		return "", ""
	}
	leftCol, leftOK := bop.Left.(*sql.ColumnRef)
	rightCol, rightOK := bop.Right.(*sql.ColumnRef)
	if !leftOK || !rightOK {
		return "", ""
	}
	// Determine which side is left and which is right
	if (leftCol.Table == "" || strings.EqualFold(leftCol.Table, leftTableName)) &&
		(strings.EqualFold(rightCol.Table, rightTableName)) {
		return leftCol.Name, rightCol.Name
	}
	if (rightCol.Table == "" || strings.EqualFold(rightCol.Table, leftTableName)) &&
		(strings.EqualFold(leftCol.Table, rightTableName)) {
		return rightCol.Name, leftCol.Name
	}
	// If neither side has a table qualifier, assume first col is left, second is right
	if leftCol.Table == "" && rightCol.Table == "" {
		return leftCol.Name, rightCol.Name
	}
	return "", ""
}

// buildCombinedRowMap creates a combined row map from left and right join sides.
// It stores values under both unqualified names and table-prefixed names so that
// qualified column references (e.g., "data.id") resolve correctly for both sides.
func (e *Engine) buildCombinedRowMap(leftMap, rightMap RowMap, tableName, leftTableName string) RowMap {
	combined := make(RowMap)
	// Copy the left map. Keys already qualified (containing a '.') are
	// copied as-is; unqualified keys (from the base table scan) get the
	// left-table prefix added once. Re-qualifying keys that are already
	// qualified would produce garbage keys like "t1.t2.a".
	for k, v := range leftMap {
		combined[k] = v
		if !strings.Contains(k, ".") && k != "rowid" {
			qk := leftTableName + "." + k
			if _, exists := combined[qk]; !exists {
				combined[qk] = v
			}
		}
	}
	for k, v := range rightMap {
		combined[tableName+"."+k] = v
		if _, exists := combined[k]; !exists {
			combined[k] = v
		}
	}
	combined[leftTableName+".rowid"] = leftMap["rowid"]
	return combined
}

// evalOnCondition evaluates a JOIN ON condition against a combined row map.
func (e *Engine) evalOnCondition(on sql.Expr, row Row) bool {
	if on == nil {
		return true
	}
	match, err := e.evalBool(on, row)
	return err == nil && match
}

// filterUsingColumns filters right-side column definitions to exclude columns
// that are part of a USING clause. The USING clause generates equality conditions
// in the ON expression, and those columns should appear only once in the result.
func (e *Engine) filterUsingColumns(rightDefs []sql.ColumnDef, on sql.Expr, naturalCols map[string]bool, usingJoin bool) []sql.ColumnDef {
	if !usingJoin || (on == nil && len(naturalCols) == 0) {
		return rightDefs
	}
	// Collect column names referenced in USING equality conditions.
	usingCols := make(map[string]bool)
	collectUsingColumns(on, usingCols)
	for c := range naturalCols {
		usingCols[c] = true
	}
	if len(usingCols) == 0 {
		return rightDefs
	}
	var filtered []sql.ColumnDef
	for _, cd := range rightDefs {
		if usingCols[cd.Name] {
			continue // skip — this column is merged by USING
		}
		filtered = append(filtered, cd)
	}
	return filtered
}

// collectUsingColumns recursively walks a USING-generated ON expression and
// collects column names from equality comparisons (col = col).
// A USING clause is converted by the parser to "col = col" where both sides
// have the SAME name and NO table qualifier. Regular ON conditions like
// "t1.a = t2.c" have table qualifiers or different names, so they are excluded.
func collectUsingColumns(expr sql.Expr, cols map[string]bool) {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		collectUsingColumns(v.Expr, cols)
	case *sql.BinaryOp:
		if v.Operator == "=" {
			leftRef, leftOK := v.Left.(*sql.ColumnRef)
			rightRef, rightOK := v.Right.(*sql.ColumnRef)
			// Treat as USING when both sides reference the SAME column name,
			// either both unqualified (legacy) or each qualified with a
			// different table (left.name = right.name, the current form).
			if leftOK && rightOK && leftRef.Name == rightRef.Name {
				if leftRef.Table == "" && rightRef.Table == "" {
					cols[leftRef.Name] = true
				} else if !strings.EqualFold(leftRef.Table, rightRef.Table) {
					cols[leftRef.Name] = true
				}
			}
		} else if v.Operator == "AND" {
			collectUsingColumns(v.Left, cols)
			collectUsingColumns(v.Right, cols)
		}
	}
}

// rightDefHasColumn reports whether defs contains a column named name
// (case-insensitive), including prefixed defs (table.col).
func rightDefHasColumn(defs []sql.ColumnDef, name string) bool {
	for _, cd := range defs {
		if strings.EqualFold(cd.Name, name) ||
			strings.HasSuffix(strings.ToLower(cd.Name), "."+strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// prefixRightColDefs prefixes right-table column names with the table name
// when they conflict with columns already in the left table. This ensures
// that * expansion resolves values using qualified keys (table.col) from
// the combined row map, avoiding incorrect resolution to the left table's values.
func (e *Engine) prefixRightColDefs(rightDefs, leftDefs []sql.ColumnDef, tableName string) []sql.ColumnDef {
	// Build set of left-column names for quick conflict detection.
	leftNames := make(map[string]bool)
	for _, cd := range leftDefs {
		leftNames[cd.Name] = true
	}
	needsPrefix := false
	for _, cd := range rightDefs {
		if leftNames[cd.Name] {
			needsPrefix = true
			break
		}
	}
	if !needsPrefix {
		return rightDefs
	}
	named := make([]sql.ColumnDef, len(rightDefs))
	for i, cd := range rightDefs {
		named[i] = cd
		if leftNames[cd.Name] {
			named[i].Name = tableName + "." + cd.Name
		}
	}
	return named
}

// buildLeftJoinRow creates a row for LEFT JOIN when no match is found.
func (e *Engine) buildLeftJoinRow(leftMap RowMap, rightDefs []sql.ColumnDef, tableName, leftTableName string) RowMap {
	combined := make(RowMap)
	for k, v := range leftMap {
		combined[k] = v
		if leftTableName != "" && !strings.Contains(k, ".") && k != "rowid" {
			qk := leftTableName + "." + k
			if _, exists := combined[qk]; !exists {
				combined[qk] = v
			}
		}
	}
	for _, cd := range rightDefs {
		combined[tableName+"."+cd.Name] = nil
		if _, exists := combined[cd.Name]; !exists {
			combined[cd.Name] = nil
		}
	}
	return combined
}

// hasAggregates checks if any SELECT column uses an aggregate function.
func (e *Engine) hasAggregates(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if e.exprHasAggregate(col.Expr) {
			return true
		}
	}
	return false
}

func (e *Engine) exprHasAggregate(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if fn, ok := e.funcs.Find(v.Name); ok && fn.Type == function.TypeAggregate {
			// MIN/MAX are scalar functions when given two or more arguments
			// (SQLite: min(X,Y,...) is scalar; min(X) is aggregate). Without
			// this check a plain per-row min(b,5) collapses the whole query
			// into a single aggregate row.
			if (strings.EqualFold(v.Name, "MIN") || strings.EqualFold(v.Name, "MAX")) && len(v.Args) >= 2 {
				for _, arg := range v.Args {
					if e.exprHasAggregate(arg) {
						return true
					}
				}
				return false
			}
			return true
		}
		for _, arg := range v.Args {
			if e.exprHasAggregate(arg) {
				return true
			}
		}
		return false
	case *sql.BinaryOp:
		return e.exprHasAggregate(v.Left) || e.exprHasAggregate(v.Right)
	case *sql.UnaryOp:
		return e.exprHasAggregate(v.Operand)
	case *sql.ParenExpr:
		return e.exprHasAggregate(v.Expr)
	case *sql.CastExpr:
		return e.exprHasAggregate(v.Operand)
	case *sql.IsNull:
		return e.exprHasAggregate(v.Operand)
	case *sql.IsNotNull:
		return e.exprHasAggregate(v.Operand)
	case *sql.IsTrue:
		return e.exprHasAggregate(v.Operand)
	case *sql.IsFalse:
		return e.exprHasAggregate(v.Operand)
	case *sql.IsDistinctFrom:
		return e.exprHasAggregate(v.Left) || e.exprHasAggregate(v.Right)
	case *sql.IsNotDistinctFrom:
		return e.exprHasAggregate(v.Left) || e.exprHasAggregate(v.Right)
	case *sql.Between:
		return e.exprHasAggregate(v.Operand) || e.exprHasAggregate(v.Low) || e.exprHasAggregate(v.High)
	case *sql.InList:
		if e.exprHasAggregate(v.Operand) {
			return true
		}
		for _, item := range v.List {
			if e.exprHasAggregate(item) {
				return true
			}
		}
		return false
	case *sql.CaseExpr:
		if v.Operand != nil && e.exprHasAggregate(v.Operand) {
			return true
		}
		for _, w := range v.Whens {
			if e.exprHasAggregate(w.When) || e.exprHasAggregate(w.Then) {
				return true
			}
		}
		if v.Else != nil {
			return e.exprHasAggregate(v.Else)
		}
		return false
	case *sql.Subquery:
		// Aggregates inside a scalar subquery are scoped to that subquery;
		// they do not make the OUTER query an aggregate query
		// (SELECT (SELECT count(*) FROM t) FROM u returns one row per u
		// row, not a single collapsed row).
		return false
	case *sql.ExistsExpr:
		return false
	case *sql.RowValue:
		for _, sub := range v.Values {
			if e.exprHasAggregate(sub) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// minMaxAggregate describes a single-argument MIN or MAX aggregate function
// call. It is used to resolve the source row for bare columns in aggregate
// queries: SQLite evaluates bare columns against the input row that produced
// the last min/max aggregate in the result set.
type minMaxAggregate struct {
	name string // "MIN" or "MAX" (uppercased)
	arg  sql.Expr
}

// lastMinMaxAggregate returns the last (rightmost) single-argument MIN/MAX
// aggregate function call found in the SELECT columns, scanning left to right
// and descending into nested expressions. Returns nil when the result set has
// no min/max aggregate.
func (e *Engine) lastMinMaxAggregate(columns []sql.SelectColumn) *minMaxAggregate {
	var last *minMaxAggregate
	for _, col := range columns {
		if mm := lastMinMaxInExpr(col.Expr, e.funcs); mm != nil {
			last = mm
		}
	}
	return last
}

// lastMinMaxInExpr walks an expression tree depth-first, left-to-right, and
// returns the last single-argument MIN/MAX aggregate function call found.
func lastMinMaxInExpr(expr sql.Expr, funcs *function.Registry) *minMaxAggregate {
	switch v := expr.(type) {
	case *sql.FuncCall:
		// A single-argument MIN/MAX is an aggregate; with two or more
		// arguments MIN/MAX is a scalar function (SQLite semantics) and
		// does not determine bare-column rows.
		if len(v.Args) == 1 && (strings.EqualFold(v.Name, "MIN") || strings.EqualFold(v.Name, "MAX")) {
			if fn, ok := funcs.Find(v.Name); ok && fn.Type == function.TypeAggregate {
				// Nested occurrences inside the argument (e.g. an aggregate
				// in an expression argument) come earlier in traversal
				// order, so check them first.
				if inner := lastMinMaxInExpr(v.Args[0], funcs); inner != nil {
					return inner
				}
				return &minMaxAggregate{name: strings.ToUpper(v.Name), arg: v.Args[0]}
			}
		}
		// Not a single-arg min/max aggregate: scan the arguments.
		var last *minMaxAggregate
		for _, arg := range v.Args {
			if mm := lastMinMaxInExpr(arg, funcs); mm != nil {
				last = mm
			}
		}
		return last
	case *sql.BinaryOp:
		if mm := lastMinMaxInExpr(v.Left, funcs); mm != nil {
			return mm
		}
		return lastMinMaxInExpr(v.Right, funcs)
	case *sql.UnaryOp:
		return lastMinMaxInExpr(v.Operand, funcs)
	case *sql.ParenExpr:
		return lastMinMaxInExpr(v.Expr, funcs)
	case *sql.CastExpr:
		return lastMinMaxInExpr(v.Operand, funcs)
	case *sql.IsNull:
		return lastMinMaxInExpr(v.Operand, funcs)
	case *sql.IsNotNull:
		return lastMinMaxInExpr(v.Operand, funcs)
	case *sql.IsTrue:
		return lastMinMaxInExpr(v.Operand, funcs)
	case *sql.IsFalse:
		return lastMinMaxInExpr(v.Operand, funcs)
	case *sql.IsDistinctFrom:
		if mm := lastMinMaxInExpr(v.Left, funcs); mm != nil {
			return mm
		}
		return lastMinMaxInExpr(v.Right, funcs)
	case *sql.IsNotDistinctFrom:
		if mm := lastMinMaxInExpr(v.Left, funcs); mm != nil {
			return mm
		}
		return lastMinMaxInExpr(v.Right, funcs)
	case *sql.Between:
		if mm := lastMinMaxInExpr(v.Operand, funcs); mm != nil {
			return mm
		}
		if mm := lastMinMaxInExpr(v.Low, funcs); mm != nil {
			return mm
		}
		return lastMinMaxInExpr(v.High, funcs)
	case *sql.InList:
		if mm := lastMinMaxInExpr(v.Operand, funcs); mm != nil {
			return mm
		}
		for _, item := range v.List {
			if mm := lastMinMaxInExpr(item, funcs); mm != nil {
				return mm
			}
		}
		return nil
	case *sql.CaseExpr:
		if v.Operand != nil {
			if mm := lastMinMaxInExpr(v.Operand, funcs); mm != nil {
				return mm
			}
		}
		for _, w := range v.Whens {
			if mm := lastMinMaxInExpr(w.When, funcs); mm != nil {
				return mm
			}
			if mm := lastMinMaxInExpr(w.Then, funcs); mm != nil {
				return mm
			}
		}
		if v.Else != nil {
			return lastMinMaxInExpr(v.Else, funcs)
		}
		return nil
	case *sql.RowValue:
		for _, sub := range v.Values {
			if mm := lastMinMaxInExpr(sub, funcs); mm != nil {
				return mm
			}
		}
		return nil
	default:
		return nil
	}
}

// minMaxSourceRow evaluates a single-argument MIN/MAX aggregate's argument
// over the given rows and returns the index of the row that produced the
// extreme value (the first row on ties). When every argument is NULL the
// aggregate yields NULL and bare columns take the last row, matching SQLite.
// Returns -1 when rows is empty.
func (e *Engine) minMaxSourceRow(mm *minMaxAggregate, rowMaps []RowMap) int {
	if len(rowMaps) == 0 {
		return -1
	}
	bestIdx := -1
	var bestVal interface{}
	for i, row := range rowMaps {
		val, err := e.evalExpr(mm.arg, row)
		if err != nil || val == nil {
			continue
		}
		val = util.UnwrapColumnValue(val)
		if bestIdx < 0 {
			bestIdx = i
			bestVal = val
			continue
		}
		cmp := util.CompareValues(val, bestVal)
		if (mm.name == "MIN" && cmp < 0) || (mm.name == "MAX" && cmp > 0) {
			bestIdx = i
			bestVal = val
		}
	}
	if bestIdx < 0 {
		// All arguments NULL (or empty rows): bare columns come from the
		// last row.
		return len(rowMaps) - 1
	}
	return bestIdx
}

// reorderRowsForMinMax moves the row that produced the last min/max aggregate
// to the front of rowMaps so bare columns in an aggregate query evaluate from
// the correct source row. Returns the reordered slice (a copy is not made
// unless reordering is needed).
func (e *Engine) reorderRowsForMinMax(columns []sql.SelectColumn, rowMaps []RowMap) []RowMap {
	mm := e.lastMinMaxAggregate(columns)
	if mm == nil || len(rowMaps) <= 1 {
		return rowMaps
	}
	idx := e.minMaxSourceRow(mm, rowMaps)
	if idx <= 0 {
		return rowMaps
	}
	rows := make([]RowMap, len(rowMaps))
	copy(rows, rowMaps)
	rows[0], rows[idx] = rows[idx], rows[0]
	return rows
}

// aggregateName returns the name of the first aggregate function found in the
// expression, or "?" if none is found.
func (e *Engine) aggregateName(expr sql.Expr) string {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if fn, ok := e.funcs.Find(v.Name); ok && fn.Type == function.TypeAggregate {
			return v.Name
		}
		return "?"
	case *sql.BinaryOp:
		if n := e.aggregateName(v.Left); n != "?" {
			return n
		}
		return e.aggregateName(v.Right)
	case *sql.UnaryOp:
		return e.aggregateName(v.Operand)
	default:
		return "?"
	}
}

// aggHasColumnRef checks if any aggregate function in the SELECT columns
// has arguments that contain column references. This identifies correlated
// aggregates that need to be evaluated over all outer rows.
func (e *Engine) aggHasColumnRef(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
				for _, arg := range fn.Args {
					if e.exprHasColumnRef(arg) {
						return true
					}
				}
			}
		}
	}
	return false
}

// exprHasColumnRef recursively checks if an expression tree contains a ColumnRef node.
// This does NOT recurse into Subquery expressions — correlated aggregate detection
// is handled separately at the SELECT level.
func (e *Engine) exprHasColumnRef(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		// A bare "*" (count(*), SELECT *) is not a column reference: it
		// means "all rows of the FROM table", so it must not count as an
		// outer reference in correlated-aggregate detection.
		return v.Name != "*"
	case *sql.BinaryOp:
		return e.exprHasColumnRef(v.Left) || e.exprHasColumnRef(v.Right)
	case *sql.UnaryOp:
		return e.exprHasColumnRef(v.Operand)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if e.exprHasColumnRef(arg) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// exprContainsSubquery checks if an expression tree contains a Subquery node.
func exprContainsSubquery(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.Subquery:
		return true
	case *sql.BinaryOp:
		return exprContainsSubquery(v.Left) || exprContainsSubquery(v.Right)
	case *sql.UnaryOp:
		return exprContainsSubquery(v.Operand)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if exprContainsSubquery(arg) {
				return true
			}
		}
		for _, ob := range v.OrderBy {
			if exprContainsSubquery(ob.Expr) {
				return true
			}
		}
		return false
	case *sql.Between:
		return exprContainsSubquery(v.Operand) || exprContainsSubquery(v.Low) || exprContainsSubquery(v.High)
	case *sql.InList:
		if exprContainsSubquery(v.Operand) {
			return true
		}
		for _, item := range v.List {
			if exprContainsSubquery(item) {
				return true
			}
		}
		return false
	case *sql.CaseExpr:
		if exprContainsSubquery(v.Operand) {
			return true
		}
		for _, w := range v.Whens {
			if exprContainsSubquery(w.When) || exprContainsSubquery(w.Then) {
				return true
			}
		}
		return exprContainsSubquery(v.Else)
	case *sql.CastExpr:
		return exprContainsSubquery(v.Operand)
	case *sql.ExistsExpr:
		return true
	default:
		return false
	}
}

// selectHasCorrelatedAggSubquery checks if a SELECT statement (or any nested
// subquery within it) contains a correlated aggregate — an aggregate function
// that references columns from an outer context.
// This detects two cases:
//  1. FROM-less SELECT with aggregates that have column references
//  2. SELECT with FROM clause where aggregate args reference only outer columns
//     (none exist in the FROM table — making the aggregate fully correlated).
func (e *Engine) selectHasCorrelatedAggSubquery(s *sql.SelectStmt) bool {
	if s == nil {
		return false
	}
	// Case 1: FROM-less SELECT with aggregates that reference columns
	if s.From.Name == "" && s.From.Subquery == nil && len(s.From.As) == 0 {
		if e.aggHasColumnRef(s.Columns) {
			return true
		}
	}
	// Case 2: SELECT with FROM table that has aggregates referencing only outer columns.
	// This means the aggregate's column references do NOT match any column in the FROM table.
	if s.From.Name != "" && e.aggHasColumnRef(s.Columns) && !e.aggRefsMatchFromTable(s) {
		return true
	}
	// Check FROM subquery recursively
	if s.From.Subquery != nil {
		if e.selectHasCorrelatedAggSubquery(s.From.Subquery) {
			return true
		}
	}
	// Check subqueries in SELECT columns
	for _, col := range s.Columns {
		if subq, ok := col.Expr.(*sql.Subquery); ok {
			if e.selectHasCorrelatedAggSubquery(subq.Select) {
				return true
			}
		}
	}
	// Check compound (UNION/INTERSECT/EXCEPT) members: an aggregate
	// referencing outer columns may live in any member (e.g.
	// (SELECT 'mmm' UNION SELECT max(outer.col) ORDER BY 1)).
	for m := s.Union; m != nil; m = m.Union {
		if e.selectHasCorrelatedAggSubquery(m) {
			return true
		}
	}
	return false
}

// aggRefsMatchFromTable checks if any aggregate function's column references
// match a column name in the FROM table. Returns true if any aggregate arg
// references a column that exists in the FROM table, indicating the aggregate
// is NOT fully correlated (it references inner columns).
func (e *Engine) aggRefsMatchFromTable(s *sql.SelectStmt) bool {
	if s.From.Name == "" {
		return false
	}
	tableEntry, err := e.schema.FindTable(s.From.Name)
	if err != nil {
		return false
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	colNames := make(map[string]bool)
	for _, cd := range colDefs {
		colNames[cd.Name] = true
	}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			for _, arg := range fn.Args {
				if exprHasColRefInMap(arg, colNames) {
					return true
				}
			}
			// Also check ORDER BY terms for column references matching inner table
			for _, ob := range fn.OrderBy {
				if exprHasColRefInMap(ob.Expr, colNames) {
					return true
				}
			}
		}
	}
	return false
}

// exprHasColRefInMap checks if an expression tree contains a ColumnRef whose
// name matches an entry in the provided column name map.
func exprHasColRefInMap(expr sql.Expr, colNames map[string]bool) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return colNames[v.Name]
	case *sql.BinaryOp:
		return exprHasColRefInMap(v.Left, colNames) || exprHasColRefInMap(v.Right, colNames)
	case *sql.UnaryOp:
		return exprHasColRefInMap(v.Operand, colNames)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if exprHasColRefInMap(arg, colNames) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// exprHasCorrelatedSubquery checks if an expression tree contains a subquery
// that has a correlated aggregate.
//
//lint:ignore U1000  Planned for query optimization
func (e *Engine) exprHasCorrelatedSubquery(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.Subquery:
		return e.selectHasCorrelatedAggSubquery(v.Select)
	case *sql.BinaryOp:
		return e.exprHasCorrelatedSubquery(v.Left) || e.exprHasCorrelatedSubquery(v.Right)
	case *sql.UnaryOp:
		return e.exprHasCorrelatedSubquery(v.Operand)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if e.exprHasCorrelatedSubquery(arg) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// hasSubqueryWithCorrelatedAgg checks if any SELECT column contains a subquery
// that has a correlated aggregate at any nesting depth.
func (e *Engine) hasSubqueryWithCorrelatedAgg(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if subq, ok := col.Expr.(*sql.Subquery); ok {
			if e.selectHasCorrelatedAggSubquery(subq.Select) {
				return true
			}
		}
	}
	return false
}

// evalAggOverOuterRows evaluates aggregate functions in FROM-less SELECT
// over all provided outer rows, returning a single-row result.
func (e *Engine) evalAggOverOuterRows(s *sql.SelectStmt, outerRows []RowMap) []interface{} {
	var outRow []interface{}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
				// Aggregate: step over all outer rows
				agg := reg.AggregateFn()
				for _, row := range outerRows {
					args := make([]interface{}, len(fn.Args))
					for i, arg := range fn.Args {
						v, err := e.evalExpr(arg, row)
						if err != nil {
							args[i] = nil
						} else {
							args[i] = util.UnwrapColumnValue(v)
						}
					}
					if err := agg.Step(args); err != nil {
						// Continue with what we have
					}
				}
				result, _ := agg.Final()
				outRow = append(outRow, result)
				continue
			}
		}
		// Non-aggregate: evaluate with nil row
		v, err := e.evalExpr(col.Expr, nil)
		if err != nil {
			outRow = append(outRow, nil)
		} else {
			outRow = append(outRow, unwrapCollatedValue(v))
		}
	}
	return outRow
}

// aggregateHasOnlyOuterRefs checks if an aggregate function's arguments and
// ORDER BY terms contain column references, and if none of those references
// match the given inner column set. Returns true only if the aggregate has
// column references and all of them are from outside the inner table.
// This distinguishes from aggregates with no column refs (like count(*))
// which should use inner rows, not outer rows.
func (e *Engine) aggregateHasOnlyOuterRefs(fn *sql.FuncCall, innerColNames map[string]bool) bool {
	// If the aggregate has a subquery in its arguments, it needs inner rows
	// for the subquery to evaluate correctly (the subquery may reference inner columns).
	for _, arg := range fn.Args {
		if exprContainsSubquery(arg) {
			return false
		}
	}
	// Check ORDER BY terms for subqueries
	for _, ob := range fn.OrderBy {
		if exprContainsSubquery(ob.Expr) {
			return false
		}
	}
	hasColRefs := false
	for _, arg := range fn.Args {
		if e.exprHasColumnRef(arg) {
			hasColRefs = true
			if innerColNames != nil && exprHasColRefInMap(arg, innerColNames) {
				return false // Found a column ref matching inner table
			}
		}
	}
	// Check ORDER BY terms for column references
	for _, ob := range fn.OrderBy {
		if e.exprHasColumnRef(ob.Expr) {
			hasColRefs = true
			if innerColNames != nil && exprHasColRefInMap(ob.Expr, innerColNames) {
				return false
			}
		}
	}
	// Only use outer rows if we have at least one column ref
	// and none matched inner columns
	return hasColRefs
}

// evalAggOverOuterRowsWithInner evaluates aggregate functions over outerRows
// and non-aggregate expressions over the first inner row (allRowMaps).
// This handles the case where a subquery with its own FROM has aggregates
// that reference only outer columns (fully correlated).
func (e *Engine) evalAggOverOuterRowsWithInner(s *sql.SelectStmt, outerRows, allRowMaps []RowMap) []interface{} {
	var outRow []interface{}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
				// Aggregate: step over all outer rows
				agg := reg.AggregateFn()
				for _, row := range outerRows {
					args := make([]interface{}, len(fn.Args))
					for i, arg := range fn.Args {
						v, err := e.evalExpr(arg, row)
						if err != nil {
							args[i] = nil
						} else {
							args[i] = util.UnwrapColumnValue(v)
						}
					}
					if err := agg.Step(args); err != nil {
						// Continue with what we have
					}
				}
				result, _ := agg.Final()
				outRow = append(outRow, result)
				continue
			}
		}
		// Non-aggregate: evaluate using first inner row
		if len(allRowMaps) > 0 {
			v, err := e.evalExpr(col.Expr, allRowMaps[0])
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, unwrapCollatedValue(v))
			}
		} else {
			v, err := e.evalExpr(col.Expr, nil)
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, unwrapCollatedValue(v))
			}
		}
	}
	return outRow
}

// evalAggregates evaluates aggregate functions across all row maps.
func (e *Engine) evalAggregates(s *sql.SelectStmt, rowMaps []RowMap) *Result {
	if len(rowMaps) == 0 {
		return e.evalAggregatesEmpty(s)
	}

	// Nested aggregate functions inside wrapper expressions (e.g.
	// round(avg(x),2)) resolve through aggRowMaps instead of per-row.
	e.aggRowMaps = rowMaps
	defer func() { e.aggRowMaps = nil }()

	// If any aggregate has ORDER BY, sort rowMaps so bare columns evaluate
	// from the correct row (the one that provides the aggregate value).
	// For max, bare columns come from the last row in sorted order.
	// For min, from the first row.
	hasMaxOrderBy := false
	var orderBy []sql.OrderByTerm
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok && len(fn.OrderBy) > 0 {
			orderBy = fn.OrderBy
			hasMaxOrderBy = strings.ToUpper(fn.Name) == "MAX"
			break
		}
	}
	if len(orderBy) > 0 && len(rowMaps) > 1 {
		sortedMaps := make([]RowMap, len(rowMaps))
		copy(sortedMaps, rowMaps)
		sort.SliceStable(sortedMaps, func(i, j int) bool {
			for _, ob := range orderBy {
				vi, errI := e.evalExpr(ob.Expr, sortedMaps[i])
				vj, errJ := e.evalExpr(ob.Expr, sortedMaps[j])
				if errI != nil || errJ != nil {
					continue
				}
				cmp := util.CompareValues(vi, vj)
				if cmp != 0 {
					if ob.Desc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
		// For max ORDER BY, the aggregate's value comes from the last row.
		// Set rowMaps[0] to the last sorted row so bare columns use it.
		if hasMaxOrderBy {
			sortedMaps[0] = sortedMaps[len(sortedMaps)-1]
		}
		rowMaps = sortedMaps
	}

	// Bare columns in an aggregate query take their values from the row
	// that produced the last min/max aggregate (SQLite semantics), not from
	// an arbitrary first row. Reorder so that row is first; aggregates
	// still evaluate over all rows.
	rowMaps = e.reorderRowsForMinMax(s.Columns, rowMaps)

	columns := e.buildColumnNames(s.Columns, nil, s)
	var outRow []interface{}
	for _, col := range s.Columns {
		v, err := e.evalAggregateExpr(col.Expr, rowMaps)
		if err != nil {
			return &Result{Error: err}
		}
		// Bare columns in an aggregate query come from a row map as a
		// ColumnValue wrapper; unwrap so the output shows the value.
		outRow = append(outRow, util.UnwrapColumnValue(v))
	}
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: [][]interface{}{outRow}}, s, nil)
}

func (e *Engine) evalAggregatesEmpty(s *sql.SelectStmt) *Result {
	columns := e.buildColumnNames(s.Columns, nil, s)
	var outRow []interface{}
	// Non-aggregate expressions in an aggregate query over zero rows are
	// evaluated against a synthetic all-NULL row (SQLite semantics: the
	// aggregate still emits one row, and bare expressions see NULL inputs,
	// e.g. SELECT a IS NULL, count(*) FROM empty → 1 0).
	emptyRow := RowMap{}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if f, found := e.funcs.Find(fn.Name); found && f.Type == function.TypeAggregate {
				switch f.Name {
				case "COUNT":
					outRow = append(outRow, int64(0))
				case "TOTAL":
					outRow = append(outRow, float64(0.0))
				default:
					// Aggregates that define an empty-input Final (e.g.
					// md5sum returns the MD5 of the empty string) report it;
					// most aggregates yield NULL over zero rows.
					if f.AggregateFn != nil {
						agg := f.AggregateFn()
						if res, err := agg.Final(); err == nil {
							outRow = append(outRow, res)
							continue
						}
					}
					outRow = append(outRow, nil)
				}
				continue
			}
		}
		v, err := e.evalExpr(col.Expr, emptyRow)
		if err != nil {
			outRow = append(outRow, nil)
		} else {
			outRow = append(outRow, util.UnwrapColumnValue(v))
		}
	}
	if outRow != nil {
		return &Result{Columns: columns, Rows: [][]interface{}{outRow}}
	}
	return nil
}

// evalAggregatesGroupBy partitions rows by GROUP BY key, evaluates aggregates
// per group, and applies HAVING.
func (e *Engine) evalAggregatesGroupBy(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result {
	if len(rowMaps) == 0 {
		return nil
	}

	// Partition rows by GROUP BY key
	groups := make(map[string][]RowMap)
	keyVals := make(map[string][]interface{})
	var keyOrder []string

	groupBy := resolveGroupByOrdinals(s, colDefs)
	for _, row := range rowMaps {
		key, vals := e.computeGroupByKeyValues(groupBy, row)
		if _, exists := groups[key]; !exists {
			keyOrder = append(keyOrder, key)
			keyVals[key] = vals
		}
		groups[key] = append(groups[key], row)
	}
	// Sort keys for deterministic output matching SQLite GROUP BY behavior.
	// Use the evaluated key VALUES (not the serialized strings) so NULL
	// sorts first and numeric keys sort numerically.
	e.sortGroupKeys(keyOrder, keyVals)

	columns := e.buildColumnNames(s.Columns, colDefs, s)
	var outRows [][]interface{}
	var outMaps []RowMap

	for _, key := range keyOrder {
		groupRows := groups[key]
		groupVals := keyVals[key]

		// Bare columns within each group take their values from the row
		// that produced the last min/max aggregate of the group (SQLite
		// semantics). Reorder so that row is first.
		groupRows = e.reorderRowsForMinMax(s.Columns, groupRows)

		// Evaluate output row for this group
		e.aggRowMaps = groupRows
		var outRow []interface{}
		for ci, col := range s.Columns {
			// If this output column is structurally the same expression as a
			// GROUP BY term, emit the group's key value (SQLite reuses the
			// grouping value; re-evaluating would break non-deterministic
			// expressions like random() and float formatting).
			if gi := matchGroupByExpr(groupBy, col.Expr); gi >= 0 {
				if gi < len(groupVals) {
					outRow = append(outRow, groupVals[gi])
					continue
				}
			}
			// Star expansion (SELECT * / t.*) inside GROUP BY output: expand
			// to the source columns, matching buildOutputRow. evalAggregateExpr
			// would otherwise evaluate a star ColumnRef as the literal "*"
			// (a single column), which is wrong for grouped output.
			if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
				groupRow := groupRows[0]
				if ref.Table != "" {
					tableCols := e.qualifiedStarColNames(ref.Table, colDefs, groupRow)
					for _, cd := range tableCols {
						outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(cd.value)))
					}
				} else {
					for _, cd := range colDefs {
						if cd.Dropped || isHiddenColumnDef(cd) {
							continue
						}
						if val, exists := groupRow.Get(cd.Name); exists {
							outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(val)))
						}
					}
				}
				continue
			}
			v, err := e.evalAggregateExpr(col.Expr, groupRows)
			if err != nil {
				return &Result{Error: err}
			}
			outRow = append(outRow, util.UnwrapColumnValue(v))
			_ = ci
		}
		e.aggRowMaps = nil

		// Apply HAVING filter
		if s.Having != nil {
			match, err := e.evalHaving(s.Having, groupRows)
			if err != nil || !match {
				continue
			}
		}

		outRows = append(outRows, outRow)
		if len(groupRows) > 0 {
			outMaps = append(outMaps, groupRows[0])
		}
	}

	if len(outRows) == 0 {
		return e.finalizeSelectResult(&Result{Columns: columns, Rows: [][]interface{}{}}, s, nil)
	}
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: outRows}, s, outMaps)
}

// resolveGroupByOrdinals maps numeric GROUP BY terms (e.g., GROUP BY 2) to
// the corresponding SELECT column expression (1-based ordinal). When the
// ordinal's SELECT column is a bare star (*), the ordinal groups by the first
// output column of the result (SQLite resolves GROUP BY N against result
// columns positionally), so it resolves to a reference to that column.
func resolveGroupByOrdinals(s *sql.SelectStmt, colDefs []sql.ColumnDef) []sql.Expr {
	if len(s.GroupBy) == 0 {
		return nil
	}
	resolved := make([]sql.Expr, len(s.GroupBy))
	for i, g := range s.GroupBy {
		if num, ok := g.(*sql.NumericLit); ok {
			var ord int64
			fmt.Sscanf(num.Value, "%d", &ord)
			if ord >= 1 && int(ord) <= len(s.Columns) {
				col := s.Columns[ord-1]
				if ref, isStar := col.Expr.(*sql.ColumnRef); isStar && ref.Name == "*" && ref.Table == "" && len(colDefs) > 0 {
					// GROUP BY 1 on SELECT * groups by the first result column.
					resolved[i] = &sql.ColumnRef{Name: colDefs[0].Name}
				} else {
					resolved[i] = col.Expr
				}
				continue
			}
		}
		resolved[i] = g
	}
	return resolved
}

// matchGroupByExpr returns the index of the GROUP BY term that matches the
// given output-column expression (same AST node or structurally identical),
// or -1 when no term matches. SQLite reuses the grouping value for output
// columns that are GROUP BY expressions, so non-deterministic functions
// (random()) and formatting stay consistent.
func matchGroupByExpr(groupBy []sql.Expr, col sql.Expr) int {
	for i, g := range groupBy {
		if g == col {
			return i
		}
		if exprStructurallyEqual(g, col) {
			return i
		}
	}
	return -1
}

// exprStructurallyEqual reports whether two expressions have identical
// structure (operator, operand kinds, column names, literals). Pointer
// fields (e.g. subquery ASTs) are compared by structural equality only for
// the leaf kinds used in GROUP BY columns.
func exprStructurallyEqual(a, b sql.Expr) bool {
	if a == nil || b == nil {
		return a == b
	}
	switch x := a.(type) {
	case *sql.ColumnRef:
		y, ok := b.(*sql.ColumnRef)
		return ok && x.Name == y.Name && x.Table == y.Table
	case *sql.NumericLit:
		y, ok := b.(*sql.NumericLit)
		return ok && x.Value == y.Value
	case *sql.StringLit:
		y, ok := b.(*sql.StringLit)
		return ok && x.Value == y.Value
	case *sql.BlobLit:
		y, ok := b.(*sql.BlobLit)
		return ok && string(x.Value) == string(y.Value)
	case *sql.BinaryOp:
		y, ok := b.(*sql.BinaryOp)
		return ok && x.Operator == y.Operator && exprStructurallyEqual(x.Left, y.Left) && exprStructurallyEqual(x.Right, y.Right)
	case *sql.UnaryOp:
		y, ok := b.(*sql.UnaryOp)
		return ok && x.Operator == y.Operator && exprStructurallyEqual(x.Operand, y.Operand)
	case *sql.FuncCall:
		y, ok := b.(*sql.FuncCall)
		if !ok || !strings.EqualFold(x.Name, y.Name) || len(x.Args) != len(y.Args) {
			return false
		}
		for i := range x.Args {
			if !exprStructurallyEqual(x.Args[i], y.Args[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// computeGroupByKeyValues evaluates each GROUP BY expression for a row,
// returning a serialized string key and the raw evaluated values (used to
// sort the output groups, matching SQLite's key-order GROUP BY output).
func (e *Engine) computeGroupByKeyValues(groupBy []sql.Expr, row Row) (string, []interface{}) {
	parts := make([]string, len(groupBy))
	values := make([]interface{}, len(groupBy))
	for i, expr := range groupBy {
		v, err := e.evalExpr(expr, row)
		if err != nil || v == nil {
			parts[i] = "\x00"
			values[i] = nil
		} else {
			uv := util.UnwrapColumnValue(v)
			parts[i] = fmt.Sprintf("%v", uv)
			values[i] = uv
		}
	}
	return strings.Join(parts, "\x00"), values
}

// evalHaving evaluates a HAVING expression by treating aggregate function
// calls as group-aware (evaluating over all rows in the group).
func (e *Engine) evalHaving(expr sql.Expr, groupRows []RowMap) (bool, error) {
	v, err := e.evalHavingExpr(expr, groupRows)
	if err != nil {
		return false, err
	}
	return toBool(v), nil
}

// evalHavingExpr recursively evaluates an expression, handling aggregate
// functions across all groupRows.
func (e *Engine) evalHavingExpr(expr sql.Expr, groupRows []RowMap) (interface{}, error) {
	if expr == nil {
		return nil, nil
	}
	switch v := expr.(type) {
	case *sql.FuncCall:
		return e.evalHavingFuncCall(v, groupRows)
	case *sql.BinaryOp:
		left, err := e.evalHavingExpr(v.Left, groupRows)
		if err != nil {
			return nil, err
		}
		right, err := e.evalHavingExpr(v.Right, groupRows)
		if err != nil {
			return nil, err
		}
		// NULL propagation for non-AND/OR ops
		if v.Operator != "AND" && v.Operator != "OR" {
			if left == nil || right == nil {
				return nil, nil
			}
		}
		return e.evalBinaryOpValues(v.Operator, left, right)
	case *sql.UnaryOp:
		return e.evalHavingUnary(v, groupRows)
	case *sql.IsNull:
		operand, err := e.evalHavingExpr(v.Operand, groupRows)
		if err != nil {
			return nil, err
		}
		operand = util.UnwrapColumnValue(operand)
		return boolToInt(operand == nil), nil
	case *sql.IsNotNull:
		return e.evalHavingIsNotNull(v, groupRows)
	case *sql.IsDistinctFrom:
		return e.evalHavingIsDistinctFrom(v, groupRows)
	case *sql.IsNotDistinctFrom:
		return e.evalHavingIsNotDistinctFrom(v, groupRows)
	case *sql.Subquery:
		return e.evalHavingSubquery(v, groupRows)
	default:
		return e.evalHavingDefault(expr, groupRows)
	}
}
func (e *Engine) evalHavingFuncCall(v *sql.FuncCall, groupRows []RowMap) (interface{}, error) {
	fn, ok := e.funcs.Find(v.Name)
	if ok && fn.Type == function.TypeAggregate {
		if v.Distinct {
			return e.evalDistinctAggregate(v, groupRows), nil
		}
		return e.evalAggFuncCall(v, groupRows)
	}
	if len(groupRows) > 0 {
		return e.evalFuncCall(v, groupRows[0])
	}
	return nil, nil
}

func (e *Engine) evalHavingUnary(v *sql.UnaryOp, groupRows []RowMap) (interface{}, error) {
	operand, err := e.evalHavingExpr(v.Operand, groupRows)
	if err != nil {
		return nil, err
	}
	switch v.Operator {
	case "NOT":
		if operand == nil {
			return nil, nil
		}
		return boolToInt(!toBool(operand)), nil
	case "-":
		return negateValue(operand)
	default:
		return nil, nil
	}
}

func (e *Engine) evalHavingIsNotNull(v *sql.IsNotNull, groupRows []RowMap) (interface{}, error) {
	operand, err := e.evalHavingExpr(v.Operand, groupRows)
	if err != nil {
		return nil, err
	}
	operand = util.UnwrapColumnValue(operand)
	return boolToInt(operand != nil), nil
}

func (e *Engine) evalHavingIsDistinctFrom(v *sql.IsDistinctFrom, groupRows []RowMap) (interface{}, error) {
	left, err := e.evalHavingExpr(v.Left, groupRows)
	if err != nil {
		return nil, err
	}
	right, err := e.evalHavingExpr(v.Right, groupRows)
	if err != nil {
		return nil, err
	}
	if left == nil && right == nil {
		return int64(0), nil
	}
	if left == nil || right == nil {
		return int64(1), nil
	}
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(0), nil
	}
	return int64(1), nil
}

func (e *Engine) evalHavingIsNotDistinctFrom(v *sql.IsNotDistinctFrom, groupRows []RowMap) (interface{}, error) {
	left, err := e.evalHavingExpr(v.Left, groupRows)
	if err != nil {
		return nil, err
	}
	right, err := e.evalHavingExpr(v.Right, groupRows)
	if err != nil {
		return nil, err
	}
	if left == nil && right == nil {
		return int64(1), nil
	}
	if left == nil || right == nil {
		return int64(0), nil
	}
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *Engine) evalHavingDefault(expr sql.Expr, groupRows []RowMap) (interface{}, error) {
	if len(groupRows) > 0 {
		return e.evalExpr(expr, groupRows[0])
	}
	return nil, nil
}

// evalHavingSubquery evaluates a Subquery expression in a HAVING clause.
// It sets outerRows to all group rows so that correlated aggregates within
// the subquery can evaluate over the entire group (not just one row).
func (e *Engine) evalHavingSubquery(v *sql.Subquery, groupRows []RowMap) (interface{}, error) {
	prevOuterRows := e.outerRows
	if len(groupRows) > 0 {
		e.outerRows = groupRows
	}
	result, err := e.evalSubquery(v, groupRows[0])
	e.outerRows = prevOuterRows
	return result, err
}

func (e *Engine) evalAggregateExpr(expr sql.Expr, rowMaps []RowMap) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if v.Distinct {
			return e.evalDistinctAggregate(v, rowMaps), nil
		}
		return e.evalAggFuncCall(v, rowMaps)
	default:
		if len(rowMaps) > 0 {
			val, err := e.evalExpr(expr, rowMaps[0])
			return val, err
		}
		return nil, nil
	}
}

func (e *Engine) evalAggFuncCall(v *sql.FuncCall, rowMaps []RowMap) (interface{}, error) {
	fn, ok := e.funcs.Find(v.Name)
	if !ok || fn.Type != function.TypeAggregate {
		if len(rowMaps) > 0 {
			val, _ := e.evalExpr(v, rowMaps[0])
			return val, nil
		}
		return nil, nil
	}

	// Check for nested aggregate functions (SQLite prohibits this)
	for _, arg := range v.Args {
		if nested := findNestedAggregate(arg, e.funcs); nested != "" {
			return nil, fmt.Errorf("misuse of aggregate function %s()", nested)
		}
	}

	// Check ORDER BY expressions for nested aggregates
	for _, ob := range v.OrderBy {
		if nested := findNestedAggregate(ob.Expr, e.funcs); nested != "" {
			return nil, fmt.Errorf("misuse of aggregate function %s()", nested)
		}
	}

	agg := fn.AggregateFn()

	// Sort rowMaps by ORDER BY terms if specified (for ordered aggregates like group_concat)
	rows := rowMaps
	if len(v.OrderBy) > 0 && len(rowMaps) > 1 {
		rows = make([]RowMap, len(rowMaps))
		copy(rows, rowMaps)
		sort.SliceStable(rows, func(i, j int) bool {
			for _, ob := range v.OrderBy {
				coll := orderByTermCollation(ob.Expr)
				obExpr := stripCollate(ob.Expr)
				vi, errI := e.evalExpr(obExpr, rows[i])
				vj, errJ := e.evalExpr(obExpr, rows[j])
				if errI != nil || errJ != nil {
					continue
				}
				cmp := e.compareValuesCollate(vi, vj, coll)
				if cmp != 0 {
					if ob.Desc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
	}

	for _, row := range rows {
		// Apply FILTER (WHERE clause) if present — skip rows that don't match
		if v.Filter != nil {
			filterVal, err := e.evalExpr(v.Filter, row)
			if err != nil || !toBool(filterVal) {
				continue
			}
		}
		args := make([]interface{}, len(v.Args))
		for i, arg := range v.Args {
			val, err := e.evalExpr(arg, row)
			if err != nil {
				args[i] = nil
			} else {
				args[i] = util.UnwrapColumnValue(val)
			}
		}
		if err := agg.Step(args); err != nil {
			return nil, err
		}
	}
	result, _ := agg.Final()
	return result, nil
}

// findNestedAggregate checks if an expression tree contains an aggregate function call
// and returns its name. It does NOT descend into subqueries, since subqueries have
// their own evaluation context. Returns "" if no nested aggregate is found.
func findNestedAggregate(expr sql.Expr, funcs *function.Registry) string {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return findNestedAggregateFuncCall(v, funcs)
	case *sql.BinaryOp:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.UnaryOp:
		return findNestedAggregate(v.Operand, funcs)
	case *sql.IsNull:
		return findNestedAggregate(v.Operand, funcs)
	case *sql.IsNotNull:
		return findNestedAggregate(v.Operand, funcs)
	case *sql.IsDistinctFrom:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.IsNotDistinctFrom:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.Between:
		return findNestedAggregateBetween(v, funcs)
	case *sql.InList:
		return findNestedAggregateInList(v, funcs)
	case *sql.CaseExpr:
		return findNestedAggregateCaseExpr(v, funcs)
	case *sql.CastExpr:
		return findNestedAggregate(v.Operand, funcs)
	case *sql.RowValue:
		return findNestedAggregateRowValue(v, funcs)
	case *sql.Subquery, *sql.ExistsExpr:
		return ""
	default:
		return ""
	}
}

func findNestedAggregateFuncCall(v *sql.FuncCall, funcs *function.Registry) string {
	if fn, ok := funcs.Find(v.Name); ok && fn.Type == function.TypeAggregate {
		return v.Name
	}
	for _, arg := range v.Args {
		if nested := findNestedAggregate(arg, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

func findNestedAggregateBinary(left, right sql.Expr, funcs *function.Registry) string {
	if nested := findNestedAggregate(left, funcs); nested != "" {
		return nested
	}
	return findNestedAggregate(right, funcs)
}

func findNestedAggregateBetween(v *sql.Between, funcs *function.Registry) string {
	if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
		return nested
	}
	if nested := findNestedAggregate(v.Low, funcs); nested != "" {
		return nested
	}
	return findNestedAggregate(v.High, funcs)
}

func findNestedAggregateInList(v *sql.InList, funcs *function.Registry) string {
	if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
		return nested
	}
	for _, item := range v.List {
		if nested := findNestedAggregate(item, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

func findNestedAggregateCaseExpr(v *sql.CaseExpr, funcs *function.Registry) string {
	if v.Operand != nil {
		if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
			return nested
		}
	}
	for _, w := range v.Whens {
		if nested := findNestedAggregate(w.When, funcs); nested != "" {
			return nested
		}
		if nested := findNestedAggregate(w.Then, funcs); nested != "" {
			return nested
		}
	}
	if v.Else != nil {
		return findNestedAggregate(v.Else, funcs)
	}
	return ""
}

func findNestedAggregateRowValue(v *sql.RowValue, funcs *function.Registry) string {
	for _, val := range v.Values {
		if nested := findNestedAggregate(val, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

// validateSelectExprs checks for invalid usage in SELECT expressions, such as
// ORDER BY with non-aggregate functions or aggregates inside UNION ALL in subqueries.
// validateRowValueUse checks an expression tree for illegal row-value usage
// and returns "row value misused" when found. A row value (a,b,c) is legal:
//   - as both operands of a comparison (a,b) = (1,2)
//   - as the operand or an element of IN
//   - on one side of a comparison when the other side is a subquery
//     returning the same column count (validated at execution)
//
// It is illegal in every other scalar context: bare SELECT list entries,
// LIMIT/OFFSET, arithmetic operands, function arguments, and comparisons
// against a scalar. topLevel indicates a SELECT column expression (a bare
// row value there is an error).
func (e *Engine) validateRowValueUse(expr sql.Expr, topLevel bool) error {
	if expr == nil {
		return nil
	}
	switch v := expr.(type) {
	case *sql.RowValue:
		if topLevel {
			return fmt.Errorf("row value misused")
		}
		// A row value nested under a scalar-producing parent is validated by
		// the parent's case; inside IN/comparison it is allowed. We cannot
		// decide legality here without parent context, so recurse into
		// elements and rely on the parent cases for the misuse decision.
		for _, sub := range v.Values {
			if err := e.validateRowValueUse(sub, false); err != nil {
				return err
			}
		}
		return nil
	case *sql.BinaryOp:
		// An explicit COLLATE clause (a COLLATE nose) — validate the
		// collation name at prepare time like SQLite.
		if strings.EqualFold(v.Operator, "COLLATE") {
			if name := getCollationName(v.Right); name != "" {
				if err := e.checkCollationString(name); err != nil {
					return err
				}
			}
		}
		leftIsRow := isRowValueExpr(v.Left)
		rightIsRow := isRowValueExpr(v.Right)
		leftIsSub := isSubqueryExpr(v.Left)
		rightIsSub := isSubqueryExpr(v.Right)
		if leftIsRow != rightIsRow {
			// A row value compared against a scalar (or scalar vs row) is an
			// error unless the row side is a subquery (validated at runtime
			// for column count).
			if leftIsRow && !rightIsSub {
				return fmt.Errorf("row value misused")
			}
			if rightIsRow && !leftIsSub {
				return fmt.Errorf("row value misused")
			}
		}
		// A scalar compared against a multi-column subquery is an error:
		// `c == (SELECT x, y FROM ...)` returns 2 columns — the subquery's
		// column count must match the other side (1 for a scalar). This is
		// validated at runtime in evalSubqueryRows; the misuse surfaces when
		// executing the comparison, which rowPassesWhere swallows. Surface it
		// here by validating the subquery's column count when the other side
		// is a plain scalar (not a row value and not a subquery).
		if !leftIsRow && !leftIsSub && rightIsSub {
			if err := e.validateSubqueryArity(v.Right, 1); err != nil {
				// SQLite reports "row value misused" for a scalar compared
				// against a multi-column subquery.
				return fmt.Errorf("row value misused")
			}
		}
		if !rightIsRow && !rightIsSub && leftIsSub {
			if err := e.validateSubqueryArity(v.Left, 1); err != nil {
				return fmt.Errorf("row value misused")
			}
		}
		if err := e.validateRowValueUse(v.Left, false); err != nil {
			return err
		}
		return e.validateRowValueUse(v.Right, false)
	case *sql.UnaryOp:
		// NOT (b = (1, 2)) — descend into unary operands (NOT, unary minus).
		return e.validateRowValueUse(v.Operand, false)
	case *sql.ParenExpr:
		return e.validateRowValueUse(v.Expr, topLevel)
	case *sql.InList:
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
	case *sql.Subquery:
		return nil
	case *sql.FuncCall:
		// A row value as a function argument is "row value misused".
		for _, arg := range v.Args {
			if isRowValueExpr(arg) {
				return fmt.Errorf("row value misused")
			}
		}
		return nil
	case *sql.CaseExpr:
		if err := e.validateRowValueUse(v.Operand, false); err != nil {
			return err
		}
		for _, w := range v.Whens {
			if err := e.validateRowValueUse(w.When, false); err != nil {
				return err
			}
			if err := e.validateRowValueUse(w.Then, false); err != nil {
				return err
			}
		}
		return e.validateRowValueUse(v.Else, false)
	default:
		return nil
	}
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
func (e *Engine) validateSubqueryArity(expr sql.Expr, wantCols int) error {
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
func (e *Engine) subqueryColumnCount(s *sql.SelectStmt) int {
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
	entry, _, err := e.findTable(s.From.Name)
	if err != nil {
		return len(s.Columns)
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
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
func (e *Engine) resolveAliasRef(name string) (sql.Expr, bool) {
	for i := len(e.aliasStack) - 1; i >= 0; i-- {
		if expr, ok := e.aliasStack[i][strings.ToLower(name)]; ok {
			return expr, true
		}
	}
	return nil, false
}

func (e *Engine) validateSelectColumnRefs(s *sql.SelectStmt, colDefs []sql.ColumnDef, tableName, fromAlias string) error {
	colByName := make(map[string]bool, len(colDefs))
	for _, cd := range colDefs {
		colByName[strings.ToLower(cd.Name)] = true
	}
	checkRef := func(ref *sql.ColumnRef) error {
		// Qualified references must name this table (or its FROM alias, or be
		// rowid aliases).
		if ref.Table != "" {
			q := strings.ToLower(ref.Table)
			// NEW.col / OLD.col references are valid inside trigger bodies when
			// the named column actually exists in the trigger's row (e.g.
			// INSTEAD OF INSERT ON v ... new.k); an unknown column is still an
			// error, exactly as SQLite reports "no such column: new.x" for a
			// trigger on a table without column x.
			if q == "new" && e.triggerNewRow != nil {
				if _, ok := e.triggerNewRow.Get(ref.Name); ok {
					return nil
				}
			}
			if q == "old" && e.triggerOldRow != nil {
				if _, ok := e.triggerOldRow.Get(ref.Name); ok {
					return nil
				}
			}
			// Strip a schema prefix (main./temp./aux.) for comparison.
			if dot := strings.LastIndex(q, "."); dot >= 0 {
				q = q[dot+1:]
			}
			tn := strings.ToLower(tableName)
			alias := strings.ToLower(fromAlias)
			if q != tn && q != alias {
				// This validator only runs for single-table queries (no JOINs),
				// so a qualifier naming any other table is an unresolvable
				// column reference, exactly like SQLite's resolver:
				// SELECT 9 IN (false.false) FROM t8 → "no such column:
				// false.false".
				return fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)
			}
		}
		if ref.Name == "*" {
			return nil
		}
		// TRUE/FALSE are boolean literals, not column references
		// (the parser represents them as unqualified ColumnRefs).
		if strings.EqualFold(ref.Name, "TRUE") || strings.EqualFold(ref.Name, "FALSE") {
			return nil
		}
		if colByName[strings.ToLower(ref.Name)] {
			return nil
		}
		if isRowIDName(ref.Name) {
			return nil
		}
		// Double-quoted identifiers fall back to string literals when DQS is
		// enabled (handled at evaluation); do not reject them here.
		if ref.Quoted {
			return nil
		}
		return fmt.Errorf("no such column: %s", ref.Name)
	}
	var checkErr error
	walk := func(expr sql.Expr) {
		if checkErr != nil || expr == nil {
			return
		}
		walkExprFull(expr, func(e sql.Expr) {
			if checkErr != nil {
				return
			}
			if ref, ok := e.(*sql.ColumnRef); ok {
				checkErr = checkRef(ref)
			}
		})
	}
	for _, col := range s.Columns {
		walk(col.Expr)
	}
	// Aliases (e.g. "SELECT a AS x ... WHERE x=1") are usable in WHERE too.
	aliasNames := make(map[string]bool)
	for _, col := range s.Columns {
		if col.As != "" {
			aliasNames[strings.ToLower(col.As)] = true
		}
	}
	walkAliasAware := func(expr sql.Expr) {
		if expr == nil || checkErr != nil {
			return
		}
		walkExprFull(expr, func(e2 sql.Expr) {
			if checkErr != nil {
				return
			}
			if ref, ok := e2.(*sql.ColumnRef); ok && aliasNames[strings.ToLower(ref.Name)] {
				return
			}
			// An unqualified reference may name an output-column alias of an
			// ENCLOSING SELECT (e.g. an inner subquery's WHERE referencing the
			// outer query's "SELECT expr AS aaa"); such references resolve
			// through the alias stack at evaluation time, so skip them here.
			if ref, ok := e2.(*sql.ColumnRef); ok && ref.Table == "" {
				if _, found := e.resolveAliasRef(ref.Name); found {
					return
				}
			}
			if ref, ok := e2.(*sql.ColumnRef); ok {
				checkErr = checkRef(ref)
			}
		})
	}
	walkAliasAware(s.Where)
	// ORDER BY, GROUP BY, and HAVING terms may reference result-column aliases
	// (e.g. GROUP BY x / ORDER BY x for "SELECT a AS x"), which are not table
	// columns; skip those. The walk below filters alias refs (including inside
	// expressions like 10-(x+y)).
	for _, g := range s.GroupBy {
		walkAliasAware(g)
	}
	walkAliasAware(s.Having)
	if !e.inCompoundMember {
		for _, ob := range s.OrderBy {
			walkAliasAware(ob.Expr)
		}
	}
	return checkErr
}

func (e *Engine) validateSelectExprs(s *sql.SelectStmt) error {
	// SQLite: an aggregate function in ORDER BY is only allowed when the
	// SELECT is an aggregate query (has GROUP BY or an aggregate in the
	// SELECT list). Otherwise it is a "misuse of aggregate" error.
	// Compound queries skip this: a trailing ORDER BY on a compound member
	// is the compound-level ORDER BY, where aggregates are permitted.
	if len(s.OrderBy) > 0 && s.GroupBy == nil && !e.inCompoundMember && s.Union == nil {
		isAgg := e.hasAggregates(s.Columns)
		for _, ob := range s.OrderBy {
			if e.exprHasAggregate(ob.Expr) && !isAgg {
				name := e.aggregateName(ob.Expr)
				return fmt.Errorf("misuse of aggregate: %s()", name)
			}
		}
	}
	for _, col := range s.Columns {
		if err := e.validateExprOrderBy(col.Expr); err != nil {
			return err
		}
		// SQLite: ORDER BY clauses are limited to SQLITE_MAX_ORDER_BY_LENGTH
		// (default 1000) terms; function-call ORDER BY (e.g. group_concat(w
		// ORDER BY a,b,...)) counts against the same limit.
		if err := validateOrderByLength(col.Expr, 1000); err != nil {
			return err
		}
		// Check column expressions for subqueries with UNION ALL aggregates
		if err := e.validateExprSubqueries(col.Expr); err != nil {
			return err
		}
		// SQLite: DISTINCT aggregates must have exactly one argument
		if err := validateDistinctAggArgs(col.Expr); err != nil {
			return err
		}
	}
	if s.Having != nil {
		if err := e.validateExprOrderBy(s.Having); err != nil {
			return err
		}
		if err := e.validateExprSubqueries(s.Having); err != nil {
			return err
		}
		if err := validateDistinctAggArgs(s.Having); err != nil {
			return err
		}
	}
	if s.Where != nil {
		if err := e.validateExprSubqueries(s.Where); err != nil {
			return err
		}
		if err := validateDistinctAggArgs(s.Where); err != nil {
			return err
		}
	}
	for _, ob := range s.OrderBy {
		if err := validateDistinctAggArgs(ob.Expr); err != nil {
			return err
		}
	}

	// Row-value misuse validation (SQLite raises "row value misused" at
	// prepare time): a row value is only legal as both sides of a
	// comparison, as an IN operand/list element, or as a subquery result
	// compared to a row value. Bare row values in the SELECT list, LIMIT,
	// arithmetic, function arguments, etc. are errors.
	for _, col := range s.Columns {
		if err := e.validateRowValueUse(col.Expr, true); err != nil {
			return err
		}
	}
	if s.Where != nil {
		if err := e.validateRowValueUse(s.Where, false); err != nil {
			return err
		}
	}
	if s.Having != nil {
		if err := e.validateRowValueUse(s.Having, false); err != nil {
			return err
		}
	}
	if s.Limit != nil {
		if err := e.validateRowValueUse(s.Limit, false); err != nil {
			return err
		}
	}
	if s.Offset != nil {
		if err := e.validateRowValueUse(s.Offset, false); err != nil {
			return err
		}
	}
	for _, ob := range s.OrderBy {
		if err := e.validateRowValueUse(ob.Expr, false); err != nil {
			return err
		}
	}

	// Check for aggregates inside UNION ALL in FROM subquery (SQLite rule)
	if s.From.Subquery != nil {
		if err := validateUnionSubqueryNoAggs(s.From.Subquery); err != nil {
			return err
		}
	}

	// Check for aggregates in ORDER BY when SELECT doesn't use aggregates and no GROUP BY
	if len(s.OrderBy) > 0 && len(s.GroupBy) == 0 && !e.inCompoundMember && !e.hasAggregates(s.Columns) {
		for _, ob := range s.OrderBy {
			if nested := findAggregateInExpr(ob.Expr); nested != "" {
				return fmt.Errorf("misuse of aggregate: %s()", nested)
			}
		}
	}

	return nil
}

// validateOrderByLength walks an expression tree and returns SQLite's
// "too many terms in ORDER BY clause" error when any function call carries
// more than limit ORDER BY terms.
func validateOrderByLength(expr sql.Expr, limit int) error {
	if expr == nil {
		return nil
	}
	if fn, ok := expr.(*sql.FuncCall); ok {
		if len(fn.OrderBy) > limit {
			return fmt.Errorf("too many terms in ORDER BY clause")
		}
		for _, a := range fn.Args {
			if err := validateOrderByLength(a, limit); err != nil {
				return err
			}
		}
	}
	switch v := expr.(type) {
	case *sql.BinaryOp:
		if err := validateOrderByLength(v.Left, limit); err != nil {
			return err
		}
		return validateOrderByLength(v.Right, limit)
	case *sql.UnaryOp:
		return validateOrderByLength(v.Operand, limit)
	case *sql.IsNull:
		return validateOrderByLength(v.Operand, limit)
	case *sql.IsNotNull:
		return validateOrderByLength(v.Operand, limit)
	case *sql.Between:
		if err := validateOrderByLength(v.Operand, limit); err != nil {
			return err
		}
		if err := validateOrderByLength(v.Low, limit); err != nil {
			return err
		}
		return validateOrderByLength(v.High, limit)
	case *sql.InList:
		if err := validateOrderByLength(v.Operand, limit); err != nil {
			return err
		}
		for _, item := range v.List {
			if err := validateOrderByLength(item, limit); err != nil {
				return err
			}
		}
	case *sql.CaseExpr:
		if err := validateOrderByLength(v.Operand, limit); err != nil {
			return err
		}
		for _, w := range v.Whens {
			if err := validateOrderByLength(w.When, limit); err != nil {
				return err
			}
			if err := validateOrderByLength(w.Then, limit); err != nil {
				return err
			}
		}
		return validateOrderByLength(v.Else, limit)
	}
	return nil
}

// validateDistinctAggArgs walks an expression tree and reports an error for
// any aggregate function used with DISTINCT but no arguments (SQLite:
// "DISTINCT aggregates must have exactly one argument"). Subqueries are not
// descended into — they have their own validation.
func validateDistinctAggArgs(expr sql.Expr) error {
	if expr == nil {
		return nil
	}
	if fn, ok := expr.(*sql.FuncCall); ok {
		if fn.Distinct && len(fn.Args) != 1 {
			return fmt.Errorf("DISTINCT aggregates must have exactly one argument")
		}
		for _, a := range fn.Args {
			if err := validateDistinctAggArgs(a); err != nil {
				return err
			}
		}
	}
	switch v := expr.(type) {
	case *sql.BinaryOp:
		if err := validateDistinctAggArgs(v.Left); err != nil {
			return err
		}
		return validateDistinctAggArgs(v.Right)
	case *sql.UnaryOp:
		return validateDistinctAggArgs(v.Operand)
	case *sql.IsNull:
		return validateDistinctAggArgs(v.Operand)
	case *sql.IsNotNull:
		return validateDistinctAggArgs(v.Operand)
	case *sql.IsDistinctFrom:
		if err := validateDistinctAggArgs(v.Left); err != nil {
			return err
		}
		return validateDistinctAggArgs(v.Right)
	case *sql.IsNotDistinctFrom:
		if err := validateDistinctAggArgs(v.Left); err != nil {
			return err
		}
		return validateDistinctAggArgs(v.Right)
	case *sql.Between:
		if err := validateDistinctAggArgs(v.Operand); err != nil {
			return err
		}
		if err := validateDistinctAggArgs(v.Low); err != nil {
			return err
		}
		return validateDistinctAggArgs(v.High)
	case *sql.InList:
		if err := validateDistinctAggArgs(v.Operand); err != nil {
			return err
		}
		for _, item := range v.List {
			if err := validateDistinctAggArgs(item); err != nil {
				return err
			}
		}
	case *sql.CaseExpr:
		if err := validateDistinctAggArgs(v.Operand); err != nil {
			return err
		}
		for _, w := range v.Whens {
			if err := validateDistinctAggArgs(w.When); err != nil {
				return err
			}
			if err := validateDistinctAggArgs(w.Then); err != nil {
				return err
			}
		}
		return validateDistinctAggArgs(v.Else)
	}
	return nil
}

// subqueryOuterAggRef returns the name of an aggregate function in the given
// SELECT that references a column outside the subquery's own FROM tables
// (a correlated/outer reference), or "" if none. SQLite rejects such
// aggregates in IN-subquery contexts with "misuse of aggregate".
func (e *Engine) subqueryOuterAggRef(s *sql.SelectStmt) string {
	if s == nil {
		return ""
	}
	// Collect column names available in the subquery's FROM tables.
	inner := make(map[string]bool)
	innerTables := make(map[string]bool) // lower-cased table names + aliases
	addTable := func(name string) {
		if name == "" {
			return
		}
		if cols, err := e.tableColumnNames(name); err == nil {
			for _, c := range cols {
				inner[strings.ToLower(c)] = true
			}
		}
	}
	if s.From.Name != "" {
		addTable(s.From.Name)
		innerTables[strings.ToLower(s.From.Name)] = true
		if s.From.As != "" {
			innerTables[strings.ToLower(s.From.As)] = true
		}
	}
	for _, j := range s.Joins {
		if j.Table.Name != "" {
			addTable(j.Table.Name)
			innerTables[strings.ToLower(j.Table.Name)] = true
			if j.Table.As != "" {
				innerTables[strings.ToLower(j.Table.As)] = true
			}
		}
	}
	for _, col := range s.Columns {
		if name := e.aggRefsOuter(col.Expr, inner, innerTables); name != "" {
			return name
		}
	}
	for _, ob := range s.OrderBy {
		if name := e.aggRefsOuter(ob.Expr, inner, innerTables); name != "" {
			return name
		}
	}
	return ""
}

// aggRefsOuter walks an expression for an aggregate function whose argument
// references a column not present in the inner column set. Returns the
// aggregate name or "".
func (e *Engine) aggRefsOuter(expr sql.Expr, inner map[string]bool, innerTables map[string]bool) string {
	if expr == nil {
		return ""
	}
	if fn, ok := expr.(*sql.FuncCall); ok {
		if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
			for _, a := range fn.Args {
				if e.exprRefsOuterCol(a, inner, innerTables) {
					return fn.Name
				}
			}
		}
		for _, a := range fn.Args {
			if name := e.aggRefsOuter(a, inner, innerTables); name != "" {
				return name
			}
		}
	}
	switch v := expr.(type) {
	case *sql.BinaryOp:
		if name := e.aggRefsOuter(v.Left, inner, innerTables); name != "" {
			return name
		}
		return e.aggRefsOuter(v.Right, inner, innerTables)
	case *sql.UnaryOp:
		return e.aggRefsOuter(v.Operand, inner, innerTables)
	case *sql.IsNull:
		return e.aggRefsOuter(v.Operand, inner, innerTables)
	case *sql.IsNotNull:
		return e.aggRefsOuter(v.Operand, inner, innerTables)
	case *sql.Between:
		if name := e.aggRefsOuter(v.Operand, inner, innerTables); name != "" {
			return name
		}
		if name := e.aggRefsOuter(v.Low, inner, innerTables); name != "" {
			return name
		}
		return e.aggRefsOuter(v.High, inner, innerTables)
	case *sql.CaseExpr:
		if name := e.aggRefsOuter(v.Operand, inner, innerTables); name != "" {
			return name
		}
		for _, w := range v.Whens {
			if name := e.aggRefsOuter(w.When, inner, innerTables); name != "" {
				return name
			}
			if name := e.aggRefsOuter(w.Then, inner, innerTables); name != "" {
				return name
			}
		}
		return e.aggRefsOuter(v.Else, inner, innerTables)
	}
	return ""
}

// exprRefsOuterCol reports whether an expression references a column outside
// the subquery's own FROM tables (ignoring subqueries). A qualified reference
// (t.col) is outer when t is not one of the subquery's tables; an unqualified
// reference is outer when the name is not an inner column.
func (e *Engine) exprRefsOuterCol(expr sql.Expr, inner map[string]bool, innerTables map[string]bool) bool {
	switch v := expr.(type) {
	case *sql.ColumnRef:
		if v.Name == "*" {
			return false
		}
		if v.Table != "" {
			t := strings.ToLower(v.Table)
			if dot := strings.Index(t, "."); dot >= 0 {
				t = t[dot+1:]
			}
			return !innerTables[t]
		}
		return !inner[strings.ToLower(v.Name)]
	case *sql.BinaryOp:
		return e.exprRefsOuterCol(v.Left, inner, innerTables) || e.exprRefsOuterCol(v.Right, inner, innerTables)
	case *sql.UnaryOp:
		return e.exprRefsOuterCol(v.Operand, inner, innerTables)
	case *sql.IsNull:
		return e.exprRefsOuterCol(v.Operand, inner, innerTables)
	case *sql.IsNotNull:
		return e.exprRefsOuterCol(v.Operand, inner, innerTables)
	case *sql.Between:
		return e.exprRefsOuterCol(v.Operand, inner, innerTables) || e.exprRefsOuterCol(v.Low, inner, innerTables) || e.exprRefsOuterCol(v.High, inner, innerTables)
	case *sql.CaseExpr:
		if e.exprRefsOuterCol(v.Operand, inner, innerTables) {
			return true
		}
		for _, w := range v.Whens {
			if e.exprRefsOuterCol(w.When, inner, innerTables) || e.exprRefsOuterCol(w.Then, inner, innerTables) {
				return true
			}
		}
		return e.exprRefsOuterCol(v.Else, inner, innerTables)
	case *sql.FuncCall:
		for _, a := range v.Args {
			if e.exprRefsOuterCol(a, inner, innerTables) {
				return true
			}
		}
	}
	return false
}

// validateExprSubqueries walks an expression tree looking for subqueries and
// checking them for invalid patterns like aggregates inside UNION ALL. A
// scalar subquery must return exactly one column (SQLite: "sub-select returns
// N columns - expected 1"); subqueries used in row-value comparisons or IN
// lists may return multiple columns, so those contexts pass rowValueOK=true.
func (e *Engine) validateExprSubqueries(expr sql.Expr) error {
	return e.validateExprSubqueriesCtx(expr, false)
}

func (e *Engine) validateExprSubqueriesCtx(expr sql.Expr, rowValueOK bool) error {
	switch v := expr.(type) {
	case *sql.Subquery:
		if v.Select != nil {
			// A scalar subquery must return exactly one column. Row-value
			// contexts (the subquery is compared against a row value or used
			// as an IN-list operand) allow multiple columns.
			if !rowValueOK {
				if n := e.subqueryColumnCount(v.Select); n > 1 {
					return fmt.Errorf("sub-select returns %d columns - expected 1", n)
				}
			}
			// Validate the subquery's SELECT statement
			if err := e.validateSelectExprs(v.Select); err != nil {
				return err
			}
		}
	case *sql.ExistsExpr:
		if v.Select != nil {
			if err := e.validateSelectExprs(v.Select); err != nil {
				return err
			}
		}
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if err := e.validateExprSubqueriesCtx(arg, false); err != nil {
				return err
			}
		}
	case *sql.BinaryOp:
		// A comparison between a row value and a subquery treats the
		// subquery as a row value: (a,b) = (SELECT x,y) is legal. Detect
		// that pattern and allow the subquery side multiple columns.
		_, leftRow := v.Left.(*sql.RowValue)
		_, rightRow := v.Right.(*sql.RowValue)
		_, leftSub := v.Left.(*sql.Subquery)
		_, rightSub := v.Right.(*sql.Subquery)
		if leftSub && rightRow {
			if err := e.validateExprSubqueriesCtx(v.Left, true); err != nil {
				return err
			}
			return e.validateExprSubqueriesCtx(v.Right, false)
		}
		if rightSub && leftRow {
			if err := e.validateExprSubqueriesCtx(v.Left, false); err != nil {
				return err
			}
			return e.validateExprSubqueriesCtx(v.Right, true)
		}
		if err := e.validateExprSubqueriesCtx(v.Left, false); err != nil {
			return err
		}
		return e.validateExprSubqueriesCtx(v.Right, false)
	case *sql.UnaryOp:
		return e.validateExprSubqueriesCtx(v.Operand, false)
	case *sql.CaseExpr:
		if v.Operand != nil {
			if err := e.validateExprSubqueriesCtx(v.Operand, false); err != nil {
				return err
			}
		}
		for _, w := range v.Whens {
			if err := e.validateExprSubqueriesCtx(w.When, false); err != nil {
				return err
			}
			if err := e.validateExprSubqueriesCtx(w.Then, false); err != nil {
				return err
			}
		}
		if v.Else != nil {
			return e.validateExprSubqueriesCtx(v.Else, false)
		}
	case *sql.Between:
		if err := e.validateExprSubqueriesCtx(v.Operand, false); err != nil {
			return err
		}
		if err := e.validateExprSubqueriesCtx(v.Low, false); err != nil {
			return err
		}
		return e.validateExprSubqueriesCtx(v.High, false)
	case *sql.InList:
		if err := e.validateExprSubqueriesCtx(v.Operand, false); err != nil {
			return err
		}
		_, opIsRow := v.Operand.(*sql.RowValue)
		for _, val := range v.List {
			// IN-list subquery items may return multiple columns (row-value
			// comparisons); a scalar operand with a multi-column subquery is
			// an error (SQLite: "sub-select returns N columns - expected 1").
			if subq, ok := val.(*sql.Subquery); ok && subq.Select != nil {
				if !opIsRow {
					if n := e.subqueryColumnCount(subq.Select); n > 1 {
						return fmt.Errorf("sub-select returns %d columns - expected 1", n)
					}
				}
				if err := e.validateSelectExprs(subq.Select); err != nil {
					return err
				}
				// SQLite: an aggregate in an IN-subquery that references a column
				// outside the subquery's own FROM (an outer/correlated reference)
				// is a "misuse of aggregate" error (e.g. x IN (SELECT count(t.x)
				// FROM other)). The correlated aggregate cannot be evaluated per
				// row in the IN context.
				if name := e.subqueryOuterAggRef(subq.Select); name != "" {
					return fmt.Errorf("misuse of aggregate: %s()", name)
				}
				continue
			}
			if err := e.validateExprSubqueriesCtx(val, false); err != nil {
				return err
			}
		}
		return nil
	case *sql.IsNull:
		return e.validateExprSubqueriesCtx(v.Operand, false)
	case *sql.IsNotNull:
		return e.validateExprSubqueriesCtx(v.Operand, false)
	case *sql.IsDistinctFrom:
		if err := e.validateExprSubqueriesCtx(v.Left, false); err != nil {
			return err
		}
		return e.validateExprSubqueriesCtx(v.Right, false)
	case *sql.IsNotDistinctFrom:
		if err := e.validateExprSubqueriesCtx(v.Left, false); err != nil {
			return err
		}
		return e.validateExprSubqueriesCtx(v.Right, false)
	}
	return nil
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
		if findAggregateInExpr(c.Expr) != "" {
			return true
		}
	}
	return false
}

// validateNoFromColumnRefs rejects column references in a FROM-less SELECT.
// SQLite's name resolver has no table to resolve against, so any column ref
// (qualified or not) is an error: SELECT false.false → "no such column:
// false.false". TRUE/FALSE literals are exempt (they parse as ColumnRefs).
func (e *Engine) validateNoFromColumnRefs(s *sql.SelectStmt) error {
	checkErr := error(nil)
	check := func(expr sql.Expr) {
		if checkErr != nil || expr == nil {
			return
		}
		walkExprFull(expr, func(e2 sql.Expr) {
			if checkErr != nil {
				return
			}
			ref, ok := e2.(*sql.ColumnRef)
			if !ok {
				return
			}
			if ref.Name == "*" {
				return
			}
			// NEW.col / OLD.col references are valid inside trigger bodies when
			// the named column actually exists in the trigger's row (e.g.
			// INSTEAD OF INSERT ON v ... new.k); an unknown column is still an
			// error, exactly as SQLite reports "no such column: new.x" for a
			// trigger on a table without column x.
			if ref.Table != "" {
				if strings.EqualFold(ref.Table, "new") && e.triggerNewRow != nil {
					if _, ok := e.triggerNewRow.Get(ref.Name); ok {
						return
					}
				}
				if strings.EqualFold(ref.Table, "old") && e.triggerOldRow != nil {
					if _, ok := e.triggerOldRow.Get(ref.Name); ok {
						return
					}
				}
			}
			if ref.Table != "" {
				checkErr = fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)
				return
			}
			// TRUE/FALSE are boolean literals in the parser, not columns — but
			// only when unqualified (a qualified false.false is a column ref).
			if strings.EqualFold(ref.Name, "TRUE") || strings.EqualFold(ref.Name, "FALSE") {
				return
			}
			// CURRENT_TIME/CURRENT_DATE/CURRENT_TIMESTAMP are keywords parsed as
			// column refs but evaluated as time literals by the engine.
			if strings.EqualFold(ref.Name, "CURRENT_TIME") || strings.EqualFold(ref.Name, "CURRENT_DATE") || strings.EqualFold(ref.Name, "CURRENT_TIMESTAMP") {
				return
			}
			// Double-quoted identifiers fall back to string literals when DQS is
			// enabled (handled at evaluation); do not reject them here.
			if ref.Quoted {
				return
			}
			checkErr = fmt.Errorf("no such column: %s", ref.Name)
		})
	}
	for _, col := range s.Columns {
		check(col.Expr)
	}
	// Output-column aliases (e.g. "SELECT 1 AS x WHERE x") are usable in the
	// WHERE clause of a no-FROM SELECT; skip references that name an alias.
	aliasNames := make(map[string]bool)
	for _, col := range s.Columns {
		if col.As != "" {
			aliasNames[strings.ToLower(col.As)] = true
		}
	}
	if s.Where != nil {
		walkExprFull(s.Where, func(e2 sql.Expr) {
			if checkErr != nil {
				return
			}
			ref, ok := e2.(*sql.ColumnRef)
			if !ok {
				return
			}
			if aliasNames[strings.ToLower(ref.Name)] {
				return
			}
			check(ref)
		})
	} else {
		check(s.Where)
	}
	return checkErr
}

// findAggregateInSelect checks if a SELECT statement directly contains an aggregate function.
func findAggregateInSelect(s *sql.SelectStmt) string {
	for _, col := range s.Columns {
		if nested := findAggregateInExpr(col.Expr); nested != "" {
			return nested
		}
	}
	return ""
}

// findAggregateInExpr walks an expression looking for aggregate function calls.
func findAggregateInExpr(expr sql.Expr) string {
	switch v := expr.(type) {
	case *sql.FuncCall:
		// Check if this is an aggregate by name (no registry lookup needed).
		// MIN/MAX are aggregates only in their single-argument form; with two
		// or more arguments they are scalar functions (SQLite semantics), so
		// a query like SELECT min(x,5) FROM t must NOT collapse to one row.
		upper := strings.ToUpper(v.Name)
		if upper == "COUNT" || upper == "SUM" || upper == "AVG" || upper == "TOTAL" || upper == "GROUP_CONCAT" || upper == "STRING_AGG" {
			return v.Name
		}
		if (upper == "MIN" || upper == "MAX") && len(v.Args) == 1 {
			return v.Name
		}
		for _, arg := range v.Args {
			if nested := findAggregateInExpr(arg); nested != "" {
				return nested
			}
		}
	case *sql.BinaryOp:
		if nested := findAggregateInExpr(v.Left); nested != "" {
			return nested
		}
		return findAggregateInExpr(v.Right)
	case *sql.UnaryOp:
		return findAggregateInExpr(v.Operand)
	case *sql.CaseExpr:
		if v.Operand != nil {
			if nested := findAggregateInExpr(v.Operand); nested != "" {
				return nested
			}
		}
		for _, w := range v.Whens {
			if nested := findAggregateInExpr(w.When); nested != "" {
				return nested
			}
			if nested := findAggregateInExpr(w.Then); nested != "" {
				return nested
			}
		}
		if v.Else != nil {
			return findAggregateInExpr(v.Else)
		}
	}
	return ""
}

func (e *Engine) validateExprOrderBy(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if len(v.OrderBy) > 0 {
			fn, ok := e.funcs.Find(v.Name)
			if ok && fn.Type != function.TypeAggregate {
				return fmt.Errorf("ORDER BY may not be used with non-aggregate %s()", v.Name)
			}
			// Check ORDER BY expressions for nested aggregates
			for _, ob := range v.OrderBy {
				if nested := findNestedAggregate(ob.Expr, e.funcs); nested != "" {
					return fmt.Errorf("misuse of aggregate function %s()", nested)
				}
			}
		}
		// Recurse into args for any nested expressions
		for _, arg := range v.Args {
			if err := e.validateExprOrderBy(arg); err != nil {
				return err
			}
		}
	case *sql.BinaryOp:
		if err := e.validateExprOrderBy(v.Left); err != nil {
			return err
		}
		return e.validateExprOrderBy(v.Right)
	case *sql.UnaryOp:
		return e.validateExprOrderBy(v.Operand)
	case *sql.CaseExpr:
		if v.Operand != nil {
			if err := e.validateExprOrderBy(v.Operand); err != nil {
				return err
			}
		}
		for _, w := range v.Whens {
			if err := e.validateExprOrderBy(w.When); err != nil {
				return err
			}
			if err := e.validateExprOrderBy(w.Then); err != nil {
				return err
			}
		}
		if v.Else != nil {
			return e.validateExprOrderBy(v.Else)
		}
	case *sql.Subquery:
		if v.Select != nil {
			return e.validateSelectExprs(v.Select)
		}
	case *sql.ExistsExpr:
		if v.Select != nil {
			return e.validateSelectExprs(v.Select)
		}
	}
	return nil
}

// evalDistinctAggregate evaluates an aggregate function with DISTINCT,
// deduplicating argument values before passing them to the aggregator.
func (e *Engine) evalDistinctAggregate(v *sql.FuncCall, rowMaps []RowMap) interface{} {
	fn, ok := e.funcs.Find(v.Name)
	if !ok || fn.Type != function.TypeAggregate {
		return nil
	}
	agg := fn.AggregateFn()
	seen := make(map[string]bool)
	var uniqueRows []RowMap

	for _, row := range rowMaps {
		// Apply FILTER (WHERE clause) if present — skip rows that don't match
		if v.Filter != nil {
			filterVal, err := e.evalExpr(v.Filter, row)
			if err != nil || !toBool(filterVal) {
				continue
			}
		}
		args := make([]interface{}, len(v.Args))
		for i, arg := range v.Args {
			val, err := e.evalExpr(arg, row)
			if err != nil {
				args[i] = nil
			} else {
				args[i] = util.UnwrapColumnValue(val)
			}
		}
		var key string
		for _, a := range args {
			if a == nil {
				key += "\x00"
			} else {
				key += fmt.Sprintf("%v", a) + "\x00"
			}
		}
		if !seen[key] {
			seen[key] = true
			uniqueRows = append(uniqueRows, row)
		}
	}

	// If ORDER BY is specified, sort unique rows by ORDER BY
	if len(v.OrderBy) > 0 && len(uniqueRows) > 1 {
		sort.SliceStable(uniqueRows, func(i, j int) bool {
			for _, ob := range v.OrderBy {
				coll := orderByTermCollation(ob.Expr)
				obExpr := stripCollate(ob.Expr)
				vi, errI := e.evalExpr(obExpr, uniqueRows[i])
				vj, errJ := e.evalExpr(obExpr, uniqueRows[j])
				if errI != nil || errJ != nil {
					continue
				}
				cmp := e.compareValuesCollate(vi, vj, coll)
				if cmp != 0 {
					if ob.Desc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
	}

	for _, row := range uniqueRows {
		args := make([]interface{}, len(v.Args))
		for i, arg := range v.Args {
			val, err := e.evalExpr(arg, row)
			if err != nil {
				args[i] = nil
			} else {
				args[i] = util.UnwrapColumnValue(val)
			}
		}
		agg.Step(args)
	}
	result, _ := agg.Final()
	return result
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

// distinctRows removes duplicate rows from a result set,
// keeping the corresponding rowMaps in sync. colls holds the collation of
// each result column (nil → BINARY).
func (e *Engine) distinctRows(rows [][]interface{}, rowMaps []RowMap, colls []string, s *sql.SelectStmt) ([][]interface{}, []RowMap) {
	if len(rows) == 0 {
		return rows, rowMaps
	}
	// When a covering index exists for the DISTINCT columns, SQLite satisfies
	// DISTINCT by scanning that index, so the output follows the index key
	// order (not insertion order). Determine the index columns so the
	// deduplicated rows can be sorted the same way.
	idxCols := e.coveringIndexForDistinct(s)
	seen := make(map[string]bool)
	var newRows [][]interface{}
	var newMaps []RowMap
	for i, row := range rows {
		key := rowKey(row, colls)
		if !seen[key] {
			seen[key] = true
			newRows = append(newRows, row)
			if i < len(rowMaps) {
				newMaps = append(newMaps, rowMaps[i])
			}
		}
	}
	if len(idxCols) > 0 && len(newMaps) == len(newRows) {
		type pair struct {
			row []interface{}
			m   RowMap
		}
		pairs := make([]pair, len(newRows))
		for i := range newRows {
			pairs[i] = pair{newRows[i], newMaps[i]}
		}
		sort.SliceStable(pairs, func(i, j int) bool {
			for _, col := range idxCols {
				vi := lookupRowMapValue(pairs[i].m, col)
				vj := lookupRowMapValue(pairs[j].m, col)
				cmp := util.CompareValues(util.UnwrapColumnValue(vi), util.UnwrapColumnValue(vj))
				if cmp != 0 {
					return cmp < 0
				}
			}
			return false
		})
		for i := range pairs {
			newRows[i] = pairs[i].row
			newMaps[i] = pairs[i].m
		}
	}
	return newRows, newMaps
}

// coveringIndexForDistinct returns the column list of an index that fully
// covers the DISTINCT output columns of s (a single-table query), so the
// DISTINCT rows can be emitted in index order like SQLite. Returns nil when
// no such index exists or the query is not a simple single-table scan.
func (e *Engine) coveringIndexForDistinct(s *sql.SelectStmt) []string {
	if s == nil || s.From.Name == "" || len(s.Joins) > 0 || s.From.Subquery != nil {
		return nil
	}
	tableName := s.From.Name
	if dot := strings.Index(tableName, "."); dot >= 0 {
		tableName = tableName[dot+1:]
	}
	alias := s.From.As
	if alias == "" {
		alias = tableName
	}
	// Collect the table column names referenced by the DISTINCT output.
	var need []string
	for _, col := range s.Columns {
		ref, ok := col.Expr.(*sql.ColumnRef)
		if !ok || ref.Name == "*" {
			return nil
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, alias) && !strings.EqualFold(ref.Table, tableName) {
			return nil
		}
		need = append(need, ref.Name)
	}
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Type != "index" || !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		cols := parseIndexColumns(entry.SQL)
		if len(cols) == 0 {
			continue
		}
		// The index's leading columns (restricted to the DISTINCT output
		// columns) provide the sort order SQLite's index scan yields. The
		// index need not cover every output column — remaining columns are
		// fetched from the table and act as a stable secondary key.
		needSet := make(map[string]bool)
		for _, n := range need {
			needSet[strings.ToLower(n)] = true
		}
		var order []string
		for _, c := range cols {
			if needSet[strings.ToLower(strings.TrimSpace(c))] {
				order = append(order, strings.TrimSpace(c))
			}
		}
		if len(order) == 0 {
			continue
		}
		return order
	}
	return nil
}

// lookupRowMapValue fetches a column value from a RowMap, trying both the
// qualified (alias.col / table.col) and unqualified forms.
func lookupRowMapValue(m RowMap, col string) interface{} {
	if m == nil {
		return nil
	}
	if v, ok := m[col]; ok {
		return v
	}
	// Try unqualified key (row maps may store "a" instead of "t1.a").
	for k, v := range m {
		if strings.HasSuffix(strings.ToLower(k), "."+strings.ToLower(col)) {
			return v
		}
	}
	return nil
}

// selectNeedsRowMaps returns true if the query requires per-row RowMap
// allocations for expression evaluation, sorting, filtering, or combining.
func selectNeedsRowMaps(e *Engine, s *sql.SelectStmt, tableName string) bool {
	// RowMaps are only needed for operations that require looking up values
	// by name in a map: JOINs evaluate expressions across row maps, ORDER BY
	// and DISTINCT need map-based comparison, UNIONS combine results, aggregates
	// group rows by map, and schema tables need filtering by name.
	// A simple WHERE clause without the above works fine with the structRow's
	// index-based lookup and doesn't need per-row map allocation.
	if len(s.Joins) > 0 {
		return true
	}
	if len(s.OrderBy) > 0 {
		return true
	}
	if s.Distinct {
		return true
	}
	if s.Union != nil {
		return true
	}
	if isSchemaTable(tableName) {
		return true
	}
	if e.hasAggregates(s.Columns) {
		return true
	}
	if len(s.GroupBy) > 0 || s.Having != nil {
		return true
	}
	// WHERE clauses with subqueries (EXISTS, scalar subqueries) need row maps
	// because the subquery evaluation passes the row as outerRow for correlated
	// references, and structRow's lazy decode may not have all columns available.
	if s.Where != nil && exprHasSubquery(s.Where) {
		return true
	}
	return false
}

// exprHasSubquery checks if an expression tree contains a Subquery or ExistsExpr.
func exprHasSubquery(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.Subquery, *sql.ExistsExpr:
		return true
	case *sql.BinaryOp:
		return exprHasSubquery(v.Left) || exprHasSubquery(v.Right)
	case *sql.UnaryOp:
		return exprHasSubquery(v.Operand)
	case *sql.ParenExpr:
		return exprHasSubquery(v.Expr)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if exprHasSubquery(arg) {
				return true
			}
		}
	case *sql.CaseExpr:
		for _, w := range v.Whens {
			if exprHasSubquery(w.When) || exprHasSubquery(w.Then) {
				return true
			}
		}
		if v.Else != nil && exprHasSubquery(v.Else) {
			return true
		}
	}
	return false
}

// scanTableRows iterates over all cells, applies WHERE, builds output rows.
func (e *Engine) scanTableRows(cursor *btree.Cursor, s *sql.SelectStmt, colDefs []sql.ColumnDef, needMaps bool) ([][]interface{}, []RowMap, error) {
	var allRowMaps []RowMap
	hasJoins := len(s.Joins) > 0

	// Determine which columns need affinity wrappers by collecting column
	// references from expressions (WHERE, SELECT list, ORDER BY, etc.).
	// Columns not referenced in comparisons can skip the ColumnValue wrapper
	// for faster row building.
	var affinityCols map[string]bool
	collectRefs := func(exprs ...sql.Expr) {
		if affinityCols == nil {
			affinityCols = make(map[string]bool)
		}
		var refs []string
		for _, expr := range exprs {
			if expr != nil {
				collectExprRefs(expr, &refs)
			}
		}
		for _, name := range refs {
			affinityCols[name] = true
		}
	}
	if s.Where != nil {
		// Collect column references from WHERE clause.
		collectRefs(s.Where)
	}
	// Also collect from SELECT columns: expressions like "xt==+xi" need the
	// affinity of xt even when xt is not referenced in WHERE/ORDER BY.
	if affinityCols == nil && len(s.Columns) > 0 {
		affinityCols = make(map[string]bool)
	}
	if len(s.Columns) > 0 {
		for _, col := range s.Columns {
			collectRefs(col.Expr)
		}
	}
	if len(s.OrderBy) > 0 {
		for _, ob := range s.OrderBy {
			collectRefs(ob.Expr)
		}
	}
	// JOIN ON clauses reference columns that need affinity wrappers for the
	// join comparison (e.g. t1 JOIN t3 ON t1.c0=t3.c0 must wrap t1.c0 so the
	// INTEGER-affinity comparison with t3.c0 applies). USING columns and
	// NATURAL join columns are also compared, so mark them too.
	for i := range s.Joins {
		j := &s.Joins[i]
		if j.On != nil {
			collectRefs(j.On)
		}
		for _, uc := range j.Using {
			if affinityCols == nil {
				affinityCols = make(map[string]bool)
			}
			affinityCols[uc] = true
		}
		// NATURAL joins compare all common columns; mark the join table's
		// columns (and, conservatively, the base's columns with the same
		// names) so the generated ON applies affinity.
		if isNaturalJoinType(j.JoinType) {
			if names, err := e.tableColumnNames(j.Table.Name); err == nil {
				if affinityCols == nil {
					affinityCols = make(map[string]bool)
				}
				for _, n := range names {
					affinityCols[n] = true
				}
			}
			// Mark the base FROM table's columns too.
			if s.From.Name != "" {
				if names, err := e.tableColumnNames(s.From.Name); err == nil {
					for _, n := range names {
						affinityCols[n] = true
					}
				}
			}
		}
	}
	if affinityCols == nil && needMaps {
		// When needMaps is true but nothing was collected, all columns need
		// affinity since maps may be used in expression evaluation downstream.
		affinityCols = make(map[string]bool)
		for _, cd := range colDefs {
			affinityCols[cd.Name] = true
		}
	} else if affinityCols == nil {
		// No WHERE and no needMaps: no affinity wrappers needed at all.
		affinityCols = nil
	}
	// Build shared column index for structRow lookups (avoids per-row map allocation).
	colIndex := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}

	// Reusable values buffer to avoid per-row slice allocation in buildStructRow
	reuseValues := make([]interface{}, len(colDefs))

	// Determine if this is a simple "SELECT *" (single star column, no joins).
	// If so, use the fast path that copies decoded values directly to output rows,
	// skipping the per-column row.Get / UnwrapColumnValue overhead in buildOutputRow.
	isSelectStar := !hasJoins && len(s.Columns) == 1
	if isSelectStar {
		if ref, ok := s.Columns[0].Expr.(*sql.ColumnRef); !ok || ref.Name != "*" {
			isSelectStar = false
		}
	}

	// Compute WHERE column indices for lazy decoding.
	// When there's a WHERE clause and no joins, we decode only the WHERE-referenced
	// columns first, evaluate the predicate, and only decode remaining columns if
	// the row passes. This avoids decoding expensive string/blob columns for rows
	// that are filtered out.
	var whereDecodeIndices map[int]bool
	var remainingDecodeIndices map[int]bool
	// Lazy decode only decodes WHERE-referenced columns first. But if the WHERE
	// contains subqueries (EXISTS, scalar), the subquery may reference any column
	// of the outer row, so we must decode all columns upfront.
	whereHasSubquery := s.Where != nil && exprHasSubquery(s.Where)
	useLazyDecode := s.Where != nil && !hasJoins && !whereHasSubquery // two-phase decode with cached serial types
	if useLazyDecode {
		whereDecodeIndices = make(map[int]bool, len(affinityCols))
		for name := range affinityCols {
			if idx, ok := colIndex[name]; ok {
				whereDecodeIndices[idx] = true
				continue
			}
			// Case-insensitive fallback: the WHERE reference may use a
			// different case than the declared column name (SQLite column
			// names are case-insensitive).
			for k, idx := range colIndex {
				if strings.EqualFold(k, name) && idx >= 0 {
					whereDecodeIndices[idx] = true
					break
				}
			}
		}
		// Pre-compute the complement set for phase 2 decoding (avoids per-row map allocation)
		remainingDecodeIndices = make(map[int]bool, len(colDefs)-len(whereDecodeIndices))
		for i := range colDefs {
			if !whereDecodeIndices[i] {
				remainingDecodeIndices[i] = true
			}
		}
	}

	// Reuse a single structRow across all rows to avoid per-row allocation.
	reuseSRow := &structRow{values: reuseValues, index: colIndex, rowID: 0}

	// Count active (non-dropped) column definitions for SELECT * fast path.
	activeColCount := 0
	for _, cd := range colDefs {
		if !cd.Dropped {
			activeColCount++
		}
	}

	// Pre-allocate output row values in a single flat slice to avoid per-row
	// allocation. Each output row is a sub-slice of this flat slice, which avoids
	// N individual make() calls (one per row) and reduces GC pressure significantly.
	// Initial capacity: 1024 rows × activeColCount values.
	outValues := make([]interface{}, 0, 1024*activeColCount)
	outRowStarts := make([]int, 0, 1024)
	var nonStarRows [][]interface{}

	for {
		payload, rowID, err := cursor.ReadCellData()
		if err != nil {
			break
		}

		passesWhere := true
		if useLazyDecode {
			// Parse header ONCE per row — inline to keep stack buffer on stack (avoids escape)
			var stackSerialTypes [16]uint64
			serialTypes := stackSerialTypes[:0]
			pos := 0
			hdrSize, n := util.GetVarint(payload[pos:])
			pos += n
			hdrEnd := int(hdrSize)
			for pos < hdrEnd {
				st, n2 := util.GetVarint(payload[pos:])
				pos += n2
				serialTypes = append(serialTypes, st)
			}
			dataStart := pos

			// Phase 1: decode only WHERE-referenced columns using stack-allocated types
			e.fillStructRowFromTypes(reuseSRow, payload, dataStart, colDefs, rowID, affinityCols, serialTypes, whereDecodeIndices)
			if !hasJoins && s.Where != nil {
				var whereErr error
				passesWhere, whereErr = e.rowPassesWhere(s.Where, reuseSRow, cursor)
				if whereErr != nil {
					return nil, nil, whereErr
				}
			}
			if !passesWhere {
				// Row filtered out — skip decoding remaining columns
				if ok, err := cursor.Next(); err != nil || !ok {
					break
				}
				continue
			}
			// Phase 2: decode remaining columns using cached types (no header re-parse)
			e.fillStructRowRemainingFromTypes(reuseSRow, payload, dataStart, colDefs, serialTypes, remainingDecodeIndices)
		} else {
			// Decode all columns at once — parse header inline with stack buffer
			// to avoid ParseRecordHeader heap allocation (saves ~40% of total alloc bytes).
			var stackSerialTypes [16]uint64
			serialTypes := stackSerialTypes[:0]
			pos := 0
			hdrSize, n := util.GetVarint(payload[pos:])
			pos += n
			hdrEnd := int(hdrSize)
			for pos < hdrEnd {
				st, n2 := util.GetVarint(payload[pos:])
				pos += n2
				serialTypes = append(serialTypes, st)
			}
			dataStart := pos
			if !useLazyDecode && s.GroupBy != nil {
			}
			e.fillStructRowFromTypes(reuseSRow, payload, dataStart, colDefs, rowID, affinityCols, serialTypes, nil)
			if !hasJoins && s.Where != nil {
				var whereErr error
				passesWhere, whereErr = e.rowPassesWhere(s.Where, reuseSRow, cursor)
				if whereErr != nil {
					return nil, nil, whereErr
				}
			}
		}

		// For joins, always build output (WHERE applied later in join processing).
		// For non-joins, only build output if WHERE passes.
		if hasJoins || passesWhere {
			// Build output row
			if isSelectStar {
				// Fast path: copy values into pre-allocated flat slice
				outRowStarts = append(outRowStarts, len(outValues))
				if affinityCols != nil {
					// Need to unwrap ColumnValue and collatedValue wrappers so
					// internal comparison metadata never leaks into the output.
					// (fillStructRowFromTypes wraps a column that has both an
					// affinity and a declared collation as collatedValue around
					// a ColumnValue; UnwrapColumnValue alone would leave the
					// collatedValue pointer visible.)
					for i, cd := range colDefs {
						if cd.Dropped {
							continue
						}
						outValues = append(outValues, util.UnwrapColumnValue(unwrapCollatedValue(reuseSRow.values[i])))
					}
				} else {
					// No affinity wrappers — values are already raw
					for i, cd := range colDefs {
						if cd.Dropped {
							continue
						}
						outValues = append(outValues, reuseSRow.values[i])
					}
				}
			} else {
				outRow := e.buildOutputRow(s.Columns, colDefs, reuseSRow)
				// For non-SELECT* paths, fall back to per-row allocation
				// since buildOutputRow returns a new slice.
				nonStarRows = append(nonStarRows, outRow)
			}
			if needMaps {
				allRowMaps = append(allRowMaps, structRowToMap(reuseSRow))
			}
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	// Build allRows: first from flat outValues (SELECT * fast path), then
	// append any individually-allocated rows (non-SELECT* path).
	totalStarRows := len(outRowStarts)
	allRows := make([][]interface{}, totalStarRows+len(nonStarRows))
	for i, start := range outRowStarts {
		allRows[i] = outValues[start : start+activeColCount : start+activeColCount]
	}
	copy(allRows[totalStarRows:], nonStarRows)

	// PRAGMA reverse_unordered_selects: reverse the scan order of the
	// top-level SELECT when it has no ORDER BY (SQLite's behavior).
	// Subqueries, compound members, and JOINed queries are not affected;
	// ORDER BY sorts after this and its output is deterministic.
	if e.reverseUnordered && len(s.OrderBy) == 0 && e.selectDepth == 1 && !hasJoins {
		for i, j := 0, len(allRows)-1; i < j; i, j = i+1, j-1 {
			allRows[i], allRows[j] = allRows[j], allRows[i]
		}
		for i, j := 0, len(allRowMaps)-1; i < j; i, j = i+1, j-1 {
			allRowMaps[i], allRowMaps[j] = allRowMaps[j], allRowMaps[i]
		}
	}

	return allRows, allRowMaps, nil
}

func (e *Engine) rowPassesWhere(where sql.Expr, row Row, cursor *btree.Cursor) (bool, error) {
	if where == nil {
		return true, nil
	}
	// Fast path: simple comparison ColumnRef OP Literal
	if bop, ok := where.(*sql.BinaryOp); ok && row != nil {
		if result, ok := e.fastEvalComparison(bop, row); ok {
			return result, nil
		}
	}
	match, err := e.evalBool(where, row)
	if err != nil {
		return false, err
	}
	return match, nil
}

// fastEvalComparison attempts to evaluate a simple BinaryOp comparison
// (ColumnRef OP Literal or Literal OP ColumnRef) without going through the
// full evalExpr → evalComplexExpr → evalBinaryOp chain. Returns (result, true)
// if the fast path was taken, or (false, false) to fall through to the slow path.
func (e *Engine) fastEvalComparison(bop *sql.BinaryOp, row Row) (bool, bool) {
	// Only handle simple comparison operators
	switch bop.Operator {
	case ">", "<", ">=", "<=", "=", "<>", "!=":
		// OK
	default:
		return false, false
	}

	// Try ColumnRef OP Literal
	if colRef, ok := bop.Left.(*sql.ColumnRef); ok {
		val, exists := fastEvalColRef(colRef, row)
		if !exists || isSQLNull(val) {
			return false, false // let slow path handle NULL
		}
		litVal, ok := e.evalLiteralFast(bop.Right)
		if !ok {
			return false, false
		}
		if litVal == nil {
			return false, false
		}
		// Fast path: both int64 — direct comparison without CompareValuesCollate
		if a, ok := util.UnwrapColumnValue(val).(int64); ok {
			if b, ok := litVal.(int64); ok {
				return applyIntComparison(bop.Operator, a, b), true
			}
		}
		cmp := e.compareValuesWithCollate(val, litVal)
		return applyComparisonOp(bop.Operator, cmp), true
	}

	// Try Literal OP ColumnRef
	if colRef, ok := bop.Right.(*sql.ColumnRef); ok {
		val, exists := fastEvalColRef(colRef, row)
		if !exists || isSQLNull(val) {
			return false, false
		}
		litVal, ok := e.evalLiteralFast(bop.Left)
		if !ok {
			return false, false
		}
		if litVal == nil {
			return false, false
		}
		// Fast path: both int64
		if b, ok := util.UnwrapColumnValue(val).(int64); ok {
			if a, ok := litVal.(int64); ok {
				return applyIntComparison(bop.Operator, a, b), true
			}
		}
		cmp := e.compareValuesWithCollate(litVal, val)
		return applyComparisonOp(bop.Operator, cmp), true
	}

	return false, false
}

// fastEvalColRef resolves a column reference against a row for the fast
// comparison path, honoring the qualifier: a qualified ref (t1.a) looks up
// the qualified key first, falling back to the unqualified key only when the
// qualifier matches the row's table or the row has no qualified keys.
func fastEvalColRef(cr *sql.ColumnRef, row Row) (interface{}, bool) {
	if cr.Table != "" {
		// Strip a schema prefix (main.t4.a) and try the qualified key.
		tableQual := cr.Table
		if dot := strings.Index(tableQual, "."); dot >= 0 {
			tableQual = tableQual[dot+1:]
		}
		if val, ok := row.Get(tableQual + "." + cr.Name); ok {
			return val, true
		}
		if val, ok := row.Get(cr.Table + "." + cr.Name); ok {
			return val, true
		}
		// No qualified key: fall back to the unqualified name only when the
		// row is NOT a join result (join maps store qualified keys).
		if !rowHasQualifiedKeys(row) {
			if val, ok := row.Get(cr.Name); ok {
				return val, true
			}
		}
		return nil, false
	}
	val, ok := row.Get(cr.Name)
	return val, ok
}

// evalLiteralFast evaluates a literal expression (NumericLit, StringLit, etc.)
// without error handling overhead.
func (e *Engine) evalLiteralFast(expr sql.Expr) (interface{}, bool) {
	switch v := expr.(type) {
	case *sql.NumericLit:
		if v.Cached() != nil {
			return v.Cached(), true
		}
		// Fall through to full eval for uncached (complex) numbers
		return nil, false
	case *sql.StringLit:
		return v.Value, true
	case *sql.ParenExpr:
		return e.evalLiteralFast(v.Expr)
	default:
		return nil, false
	}
}

// applyComparisonOp maps a comparison result to a boolean for the given operator.
func applyComparisonOp(op string, cmp int) bool {
	switch op {
	case ">":
		return cmp > 0
	case "<":
		return cmp < 0
	case ">=":
		return cmp >= 0
	case "<=":
		return cmp <= 0
	case "=":
		return cmp == 0
	case "<>", "!=":
		return cmp != 0
	default:
		return false
	}
}

// applyIntComparison evaluates a comparison operator directly on two int64
// values, avoiding the overhead of CompareValuesCollate for the common case
// of integer column vs integer literal comparisons.
func applyIntComparison(op string, a, b int64) bool {
	switch op {
	case ">":
		return a > b
	case "<":
		return a < b
	case ">=":
		return a >= b
	case "<=":
		return a <= b
	case "=":
		return a == b
	case "<>", "!=":
		return a != b
	default:
		return false
	}
}

// isSchemaTable returns true if the given table name is the sqlite_master/sqlite_schema table.
func isSchemaTable(name string) bool {
	upper := strings.ToUpper(name)
	return upper == "SQLITE_MASTER" || upper == "SQLITE_SCHEMA" ||
		upper == "MAIN.SQLITE_MASTER" || upper == "MAIN.SQLITE_SCHEMA"
}

// isSQLiteSequence reports whether name refers to the sqlite_sequence system
// table (case-insensitive, with or without a main. prefix). Unqualified
// references always resolve to the MAIN schema's sqlite_sequence, never the
// temp schema's synthetic fallback.
func isSQLiteSequence(name string) bool {
	upper := strings.ToUpper(name)
	return upper == "SQLITE_SEQUENCE" || upper == "MAIN.SQLITE_SEQUENCE"
}

// isHiddenSystemTable returns true if the table name is an internal system table
// that should not appear in sqlite_master queries. SQLite exposes sqlite_stat1
// and sqlite_stat4 as ordinary entries in sqlite_schema (they can be read and
// queried like any table), so they are NOT hidden here.
func isHiddenSystemTable(name string) bool {
	return false
}

// filterSystemTables removes rows that correspond to internal system tables
// from query results. This is applied when reading from sqlite_master/sqlite_schema.
func (e *Engine) filterSystemTables(allRows [][]interface{}, allRowMaps []RowMap, colDefs []sql.ColumnDef) ([][]interface{}, []RowMap) {
	// Find the index of the "name" column in colDefs
	nameIndex := -1
	for i, cd := range colDefs {
		if strings.EqualFold(cd.Name, "name") || strings.EqualFold(cd.Name, "tbl_name") {
			nameIndex = i
			break
		}
	}
	if nameIndex < 0 {
		return allRows, allRowMaps
	}

	var filteredRows [][]interface{}
	var filteredMaps []RowMap
	for i, rowMap := range allRowMaps {
		// Get the "name" value from the row map
		if nameVal, ok := rowMap["name"]; ok {
			nameStr := util.UnwrapColumnValue(nameVal)
			if nameStr != nil {
				if name, ok := nameStr.(string); ok && isHiddenSystemTable(name) {
					continue // skip system tables
				}
			}
		}
		if i < len(allRows) {
			filteredRows = append(filteredRows, allRows[i])
		}
		filteredMaps = append(filteredMaps, rowMap)
	}
	return filteredRows, filteredMaps
}

// buildRowMap builds a column-name-to-value map from a record.
// rowHasRowIDColumn reports whether the column definitions include a column
// named rowid, _rowid_, or oid. SQLite lets tables declare such columns; the
// column then shadows the pseudo-rowid alias for unqualified name resolution.
func rowHasRowIDColumn(colDefs []sql.ColumnDef) bool {
	for _, cd := range colDefs {
		n := strings.ToLower(cd.Name)
		if n == "rowid" || n == "_rowid_" || n == "oid" {
			return true
		}
	}
	return false
}

func (e *Engine) buildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) RowMap {
	row := make(RowMap)
	for i, v := range rec.Values {
		if i < len(colDefs) {
			// Wrap all column values with their affinity so comparison logic
			// can correctly apply SQLite affinity rules.
			aff := util.Affinity(colDefs[i].Type)
			cv := &util.ColumnValue{Value: v, Affinity: aff}
			// Wrap with the declared collation so comparisons use it (SQLite
			// column collation rules). compareValuesWithCollate extracts the
			// collation from collatedValue wrappers.
			if coll := colDefs[i].Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
				row[colDefs[i].Name] = &collatedValue{value: cv, collation: strings.ToUpper(coll)}
			} else {
				row[colDefs[i].Name] = cv
			}
		} else {
			row[fmt.Sprintf("c%d", i)] = v
		}
	}
	// A table may declare columns named rowid/oid/_rowid_ (SQLite allows it);
	// in that case the COLUMN value shadows the pseudo-rowid for name
	// resolution. Only install the pseudo-rowid when no such column exists.
	if !rowHasRowIDColumn(colDefs) {
		row["rowid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		row["_rowid_"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		row["oid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
	}
	for _, cd := range colDefs {
		if cd.PrimaryKey && row[cd.Name] == nil {
			row[cd.Name] = rowID
		}
	}
	// Rows written before ALTER TABLE ADD COLUMN have fewer record values
	// than column definitions; apply the added column's DEFAULT at read time
	// (with column affinity), matching SQLite semantics.
	if len(rec.Values) < len(colDefs) {
		vals := make([]interface{}, len(colDefs))
		copy(vals, rec.Values)
		e.applyColumnDefaults(vals, colDefs, len(rec.Values))
		for i := len(rec.Values); i < len(colDefs); i++ {
			if cd := &colDefs[i]; cd.Default != nil && !cd.Dropped {
				aff := util.Affinity(cd.Type)
				cv := &util.ColumnValue{Value: vals[i], Affinity: aff}
				if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
					row[cd.Name] = &collatedValue{value: cv, collation: strings.ToUpper(coll)}
				} else {
					row[cd.Name] = cv
				}
			}
		}
	}
	return row
}

// unwrapRowMap returns a copy of row with all affinity ColumnValue wrappers
// replaced by their raw values. Trigger bodies and RETURNING projections must
// receive raw values: the wrappers are only for WHERE-clause affinity
// comparison and would otherwise leak into trigger logs and result sets.
func unwrapRowMap(row RowMap) RowMap {
	out := make(RowMap, len(row))
	for k, v := range row {
		out[k] = util.UnwrapColumnValue(v)
	}
	return out
}

// buildStructRow creates a structRow from a record payload, wrapping values with
// ColumnValue affinity wrappers. Uses a shared column index for fast lookups
// and avoids per-row map allocation. Decodes values directly from the payload
// into the pre-allocated values slice, bypassing the intermediate Record allocation.
// fillStructRowFromTypes fills a structRow using pre-parsed serial types.
// It clears all values and decodes only the columns in colIndices.
// Unlike fillStructRow, it does not re-parse the record header.
func (e *Engine) fillStructRowFromTypes(sr *structRow, payload []byte, dataStart int, colDefs []sql.ColumnDef, rowID int64, affinityCols map[string]bool, serialTypes []uint64, colIndices map[int]bool) {
	values := sr.values
	for i := range values {
		values[i] = nil
	}
	sr.rowID = rowID

	storage.DecodeRecordValuesFromTypes(payload, dataStart, values, serialTypes, colIndices)

	// Rows written before ALTER TABLE ADD COLUMN have fewer record values
	// than column definitions; SQLite applies the added column's DEFAULT at
	// read time. Only columns beyond the record's value count get the default
	// (a column present in the record — even as NULL — keeps its value).
	e.applyColumnDefaults(values, colDefs, len(serialTypes))

	// Apply affinity wrappers for columns specified in affinityCols.
	// Match buildRowMap: wrap ALL columns (including INTEGER/REAL) with their
	// affinity so comparison logic applies the same SQLite affinity rules on
	// both the fast structRow path and the map path. Skipping I/R here made
	// results differ when ORDER BY forced the structRow path.
	if affinityCols != nil {
		for i := 0; i < len(values); i++ {
			if values[i] == nil {
				continue
			}
			// Case-insensitive: the reference in WHERE/SELECT may use a
			// different case than the declared column name.
			needAff := affinityCols[colDefs[i].Name]
			if !needAff {
				for name := range affinityCols {
					if strings.EqualFold(name, colDefs[i].Name) {
						needAff = true
						break
					}
				}
			}
			if needAff {
				aff := util.Affinity(colDefs[i].Type)
				cv := &util.ColumnValue{Value: values[i], Affinity: aff}
				// Wrap with the declared collation so comparisons use it,
				// matching buildRowMap (SQLite column collation rules).
				if coll := colDefs[i].Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
					values[i] = &collatedValue{value: cv, collation: strings.ToUpper(coll)}
				} else {
					values[i] = cv
				}
			}
		}
		// Handle rowid and PRIMARY KEY for affinity columns
		for i, cd := range colDefs {
			if isIPKRowidAliasCol(cd) && values[i] == nil && affinityCols[cd.Name] {
				aff := util.Affinity(cd.Type)
				values[i] = &util.ColumnValue{Value: rowID, Affinity: aff}
			}
		}
	}
}

// fillStructRowRemainingFromTypes decodes remaining columns using pre-parsed
// serial types. This is the second phase of lazy decoding: after WHERE
// evaluation passes, decode the columns that were skipped in the first phase.
// Only columns in affinityCols get ColumnValue wrappers (these are the WHERE-referenced
// columns — already decoded in phase 1). Remaining columns are left raw.
func (e *Engine) fillStructRowRemainingFromTypes(sr *structRow, payload []byte, dataStart int, colDefs []sql.ColumnDef, serialTypes []uint64, indices map[int]bool) {
	storage.DecodeRecordValuesFromTypes(payload, dataStart, sr.values, serialTypes, indices)
	// Same missing-column default handling as fillStructRowFromTypes: rows
	// written before ALTER TABLE ADD COLUMN need the added column's DEFAULT.
	e.applyColumnDefaults(sr.values, colDefs, len(serialTypes))
}

// applyColumnDefaults fills in DEFAULT values for columns that are absent
// from the stored record (e.g., rows written before ALTER TABLE ADD COLUMN).
// Only columns beyond the record's value count get the default: a column
// present in the record — even as NULL — keeps its stored value. The default
// expression is evaluated with an empty row (it cannot reference other
// columns) and the column's declared affinity is applied, matching SQLite's
// ALTER TABLE ADD COLUMN semantics (e.g. TEXT column with DEFAULT -123.0
// yields the text value "-123.0").
func (e *Engine) applyColumnDefaults(values []interface{}, colDefs []sql.ColumnDef, recordValueCount int) {
	for i := recordValueCount; i < len(colDefs); i++ {
		cd := &colDefs[i]
		if cd.Default != nil && !cd.Dropped {
			if dv, err := e.evalExpr(cd.Default, nil); err == nil {
				values[i] = util.ApplyColumnAffinity(dv, cd.Type)
			}
		}
	}
}

// structRowToMap converts a structRow to a RowMap, deep-copying mutable
// values (ColumnValue wrappers, []byte) so the map does not share the
// reused structRow value slots that the next decoded row overwrites.
func structRowToMap(sr *structRow) RowMap {
	m := make(RowMap, len(sr.index)+1)
	m["rowid"] = &util.ColumnValue{Value: sr.rowID, Affinity: 'I'}
	for name, idx := range sr.index {
		if idx < len(sr.values) {
			m[name] = cloneRowValue(sr.values[idx])
		}
	}
	return m
}

// cloneRowValue deep-copies a mutable value so RowMaps do not share the
// reused structRow value slots (which are overwritten by the next decoded
// row). Immutable values (int64, float64, nil) are returned as-is; pointers
// (ColumnValue, []byte, string) are copied.
func cloneRowValue(v interface{}) interface{} {
	switch t := v.(type) {
	case *util.ColumnValue:
		cp := *t
		switch inner := t.Value.(type) {
		case []byte:
			b := make([]byte, len(inner))
			copy(b, inner)
			cp.Value = b
		case string:
			cp.Value = inner
		}
		return &cp
	case []byte:
		b := make([]byte, len(t))
		copy(b, t)
		return b
	default:
		return v
	}
}

// buildOutputRow builds the output row from the SELECT columns.
func (e *Engine) buildOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) []interface{} {
	// Count expected columns for pre-allocation
	colCount := 0
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			for _, cd := range colDefs {
				if !cd.Dropped && !isHiddenColumnDef(cd) {
					colCount++
				}
			}
		} else {
			colCount++
		}
	}
	outRow := make([]interface{}, 0, colCount)
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			if ref.Table != "" {
				// Qualified star (t.*): include only the columns of that table,
				// resolved by the table's real column names (aliases allowed).
				tableCols := e.qualifiedStarColNames(ref.Table, colDefs, row)
				for _, cd := range tableCols {
					outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(cd.value)))
				}
				continue
			}
			posIdx := 0
			for _, cd := range colDefs {
				if cd.Dropped || isHiddenColumnDef(cd) {
					continue
				}
				if pos, ok := row.Get(positionalRowKey); ok {
					// Duplicate-named columns (e.g. a view aliasing several
					// columns '') cannot be distinguished by name; use the
					// retained positional slice when the colDef index matches.
					if pv, ok := pos.([]interface{}); ok && posIdx < len(pv) {
						outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(pv[posIdx])))
						posIdx++
						continue
					}
				}
				if val, exists := row.Get(cd.Name); exists {
					outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(val)))
				}
				posIdx++
			}
		} else {
			v, err := e.evalExpr(col.Expr, row)
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, util.UnwrapColumnValue(unwrapCollatedValue(v)))
			}
		}
	}
	return outRow
}

// qualifiedStarColNames resolves a qualified star (t.* / alias.*) against a
// joined row map. It returns the table's column name+value pairs in column
// order, resolving each value via the qualified key (alias.col) first, then
// the short key (col) when the qualified key is absent.
func (e *Engine) qualifiedStarColNames(tableRef string, colDefs []sql.ColumnDef, row Row) []struct {
	name  string
	value interface{}
} {
	var out []struct {
		name  string
		value interface{}
	}
	// Resolve the referenced table's column names. Prefer the schema's full
	// column list: a USING join merges the join column out of the colDefs, but
	// t.* must still include it (SQLite emits the merged value for t.*).
	// When tableRef is a join ALIAS (a in FROM t5 AS a) the schema lookup may
	// find a same-named real table; the row map's alias-qualified keys are the
	// ground truth for what the alias actually exposes.
	var colNames []string
	var err error
	if row != nil {
		if rm, ok := row.(RowMap); ok {
			prefix := tableRef + "."
			aliased := false
			for k := range rm {
				if strings.HasPrefix(k, prefix) {
					aliased = true
					break
				}
			}
			if aliased {
				var names []string
				seen := make(map[string]bool)
				for k := range rm {
					if strings.HasPrefix(k, prefix) {
						n := strings.TrimPrefix(k, prefix)
						// Skip the rowid pseudo-columns: t.* never expands them.
						if n == "rowid" || n == "_rowid_" || n == "oid" {
							continue
						}
						if !seen[n] {
							seen[n] = true
							names = append(names, n)
						}
					}
				}
				// Prefer colDefs order when the alias maps to the join operand's
				// real column names (colDefs is deterministic; map iteration is
				// not).
				if len(colDefs) > 0 {
					var ordered []string
					os := make(map[string]bool)
					for _, cd := range colDefs {
						if strings.HasPrefix(cd.Name, prefix) || !strings.Contains(cd.Name, ".") {
							n := strings.TrimPrefix(cd.Name, prefix)
							if seen[n] && !os[n] {
								os[n] = true
								ordered = append(ordered, n)
							}
						}
					}
					if len(ordered) == len(names) {
						names = ordered
					}
				}
				colNames = names
			}
		}
	}
	if len(colNames) == 0 {
		colNames, err = e.tableColumnNames(tableRef)
	}
	if err != nil || len(colNames) == 0 {
		// Fall back to the column defs in order. For each def, resolve it
		// through the qualified key (alias.col) only — the short key is
		// ambiguous when two operands share column names (the last one wins
		// in the row map). A def that is itself prefixed (table.col from a
		// conflict-renamed operand) is used only when its prefix matches.
		if len(colNames) == 0 && row != nil {
			seen := make(map[string]bool)
			add := func(name string) {
				if !seen[name] {
					seen[name] = true
					colNames = append(colNames, name)
				}
			}
			for _, cd := range colDefs {
				if cd.Dropped {
					continue
				}
				if strings.HasPrefix(cd.Name, tableRef+".") {
					add(strings.TrimPrefix(cd.Name, tableRef+"."))
					continue
				}
				if strings.Contains(cd.Name, ".") {
					// Prefixed def belonging to another operand.
					continue
				}
				if _, ok := row.Get(tableRef + "." + cd.Name); ok {
					add(cd.Name)
				}
			}
		}
		// Last resort: derive the column names from the row map's qualified
		// keys (alias.col). Order is not guaranteed (Go map iteration), but
		// this still resolves the values for unusual row shapes.
		if len(colNames) == 0 && row != nil {
			if rm, ok := row.(RowMap); ok {
				seen := make(map[string]bool)
				prefix := tableRef + "."
				for k := range rm {
					if strings.HasPrefix(k, prefix) {
						n := strings.TrimPrefix(k, prefix)
						if !seen[n] {
							seen[n] = true
							colNames = append(colNames, n)
						}
					}
				}
			}
		}
	}
	for _, name := range colNames {
		if val, ok := row.Get(tableRef + "." + name); ok {
			out = append(out, struct {
				name  string
				value interface{}
			}{name: name, value: val})
			continue
		}
		if val, ok := row.Get(name); ok {
			out = append(out, struct {
				name  string
				value interface{}
			}{name: name, value: val})
		}
	}
	return out
}

// buildColumnNames builds the column name list from SELECT columns.
// selectAliasTarget returns the underlying table name for a FROM/JOIN alias
// in the SELECT, or "" when name is not an alias. Used to resolve qualified
// stars: SELECT a.* FROM t5 AS a must resolve to t5 even when a real table
// named a exists (a join alias shadows a same-named table).
func selectAliasTarget(s *sql.SelectStmt, name string) string {
	if s == nil || name == "" {
		return ""
	}
	if s.From.As != "" && strings.EqualFold(s.From.As, name) {
		return s.From.Name
	}
	if s.From.Name != "" && strings.EqualFold(s.From.Name, name) {
		// An unaliased FROM table reference also resolves (t.* where t is
		// the FROM table); the alias check above takes precedence when an
		// alias matches, because an alias shadows a same-named table.
		if s.From.As == "" {
			return s.From.Name
		}
	}
	for _, j := range s.Joins {
		if j.Table.As != "" && strings.EqualFold(j.Table.As, name) {
			return j.Table.Name
		}
		if j.Table.Name != "" && strings.EqualFold(j.Table.Name, name) && j.Table.As == "" {
			return j.Table.Name
		}
	}
	return ""
}

func (e *Engine) buildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef, sel *sql.SelectStmt) []string {
	var names []string
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			if ref.Table != "" {
				// Qualified star (t.*): only that table's columns. Use the
				// colDefs prefixed names when present (conflicting columns are
				// stored as table.col in join colDefs); a non-conflicting
				// column stays unprefixed in colDefs, so fall back to the
				// schema column list for the complete set.
				// A JOIN alias shadows a same-named real table: SELECT a.* FROM
				// t5 AS a must resolve to t5's columns even when a table named
				// a exists.
				refTable := ref.Table
				if sel != nil {
					if t := selectAliasTarget(sel, ref.Table); t != "" {
						refTable = t
					}
				}
				schemaNames, _ := e.tableColumnNames(refTable)
				if len(schemaNames) == 0 {
					// Fall back to the column defs in order: prefixed defs for this
					// operand (alias.col / table.col) plus the unprefixed defs
					// (which belong to the first operand in a join).
					for _, cd := range colDefs {
						if cd.Dropped {
							continue
						}
						if strings.HasPrefix(cd.Name, ref.Table+".") {
							schemaNames = append(schemaNames, strings.TrimPrefix(cd.Name, ref.Table+"."))
						} else if !strings.Contains(cd.Name, ".") {
							schemaNames = append(schemaNames, cd.Name)
						}
					}
				}
				var tblNames []string
				for _, n := range schemaNames {
					// Use the prefixed name when the column conflicted with a
					// same-named column elsewhere (colDefs stores it as
					// table.col), so result column names stay unique.
					prefixed := refTable + "." + n
					found := false
					for _, cd := range colDefs {
						if cd.Name == prefixed {
							tblNames = append(tblNames, prefixed)
							found = true
							break
						}
					}
					if !found {
						tblNames = append(tblNames, n)
					}
				}
				names = append(names, tblNames...)
				continue
			}
			for _, cd := range colDefs {
				if cd.Dropped || isHiddenColumnDef(cd) {
					continue
				}
				names = append(names, cd.Name)
			}
		} else if rv, ok := col.Expr.(*sql.RowValue); ok {
			// Multi-expression RETURNING (RETURNING a, b, *): expand * inline
			// and name each expression like a SELECT column list.
			for _, sub := range rv.Values {
				if ref, ok := sub.(*sql.ColumnRef); ok && ref.Name == "*" {
					for _, cd := range colDefs {
						if cd.Dropped || isHiddenColumnDef(cd) {
							continue
						}
						names = append(names, cd.Name)
					}
				} else if ref, ok := sub.(*sql.ColumnRef); ok {
					names = append(names, ref.Name)
				} else {
					names = append(names, "")
				}
			}
		} else if col.As != "" {
			names = append(names, col.As)
		} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			names = append(names, ref.Name)
		} else {
			// Unaliased expression: SQLite names the result column after the
			// expression text (e.g. SELECT a+b names it "a+b"). Without this,
			// CREATE TABLE ... AS SELECT of an expression produces a column
			// with an empty name and SELECT * exposes zero columns.
			names = append(names, sql.ExprString(col.Expr))
		}
	}
	return names
}

// selectProjectsPlainColumns reports whether every SELECT column is a bare
// column reference or star (no computed expressions, aliases, or aggregates).
// When true, a query's joined row maps align with its output rows, so they can
// be reused when the query is materialized as a derived table.
func selectProjectsPlainColumns(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if col.As != "" {
			return false
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			if ref.Name == "*" || ref.Name != "" {
				continue
			}
		}
		return false
	}
	return true
}

// validateOrderBy checks that any positional ORDER BY terms (integer literals)
// fall within the range of result columns. Returns an error matching SQLite's
// message format when a term is out of range.
func validateOrderBy(orderBy []sql.OrderByTerm, numCols int) error {
	for i, ob := range orderBy {
		if nl, ok := ob.Expr.(*sql.NumericLit); ok {
			// Parse the positional reference
			n, ok := parsePositiveInt(nl.Value)
			if !ok || n < 1 {
				continue // not a valid positional reference
			}
			if n > numCols {
				return fmt.Errorf("%d%s ORDER BY term out of range - should be between 1 and %d",
					i+1, ordinalSuffix(i+1), numCols)
			}
		}
	}
	return nil
}

// validateCompoundOrderBy enforces SQLite's compound-SELECT ORDER BY rule:
// each term must be a result-column ordinal or a name matching one of the
// result columns of any SELECT member (case-insensitively). Expressions that
// do not match a member column (e.g. ORDER BY a+b when no member selects a
// column named a+b) are rejected with "Nth ORDER BY term does not match any
// column in the result set".
func (e *Engine) validateCompoundOrderBy(s *sql.SelectStmt, orderBy []sql.OrderByTerm) error {
	colNames := make(map[string]bool)
	collect := func(m *sql.SelectStmt) {
		for _, col := range m.Columns {
			if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name != "*" {
				colNames[strings.ToLower(ref.Name)] = true
			}
			if col.As != "" {
				colNames[strings.ToLower(col.As)] = true
			}
		}
		// A compound member that projects * (e.g. SELECT * FROM t2) makes the
		// underlying table's columns available to ORDER BY (SQLite resolves
		// ORDER BY terms against the expanded result columns).
		for _, col := range m.Columns {
			if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
				tbl := ref.Table
				if tbl == "" {
					tbl = m.From.Name
				}
				if tbl == "" {
					continue
				}
				if cols, err := e.resolveTableColumnNames(m, tbl); err == nil {
					for _, n := range cols {
						colNames[strings.ToLower(n)] = true
					}
				}
			}
		}
	}
	cur := s
	for cur != nil {
		collect(cur)
		cur = cur.Union
	}
	for i, ob := range orderBy {
		// Unwrap chained COLLATE: "1 COLLATE binary COLLATE nocase" is an
		// ordinal with a collation (each COLLATE wraps the previous).
		expr := ob.Expr
		for {
			bop, ok := expr.(*sql.BinaryOp)
			if !ok || !strings.EqualFold(bop.Operator, "COLLATE") {
				break
			}
			expr = bop.Left
		}
		// Ordinals (1, 2, ...) are always allowed; validateOrderBy already
		// checked the range.
		if nl, ok := expr.(*sql.NumericLit); ok {
			if _, ok := parsePositiveInt(nl.Value); ok {
				continue
			}
		}
		// A bare column name (possibly wrapped in COLLATE) must match a
		// member's result column.
		if ref, ok := expr.(*sql.ColumnRef); ok && colNames[strings.ToLower(ref.Name)] {
			continue
		}
		// rowid/_rowid_/oid in compound ORDER BY resolve to the source table's
		// rowid (SQLite sorts by the rowid of the underlying rows even though
		// it is not a result column).
		if ref, ok := expr.(*sql.ColumnRef); ok && ref.Table == "" && isRowIDName(ref.Name) {
			continue
		}
		// Aggregate expressions are permitted in compound ORDER BY (SQLite
		// treats the trailing ORDER BY of a compound as applying to the
		// merged result, where aggregates are legal).
		if e.exprHasAggregate(ob.Expr) {
			continue
		}
		return fmt.Errorf("%d%s ORDER BY term does not match any column in the result set",
			i+1, ordinalSuffix(i+1))
	}
	return nil
}

func parsePositiveInt(s string) (int, bool) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1000000 {
			return 0, false
		}
	}
	return n, n > 0
}

func ordinalSuffix(n int) string {
	switch n % 100 {
	case 11, 12, 13:
		return "th"
	}
	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}

// sortRowsWithMaps sorts result rows using the original row maps.
func (e *Engine) sortRowsWithMaps(result *Result, orderBy []sql.OrderByTerm, rowMaps []RowMap) {
	n := len(rowMaps)
	if n <= 1 {
		return
	}
	// Ensure result.Rows has at least as many elements as rowMaps
	if len(result.Rows) < n {
		n = len(result.Rows)
	}
	if n <= 1 {
		return
	}
	// Sort indices, then reorder both slices in-place
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool {
		return e.lessRows(orderBy, rowMaps, result.Rows, result.Columns, indices[i], indices[j])
	})
	newRows := make([][]interface{}, n)
	newMaps := make([]RowMap, n)
	for i, idx := range indices {
		newRows[i] = result.Rows[idx]
		newMaps[i] = rowMaps[idx]
	}
	result.Rows = newRows
	copy(rowMaps, newMaps)
}

// lessRows returns true if row i should come before row j according to ORDER BY.
// resultCols maps ORDER BY aliases/column names to result column positions.
func (e *Engine) lessRows(orderBy []sql.OrderByTerm, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) bool {
	for _, ob := range orderBy {
		// Handle positional ORDER BY (e.g., ORDER BY 1 means order by first
		// column). SQLite treats a unary plus as the literal, so ORDER BY +1
		// is also positional.
		obExpr := ob.Expr
		if uo, ok := obExpr.(*sql.UnaryOp); ok && (uo.Operator == "+" || uo.Operator == "-") {
			if num, ok := uo.Operand.(*sql.NumericLit); ok {
				if uo.Operator == "-" {
					obExpr = &sql.NumericLit{Value: "-" + num.Value}
				} else {
					obExpr = num
				}
			}
		}
		// ORDER BY 1 COLLATE nocase COLLATE binary: strip ALL COLLATE
		// operators to reveal the positional operand; the term's collation is
		// applied by compareOrderByValues via orderByTermCollation.
		for {
			prev := obExpr
			obExpr = stripCollate(obExpr)
			if obExpr == prev {
				break
			}
		}
		if nl, ok := obExpr.(*sql.NumericLit); ok {
			if pos, err := strconv.ParseInt(nl.Value, 10, 64); err == nil && pos >= 1 && pos <= int64(len(rows[i])) {
				left := rows[i][pos-1]
				right := rows[j][pos-1]
				cmp := e.compareOrderByValues(left, right, ob)
				if cmp < 0 {
					return true
				} else if cmp > 0 {
					return false
				}
				continue
			}
		}
		// An ORDER BY term that names a result column (alias or column name)
		// resolves to that column's value in the output row (SQLite rules:
		// aliases in the SELECT list can be referenced by ORDER BY). Strip a
		// COLLATE operator so `ORDER BY alias COLLATE nocase` still resolves.
		if ref, ok := stripCollate(obExpr).(*sql.ColumnRef); ok {
			if pos := resultColumnIndex(resultCols, ref.Name); pos >= 0 && pos < len(rows[i]) {
				left := rows[i][pos]
				right := rows[j][pos]
				cmp := e.compareOrderByValues(left, right, ob)
				if cmp < 0 {
					return true
				} else if cmp > 0 {
					return false
				}
				continue
			}
		}
		left, _ := e.evalExpr(ob.Expr, rowMaps[i])
		right, _ := e.evalExpr(ob.Expr, rowMaps[j])
		cmp := e.compareOrderByValues(left, right, ob)
		if cmp < 0 {
			return true
		} else if cmp > 0 {
			return false
		}
	}
	return false
}

// compareOrderByValues compares two values for an ORDER BY term, applying
// the term's direction and explicit NULLS FIRST/LAST rules. SQLite defaults:
// NULLs sort first for ASC, last for DESC; explicit NULLS FIRST/LAST win.
// The ORDER BY term's explicit COLLATE (e.g. "ORDER BY x COLLATE nocase")
// is applied when the compared values do not already carry a collation
// marker (e.g. positional/alias ORDER BY terms resolve to output values).
func (e *Engine) compareOrderByValues(left, right interface{}, ob sql.OrderByTerm) int {
	leftNull := isSQLNull(left)
	rightNull := isSQLNull(right)
	if leftNull || rightNull {
		// NULLs are equal to each other and sort before all non-NULL values
		// by default (ASC), after all non-NULL values by default (DESC).
		if leftNull && rightNull {
			return 0
		}
		nullsFirst := ob.NullsFirst
		if ob.NullsLast {
			nullsFirst = false
		}
		if !ob.NullsFirst && !ob.NullsLast {
			// Default: ASC → NULLs first, DESC → NULLs last.
			nullsFirst = !ob.Desc
		}
		if leftNull {
			if nullsFirst {
				return -1
			}
			return 1
		}
		if nullsFirst {
			return 1
		}
		return -1
	}
	coll := orderByTermCollation(ob.Expr)
	if coll != "" {
		// Explicit COLLATE in the ORDER BY term. SQLite resolves the term's
		// collation as: explicit COLLATE wins; otherwise the column's
		// collation (carried by the value marker). compareValuesWithCollate
		// already applies the explicit COLLATE on either side; when the
		// values carry no marker (output rows), apply the term's collation.
		lc, _ := extractValue(left)
		rc, _ := extractValue(right)
		leftHasCol := isColumnValue(left) || isExplicitCollated(left)
		rightHasCol := isColumnValue(right) || isExplicitCollated(right)
		if !leftHasCol && !rightHasCol {
			cmp := e.compareValuesCollate(lc, rc, coll)
			if ob.Desc {
				cmp = -cmp
			}
			return cmp
		}
	}
	cmp := e.compareValuesWithCollate(left, right)
	if ob.Desc {
		cmp = -cmp
	}
	return cmp
}

// isExplicitCollated reports whether v carries an explicit COLLATE marker
// (from the COLLATE operator), as opposed to a column's declared collation.
func isExplicitCollated(v interface{}) bool {
	cv, ok := v.(*collatedValue)
	return ok && cv.explicit
}

// isSQLNull reports whether v is a SQL NULL value.
func isSQLNull(v interface{}) bool {
	if v == nil {
		return true
	}
	if cv, ok := v.(*util.ColumnValue); ok {
		return cv.Value == nil
	}
	if cv, ok := v.(*collatedValue); ok {
		return isSQLNull(cv.value)
	}
	return false
}

// resultColumnIndex returns the index of a column name in resultCols
// (case-insensitive), or -1.
func resultColumnIndex(resultCols []string, name string) int {
	for i, c := range resultCols {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

// addSubqueryFromCols adds the column names of a subquery's FROM tables (and
// nested derived tables) to the given set, so ON clauses referencing a derived
// table's inner columns (ON c=b with (SELECT * FROM t2, t3)) validate.
func (e *Engine) addSubqueryFromCols(s *sql.SelectStmt, out map[string]bool) {
	if s == nil {
		return
	}
	if s.From.Name != "" {
		if names, err := e.tableColumnNames(s.From.Name); err == nil {
			for _, n := range names {
				out[n] = true
			}
		}
	}
	if s.From.Subquery != nil {
		e.addSubqueryFromCols(s.From.Subquery, out)
	}
	for i := range s.Joins {
		j := &s.Joins[i]
		if j.Table.Subquery != nil {
			e.addSubqueryFromCols(j.Table.Subquery, out)
		} else if j.Table.Name != "" {
			if names, err := e.tableColumnNames(j.Table.Name); err == nil {
				for _, n := range names {
					out[n] = true
				}
			}
		}
	}
}

// collectFromTableNames adds the table names visible in a SELECT's FROM
// clause (base table and all joined tables, recursing into derived tables) to
// the given set. Used by ON-clause validation so a right-side subquery's inner
// tables are considered available.
func collectFromTableNames(s *sql.SelectStmt, out map[string]bool) {
	if s == nil {
		return
	}
	tn := s.From.Name
	if s.From.As != "" {
		tn = s.From.As
	}
	if tn != "" {
		out[tn] = true
	}
	if s.From.Subquery != nil {
		collectFromTableNames(s.From.Subquery, out)
	}
	for _, j := range s.Joins {
		jn := j.Table.Name
		if j.Table.As != "" {
			jn = j.Table.As
		}
		if jn != "" {
			out[jn] = true
		}
		if j.Table.Subquery != nil {
			collectFromTableNames(j.Table.Subquery, out)
		}
	}
}

// collectOuterTableNames collects the FROM/JOIN operand names visible at one
// SELECT level WITHOUT descending into derived-table (subquery) operands. Used
// by ambiguity validation: a derived table's output columns shadow its inner
// tables at the outer level.
func collectOuterTableNames(s *sql.SelectStmt, out map[string]bool) {
	if s == nil {
		return
	}
	tn := s.From.Name
	if s.From.As != "" {
		tn = s.From.As
	}
	if tn != "" {
		out[tn] = true
	}
	for _, j := range s.Joins {
		jn := j.Table.Name
		if j.Table.As != "" {
			jn = j.Table.As
		}
		if jn != "" {
			out[jn] = true
		}
	}
}

// validateAmbiguousColumnRefs rejects unqualified column references that are
// ambiguous across the joined tables (SQLite: "ambiguous column name: X" at
// prepare time). Every table contributes its declared columns plus the
// implicit rowid/_rowid_/oid columns; a bare reference naming a column that
// exists in more than one joined table is ambiguous. Qualified references
// (t.col), TRUE/FALSE literals, and output-column aliases are exempt.
func (e *Engine) validateAmbiguousColumnRefs(s *sql.SelectStmt) error {
	// Collect the visible table names (base + joins, using aliases). Derived
	// tables' inner tables are NOT visible at this level — the derived table's
	// OUTPUT columns shadow them (SELECT q FROM (SELECT t3.q AS q, ... FROM t3
	// NATURAL JOIN t4) n must resolve q to n's output column, not the inner
	// t3/t4 which both have q).
	names := map[string]bool{}
	collectOuterTableNames(s, names)
	if len(names) < 2 {
		return nil
	}
	// Columns merged by USING/NATURAL joins collapse into a single value and
	// are NOT ambiguous (SQLite: SELECT b FROM t1 NATURAL JOIN t2 where both
	// have b returns the merged b). Collect them so the ambiguity check skips
	// them.
	mergedCols := map[string]bool{}
	e.collectJoinMergedColumns(s, names, mergedCols)
	// Build each table's column set (lowercased), including rowid aliases.
	colInTables := map[string][]string{}
	for tn := range names {
		cols, err := e.tableColumnNames(tn)
		if err != nil {
			// A table we cannot resolve (e.g. a CTE reference) — skip; the
			// execution path reports the missing table.
			continue
		}
		for _, c := range cols {
			colInTables[strings.ToLower(c)] = append(colInTables[strings.ToLower(c)], tn)
		}
		// rowid/_rowid_/oid exist implicitly in every table.
		for _, r := range []string{"rowid", "_rowid_", "oid"} {
			colInTables[r] = append(colInTables[r], tn)
		}
	}
	amb := func(name string) bool {
		l := strings.ToLower(name)
		if mergedCols[l] {
			return false
		}
		return len(colInTables[l]) > 1
	}
	checkExpr := func(expr sql.Expr) error {
		if expr == nil {
			return nil
		}
		var checkErr error
		walkExprFull(expr, func(e2 sql.Expr) {
			if checkErr != nil {
				return
			}
			ref, ok := e2.(*sql.ColumnRef)
			if !ok || ref.Name == "*" || ref.Table != "" {
				return
			}
			if strings.EqualFold(ref.Name, "TRUE") || strings.EqualFold(ref.Name, "FALSE") {
				return
			}
			if amb(ref.Name) {
				checkErr = fmt.Errorf("ambiguous column name: %s", ref.Name)
			}
		})
		return checkErr
	}
	for _, col := range s.Columns {
		if err := checkExpr(col.Expr); err != nil {
			return err
		}
	}
	if err := checkExpr(s.Where); err != nil {
		return err
	}
	for _, g := range s.GroupBy {
		if err := checkExpr(g); err != nil {
			return err
		}
	}
	if err := checkExpr(s.Having); err != nil {
		return err
	}
	for _, ob := range s.OrderBy {
		if err := checkExpr(ob.Expr); err != nil {
			return err
		}
	}
	return nil
}

// collectJoinMergedColumns collects the names of columns merged by USING and
// NATURAL joins in a SELECT. A USING(col) or NATURAL join collapses the named
// columns from both sides into a single result column, so a bare reference to
// that column is NOT ambiguous (SQLite join semantics; vtab6/join tests:
// "SELECT b FROM t1 NATURAL JOIN t2" returns the merged b).
func (e *Engine) collectJoinMergedColumns(s *sql.SelectStmt, names map[string]bool, merged map[string]bool) {
	if s == nil {
		return
	}
	// Track the accumulated left-side column sets for NATURAL join merging.
	var leftColSets [][]string
	addLeft := func(tn string) {
		if tn == "" {
			return
		}
		cols, err := e.tableColumnNames(tn)
		if err != nil {
			return
		}
		leftColSets = append(leftColSets, cols)
	}
	if s.From.Name != "" || s.From.As != "" {
		tn := s.From.Name
		if s.From.As != "" {
			tn = s.From.As
		}
		addLeft(tn)
	}
	for _, join := range s.Joins {
		tn := join.Table.Name
		if join.Table.As != "" {
			tn = join.Table.As
		}
		rightCols, err := e.tableColumnNames(tn)
		if err != nil {
			addLeft(tn)
			continue
		}
		if len(join.Using) > 0 {
			for _, uc := range join.Using {
				merged[strings.ToLower(uc)] = true
			}
		} else if isNaturalJoinType(join.JoinType) {
			// NATURAL: merge columns common to the accumulated left and right.
			rightSet := make(map[string]bool)
			for _, c := range rightCols {
				rightSet[strings.ToLower(c)] = true
			}
			for _, leftSet := range leftColSets {
				for _, c := range leftSet {
					if rightSet[strings.ToLower(c)] {
						merged[strings.ToLower(c)] = true
					}
				}
			}
		}
		leftColSets = append(leftColSets, rightCols)
	}
}

// validateJoinOnClauses checks that each join's ON clause only references
// tables that have already been joined (to its left). SQLite raises
// "ON clause references tables to its right" otherwise. OUTER joins always
// require this; when the query contains a RIGHT or FULL join, every join's ON
// is validated (RIGHT/FULL forces strict left-to-right processing).
func (e *Engine) validateJoinOnClauses(s *sql.SelectStmt) error {
	hasRightOrFull := false
	for _, j := range s.Joins {
		if joinTypeHas(j.JoinType, "RIGHT") || joinTypeHas(j.JoinType, "FULL") {
			hasRightOrFull = true
			break
		}
	}
	available := map[string]bool{}
	availableCols := map[string]bool{}
	// Output column aliases (SELECT expr AS b) are resolvable in ON clauses
	// (SQLite: SELECT a,(+a)b FROM t1 LEFT JOIN v1a ON z=b resolves b to the
	// (+a) expression).
	for _, col := range s.Columns {
		if col.As != "" {
			availableCols[col.As] = true
		}
	}
	// leftTables tracks the tables joined so far and their column names, for
	// the RIGHT/FULL NATURAL/USING ambiguity check.
	type tableCols struct {
		cols map[string]bool
	}
	leftTables := []tableCols{}
	addLeftTable := func(tn string) {
		if tn == "" {
			return
		}
		cols := map[string]bool{}
		if names, err := e.tableColumnNames(tn); err == nil {
			for _, n := range names {
				cols[n] = true
			}
		}
		leftTables = append(leftTables, tableCols{cols: cols})
	}
	if s.From.Name != "" || s.From.As != "" {
		tn := s.From.Name
		if s.From.As != "" {
			tn = s.From.As
		}
		if tn != "" {
			available[tn] = true
			// Column names come from the REAL table (the alias may not be a
			// schema-resolvable name), and are also available under the alias.
			colNames := s.From.Name
			if colNames == "" {
				colNames = tn
			}
			if names, err := e.tableColumnNames(colNames); err == nil {
				for _, n := range names {
					availableCols[n] = true
				}
			}
			addLeftTable(tn)
		}
	}
	// A base FROM subquery contributes its output columns (e.g. FROM
	// (SELECT c+333 AS y FROM t1) RIGHT JOIN ... ON x=y resolves y), and its
	// inner FROM tables' columns resolve through it (ON x with FROM (SELECT *
	// FROM y1) LEFT JOIN y2).
	if s.From.Subquery != nil {
		for _, col := range s.From.Subquery.Columns {
			if col.As != "" {
				availableCols[col.As] = true
			} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
				availableCols[ref.Name] = true
			}
		}
		e.addSubqueryFromCols(s.From.Subquery, availableCols)
	}
	for _, join := range s.Joins {
		tn := join.Table.Name
		if join.Table.As != "" {
			tn = join.Table.As
		}
		if tn != "" {
			available[tn] = true
			// The current right table's columns are resolvable from its ON
			// clause (ON c for t2 FULL JOIN t1 resolves c to t1). For CTEs,
			// use the CTE's declared column names. Column names come from the
			// REAL table (an alias may not be schema-resolvable).
			colNames := join.Table.Name
			if colNames == "" {
				colNames = tn
			}
			if names, err := e.tableColumnNames(colNames); err == nil {
				for _, n := range names {
					availableCols[n] = true
				}
			} else if cteDef, ok := e.findCTE(s, join.Table.Name); ok {
				if len(cteDef.Columns) > 0 {
					for _, n := range cteDef.Columns {
						availableCols[n] = true
					}
				} else if cteDef.Select != nil {
					// A VALUES CTE body exposes column1..columnN (no column
					// aliases, no ColumnRefs); resolve the actual output names.
					if cols, err := e.resolveTableColumnNames(s, join.Table.Name); err == nil {
						for _, n := range cols {
							availableCols[n] = true
						}
					} else {
						for _, col := range cteDef.Select.Columns {
							if col.As != "" {
								availableCols[col.As] = true
							} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
								availableCols[ref.Name] = true
							}
						}
					}
				}
			}
		}
		// A derived table (subquery) contributes its inner table names: an
		// ON clause may reference any table visible inside the subquery
		// (e.g. FROM t2 LEFT JOIN (dual JOIN t1 ON true) ON b=c references
		// t1's c, which lives inside the right-side subquery).
		if join.Table.Subquery != nil {
			collectFromTableNames(join.Table.Subquery, available)
			// The subquery's result columns are also resolvable via the alias
			// (ON (x=z) where z is the subquery's output column z), and the
			// inner FROM tables' columns resolve through the derived table
			// (ON c=b references t2.c inside (SELECT * FROM t2, t3)).
			for _, col := range join.Table.Subquery.Columns {
				if col.As != "" {
					availableCols[col.As] = true
				} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
					availableCols[ref.Name] = true
				}
			}
			e.addSubqueryFromCols(join.Table.Subquery, availableCols)
		}
		// NATURAL/USING ambiguity: with a RIGHT/FULL join present, a USING (or
		// NATURAL) column that appears in TWO or more tables on the left side
		// (outside a prior USING) is ambiguous — SQLite errors with
		// "ambiguous reference to X in USING()".
		if hasRightOrFull && (len(join.Using) > 0 || isNaturalJoinType(join.JoinType)) {
			var usingCols []string
			if len(join.Using) > 0 {
				usingCols = join.Using
			} else {
				// NATURAL: common columns between the accumulated left and the
				// current right table.
				rightCols := map[string]bool{}
				if names, err := e.tableColumnNames(tn); err == nil {
					for _, n := range names {
						rightCols[n] = true
					}
				}
				for _, tc := range leftTables {
					for c := range tc.cols {
						if rightCols[c] {
							usingCols = append(usingCols, c)
						}
					}
				}
			}
			for _, uc := range usingCols {
				count := 0
				for _, tc := range leftTables {
					if tc.cols[uc] {
						count++
					}
				}
				if count > 1 {
					return fmt.Errorf("ambiguous reference to %s in USING()", uc)
				}
			}
		}

		// Only OUTER joins restrict ON-clause references to their left.
		// With a RIGHT/FULL join present, all joins are validated.
		// Record this join's table as available for the NEXT join's checks
		// before the ON validation may short-circuit. After a USING/NATURAL
		// join, the merged columns collapse into a single accumulated table
		// (SQLite merges them, so a later USING on the same column is not
		// ambiguous).
		if len(join.Using) > 0 || isNaturalJoinType(join.JoinType) {
			merged := map[string]bool{}
			for _, tc := range leftTables {
				for c := range tc.cols {
					merged[c] = true
				}
			}
			if names, err := e.tableColumnNames(tn); err == nil {
				for _, n := range names {
					merged[n] = true
				}
			}
			leftTables = []tableCols{{cols: merged}}
		} else {
			addLeftTable(tn)
		}
		if join.On == nil || (!joinTypeHas(join.JoinType, "LEFT") && !joinTypeHas(join.JoinType, "RIGHT") && !hasRightOrFull) {
			continue
		}
		var bad string
		walkJoinOnExpr(join.On, func(e2 sql.Expr) {
			if cr, ok := e2.(*sql.ColumnRef); ok && cr.Table != "" && !available[cr.Table] {
				bad = cr.Table
			}
		})
		// Subqueries in the ON clause may reference their OWN FROM tables OR
		// the outer tables joined so far; a reference to an outer table that
		// comes later (to the right) is an error (SQLite: ON (SELECT 1 FROM
		// t2 RIGHT JOIN t3 ON t4.y) CROSS JOIN t4 rejects t4.y).
		if bad == "" {
			walkJoinOnExpr(join.On, func(e2 sql.Expr) {
				switch e2.(type) {
				case *sql.Subquery, *sql.ExistsExpr:
					var sel *sql.SelectStmt
					if sub, ok := e2.(*sql.Subquery); ok {
						sel = sub.Select
					} else if ex, ok := e2.(*sql.ExistsExpr); ok {
						sel = ex.Select
					}
					if sel == nil {
						return
					}
					// Local tables of the subquery are always available, and each
					// of the subquery's joins is validated left-to-right (its ON
					// may only reference tables to its left within the subquery
					// or outer tables joined so far).
					local := map[string]bool{}
					collectFromTableNames(sel, local)
					walkSelectJoinExprs(sel, func(e3 sql.Expr) {
						cr, ok := e3.(*sql.ColumnRef)
						if !ok || cr.Table == "" {
							return
						}
						if local[cr.Table] || available[cr.Table] {
							return
						}
						bad = cr.Table
					})
					// Reject references inside the subquery's join ON clauses to
					// tables that are joined LATER within the subquery (e.g. ON
					// max(0,t5.z) CROSS JOIN t5 references t5 before its join).
					subAvail := map[string]bool{}
					if sel.From.Name != "" {
						subAvail[sel.From.Name] = true
						if sel.From.As != "" {
							subAvail[sel.From.As] = true
						}
					}
					for i := range sel.Joins {
						j := &sel.Joins[i]
						jn := j.Table.Name
						if j.Table.As != "" {
							jn = j.Table.As
						}
						if jn != "" {
							subAvail[jn] = true
						}
						if j.On == nil {
							continue
						}
						walkJoinOnExpr(j.On, func(e3 sql.Expr) {
							cr, ok := e3.(*sql.ColumnRef)
							if !ok || cr.Table == "" {
								return
							}
							if subAvail[cr.Table] || available[cr.Table] {
								return
							}
							bad = cr.Table
						})
					}
				}
			})
		}
		if bad == "" && (hasRightOrFull || joinTypeHas(join.JoinType, "LEFT")) {
			// With RIGHT/FULL joins, unqualified ON references must also
			// resolve among the tables joined so far (SQLite resolves them
			// left-to-right and errors if a column belongs to a table that
			// comes later, e.g. t1 JOIN t2 ON d>b RIGHT JOIN t3 where d is
			// t3's column).
			walkJoinOnExpr(join.On, func(e2 sql.Expr) {
				if cr, ok := e2.(*sql.ColumnRef); ok && cr.Table == "" && !availableCols[cr.Name] {
					// TRUE/FALSE are boolean literals, not column references.
					if strings.EqualFold(cr.Name, "TRUE") || strings.EqualFold(cr.Name, "FALSE") {
						return
					}
					bad = cr.Name
				}
			})
		}
		if bad != "" {
			return fmt.Errorf("ON clause references tables to its right")
		}
	}
	return nil
}

// validateOnColumnRefs checks that unqualified column references in a join
// ON clause resolve among the tables joined so far.
func validateOnColumnRefs(on sql.Expr, names map[string]bool) error {
	if on == nil {
		return nil
	}
	var bad bool
	walkJoinOnExpr(on, func(e2 sql.Expr) {
		if cr, ok := e2.(*sql.ColumnRef); ok && cr.Table == "" {
			n := cr.Name
			if n == "*" || n == "rowid" || n == "oid" || n == "_rowid_" {
				return
			}
			// TRUE/FALSE are boolean literals, not column references
			// (the parser represents them as unqualified ColumnRefs).
			if strings.EqualFold(n, "TRUE") || strings.EqualFold(n, "FALSE") {
				return
			}
			if !names[n] {
				bad = true
			}
		}
	})
	if bad {
		return fmt.Errorf("ON clause references tables to its right")
	}
	return nil
}

// walkJoinOnExpr visits the direct references of a join ON expression,
// descending into function arguments but not into subqueries (which have
// their own table scope).
func walkJoinOnExpr(expr sql.Expr, fn func(sql.Expr)) {
	if expr == nil {
		return
	}
	fn(expr)
	switch e := expr.(type) {
	case *sql.ParenExpr:
		walkJoinOnExpr(e.Expr, fn)
	case *sql.BinaryOp:
		walkJoinOnExpr(e.Left, fn)
		walkJoinOnExpr(e.Right, fn)
	case *sql.UnaryOp:
		walkJoinOnExpr(e.Operand, fn)
	case *sql.Between:
		walkJoinOnExpr(e.Operand, fn)
		walkJoinOnExpr(e.Low, fn)
		walkJoinOnExpr(e.High, fn)
	case *sql.InList:
		walkJoinOnExpr(e.Operand, fn)
		for _, item := range e.List {
			walkJoinOnExpr(item, fn)
		}
	case *sql.FuncCall:
		for _, a := range e.Args {
			walkJoinOnExpr(a, fn)
		}
		for _, ob := range e.OrderBy {
			walkJoinOnExpr(ob.Expr, fn)
		}
	case *sql.CaseExpr:
		if e.Operand != nil {
			walkJoinOnExpr(e.Operand, fn)
		}
		for _, w := range e.Whens {
			walkJoinOnExpr(w.When, fn)
			walkJoinOnExpr(w.Then, fn)
		}
		if e.Else != nil {
			walkJoinOnExpr(e.Else, fn)
		}
	case *sql.CastExpr:
		walkJoinOnExpr(e.Operand, fn)
	case *sql.IsNull:
		walkJoinOnExpr(e.Operand, fn)
	case *sql.IsNotNull:
		walkJoinOnExpr(e.Operand, fn)
	case *sql.IsTrue:
		walkJoinOnExpr(e.Operand, fn)
	case *sql.IsFalse:
		walkJoinOnExpr(e.Operand, fn)
	case *sql.IsDistinctFrom:
		walkJoinOnExpr(e.Left, fn)
		walkJoinOnExpr(e.Right, fn)
	case *sql.IsNotDistinctFrom:
		walkJoinOnExpr(e.Left, fn)
		walkJoinOnExpr(e.Right, fn)
	case *sql.RowValue:
		for _, v := range e.Values {
			walkJoinOnExpr(v, fn)
		}
	case *sql.Subquery:
		// Visit the subquery node itself (so ON-clause validation can inspect
		// its inner references) but do NOT descend into its children here:
		// their references resolve against the subquery's own FROM tables
		// first (handled in validateJoinOnClauses with local-table awareness).
	case *sql.ExistsExpr:
	}
}

// derivedTableBadColumnRef returns the first column reference in a derived
// table (subquery in FROM/JOIN) that does NOT resolve to a table within the
// subquery's own FROM scope. Derived tables cannot be correlated: a reference
// like t6.a inside (SELECT ... FROM t7 JOIN t8 ON t6.a) is "no such column".
func derivedTableBadColumnRef(s *sql.SelectStmt) string {
	if s == nil {
		return ""
	}
	local := map[string]bool{}
	collectFromTableNames(s, local)
	bad := ""
	walkSelectJoinExprs(s, func(e sql.Expr) {
		if bad != "" {
			return
		}
		cr, ok := e.(*sql.ColumnRef)
		if !ok || cr.Table == "" {
			return
		}
		if !local[cr.Table] {
			bad = cr.Table + "." + cr.Name
		}
	})
	return bad
}

// subqueryColumnAffinities returns the affinity rune for each output column of
// a subquery's SELECT, derived from the expression (CAST, column refs, etc.).
// A zero rune means "unknown" (caller falls back to the column def type).
func subqueryColumnAffinities(s *sql.SelectStmt) []rune {
	if s == nil {
		return nil
	}
	affs := make([]rune, len(s.Columns))
	for i, col := range s.Columns {
		affs[i] = exprAffinitySimple(col.Expr)
	}
	return affs
}

// exprAffinitySimple computes a coarse affinity for an expression used as a
// subquery output column (CAST and numeric/string literals). Returns 0 when
// the affinity cannot be determined simply.
func exprAffinitySimple(e sql.Expr) rune {
	switch v := e.(type) {
	case *sql.CastExpr:
		return util.Affinity(v.AsType)
	case *sql.NumericLit:
		return 'R' // numeric literals behave like REAL in SQLite
	case *sql.ColumnRef:
		return 0 // resolved through the table at runtime
	default:
		return 0
	}
}

// compoundColumnAffinity returns the affinity of output column i of a compound
// SELECT (UNION/INTERSECT/EXCEPT), matching SQLite's selectAddColumnTypeAnd-
// Collation: each member's expression contributes an affinity (a table column
// contributes its declared affinity; literals contribute their expression
// affinity), and the affinities are merged. A TEXT affinity combined with a
// NUMERIC-family affinity (INTEGER/REAL/NUMERIC) yields BLOB (the affinities
// are incompatible); otherwise the most specific non-BLOB affinity wins
// (INTEGER > REAL > NUMERIC > TEXT, then BLOB). The merge is order-independent,
// matching SQLite (both "lit UNION col" and "col UNION lit" give the same
// result column affinity). Returns 0 when the affinity cannot be determined
// (no members / unknown).
func (e *Engine) compoundColumnAffinity(s *sql.SelectStmt, i int) rune {
	var affs []rune
	for cur := s; cur != nil; cur = cur.Union {
		if i >= len(cur.Columns) {
			continue
		}
		affs = append(affs, e.memberExprAffinity(cur, i))
	}
	if len(affs) == 0 {
		return 0
	}
	hasText := false
	hasNumeric := false
	best := rune(0)
	for _, a := range affs {
		switch a {
		case 'T':
			hasText = true
		case 'I', 'R', 'N':
			hasNumeric = true
		}
		if affinityPrecedence(a) > affinityPrecedence(best) {
			best = a
		}
	}
	if hasText && hasNumeric {
		return 'B'
	}
	return best
}

// affinityPrecedence orders affinities so the most specific column affinity
// wins when merging compound members: INTEGER > REAL > NUMERIC > TEXT > BLOB.
func affinityPrecedence(a rune) int {
	switch a {
	case 'I':
		return 5
	case 'R':
		return 4
	case 'N':
		return 3
	case 'T':
		return 2
	case 'B':
		return 1
	default:
		return 0
	}
}

// memberExprAffinity returns the affinity contribution of a compound member's
// expression at column index i: a column reference contributes its declared
// column affinity (resolved through the member's FROM table), literals
// contribute their expression affinity (string → TEXT, numeric → NUMERIC,
// blob → BLOB, NULL → BLOB/neutral), and CAST contributes its target type.
// Returns 0 (unknown → treated as BLOB) for everything else.
func (e *Engine) memberExprAffinity(member *sql.SelectStmt, i int) rune {
	if member == nil || i >= len(member.Columns) {
		return 0
	}
	expr := member.Columns[i].Expr
	switch v := expr.(type) {
	case *sql.ColumnRef:
		if v.Name == "*" {
			return 0
		}
		if aff, ok := e.memberFromColumnAffinity(member, v.Name); ok {
			return aff
		}
		return 0
	case *sql.CastExpr:
		return util.Affinity(v.AsType)
	case *sql.NumericLit:
		return 'N'
	case *sql.StringLit:
		return 'T'
	case *sql.BlobLit:
		return 'B'
	case *sql.NullLit:
		return 'B'
	default:
		return 0
	}
}

// memberFromColumnAffinity resolves a column reference in a compound member
// against the member's FROM table (or, for a member whose FROM is a subquery,
// the subquery's output column affinity). Returns (aff, true) when the
// column's declared affinity is known.
func (e *Engine) memberFromColumnAffinity(member *sql.SelectStmt, colName string) (rune, bool) {
	if member == nil || member.From.Name == "" {
		return 0, false
	}
	entry, _, err := e.findTable(member.From.Name)
	if err != nil {
		return 0, false
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, colName) {
			aff := util.Affinity(cd.Type)
			return aff, true
		}
	}
	return 0, false
}

// walkSelectJoinExprs walks a SELECT's columns, WHERE, HAVING, and each join's
// ON expression, used to detect outer-table references inside subqueries of a
// join ON clause.
func walkSelectJoinExprs(s *sql.SelectStmt, fn func(sql.Expr)) {
	if s == nil {
		return
	}
	for _, col := range s.Columns {
		walkJoinOnExpr(col.Expr, fn)
	}
	walkJoinOnExpr(s.Where, fn)
	walkJoinOnExpr(s.Having, fn)
	for i := range s.Joins {
		walkJoinOnExpr(s.Joins[i].On, fn)
	}
	for _, g := range s.GroupBy {
		walkJoinOnExpr(g, fn)
	}
	for _, ob := range s.OrderBy {
		walkJoinOnExpr(ob.Expr, fn)
	}
}

// pkColumnNames extracts the column names for the PRIMARY KEY of a
// WITHOUT ROWID table from the CREATE TABLE SQL. It supports both
// column-level PK (e.g., "a INTEGER PRIMARY KEY") and table-level PK
// (e.g., "PRIMARY KEY(c,a)"). Returns the names in PK order.
func pkColumnNames(createSQL string, colDefs []sql.ColumnDef) []string {
	// First check for table-level PRIMARY KEY(col1, col2, ...)
	upper := strings.ToUpper(createSQL)
	pkStart := strings.Index(upper, "PRIMARY KEY")
	if pkStart < 0 {
		pkStart = strings.Index(upper, "PRIMARY  KEY")
	}
	if pkStart >= 0 {
		// Find the opening parenthesis after PRIMARY KEY
		parenStart := strings.Index(createSQL[pkStart:], "(")
		if parenStart >= 0 {
			parenStart += pkStart
			parenEnd := strings.Index(createSQL[parenStart:], ")")
			if parenEnd >= 0 {
				parenEnd += parenStart
				colPart := createSQL[parenStart+1 : parenEnd]
				colNames := strings.Split(colPart, ",")
				var result []string
				for _, cn := range colNames {
					name := strings.TrimSpace(cn)
					fields := strings.Fields(name)
					if len(fields) > 0 {
						result = append(result, fields[0])
					}
				}
				if len(result) > 0 {
					return result
				}
			}
		}
	}

	// Fallback: column-level PRIMARY KEY
	for _, cd := range colDefs {
		if cd.PrimaryKey {
			return []string{cd.Name}
		}
	}

	return nil
}

// sortRowMapsByPKNames sorts rowMaps by the PK column values in ascending order.
// pkColNames is the ordered list of PK column names to sort by.
func sortRowMapsByPKNames(rowMaps []RowMap, pkColNames []string) {
	if len(rowMaps) <= 1 {
		return
	}
	sort.SliceStable(rowMaps, func(a, b int) bool {
		for _, name := range pkColNames {
			va := util.UnwrapColumnValue(rowMaps[a][name])
			vb := util.UnwrapColumnValue(rowMaps[b][name])
			cmp := util.CompareValues(va, vb)
			if cmp != 0 {
				return cmp < 0
			}
		}
		return false
	})
}

// checkWhereCollations walks a WHERE expression and raises "no such collation
// sequence: X" when a comparison operand is a column reference whose declared
// collation (from the scanned table) is unknown. Mirrors SQLite's compile-time
// collation resolution for comparison operators.
func (e *Engine) checkWhereCollations(where sql.Expr, colDefs []sql.ColumnDef, from sql.TableRef) error {
	colByName := make(map[string]string, len(colDefs))
	for _, c := range colDefs {
		if c.Collate != "" {
			colByName[strings.ToLower(c.Name)] = c.Collate
		}
	}
	refName := from.Name
	if from.As != "" {
		refName = from.As
	}
	var checkErr error
	walkExprFull(where, func(e2 sql.Expr) {
		if checkErr != nil {
			return
		}
		bop, ok := e2.(*sql.BinaryOp)
		if !ok {
			return
		}
		switch strings.ToUpper(bop.Operator) {
		case "=", "<>", "!=", "<", ">", "<=", ">=":
		default:
			return
		}
		for _, side := range []sql.Expr{bop.Left, bop.Right} {
			cr, ok := side.(*sql.ColumnRef)
			if !ok {
				continue
			}
			// A table qualifier must match the scanned table (or its alias);
			// an empty qualifier resolves by column name in the single-table case.
			if cr.Table != "" && !strings.EqualFold(cr.Table, refName) && !strings.EqualFold(cr.Table, from.Name) {
				continue
			}
			if coll, ok := colByName[strings.ToLower(cr.Name)]; ok {
				if err := e.checkCollationString(coll); err != nil {
					checkErr = err
					return
				}
			}
		}
	})
	return checkErr
}

// findRowIDRef returns the first rowid/_rowid_/oid column reference in a
// SELECT statement that resolves to the named table, or "" if there is none.
// Used to reject rowid references on WITHOUT ROWID tables. References
// qualified to a different table (e.g. t42.rowid in a join) are allowed, as
// are unqualified references when the query joins other tables that may
// provide a rowid.
func (e *Engine) findRowIDRef(s *sql.SelectStmt, tableName, alias string, hasJoins bool) string {
	check := func(expr sql.Expr) string {
		var found string
		walkExprFull(expr, func(e2 sql.Expr) {
			if found != "" {
				return
			}
			cr, ok := e2.(*sql.ColumnRef)
			if !ok || !isRowIDName(cr.Name) {
				return
			}
			// Qualified reference to another table: that table's rowid.
			if cr.Table != "" && !strings.EqualFold(cr.Table, tableName) && !strings.EqualFold(cr.Table, alias) {
				return
			}
			// Unqualified reference with joins may resolve to another table.
			if cr.Table == "" && hasJoins {
				return
			}
			found = cr.Name
		})
		return found
	}
	for _, col := range s.Columns {
		if ref := check(col.Expr); ref != "" {
			return ref
		}
	}
	if s.Where != nil {
		if ref := check(s.Where); ref != "" {
			return ref
		}
	}
	for _, ob := range s.OrderBy {
		if ref := check(ob.Expr); ref != "" {
			return ref
		}
	}
	for _, gb := range s.GroupBy {
		if ref := check(gb); ref != "" {
			return ref
		}
	}
	if s.Having != nil {
		if ref := check(s.Having); ref != "" {
			return ref
		}
	}
	return ""
}

// walkExprFull visits every node in an expression tree, descending into all
// expression node types.
func walkExprFull(expr sql.Expr, fn func(sql.Expr)) {
	if expr == nil {
		return
	}
	fn(expr)
	switch e := expr.(type) {
	case *sql.ParenExpr:
		walkExprFull(e.Expr, fn)
	case *sql.BinaryOp:
		walkExprFull(e.Left, fn)
		walkExprFull(e.Right, fn)
	case *sql.UnaryOp:
		walkExprFull(e.Operand, fn)
	case *sql.FuncCall:
		for _, arg := range e.Args {
			walkExprFull(arg, fn)
		}
	case *sql.CaseExpr:
		walkExprFull(e.Operand, fn)
		for _, w := range e.Whens {
			walkExprFull(w.When, fn)
			walkExprFull(w.Then, fn)
		}
		walkExprFull(e.Else, fn)
	case *sql.CastExpr:
		walkExprFull(e.Operand, fn)
	case *sql.InList:
		walkExprFull(e.Operand, fn)
		for _, item := range e.List {
			walkExprFull(item, fn)
		}
	case *sql.IsNull:
		walkExprFull(e.Operand, fn)
	case *sql.IsNotNull:
		walkExprFull(e.Operand, fn)
	case *sql.IsTrue:
		walkExprFull(e.Operand, fn)
	case *sql.IsFalse:
		walkExprFull(e.Operand, fn)
	case *sql.IsDistinctFrom:
		walkExprFull(e.Left, fn)
		walkExprFull(e.Right, fn)
	case *sql.IsNotDistinctFrom:
		walkExprFull(e.Left, fn)
		walkExprFull(e.Right, fn)
	case *sql.Between:
		walkExprFull(e.Operand, fn)
		walkExprFull(e.Low, fn)
		walkExprFull(e.High, fn)
	}
}

// hasRowIDRef reports whether expr references rowid, _rowid_, or oid.
func hasRowIDRef(expr sql.Expr) bool {
	found := false
	walkExprFull(expr, func(e2 sql.Expr) {
		if found {
			return
		}
		if cr, ok := e2.(*sql.ColumnRef); ok && isRowIDName(cr.Name) {
			found = true
		}
	})
	return found
}
