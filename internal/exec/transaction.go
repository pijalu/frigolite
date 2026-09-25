// Package exec implements query execution.
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/sql"
)

// --- COMMIT ---

func (e *Engine) execCommit() *Result {
	// SQLite raises "cannot commit - no transaction is active" when COMMIT
	// runs with no open transaction — e.g. after a constraint-aborted
	// statement rolled the transaction back (src/vdbe.c OP_Transaction /
	// sqlite3VdbeExec's OP_AutoCommit path: "cannot commit - no transaction
	// is active").
	if !e.tx.inTransaction {
		return &Result{Error: fmt.Errorf("cannot commit - no transaction is active")}
	}
	// The SQLITE_TEST interrupt countdown (vdbe.c's per-opcode decrement)
	// fires within the COMMIT program: an interrupted COMMIT never commits.
	// SQLITE_INTERRUPT is a special error (src/vdbeaux.c:3358-3383), so the
	// abort path rolls the whole transaction back — a later bare COMMIT
	// fails with "cannot commit - no transaction is active" (interrupt-3.x).
	if res, ok := e.commitInterruptCountdown(); !ok {
		return res
	}
	// Deferred foreign key constraints are checked at COMMIT. On a violation
	// the COMMIT fails and the transaction stays open (SQLite semantics:
	// "cannot start a transaction within a transaction" after a failed
	// COMMIT), so inTransaction/txSnapshots are NOT cleared.
	if res := e.commitCheckDeferredFK(); res != nil {
		return res
	}
	// Cross-connection COMMIT gate: a writer must upgrade to EXCLUSIVE, which
	// is blocked by another connection's SHARED (read transaction) or prepared
	// read lock. The transaction STAYS OPEN and the pager sits in PENDING
	// (lock2 1.5/1.7): inTransaction is NOT cleared and the writer keeps its
	// RESERVED lock so a later COMMIT can retry once the reader releases.
	if res := e.commitLockGate(); res != nil {
		return res
	}
	// fts5 secure-delete format upgrade: the deletes that requested it flush
	// at COMMIT (fts5_main.c xCommit → sqlite3Fts5StorageStorageSync →
	// fts5IndexFlush → fts5FlushSecureDelete's REPLACE 'version'=5), inside
	// the committing transaction.
	if err := e.fts5ApplySecureUpgrades(); err != nil {
		return &Result{Error: err}
	}
	// Commit hook: vdbeCommit invokes db->xCommitCallback BEFORE the btree
	// commit phases and before any transaction teardown (src/vdbeaux.c:2978-
	// 2982) — the hook observes the transaction's uncommitted changes. A
	// nonzero return aborts the COMMIT with SQLITE_CONSTRAINT_COMMITHOOK
	// ("constraint failed") and — via the sqlite3VdbeHalt abort path — rolls
	// the whole transaction back (execRollback needs the still-open
	// transaction state, so this check precedes commitClearTxState).
	if e.commitHook != nil && e.runCommitHook() {
		e.execRollback()
		return &Result{Error: fmt.Errorf("constraint failed")}
	}
	e.commitClearTxState()
	if res := e.flushFTSSegmentsGuarded(); res != nil {
		return res
	}
	// COMMIT ends all open savepoints (SQLite: committing a transaction
	// releases every savepoint it contains; a later ROLLBACK TO or RELEASE
	// of a pre-COMMIT savepoint fails).
	e.tx.savepointStack = nil
	e.commitReleaseLocksAndCounters()
	// Auto-vacuum commit (P8.INCRVACUUM phase 4, btree.c autoVacuumCommit
	// ~line 4174): for FULL mode, drain the on-disk freelist BEFORE writing
	// the commit marker, honoring the optional per-batch callback
	// registered via SetAutovacuumPagesCallback. The callback fires once
	// with (schema, nFilePages, nFreePages, pageSize) and returns the
	// pages-to-vacuum this batch (clamped to nFreePages). The vacuum
	// steps shrink the file (truncate or relocate+truncate) so the commit
	// itself writes the already-shrunken file. INCREMENTAL mode skips this
	// (incremental_vacuum is the user-driven path).
	//
	// The temp database also has a pager but its auto_vacuum mode is
	// always NONE (no PRAGMA path mutates it), so the mode lookup misses
	// and we skip it. This block thus fires only for the main database
	// (and any ATTACH'd databases that have been switched to FULL mode).
	if err := e.runAutoVacuumCommitAll(); err != nil {
		return &Result{Error: err}
	}
	if res := e.commitFlushAllPagers(); res != nil {
		return res
	}
	return &Result{}
}

