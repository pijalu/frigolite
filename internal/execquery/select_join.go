// Package exec — JOIN execution functions extracted from select.go (file-level SRP).
// All functions remain methods on *SelectEngine in package internal/exec.
package execquery

import (
	"fmt"

	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// execJoins — top-level orchestrator

// execJoins processes all JOIN clauses in a SELECT statement, returning the
// combined row maps and column definitions after all joins are applied.
func (e *SelectEngine) execJoins(s *sql.SelectStmt, baseMaps []RowMap, baseDefs []sql.ColumnDef) ([]RowMap, []sql.ColumnDef, error) {
	if err := e.validateJoinOnClauses(s); err != nil {
		return nil, nil, err
	}
	// SQLite allows output column aliases (e.g., "SELECT (+a)b ... ON z=b")
	// to be referenced in ON clauses. Collect them so validation passes.
	plainNames := collectJoinPlainNames(s, baseDefs)
	leftName := aliasOrName(s.From.Name, s.From.As)
	currentMaps, currentDefs := baseMaps, baseDefs
	lastTableName := leftName // immediate left table of the next join

	// Empty-table short-circuit (SQLite query planner).
	maps, defs, done, shortErr := e.emptyJoinShortCircuit(s, baseMaps, baseDefs)
	if shortErr != nil {
		return nil, nil, shortErr
	}
	if done {
		return maps, defs, nil
	}

	for _, join := range s.Joins {
		maps, defs, name, err := e.applyJoin(
			s, join, currentMaps, currentDefs, leftName, lastTableName, plainNames)
		if err != nil {
			return nil, nil, err
		}
		currentMaps = maps
		currentDefs = defs
		lastTableName = name
	}
	return currentMaps, currentDefs, nil
}

// applyJoin processes a single JOIN clause, returning the combined maps,
// combined defs, and the (possibly aliased) right table name.
func (e *SelectEngine) applyJoin(
	s *sql.SelectStmt, join sql.JoinClause,
	currentMaps []RowMap, currentDefs []sql.ColumnDef,
	leftName, lastTableName string, plainNames map[string]bool,
) ([]RowMap, []sql.ColumnDef, string, error) {
	rightMaps, rightDefs, tableName, corrLeftIdx, err := e.materializeJoinRight(s, join, currentMaps)
	if err != nil {
		return nil, nil, "", err
	}
	// NATURAL JOIN / USING: generate effective ON expression.
	effectiveOn, naturalCols := e.setupJoinEffectiveOn(join, currentDefs, rightDefs, lastTableName, tableName)

	// Collect right-side column names for ON-clause validation.
	addRightDefNames(rightDefs, plainNames)
	if jt := join.JoinType; joinTypeHas(jt, "LEFT") || joinTypeHas(jt, "RIGHT") {
		if err := validateOnColumnRefs(effectiveOn, plainNames); err != nil {
			return nil, nil, "", err
		}
	}

	// Build ephemeral hash index for equi-join optimization.
	autoIndex := e.buildJoinAutoIndex(effectiveOn, rightMaps, rightDefs, lastTableName, tableName)
	e.usingAutoIndex = autoIndex != nil

	// Pre-filter the right table when the ON references ONLY right-table columns.
	rightMaps, effectiveOn = e.maybePrefilterJoinRight(join, effectiveOn, rightMaps, tableName)

	// Build combined column defs (USING/NATURAL merges columns).
	usingJoin := len(join.Using) > 0 || len(naturalCols) > 0
	filteredRightDefs := e.filterUsingColumns(rightDefs, effectiveOn, naturalCols, usingJoin)
	rightDefsNamed := e.prefixRightColDefs(filteredRightDefs, currentDefs, tableName)
	combinedDefs := append(append([]sql.ColumnDef{}, currentDefs...), rightDefsNamed...)

	// Run nested-loop join.
	isRightOrFull := joinTypeHas(join.JoinType, "RIGHT") || joinTypeHas(join.JoinType, "FULL")
	matchedRight := make([]bool, len(rightMaps))
	combinedMaps := e.runJoinNestedLoop(
		s, join, currentMaps, rightMaps, corrLeftIdx,
		lastTableName, tableName, leftName, effectiveOn,
		autoIndex, rightDefs, isRightOrFull, matchedRight)

	// RIGHT/FULL JOIN: add unmatched right rows with NULL-padded left.
	if isRightOrFull {
		for ri, rm := range rightMaps {
			if !matchedRight[ri] {
				combinedMaps = append(combinedMaps, e.buildRightJoinUnmatched(rm, currentMaps, currentDefs, rightDefsNamed, tableName, leftName))
			}
		}
	}
	return combinedMaps, combinedDefs, tableName, nil
}

// setupJoinEffectiveOn returns the effective ON expression and the set of
// NATURAL-join common columns (nil for non-NATURAL joins). For NATURAL JOIN it
// auto-generates USING conditions; for a USING clause it generates equality
// conditions when no ON was already produced (NATURAL + USING).
func (e *SelectEngine) setupJoinEffectiveOn(join sql.JoinClause, currentDefs, rightDefs []sql.ColumnDef, leftName, tableName string) (sql.Expr, map[string]bool) {
	effectiveOn := join.On
	var naturalCols map[string]bool
	if isNaturalJoinType(join.JoinType) {
		naturalCols = naturalJoinCommonCols(currentDefs, rightDefs)
		effectiveOn = e.generateNaturalJoinOn(currentDefs, rightDefs, leftName, tableName)
	}
	if len(join.Using) > 0 && effectiveOn == nil {
		effectiveOn = e.generateUsingJoinOn(join.Using, leftName, tableName)
	}
	return effectiveOn, naturalCols
}

// maybePrefilterJoinRight pre-filters the right table rows when the ON clause
// references only right-table columns (the condition is independent of the left
// row). RIGHT/FULL joins keep all right rows for unmatched tracking. Returns the
// (possibly filtered) right maps and the (possibly nil-ed) ON expression.
func (e *SelectEngine) maybePrefilterJoinRight(join sql.JoinClause, effectiveOn sql.Expr, rightMaps []RowMap, tableName string) ([]RowMap, sql.Expr) {
	if joinTypeHas(join.JoinType, "RIGHT") || joinTypeHas(join.JoinType, "FULL") {
		return rightMaps, effectiveOn
	}
	if filtered, ok := e.prefilterJoinRightOnly(effectiveOn, rightMaps, tableName); ok {
		return filtered, nil
	}
	return rightMaps, effectiveOn
}

// runJoinNestedLoop performs the nested-loop join of currentMaps (left) against
// rightMaps (right), appending combined rows to the result. When the planner
// determines a comma/cross join should swap its scan order, iterates right-outer
// / left-inner to match SQLite's preferred scan order.
func (e *SelectEngine) runJoinNestedLoop(
	s *sql.SelectStmt, join sql.JoinClause,
	currentMaps, rightMaps []RowMap, corrLeftIdx []int,
	lastTableName, tableName, leftName string, effectiveOn sql.Expr,
	autoIndex map[interface{}][]joinIndexEntry, rightDefs []sql.ColumnDef,
	isRightOrFull bool, matchedRight []bool,
) []RowMap {
	var combinedMaps []RowMap
	swapJoin := e.shouldSwapCommaJoin(s, join, lastTableName, tableName)
	if swapJoin {
		for _, rightMap := range rightMaps {
			for _, leftMap := range currentMaps {
				combinedMaps = append(combinedMaps, e.buildCombinedRowMap(leftMap, rightMap, tableName, lastTableName))
			}
		}
		return combinedMaps
	}
	for leftIdx, leftMap := range currentMaps {
		effectiveJoin := join
		if effectiveOn != join.On {
			effectiveJoin.On = effectiveOn
		}
		rowRightMaps, rowCorrLeft := selectCorrelatedRightMaps(rightMaps, corrLeftIdx, leftIdx)
		matched := e.processJoinRowTrackingRight(
			leftMap, rowRightMaps, &combinedMaps, lastTableName, tableName, effectiveJoin,
			rightDefs, autoIndex,
			isRightOrFull, matchedRight, leftIdx, rowCorrLeft)
		if !matched && (joinTypeHas(join.JoinType, "LEFT") || joinTypeHas(join.JoinType, "FULL")) {
			combinedMaps = append(combinedMaps, e.buildLeftJoinRow(leftMap, rightDefs, tableName, leftName))
		}
	}
	return combinedMaps
}

// emptyJoinShortCircuit

// emptyJoinShortCircuit implements the SQLite query-planner empty-table
// short-circuit: when the FROM table has zero rows, every join result is empty
// (a LEFT JOIN with an empty left side yields no rows either); when a
// non-LEFT join operand is a plain table with zero rows, the
// INNER/CROSS/comma join result is empty. Returns done=true when the caller
// should return the given maps immediately.
func (e *SelectEngine) emptyJoinShortCircuit(s *sql.SelectStmt, baseMaps []RowMap, baseDefs []sql.ColumnDef) ([]RowMap, []sql.ColumnDef, bool, error) {
	if len(s.Joins) == 0 {
		return baseMaps, baseDefs, false, nil
	}
	// A RIGHT/FULL JOIN with an empty left side still returns the right
	// rows NULL-padded. The empty-base short-circuit below must not swallow those.
	if len(baseMaps) == 0 && !joinsHaveRightOrFull(s.Joins) {
		// The result is empty (no rows), but the output COLUMN definitions
		// must still reflect every joined table (a bare * over an empty
		// cross join reports all source columns). Merge the right-side
		// column defs before returning the empty base.
		merged := baseDefs
		for _, join := range s.Joins {
			rightMaps, rightDefs, _, _, err := e.materializeJoinRight(s, join, nil)
			if err != nil {
				// Materialization errors (no such table/column, TVF argument
				// scope violations) must surface even when the left side is
				// empty: SQLite reports prepare-time errors regardless of the
				// join cardinality.
				return nil, nil, false, err
			}
			_ = rightMaps
			// NATURAL JOIN / USING merge common columns into the left; the
			// empty-base column set must reflect that merge too.
			effectiveOn, naturalCols := e.setupJoinEffectiveOn(join, baseDefs, rightDefs, s.From.Name, join.Table.Name)
			usingJoin := len(join.Using) > 0 || len(naturalCols) > 0
			filteredRightDefs := e.filterUsingColumns(rightDefs, effectiveOn, naturalCols, usingJoin)
			merged = append(merged, filteredRightDefs...)
		}
		return baseMaps, merged, true, nil
	}
	for i, join := range s.Joins {
		// An empty non-LEFT join empties the running result — UNLESS a later
		// RIGHT/FULL join would still emit its unmatched right rows NULL-padded
		// (e.g. t1 JOIN t3 ON ... RIGHT JOIN t2 ON TRUE with t3 empty).
		if joinIsEmptyNonLeft(e, join) && !joinsHaveRightOrFull(s.Joins[i+1:]) {
			return baseMaps[:0], baseDefs, true, nil
		}
	}
	return baseMaps, baseDefs, false, nil
}

// joinsHaveRightOrFull reports whether any join in the list is RIGHT or FULL.
func joinsHaveRightOrFull(joins []sql.JoinClause) bool {
	for _, join := range joins {
		if joinTypeHas(join.JoinType, "RIGHT") || joinTypeHas(join.JoinType, "FULL") {
			return true
		}
	}
	return false
}

// joinIsEmptyNonLeft reports whether a non-LEFT join's plain-table right operand
// has zero rows (which empties the INNER/CROSS/comma result).
func joinIsEmptyNonLeft(e *SelectEngine, join sql.JoinClause) bool {
	if strings.EqualFold(join.JoinType, "LEFT") {
		return false
	}
	if join.Table.Subquery != nil || join.Table.Name == "" {
		return false
	}
	entry, _, err := e.ctx.FindTable(join.Table.Name)
	if err != nil || entry.RootPage <= 0 {
		return false
	}
	return e.tableRowCount(entry.Name) == 0
}

// materializeSubqueryJoin

// materializeSubqueryJoin builds the right-side row maps and column defs for a
// derived-table subquery join operand.
func (e *SelectEngine) materializeSubqueryJoin(join sql.JoinClause) ([]RowMap, []sql.ColumnDef, string, []int, error) {
	// A derived table cannot reference tables outside its own FROM (no
	// correlation). Validate its refs resolve within its scope.
	if bad := derivedTableBadColumnRef(join.Table.Subquery); bad != "" {
		return nil, nil, "", nil, fmt.Errorf("no such column: %s", bad)
	}
	// A parenthesized JOIN group is a non-lateral subquery: its expressions
	// (including TVF arguments) cannot reference the enclosing FROM tables.
	savedDerived := e.derivedScope
	e.derivedScope = true
	subqResult := e.execSelect(join.Table.Subquery)
	e.derivedScope = savedDerived
	if subqResult.Error != nil {
		return nil, nil, "", nil, subqResult.Error
	}
	rightDefs := resultColumnDefs(subqResult.Columns)
	tableName := joinTableName(join)
	synthetic := tableName == ""
	if synthetic {
		e.subqSeq++
		tableName = fmt.Sprintf("_subq%d", e.subqSeq)
	}
	// Build row maps from subquery result rows. When the subquery itself
	// joined tables (a parenthesized group), reuse its qualified row maps so
	// outer references like t4.a resolve; otherwise build plain maps.
	rightMaps := e.buildSubqueryRowMaps(subqResult, rightDefs, join.Table.Subquery, tableName, synthetic)
	return rightMaps, rightDefs, tableName, nil, nil
}

// buildSubqueryRowMaps builds right-side RowMaps from a subquery result. When
// the subquery already produced row maps (it joined internally), reuses them;
// otherwise wraps each projected value with its column affinity.
func (e *SelectEngine) buildSubqueryRowMaps(subqResult *Result, rightDefs []sql.ColumnDef, subquery *sql.SelectStmt, tableName string, synthetic bool) []RowMap {
	if len(subqResult.rowMaps) > 0 && len(subqResult.rowMaps) == len(subqResult.Rows) {
		return subqResult.rowMaps
	}
	subqAff := subqueryColumnAffinities(subquery)
	var rightMaps []RowMap
	for _, row := range subqResult.Rows {
		rightRowMap := make(RowMap)
		for i, val := range row {
			if i >= len(rightDefs) {
				continue
			}
			aff := subqueryAffinity(subqAff, i, rightDefs[i])
			cv := &util.ColumnValue{Value: val, Affinity: aff}
			rightRowMap[rightDefs[i].Name] = cv
			if synthetic {
				// Also store under the synthetic qualified key so the USING ON
				// clause (id = _subq.id) can match the right side independently.
				rightRowMap[tableName+"."+rightDefs[i].Name] = val
			}
		}
		rightMaps = append(rightMaps, rightRowMap)
	}
	return rightMaps
}

// materializeViewJoin

// materializeViewJoin builds the right-side row maps and column defs for a
// view join operand.
func (e *SelectEngine) materializeViewJoin(join sql.JoinClause) ([]RowMap, []sql.ColumnDef, string, []int, error) {
	viewEntry, viewCtx, viewErr := e.ctx.FindView(join.Table.Name)
	if viewErr != nil {
		// Match the original behavior: a name that is neither a table nor a
		// view returns the table lookup error.
		_, _, tableErr := e.ctx.FindTable(join.Table.Name)
		return nil, nil, "", nil, tableErr
	}
	// Pin the view's schema while executing its body so unqualified table
	// references resolve in the view's own schema (with1 24.2: aux.v2's
	// body "SELECT * FROM v3" must resolve aux.v3, not main.v3).
	savedPin := e.schemaPin
	if viewCtx != nil && !viewCtx.IsTemp {
		e.schemaPin = viewCtx
	}
	viewResult := e.execSelectView(viewEntry)
	e.schemaPin = savedPin
	if viewResult.Error != nil {
		return nil, nil, "", nil, viewResult.Error
	}
	rightDefs, err := e.buildViewRightDefs(viewEntry, viewResult)
	if err != nil {
		return nil, nil, "", nil, err
	}
	tableName := joinTableName(join)
	rightMaps := buildViewRowMaps(viewResult, rightDefs, tableName)
	return rightMaps, rightDefs, tableName, nil, nil
}

// buildViewRightDefs builds the right-side column defs for a view join: declared
// column names override, then computed typed defs, then result column names.
func (e *SelectEngine) buildViewRightDefs(viewEntry *schema.Entry, viewResult *Result) ([]sql.ColumnDef, error) {
	viewDefs, err := e.ctx.ViewColumnDefs(viewEntry)
	if err != nil {
		return nil, err
	}
	declared := ViewDeclaredColumns(viewEntry.SQL)
	if len(declared) > 0 {
		return rebuildViewDefsWithNames(declared, viewDefs), nil
	}
	if len(viewDefs) > 0 {
		return viewDefs, nil
	}
	return resultColumnDefs(viewResult.Columns), nil
}

// rebuildViewDefsWithNames rebuilds view defs with explicitly declared column
// names, preserving the computed types/collations.
func rebuildViewDefsWithNames(declared []string, viewDefs []sql.ColumnDef) []sql.ColumnDef {
	named := make([]sql.ColumnDef, 0, len(declared))
	for i, colName := range declared {
		cd := sql.ColumnDef{Name: colName}
		if i < len(viewDefs) {
			cd.Type = viewDefs[i].Type
			cd.Collate = viewDefs[i].Collate
		}
		named = append(named, cd)
	}
	return named
}

// buildViewRowMaps builds right-side RowMaps from a view result, wrapping each
// value with its column affinity and adding table-qualified keys.
func buildViewRowMaps(viewResult *Result, rightDefs []sql.ColumnDef, tableName string) []RowMap {
	var rightMaps []RowMap
	for _, row := range viewResult.Rows {
		rightRowMap := make(RowMap)
		for i, val := range row {
			if i >= len(rightDefs) {
				continue
			}
			cd := rightDefs[i]
			cv := &util.ColumnValue{Value: val, Affinity: util.Affinity(cd.Type)}
			var mapped interface{} = cv
			if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
				mapped = &CollatedValue{Value: cv, Collation: strings.ToUpper(coll)}
			}
			rightRowMap[cd.Name] = mapped
			rightRowMap[tableName+"."+cd.Name] = mapped
		}
		rightMaps = append(rightMaps, rightRowMap)
	}
	return rightMaps
}

