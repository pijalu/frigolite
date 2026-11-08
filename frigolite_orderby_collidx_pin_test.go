package frigolite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// where.c's ORDER BY index-satisfaction collation rule (S12,
// portplan/PLAN_S12_orderby_collation_index.md): the planner may satisfy an
// ORDER BY with an index when each term's EFFECTIVE collation (explicit
// COLLATE, else the table column's declared COLLATE, else BINARY) equals the
// index column's DECLARED collation (the index definition's explicit COLLATE,
// else the table column's declared COLLATE, else BINARY) by NAME — the
// current registration of the collation is irrelevant, so a stale stored
// order (an index last rebuilt under a different registration) still wins
// (e_reindex 2.3.x's contract).
// ---------------------------------------------------------------------------

// lengthCollate is e_reindex's sort_by_length.
func lengthCollate(a, b string) int {
	if r := len(a) - len(b); r != 0 {
		return r
	}
	return strings.Compare(a, b)
}

// queryOneCol runs a query returning one TEXT column and joins the values.
func queryOneCol(t *testing.T, db *DB, q string) string {
	t.Helper()
	res := db.Query(q)
	if res.Error != nil {
		t.Fatalf("%s: %v", q, res.Error)
	}
	var got []string
	for _, r := range res.Rows {
		got = append(got, fmt.Sprint(r[0]))
	}
	return strings.Join(got, " ")
}

func eqpDetail(t *testing.T, db *DB, q string) string {
	t.Helper()
	res := db.Query("EXPLAIN QUERY PLAN " + q)
	if res.Error != nil {
		t.Fatalf("EQP %s: %v", q, res.Error)
	}
	var lines []string
	for _, r := range res.Rows {
		if s, ok := r[0].(string); ok && !strings.Contains(s, "QUERY PLAN") {
			lines = append(lines, strings.TrimLeft(s, "|`- "))
		}
	}
	return strings.Join(lines, "; ")
}

const collIdxRows = "one two three four five six seven eight"

