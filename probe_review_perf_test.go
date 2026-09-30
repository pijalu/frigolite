package frigolite

import (
	"fmt"
	"testing"
)

// Adversarial review probes for the PERF optimization rounds
// (fleet/review-perf-audit). Each test pins an engine-visible edge the
// optimization could have cut. Expectations marked ORACLE were verified
// against the sqlite3 CLI; expectations marked DIFF are recorded on the
// pre-perf base commit (373665082) and compared against the review tree.

func probeOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func probeRows(t *testing.T, db *DB, sql string) []([]interface{}) {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	return r.Rows
}

func probeDump(rows []([]interface{})) string {
	s := ""
	for _, row := range rows {
		for i, v := range row {
			if i > 0 {
				s += "|"
			}
			s += fmt.Sprintf("%T:%v", v, v)
		}
		s += "\n"
	}
	return s
}

// --- Area 1: GROUP BY group-key fast path (aaf0891e9) ---

// NaN reachability: SQLite itself cannot hold NaN (stores NULL); frigolite's
// typed float equality (NaN != NaN) only diverges from %v spelling ("NaN" ==
// "NaN") if the engine can produce NaN at all.
func TestReviewNaNProduction(t *testing.T) {
	db := probeOpen(t)
	rows := probeRows(t, db, "SELECT 0.0/0.0, sqrt(-1), log(-1), exp(1000), 9e999, typeof(0.0/0.0), typeof(sqrt(-1)), typeof(log(-1)), typeof(exp(1000)), typeof(9e999)")
	t.Logf("nan-probe: %s", probeDump(rows))
}

// ORACLE: CREATE TABLE t(v REAL); INSERT -0.0,0.0; GROUP BY v -> one group,
// count 2 (SQLite compares REALs numerically; -0.0 == 0.0). Frigolite keys on
// the %v spelling ("-0" vs "0"): records the actual behavior (pre-existing if
// it differs — the typed path only runs in the collated scan).
func TestReviewNegZeroUncollated(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(v REAL); INSERT INTO t VALUES(-0.0),(0.0)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT v, count(*) FROM t GROUP BY v ORDER BY v"))
	t.Logf("negzero-uncollated: %s", got)
	if lit := probeDump(probeRows(t, db, "SELECT -0.0, 0.0, -0.0 = 0.0")); true {
		t.Logf("negzero-literals: %s", lit)
	}
}

// ORACLE: ('x',-0.0),('X',0.0) GROUP BY a COLLATE NOCASE, b -> ONE group
// (x|0.0|2): the collated term merges a/X and -0.0==0.0 numerically. This is
// the only path where groupKeyScalarEqual's typed float equality (== says
// -0.0==0.0, unlike the old "-0" vs "0" spellings) changes grouping.
// DIFF base: expect 2 groups on the pre-perf tree, 1 group here (toward oracle).
func TestReviewNegZeroCollated(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE w(a TEXT, b REAL); INSERT INTO w VALUES('x',-0.0),('X',0.0)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT a, b, count(*) FROM w GROUP BY a COLLATE NOCASE, b ORDER BY a, b"))
	t.Logf("negzero-collated: %s", got)
}

// Same shape as TestReviewNegZeroCollated but with the collated term as the
// SECOND term and NOCASE-only collisions ('Aa'/'aA'), guarding the per-term
// loop order in groupKeyValuesEqual.
func TestReviewCollatedTermOrder(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE w(x REAL, a TEXT); INSERT INTO w VALUES(-0.0,'Aa'),(0.0,'aA')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT x, a, count(*) FROM w GROUP BY x, a COLLATE NOCASE ORDER BY a"))
	t.Logf("collated-term-order: %s", got)
}

