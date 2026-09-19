// Package pager — PRAGMA-facing configuration surface.
//
// Page size, reserved bytes, max page count, pending-byte override,
// auto-vacuum flag, journal mode selection and the test hooks
// (wal hook / wal fault / journal file-op hook) live here.
package pager

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"github.com/pijalu/frigolite/internal/storage"
)

// SetPageSize changes the pager's page size. It is only valid before any
// user data pages exist (PRAGMA page_size on an empty database); the caller
// is responsible for enforcing that. Page 1's Data buffer is resized so the
// header/payload layout stays consistent. For a file-backed database the
// file is truncated to zero so the next flush writes page 1 at the new size
// (a previously-written page 1 at the old size would otherwise leave a
// stale larger file, since flushPage only ever grows the file).
func (p *Pager) SetPageSize(ps uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pageSize = ps
	if p.file != nil {
		_ = p.file.Truncate(0)
		p.fileSize = 0
		// Page 1 (the schema page) already exists from Init; keep it as the
		// only page so the next AllocatePage starts at page 2. Resetting to 0
		// would make AllocatePage overwrite the schema page with a new table
		// page.
		if _, ok := p.pages[1]; ok {
			p.numPages = 1
		} else {
			p.numPages = 0
		}
	}
	if pg, ok := p.pages[1]; ok {
		newData := make([]byte, ps)
		copy(newData, pg.Data)
		pg.Data = newData
		// Reset page 1's b-tree header: page type leaf, empty (cell content
		// pointer at the usable end, as SQLite writes empty leaves). The old
		// header carried the previous page size's cell-content pointer, which
		// a free-space/cell-area check would reject at the new size.
		coff := 100 // page 1: content starts after the 100-byte header
		if len(newData) > coff+8 {
			newData[coff] = storage.PageTypeLeafTable
			for i := coff + 1; i < coff+5; i++ {
				newData[i] = 0
			}
			binary.BigEndian.PutUint16(newData[coff+5:coff+7], uint16(ps))
			newData[coff+7] = 0
		}
		p.dirty[1] = true
	}
}

// ResetToEmpty rewrites the database as a fresh, empty single-page database
// at the given page size (backup.c sqlite3BtreeNewDb → btree.c newDatabase,
// reached from backup_step's nSrcPage==0 branch): all cached pages are
// dropped, a canonical default header replaces the old one (both in the
// pager's header cache and inside page 1's first 100 bytes, so the on-disk
// image is self-consistent), page 1 is recreated as an empty schema leaf,
// and the file is truncated to exactly one page so the next Flush
// materializes precisely pageSize bytes.
func (p *Pager) ResetToEmpty(pageSize uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	p.pageSize = pageSize
	p.pages = make(map[uint32]*Page)
	p.dirty = make(map[uint32]bool)
	hdr := storage.DefaultHeader(pageSize).Encode()
	p.header = hdr
	p.numPages = 1
	pg := &Page{PageNum: 1, Data: make([]byte, pageSize)}
	copy(pg.Data[:HeaderSize], hdr)
	// Empty leaf-table b-tree: type byte at the content offset and the
	// cell-content pointer at the usable end (an empty leaf whose pointer
	// is 0 looks crash-written — "free space corruption").
	coff := HeaderSize
	pg.Data[coff] = storage.PageTypeLeafTable
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(pageSize))
	p.pages[1] = pg
	p.dirty[1] = true
	if p.file != nil {
		end := int64(pageSize)
		if err := p.file.Truncate(end); err == nil {
			p.fileSize = end
		}
	}
}

