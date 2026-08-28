// Package lockreg tracks cross-connection database file locks for backup and
// locking semantics. Frigolite's pager has no OS-level file locking; the
// backup tests (sqlite3_backup) exercise SQLite's lock behavior (SQLITE_BUSY
// when another connection holds an exclusive lock or an open write
// transaction). This package provides a process-local registry keyed by
// database file path so a backup step can observe locks held by other
// connections in the same process.
package lockreg

import "sync"

// Global is the process-wide lock registry shared by all connections. Tests
// run one test binary per testgen package, so process-global state is
// isolated between packages.
var Global = New()

// nextConnID is the monotonic connection-ID counter. Each engine instance
// gets a unique ID so its locks can be distinguished from other connections
// on the same file.
var nextConnID int64

// NewConnID returns a fresh unique connection ID.
func NewConnID() int64 {
	nextConnID++
	return nextConnID
}

// Registry holds the cross-connection lock state. All methods are safe for
// concurrent use (backup steps may run while another connection commits).
type Registry struct {
	mu sync.Mutex
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
		exclusive:   make(map[string]int64),
		writeTx:     make(map[string]map[int64]bool),
		backupLock:  make(map[string]int),
		readTx:      make(map[string]map[int64]int),
		sharedTx:    make(map[string]map[int64]bool),
		pending:     make(map[string]int64),
		dotfileRefs: make(map[string]int),
	}
}

// SetExclusive records (on=true) or clears (on=false) an exclusive lock on
// path held by connID.
func (r *Registry) SetExclusive(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.exclusive[path] = connID
		return
	}
	if holder, ok := r.exclusive[path]; ok && holder == connID {
		delete(r.exclusive, path)
	}
}

// SetWriteTx records (on=true) or clears (on=false) an open write transaction
// on path held by connID.
func (r *Registry) SetWriteTx(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		if r.writeTx[path] == nil {
			r.writeTx[path] = make(map[int64]bool)
		}
		r.writeTx[path][connID] = true
		return
	}
	if set := r.writeTx[path]; set != nil {
		delete(set, connID)
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
}

// RemoveBackupLock decrements the backup lock count for path.
func (r *Registry) RemoveBackupLock(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.backupLock[path]; ok {
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
		if r.readTx[path] == nil {
			r.readTx[path] = make(map[int64]int)
		}
		r.readTx[path][connID]++
		return
	}
	if set := r.readTx[path]; set != nil {
		if set[connID] > 1 {
			set[connID]--
		} else {
			delete(set, connID)
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

// ClearConn removes every mark held by connID across all files (write
// transactions, exclusive locks, all read-lock levels). Called when a
// connection closes: close(2) drops the process's file locks, so a closed
// connection must not keep blocking others (savepoint7-3.x reopen loop).
func (r *Registry) ClearConn(connID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for path, holder := range r.exclusive {
		if holder == connID {
			delete(r.exclusive, path)
		}
	}
	for path, set := range r.writeTx {
		delete(set, connID)
		if len(set) == 0 {
			delete(r.writeTx, path)
		}
	}
	for path, set := range r.readTx {
		delete(set, connID)
		if len(set) == 0 {
			delete(r.readTx, path)
		}
	}
	for path, set := range r.sharedTx {
		delete(set, connID)
		if len(set) == 0 {
			delete(r.sharedTx, path)
		}
	}
	for path, holder := range r.pending {
		if holder == connID {
			delete(r.pending, path)
		}
	}
}

// SetSharedTx records (on=true) or clears (on=false) a transaction-level
// SHARED lock on path held by connID.
func (r *Registry) SetSharedTx(path string, connID int64, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		if r.sharedTx[path] == nil {
			r.sharedTx[path] = make(map[int64]bool)
		}
		r.sharedTx[path][connID] = true
		return
	}
	if set := r.sharedTx[path]; set != nil {
		delete(set, connID)
		if len(set) == 0 {
			delete(r.sharedTx, path)
		}
	}
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
		r.pending[path] = connID
		return
	}
	if holder, ok := r.pending[path]; ok && holder == connID {
		delete(r.pending, path)
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
	} else {
		r.dotfileRefs[path]--
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
