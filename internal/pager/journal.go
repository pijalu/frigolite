// Package pager — rollback journal file machinery.
//
// This file ports the subset of SQLite's pager.c rollback journal needed by
// the P7.WAL-E test suites (journal1/journal2/journal3/jrnlmode/mjournal):
//
//   - On the first page write under a non-memory/non-off/non-wal transaction,
//     a "test.db-journal" sidecar is created (or appended) in the same
//     directory as the main database file. The sidecar inherits the main
//     database file's Unix mode bits (journal3.test 1.2.x.4).
//   - A SQLite-format journal header is written first: 8-byte magic
//     (0xd9d505f920a163d7), 4-byte nRec placeholder, 4-byte random cksumInit,
//     4-byte initial database size in pages, 4-byte sector size, 4-byte
//     page size, then zero-pad to one sector (typically 512 bytes).
//   - For every dirty page flushed to the main database, the BEFORE image
//     is recorded in the journal as: 4-byte page number, page data, 8-byte
//     checksum (the cksumInit XOR'd into a running sum as each page is
//     written, like SQLite's walChecksumBytes for the rollback journal).
//   - At COMMIT, the mode decides what happens to the journal sidecar:
//     DELETE  — unlink the file (the legacy default; recovers space).
//     TRUNCATE — keep the file but truncate to 0 bytes (pager.c:5313).
//     PERSIST  — keep the file; truncate to journal_size_limit
//     (positive N), 0 (N=0), or leave intact (N<0). The PERSIST
//     file is reused on the next transaction (its header is
//     overwritten; previous records are overwritten by the new
//     cksumInit random).
//     MEMORY/OFF — no journal file is created (the rollback buffer is the
//     in-memory dirty-page set; on commit the pages are flushed
//     to the main database and discarded; on rollback the
//     dirty set is dropped and the cache is restored from
//     snapshot).
//   - At ROLLBACK, the journal records are replayed in reverse to restore
//     the before-images, and the journal file is unlinked. The Pager's
//     dirty set is dropped (the in-flight transaction is undone).
//   - On Open, if a "test.db-journal" file exists with a valid SQLite
//     journal header AND the journal contains page records AND the main
//     database file's change counter is older than the journal, the engine
//     runs hot-journal recovery: the records are replayed into the page
//     cache (NOT the main file) and the journal is then unlinked. This
//     matches test/journal1.test 1.2 ("a leftover journal from prior
//     databases do not try to rollback into new databases").
//
// The VFS layer is the host OS (no shim). A testvfs-equivalent hook
// (FileOpHook) captures xOpen/xClose/xDelete events on the *-journal
// sidecar so the journal2 test suite can assert the sequence of file
// operations; setting the hook to a non-nil function enables the log.
package pager

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/pijalu/frigolite/internal/lockreg"
)

// SQLite journal header magic (pager.c aJournalMagic) is defined in
// jrnlview.go and reused here.

// Default sector size for journal header padding (pager.c default; most
// platforms have 512-byte sectors and SQLite falls back to that).
const defaultSectorSize = 512

// defaultJournalFileOpHook is the process-wide fallback hook for
// journal-sidecar xOpen/xClose/xDelete events. When a Pager's own
// journalFileOpHook is nil, it consults this default. This lets a single
// hook installation (via SetDefaultJournalFileOpHook) observe every
// pager's journal-sidecar events without having to register the hook on
// every connection individually — important for tests that open multiple
// connections. Nil in production.
var defaultJournalFileOpHook func(op, path string)

// SetDefaultJournalFileOpHook installs (or clears, when fn is nil) the
// process-wide fallback hook consulted by every Pager's journal-sidecar
// open/close/delete path. Pass nil to clear.
func SetDefaultJournalFileOpHook(fn func(op, path string)) {
	defaultJournalFileOpHook = fn
}

// journalFileOpHook returns the effective hook for this Pager: the
// pager-local hook if set, otherwise the process-wide default. Called
// under p.mu.
func (p *Pager) journalFileOpHookFn() func(op, path string) {
	if p.journalFileOpHook != nil {
		return p.journalFileOpHook
	}
	return defaultJournalFileOpHook
}

