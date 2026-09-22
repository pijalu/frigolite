// Package exec implements query execution.
//
// This file holds DDL execution for CREATE VIEW (validation, schema context,
// and SQL-text serialization). Split out of ddl_trigger.go to keep each file
// within the repository's size budget.
package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

func (e *DDLExecutor) execCreateView(s *sql.CreateViewStmt) *Result {
	if err := e.ctx.Authorize(auth.ActionCreateView, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// build.c sqlite3CheckObjectName: the "sqlite_" prefix is reserved in
	// every namespace, including views ("object name reserved for internal
	// use: sqlite_v1", index.test 7.x).
	if res := e.validateReservedName(strings.TrimSpace(s.Name)); res != nil {
		return res
	}
	ctx, viewName := resolveViewContext(e, s)
	// SQLite validates a view body at CREATE time: it must not contain bound
	// parameters ("parameters are not allowed in views") and — for a non-temp
	// view — must not reference objects in an attached database ("view v1
	// cannot reference objects in database aux"). A view body's WITH clause
	// subqueries are walked too (with4 120/130).
	if err := e.validateViewBody(s, viewName, ctx); err != nil {
		return &Result{Error: err}
	}
	colsClause := ""
	if len(s.Columns) > 0 {
		colsClause = "(" + strings.Join(s.Columns, ", ") + ")"
	}
	sqlStr := buildViewSQL(s, viewName, colsClause)

	// Check for duplicate view name. IF NOT EXISTS silently succeeds;
	// SQLite otherwise raises "view v1 already exists".
	if existing, _ := ctx.Schema.FindView(viewName); existing != nil {
		if s.IfNotExists {
			return &Result{}
		}
		return &Result{Error: fmt.Errorf("view %s already exists", viewName)}
	}

	entry := &schema.Entry{
		Type:     schema.TypeView,
		Name:     viewName,
		TblName:  viewName,
		RootPage: 0,
	}
	// SQLite strips the "IF NOT EXISTS" prefix from stored CREATE VIEW SQL
	// (the object exists, so the clause is redundant). Match that so the
	// stored schema round-trips identically. A keyword inadvertently used as
	// the view name (e.g. "CREATE VIEW IF NOT EXISTS IF AS ..." → stored
	// "CREATE VIEW IF AS ...") then fails to re-parse, which SQLite reports
	// as a malformed schema entry.
	entry.SQL = stripIfNotExists(sqlStr)
	if _, err := parse.ParseSQL(entry.SQL); err != nil {
		return &Result{Error: fmt.Errorf("malformed database schema (%s) - %v", viewName, err)}
	}
	if err := ctx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	return &Result{}
}

func (e *DDLExecutor) validateViewBody(s *sql.CreateViewStmt, viewName string, ctx *DatabaseContext) error {
	// resolveViewContext already routes CREATE TEMP VIEW to the temp context.
	isTemp := ctx == e.ctx.GetDB("temp")
	return e.checkViewSelect(viewName, s.Select, isTemp)
}

// checkViewSelect walks one SELECT level of a view body: it validates the
// FROM/join schema references, descends into the nested SELECT bodies
// (CTEs, the FROM subquery, join subqueries, the UNION chain — with4
// 110/120/130) and the expression subqueries, and rejects bound parameters
// ("parameters are not allowed in views").
func (e *DDLExecutor) checkViewSelect(viewName string, sel *sql.SelectStmt, isTemp bool) error {
	if sel == nil {
		return nil
	}
	if err := e.checkViewFromRefs(viewName, sel, isTemp); err != nil {
		return err
	}
	for _, next := range viewChildSelects(sel) {
		if err := e.checkViewSelect(viewName, next, isTemp); err != nil {
			return err
		}
	}
	var checkErr error
	walkExprsForView(sel, func(expr sql.Expr) {
		if checkErr != nil {
			return
		}
		if _, ok := expr.(*sql.ParameterExpr); ok {
			checkErr = fmt.Errorf("parameters are not allowed in views")
			return
		}
		if sub, ok := expr.(*sql.Subquery); ok {
			if err := e.checkViewSelect(viewName, sub.Select, isTemp); err != nil {
				checkErr = err
			}
		}
	})
	return checkErr
}

// viewChildSelects lists a SELECT's directly nested SELECT bodies in walk
// order: CTE bodies, the FROM subquery, join subqueries, and the UNION chain.
func viewChildSelects(sel *sql.SelectStmt) []*sql.SelectStmt {
	var children []*sql.SelectStmt
	for _, cte := range sel.CTEs {
		children = append(children, cte.Select)
	}
	if sel.From.Subquery != nil {
		children = append(children, sel.From.Subquery)
	}
	for i := range sel.Joins {
		if sel.Joins[i].Table.Subquery != nil {
			children = append(children, sel.Joins[i].Table.Subquery)
		}
	}
	if sel.Union != nil {
		children = append(children, sel.Union)
	}
	return children
}

// checkViewFromRefs validates one SELECT's FROM/join schema references against
// the view's attached-database rule.
func (e *DDLExecutor) checkViewFromRefs(viewName string, sel *sql.SelectStmt, isTemp bool) error {
	if sel.From.Name != "" {
		if err := e.checkViewSchemaRef(viewName, sel.From.Name, isTemp); err != nil {
			return err
		}
	}
	for i := range sel.Joins {
		if err := e.checkViewSchemaRef(viewName, sel.Joins[i].Table.Name, isTemp); err != nil {
			return err
		}
	}
	return nil
}

// walkExprsForView visits every expression in a SELECT's expression positions
// (columns, WHERE, GROUP BY, HAVING, ORDER BY) with WalkExprFull.
func walkExprsForView(sel *sql.SelectStmt, fn func(sql.Expr)) {
	for _, col := range sel.Columns {
		execquery.WalkExprFull(col.Expr, fn)
	}
	execquery.WalkExprFull(sel.Where, fn)
	for _, g := range sel.GroupBy {
		execquery.WalkExprFull(g, fn)
	}
	execquery.WalkExprFull(sel.Having, fn)
	for _, ob := range sel.OrderBy {
		execquery.WalkExprFull(ob.Expr, fn)
	}
}

// checkViewSchemaRef rejects a table reference to an attached database in a
// non-temp view body (SQLite's sqlite3FixSrcList: "view v1 cannot reference
// objects in database aux"). Temp views are exempt.
func (e *DDLExecutor) checkViewSchemaRef(viewName, table string, isTemp bool) error {
	if isTemp || table == "" {
		return nil
	}
	schemaName, _ := execdml.ParseSchemaName(table)
	if schemaName == "" {
		return nil
	}
	upper := strings.ToUpper(schemaName)
	if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
		return nil
	}
	if e.ctx.GetDB(schemaName) != nil {
		return fmt.Errorf("view %s cannot reference objects in database %s", viewName, schemaName)
	}
	return nil
}

