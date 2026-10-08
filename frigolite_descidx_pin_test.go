package frigolite_test

// Native pin for the DESC-index range-scan contract (R13-L1). Two TCL
// fixtures disagree about it: descidx1.test expects a DESC index's range scan
// to walk the index in its stored (descending) order, descidx2.test — whose
// own comment reads "verify that the DESC on the index is ignored" — expects
// the 2005-era behavior where the DESC flag was not honored. The pinned
// oracle (sqlite3 3.54.0) honors DESC: `SELECT b FROM t1 WHERE a>3 AND a<7`
// over `CREATE INDEX i2 ON t1(a DESC)` returns 6,5,4, and descidx2's
// expectations are the stale side. Every value below is oracle-verified.

import (
	"testing"

	"github.com/pijalu/frigolite"
)

// descRangeFixture creates the descidx1/descidx2 shape: t1(a,b) with an
// ascending index on b and a descending one on a, holding rows (1..7,1..7).
func descRangeFixture(t *testing.T) *frigolite.DB {
	t.Helper()
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	setup := `CREATE TABLE t1(a,b);
		CREATE INDEX i1 ON t1(b ASC);
		CREATE INDEX i2 ON t1(a DESC);
		INSERT INTO t1 VALUES(1,1);
		INSERT INTO t1 VALUES(2,2);
		INSERT INTO t1 SELECT a+2, a+2 FROM t1;
		INSERT INTO t1 SELECT a+4, a+4 FROM t1;`
	if r := db.Exec(setup); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	return db
}

// TestDescIndexRangeOrder pins the DESC index's range-scan emission order
// (descidx1-2.1/2.3/2.4/2.5 and descidx2-2.1/2.3/2.4/2.5, whose expectations
// differ; the oracle sides with descidx1) plus the ascending index's order
// (descidx1-2.2, where both fixtures agree).
func TestDescIndexRangeOrder(t *testing.T) {
	db := descRangeFixture(t)
	cases := []struct {
		sql  string
		want []int64
	}{
		// DESC index on a: the range walks the index in stored order.
		{"SELECT b FROM t1 WHERE a>3 AND a<7", []int64{6, 5, 4}},
		{"SELECT b FROM t1 WHERE a>=3 AND a<7", []int64{6, 5, 4, 3}},
		{"SELECT b FROM t1 WHERE a>3 AND a<=7", []int64{7, 6, 5, 4}},
		{"SELECT b FROM t1 WHERE a>=3 AND a<=7", []int64{7, 6, 5, 4, 3}},
		// ASC index on b: ascending order, and its own range bounds.
		{"SELECT a FROM t1 WHERE b>3 AND b<7", []int64{4, 5, 6}},
		{"SELECT a FROM t1 WHERE b>=3 AND b<=7", []int64{3, 4, 5, 6, 7}},
	}
	for _, tc := range cases {
		r := db.Query(tc.sql)
		if r.Error != nil {
			t.Fatalf("query %.40q: %v", tc.sql, r.Error)
		}
		if len(r.Rows) != len(tc.want) {
			t.Errorf("%.40q = %v, want %v", tc.sql, r.Rows, tc.want)
			continue
		}
		for i, w := range tc.want {
			if r.Rows[i][0] != w {
				t.Errorf("%.40q = %v, want %v", tc.sql, r.Rows, tc.want)
				break
			}
		}
	}
}

// TestDescIndexMixedRangeOrder pins descidx1-4.6 and descidx3-4.3: a
// multi-column DESC/ASC index serves a mixed equality/range prefix with the
// DESC key leading, so the range rows still emerge in the index's stored
// order (oracle-verified).
func TestDescIndexMixedRangeOrder(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := `CREATE TABLE t2(a,b,c,d);
		CREATE INDEX i3 ON t2(a ASC, b DESC, c ASC);
		INSERT INTO t2 VALUES(2,'two',2.2,'2.2');
		INSERT INTO t2 VALUES(2,'two',2.0,'2.0');
		INSERT INTO t2 VALUES(2,'two',2.1,'2.1');
		INSERT INTO t2 VALUES(2,'two',2.3,'2.3');
		INSERT INTO t2 VALUES(2,'one',9.0,'9.0');`
	if r := db.Exec(setup); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	// Equality on a (ASC) + range on b (DESC): the loop is "(a=? AND b>?)"
	// and the index stores (a asc, b desc, c asc), so the 'two' rows emerge in
	// c-ascending order — the oracle returns 2.0 2.1 2.2 2.3.
	r := db.Query("SELECT d FROM t2 WHERE a=2 AND b>='two'")
	if r.Error != nil {
		t.Fatalf("query: %v", r.Error)
	}
	want := []string{"2.0", "2.1", "2.2", "2.3"}
	if len(r.Rows) != len(want) {
		t.Fatalf("rows=%v, want %v", r.Rows, want)
	}
	for i, w := range want {
		if got, ok := r.Rows[i][0].(string); !ok || got != w {
			t.Errorf("row %d = %v, want %q", i, r.Rows[i][0], w)
		}
	}
}
