// Statement-journal (pager.c sub-journal) tests: statement-scoped
// before-image capture and rollback.
//
// Contracts under test (see pagerstmt.go):
//   - a statement that modifies ZERO pages leaves the journal empty and
//     rollback is a no-op (the P5 perf contract);
//   - a statement that modifies N pages rolls back exactly those N pages;
//   - pages ALLOCATED during the statement are dropped by rollback;
//   - nested scopes compose: an inner rollback restores only the inner
//     statement's pages, an inner commit splices its images into the outer
//     scope so the outer rollback still reaches past it;
//   - a page already dirty at statement start (an earlier statement of the
//     same transaction wrote it) restores from memory, keeping the earlier
//     statement's writes;
//   - memory pagers roll back correctly without a file;
//   - a whole-state Restore invalidates open scopes (stale tokens no-op).
package pager

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writePageLocked writes pattern bytes into a page through the sanctioned
// write path so the dirty marking and journal capture see a real write.
func writePage(t *testing.T, p *Pager, pgno uint32, fill byte) {
	t.Helper()
	pg, err := p.ReadPage(pgno)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", pgno, err)
	}
	for i := range pg.Data {
		pg.Data[i] = fill
	}
	if err := p.WritePage(pg); err != nil {
		t.Fatalf("WritePage(%d): %v", pgno, err)
	}
}

// readPageFill returns the first data byte of a page.
func readPageFill(t *testing.T, p *Pager, pgno uint32) byte {
	t.Helper()
	pg, err := p.ReadPage(pgno)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", pgno, err)
	}
	return pg.Data[0]
}

func openStmtFilePager(t *testing.T) (*Pager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stmt.db")
	p, err := Open(path, 512)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p, path
}

// TestStmtJournalZeroPageStatement: a statement scope that modifies nothing
// journals nothing and rolls back nothing — the P5 contract that makes a
// no-match DELETE O(1).
func TestStmtJournalZeroPageStatement(t *testing.T) {
	p, _ := openStmtFilePager(t)
	// Materialize pages 1-2, write page 1, and COMMIT (flush) so every page
	// is clean — the state a scan (no-match DELETE) actually sees.
	allocPagesLocked(p, 2)
	writePage(t, p, 1, 0xAA)
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	before := p.Snapshot()

	stmt := p.BeginStatement()
	// Reads only (no writes).
	if _, err := p.ReadPage(1); err != nil {
		t.Fatalf("read: %v", err)
	}
	if stmt.stmtEntryCount() != 0 {
		t.Fatalf("read-only statement journalled %d pages, want 0", stmt.stmtEntryCount())
	}
	p.EndStatement(stmt)

	after := p.Snapshot()
	if !samePagerState(before, after) {
		t.Fatalf("read-only statement changed pager state")
	}
	// Rollback on an empty journal is a no-op too.
	stmt2 := p.BeginStatement()
	p.RollbackStatement(stmt2)
	after2 := p.Snapshot()
	if !samePagerState(before, after2) {
		t.Fatalf("empty-journal rollback changed pager state")
	}
}

// TestStmtJournalNPageRollback: a statement that dirties N pages rolls back
// exactly those pages; pages other statements wrote survive.
func TestStmtJournalNPageRollback(t *testing.T) {
	p, _ := openStmtFilePager(t)
	allocPagesLocked(p, 2)   // materialize pages 1-2
	writePage(t, p, 2, 0x11) // statement 1's write (stays dirty: no flush)
	outer := p.Snapshot()

	// Statement 2: modify page 2 AND allocate a new page, then fail.
	stmt := p.BeginStatement()
	writePage(t, p, 2, 0x22)
	newPg := p.AllocatePage()
	if newPg == nil {
		t.Fatalf("AllocatePage returned nil")
	}
	newPg.Data[0] = 0x33
	if err := p.WritePage(newPg); err != nil {
		t.Fatalf("WritePage(new): %v", err)
	}
	if stmt.stmtEntryCount() == 0 {
		t.Fatalf("statement with writes journalled nothing")
	}
	p.RollbackStatement(stmt)

	// Page 2 back to statement-2's start (0x11 — statement 1's write kept).
	if got := readPageFill(t, p, 2); got != 0x11 {
		t.Fatalf("page 2 fill after rollback = %#x, want 0x11 (earlier statement's write must survive)", got)
	}
	// The allocated page is gone; the page count is restored.
	if _, err := p.ReadPage(newPg.PageNum); err == nil {
		t.Fatalf("allocated page %d survived rollback", newPg.PageNum)
	}
	if p.numPages != outer.numPages {
		t.Fatalf("numPages after rollback = %d, want %d", p.numPages, outer.numPages)
	}
	// Dirty flags: page 2 was dirty at statement start → stays dirty (its
	// image is memory-only).
	if !p.dirty[2] {
		t.Fatalf("page 2 must stay dirty after statement rollback (uncommitted earlier write)")
	}
}