// materializeTableJoin

// materializeTableJoin builds the right-side row maps and column defs for a
// real-table join operand (scanning the b-tree, or materializing a virtual
// table when RootPage == 0).
func (e *SelectEngine) materializeTableJoin(s *sql.SelectStmt, join sql.JoinClause, tableEntry *schema.Entry) ([]RowMap, []sql.ColumnDef, string, []int, error) {
	tableName := joinTableName(join)
	if tableEntry.RootPage == 0 {
		// Created virtual tables declare their columns in the module schema,
		// not in the CREATE VIRTUAL TABLE SQL text.  Use that schema here;
		// parsing the CREATE statement yields no usable column definitions and
		// consequently loses qualified references such as a.name.
		var residual sql.Expr
		opts := e.vtabScanOptions(s)
		opts.Residual = &residual
		defs, rows, rowids, err, ok := e.ctx.MaterializeCreatedVTab(tableEntry.Name, opts)
		if !ok {
			return nil, nil, "", nil, fmt.Errorf("no such table: %s", tableEntry.Name)
		}
		if err != nil {
			return nil, nil, "", nil, err
		}
		// Constraints the module consumed (xBestIndex omit=1 — unionvtab's
		// rowid/IPK ranges) are NOT re-checked; the join must evaluate the
		// residual clause left after they were omitted. Full-source scans
		// would otherwise lose rows to a re-applied range filter.
		if residual != nil {
			s.Where = residual
		}
		rightMaps := buildScanRowMaps(rows, defs, tableName)
		// Native rowids back <table>.rowid references in the residual WHERE
		// (unionvtab.test 5.3: cc.rowid>c4.rowid), like the eponymous path.
		for i := range rightMaps {
			if i < len(rowids) {
				rightMaps[i]["rowid"] = rowids[i]
			}
		}
		return rightMaps, defs, tableName, nil, nil
	}
	rightDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	rightMaps, err := e.scanRealTableJoinRows(join.Table.Name, tableEntry.RootPage, rightDefs)
	if err != nil {
		return nil, nil, "", nil, err
	}
	return rightMaps, rightDefs, tableName, nil, nil
}

