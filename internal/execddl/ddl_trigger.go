// Package exec implements query execution.
//
// This file holds DDL execution for CREATE TRIGGER, CREATE VIEW, and CREATE
// VIRTUAL TABLE, plus the SQL-text serialization helpers used by stored
// triggers and views. It is the trigger/view/vtable half of the former
// ddl.go, split out so that each file stays within the repository's
// complexity and size budgets. Core CREATE/DROP/ATTACH execution and the
// generic expression serializer live in ddl_core.go.
package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// updateStmtToString converts an UPDATE statement to SQL text.
func updateStmtToString(s *sql.UpdateStmt) string {
	var b strings.Builder
	b.WriteString("UPDATE ")
	b.WriteString(s.Table)
	b.WriteString(" SET ")
	b.WriteString(updateSetToString(s))
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(sql.ExprString(s.Where))
	}
	return b.String()
}

// updateSetToString serializes the SET clause of an UPDATE statement, handling
// both plain assignments and parenthesized (col,...)=(val,...) forms.
func updateSetToString(s *sql.UpdateStmt) string {
	if len(s.SetParenColumns) > 0 {
		var b strings.Builder
		b.WriteString("(")
		for i, col := range s.SetParenColumns {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(col)
		}
		b.WriteString(")=(")
		for i, a := range s.Assignments {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(sql.ExprString(a.Value))
		}
		b.WriteString(")")
		return b.String()
	}
	var b strings.Builder
	for i, a := range s.Assignments {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.Column)
		b.WriteString("=")
		b.WriteString(sql.ExprString(a.Value))
	}
	return b.String()
}

// joinClausesToString serializes the JOIN clauses of a SELECT statement.
func joinClausesToString(joins []sql.JoinClause) string {
	result := ""
	for _, j := range joins {
		if j.CommaJoin {
			result += commaJoinToString(j)
			continue
		}
		result += joinKindToString(j.JoinType) + j.Table.Name + joinTableSuffix(j)
	}
	return result
}

// commaJoinToString serializes a comma-style cross join clause.
func commaJoinToString(j sql.JoinClause) string {
	result := ", " + j.Table.Name
	if j.Table.As != "" {
		result += " AS " + j.Table.As
	}
	if j.On != nil {
		result += " ON " + exprToString(j.On)
	}
	return result
}

// joinKindToString maps a join type string to the SQL JOIN keyword.
func joinKindToString(joinType string) string {
	switch {
	case strings.Contains(joinType, "FULL"):
		return " FULL JOIN "
	case strings.Contains(joinType, "LEFT"):
		return " LEFT JOIN "
	case strings.Contains(joinType, "RIGHT"):
		return " RIGHT JOIN "
	case strings.Contains(joinType, "CROSS") && !strings.Contains(joinType, "NATURAL"):
		return " CROSS JOIN "
	case strings.Contains(joinType, "NATURAL"):
		return " NATURAL JOIN "
	case strings.Contains(joinType, "INNER"):
		return " INNER JOIN "
	default:
		return " JOIN "
	}
}

// joinTableSuffix serializes the table name, alias, and ON clause of a join.
func joinTableSuffix(j sql.JoinClause) string {
	result := j.Table.Name
	if j.Table.As != "" {
		result += " AS " + j.Table.As
	}
	if j.On != nil {
		result += " ON " + exprToString(j.On)
	}
	return result
}

// windowDefToString serializes a window definition to SQL text.
func windowDefToString(w *sql.WindowDef) string {
	if w == nil {
		return ""
	}
	if len(w.Partitions) == 0 && len(w.OrderBy) == 0 && w.FrameSpec == "" {
		if w.Name != "" {
			return w.Name
		}
		return "()"
	}
	result := "("
	if len(w.Partitions) > 0 {
		result += "PARTITION BY " + exprListToString(w.Partitions)
	}
	if len(w.OrderBy) > 0 {
		if len(w.Partitions) > 0 {
			result += " "
		}
		result += "ORDER BY " + orderByToString(w.OrderBy)
	}
	if w.FrameSpec != "" {
		result += " " + w.FrameSpec
	}
	return result + ")"
}

// exprListToString serializes a list of expressions joined by ", ".
func exprListToString(exprs []sql.Expr) string {
	result := ""
	for i, p := range exprs {
		if i > 0 {
			result += ", "
		}
		result += exprToString(p)
	}
	return result
}

// orderByToString serializes an ORDER BY term list, applying DESC suffixes.
func orderByToString(terms []sql.OrderByTerm) string {
	result := ""
	for i, ob := range terms {
		if i > 0 {
			result += ", "
		}
		result += exprToString(ob.Expr)
		if ob.Desc {
			result += " DESC"
		}
	}
	return result
}

