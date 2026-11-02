package exec

import (
	"errors"
	"fmt"

	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// Shared-cache statement gates: the btree.c table-lock subsystem applied at
// statement scope. C acquires per-table locks through OP_TableLock as the
// VDBE runs; our statements execute atomically, so the equivalent is one
// acquisition phase at the outermost statement's entry (reads → READ locks,
// writes → the shared b-tree's write transaction plus WRITE locks) and a
// release at the transaction end (an autocommit statement IS its
// transaction). The sharedCacheGateState tracks the acquired locks for the
// release path.

// sharedTableRef is one table the statement touches, resolved through the
// schema (the lock key needs the b-tree's root page).
type sharedTableRef struct {
	name  string
	entry *schema.Entry
	ctx   *DatabaseContext
}

// SetSharable marks this connection as a shared-cache member (C sets
// db->sharable at open when the global shared-cache flag was on). Groups are
// joined lazily per file (execSharedCacheGate / attachSharedCacheGroup): the
// main file at the first locked statement, an attached file when a statement
// first touches it.
func (e *Engine) SetSharable(on bool) {
	if on {
		e.sharable = true
	}
}

// ensureSharedFile joins the file's shared-cache group on first use (the
// C BtShared comes into being when the first sharable connection opens the
// file and dissolves at the last member's close).
func (e *Engine) ensureSharedFile(key string) {
	if key == "" {
		return
	}
	if e.sharedFiles == nil {
		e.sharedFiles = make(map[string]bool)
	}
	if !e.sharedFiles[key] {
		lockreg.Global.JoinSharedCache(key, e.connID)
		e.sharedFiles[key] = true
	}
}

// sharableConn reports whether this connection participates in a
// shared-cache group (the gate is inert otherwise — the default).
func (e *Engine) sharableConn() bool {
	return e.sharable
}

// sharedTableRefs collects the tables an outermost statement reads or
// writes (S1 surface: the DML target and the SELECT FROM terms — the
// tables C's program opens cursors on; trigger bodies and subqueries run
// as nested frames or extend this walk in later slices).
func (e *Engine) sharedTableRefs(stmt sql.Stmt) (reads []sharedTableRef, writes []sharedTableRef) {
	addRef := func(name string, list *[]sharedTableRef) {
		if name == "" {
			return
		}
		entry, ctx, err := e.findTable(name)
		if err != nil || entry == nil || ctx == nil {
			return
		}
		*list = append(*list, sharedTableRef{name: name, entry: entry, ctx: ctx})
	}
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		addRef(s.Table, &writes)
	case *sql.UpdateStmt:
		addRef(s.Table, &writes)
	case *sql.DeleteStmt:
		addRef(s.Table, &writes)
	case *sql.SelectStmt:
		addRef(s.From.Name, &reads)
		for _, j := range s.Joins {
			addRef(j.Table.Name, &reads)
		}
	}
	return reads, writes
}

