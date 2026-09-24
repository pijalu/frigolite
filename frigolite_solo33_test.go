package frigolite

import (
	"os"
	"testing"
)

// TestSolo33_ReindexTablesWithoutIndexes pins REINDEX target resolution for
// tables with zero schema index entries (build.c sqlite3Reindex →
// sqlite3FindTable resolves the target as a TABLE; reindexTable iterates the
// possibly-empty pIndex list and skips IsVirtual tables — a successful
// no-op, never "unable to identify the object to be reindexed").
//
// Regression context: rtree1-17.1 (REINDEX t1 on an rtree virtual table)
// regressed when REINDEX gained physical index rebuilds — the resolver only
// matched INDEX schema entries and fell through to the error for a vtab
// (rtree PK/UNIQUE constraints materialize no sqlite_autoindex, mirroring
// SQLite). Oracle-verified against sqlite3 3.54.
func TestSolo33_ReindexTablesWithoutIndexes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	db, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	create := "CREATE VIRTUAL TABLE t1 USING rtree(id, x1 PRIMARY KEY, x2, y1, y2);" +
		"CREATE VIRTUAL TABLE t2 USING rtree(id, x1, x2, y1, y2 UNIQUE);"
	if res := db.Exec(create); res.Error != nil {
		t.Fatalf("create rtree vtabs: %v", res.Error)
	}
	// The engine must materialize no auto-indexes for vtab PK/UNIQUE
	// (module-delegated constraints), matching sqlite3 3.54 sqlite_master.
	r := db.Query("SELECT count(*) FROM sqlite_master WHERE type='index'")
	if r.Error != nil {
		t.Fatalf("schema index count: %v", r.Error)
	}
	if n := r.Rows[0][0]; n != int64(0) {
		t.Errorf("vtab PK/UNIQUE must not create auto-indexes; got %v entries", n)
	}

	// rtree1-17.1: named virtual tables resolve; rebuild is a no-op.
	if res := db.Exec("REINDEX t1; REINDEX t2;"); res.Error != nil {
		t.Errorf("REINDEX rtree vtab: %v", res.Error)
	}
	// rtree1-17.2: untargeted REINDEX covers every schema; virtual tables
	// are skipped, not errors.
	if res := db.Exec("REINDEX;"); res.Error != nil {
		t.Errorf("REINDEX all: %v", res.Error)
	}
	// A plain table with no indexes is likewise a successful no-op.
	if res := db.Exec("CREATE TABLE plain33(a, b); REINDEX plain33;"); res.Error != nil {
		t.Errorf("REINDEX indexless table: %v", res.Error)
	}
	// An unknown target still reports the build.c error text.
	if res := db.Exec("REINDEX solo33_bogus"); res.Error == nil || res.Error.Error() != "unable to identify the object to be reindexed" {
		t.Errorf("REINDEX bogus: want 'unable to identify the object to be reindexed', got %v", res.Error)
	}
}
