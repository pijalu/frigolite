package pager

// walindexregistry.go — process-wide WAL-index registry (P7.WAL-G7 slice 1).
//
// Every frigolite connection in WAL mode attaches, through its walWriter, to
// one shared WALIndex per database file — the Go equivalent of SQLite's
// unixShmNode (os_unix.c): ONE shm fd per path, ONE set of wal-index page
// buffers, refcounted by the attached connections.
//
// Lock arbitration: os_unix.c's DMS protocol (unixLockSharedMemory) lets the
// FIRST connection to attach truncate the -shm to 3 bytes (a debugging aid —
// the wal-index is rebuilt by recovery), while later attaches share the live
// content. POSIX fcntl locks never conflict within one process, so the
// intra-process DMS decision is made here on the registry refcount (refs==0
// ⇒ fresh attach ⇒ truncate); the inter-process flock layer is slice 2
// (internal/pager/wallocks.go), which will co-manage the same fd.
//
// Concurrency: WALIndex.mu guards the page buffers and the aLock shadow.
// Mutations happen inside WriterSection (the slice-1 stand-in for the WRITER
// lock); readers take the same mutex through the exported primitives, which
// keeps -race clean while the double-buffer header protocol (write copy 1
// before copy 0; read copy 0 before copy 1 + checksum) is kept verbatim for
// cross-process faithfulness.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// shm-lock slot indices (wal.c L283-296): bytes 120..127 of the wal-index.
const (
	walLockWrite   = 0 // WAL_WRITE_LOCK
	walLockCkpt    = 1 // WAL_CKPT_LOCK
	walLockRecover = 2 // WAL_RECOVER_LOCK
)

// walReadLockIdx maps a read-mark index (0..4) to its shm lock slot
// (WAL_READ_LOCK(i) = 3+i).
func walReadLockIdx(i int) int { return 3 + i }

// WALIndex is the shared wal-index for one database path: the shm fd, the
// 32KiB wal-index page buffers (page 0 = header) and the per-slot in-process
// lock shadow (C's aLock[] mirrored from os_unix.c unixShmSystemLock).
type WALIndex struct {
	key   string // registry key (cleaned absolute db path)
	shmFd *os.File

	mu    sync.Mutex
	pages [][]byte     // wal-index pages, 32KiB each (page 0 = header)
	dirty map[int]bool // pages whose buffer changed but is not persisted
	aLock [8]int       // in-process lock shadow: 0 free, -1 exclusive, n>0 shared count
	refs  int          // connections attached to this index
}

// walIndexRegistry is the process-wide registry (lockreg.Global precedent,
// scoped to the pager package).
var walIndexRegistry = struct {
	mu      sync.Mutex
	entries map[string]*WALIndex
}{entries: make(map[string]*WALIndex)}

// acquireWALIndex returns the shared WALIndex for dbPath, creating it (and
// taking the DMS first-attach truncate) when no live connection holds one.
func acquireWALIndex(dbPath string) (*WALIndex, error) {
	key, err := filepath.Abs(dbPath)
	if err != nil {
		key = filepath.Clean(dbPath)
	}
	walIndexRegistry.mu.Lock()
	defer walIndexRegistry.mu.Unlock()
	if wi := walIndexRegistry.entries[key]; wi != nil {
		wi.mu.Lock()
		wi.refs++
		wi.mu.Unlock()
		return wi, nil
	}
	shmPath := key + "-shm"
	fd, err := os.OpenFile(shmPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("pager: open shm %s: unable to open database file", shmPath)
	}
	// DMS first attach (os_unix.c unixLockSharedMemory): truncate to 3 bytes
	// so a stale/corrupt wal-index from a dead generation is never trusted;
	// walIndexRecover rebuilds it from the "-wal" file.
	_ = fd.Truncate(3)
	wi := &WALIndex{
		key:   key,
		shmFd: fd,
		dirty: make(map[int]bool),
		refs:  1,
	}
	walIndexRegistry.entries[key] = wi
	return wi, nil
}

