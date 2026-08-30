// Frigolite is a pure-Go SQL database engine compatible with the SQLite file format.
//
// Basic usage:
//
//	db, err := frigolite.Open(":memory:")
//	if err != nil { ... }
//	defer db.Close()
//
//	res := db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)")
//	res = db.Exec("INSERT INTO users VALUES (1, 'Alice')")
//	res = db.Query("SELECT * FROM users")
//	for _, row := range res.Rows {
//	    fmt.Println(row)
//	}
package frigolite

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/vtab"
)

// DB is an open database connection.
type DB struct {
	pager     *pager.Pager
	schema    *schema.Manager
	engine    *exec.Engine
	path      string
	lastRowID int64

	// activeBackups counts Backup objects currently using this connection as
	// source or destination. A non-zero count makes Close fail with
	// SQLITE_BUSY (sqlite3_close returns "unable to close due to unfinalized
	// statements or unfinished backups").
	activeBackups int

	// activeBlobs counts open Blob handles on this connection. A non-zero
	// count makes Close fail with SQLITE_BUSY (sqlite3_close refuses to
	// close a connection with open incremental blob handles).
	activeBlobs int

	stmtMu      sync.Mutex
	activeStmts int
}

// Result holds query results.
type Result struct {
	Columns         []string
	Rows            [][]interface{}
	Changes         int64
	InsertedChanges int64 // rows written as new inserts (excludes upsert DO UPDATE / DO NOTHING)
	Error           error
	LastInsertRowID int64
	SQL             string // The SQL statement that produced this result
}

// FilePath returns path associated with connection.
func (db *DB) FilePath() string {
	if db == nil {
		return ""
	}
	return db.path
}

// LastInsertRowID returns the rowid of the last inserted row.
func (db *DB) LastInsertRowID() int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.LastInsertRowID()
}

// FileDataVersion returns the database file's change counter (the
// SQLITE_FCNTL_DATA_VERSION equivalent). The schema argument defaults to
// "main"; the counter advances on every write commit, including this
// connection's own commits.
func (db *DB) FileDataVersion(schema string) int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.FileDataVersion(schema)
}

// TotalChanges returns the total number of rows changed by INSERT, UPDATE or
// DELETE statements since the connection opened, including changes made by
// trigger bodies and foreign-key actions (sqlite3_total_changes).
func (db *DB) TotalChanges() int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.TotalChanges()
}

// Changes returns the number of rows changed by the most recently completed
// INSERT, UPDATE or DELETE statement, exclusive of changes made by lower-level
// triggers (sqlite3_changes).
func (db *DB) Changes() int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.LastChanges()
}

// ResetChangesCounters zeroes the changes()/total_changes() counters, matching
// a fresh sqlite3 connection.
func (db *DB) ResetChangesCounters() {
	if db != nil && db.engine != nil {
		db.engine.ResetChangesCounters()
	}
}

// TableColumnMetadata reports a table column's declared type, collation,
// NOT NULL, PRIMARY KEY, and AUTOINCREMENT flags (sqlite3_table_column_metadata).
func (db *DB) TableColumnMetadata(schemaName, table, column string) (*exec.ColumnMetadata, error) {
	if db == nil || db.engine == nil {
		return nil, fmt.Errorf("database not open")
	}
	return db.engine.TableColumnMetadata(schemaName, table, column)
}

// SetAuthorizer sets the authorization callback for the database.
// A nil authorizer allows all operations (default behavior).
// The callback is invoked before each database operation to check
// whether it should be allowed.
func (db *DB) SetAuthorizer(a auth.Authorizer) {
	if db != nil && db.engine != nil {
		db.engine.SetAuthorizer(a)
	}
}