// ORACLE (mixed-type spellings): 5 (int) and 5.0 (real) -> one group count 2;
// '5'/'5.0' text separate; 9223372036854775807 int vs 9223372036854775808.0
// real separate; -9223372036854775808 alone. Pins groupKeyScalarEqual's
// int64/float64 FormatInt/FormatFloat parity for the collated scan.
func TestReviewBoundarySpellings(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(v); INSERT INTO t VALUES(9223372036854775807),(9223372036854775808.0),(-9223372036854775808),(5),(5.0),('5'),('5.0')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT typeof(v), v, count(*) FROM t GROUP BY v ORDER BY typeof(v), v"))
	want := "interface {}<nil>:-9223372036854775808\n" // placeholder, real assert below
	_ = want
	t.Logf("boundary-spellings:\n%s", got)
	// Frigolite's %v-spelling textual keys merge text '5' into the integer-5
	// group (5 groups total); the oracle keeps 6 (type priority). Pre-existing
	// design divergence — the DIFF base run must show the same 5.
	rows := probeRows(t, db, "SELECT count(*) FROM (SELECT v, count(*) c FROM t GROUP BY v)")
	if n := rows[0][0].(int64); n != 5 {
		t.Fatalf("group count = %d, want 5 (frigolite textual-key semantics, base-parity)", n)
	}
	rows = probeRows(t, db, "SELECT count(*) FROM t WHERE v = 5")
	t.Logf("v=5 matches (oracle: 2 — int 5 + real 5.0; text never equals numeric): %v", rows[0][0])
}

// int64 min boundary: int -2^63 and real -2^63 share numeric value. Oracle
// merges numerically; frigolite spells them differently ("…808" vs
// "-9.223372036854776e+18") — records whether the collated scan merges.
func TestReviewMinInt64FloatPair(t *testing.T) {
	db := probeOpen(t)
	got := probeDump(probeRows(t, db, "SELECT typeof(v), v, count(*) FROM (SELECT -9223372036854775808 AS v UNION ALL SELECT -9223372036854775808.0) GROUP BY v ORDER BY typeof(v)"))
	t.Logf("minint64-pair: %s", got)
}

// The third typed-path divergence: text '5' vs integer 5 as the UNCOLLATED
// term of a collated GROUP BY. Old code compared %v spellings ("5" == "5" →
// merged); typed groupKeyScalarEqual separates them. ORACLE: text never
// equals numeric (type priority) → 2 groups — the typed behavior matches.
// DIFF base: expect 1 group on the pre-perf tree, 2 here.
func TestReviewCollatedTextVsIntTerm(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE w(k, x); INSERT INTO w VALUES('A','5'),('a',5)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT k, x, count(*) FROM w GROUP BY k COLLATE NOCASE, x ORDER BY k"))
	t.Logf("collated-text-vs-int: %s", got)
}

// --- Area 2: row-output diet (0426c0e86) ---

// The one in-place mutation reachable on shared map slots:
// evalExprWithCollation rewrites CollatedValue.Collation/Explicit in place
// when a CASE/function/|| result IS the raw column slot value. Pre-change the
// map slot was cloned; post-change the rewrite leaks into the map. Observer:
// an ORDER BY term evaluated from the maps after the output row was built.
func TestReviewCaseCollateMutation(t *testing.T) {
	db := probeOpen(t)
	// 'ab' vs 'AB ': under NOCASE 'ab' sorts before 'AB ' (shorter prefix);
	// under RTRIM 'AB' < 'ab' binary, so 'AB ' must come first.
	if r := db.Exec("CREATE TABLE t(c TEXT COLLATE nocase); INSERT INTO t VALUES('AB '),('ab')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT CASE c WHEN 'zzz' THEN c ELSE c END COLLATE rtrim AS z, c AS y FROM t ORDER BY c"))
	t.Logf("case-collate-orderby: %s", got)
	// Also with the ORDER BY term referencing the plain column (schema
	// collation resolution path) and a DESC variant.
	got2 := probeDump(probeRows(t, db, "SELECT CASE c WHEN 'zzz' THEN c ELSE c END COLLATE rtrim AS z FROM t ORDER BY c DESC"))
	t.Logf("case-collate-orderby-desc: %s", got2)
}

// The leak into a retained row set: subquery materialization followed by an
// IN comparison whose collation resolution consults the RHS wrapper's
// (now-Explicit) marker.
func TestReviewCaseCollateSubqueryLeak(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(c TEXT COLLATE nocase); INSERT INTO t VALUES('ab'),('ab  ')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// 'ab  ' vs 'ab': equal under rtrim, different under nocase.
	got := probeDump(probeRows(t, db, "SELECT 'ab  ' IN (SELECT CASE c WHEN 'q' THEN c ELSE c END COLLATE rtrim FROM t)"))
	t.Logf("case-collate-subquery: %s", got)
	got2 := probeDump(probeRows(t, db, "SELECT 'ab  ' IN (SELECT c FROM t)"))
	t.Logf("plain-subquery: %s", got2)
}

