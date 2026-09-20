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
// empty table entry has been created. ctasResult carries the pre-executed
// SELECT result from the CREATE TABLE flow (already run before the table was
// created, mirroring SQLite's prepare-time select compilation); nil runs it.
func (e *DDLExecutor) execCreateTableAsSelect(s *sql.CreateTableStmt, ctx *DatabaseContext, tableName string, ctasResult *Result) *Result {
	e.ctx.InvalidateTableCaches()
	// The statement may have come from the parse cache (Prepare returns the
	// cached AST for identical SQL text). deriveCTASColumns below would
	// mutate s.Columns, corrupting the shared cached AST for a later
	// identical CREATE TABLE ... AS SELECT (e_createtable-2.4: x1 created
	// as SELECT * FROM t1 twice with different t1 shapes keeps the first
	// shape). Work on a shallow copy so the cache stays pristine.
	sCopy := *s
	s = &sCopy

	// Execute the SELECT query. The select has already been executed by the
	// CREATE TABLE flow BEFORE the table was created (prepate-time ordering:
	// a select failure leaves no table behind, misc1-15.1.x); its result is
	// carried in ctasResult. A nil ctasResult (direct entry) runs it here.
	result := ctasResult
	if result == nil {
		result = e.ctx.ExecSelect(s.AsSelect)
		if result.Error != nil {
			return result
		}
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
	// build.c sqlite3CheckObjectName: the "sqlite_" prefix is reserved in
	// every namespace, including triggers ("object name reserved for
	// internal use: sqlite_tr1", index.test 7.x).
	if res := e.validateReservedName(strings.TrimSpace(s.Name)); res != nil {
		return res
	}
	ctx, triggerName, tableName, explicitSchema, rerr := resolveTriggerSchema(e, s)
	if rerr != nil {
		return &Result{Error: rerr}
	}
	// A trigger on a TEMP table (resolved via the temp-first lookup or an
	// explicit temp. prefix) lives in the TEMP schema, matching SQLite. An
	// explicitly schema-qualified trigger name (main.r300) pins the trigger to
	// that schema regardless of where the ON table resolves.
	// CREATE TEMP TRIGGER always lives in the TEMP schema, even when its ON
	// table is in an ATTACHed database (SQLite stores the trigger in
	// sqlite_temp_schema; altertab-9.4 creates a TEMP trigger on aux.t1).
	ctx = e.consolidateTriggerSchema(ctx, tableName, explicitSchema, s.RawSQL)
	isTempTrigger := isTempTriggerSQL(s.RawSQL) || ctx == e.ctx.GetDB("temp")
	if res := e.validateTriggerTarget(s, ctx, triggerName, tableName, isTempTrigger); res != nil {
		return res
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
// in the schema (the duplicate CREATE outcome is decided by the caller: an
// error unless IF NOT EXISTS was given).
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
func resolveTriggerSchema(e *DDLExecutor, s *sql.CreateTriggerStmt) (ctx *DatabaseContext, triggerName, tableName string, explicitSchema bool, err error) {
	rawName := s.Name
	ctx = e.ctx.MainDB()
	triggerName = rawName
	tableName = s.Table

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		// A name token that was QUOTED keeps any dot as part of the name
		// ("r17.1" is a legal single-token name); only a bare token.a.b form
		// names a schema (sqlite3TwoPartName).
		if _, quoted, tokOK := triggerNameToken(s.RawSQL); !tokOK || !quoted {
			prefix := rawName[:dotIdx]
			schemaUpper := strings.ToUpper(prefix)
			isSchema := schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY"
			if db := e.ctx.GetDB(prefix); db != nil {
				ctx = db
				isSchema = true
			}
			if !isSchema {
				// trigger.c sqlite3BeginTrigger → sqlite3TwoPartName: a
				// schema prefix naming no attached database fails the
				// CREATE with "unknown database X" (trigger7-1.1).
				return nil, "", "", false, fmt.Errorf("unknown database %s", prefix)
			}
			// Only strip a schema prefix that names a known database.
			triggerName = rawName[dotIdx+1:]
			explicitSchema = true
		}
	}

	// Resolve schema prefix from table name
	ctx, tableName = resolveTriggerTableSchema(e, tableName, ctx)
	return ctx, triggerName, tableName, explicitSchema, nil
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
	// VALUES tuples may contain subqueries referencing other databases
	// (attach-5.8: INSERT INTO t1 VALUES((SELECT min(x) FROM temp.t6),5)).
	for _, tuple := range s.Values {
		for _, expr := range tuple {
			if err := e.checkTriggerExprSchemaRefs(trigName, expr, trigCtx); err != nil {
				return err
			}
		}
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
	if err := e.checkTriggerSchemaRef(trigName, s.Table, trigCtx); err != nil {
		return err
	}
	// The WHERE clause may hide subqueries referencing other databases
	// (attach-5.9: DELETE FROM t1 WHERE x<(SELECT min(x) FROM temp.t6)).
	return e.checkTriggerExprSchemaRefs(trigName, s.Where, trigCtx)
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
// into subquery SELECTs for schema-reference validation (via
// checkTriggerExprSchemaRefs, the per-expression walker).
func (e *DDLExecutor) checkTriggerExprSubqueries(trigName string, s *sql.SelectStmt, trigCtx *DatabaseContext) error {
	for _, col := range s.Columns {
		if err := e.checkTriggerExprSchemaRefs(trigName, col.Expr, trigCtx); err != nil {
			return err
		}
	}
	if err := e.checkTriggerExprSchemaRefs(trigName, s.Where, trigCtx); err != nil {
		return err
	}
	for _, g := range s.GroupBy {
		if err := e.checkTriggerExprSchemaRefs(trigName, g, trigCtx); err != nil {
			return err
		}
	}
	if err := e.checkTriggerExprSchemaRefs(trigName, s.Having, trigCtx); err != nil {
		return err
	}
	for _, ob := range s.OrderBy {
		if err := e.checkTriggerExprSchemaRefs(trigName, ob.Expr, trigCtx); err != nil {
			return err
		}
	}
	return nil
}

// checkTriggerExprSchemaRefs walks one expression for subqueries referencing
// other databases (the VALUES-tuple / WHERE-clause counterparts of
// checkTriggerExprSubqueries).
func (e *DDLExecutor) checkTriggerExprSchemaRefs(trigName string, expr sql.Expr, trigCtx *DatabaseContext) error {
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
