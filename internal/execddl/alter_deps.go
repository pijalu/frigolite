package execddl

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// checkTriggerTableRefs validates that the tables referenced by a trigger body
// exist (ignoring the renamed table, NEW/OLD pseudotables, and the SET keyword).
func (e *DDLExecutor) checkTriggerTableRefs(entry *schema.Entry, oldName string) error {
	bodyRefs := findTableRefsInTrigger(entry.SQL)
	for _, ref := range bodyRefs {
		if err := e.checkTriggerTableRef(entry, ref, oldName); err != nil {
			return err
		}
	}
	return nil
}

// checkTriggerTableRef validates a single table reference found in a trigger
// body, returning an error if the table does not exist anywhere.
func (e *DDLExecutor) checkTriggerTableRef(entry *schema.Entry, ref, oldName string) error {
	lookupName := ref
	if dotIdx := strings.Index(lookupName, "."); dotIdx >= 0 {
		lookupName = lookupName[dotIdx+1:]
	}
	if strings.EqualFold(lookupName, oldName) {
		return nil
	}
	if strings.EqualFold(lookupName, "NEW") || strings.EqualFold(lookupName, "OLD") || strings.EqualFold(lookupName, "SET") {
		return nil
	}
	if _, _, err := e.ctx.FindTable(lookupName); err == nil {
		return nil
	}
	if _, _, err2 := e.ctx.FindView(lookupName); err2 == nil {
		return nil
	}
	refName := ref
	if !strings.Contains(ref, ".") && !e.isTempTrigger(entry) {
		refName = "main." + ref
	}
	return fmt.Errorf("error in trigger %s: no such table: %s", entry.Name, refName)
}

// isTempTrigger reports whether a trigger lives in the TEMP schema. A trigger
// is temp if it was created with CREATE TEMP TRIGGER or if its ON table is a
// temp table (CREATE TEMP TABLE). The stored SQL strips the TEMP keyword, so
// the owning schema is detected by looking up the ON table in the temp
// database context.
func (e *DDLExecutor) isTempTrigger(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	upper := strings.ToUpper(entry.SQL)
	if strings.Contains(upper, "CREATE TEMP TRIGGER") || strings.Contains(upper, "CREATE TEMPORARY TRIGGER") {
		return true
	}
	return e.triggerOnTempTable(entry)
}

// triggerOnTempTable reports whether a trigger's ON table is a temp table
// (CREATE TEMP TABLE), detected by looking up the table in the temp database
// context or by scanning its stored SQL.
func (e *DDLExecutor) triggerOnTempTable(entry *schema.Entry) bool {
	if entry.TblName == "" {
		return false
	}
	if tc := e.ctx.GetDB("temp"); tc != nil {
		if te, err := tc.Schema.FindTable(entry.TblName); err == nil && te != nil {
			return true
		}
	}
	te, err := e.ctx.Schema().FindTable(entry.TblName)
	if err != nil || te == nil {
		return false
	}
	upper := strings.ToUpper(te.SQL)
	return strings.Contains(upper, "CREATE TEMP TABLE") || strings.Contains(upper, "CREATE TEMPORARY TABLE")
}