// SetAutoVacuum toggles pointer-map page reservation for subsequent
// AllocatePage calls (btree.c sqlite3BtreeSetAutoVacuum). SQLite applies a
// mode change immediately only while the database is still empty; callers
// are responsible for that check.
//
// S6 P8.INCRVACUUM fix: when turning auto-vacuum ON, also stamp the
// header's LargestBTreePage (offset 52) to 1 — the schema btree
// lives at page 1 and is the largest b-tree page. SQLite does this
// in btree.c::newDatabase (line 3537: `put4byte(&data[36+4*4], pBt->autoVacuum)`).
// The Go engine splits that work: schema.Init allocates page 1 (in
// the pager cache) and writes a default header if none exists, but
// never wrote the LargestBTreePage slot. A fresh DB closed right
// after `PRAGMA auto_vacuum=1` therefore lost the auto-vacuum flag
// on reopen — incrvacuum-12.4 expected 1, got 0; 12.5 then read
// EOF on SELECT * FROM sqlite_master because the engine treated
// the empty file as not-a-database.
//
// Mirroring C: setting on=true writes 1 to header[52:56] (page 1
// is the largest b-tree page) and 0 to header[64:68] (the
// incremental-vacuum flag — FULL auto-vacuum, not INCREMENTAL).
// The on=false path leaves both fields at 0 (no auto-vacuum).
//
// The write happens whether the header is the in-memory default
// (file was opened empty) or the on-disk header was read at Open.
// Page 1 is marked dirty so the commit flushes the change. For
// files already past page 1 the LargestBTreePage slot is meaningful
// (tracks the maximum rootpage); the engine's PRAGMA gate
// (`NumPages() <= 1`) prevents reaching here with a larger file.
func (p *Pager) SetAutoVacuum(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.autoVacuum = on
	if !on {
		return
	}
	if p.header == nil || len(p.header) < HeaderSize {
		// File opened empty (header not yet allocated by schema.Init).
		// Synthesize a default header so the LargestBTreePage write
		// has somewhere to land. schema.Init's own SetHeader call will
		// overwrite this with a fresh default — but the auto-vacuum
		// flag we set here will be lost (schema.Init's default has
		// LargestBTreePage=0). The schema.init path is the right
		// place to fix that, NOT here. (See schema.Init: it copies
		// the pager's autoVacuum flag into the header when
		// allocating page 1.)
		//
		// The next call to schema.Init (e.g. by the first statement
		// that touches the btree) reads the autoVacuum flag and
		// stamps header[52:56]=1 alongside the page-1 allocation.
		// Returning here is correct: the in-memory flag is set, and
		// the schema init path will write the header.
		return
	}
	if binary.BigEndian.Uint32(p.header[52:56]) == 0 {
		binary.BigEndian.PutUint32(p.header[52:56], 1)
		p.dirty[1] = true
		if pg, ok := p.pages[1]; ok && pg != nil && len(pg.Data) >= HeaderSize {
			copy(pg.Data[:HeaderSize], p.header)
		}
	}
	if binary.BigEndian.Uint32(p.header[64:68]) != 0 {
		binary.BigEndian.PutUint32(p.header[64:68], 0)
		p.dirty[1] = true
		if pg, ok := p.pages[1]; ok && pg != nil && len(pg.Data) >= HeaderSize {
			copy(pg.Data[:HeaderSize], p.header)
		}
	}
}

// SetInTransaction toggles the transaction flag (P8.INCRVACUUM.phase7).
// While true, AllocatePage skips chain consumption (extending the file
// instead) so that a ROLLBACK does not leave popped pages without an
// owner ("Page N: never used" orphans). The exec engine calls this at
// BEGIN (true) and at COMMIT/ROLLBACK (false).
func (p *Pager) SetInTransaction(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inTransaction = on
}

// AutoVacuum reports whether pointer-map pages are being reserved.
func (p *Pager) AutoVacuum() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.autoVacuum
}

// SetPendingByte overrides the PENDING_BYTE lock-byte offset for this
// pager. The SQLite C test harness installs a non-default value
// (typically 0x10000, page 65 at 1024-byte page size) so file-size
// checks in autovacuum-9.3 / 9.5 / corrupt2 / lock4 can observe a
// small expected value without creating a 1GB database. The override
// is consulted by AllocatePage / AllocatePageLE when deciding whether
// a candidate page lands on the reserved pending-byte slot. A value
// of 0 restores the production default (0x40000000).
//
// Returns the previous offset (the override, or the production default
// when none was installed) — the value sqlite3_test_control_pending_byte
// hands back so tests can restore it.
func (p *Pager) SetPendingByte(byteOffset uint32) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.pendingByteOverride
	if prev == 0 {
		prev = 0x40000000
	}
	p.pendingByteOverride = byteOffset
	return prev
}

// PendingBytePage returns the page number holding the PENDING_BYTE lock
// byte, honouring any SetPendingByte override.
func (p *Pager) PendingBytePage() uint32 {
	return p.pendingBytePageFor()
}

