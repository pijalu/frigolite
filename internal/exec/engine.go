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
	"sync"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execconstraint"
	"github.com/pijalu/frigolite/internal/execddl"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execpragma"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/exectrigger"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// rowidCacheKey identifies a table's rowid cache entry by (pager, root page).
// Two databases (main vs attached) can use the same root page number, so the
// pager pointer disambiguates them (SQLite scopes rowid state per Btree).
type rowidCacheKey struct {
	pg   *pager.Pager
	page uint32
}

// rowidCacheKeyFor builds the cache key for a table's root page on pg.
func (e *Engine) rowidCacheKey(pg *pager.Pager, page uint32) rowidCacheKey {
	return rowidCacheKey{pg: pg, page: page}
}

// tableRootKey identifies a tracked table root page: a table's identity is
// its database's pager plus its name (main.t0 and aux.t0 can share the bare
// name but live on different pagers). A nil pager keys name-scoped entries
// such as index root pages, which have no pager at tracking time.
type tableRootKey struct {
	pg   *pager.Pager
	name string
}

// Engine executes SQL statements.
type Engine struct {
	// connID uniquely identifies this connection for cross-connection lock
	// tracking (lockreg).
	connID int64

	// lockStyle selects this connection's file-locking model (mirrors SQLite's
	// unix VFS locking styles): LockStyleDefault uses the fine-grained
	// SHARED/RESERVED/PENDING/EXCLUSIVE matrix; LockStyleExclusive (unix-flock)
	// and LockStyleDotfile (unix-dotfile) collapse every level into a single
	// EXCLUSIVE mutex that excludes all other connections, and the dotfile
	// style additionally maintains a path+".lock" sentinel directory;
	// LockStyleNone (unix-none / nolock=1) performs no cross-connection
	// locking at all.
	lockStyle int

	// dotfileHeld tracks (per file path) whether THIS connection currently
	// holds the dotfile sentinel, so the sentinel directory is created on the
	// first lock and removed on the last unlock (os_unix.c dotlockLock/
	// dotlockUnlock) without churning the global refcount.
	dotfileHeld map[string]bool

	// Authorization
	authorizer auth.Authorizer // authorization callback (nil = allow all)

	// User-registered custom collation sequences (sqlite3_create_collation).
	// Keys are upper-cased collation names; values compare two strings and
	// return -1/0/1. Built-in BINARY/NOCASE/RTRIM are handled by util and are
	// not stored here.
	collations map[string]func(a, b string) int

	// autovacPagesCallback is the optional user callback fired by
	// AutoVacuumCommit before each batch (P8.INCRVACUUM phase 4,
	// sqlite3_autovacuum_pages). Signature:
	//   cb(schema, fileSize, nFree, pageSize) -> nVac
	// nil = drain all (default). Stored on the engine so the testgen
	// binding (which calls a Go function from transpiled test code)
	// can register it via SetAutovacuumPagesCallback.
	autovacPagesCallback func(schema string, fileSize, nFree, pageSize uint32) uint32

	// Multi-database support
	databases map[string]*DatabaseContext // schema_name -> context (upper-cased key)
	dbList    []*DatabaseContext          // attached databases in ATTACH order (main first)
	mainDB    *DatabaseContext            // shortcut for "main"

	// Legacy direct fields pointing to mainDB (kept for backward compat with existing code)
	pager           *pager.Pager
	schema          *schema.Manager
	funcs           *function.Registry
	vtabs           *vtab.Registry
	lastRowID       int64
	lastChanges     int64  // changes made by the last INSERT/UPDATE/DELETE
	totalChanges    int64  // all I/U/D changes since connection open (sqlite3_total_changes)
	encoding        string // database text encoding: "UTF-8", "UTF-16le", "UTF-16be"
	ftsTables       map[string]*fts.FTS3Table
	currentFTSMatch string
	// ftsMatchInfo holds the matchinfo() context for the current FTS SELECT:
	// the parsed MATCH query phrases for the table being selected. It is set
	// by execFTSSelect and cleared when the statement finishes, so
	// matchinfo(TABLE) can compute per-phrase hit statistics
	// (fts3_snippet.c fts3GetMatchinfo).
	ftsMatchInfo ftsMatchInfoCtx
	// overloadProbe enables per-TRUE-invocation of user-registered
	// like()/glob()/regexp() functions while a virtual-table scan whose
	// module opted in (vtab.OperatorOverloadCounter) feeds the current
	// statement. Set when such an instance materializes, cleared at the next
	// top-level statement dispatch.
	overloadProbe bool
	// ftsDeleteDepth tracks in-progress FTS DELETE statements per table so a
	// shadow-table trigger body that re-enters DELETE on the same FTS table is
	// rejected with "SQL logic error" (SQLite's fts3DeleteMethod recursion
	// guard, fts3aa-10.1).
	ftsDeleteDepth map[string]int
	// ftsSnapshots holds the FTS in-memory index copies captured at statement
	// start (snapshotAllPagers) so a failed statement can restore them
	// (restoreAllFTS). The FTS store is in-memory, so the pager restore does
	// not cover it.
	ftsSnapshots []ftsSnap
	// settings groups the PRAGMA/config flags and limits.
	settings engineSettings
	// caches groups the per-table and statement caches.
	caches tableCaches
	// lockingMode tracks this connection's file-locking model as set by
	// PRAGMA locking_mode (default "normal"; "exclusive" holds the EXCLUSIVE
	// lock for the connection's lifetime). It is a connection-level setting.
	lockingMode string
	// tx holds transaction state (BEGIN/COMMIT/ROLLBACK/SAVEPOINT).
	tx txState
	// progress holds the progress-handler state.
	progress progressState
	// interruptCount mirrors SQLite's SQLITE_TEST-only global
	// sqlite3_interrupt_count (src/vdbe.c:68): while positive it is
	// decremented once per engine operation and, when it reaches zero, the
	// connection is interrupted as if sqlite3_interrupt() had been called.
	// The TCL harness links it as ::sqlite_interrupt_count
	// (src/test1.c:9316), so the leftover value stays readable after a
	// statement finishes. Zero disables it.
	interruptCount int
	// interrupted records whether sqlite3_interrupt() was called on this
	// connection. It is set by Interrupt, read by IsInterrupted, and cleared
	// when the next statement begins (Exec) or by ClearInterrupt.
	interrupted bool
	// preupdateHook is the registered sqlite3_preupdate_hook callback; the
	// current event (old/new column values) is held in preupdate.
	preupdateHook func()
	preupdate     execdml.PreupdateEvent
	// commitHook / rollbackHook / updateHook hold the sqlite3_commit_hook,
	// sqlite3_rollback_hook, and sqlite3_update_hook callbacks.
	commitHook   func() int
	rollbackHook func()
	updateHook   func(op, db, table string, rowid int64)
	// walHook holds the sqlite3_wal_hook callback (fires after each WAL
	// commit with (frames appended, frames checkpointed)).
	walHook func(nLog, nCkpt int) int
	// returning holds RETURNING evaluation state.
	returning returningState
	// testState holds the backing state for test-only SQL functions.
	testState testState
	// lastErrMsg / lastErrCode record the last error on this connection for
	// sqlite3_errmsg / sqlite3_errcode emulation.
	lastErrMsg  string
	lastErrCode string
	// pragmas is the PRAGMA statement dispatcher. It owns the pragma handler
	// map and delegates state access back to this Engine via the
	// execpragma.EngineState interface.
	pragmas *execpragma.Registry
	// expr is the expression evaluation engine. It owns the evaluation tree
	// and delegates engine capability access back to this Engine via the
	// execexpr.ExprContext interface.
	expr *execexpr.Evaluator
	// selectEngine executes SELECT statements. It owns the SELECT execution
	// family (join, aggregate, validate, scan, planner, core) and the
	// query-state fields that previously lived on the Engine.
	selectEngine *execquery.SelectEngine
	// dml executes INSERT/UPDATE/DELETE statements. It owns the DML execution
	// family and the DML state fields (currentDMLTable, currentDMLCtx,
	// updateSetColumns) that previously lived on the Engine.
	dml *execdml.DMLExecutor
	// ddl executes CREATE/DROP/ALTER/ATTACH/DETACH statements. It owns the
	// DDL execution family that previously lived on the Engine.
	ddl *execddl.DDLExecutor
	// constraints enforces FOREIGN KEY constraints. It owns the FK enforcement
	// family (ON DELETE/ON UPDATE actions, deferred FK checks, PRAGMA
	// foreign_key_check) that previously lived on the Engine.
	constraints *execconstraint.ConstraintEnforcer
	// triggers owns trigger execution state (nesting depth, depth limit,
	// table chain, NEW/OLD rows, trigger caches) that previously lived on the
	// Engine.
	triggers *exectrigger.Manager
	// blobLocks tracks open incremental-blob handles per schema for PRAGMA
	// lock_status (read-only → shared, read-write → reserved).
	blobLocks map[string]blobLockCounts
	// blobTableLocks counts open incremental-blob handles per table, so DROP
	// TABLE fails with "database table is locked" while a handle is open.
	blobTableLocks map[string]int
	// unionFileDBs caches the open database handles of swarmvtab file sources
	// (unionvtab.c UnionSrc.db), keyed by file path. C keeps the handles for
	// the UnionTab's whole lifetime; keying by path (not by the per-statement
	// cfg pointer) keeps the map stable across the engine's per-statement
	// vtab re-materialization, so the openclose/maxopen LRU stays a
	// table-lifetime invariant. Sources open on first touch
	// (unionOpenDatabaseInner: openclose UDF, then open, then the missing=
	// retry) and close via UnionReleaseFile (LRU) or engine close.
	unionFileDBs map[unionFileKey]*unionFileDBHandle
	// unionVtabInstances caches the bound unionvtab/swarmvtab instance per
	// created table (keyed by lowercased table name), so the module's
	// persistent per-table state — the swarm source handles and maxopen LRU
	// — survives across statements like C's UnionTab. Instances are
	// disconnected (vtab.Disconnecter) on DROP TABLE and engine close.
	unionVtabInstances map[string]vtab.VirtualTable

	// activeExternalReads mirrors sqlite3's db->nVdbeRead for read statements
	// the harness reports as mid-RUN (db-eval callback loops): DROP TABLE /
	// DROP INDEX while any is active fails with SQLITE_LOCKED
	// "database table is locked" (src/vdbe.c OP_Destroy).
	activeReadsMu       sync.Mutex
	activeExternalReads int
}