// recoverHotJournal replays a hot rollback journal into the page cache at
// Open time (pager.c pagerPlayback / hasHotJournal). A journal is HOT when
// it holds page records for an uncommitted transaction: magic valid (or
// pre-sync zeroed), a nonzero record stream, and the main file's change
// counter older than (or equal to) the journal's dbOrigSize generation.
// Stale journals (bad magic, empty, dbOrigSize mismatch with the current
// file size, or main-file change counter NEWER than the journal) are
// unlinked WITHOUT playback (journal1.test 1.2: a leftover journal from a
// prior database must not roll back into a new database). After playback
// the journal is unlinked. The caller must NOT hold p.mu.
func recoverHotJournal(p *Pager, dbPath string) error {
	// C lock protocol (pager.c hasHotJournal -> pagerPlayback): playing back
	// a hot journal requires an EXCLUSIVE file lock, which a live writer's
	// transaction denies — the journal belongs to that in-flight transaction
	// and must be left untouched (no playback, no unlink). Frigolite journals
	// eagerly at BEGIN (C defers journal creation to the first spill/sync),
	// so without this registry gate a second connection's Open would treat
	// the live journal as hot: the dbOrigSize mismatch against a not-yet-
	// materialized main file made it unlink the writer's journal, and a
	// later ROLLBACK could then truncate the writer's file out from under
	// it (pcache.test 1.x).
	if lockreg.Global.WriteTxHeld(dbPath) {
		return nil
	}
	jpath := journalPath(dbPath)
	pages, hot := readHotJournal(p, jpath)
	if !hot {
		return nil
	}
	// HOT: replay before-images into the page cache (newest first so the
	// oldest before-image wins), then unlink the journal.
	p.mu.Lock()
	defer p.mu.Unlock()
	replayHotJournalPages(p, pages)
	p.dirty = make(map[uint32]bool)
	p.refreshKnownFileStamp()
	_ = os.Remove(jpath)
	return nil
}

// readHotJournal reads and validates a candidate hot journal at jpath,
// returning its page records and whether it is HOT (holds page records for
// an uncommitted transaction matching this database). A journal that is
// short, unparsable, from a different page-size generation, empty of
// records, or from a prior database generation (dbOrigSize mismatch —
// journal1.test 1.2) is STALE and discarded (unlinked) without playback.
// Header integers are BIG-endian (pager.c writeJournalHdr), matching our
// own writer since the C-format journal port.
func readHotJournal(p *Pager, jpath string) ([]JournalPage, bool) {
	data, err := os.ReadFile(jpath)
	if err != nil {
		return nil, false // journal vanished between Stat and read: nothing to do
	}
	if len(data) < 28 {
		_ = os.Remove(jpath)
		return nil, false
	}
	hdr, herr := DecodeJournalHeader(data[:28])
	if herr != nil {
		_ = os.Remove(jpath)
		return nil, false
	}
	if hdr.PageSize != p.pageSize {
		// Journal from a different page-size generation: stale, discard.
		_ = os.Remove(jpath)
		return nil, false
	}
	pages, perr := DecodeJournalPages(data, hdr)
	if perr != nil || len(pages) == 0 {
		_ = os.Remove(jpath)
		return nil, false
	}
	// Staleness: the journal's dbOrigSize must match the main file's current
	// page count; otherwise the journal belongs to a prior database
	// generation (journal1.test 1.2).
	p.mu.RLock()
	curPages := p.numPages
	p.mu.RUnlock()
	if hdr.DBPageCount != curPages {
		_ = os.Remove(jpath)
		return nil, false
	}
	return pages, true
}

