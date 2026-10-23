package frigolite

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestAutoincPrepareFinalizeDoesNotCreateTable(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "autoinc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stmt, err := db.Prepare("CREATE TABLE t1(x INTEGER PRIMARY KEY AUTOINCREMENT)")
	if err != nil {
		t.Fatal(err)
	}
	if err := stmt.Finalize(); err != nil {
		t.Fatal(err)
	}
	if got := db.Query("SELECT name FROM sqlite_master WHERE name='t1'"); got.Error != nil || len(got.Rows) != 0 {
		t.Fatalf("prepare/finalize created t1: result=%+v", got)
	}

	if result := db.Exec("CREATE TABLE t1(x INTEGER PRIMARY KEY AUTOINCREMENT)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	if result := db.Exec("INSERT INTO t1 VALUES(NULL)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	got := db.Query("SELECT x FROM t1")
	if got.Error != nil || len(got.Rows) != 1 || got.Rows[0][0] != int64(1) {
		t.Fatalf("insert after executed CREATE: result=%+v", got)
	}
}

func TestAutoincRejectsSwappedSequenceRootpage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autoinc-root.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sqlText := range []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT)",
		"INSERT INTO t1(b) VALUES('one')",
		"CREATE TABLE fake(name TEXT PRIMARY KEY, seq) WITHOUT ROWID",
	} {
		if result := db.Exec(sqlText); result.Error != nil {
			t.Fatalf("%s: %v", sqlText, result.Error)
		}
	}
	root := func(name string) int64 {
		t.Helper()
		result := db.Query("SELECT rootpage FROM sqlite_master WHERE name='" + name + "'")
		if result.Error != nil || len(result.Rows) != 1 {
			t.Fatalf("rootpage for %s: %+v", name, result)
		}
		return result.Rows[0][0].(int64)
	}
	seqRoot, fakeRoot := root("sqlite_sequence"), root("fake")
	update := "PRAGMA writable_schema=ON; UPDATE sqlite_master SET rootpage=" + fmt.Sprint(fakeRoot) + " WHERE name='sqlite_sequence'; UPDATE sqlite_master SET rootpage=" + fmt.Sprint(seqRoot) + " WHERE name='fake'"
	if result := db.Exec(update); result.Error != nil {
		t.Fatal(result.Error)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("INSERT INTO t1(b) VALUES('two')"); result.Error == nil || result.Error.Error() != "database disk image is malformed" {
		t.Fatalf("swapped sqlite_sequence rootpage error = %v", result.Error)
	}
}
