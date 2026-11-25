package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestS34RollbackRemovesDeleteJournal pins C's pager_playback →
// pager_end_transaction finalization: a DELETE-mode transaction leaves no
// journal sidecar after ROLLBACK.
func TestS34RollbackRemovesDeleteJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("PRAGMA journal_mode=delete; CREATE TABLE t(x); BEGIN; INSERT INTO t VALUES(1); ROLLBACK;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if _, err := os.Stat(path + "-journal"); !os.IsNotExist(err) {
		t.Fatalf("journal after ROLLBACK: stat err=%v, want no such file", err)
	}
}

// TestS64CloseReadOnlyWALPreservesOtherFrames pins the WAL regression behind
// wal3-9.3/9.4: closing a connection whose explicit transaction was read-only
// must not rewind frames another connection committed after its snapshot.
func TestS64CloseReadOnlyWALPreservesOtherFrames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := reader.Exec("PRAGMA journal_mode=WAL; CREATE TABLE t(x); INSERT INTO t VALUES(1);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := reader.Exec("BEGIN; SELECT * FROM t;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if r := writer.Exec("INSERT INTO t VALUES(2);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if r := writer.Query("PRAGMA wal_checkpoint"); r.Error != nil {
		t.Fatalf("checkpoint after read-only close: %v", r.Error)
	}
}

// TestS34CloseMidTxnRollsBack pins sqlite3_close → pagerUnlockAndRollback:
// closing a connection with an open transaction discards its uncommitted
// writes instead of committing them.
func TestS34CloseMidTxnRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := db.Exec("CREATE TABLE t(x); INSERT INTO t VALUES(1);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("BEGIN; INSERT INTO t VALUES(2);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r := reopened.Query("SELECT count(*) FROM t")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("rows after close = %#v, want [[1]]", r.Rows)
	}
}
