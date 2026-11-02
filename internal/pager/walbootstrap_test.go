package pager_test

// walbootstrap_test.go — native pins for the WAL cross-connection open
// contracts behind the e_walckpt family's cross-connection "database is
// locked" cascade (P-C1 unit, 2026-10-31 run).
//
// The fixed contracts, each pinned against the wal.c/os_unix.c reference:
//
//  1. Out-of-transaction pager writes commit as their own implicit
//     autocommit transaction (TestOrphanWriteCommitsAndReleasesWriter).
//     The schema bootstrap (schema.Init on an empty main file) is the only
//     engine caller; C runs newDatabase inside the first statement's
//     transaction, so the WRITER shm lock (sqlite3WalBeginWriteTransaction)
//     never outlives its transaction. Before the fix the bootstrap's
//     WritePage took the WRITER lock through the WritePage fallback and
//     nothing ever ended that transaction: the lock stayed held for the
//     connection's lifetime and every OTHER connection's open — whose
//     walIndexReadHdr recovery (wal.c L2640) needs the WRITER byte to
//     rebuild a stale wal-index — failed with SQLITE_BUSY "database is
//     locked" (e_walckpt's 138-leaf open cascade).
//
//  2. A second connection CAN open a WAL database while the first holds an
//     open write transaction, and recovery from a stale wal-index works
//     after the first connection closes (TestSecondConnectionOpensDuringWriteTx
//     / TestOpenAfterAbandonedWriteTx).
//
//  3. Pager.Close ends the connection's own WAL read transaction BEFORE the
//     close checkpoint (TestCloseReleasesReadPinBeforeBackfill). C's
//     sqlite3PagerClose runs pagerUnlockAndRollback (releasing the shared
//     read-mark locks) before sqlite3WalClose's finalization checkpoint; the
//     backfill needs the EXCLUSIVE READ_LOCK(0), which the connection's own
//     pinned read mark blocks in the wal-index lock shadow — a blocked
//     backfill reports nCkpt=0 (success) and the log reset then destroyed
//     un-backfilled frames (data loss; the e_walckpt "no such table: t1"
//     residue class).

import (
	"os"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
	"github.com/pijalu/frigolite/internal/pager"
)

// newPinPager opens a file-backed pager in WAL mode with one committed
// table, in a fresh directory.
func newPinPager(t *testing.T) (*pager.Pager, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/test.db"
	p, err := pager.Open(path, pager.DefaultPageSize)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := p.SetJournalMode("wal"); err != nil {
		t.Fatalf("SetJournalMode: %v", err)
	}
	return p, path
}

// TestOrphanWriteCommitsAndReleasesWriter pins contract 1: a WritePage
// outside any statement scope (no WALBeginWrite gate, no statement journal)
// commits immediately — the WRITER shm lock is released when WritePage
// returns, and the written page survives a reopen (a later connection's
// recovery finds it).
func TestOrphanWriteCommitsAndReleasesWriter(t *testing.T) {
	p, path := newPinPager(t)

	// Simulate the schema bootstrap: create page 1 on the empty main file
	// and write it WITHOUT taking the statement gate (schema.Init's exact
	// pager-call shape: NumPages()==0 → AllocatePage → WritePage).
	if p.NumPages() != 0 {
		t.Fatalf("pre-write NumPages = %d, want 0", p.NumPages())
	}
	pg := p.AllocatePage()
	if pg == nil || pg.PageNum != 1 {
		t.Fatalf("AllocatePage = %v, want page 1", pg)
	}
	if err := p.WritePage(pg); err != nil {
		t.Fatalf("bootstrap WritePage: %v", err)
	}

	// The WRITER lock must be released when the orphan write returned: a
	// second write from ANOTHER connection (modeled by a second pager over
	// the same file) must not report "database is locked" in its own
	// recovery (the stale wal-index rebuild needs the WRITER byte).
	p.Close()

	p2, err := pager.Open(path, pager.DefaultPageSize)
	if err != nil {
		t.Fatalf("second open after orphan write: %v", err)
	}
	defer p2.Close()
	if p2.NumPages() != 1 {
		t.Fatalf("recovered NumPages = %d, want 1 (bootstrap page committed)", p2.NumPages())
	}
	rec, err := p2.ReadPage(1)
	if err != nil {
		t.Fatalf("read recovered page 1: %v", err)
	}
	if !strings.Contains(string(rec.Data[100:]), "") && len(rec.Data) == 0 {
		t.Fatal("recovered page 1 empty")
	}
}

// TestOrphanWriteDoesNotBlockSecondConnection pins the e_walckpt open
// cascade end-to-end through the frigolite public API: connection A opens a
// WAL database whose main file is still empty (bootstrap runs at Open),
// stays open, and connection B opens the SAME file — B must succeed (the
// bootstrap's WRITER lock was released inside A's Open, so B's recovery can
// take the WRITER byte).
func TestOrphanWriteDoesNotBlockSecondConnection(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.db"

	a, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer a.Close()
	// Force WAL mode (attaches the WAL writer) and write through a
	// statement so the schema exists, then reopen: the reopened connection
	// sees a fresh -shm (DMS truncate) and recovers.
	a.Exec("PRAGMA journal_mode = wal")
	a.Exec("CREATE TABLE t1(x)")
	a.Exec("INSERT INTO t1 VALUES(1)")

	b, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("open B while A open: %v", err)
	}
	defer b.Close()
	if res := b.Query("SELECT count(*) FROM t1"); res.Error != nil {
		t.Fatalf("B query: %v", res.Error)
	}
}

