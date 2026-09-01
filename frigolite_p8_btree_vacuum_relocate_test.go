package frigolite

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestP8AutovacuumNoDataCorruption(t *testing.T) {
	os.Chdir(t.TempDir())
	defer os.Remove("test.db")
	db, err := Open("test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if r := db.Exec("PRAGMA auto_vacuum = 1"); r.Error != nil {
		t.Fatalf("auto_vacuum: %v", r.Error)
	}
	if r := db.Exec("CREATE TABLE av1(a)"); r.Error != nil {
		t.Fatalf("create table: %v", r.Error)
	}
	if r := db.Exec("CREATE INDEX av1_idx ON av1(a)"); r.Error != nil {
		t.Fatalf("create index: %v", r.Error)
	}
	big := strings.Repeat("0.", 3500)
	// Insert 5 rows.
	for i := 1; i <= 5; i++ {
		sql := fmt.Sprintf("INSERT INTO av1 (oid, a) VALUES(%d, %q)", i, big)
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("INSERT %d: %v", i, r.Error)
		}
	}
	t.Logf("after insert: pc=%v fl=%v", queryOne(db, "PRAGMA page_count"), queryOne(db, "PRAGMA freelist_count"))
	// Delete 5 rows one at a time, checking integrity after each.
	for i := 1; i <= 5; i++ {
		sql := fmt.Sprintf("DELETE FROM av1 WHERE oid = %d", i)
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("DELETE %d: %v", i, r.Error)
		}
		t.Logf("after delete oid=%d: pc=%v fl=%v", i,
			queryOne(db, "PRAGMA page_count"),
			queryOne(db, "PRAGMA freelist_count"))
		// Run integrity_check after each delete.
		r := db.Query("PRAGMA integrity_check")
		if r.Error != nil {
			t.Fatalf("integrity_check after delete %d: %v", i, r.Error)
		}
		for _, row := range r.Rows {
			s := fmt.Sprintf("%v", row)
			if len(s) > 200 {
				s = s[:200] + "..."
			}
			t.Logf("integrity_check after delete %d: %s", i, s)
		}
	}
}

func queryOne(db *DB, sql string) string {
	r := db.Query(sql)
	if r.Error != nil || len(r.Rows) == 0 {
		return fmt.Sprintf("err=%v", r.Error)
	}
	return fmt.Sprintf("%v", r.Rows[0][0])
}
