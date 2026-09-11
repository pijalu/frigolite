package main

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/pijalu/frigolite"
)

func main() {
	os.Remove("/tmp/t10.db")
	db, _ := frigolite.Open("/tmp/t10.db")
	defer db.Close()
	db.Exec("CREATE TABLE t1(b UNIQUE, a INT PRIMARY KEY) WITHOUT ROWID;\nINSERT INTO t1(a) VALUES('1');")
	data, _ := os.ReadFile("/tmp/t10.db")
	// find the first index leaf (0x0a) page
	for p := 1; p*1024 <= len(data); p++ {
		pg := data[(p-1)*1024 : p*1024]
		if pg[100] == 0x0a || pg[0] == 0x0a {
			off := 0
			if p == 1 {
				off = 100
			}
			ncells := int(pg[off+2])<<8 | int(pg[off+3])
			cellOff := int(pg[off+8])<<8 | int(pg[off+9])
			fmt.Printf("page %d: leaf-index cells=%d firstCell@%d\n", p, ncells, cellOff)
			n := 40
			if cellOff+n > 1024 {
				n = 1024 - cellOff
			}
			fmt.Println(hex.Dump(pg[cellOff : cellOff+n]))
		}
	}
}
