package frigolite

import (
	"strings"
	"testing"
)

// P6 pre-tests: hand-written tests for G6.MISC root causes, written BEFORE
// running the TCL testgen packages. Each test mirrors a specific testgen
// failure (e.g. tkt_8454a207b9) and documents the engine bug it covers.

// TestP6_AlterAddColumnDefault covers tkt_8454a207b9: ALTER TABLE ADD COLUMN
// with a DEFAULT expression must apply the default to pre-existing rows at
// read time, with the column's affinity applied (so typeof() reflects the
// stored/effective value).
func TestP6_AlterAddColumnDefault(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Column with TEXT affinity and numeric default: value is coerced to text.
	if err := db.Exec(`
		CREATE TABLE t1(a);
		INSERT INTO t1 VALUES(1);
		ALTER TABLE t1 ADD COLUMN b TEXT DEFAULT -123.0;
	`).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := flattenQuery(t, db, "SELECT b, typeof(b) FROM t1")
	if got != "-123.0 text" {
		t.Errorf("TEXT DEFAULT -123.0: got [%s], want [-123.0 text]", got)
	}

	// Unary minus on a string literal evaluates to 0, then TEXT affinity -> "0".
	if err := db.Exec("ALTER TABLE t1 ADD COLUMN c TEXT DEFAULT -'hello';").Error; err != nil {
		t.Fatalf("add c: %v", err)
	}
	got = flattenQuery(t, db, "SELECT c, typeof(c) FROM t1")
	if got != "0 text" {
		t.Errorf("TEXT DEFAULT -'hello': got [%s], want [0 text]", got)
	}

	// No declared type: no affinity, value stays REAL.
	if err := db.Exec("ALTER TABLE t1 ADD COLUMN e DEFAULT -123.0;").Error; err != nil {
		t.Fatalf("add e: %v", err)
	}
	got = flattenQuery(t, db, "SELECT e, typeof(e) FROM t1")
	if got != "-123.0 real" {
		t.Errorf("DEFAULT -123.0: got [%s], want [-123.0 real]", got)
	}

	// A row inserted AFTER the ADD COLUMN with an explicit value must keep it.
	if err := db.Exec("INSERT INTO t1(a,b,c,e) VALUES(2,'x','y',3.5);").Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	got = flattenQuery(t, db, "SELECT b, typeof(b), e, typeof(e) FROM t1 WHERE a=2")
	if got != "x text 3.5 real" {
		t.Errorf("post-add row: got [%s], want [x text 3.5 real]", got)
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
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