// engineSettings groups the PRAGMA/config flags and limits that previously
// lived as individual fields on the Engine (SQLite groups these in db->flags).
type engineSettings struct {
	legacyAlterTable       bool             // PRAGMA legacy_alter_table setting
	recursiveTriggers      bool             // PRAGMA recursive_triggers setting (allows trigger re-entry)
	foreignKeys            bool             // PRAGMA foreign_keys setting (enables FK constraint enforcement)
	deferForeignKeys       bool             // PRAGMA defer_foreign_keys: defer all FK checks to COMMIT (reset at COMMIT/ROLLBACK)
	ignoreCheckConstraints bool             // PRAGMA ignore_check_constraints: skip CHECK enforcement (integrity_check still reports)
	writableSchema         bool             // PRAGMA writable_schema setting (permits sqlite_schema edits)
	queryOnly              bool             // PRAGMA query_only: DML statements are rejected
	dqsDDL                 bool             // SQLITE_DBCONFIG_DQS_DDL: allow double-quoted strings in DDL (default true)
	dqsDML                 bool             // SQLITE_DBCONFIG_DQS_DML: allow double-quoted strings in DML (default true)
	reverseUnordered       bool             // PRAGMA reverse_unordered_selects: reverse the scan order of the top-level SELECT when it has no ORDER BY
	caseSensitiveLike      bool             // PRAGMA case_sensitive_like: LIKE comparisons are case-sensitive
	trustedSchema          bool             // PRAGMA trusted_schema: schema objects may reference user functions (default ON)
	shortColumnNames       bool             // PRAGMA short_column_names (default ON: unqualified result column names)
	fullColumnNames        bool             // PRAGMA full_column_names (default OFF: qualify result columns as TABLE.COL)
	countChanges           bool             // PRAGMA count_changes: DML statements return a row with the changed-row count
	cacheSpillEnabled      bool             // PRAGMA cache_spill on/off flag
	defensive              bool             // SQLITE_DBCONFIG_DEFENSIVE: ignore certain writes (e.g. schema_version)
	recursiveCTELimit      int              // PRAGMA recursive_cte_limit setting (default 1000000)
	cacheSpillSize         int              // PRAGMA cache_spill threshold in pages (negative = KiB until read)
	exprDepthLimit         int              // SQLITE_LIMIT_EXPR_DEPTH: max view/subquery nesting depth (default 1000)
	columnLimit            int              // SQLITE_LIMIT_COLUMN: max columns per table/index/view (default 2000)
	lengthLimit            int              // SQLITE_LIMIT_LENGTH: max length of a string/blob value (default 1000000000)
	skipScanEnabled        bool             // PRAGMA skip_scan: toggle skip-scan query optimization (default ON)
	secureDeletes          map[string]int64 // per-schema PRAGMA secure_delete value (inherits MAIN's value on ATTACH)
	mainSecureDelete       int64            // MAIN's per-schema PRAGMA secure_delete value (seeded from defaultSecureDelete on Open)
	defaultSecureDelete    int64            // connection-wide default (the SQLITE_FAST_SECURE_DELETE build option equivalent)
	cacheSizes             map[string]int64
	autoVacuumModes        map[string]int64 // per-schema PRAGMA auto_vacuum mode
	dataVersion            int64
}

