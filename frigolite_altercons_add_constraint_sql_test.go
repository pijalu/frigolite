package frigolite

import "testing"

func TestAlterAddConstraintPreservesExpressionSQL(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE t1(a, b)",
		"ALTER TABLE t1 ADD CONSTRAINT nn CHECK (a>=0)",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	result := db.Query("SELECT sql FROM sqlite_schema WHERE name='t1'")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := "CREATE TABLE t1(a, b, CONSTRAINT nn CHECK (a>=0))"
	if len(result.Rows) != 1 || result.Rows[0][0] != want {
		t.Fatalf("sqlite_schema.sql = %v, want %q", result.Rows, want)
	}
}
