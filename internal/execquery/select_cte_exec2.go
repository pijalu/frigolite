package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

func (e *SelectEngine) execCTEPostProcess(s *sql.SelectStmt, colDefs []sql.ColumnDef, allRowMaps []RowMap) *Result {
	if len(s.Joins) > 0 {
		var err error
		allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
		if err != nil {
			return &Result{Error: err}
		}
	}
	var whereErr error
	allRowMaps, whereErr = filterRowMapsByWhere(e, s.Where, allRowMaps)
	if whereErr != nil {
		return &Result{Error: whereErr}
	}
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	// Window-function pass over the CTE-materialized row set.
	if e.selectHasWindowFuncs(s.Columns) {
		winResult := e.execWindowPass(s, allRowMaps, colDefs)
		return e.finalizeSelectResult(winResult, s, winResult.rowMaps)
	}
	allRows, err := buildOutputRowsFromMaps(e, s.Columns, colDefs, allRowMaps)
	if err != nil {
		return &Result{Error: err}
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: allRows}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

func (e *SelectEngine) execSelectOverMaterialized(s *sql.SelectStmt, colDefs []sql.ColumnDef, rows [][]interface{}) *Result {
	return e.execSelectOverMaterializedRowids(s, colDefs, rows, nil)
}

// execSelectOverMaterializedRowids is execSelectOverMaterialized with native
// rowids from the source (virtual-table xRowid parity).
func (e *SelectEngine) execSelectOverMaterializedRowids(s *sql.SelectStmt, colDefs []sql.ColumnDef, rows [][]interface{}, rowids []int64) *Result {
	allRows := rows
	if len(allRows) == 0 && !e.hasAggregates(s.Columns) && len(s.GroupBy) == 0 && s.Union == nil {
		return &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: [][]interface{}{}}
	}
	allRowMaps := buildMaterializedRowMaps(s, colDefs, allRows, rowids)
	allRowMaps, colDefs, ferr := e.joinOrFilterRowMaps(s, allRows, allRowMaps, colDefs)
	if ferr != nil {
		return &Result{Error: ferr}
	}
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	// Window-function pass over the materialized row set.
	if e.selectHasWindowFuncs(s.Columns) {
		winResult := e.execWindowPass(s, allRowMaps, colDefs)
		result := e.finalizeSelectResult(winResult, s, winResult.rowMaps)
		if s.Distinct {
			result.Rows, _ = e.distinctRows(result.Rows, allRowMaps, e.selectOutputCollations(s), s)
		}
		return result
	}
	allRows, berr := buildOutputRowsFromMaps(e, s.Columns, colDefs, allRowMaps)
	if berr != nil {
		return &Result{Error: berr}
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
		if serr := e.sortRowsWithMaps(result, s.OrderBy, allRowMaps); serr != nil {
			return &Result{Error: serr}
		}
	}

	// Apply LIMIT / OFFSET
	lExpr, lErr := e.evalLimitExpr(s.Limit)
	if lErr != nil {
		return &Result{Error: lErr}
	}
	oExpr, oErr := e.evalLimitExpr(s.Offset)
	if oErr != nil {
		return &Result{Error: oErr}
	}
	result.Rows = applyLimitOffset(result.Rows, lExpr, oExpr)

	// Handle UNION / INTERSECT / EXCEPT
	if s.Union != nil {
		result.Rows = e.mergeUnionRows(result.Rows, s.Union, s.SetOp, s.UnionAll, e.selectOutputCollations(s))
	}

	return result
}

