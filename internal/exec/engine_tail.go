// Package exec implements query execution.
//
// The Engine orchestrates statement execution and implements the capability
// interfaces for the sub-executors: SELECT statements delegate to
// internal/execquery, DML (INSERT/UPDATE/DELETE) to internal/execdml,
// PRAGMA to internal/execpragma, and expression evaluation to
// internal/execexpr. The DDL, ALTER, FK, and trigger machinery lives here.
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

func (e *Engine) getDB(name string) *DatabaseContext {
	upper := strings.ToUpper(name)
	if db, ok := e.databases[upper]; ok {
		return db
	}
	return nil
}

// resolveDB resolves a potentially schema-qualified name to a database context and the unqualified name.
// If no schema prefix is present, returns nil for ctx (caller should use mainDB).
//
//lint:ignore U1000 Planned for P3 ATTACH
func (e *Engine) resolveDB(name string) (ctx *DatabaseContext, object string) {
	schemaName, object := parseSchemaName(name)
	if schemaName == "" {
		return nil, object
	}
	ctx = e.getDB(schemaName)
	return ctx, object
}

// detectExternalSchemaChanges checks every attached database's schema manager
// for external file modification (an attached file written by another
// connection). When a change is detected the pager cache, tableCache, and
// rowid/sequence caches are invalidated so the next lookup re-reads the file.

// findTable searches for a table across all attached databases.
// If the name has a schema prefix (e.g. "aux.t3"), it searches only that database.
// If no schema prefix, it searches main first, then attached databases.

// isNonModifiableTable reports whether a table entry cannot be modified by
// INSERT/UPDATE/DELETE: the sqlite_schema system tables and pragma virtual
// tables (PRAGMA_ prefixed) are read-only.
func (e *Engine) isNonModifiableTable(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	switch {
	case strings.EqualFold(entry.Name, "sqlite_master"),
		strings.EqualFold(entry.Name, "sqlite_schema"),
		strings.EqualFold(entry.Name, "sqlite_temp_master"),
		strings.EqualFold(entry.Name, "sqlite_temp_schema"):
		// PRAGMA writable_schema=ON permits direct edits to sqlite_schema.
		return !e.settings.writableSchema
	}
	return strings.HasPrefix(strings.ToUpper(entry.Name), "PRAGMA_")
}

// isStoragelessVirtualTable reports whether a table entry is a virtual table
// without module-backed row storage (rtree, echo, dbstat, ...). Such tables
// accept writes as no-ops; FTS tables have real storage and are excluded.
func (e *Engine) isStoragelessVirtualTable(entry *schema.Entry) bool {
	if entry == nil || !strings.HasPrefix(strings.ToUpper(entry.SQL), "CREATE VIRTUAL TABLE") {
		return false
	}
	_, isFTS := e.ftsTables[entry.Name]
	return !isFTS
}

// findView searches for a view across all attached databases.

// findTrigger searches for a trigger across all attached databases.
func (e *Engine) findTrigger(name string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		ctx := e.getDB(schemaName)
		if ctx == nil {
			return nil, nil, fmt.Errorf("no such trigger: %s", name)
		}
		entry, err := ctx.Schema.FindTrigger(objName)
		if err != nil {
			return nil, nil, err
		}
		return entry, ctx, nil
	}

	// An unqualified trigger name searches the temp schema first (temp
	// shadows main — e_droptrigger.test's unqualified DROP TRIGGER tr1
	// drops the temp tr1 even when main has one). SQLite's
	// sqlite3FindTrigger searches temp before main.
	if tempDB := e.getDB("temp"); tempDB != nil && tempDB != e.mainDB {
		if entry, err := tempDB.Schema.FindTrigger(name); err == nil {
			return entry, tempDB, nil
		}
	}

	entry, err := e.mainDB.Schema.FindTrigger(name)
	if err == nil {
		return entry, e.mainDB, nil
	}

	// Only then try attached databases.
	for _, ctx := range e.dbList {
		if ctx == e.mainDB || ctx == e.getDB("temp") {
			continue
		}
		entry, err := ctx.Schema.FindTrigger(name)
		if err == nil {
			return entry, ctx, nil
		}
	}

	return nil, nil, fmt.Errorf("no such trigger: %s", name)
}

// findIndex searches for an index across all attached databases.
func (e *Engine) findIndex(name string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		ctx := e.getDB(schemaName)
		if ctx == nil {
			return nil, nil, fmt.Errorf("no such index: %s", name)
		}
		entry, err := ctx.Schema.FindIndex(objName)
		if err != nil {
			return nil, nil, err
		}
		return entry, ctx, nil
	}

	entry, err := e.mainDB.Schema.FindIndex(name)
	if err == nil {
		return entry, e.mainDB, nil
	}

	for _, ctx := range e.dbList {
		if ctx == e.mainDB {
			continue
		}
		entry, err := ctx.Schema.FindIndex(name)
		if err == nil {
			return entry, ctx, nil
		}
	}

	return nil, nil, fmt.Errorf("no such index: %s", name)
}

