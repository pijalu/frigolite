package frigolite

import "testing"

func TestTablePrimaryKeyDescIsIntegerRowIDAlias(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("CREATE TABLE t(x INTEGER, y, z, PRIMARY KEY(x DESC))"); result.Error != nil {
		t.Fatal(result.Error)
	}
	if result := db.Exec("INSERT INTO t(x) VALUES(0)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	rows := db.Query("SELECT rowid = x FROM t")
	if rows.Error != nil || len(rows.Rows) != 1 || rows.Rows[0][0] != int64(1) {
		t.Fatalf("rowid alias result = %v, error = %v; want [[1]]", rows.Rows, rows.Error)
	}
}