// execSelectCTE executes a query that references a CTE definition.
func (e *SelectEngine) execSelectCTE(s *sql.SelectStmt, cte *sql.CTEDef) *Result {
	// Detect circular CTE references (e.g. WITH a AS (SELECT * FROM b),
	// b AS (SELECT * FROM a)). SQLite reports "circular reference: NAME".
	// The flag is set only for the recursive path — a non-recursive body
	// that references a same-named INNER CTE (with1 21.1) must not be
	// blocked by the outer name.
	if e.resolvingCTEs == nil {
		e.resolvingCTEs = make(map[*sql.SelectStmt]bool)
	}
	// Set the resolving flag for every CTE execution (keyed by the body AST
	// so a same-named inner WITH shadow is a distinct CTE — with1 21.1).
	// Mutual recursion (tmp1 references tmp2 which references tmp1, with1
	// 3.1) re-enters the same body pointer and is caught as a circular
	// reference.
	if e.resolvingCTEs[cte.Select] {
		return &Result{Error: fmt.Errorf("circular reference: %s", cte.Name)}
	}
	e.resolvingCTEs[cte.Select] = true
	defer delete(e.resolvingCTEs, cte.Select)

	// Handle recursive CTE. A CTE is recursive when its body actually
	// references the CTE name in a FROM position (SQLite accepts
	// self-referencing CTEs regardless of the RECURSIVE keyword, e.g.
	// "WITH s(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM s WHERE i<10)"). The
	// RECURSIVE keyword alone does not force recursion: a body whose
	// self-reference is shadowed by an inner same-named WITH (with1 21.1:
	// "WITH RECURSIVE t21(a,b) AS (WITH t21(x) AS (VALUES(1)) SELECT x,x
	// FROM t21)") is not recursive. The resolving flag stays set during
	// execRecursiveCTE so a nested subquery that re-references the CTE
	// mid-recursion is reported as a circular reference instead of recursing
	// forever.
	if cte.Select != nil && cteBodyReferencesSelf(cte) {
		return e.execRecursiveCTEPath(s, cte)
	}

	// Non-recursive CTE: execute the CTE's SELECT directly with the scope
	// stack truncated to the CTE's defining scopes (a CTE body must not see
	// deeper scopes from the reference site — with1 17.5). SQLite's
	// withExpand checks the declared column list against the leftmost
	// (anchor) member's width BEFORE the compound arity check, so
	// "WITH i(x) AS (SELECT 1,2 UNION ALL SELECT 1)" reports
	// "table i has 2 values for 1 columns" while
	// "WITH i(x) AS (SELECT 1 UNION ALL SELECT 1,2)" reports the compound
	// arity error.
	if len(cte.Columns) > 0 {
		if err := e.checkCTEColumnCount(cte); err != nil {
			return &Result{Error: err}
		}
	}
	savedScopes := e.withCTEBodyScope(cte)
	cteResult := e.execSelect(cte.Select)
	e.cteScopes = savedScopes

	// CTE body is now materialized — clear the resolving flag so the main
	// query (including compound UNION/INTERSECT/EXCEPT chains that reference
	// the CTE again) can access the materialized rows without a false
	// circular-reference error.
	delete(e.resolvingCTEs, cte.Select)
	if cteResult.Error != nil {
		return cteResult
	}
	colDefs := cteColDefs(cte, cteResult, e)
	// CTE references expose only the CTE's declared/result columns: SQLite
	// rejects "no such column: rowid" on a CTE reference (with1 15.1) unless
	// a CTE column is literally named rowid. Skip when an outer row exists
	// (a correlated subquery in UPDATE SET may reference the outer table's
	// columns — with1 4.3).
	if err := e.validateCTERowIDRefs(s, colDefs, cte); err != nil {
		return &Result{Error: err}
	}
	// The outer statement references the CTE through its base FROM (possibly
	// aliased, e.g. "FROM c AS c2"). Add alias-qualified row-map keys
	// (c2.x) so qualified column references resolve — mirroring the
	// subquery path's buildMaterializedRowMaps. Without them, "SELECT c2.x
	// FROM c AS c2" yields NULL because the row map only carries the bare
	// column name.
	alias := cteRefAlias(s, cte)
	allRowMaps := make([]RowMap, len(cteResult.Rows))
	for i, row := range cteResult.Rows {
		// CTE rows have no implicit rowid (SQLite: "no such column: rowid"
		// on a CTE reference — with1 15.1).
		allRowMaps[i] = buildRowMapFromValuesNoRowID(row, colDefs)
		if alias != "" {
			for k, v := range allRowMaps[i] {
				if !strings.Contains(k, ".") {
					allRowMaps[i][alias+"."+k] = v
				}
			}
		}
	}
	return e.execCTEPostProcess(s, colDefs, allRowMaps)
}