// TestStmtJournalNestedScopes: inner rollback restores only the inner
// statement's writes; inner commit splices into the outer scope so the outer
// rollback still reaches the inner pages.
func TestStmtJournalNestedScopes(t *testing.T) {
	p, _ := openStmtFilePager(t)
	allocPagesLocked(p, 2) // materialize pages 1-2
	writePage(t, p, 1, 0x01)

	// Outer statement writes page 1, then an inner statement (trigger body)
	// writes page 2 and FAILS.
	outer := p.BeginStatement()
	writePage(t, p, 1, 0x0A)

	inner := p.BeginStatement()
	writePage(t, p, 2, 0x0B)
	p.RollbackStatement(inner) // inner failure
	if got := readPageFill(t, p, 2); got == 0x0B {
		t.Fatalf("inner statement's write survived inner rollback")
	}
	// Outer statement continues and writes page 2 differently.
	writePage(t, p, 2, 0x0C)
	// Outer rollback: BOTH the 0x0A and 0x0C writes must undo.
	p.RollbackStatement(outer)
	if got := readPageFill(t, p, 1); got != 0x01 {
		t.Fatalf("page 1 after outer rollback = %#x, want 0x01", got)
	}
	if got := readPageFill(t, p, 2); got == 0x0C {
		t.Fatalf("outer statement's page-2 write survived outer rollback")
	}
}

// TestStmtJournalNestedCommitSplice: an inner scope that ENDS (statement
// success) splices its entries into the outer scope, so a later outer
// rollback still undoes the inner statement's writes.
func TestStmtJournalNestedCommitSplice(t *testing.T) {
	p, _ := openStmtFilePager(t)
	allocPagesLocked(p, 2) // materialize pages 1-2
	writePage(t, p, 1, 0x01)

	outer := p.BeginStatement()
	inner := p.BeginStatement()
	writePage(t, p, 2, 0x0B)
	p.EndStatement(inner) // inner statement SUCCEEDS
	// Outer fails after the inner committed: page 2 must roll back too.
	p.RollbackStatement(outer)
	if got := readPageFill(t, p, 2); got == 0x0B {
		t.Fatalf("committed inner write survived outer rollback (splice failed)")
	}
}

// TestStmtJournalBeginDirtyFromMemory: a page already dirty at statement
// start (earlier statement in the same transaction) rolls back to the
// earlier statement's image, not the stale disk image.
func TestStmtJournalBeginDirtyFromMemory(t *testing.T) {
	p, path := openStmtFilePager(t)
	allocPagesLocked(p, 2) // materialize pages 1-2
	// Statement 1 writes page 2 (stays dirty — no commit).
	writePage(t, p, 2, 0x11)
	// Statement 2 rewrites page 2 and fails: must restore 0x11 (memory
	// image), NOT the pre-transaction disk image.
	stmt := p.BeginStatement()
	writePage(t, p, 2, 0x22)
	p.RollbackStatement(stmt)
	if got := readPageFill(t, p, 2); got != 0x11 {
		t.Fatalf("page 2 after rollback = %#x, want 0x11 (earlier statement's uncommitted write)", got)
	}
	_ = path
}

// TestStmtJournalMemoryPager: memory pagers (no file) capture every image
// from memory and roll back correctly.
func TestStmtJournalMemoryPager(t *testing.T) {
	p := OpenInMemory(512)
	defer p.Close()
	allocPagesLocked(p, 2) // materialize pages 1-2
	writePage(t, p, 2, 0x55)

	stmt := p.BeginStatement()
	writePage(t, p, 2, 0x66)
	pg3 := p.AllocatePage()
	pg3.Data[0] = 0x77
	if err := p.WritePage(pg3); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	p.RollbackStatement(stmt)
	if got := readPageFill(t, p, 2); got != 0x55 {
		t.Fatalf("memory pager page 2 after rollback = %#x, want 0x55", got)
	}
	if _, err := p.ReadPage(pg3.PageNum); err == nil {
		t.Fatalf("memory pager allocated page survived rollback")
	}
}

