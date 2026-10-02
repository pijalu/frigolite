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

// TestRowidTextAffinityIndexedPaths pins the same conversion for the
// index-seek shapes (the seek bound and the WHERE re-check must agree),
// REAL/TEXT column controls, subquery RHS operands, mixed-type aggregates,
// UNION dedup, and the rtree/rtree_i32 xFilter comparison domain
// (sqlite3_value_numeric_type: whitespace-trimmed whole-string conversion,
// garbage stays TEXT). All expected values below are sqlite3-CLI-verified.
func TestRowidTextAffinityIndexedPaths(t *testing.T) {
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
	query := func(sql string) string {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Fatalf("%s: want one scalar row, got %v", sql, r.Rows)
		}
		switch v := r.Rows[0][0].(type) {
		case nil:
			return "NULL"
		case int64:
			return strconv.FormatInt(v, 10)
		case float64:
			return strconv.FormatFloat(v, 'g', -1, 64)
		case string:
			return v
		default:
			t.Fatalf("%s: unexpected cell type %T", sql, v)
			return ""
		}
	}

	exec("CREATE TABLE k1(k INTEGER);")
	exec("CREATE INDEX k1_k ON k1(k);")
	exec("CREATE TABLE rr(r REAL);")
	exec("CREATE TABLE m2(z);")
	for _, v := range []string{
		"INSERT INTO k1 VALUES (10),(11),(12),(50),(100)",
		"INSERT INTO rr VALUES (1.5),(50.5),(100.5),(120.5)",
		"INSERT INTO m2 VALUES ('6'),(6),(7),('7e')",
	} {
		exec(v)
	}

	cases := []struct{ sql, want string }{
		// Index seek with a whitespace-padded text bound: the seek converts.
		{"SELECT count(*) FROM k1 WHERE k = ' 50 '", "1"},
		{"SELECT group_concat(k) FROM (SELECT k FROM k1 WHERE k BETWEEN ' 11 ' AND ' 12 ' ORDER BY k)", "11,12"},
		{"SELECT count(*) FROM k1 WHERE k > ' 50.5 '", "1"},
		// The scan shape (+k strips affinity) must not convert.
		{"SELECT count(*) FROM k1 WHERE +k = ' 50 '", "0"},
		{"SELECT count(*) FROM k1 WHERE k = '+50'", "1"},
		{"SELECT count(*) FROM k1 WHERE k = '0x10'", "0"},
		// Subquery RHS text converts through the column's affinity.
		{"SELECT count(*) FROM k1 WHERE k = (SELECT ' 50 ')", "1"},
		// REAL column control: whitespace integer converts under REAL affinity.
		{"SELECT count(*) FROM rr WHERE r > ' 100 '", "2"},
		// Garbage text stays TEXT (greater than every number).
		{"SELECT count(*) FROM k1 WHERE k < '50abc'", "5"},
		// Mixed-type aggregates and set ops never apply affinity.
		{"SELECT max(z) FROM m2", "7e"},
		{"SELECT min(z) FROM m2", "6"},
		{"SELECT group_concat(z) FROM (SELECT z FROM m2 UNION SELECT 6 ORDER BY z)", "6,7,6,7e"},
	}
	for _, tc := range cases {
		if got := query(tc.sql); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.sql, got, tc.want)
		}
	}

	// DML rowid point-lookup resolves the same whitespace-trimmed bound.
	if r := db.Exec("UPDATE m2 SET z = 'u' WHERE rowid = ' 1 '"); r.Error != nil {
		t.Fatalf("UPDATE rowid = ' 1 ': %v", r.Error)
	} else if r.Changes != 1 {
		t.Errorf("UPDATE rowid = ' 1 ': %d changes, want 1", r.Changes)
	}
	if got := query("SELECT z FROM m2 WHERE rowid = 1"); got != "u" {
		t.Errorf("rowid 1 after UPDATE = %q, want u", got)
	}

	// rtree xFilter comparison domain: pushed constraints evaluate with
	// sqlite3_value_numeric_type semantics, so whitespace numerics bound the
	// walk and garbage compares as TEXT.
	exec("CREATE VIRTUAL TABLE rt USING rtree(id, x0, x1);")
	exec("INSERT INTO rt VALUES (1, 0, 100), (2, 200, 300);")
	exec("CREATE VIRTUAL TABLE ri USING rtree_i32(id, x0, x1);")
	exec("INSERT INTO ri VALUES (1, 0, 100), (2, 200, 300);")
	for _, tc := range []struct{ sql, want string }{
		{"SELECT group_concat(id) FROM rt WHERE x1 > ' 200 '", "2"},
		{"SELECT group_concat(id) FROM rt WHERE x1 = ' 300 '", "2"},
		{"SELECT group_concat(id) FROM rt WHERE x1 <= '250abc'", "1,2"},
		{"SELECT group_concat(id) FROM rt WHERE x1 > '250abc'", "NULL"},
		{"SELECT group_concat(id) FROM ri WHERE x1 > ' 50.5 '", "1,2"},
		{"SELECT group_concat(id) FROM ri WHERE x1 <= '250abc'", "1,2"},
	} {
		if got := query(tc.sql); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.sql, got, tc.want)
		}
	}
}
