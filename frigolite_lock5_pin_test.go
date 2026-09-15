package frigolite_test

// Pin test for the lock5.test 2.dotfile engine contract (FULL-SUITE-DRIFT
// pairs-pager cluster): a rollback journal copied alongside its database
// plays back on Open when no live writer holds the file (C pager.c
// hasHotJournal -> pagerPlayback). The journal must decode as C wrote it —
// BIG-endian header integers, records of [4-byte BE pageNum][pageSize bytes
// of data][4-byte BE checksum] (JOURNAL_PG_SZ = pageSize+8) — so playback
// restores the pre-transaction images and PRAGMA integrity_check reports
// "ok" (lock5-2.dotfile.5), then the pre-transaction row count is readable
// (lock5-2.dotfile.6).

import (
	"os"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestLock5HotJournalPlaybackAfterCopy(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query("CREATE TABLE t1(x, y, z); CREATE INDEX t1x ON t1(x); WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<1000) INSERT INTO t1 SELECT hex(randomblob(20)), hex(randomblob(500)), i FROM s;")
	if r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	// Open-ended write transaction: journal exists, writer still live.
	if r := db.Exec("BEGIN; UPDATE t1 SET z=z+1, x=hex(randomblob(20));"); r.Error != nil {
		t.Fatalf("begin+update: %v", r.Error)
	}
	copyFile := func(src, dst string) {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("copy %s: %v", src, err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyFile("test.db", "test.db2")
	copyFile("test.db-journal", "test.db2-journal")

	// A DIFFERENT path: the live-writer registry gate must not fire; the
	// copied journal is hot for the dead test.db2 generation.
	db2, err := frigolite.Open("test.db2")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	r = db2.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check after hot-journal playback: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check: got %v, want [[ok]]", r.Rows)
	}
	r = db2.Query("SELECT count(*) FROM t1")
	if r.Error != nil {
		t.Fatalf("post-playback count: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(1000) {
		t.Fatalf("post-playback count: got %v, want [[1000]] (pre-transaction image)", r.Rows)
	}
}