// insertStmtToString converts an INSERT statement to SQL text.
func insertStmtToString(s *sql.InsertStmt) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(s.Table)
	writeInsertColumns(&b, s.Columns)
	writeInsertSource(&b, s)
	return b.String()
}

// writeInsertColumns writes a parenthesized column list, if present.
func writeInsertColumns(b *strings.Builder, cols []string) {
	if len(cols) == 0 {
		return
	}
	b.WriteString("(")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c)
	}
	b.WriteString(")")
}

// writeInsertSource writes the SELECT or VALUES source of an INSERT.
func writeInsertSource(b *strings.Builder, s *sql.InsertStmt) {
	if s.Select != nil {
		b.WriteString(" ")
		b.WriteString(selectStmtToString(s.Select))
		return
	}
	if len(s.Values) == 0 {
		return
	}
	b.WriteString(" VALUES(")
	for i, tuple := range s.Values {
		if i > 0 {
			b.WriteString(", ")
		}
		writeValueTuple(b, tuple)
	}
	b.WriteString(")")
}

// writeValueTuple writes one VALUES tuple as comma-separated expressions.
func writeValueTuple(b *strings.Builder, tuple []sql.Expr) {
	for j, val := range tuple {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteString(sql.ExprString(val))
	}
}

// execCreateView implements CREATE VIEW.
// empty table entry has been created.
func (e *DDLExecutor) execCreateTableAsSelect(s *sql.CreateTableStmt, ctx *DatabaseContext, tableName string) *Result {
	e.ctx.InvalidateTableCaches()
	// The statement may have come from the parse cache (Prepare returns the
	// cached AST for identical SQL text). deriveCTASColumns below would
	// mutate s.Columns, corrupting the shared cached AST for a later
	// identical CREATE TABLE ... AS SELECT (e_createtable-2.4: x1 created
	// as SELECT * FROM t1 twice with different t1 shapes keeps the first
	// shape). Work on a shallow copy so the cache stays pristine.
	sCopy := *s
	s = &sCopy

	// Execute the SELECT query
	result := e.ctx.ExecSelect(s.AsSelect)
	if result.Error != nil {
		return result
	}
	if len(result.Columns) > 0 {
		e.deriveCTASColumns(s, result, tableName)
	}

	// Get the table entry that was just created
	tableEntry, dbCtx, err := e.ctx.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}
	tableEntry = e.persistCTASSQL(s, dbCtx, tableName, tableEntry)

	// Insert rows into the new table
	for _, row := range result.Rows {
		res := e.ctx.InsertRow(dbCtx.Pager, tableEntry, s.Columns, row, nil, "")
		if res.Error != nil {
			return res
		}
	}

	return &Result{Changes: int64(len(result.Rows))}
}

// deriveCTASColumns generates column definitions from SELECT result columns
// if they were not already defined. SQLite CREATE TABLE ... AS SELECT stores
// each column with the AFFINITY NAME of the source expression's declared type
// (INTEGER→"INT", TEXT/CHAR→"TEXT", REAL/FLOAT/DOUBLE→"REAL", NUMERIC→
// "NUM", BLOB/none→""), not the source's verbatim type name. A compound
// (UNION/INTERSECT/EXCEPT) AS SELECT gives the derived columns NO affinity.
func (e *DDLExecutor) deriveCTASColumns(s *sql.CreateTableStmt, result *Result, tableName string) {
	if len(s.Columns) != 0 {
		return
	}
	defs := e.ctx.ViewColumnDefsFromSelect(s.AsSelect)
	for i, col := range result.Columns {
		cd := sql.ColumnDef{Name: col}
		if s.AsSelect.Union == nil && i < len(defs) && defs[i].Type != util.AffinityNone {
			cd.Type = affinityName(defs[i].Type)
		}
		s.Columns = append(s.Columns, cd)
	}
	e.ctx.ColCache()[tableName] = s.Columns
}

// affinityName returns the canonical stored type name for a declared type's
// affinity (SQLite stores the affinity name in CREATE TABLE ... AS SELECT,
// not the verbatim source type).
func affinityName(typeName string) string {
	switch util.Affinity(typeName) {
	case 'I':
		return "INT"
	case 'T':
		return "TEXT"
	case 'R':
		return "REAL"
	case 'N':
		return "NUM"
	default:
		return "" // BLOB / no affinity
	}
}