// commitCheckDeferredFK runs COMMIT's deferred foreign-key constraint checks
// (deferred FKs are checked at COMMIT, not per statement). On a violation the
// COMMIT fails and the transaction stays open, so inTransaction/txSnapshots
// are NOT cleared.
func (e *Engine) commitCheckDeferredFK() *Result {
	if !e.settings.foreignKeys || !e.tx.inTransaction {
		return nil
	}
	if err := e.constraints.CheckDeferredFK(false); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// commitInterruptCountdown applies the SQLITE_TEST interrupt countdown
// (vdbe.c's per-opcode decrement) inside the COMMIT program. ok is false
// when the countdown hit zero: the COMMIT was interrupted, the transaction
// was rolled back (SQLITE_INTERRUPT's abort path, src/vdbeaux.c:3358-3383),
// and res carries the "interrupted" error.
func (e *Engine) commitInterruptCountdown() (res *Result, ok bool) {
	if e.interruptCount == 0 {
		return nil, true
	}
	e.interruptCount--
	if e.interruptCount == 0 {
		e.interrupted = true
		e.execRollback()
		return &Result{Error: fmt.Errorf("interrupted")}, false
	}
	return nil, true
}

// commitClearTxState clears the engine's transaction-scoped state once the
// COMMIT is allowed to proceed (inTransaction, FK deferral, dirty-file and
// DDL buffers, snapshots, RESERVED marks).
func (e *Engine) commitClearTxState() {
	e.tx.txSchemaChanged = false
	e.tx.inTransaction = false
	e.settings.deferForeignKeys = false
	e.constraints.ResetFKDirty()
	e.dml.ClearTxnWrittenFiles()
	e.tx.ddlBuffer = nil
	e.tx.txSnapshots = nil
	e.tx.txFTSnapshots = nil
	e.clearReservedDbs()
	e.dml.ClearTxnWrittenFiles()
}

// flushFTSSegmentsGuarded flushes pending FTS3 segments within the current
// statement/transaction rollback scope (SQLite's FTS3 flushes the
// pending-terms hash at COMMIT / statement end, writing one segment per
// transaction). The flush is marked (inFTSFlush) so its internal
// shadow-table writes (and the auto-incr-merge they trigger) skip the
// per-write pager snapshot — they are part of the enclosing scope, and
// copying the whole pager per %_segments block insert is O(n^2) across the
// automerge's many flushes (fts4merge4 2.2.x).
//
// The FTS flush's internal %_segdir/%_segments/%_stat writes run through
// nested Exec calls and would clobber last_insert_rowid with the shadow
// tables' rowids (SQLite's OP_VUpdate sets db->lastRowid = the FTS docid
// AFTER the module's internal writes; at COMMIT the flush is part of the
// statement so the docid value set by the last INSERT survives — see
// execFlushAutocommit's identical guard). Preserve and restore.
func (e *Engine) flushFTSSegmentsGuarded() *Result {
	e.tx.inFTSFlush = true
	savedRowID := e.lastRowID
	res := e.FlushFTSSegments()
	e.lastRowID = savedRowID
	e.tx.inFTSFlush = false
	if res != nil {
		return res
	}
	return nil
}

// commitReleaseLocksAndCounters releases the cross-connection lock marks
// (write transactions and BEGIN EXCLUSIVE) acquired during the transaction,
// clears the transaction-level SHARED lock and any PENDING mark so another
// connection's COMMIT can now upgrade to EXCLUSIVE (lock2-1.8), and bumps
// the file change counter (header offset 24) of every written database so
// other connections observe the change via PRAGMA data_version and schema
// re-reads. This connection's own data_version stays at its cached value.
func (e *Engine) commitReleaseLocksAndCounters() {
	e.releaseTransactionLocks()
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && dbCtx.Pager != nil && dbCtx.Pager.HasDirtyPages() {
			e.updateFileChangeCounter(dbCtx)
		}
	}
}