// checkTriggerConflictCols validates ON CONFLICT (...) target columns in a
// trigger's INSERT statements against the INSERT target table's columns.
func (e *DDLExecutor) checkTriggerConflictCols(entry *schema.Entry) error {
	stmts, perr := parse.ParseSQL(entry.SQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	for _, stmt := range stmts {
		trig, ok := stmt.(*sql.CreateTriggerStmt)
		if !ok {
			continue
		}
		for _, bodyStmt := range trig.Statements {
			if err := e.checkTriggerConflictInsert(entry, bodyStmt); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTriggerConflictInsert validates the ON CONFLICT target columns of one
// INSERT statement inside a trigger body against the INSERT table's columns.
func (e *DDLExecutor) checkTriggerConflictInsert(entry *schema.Entry, bodyStmt sql.Stmt) error {
	ins, ok := bodyStmt.(*sql.InsertStmt)
	if !ok || ins.OnConflict == nil || len(ins.OnConflict.ConflictColumn) == 0 {
		return nil
	}
	targetEntry, err := e.ctx.Schema().FindTable(ins.Table)
	if err != nil {
		return nil
	}
	targetCols := e.ctx.ParseColumnDefs(targetEntry.Name, targetEntry.SQL)
	for _, name := range ins.OnConflict.ConflictColumn {
		found := false
		for _, c := range targetCols {
			if strings.EqualFold(c.Name, name) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("error in trigger %s: no such column: %s", entry.Name, name)
		}
	}
	return nil
}

// collectTriggerColRefs walks a statement and collects ColumnRefs, tracking
// whether we're inside a subquery (which has its own table scope).
func collectTriggerColRefs(stmt sql.Stmt, refs *[]*sql.ColumnRef, inSubquery bool) {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		collectSelectTriggerColRefs(s, refs, inSubquery)
	case *sql.InsertStmt:
		collectInsertTriggerColRefs(s, refs)
	case *sql.UpdateStmt:
		collectUpdateTriggerColRefs(s, refs, inSubquery)
	case *sql.DeleteStmt:
		if s.Where != nil {
			collectExprTriggerColRefs(s.Where, refs, inSubquery)
		}
	case *sql.CreateTriggerStmt:
		for _, bodyStmt := range s.Statements {
			collectTriggerColRefs(bodyStmt, refs, false)
		}
	}
}

// collectInsertTriggerColRefs walks an INSERT statement collecting ColumnRefs.
// INSERT ... SELECT is a subquery; ON CONFLICT ... DO UPDATE SET assignments
// and WHERE can reference columns of the conflict target table.
func collectInsertTriggerColRefs(s *sql.InsertStmt, refs *[]*sql.ColumnRef) {
	if s.Select != nil {
		collectSelectTriggerColRefs(s.Select, refs, true)
	}
	if s.OnConflict != nil {
		for _, a := range s.OnConflict.Assignments {
			collectExprTriggerColRefs(a.Value, refs, false)
		}
		if s.OnConflict.Where != nil {
			collectExprTriggerColRefs(s.OnConflict.Where, refs, false)
		}
	}
}

// collectUpdateTriggerColRefs walks an UPDATE statement collecting ColumnRefs.
// SET values are treated as subquery scope to avoid false positives from
// UPDATE ... SET x=y FROM ... which frigolite doesn't fully parse.
func collectUpdateTriggerColRefs(s *sql.UpdateStmt, refs *[]*sql.ColumnRef, inSubquery bool) {
	for _, a := range s.Assignments {
		collectExprTriggerColRefs(a.Value, refs, true) // true = subquery scope
	}
	if s.Where != nil {
		collectExprTriggerColRefs(s.Where, refs, inSubquery)
	}
}

// collectSelectTriggerColRefs walks a SELECT and collects ColumnRefs.
func collectSelectTriggerColRefs(sel *sql.SelectStmt, refs *[]*sql.ColumnRef, inSubquery bool) {
	if sel == nil {
		return
	}
	// Column refs in SELECT columns are in the current scope
	for _, col := range sel.Columns {
		collectExprTriggerColRefs(col.Expr, refs, inSubquery)
	}
	// WHERE, HAVING, GROUP BY, ORDER BY are in the current scope
	if sel.Where != nil {
		collectExprTriggerColRefs(sel.Where, refs, inSubquery)
	}
	if sel.Having != nil {
		collectExprTriggerColRefs(sel.Having, refs, inSubquery)
	}
	for _, expr := range sel.GroupBy {
		collectExprTriggerColRefs(expr, refs, inSubquery)
	}
	for _, ob := range sel.OrderBy {
		collectExprTriggerColRefs(ob.Expr, refs, inSubquery)
	}
	// JOIN conditions are in the current scope
	for _, join := range sel.Joins {
		if join.On != nil {
			collectExprTriggerColRefs(join.On, refs, inSubquery)
		}
	}
	collectSelectNestedTriggerColRefs(sel, refs, inSubquery)
}

// collectSelectNestedTriggerColRefs walks the nested scopes of a SELECT
// (UNION, CTE subqueries, window definitions), each of which has its own scope.
func collectSelectNestedTriggerColRefs(sel *sql.SelectStmt, refs *[]*sql.ColumnRef, inSubquery bool) {
	// UNION subqueries have their own scope
	if sel.Union != nil {
		collectSelectTriggerColRefs(sel.Union, refs, true)
	}
	// CTE subqueries have their own scope
	for _, cte := range sel.CTEs {
		if cte.Select != nil {
			collectSelectTriggerColRefs(cte.Select, refs, true)
		}
	}
	// Window definitions contain PARTITION BY and ORDER BY expressions
	// that may reference columns of the current scope
	for _, w := range sel.Windows {
		collectWindowDefTriggerColRefs(&w, refs, inSubquery)
	}
}

// collectExprTriggerColRefs walks an expression tree and collects ColumnRefs,
// marking when we enter a subquery (which has its own table scope).
func collectExprTriggerColRefs(expr sql.Expr, refs *[]*sql.ColumnRef, inSubquery bool) {
	switch e := expr.(type) {
	case *sql.ColumnRef:
		// Only collect qualified refs or top-level unqualified refs
		if !inSubquery {
			*refs = append(*refs, e)
		}
	case *sql.BinaryOp:
		collectExprTriggerColRefs(e.Left, refs, inSubquery)
		collectExprTriggerColRefs(e.Right, refs, inSubquery)
	case *sql.UnaryOp:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	case *sql.FuncCall:
		collectFuncCallTriggerColRefs(e, refs, inSubquery)
	case *sql.ParenExpr:
		collectExprTriggerColRefs(e.Expr, refs, inSubquery)
	case *sql.CaseExpr:
		collectCaseExprTriggerColRefs(e, refs, inSubquery)
	case *sql.Between:
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
		collectExprTriggerColRefs(e.Low, refs, inSubquery)
		collectExprTriggerColRefs(e.High, refs, inSubquery)
	case *sql.InList:
		collectInListTriggerColRefs(e, refs, inSubquery)
	case *sql.Subquery, *sql.ExistsExpr:
		collectSubqueryTriggerColRefs(e, refs)
	case *sql.RowValue:
		collectRowValueTriggerColRefs(e, refs, inSubquery)
	}
}

// collectFuncCallTriggerColRefs walks a function call, its FILTER clause and
// window definition.
func collectFuncCallTriggerColRefs(e *sql.FuncCall, refs *[]*sql.ColumnRef, inSubquery bool) {
	for _, arg := range e.Args {
		collectExprTriggerColRefs(arg, refs, inSubquery)
	}
	if e.Filter != nil {
		collectExprTriggerColRefs(e.Filter, refs, inSubquery)
	}
	if e.Over != nil {
		collectWindowDefTriggerColRefs(e.Over, refs, inSubquery)
	}
}

// collectCaseExprTriggerColRefs walks a CASE expression's operand, WHEN/THEN
// pairs and ELSE branch.
func collectCaseExprTriggerColRefs(e *sql.CaseExpr, refs *[]*sql.ColumnRef, inSubquery bool) {
	if e.Operand != nil {
		collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	}
	for _, w := range e.Whens {
		collectExprTriggerColRefs(w.When, refs, inSubquery)
		collectExprTriggerColRefs(w.Then, refs, inSubquery)
	}
	if e.Else != nil {
		collectExprTriggerColRefs(e.Else, refs, inSubquery)
	}
}

// collectInListTriggerColRefs walks an IN-list expression's operand and items.
func collectInListTriggerColRefs(e *sql.InList, refs *[]*sql.ColumnRef, inSubquery bool) {
	collectExprTriggerColRefs(e.Operand, refs, inSubquery)
	for _, item := range e.List {
		collectExprTriggerColRefs(item, refs, inSubquery)
	}
}

// collectRowValueTriggerColRefs walks a row-value expression's elements.
func collectRowValueTriggerColRefs(e *sql.RowValue, refs *[]*sql.ColumnRef, inSubquery bool) {
	for _, v := range e.Values {
		collectExprTriggerColRefs(v, refs, inSubquery)
	}
}

// collectSubqueryTriggerColRefs descends into a subquery expression. Subquery
// expressions have their own scope — refs inside are not collected, but nested
// refs are still walked with inSubquery=true.
func collectSubqueryTriggerColRefs(expr sql.Expr, refs *[]*sql.ColumnRef) {
	var sel *sql.SelectStmt
	switch s := expr.(type) {
	case *sql.Subquery:
		sel = s.Select
	case *sql.ExistsExpr:
		sel = s.Select
	}
	if sel != nil {
		collectSelectTriggerColRefs(sel, refs, true)
	}
}

// hasViewCircularRef checks if a view has a circular reference (references its own name).
// View SQL format: "CREATE VIEW name AS SELECT ..."
func hasViewCircularRef(viewSQL, viewName string) bool {
	if viewSQL == "" || viewName == "" {
		return false
	}
	// Find " AS " after the view definition — extract the SELECT body
	upper := strings.ToUpper(viewSQL)
	idx := strings.Index(upper, " AS ")
	if idx < 0 {
		return false
	}
	// Get the SELECT body part after " AS "
	bodySQL := viewSQL[idx+4:]

	// Parse the SELECT to check for circular references
	stmts, perr := parse.ParseSQL(bodySQL)
	if perr != nil || len(stmts) == 0 {
		return false
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return false
	}
	return viewBodyCircularRef(sel, viewName)
}

// viewBodyCircularRef reports whether a parsed view body references the view's
// own name as a table (directly, via JOIN, or through a used CTE).
func viewBodyCircularRef(sel *sql.SelectStmt, viewName string) bool {
	// A CTE declared in the view body shadows the view name, so FROM v0 when
	// "WITH v0 AS (...)" exists refers to the CTE, not the view.
	hasCTE := false
	for _, cte := range sel.CTEs {
		if strings.EqualFold(cte.Name, viewName) {
			hasCTE = true
		}
	}
	if !hasCTE && strings.EqualFold(sel.From.Name, viewName) {
		return true
	}
	if !hasCTE && viewJoinsReference(sel, viewName) {
		return true
	}
	return cteBodyReferencesView(sel, viewName)
}

// viewJoinsReference reports whether any JOIN in the statement references the
// given table name.
func viewJoinsReference(sel *sql.SelectStmt, viewName string) bool {
	for _, j := range sel.Joins {
		if strings.EqualFold(j.Table.Name, viewName) {
			return true
		}
	}
	return false
}

// cteBodyReferencesView reports whether any CTE whose body references the view
// name is actually used by the main statement (a circular dependency).
func cteBodyReferencesView(sel *sql.SelectStmt, viewName string) bool {
	for _, cte := range sel.CTEs {
		if cte.Select != nil && strings.EqualFold(cte.Select.From.Name, viewName) {
			// The CTE references the view in its FROM clause.
			// Only flag as circular if the CTE is actually used in the main statement.
			// If the main statement has no FROM (e.g., VALUES subquery), the CTE is unused.
			if isCTEReferencedInMain(sel, cte.Name) {
				return true
			}
		}
	}
	return false
}

// checkIndexRenameDependencies validates that every index on the given table
// references only existing columns. SQLite re-parses index definitions when
// re-running ALTER TABLE RENAME COLUMN and rejects the rename if any indexed
// column does not exist ("error in index %s: no such column: %s").
func (e *DDLExecutor) checkIndexRenameDependencies(tableName string) *Result {
	entries, err := e.ctx.Schema().GetEntries(schema.TypeIndex)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		if res := e.checkIndexRenameCols(entry, tableName); res != nil {
			return res
		}
	}
	return nil
}

// checkIndexRenameCols validates that every column referenced by one index
// definition exists on its table.
func (e *DDLExecutor) checkIndexRenameCols(entry *schema.Entry, tableName string) *Result {
	// Auto-generated indexes (sqlite_autoindex_*) have empty SQL and are
	// not re-validated by SQLite.
	if strings.HasPrefix(strings.ToLower(entry.Name), "sqlite_autoindex_") {
		return nil
	}
	// An index with empty SQL (e.g. edited via writable_schema) is
	// malformed; SQLite reports "error in index %s: " on rename.
	if strings.TrimSpace(entry.SQL) == "" {
		return &Result{Error: fmt.Errorf("error in index %s: ", entry.Name)}
	}
	cols := indexColumnRefs(entry.SQL)
	for _, col := range cols {
		// Skip numeric literals in expression index keys (e.g. 1.0 in
		// a+1.0) — they are values, not column references.
		if _, err := strconv.ParseFloat(col, 64); err == nil {
			continue
		}
		// Skip SQL keywords that the tokenizer extracts from expressions
		// (e.g. IN in "WHERE a IN (...)").
		if isSQLKeywordOrPseudo(strings.ToUpper(col)) {
			continue
		}
		if !e.tableHasColumn(tableName, col) {
			return &Result{Error: fmt.Errorf("error in index %s: no such column: %s", entry.Name, col)}
		}
	}
	return nil
}

// checkViewDependencies validates all views before dropping a column.
// Uses simple text-based scanning to find column references.
func (e *DDLExecutor) checkViewDependencies(tableName, columnName string) *Result {
	views, err := e.ctx.Schema().GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		refTable, fromIdx := viewFromTable(view.SQL)
		if refTable == "" {
			continue
		}
		if errMsg := e.checkViewColRefs(view, refTable, fromIdx, tableName, columnName); errMsg != "" {
			return &Result{Error: fmt.Errorf("error in view %s: %s", view.Name, errMsg)}
		}
	}
	return nil
}

// checkViewColRefs validates that every column referenced in one view's
// SELECT list exists on its FROM table. Returns an error message, or "" when
// the view is valid.
func (e *DDLExecutor) checkViewColRefs(view *schema.Entry, refTable string, fromIdx int, tableName, columnName string) string {
	// Get the referenced table's column definitions
	entry, findErr := e.ctx.Schema().FindTable(refTable)
	if findErr != nil {
		// Table doesn't exist - the view references a non-existent table
		return findErr.Error()
	}
	colDefs := e.ctx.ColCache()[refTable]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	}
	// Build set of valid column names (excluding dropped)
	validCols := make(map[string]bool)
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		validCols[strings.ToUpper(cd.Name)] = true
	}
	refersToTarget := strings.EqualFold(refTable, tableName)
	for _, col := range viewSelectCols(view.SQL, fromIdx) {
		if bad := viewColTokenInvalid(col, validCols, refersToTarget, columnName); bad != "" {
			return bad
		}
	}
	return ""
}