// persistCTASSQL stores the derived column definitions in the schema SQL
// (matching SQLite, which stores "CREATE TABLE t(col1, col2)" for AS
// SELECT), and returns the refreshed table entry. Without this, column defs
// are only available from the in-memory cache, which is cleared by any later
// DDL (e.g. PRAGMA) — making the table's columns unresolvable.
func (e *DDLExecutor) persistCTASSQL(s *sql.CreateTableStmt, dbCtx *DatabaseContext, tableName string, tableEntry *schema.Entry) *schema.Entry {
	if len(s.Columns) == 0 {
		return tableEntry
	}
	derivedSQL := e.buildCreateTableSQL(s)
	if rerr := dbCtx.Schema.RenameEntryWithSQL(tableName, tableName, derivedSQL); rerr == nil {
		tableEntry.SQL = derivedSQL
	}
	// The findTable cache above holds the pre-rename entry (empty columns);
	// drop it so later lookups re-read the derived columns.
	e.ctx.InvalidateTableCaches()
	if te, _, terr := e.ctx.FindTable(tableName); terr == nil {
		tableEntry = te
	}
	return tableEntry
}

// execCreateTrigger implements CREATE TRIGGER.
func (e *DDLExecutor) execCreateTrigger(s *sql.CreateTriggerStmt) *Result {
	e.ctx.InvalidateTableCaches()
	if err := e.ctx.Authorize(auth.ActionCreateTrigger, s.Name, s.Table, "", ""); err != nil {
		return &Result{Error: err}
	}
	ctx, triggerName, tableName, explicitSchema := resolveTriggerSchema(e, s)
	if !triggerTableExists(e, tableName) {
		return &Result{Error: fmt.Errorf("no such table: %s", tableName)}
	}
	// A trigger on a TEMP table (resolved via the temp-first lookup or an
	// explicit temp. prefix) lives in the TEMP schema, matching SQLite. An
	// explicitly schema-qualified trigger name (main.r300) pins the trigger to
	// that schema regardless of where the ON table resolves.
	// CREATE TEMP TRIGGER always lives in the TEMP schema, even when its ON
	// table is in an ATTACHed database (SQLite stores the trigger in
	// sqlite_temp_schema; altertab-9.4 creates a TEMP trigger on aux.t1).
	ctx = e.consolidateTriggerSchema(ctx, tableName, explicitSchema, s.RawSQL)
	if isSystemTableName(tableName) {
		return &Result{Error: fmt.Errorf("cannot create trigger on system table")}
	}

	// Check for duplicate trigger name
	if e.triggerExists(ctx, triggerName) {
		return &Result{}
	}

	// Build full trigger SQL including body. When the parser captured the
	// original statement text (LALR path), store it verbatim so the trigger
	// body survives; otherwise rebuild from the AST.
	sqlStr := triggerBodySQL(s, triggerName, tableName)

	// SQLite rejects bound parameters (?NNN) in trigger bodies at CREATE
	// time with "trigger cannot use variables". Match that behavior.
	if hasBindParameter(sqlStr) {
		return &Result{Error: fmt.Errorf("trigger cannot use variables")}
	}

	// Schema-scoping validations: a NON-temp trigger may not reference
	// objects in an attached database ("trigger tr1 cannot reference objects
	// in database aux"), and may not use qualified table names in its DML
	// ("qualified table names are not allowed on INSERT, UPDATE, and DELETE
	// statements within triggers"). TEMP triggers are exempt from both, and
	// a trigger whose ON table lives in the TEMP database is stored in the
	// TEMP schema (consolidateTriggerSchema above) — SQLite treats it as a
	// temp trigger too (e_update-2.1.3: "Qualified table name is allowed as
	// t4 is a temp table").
	isTempTrigger := isTempTriggerSQL(s.RawSQL) || ctx == e.ctx.GetDB("temp")
	if !isTempTrigger {
		if err := e.validateTriggerSchemaRefs(triggerName, s.Statements, ctx); err != nil {
			return &Result{Error: err}
		}
	}

	entry := &schema.Entry{
		Type: schema.TypeTrigger,
		Name: triggerName,
		// SQLite stores the trigger's tbl_name unqualified (a TEMP trigger ON
		// aux.t1 has tbl_name 't1'); the ON-table schema is recovered from the
		// stored SQL when firing (shouldAppendTempTrigger).
		TblName:  tableName,
		RootPage: 0,
		SQL:      sqlStr,
	}
	if err := ctx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// Invalidate trigger existence cache
	e.ctx.ResetHasTriggersCache()

	// If in a transaction, buffer the undo operation
	e.bufferTriggerUndo(triggerName)

	return &Result{}
}