// replayHotJournalPages restores the journal's before-images into the page
// cache, newest record first so the oldest before-image wins. Page 1's
// leading bytes also refresh the cached database header. Caller holds p.mu.
func replayHotJournalPages(p *Pager, pages []JournalPage) {
	for i := len(pages) - 1; i >= 0; i-- {
		pg := pages[i]
		if uint32(len(pg.Data)) != p.pageSize {
			continue
		}
		cp, ok := p.pages[pg.PageNumber]
		if !ok {
			cp = &Page{PageNum: pg.PageNumber, Data: make([]byte, p.pageSize)}
			p.pages[pg.PageNumber] = cp
		}
		copy(cp.Data, pg.Data)
		delete(p.dirty, pg.PageNumber)
		if pg.PageNumber == 1 && len(pg.Data) >= HeaderSize {
			if p.header == nil {
				p.header = make([]byte, HeaderSize)
			}
			copy(p.header, pg.Data[:HeaderSize])
		}
	}
}

// path construction). Returns "" for in-memory pagers.
func journalPath(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	return dbPath + "-journal"
}

// journalChecksumUpdate applies the same mixer as walChecksumBytes (but
// in Go) to compute the running c1/c2 over a 4-byte little-endian
// pageNum and the page data. This mirrors pager.c's journal-page
// checksum seed chain (BIG-ENDIAN uint32 sum over a seed XOR'd buffer).
//
// The mixer: c1 += bi; c2 += c1; for each 4-byte little-endian word.
// This is the same algorithm the WAL writer uses, applied to the
// rollback journal's per-record checksum.
func journalChecksumUpdate(c1, c2 uint32, buf []byte) (uint32, uint32) {
	for i := 0; i+4 <= len(buf); i += 4 {
		w := binary.LittleEndian.Uint32(buf[i : i+4])
		c1 += w
		c2 += c1
	}
	// Trailing partial word (shouldn't happen for our records because
	// the data buffer is always a multiple of 4 bytes — the page
	// content is a multiple of 4 — but be safe).
	if tail := len(buf) % 4; tail != 0 {
		var pad [4]byte
		copy(pad[:], buf[len(buf)-tail:])
		w := binary.LittleEndian.Uint32(pad[:])
		c1 += w
		c2 += c1
	}
	return c1, c2
}

// journalWriteSkipped reports why opening the rollback journal can be
// skipped entirely: memory pagers have no journal file, one is already open,
// and an empty database's Init-time schema write must not materialize a
// sidecar. A no-dirty flush also skips (no 512-byte header write for a
// no-op post-COMMIT idempotent flush in PERSIST mode).
//
// Lazy journal creation: a database opened empty (file size == 0) is
// left untouched on disk by the Init-time schema page write (the
// engine's Open() path calls MarkClean to drop the dirty flag without
// flushing). Opening a journal file here would create a sidecar that
// the engine then immediately closes — and the journal2 test
// (journal2.test 2.1) expects no journal events from the empty-DB
// open + PRAGMA journal_mode=persist sequence. We only need a
// journal once the user makes a real (non-Init) write.
func (p *Pager) journalWriteSkipped() bool {
	if p.file == nil {
		return true // in-memory pager: no journal file
	}
	if p.journalFile != nil {
		return true // already open
	}
	if p.openedEmpty && len(p.dirty) == 1 {
		if _, only := p.dirty[1]; only {
			return true
		}
	}
	return len(p.dirty) == 0
}

// journalFileMode resolves the effective rollback-journal file mode (the
// empty journalMode is the SQLite default "delete"). WAL/MEMORY/OFF use
// other paths and create no journal file — reported as "".
func (p *Pager) journalFileMode() string {
	mode := p.journalMode
	if mode == "" {
		mode = "delete"
	}
	switch mode {
	case "memory", "off", "wal":
		return ""
	}
	return mode
}

