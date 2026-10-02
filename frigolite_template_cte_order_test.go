package frigolite_test

import (
	"testing"

	"github.com/pijalu/frigolite"
)

// The template cache normalizes a statement's literal values in TEXT order
// (normalizeSQLScratch scans left-to-right) and the clone walker must consume
// them in the same order. The WITH clause of an INSERT precedes the INSERT
// keyword, so its literals come first — before the VALUES tuples or the
// SELECT body's. An INSERT clone that walked its CTEs last cross-assigned the
// values (the value count still matched, so nothing declined): the anchor
// kept the previous statement's literal, the guard's numeric slot received a
// string (an always-true integer<text comparison), and a recursive CTE ran to
// its 1M-row limit while duplicating garbage rows into the target table.
//
// These tests drive two structurally identical statements that differ ONLY in
// their literals — the exact shape that takes the template-cache clone path.

// TestTemplateInsertCTEOrderWRTable is the P1 regression pin: two consecutive
// WITH ... INSERT INTO <without-rowid table> SELECT statements whose CTE
// bounds differ. Before the walk-order fix the second statement executed the
// first one's CTE (mutated literals), duplicated ~1M garbage rows, and
// silently bypassed the PK conflict check.
func TestTemplateInsertCTEOrderWRTable(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT) WITHOUT ROWID"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	const stmt1 = "WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 500) INSERT INTO t1 SELECT i, 'row' || i FROM s"
	if r := db.Exec(stmt1); r.Error != nil {
		t.Fatalf("insert 1: %v", r.Error)
	}
	const stmt2 = "WITH s(i) AS (SELECT 501 UNION ALL SELECT i+1 FROM s WHERE i < 5000) INSERT INTO t1 SELECT i, 'row' || i FROM s"
	if r := db.Exec(stmt2); r.Error != nil {
		t.Fatalf("insert 2: %v", r.Error)
	}
	r := db.Query("SELECT count(*) FROM t1")
	if r.Error != nil {
		t.Fatalf("count: %v", r.Error)
	}
	if got := r.Rows[0][0].(int64); got != 5000 {
		t.Fatalf("count(*) = %d, want 5000", got)
	}
	r = db.Query("SELECT b FROM t1 WHERE a = 4999")
	if r.Error != nil {
		t.Fatalf("point lookup: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "row4999" {
		t.Fatalf("a=4999 lookup got %v, want [row4999]", r.Rows)
	}
	r = db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	}
	if got := r.Rows[0][0]; got != "ok" {
		t.Fatalf("integrity_check = %v, want ok", got)
	}
}

// TestTemplateInsertCTEOrderRowidTable pins the same clone-order contract on
// a rowid table: the second statement's CTE must run with ITS literals
// (anchor 501, bound 5000), so the statement inserts rows 501..5000 without
// touching the first statement's 1..500.
func TestTemplateInsertCTEOrderRowidTable(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t2(a INTEGER PRIMARY KEY, b TEXT)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	if r := db.Exec("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 500) INSERT INTO t2 SELECT i, 'row' || i FROM s"); r.Error != nil {
		t.Fatalf("insert 1: %v", r.Error)
	}
	r := db.Exec("WITH s(i) AS (SELECT 501 UNION ALL SELECT i+1 FROM s WHERE i < 5000) INSERT INTO t2 SELECT i, 'row' || i FROM s")
	if r.Error != nil {
		t.Fatalf("insert 2: %v", r.Error)
	}
	if r.Changes != 4500 {
		t.Fatalf("changes = %d, want 4500", r.Changes)
	}
	q := db.Query("SELECT count(*), min(a), max(a) FROM t2")
	if q.Error != nil {
		t.Fatalf("count: %v", q.Error)
	}
	if got := q.Rows[0][0].(int64); got != 5000 {
		t.Fatalf("count(*) = %d, want 5000", got)
	}
	if got := q.Rows[0][1].(int64); got != 1 {
		t.Fatalf("min(a) = %d, want 1", got)
	}
	if got := q.Rows[0][2].(int64); got != 5000 {
		t.Fatalf("max(a) = %d, want 5000", got)
	}
}

