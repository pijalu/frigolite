package frigolite_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

func TestZZIV3Probe(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "iv3.db")
	db, err := frigolite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	doublings := func(n int) []string {
		sqls := []string{
			"CREATE TABLE t1(x UNIQUE);",
			"INSERT INTO t1 VALUES(randomblob(400));",
			"INSERT INTO t1 VALUES(randomblob(400));",
		}
		for i := 0; i < n; i++ {
			sqls = append(sqls, "INSERT INTO t1 SELECT randomblob(400) FROM t1;")
		}
		return sqls
	}

	setup := []string{
		"PRAGMA cache_size = 5;",
		"PRAGMA page_size = 1024;",
		"PRAGMA auto_vacuum = 2;",
	}
	setup = append(setup, doublings(7)...)
	for _, sql := range setup {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	// tn2: delete half
	if r := db.Exec("DELETE FROM t1 WHERE rowid%8 != 0;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	stat := func(stage string) {
		fl := db.Query("PRAGMA freelist_count;")
		pc := db.Query("PRAGMA page_count;")
		fmt.Printf("ZZ %s: freelist=%v page_count=%v\n", stage, fl.Rows, pc.Rows)
	}
	stat("after delete")
	// tn8
	for _, sql := range []string{
		"BEGIN;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"PRAGMA incremental_vacuum = 1000;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"COMMIT;",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("tn8 %s: %v", sql, r.Error)
		}
		stat("after "+sql)
	}
	ic := db.Query("PRAGMA integrity_check;")
	fmt.Printf("ZZ integrity: %v\n", ic.Rows)
	_ = os.Getenv
}
