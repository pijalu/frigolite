package frigolite

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/vtab"
)

// TestFTS3DeferredMatchAfterSegmentCorruption pins the engine-visible contract
// behind fts3defer2 2.2.3: overflow-sized tokens are evaluated by rescanning
// the stored row content after their segment blocks have been overwritten.
func TestFTS3DeferredMatchAfterSegmentCorruption(t *testing.T) {
	vtab.TclVarSet("sqlite_fts3_enable_parentheses", "", "1")
	defer vtab.TclVarSet("sqlite_fts3_enable_parentheses", "", "")
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
	exec(`INSERT INTO t2 VALUES('` + strings.Repeat("a ", 2000) + `')`)
	exec(`INSERT INTO t2 VALUES('b ` + strings.Repeat("z ", 2000) + `')`)
	for i := 0; i < 50; i++ {
		exec(`INSERT INTO t2 VALUES('x')`)
	}
	exec(`INSERT INTO t2 VALUES('a b c d e f g z')`)
	exec(`INSERT INTO t2 VALUES('a b c d e f g')`)
	exec(`UPDATE t2_segments SET block = zeroblob(length(block)) WHERE length(block)>10000`)

	query := func(sql string) [][]interface{} {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		return r.Rows
	}
	uint32s := func(v interface{}) []uint32 {
		t.Helper()
		blob, ok := v.([]byte)
		if !ok {
			t.Fatalf("matchinfo is %T, want blob", v)
		}
		out := make([]uint32, 0, len(blob)/4)
		for i := 0; i+4 <= len(blob); i += 4 {
			out = append(out, binary.LittleEndian.Uint32(blob[i:i+4]))
		}
		return out
	}
	wantPrefix := func(got, want []uint32) {
		t.Helper()
		if len(got) < len(want) {
			t.Fatalf("matchinfo = %v, want prefix %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("matchinfo = %v, want prefix %v", got, want)
			}
		}
	}

	rows := query(`SELECT matchinfo(t2, 'x') FROM t2 WHERE t2 MATCH 'g z'`)
	if len(rows) != 1 {
		t.Fatalf("MATCH 'g z' rows = %v, want one row", rows)
	}
	wantPrefix(uint32s(rows[0][0]), []uint32{1, 2, 2, 1, 54, 54})

	rows = query(`SELECT matchinfo(t2, 'x') FROM t2 WHERE t2 MATCH 'g OR (g z)'`)
	if len(rows) != 2 {
		t.Fatalf("MATCH 'g OR (g z)' rows = %v, want two rows", rows)
	}
	wantPrefix(uint32s(rows[0][0]), []uint32{1, 2, 2, 1, 2, 2, 1, 54, 54})
	wantPrefix(uint32s(rows[1][0]), []uint32{1, 2, 2, 1, 2, 2, 0, 54, 54})
}

// TestFTS3DeferredPartialStatsAfterSegmentCorruption pins the t3 case: a
// deferred first token makes the global phrase stats count the loaded "b c"
// suffix from stored content, not the damaged "a b c" postings.
func TestFTS3DeferredPartialStatsAfterSegmentCorruption(t *testing.T) {
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
	exec(`CREATE VIRTUAL TABLE t3 USING fts4`)
	for _, text := range []string{
		"a b c d e f", "x b c d e f", "d e f a b c", "b c d e f",
	} {
		exec(`INSERT INTO t3 VALUES('` + text + `')`)
	}
	for i := 0; i < 6; i++ {
		exec(`INSERT INTO t3 VALUES('')`)
	}
	exec(`INSERT INTO t3 VALUES('f e d c b a ` + strings.Repeat("a ", 10000) + `')`)
	exec(`UPDATE t3_segments SET block = zeroblob(length(block)) WHERE length(block)>10000`)

	r := db.Query(`SELECT docid, matchinfo(t3, 'pcxnal') FROM t3 WHERE t3 MATCH '"a b c"'`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	wantRows := []struct {
		docid int64
		vals  []uint32
	}{
		{1, []uint32{1, 1, 1, 4, 4, 11, 912, 6}},
		{3, []uint32{1, 1, 1, 4, 4, 11, 912, 6}},
	}
	if len(r.Rows) != len(wantRows) {
		t.Fatalf("rows = %v, want %d rows", r.Rows, len(wantRows))
	}
	for i, want := range wantRows {
		if r.Rows[i][0] != want.docid {
			t.Fatalf("row %d docid = %v, want %d", i, r.Rows[i][0], want.docid)
		}
		blob, ok := r.Rows[i][1].([]byte)
		if !ok {
			t.Fatalf("row %d matchinfo is %T, want blob", i, r.Rows[i][1])
		}
		got := make([]uint32, 0, len(blob)/4)
		for j := 0; j+4 <= len(blob); j += 4 {
			got = append(got, binary.LittleEndian.Uint32(blob[j:j+4]))
		}
		if len(got) < len(want.vals) {
			t.Fatalf("row %d matchinfo = %v, want prefix %v", i, got, want.vals)
		}
		for j := range want.vals {
			if got[j] != want.vals[j] {
				t.Fatalf("row %d matchinfo = %v, want prefix %v", i, got, want.vals)
			}
		}
	}
}
