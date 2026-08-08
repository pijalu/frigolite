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

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
)

// DB is an open database connection.
type DB struct {
	pager     *pager.Pager
	schema    *schema.Manager
	engine    *exec.Engine
	path      string
	lastRowID int64
}

// Result holds query results.
type Result struct {
	Columns         []string
	Rows            [][]interface{}
	Changes         int64
	Error           error
	LastInsertRowID int64
	SQL             string // The SQL statement that produced this result
}

// LastInsertRowID returns the rowid of the last inserted row.
func (db *DB) LastInsertRowID() int64 {
	if db == nil || db.engine == nil {
		return 0
	}
	return db.engine.LastInsertRowID()
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

// SetProgressHandler registers a progress callback invoked after every n
// engine operations. A true return interrupts the running statement with an
// "interrupted" error (SQLite sqlite3_progress_handler).
func (db *DB) SetProgressHandler(n int, fn func() bool) {
	if db != nil && db.engine != nil {
		db.engine.SetProgressHandler(n, fn)
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

// RegisterFunction registers a scalar SQL function for this database
// connection. It is used by the test harness to reproduce SQLite's
// TCL-defined test functions (e.g. `db func f f` where f returns a constant).
func (db *DB) RegisterFunction(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int) {
	if db != nil && db.engine != nil {
		db.engine.RegisterFunction(name, fn, minArgs, maxArgs)
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

// Open opens a database file. Use ":memory:" for an in-memory database.
func Open(path string) (*DB, error) {
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
	db.schema = schema.NewManager(pg)

	// Initialize schema if needed
	if err := db.schema.Init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("frigolite: init schema: %w", err)
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

// Close closes the database.
func (db *DB) Close() error {
	if db.engine != nil {
		return db.engine.Close()
	}
	return nil
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
		return &Result{Error: fmt.Errorf("frigolite: parse error: %w", err)}
	}

	var lastResult *exec.Result
	for _, stmt := range stmts {
		res := db.engine.Exec(stmt)
		if res.Error != nil {
			return execResult(res)
		}
		lastResult = res
		if res.LastInsertRowID > 0 {
			db.lastRowID = res.LastInsertRowID
		}
	}

	if err != nil {
		// The parseable prefix executed without error; report the trailing
		// syntax error (SQLite reaches it only after the prefix runs).
		return &Result{Error: fmt.Errorf("frigolite: parse error: %w", err)}
	}

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
		return &Result{Error: fmt.Errorf("frigolite: parse error: %w", err), SQL: sqlStr}
	}

	if len(stmts) == 0 {
		return &Result{SQL: sqlStr}
	}

	var allRows [][]interface{}
	var allColumns []string
	for _, stmt := range stmts {
		res := db.engine.Exec(stmt)
		if res.Error != nil {
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
