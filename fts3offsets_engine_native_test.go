package frigolite

import (
	"strings"
	"testing"
)

// TestFTS3OffsetsEngineNative validates the ENGINE-level offsets() behavior
// that fts3offsets.testgen builds on (native-before-TCL rule): offsets()
// returns one 4-tuple (phrase, column, start-byte, token-length) per hit,
// concatenated as a flat list, for multi-token documents and OR/NEAR queries.
// The (A)/(B)/(C) rendering in the TCL suite is a registered-proc transform
// of exactly these tuples.
func TestFTS3OffsetsEngineNative(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	steps := []string{
		`CREATE VIRTUAL TABLE xx USING fts4`,
		`INSERT INTO xx VALUES('A x x x B C x x')`,
		`INSERT INTO xx VALUES('A x x C x x x C')`,
		`INSERT INTO xx VALUES('A x x B C x x x')`,
	}
	for _, s := range steps {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}

	r := db.Query(`SELECT rowid, offsets(xx) FROM xx WHERE xx MATCH 'a OR (b NEAR/1 c)' ORDER BY rowid`)
	if r.Error != nil {
		t.Fatalf("offsets query: %v", r.Error)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(r.Rows))
	}
	// Every document starts with token 'A' at byte 0: phrase 0 ('a' via OR),
	// column 0, start 0, length 1.
	for ri, row := range r.Rows {
		offs := strings.TrimSpace(row[1].(string))
		if offs == "" {
			t.Fatalf("row %d: offsets() empty", ri)
		}
		fields := strings.Fields(offs)
		if len(fields) < 4 || len(fields)%4 != 0 {
			t.Fatalf("row %d: offsets not 4-tuples: %q", ri, offs)
		}
		if fields[0] != "0" || fields[1] != "0" || fields[2] != "0" || fields[3] != "1" {
			t.Fatalf("row %d: first tuple = %v, want [0 0 0 1]", ri, fields[:4])
		}
	}

	// NEAR constraint: doc 1 has B..C adjacent once ('B C' at bytes 6..9);
	// phrase 2 is 'c'. Its tuple must appear with start 8, length 1
	// (byte offset of 'C' inside 'A x x x B C x x': A=0,x=2,3,4,B=6,C=8).
	r = db.Query(`SELECT offsets(xx) FROM xx WHERE xx MATCH 'b NEAR/1 c' AND rowid = 1`)
	if r.Error != nil {
		t.Fatalf("near query: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("near rows = %d, want 1", len(r.Rows))
	}
	offs := strings.TrimSpace(r.Rows[0][0].(string))
	if !strings.Contains(offs, " 8 1") {
		t.Fatalf("NEAR offsets missing byte-8 hit: %q", offs)
	}
}
