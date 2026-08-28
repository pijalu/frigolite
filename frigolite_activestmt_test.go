package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteActiveStatementInterlock reproduces vtabdrop 1.1 at the engine
// level: sqlite3's DROP TABLE runs OP_Destroy, which fails with SQLITE_LOCKED
// ("database table is locked") while another read VM is mid-run
// (src/vdbe.c OP_Destroy: db->nVdbeRead > db->nVDestroy+1). The harness's
// db-eval callback keeps the scanned SELECT in RUN state for the whole row
// loop, so the DROP inside the callback must fail and the table must
// survive.
func TestSQLiteActiveStatementInterlock(t *testing.T) {
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
	mustExec("CREATE VIRTUAL TABLE rt USING rtree(id, x1, x2)")
	mustExec("CREATE TABLE t1(x, y)")
	mustExec("INSERT INTO t1 VALUES(1, 2)")

	// Simulate the harness db-eval callback: the scanned SELECT is an
	// active reader for the duration of the loop.
	db.BeginActiveStatement()
	r := db.Exec("DROP TABLE rt")
	db.EndActiveStatement()
	if r.Error == nil {
		t.Fatalf("DROP TABLE rt during an active read statement must fail")
	}
	if !strings.Contains(r.Error.Error(), "database table is locked") {
		t.Fatalf("want 'database table is locked', got %v", r.Error)
	}

	// The table survives.
	q := db.Query("SELECT name FROM sqlite_master WHERE name='rt'")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if len(q.Rows) != 1 {
		t.Fatalf("rt must survive the failed drop, got %d rows", len(q.Rows))
	}

	// Without an active reader the drop succeeds.
	if r := db.Exec("DROP TABLE rt"); r.Error != nil {
		t.Fatalf("DROP TABLE rt without active readers: %v", r.Error)
	}
}