// BeginActiveStatement marks the start of a harness-emulated active read
// statement. Upstream, a sqlite3_stmt that has returned SQLITE_ROW stays in
// RUN state until it is finalized, and DROP TABLE/DROP INDEX executed while
// such a statement exists fails with SQLITE_LOCKED "database table is locked"
// (src/vdbe.c OP_Destroy: db->nVdbeRead > db->nVDestroy+1). The Go harness
// materializes query rows, so a db-eval callback loop that executes DDL wraps
// the loop with Begin/EndActiveStatement to reproduce that behavior.
func (db *DB) BeginActiveStatement() {
	if db != nil && db.engine != nil {
		db.engine.BeginActiveStatement()
	}
}

// EndActiveStatement ends a harness-emulated active read statement opened by
// BeginActiveStatement.
func (db *DB) EndActiveStatement() {
	if db != nil && db.engine != nil {
		db.engine.EndActiveStatement()
	}
}

// SetExprDepthLimit sets the maximum view/subquery nesting depth
// (SQLITE_LIMIT_EXPR_DEPTH). A negative value queries (and returns) the
// current limit without changing it.
func (db *DB) SetExprDepthLimit(n int) int {
	if db != nil && db.engine != nil {
		return db.engine.SetExprDepthLimit(n)
	}
	return 0
}

// SetTriggerDepthLimit sets the maximum trigger nesting depth
// (SQLITE_LIMIT_TRIGGER_DEPTH). A negative value queries (and returns) the
// current limit without changing it.
func (db *DB) SetTriggerDepthLimit(n int) int {
	if db != nil && db.engine != nil {
		return db.engine.SetTriggerDepthLimit(n)
	}
	return 0
}

// Limit returns the current value of a named SQLite limit (e.g.
// "SQLITE_LIMIT_ATTACHED"). Unknown limits return 0.
func (db *DB) Limit(name string) int {
	if db != nil && db.engine != nil {
		return db.engine.Limit(name)
	}
	return 0
}

// SetLimit sets a named SQLite runtime limit (SQLITE_LIMIT_COLUMN,
// SQLITE_LIMIT_LENGTH). A negative value queries the current limit without
// changing it. A raise above the compile-time default is capped at the
// default.
func (db *DB) SetLimit(name string, n int) int {
	if db != nil && db.engine != nil {
		return db.engine.SetLimit(name, n)
	}
	return 0
}

// SetProgressHandler registers a progress callback invoked after every n
// engine operations. A true return interrupts the running statement with an
// "interrupted" error (SQLite sqlite3_progress_handler).
func (db *DB) SetProgressHandler(n int, fn func() bool) {
	if db != nil && db.engine != nil {
		db.engine.SetProgressHandler(n, fn)
	}
}

// SetInterruptCount arms SQLite's SQLITE_TEST interrupt countdown
// (::sqlite_interrupt_count, src/vdbe.c:68 + src/test1.c:9316): n > 0
// interrupts the connection after n engine operations (the running statement
// fails with "interrupted"); n <= 0 disables it. The leftover count stays
// readable via InterruptCount after a statement finishes.
func (db *DB) SetInterruptCount(n int) {
	if db != nil && db.engine != nil {
		db.engine.SetInterruptCount(n)
	}
}

// InterruptCount returns the leftover SQLITE_TEST interrupt countdown — the
// value the TCL harness reads from ::sqlite_interrupt_count after a statement
// to learn how many engine operations it consumed.
func (db *DB) InterruptCount() int {
	if db != nil && db.engine != nil {
		return db.engine.InterruptCount()
	}
	return 0
}

// Interrupt sets the connection's interrupt flag (sqlite3_interrupt). The
// next statement executed on this connection fails with an "interrupted"
// error and the flag is consumed.
func (db *DB) Interrupt() {
	if db != nil && db.engine != nil {
		db.engine.Interrupt()
	}
}

// IsInterrupted reports whether the connection's interrupt flag is currently
// set (sqlite3_is_interrupted).
func (db *DB) IsInterrupted() bool {
	if db != nil && db.engine != nil {
		return db.engine.IsInterrupted()
	}
	return false
}