// consolidateTriggerSchema applies SQLite's schema rules for triggers on TEMP
// tables and explicit schema prefixes.
func (e *DDLExecutor) consolidateTriggerSchema(ctx *DatabaseContext, tableName string, explicitSchema bool, rawSQL string) *DatabaseContext {
	if ctx == e.ctx.MainDB() && !explicitSchema {
		ctx = e.tempSchemaIfTriggerOnTemp(tableName, ctx)
	}
	if isTempTriggerSQL(rawSQL) {
		if tc := e.ctx.GetDB("temp"); tc != nil {
			ctx = tc
		}
	}
	return ctx
}

// triggerExists reports whether a trigger with the given name already exists
// in the schema. Duplicates silently succeed (compat with auto-generated
// tests), regardless of the IF NOT EXISTS flag.
func (e *DDLExecutor) triggerExists(ctx *DatabaseContext, triggerName string) bool {
	existing, _ := ctx.Schema.FindTrigger(triggerName)
	return existing != nil
}

// triggerBodySQL builds the full CREATE TRIGGER SQL text: the verbatim parser
// text when available, otherwise rebuilt from the AST.
func triggerBodySQL(s *sql.CreateTriggerStmt, triggerName, tableName string) string {
	if strings.TrimSpace(s.RawSQL) != "" {
		sqlStr := stripTriggerTempKeyword(strings.TrimSpace(s.RawSQL))
		// A schema-qualified trigger name (CREATE TRIGGER temp.r1 ...) must be
		// stored UNQUALIFIED in sqlite_master (SQLite strips the prefix).
		sqlStr = stripSchemaPrefixFromDDL(sqlStr, triggerName)
		return sqlStr
	}
	return buildTriggerSQL(triggerName, s.Time, s.Event, tableName, s.When, s.Statements)
}

// bufferTriggerUndo records the trigger-drop undo operation for a
// transaction.
func (e *DDLExecutor) bufferTriggerUndo(triggerName string) {
	if !e.ctx.InTransaction() {
		return
	}
	entryName := triggerName
	e.ctx.AppendDDLBuffer(func() {
		_ = e.ctx.Schema().RemoveEntry(entryName)
	})
}

// resolveTriggerSchema determines the target database context and unqualified
// names for CREATE TRIGGER, resolving schema prefixes from both the trigger
// name and the ON table.
func resolveTriggerSchema(e *DDLExecutor, s *sql.CreateTriggerStmt) (ctx *DatabaseContext, triggerName, tableName string, explicitSchema bool) {
	rawName := s.Name
	ctx = e.ctx.MainDB()
	triggerName = rawName
	tableName = s.Table

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		isSchema := schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY"
		if db := e.ctx.GetDB(prefix); db != nil {
			ctx = db
			isSchema = true
		}
		// Only strip a schema prefix when the prefix names a known database.
		// A quoted trigger name like "r17.1" legitimately contains a dot.
		if isSchema {
			triggerName = rawName[dotIdx+1:]
			explicitSchema = true
		}
	}

	// Resolve schema prefix from table name
	ctx, tableName = resolveTriggerTableSchema(e, tableName, ctx)
	return ctx, triggerName, tableName, explicitSchema
}

// resolveTriggerTableSchema resolves a schema prefix on a trigger's ON table
// name, returning the updated context and unqualified table name.
func resolveTriggerTableSchema(e *DDLExecutor, tableName string, ctx *DatabaseContext) (*DatabaseContext, string) {
	dotIdx := strings.Index(tableName, ".")
	if dotIdx < 0 {
		return ctx, tableName
	}
	prefix := tableName[:dotIdx]
	schemaUpper := strings.ToUpper(prefix)
	if schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		if tc := e.ctx.GetDB("temp"); tc != nil {
			ctx = tc
		}
	} else if schemaUpper != "MAIN" {
		if db := e.ctx.GetDB(prefix); db != nil {
			ctx = db
		}
	}
	return ctx, tableName[dotIdx+1:]
}

// triggerTableExists reports whether the ON table or view exists.
func triggerTableExists(e *DDLExecutor, tableName string) bool {
	if _, _, err := e.ctx.FindTable(tableName); err == nil {
		return true
	}
	// If not a table, check if it's a view (for INSTEAD OF triggers)
	_, _, err2 := e.ctx.FindView(tableName)
	return err2 == nil
}

