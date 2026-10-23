package frigolite

import "testing"

func TestDropConstraintRewritesMalformedStoredSQL(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE x1(a, b CONSTRAINT ott REFERENCES x2)",
		"PRAGMA writable_schema=1",
		"UPDATE sqlite_schema SET sql='CREATE TABLE x1(a, b CONSTRAINT ott REFERENCES' WHERE name='x1'",
		"ALTER TABLE x1 DROP CONSTRAINT ott",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	result := db.Query("SELECT sql FROM sqlite_schema WHERE name='x1'")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "CREATE TABLE x1(a, b " {
		t.Fatalf("sqlite_schema.sql = %v, want %q", result.Rows, "CREATE TABLE x1(a, b ")
	}
}
