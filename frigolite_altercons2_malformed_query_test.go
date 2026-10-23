package frigolite

import "testing"

func TestAlterMalformedSchemaQueryWithWritableSchema(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	steps := []string{
		"CREATE TABLE t1(a, b, c NOT NULL, CONSTRAINT xyz CHECK(a!=0))",
		"PRAGMA writable_schema=1",
		"UPDATE sqlite_schema SET sql='CREATE TABLE t1(a, b, c NOT NULL' WHERE name='t1'",
		"ALTER TABLE t1 ALTER c DROP NOT NULL",
	}
	for _, statement := range steps {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	result := db.Query("SELECT sql FROM sqlite_schema WHERE name='t1'")
	if result.Error != nil {
		t.Fatalf("query sqlite_schema with writable_schema: %v", result.Error)
	}
}
