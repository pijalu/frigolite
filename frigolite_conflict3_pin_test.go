package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// openConflict3Pin opens an in-memory database with recursive triggers on.
func openConflict3Pin(t *testing.T) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("PRAGMA recursive_triggers = true"); r.Error != nil {
		t.Fatalf("pragma: %v", r.Error)
	}
	return db
}

// conflict3Rows returns the tab-free concatenation of all rows of t1 ordered
// by the first column (pin-test helper).
func conflict3Rows(t *testing.T, db *frigolite.DB, q string) []any {
	t.Helper()
	r := db.Query(q)
	if r.Error != nil {
		t.Fatalf("query %q: %v", q, r.Error)
	}
	var out []any
	for _, row := range r.Rows {
		out = append(out, row...)
	}
	return out
}

// TestSQLiteConflict3MultiRowFailPin pins conflict3.test 1.x-11.x: in a
// multi-row VALUES insert, each row is processed in order with its own
// per-column ON CONFLICT clause. When a later row violates a FAIL-clause
// constraint, the statement errors but rows inserted earlier in the same
// statement are RETAINED (insert.c FAIL keeps prior statement changes).
func TestSQLiteConflict3MultiRowFailPin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		create  string
		wantErr string
	}{
		{
			name:    "integer_pk_replace",
			create:  "CREATE TABLE t1(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b UNIQUE ON CONFLICT IGNORE, c UNIQUE ON CONFLICT FAIL)",
			wantErr: "UNIQUE constraint failed: t1.c",
		},
		{
			name:    "int_pk_replace",
			create:  "CREATE TABLE t1(a INT PRIMARY KEY ON CONFLICT REPLACE, b UNIQUE ON CONFLICT IGNORE, c UNIQUE ON CONFLICT FAIL)",
			wantErr: "UNIQUE constraint failed: t1.c",
		},
		{
			name:    "without_rowid",
			create:  "CREATE TABLE t1(a UNIQUE ON CONFLICT REPLACE, b UNIQUE ON CONFLICT IGNORE, c PRIMARY KEY ON CONFLICT FAIL) WITHOUT ROWID",
			wantErr: "UNIQUE constraint failed: t1.c",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openConflict3Pin(t)
			if r := db.Exec(tc.create + ";INSERT INTO t1(a,b,c) VALUES(1,2,3),(2,3,4);INSERT INTO t1(a,b,c) VALUES(3,2,5)"); r.Error != nil {
				t.Fatalf("setup: %v", r.Error)
			}
			r := db.Exec("INSERT INTO t1(a,b,c) VALUES(4,5,6), (5,6,4)")
			if r.Error == nil || !strings.Contains(r.Error.Error(), tc.wantErr) {
				t.Fatalf("multi-row FAIL: got %v, want %q", r.Error, tc.wantErr)
			}
			// FAIL retains the first row of the statement.
			got := conflict3Rows(t, db, "SELECT a,b,c FROM t1 ORDER BY a")
			want := []any{int64(1), int64(2), int64(3), int64(2), int64(3), int64(4), int64(4), int64(5), int64(6)}
			if len(got) != len(want) {
				t.Fatalf("rows after FAIL: %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("rows after FAIL: %v, want %v (mismatch at %d: %v)", got, want, i, got[i])
				}
			}
		})
	}
}

// TestSQLiteConflict3UpdateReplaceTriggerPin pins conflict3.test 13.2: an
// UPDATE OR REPLACE whose REPLACE deletion fires a delete trigger that
// removes rows of the target table (recursive triggers on) aborts the whole
// statement with a generic "constraint failed" error and leaves the table
// unchanged (update.c REPLACE recheck + statement journal).
func TestSQLiteConflict3UpdateReplaceTriggerPin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create string
	}{
		{"without_rowid", "CREATE TABLE t2 (a PRIMARY KEY, b UNIQUE, c UNIQUE) WITHOUT ROWID"},
		{"rowid", "CREATE TABLE t2 (a PRIMARY KEY, b UNIQUE, c UNIQUE)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openConflict3Pin(t)
			if r := db.Exec(tc.create + ";CREATE TRIGGER tr3 AFTER DELETE ON t2 BEGIN DELETE FROM t2; END;" +
				"INSERT INTO t2 VALUES(1,1,1);INSERT INTO t2 VALUES(2,2,2)"); r.Error != nil {
				t.Fatalf("setup: %v", r.Error)
			}
			r := db.Exec("UPDATE OR REPLACE t2 SET c = 0")
			if r.Error == nil || !strings.Contains(r.Error.Error(), "constraint failed") {
				t.Fatalf("UPDATE OR REPLACE: got %v, want constraint failed", r.Error)
			}
			got := conflict3Rows(t, db, "SELECT * FROM t2")
			want := []any{int64(1), int64(1), int64(1), int64(2), int64(2), int64(2)}
			if len(got) != len(want) {
				t.Fatalf("rows after abort: %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("rows after abort: %v, want %v (mismatch at %d)", got, want, i)
				}
			}
		})
	}
}