// TestTemplateInsertCTEValuesOrder drives WITH ... INSERT ... VALUES, whose
// literal order is CTE body first, tuples second — the same contract with the
// VALUES arm of the clone.
func TestTemplateInsertCTEValuesOrder(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE v(k INTEGER PRIMARY KEY, tag TEXT)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	// Text-order literal slots: CTE anchor, then the tuple's k, then the
	// tuple's tag. The second statement must substitute (20, 12, 'b'): a
	// clone that walks the tuples before the WITH body cross-assigns — the
	// tag slot receives the CTE's numeric 20 and the CTE anchor receives
	// the string 'b'.
	if r := db.Exec("WITH s(x) AS (SELECT 10) INSERT INTO v VALUES(11, 'a')"); r.Error != nil {
		t.Fatalf("insert 1: %v", r.Error)
	}
	if r := db.Exec("WITH s(x) AS (SELECT 20) INSERT INTO v VALUES(12, 'b')"); r.Error != nil {
		t.Fatalf("insert 2: %v", r.Error)
	}
	q := db.Query("SELECT k, tag, typeof(tag) FROM v ORDER BY k")
	if q.Error != nil {
		t.Fatalf("select: %v", q.Error)
	}
	if len(q.Rows) != 2 {
		t.Fatalf("got %d rows, want 2: %v", len(q.Rows), q.Rows)
	}
	if q.Rows[0][0] != int64(11) || q.Rows[0][1] != "a" {
		t.Fatalf("row 0 = %v, want [11 a]", q.Rows[0])
	}
	if q.Rows[1][0] != int64(12) || q.Rows[1][1] != "b" {
		t.Fatalf("row 1 = %v, want [12 b]", q.Rows[1])
	}
}

// TestTemplateInsertCTEOrderBindPath covers the same walk-order contract in
// bind mode: a prepared WITH ... INSERT ... SELECT whose parameter markers
// live in the CTE body and the select body. The plan records occurrences in
// text order, so the clone must consume them in the same order.
func TestTemplateInsertCTEOrderBindPath(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE bb(a INTEGER PRIMARY KEY, b TEXT)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	st, err := db.Prepare("WITH s(i) AS (SELECT ?1 UNION ALL SELECT i+1 FROM s WHERE i < ?2) INSERT INTO bb SELECT i, 'r' || i FROM s")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if r := st.Exec(int64(1), int64(4)); r.Error != nil {
		t.Fatalf("exec 1: %v", r.Error)
	}
	if r := st.Reset(); r != nil {
		t.Fatalf("reset: %v", r)
	}
	if r := st.Exec(int64(10), int64(13)); r.Error != nil {
		t.Fatalf("exec 2: %v", r.Error)
	}
	q := db.Query("SELECT a, b FROM bb ORDER BY a")
	if q.Error != nil {
		t.Fatalf("select: %v", q.Error)
	}
	// The recursive term's WHERE filters INPUT rows, so the output reaches
	// the bound inclusively (SQLite queue algorithm; oracle: 1..4 for i < 4).
	want := [][]interface{}{
		{int64(1), "r1"}, {int64(2), "r2"}, {int64(3), "r3"}, {int64(4), "r4"},
		{int64(10), "r10"}, {int64(11), "r11"}, {int64(12), "r12"}, {int64(13), "r13"},
	}
	if len(q.Rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(q.Rows), len(want), q.Rows)
	}
	for i, row := range want {
		for j, v := range row {
			if q.Rows[i][j] != v {
				t.Fatalf("row %d col %d = %v, want %v", i, j, q.Rows[i][j], v)
			}
		}
	}
}

// TestTemplateSelectCTEOrder pins the top-level SELECT walker (which already
// walked WITH first) against the same two-statement shape for parity.
func TestTemplateSelectCTEOrder(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	const sel1 = "WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 10) SELECT count(*), min(i), max(i) FROM s"
	const sel2 = "WITH s(i) AS (SELECT 11 UNION ALL SELECT i+1 FROM s WHERE i < 50) SELECT count(*), min(i), max(i) FROM s"
	for si, sel := range []string{sel1, sel2, sel1, sel2} {
		q := db.Query(sel)
		if q.Error != nil {
			t.Fatalf("select %d: %v", si, q.Error)
		}
		var wantCount, wantMin, wantMax int64
		if si%2 == 0 {
			wantCount, wantMin, wantMax = 10, 1, 10
		} else {
			wantCount, wantMin, wantMax = 40, 11, 50
		}
		if got := q.Rows[0][0].(int64); got != wantCount {
			t.Fatalf("select %d: count = %d, want %d", si, got, wantCount)
		}
		if got := q.Rows[0][1].(int64); got != wantMin {
			t.Fatalf("select %d: min = %d, want %d", si, got, wantMin)
		}
		if got := q.Rows[0][2].(int64); got != wantMax {
			t.Fatalf("select %d: max = %d, want %d", si, got, wantMax)
		}
	}
}
