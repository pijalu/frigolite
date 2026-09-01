// Pure-Go reproducer for autovacuum-1 integrity_check failure.
// Uses an in-test BTree inspection to see what's actually in
// the parent after a delete that passes integrity_check.
package frigolite_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestP8AutovacuumDumpState(t *testing.T) {
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
		val := strings.Repeat(itoaDump(i)+".", 3500)
		val = val[:3500]
		if err := db.Exec("INSERT INTO av1 (oid, a) VALUES(" + itoaDump(i) + ", '" + val + "')").Error; err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Dump schema to get rootpages
	sch := db.Query("SELECT name, rootpage FROM sqlite_master")
	t.Logf("schema: %v", sch.Rows)
	for _, row := range sch.Rows {
		if name, ok := row[0].(string); ok {
			if rp, ok := row[1].(int64); ok {
				// Try to peek at the page header
				pg := db.Query(fmt.Sprintf("SELECT * FROM pragma_page_info(%d)", rp))
				t.Logf("page %d (%s) info: %v", rp, name, pg.Rows)
			}
		}
	}

	if err := db.Exec("DELETE FROM av1 WHERE oid = 1").Error; err != nil {
		t.Fatalf("delete 1: %v", err)
	}
	sel := db.Query("SELECT oid FROM av1")
	t.Logf("after delete 1: select rows=%d", len(sel.Rows))

	// Try a few more deletes to see when things break
	for _, del := range []int{2, 3} {
		if err := db.Exec("DELETE FROM av1 WHERE oid = " + itoaDump(del)).Error; err != nil {
			t.Fatalf("delete %d: %v", del, err)
		}
		sel := db.Query("SELECT oid FROM av1")
		t.Logf("after delete %d: select rows=%d", del, len(sel.Rows))
		r := db.Query("PRAGMA integrity_check")
		if r.Error != nil {
			t.Logf("integrity_check after delete %d FAIL: %v", del, r.Rows)
		} else if len(r.Rows) > 0 && r.Rows[0][0] != "ok" {
			t.Logf("integrity_check after delete %d FAIL: %v", del, r.Rows)
		} else {
			t.Logf("integrity_check after delete %d: ok", del)
		}
	}
}

var _ = itoaDump
func itoaDump(i int) string {
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
