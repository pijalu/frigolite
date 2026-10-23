package frigolite

import "testing"

func TestDropConstraintWithEscapedQuotedName(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE t1(a, b CONSTRAINT "a""b" CHECK (b IS NOT NULL))`,
		`ALTER TABLE t1 DROP CONSTRAINT "a""b"`,
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("Exec(%q): %v", statement, result.Error)
		}
	}
	result := db.Query("SELECT sql FROM sqlite_schema WHERE name='t1'")
	if result.Error != nil || len(result.Rows) != 1 || result.Rows[0][0] != "CREATE TABLE t1(a, b)" {
		t.Fatalf("schema SQL = %v, err %v", result.Rows, result.Error)
	}
}
