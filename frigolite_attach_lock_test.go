package frigolite

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachedWriteBlockedByOtherConnectionTransaction(t *testing.T) {
	dir := t.TempDir()
	auxPath := filepath.Join(dir, "test2.db")
	aux, err := Open(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer aux.Close()
	if r := aux.Exec("CREATE TABLE t1(a,b)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	main, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	if r := main.Exec("CREATE TABLE t1(a,b); ATTACH DATABASE '" + auxPath + "' AS t2; DETACH t2"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := aux.Exec("BEGIN; UPDATE t1 SET a=0 WHERE 0"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := main.Exec("ATTACH DATABASE '" + auxPath + "' AS t2"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := aux.Exec("COMMIT"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := main.Query("SELECT name FROM t2.sqlite_master"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := aux.Exec("BEGIN; UPDATE t1 SET a=0 WHERE 0"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := main.Query("SELECT name FROM t2.sqlite_master"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := main.Query("SELECT name FROM main.sqlite_master"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := main.Exec("BEGIN; INSERT INTO t1 VALUES(8,9)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := main.Exec("INSERT INTO t2.t1 VALUES(1,2)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "database is locked") {
		t.Fatalf("attached write error = %v, want database is locked", r.Error)
	}
}
