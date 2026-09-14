package frigolite

// Native anchors for P7.WAL-G7 slice 4 — the sqlite3_snapshot C-API surface
// (plan/goals/P7.WAL-G7.md, Slice 4). C ground truth: src/wal.c
// SQLITE_ENABLE_SNAPSHOT block (sqlite3WalSnapshotGet L4501,
// sqlite3WalSnapshotOpen L4523, sqlite3_snapshot_cmp L4552,
// sqlite3WalSnapshotCheck L4573, sqlite3WalSnapshotRecover L3322) +
// walBeginReadTransaction's pSnapshot branch (L3357-3448) + main.c
// sqlite3_snapshot_get/open/recover (L4976/L5016/L5072).
//
// The TCL suites (snapshot*.test) drive these APIs through testfixture-only
// commands (sqlite3_snapshot_get_blob etc.); per the 2026-05 Pure-Go
// supersession policy this file pins the engine-visible contracts natively:
//   - snapshot.test 1.x: snapshot_get error cases (non-WAL db, write txn,
//     autocommit on) — all bare SQLITE_ERROR ("SQL logic error");
//   - snapshot.test 2.x: get/open round-trip with repeatable reads at the
//     snapshot, including the BUSY_SNAPSHOT write inside a snapshot txn;
//   - snapshot.test 3.x/4.x: snapshot_open error cases and
//     SQLITE_ERROR_SNAPSHOT ("snapshot is out of date") after a checkpoint
//     past the snapshot / after a WAL restart (salt change);
//   - snapshot.test 6.x: snapshot_get right after "BEGIN; PRAGMA
//     user_version" and snapshot_open on a fresh connection;
//   - snapshot2 2.x/4.x: blob forms + snapshot_recover walking
//     nBackfillAttempted back (and its error cases);
//   - snapshot_up 1.x: snapshot_open with a read transaction already open
//     re-anchors it.
//
// Deviation from the TCL sources (documented): error TEXTS. SQLite defines
// no message text for these C-API failures (rc only); "SQL logic error" is
// sqlite3ErrStr's text for a bare SQLITE_ERROR and "snapshot is out of
// date" is frigolite's carrier for SQLITE_ERROR_SNAPSHOT (code name from
// main.c L1531). Codes are asserted via ErrorCodeFor.

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

// wantSnapshotErr asserts an error's text and SQLITE code.
func wantSnapshotErr(t *testing.T, db *DB, err error, code, text string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %q (%s), got nil", text, code)
	}
	if !strings.Contains(err.Error(), text) {
		t.Fatalf("error text = %q, want contains %q", err.Error(), text)
	}
	if got := db.ErrorCodeFor(err); got != code {
		t.Fatalf("error code = %s, want %s", got, code)
	}
}

