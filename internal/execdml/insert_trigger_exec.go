package execdml

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"strings"
)

// triggerTableContext resolves the database context for a table's triggers:
// the current DML context when set, otherwise the table's owning context.
// triggerTableContext resolves the database context for a table's triggers:
// the current DML context when set, otherwise the table's owning context.
func (e *DMLExecutor) triggerTableContext(tableName string) *DatabaseContext {
	if e.currentDMLCtx != nil {
		return e.currentDMLCtx
	}
	if entry, ctx, err := e.ctx.FindTable(tableName); err == nil && ctx != nil && entry != nil {
		return ctx
	}
	return e.ctx.MainDB()
}

// triggerInChain reports whether the specific trigger (schema.name) is already
// in the current trigger invocation chain. With recursive_triggers OFF, a
// trigger does not re-fire itself for a nested statement on the same table,
// but OTHER triggers on that table fire normally (SQLite excludes only the
// currently-executing trigger program).
func (e *DMLExecutor) triggerInChain(schemaName, triggerName string) bool {
	if e.ctx.TriggerDepth() == 0 || e.ctx.RecursiveTriggers() {
		return false
	}
	qualName := schemaName + "." + triggerName
	for _, t := range e.ctx.TriggerTables() {
		if t == qualName {
			return true
		}
	}
	return false
}

// TriggerOnTableSchema extracts the schema prefix of a trigger's ON table
// from its stored CREATE TRIGGER SQL ("CREATE TRIGGER ... ON aux.t1 ..." →
// "aux"). Returns "" for an unqualified ON table. Exported for the DDL
// layer's DROP TABLE trigger cascade, which resolves the same binding.
func TriggerOnTableSchema(triggerSQL string) string {
	upper := strings.ToUpper(triggerSQL)
	onIdx := strings.Index(upper, " ON ")
	if onIdx < 0 {
		return ""
	}
	table := strings.TrimSpace(triggerSQL[onIdx+4:])
	// Stop at the first whitespace or '(' (WHEN/INSTEAD/BEGIN clauses follow).
	end := len(table)
	for i := 0; i < len(table); i++ {
		if table[i] == ' ' || table[i] == '\t' || table[i] == '\n' || table[i] == '\r' || table[i] == '(' {
			end = i
			break
		}
	}
	table = strings.TrimSpace(table[:end])
	schema, _ := parseSchemaName(table)
	return schema
}

// TriggerTargetsSchema reports whether a trigger entry fires for an event on
// a table in the named schema: trigger.c binds every trigger to the specific
// table (and schema) it names in its ON clause (Trigger.pTabSchema, fixed at
// CREATE time), so a schema-qualified ON table ("ON main.t4" vs "ON temp.t4")
// fires only when the event table lives in that schema — even though the
// TEMP schema stores all of them side by side under the same TblName. An
// unqualified ON table passed the name match, so it stays. Exported for the
// DDL layer's DROP TABLE trigger cascade (build.c sqlite3CodeDropTable drops
// sqlite3TriggerList, the triggers bound to the dropped table).
func TriggerTargetsSchema(t *schema.Entry, tableSchemaName string) bool {
	if onSchema := TriggerOnTableSchema(t.SQL); onSchema != "" {
		return strings.EqualFold(onSchema, tableSchemaName)
	}
	return true
}

// triggerTargetsCtx reports whether a trigger stored in tableCtx's own schema
// fires for an event on tableCtx's table.
func (e *DMLExecutor) triggerTargetsCtx(t *schema.Entry, tableCtx *DatabaseContext) bool {
	return TriggerTargetsSchema(t, tableCtx.Name)
}

// appendTempTriggers adds the TEMP triggers that fire for a table event.
// TEMP triggers fire on the table they were created ON. The stored
// TblName carries the ON-table resolution: a schema-qualified ON table
// (aux.t1) fires only for that schema's events; an unqualified ON table
// fires for the table it resolved to at CREATE time (temp shadows main:
// if a temp table of that name exists, the ON table is the temp one).
func (e *DMLExecutor) appendTempTriggers(tableCtx *DatabaseContext, tableName string, triggers []*schema.Entry) []*schema.Entry {
	tc := e.ctx.GetDB("temp")
	if tc == nil || tc == tableCtx || tableCtx == nil {
		return triggers
	}
	tempTriggers, _ := tc.Schema.FindTriggersForTable(tableName)
	for _, tt := range tempTriggers {
		if tt == nil {
			continue
		}
		if e.shouldAppendTempTrigger(tt, tableCtx, tc, tableName) {
			triggers = append(triggers, tt)
		}
	}
	return triggers
}

