// Pure-Go reproducer for autovacuum-1 integrity_check failure.
// Mirrors autovacuum_test.go lines 118-152: PRAGMA auto_vacuum=1, create
// table + index, insert 20 rows of large strings, integrity_check.
package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestP8AutovacuumInsertOnlyIntegrity(t *testing.T) {
	db, err := frigolite.Open(":memory:")
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
		val := strings.Repeat("0.", 3500) // 7000 bytes per row
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
	if err := db.Exec("DELETE FROM av1 WHERE oid = 1").Error; err != nil {
		t.Fatalf("delete 1: %v", err)
	}
	r = db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check after delete: %v", r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("integrity_check after delete returned no rows")
	}
	if r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check after delete FAIL: %v", r.Rows)
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
