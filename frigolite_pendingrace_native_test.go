package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNativePendingraceHotJournalPlayback covers the engine-visible contract
// behind pendingrace.test 1.3 (whose "database is locked" expectation needs
// the tvfs2 xUnlock fault-injection VFS, stripped by the transpiler and
// recorded in tools/tcl2go/skiptests2.go as pendingrace-1.3 N-A):
//
// A hot journal left by a "crash" (uncommitted writer's journal + main file
// restored over the crashed state) must be detected and played back before
// any read succeeds: the reader never serves the crashed partial image, the
// journal is unlinked after playback, and the pre-transaction rows survive.
//
// Sequence (mirrors pendingrace.test 1.0-1.2 setup, minus the VFS race):
//  1. Create + populate t1 (writer connection).
//  2. Writer BEGIN + UPDATE (journal created with before-images), stays open.
//  3. Snapshot main + journal (db_save), then emulate my_db_restore: the
//     snapshot journal is restored over the live db while the writer is gone
//     (simulated crash: close writer WITHOUT commit after snapshotting, then
//     restore the snapshot files).
//  4. Fresh reader: SELECT serves pre-txn rows (playback), integrity_check
//     is "ok", journal file is gone.
func TestNativePendingraceHotJournalPlayback(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	jrnlPath := dbPath + "-journal"

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if r := db.Exec("PRAGMA cache_size=5"); r.Error != nil {
		t.Fatalf("cache_size: %v", r.Error)
	}
	if r := db.Exec("CREATE TABLE t1(a, b)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	if r := db.Exec("CREATE INDEX i1 ON t1(a, b)"); r.Error != nil {
		t.Fatalf("index: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t1 VALUES('orig-a', 'orig-b')"); r.Error != nil {
		t.Fatalf("insert: %v", r.Error)
	}
	db.Close()

	// Crashed writer: BEGIN + UPDATE, journal present, never commits.
	writer, err := Open(dbPath)
	if err != nil {
		t.Fatalf("writer open: %v", err)
	}
	if r := writer.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("begin: %v", r.Error)
	}
	if r := writer.Exec("UPDATE t1 SET b='crashed-b'"); r.Error != nil {
		t.Fatalf("update: %v", r.Error)
	}
	snapMain, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("snapshot main: %v", err)
	}
	snapJrnl, err := os.ReadFile(jrnlPath)
	if err != nil {
		t.Fatalf("snapshot journal: %v (a hot journal must exist mid-txn)", err)
	}
	// Simulated crash: the writer process dies without COMMIT/ROLLBACK.
	// Close releases locks; restore the snapshot files to emulate the
	// my_db_restore file surgery (journal back over the live db).
	writer.Close()
	if err := os.WriteFile(jrnlPath, snapJrnl, 0644); err != nil {
		t.Fatalf("restore journal: %v", err)
	}
	if err := os.WriteFile(dbPath, snapMain, 0644); err != nil {
		t.Fatalf("restore main: %v", err)
	}

	// Fresh reader with a hot journal present: playback before any read.
	reader, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reader open: %v", err)
	}
	defer reader.Close()
	q := reader.Query("SELECT b FROM t1")
	if q.Error != nil {
		t.Fatalf("select: %v", q.Error)
	}
	if len(q.Rows) != 1 || q.Rows[0][0] != "orig-b" {
		t.Fatalf("reader served crashed image: got %v, want [[orig-b]]", q.Rows)
	}
	ic := reader.Exec("PRAGMA integrity_check")
	if ic.Error != nil {
		t.Fatalf("integrity_check: %v", ic.Error)
	}
	if len(ic.Rows) != 1 || ic.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check rows: got %v, want [[ok]]", ic.Rows)
	}
	if _, err := os.Stat(jrnlPath); !os.IsNotExist(err) {
		t.Fatalf("hot journal not played back/unlinked (stat err=%v)", err)
	}
}