// TestOrderByCollationIndexReuse pins the matcher's accept and refuse paths.
func TestOrderByCollationIndexReuse(t *testing.T) {
	t.Run("same-name collation index satisfies a COLLATE order by", func(t *testing.T) {
		dir := t.TempDir()
		db, err := Open(filepath.Join(dir, "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.RegisterCollation("collA", lengthCollate)
		db.RegisterCollation("collB", func(a, b string) int { return strings.Compare(a, b) })
		for _, s := range []string{
			"CREATE TABLE t1(x)",
			"CREATE INDEX i1_a ON t1(x COLLATE collA)",
			"CREATE INDEX i1_b ON t1(x COLLATE collB)",
			"INSERT INTO t1 VALUES('one'),('two'),('three'),('four'),('five'),('six'),('seven'),('eight')",
		} {
			if res := db.Exec(s); res.Error != nil {
				t.Fatalf("%s: %v", s, res.Error)
			}
		}
		// collA (length) is declared on the index: the index walk satisfies
		// the ORDER BY — no temp b-tree.
		if got := eqpDetail(t, db, "SELECT x FROM t1 ORDER BY x COLLATE collA"); strings.Contains(got, "TEMP B-TREE") {
			t.Fatalf("EQP: %s — want the index walk", got)
		}
		if got := queryOneCol(t, db, "SELECT x FROM t1 ORDER BY x COLLATE collA"); got != "one six two five four eight seven three" {
			t.Fatalf("rows: %s", got)
		}
	})

	t.Run("mismatched collation index is refused", func(t *testing.T) {
		dir := t.TempDir()
		db, err := Open(filepath.Join(dir, "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.RegisterCollation("collA", lengthCollate)
		for _, s := range []string{
			"CREATE TABLE t1(x)",
			"CREATE INDEX i1_b ON t1(x COLLATE BINARY)",
			"INSERT INTO t1 VALUES('one'),('two'),('three'),('four'),('five'),('six'),('seven'),('eight')",
		} {
			if res := db.Exec(s); res.Error != nil {
				t.Fatalf("%s: %v", s, res.Error)
			}
		}
		// The only index carries BINARY; the ORDER BY wants collA — C
		// temp-sorts (and the rows must come out in collA order).
		if got := eqpDetail(t, db, "SELECT x FROM t1 ORDER BY x COLLATE collA"); !strings.Contains(got, "TEMP B-TREE") {
			t.Fatalf("EQP: %s — want a temp b-tree sort", got)
		}
		if got := queryOneCol(t, db, "SELECT x FROM t1 ORDER BY x COLLATE collA"); got != "one six two five four eight seven three" {
			t.Fatalf("rows: %s", got)
		}
	})

	t.Run("stale stored order wins after a collation swap and partial reindex", func(t *testing.T) {
		dir := t.TempDir()
		db, err := Open(filepath.Join(dir, "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.RegisterCollation("collA", lengthCollate)
		db.RegisterCollation("collB", func(a, b string) int { return strings.Compare(a, b) })
		for _, s := range []string{
			"CREATE TABLE t1(x)",
			"CREATE INDEX i1_a ON t1(x COLLATE collA)",
			"CREATE INDEX i1_b ON t1(x COLLATE collB)",
			"INSERT INTO t1 VALUES('one'),('two'),('three'),('four'),('five'),('six'),('seven'),('eight')",
			// Pre-2.2: swap collB to LENGTH and rebuild everything.
		} {
			if res := db.Exec(s); res.Error != nil {
				t.Fatalf("%s: %v", s, res.Error)
			}
		}
		db.RegisterCollation("collB", lengthCollate)
		if res := db.Exec("REINDEX"); res.Error != nil {
			t.Fatalf("REINDEX: %v", res.Error)
		}
		// Pre-2.3: swap collB back to VALUE; rebuild only the collA indexes.
		db.RegisterCollation("collB", func(a, b string) int { return strings.Compare(a, b) })
		if res := db.Exec("REINDEX collA"); res.Error != nil {
			t.Fatalf("REINDEX collA: %v", res.Error)
		}
		// The i1_b index (declared collation collB, stored LENGTH order from
		// the pre-2.2 rebuild) still satisfies ORDER BY x COLLATE collB —
		// the stored order beats a re-sort with the current registration.
		if got := eqpDetail(t, db, "SELECT x FROM t1 ORDER BY x COLLATE collB"); strings.Contains(got, "TEMP B-TREE") {
			t.Fatalf("EQP: %s — want the index walk", got)
		}
		if got := queryOneCol(t, db, "SELECT x FROM t1 ORDER BY x COLLATE collB"); got != "one six two five four eight seven three" {
			t.Fatalf("rows: %s (want the stale length order)", got)
		}
	})

	t.Run("plain order by keeps the declared-collation rule", func(t *testing.T) {
		dir := t.TempDir()
		db, err := Open(filepath.Join(dir, "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range []string{
			"CREATE TABLE t1(x)",
			"CREATE INDEX ix ON t1(x)",
			"INSERT INTO t1 VALUES(3),(1),(2)",
		} {
			if res := db.Exec(s); res.Error != nil {
				t.Fatalf("%s: %v", s, res.Error)
			}
		}
		// A bare term and a same-shape index: BINARY == BINARY — the index
		// walk satisfies the ORDER BY (the pre-S12 behavior, kept).
		if got := eqpDetail(t, db, "SELECT x FROM t1 ORDER BY x"); !strings.Contains(got, "INDEX ix") {
			t.Fatalf("EQP: %s — want the index walk", got)
		}
		if got := queryOneCol(t, db, "SELECT x FROM t1 ORDER BY x"); got != "1 2 3" {
			t.Fatalf("rows: %s", got)
		}
	})
}

// collIdxRows is referenced by the subtests above through the literals; keep
// the constant honest with one usage.
var _ = collIdxRows
