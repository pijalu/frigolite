package frigolite

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

// PERF.JOURNAL write-intent exactness pins. The statement journal's
// before-image capture moved from the READ path to the write-intent barrier
// (pager.PrepareWrite, sqlite3PagerWrite parity): the btree layer announces
// each page it is about to edit and the journal captures the pre-mutation
// bytes there. These pins hold the exactness CONTRACT over the two point-DML
// shapes the move touches most — statement-failed UPDATE and DELETE inside a
// live transaction — plus a cross-statement buffer-aliasing loop (the pooled
// before-image buffers must never let one statement's captured bytes leak
// into another's restore).

// setupWriteIntentPinDB opens a file-backed db seeded for the pin shapes:
// a wide base table (multi-leaf, overflow-bearing payloads, an index so
// deletes dirty index pages too) and a trigger-armed side table.
func setupWriteIntentPinDB(t *testing.T) *DB {
	t.Helper()
	db := setupPerfStmtDB(t)
	mustExecPerfStmt(t, db, "PRAGMA journal_mode=DELETE")
	mustExecPerfStmt(t, db, "CREATE TABLE base (id INTEGER PRIMARY KEY, payload TEXT, c INTEGER)")
	mustExecPerfStmt(t, db, "CREATE INDEX base_c ON base(c)")
	mustExecPerfStmt(t, db, "CREATE TABLE side (n INTEGER)")
	for i := 1; i <= 300; i++ {
		mustExecPerfStmt(t, db, fmt.Sprintf(
			"INSERT INTO base VALUES (%d, '%s', %d)", i, pinBlobOf(i, 120), i*13%997))
	}
	return db
}

// TestPerfStmtJournalWriteIntentUpdateRollback: a multi-row UPDATE that
// aborts mid-statement (RAISE(ABORT) trigger on the 25th row) must restore
// every page's exact bytes — the leaf pages it already updated, the index
// pages it re-keyed, and the side-table pages its BEFORE UPDATE trigger
// wrote — and leave the transaction usable with earlier statements' writes
// intact.
func TestPerfStmtJournalWriteIntentUpdateRollback(t *testing.T) {
	db := setupWriteIntentPinDB(t)
	mustExecPerfStmt(t, db, "CREATE TRIGGER side_upd BEFORE UPDATE ON base BEGIN INSERT INTO side VALUES (old.id); END")
	mustExecPerfStmt(t, db, "CREATE TRIGGER abort_upd BEFORE UPDATE ON base WHEN old.id = 25 BEGIN SELECT RAISE(ABORT, 'upd-abort'); END")

	mustExecPerfStmt(t, db, "BEGIN")
	mustExecPerfStmt(t, db, "UPDATE base SET c=c+1 WHERE id=7") // earlier in-txn write
	for _, q := range []string{"SELECT count(*) FROM base", "SELECT count(*) FROM side",
		"SELECT count(*) FROM base WHERE c > 900"} {
		if res := db.Query(q); res.Error != nil {
			t.Fatalf("warm %q: %v", q, res.Error)
		}
	}
	beforeMap := perPageHash(t, db)

	res := db.Exec("UPDATE base SET c = c + 500 WHERE id <= 100")
	if res.Error == nil {
		t.Fatalf("multi-row UPDATE past the abort trigger must fail")
	}

	requireSamePages(t, db, beforeMap)

	// The transaction is still usable and the earlier write is intact.
	mustExecPerfStmt(t, db, "UPDATE base SET c=c+1 WHERE id=8")
	mustExecPerfStmt(t, db, "COMMIT")
	rows := queryRowsPerfStmt(t, db, "SELECT c FROM base WHERE id IN (7, 8, 25) ORDER BY id")
	if len(rows) != 3 {
		t.Fatalf("post-commit rows = %v", rows)
	}
	if rows := queryRowsPerfStmt(t, db, "PRAGMA integrity_check"); len(rows) != 1 || rows[0][0] != "ok" {
		t.Errorf("integrity_check: %v", rows)
	}
}

