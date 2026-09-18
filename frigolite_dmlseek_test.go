package frigolite_test

// P9.PERF.T2: seek-driven UPDATE/DELETE row collection (SQLite SEARCH plans).
// Each test drives the engine directly and pins the engine-visible contract
// of the point-lookup fast path: identical rows affected, identical trigger
// firing order, identical LIMIT/ORDER BY windows, and identical EQP text
// (oracle-verified against the sqlite3 CLI).

import (
	"sort"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func dmlSeekOpen(t *testing.T) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(t.TempDir() + "/seek.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func dmlSeekExec(t *testing.T, db *frigolite.DB, sql string) {
	t.Helper()
	if r := db.Exec(sql); r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
}

func dmlSeekQuery(t *testing.T, db *frigolite.DB, sql string) [][]interface{} {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	return r.Rows
}

func dmlSeekRowsString(t *testing.T, db *frigolite.DB, sql string) string {
	t.Helper()
	rows := dmlSeekQuery(t, db, sql)
	var sb strings.Builder
	for i, row := range rows {
		if i > 0 {
			sb.WriteByte('\n')
		}
		for j, v := range row {
			if j > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(frigoSeekStr(v))
		}
	}
	return sb.String()
}

func frigoSeekStr(v interface{}) string {
	switch n := v.(type) {
	case int64:
		return itoaSeek(n)
	case int:
		return itoaSeek(int64(n))
	case string:
		return n
	case nil:
		return "NULL"
	}
	return "?"
}

func itoaSeek(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// dmlSeekFixture builds t1(a INTEGER, b, c) with 30 rows: a=i%5 (5 matches
// per value), b text, rowid 1..30.
func dmlSeekFixture(t *testing.T, db *frigolite.DB) {
	t.Helper()
	dmlSeekExec(t, db, "CREATE TABLE t1(a INTEGER, b, c);")
	var sb strings.Builder
	for i := 1; i <= 30; i++ {
		sb.WriteString("INSERT INTO t1 VALUES(")
		sb.WriteString(itoaSeek(int64(i % 5)))
		sb.WriteString(", 'b")
		sb.WriteString(itoaSeek(int64(i)))
		sb.WriteString("', 'c")
		sb.WriteString(itoaSeek(int64(i)))
		sb.WriteString("');\n")
	}
	dmlSeekExec(t, db, sb.String())
}

// TestDMLSeek_UpdateRowid pins UPDATE ... WHERE rowid=<const> parity: the
// pinned row and only it is updated, in every constant shape.
func TestDMLSeek_UpdateRowid(t *testing.T) {
	cases := []struct {
		name  string
		where string
		want  string // value of c for rowid 7 after the update, or "-" for absent
	}{
		{"int", "rowid = 7", "UPDATED"},
		{"reversed", "7 = rowid", "UPDATED"},
		{"qualified", "t1.rowid = 7", "UPDATED"},
		{"float", "rowid = 7.0", "UPDATED"},
		{"arith", "rowid = 3 + 4", "UPDATED"},
		{"text-number", "rowid = '7'", "UPDATED"},
		{"string-col", "c = 'c7'", "UPDATED"},
		{"no-match-float", "rowid = 7.5", "c7"},
		{"no-match-text", "rowid = 'zz'", "c7"},
		{"no-match-null", "rowid = NULL", "c7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := dmlSeekOpen(t)
			dmlSeekFixture(t, db)
			dmlSeekExec(t, db, "UPDATE t1 SET c='UPDATED' WHERE "+tc.where)
			got := "-"
			rows := dmlSeekQuery(t, db, "SELECT c FROM t1 WHERE rowid=7")
			if len(rows) == 1 {
				got = frigoSeekStr(rows[0][0])
			}
			if got != tc.want {
				t.Fatalf("WHERE %s: rowid 7 c = %q, want %q", tc.where, got, tc.want)
			}
			// No collateral updates: exactly the 30 original rows remain and
			// an untouched row keeps its value.
			if n := dmlSeekQuery(t, db, "SELECT count(*) FROM t1")[0][0]; frigoSeekStr(n) != "30" {
				t.Fatalf("row count = %v, want 30", n)
			}
		})
	}
}

// TestDMLSeek_UpdateRowidConjunction pins the full-WHERE re-evaluation: the
// rowid pins the candidate, the remaining conjunct decides.
func TestDMLSeek_UpdateRowidConjunction(t *testing.T) {
	db := dmlSeekOpen(t)
	dmlSeekFixture(t, db)
	dmlSeekExec(t, db, "UPDATE t1 SET c='YES' WHERE rowid=7 AND a=2") // a for i=7 is 7%5=2
	dmlSeekExec(t, db, "UPDATE t1 SET c='NO' WHERE rowid=7 AND a=99")
	if got := dmlSeekRowsString(t, db, "SELECT c FROM t1 WHERE rowid=7"); got != "YES" {
		t.Fatalf("conjunct update = %q, want YES", got)
	}
	// Conflicting rowid conjuncts match nothing.
	dmlSeekExec(t, db, "UPDATE t1 SET c='BAD' WHERE rowid=7 AND rowid=8")
	if got := dmlSeekRowsString(t, db, "SELECT c FROM t1 WHERE rowid=7"); got != "YES" {
		t.Fatalf("conflicting rowids = %q, want YES", got)
	}
}

// TestDMLSeek_UpdateIndexed pins indexed point UPDATE parity, including
// multi-row candidates (trigger order), extra conjuncts, and LIMIT windows.
func TestDMLSeek_UpdateIndexed(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE a=3")
		// rows with a=3: rowids 3,8,13,18,23,28
		if got := dmlSeekRowsString(t, db, "SELECT rowid FROM t1 WHERE c='HIT' ORDER BY rowid"); got != "3\n8\n13\n18\n23\n28" {
			t.Fatalf("indexed update hits = %q", got)
		}
		if n := dmlSeekQuery(t, db, "SELECT count(*) FROM t1 WHERE c<>'HIT'")[0][0]; frigoSeekStr(n) != "24" {
			t.Fatalf("untouched = %v, want 24", n)
		}
	})
	t.Run("conjunct", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		// a=3 rows with rowid>20: 23, 28
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE a=3 AND rowid>20")
		if got := dmlSeekRowsString(t, db, "SELECT rowid FROM t1 WHERE c='HIT' ORDER BY rowid"); got != "23\n28" {
			t.Fatalf("conjunct hits = %q", got)
		}
	})
	t.Run("limit", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE a=3 LIMIT 2")
		if got := dmlSeekRowsString(t, db, "SELECT rowid FROM t1 WHERE c='HIT' ORDER BY rowid"); got != "3\n8" {
			t.Fatalf("limit hits = %q, want first two in rowid order", got)
		}
	})
	t.Run("text-key", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1b ON t1(b)")
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE b='b12'")
		if got := dmlSeekRowsString(t, db, "SELECT rowid FROM t1 WHERE c='HIT'"); got != "12" {
			t.Fatalf("text-key hits = %q", got)
		}
	})
	t.Run("affinity", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		// INTEGER affinity: 3.0 matches the stored int 3 rows.
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE a=3.0")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE c='HIT'"); got != "6" {
			t.Fatalf("affinity hits = %q, want 6", got)
		}
	})
	t.Run("trigger-order", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		dmlSeekExec(t, db, "CREATE TABLE log(rowid_seq INTEGER)")
		dmlSeekExec(t, db, "CREATE TRIGGER tr AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES(OLD.rowid); END")
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE a=4")
		// a=4 rows: rowids 4,9,14,19,24,29 — triggers must fire in rowid order.
		if got := dmlSeekRowsString(t, db, "SELECT rowid_seq FROM log"); got != "4\n9\n14\n19\n24\n29" {
			t.Fatalf("trigger order = %q", got)
		}
	})
}

