package frigolite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNotADBOnWriteStatement pins misc5-4.1#01: opening a file with junk
// bytes succeeds (SQLite's open is lazy), but the first write statement
// reports "file is not a database", not "attempt to write a readonly
// database". C validates the header in lockBtree before applying the
// read-only open-mode write gate; a junk byte 18 > 1 made Frigolite mark
// the pager read-only and surface the wrong error first.
func TestNotADBOnWriteStatement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(path, []byte("This is not really a database\n"), 0644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open must stay lazy, got %v", err)
	}
	defer db.Close()

	for _, sql := range []string{
		"CREATE TABLE t1(a,b,c)",
		"INSERT INTO t1 VALUES(1,2,3)",
	} {
		r := db.Exec(sql)
		if r.Error == nil || !strings.Contains(r.Error.Error(), "file is not a database") {
			t.Fatalf("%q error = %v, want file is not a database", sql, r.Error)
		}
	}
}
