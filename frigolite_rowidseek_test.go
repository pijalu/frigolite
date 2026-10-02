package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// openRowidSeekDB builds a temp database with an INTEGER PRIMARY KEY table
// (ids 1..N), an alias-free rowid table, and a shadow table declaring its own
// rowid column.
func openRowidSeekDB(t *testing.T, n int) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "rowidseek.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE t(id INTEGER PRIMARY KEY, c TEXT)")
	mustExec("WITH RECURSIVE cnt(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM cnt WHERE x<" +
		seekN(n) + ") INSERT INTO t SELECT x, 'val'||x FROM cnt")
	mustExec("CREATE TABLE r(a TEXT, c TEXT)")
	mustExec("WITH RECURSIVE cnt(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM cnt WHERE x<" +
		seekN(n) + ") INSERT INTO r SELECT 'a'||x, 'val'||x FROM cnt")
	mustExec("CREATE TABLE sh(id INTEGER PRIMARY KEY, rowid TEXT, c TEXT)")
	mustExec("INSERT INTO sh VALUES (1,'r1','x'),(2,'r2','y'),(3,'r3','z')")
	return db
}

func seekN(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func queryColumn(t *testing.T, db *frigolite.DB, sql string) []interface{} {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("%s: %v", sql, res.Error)
	}
	out := make([]interface{}, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, r[0])
	}
	return out
}

func queryText(t *testing.T, db *frigolite.DB, sql string) string {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("%s: %v", sql, res.Error)
	}
	if len(res.Rows) == 0 {
		return ""
	}
	row := res.Rows[len(res.Rows)-1]
	s, _ := row[len(row)-1].(string)
	// EQP detail lines carry the plan-tree prefix.
	return strings.TrimPrefix(strings.TrimPrefix(s, "`--"), "|--")

}

// TestRowidSeekAliasEquality: WHERE <ipk>=<literal> seeks exactly like
// rowid=<literal> (P2) — same rows, and the EQP says SEARCH rowid=?.
func TestRowidSeekAliasEquality(t *testing.T) {
	db := openRowidSeekDB(t, 5000)
	cases := []struct {
		alias, rowid string
	}{
		{"SELECT c FROM t WHERE id=2500", "SELECT c FROM t WHERE rowid=2500"},
		{"SELECT c FROM t WHERE t.id=17", "SELECT c FROM t WHERE rowid=17"},
		{"SELECT c FROM t WHERE id='2500'", "SELECT c FROM t WHERE rowid=2500"},
		{"SELECT c FROM t WHERE id=2500.0", "SELECT c FROM t WHERE rowid=2500"},
		{"SELECT c FROM t AS a WHERE a.id=99", "SELECT c FROM t WHERE rowid=99"},
		{"SELECT c FROM t WHERE id==42", "SELECT c FROM t WHERE rowid=42"},
	}
	for _, tc := range cases {
		got := queryColumn(t, db, tc.alias)
		want := queryColumn(t, db, tc.rowid)
		if len(got) != 1 || len(want) != 1 || got[0] != want[0] {
			t.Errorf("%s = %v, want %v (via %s)", tc.alias, got, want, tc.rowid)
		}
	}
	// Provable no-match shapes still plan, returning nothing.
	for _, q := range []string{"SELECT c FROM t WHERE id=NULL", "SELECT c FROM t WHERE id='abc'", "SELECT c FROM t WHERE id=4.5"} {
		if got := queryColumn(t, db, q); len(got) != 0 {
			t.Errorf("%s = %v, want no rows", q, got)
		}
	}
}

// TestRowidSeekAliasOutput: the INTEGER PRIMARY KEY column reads as the
// rowid in both the output and the WHERE re-check through the seek path
// (the record stores NULL for the alias).
func TestRowidSeekAliasOutput(t *testing.T) {
	db := openRowidSeekDB(t, 100)
	for _, q := range []string{
		"SELECT id, c FROM t WHERE rowid=7",
		"SELECT id, c FROM t WHERE id=7",
		"SELECT typeof(id) FROM t WHERE id=7",
	} {
		res := db.Query(q)
		if res.Error != nil {
			t.Fatalf("%s: %v", q, res.Error)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("%s = %d rows, want 1", q, len(res.Rows))
		}
		last := res.Rows[0][len(res.Rows[0])-1]
		if q[:16] == "SELECT typeof(id" {
			if last != "integer" {
				t.Errorf("%s = %v, want integer", q, last)
			}
			continue
		}
		if res.Rows[0][0] != int64(7) {
			t.Errorf("%s: id = %v, want 7", q, res.Rows[0][0])
		}
	}
}

