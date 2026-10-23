package frigolite

import "testing"

func TestAlterDropNotNullPreservesOriginalColumnDefinition(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE t1(a CHECK(a<b) CONSTRAINT nn NOT NULL, b)",
		"ALTER TABLE t1 ALTER COLUMN a DROP NOT NULL",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	result := db.Query("SELECT sql FROM sqlite_schema WHERE name='t1'")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := "CREATE TABLE t1(a CHECK(a<b), b)"
	if len(result.Rows) != 1 || result.Rows[0][0] != want {
		t.Fatalf("sqlite_schema.sql = %v, want %q", result.Rows, want)
	}
}