// releaseTransactionLocks drops this connection's cross-connection lock
// marks: the write-transaction registration, the BEGIN EXCLUSIVE mark and
// the transaction-level SHARED lock.
func (e *Engine) releaseTransactionLocks() {
	e.registerWriteTx(false)
	e.ReleaseExclusive()
	e.releaseSharedTx()
}

// commitFlushAllPagers flushes every database's pager so HasDirtyPages()
// becomes false (lock_status reads "unlocked" after the commit): after
// COMMIT all databases return to unlocked. The main pager is in dbList.
// When more than one USER database is being flushed together (an ATTACH'd
// database is also part of the commit), the pagers are flushed with
// multiDB=true so PERSIST-mode journals are truncated to 0 (the
// super-journal path in pager.c zeroJournalHdr hasSuper==true branch). The
// temp database is in dbList but is an in-memory pager that never produces
// a rollback journal file, so it does NOT count toward the multi-DB total.
func (e *Engine) commitFlushAllPagers() *Result {
	multiDB := false
	nonNilPagers := 0
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && dbCtx.Pager != nil && !dbCtx.Pager.IsMemory() && dbCtx.Pager.HasDirtyPages() {
			nonNilPagers++
		}
	}
	if nonNilPagers > 1 {
		multiDB = true
	}
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && dbCtx.Pager != nil {
			if err := dbCtx.Pager.FlushWithContext(multiDB); err != nil {
				// sqlite3PagerCommitPhaseOne failure aborts the
				// transaction: the pager rolls back to its BEGIN state
				// (vdbe.c abort path). Without the rollback the flushed
				// header/pages would lead the rolled-back file state.
				e.execRollback()
				return &Result{Error: err}
			}
		}
	}
	return nil
}

// commitLockGate enforces the cross-connection COMMIT lock: a writer must
// upgrade to EXCLUSIVE, which is blocked by another connection's SHARED (read
// transaction) or prepared read lock (src/pager.c EXCLUSIVE upgrade refusal).
// On block the transaction STAYS OPEN and the pager sits in PENDING so a later
// COMMIT can retry once the reader releases — lock2 1.5/1.7. Returns a
// "database is locked" result on block, nil when the COMMIT may proceed.
func (e *Engine) commitLockGate() *Result {
	if !e.WriteTxOpen() {
		return nil
	}
	// unix-none performs no cross-connection locking: a writer always commits.
	if e.lockStyle == LockStyleNone {
		return nil
	}
	if err := e.commitLockError(); err != nil {
		e.setPendingAll()
		return &Result{Error: err}
	}
	return nil
}

// runAutoVacuumCommitAll runs AutoVacuumCommit for every attached database
// that is in FULL auto-vacuum mode (PRAGMA auto_vacuum=1) and whose pager
// is actually in autovacuum mode. Shared between execCommit (explicit
// COMMIT) and execFlushAutocommit (autocommit statements). Without this
// helper, autocommit statements never trigger auto-vacuum and the file
// grows without bound when many DELETEs run without a wrapping BEGIN/
// COMMIT (P8.INCRVACUUM.phase8 follow-up; btree.c autoVacuumCommit fires
// from sqlite3BtreeCommitPhaseOne on every commit, autocommit included).
func (e *Engine) runAutoVacuumCommitAll() error {
	for _, dbCtx := range e.dbList {
		if dbCtx == nil || dbCtx.Pager == nil {
			continue
		}
		// Only FULL mode: incremental is opt-in via PRAGMA incremental_vacuum.
		mode := int64(0)
		if e.settings.autoVacuumModes != nil {
			if m, ok := e.settings.autoVacuumModes[dbCtx.Name]; ok {
				mode = m
			}
		}
		if mode != 1 /* FULL */ {
			continue
		}
		// Skip if the pager isn't actually in autovacuum mode (the mode
		// is only set on the pager when the DB is empty; for a non-empty
		// DB the change is deferred to the next VACUUM, so the pager
		// still has AutoVacuum()=false here).
		if !dbCtx.Pager.AutoVacuum() {
			continue
		}
		if _, err := e.AutoVacuumCommit(dbCtx.Name); err != nil {
			return err
		}
	}
	return nil
}