// openRollbackJournalLocked creates (or truncates-and-reopens) the
// "test.db-journal" sidecar with the same Unix mode bits as the main
// database file (journal3.test 1.2.x.4). The journal header is written
// immediately; pages are appended as the caller flushes dirty pages to
// the main database.
//
// The caller must hold p.mu.
func (p *Pager) openRollbackJournalLocked() error {
	if p.journalWriteSkipped() {
		return nil
	}
	// Mode gating: only the rollback-journal modes (DELETE/PERSIST/
	// TRUNCATE) actually create a file. WAL/MEMORY/OFF use other paths.
	mode := p.journalFileMode()
	if mode == "" {
		return nil
	}
	// Verify the database still has the same name as when it was opened
	// (pager.c databaseIsUnmoved at sqlite3PagerOpenJournal): a database
	// file renamed or deleted out from under the pager makes it read-only
	// for journaling modes (SQLITE_READONLY_DBMOVED — pager4.test 1.3/1.4;
	// journal_mode OFF/MEMORY skip the journal and stay writable, 1.7/1.8).
	if p.databaseFileMoved() {
		return fmt.Errorf("attempt to write a readonly database")
	}
	jpath := journalPath(p.path)
	if jpath == "" {
		return nil
	}
	if err := p.openJournalFileLocked(jpath, mode); err != nil {
		return err
	}
	// Fire xOpen via the testvfs-equivalent hook (journal2 test suite).
	// The hook is the narrow VFS-layer observability path for the
	// journal sidecar; nil in production.
	if h := p.journalFileOpHookFn(); h != nil {
		h("xOpen", jpath)
	}
	return p.initJournalEpochLocked()
}

// openJournalFileLocked opens (or truncates-and-reopens) the journal sidecar
// file per journal mode. PERSIST reuses an existing file (the pager
// truncates to journal_size_limit on commit; the file stays on disk); for
// DELETE/TRUNCATE any leftover is unlinked first so a fresh header is
// written (pager.c sqlite3PagerOpenJournal — a PERSIST-mode journal file
// that already exists is kept and overwritten in place; for the others, the
// old file is unlinked first). Caller holds p.mu.
func (p *Pager) openJournalFileLocked(jpath, mode string) error {
	if mode == "persist" {
		// Open WITHOUT O_TRUNC so the previous transaction's records
		// (if any) are visible on disk; the new header is written at
		// offset 0 (overwriting the old header) and the new records
		// append after the header. The COMMIT-time finalize truncates
		// to journal_size_limit (or to 0 in the multi-DB path).
		f, err := os.OpenFile(jpath, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return fmt.Errorf("pager: open journal: %w", err)
		}
		// Seek to the end of any existing records so appends continue
		// the stream. The header write at offset 0 will overwrite the
		// old header; new records go right after the new header.
		if _, err := f.Stat(); err == nil {
			// Start at the sector size (the new header position).
			_, _ = f.Seek(int64(p.journalSectorSize), 0)
		}
		p.journalFile = f
		return nil
	}
	// DELETE/TRUNCATE: unlink any leftover journal.
	_ = os.Remove(jpath)
	f, err := os.OpenFile(jpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("pager: open journal: %w", err)
	}
	p.journalFile = f
	return nil
}

