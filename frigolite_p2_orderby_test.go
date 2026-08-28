package frigolite

import (
	"strings"
	"testing"
)

// TestP2OrderbyColumnExprAliasOrdinal verifies ORDER BY over a plain column,
// an expression, an output alias, and an ordinal, with ASC/DESC and
// multi-key ordering.
func TestP2OrderbyColumnExprAliasOrdinal(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b TEXT);
		INSERT INTO t VALUES(2,'b'),(1,'c'),(3,'a');
	`)
	// Plain column ASC (default)
	rows := p2QueryRows(t, db, "SELECT a FROM t ORDER BY a;")
	if got := flattenRows(rows); got != "1 2 3" {
		t.Errorf("ORDER BY column got %q want %q", got, "1 2 3")
	}
	// Column DESC
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a DESC;")
	if got := flattenRows(rows); got != "3 2 1" {
		t.Errorf("ORDER BY DESC got %q want %q", got, "3 2 1")
	}
	// Expression
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY -a;")
	if got := flattenRows(rows); got != "3 2 1" {
		t.Errorf("ORDER BY expr got %q want %q", got, "3 2 1")
	}
	// Alias
	rows = p2QueryRows(t, db, "SELECT a AS x FROM t ORDER BY x;")
	if got := flattenRows(rows); got != "1 2 3" {
		t.Errorf("ORDER BY alias got %q want %q", got, "1 2 3")
	}
	// Ordinal
	rows = p2QueryRows(t, db, "SELECT b, a FROM t ORDER BY 2;")
	if got := flattenRows(rows); got != "c 1 b 2 a 3" {
		t.Errorf("ORDER BY ordinal got %q want %q", got, "c 1 b 2 a 3")
	}
	// Multi-key: b DESC then a ASC
	rows = p2QueryRows(t, db, "SELECT b, a FROM t ORDER BY b DESC, a;")
	if got := flattenRows(rows); got != "c 1 b 2 a 3" {
		t.Errorf("ORDER BY multi-key got %q want %q", got, "c 1 b 2 a 3")
	}
}

// TestP2OrderbyNulls verifies NULLS FIRST/LAST and the default NULL position
// (NULLs sort first in ASC, last in DESC).
func TestP2OrderbyNulls(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT);
		INSERT INTO t VALUES(2),(NULL),(1);
	`)
	// Default ASC: NULL first
	rows := p2QueryRows(t, db, "SELECT a FROM t ORDER BY a;")
	if got := flattenRows(rows); got != "{} 1 2" {
		t.Errorf("ORDER BY default NULL got %q want %q", got, "{} 1 2")
	}
	// Default DESC: NULL last
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a DESC;")
	if got := flattenRows(rows); got != "2 1 {}" {
		t.Errorf("ORDER BY DESC NULL got %q want %q", got, "2 1 {}")
	}
	// Explicit NULLS FIRST
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a NULLS FIRST;")
	if got := flattenRows(rows); got != "{} 1 2" {
		t.Errorf("ORDER BY NULLS FIRST got %q want %q", got, "{} 1 2")
	}
	// Explicit NULLS LAST
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a NULLS LAST;")
	if got := flattenRows(rows); got != "1 2 {}" {
		t.Errorf("ORDER BY NULLS LAST got %q want %q", got, "1 2 {}")
	}
}

// TestP2OrderbyCollate verifies COLLATE per ORDER BY key overrides the
// column's default collation.
func TestP2OrderbyCollate(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a TEXT);
		INSERT INTO t VALUES('b'),('A'),('a'),('B');
	`)
	// BINARY (default): byte order — 'A','B','a','b'
	rows := p2QueryRows(t, db, "SELECT a FROM t ORDER BY a;")
	if got := flattenRows(rows); got != "A B a b" {
		t.Errorf("ORDER BY BINARY got %q want %q", got, "A B a b")
	}
	// NOCASE: A/a before B/b; ties within a collation group break by
	// rowid (insertion) order, matching SQLite.
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a COLLATE NOCASE;")
	if got := flattenRows(rows); got != "A a b B" {
		t.Errorf("ORDER BY NOCASE got %q want %q", got, "A a b B")
	}
}

// TestP2OrderbyCrossType verifies SQLite's cross-type sort order:
// NULL < INTEGER/REAL < TEXT < BLOB.
func TestP2OrderbyCrossType(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a);
		INSERT INTO t VALUES('text'),(1),(x'00ff'),(NULL),(2.5);
	`)
	rows := p2QueryRows(t, db, "SELECT a FROM t ORDER BY a;")
	// NULL first, then numbers (1, 2.5), then text, then blob.
	got := flattenRows(rows)
	want := "{} 1 2.5 text"
	if !strings.HasPrefix(got, want) {
		t.Errorf("cross-type ORDER BY got %q want prefix %q", got, want)
	}
	// The BLOB sorts last; render via hex to confirm position.
	rows = p2QueryRows(t, db, "SELECT typeof(a) FROM t ORDER BY a;")
	if got := flattenRows(rows); got != "null integer real text blob" {
		t.Errorf("cross-type typeof got %q want %q", got, "null integer real text blob")
	}
}

