//go:build scratch

package frigolite_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestScratchBTreeMin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE seg(blockid INTEGER PRIMARY KEY, block BLOB)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	insBlob := func(id int, n int) {
		if r := db.Exec(fmt.Sprintf("INSERT INTO seg(blockid, block) VALUES(%d, '%s')", id, strings.Repeat("x", n))); r.Error != nil {
			t.Fatalf("insert %d: %v", id, r.Error)
		}
	}
	live := map[int]bool{}
	next := 1
	for round := 0; round < 40; round++ {
		for i := 0; i < 35; i++ {
			insBlob(next, 50+(next*7)%900)
			live[next] = true
			next++
		}
		lo, hi := next-30, next-10
		if r := db.Exec(fmt.Sprintf("DELETE FROM seg WHERE blockid BETWEEN %d AND %d", lo, hi)); r.Error != nil {
			t.Fatal(r.Error)
		}
		for id := lo; id <= hi; id++ {
			delete(live, id)
		}
		// Ordered scan to see whether rowid is physically present.
		r := db.Query("SELECT blockid FROM seg ORDER BY blockid")
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		scan := map[int]bool{}
		for _, row := range r.Rows {
			id := int(row[0].(int64))
			scan[id] = true
		}
		for id := range live {
			if !scan[id] {
				t.Fatalf("round %d: rowid %d missing from ORDERED SCAN (physically absent)", round, id)
			}
		}
		// Point lookups via engine (uses btree seek).
		for id := range live {
			r2 := db.Query(fmt.Sprintf("SELECT blockid FROM seg WHERE blockid=%d", id))
			if r2.Error != nil || len(r2.Rows) == 0 {
				t.Fatalf("round %d: point lookup MISSING rowid %d (scan sees it? %v)", round, id, scan[id])
			}
		}
	}
	fmt.Println("min OK")
}
