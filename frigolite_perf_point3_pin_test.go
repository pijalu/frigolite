package frigolite

import (
	"strconv"
	"strings"
	"testing"
)

// Pins for the PERF.POINT3 tranche: the bare-reference column-name memo, the
// bare-reference output-collation fast path, and the root-parked seek cursor.
// Each pin drives the engine directly (Open/Exec/Query) and pins the
// observable contract the fast paths must preserve.

// TestPinPoint3ColNamesMemo pins the result-column name memo
// (select_point_memo.go): bare-reference projections must keep resolving
// their names from the CURRENT schema after DDL, must stay shape-isolated
// (aliases, qualified refs, stars, expressions take their historical paths),
// and must respect the full_column_names pragma whenever it flips.
func TestPinPoint3ColNamesMemo(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	mustOK := func(stage, sql string) {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: exec %q: %v", stage, sql, res.Error)
		}
	}
	cols := func(stage, sql string) []string {
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: query %q: %v", stage, sql, r.Error)
		}
		return r.Columns
	}
	eq := func(stage string, got, want []string) {
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: columns = %v, want %v", stage, got, want)
		}
	}

	mustOK("setup", "CREATE TABLE t(id INTEGER PRIMARY KEY, val INTEGER, txt TEXT)")
	mustOK("seed", "INSERT INTO t VALUES(1, 2, 'x')")

	// Warm the memo with repeated bare projections.
	for i := 0; i < 3; i++ {
		eq("bare", cols("bare", "SELECT id, val, txt FROM t WHERE id=1"), []string{"id", "val", "txt"})
	}
	// Shape isolation: the same table through other shapes keeps its
	// historical names (the memo must not leak across shapes sharing colDefs).
	eq("alias", cols("alias", "SELECT id AS a FROM t WHERE id=1"), []string{"a"})
	eq("qualified", cols("qualified", "SELECT t.id FROM t WHERE id=1"), []string{"id"})
	eq("star", cols("star", "SELECT * FROM t WHERE id=1"), []string{"id", "val", "txt"})
	eq("expr", cols("expr", "SELECT id+1 FROM t WHERE id=1"), []string{"id+1"})

	// DDL invalidation: a same-named column rename must surface immediately.
	mustOK("rename", "ALTER TABLE t RENAME COLUMN val TO quantity")
	for i := 0; i < 2; i++ {
		eq("post-rename", cols("post-rename", "SELECT id, quantity, txt FROM t WHERE id=1"), []string{"id", "quantity", "txt"})
	}

	// full_column_names flips the name format on the same shape.
	mustOK("pragma", "PRAGMA full_column_names=ON")
	eq("fullcolnames", cols("fullcolnames", "SELECT id, quantity FROM t WHERE id=1"), []string{"t.id", "t.quantity"})
	mustOK("pragma-off", "PRAGMA full_column_names=OFF")
	eq("shortcolnames", cols("shortcolnames", "SELECT id, quantity FROM t WHERE id=1"), []string{"id", "quantity"})
}

// TestPinPoint3OutputCollationsFast pins the bare-reference output-collation
// fast path (selectOutputCollations): the collation list a DISTINCT/ORDER BY
// consumes must keep declared collations, explicit COLLATE, compound-member
// collations, and unresolved names exactly as the general walk computed them.
func TestPinPoint3OutputCollationsFast(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	mustOK := func(stage, sql string) {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: exec %q: %v", stage, sql, res.Error)
		}
	}
	rows1 := func(stage, sql string) int {
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: query %q: %v", stage, sql, r.Error)
		}
		return len(r.Rows)
	}

	// DISTINCT over a NOCASE column groups case-variants through the fast
	// path's declared-collation read.
	mustOK("setup", "CREATE TABLE t(a COLLATE NOCASE, b TEXT)")
	mustOK("seed", "INSERT INTO t VALUES('abc','x')")
	mustOK("seed", "INSERT INTO t VALUES('ABC','y')")
	if got := rows1("distinct-nocase", "SELECT DISTINCT a FROM t"); got != 1 {
		t.Fatalf("DISTINCT over NOCASE rows = %d, want 1", got)
	}
	if got := rows1("distinct-binary", "SELECT DISTINCT b FROM t"); got != 2 {
		t.Fatalf("DISTINCT over BINARY rows = %d, want 2", got)
	}
	// An explicit COLLATE overrides the declared collation (shape the fast
	// path excludes; the general walk keeps ruling it).
	if got := rows1("explicit-collate", "SELECT DISTINCT a COLLATE BINARY FROM t"); got != 2 {
		t.Fatalf("DISTINCT a COLLATE BINARY rows = %d, want 2", got)
	}
	// Compound member: the leftmost member's collation rules the merged rows
	// (t.a is NOCASE, so 'abc' and 'ABC' merge into one distinct value).
	mustOK("setup2", "CREATE TABLE u(a TEXT)")
	mustOK("seed2", "INSERT INTO u VALUES('abc')")
	if got := rows1("union", "SELECT a FROM t UNION SELECT a FROM u"); got != 1 {
		t.Fatalf("UNION rows = %d, want 1", got)
	}
	// DDL invalidation: replacing the table with a BINARY column must stop
	// the NOCASE grouping (fingerprint flush covers the fast path).
	mustOK("replace", "DROP TABLE t")
	mustOK("replace", "CREATE TABLE t(a TEXT, b TEXT)")
	mustOK("seed3", "INSERT INTO t VALUES('abc','x')")
	mustOK("seed3", "INSERT INTO t VALUES('ABC','y')")
	if got := rows1("post-ddl", "SELECT DISTINCT a FROM t"); got != 2 {
		t.Fatalf("DISTINCT after DDL rows = %d, want 2", got)
	}
}