// Serialize returns a contiguous copy of the database image: numPages ×
// pageSize bytes, page 1 carrying the live 100-byte header (memdb.c
// sqlite3_serialize: sz = page_count × pageSize; each page copied from the
// pager cache, missing pages zero-filled). The caller owns the slice.

// DefaultMaxPageCount is the default PRAGMA max_page_count cap — the
const DefaultMaxPageCount = 1073741823

// MaxPageCount returns the current PRAGMA max_page_count cap. The
// zero-value field (no explicit cap) reports the documented default.
// Mirrors pager.c::sqlite3PagerMaxPageCount returning pPager->mxPgno.
func (p *Pager) MaxPageCount() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.maxPageCount == 0 {
		return DefaultMaxPageCount
	}
	return p.maxPageCount
}

// SetMaxPageCount sets the PRAGMA max_page_count cap. Mirrors
// pager.c::sqlite3PagerMaxPageCount: a value of 0 (or negative, already
// rejected by the pragma parse) is ignored and the current cap is kept —
// the pragma then behaves as a getter (`PRAGMA max_page_count=0` echoes
// the current value). AllocatePageMode enforces the cap on every new page
// allocation.
func (p *Pager) SetMaxPageCount(n uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n == 0 {
		return
	}
	p.maxPageCount = n
}

// SetReservedBytes records a REQUESTED per-page reserved-space byte count
// (sqlite3_file_control SQLITE_FCNTL_RESERVE_BYTES → btree.c
// sqlite3BtreeSetPageSize(-1, nRes)). Like a requested page size, the value
// is NOT written to the file immediately: header byte 20 keeps its current
// value until the next VACUUM materializes the requested reserve
// (reservebytes.test 1.2.1 reads 00 after requesting 8; 1.3.5 reads 08 only
// after VACUUM).
func (p *Pager) SetReservedBytes(n uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if uint32(p.pageSize) < n {
		return
	}
	p.requestedReserve = n
}

// RequestedReserve returns the pending reserve request (0 when none).
func (p *Pager) RequestedReserve() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.requestedReserve
}

// ApplyReservedBytes materializes the per-page reserved-space byte count:
// header byte 20 and the usable-size base change together, on page 1 and in
// the cached header (vacuum.c applies the requested reserve when the rebuilt
// image is written back).
func (p *Pager) ApplyReservedBytes(n uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if uint32(p.pageSize) < n {
		return
	}
	p.reserved = n
	p.requestedReserve = 0
	if p.header != nil && len(p.header) >= 21 {
		p.header[20] = byte(n)
		p.dirty[1] = true
		if pg, ok := p.pages[1]; ok && pg != nil && len(pg.Data) >= HeaderSize {
			copy(pg.Data[:HeaderSize], p.header)
		}
	}
}

// ReservedBytes reports the per-page reserved-space byte count.
func (p *Pager) ReservedBytes() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.reserved
}

// effectiveMaxPageCountLocked returns the cap AllocatePageMode enforces
// (the zero-value field means the documented default). Caller holds p.mu.
func (p *Pager) effectiveMaxPageCountLocked() uint32 {
	if p.maxPageCount == 0 {
		return DefaultMaxPageCount
	}
	return p.maxPageCount
}

// ReadAutoVacuumFromHeader reports the auto-vacuum mode stored in the
// on-disk database header. SQLite encodes the largest root btree page
// number at header[52:56] in autovacuum mode; the mode is FULL when
// the field is non-zero. Returns 0 (NONE) when the header is missing
// or corrupted. Used by the engine on Open to restore the auto_vacuum
// mode across connection restarts.
func (p *Pager) ReadAutoVacuumFromHeader() int {
	p.mu.RLock()
	h := p.header
	p.mu.RUnlock()
	if len(h) < 56 {
		return 0
	}
	// meta[3] = header[52:56]. In FULL autovacuum mode, btreeCreateTable
	// writes the page number of each new table root here; a non-zero value
	// means autovacuum is on. INCREMENTAL mode is opt-in via
	// header[64:68] (meta[6]); the value 1..N maps to a free page count
	// threshold, but for "is autovacuum on?" we only need != 0.
	largest := binary.BigEndian.Uint32(h[52:56])
	if largest != 0 {
		return 1 // FULL
	}
	// meta[6] = header[64:68] is the incremental vacuum mode. If set,
	// return 2 (INCREMENTAL).
	if len(h) >= 68 {
		incr := binary.BigEndian.Uint32(h[64:68])
		if incr != 0 {
			return 2
		}
	}
	return 0
}