// release drops one connection reference; the last detach closes the shm fd
// and drops the registry entry (the next attach re-runs the DMS protocol).
func (w *WALIndex) release() {
	walIndexRegistry.mu.Lock()
	defer walIndexRegistry.mu.Unlock()
	w.mu.Lock()
	w.refs--
	last := w.refs <= 0
	if last {
		w.refs = 0
	}
	w.mu.Unlock()
	if last {
		if w.shmFd != nil {
			_ = w.shmFd.Close()
			w.shmFd = nil
		}
		w.pages = nil
		w.dirty = make(map[int]bool)
		delete(walIndexRegistry.entries, w.key)
	}
}

// RefCount reports the number of attached connections (test observability).
func (w *WALIndex) RefCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refs
}

// pageLocked returns wal-index page i, materializing it (loading persisted
// content when the shm file carries any) on first touch. Caller holds w.mu.
func (w *WALIndex) pageLocked(i int) []byte {
	if i < len(w.pages) && w.pages[i] != nil {
		return w.pages[i]
	}
	for len(w.pages) <= i {
		w.pages = append(w.pages, nil)
	}
	buf := make([]byte, WalIndexPageSize)
	if w.shmFd != nil {
		n, err := w.shmFd.ReadAt(buf, int64(i)*WalIndexPageSize)
		if (err != nil || n < WalIndexPageSize) && !w.dirty[i] {
			// Short content: the extension must be persisted so later
			// generations see a well-formed (zeroed) page.
			w.dirty[i] = true
		}
	} else {
		w.dirty[i] = true
	}
	w.pages[i] = buf
	return buf
}

// markDirtyLocked records that wal-index page i changed and must be
// persisted (persistAllLocked flushes at the end of the writer section).
func (w *WALIndex) markDirtyLocked(i int) { w.dirty[i] = true }

// persistAllLocked writes every dirty page buffer back to the -shm file
// (the mmap equivalent: C's wal-index writes land in the file immediately).
func (w *WALIndex) persistAllLocked() {
	if w.shmFd == nil {
		w.dirty = make(map[int]bool)
		return
	}
	for i := range w.dirty {
		if i < len(w.pages) && w.pages[i] != nil {
			_, _ = w.shmFd.WriteAt(w.pages[i], int64(i)*WalIndexPageSize)
		}
	}
	w.dirty = make(map[int]bool)
}

// WriterSection runs fn while holding the wal-index mutex — the slice-1
// stand-in for the WRITER lock (the flock layer arrives in slice 2). All
// header/hash mutations must happen inside one section.
func (w *WALIndex) WriterSection(fn func() error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := fn(); err != nil {
		return err
	}
	w.persistAllLocked()
	return nil
}

// ---------------------------------------------------------------------------
// Wal-index header protocol (wal.c walIndexTryHdr / walIndexWriteHdr).
// ---------------------------------------------------------------------------

// tryRefreshLocked ports walIndexTryHdr: read header copy 0, then copy 1;
// a mismatch is a dirty read; isInit and the checksum must verify. On
// success the decoded header replaces *h (changed reports whether it
// differed from the caller's cached copy). Caller holds w.mu.
func (w *WALIndex) tryRefreshLocked(h *WalIndexHdr) (changed, ok bool) {
	page0 := w.pageLocked(0)
	h1 := DecodeWalIndexHdr(page0[walIndexHdrOff0:])
	// walShmBarrier equivalent: under w.mu the second read cannot overtake
	// the first; the copy-order protocol is preserved for cross-process use.
	h2 := DecodeWalIndexHdr(page0[walIndexHdrOff1:])
	if h1 != h2 {
		return false, false // dirty read
	}
	if !h1.IsInit {
		return false, false // malformed header — probably all zeros
	}
	c1, c2 := walIndexHdrChecksum(page0[walIndexHdrOff0:])
	if c1 != h1.ACksum[0] || c2 != h1.ACksum[1] {
		return false, false // checksum does not match
	}
	if *h != h1 {
		*h = h1
		changed = true
	}
	return changed, true
}