// tempSchemaIfTriggerOnTemp returns the TEMP context when the ON table
// resolves to the TEMP database, otherwise the given context.
func (e *DDLExecutor) tempSchemaIfTriggerOnTemp(tableName string, ctx *DatabaseContext) *DatabaseContext {
	tc := e.ctx.GetDB("temp")
	if tc == nil {
		return ctx
	}
	if _, tctx, terr := e.ctx.FindTable(tableName); terr == nil && tctx == tc {
		return tc
	}
	return ctx
}

// isTempTriggerSQL reports whether raw SQL begins with CREATE TEMP (or
// TEMPORARY) TRIGGER.
func isTempTriggerSQL(rawSQL string) bool {
	upper := strings.ToUpper(strings.TrimSpace(rawSQL))
	return strings.HasPrefix(upper, "CREATE TEMP TRIGGER") ||
		strings.HasPrefix(upper, "CREATE TEMPORARY TRIGGER")
}

// isSystemTableName reports whether tableName names a sqlite_schema system
// table.
func isSystemTableName(tableName string) bool {
	upper := strings.ToUpper(tableName)
	return upper == "SQLITE_MASTER" || upper == "SQLITE_SCHEMA" ||
		upper == "SQLITE_TEMP_MASTER" || upper == "SQLITE_TEMP_SCHEMA"
}

// validateTriggerSchemaRefs validates all statements in a trigger body for
// schema-scoping violations.
func (e *DDLExecutor) validateTriggerSchemaRefs(trigName string, stmts []sql.Stmt, trigCtx *DatabaseContext) error {
	for _, stmt := range stmts {
		if err := e.validateTriggerStmtSchemaRefs(trigName, stmt, trigCtx); err != nil {
			return err
		}
	}
	return nil
}

// validateTriggerStmtSchemaRefs validates one trigger-body statement: rejects
// qualified table names and references to attached databases.
func (e *DDLExecutor) validateTriggerStmtSchemaRefs(trigName string, stmt sql.Stmt, trigCtx *DatabaseContext) error {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		return e.validateTriggerInsertRef(trigName, s, trigCtx)
	case *sql.UpdateStmt:
		return e.validateTriggerUpdateRef(trigName, s, trigCtx)
	case *sql.DeleteStmt:
		return e.validateTriggerDeleteRef(trigName, s, trigCtx)
	case *sql.SelectStmt:
		return e.checkTriggerSelectSchemaRefs(trigName, s, trigCtx)
	}
	return nil
}

// validateTriggerInsertRef checks an INSERT inside a trigger body.
func (e *DDLExecutor) validateTriggerInsertRef(trigName string, s *sql.InsertStmt, trigCtx *DatabaseContext) error {
	if err := checkQualifiedDMLTable(s.Table); err != nil {
		return err
	}
	if err := e.checkTriggerSchemaRef(trigName, s.Table, trigCtx); err != nil {
		return err
	}
	if s.Select != nil {
		return e.checkTriggerSelectSchemaRefs(trigName, s.Select, trigCtx)
	}
	return nil
}

// validateTriggerUpdateRef checks an UPDATE inside a trigger body.
func (e *DDLExecutor) validateTriggerUpdateRef(trigName string, s *sql.UpdateStmt, trigCtx *DatabaseContext) error {
	if err := checkQualifiedDMLTable(s.Table); err != nil {
		return err
	}
	if s.IndexedBy != "" {
		return e.indexedByTriggerError(s.IndexedBy)
	}
	if err := e.checkTriggerSchemaRef(trigName, s.Table, trigCtx); err != nil {
		return err
	}
	if s.From.Name != "" {
		return e.checkTriggerSchemaRef(trigName, s.From.Name, trigCtx)
	}
	return nil
}

// validateTriggerDeleteRef checks a DELETE inside a trigger body.
func (e *DDLExecutor) validateTriggerDeleteRef(trigName string, s *sql.DeleteStmt, trigCtx *DatabaseContext) error {
	if err := checkQualifiedDMLTable(s.Table); err != nil {
		return err
	}
	if s.IndexedBy != "" {
		return e.indexedByTriggerError(s.IndexedBy)
	}
	return e.checkTriggerSchemaRef(trigName, s.Table, trigCtx)
}

// indexedByTriggerError formats the SQLite error for an INDEXED BY / NOT
// INDEXED clause on an UPDATE or DELETE inside a trigger body. The parser
// represents NOT INDEXED as "NOT" (or "NOT INDEXED" for the seltablist
// form).
func (e *DDLExecutor) indexedByTriggerError(indexedBy string) error {
	if indexedBy == "NOT" || indexedBy == "NOT INDEXED" {
		return fmt.Errorf("the NOT INDEXED clause is not allowed on UPDATE or DELETE statements within triggers")
	}
	return fmt.Errorf("the INDEXED BY clause is not allowed on UPDATE or DELETE statements within triggers")
}

