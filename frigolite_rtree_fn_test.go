package frigolite

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"
)

// Native rtree SQL-function tests (t6 slice): rtreenode / rtreedepth /
// rtreecheck contracts locked against ext/rtree/rtree.c source semantics —
// full node images with a 4-byte header, unsigned int16 fields, C-%g
// coordinate rendering, and verbatim error messages.

func TestNativeRtreedepthContract(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(50)) // interior root proves live usage works

	cases := []struct {
		sql     string
		want    string // expected scalar; errPrefix non-empty → error case
		errPart string
	}{
		{"SELECT rtreedepth(x'0005')", "5", ""},
		{"SELECT rtreedepth(x'C800')", "51200", ""}, // unsigned read
		{"SELECT rtreedepth(zeroblob(2))", "0", ""},
		{"SELECT rtreedepth('hello world')", "", "Invalid argument to rtreedepth()"},
		{"SELECT rtreedepth(X'00')", "", "Invalid argument to rtreedepth()"},
	}
	for _, tc := range cases {
		res := db.Query(tc.sql)
		if tc.errPart != "" {
			if res.Error == nil || !strings.Contains(res.Error.Error(), tc.errPart) {
				t.Fatalf("%s: want err containing %q, got err=%v rows=%v",
					tc.sql, tc.errPart, res.Error, res.Rows)
			}
			continue
		}
		if res.Error != nil {
			t.Fatalf("%s: unexpected error %v", tc.sql, res.Error)
		}
		if got := renderScalar(res); got != tc.want {
			t.Fatalf("%s: want %s got %s", tc.sql, tc.want, got)
		}
	}
}

func TestNativeRtreenodeContract(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()

	// One leaf cell at depth 0 renders as "{rowid x1 x2}" via C %g rules.
	lit := "x'" + hexOfNode(0, cellSpec{rowid: 7, x1: 3.5, x2: -2}) + "'"
	res := db.Query("SELECT rtreenode(1, " + lit + ")")
	if res.Error != nil {
		t.Fatalf("rtreenode: %v", res.Error)
	}
	if got, want := renderScalar(res), "{7 3.5 -2}"; got != want {
		t.Fatalf("one-cell render:\n got %q\nwant %q", got, want)
	}

	// Two cells concatenate space-separated.
	lit2 := "x'" + hexOfNode(0, cellSpec{rowid: 1, x1: 100000, x2: 200000},
		cellSpec{rowid: 2, x1: 123456789, x2: 0.000000125}) + "'"
	res = db.Query("SELECT rtreenode(1, " + lit2 + ")")
	if res.Error != nil {
		t.Fatalf("rtreenode two cells: %v", res.Error)
	}
	want := "{1 100000 200000} {2 1.23457e+08 1.25e-07}"
	if got := renderScalar(res); got != want {
		t.Fatalf("two-cell %%g rendering:\n got %q\nwant %q", got, want)
	}

	// Contract guards → NULL results (no errors).
	nullCases := []string{
		"SELECT rtreenode(0, x'00000000') IS NULL",
		"SELECT rtreenode(6, x'00000000') IS NULL",
		"SELECT rtreenode(1, NULL) IS NULL",
		"SELECT rtreenode(1, x'0000') IS NULL",
		// Declares 255 cells but carries no room for them.
		"SELECT rtreenode(1, x'0000FF00') IS NULL",
	}
	for _, q := range nullCases {
		rows := queryIDs(t, db, q)
		if rows[0][0] != int64(1) {
			t.Fatalf("%s: expected TRUE (NULL result contract)", q)
		}
	}

	// Live-tree round trip: rtreenode over every scanned node blob renders
	// without error and known rowids appear in the text.
	insertRects(t, db, seedRects(40))
	all := queryIDs(t, db, "SELECT group_concat(txt, '|') FROM ("+
		"SELECT rtreenode(2, data) AS txt FROM rt_node)")
	if len(all) == 0 || all[0][0] == nil {
		t.Fatal("live render produced no output")
	}
	joined := all[0][0].(string)
	for _, id := range []int64{1, 20, 40} {
		if !strings.Contains(joined, "{"+itoa64(id)+" ") {
			t.Fatalf("live render missing rowid %d:\n%s", id, joined)
		}
	}
}

type cellSpec struct {
	rowid  int64
	x1, x2 float64
}

// hexOfNode builds a 1-D, n-cell node image and returns its hex encoding.
func hexOfNode(depth byte, specs ...cellSpec) string {
	blob := make([]byte, 4+len(specs)*16) // 8 rowid + 2 coords*4 bytes
	blob[0] = depth
	binary.BigEndian.PutUint16(blob[2:4], uint16(len(specs)))
	for i, s := range specs {
		off := 4 + i*16
		binary.BigEndian.PutUint64(blob[off:off+8], uint64(s.rowid))
		binary.BigEndian.PutUint32(blob[off+8:off+12], math.Float32bits(float32(s.x1)))
		binary.BigEndian.PutUint32(blob[off+12:off+16], math.Float32bits(float32(s.x2)))
	}
	return strings.ToUpper(hexEncode(blob))
}

func hexEncode(b []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, digits[v>>4], digits[v&0xF])
	}
	return string(out)
}

func renderScalar(r *Result) string {
	if r == nil || len(r.Rows) == 0 || r.Rows[0][0] == nil {
		return "<nil>"
	}
	return fmt.Sprint(r.Rows[0][0])
}
