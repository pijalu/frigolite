package frigolite

import "testing"

func TestAlterAddDuplicateConstraintNameRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if result := db.Exec("CREATE TABLE b1(a, b, CONSTRAINT abc CHECK (a!=2))"); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := db.Exec("ALTER TABLE b1 ADD CONSTRAINT abc CHECK (a!=3)")
	if result.Error == nil || result.Error.Error() != "constraint abc already exists" {
		t.Fatalf("ADD duplicate constraint error = %v, want %q", result.Error, "constraint abc already exists")
	}
}