// TestStmtJournalRestoreInvalidatesScopes: a whole-state Restore (transaction
// rollback / savepoint) marks open statement scopes done — later token
// operations are no-ops.
func TestStmtJournalRestoreInvalidatesScopes(t *testing.T) {
	p, _ := openStmtFilePager(t)
	allocPagesLocked(p, 1) // materialize page 1
	writePage(t, p, 1, 0x10)
	snap := p.Snapshot()
	stmt := p.BeginStatement()
	writePage(t, p, 1, 0x20)
	p.Restore(snap) // transaction-level rollback supersedes the scope
	if got := readPageFill(t, p, 1); got != 0x10 {
		t.Fatalf("page 1 after Restore = %#x, want 0x10", got)
	}
	// Stale token operations must not panic or re-apply stale state.
	p.RollbackStatement(stmt)
	p.EndStatement(stmt)
	if got := readPageFill(t, p, 1); got != 0x10 {
		t.Fatalf("stale statement token resurrected state: %#x", got)
	}
}

// TestStmtJournalFileTruncateRestore: a statement that shrinks the page
// count (in-transaction truncate) rolls the FILE back to the scope's size in
// both directions (restoreFileImageLocked contract).
func TestStmtJournalFileTruncateRestore(t *testing.T) {
	p, path := openStmtFilePager(t)
	// Grow to 3 pages and COMMIT so the file is materialized at 3 pages.
	allocPagesLocked(p, 1) // materialize page 1
	writePage(t, p, 1, 0x01)
	for i := 0; i < 2; i++ {
		pg := p.AllocatePage()
		pg.Data[0] = byte(0x40 + pg.PageNum)
		if err := p.WritePage(pg); err != nil {
			t.Fatalf("WritePage: %v", err)
		}
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	sizeAt3 := info.Size()
	if sizeAt3 != 3*512 {
		t.Fatalf("file size after commit = %d, want %d", sizeAt3, 3*512)
	}

	// Statement shrinks the image (in-memory truncate like incremental
	// vacuum), then fails: the file must return to 3 pages.
	stmt := p.BeginStatement()
	if err := p.Truncate(1); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	p.RollbackStatement(stmt)
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != sizeAt3 {
		t.Fatalf("file size after rollback = %d, want %d (both-direction truncate)", info.Size(), sizeAt3)
	}
}

// TestStmtJournalWALModeInMemoryRollback: in WAL mode the statement rollback
// is the in-memory page restore; the main database file is untouched (it is
// updated only by Checkpoint).
func TestStmtJournalWALModeInMemoryRollback(t *testing.T) {
	p, path := openStmtFilePager(t)
	allocPagesLocked(p, 1) // materialize page 1
	writePage(t, p, 1, 0x01)
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	sizeBefore := info.Size()
	// Switch to WAL (the WAL writer needs the sidecar files next to the db).
	if err := p.SetJournalMode("wal"); err != nil {
		t.Fatalf("SetJournalMode(wal): %v", err)
	}
	stmt := p.BeginStatement()
	writePage(t, p, 1, 0x02)
	p.RollbackStatement(stmt)
	if got := readPageFill(t, p, 1); got != 0x01 {
		t.Fatalf("WAL pager page 1 after statement rollback = %#x, want 0x01", got)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != sizeBefore {
		t.Fatalf("WAL mode: main file size changed by statement rollback: %d -> %d", sizeBefore, info.Size())
	}
}

// TestStmtJournalMixedReadWriteExactRestore pins the journal's exactness
// contract end-to-end: a statement that READS and WRITES a mix of pages
// (clean pages, pages already dirty at statement start, freshly allocated
// pages — spanning both the linear-list and the map-overflow journal forms)
// restores every page's EXACT bytes on rollback. Whole page images are
// hashed before and after, not just spot-checked.
func TestStmtJournalMixedReadWriteExactRestore(t *testing.T) {
	for _, mode := range []string{"memory", "file"} {
		t.Run(mode, func(t *testing.T) {
			var p *Pager
			if mode == "memory" {
				p = OpenInMemory(512)
			} else {
				p, _ = openStmtFilePager(t)
			}
			defer p.Close()
			const nPages = 30 // past stmtListMax: the journal spills to its map
			allocPagesLocked(p, nPages)
			for pgno := uint32(1); pgno <= nPages; pgno++ {
				writePage(t, p, pgno, byte(pgno)) // commit-dirty every page
			}
			if err := p.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			// Make page 2 dirty at statement start (an earlier statement's
			// uncommitted write the rollback must keep).
			writePage(t, p, 2, 0xEE)

			hashPages := func() map[uint32][32]byte {
				t.Helper()
				out := make(map[uint32][32]byte)
				for pgno := uint32(1); pgno <= nPages; pgno++ {
					pg, err := p.ReadPage(pgno)
					if err != nil {
						t.Fatalf("ReadPage(%d): %v", pgno, err)
					}
					out[pgno] = sha256.Sum256(pg.Data)
				}
				return out
			}
			before := hashPages()

			stmt := p.BeginStatement()
			// Mixed reads and writes: rewrite a spread of pages, read others,
			// allocate pages past the statement's starting count.
			for pgno := uint32(3); pgno <= nPages; pgno += 2 {
				pg, err := p.ReadPage(pgno) // read first, then overwrite
				if err != nil {
					t.Fatalf("ReadPage(%d): %v", pgno, err)
				}
				pg.Data[10] ^= 0xFF
				if err := p.WritePage(pg); err != nil {
					t.Fatalf("WritePage(%d): %v", pgno, err)
				}
			}
			for pgno := uint32(4); pgno <= nPages; pgno += 2 {
				if _, err := p.ReadPage(pgno); err != nil { // reads only
					t.Fatalf("ReadPage(%d): %v", pgno, err)
				}
			}
			extra := p.AllocatePage()
			extra.Data[0] = 0x99
			if err := p.WritePage(extra); err != nil {
				t.Fatalf("WritePage(extra): %v", err)
			}
			p.RollbackStatement(stmt)

			after := hashPages()
			for pgno := uint32(1); pgno <= nPages; pgno++ {
				if before[pgno] != after[pgno] {
					t.Fatalf("page %d bytes changed by statement rollback (exact-restore pin)", pgno)
				}
			}
			if _, err := p.ReadPage(extra.PageNum); err == nil {
				t.Fatalf("allocated page %d survived rollback", extra.PageNum)
			}
		})
	}
}

// samePagerState compares the observable state of two snapshots.
func samePagerState(a, b *PagerState) bool {
	if a.numPages != b.numPages || a.fileSize != b.fileSize {
		return false
	}
	if len(a.pages) != len(b.pages) || len(a.dirty) != len(b.dirty) {
		return false
	}
	for n, pg := range a.pages {
		bp, ok := b.pages[n]
		if !ok || !equalBytes(pg.Data, bp.Data) {
			return false
		}
	}
	if (a.header == nil) != (b.header == nil) {
		return false
	}
	if a.header != nil && !equalBytes(a.header, b.header) {
		return false
	}
	return true
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStmtJournalHeaderRestore: header changes made by a failed statement
// (schema cookie bumps, page-count updates) roll back with the pages.
func TestStmtJournalHeaderRestore(t *testing.T) {
	p, _ := openStmtFilePager(t)
	allocPagesLocked(p, 1) // materialize page 1
	writePage(t, p, 1, 0x01)
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Re-read the file image so the 100-byte header materializes.
	p.InvalidateCache()
	if _, err := p.ReadPage(1); err != nil {
		t.Fatalf("ReadPage(1): %v", err)
	}
	if len(p.header) < 44 {
		t.Fatalf("header not materialized")
	}
	cookieBefore := binary.BigEndian.Uint32(p.header[40:44])

	stmt := p.BeginStatement()
	p.BumpSchemaCookie()
	cookieAfter := binary.BigEndian.Uint32(p.header[40:44])
	if cookieAfter != cookieBefore+1 {
		t.Fatalf("cookie bump did not apply: %d -> %d", cookieBefore, cookieAfter)
	}
	p.RollbackStatement(stmt)
	if got := binary.BigEndian.Uint32(p.header[40:44]); got != cookieBefore {
		t.Fatalf("schema cookie after rollback = %d, want %d", got, cookieBefore)
	}
}
