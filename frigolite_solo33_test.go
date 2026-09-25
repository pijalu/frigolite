package frigolite

import (
	"os"
	"testing"

	"github.com/pijalu/frigolite/internal/vtab"
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

// TestSolo33_FTS3TruncatedDoclistPositionBleed pins the fts3corrupt6-2.1
// contract: an FTS3 leaf doclist whose final entry lacks an end-of-doc
// sentinel (0x00) continues parsing its position list in the node bytes
// beyond the declared doclist length. C's position parsing is
// sentinel-driven (fts3PoslistCopy / fts3GetDeltaVarint read until POS_END),
// while the docid iteration stays bounded by the declared length — so the
// recovered positions belong to the last docid and no new docids come from
// the continuation.
//
// Oracle (sqlite3 3.54, fts3corrupt6.test 2.x): with the crafted root below,
// term "1"'s doclist is 07 82 00 (doc 7, one position, no POS_END); the
// parse bleeds into the following term header recovering column-3 positions
// {48,97,147,150}, which pair under NEAR → '1 NEAR 1' matches row 7 and
// '(1 NEAR 1) AND (aaaa OR 1)' counts 1. A properly terminated doclist
// (fts3corrupt6 1.x root) yields a single position and '1 NEAR 1' matches
// nothing — also pinned here as the negative case.
func TestSolo33_FTS3TruncatedDoclistPositionBleed(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	db, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// fts3corrupt6.test runs with the parenthesis (enhanced) MATCH syntax
	// active: set sqlite_fts3_enable_parentheses 1.
	vtab.TclVarSet("sqlite_fts3_enable_parentheses", "", "1")
	defer vtab.TclVarSet("sqlite_fts3_enable_parentheses", "", "0")

	setup := func(name, root string) {
		if res := db.Exec("CREATE VIRTUAL TABLE " + name + " USING fts3(a); INSERT INTO " +
			name + "_segdir VALUES(0,0,0,0,'0 42',X'" + root + "');"); res.Error != nil {
			t.Fatalf("%s setup: %v", name, res.Error)
		}
	}
	count := func(name, match string) int {
		r := db.Query("SELECT count(*) FROM " + name + " WHERE " + name + " MATCH '" + match + "'")
		if r.Error != nil {
			t.Fatalf("MATCH %q: %v", match, r.Error)
		}
		return int(r.Rows[0][0].(int64))
	}

	// fts3corrupt6 2.0/2.1: truncated doclist — the bleed powers NEAR.
	setup("t0", "000131030782000103323334050100fff200010461616161050101020200000462626262050101030200")
	if n := count("t0", "1"); n != 1 {
		t.Errorf("'1': want docid 7 (count 1), got %d", n)
	}
	if n := count("t0", "(1 NEAR 1) AND (aaaa OR 1)"); n != 1 {
		t.Errorf("fts3corrupt6-2.1: want count 1, got %d", n)
	}

	// fts3corrupt6 1.x root: well-formed doclists — '1 NEAR 1' needs two
	// distinct positions and must match nothing (fts3near 1.14 strictness).
	setup("t1", "000131030102000103323334050101010200000461616161050101020200000462626262050101030200")
	if n := count("t1", "1 NEAR 1"); n != 0 {
		t.Errorf("terminated single position must not self-pair under NEAR; got %d", n)
	}
}