// tableCaches groups the per-table and statement caches that previously lived
// as individual fields on the Engine.
type tableCaches struct {
	colCache       map[string][]sql.ColumnDef       // cached column definitions (tableName -> colDefs)
	tcCache        map[string][]sql.TableConstraint // cached table-level constraints
	stmtCache      map[string][]sql.Stmt            // prepared statement cache (sqlText -> parsed stmts)
	tableRootPages map[tableRootKey]uint32          // tracked root pages (updated after splits)
	tableCache     map[string]*cachedTableEntry     // cached table entry lookups
	nextRowIDCache map[rowidCacheKey]int64          // cached next rowid per (pager, root page)
	autoIncSeq     map[rowidCacheKey]int64          // AUTOINCREMENT sequence: largest rowid ever used per (pager, root page)
	templateCache  map[string]*sqlTemplateEntry     // normalized SQL → cached AST template
	uniqueIdxCache map[string][]uniqueIndexDef      // cached unique-index definitions per table name
	viewDefCache   map[string][]sql.ColumnDef       // cached view column definitions (viewName -> colDefs)
}

// txState groups transaction state (BEGIN/COMMIT/ROLLBACK/SAVEPOINT).
type txState struct {
	inTransaction  bool                         // tracks if we're inside a BEGIN/COMMIT block
	ddlBuffer      []func()                     // DDL undo operations for transaction rollback
	txSnapshots    map[string]*pager.PagerState // pager snapshots per database at BEGIN (for ROLLBACK undo)
	txFTSnapshots  []ftsSnap                    // FTS in-memory index snapshots at BEGIN (for ROLLBACK undo)
	savepointStack []savepointEntry             // nested SAVEPOINT stack
	// execDepth counts nested Exec calls (triggers, the eval() extension).
	// rollbackAborted is set when a nested statement runs ROLLBACK that
	// undoes schema changes, which aborts the enclosing statement with "abort
	// due to ROLLBACK" (SQLite's SQLITE_ABORT_ROLLBACK: a statement that was
	// executing when the schema changed out from under it fails).
	execDepth       int
	rollbackAborted bool
	// txSchemaChanged is set when DDL runs inside a transaction; a nested
	// ROLLBACK that undoes schema changes (misc8-1.7's CREATE TABLE inside
	// BEGIN) aborts the enclosing statement.
	txSchemaChanged bool
	// snapActive reports that the outermost statement of the current Exec
	// nest took a statement-rollback pager snapshot. Nested Exec calls
	// (trigger bodies, the FTS segment flush's internal shadow-table writes)
	// reuse that snapshot instead of taking their own — each inner Exec's
	// snapshot is O(pages) and would make per-row FTS builds O(n^2)
	// (fts3_build_db_2 30040: the stat REPLACE snapshots the whole pager on
	// every flush). An inner failure propagates to the outermost statement,
	// whose snapshot restores everything.
	snapActive bool
	// inFTSFlush is set while the FTS segment flush runs its internal
	// %_segdir/%_segments/%_stat shadow-table writes (nested Execs at depth
	// >1). Those writes cannot fail after partially writing in a way the
	// outer statement's rollback does not cover, so they skip the O(pages)
	// pager snapshot entirely — the dominant cost of per-row FTS builds.
	inFTSFlush bool
}