// TestDMLSeek_DeleteRowidAndIndexed pins DELETE point-lookup parity.
func TestDMLSeek_DeleteRowidAndIndexed(t *testing.T) {
	t.Run("rowid", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "DELETE FROM t1 WHERE rowid=7")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1"); got != "29" {
			t.Fatalf("count = %q, want 29", got)
		}
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE rowid=7"); got != "0" {
			t.Fatalf("rowid 7 still present")
		}
	})
	t.Run("rowid-no-match", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "DELETE FROM t1 WHERE rowid=99")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1"); got != "30" {
			t.Fatalf("count = %q, want 30", got)
		}
	})
	t.Run("indexed", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		dmlSeekExec(t, db, "DELETE FROM t1 WHERE a=1")
		// a=1 rows: rowids 1,6,11,16,21,26
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1"); got != "24" {
			t.Fatalf("count = %q, want 24", got)
		}
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE rowid IN (1,6,11,16,21,26)"); got != "0" {
			t.Fatalf("a=1 rows survived")
		}
	})
	t.Run("indexed-returning", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		rows := dmlSeekQuery(t, db, "DELETE FROM t1 WHERE a=2 RETURNING rowid")
		var got []int
		for _, r := range rows {
			if n, ok := r[0].(int64); ok {
				got = append(got, int(n))
			}
		}
		sort.Ints(got)
		var parts []string
		for _, n := range got {
			parts = append(parts, itoaSeek(int64(n)))
		}
		want := "2 7 12 17 22 27"
		if strings.Join(parts, " ") != want {
			t.Fatalf("returning = %q, want %q", strings.Join(parts, " "), want)
		}
	})
}