// --- BEGIN TRANSACTION ---

func (e *Engine) execBegin(stmt *sql.BeginStmt) *Result {
	// build.c sqlite3BeginTransaction: a BEGIN while a transaction is
	// active errors before any lock work ("cannot start a transaction
	// within a transaction") — db->autoCommit==0 is checked first, so
	// nested BEGIN IMMEDIATE/EXCLUSIVE report this, not a lock error
	// (trans-4.x, avtrans, lock3-3.x).
	if e.tx.inTransaction {
		return &Result{Error: fmt.Errorf("cannot start a transaction within a transaction")}
	}
	// Lock acquisition happens BEFORE any transaction state changes
	// (sqlite3BeginTransaction -> sqlite3BtreeBeginTrans -> pager lock
	// request; SQLITE_BUSY aborts the BEGIN with no side effects).
	// BEGIN IMMEDIATE needs RESERVED: fails when another connection holds a
	// write (RESERVED+) or exclusive lock on any attached file. BEGIN
	// EXCLUSIVE needs EXCLUSIVE: additionally fails on another connection's
	// SHARED (read) lock (pager.c lock upgrade rules; lock-2.8, lock3-3.x).
	if stmt != nil && (stmt.Type == "IMMEDIATE" || stmt.Type == "EXCLUSIVE") {
		if err := e.beginLockError(stmt, stmt.Type == "EXCLUSIVE"); err != nil {
			return &Result{Error: err}
		}
	}
	e.tx.inTransaction = true
	e.constraints.ResetFKDirty()
	e.tx.ddlBuffer = nil
	// Snapshot every attached database's pager so ROLLBACK can undo DML
	// (page-level undo images). COMMIT discards the snapshots.
	e.tx.txSnapshots = make(map[string]*pager.PagerState, len(e.databases))
	for name, ctx := range e.databases {
		e.tx.txSnapshots[name] = ctx.Pager.Snapshot()
	}
	// Snapshot the FTS in-memory indexes so ROLLBACK undoes FTS writes the
	// pager restore does not cover (the FTS store is in-memory).
	e.tx.txFTSnapshots = e.snapshotAllFTS()
	e.tx.txFTS5Snapshots = e.snapshotAllFTS5()
	if stmt != nil {
		switch stmt.Type {
		case "EXCLUSIVE":
			e.BeginExclusive()
		case "IMMEDIATE":
			e.registerWriteTx(true)
		}
	}
	return &Result{}
}

// beginLockError reports whether a BEGIN IMMEDIATE (exclusive=false) or BEGIN
// EXCLUSIVE (exclusive=true) would be blocked by another connection's locks
// on any of this connection's database files. RESERVED is blocked by another
// writer or exclusive holder; EXCLUSIVE additionally by another reader — the
// rollback-journal model only. WAL mode has no reader/writer exclusion (C's
// wal.c: BEGIN EXCLUSIVE blocks on other WRITERS; readers keep reading), so
// the reader check applies only when the target database is not in WAL mode.
// The unix-none locking style never blocks; the unix-flock / unix-dotfile
// styles collapse every lock level into a single EXCLUSIVE mutex, so any
// other holder blocks (os_unix.c flockLock / dotlockLock).
func (e *Engine) beginLockError(stmt sql.Stmt, exclusive bool) error {
	if e.lockStyle == LockStyleNone {
		return nil
	}
	// BEGIN EXCLUSIVE maps to C's PagerBegin(exFlag=true): the RESERVED
	// acquire (blocked by another writer or EXCLUSIVE holder) fails WITHOUT
	// the busy handler; the EXCLUSIVE upgrade (blocked by readers) retries
	// through it (pager.c sqlite3PagerSetBusyHandler transition table).
	for count := 0; ; count++ {
		err, invoke := e.beginLockCheck(stmt, exclusive)
		if err == nil {
			return nil
		}
		if !invoke || !e.busyRetry(count) {
			return err
		}
	}
}