// ClearInterrupt clears the connection's interrupt flag without running a
// statement (used by the test harness after a db-eval callback aborts).
func (db *DB) ClearInterrupt() {
	if db != nil && db.engine != nil {
		db.engine.ClearInterrupt()
	}
}

// DbStatus reports a per-connection status counter (sqlite3_db_status).
// name is a SQLITE_DBSTATUS_* name ("SQLITE_DBSTATUS_CACHE_USED", ...); the
// result is the current value (the highwater mark equals it in this engine's
// deterministic model).
func (db *DB) DbStatus(name string) (current, highwater int64) {
	if db != nil && db.engine != nil {
		return db.engine.DbStatus(name)
	}
	return 0, 0
}

// Status reports a global status counter (sqlite3_status). name is a
// SQLITE_STATUS_* name ("SQLITE_STATUS_MEMORY_USED", ...).
func (db *DB) Status(name string) (current, highwater int64) {
	if db != nil && db.engine != nil {
		return db.engine.Status(name)
	}
	return 0, 0
}

// StmtStatus reports a prepared-statement status counter (sqlite3_stmt_status).
// name is a SQLITE_STMTSTATUS_* name ("SQLITE_STMTSTATUS_VM_STEP", ...).
func (db *DB) StmtStatus(name string) int64 {
	if db != nil && db.engine != nil {
		return db.engine.StmtStatus(name)
	}
	return 0
}

// SetPreupdateHook registers the connection's preupdate hook
// (sqlite3_preupdate_hook). A nil callback clears it. The callback runs after
// every row-level INSERT/UPDATE/DELETE; the current event is available via
// PreupdateCount/PreupdateOld/PreupdateNew.
func (db *DB) SetPreupdateHook(fn func()) {
	if db != nil && db.engine != nil {
		db.engine.SetPreupdateHook(fn)
	}
}

// PreupdateCount returns the number of columns in the current preupdate event
// (sqlite3_preupdate_count).
func (db *DB) PreupdateCount() int {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateCount()
	}
	return 0
}

// PreupdateType returns the operation type of the current preupdate event.
func (db *DB) PreupdateType() string {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateType()
	}
	return ""
}

// PreupdateDB returns the schema name of the current preupdate event.
func (db *DB) PreupdateDB() string {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateDB()
	}
	return ""
}

// PreupdateTable returns the table name of the current preupdate event.
func (db *DB) PreupdateTable() string {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateTable()
	}
	return ""
}

// PreupdateRowID returns the first rowid of the current preupdate event.
func (db *DB) PreupdateRowID() int64 {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateRowID()
	}
	return 0
}

// PreupdateRowID2 returns the second rowid of the current preupdate event.
func (db *DB) PreupdateRowID2() int64 {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateRowID2()
	}
	return 0
}

// PreupdateOld returns the old value of column i in the current preupdate
// event (sqlite3_preupdate_old).
func (db *DB) PreupdateOld(i int) interface{} {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateOld(i)
	}
	return nil
}

// PreupdateNew returns the new value of column i in the current preupdate
// event (sqlite3_preupdate_new).
func (db *DB) PreupdateNew(i int) interface{} {
	if db != nil && db.engine != nil {
		return db.engine.PreupdateNew(i)
	}
	return nil
}

// SetCommitHook registers the connection's commit hook (sqlite3_commit_hook).
// The callback returns 0 to allow the commit or nonzero to abort it with
// "constraint failed". A nil callback clears the hook.
func (db *DB) SetCommitHook(fn func() int) {
	if db != nil && db.engine != nil {
		db.engine.SetCommitHook(fn)
	}
}

// SetAutovacuumPagesCallback registers the per-batch autovacuum-pages callback
// (sqlite3_autovacuum_pages). It fires once before each auto-vacuum commit
// (FULL mode) with (schema, fileSize, nFree, pageSize); the callback returns
// the number of pages to vacuum this batch (clamped to nFree). A nil callback
// clears the registration (default = drain all). P8.INCRVACUUM phase 4.
func (db *DB) SetAutovacuumPagesCallback(fn func(schema string, fileSize, nFree, pageSize uint32) uint32) {
	if db != nil && db.engine != nil {
		db.engine.SetAutovacuumPagesCallback(fn)
	}
}

