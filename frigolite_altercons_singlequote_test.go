package frigolite

import "testing"

func TestAlterSetNotNullPreservesSingleQuotedColumnName(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE t1(a, 'a b c' VARCHAR(10), UNIQUE(a))",
		"ALTER TABLE t1 ALTER 'a b c' SET NOT NULL",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	result := db.Query("SELECT sql FROM sqlite_schema WHERE name='t1'")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := "CREATE TABLE t1(a, 'a b c' VARCHAR(10) NOT NULL, UNIQUE(a))"
	if len(result.Rows) != 1 || result.Rows[0][0] != want {
		t.Fatalf("sqlite_schema.sql = %v, want %q", result.Rows, want)
	}
}
