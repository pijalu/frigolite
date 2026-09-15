package frigolite_test

// Pin tests for the pcache.test 1.x engine contract (FULL-SUITE-DRIFT
// pairs-pager cluster):
//
//  1. Connection-local pragma setters (cache_size, mmap_size, busy_timeout)
//     take no file lock: PRAGMA cache_size=N succeeds on db2 while db holds
//     an open write transaction (pcache-1.5). Only the header-cookie pragmas
//     (schema_version/user_version/application_id), auto_vacuum=1|2 and
//     journal_mode open a write transaction in C (pragma.c setCookie /
//     setMeta6 op lists).
//
//  2. A second connection opening the same file must not treat the live
//     writer's rollback journal as hot (C: playback needs EXCLUSIVE, denied
//     by the writer's RESERVED lock — lockreg gate), and a read transaction
//     ROLLBACK on a zero-byte database must not materialize the file
//     (pager.c lazy creation).
//
//  3. After the writer's COMMIT the file holds the committed image and the
//     journal is gone.

import (
	"os"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestPcachePragmaCacheSizeNoWriteLock(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("BEGIN; CREATE TABLE t1(a, b, c);"); r.Error != nil {
		t.Fatalf("begin+create: %v", r.Error)
	}
	db2, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	// The cache_size setter is connection-local: no write lock required.
	if r := db2.Query("PRAGMA cache_size; PRAGMA cache_size=10"); r.Error != nil {
		t.Fatalf("PRAGMA cache_size setter while db holds a write txn: %v", r.Error)
	}
	if r := db2.Query("PRAGMA mmap_size=0"); r.Error != nil {
		t.Fatalf("PRAGMA mmap_size setter: %v", r.Error)
	}
	if r := db2.Query("PRAGMA busy_timeout=100"); r.Error != nil {
		t.Fatalf("PRAGMA busy_timeout setter: %v", r.Error)
	}
	// auto_vacuum (setMeta6) and the header cookies DO take the write lock.
	if r := db2.Exec("PRAGMA auto_vacuum=2"); r.Error == nil {
		t.Fatalf("PRAGMA auto_vacuum=2 while db holds a write txn: want locked error, got success")
	}
}

func TestPcacheLiveWriterJournalNotHot(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("BEGIN; CREATE TABLE t1(a, b, c); CREATE TABLE t2(a, b, c);"); r.Error != nil {
		t.Fatalf("begin+create: %v", r.Error)
	}
	if _, err := os.Stat("test.db-journal"); err != nil {
		t.Fatalf("writer journal missing during txn: %v", err)
	}
	db2, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("test.db-journal"); err != nil {
		db2.Close()
		t.Fatalf("second connection's Open unlinked the live writer's journal: %v", err)
	}
	// The reader's read transaction + rollback must not write the file.
	if r := db2.Query("BEGIN; SELECT * FROM sqlite_master;"); r.Error != nil {
		db2.Close()
		t.Fatalf("db2 read txn: %v", r.Error)
	}
	if r := db2.Exec("ROLLBACK"); r.Error != nil {
		db2.Close()
		t.Fatalf("db2 rollback: %v", r.Error)
	}
	if fi, err := os.Stat("test.db"); err == nil && fi.Size() != 0 {
		db2.Close()
		t.Fatalf("read-only rollback materialized the empty database: size=%d", fi.Size())
	}
	db2.Close()
	// The writer commits through the protected journal.
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("commit: %v", r.Error)
	}
	if _, err := os.Stat("test.db-journal"); err == nil {
		t.Fatalf("journal survived the writer's commit")
	}
	r := db.Query("SELECT count(*) FROM sqlite_master")
	if r.Error != nil {
		t.Fatalf("post-commit read: %v", r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("post-commit schema count: got %v, want [[2]]", r.Rows)
	}
}