// SetWalHook registers the connection's WAL hook (sqlite3_wal_hook). The
// callback fires after each WAL-mode commit with (frames appended this
// commit, frames checkpointed). A nil callback clears the hook.
func (db *DB) SetWalHook(fn func(nLog, nCkpt int) int) {
	if db != nil && db.engine != nil {
		db.engine.SetWalHook(fn)
	}
}

// SetJournalFileOpHook installs a callback fired for xOpen/xClose/xDelete
// events on the "<db>-journal" rollback sidecar (testvfs equivalent for
// the journal file). The hook is the narrow observability path through
// which the journal2 TCL test suite verifies the OS-level file-ops
// sequence; frigolite does not have a full VFS plugin system. Pass nil to
// clear. The hook fires synchronously under the pager lock, so the
// callback should be lightweight (e.g. appending to a string).
func (db *DB) SetJournalFileOpHook(fn func(op, path string)) {
	if db != nil && db.engine != nil {
		db.engine.SetJournalFileOpHook(fn)
	}
}

// SetRollbackHook registers the connection's rollback hook
// (sqlite3_rollback_hook). A nil callback clears it.
func (db *DB) SetRollbackHook(fn func()) {
	if db != nil && db.engine != nil {
		db.engine.SetRollbackHook(fn)
	}
}

// SetUpdateHook registers the connection's update hook (sqlite3_update_hook).
// The callback fires for every row-level INSERT/UPDATE/DELETE on a ROWID
// table with the operation, database name, table name, and rowid. A nil
// callback clears the hook.
func (db *DB) SetUpdateHook(fn func(op, dbName, table string, rowid int64)) {
	if db != nil && db.engine != nil {
		db.engine.SetUpdateHook(fn)
	}
}

// SetDQS configures SQLite's double-quoted-string (DQS) behavior.
// ddl=true allows double-quoted strings in DDL statements (CREATE TABLE
// CHECK/DEFAULT expressions, CREATE INDEX keys); dml=true allows them in DML
// (SELECT/INSERT/UPDATE expressions). Both default to true, matching SQLite.
// When disabled, an unresolved double-quoted identifier is an error
// ("no such column: \"X\" - should this be a string literal in single-quotes?").
func (db *DB) SetDQS(ddl, dml bool) {
	if db != nil && db.engine != nil {
		db.engine.SetDQS(ddl, dml)
	}
}

// SetDefensive mirrors SQLITE_DBCONFIG_DEFENSIVE: when enabled, certain
// write operations (e.g. PRAGMA schema_version=...) are ignored.
func (db *DB) SetDefensive(enabled bool) {
	if db != nil && db.engine != nil {
		db.engine.SetDefensive(enabled)
	}
}

// RegisterRtreeGeometry installs a harness-style r-tree geometry callback
// under its SQL function name: "cube" and "circle" from SQLite's
// src/test_rtree.c (the TCL procs register_cube_geom/register_circle_geom).
// The function returns an opaque geometry marker usable only as the right
// operand of `col MATCH name(...)` against rtree-family virtual tables.
func (db *DB) RegisterRtreeGeometry(name string) error {
	if db == nil || db.engine == nil {
		return fmt.Errorf("no database connection")
	}
	switch strings.ToLower(name) {
	case "cube":
		vtab.RegisterRTreeCubeGeometry(db.engine.Database())
	case "circle":
		vtab.RegisterRTreeCircleGeometry(db.engine.Database())
	default:
		return fmt.Errorf("no such geometry: %s", name)
	}
	return nil
}

// RegisterFunction registers a scalar SQL function for this database
// connection. It is used by the test harness to reproduce SQLite's
// TCL-defined test functions (e.g. `db func f f` where f returns a constant).
func (db *DB) RegisterFunction(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int) {
	if db != nil && db.engine != nil {
		db.engine.RegisterFunction(name, fn, minArgs, maxArgs)
	}
}

