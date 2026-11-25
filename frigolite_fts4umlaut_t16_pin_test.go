package frigolite

import (
	"fmt"
	"testing"
)

// TestFTS4UmlautUnicodeEscapes is the pure-Go port of the fts4umlaut 1.x
// contract. It uses real Unicode strings, unlike the JSON fixture whose TCL
// \uXXXX escapes were emitted as literal "uXXXX" text.
func TestFTS4UmlautUnicodeEscapes(t *testing.T) {
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
	exec(`CREATE VIRTUAL TABLE t1 USING fts4(x, tokenize=unicode61)`)
	exec(`CREATE VIRTUAL TABLE t2 USING fts4(x, tokenize=unicode61 "remove_diacritics=2")`)

	count := func(table, match string) int64 {
		t.Helper()
		r := db.Query(fmt.Sprintf("SELECT count(*) FROM %s WHERE %s MATCH '%s'", table, table, match))
		if r.Error != nil {
			t.Fatalf("%s/%q: %v", table, match, r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("%s/%q rows = %v", table, match, r.Rows)
		}
		switch v := r.Rows[0][0].(type) {
		case int64:
			return v
		case int:
			return int64(v)
		default:
			t.Fatalf("%s/%q count = %T %v", table, match, v, v)
			return 0
		}
	}

	cases := []struct {
		match string
		t1    int64
		t2    int64
	}{
		{"Hà Nội", 0, 1},
		{"Hà Noi", 1, 1},
		{"Ha Noi", 1, 1},
		{"Ha N\u1ed9i", 0, 1},
		{"Ha N\u006fi", 1, 1},
		{"Ha N\u006f\u0302i", 1, 1},
		{"Ha N\u006f\u0323\u0302i", 1, 1},
	}
	for _, tc := range cases {
		exec("DELETE FROM t1")
		exec("INSERT INTO t1(rowid, x) VALUES (1, 'Ha Noi')")
		if got := count("t1", tc.match); got != tc.t1 {
			t.Errorf("t1 MATCH %q = %d, want %d", tc.match, got, tc.t1)
		}
		exec("DELETE FROM t1")
		exec(fmt.Sprintf("INSERT INTO t1(rowid, x) VALUES (1, '%s')", tc.match))
		if got := count("t1", "Ha Noi"); got != tc.t1 {
			t.Errorf("t1 insert %q = %d, want %d", tc.match, got, tc.t1)
		}

		exec("DELETE FROM t2")
		exec("INSERT INTO t2(rowid, x) VALUES (1, 'Ha Noi')")
		if got := count("t2", tc.match); got != tc.t2 {
			t.Errorf("t2 MATCH %q = %d, want %d", tc.match, got, tc.t2)
		}
		exec("DELETE FROM t2")
		exec(fmt.Sprintf("INSERT INTO t2(rowid, x) VALUES (1, '%s')", tc.match))
		if got := count("t2", "Ha Noi"); got != tc.t2 {
			t.Errorf("t2 insert %q = %d, want %d", tc.match, got, tc.t2)
		}
	}
}
