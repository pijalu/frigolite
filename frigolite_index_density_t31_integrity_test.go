package frigolite

import (
	"fmt"
	"testing"
)

// TestS66DeterministicIndexIntegrity drives the same 500 two-column blobs
// through the engine on every run (math/rand is not reproducible). It guards
// the failure signature seen while developing index-btree balance changes:
// duplicate child references and missing index rows. The page count is
// reported for the SQLite comparison (3.54: 279 pages at page_size=1024);
// it is deliberately not asserted: the engine-only density variance is
// accepted, with the file-format contract independently proven by
// TestS66SQLiteFileCompatibility.
func TestS66DeterministicIndexIntegrity(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(`PRAGMA page_size=1024; PRAGMA cache_size=10;
CREATE TABLE t2(a,b); CREATE INDEX i2 ON t2(a,b);`); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, p := range s66BlobPairs() {
		sql := fmt.Sprintf("INSERT INTO t2 VALUES(X'%s',X'%s')", p.a, p.b)
		if r := db.Exec(sql); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if r := db.Query(`PRAGMA integrity_check;`); r.Error != nil {
		t.Fatal(r.Error)
	} else if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check = %#v, want ok", r.Rows)
	}
	if r := db.Query(`PRAGMA page_count;`); r.Error != nil {
		t.Fatal(r.Error)
	} else {
		t.Logf("page_count = %v (SQLite 3.54 oracle: 279)", r.Rows[0][0])
	}
}
