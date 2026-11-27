//go:build windows

package pager

import (
	"os"
	"syscall"
	"unsafe"
)

// Win32 LockFileEx flags and error codes (winbase.h / winerror.h). The pager
// has no external dependencies, so the two kernel32 entry points are resolved
// lazily instead of importing golang.org/x/sys/windows.
const (
	lockFileFailImmediately = 0x00000001
	lockFileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
	errorIOPending          = syscall.Errno(997)
)

var (
	kernel32LockFileEx   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	kernel32UnlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

// shmFlockTry applies one non-blocking byte-range lock to the shm fd at
// WalIndexLockOffset+idx — the Win32 counterpart of os_unix.c's
// unixShmSystemLock. LockFileEx with LOCKFILE_FAIL_IMMEDIATELY returns
// ERROR_LOCK_VIOLATION (or ERROR_IO_PENDING) when another process holds a
// conflicting range; both map to SQLITE_BUSY. EINTR has no Win32 analogue.
func shmFlockTry(fd *os.File, idx, n int, excl, unlock bool) error {
	h := syscall.Handle(fd.Fd())
	ov := syscall.Overlapped{Offset: uint32(WalIndexLockOffset + idx)}
	if unlock {
		r, _, callErr := kernel32UnlockFileEx.Call(uintptr(h), 0, uintptr(n), 0, uintptr(unsafe.Pointer(&ov)))
		if r == 0 && callErr != nil {
			return callErr
		}
		return nil
	}
	flags := uintptr(lockFileFailImmediately)
	if excl {
		flags |= lockFileExclusiveLock
	}
	r, _, callErr := kernel32LockFileEx.Call(uintptr(h), flags, 0, uintptr(n), 0, uintptr(unsafe.Pointer(&ov)))
	if r != 0 {
		return nil
	}
	if callErr == errorLockViolation || callErr == errorIOPending {
		return errWalBusy
	}
	if callErr != nil {
		return callErr
	}
	return errWalBusy
}
