package frigolite

import (
	"strings"
	"testing"
)

func TestCreateTableRejectsTooManyUniqueIndexColumns(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	sql := "CREATE TABLE t1(x, UNIQUE(x" + strings.Repeat(",x", 100000) + "))"
	result := db.Exec(sql)
	if result.Error == nil || result.Error.Error() != "too many columns in index" {
		t.Fatalf("CREATE TABLE with oversized UNIQUE index error = %v, want too many columns in index", result.Error)
	}
}
