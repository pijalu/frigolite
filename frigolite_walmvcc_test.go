package frigolite

// Native anchors for P7.WAL-G7 slice 3 — reader read-marks + MVCC
// visibility (plan/goals/P7.WAL-G7.md, Slice 3). C ground truth: src/wal.c
// walTryBeginRead (read-mark selection + bumping + shared pin), walIndexReadHdr,
// sqlite3WalEndReadTransaction, walFindFrame's minFrame rule, walRestartLog.
//
// Contracts pinned here:
//   - a reader inside an explicit transaction keeps its snapshot while a
//     writer commits (repeatable reads); the commit becomes visible at the
//     next read transaction (after COMMIT/ROLLBACK);
//   - a stale-snapshot writer fails with "database is locked"
//     (SQLITE_BUSY_SNAPSHOT) and succeeds after the transaction ends;
//   - a parked writer excludes a second writer ("database is locked");
//   - a checkpoint backfills only up to the readers' marks;
//   - read marks are observable and shared (5 concurrent readers coexist);
//   - a page rewritten later in the log resolves to the newest frame at or
//     above the reader's minFrame (never a checkpointed stale version).

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pijalu/frigolite/internal/pager"
)

// TestWalMVCCExclusiveModeVisibility pins the walnoshm contract's
// engine-visible core: WAL is fully functional with every shm lock a no-op
// (locking_mode=EXCLUSIVE — wal.c walLockShared/walLockExclusive bypass,
// which is how C runs WAL with no shared memory), including cross- and
// same-connection visibility.
func TestWalMVCCExclusiveModeVisibility(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("PRAGMA locking_mode=EXCLUSIVE"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Another connection (normal locking) reads the committed row through
	// the shared wal-index.
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "1" {
		t.Errorf("conn2 count under exclusive writer = %s, want 1", got)
	}
	// The exclusive writer reads its own data back and keeps committing.
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "1" {
		t.Errorf("conn1 count = %s, want 1", got)
	}
	if res := c1.Exec("INSERT INTO t1 VALUES(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN; INSERT INTO t1 VALUES(3); COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("exclusive-mode count = %s, want 3", got)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("conn2 final count = %s, want 3", got)
	}
	r := c1.Query("PRAGMA integrity_check")
	if r.Error != nil || formatSQLiteValue(r.Rows[0][0]) != "ok" {
		t.Errorf("integrity_check: %v %v", r.Error, r.Rows)
	}
}

// ckptTripleInt reads one cell of a PRAGMA wal_checkpoint result row as an
// int (the (busy, nLog, nCkpt) triple columns are numeric).
func ckptTripleInt(t *testing.T, r *Result, col int) int {
	t.Helper()
	if r.Error != nil || len(r.Rows) == 0 {
		t.Fatalf("checkpoint row: %v", r.Error)
	}
	v, err := strconv.Atoi(formatSQLiteValue(r.Rows[0][col]))
	if err != nil {
		t.Fatalf("checkpoint column %d: %v", col, err)
	}
	return v
}

// TestWalMVCCRepeatableRead pins the core MVCC boundary: a reader that
// began a read transaction keeps its snapshot across another connection's
// commits, and observes them at the next read transaction (C: the frozen
// pWal->hdr + the shared WAL_READ_LOCK pin).
func TestWalMVCCRepeatableRead(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// Begin the reader's transaction and pin its snapshot with a read.
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "1" {
		t.Fatalf("pre-commit read = %s, want 1", got)
	}

	// Another connection commits twice while the read txn is open.
	if res := c2.Exec("INSERT INTO t1 VALUES(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c2.Exec("INSERT INTO t1 VALUES(3)"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// Repeatable read: still the BEGIN-time snapshot.
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "1" {
		t.Errorf("read txn sees later commit: count = %s, want 1 (frozen snapshot)", got)
	}
	if got := queryInt(t, c1, "SELECT sum(a) FROM t1"); got != "1" {
		t.Errorf("read txn sum = %s, want 1", got)
	}

	// After the read transaction ends, the next statement re-pins and sees
	// everything.
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("post-commit count = %s, want 3", got)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("conn2 count = %s, want 3", got)
	}
}

// TestWalMVCCVisibilityAtStatementBoundaries pins autocommit visibility:
// every new statement observes the other connection's committed writes (the
// read mark is released at statement end), including the reverse direction.
func TestWalMVCCVisibilityAtStatementBoundaries(t *testing.T) {
	c1, c2 := openWalPair(t)
	for i := 1; i <= 4; i++ {
		w := c1
		if i%2 == 0 {
			w = c2
		}
		if res := w.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
			t.Fatalf("insert %d: %v", i, res.Error)
		}
		// The OTHER connection must see the commit at its next statement.
		r := c1.Query("SELECT count(*) FROM t1")
		if r.Error != nil {
			t.Fatalf("read %d: %v", i, r.Error)
		}
		if got := formatSQLiteValue(r.Rows[0][0]); got != itoa(i) {
			t.Errorf("after insert %d: conn1 count = %s, want %d", i, got, i)
		}
	}
}