// validateCTERowIDRefs validates the outer statement's column references
// against a CTE's columns, rejecting an implicit rowid unless a CTE column is
// literally named rowid (with1 15.1). Skipped when an outer row exists (a
// correlated subquery in UPDATE SET may reference the outer table's columns —
// with1 4.3).
func (e *SelectEngine) validateCTERowIDRefs(s *sql.SelectStmt, colDefs []sql.ColumnDef, cte *sql.CTEDef) error {
	if len(s.Joins) > 0 || e.outerRow != nil {
		return nil
	}
	allowRowID := false
	for _, cd := range colDefs {
		if isRowIDName(cd.Name) {
			allowRowID = true
			break
		}
	}
	return e.validateSelectColumnRefs(s, colDefs, cte.Name, s.From.As, allowRowID)
}

// cteRefAlias returns the alias under which the outer statement references a
// CTE through its base FROM: the explicit AS alias ("FROM c AS c2") or the
// CTE name itself ("FROM c") when no alias is given. Join-position CTE
// references are handled by the join machinery instead.
func cteRefAlias(s *sql.SelectStmt, cte *sql.CTEDef) string {
	if s == nil || s.From.Name == "" {
		return ""
	}
	if s.From.As != "" {
		return s.From.As
	}
	if s.From.Name == cte.Name {
		return cte.Name
	}
	return ""
}

// cteColDefs builds column definitions from the CTE result, applying declared
// column names and carrying the body's compound column affinity.
func cteColDefs(cte *sql.CTEDef, cteResult *Result, e *SelectEngine) []sql.ColumnDef {
	colDefs := make([]sql.ColumnDef, len(cteResult.Columns))
	for i, colName := range cteResult.Columns {
		colDefs[i] = sql.ColumnDef{Name: colName}
		if aff := e.compoundColumnAffinity(cte.Select, i); aff != 0 {
			colDefs[i].Type = affinityTypeName(aff)
		}
	}
	if len(cte.Columns) > 0 {
		for i := 0; i < len(colDefs) && i < len(cte.Columns); i++ {
			colDefs[i].Name = cte.Columns[i]
		}
	}
	return colDefs
}

// execRecursiveCTEPath splits a self-referencing CTE into its anchor and
// recursive terms, validates the compound operators, and runs the recursive
// iteration.
func (e *SelectEngine) execRecursiveCTEPath(s *sql.SelectStmt, cte *sql.CTEDef) *Result {
	anchor, recursive, err := splitRecursiveCTE(cte)
	if err != nil {
		return &Result{Error: err}
	}
	// SQLite rejects window functions anywhere in a recursive query body
	// (window1 15.0: "cannot use window functions in recursive queries"). Check
	// the anchor and every recursive term before resolving any table references
	// so the error takes precedence over a sibling-table resolution failure.
	if e.termHasWindowFunc(anchor) {
		return &Result{Error: fmt.Errorf("cannot use window functions in recursive queries")}
	}
	for _, rt := range recursive {
		if rt != nil && e.termHasWindowFunc(rt) {
			return &Result{Error: fmt.Errorf("cannot use window functions in recursive queries")}
		}
	}
	// SQLite requires the operators that connect the anchor to the
	// recursive part and between recursive terms to be all the same
	// (all UNION or all UNION ALL); mixing them is reported as a
	// circular reference (e.g. "WITH RECURSIVE s(i) AS (VALUES(1)
	// UNION ALL SELECT i+1 FROM s WHERE i<3 UNION SELECT 4)").
	dedup, err := recursiveCTEOp(anchor, recursive, cte.Name)
	if err != nil {
		return &Result{Error: err}
	}
	return e.execRecursiveCTE(s, cte, anchor, recursive, dedup)
}

