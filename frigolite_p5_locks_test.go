package frigolite

import "testing"

func TestP5PreparedReadLocksWriteAndLifetime(t *testing.T) {
	path := t.TempDir() + "/locks.db"
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if r := reader.Exec("CREATE TABLE t(x)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := reader.Exec("INSERT INTO t VALUES (1)"); r.Error != nil {
		t.Fatal(r.Error)
	}

	stmt, err := reader.Prepare("SELECT x FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := stmt.Step(); err != nil || !ok {
		t.Fatalf("first step: ok=%v err=%v", ok, err)
	}
	if r := writer.Exec("INSERT INTO t VALUES (2)"); r.Error == nil || reader.errorCode(r.Error) != "SQLITE_BUSY" {
		t.Fatalf("write while cursor active: %v", r.Error)
	}
	if err := reader.Close(); err == nil {
		t.Fatal("close with finalized statement expected busy")
	}
	if err := stmt.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if r := writer.Exec("INSERT INTO t VALUES (2)"); r.Error != nil {
		t.Fatalf("write after release: %v", r.Error)
	}
}

func TestP5PreparedReadLockReleasesAtExhaustion(t *testing.T) {
	db1, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	if r := db1.Exec("CREATE TABLE t(x)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db1.Exec("INSERT INTO t VALUES (1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	stmt, err := db1.Prepare("SELECT x FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := stmt.Step(); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := stmt.Step(); err != nil || ok {
		t.Fatal(ok, err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
}
