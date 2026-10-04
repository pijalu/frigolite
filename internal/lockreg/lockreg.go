// Package lockreg tracks cross-connection database file locks for backup and
// locking semantics. Frigolite's pager has no OS-level file locking; the
// backup tests (sqlite3_backup) exercise SQLite's lock behavior (SQLITE_BUSY
// when another connection holds an exclusive lock or an open write
// transaction). This package provides a process-local registry keyed by
// database file path so a backup step can observe locks held by other
// connections in the same process.
package lockreg

import (
	"sync"
	"sync/atomic"
)

// Global is the process-wide lock registry shared by all connections. Tests
// run one test binary per testgen package, so process-global state is
// isolated between packages.
var Global = New()

// nextConnID is the monotonic connection-ID counter. Each engine instance
// gets a unique ID so its locks can be distinguished from other connections
// on the same file.
var nextConnID int64

// NewConnID returns a fresh unique connection ID. Atomic: connections open
// concurrently (parallel harness subtests), and the counter is package-level.
func NewConnID() int64 {
	return atomic.AddInt64(&nextConnID, 1)
}

// Registry holds the cross-connection lock state. All methods are safe for
// concurrent use (backup steps may run while another connection commits).
type Registry struct {
	mu sync.Mutex
	// connMarks / markConns / onlyConn maintain a per-connection count of
	// the connection-attributed lock marks it currently holds (every
	// Set* transition keeps them in step). ForeignMarks answers from the
	// counters — no per-statement walk of the mark maps. onlyConn is the
	// sole marked connection while markConns == 1.
	connMarks map[int64]int
	markConns int
	onlyConn  int64
	// anonMarks counts connection-less marks (backup locks, dotfile
	// sentinel refs), which ForeignMarks attributes to OTHER
	// conservatively.
	anonMarks int
	// exclusive maps a file path to the connection ID holding an EXCLUSIVE
	// lock (BEGIN EXCLUSIVE). Only one connection can hold it.
	exclusive map[string]int64
	// writeTx maps a file path to the set of connection IDs with an open
	// write transaction on that file.
	writeTx map[string]map[int64]bool
	// backupLock counts active backups whose destination is the file path.
	// A non-zero count blocks DETACH of that database ("database is locked").
	backupLock map[string]int
	// readTx tracks connections with an active prepared-statement read lock.
	readTx map[string]map[int64]int
	// sharedTx maps a file path to the set of connection IDs holding a
	// transaction-level SHARED lock (BEGIN + first read, held until
	// COMMIT/ROLLBACK — pager.c holds SHARED for the whole read txn).
	sharedTx map[string]map[int64]bool
	// persistentShared maps a file path to the set of connection IDs holding
	// a never-released SHARED lock (PRAGMA locking_mode=EXCLUSIVE: the pager
	// stops unlocking between transactions, src/pager.c
	// sqlite3PagerUnlock... the lock is held until the connection closes or
	// the mode reverts to normal).
	persistentShared map[string]map[int64]bool
	// pending maps a file path to the connection ID whose COMMIT failed the
	// EXCLUSIVE upgrade and now sits in PENDING: new SHARED acquisitions by
	// other connections are denied until the holder releases (lock2-1.7).
	pending map[string]int64
	// dotfileRefs counts dotfile-style connections currently holding a lock on
	// a path; the dotfile sentinel directory (path+".lock") exists iff the
	// count > 0. Mirrors SQLite's dotlock VFS: the sentinel is created on the
	// first lock and removed on the last unlock (os_unix.c dotlockLock/
	// dotlockUnlock).
	dotfileRefs map[string]int
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		connMarks:        make(map[int64]int),
		exclusive:        make(map[string]int64),
		writeTx:          make(map[string]map[int64]bool),
		backupLock:       make(map[string]int),
		readTx:           make(map[string]map[int64]int),
		sharedTx:         make(map[string]map[int64]bool),
		persistentShared: make(map[string]map[int64]bool),
		pending:          make(map[string]int64),
		dotfileRefs:      make(map[string]int),
	}
}

// markOn records one mark now held by connID (Registry.connMarks bookkeeping;
// see ForeignMarks).
func (r *Registry) markOn(connID int64) {
	r.connMarks[connID]++
	if r.connMarks[connID] == 1 {
		r.markConns++
		if r.markConns == 1 {
			r.onlyConn = connID
		}
	}
}