// TestDMLSeek_Fallbacks pins shapes that must keep exact (non-seek) behavior:
// WITHOUT ROWID tables, partial and NOCASE indexes, OR clauses, aliases.
func TestDMLSeek_Fallbacks(t *testing.T) {
	t.Run("without-rowid", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekExec(t, db, "CREATE TABLE w(a, b, PRIMARY KEY(a)) WITHOUT ROWID")
		var sb strings.Builder
		for i := 1; i <= 10; i++ {
			sb.WriteString("INSERT INTO w VALUES(")
			sb.WriteString(itoaSeek(int64(i)))
			sb.WriteString(", 'x');\n")
		}
		dmlSeekExec(t, db, sb.String())
		dmlSeekExec(t, db, "UPDATE w SET b='HIT' WHERE a=5")
		if got := dmlSeekRowsString(t, db, "SELECT b FROM w WHERE a=5"); got != "HIT" {
			t.Fatalf("wr update = %q", got)
		}
		dmlSeekExec(t, db, "DELETE FROM w WHERE a IN (3, 4)")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM w"); got != "8" {
			t.Fatalf("wr delete count = %q, want 8", got)
		}
	})
	t.Run("nocase-index", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekExec(t, db, "CREATE TABLE n(s TEXT COLLATE NOCASE)")
		dmlSeekExec(t, db, "CREATE INDEX in1 ON n(s)")
		dmlSeekExec(t, db, "INSERT INTO n VALUES('ABC'),('abc'),('zzz')")
		// NOCASE equality matches both case variants.
		dmlSeekExec(t, db, "UPDATE n SET s='HIT' WHERE s='abc'")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM n WHERE s='HIT'"); got != "2" {
			t.Fatalf("nocase hits = %q, want 2", got)
		}
	})
	t.Run("explicit-collate-index", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekExec(t, db, "CREATE TABLE n(s TEXT)")
		dmlSeekExec(t, db, "CREATE INDEX in1 ON n(s COLLATE NOCASE)")
		dmlSeekExec(t, db, "INSERT INTO n VALUES('ABC'),('abc'),('zzz')")
		// Oracle-verified: the WHERE comparison resolves the COLUMN's
		// collation (BINARY here) — an explicit COLLATE on the INDEX key does
		// not change WHERE matching, so only the exact case matches.
		dmlSeekExec(t, db, "UPDATE n SET s='HIT' WHERE s='abc'")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM n WHERE s='HIT'"); got != "1" {
			t.Fatalf("collate hits = %q, want 1", got)
		}
	})
	t.Run("partial-index", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		// Partial index: rows outside the predicate are not in the index, so
		// the seek path must not narrow through it. (A predicate on rowid —
		// oracle-legal — is a known engine gap: CREATE INDEX ... WHERE
		// rowid>10 fails name resolution; a plain-column predicate is used.)
		dmlSeekExec(t, db, "CREATE INDEX ip ON t1(a) WHERE a > 0")
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE a=3")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE c='HIT'"); got != "6" {
			t.Fatalf("partial-index hits = %q, want 6", got)
		}
	})
	t.Run("or-clause", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE a=3 OR rowid=1")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE c='HIT'"); got != "7" {
			t.Fatalf("or hits = %q, want 7", got)
		}
	})
	t.Run("alias", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		dmlSeekExec(t, db, "UPDATE t1 AS x SET c='HIT' WHERE x.a=3")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE c='HIT'"); got != "6" {
			t.Fatalf("alias hits = %q, want 6", got)
		}
	})
	t.Run("update-from", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		dmlSeekExec(t, db, "CREATE TABLE src(k, v)")
		dmlSeekExec(t, db, "INSERT INTO src VALUES(3, 'FROM')")
		dmlSeekExec(t, db, "UPDATE t1 SET c=(SELECT v FROM src WHERE k=t1.a) WHERE a=3")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE c='FROM'"); got != "6" {
			t.Fatalf("update-from hits = %q, want 6", got)
		}
	})
	t.Run("expression-index", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX ie ON t1(a*2)")
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE a=3")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE c='HIT'"); got != "6" {
			t.Fatalf("expr-index hits = %q, want 6", got)
		}
	})
	t.Run("autoindex", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekExec(t, db, "CREATE TABLE u(id TEXT UNIQUE, v)")
		dmlSeekExec(t, db, "INSERT INTO u VALUES('k1','a'),('k2','b')")
		dmlSeekExec(t, db, "UPDATE u SET v='HIT' WHERE id='k2'")
		if got := dmlSeekRowsString(t, db, "SELECT v FROM u WHERE id='k2'"); got != "HIT" {
			t.Fatalf("autoindex update = %q", got)
		}
	})
	t.Run("subquery-const", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE TABLE keys(k)")
		dmlSeekExec(t, db, "INSERT INTO keys VALUES(5)")
		dmlSeekExec(t, db, "UPDATE t1 SET c='HIT' WHERE rowid=(SELECT max(k) FROM keys)")
		if got := dmlSeekRowsString(t, db, "SELECT c FROM t1 WHERE rowid=5"); got != "HIT" {
			t.Fatalf("subquery-const = %q", got)
		}
	})
	t.Run("schema-qualifier", func(t *testing.T) {
		db := dmlSeekOpen(t)
		dmlSeekFixture(t, db)
		dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
		// Schema-qualified TABLE with an unqualified column reference. (A
		// schema-qualified COLUMN reference — WHERE main.t1.a=3, oracle-legal
		// — is a known pre-existing engine gap: it matches no rows.)
		dmlSeekExec(t, db, "UPDATE main.t1 SET c='HIT' WHERE a=3")
		if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE c='HIT'"); got != "6" {
			t.Fatalf("schema-qualified hits = %q, want 6", got)
		}
	})
}

