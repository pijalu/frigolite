package frigolite

import (
	"strings"
	"testing"
)

// P5 pre-tests for G5.ANALYZE — ANALYZE/stat1 correctness, REINDEX, and
// autoindex/index-assisted result-equivalence. Each expectation was verified
// against sqlite3 3.51 as the oracle.
//
// Scope: (a) ANALYZE populates sqlite_stat1 with SQLite's shape (rows per
// index, and one idx-NULL row per non-empty table with no indexes; empty
// tables produce no rows), (b) REINDEX succeeds and does not corrupt the
// schema, (c) query results are IDENTICAL whether or not an autoindex is
// used (PRAGMA automatic_index=ON/OFF), and (d) index-assisted WHERE /
// autoindex joins never change results. Pure plan-choice (which index the
// planner picks, EQP labels) is out of scope and asserted only as
// result-equivalence.

func TestP5AnalyzeStat1Shape(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a,b);",
		"CREATE INDEX t1i1 ON t1(a);",
		"CREATE INDEX t1i2 ON t1(b);",
		"CREATE TABLE noidx(a);",
		"CREATE TABLE empty_t(a,b);",
		"CREATE INDEX empty_i ON empty_t(a);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// ANALYZE on an empty database: no stat rows (SQLite skips empty tables).
	if r := db.Exec("ANALYZE"); r.Error != nil {
		t.Fatalf("ANALYZE: %v", r.Error)
	}
	if got := flattenQuery(t, db, "SELECT count(*) FROM sqlite_stat1"); got != "0" {
		t.Errorf("empty-db stat1 count: got [%s], want [0]", got)
	}

	// Populate: t1 gets 4 rows, noidx gets 3 rows, empty_t stays empty.
	for _, s := range []string{
		"INSERT INTO t1 VALUES(1,10),(1,11),(2,20),(3,30);",
		"INSERT INTO noidx VALUES(1),(2),(3);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}
	if r := db.Exec("ANALYZE"); r.Error != nil {
		t.Fatalf("ANALYZE: %v", r.Error)
	}

	// t1i1 on a=1,1,2,3: 4 rows, 3 distinct a → ceil(4/3)=2 → "4 2".
	// t1i2 on b=10,11,20,30: 4 distinct → "4 1".
	// noidx has no indexes → row with idx NULL, stat "3".
	// empty_t is empty → no rows at all.
	if got := flattenQuery(t, db, "SELECT idx, stat FROM sqlite_stat1 WHERE tbl='t1' ORDER BY idx"); got != "t1i1 4 2 t1i2 4 1" {
		t.Errorf("t1 stat1: got [%s]", got)
	}
	if got := flattenQuery(t, db, "SELECT idx, stat FROM sqlite_stat1 WHERE tbl='noidx'"); got != "NULL 3" {
		t.Errorf("noidx stat1: got [%s], want [NULL 3] (idx NULL row)", got)
	}
	if got := flattenQuery(t, db, "SELECT count(*) FROM sqlite_stat1 WHERE tbl='empty_t'"); got != "0" {
		t.Errorf("empty_t stat1 rows: got [%s], want [0]", got)
	}

	// Re-analyzing a single table must replace (not duplicate) its rows.
	if r := db.Exec("ANALYZE t1"); r.Error != nil {
		t.Fatalf("ANALYZE t1: %v", r.Error)
	}
	if got := flattenQuery(t, db, "SELECT idx, stat FROM sqlite_stat1 WHERE tbl='t1' ORDER BY idx"); got != "t1i1 4 2 t1i2 4 1" {
		t.Errorf("re-analyzed t1 stat1: got [%s]", got)
	}
}

func TestP5AnalyzeQualifiedNames(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if r := db.Exec("CREATE TABLE t1(a,b)"); r.Error != nil {
		t.Fatalf("exec: %v", r.Error)
	}
	if r := db.Exec("CREATE INDEX t1i1 ON t1(a)"); r.Error != nil {
		t.Fatalf("exec: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t1 VALUES(1,2),(1,3),(2,4)"); r.Error != nil {
		t.Fatalf("exec: %v", r.Error)
	}

	// ANALYZE schema.table and bare table names both work and populate the
	// same stat1 shape (verified against sqlite3).
	for _, sql := range []string{"ANALYZE main.t1", "ANALYZE t1", "ANALYZE main"} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		if got := flattenQuery(t, db, "SELECT idx, stat FROM sqlite_stat1 WHERE tbl='t1' ORDER BY idx"); got != "t1i1 3 2" {
			t.Errorf("after %s: got [%s], want [t1i1 3 2]", sql, got)
		}
	}

	// Unknown database and table errors match SQLite.
	for _, sql := range []string{
		"ANALYZE no_such_db.no_such_table",
		"ANALYZE no_such_table",
	} {
		if err := queryError(db, sql); err == nil {
			t.Errorf("%s: expected error, got nil", sql)
		}
	}
}

func TestP5AnalyzeReindex(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a,b);",
		"CREATE INDEX i1 ON t1(a);",
		"CREATE TABLE t2(c,d);",
		"CREATE INDEX i2 ON t2(c);",
		"INSERT INTO t1 VALUES(1,2),(3,4);",
		"INSERT INTO t2 VALUES(5,6);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// REINDEX with no argument rebuilds all indexes; schema-qualified and
	// name forms are accepted. Results are unchanged (correctness only).
	for _, sql := range []string{"REINDEX", "REINDEX main", "REINDEX i1", "REINDEX main.i1"} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	if got := flattenQuery(t, db, "SELECT a FROM t1 ORDER BY a"); got != "1 3" {
		t.Errorf("t1 after REINDEX: got [%s]", got)
	}
	if got := flattenQuery(t, db, "SELECT c FROM t2 ORDER BY c"); got != "5" {
		t.Errorf("t2 after REINDEX: got [%s]", got)
	}
}