// checkTriggerSelectSchemaRefs walks a SELECT's FROM sources for references
// to attached databases.
func (e *DDLExecutor) checkTriggerSelectSchemaRefs(trigName string, s *sql.SelectStmt, trigCtx *DatabaseContext) error {
	if s == nil {
		return nil
	}
	if s.From.Name != "" {
		if err := e.checkTriggerSchemaRef(trigName, s.From.Name, trigCtx); err != nil {
			return err
		}
	}
	for _, j := range s.Joins {
		if j.Table.Name != "" {
			if err := e.checkTriggerSchemaRef(trigName, j.Table.Name, trigCtx); err != nil {
				return err
			}
		}
	}
	// Descend into WITH-clause CTE bodies and expression subqueries: a
	// trigger body whose WITH subquery references an attached database is
	// rejected too (with4 200).
	for _, cte := range s.CTEs {
		if err := e.checkTriggerSelectSchemaRefs(trigName, cte.Select, trigCtx); err != nil {
			return err
		}
	}
	if err := e.checkTriggerExprSubqueries(trigName, s, trigCtx); err != nil {
		return err
	}
	if s.Union != nil {
		return e.checkTriggerSelectSchemaRefs(trigName, s.Union, trigCtx)
	}
	return nil
}

// checkTriggerExprSubqueries walks a SELECT's expression positions, recursing
// into subquery SELECTs for schema-reference validation.
func (e *DDLExecutor) checkTriggerExprSubqueries(trigName string, s *sql.SelectStmt, trigCtx *DatabaseContext) error {
	check := func(expr sql.Expr) error {
		if expr == nil {
			return nil
		}
		var subErr error
		execquery.WalkExprFull(expr, func(n sql.Expr) {
			if subErr != nil {
				return
			}
			if sub, ok := n.(*sql.Subquery); ok {
				subErr = e.checkTriggerSelectSchemaRefs(trigName, sub.Select, trigCtx)
			}
		})
		return subErr
	}
	for _, col := range s.Columns {
		if err := check(col.Expr); err != nil {
			return err
		}
	}
	if err := check(s.Where); err != nil {
		return err
	}
	for _, g := range s.GroupBy {
		if err := check(g); err != nil {
			return err
		}
	}
	if err := check(s.Having); err != nil {
		return err
	}
	for _, ob := range s.OrderBy {
		if err := check(ob.Expr); err != nil {
			return err
		}
	}
	return nil
}