// markOff records one mark no longer held by connID.
func (r *Registry) markOff(connID int64) {
	switch n := r.connMarks[connID]; {
	case n <= 1:
		delete(r.connMarks, connID)
		r.markConns--
		if r.markConns == 1 {
			for c := range r.connMarks {
				r.onlyConn = c
				break
			}
		}
	default:
		r.connMarks[connID] = n - 1
	}
}

// SetExclusive records (on=true) or clears (on=false) an exclusive lock on
// path held by connID.
func (r *Registry) SetExclusive(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		if holder, ok := r.exclusive[path]; ok {
			if holder == connID {
				return
			}
			r.markOff(holder)
		}
		r.exclusive[path] = connID
		r.markOn(connID)
		return
	}
	if holder, ok := r.exclusive[path]; ok && holder == connID {
		delete(r.exclusive, path)
		r.markOff(connID)
	}
}

// SetWriteTx records (on=true) or clears (on=false) an open write transaction
// on path held by connID.
func (r *Registry) SetWriteTx(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		set := r.writeTx[path]
		if set == nil {
			set = make(map[int64]bool)
			r.writeTx[path] = set
		}
		if !set[connID] {
			set[connID] = true
			r.markOn(connID)
		}
		return
	}
	if set := r.writeTx[path]; set != nil && set[connID] {
		delete(set, connID)
		r.markOff(connID)
		if len(set) == 0 {
			delete(r.writeTx, path)
		}
	}
}

// AddBackupLock increments the backup lock count for path.
func (r *Registry) AddBackupLock(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backupLock[path]++
	r.anonMarks++
}

// RemoveBackupLock decrements the backup lock count for path.
func (r *Registry) RemoveBackupLock(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.backupLock[path]; ok {
		r.anonMarks--
		if n <= 1 {
			delete(r.backupLock, path)
			return
		}
		r.backupLock[path] = n - 1
	}
}

// HasBackupLock reports whether any active backup locks path (blocks DETACH).
func (r *Registry) HasBackupLock(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.backupLock[path] > 0
}

// ExclusiveLockedByOther reports whether another connection holds an
// exclusive lock on path. It returns the holder's connection ID and true.
func (r *Registry) ExclusiveLockedByOther(path string, self int64) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	holder, ok := r.exclusive[path]
	if !ok || holder == self {
		return 0, false
	}
	return holder, true
}

// WriteTxHeld reports whether any connection (self included) has an open
// write transaction on path.
func (r *Registry) WriteTxHeld(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.writeTx[path]) > 0
}

// WriteTxByOther reports whether a connection other than self has an open
// write transaction on path.
func (r *Registry) WriteTxByOther(path string, self int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for connID := range r.writeTx[path] {
		if connID != self {
			return true
		}
	}
	return false
}

// SetReadTx records or clears a read lock held by a prepared statement.
func (r *Registry) SetReadTx(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		set := r.readTx[path]
		if set == nil {
			set = make(map[int64]int)
			r.readTx[path] = set
		}
		if set[connID] == 0 {
			r.markOn(connID)
		}
		set[connID]++
		return
	}
	if set := r.readTx[path]; set != nil && set[connID] > 0 {
		if set[connID] == 1 {
			delete(set, connID)
			r.markOff(connID)
		} else {
			set[connID]--
		}
		if len(set) == 0 {
			delete(r.readTx, path)
		}
	}
}

// ReadTxByOther reports whether another connection holds a prepared read lock.
func (r *Registry) ReadTxByOther(path string, self int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for connID := range r.readTx[path] {
		if connID != self {
			return true
		}
	}
	return false
}

// ReadTxHeld reports whether THIS connection holds a prepared read lock on
// the file (the self-side counterpart of ReadTxByOther; PRAGMA lock_status
// reports the pager SHARED state while a stepped-but-unreset SELECT holds
// its read transaction open — lock-7.2).
func (r *Registry) ReadTxHeld(path string, connID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readTx[path][connID] > 0
}