// progressState holds the progress-handler state (db progress N fn).
type progressState struct {
	period   int
	callback func() bool
	counter  int
}

// returningState holds RETURNING evaluation state.
type returningState struct {
	strict bool   // RETURNING eval: unknown columns are errors (SQLite semantics)
	table  string // table name for RETURNING qualified column resolution
}

// testState holds the backing state for test-only SQL functions (counter()
// and nondeter(), SQLite test1.c selectH_counter / having.test). Both reset
// at the start of each statement.
type testState struct {
	counterVal  int64
	nondeterVal int64
}

// Row provides column value lookup for expression evaluation.
// It aliases execquery.Row (which in turn aliases execexpr.Row) so the
// execution engine and the expression evaluation package share the same row
// abstraction without a dependency from evaluation back to the engine.
type Row = execquery.Row

// structRow aliases the query engine's index-based Row implementation.
type structRow = execquery.StructRow

// RowMap implements Row for map-backed row stores.
type RowMap = execquery.RowMap

// collatedValue aliases the expression evaluation package's collation
// wrapper so the execution engine and the evaluator share one type.
type collatedValue = execquery.CollatedValue

// Result aliases the SELECT query engine's result type so the rest of the
// execution engine and the public API share one Result type.
type Result = execquery.Result

// DatabaseContext aliases the SELECT query engine's per-database context.
type DatabaseContext = execquery.DatabaseContext

// uniqueIndexDef aliases the query engine's UNIQUE index description.
type uniqueIndexDef = execquery.UniqueIndexDef

// orBranchPlan aliases the OR-index plan branch.
type orBranchPlan = execquery.OrBranchPlan

// indexPragmaColumn aliases the query engine's PRAGMA index column row.
type indexPragmaColumn = execquery.IndexPragmaColumn

// LastInsertRowID returns the rowid of the last inserted row.
func (e *Engine) LastInsertRowID() int64 {
	return e.lastRowID
}

// SetAuthorizer sets the authorization callback for the engine.
// A nil authorizer allows all operations (default behavior).
func (e *Engine) SetAuthorizer(a auth.Authorizer) {
	e.authorizer = a
}

// SetExprDepthLimit sets the maximum view/subquery nesting depth
// (SQLITE_LIMIT_EXPR_DEPTH). A negative value queries the current limit.
func (e *Engine) SetExprDepthLimit(n int) int {
	if n >= 0 {
		e.settings.exprDepthLimit = n
	}
	return e.settings.exprDepthLimit
}

// SetPendingByteMain overrides the PENDING_BYTE lock-byte offset for the
// main pager. Mirrors the SQLite C test harness
// (sqlite3_test_control_pending_byte from src/test2.c), which lowers the
// byte to 0x10000 so file-size checks in autovacuum-9.3 / 9.5 / corrupt2
// / lock4 etc. observe a small expected value without creating a 1GB
// database. Pass 0 to restore the production default (0x40000000).
func (e *Engine) SetPendingByteMain(byteOffset uint32) {
	if e.mainDB != nil && e.mainDB.Pager != nil {
		e.mainDB.Pager.SetPendingByte(byteOffset)
	}
}

// SetTriggerDepthLimit sets the maximum trigger nesting depth
// (SQLITE_LIMIT_TRIGGER_DEPTH). A negative value queries the current limit.
// The limit is stored on the trigger manager and used by fireTrigger to abort
// recursive trigger chains.
func (e *Engine) SetTriggerDepthLimit(n int) int {
	return e.triggers.SetDepthLimit(n)
}

// SetLimit sets a named SQLite runtime limit (SQLITE_LIMIT_COLUMN,
// SQLITE_LIMIT_LENGTH). A negative value queries the current limit without
// changing it. SQLITE_LIMIT_EXPR_DEPTH / TRIGGER_DEPTH use their dedicated
// setters. A raise above the compile-time default is capped at the default
// (SQLite: "it is not possible to raise the column limit above its default
// compile time value").
func (e *Engine) SetLimit(name string, n int) int {
	if n < 0 {
		return e.Limit(name)
	}
	switch strings.ToUpper(name) {
	case "SQLITE_LIMIT_COLUMN":
		if n > sqliteMaxColumnDefault {
			n = sqliteMaxColumnDefault
		}
		e.settings.columnLimit = n
		return n
	case "SQLITE_LIMIT_LENGTH":
		if n > sqliteMaxLengthDefault {
			n = sqliteMaxLengthDefault
		}
		e.settings.lengthLimit = n
		return n
	}
	return e.Limit(name)
}

// sqliteMaxColumnDefault is the SQLite compile-time default SQLITE_MAX_COLUMN.
const sqliteMaxColumnDefault = 2000

// sqliteMaxLengthDefault is the SQLite compile-time default SQLITE_MAX_LENGTH.
const sqliteMaxLengthDefault = 1000000000

