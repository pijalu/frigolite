package frigolite

// Native anchors for P7.WAL-G7 slice 2 — the shm lock protocol + busy/
// BUSY_RECOVERY/BUSY_SNAPSHOT semantics + checkpoint PASS1/PASS2 under real
// locks (plan/goals/P7.WAL-G7.md, Slice 2). C ground truth: src/wal.c
// (walLockShared/walLockExclusive/walBusyLock, walIndexRecover's lock dance,
// walCheckpoint PASS1/PASS2, walRestartLog/walRestartHdr) and src/os_unix.c
// unixShmSystemLock (POSIX advisory locks at shm bytes 120..127).
//
// Oracle texts (sqlite3 3.51, src/main.c sqlite3ErrStr): SQLITE_BUSY,
// SQLITE_BUSY_RECOVERY and SQLITE_BUSY_SNAPSHOT all report "database is
// locked"; SQLITE_PROTOCOL reports "locking protocol" (walprotocol-1.3/1.4).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pijalu/frigolite/internal/pager"
)

// shmLockEvent is one xShmLock observation: formatted exactly like the C
// testvfs lock_callback lists ("{idx n op exclusive|shared}").
func shmLockEvent(idx, n int, op string, excl bool) string {
	mode := "shared"
	if excl {
		mode = "exclusive"
	}
	return fmt.Sprintf("{%d %d %s %s}", idx, n, op, mode)
}

// openWalPairN opens n connections on one file with the first in WAL mode
// and a seeded single-column table t1(a).
func openWalPairN(t *testing.T, n int) []*DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wallocks.db")
	c1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("PRAGMA journal_mode=WAL"); res.Error != nil {
		t.Fatalf("journal_mode: %v", res.Error)
	}
	if res := c1.Exec("CREATE TABLE t1(a)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	conns := []*DB{c1}
	for i := 1; i < n; i++ {
		c, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			c.Close()
		}
	})
	return conns
}

// queryInt runs a single-value query and returns it as a string.
func queryInt(t *testing.T, db *DB, sql string) string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("%s: no rows", sql)
	}
	return formatSQLiteValue(r.Rows[0][0])
}

// TestShmLockMatrix ports shmlock.test 1.3's 8-slot matrix through the
// WALIndexLock seam (the vfs_shmlock test-command parity): shared locks
// coexist, exclusive excludes both shared and exclusive, unlocks release,
// and multi-slot ranges conflict on any covered byte.
func TestShmLockMatrix(t *testing.T) {
	conns := openWalPairN(t, 3)
	d1, d2, d3 := conns[0], conns[1], conns[2]
	lock := func(d *DB, idx, n int, excl, on bool) bool {
		return d.pager.WALIndexLock(idx, n, excl, on)
	}

	// 1.3.1-7: shared 7 excludes exclusive 7; unlocks free it.
	if !lock(d1, 7, 1, false, true) {
		t.Fatal("1: shared lock 7 failed")
	}
	if lock(d2, 7, 1, true, true) {
		t.Fatal("2: exclusive lock 7 succeeded over shared — want BUSY")
	}
	if !lock(d1, 7, 1, false, false) {
		t.Fatal("3: shared unlock 7 failed")
	}
	if !lock(d2, 7, 1, true, true) {
		t.Fatal("4: exclusive lock 7 failed after unlock")
	}
	if lock(d1, 7, 1, false, true) || lock(d1, 7, 1, true, true) {
		t.Fatal("5/6: shared+exclusive over exclusive — want BUSY")
	}
	if !lock(d2, 7, 1, true, false) {
		t.Fatal("7: exclusive unlock 7 failed")
	}

	// 1.3.8-11: the whole 8-slot range locks exclusively and releases clean.
	if !lock(d1, 0, 8, true, true) {
		t.Fatal("8: exclusive lock 0..7 failed")
	}
	if !lock(d1, 0, 8, true, false) {
		t.Fatal("9: exclusive unlock 0..7 failed")
	}
	if !lock(d2, 0, 8, true, true) || !lock(d2, 0, 8, true, false) {
		t.Fatal("10/11: db2 exclusive range lock/unlock failed")
	}

	// 1.3.12-21: shared 0 coexists across three handles; exclusive is blocked
	// while ANY shared holder remains, and db3 releases its own shared first
	// (a lock cannot move directly between shared and exclusive).
	for _, d := range []*DB{d1, d2, d3} {
		if !lock(d, 0, 1, false, true) {
			t.Fatalf("12-14: shared lock 0 on %v failed", d)
		}
	}
	if !lock(d3, 0, 1, false, false) {
		t.Fatal("15: shared unlock 0 (db3) failed")
	}
	if lock(d3, 0, 1, true, true) {
		t.Fatal("16: exclusive 0 over two shared — want BUSY")
	}
	if !lock(d2, 0, 1, false, false) {
		t.Fatal("17: shared unlock 0 (db2) failed")
	}
	if lock(d3, 0, 1, true, true) {
		t.Fatal("18: exclusive 0 with one shared — want BUSY")
	}
	if !lock(d1, 0, 1, false, false) {
		t.Fatal("19: shared unlock 0 (db) failed")
	}
	if !lock(d3, 0, 1, true, true) {
		t.Fatal("20: exclusive 0 after last shared unlock failed")
	}
	if !lock(d3, 0, 1, true, false) {
		t.Fatal("21: exclusive unlock 0 failed")
	}

	// 1.3.22-29: multi-slot ranges conflict on any covered byte (slot 2/3
	// held shared block {2 2}, {0 5}, {0 4}, {0 3} exclusively).
	if !lock(d1, 3, 1, false, true) {
		t.Fatal("22: shared lock 3 failed")
	}
	if lock(d2, 2, 2, true, true) {
		t.Fatal("23: exclusive {2 2} over shared 3 — want BUSY")
	}
	if !lock(d1, 2, 1, false, true) {
		t.Fatal("24: shared lock 2 failed")
	}
	for _, rng := range [][2]int{{0, 5}, {0, 4}, {0, 3}} {
		if lock(d2, rng[0], rng[1], true, true) {
			t.Fatalf("25-27: exclusive {%d %d} over shared 2,3 — want BUSY", rng[0], rng[1])
		}
	}
	if !lock(d1, 3, 1, false, false) {
		t.Fatal("28: shared unlock 3 failed")
	}
	if lock(d2, 2, 2, true, true) {
		t.Fatal("29: exclusive {2 2} over shared 2 — want BUSY")
	}
	if !lock(d1, 2, 1, false, false) {
		t.Fatal("28b: shared unlock 2 failed")
	}
	if !lock(d2, 2, 2, true, true) || !lock(d2, 2, 2, true, false) {
		t.Fatal("29b: exclusive {2 2} lock/unlock after unlock failed")
	}
}