// GROUP BY over the mutated slot: the partition runs BEFORE output eval, but
// a correlated-subquery / window consumer may read maps after it.
func TestReviewCaseCollateGroupBy(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(k TEXT COLLATE nocase, c TEXT COLLATE nocase); INSERT INTO t VALUES('A','AB '),('a','ab')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT k, CASE c WHEN 'zzz' THEN c ELSE c END COLLATE rtrim AS z, count(*) FROM t GROUP BY k ORDER BY k"))
	t.Logf("case-collate-groupby: %s", got)
}

// Blob slot sharing: []byte payloads must be deep-copied into the map —
// a GROUP BY over blobs must still see each row's own bytes.
func TestReviewBlobMapSharing(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(b BLOB); INSERT INTO t VALUES(x'0102'),(x'0304'),(x'050607')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT hex(b), count(*) FROM t GROUP BY b ORDER BY hex(b)"))
	t.Logf("blob-groupby: %s", got)
	if got != "string:0102|int64:1\nstring:0304|int64:1\nstring:050607|int64:1\n" {
		t.Fatalf("blob groups corrupted:\n%s", got)
	}
}

// Rowsless permutation: index-satisfiable ORDER BY + GROUP BY —
// order-sensitive aggregates (group_concat) must observe the index order of
// the maps alone.
func TestReviewIndexOrderGroupConcat(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(k TEXT, x INT); CREATE INDEX i1 ON t(k, x);" +
		"INSERT INTO t VALUES('b',2),('a',9),('a',1),('b',1),('a',5)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT k, group_concat(x) FROM t GROUP BY k ORDER BY k"))
	t.Logf("index-order-groupconcat: %s", got)
	got2 := probeDump(probeRows(t, db, "SELECT group_concat(x) FROM (SELECT x FROM t ORDER BY k, x)"))
	t.Logf("suborder-groupconcat: %s", got2)
}

// --- Area 6: fastParseInt64 overflow -> float extraction (bf66d87fb) ---

// Literals at the int64 boundary through the statement template cache: run
// each twice (first prepares/substitutes, second hits the cache) — kind and
// value must be identical run-to-run.
func TestReviewInt64BoundaryTemplates(t *testing.T) {
	db := probeOpen(t)
	stmts := []string{
		"SELECT 9223372036854775807, typeof(9223372036854775807)",
		"SELECT 9223372036854775808, typeof(9223372036854775808)",
		"SELECT -9223372036854775808, typeof(-9223372036854775808)",
		"SELECT -9223372036854775809, typeof(-9223372036854775809)",
		"SELECT 9223372036854775806, typeof(9223372036854775806)",
		"SELECT -9223372036854775807, typeof(-9223372036854775807)",
		"SELECT 10000000000000000000, typeof(10000000000000000000)",
	}
	for round := 1; round <= 2; round++ {
		for _, s := range stmts {
			got := probeDump(probeRows(t, db, s))
			if round == 1 {
				t.Logf("round1 %s\n  -> %s", s, got)
			} else {
				t.Logf("round2 %s\n  -> %s", s, got)
			}
		}
	}
}

// Boundary literals substituted into an identical-shape template with
// different values (cache must not glue run 1's literal kinds onto run 2):
// grouping parity between cached and uncached executions.
func TestReviewBoundaryTemplateGrouping(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(v)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	ins := func(vals string) {
		if r := db.Exec("INSERT INTO t SELECT " + vals); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	// Same template shape, alternating boundary kinds.
	ins("9223372036854775807")
	ins("9223372036854775808")
	ins("-9223372036854775808")
	ins("7")
	ins("8.0")
	got := probeDump(probeRows(t, db, "SELECT typeof(v), v, count(*) FROM t GROUP BY v ORDER BY typeof(v), v"))
	t.Logf("boundary-template-grouping:\n%s", got)
	// Same SELECT executed twice — second run from the cache.
	got2 := probeDump(probeRows(t, db, "SELECT typeof(v), v, count(*) FROM t GROUP BY v ORDER BY typeof(v), v"))
	if got != got2 {
		t.Fatalf("cached run diverged:\nfirst:\n%s\nsecond:\n%s", got, got2)
	}
}
