// Package pager — open/read paths (pager.c sqlite3PagerOpen family).
//
// Open materializes a Pager over a database file: VFS-canonicalized path,
// header parse (deferring corruption to the first statement), hot-journal
// recovery and WAL attach. Header validation mirrors btree.c lockBtree.
package pager

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pijalu/frigolite/internal/quota"
	"github.com/pijalu/frigolite/internal/storage"
)

// Open opens a database file. If the file already exists, the page size is
// read from the database header (SQLite stores the actual page size in bytes
// 16-17 of the header); the pageSize argument is only a default for new files.
func Open(path string, pageSize uint32) (*Pager, error) { return openPager(path, pageSize, false) }

// OpenReadOnly opens an existing database file read-only:
// sqlite3_open_v2 with SQLITE_OPEN_READONLY (the TCL harness's
// "sqlite3 db test.db -readonly 1"). A missing file is an open error (no
// create, SQLITE_CANTOPEN); every write fails SQLITE_READONLY.
func OpenReadOnly(path string, pageSize uint32) (*Pager, error) {
	return openPager(path, pageSize, true)
}

func openPager(path string, pageSize uint32, forceReadOnly bool) (*Pager, error) {
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	// SQLite canonicalizes the filename through the VFS xFullPathname hook
	// (os_unix.c unixFullPathname -> appendOnePathElement) before open(2):
	// ".", ".." and duplicate/trailing slashes are resolved lexically, so a
	// path like "./a//b/../c//" opens "a/c" (lock3-1.1). filepath.Clean is
	// the lexical equivalent (no symlink resolution, matching SQLite's
	// no-readlink fallback).
	cleanPath := filepath.Clean(path)
	f, readOnlyFallback, err := openDatabaseFile(cleanPath, path, forceReadOnly)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("pager: stat %s: %w", path, err)
	}

	pr := &Pager{
		pageSize: pageSize,
		file:     f,
		pages:    make(map[uint32]*Page),
		dirty:    make(map[uint32]bool),
		fileSize: info.Size(),
		// pager.c lazy creation: a database opened empty (0 bytes) must
		// stay untouched on disk until the first real write — opening and
		// closing it never materializes the file.
		openedEmpty: info.Size() == 0,
		path:        cleanPath,
		// SQLite's default PRAGMA journal_size_limit cap is 32768 bytes
		// (pragma.c journalSizeLimit). A PERSIST journal is truncated to this
		// many bytes after a commit; negative means unlimited, 0 means zero.
		journalSizeLimit: 32768,
	}
	pr.readOnly = readOnlyFallback || forceReadOnly
	// Quota layer (test_quota.c quotaOpen): a database file opened while
	// the quota layer is initialized joins its matching quota group and
	// its size counts toward the group cap. No-op when uninitialized.
	quota.RegisterDBFile(cleanPath)

	if err := loadExistingImage(pr, f, info.Size()); err != nil {
		f.Close()
		return nil, err
	}
	// Baseline the external-change stamp on what we just opened (pager.c
	// records Pager.dbFileVers when page 1 is first read).
	pr.refreshKnownFileStamp()

	if err := recoverJournalAtOpen(pr, cleanPath); err != nil {
		f.Close()
		return nil, err
	}
	if err := attachWalAtOpen(pr, cleanPath); err != nil {
		f.Close()
		return nil, err
	}

	// A database whose file-format WRITE version exceeds 1 (WAL or a newer
	// format) cannot be written by a journal-mode connection: C marks the
	// pager read-only at lockBtree (rdonly-1.3/1.4 write version 3 into
	// byte 18 and expect reads to succeed while writes fail
	// SQLITE_READONLY, "attempt to write a readonly database"). WAL-mode
	// databases reopening with their -wal file enter WAL mode above and
	// stay writable.
	if !pr.readOnly && pr.wal == nil && len(pr.header) > 18 && pr.header[18] > 1 {
		pr.readOnly = true
	}

	return pr, nil
}