// checkCTEColumnCount validates the declared column list against the anchor
// (leftmost) member's width, skipping the check when the width cannot be
// determined statically (mutual recursion cycle).
func (e *SelectEngine) checkCTEColumnCount(cte *sql.CTEDef) error {
	anchorCols, aerr := e.cteAnchorColumnCount(cte.Select)
	if aerr == errUndeterminedCTEWidth {
		// Mutual recursion cycle: skip the width check; the execution-time
		// circular reference fires later.
		return nil
	}
	if aerr != nil {
		return aerr
	}
	if anchorCols != len(cte.Columns) {
		return fmt.Errorf("table %s has %d values for %d columns", cte.Name, anchorCols, len(cte.Columns))
	}
	return nil
}

// selectFromRefersTo walks a SELECT's FROM positions (base table, joins,
// union members, nested subqueries) looking for a reference to name.
// joinFromRefersTo checks whether any join operand references name (directly
// or through a nested subquery).
func joinFromRefersTo(joins []sql.JoinClause, name string) bool {
	for i := range joins {
		j := &joins[i]
		if j.Table.Name == name || j.Table.As == name {
			return true
		}
		if j.Table.Subquery != nil && selectFromRefersTo(j.Table.Subquery, name) {
			return true
		}
	}
	return false
}

// selectFromRefersTo walks a SELECT's FROM positions (base table, joins,
// union members, nested subqueries) looking for a reference to name. A nested
// statement that declares name in its own WITH shadows the outer name, so
// references inside it are NOT considered references to the outer (with1
// 21.1: the recursive CTE body "WITH t21(x) AS (VALUES(1)) SELECT x,x FROM
// t21" shadows t21 with the inner WITH, so the outer t21 is not recursive).
func selectFromRefersTo(s *sql.SelectStmt, name string) bool {
	if s == nil {
		return false
	}
	if declaresCTE(s, name) {
		return false
	}
	if s.From.Name == name || s.From.As == name {
		return true
	}
	if s.From.Subquery != nil && selectFromRefersTo(s.From.Subquery, name) {
		return true
	}
	if joinFromRefersTo(s.Joins, name) {
		return true
	}
	return s.Union != nil && selectFromRefersTo(s.Union, name)
}

// declaresCTE reports whether a SELECT statement's own WITH clause declares a
// CTE named name (which shadows an outer same-named CTE).
func declaresCTE(s *sql.SelectStmt, name string) bool {
	if s == nil {
		return false
	}
	for _, cte := range s.CTEs {
		if strings.EqualFold(cte.Name, name) {
			return true
		}
	}
	return false
}

// cteAnchorColumnCount counts the anchor (first) member's output columns
// without executing it: a plain expression counts 1, a star expands via the
// FROM table's columns (or the CTE's own declared columns when self-joined).
func (e *SelectEngine) cteAnchorColumnCount(sel *sql.SelectStmt) (int, error) {
	if sel == nil {
		return 0, nil
	}
	// A select list that is entirely stars with no FROM table cannot expand:
	// SQLite reports "no tables specified" before any width check (with1
	// 13.1: "WITH RECURSIVE c(i) AS (SELECT * ...)" errors "no tables
	// specified", not a column-count mismatch; a mixed list like
	// "SELECT 5,*" does hit the width check first — 13.3).
	if len(sel.Columns) > 0 && sel.From.Name == "" && sel.From.Subquery == nil {
		allStars := true
		for _, col := range sel.Columns {
			ref, ok := col.Expr.(*sql.ColumnRef)
			if !ok || ref.Name != "*" {
				allStars = false
				break
			}
		}
		if allStars {
			return 0, fmt.Errorf("no tables specified")
		}
	}
	count := 0
	for _, col := range sel.Columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			n, err := e.starExpansionCountNoExecute(sel, ref)
			if err != nil {
				return 0, err
			}
			count += n
		} else {
			count++
		}
	}
	return count, nil
}

