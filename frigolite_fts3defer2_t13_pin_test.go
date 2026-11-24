package frigolite

import (
	"encoding/binary"
	"strings"
	"testing"
)

// TestFTS3DeferredMatchinfoPins locks C's deferred-token matchinfo rule on a
// normal FTS4 table: an overflow-sized token in an AND cluster reports nDoc
// when it is the whole phrase, and its loaded partner's counts when the
// phrase mixes deferred and non-deferred tokens.
func TestFTS3DeferredMatchinfoPins(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	exec(`CREATE VIRTUAL TABLE t2 USING fts4`)
	exec(`INSERT INTO t2 VALUES('` + strings.Repeat("a ", 10000) + `')`)
	exec(`INSERT INTO t2 VALUES('b ` + strings.Repeat("z ", 10000) + `')`)
	for i := 0; i < 50; i++ {
		exec(`INSERT INTO t2 VALUES('x')`)
	}
	exec(`INSERT INTO t2 VALUES('a b c d e f g z')`)
	exec(`INSERT INTO t2 VALUES('a b c d e f g')`)

	uint32s := func(blob []byte) []uint32 {
		out := make([]uint32, 0, len(blob)/4)
		for i := 0; i+4 <= len(blob); i += 4 {
			out = append(out, binary.LittleEndian.Uint32(blob[i:i+4]))
		}
		return out
	}
	firstMatchinfo := func(sql string) []uint32 {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		if len(r.Rows) == 0 {
			t.Fatalf("%s: no rows", sql)
		}
		blob, ok := r.Rows[0][0].([]byte)
		if !ok {
			t.Fatalf("%s: matchinfo is %T, want blob", sql, r.Rows[0][0])
		}
		return uint32s(blob)
	}

	// 'a' is all-deferred; 'b' is loaded. p=2 c=1, then x for a=[1,54,54]
	// and b=[1,3,3], followed by n=54, a=372 and the row length.
	got := firstMatchinfo(`SELECT matchinfo(t2, 'pcxnal') FROM t2 WHERE t2 MATCH 'a b'`)
	want := []uint32{2, 1, 1, 54, 54, 1, 3, 3}
	if len(got) < len(want) {
		t.Fatalf("pcxnal matchinfo = %v, want prefix %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pcxnal matchinfo = %v, want prefix %v", got, want)
		}
	}

	// The phrase "g z" mixes a loaded token with deferred z; C counts its
	// potential occurrences through g alone (2 occurrences in 2 rows).
	got = firstMatchinfo(`SELECT matchinfo(t2, 'x') FROM t2 WHERE t2 MATCH 'e "g z"'`)
	want = []uint32{1, 2, 2, 1, 2, 2}
	if len(got) != len(want) {
		t.Fatalf("partial-defer matchinfo = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("partial-defer matchinfo = %v, want %v", got, want)
		}
	}
}
