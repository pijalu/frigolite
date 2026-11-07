package frigolite

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckpointInterruptPin pins the checkpoint copyback's per-page
// interrupt check (wal.c:2289-2292): sqlite3_interrupt mid-copyback aborts
// the backfill with SQLITE_INTERRUPT, the log's frames stay standing (the
// nBackfill advance and the szDb truncate are skipped), and the next
// attempt re-backfills from the old mark.
func TestCheckpointInterruptPin(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t1(a, b); CREATE INDEX t1a ON t1(a); CREATE INDEX t1b ON t1(b); PRAGMA journal_mode = wal;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("WITH ii(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM ii WHERE i<1000) INSERT INTO t1 SELECT i, i FROM ii;"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// Arm an xWrite(test.db) hook that fires the connection's interrupt at
	// the 5th copyback page write (interrupt2's write_cb).
	writes := 0
	armed := false
	db.engine.InstallVFSHook(func(op, path string, details []string) error {
		if op == "xWrite" && strings.HasSuffix(path, "test.db") {
			writes++
			if armed && writes == 5 {
				db.engine.Interrupt()
			}
		}
		return nil
	}, 0)

	armed = true
	res := db.Query("PRAGMA wal_checkpoint")
	armed = false
	if res.Error == nil || res.Error.Error() != "interrupted" {
		t.Fatalf("interrupted checkpoint: rows=%v err=%v (want interrupted)", res.Rows, res.Error)
	}
	if writes < 5 {
		t.Fatalf("copyback writes %d: the interrupt must fire inside the copyback", writes)
	}
	writesAtInterrupt := writes

	// The interrupted attempt leaves the log standing: the next (clean)
	// checkpoint re-backfills from the old nBackfill — strictly more
	// copyback writes than the aborted attempt made.
	ck := db.Query("PRAGMA wal_checkpoint")
	if ck.Error != nil {
		t.Fatalf("clean checkpoint: %v", ck.Error)
	}
	if len(ck.Rows) != 1 || len(ck.Rows[0]) != 3 || ck.Rows[0][0] != int64(0) {
		t.Fatalf("clean checkpoint triple: %v (want {0 n n})", ck.Rows)
	}
	if writes <= writesAtInterrupt {
		t.Fatalf("writes after the clean checkpoint: %d (was %d at the interrupt) — the aborted attempt must not have advanced nBackfill", writes, writesAtInterrupt)
	}

	// A TRUNCATE checkpoint on a fully backfilled log copies nothing: even
	// with the interrupt armed it succeeds and reports {0 0 0} (the log
	// truncates to zero; interrupt2-1.x.3's C expectation needs the
	// INTERRUPTED attempt to have left frames — a drained log never
	// reaches the copyback).
	armed = true
	defer func() { armed = false }()
	before := writes
	ck2 := db.Query("PRAGMA wal_checkpoint (TRUNCATE)")
	armed = false
	if ck2.Error != nil || len(ck2.Rows) != 1 || ck2.Rows[0][1] != int64(0) || ck2.Rows[0][2] != int64(0) {
		t.Fatalf("drained truncate: rows=%v err=%v (want {0 0 0})", ck2.Rows, ck2.Error)
	}
	if writes != before {
		t.Fatalf("drained truncate wrote %d pages (want 0)", writes-before)
	}
}