// openDatabaseFile opens the database file for the pager (pager.c
// sqlite3OsOpen / os_unix.c unixOpen). cleanPath is the canonicalized path
// to open; displayPath is the caller's spelling used in error strings.
// forceReadOnly opens an existing file O_RDONLY (SQLITE_OPEN_READONLY: a
// missing file is SQLITE_CANTOPEN, no create). When a read-write open of an
// existing file fails with EACCES, the file is reopened O_RDONLY and
// readOnly=true is reported (os_unix.c unixOpen fallback, readonly.test 1.1).
func openDatabaseFile(cleanPath, displayPath string, forceReadOnly bool) (*os.File, bool, error) {
	if forceReadOnly {
		// SQLITE_OPEN_READONLY: open an EXISTING file read-only; a missing
		// file is SQLITE_CANTOPEN (no create).
		f, err := os.OpenFile(cleanPath, os.O_RDONLY, 0644)
		if err != nil {
			return nil, false, fmt.Errorf("pager: open %s: unable to open database file", displayPath)
		}
		return f, false, nil
	}
	f, err := os.OpenFile(cleanPath, os.O_RDWR|os.O_CREATE, 0644)
	if err == nil {
		return f, false, nil
	}
	// sqlite3OsOpen failure maps to SQLITE_CANTOPEN "unable to open
	// database file" — including opening a path that is a directory
	// (quota-5.4.1: file mkdir test.db; sqlite3 db test.db).
	if strings.Contains(err.Error(), "is a directory") {
		return nil, false, fmt.Errorf("pager: open %s: unable to open database file", displayPath)
	}
	// os_unix.c unixOpen: a file that cannot be opened read-write
	// (EACCES — e.g. mode r--r--r--) is opened READ-ONLY and the pager
	// flagged readOnly; writes then fail with SQLITE_READONLY
	// (readonly.test 1.1). Only an existing file can fall back — a
	// missing file still surfaces the create error.
	if os.IsPermission(err) {
		if fi, serr := os.Stat(cleanPath); serr == nil && !fi.IsDir() {
			if rf, rerr := os.OpenFile(cleanPath, os.O_RDONLY, 0644); rerr == nil {
				return rf, true, nil
			}
		}
	}
	return nil, false, fmt.Errorf("pager: open %s: %w", displayPath, err)
}

// loadExistingImage reads the on-disk state of a non-empty database file at
// open time: the 100-byte header (the real page size) and the page count.
// A short or unparsable header does NOT fail the open — SQLite defers the
// error to the first statement touching the page (pr.headerCorrupt; see
// ValidateHeader). Hard I/O failures do fail the open. No-op for an empty
// file (pager.c lazy creation).
func loadExistingImage(pr *Pager, f *os.File, fileSize int64) error {
	if fileSize <= 0 {
		return nil
	}
	// Read the 100-byte header first: it contains the real page size.
	headerBuf := make([]byte, HeaderSize)
	n, err := f.ReadAt(headerBuf, 0)
	if err != nil && n < HeaderSize {
		// Short read (file smaller than the 100-byte header) — SQLite
		// does not fail on Open; it defers to the first statement that
		// touches the page (which then reports "file is not a database"
		// via btreeOpenTableCursor). Mirror that: keep the Pager open,
		// default pageSize to DefaultPageSize, and let the schema-init
		// path report the error. (corrupt2.test 1.2/1.3/1.5 and
		// incrvacuum.test-14.1 depend on this deferral.)
		pr.pageSize = DefaultPageSize
		pr.headerCorrupt = true
	} else if err != nil {
		return fmt.Errorf("pager: read header: %w", err)
	} else {
		applyOpenHeader(pr, headerBuf)
	}
	// Read full page 1 into a temporary buffer
	fullPage := make([]byte, pr.pageSize)
	if _, err := f.ReadAt(fullPage, 0); err != nil && err != io.EOF {
		return fmt.Errorf("pager: read page 1: %w", err)
	}
	pr.header = make([]byte, HeaderSize)
	copy(pr.header, fullPage[:HeaderSize])
	pr.numPages = uint32(fileSize / int64(pr.pageSize))
	if pr.numPages == 0 && fileSize > 0 {
		pr.numPages = 1
	}
	return nil
}