// TestWalLockWriterSerialization pins the writer mutex: several connections
// committing concurrently produce every row exactly once (each commit's
// frame append runs under the exclusive WRITER shm lock).
var _ = fmt.Stringer(nil)
func TestWalLockWriterSerialization(t *testing.T) {
	conns := openWalPairN(t, 4)
	// No busy timeout means a contended writer fails immediately (SQLITE_BUSY,
	// C parity): give every writer sqlite3_busy_timeout parity so they wait.
	for _, c := range conns {
		c.pager.SetBusyTimeout(5 * time.Second)
	}
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(id int, c *DB) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if res := c.Exec("INSERT INTO t1 VALUES(" + itoa(id*100+j) + ")"); res.Error != nil {
					t.Errorf("writer %d: %v", id, res.Error)
					return
				}
			}
		}(i, c)
	}
	wg.Wait()
	if got := queryInt(t, conns[0], "SELECT count(*) FROM t1"); got != "100" {
		t.Errorf("serialized commits: count = %s, want 100", got)
	}
	if got := queryInt(t, conns[3], "SELECT count(DISTINCT a) FROM t1"); got != "100" {
		t.Errorf("distinct rows = %s, want 100 (frame overlap corruption)", got)
	}
}

// TestWalLockBusyTimeout pins sqlite3_busy_timeout semantics on the WRITER
// lock: a contended writer retries until the timeout expires ("database is
// locked"), and proceeds once the holder releases.
func TestWalLockBusyTimeout(t *testing.T) {
	c1, c2 := openWalPair(t)
	// Hold the WRITER slot from another connection (a long commit in flight).
	if !c2.pager.WALIndexLock(0, 1, true, true) {
		t.Fatal("hold WRITER: lock failed")
	}
	c1.pager.SetBusyTimeout(150 * time.Millisecond)
	start := time.Now()
	if res := c1.Exec("INSERT INTO t1 VALUES(1)"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "database is locked") {
		t.Fatalf("contended insert: want 'database is locked', got %v", res.Error)
	}
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Errorf("busy retry burned only %v — want the timeout honored", elapsed)
	}
	// Release: the next write waits-then-proceeds.
	if !c2.pager.WALIndexLock(0, 1, true, false) {
		t.Fatal("release WRITER failed")
	}
	if res := c1.Exec("INSERT INTO t1 VALUES(2)"); res.Error != nil {
		t.Fatalf("insert after release: %v", res.Error)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "1" {
		t.Errorf("count after release = %s, want 1", got)
	}
}