// viewColTokenInvalid reports whether a single SELECT-list token is not a
// valid column reference for the view's FROM table. Returns the offending
// token, or "" when the token is valid (or is skipped).
func viewColTokenInvalid(col string, validCols map[string]bool, refersToTarget bool, columnName string) string {
	col = strings.TrimSpace(col)
	if col == "" {
		return ""
	}
	upperCol := strings.ToUpper(col)
	if upperCol == "DISTINCT" || upperCol == "ALL" || upperCol == "AS" {
		return ""
	}
	if strings.Contains(upperCol, ".") {
		parts := strings.Split(upperCol, ".")
		if len(parts) == 2 {
			upperCol = parts[1]
		}
	}
	// Skip the column being dropped — its validity is checked later
	if refersToTarget && strings.EqualFold(col, columnName) {
		return ""
	}
	if !validCols[strings.ToUpper(upperCol)] && upperCol != "*" {
		return "no such column: " + col
	}
	return ""
}

// checkViewDropDependencies checks if dropping the column would break
// views that reference the target table.
func (e *DDLExecutor) checkViewDropDependencies(tableName, columnName string) *Result {
	views, err := e.ctx.Schema().GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		refTable, fromIdx := viewFromTable(view.SQL)
		if fromIdx < 0 || !strings.EqualFold(refTable, tableName) {
			continue
		}
		for _, col := range viewSelectCols(view.SQL, fromIdx) {
			col = strings.TrimSpace(col)
			if strings.EqualFold(col, columnName) {
				return &Result{Error: fmt.Errorf("error in view %s after drop column: no such column: %s",
					view.Name, columnName)}
			}
		}
	}
	return nil
}

