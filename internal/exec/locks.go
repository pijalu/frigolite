package exec

import (
	"fmt"
	"os"
	"strings"

	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/pager"
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
	// Shared memdb stores (file:/name?vfs=memdb) are process-global like
	// files: every connection opening the same name shares one pager, so
	// the lock key must be the store identity, NOT per-connection
	// (memdb2.test's COMMIT-upgrade refusal needs both connections on one
	// key). Private :memory: databases stay per-connection.
	if ctx.IsMemory {
		if strings.HasPrefix(ctx.FilePath, "file:") {
			return ctx.FilePath
		}
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

// stmtLockKey returns the registry key of the database file a statement
// actually touches. DML and SELECT resolve their target table through the
// schema (sqlite3_prepare computes the OP_Transaction database from the
// table's master entry, not from the textual qualifier): an unqualified
// "UPDATE t2" where t2 lives in an attached database locks that database's
// file (attach-3.13). When the table cannot be resolved (it does not exist —
// execution will report that) the textual qualifier's key is used, matching
// the previous behavior.
func (e *Engine) stmtLockKey(stmt sql.Stmt, schemaName string, write bool) string {
	var tableName string
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		tableName = s.Table
	case *sql.UpdateStmt:
		tableName = s.Table
	case *sql.DeleteStmt:
		tableName = s.Table
	case *sql.SelectStmt:
		if s.From.Name != "" {
			tableName = s.From.Name
		}
	}
	if tableName != "" {
		if _, ctx, err := e.findTable(tableName); err == nil && ctx != nil {
			return lockKey(ctx, e.connID)
		}
	}
	return e.LockKeyForDB(schemaName)
}

// stmtWALMode reports whether the statement's target database is in WAL
// mode (resolved like stmtLockKey: through the table's schema entry). The
// WAL visibility model differs from the rollback-journal lock matrix:
// readers never block writers and writers never block readers — only the
// WRITER shm byte serializes writers (P7.WAL-G7 slice 3).
func (e *Engine) stmtWALMode(stmt sql.Stmt, schemaName string) bool {
	var tableName string
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		tableName = s.Table
	case *sql.UpdateStmt:
		tableName = s.Table
	case *sql.DeleteStmt:
		tableName = s.Table
	case *sql.SelectStmt:
		if s.From.Name != "" {
			tableName = s.From.Name
		}
	}
	var ctx *DatabaseContext
	if tableName != "" {
		if _, c, err := e.findTable(tableName); err == nil {
			ctx = c
		}
	}
	if ctx == nil {
		if schemaName == "" {
			schemaName = "main"
		}
		ctx = e.GetDB(schemaName)
	}
	return ctx != nil && ctx.Pager != nil && ctx.Pager.WALMode()
}

// walBeginStmtWrite opens the WAL write transaction eagerly for a writing
// statement — P7.WAL-G7 slice 2's sqlite3WalBeginWriteTransaction parity
// (see pager.Pager.WALBeginWrite): the WRITER shm lock is taken BEFORE the
// statement's btree phase reads pages, so the page images it commits build
// on a snapshot the WRITER lock freezes (concurrent writers cannot interleave
// b-tree edits). Non-WAL pagers and read statements no-op. Called from
// execEntry's statement gates; nested statements (trigger bodies) run inside
// the outer statement's already-held WRITER lock, so the pager-side guard
// makes the call a no-op for them.
func (e *Engine) walBeginStmtWrite(stmt sql.Stmt) error {
	// PRAGMA wal_checkpoint takes its WRITER/CKPT locks inside
	// sqlite3WalCheckpoint and reports contention as the result triple's
	// busy flag, not as a statement error (pragma.c PragTyp_WAL_CHECKPOINT).
	if p, ok := stmt.(*sql.PragmaStmt); ok && strings.EqualFold(p.Name, "wal_checkpoint") {
		return nil
	}
	write, schemaName := lockAccessForStmt(stmt)
	if !write {
		return nil
	}
	p, ctx := e.stmtWritePager(stmt, schemaName)
	if p == nil {
		return nil
	}
	changed, err := p.WALBeginWrite()
	if err != nil {
		return err
	}
	// The pager refresh consumed the external-change signal this statement
	// would otherwise see via checkDBFileCtx: apply the full engine-side reset
	// now (schema caches + table/rowid caches — the execRollback pairing of
	// invalidateTableCaches + Schema.InvalidateCache) so table objects, rowid
	// counters and btree pages rebuild on the fresh page images (R4 in
	// plan/goals/P7.WAL-G7.md).
	if changed {
		e.invalidateTableCaches()
		if ctx != nil && ctx.Schema != nil {
			ctx.Schema.InvalidateCache()
		}
	}
	return nil
}