// PtrmapPageNo returns the pointer-map page number covering pgno (btree.c
// ptrmapPageno): usableSize/5+1 pages are mapped per pointer-map page, the
// first being page 2. Returns 0 for pgno < 2.
func PtrmapPageNo(pgno, pageSize uint32) uint32 {
	if pgno < 2 {
		return 0
	}
	nPer := pageSize/5 + 1
	ret := ((pgno-2)/nPer)*nPer + 2
	// btree.c: a pointer-map page never lands on the pending-byte page.
	if ret == pendingBytePage(pageSize) {
		ret++
	}
	return ret
}

// pendingBytePage is the page holding the PENDING_BYTE lock byte
// (1073741824), which SQLite reserves and never uses (btree.c
// PENDING_BYTE_PAGE). The PENDING_BYTE offset is fixed at 0x40000000
// (1073741824) by the SQLite source; for the test harness, see
// Pager.SetPendingByte / Pager.PendingBytePage.
func pendingBytePage(pageSize uint32) uint32 {
	return 1073741824/pageSize + 1
}

// pendingBytePageFor returns the page holding the PENDING_BYTE lock byte
// for the given pager, honouring a per-pager override set by the SQLite
// test harness via sqlite3_test_control_pending_byte. Without an
// override the value matches the production default.
//
// The caller MUST hold p.mu (RLock or Lock); the function does not
// re-acquire the lock because callers that already hold the write lock
// (AllocatePageMode, AllocatePageLE, pickNextFreePageLocked) would
// otherwise self-deadlock. The lock-free read of a uint32 is safe
// under the mutex.
func (p *Pager) pendingBytePageFor() uint32 {
	if p.pendingByteOverride != 0 {
		return p.pendingByteOverride/p.pageSize + 1
	}
	return pendingBytePage(p.pageSize)
}

// IsPtrmapPageNo reports whether pgno itself is a pointer-map page.
func IsPtrmapPageNo(pgno, pageSize uint32) bool {
	return pgno >= 2 && PtrmapPageNo(pgno, pageSize) == pgno
}

// SetJournalMode switches the pager's journal mode. "wal" enables the WAL
// write path: it creates the "-wal"/"-shm" companions, writes a WAL header,
// and routes future commits through the WAL writer (the main file is then
// only updated by an explicit Checkpoint). Any other mode (the default)
// keeps the legacy direct-flush path unchanged.
func (p *Pager) SetJournalMode(mode string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "wal":
		if p.wal != nil {
			return nil // already in WAL mode
		}
		if p.file == nil {
			return fmt.Errorf("pager: cannot enable WAL on in-memory pager")
		}
		// Switching from PERSIST/TRUNCATE to WAL: close and unlink the
		// existing rollback-journal file (it is no longer the active
		// sidecar). Fire xClose + xDelete via the testvfs hook.
		prev := p.journalMode
		if p.journalFile != nil && (prev == "persist" || prev == "truncate") {
			jpath := p.journalFile.Name()
			_ = p.journalFile.Close()
			p.journalFile = nil
			if h := p.journalFileOpHookFn(); h != nil {
				h("xClose", jpath)
			}
			if h := p.journalFileOpHookFn(); h != nil {
				h("xDelete", jpath)
			}
			_ = os.Remove(jpath)
		}
		w, err := openWal(p, p.path, p.pageSize)
		if err != nil {
			return err
		}
		p.wal = w
		p.journalMode = "wal"
		return nil
	case "delete", "truncate", "persist", "memory", "off", "wal2":
		// Legacy rollback-journal modes. The mode is recorded so that
		// PRAGMA journal_mode reports it on read-back; the commit path
		// honours it when materialising / disposing of the rollback
		// journal (see the transaction commit/rollback handlers). "delete"
		// is the SQLite default and keeps the legacy direct-flush path.
		if p.wal != nil {
			p.wal.Close()
			p.wal = nil
		}
		// Switching journal modes may need to close + unlink an
		// already-open journal file (the previous mode opened it under
		// its own policy, but the new mode may want to start fresh or
		// handle it differently). We close + unlink in every
		// cross-mode transition (not just PERSIST/TRUNCATE → *) so a
		// DELETE-mode implicit open (the engine opens one on the first
		// write of an empty database) does not leak into a subsequent
		// PERSIST/TRUNCATE session — otherwise the new mode's first
		// transaction sees the stale DELETE-mode file already open and
		// skips xOpen (journal2.test 2.2 — PRAGMA persist; CREATE TABLE
		// → expected xOpen, but the file is already open).
		prev := p.journalMode
		if p.journalFile != nil && m != prev {
			jpath := p.journalFile.Name()
			_ = p.journalFile.Close()
			p.journalFile = nil
			if h := p.journalFileOpHookFn(); h != nil {
				h("xClose", jpath)
			}
			if h := p.journalFileOpHookFn(); h != nil {
				h("xDelete", jpath)
			}
			_ = os.Remove(jpath)
		}
		p.journalMode = m
		return nil
	default:
		// SQLite treats an unrecognised journal mode token as a no-op:
		// the current mode is left unchanged and the statement returns
		// the current mode without an error (test/journal.c jrnlmode-1.8).
		return nil
	}
}

