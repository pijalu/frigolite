package frigolite

import "testing"

func TestCreateTableIfNotExistsIndexNameCollisionErrors(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("CREATE TABLE t1(a); CREATE INDEX i1 ON t1(a)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := db.Exec("CREATE TABLE IF NOT EXISTS i1(a, b)")
	if result.Error == nil || result.Error.Error() != "there is already an index named i1" {
		t.Fatalf("CREATE TABLE IF NOT EXISTS index collision error = %v, want index-name conflict", result.Error)
	}
}