// starExpansionCountNoExecute counts the columns a * reference expands to
// WITHOUT executing the referenced CTE body (which could recurse back into the
// CTE being defined — with2 1.11: the outer i's anchor "SELECT * FROM j"
// where j's body references i). For a CTE it uses the declared column list; a
// real table resolves its schema columns.
func (e *SelectEngine) starExpansionCountNoExecute(sel *sql.SelectStmt, ref *sql.ColumnRef) (int, error) {
	if ref.Table != "" {
		cols, err := e.tableColumnNames(ref.Table)
		if err != nil {
			return 0, err
		}
		return len(cols), nil
	}
	if sel.From.Subquery != nil {
		// FROM (subquery): count the subquery's output width without
		// executing it (with2 3.3's "SELECT * FROM (SELECT * FROM j)").
		return e.cteAnchorColumnCount(sel.From.Subquery)
	}
	if sel.From.Name != "" {
		if cte, ok := e.findCTE(sel, sel.From.Name); ok {
			if e.resolvingCTEs[cte.Select] {
				// The referenced CTE is already being resolved — a mutual
				// recursion cycle in the anchor-width check (with2 3.2:
				// i→j→k→i). The execution-time circular reference will be
				// reported; skip the width check.
				return 0, errUndeterminedCTEWidth
			}
			if len(cte.Columns) > 0 {
				return len(cte.Columns), nil
			}
			// No declared columns: count the CTE body's own anchor width
			// (recursively, without executing).
			return e.cteAnchorColumnCount(cte.Select)
		}
		cols, err := e.resolveTableColumnNames(sel, sel.From.Name)
		if err != nil {
			return 0, err
		}
		return len(cols), nil
	}
	return 0, nil
}

// evalRecursiveTerms evaluates all recursive terms against a single row of
// the previous iteration, returning the output rows produced.
func (e *SelectEngine) evalRecursiveTerms(recursiveTerms []*sql.SelectStmt, row []interface{}, colDefs []sql.ColumnDef, cte *sql.CTEDef) ([][]interface{}, error) {
	var newRows [][]interface{}
	for _, term := range recursiveTerms {
		rows, err := e.evalRecursiveTerm(term, row, colDefs, cte)
		if err != nil {
			return nil, err
		}
		newRows = append(newRows, rows...)
	}
	return newRows, nil
}

// processCTEOuterQuery applies the outer query's JOINs, WHERE filter, and
// aggregate handling to CTE rows. Returns updated rowMaps/colDefs or a Result
// when the aggregate handler short-circuits.
func (e *SelectEngine) processCTEOuterQuery(s *sql.SelectStmt, allRowMaps []RowMap, colDefs []sql.ColumnDef) ([]RowMap, []sql.ColumnDef, *Result) {
	if len(s.Joins) > 0 {
		var err error
		allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
		if err != nil {
			return nil, nil, &Result{Error: err}
		}
	}
	var whereErr error
	allRowMaps, whereErr = filterRowMapsByWhere(e, s.Where, allRowMaps)
	if whereErr != nil {
		return nil, nil, &Result{Error: whereErr}
	}
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return nil, nil, result
	}
	return allRowMaps, colDefs, nil
}

