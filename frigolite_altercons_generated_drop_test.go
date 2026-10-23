package frigolite

import "testing"

func TestAlterDropConstraintPreservesStoredGeneratedColumn(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE t1(a,b,c CONSTRAINT abc GENERATED ALWAYS AS (b+1) STORED)",
		"ALTER TABLE t1 DROP CONSTRAINT abc",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	result := db.Query("SELECT sql FROM sqlite_schema WHERE name='t1'")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "CREATE TABLE t1(a, b, c GENERATED ALWAYS AS (b+1) STORED)" {
		t.Fatalf("sqlite_schema.sql = %v, want CREATE TABLE t1(a, b, c GENERATED ALWAYS AS (b+1) STORED)", result.Rows)
	}
}