// viewFromTable extracts the first table name following " FROM " in a view's
// SQL, along with the byte offset of the FROM keyword. Returns ("", -1) when
// the view SQL has no FROM clause.
func viewFromTable(viewSQL string) (string, int) {
	upperSQL := strings.ToUpper(viewSQL)
	fromIdx := strings.Index(upperSQL, " FROM ")
	if fromIdx < 0 {
		return "", -1
	}
	fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
	spaceIdx := strings.IndexAny(fromRest, " \n\t\r")
	if spaceIdx > 0 {
		return fromRest[:spaceIdx], fromIdx
	}
	return fromRest, fromIdx
}

// viewSelectCols splits the SELECT-list fragment of a view's SQL into
// whitespace/comma-separated tokens. Returns nil when the SQL has no SELECT.
func viewSelectCols(viewSQL string, fromIdx int) []string {
	selIdx := strings.Index(strings.ToUpper(viewSQL), "SELECT ")
	if selIdx < 0 {
		return nil
	}
	afterSelect := viewSQL[selIdx+7 : fromIdx]
	return strings.FieldsFunc(afterSelect, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	})
}

// validateViewSQL checks if a view's SQL references a valid table and columns.
// Returns an error message if the view has issues, empty string otherwise.
//
//lint:ignore U1000  Planned for P1 ALTER TABLE
func (e *DDLExecutor) validateViewSQL(viewSQL, tableName, columnName string) string {
	stmts, perr := parse.ParseSQL(viewSQL)
	if perr != nil || len(stmts) == 0 {
		return ""
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || sel == nil {
		return ""
	}
	// Find referenced table in FROM clause
	refTable := sel.From.Name
	if refTable == "" {
		return ""
	}
	// Check if the referenced table exists and has the expected columns
	entry, err := e.ctx.Schema().FindTable(refTable)
	if err != nil {
		// Table doesn't exist
		return fmt.Sprintf("no such table: %s", refTable)
	}
	// Parse the view's column references
	colRefs := collectColumnRefs(sel)
	colDefs := e.ctx.ColCache()[refTable]
	if colDefs == nil {
		colDefs = e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	}
	// Build set of valid column names (including dropped for position check)
	validCols := make(map[string]bool)
	for _, cd := range colDefs {
		validCols[strings.ToUpper(cd.Name)] = true
	}
	// Check each column reference in the view
	for _, ref := range colRefs {
		if !validCols[strings.ToUpper(ref)] {
			return fmt.Sprintf("no such column: %s", ref)
		}
	}
	return ""
}

// collectExprRefs collects column references from an expression.
//
//lint:ignore U1000  Utility for future use
func collectExprRefs(expr sql.Expr, refs *[]string) {
	switch e := expr.(type) {
	case *sql.ColumnRef:
		*refs = append(*refs, e.Name)
	case *sql.ParenExpr:
		collectExprRefs(e.Expr, refs)
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		collectBinaryExprRefs(e, refs)
	case *sql.UnaryOp:
		collectExprRefs(e.Operand, refs)
	case *sql.FuncCall:
		collectFuncCallExprRefs(e, refs)
	case *sql.CaseExpr:
		collectCaseExprRefs(e, refs)
	case *sql.RowValue:
		collectRowValueExprRefs(e, refs)
	case *sql.CastExpr, *sql.IsNull, *sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		collectExprRefs(exprOperandOf(e), refs)
	case *sql.InList:
		collectInListExprRefs(e, refs)
	case *sql.Between:
		collectBetweenExprRefs(e, refs)
	case *sql.Subquery:
		collectSubqueryExprRefs(e, refs)
	}
}

// collectBinaryExprRefs walks both sides of a binary-style expression.
func collectBinaryExprRefs(e sql.Expr, refs *[]string) {
	var left, right sql.Expr
	switch s := e.(type) {
	case *sql.BinaryOp:
		left, right = s.Left, s.Right
	case *sql.IsDistinctFrom:
		left, right = s.Left, s.Right
	case *sql.IsNotDistinctFrom:
		left, right = s.Left, s.Right
	}
	collectExprRefs(left, refs)
	collectExprRefs(right, refs)
}

// exprOperandOf returns the single operand of a unary-style expression, or nil
// when the expression type has no single operand.
func exprOperandOf(e sql.Expr) sql.Expr {
	switch s := e.(type) {
	case *sql.UnaryOp:
		return s.Operand
	case *sql.CastExpr:
		return s.Operand
	case *sql.IsNull:
		return s.Operand
	case *sql.IsNotNull:
		return s.Operand
	case *sql.IsTrue:
		return s.Operand
	case *sql.IsFalse:
		return s.Operand
	}
	return nil
}

// collectFuncCallExprRefs walks a function call's arguments.
func collectFuncCallExprRefs(e *sql.FuncCall, refs *[]string) {
	for _, arg := range e.Args {
		collectExprRefs(arg, refs)
	}
}

// collectCaseExprRefs walks a CASE expression's operand, WHEN/THEN pairs and
// ELSE branch.
func collectCaseExprRefs(e *sql.CaseExpr, refs *[]string) {
	collectExprRefs(e.Operand, refs)
	for _, w := range e.Whens {
		collectExprRefs(w.When, refs)
		collectExprRefs(w.Then, refs)
	}
	if e.Else != nil {
		collectExprRefs(e.Else, refs)
	}
}

// collectRowValueExprRefs walks a row-value expression's elements.
func collectRowValueExprRefs(e *sql.RowValue, refs *[]string) {
	for _, v := range e.Values {
		collectExprRefs(v, refs)
	}
}

// collectInListExprRefs walks an IN-list expression's operand and items. IN
// items may be column references and must be decoded with affinity too.
func collectInListExprRefs(e *sql.InList, refs *[]string) {
	collectExprRefs(e.Operand, refs)
	for _, item := range e.List {
		collectExprRefs(item, refs)
	}
}

// collectBetweenExprRefs walks a BETWEEN expression's three parts.
func collectBetweenExprRefs(e *sql.Between, refs *[]string) {
	collectExprRefs(e.Operand, refs)
	collectExprRefs(e.Low, refs)
	collectExprRefs(e.High, refs)
}

// collectSubqueryExprRefs descends into a subquery to collect column
// references that may be correlated with the outer query.
func collectSubqueryExprRefs(e *sql.Subquery, refs *[]string) {
	if e.Select != nil {
		if e.Select.Where != nil {
			collectExprRefs(e.Select.Where, refs)
		}
		for _, col := range e.Select.Columns {
			collectExprRefs(col.Expr, refs)
		}
	}
}

// exprReferencesColumn checks if an expression references a specific column.
func exprReferencesColumn(expr sql.Expr, columnName string) bool {
	switch e := expr.(type) {
	case *sql.ColumnRef:
		return strings.EqualFold(e.Name, columnName)
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		return exprRefsBinaryColumn(e, columnName)
	case *sql.UnaryOp, *sql.IsNull, *sql.IsNotNull, *sql.CastExpr:
		return exprReferencesColumn(exprOperandOf(e), columnName)
	case *sql.FuncCall:
		return exprRefsFuncCallColumn(e, columnName)
	case *sql.Between:
		return exprRefsBetweenColumn(e, columnName)
	case *sql.InList:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.CaseExpr:
		return exprRefsCaseColumn(e, columnName)
	case *sql.RowValue:
		return exprRefsRowValueColumn(e, columnName)
	default:
		return false // literals, subqueries, unknown nodes
	}
}

// exprRefsBinaryColumn reports whether either side of a binary-style expression
// references the given column.
func exprRefsBinaryColumn(e sql.Expr, columnName string) bool {
	var left, right sql.Expr
	switch s := e.(type) {
	case *sql.BinaryOp:
		left, right = s.Left, s.Right
	case *sql.IsDistinctFrom:
		left, right = s.Left, s.Right
	case *sql.IsNotDistinctFrom:
		left, right = s.Left, s.Right
	}
	return exprReferencesColumn(left, columnName) || exprReferencesColumn(right, columnName)
}

// exprRefsFuncCallColumn reports whether any argument of a function call
// references the given column.
func exprRefsFuncCallColumn(e *sql.FuncCall, columnName string) bool {
	for _, arg := range e.Args {
		if exprReferencesColumn(arg, columnName) {
			return true
		}
	}
	return false
}

// exprRefsBetweenColumn reports whether a BETWEEN expression references the
// given column in any of its three parts.
func exprRefsBetweenColumn(e *sql.Between, columnName string) bool {
	return exprReferencesColumn(e.Operand, columnName) ||
		exprReferencesColumn(e.Low, columnName) || exprReferencesColumn(e.High, columnName)
}

// exprRefsCaseColumn reports whether a CASE expression references the given
// column in its operand, WHEN/THEN pairs or ELSE branch.
func exprRefsCaseColumn(e *sql.CaseExpr, columnName string) bool {
	if exprReferencesColumn(e.Operand, columnName) {
		return true
	}
	for _, when := range e.Whens {
		if exprReferencesColumn(when.When, columnName) || exprReferencesColumn(when.Then, columnName) {
			return true
		}
	}
	return e.Else != nil && exprReferencesColumn(e.Else, columnName)
}

// exprRefsRowValueColumn reports whether any element of a row value references
// the given column.
func exprRefsRowValueColumn(e *sql.RowValue, columnName string) bool {
	for _, v := range e.Values {
		if exprReferencesColumn(v, columnName) {
			return true
		}
	}
	return false
}