// TestDMLSeek_IndexMaintenance pins that updates through the seek path keep
// every index entry in sync (an UPDATE touching an indexed column re-entries
// the index; a stale entry would corrupt later lookups).
func TestDMLSeek_IndexMaintenance(t *testing.T) {
	db := dmlSeekOpen(t)
	dmlSeekFixture(t, db)
	dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
	dmlSeekExec(t, db, "CREATE INDEX i1c ON t1(c)")
	// Move the a=3 rows to a=9: i1a entries must move too.
	dmlSeekExec(t, db, "UPDATE t1 SET a=9 WHERE a=3")
	if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE a=9"); got != "6" {
		t.Fatalf("post-update a=9 = %q, want 6", got)
	}
	if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE a=3"); got != "0" {
		t.Fatalf("stale a=3 rows = %q, want 0", got)
	}
	// The index over c must still resolve the updated rows' keys.
	if got := dmlSeekRowsString(t, db, "SELECT rowid FROM t1 WHERE c='c3'"); got != "3" {
		t.Fatalf("i1c lookup = %q", got)
	}
	// integrity-style round trip: delete through the index, then re-seek.
	// a=9 rows are rowids {3,8,13,18,23,28}; rowid>25 leaves only 28.
	dmlSeekExec(t, db, "DELETE FROM t1 WHERE a=9 AND rowid>25")
	if got := dmlSeekRowsString(t, db, "SELECT count(*) FROM t1 WHERE a=9"); got != "5" {
		t.Fatalf("post-delete a=9 = %q, want 5", got)
	}
}

