package frigolite

import "testing"

func TestAlterDropNotNullUsesCachedColumnsAfterWritableSchemaEdit(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE t1(a, b, c NOT NULL, CONSTRAINT xyz CHECK(a!=0))",
		"PRAGMA writable_schema=1",
		"UPDATE sqlite_schema SET sql='CREATE TABLE t1(a, b, ' WHERE name='t1'",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	if result := db.Exec("ALTER TABLE t1 ALTER c DROP NOT NULL"); result.Error != nil {
		t.Fatalf("ALTER with cached schema columns: %v", result.Error)
	}
}
