package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDBCacheFlushBusyAndContinuesAttachedPagers(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	auxPath := filepath.Join(dir, "aux.db")
	main, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	aux, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer aux.Close()
	for _, db := range []*DB{main, aux} {
		if r := db.Exec("CREATE TABLE t(a); INSERT INTO t VALUES(1)"); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if r := main.Exec("ATTACH DATABASE '" + auxPath + "' AS aux; BEGIN; INSERT INTO main.t VALUES(2); INSERT INTO aux.t VALUES(2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	reader, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if r := reader.Query("SELECT * FROM t"); r.Error != nil {
		t.Fatal(r.Error)
	}
	reader.engine.SetPreparedReadLock("main", true)
	err = main.CacheFlush()
	if err == nil {
		t.Fatal("CacheFlush succeeded despite main reader")
	}
	auxDisk, err := os.ReadFile(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(auxDisk) == 0 {
		t.Fatal("auxiliary database file is empty")
	}
	auxReader, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	rows := auxReader.Query("SELECT a FROM t ORDER BY a")
	if rows.Error != nil || len(rows.Rows) != 2 {
		t.Fatalf("auxiliary spill not visible: rows=%v err=%v", rows.Rows, rows.Error)
	}
	_ = auxReader.Close()
	reader.engine.SetPreparedReadLock("main", false)
	if r := main.Exec("ROLLBACK"); r.Error != nil {
		t.Fatal(r.Error)
	}
}

func TestDBCacheFlushWALSavepointRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal-savepoint.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE t(a); INSERT INTO t VALUES(1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("BEGIN; SAVEPOINT s; INSERT INTO t VALUES(2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if err := db.CacheFlush(); err != nil {
		t.Fatal(err)
	}
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := reader.Query("SELECT a FROM t ORDER BY a")
	if rows.Error != nil || len(rows.Rows) != 1 {
		t.Fatalf("uncommitted WAL spill visible to reader: rows=%v err=%v", rows.Rows, rows.Error)
	}
	_ = reader.Close()
	if r := db.Exec("ROLLBACK TO s; RELEASE s; COMMIT"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query("SELECT a FROM t ORDER BY a")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("after WAL savepoint rollback: rows=%v err=%v", r.Rows, r.Error)
	}
}

func TestDBCacheFlushSavepointRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "savepoint.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(a); INSERT INTO t VALUES(1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("BEGIN; SAVEPOINT s; INSERT INTO t VALUES(2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if err := db.CacheFlush(); err != nil {
		t.Fatal(err)
	}
	if r := db.Exec("ROLLBACK TO s; RELEASE s; COMMIT"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query("SELECT a FROM t ORDER BY a")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("after savepoint rollback: rows=%v err=%v", r.Rows, r.Error)
	}
}

func TestDBCacheFlushWritesWithoutEndingTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cacheflush.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(a); INSERT INTO t VALUES(1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("BEGIN; INSERT INTO t VALUES(2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	dirtyBefore := db.pager.DirtyPageCount()
	if dirtyBefore == 0 {
		t.Fatal("expected dirty pager before cache flush")
	}
	if err := db.CacheFlush(); err != nil {
		t.Fatal(err)
	}
	if got := db.pager.DirtyPageCount(); got != dirtyBefore {
		t.Fatalf("dirty page count=%d, want unchanged %d", got, dirtyBefore)
	}
	if !db.InTransaction() {
		t.Fatal("cache flush ended transaction")
	}
	if st, err := os.Stat(path + "-journal"); err != nil || st.Size() == 0 {
		t.Fatalf("rollback journal missing or empty: stat=%v err=%v", st, err)
	}
	if r := db.Exec("ROLLBACK"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query("SELECT a FROM t ORDER BY a")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("after rollback: rows=%v err=%v", r.Rows, r.Error)
	}
}
