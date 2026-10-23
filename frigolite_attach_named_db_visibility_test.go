package frigolite

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestAttachSeesTableCreatedByNamedConnection(t *testing.T) {
	dir := t.TempDir()
	mainDB, err := Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer mainDB.Close()
	namedDB, err := Open(filepath.Join(dir, "aux.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer namedDB.Close()
	if r := namedDB.Exec("CREATE TABLE t(x); INSERT INTO t VALUES(7)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := mainDB.Exec("ATTACH DATABASE '" + filepath.Join(dir, "aux.db") + "' AS aux; SELECT * FROM aux.t"); r.Error != nil {
		t.Fatal(r.Error)
	} else if !reflect.DeepEqual(r.Rows, [][]interface{}{{int64(7)}}) {
		t.Fatalf("attached rows = %#v, want [[7]]", r.Rows)
	}
	if r := mainDB.Exec("DETACH aux"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := mainDB.Query("SELECT * FROM t"); r.Error == nil {
		t.Fatalf("query after DETACH unexpectedly succeeded: %#v", r.Rows)
	}
}