// initJournalEpochLocked starts a fresh journal epoch on the just-opened
// sidecar: mirror the main file's mode bits, seed the checksum chain, record
// dbOrigSize, write the journal header and position the append cursor after
// it. Caller holds p.mu.
func (p *Pager) initJournalEpochLocked() error {
	jpath := p.journalFile.Name()
	// Mirror the main database file's mode bits (journal3.test 1.2.x.4
	// asserts the journal has the same -perm as the main db). Falls
	// back to 0o644 if the Stat fails (e.g. a brand-new file with no
	// mode yet).
	if st, err := os.Stat(p.path); err == nil {
		_ = os.Chmod(jpath, st.Mode().Perm())
	}
	// Sector size: default to 512 (matches pager.c setSectorSize when
	// no FCNTL sector-size hint is provided).
	p.journalSectorSize = defaultSectorSize
	// Initialize checksum seed with two random uint32s. SQLite uses
	// sqlite3_randomness; we use Go's crypto/rand via the walChecksum
	// path — but to keep the journal.go file dependency-light we just
	// derive two deterministic-from-time seeds (the journal is not a
	// security boundary; it's only checked at recovery to detect torn
	// writes). A real port would use crypto/rand here.
	// We use the caller's Pager.ckInit1/2 if already set; otherwise we
	// derive from the file's inode + size so concurrent journals on
	// different files have different cksumInit seeds.
	p.journalCksum1 = p.deriveCksumInit()
	p.journalCksum2 = p.deriveCksumInit() ^ 0xa5a5a5a5
	// dbOrigSize: the main database's page count at journal-open time
	// (pager.c dbOrigSize). Used by recovery to detect a stale journal
	// (different dbOrigSize → the journal belongs to a different db).
	p.journalDBOrigSize = p.numPages
	// Write the journal header.
	if err := p.writeJournalHeaderLocked(); err != nil {
		_ = p.journalFile.Close()
		p.journalFile = nil
		return err
	}
	// After WriteAt the file position is unchanged; seek to the end of
	// the new header so subsequent records (which use Write, not WriteAt)
	// append after it. Without this the first record overwrites the
	// header bytes.
	if _, err := p.journalFile.Seek(int64(p.journalSectorSize), 0); err != nil {
		return fmt.Errorf("pager: seek journal: %w", err)
	}
	// Initialize the running-checksum state from the header seed.
	p.journalRecC1 = p.journalCksum1
	p.journalRecC2 = p.journalCksum2
	// Fresh journal epoch: every page's before-image is recorded at most
	// once until the journal is finalized or replayed (pInJournal bitvec
	// lifetime in pager.c).
	p.resetJournalPagesLocked()
	return nil
}

// writeJournalHeaderLocked writes one sector's worth of journal header
// (8-byte magic + 4-byte nRec (0xffffffff for now, like the no-sync
// fast path in pager.c:1485) + 4-byte cksumInit1 + 4-byte dbOrigSize
// + 4-byte sectorSize + 4-byte pageSize + zero-pad to one sector).
func (p *Pager) writeJournalHeaderLocked() error {
	if p.journalFile == nil {
		return nil
	}
	hdr := make([]byte, p.journalSectorSize)
	copy(hdr[0:8], journalMagic[:])
	// nRec placeholder: use 0xffffffff (no-sync / SAFE_APPEND path). The
	// reader interprets this as "rest of file is page records".
	// All header integers are BIG-endian (pager.c writeJournalHdr uses
	// put4byte), matching DecodeJournalHeader and C-written journals.
	binary.BigEndian.PutUint32(hdr[8:12], 0xffffffff)
	// cksumInit (two uint32s): the rollback journal records' checksum
	// chain starts from these. We use the same word in both places for
	// a one-seed chain (the WAL writer uses two seeds; the journal
	// header also stores a single random value and XORs through the
	// records — pager.c stores cksumInit as a single u32 and derives
	// the second via a fixed permutation, but for recovery we only
	// need a non-zero seed so corruption is detectable).
	binary.BigEndian.PutUint32(hdr[12:16], p.journalCksum1)
	// dbOrigSize
	binary.BigEndian.PutUint32(hdr[16:20], p.journalDBOrigSize)
	// sectorSize
	binary.BigEndian.PutUint32(hdr[20:24], p.journalSectorSize)
	// pageSize
	binary.BigEndian.PutUint32(hdr[24:28], p.pageSize)
	// rest of the sector is zero
	if _, err := p.journalFile.WriteAt(hdr, 0); err != nil {
		return fmt.Errorf("pager: journal hdr: %w", err)
	}
	return nil
}