// Limit returns the current value of a named SQLite compile-time/run-time
// limit (e.g. "SQLITE_LIMIT_ATTACHED", "SQLITE_LIMIT_EXPR_DEPTH").
// Unknown limits return 0. Used by the test harness to query the engine's
// configured limits (attach4-1.1 checks SQLITE_LIMIT_ATTACHED).
func (e *Engine) Limit(name string) int {
	switch strings.ToUpper(name) {
	case "SQLITE_LIMIT_ATTACHED":
		return execddl.MaxAttachedDatabases
	case "SQLITE_LIMIT_EXPR_DEPTH":
		return e.settings.exprDepthLimit
	case "SQLITE_LIMIT_TRIGGER_DEPTH":
		return e.triggers.DepthLimit()
	case "SQLITE_LIMIT_COLUMN":
		return e.settings.columnLimit
	case "SQLITE_LIMIT_LENGTH":
		return e.settings.lengthLimit
	default:
		return 0
	}
}

// SetProgressHandler registers a progress callback invoked after every n
// engine operations (n <= 0 disables it). A true return interrupts the
// running statement with an "interrupted" error, matching SQLite's
// sqlite3_progress_handler.
func (e *Engine) SetProgressHandler(n int, fn func() bool) {
	e.progress.period = n
	e.progress.callback = fn
	e.progress.counter = 0
}

// SetInterruptCount arms the SQLITE_TEST interrupt countdown
// (::sqlite_interrupt_count, src/vdbe.c:68): n > 0 interrupts the connection
// after n engine operations; n <= 0 disables it. The engine mirrors the
// counter decrement-per-op of sqlite3VdbeExec's SQLITE_TEST block.
func (e *Engine) SetInterruptCount(n int) {
	e.interruptCount = n
}

// InterruptCount returns the leftover countdown (the TCL harness reads
// ::sqlite_interrupt_count after a statement to learn how many ops ran).
func (e *Engine) InterruptCount() int {
	return e.interruptCount
}

// Interrupt sets the connection's interrupt flag (sqlite3_interrupt). The
// flag is consumed (cleared) by the next statement executed on this
// connection, which fails with an "interrupted" error.
func (e *Engine) Interrupt() {
	e.interrupted = true
}

// IsInterrupted reports whether the interrupt flag is currently set
// (sqlite3_is_interrupted).
func (e *Engine) IsInterrupted() bool {
	return e.interrupted
}

// ClearInterrupt clears the connection's interrupt flag without running a
// statement (used by the TCL harness after a db-eval callback aborts).
func (e *Engine) ClearInterrupt() {
	e.interrupted = false
}

// checkProgress counts engine operations and, every progressPeriod calls,
// runs the registered callback. It also decrements the SQLITE_TEST
// interrupt countdown (src/vdbe.c sqlite3VdbeExec): when the countdown
// reaches zero the connection is interrupted and the running statement
// aborts with SQLITE_INTERRUPT. A nil callback and zero countdown are a
// no-op fast path. Returns a non-nil "interrupted" error when the callback
// requests an abort or the countdown fires.
func (e *Engine) checkProgress() error {
	if e.progress.callback == nil && e.interruptCount <= 0 {
		return nil
	}
	if e.interruptCount > 0 {
		e.interruptCount--
		if e.interruptCount == 0 {
			// sqlite3_interrupt(db): the running statement fails immediately
			// with SQLITE_INTERRUPT and the flag stays set until consumed
			// (vdbeapi.c clears it when the last statement finishes).
			e.interrupted = true
			return fmt.Errorf("interrupted")
		}
	}
	if e.progress.callback == nil || e.progress.period <= 0 {
		return nil
	}
	e.progress.counter++
	if e.progress.counter >= e.progress.period {
		e.progress.counter = 0
		if e.progress.callback() {
			return fmt.Errorf("interrupted")
		}
	}
	return nil
}

// SetDQS configures SQLite's double-quoted-string (DQS) behavior.
// ddl=true allows double-quoted strings in DDL statements (CREATE TABLE
// CHECK/DEFAULT expressions, CREATE INDEX keys); dml=true allows them in DML
// (SELECT/INSERT/UPDATE expressions). Both default to true, matching SQLite.
// When disabled, an unresolved double-quoted identifier is an error
// ("no such column: \"X\" - should this be a string literal in single-quotes?").
func (e *Engine) SetDQS(ddl, dml bool) {
	e.settings.dqsDDL = ddl
	e.settings.dqsDML = dml
}

// SetMainFilePath records the filesystem path of the main database, reported
// by PRAGMA database_list (SQLite reports the path passed to sqlite3_open).
func (e *Engine) SetMainFilePath(path string) {
	if e.mainDB != nil {
		e.mainDB.FilePath = path
	}
}

// SetTrackExternalModForMain enables external-modification detection for the
// main database (a second connection to the same file observes writes made by
// this one). Called by frigolite.Open for file-based databases.
func (e *Engine) SetTrackExternalModForMain(enabled bool) {
	if e.mainDB != nil && e.mainDB.Schema != nil {
		e.mainDB.Schema.SetTrackExternalMod(enabled)
		e.mainDB.Schema.CaptureFileStamp()
	}
}

// SetDefensive mirrors SQLITE_DBCONFIG_DEFENSIVE: when enabled, certain
// write operations (e.g. PRAGMA schema_version=...) are ignored.
func (e *Engine) SetDefensive(enabled bool) {
	e.settings.defensive = enabled
}