// ClearConn removes every mark held by connID across all files (write
// transactions, exclusive locks, all read-lock levels). Called when a
// connection closes: close(2) drops the process's file locks, so a closed
// connection must not keep blocking others (savepoint7-3.x reopen loop).
func (r *Registry) ClearConn(connID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dropExclusiveHolder(r.exclusive, connID)
	dropExclusiveHolder(r.pending, connID)
	dropConnFromSets(r.writeTx, connID)
	dropConnFromSets(r.readTx, connID)
	dropConnFromSets(r.sharedTx, connID)
	dropConnFromSets(r.persistentShared, connID)
	// Every mark class above was connection-attributed, so the walk removed
	// exactly connMarks[connID] of them.
	if n := r.connMarks[connID]; n > 0 {
		r.markConns--
		delete(r.connMarks, connID)
		if r.markConns == 1 {
			for c := range r.connMarks {
				r.onlyConn = c
				break
			}
		}
	}
}

// dropExclusiveHolder removes a single-holder map entry held by connID; see
// ClearConn.
func dropExclusiveHolder(m map[string]int64, connID int64) {
	for path, holder := range m {
		if holder == connID {
			delete(m, path)
		}
	}
}

// dropConnFromSets removes connID from every per-path holder set, deleting
// sets that become empty; see ClearConn.
func dropConnFromSets[K comparable, V any](m map[string]map[K]V, connID K) {
	for path, set := range m {
		delete(set, connID)
		if len(set) == 0 {
			delete(m, path)
		}
	}
}

// SetSharedTx records (on=true) or clears (on=false) a transaction-level
// SHARED lock on path held by connID.
func (r *Registry) SetSharedTx(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		set := r.sharedTx[path]
		if set == nil {
			set = make(map[int64]bool)
			r.sharedTx[path] = set
		}
		if !set[connID] {
			set[connID] = true
			r.markOn(connID)
		}
		return
	}
	if set := r.sharedTx[path]; set != nil && set[connID] {
		delete(set, connID)
		r.markOff(connID)
		if len(set) == 0 {
			delete(r.sharedTx, path)
		}
	}
}

// SetPersistentShared records (on=true) a never-released SHARED lock on path
// held by connID (PRAGMA locking_mode=EXCLUSIVE). on=false clears it (mode
// reverted to normal).
func (r *Registry) SetPersistentShared(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		set := r.persistentShared[path]
		if set == nil {
			set = make(map[int64]bool)
			r.persistentShared[path] = set
		}
		if !set[connID] {
			set[connID] = true
			r.markOn(connID)
		}
		return
	}
	if set := r.persistentShared[path]; set != nil && set[connID] {
		delete(set, connID)
		r.markOff(connID)
		if len(set) == 0 {
			delete(r.persistentShared, path)
		}
	}
}

// PersistentSharedByOther reports whether a connection other than self holds
// a persistent SHARED lock on path (an EXCLUSIVE upgrade by self is blocked).
func (r *Registry) PersistentSharedByOther(path string, self int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for holder := range r.persistentShared[path] {
		if holder != self {
			return true
		}
	}
	return false
}

// SharedTxByOther reports whether a connection other than self holds a
// transaction-level SHARED lock on path.
func (r *Registry) SharedTxByOther(path string, self int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for connID := range r.sharedTx[path] {
		if connID != self {
			return true
		}
	}
	return false
}

// SetPending records (on=true) or clears (on=false) a PENDING lock on path
// held by connID (a writer whose COMMIT could not get EXCLUSIVE).
func (r *Registry) SetPending(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		if holder, ok := r.pending[path]; ok {
			if holder == connID {
				return
			}
			r.markOff(holder)
		}
		r.pending[path] = connID
		r.markOn(connID)
		return
	}
	if holder, ok := r.pending[path]; ok && holder == connID {
		delete(r.pending, path)
		r.markOff(connID)
	}
}

// PendingByOther reports whether another connection holds a PENDING lock on
// path (new SHARED acquisitions are denied — pager.c PENDING semantics).
func (r *Registry) PendingByOther(path string, self int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	holder, ok := r.pending[path]
	return ok && holder != self
}

// ConnHoldsLock reports whether connID currently holds ANY lock (shared,
// prepared-read, write, exclusive, or pending) on path. Used by the dotfile/
// flock locking styles to drive the sentinel directory and to implement the
// single-mutex lock matrix (os_unix.c dotlockLock / flockLock collapse every
// lock level into one EXCLUSIVE lock).
func (r *Registry) ConnHoldsLock(path string, connID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sharedTx[path][connID] ||
		r.readTx[path][connID] > 0 ||
		r.writeTx[path][connID] ||
		r.exclusive[path] == connID ||
		r.pending[path] == connID
}

