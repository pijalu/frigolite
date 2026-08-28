//go:build scratch

package frigolite_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestScratchBTreeStress(t *testing.T) {
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
	verifyAll := func(live map[int]bool, phase string) {
		for id := range live {
			r := db.Query(fmt.Sprintf("SELECT blockid FROM seg WHERE blockid=%d", id))
			if r.Error != nil || len(r.Rows) == 0 {
				t.Fatalf("%s: rowid %d MISSING", phase, id)
			}
		}
	}
	live := map[int]bool{}
	next := 1
	// Phase A: grow with periodic range deletes (like FTS block lifecycle).
	for round := 0; round < 40; round++ {
		for i := 0; i < 35; i++ {
			insBlob(next, 50+(next*7)%900)
			live[next] = true
			next++
		}
		// delete a contiguous range (mimics deleteFTSBlocksRangeWithMarker)
		lo, hi := next-30, next-10
		if r := db.Exec(fmt.Sprintf("DELETE FROM seg WHERE blockid BETWEEN %d AND %d", lo, hi)); r.Error != nil {
			t.Fatal(r.Error)
		}
		for id := lo; id <= hi; id++ {
			delete(live, id)
		}
		verifyAll(live, fmt.Sprintf("round %d", round))
	}
	fmt.Println("stress OK, live:", len(live), "next:", next)
}