// beginLockCheck performs one pass of the BEGIN IMMEDIATE/EXCLUSIVE lock
// matrix; invoke reports whether the busy handler may retry this failure
// (only the reader-blocked EXCLUSIVE upgrade qualifies).
func (e *Engine) beginLockCheck(stmt sql.Stmt, exclusive bool) (err error, invoke bool) {
	walMode := e.stmtWALMode(stmt, "main")
	for _, k := range e.allLockKeys() {
		if e.lockStyle == LockStyleExclusive || e.lockStyle == LockStyleDotfile {
			if lockreg.Global.ConnLockedByOther(k, e.connID) {
				return fmt.Errorf("database is locked"), false
			}
			continue
		}
		if _, ok := lockreg.Global.ExclusiveLockedByOther(k, e.connID); ok {
			return fmt.Errorf("database is locked"), false
		}
		if lockreg.Global.WriteTxByOther(k, e.connID) {
			return fmt.Errorf("database is locked"), false
		}
		if exclusive && !walMode && lockreg.Global.ReadTxByOther(k, e.connID) {
			return fmt.Errorf("database is locked"), true
		}
	}
	return nil, false
}

// --- ROLLBACK ---

func (e *Engine) execRollback() *Result {
	// SQLite raises "cannot rollback - no transaction is active" when a
	// bare ROLLBACK is issued without an open transaction (src/vdbe.c:4056).
	if !e.tx.inTransaction {
		// Clear externally emulated BEGIN EXCLUSIVE marks even when the
		// transaction parser did not open an engine transaction.
		e.releaseTransactionLocks()
		return &Result{Error: fmt.Errorf("cannot rollback - no transaction is active")}
	}
	// A ROLLBACK issued from a nested statement (the eval() extension, a
	// trigger body) that undoes schema changes (DDL inside the transaction)
	// aborts the enclosing statement: SQLite bumps the schema cookie on DDL,
	// and the executing statement detects the schema change and fails with
	// "abort due to ROLLBACK" (SQLITE_ABORT_ROLLBACK). A rollback that undoes
	// only DML (misc8-1.4's BEGIN; INSERT; SELECT ... eval ROLLBACK) does NOT
	// abort the enclosing statement.
	if e.tx.execDepth > 1 && e.tx.txSchemaChanged {
		e.tx.rollbackAborted = true
	}
	// A rollback performed by a NESTED statement (trigger body OR ROLLBACK,
	// eval()) invalidates every enclosing statement's pager snapshots: they
	// were taken after BEGIN, and restoring them would resurrect rows the
	// transaction rollback already undid (trigger2-6.1h/6.2h).
	if e.tx.execDepth > 1 {
		e.tx.nestedRollback = true
	}
	e.tx.txSchemaChanged = false
	e.tx.inTransaction = false
	e.settings.deferForeignKeys = false
	e.constraints.ResetFKDirty()
	// Release cross-connection lock marks (write transactions and BEGIN
	// EXCLUSIVE) acquired during the transaction, and clear the
	// transaction-level SHARED lock and any PENDING mark (a failed COMMIT
	// leaves the connection PENDING; ROLLBACK releases it so another writer
	// can proceed).
	e.releaseTransactionLocks()
	// ROLLBACK cancels EVERY savepoint opened in the transaction
	// (lang_savepoint.html: "the transaction is rolled back and all
	// savepoints are cancelled"). A stale stack made a later RELEASE of a
	// pre-ROLLBACK savepoint find idx>0 (startsTransaction false) and leave
	// an implicit transaction open, so the next BEGIN failed with "cannot
	// start a transaction within a transaction" (savepoint-4.2).
	e.tx.savepointStack = nil
	e.clearReservedDbs()
	// Undo all DDL operations that were performed during the transaction
	for i := len(e.tx.ddlBuffer) - 1; i >= 0; i-- {
		e.tx.ddlBuffer[i]()
	}
	e.tx.ddlBuffer = nil
	e.restoreTxSnapshots()
	// The rolled-back deletes' pending format-upgrade requests are dropped
	// with them (sqlite3Fts5StorageRollback discards the pending data).
	e.fts5DiscardSecureUpgrades()
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
	// Fire the rollback hook after the rollback completes
	// (sqlite3_rollback_hook).
	e.fireRollbackHook()
	return &Result{}
}