// TestRowidSeekRange: literal rowid ranges seek+iterate with exact rows
// (P4), for rowid and alias spellings, with non-integral and empty bounds.
func TestRowidSeekRange(t *testing.T) {
	db := openRowidSeekDB(t, 100)
	cases := []struct {
		q    string
		want int
	}{
		{"SELECT rowid FROM t WHERE rowid BETWEEN 10 AND 20", 11},
		{"SELECT rowid FROM t WHERE id BETWEEN 10 AND 20", 11},
		{"SELECT rowid FROM t WHERE rowid>10 AND rowid<20", 9},
		{"SELECT rowid FROM t WHERE rowid>=10 AND rowid<=20", 11},
		{"SELECT rowid FROM t WHERE rowid>=10 AND rowid<20", 10},
		{"SELECT rowid FROM t WHERE rowid>10 AND rowid<=20", 10},
		{"SELECT rowid FROM t WHERE rowid BETWEEN 4.5 AND 8.5", 4},
		{"SELECT rowid FROM t WHERE rowid>4.5 AND rowid<=8", 4},
		{"SELECT rowid FROM t WHERE rowid BETWEEN 50 AND 49", 0},
		{"SELECT rowid FROM t WHERE rowid>100", 0},
		{"SELECT rowid FROM t WHERE rowid<1", 0},
		{"SELECT rowid FROM t WHERE rowid BETWEEN -10 AND 0", 0},
		{"SELECT rowid FROM t WHERE rowid>90", 10},
		{"SELECT rowid FROM t WHERE rowid BETWEEN ' 10 ' AND 12", 3}, // text bound converts under rowid affinity (oracle: rows 10,11,12)
		{"SELECT rowid FROM t WHERE rowid<'abc'", 100},
		{"SELECT rowid FROM t WHERE rowid>'abc'", 0},
		{"SELECT rowid FROM t WHERE rowid>NULL", 0},
		{"SELECT rowid FROM t WHERE rowid BETWEEN NULL AND 5", 0},
		{"SELECT rowid FROM r WHERE rowid BETWEEN 10 AND 20", 11},
		{"SELECT rowid FROM t WHERE rowid BETWEEN 40 AND 60 AND c LIKE 'val5%'", 10},
		{"SELECT rowid FROM t WHERE id=50 AND rowid>1", 1},
	}
	for _, tc := range cases {
		got := queryColumn(t, db, tc.q)
		if len(got) != tc.want {
			t.Errorf("%s = %d rows, want %d", tc.q, len(got), tc.want)
		}
	}
}

// TestRowidSeekShadow: a declared rowid column shadows the pseudo-column
// (scans) while the INTEGER PRIMARY KEY alias keeps seeking — both
// semantically correct.
func TestRowidSeekShadow(t *testing.T) {
	db := openRowidSeekDB(t, 10)
	if got := queryColumn(t, db, "SELECT c FROM sh WHERE id=1"); len(got) != 1 || got[0] != "x" {
		t.Errorf("alias seek under shadow = %v, want [x]", got)
	}
	if got := queryColumn(t, db, "SELECT c FROM sh WHERE rowid='r2'"); len(got) != 1 || got[0] != "y" {
		t.Errorf("declared rowid column = %v, want [y]", got)
	}
	if got := queryColumn(t, db, "SELECT c FROM sh WHERE rowid=2"); len(got) != 0 {
		t.Errorf("shadowed pseudo-column text compare = %v, want none", got)
	}
}

// TestRowidSeekExplain: the plan text matches execution for every seek
// shape (equality, ranges, alias spellings, sqlite3 CLI wording).
func TestRowidSeekExplain(t *testing.T) {
	db := openRowidSeekDB(t, 100)
	cases := map[string]string{
		"EXPLAIN QUERY PLAN SELECT c FROM t WHERE id=50":                           "SEARCH t USING INTEGER PRIMARY KEY (rowid=?)",
		"EXPLAIN QUERY PLAN SELECT c FROM t WHERE rowid=50":                        "SEARCH t USING INTEGER PRIMARY KEY (rowid=?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t AS a WHERE a.id=50":                    "SEARCH a USING INTEGER PRIMARY KEY (rowid=?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid>1 AND rowid<9":             "SEARCH t USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid BETWEEN 10 AND 20":         "SEARCH t USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid>=10 AND rowid<=20":         "SEARCH t USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid>10":                        "SEARCH t USING INTEGER PRIMARY KEY (rowid>?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid<=10":                       "SEARCH t USING INTEGER PRIMARY KEY (rowid<?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE id>10 AND id<20":                 "SEARCH t USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid>10 AND rowid<20 AND c='x'": "SEARCH t USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid=5 AND rowid>1":             "SEARCH t USING INTEGER PRIMARY KEY (rowid=?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t AS a WHERE a.id>1 AND a.id<9":          "SEARCH a USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)",
		"EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid>'abc'":                     "SEARCH t USING INTEGER PRIMARY KEY (rowid>?)",
	}
	for q, want := range cases {
		if got := queryText(t, db, q); got != want {
			t.Errorf("%s =\n  %s\nwant:\n  %s", q, got, want)
		}
	}
	// OR of rowid conjuncts keeps the scan (no MULTI-INDEX OR).
	if got := queryText(t, db, "EXPLAIN QUERY PLAN SELECT * FROM t WHERE rowid>1 OR rowid<9"); got != "SCAN t" {
		t.Errorf("OR rowid plan = %q, want SCAN t", got)
	}
}
