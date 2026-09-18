package frigolite

// Pins for the FULL-SUITE-DRIFT.T26-fts34 engine fixes. Each test drives the
// public API against behavior verified with the sqlite3 oracle.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/fts"
)

// TestPinFTS3MatchParserPhraseGroups pins the MATCH parser's handling of
// parenthesized groups whose contents open with a quoted phrase: a grouping
// paren directly after a closing quote is a group close, not tokenizer
// content (e_fts3 4.1: '("sqlite database" OR "sqlite library") AND linux').
func TestPinFTS3MatchParserPhraseGroups(t *testing.T) {
	node, err := fts.ParseMatchQuery(`("sqlite database" OR "sqlite library") AND linux`)
	if err != nil {
		t.Fatalf("parenthesized phrase OR group must parse: %v", err)
	}
	if node == nil || !strings.Contains(node.String(), "AND") {
		t.Fatalf("expected an AND of groups, got %v", node)
	}
}

// TestPinFTS3EmptyMatchQueryMatchesNothing pins the empty-MATCH contract:
// MATCH '' (and whitespace-only queries) matches zero rows without error —
// C's fts3ExprParse exhausts the empty input and the cursor sits at EOF
// (fts3expr5 1.0, e_fts3 6.5/6.6).
func TestPinFTS3EmptyMatchQueryMatchesNothing(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE VIRTUAL TABLE t6 USING fts3(x)",
		"INSERT INTO t6 VALUES('one two three')",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	for _, q := range []string{
		"SELECT * FROM t6 WHERE t6 MATCH ''",
		"SELECT * FROM t6 WHERE x MATCH ''",
	} {
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("%s: %v", q, r.Error)
		}
		if len(r.Rows) != 0 {
			t.Fatalf("%s: expected 0 rows, got %v", q, r.Rows)
		}
	}
}

// TestPinFTS3SnippetZeroArgContextError pins the zero-argument snippet()
// diagnostic: fts3.c fts3SnippetFunc checks argc<1 before it can identify a
// table argument, reporting the context error rather than an arity error
// (e_fts3 2.1.7; oracle-verified).
func TestPinFTS3SnippetZeroArgContextError(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE VIRTUAL TABLE t1 USING fts3(a, b)",
		"INSERT INTO t1 VALUES('one two three', 'x')",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query("SELECT snippet() FROM t1 WHERE a MATCH 'one'")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "unable to use function snippet in the requested context") {
		t.Fatalf("expected context error, got %v", r.Error)
	}
}

// TestPinFTS3VarintNineByteForm round-trips the FTS3 varint codec's 9-byte
// form: values >= 2^56 (including negative docid deltas) carry bits [56,64)
// in their 9th byte (fts3.c fts3GetVarint64 / fts3PutVarint), and an
// over-long continuation run decodes as a 9-byte varint plus a fresh varint
// (fts3corrupt3 1.3: a doclist of FF*12 02 00 reads as three varints).
func TestPinFTS3VarintNineByteForm(t *testing.T) {
	// -1 as u64 encodes to 9 bytes of 0xFF and decodes back to -1.
	var buf []byte
	buf = fts.AppendFTS3Varint(buf, ^uint64(0))
	if len(buf) != 9 {
		t.Fatalf("u64 -1 must encode as 9 bytes, got %d (% x)", len(buf), buf)
	}
	got, n := fts.GetFTS3Varint(buf)
	if n != 9 || int64(got) != -1 {
		t.Fatalf("decode(-1) = %d, consumed %d; want -1, 9", int64(got), n)
	}
	// FF*12 02 00: a 9-byte varint (all-ones), then varint 2, then 0.
	tail := []byte{
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // 9 bytes
		0xFF, 0xFF, 0xFF, // continuation run: byte 9 of the next varint
		0x02, 0x00,
	}
	v1, n1 := fts.GetFTS3Varint(tail)
	v2, n2 := fts.GetFTS3Varint(tail[n1:])
	v3, n3 := fts.GetFTS3Varint(tail[n1+n2:])
	if n1 != 9 || int64(v1) != -1 || v2 != 0x5FFFFF || n2 != 4 || v3 != 0 || n3 != 1 {
		t.Fatalf("FF-run decode: v1=%d n1=%d v2=%d n2=%d v3=%d n3=%d", int64(v1), n1, v2, n2, v3, n3)
	}
}

// TestPinFTS3DuplicateMatchRejected pins that a second MATCH constraint on
// the same FTS table in one query is rejected at prepare (fts3.c xBestIndex
// binds one MATCH per table; e_fts3 7.3.1).
func TestPinFTS3DuplicateMatchRejected(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE VIRTUAL TABLE t7 USING fts3(a, b)",
		"INSERT INTO t7 VALUES('number four', 'x')",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query("SELECT * FROM t7 WHERE a MATCH 'number' AND a MATCH 'four'")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "unable to use function MATCH in the requested context") {
		t.Fatalf("expected duplicate-MATCH rejection, got %v", r.Error)
	}
}

// TestPinFTS3JoinRightOperand pins that an FTS3/4 table as a JOIN's right
// operand materializes its documents (fts3join 2.1: FROM ft2, ft3 WHERE
// x MATCH y must not report "no such table" for the right table).
func TestPinFTS3JoinRightOperand(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE VIRTUAL TABLE ft2 USING fts4(x)",
		"CREATE VIRTUAL TABLE ft3 USING fts4(y)",
		"INSERT INTO ft2 VALUES('abc')",
		"INSERT INTO ft2 VALUES('def')",
		"INSERT INTO ft3 VALUES('ghi')",
		"INSERT INTO ft3 VALUES('abc')",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	r := db.Query(" SELECT * FROM ft2, ft3 WHERE x MATCH y ")
	if r.Error != nil {
		t.Fatalf("join scan: %v", r.Error)
	}
	if got := flattenRowsP2(r.Rows); got != "abc abc" {
		t.Fatalf("got %q, want abc abc", got)
	}
}

// flattenRowsP2 renders query rows space-joined (test harness parity).
func flattenRowsP2(rows [][]interface{}) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, 0, len(row))
		for _, c := range row {
			cells = append(cells, fmt.Sprintf("%v", c))
		}
		parts = append(parts, strings.Join(cells, " "))
	}
	return strings.Join(parts, " ")
}
