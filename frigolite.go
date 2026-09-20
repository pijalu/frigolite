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

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
)

// memdbStores is the shared in-memory store registry backing memdb URIs
// (memdb.c: file:/name?vfs=memdb names a process-global MemStore shared by
// every connection opening the same name). Separate (non-"/"-prefixed)
// names get a private store per connection; the memdb2.test names all start
// with "/" so both connections share one store here.
var (
	memdbMu     sync.Mutex
	memdbStores = map[string]*pager.Pager{}
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

	// closedFlag records a completed Close (a test-harness probe such as
	// the quota shutdown's open-connection check must distinguish a
	// closed handle from an open one; Query on a closed DB still works
	// because the engine object survives).
	closedFlag bool

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

// InTransaction reports whether an explicit transaction is open on the
// connection — the negation of sqlite3_get_autocommit's return.
func (db *DB) InTransaction() bool {
	if db == nil || db.engine == nil {
		return false
	}
	return db.engine.InTransaction()
}

// FilePath returns path associated with connection.
func (db *DB) FilePath() string {
	if db == nil {
		return ""
	}
	return db.path
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

// memdbName reports whether path is a memdb VFS URI
// (file:/name?vfs=memdb, memdb.c) and returns the shared-store name.
// Both slashes and backslashes after "file:" start the shared name:
// memdb2.test opens file:/test.db?vfs=memdb and file:\\test.db?vfs=memdb
// (TCL-escaped backslashes) for the same shared store.
func memdbName(path string) (string, bool) {
	if !strings.HasPrefix(path, "file:") || !strings.Contains(path, "vfs=memdb") {
		return "", false
	}
	rest := strings.TrimPrefix(path, "file:")
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.ReplaceAll(rest, "\\", "/")
	rest = strings.ReplaceAll(rest, "//", "/")
	if !strings.HasPrefix(rest, "/") {
		return "", false
	}
	return rest, true
}

// openMemdb opens (or joins) the shared in-memory store for a memdb URI.
// Every connection gets its own Engine over the SAME *pager.Pager, so
// cross-connection locking (memdb2.test's lock-upgrade COMMIT failure)
// and shared content work exactly like a shared file. Closing the last
// connection drops the store (memdbClose frees the MemStore at nRef 0).
func openMemdb(name string) (*DB, error) {
	memdbMu.Lock()
	pg, ok := memdbStores[name]
	if !ok {
		pg = pager.OpenInMemory(pager.DefaultPageSize)
		memdbStores[name] = pg
	}
	memdbRefcounts[name]++
	memdbMu.Unlock()
	return newDBOverPager(pg, "file:"+name+"?vfs=memdb")
}

// memdbRefcounts tracks open connections per shared memdb store so the
// store is dropped when the LAST connection closes (memdb.c memdbClose
// frees the MemStore at nRef 0), not the first.
var memdbRefcounts = map[string]int{}

// releaseMemdb drops a connection's reference to a shared memdb store,
// deleting the store when the last connection closes.

func releaseMemdb(db *DB) {
	if db == nil || db.pager == nil {
		return
	}
	memdbMu.Lock()
	defer memdbMu.Unlock()
	for name, pg := range memdbStores {
		if pg == db.pager {
			memdbRefcounts[name]--
			if memdbRefcounts[name] <= 0 {
				delete(memdbStores, name)
				delete(memdbRefcounts, name)
			}
			return
		}
	}
}

// newDBOverPager builds a connection over an opened pager: it wires the
// engine, records the main file path for file-backed databases and
// initializes the schema manager (the shared construction tail of Open /
// OpenReadOnly / openMemdb).
func newDBOverPager(pg *pager.Pager, path string) (*DB, error) {
	db := &DB{
		pager:  pg,
		engine: exec.NewEngine(pg),
		path:   path,
	}
	if path != "" && path != ":memory:" {
		db.engine.SetMainFilePath(path)
	}
	db.schema = schema.NewManager(pg)
	if err := db.schema.Init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("frigolite: init schema: %w", err)
	}
	return db, nil
}

// Open opens a database file. Use ":memory:" for an in-memory database.
// A SQLite URI filename ("file:path?mode=ro") is reduced to its real path
// ("path") — URI access-mode parameters are a C-API feature the engine does
// not enforce, but the file the URI names is still opened.
// OpenReadOnly opens an existing database file in read-only mode
// (sqlite3_open_v2 with SQLITE_OPEN_READONLY — the TCL harness's
// `sqlite3 db test.db -readonly 1`). Every write fails with SQLITE_READONLY,
// "attempt to write a readonly database". A missing file is an open error.
func OpenReadOnly(path string) (*DB, error) {
	path = normalizeURIPath(path)
	if path != "" && path != ":memory:" {
		path = filepath.Clean(path)
	}
	var pg *pager.Pager
	var err error
	if path == "" || path == ":memory:" {
		pg = pager.OpenInMemoryReadOnly(pager.DefaultPageSize)
	} else {
		pg, err = pager.OpenReadOnly(path, pager.DefaultPageSize)
		if err != nil {
			return nil, fmt.Errorf("frigolite: open: %w", err)
		}
	}
	return newDBOverPager(pg, path)
}

func Open(path string) (*DB, error) {
	// memdb VFS URIs (memdb.c: file:/name?vfs=memdb) name a process-global
	// shared in-memory store, NOT a filesystem path. Route them to the
	// shared registry before any filesystem handling (memdb2.test opens
	// file:/test.db?vfs=memdb and file:\\test.db?vfs=memdb on two
	// connections that must share one store for the lock-upgrade test).
	if name, ok := memdbName(path); ok {
		return openMemdb(name)
	}
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

	db, err := newDBOverPager(pg, path)
	if err != nil {
		return nil, err
	}

	// For a database that was EMPTY at open, pager.c lazy creation applies:
	// do not write the Init-time schema page to disk (opening — even
	// followed by close — must leave a 0-byte file untouched). Drop the
	// dirty flags instead; the in-memory page keeps the connection usable
	// and the first real write flushes it. A non-empty file has no
	// Init-time allocation, but flushing normalizes any leftover state so a
	// second connection's external-modification detection works.
	// Enable external-modification detection for file-based databases so a
	// second connection to the same file observes writes made by the first
	// (SQLite re-reads the schema when another connection commits). In-memory
	// databases have no file to watch.
	if path != "" && path != ":memory:" {
		if pg.OpenedEmpty() {
			pg.MarkClean()
		} else {
			_ = pg.Flush()
		}
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
	// SQLITE_TRACE_CLOSE fires before the connection is torn down
	// (sqlite3_trace_v2; trace3-11.x).
	if db.engine != nil {
		db.engine.FireTraceClose()
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
		err := db.engine.Close()
		db.closedFlag = true
		// Shared memdb stores (file:/name?vfs=memdb) live as long as at
		// least one connection references them (memdbClose frees the
		// MemStore at nRef 0). Track closings here: when the test closes
		// both connections at the end of a loop iteration (memdb2.test's
		// per-tn db/db2 close), the store is dropped so the next iteration
		// starts empty — just like the file-backed suites' forcedelete.
		releaseMemdb(db)
		return err
	}
	db.closedFlag = true
	return nil
}

// IsClosed reports whether Close has completed on this connection.
func (db *DB) IsClosed() bool {
	return db == nil || db.closedFlag
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