// validateRecursiveCTEPreconditions runs the recursive CTE validation
// preamble: the anchor-width check, compound arity, body ORDER BY result-column
// resolution, and recursive-term column-reference validation. It returns the
// recursive table's column set and whether an implicit rowid is allowed.
func (e *SelectEngine) validateRecursiveCTEPreconditions(cte *sql.CTEDef, recursiveTerms []*sql.SelectStmt) (map[string]bool, bool, error) {
	if bodyCols, err := e.cteBodyColumnCount(cte); err == errUndeterminedCTEWidth {
		// Mutual recursion cycle: skip the width check; the execution-time
		// circular reference fires below.
	} else if err != nil {
		return nil, false, err
	} else if len(cte.Columns) > 0 && bodyCols != len(cte.Columns) {
		return nil, false, fmt.Errorf("table %s has %d values for %d columns", cte.Name, bodyCols, len(cte.Columns))
	}
	// Validate compound member widths (SQLite's compound arity check), which
	// the recursive path cannot reach through execSelect because the body is
	// executed term-by-term.
	if err := e.validateCompoundColumnCounts(cte.Select); err != nil {
		return nil, false, err
	}
	// The body's trailing ORDER BY resolves against the compound result
	// column names, not the declared CTE columns: "WITH t(a) AS (SELECT 1 AS
	// b UNION ALL SELECT a+1 AS c FROM t ORDER BY a)" is an error while
	// ORDER BY b/c work (with1 10.7.1-3).
	if orderBy, _, _ := cteBodyLimitOffset(cte); len(orderBy) > 0 {
		if err := e.validateCompoundOrderBy(cte.Select, orderBy); err != nil {
			return nil, false, err
		}
	}
	// Recursive terms expose only the CTE's declared columns — no implicit
	// rowid (SQLite rejects "no such column: rowid" in a recursive term —
	// with1 15.1).
	colSet := make(map[string]bool, len(cte.Columns))
	allowRowID := false
	for _, name := range cte.Columns {
		colSet[strings.ToLower(name)] = true
		if isRowIDName(name) {
			allowRowID = true
		}
	}
	if len(colSet) == 0 {
		// No declared columns: the recursive table's column is the anchor's
		// single output column (unnamed, resolved positionally).
		colSet["x"] = true
	}
	if err := e.validateRecursiveTermRefs(recursiveTerms, cte.Name, colSet, allowRowID); err != nil {
		return nil, false, err
	}
	return colSet, allowRowID, nil
}

