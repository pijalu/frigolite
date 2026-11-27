//go:build !windows

package pager

import (
	"io"
	"os"
	"syscall"
)

// shmFlockTry applies one non-blocking POSIX advisory lock operation to the
// shm fd at byte WalIndexLockOffset+idx (os_unix.c unixShmSystemLock:
// F_SETLK with l_start = ofst, l_len = n). EINTR is retried; EWOULDBLOCK
// (another process holds a conflicting lock) reports busy.
func shmFlockTry(fd *os.File, idx, n int, excl, unlock bool) error {
	typ := int16(syscall.F_RDLCK)
	if excl {
		typ = syscall.F_WRLCK
	}
	if unlock {
		typ = syscall.F_UNLCK
	}
	for {
		err := syscall.FcntlFlock(fd.Fd(), syscall.F_SETLK, &syscall.Flock_t{
			Type:   typ,
			Whence: int16(io.SeekStart),
			Start:  int64(WalIndexLockOffset + idx),
			Len:    int64(n),
		})
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return errWalBusy
		}
		return err
	}
}