// TestWalMVCCWriterSnapshotBusy pins walprotocol2-2.2/2.3 under a real
// transaction: a writer whose read transaction pinned an older snapshot
// fails with "database is locked" (SQLITE_BUSY_SNAPSHOT) when another
// connection committed in between; after ROLLBACK the write succeeds.
func TestWalMVCCWriterSnapshotBusy(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Open the reader's transaction and pin the snapshot.
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "1" {
		t.Fatal(got)
	}
	// The competing commit lands behind the frozen pin.
	if res := c2.Exec("INSERT INTO t1 VALUES(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// The stale-snapshot write must fail; the transaction stays open.
	if res := c1.Exec("INSERT INTO t1 VALUES(3)"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "database is locked") {
		t.Fatalf("stale-snapshot write: want 'database is locked', got %v", res.Error)
	}
	// The read snapshot is still frozen (and the failed write rolled back).
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "1" {
		t.Errorf("post-busy read = %s, want 1", got)
	}
	// Ending the transaction releases the pin: the write now lands.
	if res := c1.Exec("ROLLBACK"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("INSERT INTO t1 VALUES(3)"); res.Error != nil {
		t.Fatalf("write after ROLLBACK: %v", res.Error)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("conn2 count = %s, want 3", got)
	}
}

// TestWalMVCCSecondWriterBusy pins the WRITER-lock exclusion: a connection
// holding an open write transaction (BEGIN IMMEDIATE + write) excludes a
// second writer ("database is locked", C SQLITE_BUSY); once the holder
// commits, the second writer proceeds. A read transaction never blocks the
// writer (WAL readers don't take the WRITER byte).
func TestWalMVCCSecondWriterBusy(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("BEGIN IMMEDIATE"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Second writer: excluded while the write transaction is open.
	if res := c2.Exec("INSERT INTO t1 VALUES(2)"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "database is locked") {
		t.Fatalf("second writer: want 'database is locked', got %v", res.Error)
	}
	// Second READER: unaffected (the read-mark pin never needs the WRITER).
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "0" {
		t.Errorf("reader behind writer = %s, want 0 (uncommitted invisible)", got)
	}
	// After COMMIT the second writer proceeds and sees both rows.
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c2.Exec("INSERT INTO t1 VALUES(2)"); res.Error != nil {
		t.Fatalf("writer after commit: %v", res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "2" {
		t.Errorf("count = %s, want 2", got)
	}
}

// TestWalMVCCReadMarksObservable pins the read-mark protocol state: a
// finished statement leaves its mark value in aReadMark[] (a later reader
// shares it) and no read lock held between statements; a checkpoint's
// backfill covers the marks (nCkpt == nLog with no active reader).
func TestWalMVCCReadMarksObservable(t *testing.T) {
	c1, c2 := openWalPair(t)
	for i := 1; i <= 3; i++ {
		if res := c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	// A read pins a mark at the current mxFrame; the pin is released at
	// statement end but the mark value stays for later readers to share.
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Fatal(got)
	}
	// No active reader between statements: the read lock is released.
	if _, rl, ok := c1.pager.WalReadMarks(); !ok || rl != -1 {
		t.Fatalf("readLock between statements = %d (ok=%v), want -1", rl, ok)
	}
	// The checkpoint triple reports full coverage: (0, nLog, nCkpt) with
	// nLog == nCkpt (no active reader caps the backfill).
	r := c2.Query("PRAGMA wal_checkpoint")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	nLog := ckptTripleInt(t, r, 1)
	nCkpt := ckptTripleInt(t, r, 2)
	if nLog == 0 || nLog != nCkpt {
		t.Fatalf("checkpoint triple = [.. %d %d], want nLog == nCkpt > 0", nLog, nCkpt)
	}
	// At least one mark equals mxFrame (== nLog): the pinned mark value.
	info, _, ok := c2.pager.WalReadMarks()
	if !ok {
		t.Fatal("no wal read marks visible")
	}
	found := false
	for _, m := range info.AReadMark {
		if m != pager.ReadmarkNotUsed && int(m) == nLog {
			found = true
		}
	}
	if !found {
		t.Errorf("no aReadMark equals mxFrame %d: %v", nLog, info.AReadMark)
	}
}

// TestWalMVCCCheckpointRespectsReader pins checkpoint PASS1's reader-mark
// cap (wal.c mxSafeFrame): an active reader's mark stops the backfill short
// of mxFrame (the checkpoint triple reports nCkpt < nLog); once the reader
// ends its transaction, the log backfills fully.
func TestWalMVCCCheckpointRespectsReader(t *testing.T) {
	c1, c2 := openWalPair(t)
	for i := 1; i <= 3; i++ {
		if res := c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	// Park the reader: its mark pins the log at the current mxFrame.
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Fatal(got)
	}
	// A writer commits behind the parked reader.
	if res := c2.Exec("INSERT INTO t1 VALUES(4)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// PASSIVE backfill stops at the reader's mark.
	r := c2.Query("PRAGMA wal_checkpoint")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	busy := formatSQLiteValue(r.Rows[0][0])
	nLog := ckptTripleInt(t, r, 1)
	nCkpt := ckptTripleInt(t, r, 2)
	if busy != "0" {
		t.Errorf("PASSIVE busy = %s, want 0", busy)
	}
	if nCkpt >= nLog {
		t.Errorf("capped backfill: triple [%s %d %d], want nCkpt < nLog", busy, nLog, nCkpt)
	}
	// The parked reader still sees its snapshot...
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("parked reader count = %s, want 3", got)
	}
	// ...and releasing it lets the next checkpoint cover the whole log.
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	r = c2.Query("PRAGMA wal_checkpoint")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := ckptTripleInt(t, r, 2); got != nLog {
		t.Errorf("post-reader nCkpt = %d, want %d (full backfill)", got, nLog)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "4" {
		t.Errorf("final count = %s, want 4", got)
	}
}

// TestWalMVCCFiveReadersCoexist pins the 5-slot read-mark matrix: five
// connections pinned at successive snapshots (one commit apart) all read
// their own frozen view while a dedicated writer commits, and the
// walRestartLog wrap stays excluded while any of them is parked. The fifth
// reader shares the largest qualifying mark (shared locks coexist).
func TestWalMVCCFiveReadersCoexist(t *testing.T) {
	conns := openWalPairN(t, 6)
	writer := conns[5]
	if res := writer.Exec("INSERT INTO t1 VALUES(0)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Pin each reader one commit apart (distinct mxFrame per reader).
	for i := 0; i < 5; i++ {
		if res := conns[i].Exec("BEGIN"); res.Error != nil {
			t.Fatal(res.Error)
		}
		if got := queryInt(t, conns[i], "SELECT count(*) FROM t1"); got != itoa(i+1) {
			t.Fatalf("reader %d pre-count = %s, want %d", i, got, i+1)
		}
		if res := writer.Exec("INSERT INTO t1 VALUES(" + itoa(i+1) + ")"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	// All five readers still see their own frozen counts.
	for i := 0; i < 5; i++ {
		if got := queryInt(t, conns[i], "SELECT count(*) FROM t1"); got != itoa(i+1) {
			t.Errorf("reader %d frozen count = %s, want %d", i, got, i+1)
		}
	}
	// The writer sees everything it committed.
	if got := queryInt(t, writer, "SELECT count(*) FROM t1"); got != "6" {
		t.Errorf("writer count = %s, want 6", got)
	}
	// End the reader transactions; integrity holds.
	for i := 0; i < 5; i++ {
		if res := conns[i].Exec("COMMIT"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	if got := queryInt(t, conns[0], "SELECT count(*) FROM t1"); got != "6" {
		t.Errorf("final count = %s, want 6", got)
	}
	if res := writer.Exec("PRAGMA integrity_check"); res.Error != nil {
		t.Fatal(res.Error)
	}
}

// TestWalMVCCOwnCommitVisible pins read-your-own-writes at the boundary: a
// connection sees its own committed rows at the next statement (the read
// snapshot is re-pinned from the header its writer just advanced).
func TestWalMVCCOwnCommitVisible(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "0" {
		t.Fatal(got)
	}
	if res := c1.Exec("INSERT INTO t1 VALUES(11),(12)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Inside the transaction: own writes visible.
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "2" {
		t.Errorf("in-txn count = %s, want 2", got)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "2" {
		t.Errorf("post-commit count = %s, want 2", got)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "2" {
		t.Errorf("conn2 count = %s, want 2", got)
	}
}

// TestWalMVCCRaceWriterReaders exercises the pin/unpin churn under -race:
// concurrent writers (busy-timeout serialized) and parked readers on two
// connections leave a consistent database.
func TestWalMVCCRaceWriterReaders(t *testing.T) {
	conns := openWalPairN(t, 3)
	for _, c := range conns {
		c.pager.SetBusyTimeout(10_000 * time.Millisecond)
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); walMVCCWriterLoop(t, conns[0], 0, 20) }()
	go func() { defer wg.Done(); walMVCCWriterLoop(t, conns[1], 100, 20) }()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if res := conns[2].Exec("BEGIN; SELECT count(*) FROM t1; COMMIT"); res.Error != nil {
				t.Errorf("reader: %v", res.Error)
				return
			}
		}
	}()
	wg.Wait()
	if got := queryInt(t, conns[2], "SELECT count(*) FROM t1"); got != "40" {
		t.Errorf("count = %s, want 40", got)
	}
	r := conns[0].Query("PRAGMA integrity_check")
	if r.Error != nil || formatSQLiteValue(r.Rows[0][0]) != "ok" {
		t.Errorf("integrity_check: %v %v", r.Error, r.Rows)
	}
}

// walMVCCWriterLoop inserts n rows from one connection, occasionally inside
// an explicit transaction.
func walMVCCWriterLoop(t *testing.T, db *DB, base, n int) {
	t.Helper()
	for j := 0; j < n; j++ {
		stmt := "INSERT INTO t1 VALUES(" + itoa(base+j) + ")"
		if j%5 == 4 {
			stmt = "BEGIN; " + stmt + "; COMMIT"
		}
		if res := db.Exec(stmt); res.Error != nil {
			t.Errorf("writer %d row %d: %v", base, j, res.Error)
			return
		}
	}
}

// TestWalMVCCReaderAfterBackfill pins the reader-vs-checkpoint consistency
// contract behind walFindFrame's minFrame rule: a reader that re-pins after
// a partial backfill (nBackfill < mxFrame, capped earlier by its own mark)
// must see the newest committed page versions — the table leaf rewritten in
// a frame above nBackfill — never the checkpointed stale version of the
// same page (a frozen count of 3 here would be exactly that resurrection).
// After the log fully backfills, the READ_LOCK(0) pin reads the main file
// and still agrees.
func TestWalMVCCReaderAfterBackfill(t *testing.T) {
	c1, c2 := openWalPair(t)
	for i := 1; i <= 3; i++ {
		if res := c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	// Park a reader (mark at F1), commit a new row behind it (F2), and
	// backfill up to the mark only: frames (F1, F2] stay WAL-only.
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Fatal(got)
	}
	if res := c2.Exec("INSERT INTO t1 VALUES(4)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	r := c2.Query("PRAGMA wal_checkpoint")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	nLog := ckptTripleInt(t, r, 1)
	nCkpt := ckptTripleInt(t, r, 2)
	if nCkpt >= nLog {
		t.Fatalf("expected a reader-capped backfill, triple [%s %d %d]",
			formatSQLiteValue(r.Rows[0][0]), nLog, nCkpt)
	}
	// End the parked transaction; the fresh read transaction re-pins at F2
	// with minFrame = nBackfill+1.
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "4" {
		t.Errorf("re-pinned reader count = %s, want 4 (newest page version)", got)
	}
	// Full backfill: the next reader pins READ_LOCK(0) and reads the main
	// file — the same four rows.
	if res := c2.Exec("PRAGMA wal_checkpoint"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT sum(a) FROM t1"); got != "10" {
		t.Errorf("post-backfill sum = %s, want 10", got)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "4" {
		t.Errorf("conn2 count = %s, want 4", got)
	}
}
