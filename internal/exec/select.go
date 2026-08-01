// Package exec implements query execution.
package exec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
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

	groupBy := resolveGroupByOrdinals(s)
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
		// Apply HAVING filter
		if s.Having != nil {
			match, err := e.evalHaving(s.Having, groupRows)
			if err != nil || !match {
				continue
			}
		}
		// Use the first row of the group as the representative for non-aggregated columns
		row := groupRows[0]
		outRow := e.buildOutputRow(s.Columns, colDefs, row)
		outRows = append(outRows, outRow)
	}

	columns := e.buildColumnNames(s.Columns, colDefs)
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
	// Validate expressions before executing: check for invalid ORDER BY usage and
	// aggregates inside UNION ALL in subqueries.
	if err := e.validateSelectExprs(s); err != nil {
		return &Result{Error: err}
	}

	// Handle SELECT without FROM (e.g., SELECT 1, SELECT CASE...)
	if s.From.Name == "" && s.From.Subquery == nil && len(s.From.As) == 0 {
		return e.execSelectNoFrom(s)
	}

	// Handle subquery in FROM: (SELECT ...) AS t
	if s.From.Subquery != nil {
		return e.execSelectFromSubquery(s)
	}

	// Handle CTE: check if the from table matches a CTE definition
	for _, cte := range s.CTEs {
		if cte.Name == s.From.Name || cte.Name == s.From.As {
			return e.execSelectCTE(s, &cte)
		}
	}

	// Table-valued pragma functions: FROM pragma_table_info('t1')
	if isPragmaTableFunc(s.From.Name) {
		return e.execPragmaTableValued(s)
	}

	tableEntry, dbCtx, err := e.findTable(s.From.Name)
	if err != nil {
		viewEntry, _, viewErr := e.findView(s.From.Name)
		if viewErr != nil {
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
		result := e.execSelectViewWithOuter(s, viewEntry)
		delete(e.resolvingViews, s.From.Name)
		return result
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// Check if this is a virtual table (RootPage = 0)
	if tableEntry.RootPage == 0 {
		// For FTS virtual tables, use full SELECT processing (WHERE, ORDER BY, LIMIT)
		if ftsTable, ok := e.ftsTables[tableEntry.Name]; ok {
			return e.execFTSSelect(s, tableEntry, ftsTable, colDefs)
		}
		// Non-FTS virtual tables: return all rows directly (no WHERE/ORDER BY)
		rows, err := e.virtualTableRows(tableEntry)
		if err != nil {
			return &Result{Error: err}
		}
		result := &Result{
			Columns: e.buildColumnNames(s.Columns, colDefs),
			Rows:    rows,
		}
		return result
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

	allRows, allRowMaps := e.scanTableRows(cursor, s, colDefs, needMaps)

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
			columns := e.buildColumnNames(s.Columns, colDefs)
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
		columns := e.buildColumnNames(s.Columns, colDefs)
		result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
		return e.finalizeSelectResult(result, s, allRowMaps)
	}

	// If there are JOINs, process them (nested-loop join)
	if len(s.Joins) > 0 {
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
				if e.rowPassesWhere(s.Where, rowMap, nil) {
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
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: allRows}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// finalizeSelectResult applies DISTINCT, ORDER BY, LIMIT, and UNION.
func (e *Engine) finalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result {
	if s.Distinct {
		result.Rows, rowMaps = e.distinctRows(result.Rows, rowMaps)
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
				if memberResult.Error == nil {
					result.Rows = applySetOp(result.Rows, memberResult.Rows, cur.SetOp, cur.UnionAll)
				}
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
			memberResult := e.execSelect(&memberCopy)
			if memberResult.Error == nil {
				result.Rows = applySetOp(result.Rows, memberResult.Rows, cur.SetOp, cur.UnionAll)
			}
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
		e.sortRowsWithMaps(result, orderBy, rowMaps)
	}
	result.Rows = applyLimitOffset(result.Rows, limit, offset)
	return result
}

func (e *Engine) mergeUnionRows(rows [][]interface{}, union *sql.SelectStmt, op sql.SetOp, unionAll bool) [][]interface{} {
	unionResult := e.execSelect(union)
	if unionResult.Error != nil {
		return rows
	}
	return applySetOp(rows, unionResult.Rows, op, unionAll)
}

// applySetOp combines left and right row sets with a compound set operator.
func applySetOp(rows, rightRows [][]interface{}, op sql.SetOp, unionAll bool) [][]interface{} {
	switch op {
	case sql.SetUnion:
		if unionAll {
			// UNION ALL: concatenate without dedup
			return append(rows, rightRows...)
		}
		// UNION: deduplicate combined rows
		return dedupeRows(append(rows, rightRows...))
	case sql.SetIntersect:
		// INTERSECT: rows that appear in both sets
		return intersectRows(rows, rightRows)
	case sql.SetExcept:
		// EXCEPT: rows in left but not in right
		return exceptRows(rows, rightRows)
	default:
		return append(rows, rightRows...)
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
func dedupeRows(rows [][]interface{}) [][]interface{} {
	if len(rows) == 0 {
		return rows
	}
	seen := make(map[string]bool)
	var result [][]interface{}
	for _, row := range rows {
		key := rowKey(row)
		if !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
}

// intersectRows returns rows that exist in both a and b (INTERSECT).
func intersectRows(a, b [][]interface{}) [][]interface{} {
	if len(a) == 0 || len(b) == 0 {
		return [][]interface{}{}
	}
	// Build set of b rows
	bSet := make(map[string]bool)
	for _, row := range b {
		bSet[rowKey(row)] = true
	}
	// Find a rows that are also in b
	var result [][]interface{}
	seen := make(map[string]bool)
	for _, row := range a {
		key := rowKey(row)
		if bSet[key] && !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
}

// exceptRows returns rows in a that are not in b (EXCEPT).
func exceptRows(a, b [][]interface{}) [][]interface{} {
	if len(a) == 0 {
		return [][]interface{}{}
	}
	bSet := make(map[string]bool)
	for _, row := range b {
		bSet[rowKey(row)] = true
	}
	var result [][]interface{}
	seen := make(map[string]bool)
	for _, row := range a {
		key := rowKey(row)
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
func rowKey(row []interface{}) string {
	parts := make([]string, len(row))
	for i, v := range row {
		if v == nil {
			parts[i] = "\x00"
		} else {
			switch x := v.(type) {
			case int64:
				parts[i] = "i:" + strconv.FormatInt(x, 10)
			case float64:
				parts[i] = "f:" + strconv.FormatFloat(x, 'g', -1, 64)
			case string:
				parts[i] = "s:" + x
			case []byte:
				parts[i] = "b:" + string(x)
			default:
				parts[i] = "o:" + fmt.Sprintf("%v", x)
			}
		}
	}
	return strings.Join(parts, "\x00")
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
	if !strings.HasPrefix(trimmedUpper, "SELECT") && !strings.HasPrefix(trimmedUpper, "WITH") {
		return &Result{Error: fmt.Errorf("exec: view does not contain SELECT: %s", sqlStr)}
	}
	// Check for circular view references before expanding
	if hasViewCircularRef(sqlStr, entry.Name) {
		return &Result{Error: fmt.Errorf("view %s is circularly defined", entry.Name)}
	}
	parser := sql.NewParser(selectSQL)
	stmts := parser.Parse()
	if parser.Err() != nil || len(stmts) == 0 {
		return &Result{Error: fmt.Errorf("exec: view parse error: %v", parser.Err())}
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok {
		return e.execSelect(sel)
	}
	return &Result{Error: fmt.Errorf("exec: view does not contain SELECT")}
}

// execSelectViewWithOuter executes a view and applies the outer SELECT's
// column expressions, aggregates, ORDER BY, etc. on the view's result.
func (e *Engine) execSelectViewWithOuter(s *sql.SelectStmt, viewEntry *schema.Entry) *Result {
	viewResult := e.execSelectView(viewEntry)
	if viewResult.Error != nil {
		return viewResult
	}
	// Build colDefs from view result's column names, preferring the view's
	// declared column list (CREATE VIEW v(c0, c1) AS ...) when present.
	var viewColDefs []sql.ColumnDef
	if declared := viewDeclaredColumns(viewEntry.SQL); len(declared) > 0 {
		for _, colName := range declared {
			viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
		}
	} else {
		for _, colName := range viewResult.Columns {
			viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
		}
	}
	// Build rowMaps from view result rows for expression evaluation
	var rowMaps []RowMap
	for _, row := range viewResult.Rows {
		rowMap := make(RowMap)
		for i, val := range row {
			if i < len(viewColDefs) {
				rowMap[viewColDefs[i].Name] = val
			}
		}
		rowMaps = append(rowMaps, rowMap)
	}

	// Handle outer JOINs: the outer query may join the view against other
	// tables/views (e.g. FROM v1 RIGHT JOIN t ON v1.x=t.x).
	if len(s.Joins) > 0 {
		var err error
		rowMaps, viewColDefs, err = e.execJoins(s, rowMaps, viewColDefs)
		if err != nil {
			return &Result{Error: err}
		}
		if s.Where != nil {
			filtered := rowMaps[:0]
			for _, rowMap := range rowMaps {
				if e.rowPassesWhere(s.Where, rowMap, nil) {
					filtered = append(filtered, rowMap)
				}
			}
			rowMaps = filtered
		}
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
		Columns: e.buildColumnNames(s.Columns, viewColDefs),
		Rows:    allRows,
	}
	return e.finalizeSelectResult(result, s, rowMaps)
}

// execSelectNoFrom handles SELECT without FROM clause.
func (e *Engine) execSelectNoFrom(s *sql.SelectStmt) *Result {
	columns := e.buildColumnNames(s.Columns, nil)

	// Validate positional ORDER BY terms against the number of result columns.
	if len(s.OrderBy) > 0 {
		if err := validateOrderBy(s.OrderBy, len(columns)); err != nil {
			return &Result{Error: err}
		}
	}

	// Apply WHERE filter for FROM-less SELECT
	if s.Where != nil {
		// Use nil row since there are no columns to reference
		pass := e.rowPassesWhere(s.Where, nil, nil)
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
			outRow = append(outRow, v)
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
				rows = applySetOp(rows, memberResult.Rows, cur.SetOp, cur.UnionAll)
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
			rows = applySetOp(rows, memberResult.Rows, cur.SetOp, cur.UnionAll)
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

	// Build colDefs from subquery column names
	colDefs := make([]sql.ColumnDef, len(subqResult.Columns))
	for i, col := range subqResult.Columns {
		colDefs[i] = sql.ColumnDef{Name: col}
	}

	return e.execSelectOverMaterialized(s, colDefs, subqResult.Rows)
}

// execSelectOverMaterialized runs the outer SELECT pipeline (WHERE, JOINs,
// aggregates, projection, DISTINCT, ORDER BY, LIMIT/OFFSET, UNION) over a
// materialized set of rows with known column definitions. It is shared by
// subquery-in-FROM execution and table-valued pragma functions.
func (e *Engine) execSelectOverMaterialized(s *sql.SelectStmt, colDefs []sql.ColumnDef, rows [][]interface{}) *Result {
	// Build rowMaps from result rows
	allRows := rows
	if len(allRows) == 0 {
		return &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: [][]interface{}{}}
	}
	allRowMaps := make([]RowMap, len(allRows))
	for i, row := range allRows {
		rowMap := make(RowMap)
		for j, val := range row {
			if j < len(colDefs) {
				rowMap[colDefs[j].Name] = val
			}
		}
		allRowMaps[i] = rowMap
	}

	// Apply WHERE filter. When the query has JOINs, the WHERE may reference
	// joined tables (e.g. t2.a=t3.a), so it cannot be applied before the
	// join — the post-join filter below handles it.
	if len(s.Joins) == 0 {
		_, allRowMaps = e.filterSubqueryRows(allRows, allRowMaps, s.Where)
	}

	// Handle outer JOINs: the outer query may join the subquery against other
	// tables/views (e.g. FROM (SELECT ...) v RIGHT JOIN t ON v.x=t.x).
	if len(s.Joins) > 0 {
		var err error
		allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
		if err != nil {
			return &Result{Error: err}
		}
		if s.Where != nil {
			filtered := allRowMaps[:0]
			for _, rowMap := range allRowMaps {
				if e.rowPassesWhere(s.Where, rowMap, nil) {
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

	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: allRows}

	// Apply DISTINCT
	if s.Distinct {
		result.Rows, allRowMaps = e.distinctRows(result.Rows, allRowMaps)
	}

	// Apply ORDER BY
	if len(s.OrderBy) > 0 {
		if err := validateOrderBy(s.OrderBy, len(result.Columns)); err != nil {
			return &Result{Error: err}
		}
		e.sortRowsWithMaps(result, s.OrderBy, allRowMaps)
	}

	// Apply LIMIT / OFFSET
	result.Rows = applyLimitOffset(result.Rows, s.Limit, s.Offset)

	// Handle UNION / INTERSECT / EXCEPT
	if s.Union != nil {
		result.Rows = e.mergeUnionRows(result.Rows, s.Union, s.SetOp, s.UnionAll)
	}

	return result
}

// execSelectCTE executes a query that references a CTE definition.
func (e *Engine) execSelectCTE(s *sql.SelectStmt, cte *sql.CTEDef) *Result {
	// Handle recursive CTE (WITH RECURSIVE ...)
	if cte.Select != nil && cte.Select.Union != nil {
		return e.execRecursiveCTE(s, cte)
	}
	// Non-recursive CTE: execute the CTE's SELECT directly
	cteResult := e.execSelect(cte.Select)
	if cteResult.Error != nil {
		return cteResult
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
	allRowMaps := make([]RowMap, len(cteResult.Rows))
	for i, row := range cteResult.Rows {
		allRowMaps[i] = buildRowMapFromValues(row, colDefs, int64(i+1))
	}
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	allRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: allRows}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// execRecursiveCTE executes a recursive CTE (WITH RECURSIVE ...).
// The CTE definition is a UNION ALL with an anchor part and a recursive part.
func (e *Engine) execRecursiveCTE(s *sql.SelectStmt, cte *sql.CTEDef) *Result {
	// Build column definitions from CTE column names
	colDefs := make([]sql.ColumnDef, len(cte.Columns))
	for i, name := range cte.Columns {
		colDefs[i] = sql.ColumnDef{Name: name}
	}
	if len(colDefs) == 0 {
		colDefs = []sql.ColumnDef{{Name: "x"}}
	}

	// Execute the anchor part (the VALUES/SELECT before UNION)
	anchorSelect := *cte.Select
	anchorSelect.Union = nil
	anchorResult := e.execSelect(&anchorSelect)
	if anchorResult.Error != nil {
		return anchorResult
	}

	// Collect all rows (anchor + recursive iterations)
	var allRows [][]interface{}
	allRows = append(allRows, anchorResult.Rows...)

	// Iterate the recursive part until no more rows
	currentRows := anchorResult.Rows
	recursiveSelect := cte.Select.Union
	maxIter := 1000 // SQLite's default recursion limit
	for iter := 0; iter < maxIter; iter++ {
		var newRows [][]interface{}
		for _, row := range currentRows {
			rowMap := buildRowMapFromValues(row, colDefs, int64(len(allRows)+1))

			// Evaluate WHERE clause if present
			if recursiveSelect.Where != nil {
				pass := e.rowPassesWhere(recursiveSelect.Where, rowMap, nil)
				if !pass {
					continue
				}
			}

			// Evaluate column expressions
			outRow := make([]interface{}, len(recursiveSelect.Columns))
			for i, col := range recursiveSelect.Columns {
				val, err := e.evalExpr(col.Expr, rowMap)
				if err != nil {
					return &Result{Error: err}
				}
				outRow[i] = val
			}
			newRows = append(newRows, outRow)
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
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}

	// Build output rows
	outRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		outRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: outRows}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// the base table rows and each joined table. Returns combined rowMaps and
// colDefs.

// filterSubqueryRows applies a WHERE expression to filter rows from a subquery result.
func (e *Engine) filterSubqueryRows(allRows [][]interface{}, allRowMaps []RowMap, where sql.Expr) ([][]interface{}, []RowMap) {
	if where == nil {
		return allRows, allRowMaps
	}
	var filteredRows [][]interface{}
	var filteredMaps []RowMap
	for i, rowMap := range allRowMaps {
		if e.rowPassesWhere(where, rowMap, nil) {
			filteredRows = append(filteredRows, allRows[i])
			filteredMaps = append(filteredMaps, rowMap)
		}
	}
	return filteredRows, filteredMaps
}

func (e *Engine) execJoins(s *sql.SelectStmt, baseMaps []RowMap, baseDefs []sql.ColumnDef) ([]RowMap, []sql.ColumnDef, error) {
	if err := validateJoinOnClauses(s); err != nil {
		return nil, nil, err
	}
	plainNames := map[string]bool{}
	for _, d := range baseDefs {
		plainNames[d.Name] = true
	}
	currentMaps := baseMaps
	currentDefs := baseDefs

	// The left table's qualified-name prefix in combined row maps must use
	// the table's alias when present (e.g. "c.id" for "FROM customer c"),
	// so JOIN ON conditions referencing the alias resolve correctly.
	leftName := s.From.Name
	if s.From.As != "" {
		leftName = s.From.As
	}

	for _, join := range s.Joins {
		var rightMaps []RowMap
		var rightDefs []sql.ColumnDef
		var tableName string

		// Handle derived table (subquery) in JOIN: JOIN (SELECT ...) AS t
		if join.Table.Subquery != nil {
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
			// Build row maps from subquery result rows
			for _, row := range subqResult.Rows {
				rightRowMap := make(RowMap)
				for i, val := range row {
					if i < len(rightDefs) {
						rightRowMap[rightDefs[i].Name] = val
					}
				}
				rightMaps = append(rightMaps, rightRowMap)
			}
		} else if isPragmaTableFunc(join.Table.Name) {
			// Table-valued pragma function in a JOIN: pragma_table_info('t1') AS t
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
		} else if tableEntry, err := e.schema.FindTable(join.Table.Name); err != nil {
			viewEntry, viewErr := e.schema.FindView(join.Table.Name)
			if viewErr != nil {
				return nil, nil, err
			}
			// Execute the view to get its columns and rows
			viewResult := e.execSelectView(viewEntry)
			if viewResult.Error != nil {
				return nil, nil, viewResult.Error
			}
			// Build column defs from the view's declared column list when
			// present, otherwise from the view result column names.
			declared := viewDeclaredColumns(viewEntry.SQL)
			if len(declared) > 0 {
				for _, colName := range declared {
					rightDefs = append(rightDefs, sql.ColumnDef{Name: colName})
				}
			} else {
				for _, colName := range viewResult.Columns {
					rightDefs = append(rightDefs, sql.ColumnDef{Name: colName})
				}
			}
			tableName = join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}
			// Build row maps from view result rows
			for _, row := range viewResult.Rows {
				rightRowMap := make(RowMap)
				for i, val := range row {
					if i < len(rightDefs) {
						rightRowMap[rightDefs[i].Name] = val
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

			// Scan all rows from the right table
			tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
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

		// NATURAL JOIN: auto-generate USING conditions for all common columns.
		effectiveOn := join.On
		if join.JoinType == "NATURAL" {
			effectiveOn = e.generateNaturalJoinOn(currentDefs, rightDefs)
		}
		// USING(col1, col2): generate ON left.col = right.col for each column.
		if len(join.Using) > 0 && effectiveOn == nil {
			effectiveOn = e.generateUsingJoinOn(join.Using, leftName, tableName)
		}

		// SQLite forbids an ON clause from referencing tables to its right;
		// unqualified column references must resolve among the joined tables.
		for _, d := range rightDefs {
			plainNames[d.Name] = true
		}
		if (join.JoinType == "LEFT" || join.JoinType == "RIGHT") {
			if err := validateOnColumnRefs(effectiveOn, plainNames); err != nil {
				return nil, nil, err
			}
		}

		// Build ephemeral hash index for equi-join optimization.
		// Detect simple "left.col = right.col" patterns in the ON clause
		// and create a temporary index on the right table's column.
		var autoIndex map[interface{}][]RowMap
		_, rightColName := extractEquiJoinCols(effectiveOn, leftName, tableName)
		if rightColName != "" && len(rightMaps) > 0 {
			autoIndex = make(map[interface{}][]RowMap)
			for _, rm := range rightMaps {
				if val, ok := rm[rightColName]; ok {
					// Unwrap *ColumnValue so the map key compares by value,
					// not by pointer identity. Without this, two rows with
					// equal column values but different *ColumnValue pointers
					// never match in the hash lookup.
					autoIndex[util.UnwrapColumnValue(val)] = append(autoIndex[util.UnwrapColumnValue(val)], rm)
				}
			}
		}
		// Track autoindex usage for EXPLAIN QUERY PLAN output
		e.usingAutoIndex = autoIndex != nil

		// Nested-loop join (for both table and view)
		var combinedMaps []RowMap
		// For USING clause, exclude the merged columns from the right table's
		// column definitions so that SELECT * expansion does not duplicate them.
		filteredRightDefs := e.filterUsingColumns(rightDefs, effectiveOn)
		// Prefix remaining right-table column names when they conflict with
		// existing left-table column names, so * expansion resolves values
		// from the combined row map using qualified keys (table.col).
		rightDefsNamed := e.prefixRightColDefs(filteredRightDefs, currentDefs, tableName)
		combinedDefs := append(append([]sql.ColumnDef{}, currentDefs...), rightDefsNamed...)

		// Track which right rows were matched — needed for RIGHT/FULL JOIN
		// to find unmatched right rows that must be included with NULL left side.
		isRightOrFull := join.JoinType == "RIGHT" || join.JoinType == "FULL"
		matchedRight := make([]bool, len(rightMaps))

		for leftIdx, leftMap := range currentMaps {
			// Use the effective ON (NATURAL JOIN may have generated conditions)
			effectiveJoin := join
			if effectiveOn != join.On {
				effectiveJoin.On = effectiveOn
			}
			matched := e.processJoinRowTrackingRight(
				leftMap, rightMaps, &combinedMaps, tableName, effectiveJoin, s, rightDefs, autoIndex,
				isRightOrFull, matchedRight, leftIdx)
			if !matched && (join.JoinType == "LEFT" || join.JoinType == "FULL") {
				combinedMaps = append(combinedMaps, e.buildLeftJoinRow(leftMap, rightDefs, tableName))
			}
		}

		// For RIGHT and FULL JOIN: add unmatched right rows with NULL-padded left
		if isRightOrFull {
			for ri, rm := range rightMaps {
				if !matchedRight[ri] {
					combinedMaps = append(combinedMaps, e.buildRightJoinUnmatched(rm, currentDefs, rightDefsNamed, tableName))
				}
			}
		}

		currentMaps = combinedMaps
		currentDefs = combinedDefs
	}

	return currentMaps, currentDefs, nil
}

// processJoinRowTrackingRight processes a single left row against all right rows
// for a JOIN, optionally tracking which right rows were matched (for RIGHT/FULL JOIN).
// When trackMatchedRight is true, disables the autoIndex hash optimization so that
// right-row indices are available for tracking. leftIdx is the index of the left row
// (unused but kept for API symmetry).
// Returns true if at least one match was found (for the ON condition).
func (e *Engine) processJoinRowTrackingRight(
	leftMap RowMap, rightMaps []RowMap, combinedMaps *[]RowMap,
	tableName string, join sql.JoinClause, s *sql.SelectStmt,
	rightDefs []sql.ColumnDef, autoIndex map[interface{}][]RowMap,
	trackMatchedRight bool, matchedRight []bool, leftIdx int,
) bool {
	// The left table's qualified-name prefix uses its alias when present.
	leftName := s.From.Name
	if s.From.As != "" {
		leftName = s.From.As
	}
	matched := false

	// For RIGHT/FULL JOIN tracking, we must iterate right rows by index.
	// Skip autoIndex optimization when tracking.
	if autoIndex != nil && !trackMatchedRight {
		leftColName, _ := extractEquiJoinCols(join.On, leftName, tableName)
		if leftColVal, ok := leftMap[leftColName]; ok {
			if rightRows, ok := autoIndex[util.UnwrapColumnValue(leftColVal)]; ok {
				for _, rightMap := range rightRows {
					combinedMap := e.buildCombinedRowMap(leftMap, rightMap, tableName, leftName)
					if e.evalOnCondition(join.On, combinedMap) {
						matched = true
						*combinedMaps = append(*combinedMaps, combinedMap)
					}
				}
			}
		}
	} else {
		for ri, rightMap := range rightMaps {
			combinedMap := e.buildCombinedRowMap(leftMap, rightMap, tableName, leftName)
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
	// CROSS JOIN: always produces a match
	if !matched && join.JoinType == "CROSS" {
		for ri, rightMap := range rightMaps {
			*combinedMaps = append(*combinedMaps, e.buildCombinedRowMap(leftMap, rightMap, tableName, leftName))
			if trackMatchedRight {
				matchedRight[ri] = true
			}
		}
		matched = true
	}
	return matched
}

// processJoinRow processes a single left row against all right rows for a JOIN.
// Returns true if at least one match was found (for the ON condition).
// When autoIndex is non-nil, uses hash-index lookup instead of full scan.
func (e *Engine) processJoinRow(leftMap RowMap, rightMaps []RowMap, combinedMaps *[]RowMap, tableName string, join sql.JoinClause, s *sql.SelectStmt, rightDefs []sql.ColumnDef, autoIndex map[interface{}][]RowMap) bool {
	return e.processJoinRowTrackingRight(leftMap, rightMaps, combinedMaps, tableName, join, s, rightDefs, autoIndex, false, nil, 0)
}

// buildRightJoinUnmatched creates a combined row for an unmatched right row
// in a RIGHT or FULL JOIN. The left side columns are set to NULL.
func (e *Engine) buildRightJoinUnmatched(rightMap RowMap, leftDefs, rightDefs []sql.ColumnDef, tableName string) RowMap {
	combined := make(RowMap)
	// Set all left-side columns to NULL (both qualified and unqualified)
	for _, cd := range leftDefs {
		combined[cd.Name] = nil
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
	// Also copy all right map keys with table prefix
	for k, v := range rightMap {
		combined[tableName+"."+k] = v
	}
	return combined
}

// generateNaturalJoinOn creates an ON expression for a NATURAL JOIN by finding
// all common column names between left and right table definitions and creating
// equality conditions: col = col AND col2 = col2 ...
// If no common columns exist, NATURAL JOIN behaves as a CROSS JOIN (nil ON).
func (e *Engine) generateNaturalJoinOn(leftDefs, rightDefs []sql.ColumnDef) sql.Expr {
	rightNames := make(map[string]bool)
	for _, cd := range rightDefs {
		rightNames[cd.Name] = true
	}
	var onExpr sql.Expr
	for _, cd := range leftDefs {
		if rightNames[cd.Name] {
			// Generate unqualified "col = col" (same format as USING clause)
			eq := &sql.BinaryOp{
				Left:     &sql.ColumnRef{Name: cd.Name},
				Right:    &sql.ColumnRef{Name: cd.Name},
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
		eq := &sql.BinaryOp{
			Left:     &sql.ColumnRef{Name: col},
			Right:    &sql.ColumnRef{Name: col},
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
	bop, ok := on.(*sql.BinaryOp)
	if !ok || bop.Operator != "=" {
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
			combined[leftTableName+"."+k] = v
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
func (e *Engine) filterUsingColumns(rightDefs []sql.ColumnDef, on sql.Expr) []sql.ColumnDef {
	if on == nil {
		return rightDefs
	}
	// Collect column names referenced in USING equality conditions.
	usingCols := make(map[string]bool)
	collectUsingColumns(on, usingCols)
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
			// Only treat as USING if both sides are unqualified column refs
			// with the same name — this is the signature of a USING clause.
			if leftOK && rightOK &&
				leftRef.Table == "" && rightRef.Table == "" &&
				leftRef.Name == rightRef.Name {
				cols[leftRef.Name] = true
			}
		} else if v.Operator == "AND" {
			collectUsingColumns(v.Left, cols)
			collectUsingColumns(v.Right, cols)
		}
	}
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
func (e *Engine) buildLeftJoinRow(leftMap RowMap, rightDefs []sql.ColumnDef, tableName string) RowMap {
	combined := make(RowMap)
	for k, v := range leftMap {
		combined[k] = v
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
			return true
		}
		return false
	case *sql.BinaryOp:
		return e.exprHasAggregate(v.Left) || e.exprHasAggregate(v.Right)
	case *sql.UnaryOp:
		return e.exprHasAggregate(v.Operand)
	default:
		return false
	}
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
		return true
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
			outRow = append(outRow, v)
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
				outRow = append(outRow, v)
			}
		} else {
			v, err := e.evalExpr(col.Expr, nil)
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, v)
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

	columns := e.buildColumnNames(s.Columns, nil)
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
	columns := e.buildColumnNames(s.Columns, nil)
	var outRow []interface{}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if f, found := e.funcs.Find(fn.Name); found && f.Type == function.TypeAggregate {
				switch f.Name {
				case "COUNT":
					outRow = append(outRow, int64(0))
				case "TOTAL":
					outRow = append(outRow, float64(0.0))
				default:
					outRow = append(outRow, nil)
				}
				continue
			}
		}
		outRow = append(outRow, nil)
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
	var keyOrder []string

	groupBy := resolveGroupByOrdinals(s)
	for _, row := range rowMaps {
		key := e.computeGroupByKey(groupBy, row)
		if _, exists := groups[key]; !exists {
			keyOrder = append(keyOrder, key)
		}
		groups[key] = append(groups[key], row)
	}
	// Sort keys for deterministic output matching SQLite GROUP BY behavior
	sort.Strings(keyOrder)

	columns := e.buildColumnNames(s.Columns, colDefs)
	var outRows [][]interface{}

	for _, key := range keyOrder {
		groupRows := groups[key]

		// Evaluate output row for this group
		var outRow []interface{}
		for _, col := range s.Columns {
			v, err := e.evalAggregateExpr(col.Expr, groupRows)
			if err != nil {
				return &Result{Error: err}
			}
			outRow = append(outRow, util.UnwrapColumnValue(v))
		}

		// Apply HAVING filter
		if s.Having != nil {
			match, err := e.evalHaving(s.Having, groupRows)
			if err != nil || !match {
				continue
			}
		}

		outRows = append(outRows, outRow)
	}

	if len(outRows) == 0 {
		return e.finalizeSelectResult(&Result{Columns: columns, Rows: [][]interface{}{}}, s, nil)
	}
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: outRows}, s, nil)
}

// resolveGroupByOrdinals maps numeric GROUP BY terms (e.g., GROUP BY 2) to
// the corresponding SELECT column expression (1-based ordinal).
func resolveGroupByOrdinals(s *sql.SelectStmt) []sql.Expr {
	if len(s.GroupBy) == 0 {
		return nil
	}
	resolved := make([]sql.Expr, len(s.GroupBy))
	for i, g := range s.GroupBy {
		if num, ok := g.(*sql.NumericLit); ok {
			var ord int64
			fmt.Sscanf(num.Value, "%d", &ord)
			if ord >= 1 && int(ord) <= len(s.Columns) {
				resolved[i] = s.Columns[ord-1].Expr
				continue
			}
		}
		resolved[i] = g
	}
	return resolved
}

// computeGroupByKey serializes the GROUP BY expression values for a row into a
// string key used to partition rows into groups.
func (e *Engine) computeGroupByKey(groupBy []sql.Expr, row Row) string {
	key, _ := e.computeGroupByKeyValues(groupBy, row)
	return key
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
		return evalBinaryOpValues(v.Operator, left, right)
	case *sql.UnaryOp:
		return e.evalHavingUnary(v, groupRows)
	case *sql.IsNull:
		operand, err := e.evalHavingExpr(v.Operand, groupRows)
		if err != nil {
			return nil, err
		}
		operand = util.UnwrapColumnValue(operand)
		return operand == nil, nil
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
		return !toBool(operand), nil
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
	return operand != nil, nil
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
				vi, errI := e.evalExpr(ob.Expr, rows[i])
				vj, errJ := e.evalExpr(ob.Expr, rows[j])
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
		agg.Step(args)
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
func (e *Engine) validateSelectExprs(s *sql.SelectStmt) error {
	// SQLite: an aggregate function in ORDER BY is only allowed when the
	// SELECT is an aggregate query (has GROUP BY or an aggregate in the
	// SELECT list). Otherwise it is a "misuse of aggregate" error.
	if len(s.OrderBy) > 0 && s.GroupBy == nil {
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
		// Check column expressions for subqueries with UNION ALL aggregates
		if err := e.validateExprSubqueries(col.Expr); err != nil {
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
	}
	if s.Where != nil {
		if err := e.validateExprSubqueries(s.Where); err != nil {
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
	if len(s.OrderBy) > 0 && len(s.GroupBy) == 0 && !e.hasAggregates(s.Columns) {
		for _, ob := range s.OrderBy {
			if nested := findAggregateInExpr(ob.Expr); nested != "" {
				return fmt.Errorf("misuse of aggregate: %s()", nested)
			}
		}
	}

	return nil
}

// validateExprSubqueries walks an expression tree looking for subqueries and
// checking them for invalid patterns like aggregates inside UNION ALL.
func (e *Engine) validateExprSubqueries(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.Subquery:
		if v.Select != nil {
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
			if err := e.validateExprSubqueries(arg); err != nil {
				return err
			}
		}
	case *sql.BinaryOp:
		if err := e.validateExprSubqueries(v.Left); err != nil {
			return err
		}
		return e.validateExprSubqueries(v.Right)
	case *sql.UnaryOp:
		return e.validateExprSubqueries(v.Operand)
	case *sql.CaseExpr:
		if v.Operand != nil {
			if err := e.validateExprSubqueries(v.Operand); err != nil {
				return err
			}
		}
		for _, w := range v.Whens {
			if err := e.validateExprSubqueries(w.When); err != nil {
				return err
			}
			if err := e.validateExprSubqueries(w.Then); err != nil {
				return err
			}
		}
		if v.Else != nil {
			return e.validateExprSubqueries(v.Else)
		}
	case *sql.Between:
		if err := e.validateExprSubqueries(v.Operand); err != nil {
			return err
		}
		if err := e.validateExprSubqueries(v.Low); err != nil {
			return err
		}
		return e.validateExprSubqueries(v.High)
	case *sql.InList:
		if err := e.validateExprSubqueries(v.Operand); err != nil {
			return err
		}
		for _, val := range v.List {
			if err := e.validateExprSubqueries(val); err != nil {
				return err
			}
		}
	case *sql.IsNull:
		return e.validateExprSubqueries(v.Operand)
	case *sql.IsNotNull:
		return e.validateExprSubqueries(v.Operand)
	case *sql.IsDistinctFrom:
		if err := e.validateExprSubqueries(v.Left); err != nil {
			return err
		}
		return e.validateExprSubqueries(v.Right)
	case *sql.IsNotDistinctFrom:
		if err := e.validateExprSubqueries(v.Left); err != nil {
			return err
		}
		return e.validateExprSubqueries(v.Right)
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
		// Check if this is an aggregate by name (no registry lookup needed)
		upper := strings.ToUpper(v.Name)
		if upper == "COUNT" || upper == "SUM" || upper == "AVG" || upper == "MIN" || upper == "MAX" || upper == "TOTAL" || upper == "GROUP_CONCAT" || upper == "STRING_AGG" {
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
				vi, errI := e.evalExpr(ob.Expr, uniqueRows[i])
				vj, errJ := e.evalExpr(ob.Expr, uniqueRows[j])
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
// keeping the corresponding rowMaps in sync.
func (e *Engine) distinctRows(rows [][]interface{}, rowMaps []RowMap) ([][]interface{}, []RowMap) {
	if len(rows) == 0 {
		return rows, rowMaps
	}
	seen := make(map[string]bool)
	var newRows [][]interface{}
	var newMaps []RowMap
	for i, row := range rows {
		key := rowKey(row)
		if !seen[key] {
			seen[key] = true
			newRows = append(newRows, row)
			if i < len(rowMaps) {
				newMaps = append(newMaps, rowMaps[i])
			}
		}
	}
	return newRows, newMaps
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
func (e *Engine) scanTableRows(cursor *btree.Cursor, s *sql.SelectStmt, colDefs []sql.ColumnDef, needMaps bool) ([][]interface{}, []RowMap) {
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
				passesWhere = e.rowPassesWhere(s.Where, reuseSRow, cursor)
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
				passesWhere = e.rowPassesWhere(s.Where, reuseSRow, cursor)
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
					// Need to unwrap ColumnValue wrappers
					for i, cd := range colDefs {
						if cd.Dropped {
							continue
						}
						outValues = append(outValues, util.UnwrapColumnValue(reuseSRow.values[i]))
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
	return allRows, allRowMaps
}

func (e *Engine) rowPassesWhere(where sql.Expr, row Row, cursor *btree.Cursor) bool {
	if where == nil {
		return true
	}
	// Fast path: simple comparison ColumnRef OP Literal
	if bop, ok := where.(*sql.BinaryOp); ok && row != nil {
		if result, ok := e.fastEvalComparison(bop, row); ok {
			return result
		}
	}
	match, err := e.evalBool(where, row)
	if err != nil {
		return false
	}
	return match
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
		val, exists := row.Get(colRef.Name)
		if !exists || val == nil {
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
		cmp := compareValuesWithCollate(val, litVal)
		return applyComparisonOp(bop.Operator, cmp), true
	}

	// Try Literal OP ColumnRef
	if colRef, ok := bop.Right.(*sql.ColumnRef); ok {
		val, exists := row.Get(colRef.Name)
		if !exists || val == nil {
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
		cmp := compareValuesWithCollate(litVal, val)
		return applyComparisonOp(bop.Operator, cmp), true
	}

	return false, false
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

// isHiddenSystemTable returns true if the table name is an internal system table
// that should not appear in sqlite_master queries. SQLite hides these tables from
// direct schema introspection while still allowing direct access by name.
func isHiddenSystemTable(name string) bool {
	upper := strings.ToUpper(name)
	return upper == "SQLITE_STAT1" || upper == "SQLITE_STAT4"
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
func (e *Engine) buildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) RowMap {
	row := make(RowMap)
	for i, v := range rec.Values {
		if i < len(colDefs) {
			// Wrap all column values with their affinity so comparison logic
			// can correctly apply SQLite affinity rules.
			aff := util.Affinity(colDefs[i].Type)
			row[colDefs[i].Name] = &util.ColumnValue{Value: v, Affinity: aff}
		} else {
			row[fmt.Sprintf("c%d", i)] = v
		}
	}
	row["rowid"] = rowID
	for _, cd := range colDefs {
		if cd.PrimaryKey && row[cd.Name] == nil {
			row[cd.Name] = rowID
		}
	}
	return row
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

	// Apply affinity wrappers for columns specified in affinityCols.
	// Match buildRowMap: wrap ALL columns (including INTEGER/REAL) with their
	// affinity so comparison logic applies the same SQLite affinity rules on
	// both the fast structRow path and the map path. Skipping I/R here made
	// results differ when ORDER BY forced the structRow path.
	if affinityCols != nil {
		for i := 0; i < len(values); i++ {
			if values[i] != nil && affinityCols[colDefs[i].Name] {
				aff := util.Affinity(colDefs[i].Type)
				values[i] = &util.ColumnValue{Value: values[i], Affinity: aff}
			}
		}
		// Handle rowid and PRIMARY KEY for affinity columns
		for i, cd := range colDefs {
			if cd.PrimaryKey && values[i] == nil && affinityCols[cd.Name] {
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
}

// structRowToMap converts a structRow to a RowMap, deep-copying mutable
// values (ColumnValue wrappers, []byte) so the map does not share the
// reused structRow value slots that the next decoded row overwrites.
func structRowToMap(sr *structRow) RowMap {
	m := make(RowMap, len(sr.index)+1)
	m["rowid"] = sr.rowID
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
				if !cd.Dropped {
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
			for _, cd := range colDefs {
				if cd.Dropped {
					continue
				}
				if val, exists := row.Get(cd.Name); exists {
					outRow = append(outRow, util.UnwrapColumnValue(val))
				}
			}
		} else {
			v, err := e.evalExpr(col.Expr, row)
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, util.UnwrapColumnValue(v))
			}
		}
	}
	return outRow
}

// buildColumnNames builds the column name list from SELECT columns.
func (e *Engine) buildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef) []string {
	var names []string
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			for _, cd := range colDefs {
				if cd.Dropped {
					continue
				}
				names = append(names, cd.Name)
			}
		} else if col.As != "" {
			names = append(names, col.As)
		} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			names = append(names, ref.Name)
		} else {
			names = append(names, "")
		}
	}
	return names
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
		return e.lessRows(orderBy, rowMaps, result.Rows, indices[i], indices[j])
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
func (e *Engine) lessRows(orderBy []sql.OrderByTerm, rowMaps []RowMap, rows [][]interface{}, i, j int) bool {
	for _, ob := range orderBy {
		// Handle positional ORDER BY (e.g., ORDER BY 1 means order by first column)
		if nl, ok := ob.Expr.(*sql.NumericLit); ok {
			if pos, err := strconv.ParseInt(nl.Value, 10, 64); err == nil && pos >= 1 && pos <= int64(len(rows[i])) {
				left := rows[i][pos-1]
				right := rows[j][pos-1]
				cmp := util.CompareValues(left, right)
				if ob.Desc {
					cmp = -cmp
				}
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
		cmp := util.CompareValues(left, right)
		if ob.Desc {
			cmp = -cmp
		}
		if cmp < 0 {
			return true
		} else if cmp > 0 {
			return false
		}
	}
	return false
}

// validateJoinOnClauses checks that each join's ON clause only references
// tables that have already been joined (to its left). SQLite raises
// "ON clause references tables to its right" otherwise.
func validateJoinOnClauses(s *sql.SelectStmt) error {
	available := map[string]bool{}
	if s.From.Name != "" || s.From.As != "" {
		tn := s.From.Name
		if s.From.As != "" {
			tn = s.From.As
		}
		if tn != "" {
			available[tn] = true
		}
	}
	for _, join := range s.Joins {
		tn := join.Table.Name
		if join.Table.As != "" {
			tn = join.Table.As
		}
		if tn != "" {
			available[tn] = true
		}
		// Only OUTER joins restrict ON-clause references to their left.
		if join.On == nil || (join.JoinType != "LEFT" && join.JoinType != "RIGHT") {
			continue
		}
		var bad string
		walkJoinOnExpr(join.On, func(e2 sql.Expr) {
			if cr, ok := e2.(*sql.ColumnRef); ok && cr.Table != "" && !available[cr.Table] {
				bad = cr.Table
			}
		})
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
	}
}