// materializeVTabJoinRows materializes a virtual table's rows into RowMaps for
// a join.
func (e *SelectEngine) materializeVTabJoinRows(tableEntry *schema.Entry, rightDefs []sql.ColumnDef, tableName string) ([]RowMap, error) {
	rows, err := e.ctx.VirtualTableRows(tableEntry, 0, "", false)
	if err != nil {
		return nil, err
	}
	// An FTS table's row maps must carry the real docid (as rowid/docid plus
	// the qualified <table>.rowid key) so MATCH evaluation resolves the FTS
	// document being matched in a joined row. The vtab cursor returns only
	// the user column values; the docids come from the in-memory FTS index.
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		return e.ftsJoinRowMaps(ftsTable, rightDefs, tableName), nil
	}
	return buildScanRowMaps(rows, rightDefs, tableName), nil
}

// scanRealTableJoinRows scans all rows from a real table's b-tree into RowMaps
// for a join.
func (e *SelectEngine) scanRealTableJoinRows(tableQualifiedName string, rootPage uint32, rightDefs []sql.ColumnDef) ([]RowMap, error) {
	tree := e.ctx.TableBTreeForName(tableQualifiedName, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, err
	}
	var rightMaps []RowMap
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}
		rightMaps = append(rightMaps, e.buildRowMap(rec, rightDefs, cell.RowID))
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return rightMaps, nil
}

