package exec

import (
	"fmt"
	"os"
	"strings"

	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/sql"
)

// Lock-style constants mirror SQLite's unix VFS locking styles. They are
// exported through the public frigolite.LockStyle type, which aliases these
// int values so the engine can store the style as a plain int.
const (
	// LockStyleDefault is the fine-grained SHARED/RESERVED/PENDING/EXCLUSIVE
	// matrix (the default unix VFS).
	LockStyleDefault = iota
	// LockStyleExclusive collapses every lock level into a single EXCLUSIVE
	// mutex that excludes all other connections (unix-flock).
	LockStyleExclusive
	// LockStyleDotfile is like LockStyleExclusive but also maintains a
	// path+".lock" sentinel directory (unix-dotfile).
	LockStyleDotfile
	// LockStyleNone performs no cross-connection locking (unix-none / nolock=1).
	LockStyleNone
)

// SetLockStyle selects this connection's file-locking model.
func (e *Engine) SetLockStyle(style int) {
	e.lockStyle = style
}

// ConnID returns this connection's unique ID for cross-connection lock
// tracking.
func (e *Engine) ConnID() int64 {
	return e.connID
}

// WriteTxOpen reports whether this connection currently has an open write
// transaction (BEGIN ... with a write). Backup steps on the source return
// SQLITE_BUSY while a write transaction is open.
func (e *Engine) WriteTxOpen() bool {
	return e.tx.inTransaction && e.hasWriteInTx()
}

// WriteTxOpenOn reports whether named schema has dirty pages in this transaction.
func (e *Engine) WriteTxOpenOn(name string) bool {
	if !e.tx.inTransaction {
		return false
	}
	ctx := e.GetDB(name)
	return ctx != nil && ctx.Pager != nil && ctx.Pager.HasDirtyPages()
}

// DestSchemaInUse reports active read state on destination schema.
func (e *Engine) DestSchemaInUse(name string) bool {
	ctx := e.GetDB(name)
	if ctx == nil {
		return false
	}
	if e.tx.inTransaction {
		return true
	}
	key := lockKey(ctx, e.connID)
	return key != "" && lockreg.Global.ReadTxByConn(key, e.connID)
}

// hasWriteInTx reports whether any database pager has dirty pages (a write
// happened since BEGIN). A read-only transaction (BEGIN; SELECT) does not
// block a backup.
func (e *Engine) hasWriteInTx() bool {
	for _, ctx := range e.dbList {
		if ctx != nil && ctx.Pager != nil && ctx.Pager.HasDirtyPages() {
			return true
		}
	}
	return false
}

// lockKey returns the registry key for a database context's file. In-memory
// databases have no file path; key them by connection + schema name so
// different connections' memory databases never collide.
func lockKey(ctx *DatabaseContext, connID int64) string {
	if ctx == nil {
		return ""
	}
	if ctx.IsMemory {
		return fmt.Sprintf("mem:%d:%s", connID, ctx.Name)
	}
	return ctx.FilePath
}