// JournalMode reports the active journal mode ("wal", "delete", "truncate",
// "persist", "memory", "off", or "" which the caller maps to "delete").
func (p *Pager) JournalMode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.journalMode
}

// SetJournalSizeLimit records the PRAGMA journal_size_limit cap (bytes) for
// this database. A negative value means unlimited (the journal keeps its full
// content after a PERSIST commit); 0 truncates the journal to zero; a positive
// value truncates it down to that many bytes. SQLite's default is 32768
// (pragma.c journalSizeLimit). The value is stored verbatim so the getter can
// echo it.
func (p *Pager) SetJournalSizeLimit(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.journalSizeLimit = n
}

// JournalSizeLimit reports the recorded PRAGMA journal_size_limit cap.
func (p *Pager) JournalSizeLimit() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.journalSizeLimit
}

// SetPendingJournalMode records a journal-mode change requested while a
// transaction was open. pager.c defers the actual switch until the transaction
// ends (sqlite3BtreeSetJournalMode sets pBt->pendingJournalMode and
// btreeEndTransaction applies it). An unrecognised mode is ignored (no-op),
// matching the setter's behaviour outside a transaction.
func (p *Pager) SetPendingJournalMode(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "delete", "truncate", "persist", "memory", "off", "wal", "wal2":
		p.pendingJournalMode = m
	default:
		// unrecognised token: leave the pending change unset
	}
}

// ApplyPendingJournalMode commits a deferred journal-mode change (recorded by
// SetPendingJournalMode) at transaction end. It is a no-op when no change is
// pending. Called from the engine's COMMIT/ROLLBACK path for every database.
func (p *Pager) ApplyPendingJournalMode() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pendingJournalMode == "" {
		return
	}
	if p.wal != nil && p.pendingJournalMode != "wal" {
		p.wal.Close()
		p.wal = nil
	}
	p.journalMode = p.pendingJournalMode
	p.pendingJournalMode = ""
}

// SetWalHook registers the sqlite3_wal_hook callback, invoked after each WAL
// commit with (frames appended, frames checkpointed).
func (p *Pager) SetWalHook(fn func(nLog, nCkpt int) int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.walHook = fn
}

// SetWalFault installs an I/O fault injector for WAL writes (test_syscall
// equivalent). When fn returns a non-nil error for a given operation, the WAL
// writer aborts that write with it. Pass nil to clear.
func (p *Pager) SetWalFault(fn func(op string) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.walFault = fn
}

// SetJournalFileOpHook installs a callback that fires for xOpen/xClose/xDelete
// events on the "test.db-journal" sidecar. The hook is the testvfs equivalent
// for the journal file: frigolite does not have a full VFS plugin system, but
// the journal2 TCL test suite needs to observe the OS-level sequence of file
// operations on the journal sidecar. Pass nil to clear.
//
// The hook is called with the operation name ("xOpen", "xClose", "xDelete")
// and the absolute path of the journal file. It fires synchronously under
// p.mu, so the hook should be lightweight (e.g. appending to a string).
func (p *Pager) SetJournalFileOpHook(fn func(op, path string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.journalFileOpHook = fn
}