// buildScanRowMaps builds RowMaps from raw rows (e.g. from a virtual table),
// wrapping each value with its column affinity and adding qualified keys.
func buildScanRowMaps(rows [][]interface{}, rightDefs []sql.ColumnDef, tableName string) []RowMap {
	var rightMaps []RowMap
	for _, row := range rows {
		rightRowMap := make(RowMap)
		for i, val := range row {
			if i >= len(rightDefs) {
				continue
			}
			cv := &util.ColumnValue{Value: val, Affinity: util.Affinity(rightDefs[i].Type)}
			rightRowMap[rightDefs[i].Name] = cv
			rightRowMap[tableName+"."+rightDefs[i].Name] = cv
		}
		rightMaps = append(rightMaps, rightRowMap)
	}
	return rightMaps
}

// columnIndexed

// columnIndexed reports whether a column is backed by an index (PRIMARY KEY,
// UNIQUE constraint, or a unique index) usable by the join planner.
func (e *SelectEngine) columnIndexed(defs []sql.ColumnDef, col, tableName string) bool {
	return colDefIndexed(defs, col) ||
		e.tableConstraintIndexed(tableName, col) ||
		e.uniqueIndexHasColumn(tableName, col)
}

// colDefIndexed reports whether a column has a per-column PRIMARY KEY or UNIQUE
// constraint in the column definitions.
func colDefIndexed(defs []sql.ColumnDef, col string) bool {
	for _, cd := range defs {
		if strings.EqualFold(cd.Name, col) && (isIPKRowidAliasCol(cd) || cd.PrimaryKey || cd.Unique) {
			return true
		}
	}
	return false
}

