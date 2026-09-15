package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteEmptyTableNamePin pins tkt-78e04e52ea: a quoted empty identifier
// is a valid zero-length table name — FROM "" selects it while a FROM-less
// SELECT keeps its "no tables specified" error.
func TestSQLiteEmptyTableNamePin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE \"\"(\"\" UNIQUE, x CHAR(100))")
	mustExec("CREATE TABLE t2(x)")
	mustExec("INSERT INTO \"\"(\"\") VALUES(1)")
	mustExec("INSERT INTO t2 VALUES(2)")

	r := db.Query("SELECT * FROM \"\", t2")
	if r.Error != nil {
		t.Fatalf("FROM \"\": %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(r.Rows))
	}

	// A FROM-less SELECT keeps its error.
	r = db.Query("SELECT *")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no tables specified") {
		t.Fatalf("FROM-less SELECT *: got %v, want no-tables error", r.Error)
	}

	// The zero-length table's UNIQUE constraint reports the empty name.
	r = db.Exec("INSERT INTO \"\"(\"\") VALUES(1)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "UNIQUE constraint failed: .") {
		t.Fatalf("duplicate insert: got %v, want UNIQUE error naming empty column", r.Error)
	}
}
