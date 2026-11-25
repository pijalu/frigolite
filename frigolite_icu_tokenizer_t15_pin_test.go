package frigolite

import (
	"testing"

	"github.com/pijalu/frigolite/internal/fts"
)

// TestICUTokenizerText pins the ICU word-boundary contract behind
// fts3atoken-3.3: case folding, mid-word apostrophes and source byte spans.
func TestICUTokenizerText(t *testing.T) {
	tok, err := fts.NewTokenizerFromSpec("icu en_US", nil)
	if err != nil {
		t.Fatal(err)
	}
	icu, ok := tok.(*fts.ICUTokenizer)
	if !ok {
		t.Fatalf("tokenizer = %T, want *fts.ICUTokenizer", tok)
	}
	if got := icu.Locale(); got != "en_US" {
		t.Fatalf("locale = %q, want en_US", got)
	}
	got := tok.Tokenize("I don't see how")
	want := []string{"i", "don't", "see", "how"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Term != want[i] || got[i].Position != i {
			t.Fatalf("token %d = %+v, want %q at %d", i, got[i], want[i], i)
		}
	}
	spans := tok.TokenizeOffsets("I don't see how")
	wantSpans := []fts.OffsetToken{
		{Term: "i", Start: 0, End: 1},
		{Term: "don't", Start: 2, End: 7},
		{Term: "see", Start: 8, End: 11},
		{Term: "how", Start: 12, End: 15},
	}
	if len(spans) != len(wantSpans) {
		t.Fatalf("spans = %v, want %v", spans, wantSpans)
	}
	for i := range wantSpans {
		if spans[i] != wantSpans[i] {
			t.Fatalf("span %d = %+v, want %+v", i, spans[i], wantSpans[i])
		}
	}
}

// TestICUTokenizerFTS4Defer pins fts3defer-5.2.2: an ICU-tokenized FTS4 table
// with an overflow-sized "b" doclist still matches 'a b' only on the row that
// contains both tokens.
func TestICUTokenizerFTS4Defer(t *testing.T) {
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
	exec(`CREATE VIRTUAL TABLE x3 USING FTS4(a, b, TOKENIZE icu en_US)`)
	exec(`INSERT INTO x3 VALUES('b b b b b b b b b b b', 'b b b b b b b b b b b b b')`)
	for i := 0; i < 12; i++ {
		exec(`INSERT INTO x3 SELECT * FROM x3`)
	}
	exec(`INSERT INTO x3 VALUES('a b c', NULL)`)
	exec(`INSERT INTO x3 VALUES('a x c', NULL)`)

	r := db.Query(`SELECT a, b FROM x3 WHERE x3 MATCH 'a b'`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("rows = %v, want one row", r.Rows)
	}
	if got := r.Rows[0][0]; got != "a b c" {
		t.Fatalf("column a = %v, want a b c", got)
	}
}
