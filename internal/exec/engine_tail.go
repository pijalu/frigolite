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
	"github.com/pijalu/frigolite/internal/quota"
	"strings"

	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
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
	// Screen on the first byte: every reserved name starts with 's'/'S'
	// (sqlite_*) or 'p'/'P' (pragma_*) — an O(1) reject for ordinary tables
	// (this gate runs per statement on the DML paths).
	if len(entry.Name) == 0 {
		return false
	}
	if c := entry.Name[0]; c != 's' && c != 'S' && c != 'p' && c != 'P' {
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
	return len(entry.Name) >= 7 && strings.EqualFold(entry.Name[:7], "pragma_")
}

// isStoragelessVirtualTable reports whether a table entry is a virtual table
// without module-backed row storage (rtree, echo, dbstat, ...). Such tables
// accept writes as no-ops; FTS tables have real storage and are excluded.
func (e *Engine) isStoragelessVirtualTable(entry *schema.Entry) bool {
	if entry == nil || !util.HasPrefixFoldASCII(entry.SQL, "CREATE VIRTUAL TABLE") {
		return false
	}
	if _, isFTS := e.ftsTables[entry.Name]; isFTS {
		return false
	}
	_, isFTS5 := e.fts5Tables[entry.Name]
	return !isFTS5
}

// findView searches for a view across all attached databases.

