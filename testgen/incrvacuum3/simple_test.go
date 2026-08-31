package incrvacuum3

import (
	"fmt"
	"os"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestSimpleRollback(t *testing.T) {
	os.Chdir(t.TempDir())
	db, _ := frigolite.Open("test.db")
	defer db.Close()
	db.Exec("PRAGMA auto_vacuum = 2")
	db.Exec("PRAGMA page_size = 1024")
	db.Exec("CREATE TABLE t1(a)")
	db.Exec("INSERT INTO t1 VALUES(randomblob(400))")
	for i := 0; i < 7; i++ {
		db.Exec("INSERT INTO t1 SELECT randomblob(400) FROM t1")
	}
	db.Exec("COMMIT")
	r := db.Exec("DELETE FROM t1 WHERE rowid%8")
	fmt.Printf("DELETE: err=%v\n", r.Error)
	db.Exec("COMMIT")
	r = db.Exec("BEGIN")
	fmt.Printf("BEGIN: err=%v\n", r.Error)
	r = db.Exec("PRAGMA incremental_vacuum = 100")
	fmt.Printf("VACUUM: err=%v\n", r.Error)
	r = db.Exec("INSERT INTO t1 SELECT randomblob(400) FROM t1")
	fmt.Printf("INSERT1: err=%v\n", r.Error)
}