// TestPinPoint3SeekPathValueParity pins that the point-seek fast path
// (root-parked cursor + full record decode + affinity pipeline) returns
// values identical to the scan path over a mixed-type table: NULL, integer,
// float, short text, blob, and overflow-page-length text.
func TestPinPoint3SeekPathValueParity(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	mustOK := func(stage, sql string) {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: exec %q: %v", stage, sql, res.Error)
		}
	}
	long := strings.Repeat("overflow-", 3000) // 27000 bytes: spills to overflow pages
	mustOK("setup", "CREATE TABLE t(id INTEGER PRIMARY KEY, n INTEGER, r REAL, s TEXT, b BLOB, big TEXT)")
	ins := []string{
		"INSERT INTO t VALUES(1, NULL, NULL, NULL, NULL, NULL)",
		"INSERT INTO t VALUES(2, -42, 1.5, 'txt', x'0001FF', '" + long + "')",
		"INSERT INTO t VALUES(3, 0, 0.0, '', x'', 'tail')",
		"INSERT INTO t VALUES(4, 9223372036854775807, -2.25, 'quote''it', x'62696e', '')",
	}
	for _, s := range ins {
		mustOK("insert", s)
	}

	seek := func(id int) []interface{} {
		r := db.Query("SELECT id, n, r, s, b, big FROM t WHERE id=" + strconv.Itoa(id))
		if r.Error != nil {
			t.Fatalf("seek %d: %v", id, r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("seek %d: rows = %d, want 1", id, len(r.Rows))
		}
		return r.Rows[0]
	}
	scan := func(id int) []interface{} {
		r := db.Query("SELECT id, n, r, s, b, big FROM t WHERE rowid>" + strconv.Itoa(id-1) + " AND rowid<" + strconv.Itoa(id+1))
		if r.Error != nil {
			t.Fatalf("scan %d: %v", id, r.Error)
		}
		if len(r.Rows) != 1 {
			t.Fatalf("scan %d: rows = %d, want 1", id, len(r.Rows))
		}
		return r.Rows[0]
	}
	valEq := func(stage string, got, want interface{}) {
		gb, gok := got.([]byte)
		wb, wok := want.([]byte)
		if gok && wok {
			if string(gb) != string(wb) {
				t.Fatalf("%s: blob mismatch (%d vs %d bytes)", stage, len(gb), len(wb))
			}
			return
		}
		if gok != wok || got != want {
			t.Fatalf("%s: value = %#v, want %#v", stage, got, want)
		}
	}
	for _, id := range []int{1, 2, 3, 4} {
		sv, xv := seek(id), scan(id)
		for i := range sv {
			valEq("id"+strconv.Itoa(id)+" col"+strconv.Itoa(i), sv[i], xv[i])
		}
	}
	// The overflow text round-trips byte-exact through the seek path.
	r := db.Query("SELECT big FROM t WHERE id=2")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatalf("overflow seek: %v", r.Error)
	}
	if got, _ := r.Rows[0][0].(string); got != long {
		t.Fatalf("overflow text length = %d, want %d", len(got), len(long))
	}
	// Missing rowid: empty result, not an error.
	r = db.Query("SELECT id FROM t WHERE id=99")
	if r.Error != nil || len(r.Rows) != 0 {
		t.Fatalf("missing rowid: rows=%d err=%v", len(r.Rows), r.Error)
	}
}
