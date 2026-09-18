package frigolite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorruptBAllocateRootRelocationPin pins corruptB-3.1.1's engine
// contract: on a PRISTINE auto_vacuum database whose btree has grown
// past a pointer-map page boundary, CREATE TABLE must allocate the new
// root (relocating the occupant of the ptrmap slot) WITHOUT reading a
// stale PTRMAP_BTREE parent. The pre-fix failure was
//
//	"AllocateRootPage: relocate occupant 4 -> 1042: ... parent 3 does
//	 not reference child 4"
//
// — relocateRootSplit's segment rotation moved an interior root's
// children wholesale to a new child page without re-pointing their
// pointer-map entries (btree.c balance_deeper ptrmapPut,
// src/btree.c:9028; fixed via setChildPtrmaps). Recipe mirrors
// test/corruptB.test: auto_vacuum=1, t1 grown by 12 doubling inserts
// of randomblob(200), reopen, then CREATE TABLE t2 + a 2000-byte row.
func TestCorruptBAllocateRootRelocationPin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "corruptb311.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	grow := strings.Repeat("INSERT INTO t1 SELECT randomblob(200) FROM t1;\n", 12)
	if res := db.Query("PRAGMA auto_vacuum = 1;\nCREATE TABLE t1(x);\nINSERT INTO t1 VALUES(randomblob(200));\n" + grow); res.Error != nil {
		t.Fatalf("setup: %v", res.Error)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t2(a)"); res.Error != nil {
		t.Fatalf("CREATE TABLE t2: %v", res.Error)
	}
	v := strings.Repeat("abcdefghij", 200)
	if res := db.Exec(fmt.Sprintf("INSERT INTO t2 VALUES('%s')", v)); res.Error != nil {
		t.Fatalf("INSERT t2: %v", res.Error)
	}
	res := db.Query("PRAGMA integrity_check")
	if res.Error != nil {
		t.Fatalf("integrity_check: %v", res.Error)
	}
	if len(res.Rows) != 1 || fmt.Sprintf("%v", res.Rows[0][0]) != "ok" {
		t.Fatalf("integrity_check:\n%v", res.Rows)
	}
}
