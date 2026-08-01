package main

import (
	"fmt"
	"github.com/pijalu/frigolite"
)

func main() {
	db, _ := frigolite.Open(":memory:")
	defer db.Close()
	db.Exec("CREATE TABLE t1(a INT, b INT); INSERT INTO t1 VALUES(1,2),(1,3),(1,4); CREATE INDEX t1a ON t1(a); CREATE TABLE t2(c INT, d INT); INSERT INTO t2 VALUES(3,33),(4,44),(5,55); CREATE INDEX t2c ON t2(c);")
	r := db.Query("SELECT t1.*, t2.* FROM t2 FULL OUTER JOIN t1 ON b=c ORDER BY +b")
	fmt.Printf("err=%v\n", r.Error)
	for _, row := range r.Rows { out := ""; for _, v := range row { out += fmt.Sprintf("%v ", v) }; fmt.Printf("[%s]\n", out) }
}
