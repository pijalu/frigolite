package frigolite

import (
	"strconv"
	"testing"
)

// TestRowidTextAffinity pins the SQLite comparison-affinity conversion of
// TEXT operands against INTEGER-affinity comparisons (rowid, INTEGER
// PRIMARY KEY aliases, INT columns): leading and trailing whitespace is
// skipped, a well-formed number converts, and the WHOLE string must be
// consumed (applyNumericAffinity → sqlite3AtoF; '5000abc' and '5000.0abc'
// stay TEXT). All expected values below are sqlite3-CLI-verified.
func TestRowidTextAffinity(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	queryCount := func(sql string) int64 {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Fatalf("%s: want one scalar row, got %v", sql, r.Rows)
		}
		n, _ := r.Rows[0][0].(int64)
		return n
	}

	exec("CREATE TABLE w(id INTEGER PRIMARY KEY, x);")
	exec("CREATE TABLE c(a INT);")
	for i := 1; i <= 200; i++ {
		exec("INSERT INTO w(id, x) VALUES (" + strconv.Itoa(i) + ", 'x');")
		exec("INSERT INTO c(a) VALUES (" + strconv.Itoa(i) + ");")
	}

	cases := []struct {
		sql  string
		want int64
	}{
		// Whitespace is skipped by the rowid's numeric affinity.
		{"SELECT count(*) FROM w WHERE rowid > ' 50 '", 150},
		{"SELECT count(*) FROM w WHERE rowid >= ' 50 '", 151},
		{"SELECT count(*) FROM w WHERE rowid < ' 50 '", 49},
		{"SELECT count(*) FROM w WHERE rowid <= ' 50 '", 50},
		{"SELECT count(*) FROM w WHERE rowid = ' 50 '", 1},
		{"SELECT count(*) FROM w WHERE rowid = '  +25  '", 1},
		// The whole string must convert: garbage-prefixed integers stay
		// TEXT, which sorts above every rowid.
		{"SELECT count(*) FROM w WHERE rowid > '50abc'", 0},
		{"SELECT count(*) FROM w WHERE rowid < '50abc'", 200},
		{"SELECT count(*) FROM w WHERE rowid = '50.0abc'", 0},
		{"SELECT count(*) FROM w WHERE rowid = 'abc'", 0},
		// Reals convert; integral ones match rowids.
		{"SELECT count(*) FROM w WHERE rowid = '50.0'", 1},
		{"SELECT count(*) FROM w WHERE rowid > ' 50.5 '", 150},
		{"SELECT count(*) FROM w WHERE rowid > '5e1'", 150},
		{"SELECT count(*) FROM w WHERE rowid = ' 5e1 '", 1},
		{"SELECT count(*) FROM w WHERE rowid > '.5'", 200},
		// Hex text never converts under affinity.
		{"SELECT count(*) FROM w WHERE rowid = '0x10'", 0},
		// Unary plus strips the column affinity: TEXT compares as TEXT.
		{"SELECT count(*) FROM w WHERE +rowid > ' 50 '", 0},
		{"SELECT count(*) FROM w WHERE +rowid < ' 50 '", 200},
		// INTEGER PRIMARY KEY alias and a plain INT column: same rule.
		{"SELECT count(*) FROM w WHERE id > ' 50 '", 150},
		{"SELECT count(*) FROM c WHERE a > ' 50 '", 150},
		{"SELECT count(*) FROM c WHERE a = ' 50.0 '", 1},
		// BETWEEN bounds convert the same way.
		{"SELECT count(*) FROM w WHERE rowid BETWEEN ' 45 ' AND ' 55 '", 11},
		{"SELECT count(*) FROM w WHERE rowid NOT BETWEEN ' 45 ' AND ' 55 '", 189},
		// No affinity between two bare literals: no conversion at all.
		{"SELECT 1 = '1'", 0},
		{"SELECT 5000 = ' 5000 '", 0},
	}
	for _, tc := range cases {
		if got := queryCount(tc.sql); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.sql, got, tc.want)
		}
	}

	// DML point-op paths resolve the same text constants as the eval.
	exec("DELETE FROM w WHERE rowid = ' 200 ';")
	if got := queryCount("SELECT count(*) FROM w"); got != 199 {
		t.Errorf("DELETE WHERE rowid = ' 200 ': %d rows left, want 199", got)
	}
	exec("UPDATE w SET x = 'u' WHERE id = ' 100.0 ';")
	if got := queryCount("SELECT count(*) FROM w WHERE x = 'u'"); got != 1 {
		t.Errorf("UPDATE WHERE id = ' 100.0 ': %d rows updated, want 1", got)
	}
	exec("DELETE FROM w WHERE rowid = '50.0abc';")
	if got := queryCount("SELECT count(*) FROM w"); got != 199 {
		t.Errorf("DELETE WHERE rowid = '50.0abc' must not match; %d rows left, want 199", got)
	}
}
