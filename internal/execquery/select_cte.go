package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func ViewDeclaredColumns(viewSQL string) []string {
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
func (e *SelectEngine) execSelectView(entry *schema.Entry) *Result {
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
		// trusted_schema=OFF blocks non-innocuous user functions in view
		// bodies (trustschema1-2.111/2.141); TEMP views are always trusted
		// (2.120/2.150). Directonly functions are always blocked.
		if !e.expandingTempView {
			if name := e.unsafeSelectFunc(sel); name != "" {
				return &Result{Error: fmt.Errorf("unsafe use of %s()", name)}
			}
		}
		return e.execSelect(sel)
	}
	return &Result{Error: fmt.Errorf("exec: view does not contain SELECT")}
}

// unsafeSelectFunc returns the name of the first function call in a SELECT
// that is unsafe under the current trusted_schema setting, or "" when safe.
// It walks the SELECT's result columns, WHERE, GROUP BY, HAVING, and ORDER BY.
func (e *SelectEngine) unsafeSelectFunc(sel *sql.SelectStmt) string {
	var unsafe string
	check := func(expr sql.Expr) {
		if expr == nil || unsafe != "" {
			return
		}
		WalkExprFull(expr, func(en sql.Expr) {
			if unsafe != "" {
				return
			}
			if fc, ok := en.(*sql.FuncCall); ok && !e.ctx.SchemaFunctionSafe(fc.Name) {
				unsafe = fc.Name
			}
		})
	}
	for _, col := range sel.Columns {
		check(col.Expr)
	}
	check(sel.Where)
	for _, g := range sel.GroupBy {
		check(g)
	}
	check(sel.Having)
	for _, ob := range sel.OrderBy {
		check(ob.Expr)
	}
	return unsafe
}