func (e *SelectEngine) execRecursiveCTE(s *sql.SelectStmt, cte *sql.CTEDef, anchor *sql.SelectStmt, recursiveTerms []*sql.SelectStmt, dedup bool) *Result {
	// validateRecursiveTermRefs validates that every column reference in the
	// recursive terms resolves to the recursive table's declared columns (or
	// an outer-table column via the enclosing scope). SQLite rejects an
	// implicit rowid reference in a recursive term with "no such column:
	// rowid" (with1 15.1: "SELECT rowid+1 FROM d").
	//
	// SQLite's withExpand checks the declared column list against the anchor
	// (leftmost) member's width before the compound arity check, so the
	// column-count error takes precedence when the anchor is wider.
	if _, _, err := e.validateRecursiveCTEPreconditions(cte, recursiveTerms); err != nil {
		return &Result{Error: err}
	}
	colDefs := make([]sql.ColumnDef, len(cte.Columns))
	for i, name := range cte.Columns {
		colDefs[i] = sql.ColumnDef{Name: name}
	}
	if len(colDefs) == 0 {
		colDefs = []sql.ColumnDef{{Name: "x"}}
	}
	// The anchor is the compound prefix up to (but not including) the first
	// recursive term. Execute it as a real compound so multi-row VALUES
	// anchors and INTERSECT/EXCEPT prefixes (e.g. VALUES(1),(200) INTERSECT
	// VALUES(1)) produce the correct seed rows.
	var anchorFirst *sql.SelectStmt
	if len(recursiveTerms) > 0 {
		anchorFirst = copyCompoundChain(anchor, recursiveTerms[0])
	} else {
		anchorFirst = copyCompoundChain(anchor, nil)
	}
	savedScopes := e.withCTEBodyScope(cte)
	anchorResult := e.execSelect(anchorFirst)
	e.cteScopes = savedScopes
	if anchorResult.Error != nil {
		return anchorResult
	}
	rowLimit, bodyLimit, bodyOffset := e.cteRowBudget(s, cte)
	bodyOrderBy, _, _ := cteBodyLimitOffset(cte)
	allRows, err := e.iterateRecursiveCTE(anchorResult.Rows, colDefs, anchorResult.Columns, cte, recursiveTerms, dedup, rowLimit, bodyOrderBy, bodyLimit, bodyOffset)
	if err != nil {
		return &Result{Error: err}
	}
	allRowMaps := make([]RowMap, len(allRows))
	for i, row := range allRows {
		// CTE rows have no implicit rowid (SQLite: "no such column: rowid"
		// on a CTE reference — with1 15.1).
		allRowMaps[i] = buildRowMapFromValuesNoRowID(row, colDefs)
	}
	delete(e.resolvingCTEs, cte.Select)
	allRowMaps, colDefs, procResult := e.processCTEOuterQuery(s, allRowMaps, colDefs)
	if procResult != nil {
		return procResult
	}
	// Window-function pass over the recursive CTE's materialized rows.
	if e.selectHasWindowFuncs(s.Columns) {
		winResult := e.execWindowPass(s, allRowMaps, colDefs)
		if winResult != nil {
			delete(e.resolvingCTEs, cte.Select)
			return e.finalizeSelectResult(winResult, s, winResult.rowMaps)
		}
	}
	outRows, err := buildOutputRowsFromMaps(e, s.Columns, colDefs, allRowMaps)
	if err != nil {
		return &Result{Error: err}
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs, s), Rows: outRows}
	delete(e.resolvingCTEs, cte.Select)
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// validateRecursiveTermRefs validates that every column reference in the
// recursive terms resolves to the recursive table's declared columns. SQLite
// rejects an implicit rowid reference in a recursive term with "no such
// column: rowid" (with1 15.1: "SELECT rowid+1 FROM d").
func (e *SelectEngine) validateRecursiveTermRefs(recursiveTerms []*sql.SelectStmt, cteName string, colSet map[string]bool, allowRowID bool) error {
	for _, term := range recursiveTerms {
		if term == nil {
			continue
		}
		// SQLite rejects aggregate functions in recursive terms (with1
		// 16.x: "recursive aggregate queries not supported").
		if e.termHasAggregate(term) {
			return fmt.Errorf("recursive aggregate queries not supported")
		}
		// SQLite rejects window functions in recursive terms (window1 15.0:
		// "cannot use window functions in recursive queries").
		if e.termHasWindowFunc(term) {
			return fmt.Errorf("cannot use window functions in recursive queries")
		}
		if err := walkRecursiveTermRefs(term, cteName, colSet, allowRowID, e); err != nil {
			return err
		}
	}
	return nil
}

// termHasWindowFunc reports whether a recursive term uses a window function in
// any expression position (SQLite rejects window functions in recursive
// terms).
func (e *SelectEngine) termHasWindowFunc(term *sql.SelectStmt) bool {
	check := func(expr sql.Expr) bool {
		if expr == nil {
			return false
		}
		found := false
		WalkExprFull(expr, func(en sql.Expr) {
			if found {
				return
			}
			fc, ok := en.(*sql.FuncCall)
			if ok && fc.Over != nil {
				found = true
			}
		})
		return found
	}
	for _, col := range term.Columns {
		if check(col.Expr) {
			return true
		}
	}
	if check(term.Where) {
		return true
	}
	for _, g := range term.GroupBy {
		if check(g) {
			return true
		}
	}
	return check(term.Having)
}

// termHasAggregate reports whether a recursive term uses an aggregate function
// in any expression position (SQLite rejects aggregates in recursive terms).
func (e *SelectEngine) termHasAggregate(term *sql.SelectStmt) bool {
	check := func(expr sql.Expr) bool {
		if expr == nil {
			return false
		}
		found := false
		WalkExprFull(expr, func(en sql.Expr) {
			if found {
				return
			}
			fc, ok := en.(*sql.FuncCall)
			if !ok {
				return
			}
			if fn, ok := e.ctx.Functions().Find(fc.Name); ok && fn.Type == function.TypeAggregate {
				// MIN/MAX with 2+ args are scalar.
				if (strings.EqualFold(fc.Name, "MIN") || strings.EqualFold(fc.Name, "MAX")) && len(fc.Args) >= 2 {
					return
				}
				found = true
			}
		})
		return found
	}
	for _, col := range term.Columns {
		if check(col.Expr) {
			return true
		}
	}
	if check(term.Where) {
		return true
	}
	for _, g := range term.GroupBy {
		if check(g) {
			return true
		}
	}
	return check(term.Having)
}

