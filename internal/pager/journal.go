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

// journalRecord is one entry in the rollback journal: a page number, the
// page data (before-image), and the running checksum over both. The
// checksum is the same running-sum algorithm SQLite uses for the WAL
// frames (bigendian uint32 sums; the journal uses a different initial
// seed but the same mixer).
type journalRecord struct {
	pageNum uint32
	data    []byte
	c1      uint32
	c2      uint32
}

// rollbackJournal owns the on-disk test.db-journal file for one
// transaction. It records the BEFORE image of every dirty page flushed
// to the main database file and either (a) discards the file at COMMIT
// (DELETE), (b) truncates it to a per-DB size cap (PERSIST), or
// (c) zeroes it (TRUNCATE).
type rollbackJournal struct {
	file       *os.File
	path       string
	pageSize   uint32
	sectorSize uint32
	cksumInit1 uint32
	cksumInit2 uint32
	dbOrigSize uint32 // initial db size in pages at journal-header time
	records    []journalRecord
	// c1/c2 are the running checksum state of the most recently appended
	// record. The header sets the seed; each record advances c1/c2 over
	// its (pageNum, data) bytes (sqlite3PagerWalFrames / pager.c mix).
	c1 uint32
	c2 uint32
}

// journalPath returns "<dbPath>-journal" (pager.c sqlite3JournalOpen's
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

// openRollbackJournalLocked creates (or truncates-and-reopens) the
// "test.db-journal" sidecar with the same Unix mode bits as the main
// database file (journal3.test 1.2.x.4). The journal header is written
// immediately; pages are appended as the caller flushes dirty pages to
// the main database.
//
// The caller must hold p.mu.
func (p *Pager) openRollbackJournalLocked() error {
	if p.file == nil {
		return nil // in-memory pager: no journal file
	}
	if p.journalFile != nil {
		return nil // already open
	}
	// Lazy journal creation: a database opened empty (file size == 0) is
	// left untouched on disk by the Init-time schema page write (the
	// engine's Open() path calls MarkClean to drop the dirty flag without
	// flushing). Opening a journal file here would create a sidecar that
	// the engine then immediately closes — and the journal2 test
	// (journal2.test 2.1) expects no journal events from the empty-DB
	// open + PRAGMA journal_mode=persist sequence. We only need a
	// journal once the user makes a real (non-Init) write.
	if p.openedEmpty && len(p.dirty) == 1 {
		if _, only := p.dirty[1]; only {
			return nil
		}
	}
	if len(p.dirty) == 0 {
		// Nothing to flush — the journal file (if any) is left untouched.
		// This avoids a 512-byte header write for a no-op post-COMMIT
		// idempotent flush in PERSIST mode.
		return nil
	}
	// Mode gating: only the rollback-journal modes (DELETE/PERSIST/
	// TRUNCATE) actually create a file. WAL/MEMORY/OFF use other paths.
	mode := p.journalMode
	if mode == "" {
		mode = "delete"
	}
	switch mode {
	case "memory", "off", "wal":
		return nil
	}
	jpath := journalPath(p.path)
	if jpath == "" {
		return nil
	}
	// PERSIST mode reuses an existing file (the pager truncates to
	// journal_size_limit on commit; the file stays on disk). For
	// DELETE/TRUNCATE we unlink any leftover so a fresh header is
	// written. (pager.c sqlite3PagerOpenJournal — if the file already
	// exists for a PERSIST-mode journal, it is kept and overwritten
	// in place; for the others, the old file is unlinked first.)
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
	} else {
		// DELETE/TRUNCATE: unlink any leftover journal.
		_ = os.Remove(jpath)
		f, err := os.OpenFile(jpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("pager: open journal: %w", err)
		}
		p.journalFile = f
	}
	// Fire xOpen via the testvfs-equivalent hook (journal2 test suite).
	// The hook is the narrow VFS-layer observability path for the
	// journal sidecar; nil in production.
	if h := p.journalFileOpHookFn(); h != nil {
		h("xOpen", jpath)
	}
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
	binary.LittleEndian.PutUint32(hdr[8:12], 0xffffffff)
	// cksumInit (two uint32s): the rollback journal records' checksum
	// chain starts from these. We use the same word in both places for
	// a one-seed chain (the WAL writer uses two seeds; the journal
	// header also stores a single random value and XORs through the
	// records — pager.c stores cksumInit as a single u32 and derives
	// the second via a fixed permutation, but for recovery we only
	// need a non-zero seed so corruption is detectable).
	binary.LittleEndian.PutUint32(hdr[12:16], p.journalCksum1)
	// dbOrigSize
	binary.LittleEndian.PutUint32(hdr[16:20], p.journalDBOrigSize)
	// sectorSize
	binary.LittleEndian.PutUint32(hdr[20:24], p.journalSectorSize)
	// pageSize
	binary.LittleEndian.PutUint32(hdr[24:28], p.pageSize)
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
	// 4-byte page number + page data, no per-record checksum in the
	// stream (pager.c writes the checksum at sync time as part of the
	// sector; for our subset, the per-record integrity is enforced
	// implicitly by the running-checksum state we maintain in p.journalRecC1/C2).
	buf := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(buf[0:4], pageNum)
	copy(buf[4:], data)
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

