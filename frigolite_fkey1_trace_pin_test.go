package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestFkeyActionTracePin pins fkey1-5.2.1: FK ON DELETE/ON UPDATE actions run
// as per-row trigger programs in SQLite, and the legacy sqlite3_trace callback
// fires for EVERY program run — the top-level statement plus one fire per
// parent-row FK-action program (each reporting the top-level statement SQL).
// INSERT OR REPLACE INTO t11 (self-referential ON DELETE CASCADE) traces 3
// times: the REPLACE statement, the REPLACE's implicit delete of (2,1) running
// the FK-action program, and the cascaded delete of (3,2) running it again.
func TestFkeyActionTracePin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var trace []string
	db.SetTraceHook(func(sql string) { trace = append(trace, sql) })
	for _, s := range []string{
		"PRAGMA foreign_keys = on",
		"CREATE TABLE t11(x INTEGER PRIMARY KEY, parent REFERENCES t11 ON DELETE CASCADE)",
		"INSERT INTO t11 VALUES (1, NULL), (2, 1), (3, 2)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	trace = nil
	if r := db.Exec("INSERT OR REPLACE INTO t11 VALUES (2, 3);"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("want FOREIGN KEY constraint failed, got %v", r.Error)
	}
	if len(trace) != 3 {
		t.Fatalf("want 3 trace events (statement + 2 FK-action programs), got %d: %q", len(trace), trace)
	}
	for i, s := range trace {
		if s != "INSERT OR REPLACE INTO t11 VALUES (2, 3);" {
			t.Fatalf("trace[%d] = %q, want the top-level statement text", i, s)
		}
	}
}