// shouldAppendTempTrigger decides whether one TEMP trigger fires for an event
// on the given table context.

// shouldAppendTempTrigger decides whether one TEMP trigger fires for an event
// on the given table context.

// shouldAppendTempTrigger decides whether one TEMP trigger fires for an event
// on the given table context.
// shouldAppendTempTrigger decides whether one TEMP trigger fires for an event
// on the given table context.
func (e *DMLExecutor) shouldAppendTempTrigger(tt *schema.Entry, tableCtx, tc *DatabaseContext, tableName string) bool {
	onSchema := TriggerOnTableSchema(tt.SQL)
	if onSchema != "" {
		// Schema-qualified ON table: fire only when the event table is
		// in that schema.
		return strings.EqualFold(onSchema, tableCtx.Name)
	}
	// Unqualified ON table. If a temp table of this name shadows
	// main, the trigger is on the temp table (fires only for temp
	// events); otherwise it is on the main table and fires for main
	// events (tableCtx == main).
	shadowed := false
	if _, err := tc.Schema.FindTable(tableName); err == nil {
		shadowed = true
	}
	if shadowed {
		return tableCtx == tc
	}
	return tableCtx == e.ctx.MainDB()
}

// triggerOnTableSchema extracts the schema prefix of a trigger's ON table from
// its stored CREATE TRIGGER SQL ("CREATE TRIGGER ... ON aux.t1 ..." → "aux").
// Returns "" for an unqualified ON table.
// maxTriggerDepth is SQLite's SQLITE_MAX_TRIGGER_DEPTH default: recursive
// trigger programs abort with "too many levels of trigger recursion" once
// the nesting exceeds this limit.

// validateLoadedTriggerSchemaCtx parses a trigger body loaded from sqlite_master
// and checks that every referenced table exists in the trigger's database
// context. A trigger whose references no longer resolve (after a reopen with
// different attachments) is malformed: SQLite reports "malformed database
// schema (NAME) - trigger NAME cannot reference objects in database X".
// Unqualified references resolve in the trigger's owning database context (a
// trigger inside an ATTACHed database references tables there).

// maxTriggerDepth is SQLite's SQLITE_MAX_TRIGGER_DEPTH default: recursive
// trigger programs abort with "too many levels of trigger recursion" once
// the nesting exceeds this limit.
// validateLoadedTriggerSchemaCtx parses a trigger body loaded from sqlite_master
// and checks that every referenced table exists in the trigger's database
// context. A trigger whose references no longer resolve (after a reopen with
// different attachments) is malformed: SQLite reports "malformed database
// schema (NAME) - trigger NAME cannot reference objects in database X".
// Unqualified references resolve in the trigger's owning database context (a
// trigger inside an ATTACHed database references tables there).

// checkTriggerStmtRefs validates one trigger-body statement's table references
// against the trigger's schema context.
// checkTriggerStmtRefs validates one trigger-body statement's table references
// against the trigger's schema context.
func (e *DMLExecutor) checkTriggerStmtRefs(stmt sql.Stmt, t *schema.Entry, trigCtx *DatabaseContext) error {
	switch s := stmt.(type) {
	case *sql.UpdateStmt:
		if err := e.checkLoadedTableRefCtx(s.Table, t, trigCtx); err != nil {
			return err
		}
		if s.From.Name != "" {
			if err := e.checkLoadedTableRefCtx(s.From.Name, t, trigCtx); err != nil {
				return err
			}
		}
		return e.ctx.ValidateDMLSubqueries(stmt)
	case *sql.DeleteStmt:
		if err := e.checkLoadedTableRefCtx(s.Table, t, trigCtx); err != nil {
			return err
		}
		return e.ctx.ValidateDMLSubqueries(stmt)
	case *sql.InsertStmt:
		return e.ctx.ValidateDMLSubqueries(stmt)
	case *sql.SelectStmt:
		return e.checkTriggerSelectRefs(s, t, trigCtx)
	}
	return nil
}