// stmtWritePager resolves the statement's target database pager (and its
// context) for the eager WAL write transaction: the lock key's database when
// resolvable, else the main pager.
func (e *Engine) stmtWritePager(stmt sql.Stmt, schemaName string) (*pager.Pager, *DatabaseContext) {
	key := e.stmtLockKey(stmt, schemaName, true)
	if key == "" {
		return e.pager, nil
	}
	for _, dbc := range e.dbList {
		if dbc != nil && dbc.Pager != nil && lockKey(dbc, e.connID) == key {
			return dbc.Pager, dbc
		}
	}
	return e.pager, nil
}

// walEndStmtRead ends the outermost statement's WAL read snapshot on every
// file-backed database (sqlite3WalEndReadTransaction parity, P7.WAL-G7
// slice 3): an autocommit statement releases its shared read-mark lock at
// statement end so the NEXT statement re-pins a fresh snapshot and observes
// other connections' commits. Inside an explicit transaction the snapshot
// stays pinned until COMMIT/ROLLBACK ends the transaction (repeatable
// reads). Statement failures reach this through the same execDepthLeave
// path, so a failed statement never leaks its mark.
func (e *Engine) walEndStmtRead() {
	if e.tx.inTransaction {
		return
	}
	for _, ctx := range e.dbList {
		if ctx != nil && ctx.Pager != nil {
			ctx.Pager.WALEndRead()
		}
	}
}

// AttachFileLockError reports whether ATTACHing the file at path would be
// blocked by another connection's lock (src/attach.c sqlite3BtreeOpen → the
// new pager's first read takes a SHARED lock via sqlite3PagerSharedLock, which
// os_unix.c unixLock refuses while another connection holds EXCLUSIVE or
// PENDING; SHARED and RESERVED holders do not block a reader). In-memory
// attachments are never file-locked.
func (e *Engine) AttachFileLockError(path string) error {
	if path == "" {
		return nil
	}
	switch e.lockStyle {
	case LockStyleNone:
		return nil
	case LockStyleExclusive, LockStyleDotfile:
		// flock/dotfile collapse every level into one EXCLUSIVE mutex: any
		// other holder blocks the attach.
		if lockreg.Global.ConnLockedByOther(path, e.connID) {
			return fmt.Errorf("database is locked")
		}
		return nil
	default:
		if _, ok := lockreg.Global.ExclusiveLockedByOther(path, e.connID); ok {
			return fmt.Errorf("database is locked")
		}
		// PENDING denies NEW SHARED acquisitions (lock2-1.7 semantics).
		if lockreg.Global.PendingByOther(path, e.connID) {
			return fmt.Errorf("database is locked")
		}
		return nil
	}
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
	key := e.stmtLockKey(stmt, schemaName, write)
	if key == "" {
		return nil
	}
	// locking_mode=EXCLUSIVE: the first access to the database establishes a
	// SHARED lock that is never released between transactions (pager.c keeps
	// the pager lock while lockingMode is EXCLUSIVE), blocking every other
	// connection's EXCLUSIVE upgrade. The addressed database's own mode
	// governs (pragma.c resolves pDb first); an empty schema is MAIN
	// (aDb[0], the pDb fallback when pId2 is empty).
	lockSchema := schemaName
	if lockSchema == "" {
		lockSchema = "MAIN"
	}
	if strings.EqualFold(e.schemaLockingMode(lockSchema), "exclusive") {
		lockreg.Global.SetPersistentShared(key, e.connID, true)
	}
	switch e.lockStyle {
	case LockStyleNone:
		// unix-none / nolock=1: no cross-connection locking at all.
		return nil
	}
	// Busy-handler gate (pager.c sqlite3PagerSetBusyHandler transition
	// table): the handler runs for attempts made OUTSIDE an explicit
	// transaction — the NO_LOCK→SHARED upgrade every autocommit statement
	// performs first (lock-2.3.1 fires the handler) — and never for a
	// connection already holding its own SHARED read transaction, whose
	// failure is the SHARED→RESERVED upgrade (lock-2.3.2 gets SQLITE_BUSY
	// with no callback).
	for count := 0; ; count++ {
		err := e.crossConnLockCheck(stmt, key, schemaName, write)
		if err == nil {
			return nil
		}
		if e.tx.inTransaction || !e.busyRetry(count) {
			return err
		}
	}
}