// appendRollbackRecordLocked writes one page's before-image to the open
// journal file. Called from flushPage for every dirty page flushed to
// the main database. The record format is: 4-byte page number,
// pageSize bytes of data, then we update the running c1/c2. (The 8-byte
// per-record checksum is computed lazily at COMMIT/sync time in pager.c;
// for the journal file's flush-time record we just need the pageNum +
// data to be on disk so a recovery can re-derive the checksum from
// cksumInit + the data stream.)
func (p *Pager) appendRollbackRecordLocked(pageNum uint32, data []byte) error {
	if p.journalFile == nil {
		return nil
	}
	// C record layout (pager.c pager_write_pagelist / readJournalHdr parse):
	// [4-byte BIG-endian pageNum][pageSize bytes of data][4-byte BIG-endian
	// record checksum]. JOURNAL_PG_SZ is pageSize+8; recoverHotJournal and
	// rollbackFromJournalLocked decode through DecodeJournalPages, and
	// C/hexio-written journals round-trip byte-identically. The per-record
	// checksum uses the pager_cksum sparse sampler seeded from the header
	// (recovery does not verify it, matching the no-sync fast path).
	buf := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(buf[0:4], pageNum)
	copy(buf[4:4+len(data)], data)
	binary.BigEndian.PutUint32(buf[4+len(data):], JournalChecksum(p.journalCksum1, data))
	if _, err := p.journalFile.Write(buf); err != nil {
		return fmt.Errorf("pager: journal write pg %d: %v", pageNum, err)
	}
	// Update the running checksum over (pageNum, data). The same mixer
	// as the WAL writer.
	c1, c2 := journalChecksumUpdate(p.journalRecC1, p.journalRecC2, buf)
	p.journalRecC1 = c1
	p.journalRecC2 = c2
	return nil
}

// finalizeRollbackJournalLockedMulti applies the post-flush action dictated
// by the journal mode. Called from flushAllCtx after all dirty pages
// have been written to the main database file. The multiDB flag is true
// when this flush is part of a multi-database COMMIT (super-journal
// path) — it forces PERSIST journals to truncate to 0 (pager.c
// zeroJournalHdr with hasSuper=true), regardless of journal_size_limit.
//
//   - DELETE:     unlink the journal file (free the sidecar).
//   - TRUNCATE:   truncate the journal file to 0 bytes (pager.c:5313
//     "running in journal_mode=truncate mode").
//   - PERSIST:    truncate the journal file to journal_size_limit
//     (positive N), 0 (N=0), or leave intact (N<0). The
//     next transaction's openRollbackJournalLocked will
//     overwrite the header. With multiDB=true, force 0.
//   - MEMORY/OFF/WAL: no journal file to finalise.
//
// This MUST be called with p.mu held.
func (p *Pager) finalizeRollbackJournalLockedMulti(multiDB bool) error {
	if p.journalFile == nil {
		return nil
	}
	// The transaction is committed: the journalled-before-image epoch ends
	// here (PERSIST/TRUNCATE keep the file open, but the next transaction
	// must record fresh before-images).
	p.resetJournalPagesLocked()
	mode := p.journalMode
	if mode == "" {
		mode = "delete"
	}
	jpath := p.journalFile.Name()
	switch mode {
	case "delete":
		return p.finalizeJournalDeleteLocked(jpath)
	case "truncate":
		return p.finalizeJournalTruncateLocked(jpath)
	case "persist":
		return p.finalizeJournalPersistLocked(jpath, multiDB)
	}
	return nil
}

// finalizeJournalDeleteLocked applies DELETE-mode finalization: close (some
// platforms refuse unlink of an open file) then unlink. Fires xClose +
// xDelete via the testvfs hook. Caller holds p.mu.
func (p *Pager) finalizeJournalDeleteLocked(jpath string) error {
	_ = p.journalFile.Close()
	p.journalFile = nil
	if h := p.journalFileOpHookFn(); h != nil {
		h("xClose", jpath)
	}
	err := os.Remove(jpath)
	if h := p.journalFileOpHookFn(); h != nil {
		h("xDelete", jpath)
	}
	if err != nil && !os.IsNotExist(err) {
		// The journal may already be gone (playback at Open, a prior
		// commit, or a transaction that never spilled); SQLite's
		// xDelete treats a missing file as deleted (os_unix.c).
		return err
	}
	return nil
}

