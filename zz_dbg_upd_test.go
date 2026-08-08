package frigolite_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

func TestDbgUpdReplace(t *testing.T) {
	stmts, _ := parse.ParseSQL(`UPDATE OR REPLACE t1 SET a = 1`)
	u := stmts[0].(*sql.UpdateStmt)
	fmt.Printf("OnConflict=%q\n", u.OnConflict)
}
