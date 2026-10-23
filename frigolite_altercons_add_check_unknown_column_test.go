package frigolite

import "testing"

func TestAlterAddCheckRejectsUnknownColumn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if result := db.Exec("CREATE TABLE abc(x,y)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := db.Exec("ALTER TABLE abc ADD CHECK (z>=0)")
	if result.Error == nil || result.Error.Error() != "no such column: z" {
		t.Fatalf("ADD CHECK error = %v, want %q", result.Error, "no such column: z")
	}
}
