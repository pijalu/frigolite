package frigolite

import (
	"fmt"
	"testing"
)

// TestPrintfJSONStringConversion pins SQLite's printf %j/%J conversions
// (printf-20.*): NULL handling, JSON escaping, UTF-8 precision, width, and
// the '!' alternate-form-2 flag. Both verbs currently flow through %s.
func TestPrintfJSONStringConversion(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cases := []struct {
		sql  string
		want string
	}{
		{`SELECT format('{%!.3J:5}','abcdefg')->>'abc';`, `5`},
		{`SELECT format('{%!.3J:5}','αβγδεζη')->>'αβγ';`, `5`},
		{`SELECT format('{%.6J:5}','abcdefg')->>'abcdef';`, `5`},
		{`SELECT format('{%.6J:5}','αβγδεζη')->>'αβγ';`, `5`},
		{`SELECT format('{"%!.3j":5}','abcdefg')->>'abc';`, `5`},
		{`SELECT format('{"%!.3j":5}','αβγδεζη')->>'αβγ';`, `5`},
		{`SELECT format('{"%.6j":5}','abcdefg')->>'abcdef';`, `5`},
		{`SELECT format('{"%.6j":5}','αβγδεζη')->>'αβγ';`, `5`},
		{"WITH RECURSIVE c(n,t) AS (\n    VALUES(1,char(1))\n    UNION ALL\n    SELECT n+1, t||char(n+1) FROM c WHERE n<0x7e\n  )\n  SELECT hex(format('{\"a\":%J}',t)->>'a') FROM c WHERE n=0x7e;",
			`0102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F202122232425262728292A2B2C2D2E2F303132333435363738393A3B3C3D3E3F404142434445464748494A4B4C4D4E4F505152535455565758595A5B5C5D5E5F606162636465666768696A6B6C6D6E6F707172737475767778797A7B7C7D7E`},
		{`SELECT format('1-%j-2-%J-3',null,null);`, `1--2-null-3`},
		{`SELECT format('<%6J>','いち');`, `<"いち">`},
		{`SELECT format('<%!6J>','いち');`, `<  "いち">`},
		{`SELECT format('<%!-6J>','いち');`, `<"いち"  >`},
		{`SELECT format('<%6.8J>','いち, に, さん');`, `<"いち, ">`},
		{`SELECT format('<%!6.2J>','いち, に, さん');`, `<  "いち">`},
		{`SELECT format('<%!-6.2J>','いち, に, さん');`, `<"いち"  >`},
		{`SELECT format('<%6j>','いち');`, `<いち>`},
		{`SELECT format('<%!6j>','いち');`, `<    いち>`},
		{`SELECT format('<%!-6j>','いち');`, `<いち    >`},
		{`SELECT format('<%6.8j>','いち, に, さん');`, `<いち,   >`},
		{`SELECT format('<%!6.2j>','いち, に, さん');`, `<    いち>`},
		{`SELECT format('<%!-6.2j>','いち, に, さん');`, `<いち    >`},
		{`SELECT format('<%.7j>','abc"""def');`, `<abc\"\"\"d>`},
		{`SELECT format('<%j>','abc'||char(0x1f)||'def');`, `<abc\u001fdef>`},
		{`SELECT format('<%.4j>','a"b"c');`, `<a\"b\">`},
		{`SELECT format('<%.1J>','abcdef');`, `<"a">`},
		{`SELECT format('<%.0J>','abcdef');`, `<"">`},
	}

	for _, tc := range cases {
		r := db.Query(tc.sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", tc.sql, r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Fatalf("%s: rows = %#v, want one scalar", tc.sql, r.Rows)
		}
		if got := fmt.Sprint(r.Rows[0][0]); got != tc.want {
			t.Errorf("%s\n got  %q\n want %q", tc.sql, got, tc.want)
		}
	}
}