// applyOpenHeader adopts a parsed 100-byte header into the opening pager:
// page size, reserved-space byte and auto-vacuum mode. An unparsable header
// or a page-size field outside [512, 65536] power-of-two defers to the
// first statement (pr.headerCorrupt) instead of failing the open —
// sqlite3PagerOpen does not error on a bad header (corrupt2.test
// 1.2/1.3/1.5; corruptC-3 single-byte pokes into offsets 16-17).
func applyOpenHeader(pr *Pager, headerBuf []byte) {
	hdr, perr := storage.ParseHeader(headerBuf)
	if perr != nil {
		// SQLite's sqlite3PagerOpen does NOT fail on bad header parse: the
		// error is surfaced by the first statement that touches the page
		// (sqlite_master scan reports "file is not a database" via
		// btreeOpenTableCursor's locked-table flag and schema init's
		// SQLITE_NOTADB error path). Mirroring that: keep the Pager
		// open, default pageSize to DefaultPageSize, and let subsequent
		// reads detect the corruption. corrupt2.test 1.2/1.3/1.5
		// (corrupt2-1.2 expects `file is not a database` on the FIRST
		// statement, not on Open) and many other crash-recovery tests
		// require this deferral.
		pr.pageSize = DefaultPageSize
		pr.headerCorrupt = true
		return
	}
	if !validHeaderPageSize(hdr.PageSize) {
		// Header parses (magic intact) but the page-size field is not a
		// power of two in [512, 65536]. SQLite's lockBtree
		// (btree.c:3405-3411) rejects such a page 1 with SQLITE_NOTADB on
		// first use — adopting it here would e.g. make([]byte, 0) for a
		// zeroed field and crash before the check ever runs (corruptC-3
		// pokes single bytes into offsets 16-17). Same deferral as the
		// parse-error branch: Open stays non-failing, the first statement
		// surfaces "file is not a database" (verified against the
		// /usr/bin/sqlite3 oracle: invalid page size → error 26 on first
		// use).
		pr.pageSize = DefaultPageSize
		pr.headerCorrupt = true
		return
	}
	pr.pageSize = hdr.PageSize
	pr.header = make([]byte, HeaderSize)
	copy(pr.header, headerBuf)
	// Header byte 20: bytes reserved at the end of every page (used by
	// e.g. codec/checksum extensions). Payload distribution math must use
	// the USABLE size (pageSize - reserved), not the raw page size —
	// SQLite files written with reserved > 0 are otherwise unreadable.
	pr.reserved = uint32(hdr.ReservedSpace)
	// Restore the autovacuum mode from the file header. SQLite stores
	// the flag at offset 52, the same 4-byte slot the btree later
	// uses for BTREE_LARGEST_ROOT_PAGE (btree.c:2727 / 3537:
	// `pBt->autoVacuum = (get4byte(&zDbHeader[36+4*4])?1:0)`
	// and `put4byte(&data[36+4*4], pBt->autoVacuum)`). The
	// dual-use is intentional: a freshly-created autovacuum DB
	// has largest_root=1 (the schema btree lives at page 1), so
	// writing 1 there serves both purposes. A subsequent
	// btreeCreateTable raises the value to ≥3 once a user table
	// exists with the ptrmap page 2 reserved — still flagging
	// auto_vacuum=true. Without this restore, the engine reports
	// auto_vacuum=0 for a DB created with PRAGMA auto_vacuum=1
	// (incrvacuum-12.4 expects auto_vacuum=1 after close/reopen).
	if hdr.LargestBTreePage != 0 {
		pr.autoVacuum = true
	}
}