// writeHdrLocked ports walIndexWriteHdr: stamp isInit/iVersion, recompute the
// header checksum, write copy 1 first, then copy 0. Caller holds w.mu.
func (w *WALIndex) writeHdrLocked(h *WalIndexHdr) {
	h.IsInit = true
	h.IVersion = WalIndexMaxVersion
	page0 := w.pageLocked(0)
	var buf [48]byte
	EncodeWalIndexHdr(h, buf[:])
	c1, c2 := walIndexHdrChecksum(buf[:])
	h.ACksum = [2]uint32{c1, c2}
	EncodeWalIndexHdr(h, buf[:])
	copy(page0[walIndexHdrOff1:walIndexHdrOff1+48], buf[:])
	copy(page0[walIndexHdrOff0:walIndexHdrOff0+48], buf[:])
	w.markDirtyLocked(0)
}

// ckptInfoLocked reads the WalCkptInfo from page 0. Caller holds w.mu.
func (w *WALIndex) ckptInfoLocked() WalCkptInfo {
	return DecodeWalCkptInfo(w.pageLocked(0))
}

// setCkptInfoLocked applies fn to the shared WalCkptInfo. Caller holds w.mu.
func (w *WALIndex) setCkptInfoLocked(fn func(*WalCkptInfo)) {
	page0 := w.pageLocked(0)
	info := DecodeWalCkptInfo(page0)
	fn(&info)
	EncodeWalCkptInfo(&info, page0)
	w.markDirtyLocked(0)
}

// ---------------------------------------------------------------------------
// Hash tables (wal.c walIndexAppend / walFindFrame / walCleanupHash).
// ---------------------------------------------------------------------------

// appendLocked ports walIndexAppend (wal.c L1295): record that database page
// iPage occupies WAL frame iFrame in the pgno→frame hash tables. mxFrame is
// the caller's header snapshot (used by the crashed-writer cleanup). Caller
// holds w.mu.
func (w *WALIndex) appendLocked(iFrame, iPage, mxFrame uint32) error {
	iHash := walFramePageOf(iFrame)
	loc := newWalHashLoc(w.pageLocked(iHash), iHash)
	idx := int(iFrame - loc.iZero)
	if idx <= 0 || idx > WalHashtableNSlot/2+1 {
		return errWalCorrupt
	}
	// First entry in this hash table: reset the mapping region.
	if idx == 1 {
		loc.zeroMapping()
		w.markDirtyLocked(iHash)
	}
	// Remnant of a writer that died mid-transaction: drop entries beyond
	// mxFrame before reusing the slot (wal.c walCleanupHash call site).
	if loc.pgno(idx-1) != 0 {
		w.walCleanupHashLocked(mxFrame)
	}
	iKey := walIndexHash(iPage)
	nCollide := 0
	for loc.slot(iKey) != 0 {
		nCollide++
		if nCollide > WalHashtableNSlot {
			return errWalCorrupt
		}
		iKey = walIndexNextHash(iKey)
	}
	loc.setPgno(idx-1, iPage)
	loc.setSlot(iKey, uint16(idx))
	w.markDirtyLocked(iHash)
	return nil
}

// walCleanupHashLocked ports walCleanupHash (wal.c L1233): zero every hash
// entry pointing beyond mxFrame in the table containing it. Caller holds w.mu.
func (w *WALIndex) walCleanupHashLocked(mxFrame uint32) {
	if mxFrame == 0 {
		return
	}
	iHash := walFramePageOf(mxFrame)
	loc := newWalHashLoc(w.pageLocked(iHash), iHash)
	iLimit := int(mxFrame - loc.iZero)
	if iLimit <= 0 {
		return
	}
	for i := 0; i < WalHashtableNSlot; i++ {
		if int(loc.slot(i)) > iLimit {
			loc.setSlot(i, 0)
		}
	}
	off, _ := walHashLocApgno(iHash)
	for i := off + 4*iLimit; i < walHashApgnoEnd; i++ {
		loc.page[i] = 0
	}
	w.markDirtyLocked(iHash)
}

