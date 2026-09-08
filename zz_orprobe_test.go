package frigolite

import (
	"os"
	"testing"
)

func TestProbeVacuumInto(t *testing.T) {
	os.Remove("vacuum_out2.db")
	db, _ := Open(":memory:")
	defer db.Close()
	if r := db.Exec("CREATE TABLE t1(a)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("VACUUM INTO 'vacuum_out2.db'"); r.Error != nil {
		t.Log("vacuum into err:", r.Error)
	} else {
		t.Log("vacuum into ok, exists:", fileExists("vacuum_out2.db"))
	}
	os.Remove("vacuum_out2.db")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
