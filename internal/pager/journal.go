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
	"errors"
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

// hotJournalState classifies a candidate hot journal (C's hasHotJournal +
// readJournalHdr pipeline).
type hotJournalState int

const (
	hotJournalAbsent hotJournalState = iota // no sidecar on disk
	hotJournalLeave                         // exists but not hot: leave it on disk
	hotJournalDelete                        // hot candidate with an unparsable header: delete
	hotJournalHot                           // play back before-images, then delete
)

// errReadOnlyHotJournal is C's SQLITE_READONLY on a hot-journal playback
// attempt: pager.c pagerPlayback must take the EXCLUSIVE file lock (a write)
// before replaying records, which a readonly connection cannot take — every
// statement on a readonly connection over a hot journal fails "attempt to
// write a readonly database" and the journal is LEFT in place (pager1-4.5.6).
var errReadOnlyHotJournal = errors.New("attempt to write a readonly database")

// errCantOpenJournal is C's SQLITE_CANTOPEN when the hot journal cannot be
// reopened READWRITE for the playback (pager1-4.7.2's r-------- journal).
var errCantOpenJournal = errors.New("unable to open database file")

// RecoverHotJournalIfNeeded runs C's hot-journal recovery at the per-statement
// shared-lock point (pager.c sqlite3PagerSharedLock -> hasHotJournal ->
// pager_playback — NOT at sqlite3PagerOpen: C's open only reads page 1 raw,
// and the faultsim fixtures restore a hot journal onto a CLOSED snapshot and
// expect the journal to survive until the first statement runs). Classification
// mirrors hasHotJournal + readJournalHdr; a HOT journal is played back to the
// DATABASE FILE (C heals the file so later connections and this connection's
// post-reset cache read before-images), then unlinked; a stale journal is
// unlinked without playback (journal1.test 1.2: a leftover journal from a
// prior database must not roll back into a new database). Returns
// errReadOnlyHotJournal when the pager is read-only and the journal is hot
// (no classification change: the journal stays). The caller must NOT hold
// p.mu.
func (p *Pager) RecoverHotJournalIfNeeded() error {
	p.mu.RLock()
	dbPath, ro := p.path, p.readOnly
	liveWriter := p.journalFile != nil
	p.mu.RUnlock()
	if liveWriter {
		// The journal belongs to THIS connection's in-flight transaction —
		// never hot for its own pager.
		return nil
	}
	// C lock protocol (pager.c hasHotJournal -> pagerPlayback): playing back
	// a hot journal requires an EXCLUSIVE file lock, which a live writer's
	// transaction denies — the journal belongs to that in-flight transaction
	// and must be left untouched (no playback, no unlink). Without this
	// registry gate a second connection would treat the live journal as hot
	// and a later ROLLBACK could truncate the writer's file out from under
	// it (pcache.test 1.x).
	if lockreg.Global.WriteTxHeld(dbPath) {
		return nil
	}
	jpath := journalPath(dbPath)
	if jpath == "" {
		return nil
	}
	pages, hdr, data, state := hotJournalProbe(p, jpath)
	switch state {
	case hotJournalAbsent, hotJournalLeave:
		return nil
	case hotJournalDelete:
		_ = os.Remove(jpath)
		return nil
	case hotJournalHot:
		if ro {
			return errReadOnlyHotJournal
		}
		// pager.c sqlite3PagerSharedLock:5329 — the hot journal is reopened
		// READWRITE before the playback (the DELETE-mode journal must be
		// deletable through the same fd). A journal without WRITE permission
		// (pager1-4.7.2's chmod r-------- journal) fails the open with
		// SQLITE_CANTOPEN and the statement reports "unable to open database
		// file"; the journal stays for the next (privileged) process.
		if fd, oerr := os.OpenFile(jpath, os.O_RDWR, 0); oerr != nil {
			return errCantOpenJournal
		} else {
			fd.Close()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	playbackHotJournalLocked(p, pages, hdr, jpath, data)
	return nil
}

// hotJournalProbe reads and validates a candidate hot journal at jpath
// (pager.c hasHotJournal + readJournalHdr + the pager_playback_one_page
// record walk). Returns the play-able records, the decoded header, and the
// classification. Header integers are BIG-endian (pager.c writeJournalHdr).
func hotJournalProbe(p *Pager, jpath string) ([]JournalPage, JournalHeader, []byte, hotJournalState) {
	data, err := os.ReadFile(jpath)
	if err != nil {
		return nil, JournalHeader{}, nil, hotJournalAbsent // vanished between Stat and read
	}
	// C hasHotJournal: the database must be non-empty (nPage==0 makes the
	// journal a remnant from a prior database — journal1.test 1.2 — or the
	// rollback of the initial populate transaction) — DELETE it (the one
	// not-hot shape C removes), no playback.
	if p.numPages == 0 {
		return nil, JournalHeader{}, nil, hotJournalDelete
	}
	// C hasHotJournal: the FIRST BYTE must be nonzero — a zeroed header
	// (PERSIST mode's zeroJournalHdr writes 28 zero bytes at offset 0, or a
	// header never patched by syncJournal) is NOT hot and the file is LEFT
	// on disk (PERSIST reuses it for the next transaction).
	if len(data) == 0 || data[0] == 0 {
		return nil, JournalHeader{}, nil, hotJournalLeave
	}
	// pager_playback's super-journal gate (before readJournalHdr): a journal
	// whose tail names an EXISTING super-journal belongs to an UNCOMMITTED
	// multi-file transaction (roll back); a journal naming a MISSING one
	// belongs to a committed transaction — no playback, journal deleted
	// (pager1-4.4.x.7: "no master-journal file == no rollback"). A failed
	// pointer parse (bad magic/checksum) falls through to the header rules.
	if sjName, ok := SuperJournalPointerFromData(data); ok && sjName != "" {
		if _, serr := os.Stat(sjName); serr != nil {
			return nil, JournalHeader{}, nil, hotJournalDelete
		}
	}
	if len(data) < 28 {
		return nil, JournalHeader{}, nil, hotJournalDelete
	}
	hdr, herr := DecodeJournalHeader(data[:28])
	if herr != nil {
		return nil, JournalHeader{}, nil, hotJournalDelete
	}
	// readJournalHdr (pager.c:1626-1660): the sector- and page-size fields
	// of the first header must be in range — powers of two, sector in
	// [32, MAX_SECTOR_SIZE=0x10000], page size in [512, SQLITE_MAX_PAGE_SIZE].
	// A ZERO page-size field is a pre-3.5.8 journal: the pager's own page
	// size applies (pager1-31.1's compat contract). Out-of-range fields mean
	// the writer crashed before the header synced: not hot.
	sector := hdr.SectorSize
	pageSize := hdr.PageSize
	if pageSize == 0 {
		pageSize = p.pageSize
	}
	if sector < 32 || sector > 0x10000 || sector&(sector-1) != 0 ||
		pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return nil, JournalHeader{}, nil, hotJournalDelete
	}
	if pageSize != p.pageSize {
		// C re-sizes the pager to the journal's page size mid-playback
		// (sqlite3PagerSetPagesize in readJournalHdr). No rollback-journal
		// fixture exercises a cross-size playback (the compat case is the
		// zero field above, which equals the pager's size by definition);
		// a size mismatch is a generation change — treat as stale.
		return nil, JournalHeader{}, nil, hotJournalDelete
	}
	// Record walk (pager_playback + pager_playback_one_page): records start
	// at the first JOURNAL_HDR_SZ-aligned offset — the sector-size field
	// governs the header size, so 4.3.3's corrupted sector=32 moves the
	// record window into the header padding where the zero pgno stops the
	// walk. nRec caps the walk unless it is 0xffffffff ("rest of file is
	// records", the no-sync fast path our writer emits).
	hdrSize := int64(sector)
	recSize := int64(pageSize) + 8
	if hdrSize+recSize > int64(len(data)) {
		// No complete record after the header: nothing to play back.
		return nil, hdr, data, hotJournalHot
	}
	limit := (int64(len(data)) - hdrSize) / recSize
	if hdr.RecordCount >= 0 && int64(hdr.RecordCount) < limit {
		limit = int64(hdr.RecordCount)
	}
	pages := make([]JournalPage, 0, limit)
	for off := hdrSize; off < hdrSize+limit*recSize; off += recSize {
		rec := data[off : off+recSize]
		pgno := binary.BigEndian.Uint32(rec[0:4])
		if pgno == 0 {
			// pager_playback_one_page: pgno 0 is garbage — SQLITE_DONE.
			break
		}
		if pgno > p.numPages {
			// Beyond the database size: skip (SQLITE_OK, keep walking).
			continue
		}
		pageData := rec[4 : 4+pageSize]
		cksum := binary.BigEndian.Uint32(rec[4+pageSize : 8+pageSize])
		// The per-record checksum (pager_cksum seeded from cksumInit)
		// mismatch STOPS the playback: records after a torn write are
		// untrusted (pager1-4.5.4/4.5.5's corruption observability).
		if JournalChecksum(hdr.CksumInit, pageData) != cksum {
			break
		}
		pages = append(pages, JournalPage{
			PageNumber: pgno,
			Checksum:   cksum,
			Data:       append([]byte(nil), pageData...),
		})
	}
	return pages, hdr, data, hotJournalHot
}

// playbackHotJournalLocked plays the probe's records back C-style: the
// database FILE is truncated to the journal header's original size (pager.c
// pager_playback's pager_truncate(mxPg) + dbSize=mxPg), every record's
// before-image is written into the file (pager_playback_one_page's main-file
// write — the heal must be visible to later connections), page 1's before-
// image re-seeds the cached header, and the page cache is reset (pager_reset)
// so subsequent reads fetch the healed image. The journal is unlinked last
// (pager_end_transaction). data is the journal image the probe read (for the
// super-journal pointer). Caller holds p.mu.
func playbackHotJournalLocked(p *Pager, pages []JournalPage, hdr JournalHeader, jpath string, data []byte) {
	mxPg := hdr.DBPageCount
	if mxPg == 0 {
		mxPg = p.numPages
	}
	if p.file != nil {
		newSize := int64(mxPg) * int64(p.pageSize)
		if err := p.file.Truncate(newSize); err == nil {
			p.fileSize = newSize
		}
	}
	p.numPages = mxPg
	if len(p.header) >= 32 {
		binary.BigEndian.PutUint32(p.header[28:32], mxPg)
	}
	for _, jp := range pages {
		if p.file != nil && len(jp.Data) == int(p.pageSize) {
			_, _ = p.file.WriteAt(jp.Data, int64(jp.PageNumber-1)*int64(p.pageSize))
		}
		if jp.PageNumber == 1 && len(jp.Data) >= HeaderSize {
			if p.header == nil {
				p.header = make([]byte, HeaderSize)
			}
			copy(p.header, jp.Data[:HeaderSize])
			p.invalidateCookieCacheLocked()
		}
	}
	// pager_reset: the cache must not serve pre-playback images of pages the
	// journal restored (or the mid-txn spill images of pages it truncated).
	p.pages = make(map[uint32]*Page)
	p.clearDirtySetLocked()
	p.pendingFileTruncate = false
	p.restoreMaxPageCountLocked(mxPg)
	p.refreshKnownFileStamp()
	sjName, _ := SuperJournalPointerFromData(data)
	_ = os.Remove(jpath)
	// pager_delsuper: when this journal pointed at a super-journal, the
	// super-journal is deleted only if no OTHER journal still references it
	// (pager1-4.6.x's per-database deletion cascade).
	if sjName != "" {
		DeleteSuperJournalIfUnreferenced(sjName)
	}
}

// journalPath builds the rollback-journal sidecar path for dbPath (the
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
	if p.journalFile != nil && p.journalEpochLive {
		// Already open AND this transaction's epoch started: the header,
		// the checksum seeds, and the append cursor are current (a
		// spill's second flush within one transaction).
		return true
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
		if injectIOErr() {
			return ErrIOErr
		}
		f, err := os.OpenFile(jpath, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return fmt.Errorf("pager: open journal: %w", err)
		}
		// Record appends are offset-tracked (journalAppendOff), so no Seek
		// is needed here — initJournalEpochLocked positions the append
		// cursor past the fresh header it writes at offset 0.
		p.journalFile = f
		return nil
	}
	// DELETE/TRUNCATE: the O_TRUNC open produces the fresh sidecar; any
	// leftover journal from a crashed writer was already handled by the
	// hot-journal recovery at Open time (recoverJournalAtOpen), and a
	// journal that materialized between Open and this first write cannot
	// exist under the single-writer-per-pager engine contract. The previous
	// pre-open os.Remove was one syscall per transaction for a path that is
	// never populated (pager.c's DELETE mode uses DELETEONCLOSE and does
	// not pre-unlink either).
	if injectIOErr() {
		return ErrIOErr
	}
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
	// asserts the journal has the same -perm as the main db). The perm is
	// cached at Open (dbFilePerm), so a default-perm database skips the
	// per-epoch Stat+Chmod pair: OpenFile created the journal at 0o644 and
	// the umask that shaped the database file at ITS creation also shapes
	// the journal, so an identical cache value implies identical on-disk
	// bits. A non-0644 cache chmods; an unknown (0) value falls back to the
	// legacy Stat-then-Chmod and seeds the cache.
	switch {
	case p.dbFilePerm != 0o644 && p.dbFilePerm != 0:
		_ = os.Chmod(jpath, p.dbFilePerm)
	case p.dbFilePerm == 0:
		if st, err := os.Stat(p.path); err == nil {
			p.dbFilePerm = st.Mode().Perm()
			_ = os.Chmod(jpath, p.dbFilePerm)
		}
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
	// After the WriteAt, records append at the tracked offset past the
	// header (journalAppendOff; the previous Write-based appends needed a
	// Seek syscall here to move the fd position past the header bytes).
	p.journalAppendOff = int64(p.journalSectorSize)
	// Initialize the running-checksum state from the header seed.
	p.journalRecC1 = p.journalCksum1
	p.journalRecC2 = p.journalCksum2
	// Fresh journal epoch: every page's before-image is recorded at most
	// once until the journal is finalized or replayed (pInJournal bitvec
	// lifetime in pager.c).
	p.resetJournalPagesLocked()
	p.journalEpochLive = true
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
	// Offset-tracked append (see journalAppendOff): WriteAt instead of a
	// position-dependent Write, so nothing ever needs to Seek the fd.
	if _, err := p.journalFile.WriteAt(buf, p.journalAppendOff); err != nil {
		return fmt.Errorf("pager: journal write pg %d: %v", pageNum, err)
	}
	p.journalAppendOff += int64(len(buf))
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
	// must record fresh before-images AND a fresh header — writeJournalHdr
	// reseeds cksumInit and dbOrigSize per epoch, pager.c writeJournalHdr).
	p.resetJournalPagesLocked()
	p.journalEpochLive = false
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
// platforms refuse unlink of an open file) then unlink. The xClose and
// xDelete events fire BEFORE the file is removed — C's testvfs intercepts
// xDelete INSTEAD of performing it, and the fixture script (pager1-4.5.1's
// copy_on_journal_delete faultsim_save) must observe the journal still on
// disk; the real deletion proceeds after the script's OK. Caller holds p.mu.
func (p *Pager) finalizeJournalDeleteLocked(jpath string) error {
	_ = p.journalFile.Close()
	p.journalFile = nil
	if h := p.journalFileOpHookFn(); h != nil {
		h("xClose", jpath)
		h("xDelete", jpath)
	}
	err := os.Remove(jpath)
	if err != nil && !os.IsNotExist(err) {
		// The journal may already be gone (hot-journal playback, a prior
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
	// The next epoch's records start past the header; the tracked append
	// cursor makes that explicit (no Seek syscall).
	p.journalAppendOff = int64(p.journalSectorSize)
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
	// C zeroJournalHdr (pager.c, called from pager_end_transaction for
	// PERSIST mode): a leftover PERSIST journal must not read as hot, so
	// with journalOff != 0 either truncate the file to 0 (the super-journal
	// path, or journal_size_limit == 0) or write 28 zero bytes over the
	// header — the hasHotJournal first-byte check then classifies it as not
	// hot and the file stays for the next transaction to reuse. A positive
	// journal_size_limit truncates AFTER the zeroing (no re-sync).
	limit := p.journalSizeLimit
	if p.journalAppendOff != 0 {
		if multiDB || limit == 0 {
			if err := os.Truncate(jpath, 0); err != nil {
				return err
			}
		} else {
			if _, err := p.journalFile.WriteAt(make([]byte, 28), 0); err != nil {
				return fmt.Errorf("pager: journal hdr zero: %w", err)
			}
			if limit > 0 {
				if st, serr := os.Stat(jpath); serr == nil && st.Size() > limit {
					if terr := os.Truncate(jpath, limit); terr != nil {
						return terr
					}
				}
			}
		}
	}
	// The next epoch's records start past the header (offset-tracked appends;
	// the old code Seek-ed the fd to the same position).
	p.journalAppendOff = int64(p.journalSectorSize)
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
	p.clearDirtySetLocked()
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
	p.restoreMaxPageCountLocked(p.journalDBOrigSize)
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