// recoverJournalAtOpen runs hot-journal crash recovery when a "-journal"
// sidecar accompanies the main database at open (pager.c pagerPlayback /
// hasHotJournal): its page records are the before-images of an uncommitted
// transaction whose process died. A reader must replay them into the page
// cache before the first read (never serving the crashed partial image) and
// unlink the journal. Stale journals (dbOrigSize mismatch, bad magic, or
// main-file change counter newer than the journal) are discarded without
// playback (journal1.test 1.2: a leftover journal from a prior database must
// not roll back into a new database).
func recoverJournalAtOpen(pr *Pager, cleanPath string) error {
	jpath := journalPath(cleanPath)
	if jpath == "" {
		return nil
	}
	if _, err := os.Stat(jpath); err != nil {
		return nil
	}
	return recoverHotJournal(pr, cleanPath)
}

// attachWalAtOpen performs WAL crash recovery / WAL-mode detection at open:
// SQLite auto-detects WAL from the presence of a valid "-wal" file. When one
// accompanies the main database, attach to the shared wal-index (rebuilding
// it from the "-wal" via the walIndexRecover port when the shared header does
// not parse) and place the connection in WAL mode so the WAL write path and
// the wal-index read path are active. A WAL database whose main file is still
// empty (uncheckpointed) carries its page size only in the "-wal" header, so
// prefer that when the main file did not yield a size.
func attachWalAtOpen(pr *Pager, cleanPath string) error {
	if _, err := os.Stat(cleanPath + "-wal"); err != nil {
		return nil
	}
	if pr.pageSize == 0 {
		if wps, ok := readWalPageSize(cleanPath + "-wal"); ok {
			pr.pageSize = wps
		}
	}
	w, err := openWal(pr, cleanPath, pr.pageSize)
	if err != nil {
		return err
	}
	pr.wal = w
	pr.journalMode = "wal"
	// Adopt the recovered wal-index state: the committed page count comes
	// from the last commit record (pager.c pagerPagecount reads nPage
	// from the wal-index when a WAL is open), and page 1 is materialized
	// through the wal-index read path so the cached header is the
	// committed image, not a stale main-file one.
	if n := w.hdr.NPage; n > 0 {
		pr.numPages = n
	}
	pr.header = nil
	if pr.numPages > 0 {
		if _, err := pr.readPageLocked(1); err != nil {
			return err
		}
	}
	return nil
}

// readWalPageSize returns the page size recorded in a "-wal" file's header, if
// the header is present and valid (used to size a WAL database whose main file
// is still empty).
func readWalPageSize(walPath string) (uint32, bool) {
	f, err := os.Open(walPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, WalHdrSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return 0, false
	}
	h, err := DecodeWalHeader(buf)
	if err != nil || !h.HeaderCksumOK {
		return 0, false
	}
	if h.PageSize == 0 {
		return 0, false
	}
	return h.PageSize, true
}

// OpenInMemory creates an in-memory pager.
func OpenInMemory(pageSize uint32) *Pager {
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	dh := storage.DefaultHeader(pageSize)
	return &Pager{
		pageSize: pageSize,
		file:     nil,
		pages:    make(map[uint32]*Page),
		dirty:    make(map[uint32]bool),
		numPages: 0,
		header:   dh.Encode(),
	}
}

// OpenInMemoryReadOnly is OpenInMemory for a :memory: database opened with
// SQLITE_OPEN_READONLY (sqlite3 db :memory: -readonly 1): every write fails
// with SQLITE_READONLY, "attempt to write a readonly database"
// (openv2-2.2).
func OpenInMemoryReadOnly(pageSize uint32) *Pager {
	pg := OpenInMemory(pageSize)
	pg.readOnly = true
	return pg
}

