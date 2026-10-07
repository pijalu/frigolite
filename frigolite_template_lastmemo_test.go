package frigolite_test

import (
	"fmt"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// TestPinTemplateLastMemo pins the template cache's single-entry memo: a
// same-shape statement stream hits the memo on every statement (the entry is
// verified against the statement's literal spans, then served), and an
// alternating two-shape stream misses it on every statement, falling through
// to the hash-keyed map. Either path must serve exactly what a fresh parse
// of the current text produces — a stale memo (the previous shape's entry)
// would substitute the wrong literals into one of the two shapes and the
// per-step result checks below would catch it.
func TestPinTemplateLastMemo(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	queryInt := func(sql string) int64 {
		t.Helper()
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
		if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
			t.Fatalf("%s: want 1x1 row, got %v", sql, r.Rows)
		}
		v, ok := r.Rows[0][0].(int64)
		if !ok {
			t.Fatalf("%s: want int64, got %T", sql, r.Rows[0][0])
		}
		return v
	}
	must("CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)")

	// Same-shape stream: memo hit per statement (map path skipped).
	for i := 1; i <= 60; i++ {
		must(fmt.Sprintf("INSERT INTO t VALUES(%d, %d)", i, i*10))
	}
	if got := queryInt("SELECT COUNT(*) FROM t"); got != 60 {
		t.Fatalf("same-shape stream: count=%d", got)
	}
	if got := queryInt("SELECT c FROM t WHERE id=7"); got != 70 {
		t.Fatalf("same-shape stream: c(7)=%d", got)
	}

	// Alternating shapes: memo miss per statement, map path serves. Each
	// step's result is checked, so a memo serving the previous shape's
	// template (wrong literal positions) fails loudly.
	for i := 1; i <= 40; i++ {
		must(fmt.Sprintf("INSERT INTO t VALUES(%d, 0)", 1000+i))
	}
	for i := 1; i <= 40; i++ {
		uid := 1000 + i
		did := 1000 - i
		must(fmt.Sprintf("UPDATE t SET c=%d WHERE id=%d", i*5, uid))
		if got := queryInt(fmt.Sprintf("SELECT c FROM t WHERE id=%d", uid)); got != int64(i*5) {
			t.Fatalf("alternating stream step %d: updated c=%d, want %d", i, got, i*5)
		}
		must(fmt.Sprintf("INSERT INTO t VALUES(%d, %d)", did, i))
		if got := queryInt(fmt.Sprintf("SELECT c FROM t WHERE id=%d", did)); got != int64(i) {
			t.Fatalf("alternating stream step %d: inserted c=%d, want %d", i, got, i)
		}
	}

	// The memo must also survive an exact-text repeat (same literals: the
	// template text matches its own spans).
	for i := 0; i < 10; i++ {
		if got := queryInt("SELECT c FROM t WHERE id=7"); got != 70 {
			t.Fatalf("exact-text repeat %d: c(7)=%d", i, got)
		}
	}

	// State integrity after the whole mixed stream.
	if got := queryInt("SELECT COUNT(*) FROM t"); got != 60+80 {
		t.Fatalf("final count=%d", got)
	}
	var sum int64
	for i := 1; i <= 40; i++ {
		sum += int64(1000 + i)
	}
	if got := queryInt("SELECT SUM(id) FROM t WHERE id>1000"); got != sum {
		t.Fatalf("final sum(ids>1000)=%d, want %d", got, sum)
	}
}
