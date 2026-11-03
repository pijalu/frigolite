package pager

import (
	"errors"
	"io/fs"
	"sync"
)

// The TEST io-error injector: os_unix.c's SQLITE_TEST counters
// (::sqlite_io_error_persist / ::sqlite_io_error_pending / the hit flag)
// ported as a process-global fault point consulted by every pager I/O site.
// malloc_common.tcl's ioerr_injectstart(persist, iFail) arms it: the iFail-th
// OS-layer I/O (and every following one in persistent mode) fails with
// SQLITE_IOERR ("disk I/O error").

// ErrIOErr is the simulated SQLITE_IOERR.
var ErrIOErr = errors.New("disk I/O error")

type ioerrInjector struct {
	mu      sync.Mutex
	pending int64
	persist bool
	hit     bool
	nFail   int64
}

// theIOErr is the process-global injector state.
var theIOErr ioerrInjector

// IOErrInjectStart arms the injector: the iFail-th I/O from now fails
// (transient) or every I/O from the iFail-th on (persistent).
func IOErrInjectStart(persist bool, iFail int64) {
	theIOErr.mu.Lock()
	defer theIOErr.mu.Unlock()
	theIOErr.pending = iFail
	theIOErr.persist = persist
	theIOErr.hit = false
}

// IOErrInjectStop disarms the injector and reports whether any I/O failed
// during the trial (malloc_common.tcl's ioerr_injectstop → $nfail).
func IOErrInjectStop() bool {
	theIOErr.mu.Lock()
	defer theIOErr.mu.Unlock()
	sv := theIOErr.hit
	theIOErr.pending = 0
	theIOErr.persist = false
	theIOErr.hit = false
	return sv
}

// IOErrNFail reports the total number of injected failures.
func IOErrNFail() int64 {
	theIOErr.mu.Lock()
	defer theIOErr.mu.Unlock()
	return theIOErr.nFail
}

// injectIOErr consults the injector: false means the I/O proceeds; true
// means this call (and every call in persistent mode until disarmed) must
// fail with ErrIOErr.
func injectIOErr() bool {
	theIOErr.mu.Lock()
	defer theIOErr.mu.Unlock()
	if theIOErr.pending == 0 {
		return false
	}
	theIOErr.pending--
	if theIOErr.pending == 0 || (theIOErr.pending < 0 && theIOErr.persist) {
		theIOErr.hit = true
		theIOErr.nFail++
		return true
	}
	return false
}

// faultFile wraps a pagerFile with the global io-error injector: every
// ReadAt/WriteAt/Sync/Truncate consults injectIOErr() first — the
// os_unix.c TEST block's position in the I/O path.
type faultFile struct {
	pagerFile
}

// newFaultFile wraps f (nil passes through).
func newFaultFile(f pagerFile) pagerFile {
	if f == nil {
		return nil
	}
	return &faultFile{pagerFile: f}
}

// ReadAt implements pagerFile.
func (f *faultFile) ReadAt(p []byte, off int64) (int, error) {
	if injectIOErr() {
		return 0, ErrIOErr
	}
	return f.pagerFile.ReadAt(p, off)
}

// WriteAt implements pagerFile.
func (f *faultFile) WriteAt(p []byte, off int64) (int, error) {
	if injectIOErr() {
		return 0, ErrIOErr
	}
	return f.pagerFile.WriteAt(p, off)
}

// Sync implements pagerFile.
func (f *faultFile) Sync() error {
	if injectIOErr() {
		return ErrIOErr
	}
	return f.pagerFile.Sync()
}

// Truncate implements pagerFile.
func (f *faultFile) Truncate(size int64) error {
	if injectIOErr() {
		return ErrIOErr
	}
	return f.pagerFile.Truncate(size)
}

// Stat implements pagerFile (fault-free: C's xFileSize is unmasked by the
// ioerr injector — TESTVFS_WRITE_MASK/READ_MASK do not cover it).
func (f *faultFile) Stat() (fs.FileInfo, error) {
	return f.pagerFile.Stat()
}