// resolveViewContext determines the target database context and unqualified
// view name for CREATE VIEW (CREATE TEMP VIEW without a prefix goes to the
// temp schema, matching SQLite).
func resolveViewContext(e *DDLExecutor, s *sql.CreateViewStmt) (*DatabaseContext, string) {
	rawName := s.Name
	ctx := e.ctx.MainDB()
	viewName := rawName

	if s.Temporary {
		if tc := e.ctx.GetDB("temp"); tc != nil {
			ctx = tc
		}
	}

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		switch schemaUpper {
		case "MAIN":
			ctx = e.ctx.MainDB()
		case "TEMP", "TEMPORARY":
			if tc := e.ctx.GetDB("temp"); tc != nil {
				ctx = tc
			}
		default:
			if db := e.ctx.GetDB(prefix); db != nil {
				ctx = db
			}
			// For unknown schemas, try to create anyway (may fail if schema doesn't have Init)
		}
		viewName = rawName[dotIdx+1:]
	}
	return ctx, viewName
}

// buildViewSQL constructs the stored SQL text for a CREATE VIEW statement,
// preserving a verbatim RawSQL body when present (keeps CTEs) but stripping a
// main/temp schema prefix from the view name (SQLite stores "CREATE VIEW ttt
// ..." not "CREATE VIEW temp.ttt ...").
func buildViewSQL(s *sql.CreateViewStmt, viewName, colsClause string) string {
	if s.RawSQL != "" {
		sqlStr := s.RawSQL
		// SQLite stores every view's SQL in its own schema's sqlite_schema
		// with the UNQUALIFIED name ("CREATE VIEW v1 AS ..." in aux's
		// schema for "CREATE VIEW aux.v1" — attach3-6.x), so strip any
		// schema prefix, not just main/temp.
		if dotIdx := strings.Index(s.Name, "."); dotIdx >= 0 {
			sqlStr = stripViewSchemaPrefix(sqlStr, s.Name[:dotIdx])
		}
		return sqlStr
	}
	return fmt.Sprintf("CREATE VIEW %s%s AS %s", viewName, colsClause, selectStmtToString(s.Select))
}

// execCreateTableAsSelect implements CREATE TABLE ... AS SELECT after the