// UnregisterVTabModulesExcept drops every virtual table module except the
// named ones; later CREATE VIRTUAL TABLE statements on a dropped module fail
// with "no such module: <name>" (SQLite's sqlite3_drop_modules test command;
// fts3dropmod.test).
func (db *DB) UnregisterVTabModulesExcept(keep []string) {
	if db != nil && db.engine != nil {
		db.engine.UnregisterVTabModulesExcept(keep)
	}
}

// RegisterFunctionFlags registers a scalar SQL function with SQLite
// function-safety flags (innocuous / directonly) controlling its use in
// schema objects under PRAGMA trusted_schema.
func (db *DB) RegisterFunctionFlags(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int, innocuous, directOnly bool) {
	if db != nil && db.engine != nil {
		db.engine.RegisterFunctionFlags(name, fn, minArgs, maxArgs, innocuous, directOnly)
	}
}

// RegisterCollation registers a custom collation sequence for this database
// connection (sqlite3_create_collation). The function compares two strings
// and returns -1/0/1. Collation names are case-insensitive; registering a
// name that shadows a built-in (BINARY/NOCASE/RTRIM) replaces the built-in
// for this connection, matching SQLite.
func (db *DB) RegisterCollation(name string, fn func(a, b string) int) {
	if db != nil && db.engine != nil {
		db.engine.RegisterCollation(name, fn)
	}
}

// UnregisterCollation removes a registered custom collation sequence
// (sqlite_delete_collation). It reports whether a collation was removed.
func (db *DB) UnregisterCollation(name string) bool {
	if db != nil && db.engine != nil {
		return db.engine.UnregisterCollation(name)
	}
	return false
}

// LockStyle selects a connection's file-locking model, mirroring SQLite's unix
// VFS locking styles. The default (LockStyleDefault) uses the fine-grained
// SHARED/RESERVED/PENDING/EXCLUSIVE matrix. LockStyleExclusive (unix-flock) and
// LockStyleDotfile (unix-dotfile) collapse every lock level into a single
// EXCLUSIVE mutex that excludes all other connections, and the dotfile style
// additionally maintains a path+".lock" sentinel directory; LockStyleNone
// (unix-none / nolock=1) performs no cross-connection locking at all. Set it
// with DB.SetLockStyle immediately after Open.
type LockStyle int

const (
	// LockStyleDefault is the fine-grained SHARED/RESERVED/PENDING/EXCLUSIVE matrix.
	LockStyleDefault LockStyle = iota
	// LockStyleExclusive collapses every lock level into a single EXCLUSIVE mutex (unix-flock).
	LockStyleExclusive
	// LockStyleDotfile is like LockStyleExclusive but also maintains a path+".lock" sentinel (unix-dotfile).
	LockStyleDotfile
	// LockStyleNone performs no cross-connection locking (unix-none / nolock=1).
	LockStyleNone
)

// SetLockStyle selects this connection's file-locking model (see LockStyle).
func (db *DB) SetLockStyle(style LockStyle) {
	if db != nil && db.engine != nil {
		db.engine.SetLockStyle(int(style))
	}
}