// restoreTxSnapshots restores the page-level and in-memory FTS states taken
// at BEGIN, undoing DML writes the buffers do not cover, and clears the
// snapshot registries.
func (e *Engine) restoreTxSnapshots() {
	// Restore page-level state taken at BEGIN to undo DML writes.
	for name, ctx := range e.databases {
		if snap, ok := e.tx.txSnapshots[name]; ok {
			ctx.Pager.Restore(snap)
		} else {
		}
	}
	e.tx.txSnapshots = nil
	// Restore the FTS in-memory indexes captured at BEGIN.
	for _, snap := range e.tx.txFTSnapshots {
		if snap.table != nil {
			if snap.state != nil {
				snap.table.Restore(snap.state)
			}
			snap.table.RestorePending(snap.pending)
		}
	}
	e.tx.txFTSnapshots = nil
	for _, snap := range e.tx.txFTS5Snapshots {
		if snap.table != nil && snap.state != nil {
			snap.table.Restore(snap.state)
		}
	}
	e.tx.txFTS5Snapshots = nil
}

// savepointEntry records the pager state at a SAVEPOINT so ROLLBACK TO can
// undo writes since the savepoint (mirroring the BEGIN snapshot mechanism).
type savepointEntry struct {
	name          string
	snapshots     map[string]*pager.PagerState
	ftsSnapshots  []ftsSnap
	fts5Snapshots []fts5Snap
	ddlLen        int
	inTxBefore    bool
}

// --- SAVEPOINT / RELEASE / ROLLBACK TO ---

// noteReservedDbs records every attached database whose pager currently holds
// dirty pages (i.e. took the WRITER/RESERVED lock) as locked-for-the-
// transaction. A later savepoint rollback may clean the pages, but C's pager
// keeps the lock until COMMIT / full ROLLBACK (see txState.reservedDbs).
func (e *Engine) noteReservedDbs() {
	marked := false
	for _, ctx := range e.dbList {
		if ctx == nil || ctx.Pager == nil || !ctx.Pager.HasDirtyPages() {
			continue
		}
		if e.tx.reservedDbs == nil {
			e.tx.reservedDbs = make(map[string]bool)
		}
		key := strings.ToUpper(ctx.Name)
		if !e.tx.reservedDbs[key] {
			e.tx.reservedDbs[key] = true
			marked = true
		}
	}
	_ = marked
}

// clearReservedDbs releases the per-transaction RESERVED marks (COMMIT /
// full ROLLBACK; pager.c clears the WRITER state when the transaction ends)
// and the per-transaction SHARED read marks.
func (e *Engine) clearReservedDbs() {
	e.tx.reservedDbs = nil
	e.tx.readDbs = nil
}

func (e *Engine) execSavepoint(s *sql.SavepointStmt) *Result {
	// The authorizer sees SQLITE_SAVEPOINT with the operation name and the
	// savepoint name BEFORE the statement executes (build.c sqlite3Savepoint:
	// sqlite3AuthCheck(pParse, SQLITE_SAVEPOINT, az[op], zName, 0) with
	// az = {"BEGIN", "RELEASE", "ROLLBACK"}; savepoint-9.1..9.3). DENY fails
	// the statement with "not authorized" and no savepoint work happens.
	ops := map[string]string{"SAVEPOINT": "BEGIN", "RELEASE": "RELEASE", "ROLLBACK": "ROLLBACK"}
	op, ok := ops[strings.ToUpper(s.Type)]
	if ok {
		if err := e.Authorize(auth.ActionSavepoint, op, s.Name, "", ""); err != nil {
			return &Result{Error: err}
		}
	}
	switch strings.ToUpper(s.Type) {
	case "SAVEPOINT":
		return e.execSavepointCreate(s)
	case "RELEASE":
		return e.execSavepointRelease(s)
	case "ROLLBACK":
		return e.execSavepointRollback(s)
	}
	return &Result{}
}