// ValidateHeader checks the database header fields SQLite validates at
// open/lockBtree time. A truncated file whose header still advertises more
// pages indicates a corrupt database ("database disk image is malformed").
//
// The freelist trunk/count and largest-root header fields are deliberately
// NOT validated here: SQLite reads them lazily at the point of use —
// allocateBtreePage (getAndInitPage on the head trunk → SQLITE_CORRUPT)
// and integrity_check's checkList ("Freelist: invalid page number N") —
// and never during schema reads (sqlite3InitOne). Schema loading must
// succeed on an image with a corrupt freelist pointer so integrity_check
// can REPORT the corruption (pragma6-1.2 loads a DB whose header trunk is
// 12255232; integrity_check returns the freelist message as a row).
// validHeaderPageSize mirrors lockBtree's page-size field check
// (btree.c: `((pageSize-1)&pageSize)!=0 || pageSize>SQLITE_MAX_PAGE_SIZE ||
// pageSize<=256` → SQLITE_NOTADB): the decoded field must be a power of two
// in [512, 65536] (storage.ParseHeader already maps the on-file value 1 to
// 65536). Used by openPager to defer obviously-bogus page sizes to the first
// statement instead of sizing internal buffers with them.
func validHeaderPageSize(ps uint32) bool {
	return ps >= 512 && ps <= 65536 && (ps&(ps-1)) == 0
}

// validateLockBtreeHeader mirrors btree.c lockBtree's page-1 header checks
// (SQLITE_NOTADB surface as "file is not a database"): magic prefix,
// payload fractions at offsets 21-23 (must be 64/32/32), page size at
// offset 16-17 (power of 2 in [512, 65536]; value 1 means 65536), and
// usable size (pageSize - reserved byte 20) >= 480. filefmt-1.2/1.6/1.7/1.8.
func validateLockBtreeHeader(hdr []byte) error {
	notadb := func() error { return fmt.Errorf("file is not a database") }
	if len(hdr) < HeaderSize {
		return notadb()
	}
	if string(hdr[:16]) != storage.HeaderMagic {
		return notadb()
	}
	if hdr[21] != 64 || hdr[22] != 32 || hdr[23] != 32 {
		return notadb()
	}
	ps := uint32(hdr[16])<<8 | uint32(hdr[17])
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps > 65536 || (ps&(ps-1)) != 0 {
		return notadb()
	}
	if ps-uint32(hdr[20]) < 480 {
		return notadb()
	}
	return nil
}

func (p *Pager) ValidateHeader() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.header == nil || p.numPages == 0 {
		return nil
	}
	// If Open observed a header that did not parse (bad magic / short header),
	// mirror SQLite's deferral: surface "file is not a database" on the first
	// statement that actually reads the schema btree. corrupt2.test 1.2/1.3/1.5
	// rely on this (Open succeeds, the next SELECT * FROM sqlite_master
	// returns the error).
	if p.headerCorrupt {
		return fmt.Errorf("file is not a database")
	}
	// lockBtree field checks (btree.c): magic, payload fractions
	// (offsets 21-23 must be 64/32/32), page size (offset 16-17: power of
	// 2 in [512, maxPageSize]), and usable size >= 480. filefmt-1.2/1.6/1.7
	// (bad magic, page size 1025/256) and filefmt-1.8 (usable 512-33<480)
	// expect SQLITE_NOTADB ("file is not a database") at prepare time.
	if err := validateLockBtreeHeader(p.header); err != nil {
		return err
	}
	// lockBtree (btree.c:3401): a header page count (offset 28, trusted
	// only when the change counter matches version-valid-for) that
	// exceeds the file's actual page count means the file was truncated
	// underneath the header — malformed. Corrupt2/incrvacuum suites load
	// images cut short while the header still advertises more pages.
	if p.HeaderBeyondFile() {
		return fmt.Errorf("database disk image is malformed")
	}
	return nil
}