// FindFrame ports walFindFrame (wal.c L3505, minFrame rule included): the
// largest frame ≤ mxFrame containing pgno, searched newest hash table first;
// 0 when the page is not in the WAL (or mxFrame is 0 — the WAL is ignored).
func (w *WALIndex) FindFrame(pgno, mxFrame, minFrame uint32) uint32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if mxFrame == 0 {
		return 0
	}
	iRead := uint32(0)
	iMinHash := walFramePageOf(minFrame)
	for iHash := walFramePageOf(mxFrame); iHash >= iMinHash; iHash-- {
		loc := newWalHashLoc(w.pageLocked(iHash), iHash)
		iKey := walIndexHash(pgno)
		for nCollide := WalHashtableNSlot; ; {
			iH := loc.slot(iKey)
			if iH == 0 {
				break
			}
			iFrame := uint32(iH) + loc.iZero
			if iFrame <= mxFrame && iFrame >= minFrame && loc.pgno(int(iH)-1) == pgno {
				iRead = iFrame
			}
			nCollide--
			if nCollide < 0 {
				return 0 // collision chain longer than the table: corrupt
			}
			iKey = walIndexNextHash(iKey)
		}
		if iRead != 0 {
			break
		}
	}
	return iRead
}

// ---------------------------------------------------------------------------
// aLock shadow (os_unix.c unixShmSystemLock in-process mirror). The flock
// layer over the same slots arrives in slice 2; these methods are the
// intra-process arbitration that POSIX fcntl locks cannot provide (R1).
// ---------------------------------------------------------------------------

// lockSlotsLocked applies op to shadow slots [idx, idx+n). Caller holds w.mu.
func (w *WALIndex) lockSlotsLocked(idx, n int, exclusive bool) bool {
	for i := idx; i < idx+n; i++ {
		if i < 0 || i >= len(w.aLock) {
			return false
		}
		if exclusive && w.aLock[i] != 0 {
			return false
		}
		if !exclusive && w.aLock[i] < 0 {
			return false
		}
	}
	for i := idx; i < idx+n; i++ {
		if exclusive {
			w.aLock[i] = -1
		} else {
			w.aLock[i]++
		}
	}
	return true
}

// unlockSlotsLocked releases shadow slots [idx, idx+n). Caller holds w.mu.
func (w *WALIndex) unlockSlotsLocked(idx, n int, exclusive bool) {
	for i := idx; i < idx+n; i++ {
		if i < 0 || i >= len(w.aLock) {
			continue
		}
		if exclusive {
			w.aLock[i] = 0
		} else if w.aLock[i] > 0 {
			w.aLock[i]--
		}
	}
}

// LockExclusive takes exclusive in-process locks on shm slots [idx, idx+n);
// false means BUSY (another connection holds any of the slots).
func (w *WALIndex) LockExclusive(idx, n int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lockSlotsLocked(idx, n, true)
}

// tryExclusiveLocked is LockExclusive for callers already inside a
// WriterSection (the wal-index mutex is held — e.g. the checkpoint PASS1/PASS2
// read-mark coordination, wal.c's walBusyLock calls which run under the
// WRITER lock).
func (w *WALIndex) tryExclusiveLocked(idx, n int) bool {
	return w.lockSlotsLocked(idx, n, true)
}

// releaseExclusiveLocked is UnlockExclusive for callers already inside a
// WriterSection.
func (w *WALIndex) releaseExclusiveLocked(idx, n int) {
	w.unlockSlotsLocked(idx, n, true)
}

// LockShared takes shared in-process locks on shm slots [idx, idx+n); false
// means BUSY (an exclusive holder is present).
func (w *WALIndex) LockShared(idx, n int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lockSlotsLocked(idx, n, false)
}

// UnlockExclusive releases exclusive locks on shm slots [idx, idx+n).
func (w *WALIndex) UnlockExclusive(idx, n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.unlockSlotsLocked(idx, n, true)
}

// UnlockShared releases shared locks on shm slots [idx, idx+n).
func (w *WALIndex) UnlockShared(idx, n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.unlockSlotsLocked(idx, n, false)
}
