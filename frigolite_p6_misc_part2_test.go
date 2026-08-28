package frigolite

import (
	"strings"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

// queryError runs a query and returns its error (nil when the query
// succeeds). Used to assert that invalid SQL is rejected.
func queryError(db *DB, sql string) error {
	return db.Query(sql).Error
}

func flattenQuery(t *testing.T, db *DB, sql string) string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query error: %v\n  sql: %s", r.Error, sql)
	}
	var parts []string
	for _, row := range r.Rows {
		for _, v := range row {
			if v == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, formatSQLiteValue(v))
			}
		}
	}
	return strings.Join(parts, " ")
}

// P6 pre-tests: hand-written tests for G6.MISC root causes, written BEFORE
// running the TCL testgen packages. Each test mirrors a specific testgen
// failure (e.g. tkt_8454a207b9) and documents the engine bug it covers.

// TestP6_AlterAddColumnDefault covers tkt_8454a207b9: ALTER TABLE ADD COLUMN
// with a DEFAULT expression must apply the default to pre-existing rows at
// read time, with the column's affinity applied (so typeof() reflects the
// stored/effective value).

func TestP6_EmptyUnionMember(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`CREATE TABLE t1(c1); INSERT INTO t1 VALUES(101);`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, `SELECT * FROM (SELECT 0 FROM t1 WHERE c1 = 2) UNION ALL SELECT 0`)
	if got != "0" {
		t.Errorf("empty union member: got [%s] want [0]", got)
	}
	got = flattenQuery(t, db, `SELECT 0 IN (SELECT c_0 FROM (SELECT 0 as c_0 FROM t1 WHERE c1 = 2 ORDER BY c_0 desc) as subq_2 UNION ALL SELECT 0 as c_0)`)
	if got != "1" {
		t.Errorf("IN with empty union member: got [%s] want [1]", got)
	}
}

// TestP6_CastTextAffinity covers tkt3527: CAST(x AS VARCHAR(50)) (any TEXT-
// affinity target) converts to text, so compound ORDER BY over mixed
// int/text ElemId values sorts correctly.
func TestP6_CastTextAffinity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	got := flattenQuery(t, db, `SELECT CAST(1 AS VARCHAR(50)), typeof(CAST(1 AS VARCHAR(50)))`)
	if got != "1 text" {
		t.Errorf("CAST AS VARCHAR: got [%s] want [1 text]", got)
	}
	// The tkt3527 shape: CAST(Code AS VARCHAR) values must sort as text.
	if err := db.Exec(`
		CREATE TABLE Element(Code INTEGER PRIMARY KEY, Name VARCHAR(60));
		INSERT INTO Element VALUES(1,'Elem1'),(3,'Elem3');
	`).Error; err != nil {
		t.Fatal(err)
	}
	got = flattenQuery(t, db, `SELECT CAST(Code AS VARCHAR(50)) FROM Element UNION ALL SELECT '1.3' ORDER BY 1`)
	if got != "1 1.3 3" {
		t.Errorf("CAST VARCHAR compound sort: got [%s] want [1 1.3 3]", got)
	}
}

// TestP6_UpdateReplaceConflict covers tkt2832: UPDATE OR REPLACE over rows
// whose new PK value conflicts must delete the conflicting row, not abort
// with 'constraint failed'.
func TestP6_UpdateReplaceConflict(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t1(a PRIMARY KEY);
		INSERT INTO t1 VALUES(2),(1),(3);
	`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE OR REPLACE t1 SET a = 1`).Error; err != nil {
		t.Fatalf("UPDATE OR REPLACE: %v", err)
	}
	got := flattenQuery(t, db, "SELECT * FROM t1")
	if got != "1" {
		t.Errorf("UPDATE OR REPLACE conflict: got [%s] want [1]", got)
	}
}

// TestP6_UniqueIndexNull covers tkt3824: multiple NULL keys are allowed in a
// UNIQUE index (SQLite treats NULLs as distinct).
func TestP6_UniqueIndexNull(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t1(a,b);
		INSERT INTO t1 VALUES(1,NULL),(9,NULL);
		CREATE UNIQUE INDEX t1b ON t1(b);
	`).Error; err != nil {
		t.Fatalf("UNIQUE index with NULLs: %v", err)
	}
	got := flattenQuery(t, db, "SELECT count(*) FROM t1")
	if got != "2" {
		t.Errorf("UNIQUE index NULL rows: got [%s] want [2]", got)
	}
}

// TestP6_BeforeTriggerRowidSentinel covers tkt3832: a BEFORE INSERT trigger
// sees new.<ipk> as -1 for an auto-assigned INTEGER PRIMARY KEY.
func TestP6_BeforeTriggerRowidSentinel(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t1(a INT, b INTEGER PRIMARY KEY);
		CREATE TABLE log(x);
		CREATE TRIGGER t1r1 BEFORE INSERT ON t1 BEGIN INSERT INTO log VALUES(new.b); END;
		INSERT INTO t1 VALUES(NULL,5);
		INSERT INTO t1 SELECT b, a FROM t1 ORDER BY b;
	`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, "SELECT rowid, * FROM log")
	if got != "1 5 2 -1" {
		t.Errorf("BEFORE trigger rowid sentinel: got [%s] want [1 5 2 -1]", got)
	}
}

// TestP6_TextArithmeticPrecision covers tkt_a8a0d2996: text operands with
// integer numeric prefixes subtract exactly (no float round-trip), and a
// decimal point/exponent prefix makes division REAL.
func TestP6_TextArithmeticPrecision(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	got := flattenQuery(t, db, `SELECT '-9223372036854775807x'-'1x'`)
	if got != "-9223372036854775808" {
		t.Errorf("text int subtraction: got [%s] want [-9223372036854775808]", got)
	}
	got = flattenQuery(t, db, `SELECT '1234x'/'10y', '1234x'/'10.y', '1234x'/'1e1y'`)
	if got != "123 123.4 123.4" {
		t.Errorf("text division REAL: got [%s] want [123 123.4 123.4]", got)
	}
}

// TestP6_AutoIncrementSurvivesDelete covers tkt_d82e3f: the AUTOINCREMENT
// sequence persists across DELETE (next rowid continues from the old max).
func TestP6_AutoIncrementSurvivesDelete(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`
		CREATE TABLE t1(a INTEGER PRIMARY KEY AUTOINCREMENT, b);
		INSERT INTO t1 VALUES(null,'abc');
		INSERT INTO t1 VALUES(null,'def');
		DELETE FROM t1;
		INSERT INTO t1 VALUES(null,'ghi');
	`).Error; err != nil {
		t.Fatal(err)
	}
	got := flattenQuery(t, db, "SELECT a FROM t1")
	if got != "3" {
		t.Errorf("AUTOINCREMENT survives DELETE: got [%s] want [3]", got)
	}
}

// TestP6_DropTableDeferredFK covers tkt_b1d3a2e: dropping a parent table
// with DEFERRABLE INITIALLY DEFERRED children fails at COMMIT with
// "FOREIGN KEY constraint failed" (the orphaned child has no parent).
func TestP6_DropTableDeferredFK(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if err := db.Exec(`PRAGMA foreign_keys=ON`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		CREATE TABLE pp1(x PRIMARY KEY);
		CREATE TABLE cc2(y REFERENCES pp1 DEFERRABLE INITIALLY DEFERRED);
		INSERT INTO pp1 VALUES(2200);
		INSERT INTO cc2 VALUES(2200);
		BEGIN;
		DROP TABLE pp1;
	`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("COMMIT").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("DROP TABLE with deferred FK child at COMMIT: got %v, want FK error", err)
	}
}