// RegisterFunction registers a scalar SQL function for this engine instance.
// It is used by the test harness to reproduce SQLite's TCL-defined functions
// (e.g. `db func f f` where f returns a constant).
func (e *Engine) RegisterFunction(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int) {
	e.funcs.Register(name, fn, minArgs, maxArgs)
}

// RegisterFunctionFlags registers a scalar SQL function with SQLite
// function-safety flags (innocuous / directonly) controlling its use in
// schema objects under PRAGMA trusted_schema.
func (e *Engine) RegisterFunctionFlags(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int, innocuous, directOnly bool) {
	e.funcs.RegisterFlags(name, fn, minArgs, maxArgs, innocuous, directOnly)
}

// SchemaFunctionSafe reports whether a function may be used in a schema
// object under the current trusted_schema setting.
func (e *Engine) SchemaFunctionSafe(name string) bool {
	return e.funcs.SchemaSafe(name, e.settings.trustedSchema)
}

// RegisterCollation registers a custom collation sequence for this engine
// (sqlite3_create_collation). The function compares two strings and returns
// -1/0/1. Collation names are case-insensitive; registering a name that is a
// built-in (BINARY/NOCASE/RTRIM) replaces the built-in for this connection,
// matching SQLite (the user collation shadows the built-in of the same name).
func (e *Engine) RegisterCollation(name string, fn func(a, b string) int) {
	if e == nil || fn == nil {
		return
	}
	if e.collations == nil {
		e.collations = make(map[string]func(a, b string) int)
	}
	e.collations[strings.ToUpper(name)] = fn
}

// UnregisterCollation removes a registered custom collation sequence
// (sqlite_delete_collation). It reports whether a collation was removed.
func (e *Engine) UnregisterCollation(name string) bool {
	if e == nil || e.collations == nil {
		return false
	}
	_, ok := e.collations[strings.ToUpper(name)]
	delete(e.collations, strings.ToUpper(name))
	return ok
}

// lookupCollation returns a registered custom collation function for name
// (case-insensitive), or nil if name is not registered.
func (e *Engine) lookupCollation(name string) func(a, b string) int {
	if e == nil || e.collations == nil {
		return nil
	}
	return e.collations[strings.ToUpper(name)]
}

// compareValuesCollate compares two SQL values with a collation name,
// consulting this engine's registered custom collations in addition to the
// built-in BINARY/NOCASE/RTRIM. An empty or unknown collation falls back to
// BINARY (SQLite's default), matching util.CompareValuesCollate.
func (e *Engine) compareValuesCollate(a, b interface{}, collation string) int {
	return util.CompareValuesCollateFn(a, b, collation, func(name string) (util.CollationFunc, bool) {
		if f := e.lookupCollation(name); f != nil {
			return util.CollationFunc(f), true
		}
		return nil, false
	})
}

// CompareValuesCollate compares two SQL values with a collation name.
// Exported for the expression evaluator's collation comparison.
func (e *Engine) CompareValuesCollate(a, b interface{}, collation string) int {
	return e.compareValuesCollate(a, b, collation)
}

// LookupCollation returns a registered custom collation function for name
// (case-insensitive), or nil if name is not registered. Exported for the
// expression evaluator's COLLATE operator.
func (e *Engine) LookupCollation(name string) func(a, b string) int {
	return e.lookupCollation(name)
}

// authorize checks whether an operation is allowed by the authorizer.
// Returns nil if allowed, or an error with "not authorized" if denied.
// ResultIgnore is treated as OK for non-READ operations.
func (e *Engine) authorize(action auth.Action, arg1, arg2, arg3, arg4 string) error {
	a := e.authorizer
	if a == nil {
		return nil
	}
	result := a.Authorize(action, arg1, arg2, arg3, arg4)
	switch result {
	case auth.ResultOK, auth.ResultIgnore:
		return nil
	case auth.ResultDeny:
		return fmt.Errorf("not authorized")
	default:
		return fmt.Errorf("not authorized")
	}
}

// invalidateTableCache clears the table entry cache. Must be called after
// any DDL operation that modifies the schema (CREATE, DROP, ALTER TABLE/INDEX/VIEW/TRIGGER).
func (e *Engine) invalidateTableCache() {
	e.caches.tableCache = make(map[string]*cachedTableEntry)
	// Column definitions are derived from sqlite_schema SQL; any DDL can
	// change them, so drop the parsed-column cache too.
	e.caches.colCache = make(map[string][]sql.ColumnDef)
}

// rootPage returns the current root page for a table, checking the engine's
// tracked root pages first, then falling back to the schema entry. The name
// resolves through tablePager so the tracked key is pager-qualified.
func (e *Engine) rootPage(tableName string, schemaRoot uint32) uint32 {
	return e.rootPagePg(e.tablePager(tableName), tableName, schemaRoot)
}

// rootPagePg returns the current root page for the named table on the given
// pager, checking tracked root pages first, then falling back to the schema
// root.
func (e *Engine) rootPagePg(pg *pager.Pager, tableName string, schemaRoot uint32) uint32 {
	if tracked, ok := e.caches.tableRootPages[tableRootKey{pg: pg, name: tableName}]; ok {
		return tracked
	}
	return schemaRoot
}

// updateRootPage tracks a root page change after a b-tree split and persists
// it to sqlite_schema so the correct root survives a reopen (the in-memory
// map alone would be lost, and queries would fall back to the stale schema
// rootpage after the map is cleared). The name resolves through tablePager.
func (e *Engine) updateRootPage(tableName string, newRoot uint32) {
	e.updateRootPagePg(e.tablePager(tableName), tableName, newRoot)
}