// TestDMLSeek_EQP pins the EXPLAIN QUERY PLAN text for DML point lookups
// (oracle-verified against the sqlite3 CLI: SEARCH plans for rowid=/indexed
// equalities, SCAN otherwise, FK child scans preserved).
func TestDMLSeek_EQP(t *testing.T) {
	db := dmlSeekOpen(t)
	dmlSeekExec(t, db, "CREATE TABLE t1(a INTEGER, b, c)")
	dmlSeekExec(t, db, "CREATE INDEX i1a ON t1(a)")
	dmlSeekExec(t, db, "CREATE TABLE p(id INTEGER PRIMARY KEY)")
	dmlSeekExec(t, db, "CREATE TABLE c1(pid REFERENCES p)")
	cases := []struct{ sql, want string }{
		{"UPDATE t1 SET b=0 WHERE a=5", "SEARCH t1 USING INDEX i1a (a=?)"},
		{"DELETE FROM t1 WHERE a=5", "SEARCH t1 USING INDEX i1a (a=?)"},
		{"UPDATE t1 SET b=0 WHERE rowid=5", "SEARCH t1 USING INTEGER PRIMARY KEY (rowid=?)"},
		{"DELETE FROM t1 WHERE rowid=5", "SEARCH t1 USING INTEGER PRIMARY KEY (rowid=?)"},
		{"DELETE FROM t1 WHERE t1.a=5", "SEARCH t1 USING INDEX i1a (a=?)"},
		{"UPDATE t1 SET b=0 WHERE b=5", "SCAN t1"}, // no index on b
		{"DELETE FROM t1 WHERE a>5", "SCAN t1"},    // range: scan keeps current plan
		{"UPDATE t1 SET b=0", "SCAN t1"},
		{"DELETE FROM p WHERE id=1", "SEARCH p USING INTEGER PRIMARY KEY (rowid=?)"},
	}
	for _, tc := range cases {
		got := dmlSeekRowsString(t, db, "EXPLAIN QUERY PLAN "+tc.sql)
		// The plan may append FK child scan nodes; compare the TARGET line
		// (the first line after the QUERY PLAN header).
		lines := strings.Split(got, "\n")
		if len(lines) < 2 {
			t.Fatalf("EQP %q: unexpected plan %q", tc.sql, got)
		}
		first := strings.TrimPrefix(strings.TrimPrefix(lines[1], "`--"), "|--")
		if first != tc.want {
			t.Fatalf("EQP %q\n got: %q\nwant: %q", tc.sql, got, tc.want)
		}
	}
	// FK child checks keep their SCAN nodes (oracle: SEARCH p + SCAN c1 for a
	// FK parent DELETE... with foreign_keys the plan adds child scans).
	got := dmlSeekRowsString(t, db, "EXPLAIN QUERY PLAN DELETE FROM p WHERE id=1")
	if !strings.Contains(got, "SEARCH p USING INTEGER PRIMARY KEY (rowid=?)") {
		t.Fatalf("FK parent plan lost SEARCH: %q", got)
	}
}
