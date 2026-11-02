package lockreg

import (
	"errors"
)

// Shared-cache support: the btree.c shared-cache subsystem's cross-connection
// coordination, mirroring the pieces C keeps on the BtShared and the
// process-global structures of notify.c:
//
//   - the BtLock table-lock list (btree.c querySharedCacheTableLock /
//     setSharedCacheTableLock): one READ or WRITE lock per (connection,
//     table root);
//   - the single writer of the shared b-tree (BtShared.pWriter): a write
//     transaction is exclusive among the group members (btree.c:3638 — a
//     second write transaction returns SQLITE_LOCKED_SHAREDCACHE);
//   - the unlock-notify blocked-connection graph (notify.c
//     sqlite3BlockedList): pBlockingConnection / pUnlockConnection / one
//     callback per connection, firing when a blocker's transaction ends, and
//     the deadlock walk in sqlite3_unlock_notify.
//
// C's blocked-list grouping (addToBlockedList inserts before the first entry
// with the same xUnlockNotify) collapses here: each connection owns one
// callback closure, so every entry has a distinct callback and the list is a
// plain FIFO append — firing in list order is C's grouped firing.

// ErrTableLocked is the shared-cache SQLITE_LOCKED_SHAREDCACHE sentinel:
// btree.c returns it (main.c errstr: "database table is locked") without a
// table name from the begin-transaction path, and the VDBE wraps the
// OP_TableLock failures with the table name ("database table is locked: %s").
var ErrTableLocked = errors.New("database table is locked")

// ErrDeadlocked is notify.c's SQLITE_LOCKED from the deadlock walk
// (sqlite3ErrorWithMsg: "database is deadlocked").
var ErrDeadlocked = errors.New("database is deadlocked")

// SharedCache is one file's shared-cache group: the C BtShared analog.
// Members are the connections opened while the global shared-cache flag was
// set (each contributed one reference; the group dissolves at the last one).
type SharedCache struct {
	// connID of the group member holding the shared b-tree's write
	// transaction (btree.c BtShared.pWriter); 0 = none.
	writer int64
	// tableLocks is the BtLock list: one entry per (connection, root) lock.
	tableLocks []sharedTableLock
	// refs counts the member connections.
	refs int
}

// sharedTableLock is one BtLock entry.
type sharedTableLock struct {
	conn  int64
	root  uint32
	write bool
}

// TableLockKey identifies one table's b-tree inside one database file
// (the OP_TableLock operands: the b-tree and its root page).
type TableLockKey struct {
	File string
	Root uint32
}

// EnableSharedCache toggles the process-global shared-cache flag
// (sqlite3GlobalConfig.sharedCacheEnabled via sqlite3_enable_shared_cache).
// Connections opened while it is set join a per-file shared-cache group.
func (r *Registry) EnableSharedCache(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sharedCacheEnabled = on
}

// SharedCacheEnabled reports the process-global flag.
func (r *Registry) SharedCacheEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sharedCacheEnabled
}

// JoinSharedCache adds connID as a member of the file's shared-cache group,
// creating it on first use. The group lives until the last member leaves.
func (r *Registry) JoinSharedCache(fileKey string, connID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sharedCaches == nil {
		r.sharedCaches = make(map[string]*SharedCache)
	}
	group := r.sharedCaches[fileKey]
	if group == nil {
		group = &SharedCache{}
		r.sharedCaches[fileKey] = group
	}
	group.refs++
}

// LeaveSharedCache removes connID from the file's group: its table locks and
// writer registration die with it (btree.c sqlite3BtreeClose's lock cleanup).
func (r *Registry) LeaveSharedCache(fileKey string, connID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	group := r.sharedCaches[fileKey]
	if group == nil {
		return
	}
	r.releaseSharedTxLocksLocked(group, connID)
	group.refs--
	if group.refs <= 0 {
		delete(r.sharedCaches, fileKey)
	}
}

