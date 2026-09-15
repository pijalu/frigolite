package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteReplaceSecondaryConstraintPin pins tkt-4a03edc4c8: when an
// INSERT resolves a conflict via one constraint's ON CONFLICT REPLACE, a
// conflict on ANOTHER unique constraint keeps its own algorithm (insert.c
// statement journal): FAIL errors and undoes the REPLACE's deletes, IGNORE
// skips the row silently, and a statement-level OR REPLACE overrides both.
func TestSQLiteReplaceSecondaryConstraintPin(t *testing.T) {
	open := func() *frigolite.DB {
		db, err := frigolite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	rows := func(db *frigolite.DB) string {
		r := db.Query("SELECT group_concat(a || ':' || b) FROM t")
		if r.Error != nil {
			t.Fatalf("select: %v", r.Error)
		}
		if len(r.Rows) == 0 || r.Rows[0][0] == nil {
			return ""
		}
		return r.Rows[0][0].(string)
	}

	db := open()
	if r := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b UNIQUE ON CONFLICT FAIL);INSERT INTO t VALUES(1,1);INSERT INTO t VALUES(2,2)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	r := db.Exec("INSERT INTO t VALUES(1,2)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "UNIQUE constraint failed: t.b") {
		t.Fatalf("REPLACE + FAIL: got %v, want t.b UNIQUE error", r.Error)
	}
	if got := rows(db); got != "1:1,2:2" {
		t.Fatalf("REPLACE + FAIL: rows %q, want both originals kept", got)
	}

	db = open()
	if r := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b UNIQUE ON CONFLICT IGNORE);INSERT INTO t VALUES(1,1);INSERT INTO t VALUES(2,2)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(1,2)"); r.Error != nil {
		t.Fatalf("REPLACE + IGNORE must skip silently: %v", r.Error)
	}
	if got := rows(db); got != "1:1,2:2" {
		t.Fatalf("REPLACE + IGNORE: rows %q, want both originals kept", got)
	}

	db = open()
	if r := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY ON CONFLICT REPLACE, b UNIQUE ON CONFLICT FAIL);INSERT INTO t VALUES(1,1);INSERT INTO t VALUES(2,2)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	if r := db.Exec("INSERT OR REPLACE INTO t VALUES(1,2)"); r.Error != nil {
		t.Fatalf("OR REPLACE overrides column FAIL: %v", r.Error)
	}
	if got := rows(db); got != "1:2" {
		t.Fatalf("OR REPLACE: rows %q, want 1:2", got)
	}
}