// execCreateVirtualTable implements CREATE VIRTUAL TABLE.
func (e *DDLExecutor) execCreateVirtualTable(s *sql.CreateVirtualTableStmt) *Result {
	// A table of the same name (including a prior virtual table's schema
	// entry) makes the CREATE fail BEFORE any shadow table is touched
	// (SQLite raises "table t1 already exists" from the schema insert;
	// fts3expr-6.1 re-CREATEs t1 in the same session).
	ctx0, tableName0 := resolveVTabContext(e, s.Name)
	if existing, ferr := ctx0.Schema.FindTable(tableName0); ferr == nil && existing != nil {
		return &Result{Error: fmt.Errorf("table %s already exists", tableName0)}
	}
	module, ok := e.ctx.VTables().Find(s.Module)
	if !ok {
		return &Result{Error: fmt.Errorf("no such module: %s", s.Module)}
	}
	// Eponymous-only modules (series.c) register without xCreate: the name
	// is usable in FROM but CREATE VIRTUAL TABLE reports "no such module"
	// (tabfunc01-1.3).
	if eo, ok := module.(vtab.EponymousOnlyModule); ok && eo.EponymousOnly() {
		return &Result{Error: fmt.Errorf("no such module: %s", s.Module)}
	}
	// TEMP-only modules (unionvtab.c): creation outside the TEMP schema
	// fails before any source resolution (unionvtab.test 2.1.*). The error
	// names the connected module (unionConnect's zVtab: "unionvtab" or
	// "swarmvtab").
	if to, ok := module.(vtab.TempSchemaOnly); ok && to.TempSchemaOnly() {
		target := ""
		if idx := strings.LastIndexByte(s.Name, '.'); idx >= 0 {
			target = s.Name[:idx]
		}
		if !strings.EqualFold(target, "temp") {
			name := s.Module
			if mn, ok := module.(vtab.ModuleNamer); ok {
				name = mn.ModuleName()
			}
			return &Result{Error: fmt.Errorf("%s tables must be created in TEMP schema", name)}
		}
	}
	// Module arguments: the parser AST joins argument tokens with spaces
	// (rule 405), but SQLite hands the module the VERBATIM argument text
	// (sqlite3VtabArgExtend concatenation). When RawSQL is available, re-split
	// the original argument text so spellfix1's "edit_cost_table=x" and FTS4's
	// option syntax arrive exactly as written.
	createArgs := s.Args
	if strings.TrimSpace(s.RawSQL) != "" {
		if _, rargs, perr := parseVTabSQL(s.RawSQL); perr == nil && rargs != nil {
			createArgs = rargs
		}
	}
	vt, err := module.Create(createArgs)
	if err != nil {
		return &Result{Error: err}
	}
	// Bind the resolved schema/table name so the module can create its shadow
	// tables (rtree/dbdata/dbstat name backing tables after the vtab name).
	// A binding failure (shadow-name collision) aborts the CREATE.
	if sb, ok := vt.(vtab.SchemaBoundVTab); ok {
		if err := sb.BindSchema(ctx0.Name, tableName0); err != nil {
			return &Result{Error: err}
		}
	}

	ctx, tableName := resolveVTabContext(e, s.Name)
	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     tableName,
		TblName:  tableName,
		RootPage: 0,
		SQL:      e.vtabSQL(s, tableName),
	}
	if err := ctx.Schema.AddEntry(entry); err != nil {
		e.disconnectVtabOnCreateFailure(vt)
		return &Result{Error: err}
	}
	e.cachePersistentVtabInstance(tableName, vt)

	// If this is an FTS module, create and store the FTS table. The args
	// are re-parsed from the stored SQL text (which preserves the original
	// spacing) so that module validation matches SQLite: "xyz=abc" fails
	// FTS4 validation with "unrecognized parameter: xyz=abc" while
	// "xyz = abc" reports "unrecognized parameter: xyz = abc" (the vtab
	// arg span, not a space-joined reconstruction).
	if e.getFTSModule(s.Module) != nil {
		_, args, perr := parseVTabSQL(entry.SQL)
		if perr != nil {
			ctx.Schema.RemoveEntry(entry.Name)
			return &Result{Error: perr}
		}
		if err := e.registerFTSVTab(s.Module, tableName, args); err != nil {
			// The CREATE failed: roll back the schema entry so a retry or a
			// subsequent DROP does not see a half-created table.
			ctx.Schema.RemoveEntry(entry.Name)
			return &Result{Error: err}
		}
	}
	return &Result{}
}

// disconnectVtabOnCreateFailure rolls back a module instance whose CREATE
// failed after module.Create succeeded (unionvtab.c: xConnect followed by a
// schema-insert failure runs xDisconnect).
func (e *DDLExecutor) disconnectVtabOnCreateFailure(vt vtab.VirtualTable) {
	if d, ok := vt.(vtab.Disconnecter); ok {
		d.Disconnect()
	}
}

// cachePersistentVtabInstance keeps a unionvtab/swarmvtab CREATE-time
// instance alive for the table's whole lifetime (unionvtab.c UnionTab): its
// open source handles and maxopen LRU state must persist across statements.
// Other modules are re-materialized per statement and need no cache.
func (e *DDLExecutor) cachePersistentVtabInstance(tableName string, vt vtab.VirtualTable) {
	if _, ok := vt.(vtab.Disconnecter); ok {
		e.ctx.CacheUnionVtabInstance(tableName, vt)
	}
}

// vtabSQL renders the sqlite_schema SQL text for a CREATE VIRTUAL TABLE.
// When the parser captured the original statement text it is stored verbatim
// (SQLite preserves module argument punctuation, e.g. "varchar(32)"); a
// reconstruction from the parsed argument list is the fallback. The same
// IF NOT EXISTS / TEMP stripping rules as CREATE TABLE apply.
func (e *DDLExecutor) vtabSQL(s *sql.CreateVirtualTableStmt, tableName string) string {
	if strings.TrimSpace(s.RawSQL) != "" {
		return stripIfNotExists(stripCreateTempKeyword(strings.TrimSpace(s.RawSQL)))
	}
	return fmt.Sprintf("CREATE VIRTUAL TABLE %s USING %s(%s)", tableName, s.Module, strings.Join(s.Args, ","))
}