// SharedWriteTxBegin registers connID as the group's write transaction
// holder. Another member's open write transaction fails with the BARE
// ErrTableLocked — btree.c:3638's begin_transaction path, which has no table
// name to report (notify1-1.2's bare "database table is locked").
func (r *Registry) SharedWriteTxBegin(fileKey string, connID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	group := r.sharedCaches[fileKey]
	if group == nil {
		return nil
	}
	if group.writer != 0 && group.writer != connID {
		r.connectionBlockedLocked(connID, group.writer)
		return ErrTableLocked
	}
	group.writer = connID
	return nil
}

// SharedWriteTxEnd clears connID's writer registration (transaction end).
func (r *Registry) SharedWriteTxEnd(fileKey string, connID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	group := r.sharedCaches[fileKey]
	if group != nil && group.writer == connID {
		group.writer = 0
	}
}

// AcquireTableLock takes a READ or WRITE lock on the table's b-tree for
// connID (sqlite3BtreeLockTable): query then set. Another member holding a
// lock of the OTHER mode on the same b-tree fails with ErrTableLocked and
// marks this connection blocked on the owner (sqlite3ConnectionBlocked) —
// the OP_TableLock path, whose VDBE wrapper adds the table name.
func (r *Registry) AcquireTableLock(key TableLockKey, connID int64, write bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	group := r.sharedCaches[key.File]
	if group == nil {
		return nil
	}
	for _, l := range group.tableLocks {
		if l.conn != connID && l.root == key.Root && l.write != write {
			r.connectionBlockedLocked(connID, l.conn)
			return ErrTableLocked
		}
	}
	for i := range group.tableLocks {
		if group.tableLocks[i].conn == connID && group.tableLocks[i].root == key.Root {
			// C's setSharedCacheTableLock replaces a same-(conn, root) entry
			// (a READ lock upgrades to WRITE once the connection is the
			// writer); a WRITE downgrade is never requested by the VDBE.
			group.tableLocks[i].write = group.tableLocks[i].write || write
			return nil
		}
	}
	group.tableLocks = append(group.tableLocks, sharedTableLock{conn: connID, root: key.Root, write: write})
	return nil
}

// SharedWriteTxHeld reports whether connID currently holds the file's write
// transaction (the gate uses it to undo only what it registered itself).
func (r *Registry) SharedWriteTxHeld(fileKey string, connID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	group := r.sharedCaches[fileKey]
	return group != nil && group.writer == connID
}

// ReleaseTableLock drops ONE (connection, b-tree) lock — the statement-gate
// undo for a partially failed acquisition phase (a failed OP_TableLock ends
// only the statement; the transaction's earlier locks persist).
func (r *Registry) ReleaseTableLock(key TableLockKey, connID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	group := r.sharedCaches[key.File]
	if group == nil {
		return
	}
	for i := range group.tableLocks {
		if group.tableLocks[i].conn == connID && group.tableLocks[i].root == key.Root {
			group.tableLocks = append(group.tableLocks[:i], group.tableLocks[i+1:]...)
			return
		}
	}
}

// ReleaseSharedTxLocks drops connID's table locks and writer registration —
// btree.c clears the BtLock entries and pWriter when the transaction ends
// (sqlite3BtreeCommit / sqlite3BtreeRollback).
func (r *Registry) ReleaseSharedTxLocks(fileKey string, connID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	group := r.sharedCaches[fileKey]
	if group != nil {
		r.releaseSharedTxLocksLocked(group, connID)
	}
}

func (r *Registry) releaseSharedTxLocksLocked(group *SharedCache, connID int64) {
	if group.writer == connID {
		group.writer = 0
	}
	locks := group.tableLocks[:0]
	for _, l := range group.tableLocks {
		if l.conn != connID {
			locks = append(locks, l)
		}
	}
	group.tableLocks = locks
}

// --- Unlock-notify (notify.c) ---

