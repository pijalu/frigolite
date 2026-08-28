//go:build scratch

package frigolite_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func runMin(t *testing.T, batch, dLo, dHi, blobMax, rounds int) string {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE seg(blockid INTEGER PRIMARY KEY, block BLOB)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	live := map[int]bool{}
	next := 1
	for round := 0; round < rounds; round++ {
		for i := 0; i < batch; i++ {
			n := 50 + (next*7)%blobMax
			if r := db.Exec(fmt.Sprintf("INSERT INTO seg(blockid, block) VALUES(%d, '%s')", next, strings.Repeat("x", n))); r.Error != nil {
				t.Fatalf("insert %d: %v", next, r.Error)
			}
			live[next] = true
			next++
		}
		lo, hi := next-dLo, next-dHi
		if r := db.Exec(fmt.Sprintf("DELETE FROM seg WHERE blockid BETWEEN %d AND %d", lo, hi)); r.Error != nil {
			t.Fatal(r.Error)
		}
		for id := lo; id <= hi; id++ {
			delete(live, id)
		}
		r := db.Query("SELECT blockid FROM seg ORDER BY blockid")
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		scan := map[int]bool{}
		for _, row := range r.Rows {
			scan[int(row[0].(int64))] = true
		}
		for id := range live {
			if !scan[id] {
				return fmt.Sprintf("round %d: lost %d", round, id)
			}
		}
	}
	return ""
}

func TestScratchBTreeMin2(t *testing.T) {
	cases := []struct{ batch, dLo, dHi, blobMax, rounds int }{
		{35, 30, 10, 900, 40},
		{35, 30, 10, 100, 40},
		{10, 8, 3, 900, 60},
		{10, 8, 3, 100, 60},
		{5, 4, 2, 900, 100},
		{5, 4, 2, 50, 100},
		{3, 2, 1, 50, 200},
		{2, 1, 1, 50, 300},
	}
	for _, c := range cases {
		msg := ""
		func() {
			defer func() { recover() }()
			msg = runMin(t, c.batch, c.dLo, c.dHi, c.blobMax, c.rounds)
		}()
		fmt.Printf("case %+v: %s\n", c, msg)
	}
	t.Skip("report-only")
}