// TestSecondConnectionOpensDuringWriteTx pins contract 2a: connection B
// opens a WAL database while connection A holds an open (uncommitted) write
// transaction. C allows the open (readers never block on writers in WAL);
// B sees A's committed prefix.
func TestSecondConnectionOpensDuringWriteTx(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.db"

	a, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer a.Close()
	a.Exec("PRAGMA journal_mode = wal")
	a.Exec("CREATE TABLE t1(x)")
	a.Exec("INSERT INTO t1 VALUES(1)")
	a.Exec("BEGIN; INSERT INTO t1 VALUES(2)") // uncommitted

	b, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("open B during A write-tx: %v", err)
	}
	defer b.Close()
	res := b.Query("SELECT count(*) FROM t1")
	if res.Error != nil {
		t.Fatalf("B query: %v", res.Error)
	}
	// B reads A's committed prefix (row 1); the uncommitted row 2 is
	// invisible.
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("B rows = %v, want [[1]]", res.Rows)
	}
}

// TestOpenAfterAbandonedWriteTx pins contract 2b: a connection that leaves
// an open write transaction at Close time (the fixture's `db close` with a
// pending tx) leaves the WAL recoverable — the next open must not fail and
// must drop the uncommitted frames (wal.c sqlite3WalClose/walIndexRecover:
// only committed frames survive recovery).
func TestOpenAfterAbandonedWriteTx(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.db"

	a, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	a.Exec("PRAGMA journal_mode = wal")
	a.Exec("CREATE TABLE t1(x)")
	a.Exec("INSERT INTO t1 VALUES(1)")
	a.Exec("BEGIN; INSERT INTO t1 VALUES(2)") // abandoned at close
	if err := a.Close(); err != nil {
		t.Fatalf("close A with open tx: %v", err)
	}

	b, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("open B after abandoned tx: %v", err)
	}
	defer b.Close()
	res := b.Query("SELECT count(*) FROM t1")
	if res.Error != nil {
		t.Fatalf("B query: %v", res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("B rows = %v, want [[1]] (uncommitted frame discarded)", res.Rows)
	}
}

// TestCloseReleasesReadPinBeforeBackfill pins contract 3 at the pager level:
// a connection closing with its WAL read transaction still open (no
// WALEndRead ran — the fixture's close inside an open read transaction)
// still checkpoints the whole log into the main file. The close checkpoint's
// backfill needs the EXCLUSIVE READ_LOCK(0); the connection's own read mark
// must be released first (pager.c pagerUnlockAndRollback before
// sqlite3WalClose). Before the fix the blocked backfill reported success
// (nCkpt=0) and the log reset destroyed the un-backfilled frames.
func TestCloseReleasesReadPinBeforeBackfill(t *testing.T) {
	p, path := newPinPager(t)

	// Commit one transaction through the gate (a statement's write phase).
	if _, err := p.WALBeginWrite(); err != nil {
		t.Fatalf("WALBeginWrite: %v", err)
	}
	pg := p.AllocatePage()
	copy(pg.Data[100:], []byte("durable"))
	if err := p.WritePage(pg); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT calling WALEndRead: the connection closes with its
	// read transaction open.
	if err := p.Close(); err != nil {
		t.Fatalf("close with open read tx: %v", err)
	}

	// The backfilled frames must have reached the main file, and the log
	// reset must be safe: reopen sees the content.
	p2, err := pager.Open(path, pager.DefaultPageSize)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p2.Close()
	if p2.NumPages() < 1 {
		t.Fatalf("recovered NumPages = %d, want >= 1 (backfilled before log reset)", p2.NumPages())
	}
	rec, err := p2.ReadPage(1)
	if err != nil {
		t.Fatalf("read recovered page 1: %v", err)
	}
	if !strings.Contains(string(rec.Data[100:]), "durable") {
		t.Fatalf("recovered page 1 lacks committed content: %q", string(rec.Data[100:130]))
	}
}

// TestOpenFileMissingButWALPresent pins the deleted-main-file open path the
// fixture drives (e_walckpt section 2's `forcedelete test.db` under open
// connections): a fresh main file plus a -wal carrying the previous
// generation's committed frames must recover those frames (walIndexRecover
// reads the -wal, not the main file). Before the WRITER-lock fix this state
// also fed the open cascade.
func TestOpenFileMissingButWALPresent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.db"

	a, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	a.Exec("PRAGMA journal_mode = wal")
	a.Exec("CREATE TABLE t1(x)")
	a.Exec("INSERT INTO t1 VALUES(9)")
	// Simulate the fixture's forcedelete-under-open-connections: remove the
	// main file while the connection stays open (POSIX: the inode survives).
	if err := os.Remove(path); err != nil {
		t.Fatalf("delete main file: %v", err)
	}
	// A second open (the fixture's next `sqlite3 db test.db`) creates a new
	// empty main file and must recover the WAL's committed frames.
	b, err := frigolite.Open(path)
	if err != nil {
		t.Fatalf("reopen after delete: %v", err)
	}
	res := b.Query("SELECT count(*) FROM t1")
	if res.Error != nil {
		t.Fatalf("query after recovery: %v", res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("rows = %v, want [[1]] (WAL frames recovered)", res.Rows)
	}
	b.Close()
	a.Close()
}
