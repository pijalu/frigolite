package frigolite

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"testing"
)

// P5 statement-journal exactness pin: statement-level rollback after mixed
// reads/writes must restore the page images EXACTLY — byte-for-byte, not just
// logically. The pager's before-image journal (pager.c sub-journal parity)
// saves every page a statement dirties and restores it on rollback; this pin
// hashes every cached page (plus the file header) around a failing statement
// and demands identical digests.

// hashPagerPages returns one digest over the pager's whole in-memory page set:
// the file header, then every cached page's bytes keyed by page number (sorted,
// so map order cannot jitter the hash).
func hashPagerPages(t *testing.T, db *DB) string {
	t.Helper()
	h := sha256.New()
	h.Write(db.pager.Header())
	nums := make([]int, 0, len(db.pager.Pages()))
	for num := range db.pager.Pages() {
		nums = append(nums, int(num))
	}
	sort.Ints(nums)
	for _, num := range nums {
		fmt.Fprintf(h, "p%d:", num)
		h.Write(db.pager.Pages()[uint32(num)].Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// warmPageCache touches every table so all pages are resident before hashing.
func warmPageCache(t *testing.T, db *DB) {
	t.Helper()
	for _, q := range []string{
		"SELECT count(*) FROM base",
		"SELECT count(*) FROM uniq",
		"SELECT count(*) FROM side",
	} {
		if res := db.Query(q); res.Error != nil {
			t.Fatalf("warm %q: %v", q, res.Error)
		}
	}
}

// TestPerfStmtJournalPageBytesExactAfterRollback: inside a transaction with
// prior reads and writes, a statement that fails mid-write (UNIQUE conflict
// via INSERT OR FAIL on a table with an overflow-chunked TEXT payload, an
// index, and a trigger-armed side table) must leave every page's bytes
// exactly as they were before the statement — and the transaction must stay
// usable.
func TestPerfStmtJournalPageBytesExactAfterRollback(t *testing.T) {
	db := setupPerfStmtDB(t)
	mustExecPerfStmt(t, db, "PRAGMA journal_mode=DELETE")
	mustExecPerfStmt(t, db, "CREATE TABLE base (id INTEGER PRIMARY KEY, payload TEXT)")
	mustExecPerfStmt(t, db, "CREATE TABLE uniq (k INTEGER PRIMARY KEY, v TEXT UNIQUE)")
	mustExecPerfStmt(t, db, "CREATE TABLE side (n INTEGER)")
	mustExecPerfStmt(t, db, "CREATE TRIGGER side_tr AFTER INSERT ON uniq BEGIN INSERT INTO side VALUES (new.k); END")
	for i := 1; i <= 400; i++ {
		mustExecPerfStmt(t, db, fmt.Sprintf(
			"INSERT INTO base VALUES (%d, '%s')", i, pinBlobOf(i, 120)))
		if i <= 50 {
			mustExecPerfStmt(t, db, fmt.Sprintf(
				"INSERT INTO uniq VALUES (%d, 'u%06d')", i, i))
		}
	}

	mustExecPerfStmt(t, db, "BEGIN")
	// Mixed reads and writes ahead of the failing statement.
	mustExecPerfStmt(t, db, "INSERT INTO base VALUES (401, 'txn-keep')")
	mustExecPerfStmt(t, db, "UPDATE base SET payload='bumped' WHERE id=7")
	if got := queryIntPerfStmt(t, db, "SELECT count(*) FROM base"); got != 401 {
		t.Fatalf("pre-failure count = %d, want 401", got)
	}
	warmPageCache(t, db)
	before := hashPagerPages(t, db)

	// OR FAIL hits the UNIQUE conflict mid-statement; the trigger has already
	// written the side table for earlier rows of a multi-row form — here the
	// single-row insert still dirties uniq/side pages before failing.
	if res := db.Exec("INSERT OR FAIL INTO uniq VALUES (99, 'u000007')"); res.Error == nil {
		t.Fatalf("INSERT OR FAIL on duplicate v must fail")
	}

	after := hashPagerPages(t, db)
	if after != before {
		t.Fatalf("statement rollback changed page bytes:\nbefore %s\nafter  %s", before, after)
	}

	// Transaction still usable; earlier writes intact; exactness holds at
	// commit too.
	mustExecPerfStmt(t, db, "INSERT INTO base VALUES (402, 'txn-keep-2')")
	mustExecPerfStmt(t, db, "COMMIT")
	rows := queryRowsPerfStmt(t, db, "SELECT payload FROM base WHERE id IN (7, 401, 402) ORDER BY id")
	if len(rows) != 3 || rows[0][0] != "bumped" || rows[1][0] != "txn-keep" || rows[2][0] != "txn-keep-2" {
		t.Fatalf("post-commit rows = %v", rows)
	}
	if got := queryIntPerfStmt(t, db, "SELECT count(*) FROM uniq"); got != 50 {
		t.Fatalf("uniq count = %d, want 50", got)
	}
	// The failed statement's trigger write (AFTER INSERT fired before the
	// conflict aborted it) must not persist: side keeps only the 50 seed rows.
	if got := queryIntPerfStmt(t, db, "SELECT count(*) FROM side"); got != 50 {
		t.Fatalf("side rows after failed statement = %d, want 50", got)
	}
	if rows := queryRowsPerfStmt(t, db, "PRAGMA integrity_check"); len(rows) != 1 || rows[0][0] != "ok" {
		t.Errorf("integrity_check: %v", rows)
	}

	// Reopen: the file on disk must match what the pager held (no rollback
	// residue flushed).
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := Open(db.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := queryIntT2(t, db2, "SELECT count(*) FROM base"); got != 402 {
		t.Fatalf("reopened count = %d, want 402", got)
	}
	if rows := queryRowsPerfStmt(t, db2, "PRAGMA integrity_check"); len(rows) != 1 || rows[0][0] != "ok" {
		t.Errorf("reopened integrity_check: %v", rows)
	}
}

func queryIntPerfStmt(t *testing.T, db *DB, sql string) int64 {
	t.Helper()
	rows := queryRowsPerfStmt(t, db, sql)
	if len(rows) == 0 || len(rows[0]) == 0 {
		t.Fatalf("no rows from %q", sql)
	}
	n, ok := rows[0][0].(int64)
	if !ok {
		t.Fatalf("non-integer count from %q: %T %v", sql, rows[0][0], rows[0][0])
	}
	return n
}

func queryIntT2(t *testing.T, db *DB, sql string) int64 {
	t.Helper()
	return queryIntPerfStmt(t, db, sql)
}

// pinBlobOf builds a deterministic pseudo-text payload (local copy; the
// dml2perf helper of the same shape lives in package frigolite_test).
func pinBlobOf(seed, n int) string {
	b := make([]byte, n)
	x := seed
	for i := range b {
		x = x*1103515245 + 12345
		b[i] = byte('a' + x%26)
	}
	return string(b)
}