// updateRootPagePg tracks a root page change for the named table on the given
// pager and persists it to that pager's schema (the database the tree
// actually lives in — a main-first name search would persist an attached
// table's split into main's schema entry).
func (e *Engine) updateRootPagePg(pg *pager.Pager, tableName string, newRoot uint32) {
	e.caches.tableRootPages[tableRootKey{pg: pg, name: tableName}] = newRoot
	for _, ctx := range e.dbList {
		if ctx != nil && ctx.Pager == pg {
			if entry, err := ctx.Schema.FindTable(tableName); err == nil && entry != nil {
				_ = ctx.Schema.UpdateEntryRoot(entry.Name, newRoot)
			}
			return
		}
	}
	// Pager not registered (transient): fall back to a name search.
	if entry, ctx, err := e.findTable(tableName); err == nil && entry != nil {
		_ = ctx.Schema.UpdateEntryRoot(entry.Name, newRoot)
	}
}

// tableBTree creates a BTree for a table, using the engine's tracked root page.
// invalidateTableCaches clears per-table caches that depend on the schema
// (column defs, table constraints, unique-index columns). Called after any
// DDL change (CREATE/DROP/ALTER TABLE, INDEX, TRIGGER) so stale entries from
// a previous incarnation of the same table name are not reused.
// The AUTOINCREMENT sequences are NOT cleared here: SQLite keeps
// sqlite_sequence values across unrelated DDL (CREATE/DROP of OTHER tables,
// CREATE TRIGGER), and the engine's per-table sequence survives via
// ClearRowIDState (called for the specific table being created/dropped).
func (e *Engine) invalidateTableCaches() {
	e.caches.colCache = make(map[string][]sql.ColumnDef)
	e.caches.tcCache = make(map[string][]sql.TableConstraint)
	e.caches.uniqueIdxCache = make(map[string][]uniqueIndexDef)
	e.caches.nextRowIDCache = make(map[rowidCacheKey]int64)
	e.caches.tableCache = make(map[string]*cachedTableEntry)
	e.caches.tableRootPages = make(map[tableRootKey]uint32)
	e.caches.viewDefCache = make(map[string][]sql.ColumnDef)
}

// restorePager restores a pager snapshot and invalidates all schema caches.
// A pager Restore rolls back page 1 (the schema btree), but the schema
// managers' in-memory caches are NOT automatically invalidated — a stale cache
// can describe a schema that no longer matches the restored btree, causing
// "table X already exists" / missing tables. Call this instead of raw
// Pager.Restore everywhere a statement-level rollback happens.
func (e *Engine) restorePager(pg *pager.Pager, snap *pager.PagerState) {
	if pg == nil || snap == nil {
		return
	}
	pg.Restore(snap)
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
}

func (e *Engine) tableBTree(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return btree.NewBTree(e.tablePager(tableName), e.rootPage(tableName, schemaRoot), isTable)
}

// tableBTreeForName resolves the table's owning database context and builds a
// BTree over that context's pager (a table in an ATTACHed database lives on
// the attached pager, not the main pager).
func (e *Engine) tableBTreeForName(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return btree.NewBTree(e.tablePager(tableName), e.rootPage(tableName, schemaRoot), isTable)
}

// tablePager returns the pager that owns the given table: the attached
// database's pager for tables in ATTACHed databases, else the main pager.
func (e *Engine) tablePager(tableName string) *pager.Pager {
	pg := e.pager
	if _, ctx, err := e.findTable(tableName); err == nil && ctx != nil && ctx.Pager != nil {
		pg = ctx.Pager
	}
	return pg
}

// tableBTreePg creates a BTree for a table using a specific pager.
func (e *Engine) tableBTreePg(pg *pager.Pager, tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return btree.NewBTree(pg, e.rootPage(tableName, schemaRoot), isTable)
}

// maxStmtCacheSize limits the prepared statement cache to avoid unbounded
// memory growth when many unique SQL strings are executed (e.g. INSERT with
// fmt.Sprintf). When the limit is reached, the cache is cleared and rebuilt.
const maxStmtCacheSize = 1000

// cachedTableEntry caches the result of a table lookup to avoid repeated
// schema btree scans. The cache is invalidated on DDL operations.
type cachedTableEntry struct {
	entry *schema.Entry
	ctx   *DatabaseContext
}

// sqlTemplateCache stores parsed statement templates keyed by normalized SQL
// (with literal values replaced by placeholders). When a new SQL string matches
// a cached template, the literal values are substituted into a cloned AST,
// avoiding full re-parsing. This primarily helps INSERT and UPDATE statements
// with varying literal values (e.g. fmt.Sprintf-based benchmarks).
type sqlTemplateEntry struct {
	template string     // normalized SQL with ? for literals
	ast      []sql.Stmt // cached AST (with original values)
}

// maxTemplateCacheSize limits the template cache entries.
const maxTemplateCacheSize = 100

// normalizeSQL replaces all numeric and string literals in a SQL string with '?'.
// Returns the normalized string and the extracted literal values.
// This is a fast pre-parse scan — it does NOT use the full parser.
// Only handles simple quoted strings and decimal integers/floats.

// fastParseInt64 parses a non-negative decimal integer string without sign.
// Faster than strconv.ParseInt for the common case of simple digits.