// finalizeJournalTruncateLocked applies TRUNCATE-mode finalization: keep the
// file open across COMMITs (the next transaction reuses the open FD;
// pager.c sqlite3PagerClose is the only path that releases it) and truncate
// it to 0 bytes (pager.c:5313 "running in journal_mode=truncate mode"). No
// xClose / xDelete — the file is reused. Caller holds p.mu.
func (p *Pager) finalizeJournalTruncateLocked(jpath string) error {
	if err := os.Truncate(jpath, 0); err != nil {
		return err
	}
	// Seek back to the end of the header so the next transaction
	// that appends records starts at the right offset.
	if _, err := p.journalFile.Seek(int64(p.journalSectorSize), 0); err != nil {
		return fmt.Errorf("pager: seek journal after truncate: %w", err)
	}
	return nil
}

// finalizeJournalPersistLocked applies PERSIST-mode finalization: keep the
// journal file open across COMMITs and apply journal_size_limit (or 0 in
// the super-journal / multi-DB case). The header is overwritten on the next
// transaction's openRollbackJournalLocked.
//
// We honour journal_size_limit in PERSIST mode at COMMIT, but force a
// 0-truncate when multiDB=true (the super-journal path) regardless of
// journal_size_limit:
//   - multiDB=true: truncate to 0 (matches jrnlmode-2.2/2.4).
//   - multiDB=false, journal_size_limit < 0: unlimited (file left intact).
//   - multiDB=false, journal_size_limit == 0: truncate to 0.
//   - multiDB=false, journal_size_limit > 0: truncate to the limit if the
//     file is larger; otherwise leave it (matches jrnlmode-5.13/5.15).
//
// Caller holds p.mu.
func (p *Pager) finalizeJournalPersistLocked(jpath string, multiDB bool) error {
	if err := persistTruncateLimit(p.journalSizeLimit, jpath, multiDB); err != nil {
		return err
	}
	// Seek back to the end of the header so the next transaction
	// that appends records starts at the right offset.
	if _, err := p.journalFile.Seek(int64(p.journalSectorSize), 0); err != nil {
		return fmt.Errorf("pager: seek journal after persist: %w", err)
	}
	return nil
}

// persistTruncateLimit truncates the PERSIST journal per journal_size_limit
// (see finalizeJournalPersistLocked's size-limit table). multiDB forces the
// 0-truncate of the super-journal path.
func persistTruncateLimit(limit int64, jpath string, multiDB bool) error {
	if multiDB {
		return os.Truncate(jpath, 0)
	}
	if limit < 0 {
		return nil
	}
	if limit == 0 {
		return os.Truncate(jpath, 0)
	}
	st, _ := os.Stat(jpath)
	var sz int64
	if st != nil {
		sz = st.Size()
	}
	if sz > limit {
		return os.Truncate(jpath, limit)
	}
	return nil
}

// rollbackFromJournalLocked replays the journal's page records in
// reverse order, restoring the before-images into the page cache. The
// caller has just decided to ROLLBACK a transaction; the journal file
// is unlinked afterwards. This MUST be called with p.mu held.
func (p *Pager) rollbackFromJournalLocked() error {
	if p.journalFile == nil {
		return nil
	}
	// Rollback replays the journal from disk; the current journalled-page
	// epoch is over either way.
	p.resetJournalPagesLocked()
	// Read the journal records back: one sector header, then a stream of
	// [4-byte BE pageNum][pageSize bytes of data][4-byte BE cksum] records
	// (JOURNAL_PG_SZ = pageSize+8) — the exact layout DecodeJournalPages
	// parses (pager.c readJournalHdr's record walk).
	jpath := p.journalFile.Name()
	data, err := os.ReadFile(jpath)
	if err != nil {
		p.discardJournalFileLocked(jpath)
		return fmt.Errorf("pager: rollback read journal: %w", err)
	}
	p.closeJournalFileLocked(jpath)
	hdr, herr := DecodeJournalHeader(data[:28])
	if herr != nil {
		return fmt.Errorf("pager: rollback journal header: %w", herr)
	}
	jpages, perr := DecodeJournalPages(data, hdr)
	if perr != nil {
		return fmt.Errorf("pager: rollback journal records: %w", perr)
	}
	p.restoreJournalTruncationLocked(jpages)
	p.restoreBeforeImagesLocked(jpages)
	// Drop the entire dirty set: every dirty page is being rolled back.
	// (If a dirty page was never flushed during this transaction, the
	// dirty cache state is still in memory and the rollback drops it.)
	p.dirty = make(map[uint32]bool)
	// Unlink the journal file (the in-memory rollback is complete;
	// the file is no longer needed). Fire xDelete via the testvfs
	// hook so journal2 sees a balanced sequence.
	if h := p.journalFileOpHookFn(); h != nil {
		h("xDelete", jpath)
	}
	return os.Remove(jpath)
}

