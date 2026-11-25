package frigolite

import (
	"fmt"
	"testing"
)

// TestMalformedNumericSeparatorTokenText pins SQLite's QNUMBER contract for
// underscore digit separators (literal-4.2/4.13/4.14): the tokenizer consumes
// a whole numeric run containing '_', then rejects malformed separators with
// the ORIGINAL token text. Valid separators are dequoted before evaluation.
func TestMalformedNumericSeparatorTokenText(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	catch := func(sql, want string) {
		t.Helper()
		r := db.Query(sql)
		if r.Error == nil {
			t.Fatalf("%s: got success, want %s", sql, want)
		}
		if got := r.Error.Error(); got != want {
			t.Fatalf("%s: error = %q, want %q", sql, got, want)
		}
	}
	query := func(sql, want string) {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Fatalf("%s: rows = %#v, want one scalar", sql, r.Rows)
		}
		if got := fmt.Sprint(r.Rows[0][0]); got != want {
			t.Fatalf("%s = %q, want %q", sql, got, want)
		}
	}

	// The three target leaves: a separator followed by a dot, a separator
	// after a dot, and a separator before a dot. SQLite's sqlite3GetToken
	// keeps the whole run; sqlite3DequoteNumber rejects the separator.
	catch(`SELECT 1_.4`, `unrecognized token: "1_.4"`)
	catch(`SELECT 12._34`, `unrecognized token: "12._34"`)
	catch(`SELECT 12_.34`, `unrecognized token: "12_.34"`)

	// Neighbouring literal-4.x shapes guard the token boundary rules.
	catch(`SELECT 1_`, `unrecognized token: "1_"`)
	catch(`SELECT 1e_4`, `unrecognized token: "1e_4"`)
	catch(`SELECT 1_e4`, `unrecognized token: "1_e4"`)
	catch(`SELECT 1.4_e4`, `unrecognized token: "1.4_e4"`)
	catch(`SELECT 1.4e+_4`, `unrecognized token: "1.4e"`)
	catch(`SELECT 1.4e-_4`, `unrecognized token: "1.4e"`)
	catch(`SELECT 1.4e4_`, `unrecognized token: "1.4e4_"`)
	catch(`SELECT 1234_`, `unrecognized token: "1234_"`)
	catch(`SELECT 12__34`, `unrecognized token: "12__34"`)
	catch(`SELECT 12.34_`, `unrecognized token: "12.34_"`)
	catch(`SELECT 1.0e1_______2`, `unrecognized token: "1.0e1_______2"`)
	catch(`SELECT 0xFF__EF`, `unrecognized token: "0xFF__EF"`)
	catch(`SELECT 0xFFEF_`, `unrecognized token: "0xFFEF_"`)

	// Valid separators must keep their current values.
	query(`SELECT 123_456`, `123456`)
	query(`SELECT 1.4_2`, `1.42`)
	query(`SELECT 1.0e1_2 = 1e12`, `1`)
	query(`SELECT 0xFF_FF`, `65535`)
	query(`SELECT 0xFF_EF`, `65519`)
}