// checkTriggerSelectRefs validates a trigger-body SELECT's FROM and JOIN
// table references.

// checkTriggerSelectRefs validates a trigger-body SELECT's FROM and JOIN
// table references.

// checkTriggerSelectRefs validates a trigger-body SELECT's FROM and JOIN
// table references.
// checkTriggerSelectRefs validates a trigger-body SELECT's FROM and JOIN
// table references.
func (e *DMLExecutor) checkTriggerSelectRefs(s *sql.SelectStmt, t *schema.Entry, trigCtx *DatabaseContext) error {
	// A trigger body SELECT only needs its referenced tables to exist
	// in the trigger's schema (a reopen with different attachments
	// makes it malformed). Expression-level checks — scalar-subquery
	// arity, row-value misuse — are deferred to the DML statement that
	// fires the trigger (validateDMLSubqueries), matching SQLite:
	// CREATE TRIGGER ... SELECT (SELECT c0,c1 FROM t0) ... is
	// accepted, and the DELETE that fires it errors "sub-select
	// returns 2 columns - expected 1" (rowvalue 28.10).
	if s.From.Name != "" {
		if err := e.checkLoadedTableRefCtx(s.From.Name, t, trigCtx); err != nil {
			return err
		}
	}
	for _, j := range s.Joins {
		if j.Table.Name != "" {
			if err := e.checkLoadedTableRefCtx(j.Table.Name, t, trigCtx); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkLoadedTableRefCtx verifies a table reference in a loaded trigger
// resolves in the trigger's schema context.

// fireTrigger fires a single trigger matching the given event and timing.
// Returns a Result with an error if execution fails, or nil on success
// (including when the trigger does not match or its WHEN clause is false).

// checkLoadedTableRefCtx verifies a table reference in a loaded trigger
// resolves in the trigger's schema context.
// fireTrigger fires a single trigger matching the given event and timing.
// Returns a Result with an error if execution fails, or nil on success
// (including when the trigger does not match or its WHEN clause is false).

// triggerDepthExceeded reports whether the trigger nesting limit is reached.
// triggerDepthExceeded reports whether the trigger nesting limit is reached.
func (e *DMLExecutor) triggerDepthExceeded() bool {
	limit := maxTriggerDepth
	if e.ctx.TriggerDepthLimit() > 0 {
		limit = e.ctx.TriggerDepthLimit()
	}
	return e.ctx.TriggerDepth() >= limit
}

// triggerMatchesUpdateOf reports whether an UPDATE trigger's OF column list is
// satisfied by the current statement.

// triggerMatchesUpdateOf reports whether an UPDATE trigger's OF column list is
// satisfied by the current statement.

// triggerMatchesUpdateOf reports whether an UPDATE trigger's OF column list is
// satisfied by the current statement.
// triggerMatchesUpdateOf reports whether an UPDATE trigger's OF column list is
// satisfied by the current statement.
func (e *DMLExecutor) triggerMatchesUpdateOf(t *schema.Entry) bool {
	if ofCols := parseTriggerUpdateOf(t.SQL); len(ofCols) > 0 {
		return e.triggerSetsColumn(ofCols)
	}
	return true
}

// triggerWhenPasses evaluates a trigger's WHEN clause; a missing WHEN passes.

// triggerWhenPasses evaluates a trigger's WHEN clause; a missing WHEN passes.

// triggerWhenPasses evaluates a trigger's WHEN clause; a missing WHEN passes.
// triggerWhenPasses evaluates a trigger's WHEN clause; a missing WHEN passes.
func (e *DMLExecutor) triggerWhenPasses(t *schema.Entry) (bool, error) {
	whenExpr := e.parseTriggerWhen(t.SQL)
	if whenExpr == nil {
		return true, nil
	}
	// resolve.c: the trigger program's WHEN is compiled against the subject
	// table's columns when the firing statement is prepared — an unknown
	// column errors "no such column: NAME" (insert3-131, update-9.14: the
	// WHEN expression evaluates per firing row via the NEW./OLD. row).
	if err := e.resolveTriggerWhenColumns(t); err != nil {
		return false, err
	}
	val, err := e.ctx.EvalExpr(whenExpr, nil)
	if err != nil {
		return false, err
	}
	return val != nil && execexpr.ToBool(val), nil
}

// resolveTriggerWhenColumns resolves a trigger's WHEN clause against its
// subject table's columns (SQLite resolve.c resolveTriggerStep at trigger
// program code time): an unknown column errors "no such column: NAME". A
// trigger without a WHEN clause, or whose subject table cannot be resolved,
// passes (the latter is reported by the schema-load validation instead).
func (e *DMLExecutor) resolveTriggerWhenColumns(t *schema.Entry) error {
	whenExpr := e.parseTriggerWhen(t.SQL)
	if whenExpr == nil {
		return nil
	}
	te, _, terr := e.ctx.FindTable(t.TblName)
	if terr != nil || te == nil {
		return nil
	}
	colDefs := e.ctx.ParseColumnDefs(te.Name, te.SQL)
	lookup := make(map[string]bool, len(colDefs))
	for _, cd := range colDefs {
		lookup[strings.ToLower(cd.Name)] = true
	}
	var bad string
	execquery.WalkExprFull(whenExpr, func(n sql.Expr) {
		if bad != "" {
			return
		}
		ref, ok := n.(*sql.ColumnRef)
		if !ok {
			return
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, "new") && !strings.EqualFold(ref.Table, "old") {
			return // some other qualifier: its own resolution scope
		}
		if !lookup[strings.ToLower(ref.Name)] {
			bad = ref.Name
		}
	})
	if bad != "" {
		return fmt.Errorf("no such column: %s", bad)
	}
	return nil
}

// prepareUpdateTriggers resolves the WHEN clauses of the target table's
// UPDATE triggers. SQLite codes the UPDATE statement's trigger programs at
// prepare time (sqlite3CodeRowTriggerProgram), so a WHEN clause referencing
// an unknown column errors "no such column: NAME" even when no row matches
// the WHERE clause (update.test 14.2/14.4).
func (e *DMLExecutor) prepareUpdateTriggers(tableEntry *schema.Entry) *Result {
	if e.ctx.TriggersSuppressed() || !e.hasTriggersForTable(tableEntry.Name) {
		return nil
	}
	_, triggers := e.collectTableTriggers(tableEntry.Name)
	for _, t := range triggers {
		if !e.isCodedUpdateTrigger(t) {
			continue
		}
		if err := e.resolveTriggerWhenColumns(t); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

// isCodedUpdateTrigger reports whether t is an UPDATE trigger whose OF column
// list intersects the statement's SET columns. OF-column selectivity is a
// code-time decision (sqlite3CodeRowTriggerProgram): a trigger that is not
// coded for the statement never compiles its WHEN.
func (e *DMLExecutor) isCodedUpdateTrigger(t *schema.Entry) bool {
	_, declEvent := parseTriggerHeader(t.SQL)
	if declEvent != "UPDATE" {
		return false
	}
	return e.triggerMatchesUpdateOf(t)
}

// parseTriggerBody extracts and parses the statements between a trigger's BEGIN
// and END keywords.

// parseTriggerBody extracts and parses the statements between a trigger's BEGIN
// and END keywords.

// parseTriggerBody extracts and parses the statements between a trigger's BEGIN
// and END keywords.
// parseTriggerBody extracts and parses the statements between a trigger's BEGIN
// and END keywords.
func parseTriggerBody(t *schema.Entry) ([]sql.Stmt, bool) {
	upper := strings.ToUpper(t.SQL)
	beginIdx := strings.Index(upper, "BEGIN")
	if beginIdx < 0 {
		return nil, false
	}
	endIdx := strings.LastIndex(upper, "END")
	if endIdx < 0 {
		return nil, false
	}
	body := t.SQL[beginIdx+5 : endIdx]
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, false
	}
	stmts, perr := parse.ParseSQL(body)
	if perr != nil {
		return nil, false
	}
	return stmts, true
}

// execTriggerBody runs a trigger's parsed statements, handling RAISE(IGNORE)
// and table-not-found error qualification.

// execTriggerBody runs a trigger's parsed statements, handling RAISE(IGNORE)
// and table-not-found error qualification.

// applyOuterOrConflict applies the firing statement's ON CONFLICT policy to
// a trigger-body step (SQLite trigger.c codeTriggerProgram: pParse->eOrconf =
// (orconf==OE_Default) ? pStep->orconf : orconf — a firing statement that
// specified an ON CONFLICT clause REPLACES the body step's own policy
// outright, whatever it is). Only INSERT and UPDATE steps carry an OR policy;
// other statements are untouched.
func (e *DMLExecutor) applyOuterOrConflict(stmt sql.Stmt) {
	outer := e.ctx.OuterOrConflict()
	if outer == "" || strings.EqualFold(outer, "DEFAULT") {
		return
	}
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		// Rewrite the full OR-clause flag set exactly as the parser would
		// have for the outer mode (trigger2-6.1f: outer INSERT OR REPLACE
		// turns the body's INSERT OR IGNORE into REPLACE, so the trigger's
		// (new.a,0,0) row replaces the just-inserted row; trigger2-6.1b:
		// outer INSERT OR ABORT raises the body step's UNIQUE violation).
		s.OrConflict = outer
		s.IsReplace = strings.EqualFold(outer, "REPLACE")
		s.OrIgnore = strings.EqualFold(outer, "IGNORE")
		s.OrFail = strings.EqualFold(outer, "FAIL")
	case *sql.UpdateStmt:
		s.OnConflict = outer
	}
}

// execTriggerBody runs a trigger's parsed statements, handling RAISE(IGNORE)
// and table-not-found error qualification.
func (e *DMLExecutor) execTriggerBody(stmts []sql.Stmt, timing string) *Result {
	for _, stmt := range stmts {
		// SQLite trigger.c codeTriggerProgram: a trigger-body step without an
		// explicit ON CONFLICT inherits the firing statement's policy
		// (pParse->eOrconf). Apply it here so INSERT OR ABORT/FAIL/ROLLBACK
		// propagates into the body (trigger2-6.1b).
		e.applyOuterOrConflict(stmt)
		res := e.ctx.Exec(stmt)
		if res.Error == nil {
			continue
		}
		// RAISE(IGNORE) aborts the trigger program: the statement that
		// raised it is skipped and remaining statements in the program do
		// not run. For BEFORE triggers this also aborts the triggering
		// statement (the row is skipped, no error); for AFTER triggers the
		// statement already committed, so just stop the program.
		if res.Error == errRaiseIgnore {
			if timing == "BEFORE" {
				return &Result{Error: errRaiseIgnore}
			}
			return nil
		}
		// Add the trigger's schema prefix to table-not-found errors during
		// trigger execution, matching SQLite's behavior where trigger
		// execution errors include the owning database's schema (a trigger
		// in an attached database reports "aux.t10", not "main.t10").
		res.Error = e.qualifyTriggerTableError(res.Error)
		return res
	}
	return nil
}

// qualifyTriggerTableError adds the current trigger's schema prefix to
// unqualified table-not-found errors raised during trigger execution. The
// schema is the trigger's owning database (the current DML context), matching
// SQLite's behavior: a trigger in main reports "main.t9", one in aux reports
// "aux.t10".
func (e *DMLExecutor) qualifyTriggerTableError(err error) error {
	errMsg := err.Error()
	if !strings.Contains(errMsg, "no such table:") {
		return err
	}
	// Extract the table name and add the schema prefix if not already qualified
	if parts := strings.SplitN(errMsg, "no such table: ", 2); len(parts) == 2 {
		tableName := parts[1]
		if !strings.Contains(tableName, ".") {
			schema := "main"
			if e.currentDMLCtx != nil && e.currentDMLCtx.Name != "" {
				schema = e.currentDMLCtx.Name
			}
			return fmt.Errorf("no such table: %s.%s", schema, tableName)
		}
	}
	return err
}

// parseTriggerWhen extracts and parses the WHEN expression of a trigger's
// CREATE TRIGGER SQL text. Returns nil when the trigger has no WHEN clause.

// parseTriggerWhen extracts and parses the WHEN expression of a trigger's
// CREATE TRIGGER SQL text. Returns nil when the trigger has no WHEN clause.

// mapNamedTupleValues starts with each column's DEFAULT and overrides with the
