package frigolite

import (
	"strings"
	"testing"
)

// TestWindowOverEmpty covers the minimal window-function form required by
// the cast-9.0 SQLite compat test: COUNT(*) OVER () inside a view definition
// must parse, serialize back into the stored view SQL, and execute so the
// view's declared columns resolve correctly.
func TestWindowOverEmpty(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := db.Query(`
  CREATE TABLE t0(c0);
  INSERT INTO t0(c0) VALUES (0);
  CREATE VIEW v1(c0, c1) AS
    SELECT CAST(0.0 AS NUMERIC), COUNT(*) OVER () FROM t0;
  SELECT v1.c0 FROM v1, t0 WHERE v1.c0=0;
`)
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(r.Rows), r.Rows)
	}
	got := flattenResult(r)
	if got != "0.0" {
		t.Errorf("result mismatch\n  got:  [%s]\n  want: [0.0]", got)
	}
}

// TestWindowOverStoredSQL verifies the OVER clause survives view SQL
// serialization so the view definition round-trips correctly.
func TestWindowOverStoredSQL(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if r := db.Query("CREATE TABLE t0(c0); CREATE VIEW v1(c0, c1) AS SELECT CAST(0.0 AS NUMERIC), COUNT(*) OVER () FROM t0;"); r.Error != nil {
		t.Fatalf("create error: %v", r.Error)
	}
	r := db.Query("SELECT sql FROM sqlite_master WHERE name='v1'")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("expected 1 schema row, got %d", len(r.Rows))
	}
	stored, _ := r.Rows[0][0].(string)
	if !strings.Contains(stored, "COUNT(*) OVER ()") {
		t.Errorf("stored view SQL lost OVER clause:\n  got: %q", stored)
	}
	if !strings.Contains(stored, "(c0, c1)") {
		t.Errorf("stored view SQL lost declared column list:\n  got: %q", stored)
	}
}

// TestWindowOverPartition verifies that a PARTITION BY window clause parses
// and serializes correctly (full per-group window execution is beyond the
// minimal OVER () scope required by cast-9.0).
func TestWindowOverPartition(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := db.Query("CREATE TABLE t1(g, v); INSERT INTO t1 VALUES ('a', 1), ('a', 2), ('b', 5); CREATE VIEW w1 AS SELECT g, COUNT(*) OVER (PARTITION BY g) FROM t1;")
	if r.Error != nil {
		t.Fatalf("setup error: %v", r.Error)
	}
	r = db.Query("SELECT sql FROM sqlite_master WHERE name='w1'")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("expected 1 schema row, got %d", len(r.Rows))
	}
	stored, _ := r.Rows[0][0].(string)
	if !strings.Contains(stored, "COUNT(*) OVER (PARTITION BY g)") {
		t.Errorf("stored view SQL lost PARTITION BY OVER clause:\n  got: %q", stored)
	}
}