// containsDoubleQuote checks if a string contains SQL escaped quotes (”).

// containsExp checks if a string contains 'e' or 'E' (scientific notation marker).

// cloneStmtsWithValues clones the cached statement list and substitutes new
// literal values. This avoids re-parsing structurally identical SQL.
// Currently handles InsertStmt values; other types are returned as-is.

// Prepare parses and caches a SQL statement. Repeated calls with the same SQL
// string return the cached parsed statements without re-parsing.
// Additionally, structurally identical SQL (same after replacing literal values
// with placeholders) uses a template cache to avoid full re-parsing.

// NewEngine creates a new execution engine.
func NewEngine(pg *pager.Pager) *Engine {
	mainCtx, tempCtx := newEngineContexts(pg)
	e := &Engine{
		databases: map[string]*DatabaseContext{
			"MAIN": mainCtx,
		},
		connID:             lockreg.NewConnID(),
		dbList:             []*DatabaseContext{mainCtx},
		mainDB:             mainCtx,
		pager:              mainCtx.Pager,
		schema:             mainCtx.Schema,
		funcs:              function.NewRegistry(),
		vtabs:              vtab.NewRegistry(),
		collations:         make(map[string]func(a, b string) int),
		encoding:           encodingName(headerTextEncoding(pg)),
		ftsTables:          make(map[string]*fts.FTS3Table),
		ftsDeleteDepth:     make(map[string]int),
		unionFileDBs:       make(map[unionFileKey]*unionFileDBHandle),
		unionVtabInstances: make(map[string]vtab.VirtualTable),
		settings:           newEngineSettings(),
		caches: tableCaches{
			colCache:       make(map[string][]sql.ColumnDef),
			stmtCache:      make(map[string][]sql.Stmt),
			tableRootPages: make(map[tableRootKey]uint32),
			tableCache:     make(map[string]*cachedTableEntry),
			nextRowIDCache: make(map[rowidCacheKey]int64),
			autoIncSeq:     make(map[rowidCacheKey]int64),
			viewDefCache:   make(map[string][]sql.ColumnDef),
		},
		pragmas: execpragma.New(),
	}
	e.expr = execexpr.New(e)
	e.selectEngine = execquery.NewSelectEngine(e)
	e.dml = execdml.NewDMLExecutor(e)
	e.ddl = execddl.NewDDLExecutor(e)
	e.constraints = execconstraint.New(e)
	e.triggers = exectrigger.New()
	if tempCtx != nil {
		e.databases["TEMP"] = tempCtx
		e.databases["TEMPORARY"] = tempCtx
		e.dbList = append(e.dbList, tempCtx)
	}
	e.registerVTabModules()
	e.registerEngineFuncs()
	// P8.INCRVACUUM.phase9 follow-up: restore the auto_vacuum mode from
	// the on-disk header so re-opened databases behave like the SQLite
	// engine (which bakes the mode into header[52:56]/[64:68]). Without
	// this, a re-opened database silently runs in NONE mode even though
	// the file was created with PRAGMA auto_vacuum=1, and pages freed
	// by subsequent DELETEs/DROPs stay on the freelist forever.
	if mode := pg.ReadAutoVacuumFromHeader(); mode != 0 {
		if e.settings.autoVacuumModes == nil {
			e.settings.autoVacuumModes = make(map[string]int64)
		}
		e.settings.autoVacuumModes["main"] = int64(mode)
		pg.SetAutoVacuum(mode > 0)
	}
	return e
}

// newEngineSettings returns the connection defaults (src/main.c openDatabase
// defaults: SQLITE_LIMIT_EXPR_DEPTH, SQLITE_MAX_COLUMN, SQLITE_MAX_LENGTH,
// DQS on, short_column_names on, trusted_schema on, cache_spill on).
func newEngineSettings() engineSettings {
	return engineSettings{
		recursiveCTELimit:   1000000,
		exprDepthLimit:      1000,       // SQLite default SQLITE_LIMIT_EXPR_DEPTH
		columnLimit:         2000,       // SQLite default SQLITE_MAX_COLUMN
		lengthLimit:         1000000000, // SQLite default SQLITE_MAX_LENGTH
		dqsDDL:              true,       // SQLite default: double-quoted strings allowed in DDL
		dqsDML:              true,       // SQLite default: double-quoted strings allowed in DML
		shortColumnNames:    true,       // SQLite default: short_column_names=ON
		fullColumnNames:     false,      // SQLite default: full_column_names=OFF
		trustedSchema:       true,       // SQLite default: trusted_schema=ON
		cacheSpillEnabled:   true,
		skipScanEnabled:     true, // SQLite default: skip-scan optimization ON
		secureDeletes:       make(map[string]int64),
		mainSecureDelete:    2, // SQLITE_FAST_SECURE_DELETE equivalent (test/securedel.test DEFAULT_SECDEL=2)
		defaultSecureDelete: 2, // Mirrors SQLite's SQLITE_FAST_SECURE_DELETE build option
	}
}

// getDB returns the database context for a given schema name.
// Returns nil if the schema is not found.

// UnregisterVTabModulesExcept drops every virtual table module EXCEPT the
// named ones (SQLite's sqlite3_drop_modules test command keeps the named
// modules and removes all others; fts3dropmod.test).
func (e *Engine) UnregisterVTabModulesExcept(keep []string) {
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[strings.ToLower(k)] = true
	}
	for _, name := range e.vtabs.List() {
		if !keepSet[name] {
			e.vtabs.Unregister(name)
		}
	}
}