// crossConnLockCheck performs one pass of the cross-connection lock matrix;
// CrossConnLockError loops it while the busy handler asks for retries
// (pager.c pager_wait_on_lock).
func (e *Engine) crossConnLockCheck(stmt sql.Stmt, key, schemaName string, write bool) error {
	switch e.lockStyle {
	case LockStyleExclusive, LockStyleDotfile:
		// unix-flock / unix-dotfile collapse every lock level into a single
		// EXCLUSIVE mutex (os_unix.c flockLock / dotlockLock): any lock held
		// by another connection excludes all other connections.
		if lockreg.Global.ConnLockedByOther(key, e.connID) {
			return fmt.Errorf("database is locked")
		}
		return nil
	default: // LockStyleDefault — fine-grained SHARED/RESERVED/PENDING/EXCLUSIVE matrix
		return e.crossConnFineGrainedCheck(stmt, key, schemaName, write)
	}
}

// crossConnFineGrainedCheck applies the default lock style's fine-grained
// SHARED/RESERVED/PENDING/EXCLUSIVE matrix for one key.
func (e *Engine) crossConnFineGrainedCheck(stmt sql.Stmt, key, schemaName string, write bool) error {
	walMode := e.stmtWALMode(stmt, schemaName)
	if _, ok := lockreg.Global.ExclusiveLockedByOther(key, e.connID); ok {
		return fmt.Errorf("database is locked")
	}
	// A read transaction holds SHARED on the file (pager.c holds the
	// SHARED lock for the whole read txn): another connection's write
	// must reserve (RESERVED→EXCLUSIVE upgrade blocked by the reader) —
	// attach2-4.4: db2's autocommit INSERT fails while db holds
	// BEGIN + SELECT on the same file. Autocommit writes go through
	// the COMMIT upgrade path, so they are refused up front; writes
	// inside an explicit transaction take RESERVED (allowed) and fail
	// later at COMMIT (attach2-4.10) via commitLockError.
	// WAL mode exempts this rule: readers never block writers (the
	// WRITER shm byte is the only writer serialization — C's wal.c
	// protocol has no reader/writer exclusion).
	if write && !walMode && !e.tx.inTransaction && lockreg.Global.SharedTxByOther(key, e.connID) {
		return fmt.Errorf("database is locked")
	}
	// PENDING blocks only NEW SHARED acquisitions by other connections. A
	// connection that already holds a transaction-level SHARED lock on the file
	// keeps reading (src/os_unix.c unixLock: the PENDING check applies on the
	// SHARED acquire path, not to an already-held SHARED) — lock2-1.6.
	if lockreg.Global.PendingByOther(key, e.connID) && !lockreg.Global.SharedTxByConn(key, e.connID) {
		return fmt.Errorf("database is locked")
	}
	if write {
		return e.crossConnWriteRefused(key)
	}
	return nil
}

// crossConnWriteRefused applies the write-only rules of the fine-grained
// matrix: another connection's write transaction, or its persistent SHARED
// (locking_mode=EXCLUSIVE), blocks this connection's write. The persistent
// SHARED blocks immediately for an autocommit write (the statement cannot
// commit), at COMMIT for a write inside an explicit transaction (RESERVED is
// still acquirable — exclusive.test 2.5 vs 2.6/2.7).
func (e *Engine) crossConnWriteRefused(key string) error {
	if lockreg.Global.WriteTxByOther(key, e.connID) {
		return fmt.Errorf("database is locked")
	}
	if !e.tx.inTransaction && lockreg.Global.PersistentSharedByOther(key, e.connID) {
		return fmt.Errorf("database is locked")
	}
	return nil
}

