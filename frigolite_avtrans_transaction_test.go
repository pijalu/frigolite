package frigolite

import "testing"

func TestAvtransDropRecreateKeepsExplicitTransaction(t *testing.T) {
	db, err := Open(t.TempDir() + "/avtrans.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE t1(a,b,c)",
		"BEGIN TRANSACTION",
		"DROP TABLE t1",
		"CREATE TABLE t1(a,b,c)",
		"INSERT INTO t1 VALUES(4,5,6)",
		"DROP TABLE t1",
	} {
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("%s: %v", q, r.Error)
		}
	}
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("COMMIT: %v", r.Error)
	}
	if r := db.Query("SELECT * FROM t1"); r.Error == nil || r.Error.Error() != "no such table: t1" {
		t.Fatalf("SELECT after committed DROP: got %v, want no such table: t1", r.Error)
	}
}