// tableConstraintIndexed reports whether a column is part of a table-level
// PRIMARY KEY or UNIQUE constraint.
func (e *SelectEngine) tableConstraintIndexed(tableName, col string) bool {
	for _, tc := range e.ctx.TableConstraints(tableName, e.tableCreateSQL(tableName)) {
		if tc.Type != sql.ConstraintPrimaryKey && tc.Type != sql.ConstraintUnique {
			continue
		}
		for _, ic := range tc.Columns {
			if strings.EqualFold(ic.Name, col) {
				return true
			}
		}
	}
	return false
}

// uniqueIndexHasColumn reports whether a column is covered by a unique index.
func (e *SelectEngine) uniqueIndexHasColumn(tableName, col string) bool {
	for _, ui := range e.ctx.UniqueIndexColumns(tableName) {
		for _, c := range ui.Cols {
			if strings.EqualFold(c, col) {
				return true
			}
		}
	}
	return false
}

// processJoinRowTrackingRight

// processJoinRowTrackingRight processes a single left row against all right rows
// for a JOIN, optionally tracking which right rows were matched (for RIGHT/FULL JOIN).
// When trackMatchedRight is true, disables the autoIndex hash optimization so that
// right-row indices are available for tracking.
// Returns true if at least one match was found (for the ON condition).
func (e *SelectEngine) processJoinRowTrackingRight(
	leftMap RowMap, rightMaps []RowMap, combinedMaps *[]RowMap,
	leftTableName, tableName string, join sql.JoinClause,
	rightDefs []sql.ColumnDef, autoIndex map[interface{}][]joinIndexEntry,
	trackMatchedRight bool, matchedRight []bool, leftIdx int, corrLeftIdx []int,
) bool {
	matched := false
	if autoIndex != nil {
		hashMatched, leftOK := e.joinRowViaHashIndex(
			leftMap, autoIndex, leftTableName, tableName, join.On,
			combinedMaps, trackMatchedRight, matchedRight)
		if leftOK {
			return hashMatched
		}
		if !hashMatched {
			matched = e.nestedLoopJoinRight(
				leftMap, rightMaps, combinedMaps, leftTableName, tableName,
				join.On, trackMatchedRight, matchedRight)
		}
	} else {
		matched = e.nestedLoopJoinRight(
			leftMap, rightMaps, combinedMaps, leftTableName, tableName,
			join.On, trackMatchedRight, matchedRight)
	}
	// CROSS JOIN: always produces a match. A NATURAL CROSS JOIN still applies
	// its generated equality condition, so it must not take this fallback. A
	// comma join with an explicit ON clause (FROM a, b ON (...)) is an inner
	// join whose ON must filter rows — it must not fall back to the full
	// cross product either.
	if !matched && join.JoinType == "CROSS" && join.On == nil && len(join.Using) == 0 && !isNaturalJoinType(join.JoinType) {
		matched = e.crossJoinAllRight(
			leftMap, rightMaps, combinedMaps, tableName, leftTableName,
			trackMatchedRight, matchedRight)
	}
	return matched
}