// execSharedCacheGate acquires the statement's shared-cache locks at the
// outermost statement's entry. A write statement first registers the shared
// b-tree's write transaction (btree.c begin_transaction — another member's
// open write transaction fails with the BARE "database table is locked",
// notify1-1.2), then takes WRITE locks on the written tables; a read
// statement takes READ locks. Each lock keys on the TABLE'S OWN FILE (the
// BtShared is per file; attached databases have groups of their own).
// Table-lock conflicts fail with the table name in the message
// ("database table is locked: t1", vdbe.c OP_TableLock) and mark this
// connection blocked on the owner (sqlite3ConnectionBlocked).
func (e *Engine) execSharedCacheGate(stmt sql.Stmt) *Result {
	if !e.sharableConn() {
		return nil
	}
	reads, writes := e.sharedTableRefs(stmt)
	refKey := func(ref sharedTableRef) string {
		if ref.ctx == nil {
			return ""
		}
		return e.dbLockKey(ref.ctx)
	}
	for _, ref := range append(append([]sharedTableRef{}, writes...), reads...) {
		e.ensureSharedFile(refKey(ref))
	}
	// A failed gate undoes ONLY this statement's acquisitions (a failed
	// OP_TableLock ends the statement; the transaction's earlier locks —
	// writer registration and prior BtLock entries — persist, btree.c's
	// trans_begun / setSharedCacheTableLock split).
	type gateLock struct {
		key  lockreg.TableLockKey
		name string
	}
	var held []gateLock
	beginKey := ""
	beginRegistered := false
	undo := func(err error) *Result {
		for _, l := range held {
			lockreg.Global.ReleaseTableLock(l.key, e.connID)
		}
		if beginRegistered {
			lockreg.Global.SharedWriteTxEnd(beginKey, e.connID)
		}
		return &Result{Error: err}
	}
	if len(writes) > 0 {
		writeKey := refKey(writes[0])
		if writeKey != "" {
			beginRegistered = !lockreg.Global.SharedWriteTxHeld(writeKey, e.connID)
			if err := lockreg.Global.SharedWriteTxBegin(writeKey, e.connID); err != nil {
				// The begin-transaction path carries no table name.
				return undo(err)
			}
			beginKey = writeKey
		}
	}
	for _, ref := range writes {
		key := refKey(ref)
		if key == "" {
			continue
		}
		lk := lockreg.TableLockKey{File: key, Root: ref.entry.RootPage}
		if err := lockreg.Global.AcquireTableLock(lk, e.connID, true); err != nil {
			return undo(fmt.Errorf("%w: %s", err, ref.name))
		}
		held = append(held, gateLock{key: lk, name: ref.name})
	}
	for _, ref := range reads {
		key := refKey(ref)
		if key == "" {
			continue
		}
		lk := lockreg.TableLockKey{File: key, Root: ref.entry.RootPage}
		if err := lockreg.Global.AcquireTableLock(lk, e.connID, false); err != nil {
			return undo(fmt.Errorf("%w: %s", err, ref.name))
		}
		held = append(held, gateLock{key: lk, name: ref.name})
	}
	if len(held) > 0 || beginRegistered {
		e.tx.sharedCacheLocked = true
	}
	return nil
}

// sharedCacheStatementEnd runs at the outermost statement's exit: an
// autocommit statement's implicit transaction ends here, releasing its
// table locks and firing the unlock-notify callbacks of connections blocked
// on this one. Locks taken inside an explicit transaction survive until
// COMMIT/ROLLBACK (releaseTransactionLocks).
func (e *Engine) sharedCacheStatementEnd() {
	// Only the outermost frame's epilogue ends the statement (nested trigger
	// bodies run inside the outer statement's transaction scope; their Exec
	// frames register this defer too, at depth > 1).
	if e.tx.execDepth != 1 {
		return
	}
	if !e.sharableConn() || !e.tx.sharedCacheLocked {
		return
	}
	if e.tx.inTransaction {
		return
	}
	e.tx.sharedCacheLocked = false
	e.releaseSharedCacheLocks()
}

// releaseSharedCacheLocks drops this connection's shared-cache locks and
// writer registrations on every joined file and fires the blocked
// connections' callbacks (the transaction-end path: autocommit statement
// end, COMMIT, ROLLBACK).
func (e *Engine) releaseSharedCacheLocks() {
	for key := range e.sharedFiles {
		lockreg.Global.ReleaseSharedTxLocks(key, e.connID)
		lockreg.Global.SharedWriteTxEnd(key, e.connID)
	}
	lockreg.Global.ConnectionUnlocked(e.connID)
}

// LeaveSharedCache removes the connection from its shared-cache groups at
// Close (btree.c sqlite3BtreeClose drops the connection's BtLock entries;
// notify.c removes the connection from the blocked list).
func (e *Engine) LeaveSharedCache() {
	if !e.sharable {
		return
	}
	e.sharable = false
	e.tx.sharedCacheLocked = false
	lockreg.Global.ClearUnlockNotify(e.connID)
	for key := range e.sharedFiles {
		lockreg.Global.LeaveSharedCache(key, e.connID)
	}
	e.sharedFiles = nil
}

// SharedCacheUnlockNotify registers the connection's one unlock-notify
// callback (sqlite3_unlock_notify): it fires immediately when the connection
// is not blocked, reports the deadlock error for a cyclic wait graph, and
// otherwise fires when the blocking connection's transaction ends.
func (e *Engine) SharedCacheUnlockNotify(cb func()) error {
	err := lockreg.Global.SetUnlockNotify(e.connID, cb)
	if errors.Is(err, lockreg.ErrDeadlocked) {
		return errors.New("database is deadlocked")
	}
	return err
}

// SharableBlocked reports the connection this one is blocked on (0 = none) —
// the native-pin probe for the notify graph.
func (e *Engine) SharableBlocked() int64 {
	return lockreg.Global.BlockedBy(e.connID)
}