// execSavepointCreate pushes a new savepoint snapshot (nesting). Reusing a
// name creates a new savepoint above the old one (SQLite allows same-name
// nesting).
func (e *Engine) execSavepointCreate(s *sql.SavepointStmt) *Result {
	// fts5SavepointMethod flushes the pending index (and its secure-delete
	// format upgrade) to disk BEFORE the savepoint is recorded, so a later
	// ROLLBACK TO does not undo the flushed writes.
	if err := e.fts5ApplySecureUpgrades(); err != nil {
		return &Result{Error: err}
	}
	snaps := make(map[string]*pager.PagerState, len(e.databases))
	for name, ctx := range e.databases {
		snaps[name] = ctx.Pager.Snapshot()
	}
	e.tx.savepointStack = append(e.tx.savepointStack, savepointEntry{
		name:          s.Name,
		snapshots:     snaps,
		ftsSnapshots:  e.snapshotAllFTS(),
		fts5Snapshots: e.snapshotAllFTS5(),
		ddlLen:        len(e.tx.ddlBuffer),
		inTxBefore:    e.tx.inTransaction,
	})
	// A SAVEPOINT outside BEGIN implicitly starts a transaction.
	if !e.tx.inTransaction {
		e.tx.inTransaction = true
		e.constraints.ResetFKDirty()
	}
	return &Result{}
}

// execSavepointRelease pops savepoints up to and including the named one
// (SQLite releases the named savepoint and any nested above it). Releasing a
// transaction savepoint fails when deferred foreign key constraints are
// violated (R-37736-42616: "If a COMMIT statement ... fails because the
// database is currently in a state that violates a deferred foreign key
// constraint ... the nested savepoints remain open"). SQLite checks the
// deferred FK constraints only when the release pops the OUTERMOST savepoint
// (a nested RELEASE just merges into the enclosing savepoint).
func (e *Engine) execSavepointRelease(s *sql.SavepointStmt) *Result {
	idx := e.findSavepoint(s.Name)
	if idx < 0 {
		return &Result{Error: fmt.Errorf("no such savepoint: %s", s.Name)}
	}
	// The FK check applies only when releasing the savepoint that IMPLICITLY
	// started the transaction (inTxBefore false — a bare SAVEPOINT outside
	// BEGIN). Releasing inside an explicit BEGIN (or a nested savepoint) just
	// merges into the enclosing scope, matching SQLite (e_fkey-36.2 succeeds,
	// e_fkey-37.2/37.5 fail).
	startsTransaction := idx == 0 && !e.tx.savepointStack[idx].inTxBefore
	popped := append([]savepointEntry{}, e.tx.savepointStack[idx:]...)
	e.tx.savepointStack = e.tx.savepointStack[:idx]
	if startsTransaction && e.settings.foreignKeys && e.tx.inTransaction {
		if err := e.constraints.CheckDeferredFK(false); err != nil {
			e.tx.savepointStack = append(e.tx.savepointStack, popped...)
			return &Result{Error: err}
		}
	}
	if startsTransaction {
		return e.releaseImplicitTxSavepoint(popped)
	}
	return &Result{}
}

// releaseImplicitTxSavepoint commits the transaction implicitly started by a
// bare SAVEPOINT when its outermost savepoint is released (SQLite
// lang_savepoint.html: releasing the outermost savepoint that started the
// transaction commits it). popped is pushed back on the flush failure so the
// savepoints remain open (R-37736-42616).
func (e *Engine) releaseImplicitTxSavepoint(popped []savepointEntry) *Result {
	// Releasing the outermost savepoint commits: flush the fts5
	// secure-delete format upgrade (fts5SavepointMethod's flush).
	if err := e.fts5ApplySecureUpgrades(); err != nil {
		e.tx.savepointStack = append(e.tx.savepointStack, popped...)
		return &Result{Error: err}
	}
	e.tx.inTransaction = false
	e.settings.deferForeignKeys = false
	e.constraints.ResetFKDirty()
	e.tx.ddlBuffer = nil
	e.tx.txSnapshots = nil
	e.clearReservedDbs()
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && dbCtx.Pager != nil {
			if err := dbCtx.Pager.FlushWithContext(false); err != nil {
				return &Result{Error: err}
			}
		}
	}
	return &Result{}
}