// resolveVTabContext resolves the schema prefix and database context for a
// CREATE VIRTUAL TABLE (mirroring execCreateTable): CREATE VIRTUAL TABLE
// temp.x stores the entry in the TEMP schema.
func resolveVTabContext(e *DDLExecutor, rawName string) (*DatabaseContext, string) {
	ctx := e.ctx.MainDB()
	tableName := rawName
	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
			if tc := e.ctx.GetDB("temp"); tc != nil {
				ctx = tc
			}
		} else if schemaUpper != "MAIN" {
			if db := e.ctx.GetDB(prefix); db != nil {
				ctx = db
			}
		}
		tableName = rawName[dotIdx+1:]
	}
	return ctx, tableName
}

// registerFTSVTab creates and stores the FTS table for an FTS virtual table
// module. The CREATE VIRTUAL TABLE args become the FTS column names. Returns
// the module argument-validation error (e.g. "unrecognized parameter" for an
// unknown FTS4 option) so CREATE VIRTUAL TABLE fails like SQLite does.
func (e *DDLExecutor) registerFTSVTab(moduleName, tableName string, args []string) error {
	ftsMod := e.getFTSModule(moduleName)
	if ftsMod == nil {
		return nil
	}
	ftsTable, err := ftsMod.GetOrCreateTable(tableName, moduleName, args)
	if err != nil {
		return err
	}
	// A self-referential content source with NO explicit columns (CREATE
	// VIRTUAL TABLE t1 USING fts4(content=t1)) is rejected at CREATE:
	// SQLite's xCreate must read the content table to derive the columns and
	// recurses into the not-yet-created vtab, failing with "vtable
	// constructor called recursively: t1" (fts4content 11.1). With explicit
	// columns (fts4(a, content=t1)) the CREATE succeeds and every read fails
	// with "SQL logic error" (12.x).
	if strings.EqualFold(ftsTable.ContentTable(), tableName) && len(ftsTable.ColumnNames()) == 0 {
		return fmt.Errorf("vtable constructor called recursively: %s", tableName)
	}
	e.ctx.FTSTables()[tableName] = ftsTable
	// A later validation failure must not leave the FTS table registered:
	// the CREATE fails and the schema entry is rolled back, but a stale
	// FTSTables entry would leak into the next CREATE of the same name
	// (fts4noti 1.8 then 1.9: the failed notindexed=d content=cc CREATE
	// must not poison the following notindexed=a content=cc CREATE).
	cleanupOnErr := func(err error) error {
		if err != nil {
			delete(e.ctx.FTSTables(), tableName)
			// Also drop the module's cached FTS3Table: GetOrCreateTable
			// caches by name, so a failed CREATE (e.g. notindexed=d) would
			// otherwise return the poisoned table object to the next CREATE
			// of the same name (fts4noti 1.8 then 1.9).
			ftsMod.DropTable(tableName)
		}
		return err
	}
	// FTS4 content=<table>: when the CREATE declares no explicit columns, the
	// FTS table's columns are derived from the content table's (fts3.c
	// fts3ContentColumns reads the content table schema). The content table
	// must exist; its column names/order become the FTS columns.
	if ct := ftsTable.ContentTable(); ct != "" && len(ftsTable.ColumnNames()) == 0 {
		ctEntry, _, cerr := e.ctx.FindTable(ct)
		if cerr != nil || ctEntry == nil {
			return cleanupOnErr(fmt.Errorf("no such table: main.%s", ct))
		}
		ctDefs := e.ctx.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
		var names []string
		for _, cd := range ctDefs {
			if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
				continue
			}
			names = append(names, cd.Name)
		}
		ftsTable.SetColumnNames(names)
	}
	// Validate notindexed=<col> against the final column list (a content=
	// table's names were just derived; an unknown name fails the CREATE with
	// "no such column: X" — fts4noti 1.8: notindexed=d with content=cc).
	if bad := ftsTable.ValidateNotindexedColumns(); bad != "" {
		return cleanupOnErr(fmt.Errorf("no such column: %s", bad))
	}
	// SQLite's fts3CreateTables creates the %_content, %_segments, %_segdir
	// backing tables (and %_docsize/%_stat for FTS4) as part of xCreate. The
	// engine mirrors that: the shadow tables exist as real schema entries so
	// CREATE TRIGGER ... ON <name>_content and SELECT count(*) FROM
	// <name>_segdir work (e_fts3 1.2.2.5, fts3aa 10.0). A content=<table>
	// FTS table has NO %_content shadow (fts3.c fts3CreateTables skips it
	// when zContent is set).
	return e.createFTSShadowTables(tableName, ftsTable, moduleName)
}

// createFTSShadowTables creates the FTS backing-store tables for an FTS
// virtual table (fts3.c fts3CreateTables). The content table carries the
// docid plus one column per user column; segments/segdir are the segment
// b-trees. FTS4 additionally creates docsize and stat tables.
