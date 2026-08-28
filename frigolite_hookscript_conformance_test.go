package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestHookScriptConformance replays the committed oracle script
// (testdata/hookconformance/script.sql) through frigolite and asserts the
// SQL-observable hook/transaction semantics match /usr/bin/sqlite3's verbatim
// output (testdata/hookconformance/expected_output.txt): changes() /
// total_changes() counters, ROLLBACK undoing an open transaction, and
// RAISE(ABORT/ROLLBACK) error delivery. Expectations come ONLY from the
// oracle transcript (UCL rule U1).
func TestHookScriptConformance(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	type step struct {
		sql     string
		wantErr string // substring of the expected error, "" for success
		query   string // optional query asserted right after this step
		want    []string
	}
	steps := []step{
		{sql: `CREATE TABLE t1(a,b); INSERT INTO t1 VALUES(1,'one');`},
		{sql: `INSERT INTO t1 VALUES(2,'two');`},
		{query: `SELECT 'changes_after_insert', changes();`, want: []string{"changes_after_insert", "1"}},
		{sql: `UPDATE t1 SET b='ONE' WHERE a=1;`},
		{query: `SELECT 'changes_after_update', changes();`, want: []string{"changes_after_update", "1"}},
		{sql: `DELETE FROM t1 WHERE a=2;`},
		{query: `SELECT 'changes_after_delete', changes();`, want: []string{"changes_after_delete", "1"}},
		{query: `SELECT 'total_changes', total_changes();`, want: []string{"total_changes", "4"}},
		{sql: `BEGIN; INSERT INTO t1 VALUES(3,'three'); ROLLBACK;`},
		{query: `SELECT 'after_rollback_count', count(*) FROM t1;`, want: []string{"after_rollback_count", "1"}},
		{query: `SELECT 'after_rollback_changes', changes();`, want: []string{"after_rollback_changes", "1"}},
		{sql: `CREATE TRIGGER t1_no_delete BEFORE DELETE ON t1 BEGIN SELECT RAISE(ABORT,'delete blocked'); END;`},
		{sql: `DELETE FROM t1 WHERE a=1;`, wantErr: "delete blocked"},
		{query: `SELECT 'after_abort_delete', count(*) FROM t1;`, want: []string{"after_abort_delete", "1"}},
	}
	for i, s := range steps {
		if s.sql != "" {
			r := db.Exec(s.sql)
			if s.wantErr == "" {
				if r.Error != nil {
					t.Fatalf("step %d (%q): unexpected error: %v", i, oneLine(s.sql), r.Error)
				}
			} else if r.Error == nil || !strings.Contains(r.Error.Error(), s.wantErr) {
				t.Fatalf("step %d (%q): got error %v, want containing %q", i, oneLine(s.sql), r.Error, s.wantErr)
			}
		}
		if s.query != "" {
			r := db.Query(s.query)
			if r.Error != nil {
				t.Fatalf("%s: %v", s.query, r.Error)
			}
			if len(r.Rows) == 0 {
				t.Fatalf("%s: no rows, want %v", s.query, s.want)
			}
			var got []string
			for _, v := range r.Rows[0] {
				got = append(got, fmt.Sprint(v))
			}
			if strings.Join(got, "|") != strings.Join(s.want, "|") {
				t.Fatalf("%q = %v, want %v", s.query, got, s.want)
			}
		}
	}
}

// oneLine collapses a SQL block to a single line for messages.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