// Open opens a database file. Use ":memory:" for an in-memory database.
// A SQLite URI filename ("file:path?mode=ro") is reduced to its real path
// ("path") — URI access-mode parameters are a C-API feature the engine does
// not enforce, but the file the URI names is still opened.
func Open(path string) (*DB, error) {
	path = normalizeURIPath(path)
	// Canonicalize the filesystem path the way SQLite's unix VFS does in
	// xFullPathname (os_unix.c unixFullPathname): collapse ".", "..",
	// duplicate and trailing slashes lexically BEFORE opening. The canonical
	// form is both the open target and the cross-connection lock key, so two
	// connections spelling the same file differently share lock state
	// (lock3-1.1's messy path opens ./test.db).
	if path != "" && path != ":memory:" {
		path = filepath.Clean(path)
	}
	var pg *pager.Pager
	var err error

	if path == "" || path == ":memory:" {
		pg = pager.OpenInMemory(pager.DefaultPageSize)
	} else {
		pg, err = pager.Open(path, pager.DefaultPageSize)
		if err != nil {
			return nil, fmt.Errorf("frigolite: open: %w", err)
		}
	}

	db := &DB{
		pager:  pg,
		engine: exec.NewEngine(pg),
		path:   path,
	}
	if path != "" && path != ":memory:" {
		db.engine.SetMainFilePath(path)
	}
	db.schema = schema.NewManager(pg)

	// Initialize schema if needed
	if err := db.schema.Init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("frigolite: init schema: %w", err)
	}

	// For a database that was EMPTY at open, pager.c lazy creation applies:
	// do not write the Init-time schema page to disk (opening — even
	// followed by close — must leave a 0-byte file untouched). Drop the
	// dirty flags instead; the in-memory page keeps the connection usable
	// and the first real write flushes it. A non-empty file has no
	// Init-time allocation, but flushing normalizes any leftover state so a
	// second connection's external-modification detection works.
	if path != "" && path != ":memory:" {
		if pg.OpenedEmpty() {
			pg.MarkClean()
		} else {
			_ = pg.Flush()
		}
	}

	// Enable external-modification detection for file-based databases so a
	// second connection to the same file observes writes made by the first
	// (SQLite re-reads the schema when another connection commits). In-memory
	// databases have no file to watch.
	if path != "" && path != ":memory:" {
		db.engine.SetTrackExternalModForMain(true)
	}

	// sqlite_stat1/sqlite_stat4 are created lazily by ANALYZE (execAnalyze),
	// matching SQLite: they only appear in sqlite_master after ANALYZE runs.
	// (Do NOT call InitStatTable here — that would expose the stat tables to
	// `SELECT * FROM sqlite_master` before any ANALYZE.)

	return db, nil
}

// normalizeURIPath reduces a SQLite URI filename to its real filesystem path:
// strips a leading "file:" prefix and any "?query" (or "#fragment")
// suffix. Non-URI paths pass through unchanged.
func normalizeURIPath(path string) string {
	if !strings.HasPrefix(path, "file:") {
		return path
	}
	p := strings.TrimPrefix(path, "file:")
	// file://localhost/path and file:///path forms.
	p = strings.TrimPrefix(p, "//localhost")
	p = strings.TrimPrefix(p, "//")
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return p
}

// Close closes the database.
func (db *DB) Close() error {
	if db == nil {
		return nil
	}
	// An active backup or open blob handle using this connection blocks close
	// (sqlite3_close returns SQLITE_BUSY "unable to close due to unfinalized
	// statements or unfinished backups"). The connection is being torn down:
	// its blob handles no longer hold locks (SQLite abandons them when the
	// connection is replaced, e.g. `db close; sqlite3 db test.db`).
	db.stmtMu.Lock()
	activeStmts := db.activeStmts
	db.stmtMu.Unlock()
	if activeStmts > 0 || db.activeBackups > 0 || db.activeBlobs > 0 {
		err := fmt.Errorf("unable to close due to unfinalized statements or unfinished backups")
		if db.engine != nil {
			db.engine.SetLastErr(err.Error(), "SQLITE_BUSY")
			db.engine.ClearBlobLocks()
		}
		return err
	}
	if db.engine != nil {
		db.engine.ClearBlobLocks()
		return db.engine.Close()
	}
	return nil
}

func (db *DB) registerStmt() {
	db.stmtMu.Lock()
	db.activeStmts++
	db.stmtMu.Unlock()
}

func (db *DB) unregisterStmt() {
	db.stmtMu.Lock()
	if db.activeStmts > 0 {
		db.activeStmts--
	}
	db.stmtMu.Unlock()
}

