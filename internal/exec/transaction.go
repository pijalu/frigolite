// Package exec implements query execution.
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/sql"
)

// --- COMMIT ---

func (e *Engine) execCommit() *Result {
	// Deferred foreign key constraints are checked at COMMIT. On a violation
	// the COMMIT fails and the transaction stays open (SQLite semantics:
	// "cannot start a transaction within a transaction" after a failed
	// COMMIT), so inTransaction/txSnapshots are NOT cleared.
	if e.settings.foreignKeys && e.tx.inTransaction {
		if err := e.constraints.CheckDeferredFK(false); err != nil {
			return &Result{Error: err}
		}
	}
	// Cross-connection COMMIT gate: a writer must upgrade to EXCLUSIVE, which
	// is blocked by another connection's SHARED (read transaction) or prepared
	// read lock. The transaction STAYS OPEN and the pager sits in PENDING
	// (lock2 1.5/1.7): inTransaction is NOT cleared and the writer keeps its
	// RESERVED lock so a later COMMIT can retry once the reader releases.
	if res := e.commitLockGate(); res != nil {
		return res
	}
	e.tx.txSchemaChanged = false
	e.tx.inTransaction = false
	e.settings.deferForeignKeys = false
	e.constraints.ResetFKDirty()
	e.tx.ddlBuffer = nil
	e.tx.txSnapshots = nil
	e.tx.txFTSnapshots = nil
	// Flush pending FTS3 segments (SQLite's FTS3 flushes the pending-terms
	// hash at COMMIT, writing one segment per transaction). Mark the flush so
	// its internal shadow-table writes (and the auto-incr-merge they trigger)
	// skip the per-write pager snapshot — they are part of this COMMIT's
	// rollback scope, and copying the whole pager per %_segments block insert
	// is O(n^2) across the automerge's many flushes (fts4merge4 2.2.x).
	e.tx.inFTSFlush = true
	// The FTS flush's internal %_segdir/%_segments/%_stat writes run through
	// nested Exec calls and would clobber last_insert_rowid with the shadow
	// tables' rowids (SQLite's OP_VUpdate sets db->lastRowid = the FTS docid
	// AFTER the module's internal writes; at COMMIT the flush is part of the
	// statement so the docid value set by the last INSERT survives — see
	// execFlushAutocommit's identical guard). Preserve and restore.
	savedRowID := e.lastRowID
	res := e.FlushFTSSegments()
	e.lastRowID = savedRowID
	e.tx.inFTSFlush = false
	if res != nil {
		return res
	}
	// COMMIT ends all open savepoints (SQLite: committing a transaction
	// releases every savepoint it contains; a later ROLLBACK TO or RELEASE
	// of a pre-COMMIT savepoint fails).
	e.tx.savepointStack = nil
	// Release cross-connection lock marks (write transactions and BEGIN
	// EXCLUSIVE) acquired during the transaction.
	e.registerWriteTx(false)
	e.ReleaseExclusive()
	// Clear the transaction-level SHARED lock and any PENDING mark so another
	// connection's COMMIT can now upgrade to EXCLUSIVE (lock2-1.8).
	e.releaseSharedTx()
	// A commit that wrote data bumps the file change counter (header offset
	// 24) of every written database so other connections observe the change
	// via PRAGMA data_version and schema re-reads. This connection's own
	// data_version stays at its cached value.
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && dbCtx.Pager != nil && dbCtx.Pager.HasDirtyPages() {
			e.updateFileChangeCounter(dbCtx)
		}
	}
	// Release locks: after COMMIT all databases return to unlocked.
	// Flush each pager so HasDirtyPages() becomes false (lock_status reads
	// "unlocked" after the commit). The main pager is in dbList.
	for _, dbCtx := range e.dbList {
		if dbCtx != nil && dbCtx.Pager != nil {
			if err := dbCtx.Pager.Flush(); err != nil {
				return &Result{Error: err}
			}
		}
	}
	// Fire the commit hook after the commit completes (sqlite3_commit_hook).
	// A nonzero return aborts the COMMIT: the transaction is rolled back and
	// the COMMIT statement fails with "constraint failed".
	if e.commitHook != nil && e.runCommitHook() {
		e.execRollback()
		return &Result{Error: fmt.Errorf("constraint failed")}
	}
	return &Result{}
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

// --- BEGIN TRANSACTION ---