// findSavepoint locates a savepoint by name (case-insensitive, innermost
// first); -1 when the name is not on the stack.
func (e *Engine) findSavepoint(name string) int {
	for i := len(e.tx.savepointStack) - 1; i >= 0; i-- {
		if strings.EqualFold(e.tx.savepointStack[i].name, name) {
			return i
		}
	}
	return -1
}

// execSavepointRollback restores the pager state at the named savepoint and
// pops savepoints above it (the named savepoint stays for reuse).
func (e *Engine) execSavepointRollback(s *sql.SavepointStmt) *Result {
	idx := e.findSavepoint(s.Name)
	if idx < 0 {
		return &Result{Error: fmt.Errorf("no such savepoint: %s", s.Name)}
	}
	sp := e.tx.savepointStack[idx]
	e.restoreSavepointState(sp)
	// Pop savepoints above the named one (the named one stays).
	e.tx.savepointStack = e.tx.savepointStack[:idx+1]
	return &Result{}
}

// restoreSavepointState undoes everything recorded after the savepoint:
// buffered DDL closures, pager pages, and the in-memory FTS indexes, then
// invalidates the derived caches. (fts5RollbackToMethod →
// sqlite3Fts5StorageRollback drops the deletes' pending format-upgrade
// requests with the rolled-back rows.)
func (e *Engine) restoreSavepointState(sp savepointEntry) {
	// Undo DDL performed after the savepoint.
	for i := len(e.tx.ddlBuffer) - 1; i >= sp.ddlLen; i-- {
		e.tx.ddlBuffer[i]()
	}
	e.tx.ddlBuffer = e.tx.ddlBuffer[:sp.ddlLen]
	// Restore pager state.
	for name, ctx := range e.databases {
		if snap, ok := sp.snapshots[name]; ok {
			ctx.Pager.Restore(snap)
		}
	}
	// Restore the FTS in-memory indexes captured at the savepoint.
	for _, snap := range sp.ftsSnapshots {
		if snap.table != nil {
			if snap.state != nil {
				snap.table.Restore(snap.state)
			}
			snap.table.RestorePending(snap.pending)
		}
	}
	for _, snap := range sp.fts5Snapshots {
		if snap.table != nil && snap.state != nil {
			snap.table.Restore(snap.state)
		}
	}
	e.fts5DiscardSecureUpgrades()
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
}

// fts5ApplySecureUpgrades persists every fts5 table's pending secure-delete
// format upgrade ('version'=5 in %_config — fts5FlushSecureDelete's
// one-time REPLACE). Called at the flush points: COMMIT, RELEASE-of-outermost
// savepoint, and SAVEPOINT creation (fts5SavepointMethod's flush).
func (e *Engine) fts5ApplySecureUpgrades() error {
	for _, t := range e.fts5Tables {
		if t != nil {
			if err := t.ApplySecureUpgrade(); err != nil {
				return err
			}
		}
	}
	return nil
}

// fts5DiscardSecureUpgrades drops every fts5 table's pending secure-delete
// format-upgrade request (the deletes that requested it were rolled back —
// sqlite3Fts5StorageRollback discards the pending data).
func (e *Engine) fts5DiscardSecureUpgrades() {
	for _, t := range e.fts5Tables {
		if t != nil {
			t.DiscardSecureUpgrade()
		}
	}
}

// BeginInternalWrites marks the engine as executing schema-maintenance
// statements (VACUUM's logical copy): DML executed inside this window does
// not accumulate into sqlite3_total_changes — the C library's internal vdbe
// programs never touch db->nTotalChange (e_totalchanges-2.3). The returned
// function ends the window and must be called by the caller.
func (e *Engine) BeginInternalWrites() (end func()) {
	e.tx.internalWrites++
	return func() { e.tx.internalWrites-- }
}