// DetachAll detaches all attached databases except "main", "temp", and
// "temporary".
func (db *DB) DetachAll() {
	db.engine.DetachAll()
}

// execResult converts an exec.Result to a public Result.
func execResult(er *exec.Result) *Result {
	if er == nil {
		return nil
	}
	return &Result{
		Columns:         er.Columns,
		Rows:            er.Rows,
		Changes:         er.Changes,
		Error:           er.Error,
		LastInsertRowID: er.LastInsertRowID,
	}
}

// Exec executes a SQL statement that does not return rows.
// Multiple statements in the same string are all executed (consistent with
// SQLite's sqlite3_prepare_v2 behavior for DDL/DML batches).
func (db *DB) Exec(sqlStr string) *Result {
	if db == nil || db.engine == nil {
		return &Result{Error: fmt.Errorf("frigolite: database not initialized")}
	}
	stmts, err := db.engine.Prepare(sqlStr)
	if err != nil && len(stmts) == 0 {
		db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
		return &Result{Error: fmt.Errorf("frigolite: parse error: %w", err)}
	}

	var lastResult *exec.Result
	for _, stmt := range stmts {
		res := db.engine.Exec(stmt)
		if res.Error != nil {
			db.engine.SetLastErr(res.Error.Error(), db.errorCode(res.Error))
			return execResult(res)
		}
		lastResult = res
		if strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(sqlStr, ";")), "BEGIN EXCLUSIVE") {
			db.engine.BeginExclusive()
		}
		if res.LastInsertRowID > 0 {
			db.lastRowID = res.LastInsertRowID
		}
	}

	if err != nil {
		// The parseable prefix executed without error; report the trailing
		// syntax error (SQLite reaches it only after the prefix runs).
		db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
		return &Result{Error: fmt.Errorf("frigolite: parse error: %w", err)}
	}

	// A successful statement clears the connection's last-error state
	// (sqlite3_errcode returns SQLITE_OK after the most recent API call
	// succeeds; sqlite3_errmsg returns "not an error").
	db.engine.SetLastErr("", "")

	if lastResult == nil {
		return &Result{}
	}

	result := execResult(lastResult)

	return result
}

// Query executes a SQL query and returns rows.
// Multiple semicolon-separated statements are all executed and their results
// concatenated, matching SQLite's behavior for multi-statement queries.
func (db *DB) Query(sqlStr string) *Result {
	if db == nil || db.engine == nil {
		return &Result{Error: fmt.Errorf("frigolite: database not initialized"), SQL: sqlStr}
	}
	stmts, err := db.engine.Prepare(sqlStr)
	if err != nil && len(stmts) == 0 {
		db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
		return &Result{Error: fmt.Errorf("frigolite: parse error: %w", err), SQL: sqlStr}
	}

	if len(stmts) == 0 {
		db.engine.SetLastErr("", "")
		return &Result{SQL: sqlStr}
	}

	var allRows [][]interface{}
	var allColumns []string
	for _, stmt := range stmts {
		res := db.engine.Exec(stmt)
		if res.Error != nil {
			db.engine.SetLastErr(res.Error.Error(), db.errorCode(res.Error))
			r := execResult(res)
			r.SQL = sqlStr
			return r
		}
		allRows = append(allRows, res.Rows...)
		if allColumns == nil {
			allColumns = res.Columns
		}
		if res.LastInsertRowID > 0 {
			db.lastRowID = res.LastInsertRowID
		}
	}

	db.engine.SetLastErr("", "")

	return &Result{
		Columns: allColumns,
		Rows:    allRows,
		SQL:     sqlStr,
	}
}

