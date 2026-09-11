// Throwaway probe for pragma2-5.1 (T7.4). NOT committed.
package main

import (
	"fmt"
	"os"

	"github.com/pijalu/frigolite"
)

func main() {
	dir := "/tmp/t74probe"
	os.RemoveAll(dir)
	os.MkdirAll(dir, 0755)
	os.Chdir(dir)
	os.Remove("test.db")
	os.Remove("test.db-journal")
	os.Remove("test2.db")
	os.Remove("test2.db-journal")

	db, err := frigolite.Open("test.db")
	if err != nil {
		panic(err)
	}
	must := func(res *frigolite.Result, label string) {
		if res.Error != nil {
			fmt.Printf("%s ERR: %v\n", label, res.Error)
		} else {
			fmt.Printf("%s ok\n", label)
		}
	}
	must(db.Exec("PRAGMA auto_vacuum=0"), "auto_vacuum=0")
	must(db.Exec("PRAGMA main.cache_size=2000;\nPRAGMA temp.cache_size=2000"), "4.1 sizes")
	must(db.Exec("PRAGMA cache_spill=OFF"), "4.2")
	must(db.Exec("PRAGMA page_size=1024;\nPRAGMA cache_size=50;\nBEGIN;\nCREATE TABLE t1(a INTEGER PRIMARY KEY, b, c, d);\n"+
		"INSERT INTO t1 VALUES(1, randomblob(400), 1, randomblob(400));"+
		"INSERT INTO t1 SELECT a+1, randomblob(400), a+1, randomblob(400) FROM t1;"+
		"INSERT INTO t1 SELECT a+2, randomblob(400), a+2, randomblob(400) FROM t1;"+
		"INSERT INTO t1 SELECT a+4, randomblob(400), a+4, randomblob(400) FROM t1;"+
		"INSERT INTO t1 SELECT a+8, randomblob(400), a+8, randomblob(400) FROM t1;"+
		"INSERT INTO t1 SELECT a+16, randomblob(400), a+16, randomblob(400) FROM t1;"+
		"INSERT INTO t1 SELECT a+32, randomblob(400), a+32, randomblob(400) FROM t1;"+
		"INSERT INTO t1 SELECT a+64, randomblob(400), a+64, randomblob(400) FROM t1;\nCOMMIT;\n"+
		"ATTACH 'test2.db' AS aux1;\nCREATE TABLE aux1.t2(a INTEGER PRIMARY KEY, b, c, d);\n"+
		"INSERT INTO t2 SELECT * FROM t1;\nDETACH aux1;\nPRAGMA cache_spill=ON;"), "4.3 big build")
	r := db.Query("ROLLBACK;\nPRAGMA cache_spill(-25);\nPRAGMA main.cache_spill;\nBEGIN;\nUPDATE t1 SET c=c+1;\nPRAGMA lock_status;")
	fmt.Println("4.5:", r.Rows, r.Error)
	must(db.Exec("ROLLBACK"), "4.5 rollback")
	r = db.Query("ROLLBACK;\nPRAGMA cache_spill=OFF;\nATTACH 'test2.db' AS aux1;\nPRAGMA aux1.cache_size=50;\nBEGIN;\nUPDATE t2 SET c=c+1;\nPRAGMA lock_status;")
	fmt.Println("4.6:", r.Rows, r.Error)
	must(db.Exec("COMMIT"), "4.7")
	r = db.Query("PRAGMA cache_spill=ON;\nBEGIN;\nUPDATE t2 SET c=c-1;\nPRAGMA lock_status;")
	fmt.Println("4.8:", r.Rows, r.Error)
	db.Close()

	db, err = frigolite.Open("test.db")
	if err != nil {
		panic(fmt.Sprintf("REOPEN: %v", err))
	}
	r = db.Query("PRAGMA page_size=16384;\nCREATE TABLE t1(x);\nPRAGMA cache_size=2;\nPRAGMA cache_spill=YES;\nPRAGMA cache_spill;")
	fmt.Println("5.1:", r.Rows, r.Error)
	db.Close()
}