// discardJournalFileLocked closes and unlinks a journal sidecar whose
// records could not be read back (a rollback read failure): the file is
// gone either way, so xClose and xDelete both fire. Caller holds p.mu.
func (p *Pager) discardJournalFileLocked(jpath string) {
	_ = p.journalFile.Close()
	p.journalFile = nil
	_ = os.Remove(jpath)
	if h := p.journalFileOpHookFn(); h != nil {
		h("xClose", jpath)
		h("xDelete", jpath)
	}
}

// closeJournalFileLocked closes the journal sidecar after its records have
// been read into memory (the replay owns them now); xClose fires, and the
// unlink happens after the replay completes. Caller holds p.mu.
func (p *Pager) closeJournalFileLocked(jpath string) {
	_ = p.journalFile.Close()
	p.journalFile = nil
	if h := p.journalFileOpHookFn(); h != nil {
		h("xClose", jpath)
	}
}

// restoreJournalTruncationLocked applies C nTrunc semantics (pager.c
// pager_rollback): when the transaction shrank the file (in-transaction
// incremental-vacuum steps truncate through the journal-protected path),
// restore the file length to the size recorded at journal start
// (header[16:20], dbOrigSize) BEFORE replaying records, writing the
// journalled before-images of tail pages back to disk — the file.Truncate
// in truncatePages removed them mid-transaction and cache-only replay would
// leave zeros on disk. The in-memory page count is repaired and cache pages
// beyond the restored size dropped. Caller holds p.mu.
func (p *Pager) restoreJournalTruncationLocked(jpages []JournalPage) {
	if p.file == nil || p.journalDBOrigSize == 0 || p.numPages >= p.journalDBOrigSize {
		return
	}
	newSize := int64(p.journalDBOrigSize) * int64(p.pageSize)
	if err := p.file.Truncate(newSize); err != nil {
		return
	}
	p.fileSize = newSize
	for i := len(jpages) - 1; i >= 0; i-- {
		jp := jpages[i]
		if jp.PageNumber > p.numPages && jp.PageNumber <= p.journalDBOrigSize {
			if _, err := p.file.WriteAt(jp.Data, int64(jp.PageNumber-1)*int64(p.pageSize)); err != nil {
				break
			}
		}
	}
	for pgno := range p.pages {
		if pgno > p.journalDBOrigSize {
			delete(p.pages, pgno)
			delete(p.dirty, pgno)
		}
	}
	p.numPages = p.journalDBOrigSize
	if len(p.header) >= 32 {
		binary.BigEndian.PutUint32(p.header[28:32], p.journalDBOrigSize)
	}
}

// restoreBeforeImagesLocked replays the journal's page records in reverse
// order (so later records' before-images win), replacing each in-memory
// page's content with its before-image. The page is restored as clean: the
// in-flight dirty change is undone, and on the next flush the restored page
// is the before-image that should be on disk. Caller holds p.mu.
func (p *Pager) restoreBeforeImagesLocked(jpages []JournalPage) {
	for i := len(jpages) - 1; i >= 0; i-- {
		jp := jpages[i]
		pg, ok := p.pages[jp.PageNumber]
		if !ok {
			pg = &Page{PageNum: jp.PageNumber}
			p.pages[jp.PageNumber] = pg
		}
		pg.Data = jp.Data
		delete(p.dirty, jp.PageNumber)
	}
}