// DumpAll logs all schema entries and table contents (debug helper).
func (db *DB) DumpAll() {
	entries, err := db.schema.GetEntries("")
	if err != nil {
		fmt.Printf("dump error: %v\n", err)
		return
	}
	fmt.Printf("=== Schema (%d entries) ===\n", len(entries))
	for _, e := range entries {
		fmt.Printf("  type=%s name=%s tbl_name=%s root=%d\n", e.Type, e.Name, e.TblName, e.RootPage)
	}

	// Dump table contents
	for _, e := range entries {
		if e.Type == schema.TypeTable {
			res := db.Query("SELECT rowid, * FROM " + e.Name)
			if res.Error != nil {
				fmt.Printf("  dump %s: %v\n", e.Name, res.Error)
				continue
			}
			fmt.Printf("\n=== %s (%d rows) ===\n", e.Name, len(res.Rows))
			fmt.Printf("  columns: %v\n", res.Columns)
			for _, row := range res.Rows {
				fmt.Printf("  %v\n", row)
			}
		}
	}
}

// Save persists an in-memory database to a file.
func (db *DB) Save(path string) error {
	if db.pager == nil {
		return fmt.Errorf("frigolite: database not open")
	}
	return db.pager.Flush()
}

// Path returns the database path.
func (db *DB) Path() string {
	return db.path
}

// FileExists checks if a database file exists.
func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ErrorCodeFor returns the SQLITE_* result code string the connection
// would report for err (sqlite3_errcode emulation). Exported for generated
// C-API tests that record an error with SetLastErr but need the proper
// result code (SetLastErr itself does not classify messages).
func (db *DB) ErrorCodeFor(err error) string {
	return db.errorCode(err)
}

// errorCode maps an engine error to the SQLITE_* result code string reported
// by sqlite3_errcode for that failure. The mapping is heuristic, based on the
// error message prefix, matching the SQLite result codes the C-API tests
// assert (capi2/capi3/capi3c). Constraint and DML errors surface as
// SQLITE_ERROR from sqlite3_step (the C API reports the specific extended
// code only from sqlite3_finalize / sqlite3_extended_errcode).
func (db *DB) errorCode(err error) string {
	if err == nil {
		return "SQLITE_OK"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "malformed"):
		// "database disk image is malformed" — SQLite's SQLITE_CORRUPT
		// message (btree.c SQLITE_CORRUPT_BKPT). Checked before the generic
		// clauses so any malformed-database error reports SQLITE_CORRUPT
		// (sqlite3_errcode, incrcorrupt-1.4).
		return "SQLITE_CORRUPT"
	case strings.Contains(msg, "interrupted"):
		return "SQLITE_INTERRUPT"
	case strings.Contains(msg, "database schema has changed"):
		return "SQLITE_SCHEMA"
	case strings.Contains(msg, "database is locked"), strings.Contains(msg, "busy"):
		return "SQLITE_BUSY"
	case strings.Contains(msg, "unable to open database file"), strings.Contains(msg, "no such file"), strings.Contains(msg, "unable to open"):
		return "SQLITE_CANTOPEN"
	case strings.Contains(msg, "misuse"), strings.Contains(msg, "bad parameter"),
		strings.Contains(msg, "no more rows available"):
		// vdbeapi.c: stepping a completed statement without a reset reports
		// SQLITE_MISUSE ("no more rows available" state text).
		return "SQLITE_MISUSE"
	case strings.Contains(msg, "column index out of range"):
		// vdbeapi.c SQLITE_RANGE from sqlite3_bind_* with an index outside
		// 1..sqlite3_bind_parameter_count.
		return "SQLITE_RANGE"
	case strings.Contains(msg, "out of memory"):
		return "SQLITE_NOMEM"
	case strings.Contains(msg, "no such table"), strings.Contains(msg, "no such column"),
		strings.Contains(msg, "syntax error"), strings.Contains(msg, "near "),
		strings.Contains(msg, "constraint"), strings.Contains(msg, "UNIQUE"),
		strings.Contains(msg, "NOT NULL"), strings.Contains(msg, "CHECK"),
		strings.Contains(msg, "FOREIGN KEY"), strings.Contains(msg, "PRIMARY KEY"):
		return "SQLITE_ERROR"
	default:
		return "SQLITE_ERROR"
	}
}