// TestWalLockRecoverBusy pins the SQLITE_BUSY_RECOVERY contract (wal.c
// walTryBeginRead): while another connection is mid-recovery (RECOVER lock
// held), a reader whose wal-index needs rebuilding reports "database is
// locked", and succeeds once the recovery completes.
func TestWalLockRecoverBusy(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(7)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// The -shm content becomes garbage (walsetlk-1.2 parity) and a recovery
	// is "in flight" (RECOVER exclusively held by another connection).
	c1.pager.CorruptWalIndex()
	if !c2.pager.WALIndexLock(2, 1, true, true) {
		t.Fatal("hold RECOVER failed")
	}
	r := c1.Query("SELECT count(*) FROM t1")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "database is locked") {
		t.Fatalf("read during recovery: want 'database is locked', got %v", r.Error)
	}
	// Recovery completes: the read now proceeds and sees the committed row.
	if !c2.pager.WALIndexLock(2, 1, true, false) {
		t.Fatal("release RECOVER failed")
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "1" {
		t.Errorf("post-recovery count = %s, want 1", got)
	}
	// The second connection's view is rebuilt too.
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "1" {
		t.Errorf("conn2 post-recovery count = %s, want 1", got)
	}
}

// TestWalLockProtocol pins walprotocol-1.3/1.4: an xShmLock that keeps
// reporting BUSY for the recovery lock range {1 2 lock exclusive} (or the
// WRITER {0 1 lock exclusive}) burns the WAL_RETRY budget into SQLITE_
// PROTOCOL — the oracle text is "locking protocol".
func TestWalLockProtocol(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "walproto.db")
	c1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c1.Exec("PRAGMA journal_mode=WAL")
	c1.Exec("CREATE TABLE x(y)")
	c1.Exec("INSERT INTO x VALUES('z')")
	c1.Close()

	for _, tc := range []struct {
		name string
		veto string
	}{{"recover-range-busy", "{1 2 lock exclusive}"}, {"writer-busy", "{0 1 lock exclusive}"}} {
		t.Run(tc.name, func(t *testing.T) {
			pager.SetShmLockHook(func(idx, n int, op string, excl bool) error {
				if op == "lock" && excl && shmLockEvent(idx, n, op, excl) == tc.veto {
					return errTestBusy
				}
				return nil
			})
			t.Cleanup(func() { pager.SetShmLockHook(nil) })

			c2, err := Open(path)
			if err != nil {
				// The protocol error surfaced at open (recovery runs there).
				if !strings.Contains(err.Error(), "locking protocol") {
					t.Fatalf("open: want 'locking protocol', got %v", err)
				}
				return
			}
			defer c2.Close()
			// Lazy attach: the first statement drives recovery.
			r := c2.Query("SELECT * FROM x")
			if r.Error == nil || !strings.Contains(r.Error.Error(), "locking protocol") {
				t.Fatalf("select: want 'locking protocol', got %v", r.Error)
			}
		})
	}
}

// errTestBusy is the hook's veto (testvfs lock_callback returning SQLITE_BUSY).
var errTestBusy = fmt.Errorf("busy")

// TestWalLockHookRecoverySequence pins walprotocol-1.1/1.2's xShmLock
// sequence for recovery-on-open: WRITER {0 1 lock exclusive}, the
// CKPT+RECOVER range {1 2 lock exclusive}, each read mark {4..7 1
// lock/unlock exclusive}, then {1 2 unlock exclusive} and {0 1 unlock
// exclusive} — exactly C's observed order.
func TestWalLockHookRecoverySequence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "walseq.db")
	c1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c1.Exec("PRAGMA journal_mode=WAL")
	c1.Exec("CREATE TABLE x(y)")
	c1.Exec("INSERT INTO x VALUES('z')")
	c1.Close()

	var mu sync.Mutex
	var seq []string
	pager.SetShmLockHook(func(idx, n int, op string, excl bool) error {
		mu.Lock()
		seq = append(seq, shmLockEvent(idx, n, op, excl))
		mu.Unlock()
		return nil
	})
	t.Cleanup(func() { pager.SetShmLockHook(nil) })

	c2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if r := c2.Query("SELECT * FROM x"); r.Error != nil {
		t.Fatal(r.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"{0 1 lock exclusive}", "{1 2 lock exclusive}",
		"{4 1 lock exclusive}", "{4 1 unlock exclusive}",
		"{5 1 lock exclusive}", "{5 1 unlock exclusive}",
		"{6 1 lock exclusive}", "{6 1 unlock exclusive}",
		"{7 1 lock exclusive}", "{7 1 unlock exclusive}",
		"{1 2 unlock exclusive}", "{0 1 unlock exclusive}",
	}
	if len(seq) < len(want) {
		t.Fatalf("xShmLock sequence too short: %v", seq)
	}
	for i, w := range want {
		if seq[i] != w {
			t.Errorf("xShmLock[%d] = %s, want %s\nfull: %v", i, seq[i], w, seq)
			break
		}
	}
}