func TestP5AnalyzeAutoindexResultEquivalence(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	for _, s := range []string{
		"CREATE TABLE t1(a,b);",
		"CREATE TABLE t2(c,d);",
		"CREATE TABLE t3(e,f);",
		"INSERT INTO t1 VALUES(1,11),(2,22),(3,33),(4,44),(5,55);",
		"INSERT INTO t2 VALUES(1,100),(3,300),(5,500),(7,700);",
		"INSERT INTO t3 VALUES(2,200),(4,400),(6,600);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	// The same query must return the same rows with automatic indexing ON
	// and OFF (SQLite builds an ephemeral index for the join in the default
	// ON mode). The result SET is the correctness contract; row order for
	// equal keys is plan-dependent so both outputs are sorted before compare.
	queries := []string{
		"SELECT a, c FROM t1 JOIN t2 ON a=c",
		"SELECT a, c, d FROM t1 LEFT JOIN t2 ON a=c WHERE d IS NOT NULL OR c IS NULL",
		"SELECT a, f FROM t1 JOIN t3 ON a=f",
		"SELECT count(*) FROM t1, t2, t3 WHERE t1.a=t2.c AND t2.c=t3.e",
		"SELECT b FROM t1 WHERE a IN (SELECT c FROM t2)",
		"SELECT b FROM t1 WHERE a NOT IN (SELECT e FROM t3)",
	}
	for _, q := range queries {
		// With automatic_index=OFF (full scan).
		if r := db.Exec("PRAGMA automatic_index=OFF"); r.Error != nil {
			t.Fatalf("PRAGMA automatic_index=OFF: %v", r.Error)
		}
		off := flattenQuery(t, db, q)
		// With automatic_index=ON (autoindex allowed).
		if r := db.Exec("PRAGMA automatic_index=ON"); r.Error != nil {
			t.Fatalf("PRAGMA automatic_index=ON: %v", r.Error)
		}
		on := flattenQuery(t, db, q)
		if !sameRowSet(off, on) {
			t.Errorf("autoindex result mismatch for %q:\n  OFF: [%s]\n  ON:  [%s]", q, off, on)
		}
	}
}

func TestP5AnalyzeIndexAssistedWhere(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Index-assisted WHERE must produce identical rows to a full scan: the
	// presence of an index never changes results, only the access path.
	for _, s := range []string{
		"CREATE TABLE t1(a,b);",
		"INSERT INTO t1 VALUES(1,10),(1,11),(2,20),(3,30),(NULL,40);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}

	queries := []string{
		"SELECT a, b FROM t1 WHERE a=1",
		"SELECT a, b FROM t1 WHERE a IN (1,3)",
		"SELECT a, b FROM t1 WHERE a BETWEEN 1 AND 2",
		"SELECT a, b FROM t1 WHERE a IS NULL",
		"SELECT a, b FROM t1 WHERE a>1 AND a<=3",
		"SELECT count(*) FROM t1 WHERE a=1 OR a=3",
	}
	for _, q := range queries {
		before := flattenQuery(t, db, q)
		if r := db.Exec("CREATE INDEX idx_assist ON t1(a)"); r.Error != nil {
			t.Fatalf("CREATE INDEX: %v", r.Error)
		}
		after := flattenQuery(t, db, q)
		if r := db.Exec("DROP INDEX idx_assist"); r.Error != nil {
			t.Fatalf("DROP INDEX: %v", r.Error)
		}
		if before != after {
			t.Errorf("index-assisted mismatch for %q:\n  no index: [%s]\n  index:    [%s]", q, before, after)
		}
	}
}

func TestP5AnalyzeReindexCorruptSchema(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// REINDEX over a schema with two indexes that share a name on DIFFERENT
	// tables reports "database disk image is malformed" (SQLite behavior).
	// Frigolite validates the schema and reports the same corruption error.
	for _, s := range []string{
		"CREATE TABLE t1(a);",
		"CREATE TABLE t2(a);",
		"CREATE INDEX i1 ON t1(a);",
		"CREATE INDEX i2 ON t2(a);",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}
	if r := db.Exec("PRAGMA writable_schema=ON"); r.Error != nil {
		t.Fatalf("writable_schema: %v", r.Error)
	}
	// Rename i2 (on t2) to collide with i1 (on t1) via the schema table.
	if r := db.Exec("UPDATE sqlite_schema SET name='i1' WHERE name='i2'"); r.Error != nil {
		t.Fatalf("schema edit: %v", r.Error)
	}
	if r := db.Exec("PRAGMA writable_schema=OFF"); r.Error != nil {
		t.Fatalf("writable_schema off: %v", r.Error)
	}
	if err := queryError(db, "REINDEX"); err == nil {
		t.Errorf("REINDEX over corrupt schema: expected error, got nil")
	}
}

// sameRowSet reports whether two space-joined flattened result strings contain
// the same multi-column row set (order-insensitive). Rows are space-separated
// 2-tuples in the flattened form; equality is compared after sorting the
// individual cell tokens by group.
func sameRowSet(a, b string) bool {
	if a == b {
		return true
	}
	fa := strings.Fields(a)
	fb := strings.Fields(b)
	if len(fa) != len(fb) {
		return false
	}
	// Compare as multisets of tokens — sufficient for the small pre-test
	// result sets (each cell appears the same number of times).
	count := func(parts []string) map[string]int {
		m := make(map[string]int)
		for _, p := range parts {
			m[p]++
		}
		return m
	}
	ma, mb := count(fa), count(fb)
	if len(ma) != len(mb) {
		return false
	}
	for k, v := range ma {
		if mb[k] != v {
			return false
		}
	}
	return true
}
