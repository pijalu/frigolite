package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// validateTriggerTarget runs the CREATE TRIGGER validations of trigger.c
// sqlite3BeginTrigger, in SQLite's order, and returns the failing Result (nil
// when the trigger target is acceptable):
//  1. the ON table must exist — schema-fixed to the trigger's database
//     (build.c sqlite3LocateTable + attach.c fixSelectCb), so a missing table
//     reports that schema's qualifier ("no such table: main.no_such_table");
//     TEMP triggers are never schema-fixed (fixSelectCb bTemp), leaving the
//     name unqualified;
//  2. no triggers on virtual tables (vtab5-1.2);
//  3. no duplicate trigger name in the target schema ("trigger %T already
//     exists", the name token verbatim) unless IF NOT EXISTS;
//  4. no triggers on system tables;
//  5. views accept only INSTEAD OF, and INSTEAD OF only applies to views.
func (e *DDLExecutor) validateTriggerTarget(s *sql.CreateTriggerStmt, ctx *DatabaseContext, triggerName, tableName string, isTempTrigger bool) *Result {
	if res := e.validateTriggerOnTable(tableName, isTempTrigger, ctx); res != nil {
		return res
	}
	if res := e.validateTriggerName(s, ctx, triggerName, tableName); res != nil {
		return res
	}
	return nil
}

// validateTriggerOnTable checks the CREATE TRIGGER ON target: the table must
// exist (the not-found error is schema-fixed, trigger.c sqlite3BeginTrigger →
// build.c sqlite3LocateTable) and must not be a virtual table.
func (e *DDLExecutor) validateTriggerOnTable(tableName string, isTempTrigger bool, ctx *DatabaseContext) *Result {
	if !triggerTableExists(e, tableName) {
		if isTempTrigger {
			return &Result{Error: fmt.Errorf("no such table: %s", tableName)}
		}
		return &Result{Error: fmt.Errorf("no such table: %s.%s", ctx.Name, tableName)}
	}
	if te, _, terr := e.ctx.FindTable(tableName); terr == nil && te != nil {
		if e.ctx.IsStoragelessVirtualTable(te) || te.RootPage == 0 {
			return &Result{Error: fmt.Errorf("cannot create triggers on virtual tables")}
		}
	}
	return nil
}

// validateTriggerName checks the trigger's name and target type: no duplicate
// name in the target schema ("trigger %T already exists" unless IF NOT
// EXISTS), no system tables, views accept only INSTEAD OF and INSTEAD OF only
// applies to tables' counterpart views.
func (e *DDLExecutor) validateTriggerName(s *sql.CreateTriggerStmt, ctx *DatabaseContext, triggerName, tableName string) *Result {
	if e.triggerExists(ctx, triggerName) {
		if !s.IfNotExists {
			return &Result{Error: fmt.Errorf("trigger %s already exists", verbatimTriggerName(s))}
		}
		// IF NOT EXISTS silently keeps the existing trigger.
		return &Result{}
	}
	if isSystemTableName(tableName) {
		return &Result{Error: fmt.Errorf("cannot create trigger on system table")}
	}
	if te, _, terr := e.ctx.FindTable(tableName); terr == nil && te != nil {
		if strings.EqualFold(s.Time, "INSTEAD") {
			return &Result{Error: fmt.Errorf("cannot create INSTEAD OF trigger on table: %s", tableName)}
		}
	} else if ve, _, verr := e.ctx.FindView(tableName); verr == nil && ve != nil {
		if !strings.EqualFold(s.Time, "INSTEAD") {
			return &Result{Error: fmt.Errorf("cannot create %s trigger on view: %s", triggerTimingLabel(s.Time), tableName)}
		}
	}
	return nil
}

// matchKeyword consumes fields[idx] when it equals one of the keywords
// (case-insensitive) and returns the index after it; idx otherwise.
func matchKeyword(fields []string, idx int, keywords ...string) int {
	for _, kw := range keywords {
		if idx < len(fields) && strings.EqualFold(fields[idx], kw) {
			return idx + 1
		}
	}
	return idx
}

// triggerNameToken extracts the trigger-name token as written in the CREATE
// TRIGGER text and reports whether it was quoted. SQLite treats a quoted
// token as one name (a dot inside it is not a schema separator) and formats
// the token verbatim in errors (%T). ok is false when the statement text is
// missing or does not start with the expected keyword sequence.
func triggerNameToken(rawSQL string) (token string, quoted bool, ok bool) {
	fields := strings.Fields(rawSQL)
	idx := matchKeyword(fields, 0, "CREATE")
	if idx == 0 {
		return "", false, false
	}
	afterTemp := matchKeyword(fields, idx, "TEMP", "TEMPORARY")
	afterTrigger := matchKeyword(fields, afterTemp, "TRIGGER")
	if afterTrigger == afterTemp {
		return "", false, false
	}
	afterIf := matchKeyword(fields, afterTrigger, "IF")
	if afterIf != afterTrigger {
		afterNot := matchKeyword(fields, afterIf, "NOT")
		afterExists := matchKeyword(fields, afterNot, "EXISTS")
		if afterNot == afterIf || afterExists == afterNot {
			return "", false, false
		}
		afterTrigger = afterExists
	}
	if afterTrigger >= len(fields) {
		return "", false, false
	}
	tok := fields[afterTrigger]
	switch tok[0] {
	case '"', '[', '`', '\'':
		return tok, true, true
	}
	return tok, false, true
}

// verbatimTriggerName returns the trigger name as written in the CREATE
// TRIGGER text for the "trigger %T already exists" error (trigger.c): the
// token verbatim with its original quoting, and for a schema.name input only
// the name token. Falls back to the decoded AST name.
func verbatimTriggerName(s *sql.CreateTriggerStmt) string {
	tok, quoted, ok := triggerNameToken(s.RawSQL)
	if !ok {
		return s.Name
	}
	if !quoted {
		if dot := strings.Index(tok, "."); dot >= 0 {
			return tok[dot+1:]
		}
	}
	return tok
}

// triggerTimingLabel renders a trigger's declared timing for error messages
// the way trigger.c does: the label is always uppercase and a missing timing
// defaults to BEFORE.
func triggerTimingLabel(time string) string {
	if strings.EqualFold(time, "AFTER") {
		return "AFTER"
	}
	return "BEFORE"
}