// TestWalSnapshotGetErrors ports snapshot.test 1.1-1.3: snapshot_get fails
// with SQLITE_ERROR on a non-WAL database, inside a write transaction, in
// autocommit mode, and for the temp schema; it succeeds inside a read
// transaction.
func TestWalSnapshotGetErrors(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nonwal.db"
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1,2),(3,4)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 1.1.1: rollback-journal db — even with a read transaction open.
	if res := db.Exec("BEGIN; SELECT * FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	_, err = db.SnapshotGet("main")
	wantSnapshotErr(t, db, err, "SQLITE_ERROR", "SQL logic error")
	if res := db.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 1.3.1: autocommit mode.
	_, err = db.SnapshotGet("main")
	wantSnapshotErr(t, db, err, "SQLITE_ERROR", "SQL logic error")

	// 1.2.1: WAL mode with an open WRITE transaction.
	if res := db.Exec("PRAGMA journal_mode = WAL"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("BEGIN; INSERT INTO t1 VALUES(5,6),(7,8)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	_, err = db.SnapshotGet("main")
	wantSnapshotErr(t, db, err, "SQLITE_ERROR", "SQL logic error")
	if res := db.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// temp schema: main.c's iDb==1 gate.
	if res := db.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if _, err = db.SnapshotGet("temp"); err == nil ||
		db.ErrorCodeFor(err) != "SQLITE_ERROR" {
		t.Fatalf("snapshot_get(temp) = %v, want SQLITE_ERROR", err)
	}

	// 1.3.2: inside a read transaction it works, and get is repeatable.
	if got := queryInt(t, db, "SELECT count(*) FROM t1"); got != "4" {
		t.Fatal(got)
	}
	snap, err := db.SnapshotGet("main")
	if err != nil {
		t.Fatalf("snapshot_get: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot_get returned nil")
	}

	// snapshot.test 6.1: get immediately after "BEGIN; PRAGMA user_version"
	// (a read transaction opened by a pragma, not a SELECT).
	if res := db.Exec("COMMIT; BEGIN; PRAGMA user_version"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if _, err := db.SnapshotGet("main"); err != nil {
		t.Fatalf("snapshot_get after BEGIN+PRAGMA: %v", err)
	}
}

// TestWalSnapshotRepeatableRead ports snapshot.test 2.1/2.2: a snapshot
// taken inside a read transaction keeps reading the old data after the
// connection (or a second connection) re-opens it, while fresh transactions
// see everything.
func TestWalSnapshotRepeatableRead(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(1),(2),(3),(4)"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// 2.1.0/2.1.1: take the snapshot, then advance the database.
	if res := c1.Exec("BEGIN; SELECT * FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	snap, err := c1.SnapshotGet("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("COMMIT; INSERT INTO t1 VALUES(9),(10)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "6" {
		t.Fatalf("fresh count = %s, want 6", got)
	}

	// 2.1.2: re-open the snapshot in a new transaction: frozen view.
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotOpen("main", snap); err != nil {
		t.Fatalf("snapshot_open: %v", err)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "4" {
		t.Errorf("snapshot count = %s, want 4", got)
	}
	if got := queryInt(t, c1, "SELECT sum(a) FROM t1"); got != "10" {
		t.Errorf("snapshot sum = %s, want 10", got)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// 2.2.x: a SECOND connection takes a snapshot of its own pinned view
	// and the FIRST connection opens it (blob form) after another commit.
	if res := c2.Exec("BEGIN; SELECT count(*) FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	blob, err := c2.SnapshotGetBlob("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("INSERT INTO t1 VALUES(11),(12)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotOpenBlob("main", blob); err != nil {
		t.Fatalf("snapshot_open_blob: %v", err)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "6" {
		t.Errorf("conn1 snapshot count = %s, want 6", got)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "6" {
		t.Errorf("conn2 pinned count = %s, want 6", got)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c2.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// After the snapshot transactions end, both see the head again.
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "8" {
		t.Errorf("conn1 post-snapshot count = %s, want 8", got)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "8" {
		t.Errorf("conn2 post-snapshot count = %s, want 8", got)
	}
}

// TestWalSnapshotWriteInSnapshotBusy pins snapshot.test 2.3.3: a write
// inside a transaction anchored at an old snapshot fails with
// SQLITE_BUSY_SNAPSHOT ("database is locked") — the frozen header no longer
// matches the shared head.
func TestWalSnapshotWriteInSnapshotBusy(t *testing.T) {
	c1, _ := openWalPair(t)
	for i := 1; i <= 6; i++ {
		if res := c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	snap := walSnapshotAt(t, c1, 6)
	if res := c1.Exec("INSERT INTO t1 VALUES(7),(8)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 2.3.2: re-anchor at the old snapshot.
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotOpen("main", snap); err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "6" {
		t.Fatalf("re-anchored count = %s, want 6", got)
	}
	// 2.3.3: the write fails; the transaction stays usable.
	if res := c1.Exec("INSERT INTO t1 VALUES(99)"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "database is locked") {
		t.Fatalf("write at old snapshot = %v, want 'database is locked'", res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "6" {
		t.Errorf("post-busy count = %s, want 6", got)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "8" {
		t.Errorf("final count = %s, want 8", got)
	}
}

// TestWalSnapshotStaleAfterCheckpoint ports snapshot.test 4.1: a snapshot
// from the middle of a WAL cannot be opened once a checkpoint has backfilled
// past it (SQLITE_ERROR_SNAPSHOT, "snapshot is out of date").
func TestWalSnapshotStaleAfterCheckpoint(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(1),(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 4.1.1: snapshot at the current head.
	if res := c1.Exec("BEGIN; SELECT * FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	snap, err := c1.SnapshotGet("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 4.1.2: a later commit (still uncheckpointed) — the snapshot opens.
	if res := c1.Exec("INSERT INTO t1 VALUES(3)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotOpen("main", snap); err != nil {
		t.Fatalf("snapshot before checkpoint: %v", err)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "2" {
		t.Errorf("pre-checkpoint snapshot count = %s, want 2", got)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 4.1.3: a PASSIVE checkpoint backfills the whole log
	// (nBackfillAttempted moves past the snapshot).
	if res := c2.Exec("PRAGMA wal_checkpoint"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	err = c1.SnapshotOpen("main", snap)
	wantSnapshotErr(t, c1, err, "SQLITE_ERROR_SNAPSHOT", "snapshot is out of date")
	// The failed open must not leave a pinned snapshot: reads see the head.
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("post-stale count = %s, want 3", got)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
}

// TestWalSnapshotStaleAfterRestart ports snapshot.test 4.2: a snapshot at
// the head stays openable across a checkpoint (its frames are all in the
// main file), but a later commit WRAPS the log (salt1 changes — no readers
// hold marks) and the snapshot becomes unavailable with
// SQLITE_ERROR_SNAPSHOT.
func TestWalSnapshotStaleAfterRestart(t *testing.T) {
	c1, _ := openWalPair(t)
	for i := 1; i <= 4; i++ {
		if res := c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	// 4.2.2: snapshot at the head, checkpoint, re-open — works (the whole
	// snapshot is checkpointed content).
	if res := c1.Exec("BEGIN; SELECT count(*) FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	snap, err := c1.SnapshotGet("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("COMMIT; PRAGMA wal_checkpoint"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotOpen("main", snap); err != nil {
		t.Fatalf("snapshot at head after checkpoint: %v", err)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "4" {
		t.Errorf("checkpointed snapshot count = %s, want 4", got)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 4.2.3: the next commit restarts the log (nBackfill==mxFrame, no
	// readers) — the snapshot's salt is gone.
	if res := c1.Exec("INSERT INTO t1 VALUES(5)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	err = c1.SnapshotOpen("main", snap)
	wantSnapshotErr(t, c1, err, "SQLITE_ERROR_SNAPSHOT", "snapshot is out of date")
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
}

// TestWalSnapshotOpenErrors ports snapshot.test 3.1/3.3: snapshot_open in
// autocommit mode and on a non-WAL database are SQLITE_ERROR.
func TestWalSnapshotOpenErrors(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir + "/openerr.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t2(x, y); INSERT INTO t2 VALUES('a','b')"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("PRAGMA journal_mode = WAL; INSERT INTO t2 VALUES('c','d')"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("BEGIN; SELECT * FROM t2"); res.Error != nil {
		t.Fatal(res.Error)
	}
	snap, err := db.SnapshotGet("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("COMMIT; INSERT INTO t2 VALUES('e','f')"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 3.1: autocommit mode.
	err = db.SnapshotOpen("main", snap)
	wantSnapshotErr(t, db, err, "SQLITE_ERROR", "SQL logic error")
	// Unknown schema.
	if res := db.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := db.SnapshotOpen("nosuch", snap); err == nil ||
		db.ErrorCodeFor(err) != "SQLITE_ERROR" {
		t.Fatalf("snapshot_open(nosuch) = %v, want SQLITE_ERROR", err)
	}
	if res := db.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 3.3.1: the snapshot outlives a mode switch to DELETE — the database
	// no longer has a WAL object.
	if res := db.Exec("PRAGMA journal_mode = DELETE"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	err = db.SnapshotOpen("main", snap)
	wantSnapshotErr(t, db, err, "SQLITE_ERROR", "SQL logic error")
	if res := db.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
}

// TestWalSnapshotFreshConnectionOpen ports snapshot.test 6.3/6.4: a snapshot
// blob taken by one connection opens on a brand-new connection (after the
// mandatory first read that attaches the WAL).
func TestWalSnapshotFreshConnectionOpen(t *testing.T) {
	c1, _ := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(1),(2),(3)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN; SELECT count(*) FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	blob, err := c1.SnapshotGetBlob("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Keep c1's registry entry alive across the second connection's open:
	// a third connection sees the same WAL and opens the snapshot.
	c2, err := Open(c1.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	// 6.4's shape: a non-btree statement, then BEGIN, then open.
	if res := c2.Exec("PRAGMA application_id"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c2.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c2.SnapshotOpenBlob("main", blob); err != nil {
		t.Fatalf("fresh-connection snapshot_open_blob: %v", err)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("fresh-connection snapshot count = %s, want 3", got)
	}
	if res := c2.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
}

// TestWalSnapshotCmp pins sqlite3_snapshot_cmp's ordering (wal.c L4552):
// aSalt[0] dominates, then mxFrame; plus the blob round-trip and "bad
// SNAPSHOT" length checks.
func TestWalSnapshotCmp(t *testing.T) {
	old := pager.Snapshot{ASalt: [2]uint32{7, 99}, MxFrame: 10}
	newer := pager.Snapshot{ASalt: [2]uint32{7, 99}, MxFrame: 12}
	wrapped := pager.Snapshot{ASalt: [2]uint32{8, 5}, MxFrame: 1}
	if got := pager.SnapshotCmp(&old, &newer); got >= 0 {
		t.Errorf("cmp(old, newer) = %d, want < 0", got)
	}
	if got := pager.SnapshotCmp(&newer, &old); got <= 0 {
		t.Errorf("cmp(newer, old) = %d, want > 0", got)
	}
	if got := pager.SnapshotCmp(&old, &old); got != 0 {
		t.Errorf("cmp(old, old) = %d, want 0", got)
	}
	// Salt dominates mxFrame (a WAL restart makes every snapshot older).
	if got := pager.SnapshotCmp(&newer, &wrapped); got >= 0 {
		t.Errorf("cmp(newer, wrapped) = %d, want > 0", got)
	}

	// Blob forms: 48 bytes, little-endian image, round trip.
	c1, _ := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN; SELECT count(*) FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	blob, err := c1.SnapshotGetBlob("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(blob) != 48 {
		t.Fatalf("blob len = %d, want 48 (sizeof(sqlite3_snapshot))", len(blob))
	}
	same, err := SnapshotCmpBlob(blob, blob)
	if err != nil || same != 0 {
		t.Fatalf("cmp_blob(blob, blob) = %d, %v; want 0, nil", same, err)
	}
	// A snapshot of a LATER state compares newer, blob-vs-blob.
	if res := c1.Exec("INSERT INTO t1 VALUES(2),(3)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c1.Exec("BEGIN; SELECT count(*) FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	blob2, err := c1.SnapshotGetBlob("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if same, _ := SnapshotCmpBlob(blob, blob2); same >= 0 {
		t.Errorf("cmp_blob(old, new) = %d, want < 0", same)
	}
	if same, _ := SnapshotCmpBlob(blob2, blob); same <= 0 {
		t.Errorf("cmp_blob(new, old) = %d, want > 0", same)
	}
	// Length violations: test1.c "bad SNAPSHOT".
	if _, err := SnapshotCmpBlob(blob[:47], blob); err == nil ||
		!strings.Contains(err.Error(), "bad SNAPSHOT") {
		t.Fatalf("short blob cmp = %v, want 'bad SNAPSHOT'", err)
	}
	// Open with a bad blob fails.
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotOpenBlob("main", blob[:40]); err == nil ||
		!strings.Contains(err.Error(), "bad SNAPSHOT") {
		t.Fatalf("open short blob = %v, want 'bad SNAPSHOT'", err)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
}

// TestWalSnapshotOpenReanchorsReadTxn ports snapshot_up.test 1.3-1.5:
// snapshot_open with a read transaction already open re-anchors it to
// successively older snapshots without ending the transaction.
func TestWalSnapshotOpenReanchorsReadTxn(t *testing.T) {
	c1, _ := openWalPair(t)
	for i := 1; i <= 3; i++ {
		if res := c1.Exec("INSERT INTO t1 VALUES(" + itoa(i) + ")"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	snap1 := walSnapshotAt(t, c1, 3)
	if res := c1.Exec("INSERT INTO t1 VALUES(16)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	snap2 := walSnapshotAt(t, c1, 4)
	if res := c1.Exec("INSERT INTO t1 VALUES(17)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	snap3 := walSnapshotAt(t, c1, 5)

	// 1.2-1.5: inside ONE open transaction, hop snapshots oldest→newest.
	if res := c1.Exec("BEGIN; SELECT count(*) FROM t1"); res.Error != nil {
		t.Fatal(res.Error)
	}
	for _, tc := range []struct {
		snap *Snapshot
		want string
	}{{snap1, "3"}, {snap2, "4"}, {snap3, "5"}} {
		if err := c1.SnapshotOpen("main", tc.snap); err != nil {
			t.Fatalf("snapshot_open in open txn: %v", err)
		}
		if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != tc.want {
			t.Errorf("re-anchored count = %s, want %s", got, tc.want)
		}
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
}

// walSnapshotAt takes a snapshot of db's head after confirming n rows are
// visible (the "BEGIN; SELECT; snapshot_get; COMMIT" TCL idiom).
func walSnapshotAt(t *testing.T, db *DB, n int) *Snapshot {
	t.Helper()
	if res := db.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, db, "SELECT count(*) FROM t1"); got != itoa(n) {
		t.Fatalf("count = %s, want %d", got, n)
	}
	snap, err := db.SnapshotGet("main")
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	return snap
}

// TestWalSnapshotRecover ports snapshot2.test 2.x/3.x/4.x: recover walks
// nBackfillAttempted back so a snapshot stranded by a wal-index rebuild
// becomes openable again; it errors with an open read transaction, an
// unknown schema, and on a non-WAL database, and does not disturb live data
// (snapshot2 3.2).
func TestWalSnapshotRecover(t *testing.T) {
	c1, c2 := openWalPair(t) // c2 keeps the shared wal-index alive across c1's close
	if res := c1.Exec("INSERT INTO t1 VALUES(1),(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	snap := walSnapshotAt(t, c1, 2)

	// A commit past the snapshot, then a wal-index REBUILD (walsetlk 1.2's
	// corrupt-shm seam): recovery resets nBackfillAttempted to the new
	// mxFrame (wal.c L1574), which strands the snapshot (main.c's reason
	// for sqlite3_snapshot_recover).
	if res := c1.Exec("INSERT INTO t1 VALUES(3)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	c1.pager.CorruptWalIndex()
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "3" {
		t.Fatalf("post-recovery count = %s, want 3", got)
	}
	// The snapshot is now stale (mxFrame 2 < nBackfillAttempted 3).
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	err := c1.SnapshotOpen("main", snap)
	wantSnapshotErr(t, c1, err, "SQLITE_ERROR_SNAPSHOT", "snapshot is out of date")
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// snapshot2 2.3: recover, then the snapshot opens again and reads its
	// frozen view. The log was never checkpointed, so every frame the
	// rebuild claimed is provably absent from the main file and
	// nBackfillAttempted walks back to the snapshot.
	if err := c1.SnapshotRecover("main"); err != nil {
		t.Fatalf("snapshot_recover: %v", err)
	}
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotOpen("main", snap); err != nil {
		t.Fatalf("snapshot_open after recover: %v", err)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "2" {
		t.Errorf("recovered snapshot count = %s, want 2", got)
	}
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("post-recover head count = %s, want 3", got)
	}
	if res := c1.Exec("PRAGMA integrity_check"); res.Error != nil {
		t.Fatal(res.Error)
	}
}

// TestWalSnapshotRecoverErrors ports snapshot2.test 4.x: recover fails with
// SQLITE_ERROR when a read transaction is open, for an unknown schema, and
// on a non-WAL database; and succeeds harmlessly in autocommit (4.1/4.3).
func TestWalSnapshotRecoverErrors(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir + "/recov.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t1(x)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Non-WAL database (pager.c L7766: pPager->pWal == 0).
	if err := db.SnapshotRecover("main"); err == nil ||
		db.ErrorCodeFor(err) != "SQLITE_ERROR" {
		t.Fatalf("recover on non-WAL = %v, want SQLITE_ERROR", err)
	}
	if res := db.Exec("PRAGMA journal_mode = WAL; INSERT INTO t1 VALUES(1)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 4.1: autocommit — fine.
	if err := db.SnapshotRecover("main"); err != nil {
		t.Fatalf("recover in autocommit: %v", err)
	}
	// 4.2: an open read transaction.
	if res := db.Exec("BEGIN; SELECT * FROM sqlite_master"); res.Error != nil {
		t.Fatal(res.Error)
	}
	err = db.SnapshotRecover("main")
	wantSnapshotErr(t, db, err, "SQLITE_ERROR", "SQL logic error")
	if res := db.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// 4.3: fine again after the transaction ends.
	if err := db.SnapshotRecover("main"); err != nil {
		t.Fatalf("recover after COMMIT: %v", err)
	}
	// 4.4: unknown schema.
	if err := db.SnapshotRecover("aux"); err == nil ||
		db.ErrorCodeFor(err) != "SQLITE_ERROR" {
		t.Fatalf("recover(aux) = %v, want SQLITE_ERROR", err)
	}
	// temp schema: main.c's iDb==1 gate.
	if err := db.SnapshotRecover("temp"); err == nil ||
		db.ErrorCodeFor(err) != "SQLITE_ERROR" {
		t.Fatalf("recover(temp) = %v, want SQLITE_ERROR", err)
	}
}

// TestWalSnapshotFullCheckpointKeepsStale pins snapshot2.test 2.5's
// negative arm: after a REAL checkpoint puts the snapshot's pages into the
// main file, recover cannot help — the snapshot stays unavailable (its page
// versions no longer exist anywhere).
func TestWalSnapshotFullCheckpointKeepsStale(t *testing.T) {
	c1, _ := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(1),(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	snap := walSnapshotAt(t, c1, 2)
	if res := c1.Exec("INSERT INTO t1 VALUES(3)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Full backfill: nBackfill == nBackfillAttempted == mxFrame — the
	// recover scan range is empty.
	if res := c1.Exec("PRAGMA wal_checkpoint"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotRecover("main"); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if res := c1.Exec("BEGIN"); res.Error != nil {
		t.Fatal(res.Error)
	}
	err := c1.SnapshotOpen("main", snap)
	wantSnapshotErr(t, c1, err, "SQLITE_ERROR_SNAPSHOT", "snapshot is out of date")
	if res := c1.Exec("COMMIT"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("head count = %s, want 3", got)
	}
}

// TestWalSnapshotRecoverAfterGrow pins snapshot2.test 3.x: a recover run
// against a database whose WAL carries another connection's committed frames
// leaves the live data readable (no pager-cache confusion).
func TestWalSnapshotRecoverAfterGrow(t *testing.T) {
	c1, c2 := openWalPair(t)
	if res := c1.Exec("INSERT INTO t1 VALUES(1),(2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := c2.Exec("INSERT INTO t1 VALUES(3)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := c1.SnapshotRecover("main"); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := queryInt(t, c1, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("post-recover count = %s, want 3", got)
	}
	if got := queryInt(t, c2, "SELECT count(*) FROM t1"); got != "3" {
		t.Errorf("writer post-recover count = %s, want 3", got)
	}
	if res := c1.Exec("PRAGMA integrity_check"); res.Error != nil {
		t.Fatal(res.Error)
	}
}