// TestPerfStmtJournalWriteIntentDeleteRollback: the DELETE shape — a
// multi-row delete that aborts after dirtying several leaf + index pages
// (and the trigger's side table) restores everything byte-exactly.
func TestPerfStmtJournalWriteIntentDeleteRollback(t *testing.T) {
	db := setupWriteIntentPinDB(t)
	mustExecPerfStmt(t, db, "CREATE TRIGGER side_del AFTER DELETE ON base BEGIN INSERT INTO side VALUES (old.id); END")
	mustExecPerfStmt(t, db, "CREATE TRIGGER abort_del BEFORE DELETE ON base WHEN old.id = 25 BEGIN SELECT RAISE(ABORT, 'del-abort'); END")

	mustExecPerfStmt(t, db, "BEGIN")
	mustExecPerfStmt(t, db, "DELETE FROM base WHERE id = 7") // earlier in-txn write
	for _, q := range []string{"SELECT count(*) FROM base", "SELECT count(*) FROM side",
		"SELECT count(*) FROM base WHERE c > 900"} {
		if res := db.Query(q); res.Error != nil {
			t.Fatalf("warm %q: %v", q, res.Error)
		}
	}
	beforeMap := perPageHash(t, db)

	res := db.Exec("DELETE FROM base WHERE id <= 100")
	if res.Error == nil {
		t.Fatalf("multi-row DELETE past the abort trigger must fail")
	}

	requireSamePages(t, db, beforeMap)

	mustExecPerfStmt(t, db, "COMMIT")
	if got := queryIntPerfStmt(t, db, "SELECT count(*) FROM base"); got != 299 {
		t.Fatalf("post-commit base rows = %d, want 299 (only the committed delete applied)", got)
	}
	if rows := queryRowsPerfStmt(t, db, "PRAGMA integrity_check"); len(rows) != 1 || rows[0][0] != "ok" {
		t.Errorf("integrity_check: %v", rows)
	}
}

// TestPerfStmtJournalBufferAliasingAcrossStatements drives the pooled
// before-image buffers through 40 alternating committed/failed statements
// over the SAME pages: every failed statement must roll back to the exact
// generation the preceding committed statement left (never an earlier or a
// later one — a buffer shared across statements would surface as a wrong
// generation). Memory pager: every capture is a pooled memory image.
func TestPerfStmtJournalBufferAliasingAcrossStatements(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	mustExecPerfStmt(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, c INTEGER)")
	for i := 1; i <= 40; i++ {
		mustExecPerfStmt(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i))
	}
	mustExecPerfStmt(t, db, "CREATE TRIGGER abort_upd BEFORE UPDATE ON t WHEN old.id = 3 BEGIN SELECT RAISE(ABORT, 'x'); END")

	mustExecPerfStmt(t, db, "BEGIN")
	for gen := 1; gen <= 40; gen++ {
		// Committed statement: bump every row but the trigger-armed one
		// (gen marker on row 1).
		mustExecPerfStmt(t, db, fmt.Sprintf("UPDATE t SET c = %d WHERE id <> 3", gen*1000+1))
		// Failing statement: must restore EXACTLY the gen*1000+1 state.
		if res := db.Exec("UPDATE t SET c = 999999 WHERE id <= 40"); res.Error == nil {
			t.Fatalf("gen %d: failing UPDATE must fail", gen)
		}
		rows := queryRowsPerfStmt(t, db, "SELECT c FROM t WHERE id = 1")
		if len(rows) != 1 {
			t.Fatalf("gen %d: no row", gen)
		}
		if got := rows[0][0].(int64); got != int64(gen*1000+1) {
			t.Fatalf("gen %d: rollback restored c=%d, want %d (before-image aliasing)", gen, got, gen*1000+1)
		}
	}
	mustExecPerfStmt(t, db, "COMMIT")
}

// perPageHash digests every currently cached page (re-reading each one, so
// the digest covers actual bytes, not residency).
func perPageHash(t *testing.T, db *DB) map[uint32]string {
	t.Helper()
	m := map[uint32]string{}
	for num := range db.pager.Pages() {
		m[num] = pageHashAt(t, db, num)
	}
	return m
}

// pageHashAt re-reads page num (cache hit or fresh load) and hashes its bytes.
func pageHashAt(t *testing.T, db *DB, num uint32) string {
	t.Helper()
	pg, err := db.pager.ReadPage(num)
	if err != nil {
		t.Fatalf("re-read page %d: %v", num, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(pg.Data))
}

// requireSamePages demands every page that existed before the failing
// statement still holds the SAME bytes after its rollback. Pages journalled
// as from-file entries restore by cache EVICTION (the disk image is the
// statement-start state), so the universe is the before-map's page numbers
// and every page is re-read — residency may differ, bytes may not.
func requireSamePages(t *testing.T, db *DB, before map[uint32]string) {
	t.Helper()
	for num, want := range before {
		if got := pageHashAt(t, db, num); got != want {
			t.Fatalf("page %d bytes changed by statement rollback (%s -> %s)", num, want, got)
		}
	}
}
