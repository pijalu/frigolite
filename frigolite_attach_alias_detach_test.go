package frigolite

import (
	"path/filepath"
	"testing"
)

func TestDetachMultipleAliasesInAttachOrder(t *testing.T) {
	mainPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t1(x)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, name := range []string{"db2", "db3", "db4", "db5", "db6", "db7", "db8", "db9", "db10", "db11"} {
		if r := db.Exec("ATTACH DATABASE '" + mainPath + "' AS " + name); r.Error != nil {
			t.Fatalf("attach %s: %v", name, r.Error)
		}
	}
	if r := db.Exec("DETACH db5"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("DETACH db11; DETACH db10; DETACH db9; DETACH db8; DETACH db7; DETACH db6; DETACH db4; DETACH db3; DETACH db2"); r.Error != nil {
		t.Fatalf("detach aliases: %v", r.Error)
	}
}