// findTrigger searches for a trigger across all attached databases.
func (e *Engine) findTrigger(name string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		return e.findTriggerQualified(name, schemaName, objName)
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

// findTriggerQualified resolves a schema-qualified trigger name strictly
// within that schema ("no such trigger" when the schema is unknown).
func (e *Engine) findTriggerQualified(name, schemaName, objName string) (*schema.Entry, *DatabaseContext, error) {
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

// pagerStmtScope pairs a pager with the statement journal opened on it, so a
// rollback can match each pager to its own scope regardless of map iteration
// order.
type pagerSnap struct {
	pg      *pager.Pager
	journal *pager.StmtJournal
}

// dmlCanSkipSnapshot was removed when the per-statement PagerState snapshot
// became a lazily-captured statement journal (pager.c sub-journal): the
// journal costs O(1) per statement and O(modified pages) per rollback, so
// every DML statement can afford one and no statement shape needs skipping.

// ftsSnap pairs an FTS table with a deep copy of its in-memory index, so a
// rollback can undo FTS changes that the pager journals do not cover (the
// FTS store lives in memory, not in the btree pages). It also carries the
// pending-docid list so a rolled-back insert does not get flushed as a
// segment later.
type ftsSnap struct {
	table         *fts.FTS3Table
	state         *fts.InvertedIndex
	pending       []int64
	deleteMarkers map[int64][]string
}

// fts5Snap pairs an fts5 table with a snapshot of its in-memory state (index
// + content mirror), the fts5 counterpart of ftsSnap.
type fts5Snap struct {
	table *fts5.Table
	state *fts5.TableState
}

// dmlCanSkipSnapshot reports whether a DML statement keeps its historical
// full skip of the statement-atomicity rollback: a single-row VALUES INSERT
// (no SELECT, no RETURNING, not REPLACE/upsert, no triggers, no FK
// enforcement) either writes its one row or fails before writing — and the
// interrupted/failing row of that shape LEAVES ITS WRITES VISIBLE, the seam
// frigolite_fts5interrupt_test.go pins (oracle-verified against C: an fts5
// single-row insert interrupted by a progress handler keeps the row). The
// statement journal made every other shape affordable, but re-enabling the
// rollback for THIS shape changed that pinned semantics, so the skip stays.
// (Its original motivation was the O(pages) snapshot copy — the journal
// removed that cost for every non-skipped shape.)
//
// R10.DML extends the skip to the can't-abort point UPDATE/DELETE: a statement
// whose WHERE contains a rowid equality conjunct (rowid/_rowid_/oid or the
// table's INTEGER PRIMARY KEY alias) matches at most ONE row under any
// evaluation, and its target gates (plain statement — no OR clause, no
// RETURNING, no ORDER BY/LIMIT, no FROM; ordinary rowid table — no triggers,
// no FK enforcement, no virtual/FTS target) remove every fail-after-write
// source: SET/WHERE/RETURNING expression errors and the NOT NULL/CHECK/
// UNIQUE gates all run BEFORE the single row's write, and the write itself
// (cell overwrite or delete+reinsert of a just-freed slot) cannot fail on a
// non-quota, non-WAL pager. SQLite keeps the same guarantee statement-scoped
// (vdbe.c opens the sub-journal only for statements that may abort after
// writing; a one-row rewrite of this shape does not).
//
// The main database being in WAL mode disables the skip: a WAL commit writes
// to the "-wal" file, which is a SEPARATE I/O that can fail (disk error,
// fault injection) AFTER the in-memory row write succeeds. The "cannot fail
// after partially writing" assumption is then false, so the rollback must be
// available (otherwise the uncommitted pages stay dirty and the next flush —
// e.g. at Close — re-attempts and re-reports the error).
func (e *Engine) dmlCanSkipSnapshot(stmt sql.Stmt) bool {
	if e.mainDB != nil && e.mainDB.Pager != nil && e.mainDB.Pager.JournalMode() == "wal" {
		return false
	}
	// Quota layer active (test_quota.c shim): a flush can refuse file
	// growth with SQLITE_FULL after the in-memory write succeeded, so the
	// "commit cannot fail" assumption below is void — keep the rollback.
	if quota.Active() {
		return false
	}
	// A registered commit hook makes every commit vetoable after the rows
	// are written: a nonzero hook return fails the implicit COMMIT with
	// SQLITE_CONSTRAINT_COMMITHOOK and rolls the transaction back
	// (vdbeCommit's xCommitCallback check runs BEFORE btree commit phase
	// one, src/vdbeaux.c:2978-2982).
	if e.commitHook != nil {
		return false
	}
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		if !isSimpleSingleValuesInsert(s) {
			return false
		}
		if s.OnConflict != nil {
			return false // DO NOTHING / DO UPDATE upsert paths may skip or modify rows
		}
		if e.settings.foreignKeys {
			return false // FK enforcement could reject after other writes
		}
		if e.hasTriggersForTable(s.Table) {
			return false // a trigger could fail after the insert
		}
		return true
	case *sql.UpdateStmt:
		if s.OnConflict != "" || s.HasReturning || len(s.OrderBy) > 0 || s.Limit != nil {
			return false // OR-clause dispositions and RETURNING need the rollback
		}
		if s.From.Name != "" || s.From.Subquery != nil || len(s.FromJoins) > 0 {
			return false // UPDATE ... FROM is a multi-row join statement
		}
	case *sql.DeleteStmt:
		if s.HasReturning || len(s.OrderBy) > 0 || s.Limit != nil {
			return false
		}
	default:
		return false
	}
	return e.pointDMLCannotAbort(stmt)
}

// pointDMLCannotAbort reports whether an UPDATE/DELETE that passed the clause
// gates (no OR clause / RETURNING / ORDER BY / LIMIT / FROM) additionally
// pins at most one row and targets an ordinary table: a rowid equality
// conjunct in the WHERE (the planner's own rowid-lookup term — any correct
// evaluation must satisfy it, and rowids are unique), a rowid table without
// triggers, FK enforcement, virtual-table storage or FTS content storage.
func (e *Engine) pointDMLCannotAbort(stmt sql.Stmt) bool {
	var table string
	var where sql.Expr
	switch s := stmt.(type) {
	case *sql.UpdateStmt:
		table, where = s.Table, s.Where
	case *sql.DeleteStmt:
		table, where = s.Table, s.Where
	default:
		return false
	}
	if table == "" || where == nil {
		return false
	}
	if e.settings.foreignKeys {
		return false // FK enforcement could reject or cascade after other writes
	}
	if e.hasTriggersForTable(table) {
		return false // a trigger could fail after the row write
	}
	if e.stmtTargetsFTSContent(stmt) {
		return false // the in-memory FTS index needs its own snapshot
	}
	entry, _, err := e.findTable(table)
	if err != nil || entry == nil {
		return false
	}
	alias, rowidTable := e.rowidAliasCached(entry)
	if !rowidTable {
		return false // WITHOUT ROWID: a "rowid" name would be an ordinary column
	}
	if util.HasPrefixFoldASCII(entry.SQL, "CREATE VIRTUAL TABLE") {
		return false // module storage: the write path is the module's own
	}
	return exprPinsRowid(where, entry, alias)
}

// rowidAliasCached resolves one table entry's rowid facts — the INTEGER
// PRIMARY KEY alias column name ("" when the table has no alias) and whether
// the table is a rowid table at all — memoized per (entry, schema
// fingerprint), the withoutRowidCached guard pattern: any DDL replaces the
// entry or moves the fingerprint, so a stale verdict cannot survive.
func (e *Engine) rowidAliasCached(entry *schema.Entry) (alias string, rowidTable bool) {
	fp := e.allSchemasFingerprint()
	if e.ptAbortEntry == entry && e.ptAbortFp == fp {
		return e.ptAbortAlias, e.ptAbortRowid
	}
	rowidTable = !execdml.TableIsWithoutRowid(entry.SQL)
	if rowidTable {
		colDefs := e.ParseColumnDefs(entry.Name, entry.SQL)
		if execquery.RowHasRowIDColumn(colDefs) {
			// A declared rowid/_rowid_/oid column shadows the pseudo-column.
			rowidTable = false
		} else {
			for i := range colDefs {
				if execdml.IsIPKRowidAliasCol(colDefs[i]) {
					alias = colDefs[i].Name
					break
				}
			}
		}
	}
	e.ptAbortEntry, e.ptAbortFp, e.ptAbortAlias, e.ptAbortRowid = entry, fp, alias, rowidTable
	return alias, rowidTable
}

// exprPinsRowid reports whether where contains, among its top-level AND
// conjuncts, an equality term with the rowid on one side — either the
// rowid pseudo-column (or a spelling of it no declared column shadows, which
// rowidAliasCached already normalized) or the table's INTEGER PRIMARY KEY
// alias. Such a term bounds the statement to one matching row: every
// candidate row must satisfy it, and rowids are unique.
func exprPinsRowid(where sql.Expr, entry *schema.Entry, alias string) bool {
	switch v := unwrapParenExpr(where).(type) {
	case *sql.BinaryOp:
		if strings.EqualFold(v.Operator, "AND") {
			return exprPinsRowid(v.Left, entry, alias) || exprPinsRowid(v.Right, entry, alias)
		}
		if v.Operator != "=" {
			return false
		}
		return exprIsRowidRef(v.Left, alias) || exprIsRowidRef(v.Right, alias)
	}
	return false
}

// exprIsRowidRef reports whether expr is a bare column reference (a unary +
// wrapper allowed, the planner's +col affinity-elision spelling) naming the
// rowid or the table's IPK alias column.
func exprIsRowidRef(expr sql.Expr, alias string) bool {
	expr = unwrapParenExpr(expr)
	if u, ok := expr.(*sql.UnaryOp); ok && u.Operator == "+" {
		expr = unwrapParenExpr(u.Operand)
	}
	ref, ok := expr.(*sql.ColumnRef)
	if !ok {
		return false
	}
	if alias != "" && util.EqualFoldASCII(ref.Name, alias) {
		return true
	}
	return util.EqualFoldASCII(ref.Name, "rowid") ||
		util.EqualFoldASCII(ref.Name, "_rowid_") ||
		util.EqualFoldASCII(ref.Name, "oid")
}

// unwrapParenExpr peels ParenExpr wrappers (or.go's unwrapParen shape, kept
// local so the exec package's skip gate does not depend on execdml internals).
func unwrapParenExpr(expr sql.Expr) sql.Expr {
	for {
		p, ok := expr.(*sql.ParenExpr)
		if !ok {
			return expr
		}
		expr = p.Expr
	}
}

// isSimpleSingleValuesInsert reports whether the INSERT is the rollback-free
// shape: a single-row VALUES insert with no source SELECT, no RETURNING, no
// REPLACE form, and no multi-row values list.
func isSimpleSingleValuesInsert(ins *sql.InsertStmt) bool {
	return ins.Select == nil && !ins.HasReturning && !ins.IsReplace && len(ins.Values) == 1
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
		_, isFTS5 := e.fts5Tables[s.Table]
		return isFTS || isFTS5
	case *sql.UpdateStmt:
		_, isFTS := e.ftsTables[s.Table]
		_, isFTS5 := e.fts5Tables[s.Table]
		return isFTS || isFTS5
	case *sql.DeleteStmt:
		_, isFTS := e.ftsTables[s.Table]
		_, isFTS5 := e.fts5Tables[s.Table]
		return isFTS || isFTS5
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
	for name := range e.fts5Tables {
		for _, suffix := range []string{"_data", "_idx", "_content", "_docsize", "_config"} {
			if strings.EqualFold(target, name+suffix) {
				return name
			}
		}
	}
	return ""
}

// snapshotAllPagers opens a statement journal scope on every database pager
// (pager.c sub-journal at statement begin), pairing each scope with the pager
// it came from. The scope's before-images are captured lazily — a statement
// that modifies nothing journals nothing — so the per-statement cost is O(1)
// instead of the O(database) deep copy the previous PagerState snapshot took.
// It also snapshots every FTS table's in-memory index so a failed statement
// can undo FTS writes the pager journal does not cover. The entry slice
// recycles through a per-execDepth slot: a statement's list dies at its
// restore (restoreAllPagers / the FTS restores consume the entries within
// the same Exec frame), and a nested statement's snapshot lives on a deeper
// slot, so the reset-on-acquire never clobbers an enclosing scope's list.
func (e *Engine) snapshotAllPagers() []pagerSnap {
	d := e.tx.execDepth
	if d >= len(e.snapBufs) {
		e.snapBufs = append(e.snapBufs, make([][]pagerSnap, d+1-len(e.snapBufs))...)
	}
	snaps := e.snapBufs[d][:0]
	// dbList (ATTACH order) holds the same contexts as the databases map —
	// iterate the slice to keep this per-statement scope open off the map-
	// iteration path (the external-mod probe made the same switch for the
	// same reason).
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager == nil {
			continue
		}
		// The attached-database count is tiny (a linear scan replaces the
		// per-statement dedupe map this used to allocate; two databases
		// sharing one file share one pager — attach-9.2).
		dup := false
		for _, s := range snaps {
			if s.pg == ctx.Pager {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		snaps = append(snaps, pagerSnap{pg: ctx.Pager, journal: ctx.Pager.BeginStatement()})
	}
	// Attach the FTS snapshots to the statement snapshot list via a marker:
	// the pager restore loop ignores entries whose pg is nil, and the FTS
	// restore below runs alongside the pager restore in execRollbackOnError
	// (restoreAllPagers restores only pager entries; the FTS entries are
	// consumed by restoreFTSAll). The empty-registry scans cost a map-
	// iterator setup per statement on FTS-less workloads — skip them.
	if len(e.ftsTables) > 0 {
		e.ftsSnapshots = e.snapshotAllFTS()
	}
	if len(e.fts5Tables) > 0 {
		e.fts5Snapshots = e.snapshotAllFTS5()
	}
	e.snapBufs[d] = snaps
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

// snapshotAllFTS5 captures the in-memory state of every registered fts5 table.
func (e *Engine) snapshotAllFTS5() []fts5Snap {
	var snaps []fts5Snap
	for _, t := range e.fts5Tables {
		if t != nil {
			snaps = append(snaps, fts5Snap{table: t, state: t.Snapshot()})
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
	for _, snap := range e.fts5Snapshots {
		if snap.table != nil && snap.state != nil {
			snap.table.Restore(snap.state)
		}
	}
	e.fts5Snapshots = nil
}

// restoreAllPagers rolls back each pager's statement journal opened by
// snapshotAllPagers (statement failure): only the pages the failing
// statement actually modified are restored. Pairing by pager identity
// (rather than positional index) keeps scopes matched even though
// e.databases is a map with random iteration order.
func (e *Engine) restoreAllPagers(snaps []pagerSnap) {
	if len(snaps) == 0 {
		return
	}
	for _, snap := range snaps {
		if snap.pg != nil && snap.journal != nil {
			snap.pg.RollbackStatement(snap.journal)
		}
	}
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
}

// endStatementScopes closes the statement's journal scopes after a succeeded
// statement (entries splice into any enclosing scope, e.g. a trigger body's
// statement inside an outer DML statement). Scopes already rolled back by a
// failure path are no-ops.
func (e *Engine) endStatementScopes(snaps []pagerSnap) {
	for _, snap := range snaps {
		if snap.pg != nil && snap.journal != nil {
			snap.pg.EndStatement(snap.journal)
		}
	}
}

func (e *Engine) execOtherDDL(stmt sql.Stmt) *Result {
	// A write statement inside an explicit transaction marks the cross-connection
	// write transaction so other connections' lock gates observe it (SQLite's
	// pager acquires RESERVED on the first write). Read-only statements (PRAGMA,
	// EXPLAIN) must NOT mark a write transaction, or a deferred BEGIN followed by
	// PRAGMA lock_status would wrongly block another connection's writes
	// (lock7-1.4).
	e.registerWriteUnlessReadOnly(stmt)
	if err := e.ddlWriteRefused(stmt); err != nil {
		return &Result{Error: err}
	}
	// Invalidate table cache on any DDL operation to ensure consistency
	e.invalidateTableCache()

	switch s := stmt.(type) {
	case *sql.CreateTableStmt, *sql.CreateIndexStmt, *sql.CreateViewStmt, *sql.CreateTriggerStmt, *sql.CreateVirtualTableStmt:
		return e.execCreateStmt(s)
	case *sql.DropTableStmt, *sql.DropIndexStmt, *sql.DropViewStmt, *sql.DropTriggerStmt:
		return e.execDropStmt(s)
	case *sql.AnalyzeStmt:
		// ANALYZE's sqlite_stat1 writes are schema maintenance, not
		// application DML: they do not accumulate into total_changes
		// (e_totalchanges-2.3). REINDEX likewise.
		return e.execAnalyzeInternal(s)
	case *sql.PragmaStmt:
		return e.execPragma(s)
	case *sql.AlterTableStmt:
		return e.ddl.Alter(s)
	case *sql.ExplainStmt:
		return e.execExplain(s)
	case *sql.AttachStmt:
		return e.execAttachOrDetach(s)
	case *sql.ReindexStmt:
		return e.execReindexInternal(s)
	default:
		// Begin, Rollback, Vacuum, Reindex, Savepoint — all no-ops
		return &Result{}
	}
}

// ddlWriteRefused applies the write gates of the DDL/pragma dispatch: PRAGMA
// query_only and a pager opened read-only (permission fallback). PRAGMA
// statements stay exempt so journal_mode can still be observed. ATTACH/DETACH
// are exempt from the read-only pager: the flag applies to the MAIN database
// only, and SQLite permits attaching a separate writable file to a read-only
// connection (misc7-7.3: OpenReadOnly + ATTACH test2.db AS aux).
func (e *Engine) ddlWriteRefused(stmt sql.Stmt) error {
	if _, isPragma := stmt.(*sql.PragmaStmt); isPragma {
		return nil
	}
	// PRAGMA query_only rejects all write statements, including DDL.
	if e.settings.queryOnly {
		return fmt.Errorf("attempt to write a readonly database")
	}
	// A read-only pager rejects DDL writes the same way
	// (sqlite3PagerBegin SQLITE_READONLY).
	if e.mainReadOnly() {
		if _, isAttach := stmt.(*sql.AttachStmt); !isAttach {
			return fmt.Errorf("attempt to write a readonly database")
		}
	}
	return nil
}

// execAnalyzeInternal runs ANALYZE with its sqlite_stat writes marked
// internal (not accumulated into total_changes).
func (e *Engine) execAnalyzeInternal(s *sql.AnalyzeStmt) *Result {
	e.tx.internalWrites++
	res := e.execAnalyze(s)
	e.tx.internalWrites--
	return res
}

// execReindexInternal runs REINDEX with its index rebuilds marked internal
// (not accumulated into total_changes).
func (e *Engine) execReindexInternal(s *sql.ReindexStmt) *Result {
	e.tx.internalWrites++
	res := e.execReindex(s)
	e.tx.internalWrites--
	return res
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