// validateNoRaiseOutsideTrigger walks a statement's expression trees and
// rejects RAISE() expressions when not inside a trigger program (SQLite's
// "RAISE() may only be used within a trigger-program"). This is a compile-
// time check: the runtime evaluation in evalRaiseExpr would miss RAISE()
// inside expressions that never execute (e.g. GROUP BY/HAVING over an empty
// table).

// checkSelectRaise walks a SELECT's columns, WHERE, GROUP BY, HAVING, ORDER
// BY, and compound tails for RAISE() expressions.

// Exec executes a single SQL statement and returns the result.

// withDMLCTEs runs a DML statement (INSERT/UPDATE/DELETE) with its WITH (CTE)
// definitions pushed onto the CTE scope stack, so subqueries inside the
// statement (e.g. UPDATE t1 SET x=(SELECT b FROM uset WHERE ...)) can resolve
// the CTE by name. SQLite scopes a WITH clause to the single statement it
// prefixes, whether SELECT or DML.
func (e *Engine) withDMLCTEs(ctes []sql.CTEDef, fn func() *Result) *Result {
	if len(ctes) == 0 {
		return fn()
	}
	if dup := duplicateCTEName(ctes); dup != "" {
		return &Result{Error: fmt.Errorf("duplicate WITH table name: %s", dup)}
	}
	e.selectEngine.PushCTEScope(ctes)
	defer e.selectEngine.PopCTEScope()
	return fn()
}

// duplicateCTEName returns the name of a CTE declared twice in the same WITH
// clause, or "" when all names are unique. SQLite reports
// "duplicate WITH table name: NAME" at prepare time.
func duplicateCTEName(ctes []sql.CTEDef) string {
	seen := make(map[string]bool, len(ctes))
	for _, c := range ctes {
		key := strings.ToLower(c.Name)
		if seen[key] {
			return c.Name
		}
		seen[key] = true
	}
	return ""
}

// pagerSnap pairs a pager with the snapshot taken from it, so a restore can
// match each pager to its own snapshot regardless of map iteration order.
type pagerSnap struct {
	pg    *pager.Pager
	state *pager.PagerState
}

// ftsSnap pairs an FTS table with a deep copy of its in-memory index, so a
// rollback can undo FTS changes that the pager snapshots do not cover (the
// FTS store lives in memory, not in the btree pages). It also carries the
// pending-docid list so a rolled-back insert does not get flushed as a
// segment later.
type ftsSnap struct {
	table         *fts.FTS3Table
	state         *fts.InvertedIndex
	pending       []int64
	deleteMarkers map[int64][]string
}

// dmlCanSkipSnapshot reports whether a DML statement can skip the pre-rollback
// pager snapshot because it cannot fail after partially writing. A single-row
// VALUES INSERT (no SELECT, no RETURNING, not REPLACE/upsert, no triggers, no
// FK enforcement) either writes its one row or fails before writing — there is
// no partial state to restore.
func (e *Engine) dmlCanSkipSnapshot(stmt sql.Stmt) bool {
	ins, ok := stmt.(*sql.InsertStmt)
	if !ok {
		return false // UPDATE/DELETE can fail mid-scan after earlier writes
	}
	if ins.Select != nil || ins.HasReturning || ins.IsReplace || len(ins.Values) != 1 {
		return false
	}
	if ins.OnConflict != nil {
		return false // DO NOTHING / DO UPDATE upsert paths may skip or modify rows
	}
	if e.settings.foreignKeys {
		return false // FK enforcement could reject after other writes
	}
	if e.hasTriggersForTable(ins.Table) {
		return false // a trigger could fail after the insert
	}
	return true
}

// stmtTargetsFTSContent reports whether a statement writes to an FTS table's
// CONTENT (the virtual table itself), which modifies the in-memory index and
// therefore needs the O(index) InvertedIndex snapshot for rollback. Writes to
// FTS SHADOW tables (%_segdir, %_segments, %_stat) touch only the pager btrees
// and are covered by the pager snapshot alone.
func (e *Engine) stmtTargetsFTSContent(stmt sql.Stmt) bool {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		_, isFTS := e.ftsTables[s.Table]
		return isFTS
	case *sql.UpdateStmt:
		_, isFTS := e.ftsTables[s.Table]
		return isFTS
	case *sql.DeleteStmt:
		_, isFTS := e.ftsTables[s.Table]
		return isFTS
	}
	return false
}

// stmtFTSShadowOwner returns the name of the FTS table whose SHADOW table
// (%_segdir, %_segments, %_content, %_docsize, %_stat) the statement targets,
// or "" when the statement does not touch an FTS shadow table. A direct user
// write to a shadow table makes the in-memory index stale (SQLite always
// reads the index from the segments), so the engine reloads it after the
// statement.
func (e *Engine) stmtFTSShadowOwner(stmt sql.Stmt) string {
	target := ""
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		target = s.Table
	case *sql.UpdateStmt:
		target = s.Table
	case *sql.DeleteStmt:
		target = s.Table
	}
	if target == "" {
		return ""
	}
	for name := range e.ftsTables {
		for _, suffix := range []string{"_segdir", "_segments", "_content", "_docsize", "_stat"} {
			if strings.EqualFold(target, name+suffix) {
				return name
			}
		}
	}
	return ""
}