// joinRowViaHashIndex looks up the left row's equi-join column in the ephemeral
// hash index and produces matches. Returns (matched, leftOK): leftOK is false
// when the extracted left column is absent and the caller should fall back to
// the nested loop.
func (e *SelectEngine) joinRowViaHashIndex(
	leftMap RowMap, autoIndex map[interface{}][]joinIndexEntry,
	leftTableName, tableName string, on sql.Expr,
	combinedMaps *[]RowMap, trackMatchedRight bool, matchedRight []bool,
) (bool, bool) {
	leftColName, _ := extractEquiJoinCols(on, leftTableName, tableName)
	// Prefer the QUALIFIED key: in a chained join the unqualified name
	// resolves to the first table's column, NOT the immediate-left table.
	// For a NATURAL/USING-generated ON the left ref is UNQUALIFIED and the
	// merged column lives under the plain key (the immediate-left table's
	// qualified key is NULL for rows that only matched a deeper table of a
	// chained FULL join), so look up the plain key first there.
	leftColVal, leftOK := leftMap[leftTableName+"."+leftColName]
	if equiJoinLeftUnqualified(on, leftTableName, tableName) {
		leftColVal, leftOK = leftMap[leftColName]
		if !leftOK {
			leftColVal, leftOK = leftMap[leftTableName+"."+leftColName]
		}
	} else if !leftOK {
		leftColVal, leftOK = leftMap[leftColName]
	}
	if !leftOK {
		return false, false
	}
	matched := false
	uv := joinIndexKey(leftColVal)
	if rightRows, ok := autoIndex[uv]; ok {
		for _, entry := range rightRows {
			combinedMap := e.buildCombinedRowMap(leftMap, entry.row, tableName, leftTableName)
			if e.evalOnCondition(on, combinedMap) {
				matched = true
				*combinedMaps = append(*combinedMaps, combinedMap)
				if trackMatchedRight {
					matchedRight[entry.idx] = true
				}
			}
		}
	}
	return matched, true
}

// nestedLoopJoinRight iterates all right rows for a single left row, evaluating
// the ON condition and appending combined rows. Returns true if any matched.
func (e *SelectEngine) nestedLoopJoinRight(
	leftMap RowMap, rightMaps []RowMap, combinedMaps *[]RowMap,
	leftTableName, tableName string, on sql.Expr,
	trackMatchedRight bool, matchedRight []bool,
) bool {
	matched := false
	for ri, rightMap := range rightMaps {
		combinedMap := e.buildCombinedRowMap(leftMap, rightMap, tableName, leftTableName)
		if e.evalOnCondition(on, combinedMap) {
			matched = true
			*combinedMaps = append(*combinedMaps, combinedMap)
			if trackMatchedRight {
				matchedRight[ri] = true
			}
		}
	}
	return matched
}

