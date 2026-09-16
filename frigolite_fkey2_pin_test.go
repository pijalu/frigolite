package frigolite_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestFkeyIPKParentKeyPin pins fkey2-4.x: a child FK whose parent key is the
// parent's INTEGER PRIMARY KEY rowid alias (explicit REFERENCES p(b) or
// implicit REFERENCES p) must match the parent row through the alias — the
// parent record stores NULL in the alias slot and the rowid carries the value
// (regression: fkParentRowExists filled rowid-alias slots using the per-FK
// column def subset instead of the full declared column list, so the alias
// slot stayed NULL and every child key was rejected).
func TestFkeyIPKParentKeyPin(t *testing.T) {
	schemas := []string{
		"CREATE TABLE t7(a, b INTEGER PRIMARY KEY); CREATE TABLE t8(c REFERENCES t7, d)",
		"CREATE TABLE t7(a, b INTEGER PRIMARY KEY); CREATE TABLE t8(c REFERENCES t7(b), d)",
		"CREATE TABLE t7(a, b INTEGER PRIMARY KEY); CREATE TABLE t8(c, d, FOREIGN KEY(c) REFERENCES t7)",
	}
	for _, schema := range schemas {
		db, err := frigolite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer db.Close()
			if r := db.Exec("PRAGMA foreign_keys = on"); r.Error != nil {
				t.Fatalf("%s: %v", schema, r.Error)
			}
			if r := db.Exec(schema); r.Error != nil {
				t.Fatalf("%s: %v", schema, r.Error)
			}
			expect := func(sql, wantErr string) {
				t.Helper()
				r := db.Exec(sql)
				if wantErr == "" {
					if r.Error != nil {
						t.Fatalf("%s: %s: unexpected error: %v", schema, sql, r.Error)
					}
					return
				}
				if r.Error == nil || !strings.Contains(r.Error.Error(), wantErr) {
					t.Fatalf("%s: %s: want error %q, got %v", schema, sql, wantErr, r.Error)
				}
			}
			expect("INSERT INTO t8 VALUES(1, 3)", "FOREIGN KEY constraint failed")
			expect("INSERT INTO t7 VALUES(2, 1)", "")
			expect("INSERT INTO t8 VALUES(1, 3)", "")
			expect("INSERT INTO t8 VALUES(2, 4)", "FOREIGN KEY constraint failed")
			expect("INSERT INTO t8 VALUES(NULL, 4)", "")
			expect("UPDATE t8 SET c=2 WHERE d=4", "FOREIGN KEY constraint failed")
			expect("UPDATE t8 SET c=1 WHERE d=4", "")
			expect("UPDATE t8 SET c=NULL WHERE d=4", "")
			expect("DELETE FROM t7 WHERE b=1", "FOREIGN KEY constraint failed")
			expect("UPDATE t7 SET b = 2", "FOREIGN KEY constraint failed")
			expect("UPDATE t7 SET b = 1", "")
			expect("INSERT INTO t8 VALUES('a', 'b')", "FOREIGN KEY constraint failed")
			expect("UPDATE t7 SET b = 5", "FOREIGN KEY constraint failed")
			expect("UPDATE t7 SET rowid = 5", "FOREIGN KEY constraint failed")
			expect("UPDATE t7 SET a = 10", "")
		}()
	}
}