// TestWalLockBusySnapshot pins walprotocol2-2.2/2.3 (SQLITE_BUSY_SNAPSHOT):
// another connection commits between this connection's read snapshot pin and
// its write — the commit fails with "database is locked"; a retried statement
// succeeds (2.4/2.5 behavior).
func TestWalLockBusySnapshot(t *testing.T) {
	c1, c2 := openWalPair(t)
	c1.Exec("INSERT INTO t1 VALUES(1)")
	// Pin c1's read snapshot.
	if r := c1.Query("SELECT count(*) FROM t1"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Rig the hook: the moment c1 reaches for the WRITER lock, c2 jumps in
	// and commits (exactly walprotocol2's lock_callback sabotage).
	var armed sync.Map
	armed.Store("armed", true)
	pager.SetShmLockHook(func(idx, n int, op string, excl bool) error {
		if idx == 0 && n == 1 && op == "lock" && excl {
			if _, ok := armed.Load("armed"); ok {
				armed.Delete("armed")
				if res := c2.Exec("INSERT INTO t1 VALUES(2)"); res.Error != nil {
					t.Errorf("saboteur insert: %v", res.Error)
				}
			}
		}
		return nil
	})
	t.Cleanup(func() { pager.SetShmLockHook(nil) })

	if res := c1.Exec("INSERT INTO t1 VALUES(3)"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "database is locked") {
		t.Fatalf("stale-snapshot commit: want 'database is locked', got %v", res.Error)
	}
	// Retry from a fresh snapshot (2.4/2.5): succeeds and both rows show up.
	if res := c1.Exec("INSERT INTO t1 VALUES(3)"); res.Error != nil {
		t.Fatalf("retried commit: %v", res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("count = %s, want 3 (z y x parity)", got)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("conn2 count = %s, want 3", got)
	}
}

// TestWalLockCorruptShmRecovery ports walsetlk-1.2..1.6: a corrupt -shm
// forces recovery on the next use; the second connection's BEGIN EXCLUSIVE
// reports "database is locked" behind the open write transaction and sees
// the committed rows afterwards.
func TestWalLockCorruptShmRecovery(t *testing.T) {
	c1, c2 := openWalPair(t)
	for i := 1; i <= 8; i += 2 {
		if res := c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + "),(" + itoa(i+1) + ")"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "8" {
		t.Fatalf("pre-corruption count = %s, want 8", got)
	}
	// "puts $fd blahblahblahblah" into the -shm: the wal-index header is
	// garbage; the next use must recover from the -wal.
	c1.pager.CorruptWalIndex()

	// 1.2: the writer's next transaction recovers and commits.
	if res := c1.Exec("BEGIN; INSERT INTO t1 VALUES(9),(10)"); res.Error != nil {
		t.Fatalf("1.2: %v", res.Error)
	}
	// 1.3: the second connection still sees the last committed state (9,10
	// are uncommitted).
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "8" {
		t.Errorf("1.3: count = %s, want 8", got)
	}
	// 1.4: BEGIN EXCLUSIVE behind the open write transaction is locked.
	if res := c2.Exec("BEGIN EXCLUSIVE"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "database is locked") {
		t.Fatalf("1.4: want 'database is locked', got %v", res.Error)
	}
	// 1.5/1.6: the writer commits; the reader sees everything.
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatalf("1.5: %v", res.Error)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "10" {
		t.Errorf("1.6: count = %s, want 10", got)
	}
}

// TestWalCheckpointTruncateZero pins walsetlk-1.7/1.8 (and C's
// R-44699-57140): PRAGMA wal_checkpoint(TRUNCATE) returns {0 0 0} and
// truncates the -wal to ZERO bytes; the next write re-creates the header and
// the log stays consistent.
func TestWalCheckpointTruncateZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waltrunc.db")
	c1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c1.Exec("PRAGMA journal_mode=WAL")
	c1.Exec("CREATE TABLE t1(a)")
	for i := 0; i < 5; i++ {
		c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")")
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() <= 32 {
		t.Fatalf("pre-checkpoint -wal size = %v (err %v), want > 32", fi, err)
	}
	r := c1.Query("PRAGMA wal_checkpoint(TRUNCATE)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := formatSQLiteValue(r.Rows[0][0]) + " " + formatSQLiteValue(r.Rows[0][1]) + " " + formatSQLiteValue(r.Rows[0][2]); got != "0 0 0" {
		t.Errorf("TRUNCATE triple = [%s], want [0 0 0]", got)
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() != 0 {
		t.Fatalf("1.8: -wal size after TRUNCATE = %v (err %v), want 0", fi, err)
	}
	// The next writer rewrites the header (walFrames' iFrame==0 branch).
	c1.Exec("INSERT INTO t1 VALUES(99)")
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "6" {
		t.Errorf("post-TRUNCATE count = %s, want 6", got)
	}
	// A fresh connection recovers the post-TRUNCATE state identically.
	c2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "6" {
		t.Errorf("reopen count = %s, want 6", got)
	}
}

// TestWalLockCheckpointBusy pins the checkpoint busy triple: a non-PASSIVE
// checkpoint behind a WRITER holder reports {1 0 0}; a PASSIVE checkpoint
// behind a CKPT holder reports {1 0 0} (PASSIVE never blocks — wal.c
// EVIDENCE-OF R-62920-47450).
func TestWalLockCheckpointBusy(t *testing.T) {
	c1, c2 := openWalPair(t)
	c1.Exec("INSERT INTO t1 VALUES(1)")
	c1.Exec("INSERT INTO t1 VALUES(2)")

	if !c2.pager.WALIndexLock(0, 1, true, true) {
		t.Fatal("hold WRITER failed")
	}
	r := c1.Query("PRAGMA wal_checkpoint(FULL)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := formatSQLiteValue(r.Rows[0][0]); got != "1" {
		t.Errorf("FULL behind WRITER: busy = %s, want 1", got)
	}
	c2.pager.WALIndexLock(0, 1, true, false)

	// PASSIVE with the CKPT lock held elsewhere: busy, no error.
	if !c2.pager.WALIndexLock(1, 1, true, true) {
		t.Fatal("hold CKPT failed")
	}
	r = c1.Query("PRAGMA wal_checkpoint")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := formatSQLiteValue(r.Rows[0][0]); got != "1" {
		t.Errorf("PASSIVE behind CKPT: busy = %s, want 1", got)
	}
	c2.pager.WALIndexLock(1, 1, true, false)

	// Uncontended: backfills and reports (busy, nLog, nCkpt) with full coverage.
	r = c1.Query("PRAGMA wal_checkpoint")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := formatSQLiteValue(r.Rows[0][0]); got != "0" {
		t.Errorf("uncontended busy = %s, want 0", got)
	}
}

// TestWalRestartConcurrent pins the walrestart-1.5 contract (checkpoint vs
// writer races leave the database consistent): concurrent checkpoints and
// commits all land, and integrity_check stays clean.
func TestWalRestartConcurrent(t *testing.T) {
	c1, c2 := openWalPair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if res := c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
				t.Errorf("writer: %v", res.Error)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			r := c2.Query("PRAGMA wal_checkpoint")
			if r.Error != nil {
				t.Errorf("checkpoint: %v", r.Error)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "50" {
		t.Errorf("count = %s, want 50", got)
	}
	r := c1.Query("PRAGMA integrity_check")
	if r.Error != nil || formatSQLiteValue(r.Rows[0][0]) != "ok" {
		t.Errorf("integrity_check: %v %v", r.Error, r.Rows)
	}
}

// TestWalLockExclusiveMode pins locking_mode=EXCLUSIVE's shm-lock bypass
// (wal.c: walLockShared/walLockExclusive become no-ops — no xShmLock calls).
func TestWalLockExclusiveMode(t *testing.T) {
	c1, _ := openWalPair(t)
	c1.Exec("PRAGMA locking_mode=EXCLUSIVE")
	var calls int
	pager.SetShmLockHook(func(idx, n int, op string, excl bool) error {
		calls++
		return nil
	})
	t.Cleanup(func() { pager.SetShmLockHook(nil) })
	if res := c1.Exec("INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if calls != 0 {
		t.Errorf("exclusive mode issued %d xShmLock calls, want 0", calls)
	}
	c1.Exec("PRAGMA locking_mode=NORMAL")
	if res := c1.Exec("INSERT INTO t1 VALUES(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if calls == 0 {
		t.Error("normal mode issued no xShmLock calls after revert")
	}
}
