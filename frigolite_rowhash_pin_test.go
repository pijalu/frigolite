package frigolite

import (
	"fmt"
	"sort"
	"testing"
)

// TestRowhashPin is the native port of rowhash.test's substance (the tcl2go
// transpiler drops the do_keyset_test proc and its calls, so the generated
// package only exercises the empty-loop scaffolding). The contract: with
// secondary indexes i1/i2/i3 on t1(a,b,c), an OR-of-equalities query across
// all three indexes must return exactly the rowids of the inserted rows
// (the union of the per-index rowid sets, no duplicates, no missing rows) —
// the file's regression guard for the removed rowhash.c duplicate-row bug.
func TestRowhashPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(id INTEGER PRIMARY KEY, a, b, c)",
		"CREATE INDEX i1 ON t1(a)",
		"CREATE INDEX i2 ON t1(b)",
		"CREATE INDEX i3 ON t1(c)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	keyset := func(keys []int64) {
		t.Helper()
		db.Exec("DELETE FROM t1")
		for _, k := range keys {
			if r := db.Exec(fmt.Sprintf("INSERT OR IGNORE INTO t1 VALUES(%d,'a','b','c')", k)); r.Error != nil {
				t.Fatalf("insert %d: %v", k, r.Error)
			}
		}
		r := db.Query("SELECT id FROM t1 WHERE a='a' OR b='b' OR c='c'")
		if r.Error != nil {
			t.Fatalf("query: %v", r.Error)
		}
		got := make([]int64, 0, len(r.Rows))
		for _, row := range r.Rows {
			id, ok := row[0].(int64)
			if !ok {
				t.Fatalf("rowid cell %v is not an integer", row[0])
			}
			got = append(got, id)
		}
		want := append([]int64(nil), keys...)
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("keyset %v => got %v", keys, got)
		}
	}
	keyset([]int64{1, 2, 3})
	keyset([]int64{0, 1, 2, 3})
	keyset([]int64{62, 125, 188})
	// Deterministic pseudo-random keysets in the spirit of rowhash-2.4..2.9.
	seed := uint64(1)
	for i := 4; i < 10; i++ {
		var keys []int64
		for j := 0; j < 200; j++ {
			seed = seed*6364136223846793005 + 1442695040888963407
			keys = append(keys, int64(seed%1000000000))
		}
		keyset(keys)
	}
}