// clearPersistentShared releases this connection's persistent SHARED marks
// (PRAGMA locking_mode=normal downgrades the pager locks back to normal
// release-at-transaction-end behavior).
func (e *Engine) clearPersistentShared() {
	for _, k := range e.allLockKeys() {
		lockreg.Global.SetPersistentShared(k, e.connID, false)
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
// Only files this transaction DIRTIED participate: a writer upgrades the
// files it holds RESERVED on, not every attached file (attach2-4.12: db2's
// COMMIT upgrades file2 only; db's released main SHARED must not block it).
// Dirty pages (not the DML write-tracker) decide: the tracker misses writes
// that bypass the DML executor paths, while the pager records every write.
func (e *Engine) commitLockError() error {
	// WAL mode has no EXCLUSIVE upgrade at COMMIT: the writer appended its
	// frames under the WRITER shm byte and commits regardless of readers
	// (wal.c — readers/writer exclusion does not exist in the WAL protocol).
	keys, walMode := e.commitDirtyKeys()
	if walMode {
		return nil
	}
	// COMMIT's upgrade is RESERVED→EXCLUSIVE: the busy handler runs between
	// retries (pager.c sqlite3PagerSetBusyHandler transition table).
	for count := 0; ; count++ {
		if !e.commitUpgradeBlocked(keys) {
			return nil
		}
		if !e.busyRetry(count) {
			return fmt.Errorf("database is locked")
		}
	}
}

// commitUpgradeBlocked reports whether any key a COMMIT must upgrade is held
// against this connection: for the unix-flock / unix-dotfile styles (which
// collapse every lock level into a single EXCLUSIVE mutex) ANY other holder
// blocks; the default style is refused by another connection's persistent
// SHARED, transaction-level SHARED (read transaction) or prepared read lock
// (src/pager.c sqlite3PagerSharedLock EXCLUSIVE upgrade refusal).
func (e *Engine) commitUpgradeBlocked(keys []string) bool {
	for _, k := range keys {
		if e.lockStyle == LockStyleExclusive || e.lockStyle == LockStyleDotfile {
			if lockreg.Global.ConnLockedByOther(k, e.connID) {
				return true
			}
			continue
		}
		if lockreg.Global.PersistentSharedByOther(k, e.connID) ||
			lockreg.Global.SharedTxByOther(k, e.connID) ||
			lockreg.Global.ReadTxByOther(k, e.connID) {
			return true
		}
	}
	return false
}

// commitDirtyKeys returns the registry keys a COMMIT must upgrade — the
// files this transaction DIRTIED when any exist (a writer upgrades the files
// it holds RESERVED on, not every attached file — attach2-4.12), else every
// attached file's key — and reports whether any dirtied pager is in WAL
// mode. Dirty pages (not the DML write-tracker) decide: the tracker misses
// writes that bypass the DML executor paths, while the pager records every
// write.
func (e *Engine) commitDirtyKeys() (keys []string, walMode bool) {
	all := e.allLockKeys()
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager == nil || !ctx.Pager.HasDirtyPages() {
			continue
		}
		if k := lockKey(ctx, e.connID); k != "" {
			keys = append(keys, k)
		}
		walMode = walMode || ctx.Pager.WALMode()
	}
	if len(keys) > 0 {
		return keys, walMode
	}
	return all, false
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
	case *sql.PragmaStmt:
		// Only pragma setters that open a write transaction in C take the
		// cross-conn write lock. pragma.c emits OP_Transaction(p2=1) for the
		// header cookies (PragTyp_HEADER_VALUE: schema_version/user_version/
		// application_id, setCookie op list) and for auto_vacuum=1|2 (setMeta6
		// op list); the journal_mode switch drives the pager's exclusive-lock
		// path. All other setters (cache_size, mmap_size, busy_timeout,
		// cache_spill, secure_delete, ...) are connection-local in-memory
		// settings (sqlite3BtreeSetCacheSize & co. never touch the file) and
		// take no lock — pcache-1.5 sets cache_size on db2 while db holds a
		// write transaction. A bare getter (Value == "") reads the in-memory
		// flag and takes no lock. incrvacuum-12.2 expects "database is
		// locked" when PRAGMA auto_vacuum=2 is issued while another
		// connection holds BEGIN EXCLUSIVE on the same file.
		if s.Value == "" {
			return false, ""
		}
		switch strings.ToLower(s.Name) {
		case "schema_version", "user_version", "application_id", "auto_vacuum":
			schema := s.Schema
			if schema == "" {
				schema = "main"
			}
			return true, schema
		case "journal_mode":
			// pager.c sqlite3PagerSetJournalMode: a rollback↔rollback mode
			// change takes NO file lock at all — the pager records the mode
			// and defers any journal-file disposition (tkt-fc62af4523:
			// persist→delete must succeed while another connection's hot
			// journal is outstanding). Only WAL-involving changes drive the
			// exclusive-lock path, enforced inside Engine.JournalMode
			// (journalModeChangeLockError), which knows both the old and new
			// modes — this classifier does not.
			return false, ""
		}
		return false, ""
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
