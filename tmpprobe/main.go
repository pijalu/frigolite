// Throwaway probe for upsert2-201 (T9). NOT committed.
package main

import (
	"fmt"
	"os"

	"github.com/pijalu/frigolite"
)

func main() {
	os.Remove("/tmp/t9.db")
	db, err := frigolite.Open("/tmp/t9.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)"); res.Error != nil {
		panic(res.Error)
	}
	// simpler DO UPDATE first (no alias, no WITH)
	r := db.Query("INSERT INTO t1(a,b) VALUES(1,2),(3,4);\nWITH nx(a,b) AS (VALUES(1,8),(2,11))\n  INSERT INTO main.t1 AS t2(a,b) SELECT a, b FROM nx WHERE true\n    ON CONFLICT(a) DO UPDATE SET b=excluded.b, c=t2.c+1 WHERE t2.b<excluded.b;\nSELECT * FROM t1;")
	fmt.Println("with-alias:", r.Rows, r.Error)
	r = db.Query("WITH nx(a,b) AS (VALUES(1,8))\n  INSERT INTO main.t1 SELECT a, b FROM nx WHERE true\n    ON CONFLICT(a) DO UPDATE SET b=excluded.b;\nSELECT * FROM t1;")
	fmt.Println("no-alias:", r.Rows, r.Error)
}
