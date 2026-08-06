package frigolite

import (
	"reflect"
	"testing"
)

// TestOrIndexOptimization verifies the OR-index optimization: WHERE clauses of
// the form (col1=v1 AND col2=v2) OR (col3=v3 AND col4=v4) where each branch's
// columns are covered by an index produce rows in SQLite's index-ordered union
// order (branch 1's index scan order, then branch 2's minus duplicates), and
// that queries which cannot use the optimization still return correct rows.
// Every case is validated against the system sqlite3 CLI (oracle), skipping
// when the CLI is unavailable.
func TestOrIndexOptimization(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	setup := `
		CREATE TABLE t(i,j,k,m,n);
		CREATE INDEX ijk ON t(i,j,k);
		CREATE INDEX jmn ON t(j,m,n);
		INSERT INTO t VALUES(3, 3, 'three', 3, 'tres');
		INSERT INTO t VALUES(2, 2, 'two', 2, 'dos');
		INSERT INTO t VALUES(1, 1, 'one', 1, 'uno');
		INSERT INTO t VALUES(4, 4, 'four', 4, 'cuatro');
	`
	runSQL(t, db, setup)

	// OR of conjunctions: index-ordered union (branch 1 rows, then branch 2).
	cases := []string{
		"SELECT k FROM t WHERE (i=1 AND j=1) OR (i=2 AND j=2)",
		"SELECT k FROM t WHERE (i=1 AND j=1) OR (j=2 AND m=2)",
		"SELECT k FROM t WHERE (i=1 AND j=1) OR (i=2 AND j=2) OR (j=3 AND m=3)",
		"SELECT k FROM t WHERE (i=1 AND (j=1 or j=2)) OR (i=3 AND j=3)",
		"SELECT k FROM t WHERE (i=1 AND j=1) OR (j=2 AND m=2) OR (i=3 AND j=3)",
		"SELECT k FROM t WHERE (j=1 AND m=1) OR (i=2 AND j=2) OR (i=3 AND j=3)",
		"SELECT k FROM t WHERE (i=1 AND j=2) OR (i=2 AND j=1) OR (i=3 AND j=4)",
	}
	for _, q := range cases {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}

	// Multiple rows per branch: within a branch, index scan order.
	setup2 := `
		CREATE TABLE u(a INTEGER, b TEXT);
		CREATE INDEX ua ON u(a);
		CREATE INDEX ub ON u(b);
		INSERT INTO u VALUES(1, 'x');
		INSERT INTO u VALUES(1, 'y');
		INSERT INTO u VALUES(2, 'x');
		INSERT INTO u VALUES(2, 'z');
		INSERT INTO u VALUES(3, 'w');
	`
	runSQL(t, db, setup2)
	for _, q := range []string{
		"SELECT a, b FROM u WHERE a=1 OR a=2",
		"SELECT a, b FROM u WHERE a=1 OR a=2 ORDER BY b",
		"SELECT a, b FROM u WHERE a=1 OR a=2 LIMIT 3",
		"SELECT count(*), sum(a) FROM u WHERE a=1 OR a=3",
		"SELECT a, b FROM u WHERE a=1 OR b='z'",
		"SELECT a FROM u WHERE a=1 OR a=NULL",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup2)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}

	// Deduplication: one row matching several OR branches is emitted once.
	setup3 := `
		CREATE TABLE d(c0,c1,c2,c3,c4,c5,c6,c7,c8,c9,c10,c11,c12,c13,c14,c15,c16,c17);
		CREATE INDEX dc0 ON d(c0);
		CREATE INDEX dc1 ON d(c1);
		CREATE INDEX dc16 ON d(c16);
		CREATE INDEX dc17 ON d(c17);
		INSERT INTO d(c0,c17) VALUES(1,1);
	`
	runSQL(t, db, setup3)
	for _, q := range []string{
		"SELECT c0, c17 FROM d WHERE c0=1 OR c17=1",
		"SELECT count(*) FROM d WHERE c0=1 OR c1=1 OR c17=1",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup3)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}

	// OR over WITHOUT ROWID tables: index order uses the index key.
	setup4 := `
		CREATE TABLE wr(a, b, c, d, PRIMARY KEY(c, b)) WITHOUT ROWID;
		INSERT INTO wr VALUES('f', 1, 1, 'o');
		INSERT INTO wr VALUES('o', 2, 1, 't');
		INSERT INTO wr VALUES('t', 1, 2, 't');
		INSERT INTO wr VALUES('t', 2, 2, 'f');
		CREATE INDEX wr1 ON wr(d);
		CREATE INDEX wr2 ON wr(a);
	`
	runSQL(t, db, setup4)
	for _, q := range []string{
		"SELECT c||'.'||b FROM wr WHERE a='t' OR d='t'",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup4)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}

	// AND with a constant subquery constraining a second index column
	// (where-21.1 shape): a=(subquery) AND (b=1 OR c=1).
	setup5 := `
		CREATE TABLE t12(a, b, c);
		CREATE TABLE t13(x);
		CREATE INDEX t12ab ON t12(b, a);
		CREATE INDEX t12ac ON t12(c, a);
		INSERT INTO t12 VALUES(4, 0, 1);
		INSERT INTO t12 VALUES(4, 1, 0);
		INSERT INTO t12 VALUES(5, 0, 1);
		INSERT INTO t12 VALUES(5, 1, 0);
		INSERT INTO t13 VALUES(1), (2), (3), (4);
	`
	runSQL(t, db, setup5)
	for _, q := range []string{
		"SELECT * FROM t12 WHERE a = (SELECT count(*) FROM t13) AND (b=1 OR c=1)",
		"SELECT * FROM t12 WHERE a = (SELECT count(*) FROM t13) AND (b=1 OR c=1) ORDER BY rowid",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup5)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}

	// Non-indexable OR branches: correct row set, table scan order.
	setup6 := `
		CREATE TABLE n(x, y, z);
		CREATE INDEX nyz ON n(y, z);
		INSERT INTO n VALUES(1, 2, 3);
		INSERT INTO n VALUES(4, 5, 6);
	`
	runSQL(t, db, setup6)
	for _, q := range []string{
		"SELECT x FROM n WHERE z=3 OR x=4",
		"SELECT x FROM n WHERE y=2 OR z=3 OR x=4",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup6)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}

// TestOrIndexOptimizationAffinity covers affinity edge cases in OR index
// prefix matching: a NUMERIC-affinity column compared against text literals,
// and unary-plus (no-affinity) comparisons.
func TestOrIndexOptimizationAffinity(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	setup := `
		CREATE TABLE af(a NUMERIC, b TEXT);
		CREATE INDEX afa ON af(a);
		CREATE INDEX afb ON af(b);
		INSERT INTO af VALUES('123', 'abc');
		INSERT INTO af VALUES(456, 'def');
		INSERT INTO af VALUES('789', 'ghi');
	`
	runSQL(t, db, setup)
	for _, q := range []string{
		"SELECT a, b FROM af WHERE a='123' OR b='ghi'",
		"SELECT a, b FROM af WHERE a='456' OR a=789",
		"SELECT a, b FROM af WHERE +a='123' OR b='def'",
	} {
		got := queryRows(t, db, q)
		want := oracle(t, q, setup)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %#v\n want %#v", q, got, want)
		}
	}
}
