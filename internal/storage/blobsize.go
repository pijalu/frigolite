package storage

import "sync"

// maxBlobsize mirrors SQLite's test-only global sqlite3_max_blobsize
// (src/vdbe.c): the byte size of the largest MEM_Blob/MEM_Str used by a
// statement. The TCL test harness links this variable
// (src/test1.c Tcl_LinkVar) and resets it before statements whose transient
// buffer usage it wants to assert (zeroblob deferred-materialization tests).
var maxBlobsize struct {
	sync.Mutex
	n int
}

// MaxBlobsize returns the current max-blobsize instrumentation value.
func MaxBlobsize() int {
	maxBlobsize.Lock()
	defer maxBlobsize.Unlock()
	return maxBlobsize.n
}

// SetMaxBlobsize overwrites the tracker, matching a TCL
// `set ::sqlite3_max_blobsize N` write through the linked variable.
func SetMaxBlobsize(n int) {
	maxBlobsize.Lock()
	defer maxBlobsize.Unlock()
	maxBlobsize.n = n
}

// updateMaxBlobsize records n when it exceeds the current maximum
// (SQLite UPDATE_MAX_BLOBSIZE).
func updateMaxBlobsize(n int) {
	maxBlobsize.Lock()
	defer maxBlobsize.Unlock()
	if n > maxBlobsize.n {
		maxBlobsize.n = n
	}
}