// SetUnlockNotify registers connID's one unlock-notify callback, replacing
// any prior one. Called on the blocked connection after a lock failure:
//
//   - cb == nil unregisters (the method with no arguments, notify.c's
//     xNotify==0 path);
//   - with no blocking connection the callback fires IMMEDIATELY
//     (notify.c:170);
//   - a blocking chain that reaches back to connID is a deadlock (the walk
//     in sqlite3_unlock_notify) — ErrDeadlocked;
//   - otherwise the connection joins the blocked list.
func (r *Registry) SetUnlockNotify(connID int64, cb func()) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cb == nil {
		r.removeFromBlockedListLocked(connID)
		delete(r.blockedBy, connID)
		delete(r.unlockTarget, connID)
		delete(r.notifyCB, connID)
		return nil
	}
	if r.blockedBy[connID] == 0 {
		// The blocking transaction has been concluded (or never was): fire
		// immediately (notify.c "invoke the notify callback immediately").
		cb()
		return nil
	}
	// Deadlock walk: follow the blockers' registration chain; reaching
	// connID means the wait graph is cyclic (notify.c:183-188).
	for p := r.blockedBy[connID]; p != 0; p = r.unlockTarget[p] {
		if p == connID {
			return ErrDeadlocked
		}
	}
	r.notifyCB[connID] = cb
	r.unlockTarget[connID] = r.blockedBy[connID]
	r.removeFromBlockedListLocked(connID)
	r.blockedOrder = append(r.blockedOrder, connID)
	return nil
}

// ClearUnlockNotify cancels the registration.
func (r *Registry) ClearUnlockNotify(connID int64) {
	_ = r.SetUnlockNotify(connID, nil)
}

// ConnectionBlocked records that connID failed a shared-cache lock request
// against blocker (sqlite3ConnectionBlocked): the connection joins the
// blocked list on first failure and re-points at the current blocker.
func (r *Registry) ConnectionBlocked(connID, blocker int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectionBlockedLocked(connID, blocker)
}

func (r *Registry) connectionBlockedLocked(connID, blocker int64) {
	if connID == blocker {
		return
	}
	if r.blockedBy == nil {
		r.blockedBy = make(map[int64]int64)
		r.unlockTarget = make(map[int64]int64)
		r.notifyCB = make(map[int64]func())
	}
	if _, tracked := r.blockedBy[connID]; !tracked && r.unlockTarget[connID] == 0 {
		r.blockedOrder = append(r.blockedOrder, connID)
	}
	r.blockedBy[connID] = blocker
}

// ConnectionUnlocked runs when connID's transaction ends and its locks are
// released (sqlite3ConnectionUnlocked): every blocked connection waiting on
// connID (unlockTarget) fires its callback; connections merely blocked on it
// just have the block cleared. Firing walks the blocked list in order; the
// callbacks run OUTSIDE the registry mutex (harness scripts may re-enter).
func (r *Registry) ConnectionUnlocked(connID int64) {
	r.mu.Lock()
	var fire []func()
	for i := 0; i < len(r.blockedOrder); {
		conn := r.blockedOrder[i]
		if r.blockedBy[conn] == connID {
			delete(r.blockedBy, conn)
		}
		if r.unlockTarget[conn] == connID {
			delete(r.unlockTarget, conn)
			if cb := r.notifyCB[conn]; cb != nil {
				fire = append(fire, cb)
				delete(r.notifyCB, conn)
			}
		}
		if r.blockedBy[conn] == 0 && r.unlockTarget[conn] == 0 {
			r.blockedOrder = append(r.blockedOrder[:i], r.blockedOrder[i+1:]...)
			continue
		}
		i++
	}
	r.mu.Unlock()
	for _, cb := range fire {
		cb()
	}
}

// BlockedBy reports the connection connID is currently blocked on (0 = none).
func (r *Registry) BlockedBy(connID int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.blockedBy[connID]
}

func (r *Registry) removeFromBlockedListLocked(connID int64) {
	for i, conn := range r.blockedOrder {
		if conn == connID {
			r.blockedOrder = append(r.blockedOrder[:i], r.blockedOrder[i+1:]...)
			return
		}
	}
}
