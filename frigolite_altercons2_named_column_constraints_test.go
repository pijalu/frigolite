package frigolite

import "testing"

func TestDropNamedConstraintsPreservesFollowingColumnClauses(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE abc(a, b CONSTRAINT two COLLATE nocase CHECK (a!=b), c CONSTRAINT one DEFAULT 'abc')",
		"ALTER TABLE abc DROP CONSTRAINT one",
		"ALTER TABLE abc DROP CONSTRAINT two",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	result := db.Query("SELECT sql FROM sqlite_schema WHERE name='abc'")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := "CREATE TABLE abc(a, b COLLATE nocase CHECK (a!=b), c DEFAULT 'abc')"
	if len(result.Rows) != 1 || result.Rows[0][0] != want {
		t.Fatalf("sqlite_schema.sql = %v, want %q", result.Rows, want)
	}
}