// crossJoinAllRight produces a Cartesian product of a single left row with all
// right rows (CROSS JOIN fallback). Always returns true.
func (e *SelectEngine) crossJoinAllRight(
	leftMap RowMap, rightMaps []RowMap, combinedMaps *[]RowMap,
	tableName, leftTableName string,
	trackMatchedRight bool, matchedRight []bool,
) bool {
	for ri, rightMap := range rightMaps {
		*combinedMaps = append(*combinedMaps, e.buildCombinedRowMap(leftMap, rightMap, tableName, leftTableName))
		if trackMatchedRight {
			matchedRight[ri] = true
		}
	}
	return true
}

// buildRightJoinUnmatched

// buildRightJoinUnmatched creates a combined row for an unmatched right row
// in a RIGHT or FULL JOIN. The left side columns are set to NULL.
func (e *SelectEngine) buildRightJoinUnmatched(rightMap RowMap, leftMaps []RowMap, leftDefs, rightDefs []sql.ColumnDef, tableName, leftTableName string) RowMap {
	combined := make(RowMap)
	// Null out every left-side key from a sample left row. The left side may
	// be a compound of several tables whose qualified keys (t1.a, dual.dummy)
	// cannot be derived from a single table name; using a real left row's key
	// set preserves exactly the keys the left operands expose.
	if len(leftMaps) > 0 {
		for k := range leftMaps[0] {
			combined[k] = nil
		}
	} else {
		setLeftDefsNull(combined, leftDefs, leftTableName)
	}
	setRightDefsFromMap(combined, rightMap, rightDefs)
	mergeUnmatchedUsingCols(combined, rightMap, rightDefs)
	copyRightQualifiedKeys(combined, rightMap, tableName)
	return combined
}

// setLeftDefsNull sets all left-side column definitions to NULL (both qualified
// and unqualified).
func setLeftDefsNull(combined RowMap, leftDefs []sql.ColumnDef, leftTableName string) {
	for _, cd := range leftDefs {
		combined[cd.Name] = nil
		if leftTableName != "" {
			combined[leftTableName+"."+cd.Name] = nil
		}
	}
}

// setRightDefsFromMap copies right-side column values from the right row map
// into the combined map, handling prefixed (table.col) def names.
func setRightDefsFromMap(combined RowMap, rightMap RowMap, rightDefs []sql.ColumnDef) {
	for _, cd := range rightDefs {
		baseName := unprefixedColName(cd.Name)
		if val, ok := rightMap[baseName]; ok {
			combined[cd.Name] = val
			if _, exists := combined[baseName]; !exists {
				combined[baseName] = val
			}
		}
	}
}

// mergeUnmatchedUsingCols overwrites merged USING/NATURAL columns that were
// filtered out of rightDefs with the right row's value (an unmatched right row
// of t1 FULL JOIN t2 USING(id) has a real id from t2).
func mergeUnmatchedUsingCols(combined RowMap, rightMap RowMap, rightDefs []sql.ColumnDef) {
	rightDefNames := rightDefBaseNames(rightDefs)
	for k, v := range rightMap {
		if _, isLeft := combined[k]; isLeft && k != "rowid" && !rightDefNames[k] {
			combined[k] = v
		}
	}
}

// copyRightQualifiedKeys copies the right row's qualified keys as-is (a derived
// table's row map carries keys like t3.a), and adds table-name prefixes for
// unqualified keys.
func copyRightQualifiedKeys(combined RowMap, rightMap RowMap, tableName string) {
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
}

// extractEquiJoinCols

// extractEquiJoinCols examines a join ON expression looking for a simple
// equality comparison (col = col) where one column belongs to the left table
// and the other to the right table. Returns (leftCol, rightCol) or ("", "").
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
		return extractEquiJoinAndChain(bop, leftTableName, rightTableName)
	}
	if !isEquiJoinOperator(bop.Operator) {
		return "", ""
	}
	leftCol, leftOK := bop.Left.(*sql.ColumnRef)
	rightCol, rightOK := bop.Right.(*sql.ColumnRef)
	if !leftOK || !rightOK {
		return "", ""
	}
	return matchEquiJoinColumns(leftCol, rightCol, leftTableName, rightTableName)
}

// extractEquiJoinAndChain searches an AND chain for a usable equality (e.g.
// ON t1.d=t4.d AND t4.z>0): the equality drives the hash index, the rest is
// still evaluated per row by evalOnCondition.
func extractEquiJoinAndChain(bop *sql.BinaryOp, leftTableName, rightTableName string) (string, string) {
	if l, r := extractEquiJoinCols(bop.Left, leftTableName, rightTableName); l != "" {
		return l, r
	}
	return extractEquiJoinCols(bop.Right, leftTableName, rightTableName)
}

