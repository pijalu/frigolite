package frigolite

import "testing"

func TestAlterDropUniqueConstraintRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if result := db.Exec("CREATE TABLE t2(x, y CONSTRAINT ccc UNIQUE)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := db.Exec("ALTER TABLE t2 DROP CONSTRAINT ccc")
	if result.Error == nil || result.Error.Error() != "constraint may not be dropped: ccc" {
		t.Fatalf("DROP CONSTRAINT error = %v, want %q", result.Error, "constraint may not be dropped: ccc")
	}
}