func (e *Engine) execBegin(stmt *sql.BeginStmt) *Result {
	// Lock acquisition happens BEFORE any transaction state changes
	// (sqlite3BeginTransaction -> sqlite3BtreeBeginTrans -> pager lock
	// request; SQLITE_BUSY aborts the BEGIN with no side effects).
	// BEGIN IMMEDIATE needs RESERVED: fails when another connection holds a
	// write (RESERVED+) or exclusive lock on any attached file. BEGIN
	// EXCLUSIVE needs EXCLUSIVE: additionally fails on another connection's
	// SHARED (read) lock (pager.c lock upgrade rules; lock-2.8, lock3-3.x).
	if stmt != nil && (stmt.Type == "IMMEDIATE" || stmt.Type == "EXCLUSIVE") {
		if err := e.beginLockError(stmt.Type == "EXCLUSIVE"); err != nil {
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
// writer or exclusive holder; EXCLUSIVE additionally by another reader. The
// unix-none locking style never blocks; the unix-flock / unix-dotfile styles
// collapse every lock level into a single EXCLUSIVE mutex, so any other holder
// blocks (os_unix.c flockLock / dotlockLock).
func (e *Engine) beginLockError(exclusive bool) error {
	if e.lockStyle == LockStyleNone {
		return nil
	}
	for _, k := range e.allLockKeys() {
		if e.lockStyle == LockStyleExclusive || e.lockStyle == LockStyleDotfile {
			if lockreg.Global.ConnLockedByOther(k, e.connID) {
				return fmt.Errorf("database is locked")
			}
			continue
		}
		if _, ok := lockreg.Global.ExclusiveLockedByOther(k, e.connID); ok {
			return fmt.Errorf("database is locked")
		}
		if lockreg.Global.WriteTxByOther(k, e.connID) {
			return fmt.Errorf("database is locked")
		}
		if exclusive && lockreg.Global.ReadTxByOther(k, e.connID) {
			return fmt.Errorf("database is locked")
		}
	}
	return nil
}

// --- ROLLBACK ---

func (e *Engine) execRollback() *Result {
	// SQLite raises "cannot rollback - no transaction is active" when a
	// bare ROLLBACK is issued without an open transaction (src/vdbe.c:4056).
	if !e.tx.inTransaction {
		// Clear externally emulated BEGIN EXCLUSIVE marks even when the
		// transaction parser did not open an engine transaction.
		e.registerWriteTx(false)
		e.ReleaseExclusive()
		e.releaseSharedTx()
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
	e.tx.txSchemaChanged = false
	e.tx.inTransaction = false
	e.settings.deferForeignKeys = false
	e.constraints.ResetFKDirty()
	// Release cross-connection lock marks (write transactions and BEGIN
	// EXCLUSIVE) acquired during the transaction.
	e.registerWriteTx(false)
	e.ReleaseExclusive()
	// Clear the transaction-level SHARED lock and any PENDING mark (a failed
	// COMMIT leaves the connection PENDING; ROLLBACK releases it so another
	// writer can proceed).
	e.releaseSharedTx()
	// Undo all DDL operations that were performed during the transaction
	for i := len(e.tx.ddlBuffer) - 1; i >= 0; i-- {
		e.tx.ddlBuffer[i]()
	}
	e.tx.ddlBuffer = nil
	// Restore page-level state taken at BEGIN to undo DML writes.
	for name, ctx := range e.databases {
		if snap, ok := e.tx.txSnapshots[name]; ok {
			ctx.Pager.Restore(snap)
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
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
	// Fire the rollback hook after the rollback completes
	// (sqlite3_rollback_hook).
	e.fireRollbackHook()
	return &Result{}
}

// savepointEntry records the pager state at a SAVEPOINT so ROLLBACK TO can
// undo writes since the savepoint (mirroring the BEGIN snapshot mechanism).
type savepointEntry struct {
	name         string
	snapshots    map[string]*pager.PagerState
	ftsSnapshots []ftsSnap
	ddlLen       int
	inTxBefore   bool
}

// --- SAVEPOINT / RELEASE / ROLLBACK TO ---

func (e *Engine) execSavepoint(s *sql.SavepointStmt) *Result {
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
	snaps := make(map[string]*pager.PagerState, len(e.databases))
	for name, ctx := range e.databases {
		snaps[name] = ctx.Pager.Snapshot()
	}
	e.tx.savepointStack = append(e.tx.savepointStack, savepointEntry{
		name:         s.Name,
		snapshots:    snaps,
		ftsSnapshots: e.snapshotAllFTS(),
		ddlLen:       len(e.tx.ddlBuffer),
		inTxBefore:   e.tx.inTransaction,
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
	idx := -1
	for i := len(e.tx.savepointStack) - 1; i >= 0; i-- {
		if strings.EqualFold(e.tx.savepointStack[i].name, s.Name) {
			idx = i
			break
		}
	}
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
	// Releasing the savepoint that implicitly started the transaction is
	// equivalent to COMMIT (SQLite lang_savepoint.html: releasing the
	// outermost savepoint that started the transaction commits it). End the
	// transaction so the next bare SAVEPOINT starts a fresh implicit
	// transaction whose RELEASE re-checks deferred FKs (e_fkey-37.x).
	if startsTransaction {
		e.tx.inTransaction = false
		e.settings.deferForeignKeys = false
		e.constraints.ResetFKDirty()
		e.tx.ddlBuffer = nil
		e.tx.txSnapshots = nil
		for _, dbCtx := range e.dbList {
			if dbCtx != nil && dbCtx.Pager != nil {
				if err := dbCtx.Pager.Flush(); err != nil {
					return &Result{Error: err}
				}
			}
		}
	}
	return &Result{}
}

// execSavepointRollback restores the pager state at the named savepoint and
// pops savepoints above it (the named savepoint stays for reuse).
func (e *Engine) execSavepointRollback(s *sql.SavepointStmt) *Result {
	idx := -1
	for i := len(e.tx.savepointStack) - 1; i >= 0; i-- {
		if strings.EqualFold(e.tx.savepointStack[i].name, s.Name) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return &Result{Error: fmt.Errorf("no such savepoint: %s", s.Name)}
	}
	sp := e.tx.savepointStack[idx]
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
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
	// Pop savepoints above the named one (the named one stays).
	e.tx.savepointStack = e.tx.savepointStack[:idx+1]
	return &Result{}
}