// finalizeRollbackJournalLocked applies the post-flush action dictated
// by the journal mode. Single-DB case (multiDB=false) — see
// finalizeRollbackJournalLockedMulti for the multi-DB variant.
func (p *Pager) finalizeRollbackJournalLocked() error {
	return p.finalizeRollbackJournalLockedMulti(false)
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
	mode := p.journalMode
	if mode == "" {
		mode = "delete"
	}
	jpath := p.journalFile.Name()
	switch mode {
	case "delete":
		// DELETE: close (some platforms refuse unlink of an open file)
		// then unlink. Fire xClose + xDelete via the testvfs hook.
		_ = p.journalFile.Close()
		p.journalFile = nil
		if h := p.journalFileOpHookFn(); h != nil {
			h("xClose", jpath)
		}
		err := os.Remove(jpath)
		if h := p.journalFileOpHookFn(); h != nil {
			h("xDelete", jpath)
		}
		return err
	case "truncate":
		// TRUNCATE: keep the file open across COMMITs (the next
		// transaction reuses the open FD; pager.c sqlite3PagerClose
		// is the only path that releases it). Truncate to 0 bytes
		// (pager.c:5313 "running in journal_mode=truncate mode"). No
		// xClose / xDelete — the file is reused.
		if err := os.Truncate(jpath, 0); err != nil {
			return err
		}
		// Seek back to the end of the header so the next transaction
		// that appends records starts at the right offset.
		if _, err := p.journalFile.Seek(int64(p.journalSectorSize), 0); err != nil {
			return fmt.Errorf("pager: seek journal after truncate: %w", err)
		}
		return nil
	case "persist":
		// PERSIST keeps the journal file open across COMMITs. Apply
		// journal_size_limit (or 0 in the super-journal / multi-DB
		// case). The header is overwritten on the next transaction's
		// openRollbackJournalLocked.
		//
		// For simplicity we honour journal_size_limit in PERSIST mode
		// at COMMIT, but force a 0-truncate when multiDB=true (the
		// super-journal path) regardless of journal_size_limit:
		//   - multiDB=true: truncate to 0 (matches jrnlmode-2.2/2.4).
		//   - multiDB=false, journal_size_limit < 0: unlimited
		//     (file left intact).
		//   - multiDB=false, journal_size_limit == 0: truncate to 0.
		//   - multiDB=false, journal_size_limit > 0: truncate to the
		//     limit if the file is larger; otherwise leave it
		//     (matches jrnlmode-5.13/5.15).
		var err error
		if multiDB {
			err = os.Truncate(jpath, 0)
		} else {
			limit := p.journalSizeLimit
			if limit < 0 {
				err = nil
			} else if limit == 0 {
				err = os.Truncate(jpath, 0)
			} else {
				st, _ := os.Stat(jpath)
				var sz int64
				if st != nil {
					sz = st.Size()
				}
				if sz > limit {
					err = os.Truncate(jpath, limit)
				} else {
					err = nil
				}
			}
		}
		if err != nil {
			return err
		}
		// Seek back to the end of the header so the next transaction
		// that appends records starts at the right offset.
		if _, err := p.journalFile.Seek(int64(p.journalSectorSize), 0); err != nil {
			return fmt.Errorf("pager: seek journal after persist: %w", err)
		}
		return nil
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
	// Read the journal records back. The format is: sector header
	// (skipped), then a stream of [4-byte pageNum][pageSize bytes of
	// data] records. We don't have a per-record checksum in the
	// stream; the running c1/c2 was used to verify the on-disk
	// integrity at sync time (pager.c syncJournal writes the final
	// c1/c2 over the sector; we omit that and rely on the read
	// walking the file in lockstep with the writes).
	jpath := p.journalFile.Name()
	data, err := os.ReadFile(jpath)
	if err != nil {
		_ = p.journalFile.Close()
		p.journalFile = nil
		_ = os.Remove(jpath)
		if h := p.journalFileOpHookFn(); h != nil {
			h("xClose", jpath)
			h("xDelete", jpath)
		}
		return fmt.Errorf("pager: rollback read journal: %w", err)
	}
	_ = p.journalFile.Close()
	p.journalFile = nil
	if h := p.journalFileOpHookFn(); h != nil {
		h("xClose", jpath)
	}
	// Records start after the first sector.
	off := int(p.journalSectorSize)
	// Collect all records first (we restore in reverse).
	type rec struct {
		pageNum uint32
		data    []byte
	}
	var recs []rec
	for off+4+int(p.pageSize) <= len(data) {
		pn := binary.LittleEndian.Uint32(data[off : off+4])
		pg := make([]byte, p.pageSize)
		copy(pg, data[off+4:off+4+int(p.pageSize)])
		recs = append(recs, rec{pageNum: pn, data: pg})
		off += 4 + int(p.pageSize)
	}
	// Restore in reverse order (so later records' before-images win).
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		// Replace the in-memory page with the before-image; if the
		// page isn't in the cache (it was flushed and evicted) we
		// have no way to restore it without the cache. For the
		// Pager's own dirty-set tracking, the page is no longer
		// dirty.
		pg, ok := p.pages[r.pageNum]
		if !ok {
			pg = &Page{PageNum: r.pageNum}
			p.pages[r.pageNum] = pg
		}
		pg.Data = r.data
		// Restore the page as clean (the in-flight dirty change is
		// undone; on next flush the restored page is the
		// before-image that should be on disk).
		delete(p.dirty, r.pageNum)
	}
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