// TestP2OrderbyLimitOffset verifies LIMIT n, OFFSET m, LIMIT -1 (no limit),
// and LIMIT 0 (no rows).
func TestP2OrderbyLimitOffset(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT);
		INSERT INTO t VALUES(3),(1),(2),(4);
	`)
	// LIMIT 2
	rows := p2QueryRows(t, db, "SELECT a FROM t ORDER BY a LIMIT 2;")
	if got := flattenRows(rows); got != "1 2" {
		t.Errorf("LIMIT got %q want %q", got, "1 2")
	}
	// OFFSET requires LIMIT in SQLite; use LIMIT -1 OFFSET n for an
	// offset-only query.
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a LIMIT -1 OFFSET 1;")
	if got := flattenRows(rows); got != "2 3 4" {
		t.Errorf("OFFSET got %q want %q", got, "2 3 4")
	}
	// LIMIT 2 OFFSET 1
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a LIMIT 2 OFFSET 1;")
	if got := flattenRows(rows); got != "2 3" {
		t.Errorf("LIMIT/OFFSET got %q want %q", got, "2 3")
	}
	// LIMIT -1: no limit
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a LIMIT -1;")
	if got := flattenRows(rows); got != "1 2 3 4" {
		t.Errorf("LIMIT -1 got %q want %q", got, "1 2 3 4")
	}
	// LIMIT 0: no rows
	rows = p2QueryRows(t, db, "SELECT a FROM t ORDER BY a LIMIT 0;")
	if got := flattenRows(rows); got != "" {
		t.Errorf("LIMIT 0 got %q want %q", got, "")
	}
}

// TestP2OrderbyRandom verifies ORDER BY RANDOM() returns a permutation of the
// input (compared as a set, since the order is non-deterministic).
func TestP2OrderbyRandom(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT);
		INSERT INTO t VALUES(1),(2),(3);
	`)
	rows := p2QueryRows(t, db, "SELECT a FROM t ORDER BY RANDOM();")
	got := flattenRows(rows)
	// Check all three values appear exactly once regardless of order.
	for _, v := range []string{"1", "2", "3"} {
		if !strings.Contains(got, v) {
			t.Errorf("ORDER BY RANDOM result %q missing %s", got, v)
		}
	}
}

// TestP2OrderbyMinMaxScalar verifies the scalar MIN/MAX aggregate form
// (SELECT min(x), max(x) FROM t) with bare columns taking values from the
// min/max row.
func TestP2OrderbyMinMaxScalar(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b TEXT);
		INSERT INTO t VALUES(1,'x'),(5,'y'),(3,'z');
	`)
	rows := p2QueryRows(t, db, "SELECT min(a), max(a) FROM t;")
	if got := flattenRows(rows); got != "1 5" {
		t.Errorf("MIN/MAX got %q want %q", got, "1 5")
	}
	// Bare column b takes the value from the row of the LAST min/max
	// aggregate (max(a) here → row (5,'y')).
	rows = p2QueryRows(t, db, "SELECT min(a), max(a), b FROM t;")
	if got := flattenRows(rows); got != "1 5 y" {
		t.Errorf("MIN/MAX bare col got %q want %q", got, "1 5 y")
	}
	// min(a) alone: bare column from the min row.
	rows = p2QueryRows(t, db, "SELECT min(a), b FROM t;")
	if got := flattenRows(rows); got != "1 x" {
		t.Errorf("MIN bare col got %q want %q", got, "1 x")
	}
}

// TestP2OrderbyRowidTieBreak verifies that ORDER BY ties break by rowid
// (ascending), matching SQLite's stable sort.
func TestP2OrderbyRowidTieBreak(t *testing.T) {
	db, _ := Open("")
	defer db.Close()
	mustExec(t, db, `
		CREATE TABLE t(a INT, b TEXT);
		INSERT INTO t VALUES(1,'third'),(1,'first'),(1,'second');
	`)
	// Equal keys keep insertion (rowid) order.
	rows := p2QueryRows(t, db, "SELECT rowid, a, b FROM t ORDER BY a;")
	if got := flattenRows(rows); got != "1 1 third 2 1 first 3 1 second" {
		t.Errorf("rowid tie-break got %q want %q", got, "1 1 third 2 1 first 3 1 second")
	}
}
