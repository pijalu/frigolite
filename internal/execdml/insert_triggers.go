// This file holds INSERT's trigger machinery (BEFORE/AFTER INSERT row and
// statement triggers, with schema-context validation for triggers loaded
// from attached databases), the statement expression walker used to
// validate trigger bodies, and sqlite_sequence bookkeeping validation for
// INSERTs into tables with AUTOINCREMENT.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// and assigns an auto-generated rowid to an empty INTEGER PRIMARY KEY column.

// fireTriggers fires triggers matching the given event and timing for the table.
func (e *DMLExecutor) fireTriggers(tableName, event, timing string, newRow, oldRow RowMap) *Result {
	// Engine-wide suppression (logical backup/VACUUM rebuild): the copy
	// replays DDL and rows without running trigger programs, matching
	// SQLite's page-level vacuum.c copy.
	if e.ctx.TriggersSuppressed() {
		return &Result{}
	}
	tableCtx, refs := e.collectTableTriggerRefs(tableName)
	if len(refs) == 0 {
		return &Result{}
	}
	// SQLite fires AFTER triggers in REVERSE creation order (the most recently
	// created AFTER trigger fires first; e_droptrigger.test's aux.tr3 fires
	// before aux.tr2). BEFORE triggers fire in creation order. fireTrigger
	// filters by timing, so reverse the whole slice for the AFTER pass.
	if strings.EqualFold(timing, "AFTER") {
		for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
			refs[i], refs[j] = refs[j], refs[i]
		}
	}
	for _, ref := range refs {
		// Recursive-trigger guard (recursive_triggers OFF): a trigger does not
		// re-fire itself for a nested statement on the same table, but OTHER
		// triggers on the table DO fire (SQLite: the currently-executing
		// trigger program is excluded; e_changes autoinc-3928 fires r2 for
		// r1's inner inserts). fireTrigger pushes its own key onto the chain.
		if e.triggerInChain(tableCtx.Name, ref.entry.Name) {
			continue
		}
		if res := e.fireTrigger(ref.entry, ref.ctx, event, timing, newRow, oldRow); res != nil {
			return res
		}
	}
	return &Result{}
}

// triggerRef pairs a trigger schema entry with the database context whose
// schema contains it (SQLite's Trigger.pTabSchema / owning iDb). The owning
// context scopes the trigger body's unqualified name resolution, so it must
// be the schema the trigger was collected FROM — never re-derived by name,
// because two schemas may hold same-named triggers (attach-4.6/4.7 create
// t3r3 in both main and the attached db).
type triggerRef struct {
	ctx   *DatabaseContext
	entry *schema.Entry
}

// collectTableTriggerRefs returns the database context owning the named table
// and the triggers bound to it paired with their owning contexts: the table's
// own-schema triggers plus the TEMP triggers that target it, in registration
// order (trigger.c sqlite3TriggerList: pTab->pTrigger with the TEMP triggers
// on pTabSchema).
func (e *DMLExecutor) collectTableTriggerRefs(tableName string) (*DatabaseContext, []triggerRef) {
	tableCtx := e.triggerTableContext(tableName)
	var refs []triggerRef
	if ts, err := tableCtx.Schema.FindTriggersForTable(tableName); err == nil {
		for _, t := range ts {
			if e.triggerTargetsCtx(t, tableCtx) {
				refs = append(refs, triggerRef{ctx: tableCtx, entry: t})
			}
		}
	}
	if tc := e.ctx.GetDB("temp"); tc != nil && tc != tableCtx && tableCtx != nil {
		tempTriggers, _ := tc.Schema.FindTriggersForTable(tableName)
		for _, tt := range tempTriggers {
			if tt == nil {
				continue
			}
			if e.shouldAppendTempTrigger(tt, tableCtx, tc, tableName) {
				refs = append(refs, triggerRef{ctx: tc, entry: tt})
			}
		}
	}
	return tableCtx, refs
}

// collectTableTriggers returns the database context owning the named table
// and the triggers bound to it: the table's own-schema triggers plus the
// TEMP triggers that target it, in registration order (trigger.c
// sqlite3TriggerList: pTab->pTrigger with the TEMP triggers on pTabSchema).
func (e *DMLExecutor) collectTableTriggers(tableName string) (*DatabaseContext, []*schema.Entry) {
	tableCtx := e.triggerTableContext(tableName)
	var triggers []*schema.Entry
	if ts, err := tableCtx.Schema.FindTriggersForTable(tableName); err == nil {
		for _, t := range ts {
			if e.triggerTargetsCtx(t, tableCtx) {
				triggers = append(triggers, t)
			}
		}
	}
	return tableCtx, e.appendTempTriggers(tableCtx, tableName, triggers)
}

// triggerTableContext resolves the database context for a table's triggers:
// the current DML context when set, otherwise the table's owning context.