// snapshotAllPagers captures the in-memory state of every database pager,
// pairing each snapshot with the pager it came from. It also snapshots every
// FTS table's in-memory index so a failed statement can undo FTS writes the
// pager restore does not cover.
func (e *Engine) snapshotAllPagers() []pagerSnap {
	var snaps []pagerSnap
	seen := make(map[*pager.Pager]bool)
	for _, ctx := range e.databases {
		if ctx == nil || ctx.Pager == nil || seen[ctx.Pager] {
			continue
		}
		seen[ctx.Pager] = true
		snaps = append(snaps, pagerSnap{pg: ctx.Pager, state: ctx.Pager.Snapshot()})
	}
	// Attach the FTS snapshots to the statement snapshot list via a marker:
	// the pager restore loop ignores entries whose pg is nil, and the FTS
	// restore below runs alongside the pager restore in execRollbackOnError
	// (restoreAllPagers restores only pager entries; the FTS entries are
	// consumed by restoreFTSAll).
	e.ftsSnapshots = e.snapshotAllFTS()
	return snaps
}

// snapshotAllFTS captures the in-memory index of every registered FTS table.
func (e *Engine) snapshotAllFTS() []ftsSnap {
	var snaps []ftsSnap
	for _, t := range e.ftsTables {
		if t != nil {
			snaps = append(snaps, ftsSnap{table: t, state: t.Snapshot(), pending: t.PendingSnapshot(), deleteMarkers: t.DeleteMarkerTermsSnapshot()})
		}
	}
	return snaps
}

// restoreAllFTS restores every FTS table to the snapshot captured by
// snapshotAllFTS (used when a statement fails and the pager restore undoes
// btree writes but not the in-memory FTS index).
func (e *Engine) restoreAllFTS() {
	for _, snap := range e.ftsSnapshots {
		if snap.table != nil {
			if snap.state != nil {
				snap.table.Restore(snap.state)
			}
			snap.table.RestorePending(snap.pending)
			snap.table.RestoreDeleteMarkerTerms(snap.deleteMarkers)
		}
	}
	e.ftsSnapshots = nil
}

// restoreAllPagers restores each pager to the snapshot captured from it by
// snapshotAllPagers. Pairing by pager identity (rather than positional index)
// keeps snapshots matched even though e.databases is a map with random
// iteration order.
func (e *Engine) restoreAllPagers(snaps []pagerSnap) {
	if len(snaps) == 0 {
		return
	}
	for _, snap := range snaps {
		if snap.pg != nil && snap.state != nil {
			snap.pg.Restore(snap.state)
		}
	}
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
}

func (e *Engine) execOtherDDL(stmt sql.Stmt) *Result {
	// PRAGMA query_only rejects all write statements, including DDL. PRAGMA
	// statements are exempt (the query_only pragma itself must toggle).
	if e.settings.queryOnly {
		if _, isPragma := stmt.(*sql.PragmaStmt); !isPragma {
			return &Result{Error: fmt.Errorf("attempt to write a readonly database")}
		}
	}
	// Invalidate table cache on any DDL operation to ensure consistency
	e.invalidateTableCache()

	switch s := stmt.(type) {
	case *sql.CreateTableStmt, *sql.CreateIndexStmt, *sql.CreateViewStmt, *sql.CreateTriggerStmt, *sql.CreateVirtualTableStmt:
		return e.execCreateStmt(s)
	case *sql.DropTableStmt, *sql.DropIndexStmt, *sql.DropViewStmt, *sql.DropTriggerStmt:
		return e.execDropStmt(s)
	case *sql.AnalyzeStmt:
		return e.execAnalyze(s)
	case *sql.PragmaStmt:
		return e.execPragma(s)
	case *sql.AlterTableStmt:
		return e.ddl.Alter(s)
	case *sql.ExplainStmt:
		return e.execExplain(s)
	case *sql.AttachStmt:
		return e.execAttachOrDetach(s)
	case *sql.ReindexStmt:
		return e.execReindex(s)
	default:
		// Begin, Rollback, Vacuum, Reindex, Savepoint — all no-ops
		return &Result{}
	}
}

// execCreateStmt dispatches a CREATE statement to its executor.
func (e *Engine) execCreateStmt(stmt sql.Stmt) *Result {
	if e.tx.inTransaction {
		e.tx.txSchemaChanged = true
	}
	return e.ddl.CreateStmt(stmt)
}

// execDropStmt dispatches a DROP statement to its executor.
func (e *Engine) execDropStmt(stmt sql.Stmt) *Result {
	return e.ddl.DropStmt(stmt)
}

// execAttachOrDetach dispatches ATTACH / DETACH to the matching executor.
func (e *Engine) execAttachOrDetach(s *sql.AttachStmt) *Result {
	return e.ddl.AttachOrDetach(s)
}
