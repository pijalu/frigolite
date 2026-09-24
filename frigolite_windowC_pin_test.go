package frigolite

import (
	"os"
	"testing"
)

// Pin for windowC-2.x (windowC.test): a BLOB read under a UTF-16 database
// carries the database encoding tag (SQLite vdbe.c OP_Column sets
// pDest->enc = encoding for every disk value, blobs included), so
// group_concat's sqlite3_value_text rendering of the blob decodes UTF-16
// code units. With PRAGMA encoding=UTF16le the blob x'5585d09013455178cd11ce4a'
// renders as UTF-16le text; UTF16be decodes byte-swapped. Oracle
// (/usr/bin/sqlite3 3.51.0 and 3.54.0) verified both outputs. The first
// window has no preceding row, so its group_concat is SQL NULL — rendered
// "NULL" by flattenResult (the oracle prints the NULL row as an empty
// field; the original "{}" transcription — TCL for empty string, not NULL
// — never matched any engine output).
func TestWindowCGroupConcatBlobUTF16(t *testing.T) {
	const query = `
  WITH separator(x) AS (VALUES(',a,'),(',bc,')),
       value(y) AS (VALUES(1),(x'5585d09013455178cd11ce4a'))
  SELECT group_concat(y,x) OVER (ORDER BY x ROWS BETWEEN 1 PRECEDING AND 1 PRECEDING)
  FROM separator, value;`
	cases := []struct {
		pragma string
		want   string
	}{
		{"UTF16le", "NULL 1 蕕郐䔓硑ᇍ䫎 1"},
		{"UTF16be", "NULL 1 喅킐ፅ典촑칊 1"},
	}
	for _, tc := range cases {
		f := "windowcpin.db"
		os.Remove(f)
		db, err := Open(f)
		if err != nil {
			t.Fatal(err)
		}
		if res := db.Exec("PRAGMA encoding=" + tc.pragma + ";"); res.Error != nil {
			t.Fatalf("%s: pragma error: %v", tc.pragma, res.Error)
		}
		r := db.Query(query)
		if r.Error != nil {
			t.Fatalf("%s: query error: %v", tc.pragma, r.Error)
		}
		got := flattenResult(r)
		if got != tc.want {
			t.Errorf("%s: result mismatch\n  got:  [%s]\n  want: [%s]", tc.pragma, got, tc.want)
		}
		db.Close()
		os.Remove(f)
	}
}

// The UTF-8 path must keep byte-identical behavior: blob bytes render raw.
func TestWindowCGroupConcatBlobUTF8(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query(`
  WITH separator(x) AS (VALUES(',a,'),(',bc,')),
       value(y) AS (VALUES(1),(x'5585d09013455178cd11ce4a'))
  SELECT group_concat(y,x) OVER (ORDER BY x ROWS BETWEEN 1 PRECEDING AND 1 PRECEDING)
  FROM separator, value;`)
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := flattenResult(r)
	want := "NULL 1 " + string([]byte{0x55, 0x85, 0xd0, 0x90, 0x13, 0x45, 0x51, 0x78, 0xcd, 0x11, 0xce, 0x4a}) + " 1"
	if got != want {
		t.Errorf("result mismatch\n  got:  [%q]\n  want: [%q]", got, want)
	}
}