// TestFkViolationKeepsTransactionPin pins fkey2-20.2.3/20.2.4 (and the same
// shape in fkey2-17.1.6/17.1.7): FOREIGN KEY violations always halt with
// OE_Abort — fkey.c hardcodes the conflict action for every
// sqlite3HaltConstraint it emits, so even an INSERT OR ROLLBACK (or UPDATE OR
// ROLLBACK) that violates a child FK rolls back only the STATEMENT; the
// explicit BEGIN stays open and a following COMMIT commits the rows inserted
// earlier in that transaction.
func TestFkViolationKeepsTransactionPin(t *testing.T) {
	for _, stmt := range []string{
		"INSERT",
		"INSERT OR ROLLBACK",
		"INSERT OR ABORT",
		"INSERT OR FAIL",
		"UPDATE OR ROLLBACK",
	} {
		db, err := frigolite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer db.Close()
			must := func(sql string) {
				t.Helper()
				if r := db.Exec(sql); r.Error != nil {
					t.Fatalf("%s: %s: %v", stmt, sql, r.Error)
				}
			}
			must("PRAGMA foreign_keys = on")
			must("CREATE TABLE pp(a PRIMARY KEY, b)")
			must("CREATE TABLE cc(c PRIMARY KEY, d REFERENCES pp)")
			must("INSERT INTO pp VALUES(2, 'two')")
			must("BEGIN")
			// Row inserted inside the transaction BEFORE the failing
			// statement must survive the FK error (statement-scope undo).
			must("INSERT INTO cc VALUES(1, 2)")
			var bad string
			switch {
			case strings.HasPrefix(stmt, "INSERT"):
				bad = stmt + " INTO cc VALUES(3, 4)"
			default:
				must("INSERT INTO cc VALUES(4, 2)")
				bad = "UPDATE OR ROLLBACK cc SET d = 9"
			}
			r := db.Exec(bad)
			if r.Error == nil || !strings.Contains(r.Error.Error(), "FOREIGN KEY constraint failed") {
				t.Fatalf("%s: %s: want FK constraint failure, got %v", stmt, bad, r.Error)
			}
			// The transaction must still be active: COMMIT succeeds and
			// preserves the pre-error rows.
			must("COMMIT")
			rows := db.Query("SELECT c FROM cc ORDER BY c")
			if rows.Error != nil {
				t.Fatalf("%s: %v", stmt, rows.Error)
			}
			want := "1"
			if strings.HasPrefix(stmt, "UPDATE") {
				want = "1 4"
			}
			var got []string
			for _, row := range rows.Rows {
				got = append(got, fmt.Sprint(row[0]))
			}
			if strings.Join(got, " ") != want {
				t.Fatalf("%s: cc rows after COMMIT = %v, want %q", stmt, got, want)
			}
		}()
	}
}

// TestAlterAddColumnRawTextPin pins fkey2-14.1.6: ALTER TABLE ADD COLUMN
// splices the RAW column-definition substring of the ALTER statement into
// the stored CREATE TABLE SQL (alter.c sqlite3AlterFinishAddColumn's verbatim
// pColDef span). The user's constraint-clause order and DEFAULT spelling must
// survive verbatim in sqlite_schema.sql — not a canonical AST re-rendering
// ("e REFERENCES t1 DEFAULT NULL" must not become "e DEFAULT (NULL)
// REFERENCES t1").
func TestAlterAddColumnRawTextPin(t *testing.T) {
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
	must("CREATE TABLE t1(a PRIMARY KEY)")
	must("CREATE TABLE t2(a, b)")
	must("ALTER TABLE t2 ADD COLUMN c REFERENCES t1")
	must("ALTER TABLE t2 ADD COLUMN d DEFAULT NULL REFERENCES t1")
	must("ALTER TABLE t2 ADD COLUMN e REFERENCES t1 DEFAULT NULL")
	must("ALTER TABLE t2 ADD COLUMN h DEFAULT 'text' REFERENCES t1")
	rows := db.Query("SELECT sql FROM sqlite_master WHERE name='t2'")
	if rows.Error != nil || len(rows.Rows) != 1 {
		t.Fatalf("query: %v", rows.Error)
	}
	want := "CREATE TABLE t2(a, b, c REFERENCES t1, d DEFAULT NULL REFERENCES t1, e REFERENCES t1 DEFAULT NULL, h DEFAULT 'text' REFERENCES t1)"
	if got := fmt.Sprint(rows.Rows[0][0]); got != want {
		t.Fatalf("stored sql = %q, want %q", got, want)
	}
}
