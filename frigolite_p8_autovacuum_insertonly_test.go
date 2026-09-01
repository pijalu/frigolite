// Pure-Go reproducer for autovacuum-1 integrity_check failure.
// Mirrors autovacuum_test.go lines 118-152: PRAGMA auto_vacuum=1, create
// table + index, insert 20 rows of large strings, integrity_check.
package frigolite_test

import (
	"os"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestP8AutovacuumInsertOnlyIntegrity(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := db.Exec("PRAGMA auto_vacuum = 1").Error; err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	if err := db.Exec("CREATE TABLE av1(a)").Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Exec("CREATE INDEX av1_idx ON av1(a)").Error; err != nil {
		t.Fatalf("create index: %v", err)
	}
	for i := 1; i <= 20; i++ {
		val := strings.Repeat(itoa(i)+".", 3500)
		val = val[:3500]
		if err := db.Exec("INSERT INTO av1 (oid, a) VALUES(" + itoa(i) + ", '" + val + "')").Error; err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check after inserts: %v", r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("integrity_check after inserts returned no rows")
	}
	if r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check after inserts FAIL: %v", r.Rows)
	}

	// Now delete a few rows to trigger autovacuum on commit.
	for _, del := range []int{1, 2} {
		if err := db.Exec("DELETE FROM av1 WHERE oid = " + itoa(del)).Error; err != nil {
			t.Fatalf("delete %d: %v", del, err)
		}
		// SELECT all to see what we have
		sel := db.Query("SELECT oid FROM av1")
		t.Logf("after delete %d: select rows=%d", del, len(sel.Rows))
		pc := db.Query("PRAGMA page_count")
		fc := db.Query("PRAGMA freelist_count")
		t.Logf("after delete %d: page_count=%v freelist_count=%v", del, pc.Rows, fc.Rows)
		// Dump all pages' first 4 bytes
		for p := 1; p <= 144; p++ {
			pg := db.Query("SELECT hex(substr(data, 1, 8)) FROM pragma_page_dump('" + itoa(p) + "')")
			if pg.Error == nil && len(pg.Rows) > 0 {
				t.Logf("page %d: %v", p, pg.Rows[0])
			}
		}
		r = db.Query("PRAGMA integrity_check")
		if r.Error != nil {
			t.Fatalf("integrity_check after delete %d: %v", del, r.Error)
		}
		if len(r.Rows) == 0 {
			t.Fatalf("integrity_check after delete %d returned no rows", del)
		}
		if r.Rows[0][0] != "ok" {
			t.Fatalf("integrity_check after delete %d FAIL: %v", del, r.Rows)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var s string
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