// ConnLockedByOther reports whether any connection other than self currently
// holds ANY lock on path. The dotfile and flock VFSes collapse all lock levels
// into a single EXCLUSIVE mutex, so any holder excludes every other connection
// (readers and writers); this differs from the default unix VFS, where multiple
// SHARED readers may coexist.
func (r *Registry) ConnLockedByOther(path string, self int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exclusive[path] != 0 && r.exclusive[path] != self {
		return true
	}
	if r.pending[path] != 0 && r.pending[path] != self {
		return true
	}
	for cid := range r.writeTx[path] {
		if cid != self {
			return true
		}
	}
	for cid := range r.sharedTx[path] {
		if cid != self {
			return true
		}
	}
	for cid := range r.readTx[path] {
		if cid != self {
			return true
		}
	}
	return false
}

// SetDotfileHeld records (on=true) or clears (on=false) that connID holds a
// dotfile lock on path, adjusting the sentinel refcount. It returns whether
// the aggregate hold count for path is now non-zero (the sentinel directory
// should exist).
func (r *Registry) SetDotfileHeld(path string, connID int64, on bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.dotfileRefs[path]++
		r.anonMarks++
	} else {
		r.dotfileRefs[path]--
		r.anonMarks--
		if r.dotfileRefs[path] <= 0 {
			delete(r.dotfileRefs, path)
		}
	}
	return r.dotfileRefs[path] > 0
}

// DotfileHeld reports whether any connection currently holds a dotfile lock on
// path (the sentinel directory should exist).
func (r *Registry) DotfileHeld(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dotfileRefs[path] > 0
}

func (r *Registry) ReadTxByConn(path string, connID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readTx[path][connID] > 0
}

// SharedTxByConn reports whether connID currently holds a transaction-level
// SHARED lock on path (returned after a read inside a transaction). Used by the
// cross-connection read gate to exempt a connection that already holds SHARED
// from a PENDING block: PENDING denies NEW SHARED acquisitions only, an existing
// holder keeps reading (src/os_unix.c unixLock: the PENDING check runs on the
// SHARED acquire path, not on an already-held SHARED).
func (r *Registry) SharedTxByConn(path string, connID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sharedTx[path][connID]
}

// AnyMarks reports whether the registry holds any lock mark at all — for any
// path, any connection, of any kind. A registry without marks cannot fail any
// cross-connection check (every query is "locked/marked by OTHER"), so a
// statement's lock gate can skip its per-statement resolution entirely. The
// check is advisory: marks appearing after a false answer is harmless (the
// caller then runs the full check path).
func (r *Registry) AnyMarks() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.exclusive) > 0 ||
		len(r.writeTx) > 0 ||
		len(r.backupLock) > 0 ||
		len(r.readTx) > 0 ||
		len(r.sharedTx) > 0 ||
		len(r.persistentShared) > 0 ||
		len(r.pending) > 0 ||
		len(r.dotfileRefs) > 0
}

// ForeignMarks reports whether any lock mark in the registry belongs to a
// connection OTHER than self (the "held by OTHER" half of every
// cross-connection check). When it answers false, no
// *ByOther/ExclusiveLockedByOther-style check can fire for self, so a
// statement's lock gate can skip its per-statement key resolution and the
// whole check loop in one registry pass. Connection-less marks — the backup
// lock count and the dotfile reference count — are attributed to OTHER
// (conservative: they make the answer true and send the caller down the
// full check path, which then applies their actual rules). The check is
// advisory: marks appearing after a false answer is harmless (the caller
// then runs the full check path on its next statement).
//
// The answer reads the per-connection mark counters (Registry.connMarks /
// markConns / onlyConn / anonMarks, kept in step by every Set* transition),
// so the common single-connection case pays two integer compares instead of
// a walk of the mark maps.
func (r *Registry) ForeignMarks(self int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.anonMarks > 0 || r.markConns > 1 {
		return true
	}
	if r.markConns == 1 {
		return r.onlyConn != self
	}
	return false
}
