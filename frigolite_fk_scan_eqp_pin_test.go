package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// Pins for the FK child-check EXPLAIN QUERY PLAN contract (e_fkey-26.x,
// fkey.c fkScanChildren): the child scan a parent DELETE/UPDATE runs is
// planned through the NORMAL query planner with equality terms on the child
// key columns, so an index on those columns renders
// "SEARCH <child> USING [COVERING] INDEX <idx> (...)" and the constraint
// list follows the INDEX's column order (wherecode.c explainIndexRange),
// not the WHERE's textual order.
func setupFKScanDB(t *testing.T, childIndex string) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"CREATE TABLE parent(x, y, UNIQUE(y, x))",
		"CREATE TABLE child(a, b, FOREIGN KEY(a, b) REFERENCES parent(x, y))",
		childIndex,
		"PRAGMA foreign_keys = ON",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	return db
}

// eqpDetails returns the EQP detail lines (no "QUERY PLAN" header, tree
// markers trimmed) for one statement.
func eqpDetails(t *testing.T, db *frigolite.DB, sql string) []string {
	t.Helper()
	r := db.Query("EXPLAIN QUERY PLAN " + sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	var out []string
	for i, row := range r.Rows {
		if i == 0 {
			continue // "QUERY PLAN" header
		}
		d := strings.TrimLeft(fmtRow(row), " \t|-`")
		d = strings.TrimSpace(d)
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

func fmtRow(row []interface{}) string {
	cells := make([]string, 0, len(row))
	for _, c := range row {
		cells = append(cells, strings.TrimSpace(toStringCell(c)))
	}
	return strings.Join(cells, " ")
}

func toStringCell(c interface{}) string {
	if s, ok := c.(string); ok {
		return s
	}
	return ""
}

// TestFKScanEQPCoveringIndex pins the FK child-check plan for an index on the
// FK columns in FK order (a,b): DELETE FROM parent plans
// "SEARCH child USING COVERING INDEX childi (a=? AND b=?)" after "SCAN parent".
func TestFKScanEQPCoveringIndex(t *testing.T) {
	db := setupFKScanDB(t, "CREATE INDEX childi ON child(a, b)")
	got := eqpDetails(t, db, "DELETE FROM parent WHERE 1")
	want := []string{
		"SCAN parent",
		"SEARCH child USING COVERING INDEX childi (a=? AND b=?)",
	}
	if len(got) != len(want) {
		t.Fatalf("plan nodes: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("node %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestFKScanEQPReversedIndexOrder pins that the constraint list renders in
// INDEX column order: with a UNIQUE index on child(b,a), both the standalone
// lookup and the FK scan render "(b=? AND a=?)".
func TestFKScanEQPReversedIndexOrder(t *testing.T) {
	db := setupFKScanDB(t, "CREATE UNIQUE INDEX childi ON child(b, a)")
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{
			"SELECT rowid FROM child WHERE a = 1 AND b = 2",
			[]string{"SEARCH child USING COVERING INDEX childi (b=? AND a=?)"},
		},
		{
			"DELETE FROM parent WHERE 1",
			[]string{
				"SCAN parent",
				"SEARCH child USING COVERING INDEX childi (b=? AND a=?)",
			},
		},
	} {
		got := eqpDetails(t, db, tc.sql)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: plan nodes: got %v want %v", tc.sql, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("%s: node %d: got %q want %q", tc.sql, i, got[i], tc.want[i])
			}
		}
	}
}

// TestFKScanEQPOffPlanPlain pins that with foreign_keys OFF no child scan
// node is planned (the FK counters are compile-time gated on the pragma).
func TestFKScanEQPOffPlanPlain(t *testing.T) {
	db := setupFKScanDB(t, "CREATE INDEX childi ON child(a, b)")
	if r := db.Exec("PRAGMA foreign_keys = OFF"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := eqpDetails(t, db, "DELETE FROM parent WHERE 1")
	want := []string{"SCAN parent"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("plan nodes: got %v want %v", got, want)
	}
}