// allLockKeys returns the registry keys for every database attached to the
// engine (main, temp, and attached).
func (e *Engine) allLockKeys() []string {
	seen := make(map[string]bool)
	var keys []string
	for _, ctx := range e.dbList {
		k := lockKey(ctx, e.connID)
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// registerWriteTx marks every database file of this connection as having an
// open write transaction (or clears the mark). Called on the first write
// inside a transaction and on COMMIT/ROLLBACK.
func (e *Engine) registerWriteTx(on bool) {
	for _, k := range e.allLockKeys() {
		lockreg.Global.SetWriteTx(k, e.connID, on)
	}
	e.syncDotfileSentinel()
}

// BeginExclusive marks every database file of this connection as exclusively
// locked (BEGIN EXCLUSIVE). Other connections' backup steps on those files
// return SQLITE_BUSY until COMMIT/ROLLBACK clears the mark.
func (e *Engine) BeginExclusive() {
	for _, k := range e.allLockKeys() {
		lockreg.Global.SetExclusive(k, e.connID, true)
	}
	e.syncDotfileSentinel()
}

// ReleaseExclusive clears an exclusive-lock mark set by BeginExclusive
// (COMMIT/ROLLBACK of the transaction).
func (e *Engine) ReleaseExclusive() {
	for _, k := range e.allLockKeys() {
		lockreg.Global.SetExclusive(k, e.connID, false)
	}
	e.syncDotfileSentinel()
}

// LockKeyForDB returns the registry key for the named schema's file (used by
// the backup step to check busy state for the source/destination databases).
// The schema name is case-insensitive.
func (e *Engine) LockKeyForDB(name string) string {
	ctx := e.GetDB(name)
	return lockKey(ctx, e.connID)
}

// ReadLockedByOther reports whether another connection has an active prepared
// SELECT holding a read lock on the named database.
func (e *Engine) ReadLockedByOther(name string) bool {
	key := e.LockKeyForDB(name)
	return key != "" && lockreg.Global.ReadTxByOther(key, e.connID)
}

// SetPreparedReadLock records or clears this connection's prepared SELECT lock.
func (e *Engine) SetPreparedReadLock(name string, on bool) {
	key := e.LockKeyForDB(name)
	if key != "" {
		lockreg.Global.SetReadTx(key, e.connID, on)
	}
}

// WriteBlockedByPreparedRead reports whether stmt is a write blocked by a
// prepared SELECT on another connection.
func (e *Engine) WriteBlockedByPreparedRead(stmt sql.Stmt) bool {
	switch stmt.(type) {
	case *sql.InsertStmt, *sql.UpdateStmt, *sql.DeleteStmt:
		return e.ReadLockedByOther("main")
	default:
		return false
	}
}

// ReleaseAllLocks clears every lock mark (write transaction, exclusive,
// prepared read) this connection holds on any file. SQLite releases all of a
// connection's file locks when the connection closes (os_unix.c: close(2)
// drops the process's POSIX locks on the file); without this, a connection
// closed mid-transaction would leave stale marks that block later connections
// opening the same path (savepoint7-3.x db.Close/reopen loop).
func (e *Engine) ReleaseAllLocks() {
	lockreg.Global.ClearConn(e.connID)
	e.syncDotfileSentinel()
}

// syncDotfileSentinel maintains the path+".lock" sentinel directory for the
// unix-dotfile VFS locking style: the sentinel exists iff this connection
// currently holds any lock on the file (os_unix.c dotlockLock creates the lock
// directory on the first lock; dotlockUnlock removes it on the last unlock).
// Other locking styles (default/flock/none) do not use a sentinel.
func (e *Engine) syncDotfileSentinel() {
	if e.lockStyle != LockStyleDotfile {
		return
	}
	for _, k := range e.allLockKeys() {
		if strings.HasPrefix(k, "mem:") {
			continue
		}
		e.syncDotfileSentinelForKey(k)
	}
}

// syncDotfileSentinelForKey reconciles the dotfile sentinel for a single file:
// it creates the sentinel directory when this connection transitions from
// not-holding to holding a lock, and removes it on the reverse transition
// (unless another dotfile connection still holds the file).
func (e *Engine) syncDotfileSentinelForKey(k string) {
	held := lockreg.Global.ConnHoldsLock(k, e.connID)
	if held == e.dotfileHeld[k] {
		return
	}
	if held {
		lockreg.Global.SetDotfileHeld(k, e.connID, true)
		createDotfileSentinel(k)
		if e.dotfileHeld == nil {
			e.dotfileHeld = make(map[string]bool)
		}
		e.dotfileHeld[k] = true
		return
	}
	if !lockreg.Global.SetDotfileHeld(k, e.connID, false) {
		removeDotfileSentinel(k)
	}
	delete(e.dotfileHeld, k)
}

// createDotfileSentinel creates the unix-dotfile lock directory (path+".lock"),
// mirroring SQLite's dotlockLock osMkdir. A pre-existing sentinel (created by
// another connection or by the test harness) is tolerated.
func createDotfileSentinel(path string) {
	_ = os.Mkdir(path+".lock", 0o777)
}

// removeDotfileSentinel removes the unix-dotfile lock directory, mirroring
// SQLite's dotlockUnlock osRmdir.
func removeDotfileSentinel(path string) {
	_ = os.Remove(path + ".lock")
}

// CrossConnLockError implements the cross-connection pager lock matrix
// (src/pager.c sqlite3PagerSharedLock lock upgrades + os_unix.c unixFileLock):
// a RESERVED lock (BEGIN IMMEDIATE or an open write transaction) blocks other
// connections' writes; an EXCLUSIVE lock (BEGIN EXCLUSIVE) blocks other
// connections' reads AND writes. Returns a "database is locked" error when
// stmt touches a file locked by another connection, nil otherwise.
func (e *Engine) CrossConnLockError(stmt sql.Stmt) error {
	write, schemaName := lockAccessForStmt(stmt)
	if schemaName == "" && !write {
		return nil // statement class participates in no file lock
	}
	key := e.LockKeyForDB(schemaName)
	if key == "" {
		return nil
	}
	switch e.lockStyle {
	case LockStyleNone:
		// unix-none / nolock=1: no cross-connection locking at all.
		return nil
	case LockStyleExclusive, LockStyleDotfile:
		// unix-flock / unix-dotfile collapse every lock level into a single
		// EXCLUSIVE mutex (os_unix.c flockLock / dotlockLock): any lock held
		// by another connection excludes all other connections.
		if lockreg.Global.ConnLockedByOther(key, e.connID) {
			return fmt.Errorf("database is locked")
		}
		return nil
	default: // LockStyleDefault — fine-grained SHARED/RESERVED/PENDING/EXCLUSIVE matrix
		if _, ok := lockreg.Global.ExclusiveLockedByOther(key, e.connID); ok {
			return fmt.Errorf("database is locked")
		}
		// PENDING blocks only NEW SHARED acquisitions by other connections. A
		// connection that already holds a transaction-level SHARED lock on the file
		// keeps reading (src/os_unix.c unixLock: the PENDING check applies on the
		// SHARED acquire path, not to an already-held SHARED) — lock2-1.6.
		if lockreg.Global.PendingByOther(key, e.connID) && !lockreg.Global.SharedTxByConn(key, e.connID) {
			return fmt.Errorf("database is locked")
		}
		if write && lockreg.Global.WriteTxByOther(key, e.connID) {
			return fmt.Errorf("database is locked")
		}
		return nil
	}
}

// registerSharedTx records a transaction-level SHARED lock on the named schema's
// file (held until COMMIT/ROLLBACK). SQLite's pager holds SHARED for the whole
// read transaction (sqlite3PagerSharedLock), so a read inside BEGIN ... COMMIT
// blocks another connection's COMMIT (which must upgrade to EXCLUSIVE) — lock2
// and the shared-cache read lock. Only called for reads inside an open
// transaction; an auto-commit read releases its lock at statement end and takes
// no cross-connection mark (lock2-1.1).
func (e *Engine) registerSharedTx(schemaName string) {
	if !e.tx.inTransaction {
		return
	}
	key := e.LockKeyForDB(schemaName)
	if key != "" {
		lockreg.Global.SetSharedTx(key, e.connID, true)
	}
	e.syncDotfileSentinel()
}

// releaseSharedTx clears this connection's transaction-level SHARED lock and
// PENDING mark on every database file it has open (called on COMMIT success and
// ROLLBACK so the locks do not outlive the transaction).
func (e *Engine) releaseSharedTx() {
	for _, k := range e.allLockKeys() {
		lockreg.Global.SetSharedTx(k, e.connID, false)
		lockreg.Global.SetPending(k, e.connID, false)
	}
	e.syncDotfileSentinel()
}

// setPendingAll marks every database file of this connection as PENDING (a
// writer whose COMMIT could not get EXCLUSIVE). New SHARED acquisitions by
// other connections are denied until the holder releases — lock2-1.5/1.7.
func (e *Engine) setPendingAll() {
	for _, k := range e.allLockKeys() {
		lockreg.Global.SetPending(k, e.connID, true)
	}
}

// commitLockError reports whether this connection's COMMIT would be blocked by
// another connection's SHARED (read transaction) or prepared read lock: the
// writer must upgrade to EXCLUSIVE but another reader holds the file. Matches
// src/pager.c sqlite3PagerSharedLock EXCLUSIVE upgrade refusal. For the
// unix-flock / unix-dotfile locking styles (which collapse every lock level
// into a single EXCLUSIVE mutex) ANY other holder blocks the upgrade.
func (e *Engine) commitLockError() error {
	for _, k := range e.allLockKeys() {
		if e.lockStyle == LockStyleExclusive || e.lockStyle == LockStyleDotfile {
			if lockreg.Global.ConnLockedByOther(k, e.connID) {
				return fmt.Errorf("database is locked")
			}
			continue
		}
		if lockreg.Global.SharedTxByOther(k, e.connID) {
			return fmt.Errorf("database is locked")
		}
		if lockreg.Global.ReadTxByOther(k, e.connID) {
			return fmt.Errorf("database is locked")
		}
	}
	return nil
}

// lockAccessForStmt classifies a statement's file access: write=true for
// DML/DDL (RESERVED-or-higher needed), write=false for SELECT (SHARED).
// schemaName is the statement's target schema ("main" when unqualified).
// An empty schemaName with write=false marks statements that take no file
// lock (transactions, pragmas, etc.).
func lockAccessForStmt(stmt sql.Stmt) (write bool, schemaName string) {
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		return true, stmtSchema(s.Table)
	case *sql.UpdateStmt:
		return true, stmtSchema(s.Table)
	case *sql.DeleteStmt:
		return true, stmtSchema(s.Table)
	case *sql.SelectStmt:
		return false, selectSchema(s)
	case *sql.CreateTableStmt:
		return true, stmtSchema(s.Name)
	case *sql.DropTableStmt:
		return true, stmtSchema(s.Name)
	default:
		return false, ""
	}
}

// stmtSchema returns the schema qualifier of a possibly qualified table name
// ("main" when unqualified).
func stmtSchema(table string) string {
	schemaName, _ := parseSchemaName(table)
	if schemaName == "" {
		return "main"
	}
	return schemaName
}

// selectSchema returns the schema of a SELECT's first FROM table ("main" when
// unqualified or when the SELECT has no FROM clause — SQLite still takes the
// SHARED lock on main for schema access in the general case; a FROM-less
// SELECT reads no file and is left ungated by returning "main" only when a
// FROM table exists).
func selectSchema(s *sql.SelectStmt) string {
	if s == nil {
		return ""
	}
	if s.From.Name != "" {
		return stmtSchema(s.From.Name)
	}
	if !s.ValuesChain && len(s.CTEs) == 0 {
		// FROM-less SELECT (e.g. SELECT 1) reads no database file.
		return ""
	}
	return "main"
}

// BackupLocked reports whether an active backup has locked the named schema's
// file (blocking DETACH of that database).
func (e *Engine) BackupLocked(name string) bool {
	ctx := e.GetDB(name)
	if ctx == nil {
		return false
	}
	k := lockKey(ctx, e.connID)
	return k != "" && lockreg.Global.HasBackupLock(k)
}

// AddBackupLock registers an active backup whose destination is the named
// schema's file (blocks DETACH until RemoveBackupLock).
func (e *Engine) AddBackupLock(name string) {
	ctx := e.GetDB(name)
	if ctx == nil {
		return
	}
	if k := lockKey(ctx, e.connID); k != "" {
		lockreg.Global.AddBackupLock(k)
	}
}

// RemoveBackupLock unregisters an active backup lock on the named schema's
// file.
func (e *Engine) RemoveBackupLock(name string) {
	ctx := e.GetDB(name)
	if ctx == nil {
		return
	}
	if k := lockKey(ctx, e.connID); k != "" {
		lockreg.Global.RemoveBackupLock(k)
	}
}

// SetLastErr records the last error message and code on this connection (for
// sqlite3_errmsg / sqlite3_errcode emulation).
func (e *Engine) SetLastErr(msg, code string) {
	e.lastErrMsg = msg
	e.lastErrCode = code
}

// LastErr returns the last error message recorded on this connection.
func (e *Engine) LastErr() string {
	return e.lastErrMsg
}

// LastErrCode returns the last error code recorded on this connection
// (e.g. "SQLITE_ERROR").
func (e *Engine) LastErrCode() string {
	if e.lastErrCode == "" {
		return "SQLITE_OK"
	}
	return e.lastErrCode
}
