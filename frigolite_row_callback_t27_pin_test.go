package frigolite

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRowCallbackBusySnapshotContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	if r := db1.Exec("PRAGMA journal_mode=wal"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db1.Query("PRAGMA journal_mode"); r.Error != nil || len(r.Rows) == 0 || r.Rows[0][0] != "wal" {
		t.Fatalf("journal mode: %#v", r)
	}
	if r := db1.Exec("CREATE TABLE t1(x, y)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db1.Exec("INSERT INTO t1 VALUES(1, 2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// db2 adopts the current WAL snapshot only after db1's writes are visible;
	// an earlier read would make its later insert a correct BUSY_SNAPSHOT.
	if r := db2.Query("PRAGMA journal_mode"); r.Error != nil || len(r.Rows) == 0 || r.Rows[0][0] != "wal" {
		t.Fatalf("db2 journal mode: %#v", r)
	}
	if !db1.engine.GetDB("main").Pager.WALMode() || !db2.engine.GetDB("main").Pager.WALMode() {
		t.Fatalf("pager WAL flags: db1=%v db2=%v", db1.engine.GetDB("main").Pager.WALMode(), db2.engine.GetDB("main").Pager.WALMode())
	}

	st, err := db1.Prepare("SELECT * FROM t1")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ok, err := st.Step()
	if err != nil || !ok {
		t.Fatalf("first step: %v %v", ok, err)
	}
	if r := db2.Exec("INSERT INTO t1 VALUES(5, 6)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db1.Exec("BEGIN EXCLUSIVE")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "database is locked") {
		t.Fatalf("BEGIN EXCLUSIVE got %v, want database is locked", r.Error)
	}
	if got := db1.LastErrCode(); got != "SQLITE_BUSY_SNAPSHOT" {
		t.Fatalf("LastErrCode got %s want SQLITE_BUSY_SNAPSHOT", got)
	}
}

func TestRowCallbackCheckpointInterruptContract(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("PRAGMA journal_mode=wal"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("CREATE TABLE t1(x)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for i := 0; i < 20; i++ {
		if r := db.Exec("INSERT INTO t1 VALUES(" + strconv.Itoa(i) + ")"); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	db.engine.Interrupt()
	res := db.engine.WalCheckpoint("", "truncate")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "interrupted") {
		t.Fatalf("interrupted checkpoint got %v, want interrupted", res.Error)
	}
}