// buildNoFromRowMaps creates RowMaps for no-FROM SELECT results (used for ORDER BY).
func (e *SelectEngine) buildNoFromRowMaps(rows [][]interface{}, columns []string) []RowMap {
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
func (e *SelectEngine) execSelectFromSubquery(s *sql.SelectStmt) *Result {
	// Execute the subquery. A FROM-clause derived table is non-lateral
	// (SQLite SF_NestedFrom): see SelectEngine.derivedScope.
	savedDerived := e.derivedScope
	e.derivedScope = true
	subqResult := e.execSelect(s.From.Subquery)
	e.derivedScope = savedDerived
	if subqResult.Error != nil {
		return subqResult
	}

	// Build colDefs from subquery column names, carrying the subquery's
	// expression affinities so row-map values wrap correctly (e.g. CAST(...
	// AS REAL) produces a REAL-affinity column; a table column reference
	// inherits the table column's declared type; a function call has no
	// affinity, matching SQLite sqlite3ExprAffinity).
	colDefs := make([]sql.ColumnDef, len(subqResult.Columns))
	subqDefs := e.ctx.ViewColumnDefsFromSelect(s.From.Subquery)
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

// splitRecursiveCTE splits a self-referencing CTE body into the anchor
// compound (the terms before the first term that references the CTE) and the
// recursive terms (the self-referencing suffix). SQLite requires the
// self-referencing terms to form a non-empty suffix of the compound: the
// first term must not reference the CTE (an anchor must exist) and every
// term from the first self-reference to the end must reference it. Returns an
// error ("circular reference: NAME") when the body violates that shape.
// A body that never references the CTE (WITH RECURSIVE on a non-recursive
// body) yields the whole chain as the anchor with no recursive terms.
func splitRecursiveCTE(cte *sql.CTEDef) (anchor *sql.SelectStmt, recursive []*sql.SelectStmt, err error) {
	if cte == nil || cte.Select == nil {
		return nil, nil, nil
	}
	var terms []*sql.SelectStmt
	for t := cte.Select; t != nil; t = t.Union {
		terms = append(terms, t)
	}
	firstSelf := -1
	for i, t := range terms {
		if termFromRefersTo(t, cte.Name) {
			firstSelf = i
			break
		}
	}
	if firstSelf == 0 {
		// The first term itself references the CTE: there is no anchor, so
		// the self-reference cannot seed a recursion (e.g. "WITH i AS
		// (SELECT 5 FROM i UNION SELECT 8 FROM i)").
		return nil, nil, fmt.Errorf("circular reference: %s", cte.Name)
	}
	if firstSelf == -1 {
		// WITH RECURSIVE on a body that does not reference the CTE: the
		// whole body is the anchor.
		return cte.Select, nil, nil
	}
	// Every term from the first self-reference to the end must reference the
	// CTE; a non-recursive term after the recursion starts is reported as a
	// circular reference (e.g. "SELECT 1 UNION ALL SELECT i+1 FROM s WHERE
	// i<3 UNION ALL SELECT 4"). SQLite distinguishes two over-reference
	// errors: a term whose FROM names the recursive table more than once
	// reports "multiple references to recursive table: NAME" (with2 1.16:
	// "FROM t4, main.t4, t4"), while a term whose FROM names it once but a
	// nested subquery references it again reports "multiple recursive
	// references: NAME" (with1 7.5: "FROM tree, t WHERE p IN (SELECT id
	// FROM t)"; 7.6's subquery reference in the anchor stays "circular
	// reference").
	for i := firstSelf; i < len(terms); i++ {
		if !termFromRefersTo(terms[i], cte.Name) {
			return nil, nil, fmt.Errorf("circular reference: %s", cte.Name)
		}
		if termFromRefCount(terms[i], cte.Name) > 1 {
			return nil, nil, fmt.Errorf("multiple references to recursive table: %s", cte.Name)
		}
		if termReferenceCount(terms[i], cte.Name) > 1 {
			return nil, nil, fmt.Errorf("multiple recursive references: %s", cte.Name)
		}
	}
	return terms[0], terms[firstSelf:], nil
}

// termFromRefCount counts references to name in a compound term's FROM
// positions only (base table + joins, including nested FROM subqueries). A
// value over 1 means the FROM names the recursive table more than once
// (SQLite's "multiple references to recursive table").
func termFromRefCount(s *sql.SelectStmt, name string) int {
	if s == nil {
		return 0
	}
	count := fromRefCount(&s.From, name)
	for i := range s.Joins {
		count += fromRefCount(&s.Joins[i].Table, name)
	}
	return count
}

// termReferenceCount counts how many times a compound term references name in
// its FROM sources, joins, and nested subqueries (in both FROM and WHERE/other
// expression positions). A value over 1 means the term is recursive and also
// re-references the recursive table from a subquery, which SQLite rejects as
// "multiple recursive references".
func termReferenceCount(s *sql.SelectStmt, name string) int {
	if s == nil {
		return 0
	}
	count := 0
	count += fromRefCount(&s.From, name)
	for i := range s.Joins {
		count += fromRefCount(&s.Joins[i].Table, name)
	}
	// Nested subqueries in expression positions (WHERE/HAVING/SELECT list)
	// referencing the CTE also count (with1 7.5's "p IN (SELECT id FROM t)").
	count += exprTreeRefCount(s.Where, name)
	for _, col := range s.Columns {
		count += exprTreeRefCount(col.Expr, name)
	}
	for _, g := range s.GroupBy {
		count += exprTreeRefCount(g, name)
	}
	count += exprTreeRefCount(s.Having, name)
	for _, ob := range s.OrderBy {
		count += exprTreeRefCount(ob.Expr, name)
	}
	return count
}

// fromRefCount counts references to name in a FROM operand (direct name/alias
// or a nested subquery).
func fromRefCount(ref *sql.TableRef, name string) int {
	if ref == nil {
		return 0
	}
	if ref.Name == name || ref.As == name {
		return 1
	}
	if ref.Subquery != nil {
		return selectFromRefCount(ref.Subquery, name)
	}
	return 0
}

// selectFromRefCount counts references to name across a SELECT's FROM/joins
// and its union chain.
func selectFromRefCount(s *sql.SelectStmt, name string) int {
	if s == nil {
		return 0
	}
	count := fromRefCount(&s.From, name)
	for i := range s.Joins {
		count += fromRefCount(&s.Joins[i].Table, name)
	}
	if s.Union != nil {
		count += selectFromRefCount(s.Union, name)
	}
	return count
}

// exprTreeRefCount counts references to name inside an expression tree,
// descending into subqueries.
func exprTreeRefCount(expr sql.Expr, name string) int {
	if expr == nil {
		return 0
	}
	count := 0
	WalkExprFull(expr, func(en sql.Expr) {
		if sub, ok := en.(*sql.Subquery); ok && sub.Select != nil {
			count += selectFromRefCount(sub.Select, name)
			return
		}
	})
	return count
}

// termFromRefersTo reports whether a single compound term's FROM sources (base
// table, joins, nested subqueries — but NOT its Union chain) reference name.
func termFromRefersTo(s *sql.SelectStmt, name string) bool {
	if s == nil {
		return false
	}
	if declaresCTE(s, name) {
		// The term's own WITH shadows the outer name; its FROM reference
		// resolves to the shadow (with1 21.1).
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

// withCTEBodyScope truncates e.cteScopes to the scopes visible to a CTE body
// (the scopes up to and including the CTE's defining scope), returning the
// previous stack for the caller to restore. A CTE body must not see deeper
// scopes from the reference site: with1 17.5 has x2 (defined in the outer
// WITH) referenced from inside x3's nested WITH that shadows x1 — x2's body
// must resolve the outer x1, not the shadowed one.
func (e *SelectEngine) withCTEBodyScope(cte *sql.CTEDef) [][]sql.CTEDef {
	saved := e.cteScopes
	if cte != nil && cte.ScopeDepth >= 0 && cte.ScopeDepth+1 <= len(e.cteScopes) {
		e.cteScopes = e.cteScopes[:cte.ScopeDepth+1]
	}
	return saved
}

// findCTE returns the CTE definition whose name matches the given table
// reference. It first checks the CTEs declared directly on the statement,
// then consults enclosing WITH clauses (innermost scope first), matching
// SQLite's name-resolution order for nested queries.
func (e *SelectEngine) findCTE(s *sql.SelectStmt, name string) (sql.CTEDef, bool) {
	if s == nil {
		return sql.CTEDef{}, false
	}
	for _, cte := range s.CTEs {
		if strings.EqualFold(cte.Name, name) {
			return cte, true
		}
	}
	for i := len(e.cteScopes) - 1; i >= 0; i-- {
		for _, cte := range e.cteScopes[i] {
			if strings.EqualFold(cte.Name, name) {
				return cte, true
			}
		}
	}
	return sql.CTEDef{}, false
}

// materializeCTEForJoin executes a CTE body and builds column definitions and
// row maps suitable for use as a join operand.
func (e *SelectEngine) materializeCTEForJoin(cte *sql.CTEDef) ([]sql.ColumnDef, []RowMap, error) {
	savedScopes := e.withCTEBodyScope(cte)
	cteResult := e.execSelect(cte.Select)
	e.cteScopes = savedScopes
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
		// CTE rows have no implicit rowid (SQLite: "no such column: rowid"
		// on a CTE reference — with1 15.1).
		rowMaps[i] = buildRowMapFromValuesNoRowID(row, colDefs)
	}
	return colDefs, rowMaps, nil
}

// execRecursiveCTE executes a recursive CTE (WITH RECURSIVE ...).
// The CTE definition is a compound SELECT: the first term is the anchor
// (seed) and every following term is a recursive term (SQLite allows several
// recursive terms; each is evaluated per iteration against the rows produced
// in the previous iteration).
// cteBodyColumnCount returns the output column width of a CTE body, taken
// from the anchor (leftmost) member's select list. The width is derived
// statically (executing the body would re-enter the CTE and report a false
// circular reference).
func (e *SelectEngine) cteBodyColumnCount(cte *sql.CTEDef) (int, error) {
	if cte == nil || cte.Select == nil {
		return 0, nil
	}
	return e.cteAnchorColumnCount(cte.Select)
}

// the base table rows and each joined table. Returns combined rowMaps and
// colDefs.

// filterSubqueryRows applies a WHERE expression to filter rows from a subquery result.
func (e *SelectEngine) filterSubqueryRows(allRows [][]interface{}, allRowMaps []RowMap, where sql.Expr) ([][]interface{}, []RowMap, error) {
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

// FindCTE returns the CTE definition for name in the statement's CTE scope.
// Exported for the expression evaluator's column-affinity analysis.
func (e *SelectEngine) FindCTE(s *sql.SelectStmt, name string) (sql.CTEDef, bool) {
	return e.findCTE(s, name)
}

// FindCTEByScope returns the CTE definition for name from the enclosing
// WITH-clause scopes only (no statement-level CTE list). Used by UPDATE ...
// FROM / DELETE ... FROM to resolve a CTE used as a FROM table, where the
// statement's own CTEs were pushed onto the scope stack.
func (e *SelectEngine) FindCTEByScope(name string) (sql.CTEDef, bool) {
	for i := len(e.cteScopes) - 1; i >= 0; i-- {
		for _, cte := range e.cteScopes[i] {
			if strings.EqualFold(cte.Name, name) {
				return cte, true
			}
		}
	}
	return sql.CTEDef{}, false
}