// isEquiJoinOperator reports whether an operator is a usable equi-join equality.
func isEquiJoinOperator(op string) bool {
	return op == "=" || op == "IS NOT DISTINCT FROM" || op == "IS"
}

// matchEquiJoinColumns determines which column is left vs right in an equi-join
// equality, based on table qualifiers. Returns ("", "") when no side matches.
func matchEquiJoinColumns(leftCol, rightCol *sql.ColumnRef, leftTableName, rightTableName string) (string, string) {
	if colRefMatchesTable(leftCol, leftTableName) && strings.EqualFold(rightCol.Table, rightTableName) {
		return leftCol.Name, rightCol.Name
	}
	if colRefMatchesTable(rightCol, leftTableName) && strings.EqualFold(leftCol.Table, rightTableName) {
		return rightCol.Name, leftCol.Name
	}
	// If neither side has a table qualifier, assume first col is left, second right.
	if leftCol.Table == "" && rightCol.Table == "" {
		return leftCol.Name, rightCol.Name
	}
	return "", ""
}

// colRefMatchesTable reports whether a column reference belongs to the given
// table (unqualified columns match any table).
func colRefMatchesTable(col *sql.ColumnRef, tableName string) bool {
	return col.Table == "" || strings.EqualFold(col.Table, tableName)
}

// equiJoinLeftUnqualified reports whether the equi-join equality's LEFT column
// reference is unqualified (as in the ON generated for NATURAL/USING joins:
// "id = t3.id"). Such an ON reads the merged column from the unqualified row
// key, not from the immediate-left table's qualified key.
func equiJoinLeftUnqualified(on sql.Expr, leftTableName, rightTableName string) bool {
	if ndf, ok := on.(*sql.IsNotDistinctFrom); ok {
		return equiJoinLeftUnqualified(
			&sql.BinaryOp{Left: ndf.Left, Right: ndf.Right, Operator: "="},
			leftTableName, rightTableName)
	}
	bop, ok := on.(*sql.BinaryOp)
	if !ok {
		return false
	}
	if bop.Operator == "AND" {
		return equiJoinLeftUnqualified(bop.Left, leftTableName, rightTableName) ||
			equiJoinLeftUnqualified(bop.Right, leftTableName, rightTableName)
	}
	if !isEquiJoinOperator(bop.Operator) {
		return false
	}
	return equiJoinRefsHaveUnqualifiedLeft(bop, leftTableName, rightTableName)
}

// equiJoinRefsHaveUnqualifiedLeft inspects an equi-join equality's two column
// references and reports whether the side that belongs to the left table is
// unqualified (an unqualified ref matches any table; two unqualified refs treat
// the first as left).
func equiJoinRefsHaveUnqualifiedLeft(bop *sql.BinaryOp, leftTableName, rightTableName string) bool {
	leftCol, leftOK := bop.Left.(*sql.ColumnRef)
	rightCol, rightOK := bop.Right.(*sql.ColumnRef)
	if !leftOK || !rightOK {
		return false
	}
	if colRefMatchesTable(leftCol, leftTableName) && strings.EqualFold(rightCol.Table, rightTableName) {
		return leftCol.Table == ""
	}
	if colRefMatchesTable(rightCol, leftTableName) && strings.EqualFold(leftCol.Table, rightTableName) {
		return rightCol.Table == ""
	}
	// Neither side qualified: treat the first column as the left one.
	return leftCol.Table == "" && rightCol.Table == ""
}

// collectUsingColumns

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
		collectUsingBinaryOp(v, cols)
	}
}

// collectUsingBinaryOp dispatches a BinaryOp in a USING ON expression.
func collectUsingBinaryOp(v *sql.BinaryOp, cols map[string]bool) {
	switch v.Operator {
	case "=":
		collectUsingEquality(v, cols)
	case "AND":
		collectUsingColumns(v.Left, cols)
		collectUsingColumns(v.Right, cols)
	}
}

// collectUsingEquality records a column name when both sides of an equality
// reference the SAME column (USING semantics).
func collectUsingEquality(v *sql.BinaryOp, cols map[string]bool) {
	leftRef, leftOK := v.Left.(*sql.ColumnRef)
	rightRef, rightOK := v.Right.(*sql.ColumnRef)
	if !leftOK || !rightOK || leftRef.Name != rightRef.Name {
		return
	}
	if leftRef.Table == "" && rightRef.Table == "" {
		cols[leftRef.Name] = true
	} else if !strings.EqualFold(leftRef.Table, rightRef.Table) {
		cols[leftRef.Name] = true
	}
}

// collectJoinMergedColumns

// collectJoinMergedColumns records column names merged by USING/NATURAL joins
// into the merged set, so SELECT * expansion does not duplicate them.