// walkRecursiveTermRefs walks a recursive term's expression positions checking
// every unqualified column reference against the recursive table's declared
// columns and the term's other FROM tables. SQLite rejects an implicit rowid
// reference in a recursive term with "no such column: rowid" (with1 15.1:
// "SELECT rowid+1 FROM d"). Qualified references resolve against their own
// table at join time and are left to the join machinery.
func walkRecursiveTermRefs(term *sql.SelectStmt, cteName string, colSet map[string]bool, allowRowID bool, e *SelectEngine) error {
	// Collect the columns of the term's non-CTE FROM tables (they are valid
	// unqualified references too, e.g. with1 6.2's "SELECT id, fpath || '/'
	// || name FROM f, flat" where id/fpath/name belong to table f).
	otherCols := make(map[string]bool)
	collectFromTableCols(&term.From, cteName, otherCols, e)
	for i := range term.Joins {
		collectFromTableCols(&term.Joins[i].Table, cteName, otherCols, e)
	}
	check := func(expr sql.Expr) error {
		if expr == nil {
			return nil
		}
		var checkErr error
		WalkExprFull(expr, func(en sql.Expr) {
			if checkErr != nil {
				return
			}
			checkErr = checkRecursiveTermRef(en, colSet, otherCols, allowRowID)
		})
		return checkErr
	}
	for _, col := range term.Columns {
		if err := check(col.Expr); err != nil {
			return err
		}
	}
	if err := check(term.Where); err != nil {
		return err
	}
	for _, g := range term.GroupBy {
		if err := check(g); err != nil {
			return err
		}
	}
	if err := check(term.Having); err != nil {
		return err
	}
	return nil
}

// checkRecursiveTermRef validates a single expression node as a column
// reference in a recursive term: it must name the recursive table's declared
// column, another FROM table's column, an allowed rowid, or a quoted
// identifier.
func checkRecursiveTermRef(en sql.Expr, colSet, otherCols map[string]bool, allowRowID bool) error {
	ref, ok := en.(*sql.ColumnRef)
	if !ok || ref.Name == "*" || ref.Table != "" {
		return nil
	}
	if colSet[strings.ToLower(ref.Name)] {
		return nil
	}
	if otherCols[strings.ToLower(ref.Name)] {
		return nil
	}
	if allowRowID && isRowIDName(ref.Name) {
		return nil
	}
	if ref.Quoted {
		return nil
	}
	return fmt.Errorf("no such column: %s", ref.Name)
}

// collectFromTableCols adds the columns of a FROM operand to otherCols unless
// it references the recursive CTE (by name) or is a subquery/aliased operand.
// Subquery operands contribute their own select-list columns.
func collectFromTableCols(ref *sql.TableRef, cteName string, otherCols map[string]bool, e *SelectEngine) {
	if ref == nil {
		return
	}
	// The recursive table reference itself: its columns are handled by
	// colSet in the checker, so do not re-collect them here (and resolving
	// it would recurse).
	if ref.Name != "" && strings.EqualFold(ref.Name, cteName) {
		return
	}
	if ref.Subquery != nil {
		for _, col := range ref.Subquery.Columns {
			if col.As != "" {
				otherCols[strings.ToLower(col.As)] = true
			}
		}
		return
	}
	if ref.Name == "" {
		return
	}
	// A real table: resolve its column names (best-effort — unknown tables
	// will fail at join time with the proper error).
	if cols, err := e.resolveTableColumnNames(&sql.SelectStmt{From: *ref}, ref.Name); err == nil {
		for _, c := range cols {
			otherCols[strings.ToLower(c)] = true
		}
	}
}