// triggerTableContext resolves the database context for a table's triggers:
// the current DML context when set, otherwise the table's owning context.

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
func (e *DMLExecutor) validateLoadedTriggerSchemaCtx(t *schema.Entry, trigCtx *DatabaseContext) error {
	stmts, perr := parse.ParseSQL(t.SQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	var trig *sql.CreateTriggerStmt
	for _, st := range stmts {
		if c, ok := st.(*sql.CreateTriggerStmt); ok {
			trig = c
			break
		}
	}
	if trig == nil {
		return nil
	}
	for _, stmt := range trig.Statements {
		if err := e.checkTriggerStmtRefs(stmt, t, trigCtx); err != nil {
			return err
		}
	}
	return nil
}

// visitExprsInStmt walks every expression of a DML/SELECT statement (the
// statement kinds that can appear in a trigger body), invoking fn on each.
func visitExprsInStmt(stmt sql.Stmt, fn func(sql.Expr)) {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		for _, tuple := range s.Values {
			for _, e := range tuple {
				fn(e)
			}
		}
		if s.Select != nil {
			visitSelectExprs(s.Select, fn)
		}
	case *sql.UpdateStmt:
		for _, a := range s.Assignments {
			fn(a.Value)
		}
		fn(s.Where)
	case *sql.DeleteStmt:
		fn(s.Where)
	case *sql.SelectStmt:
		visitSelectExprs(s, fn)
	}
}

// visitSelectExprs walks a SELECT's result columns, WHERE, GROUP BY, HAVING,
// and ORDER BY expressions, invoking fn on each.
func visitSelectExprs(s *sql.SelectStmt, fn func(sql.Expr)) {
	if s == nil {
		return
	}
	for _, col := range s.Columns {
		fn(col.Expr)
	}
	fn(s.Where)
	for _, g := range s.GroupBy {
		fn(g)
	}
	fn(s.Having)
	for _, ob := range s.OrderBy {
		fn(ob.Expr)
	}
}

// isTempTrigger reports whether a trigger entry lives in the TEMP schema
// (TEMP triggers are always trusted, so the trusted_schema function-safety
// check skips them — trustschema1-2.120/2.150/3.120).

// validateSequenceTable checks the sqlite_sequence schema entry before an
// AUTOINCREMENT insert reads or writes it: the table must exist and be an
// ordinary rowid table. A renamed-away sequence or an impostor (WITHOUT
// ROWID / virtual table planted via writable_schema) is SQLITE_CORRUPT,
func (e *DMLExecutor) validateSequenceTable(dbCtx *DatabaseContext) *Result {
	entries, err := dbCtx.Schema.GetEntries(schema.TypeTable)
	if err != nil {
		return nil
	}
	for _, ent := range entries {
		if !isSequenceEntry(ent) {
			continue
		}
		if up := strings.ToUpper(ent.SQL); strings.Contains(up, "WITHOUT ROWID") || strings.Contains(up, "VIRTUAL TABLE") {
			return &Result{Error: fmt.Errorf("database disk image is malformed")}
		}
		// The sequence table must declare EXACTLY two columns (insert.c
		// autoIncBegin: pSeqTab->nCol!=2 → SQLITE_CORRUPT_SEQUENCE,
		// ticket d8dc2b3a58cd5dc2918a1d4acb): the sequence update reads
		// (name, seq) positionally. A writable_schema rewrite to a
		// 1-column declaration is corruption (autoinc-12.5); any other
		// 2-column spelling keeps working — the columns are accessed by
		// position, not name (autoinc-12.6/12.7).
		if len(e.ctx.ParseColumnDefs(ent.Name, ent.SQL)) != 2 {
			return &Result{Error: fmt.Errorf("database disk image is malformed")}
		}
		if !sequenceBTreeOK(dbCtx, ent) {
			return &Result{Error: fmt.Errorf("database disk image is malformed")}
		}
		return nil
	}
	return &Result{Error: fmt.Errorf("database disk image is malformed")}
}

// isSequenceEntry reports whether the schema entry is the sqlite_sequence
// table or one of its renamed leftovers.
func isSequenceEntry(ent *schema.Entry) bool {
	return strings.EqualFold(ent.Name, "sqlite_sequence") || strings.EqualFold(ent.TblName, "sqlite_sequence")
}

// sequenceBTreeOK reports whether the sequence table's root page holds a
// table btree. A rootpage swap (autoinc-12.4: writable_schema points
// sqlite_sequence at another table's btree) leaves an index-type page where
// a table btree must be — SQLite reports corruption when the sequence btree
// opens.
func sequenceBTreeOK(dbCtx *DatabaseContext, ent *schema.Entry) bool {
	pg, err := dbCtx.Pager.ReadPage(ent.RootPage)
	if err != nil || len(pg.Data) == 0 {
		return true
	}
	coff := 0
	if ent.RootPage == 1 {
		coff = 100
	}
	switch pg.Data[coff] {
	case 0x0D, 0x05: // table leaf / table interior
		return true
	}
	return false
}
