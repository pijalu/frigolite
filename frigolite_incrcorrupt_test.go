package frigolite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestIncrCorruptPageCountParity mirrors incrcorrupt-1.0/2.1: with
// auto_vacuum = 1 or 2 (FULL/INCREMENTAL), a table with 20 rows of ~600-byte
// random blobs plus a PRIMARY KEY autoindex occupies 24 pages at page_size
// 1024 (SQLite: header 1, ptrmap 2, table root 3, autoindex root 4, 20
// single-row leaves 5-24). The engine must allocate ptrmap pages and a
// physical autoindex root to match (btree.c allocateBtreePage auto-vacuum
// branch + createAutoIndexes).
func TestIncrCorruptPageCountParity(t *testing.T) {
	for _, mode := range []string{"1", "2"} {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "test.db")
		db, err := Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		r := db.Query("PRAGMA page_size=1024; PRAGMA auto_vacuum=" + mode + "; CREATE TABLE t1(a PRIMARY KEY, b); WITH data(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM data) INSERT INTO t1 SELECT i, randomblob(600) FROM data LIMIT 20; PRAGMA page_count;")
		if r.Error != nil {
			db.Close()
			t.Fatalf("mode %s: setup failed: %v", mode, r.Error)
		}
		got := flattenTestRows(r.Rows)
		db.Close()
		if got != "24" {
			t.Errorf("mode %s: page_count got %s want 24", mode, got)
		}
	}
}

// TestIncrCorruptAutoVacuumModeMapping checks the numeric/string mapping of
// PRAGMA auto_vacuum against SQLite's getAutoVacuum (pragma.c): none=0,
// full=1, incremental=2 — the engine previously mapped full=2/incremental=1.
func TestIncrCorruptAutoVacuumModeMapping(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cases := []struct {
		set   string
		want  string
	}{
		{"PRAGMA auto_vacuum=NONE", "0"},
		{"PRAGMA auto_vacuum=FULL", "1"},
		{"PRAGMA auto_vacuum=INCREMENTAL", "2"},
	}
	for _, tc := range cases {
		if r := db.Exec(tc.set); r.Error != nil {
			t.Fatalf("%s: %v", tc.set, r.Error)
		}
		r := db.Query("PRAGMA auto_vacuum;")
		if r.Error != nil {
			t.Fatalf("read back: %v", r.Error)
		}
		if got := flattenTestRows(r.Rows); got != tc.want {
			t.Errorf("%s: auto_vacuum got %s want %s", tc.set, got, tc.want)
		}
	}
}

// TestIncrCorruptTruncatedFile mirrors incrcorrupt-2.2: truncating the
// database file below the header page count (header @28 says 24 pages, file
// holds 22) makes the next statement fail with SQLITE_CORRUPT / "database
// disk image is malformed" (btree.c lockBtree nPage>nPageFile check via
// pagerPagecount on every statement start).
func TestIncrCorruptTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	r := db.Query("PRAGMA page_size=1024; PRAGMA auto_vacuum=1; CREATE TABLE t1(a PRIMARY KEY, b); WITH data(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM data) INSERT INTO t1 SELECT i, randomblob(600) FROM data LIMIT 20; PRAGMA page_count;")
	if r.Error != nil || flattenTestRows(r.Rows) != "24" {
		db.Close()
		t.Skipf("setup not at parity yet: %v %v", r.Error, r.Rows)
	}
	db.Close()

	// Truncate the file to 22 pages under a fresh connection, then run a
	// statement: the header's page count (24) exceeds the file's pages.
	if err := os.Truncate(dbPath, 22*1024); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	res := db2.Exec("PRAGMA incremental_vacuum")
	if res.Error == nil || res.Error.Error() != "database disk image is malformed" {
		t.Errorf("incremental_vacuum after truncate: got %v want database disk image is malformed", res.Error)
	}
	if code := db2.LastErrCode(); code != "SQLITE_CORRUPT" {
		t.Errorf("LastErrCode got %s want SQLITE_CORRUPT", code)
	}
}

// TestIncrCorruptFreelistCount mirrors incrcorrupt-1.2: patching the
// freelist page count (header offset 36) beyond the database size makes
// PRAGMA incremental_vacuum fail with SQLITE_CORRUPT (btree.c
// sqlite3BtreeIncrVacuum: nFree >= nOrig → SQLITE_CORRUPT_BKPT).
func TestIncrCorruptFreelistCount(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	r := db.Query("PRAGMA page_size=1024; PRAGMA auto_vacuum=2; CREATE TABLE t1(a PRIMARY KEY, b); WITH data(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM data) INSERT INTO t1 SELECT i, randomblob(600) FROM data LIMIT 20; PRAGMA page_count;")
	if r.Error != nil || flattenTestRows(r.Rows) != "24" {
		db.Close()
		t.Skipf("setup not at parity yet: %v %v", r.Error, r.Rows)
	}
	db.Close()

	// hexio_write test.db 36 00000019: freelist count = 25 > 24 pages.
	if err := writeHexAt(dbPath, 36, "00000019"); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	res := db2.Exec("PRAGMA incremental_vacuum")
	if res.Error == nil || res.Error.Error() != "database disk image is malformed" {
		t.Errorf("incremental_vacuum with oversized freelist count: got %v want database disk image is malformed", res.Error)
	}
	if code := db2.LastErrCode(); code != "SQLITE_CORRUPT" {
		t.Errorf("LastErrCode got %s want SQLITE_CORRUPT", code)
	}
}

// TestIncrVacuumNoopOnEmptyFreelist mirrors incrcorrupt-1.1: on a healthy
// incremental-vacuum database with an empty freelist, PRAGMA
// incremental_vacuum is a no-op returning no rows and no error.
func TestIncrVacuumNoopOnEmptyFreelist(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query("PRAGMA auto_vacuum=2; CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1, 'x');")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	res := db.Query("PRAGMA incremental_vacuum;")
	if res.Error != nil {
		t.Fatalf("incremental_vacuum no-op: %v", res.Error)
	}
	if len(res.Rows) != 0 {
		t.Errorf("incremental_vacuum no-op returned rows: %v", res.Rows)
	}
}

// flattenTestRows renders single-cell query results as a string.
func flattenTestRows(rows [][]interface{}) string {
	if len(rows) == 0 {
		return ""
	}
	if len(rows[0]) == 0 {
		return ""
	}
	return fmt.Sprint(rows[0][0])
}

// writeHexAt patches a file with hex-decoded bytes at a byte offset (the
// test framework's hexio_write).
func writeHexAt(path string, offset int64, hexStr string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	var data []byte
	for i := 0; i+1 < len(hexStr); i += 2 {
		hi := hexVal(hexStr[i])
		lo := hexVal(hexStr[i+1])
		if hi < 0 || lo < 0 {
			return os.ErrInvalid
		}
		data = append(data, byte(hi<<4|lo))
	}
	_, err = f.WriteAt(data, offset)
	return err
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
