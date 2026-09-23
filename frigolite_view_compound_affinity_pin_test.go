package frigolite_test

import (
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite"
)

// Pins for the compound-view column affinity contract (unionall-8.4/8.7/8.10,
// sqlite3SubqueryColumnTypes in select.c): a compound SELECT's result column
// takes the LEFTMOST member's non-NONE affinity, then refines to BLOB when a
// later member's expression can produce a conflicting datatype (a TEXT
// affinity meeting a numeric member, and vice versa). Values are NOT
// converted — the affinity governs comparisons only.
//
// Oracle (sqlite3 3.5x), for the schema below:
//
//	PRAGMA table_info(t1)                -> column b has type BLOB
//	SELECT ... FROM t1 WHERE t1.b = '2'  -> no rows   (INTEGER 2 < TEXT '2')
//	SELECT ... FROM t1 WHERE t1.b = 2    -> one row   (2|2)
func setupCompoundViewDB(t *testing.T) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"CREATE TABLE t0(c0 INT)",
		"INSERT INTO t0 VALUES(0)",
		"CREATE TABLE t1_a(a INTEGER PRIMARY KEY, b TEXT)",
		"INSERT INTO t1_a VALUES(1,'one')",
		"INSERT INTO t1_a VALUES(4,'four')",
		"CREATE TABLE t1_b(c INTEGER PRIMARY KEY, d TEXT)",
		"INSERT INTO t1_b VALUES(2,'two')",
		"INSERT INTO t1_b VALUES(5,'five')",
		"CREATE TABLE t1_c(e INTEGER PRIMARY KEY, f TEXT)",
		"INSERT INTO t1_c VALUES(3,'three')",
		"INSERT INTO t1_c VALUES(6,'six')",
		"CREATE VIEW v0(c0) AS SELECT CAST(t0.c0 AS INTEGER) FROM t0",
		"CREATE VIEW t1 AS " +
			"SELECT a, b FROM t1_a UNION ALL " +
			"SELECT c, c FROM t1_b UNION ALL " +
			"SELECT e, f FROM t1_c",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	return db
}

// TestViewCompoundColumnAffinityTableInfo pins PRAGMA table_info on a
// compound view: column b (leftmost member TEXT, later member INTEGER)
// refines to BLOB.
func TestViewCompoundColumnAffinityTableInfo(t *testing.T) {
	db := setupCompoundViewDB(t)
	r := db.Query("PRAGMA table_info(t1)")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	got := flatRows(r)
	want := "0 a INTEGER 0 <nil> 0\n1 b BLOB 0 <nil> 0"
	if got != want {
		t.Errorf("table_info(t1) got %q want %q", got, want)
	}
}

// TestViewCompoundColumnAffinityComparison pins that the compound affinity
// governs comparisons on the view AND on a materialized derived table built
// over it: b='2' (text) never matches the raw INTEGER 2, b=2 does.
func TestViewCompoundColumnAffinityComparison(t *testing.T) {
	db := setupCompoundViewDB(t)
	derived := "SELECT * FROM (SELECT t1.a, t1.b, t0.c0 AS c, v0.c0 AS d FROM t0 LEFT JOIN v0 ON v0.c0>'0',t1)"
	for _, tc := range []struct{ query, want string }{
		{"SELECT t1.a, t1.b FROM t1 WHERE t1.b='2'", ""},
		{"SELECT t1.a, t1.b FROM t1 WHERE t1.b=2", "2 2"},
		{derived + " WHERE b='2'", ""},
		{derived + " WHERE b=2", "2 2 0 <nil>"},
	} {
		r := db.Query(tc.query)
		if r.Error != nil {
			t.Fatalf("%s: %v", tc.query, r.Error)
		}
		if got := flatRows(r); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.query, got, tc.want)
		}
	}
}
