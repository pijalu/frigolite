package frigolite

import (
	"testing"

	"github.com/pijalu/frigolite/internal/auth"
)

func TestAlterTableAuthorizerDeniesConstraintChanges(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if result := db.Exec("CREATE TABLE t(a)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	db.SetAuthorizer(auth.NewActionFilterAuthorizer(auth.ActionAlterTable))
	result := db.Exec("ALTER TABLE t ADD CONSTRAINT c CHECK(a>0)")
	if result.Error == nil || result.Error.Error() != "not authorized" {
		t.Fatalf("ALTER with denied authorizer = %v, want not authorized", result.Error)
	}
	query := db.Query("SELECT sql FROM sqlite_schema WHERE name='t'")
	if query.Error != nil || len(query.Rows) != 1 {
		t.Fatalf("schema query after denied ALTER = rows %v, err %v", query.Rows, query.Error)
	}
}
