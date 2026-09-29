package frigolite_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

// idxCacheDB opens an in-memory database with table it(k INTEGER PRIMARY
// KEY, c TEXT) and inserts n rows.
func idxCacheDB(t *testing.T, n int) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE it(k INTEGER PRIMARY KEY, c TEXT)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}
	for i := 0; i < n; i++ {
		if r := db.Exec(fmt.Sprintf("INSERT INTO it VALUES(%d, 'v%d')", i, i)); r.Error != nil {
			t.Fatalf("seed %d: %v", i, r.Error)
		}
	}
	return db
}

// mustExecIdx runs one statement, failing the test on any error.
func mustExecIdx(t *testing.T, db *frigolite.DB, q string) {
	t.Helper()
	if r := db.Exec(q); r.Error != nil {
		t.Fatalf("%s: %v", q, r.Error)
	}
}

// TestIndexDefsCacheInvalidationOnCreateIndex inserts through a CREATE INDEX
// that happens mid-loop: rows inserted after the DDL must appear in the new
// index, so an index-driven lookup finds them (and finds ONLY them).
func TestIndexDefsCacheInvalidationOnCreateIndex(t *testing.T) {
	db := idxCacheDB(t, 5)
	// Prime the def cache: every insert so far ran allTableIndexes.
	for i := 5; i < 10; i++ {
		mustExecIdx(t, db, fmt.Sprintf("INSERT INTO it VALUES(%d, 'v%d')", i, i))
	}
	// DDL mid-stream: the next insert must rebuild its index-def list.
	mustExecIdx(t, db, "CREATE INDEX idx_it_c ON it(c)")
	for i := 10; i < 15; i++ {
		mustExecIdx(t, db, fmt.Sprintf("INSERT INTO it VALUES(%d, 'w%d')", i, i))
	}
	// Indexed lookup must see the post-DDL rows.
	r := db.Query("SELECT k FROM it WHERE c = 'w12'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(12) {
		t.Fatalf("indexed lookup after mid-stream CREATE INDEX: %v, want [[12]]", r.Rows)
	}
	// And a pre-DDL value still resolves exactly once.
	r = db.Query("SELECT COUNT(*) FROM it WHERE c = 'v7'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Rows[0][0] != int64(1) {
		t.Fatalf("indexed lookup of pre-DDL row: %v, want [1]", r.Rows[0][0])
	}
}

// TestIndexDefsCacheInvalidationOnDropIndex drops an index mid-stream; the
// dropped index's btree must stop being maintained (recreating it rebuilds
// from the table, and lookups stay correct).
func TestIndexDefsCacheInvalidationOnDropIndex(t *testing.T) {
	db := idxCacheDB(t, 5)
	mustExecIdx(t, db, "CREATE INDEX idx_it_c ON it(c)")
	for i := 5; i < 10; i++ {
		mustExecIdx(t, db, fmt.Sprintf("INSERT INTO it VALUES(%d, 'v%d')", i, i))
	}
	mustExecIdx(t, db, "DROP INDEX idx_it_c")
	// Rows inserted while the index is dropped would corrupt a stale cached
	// def list that kept maintaining it: recreate and check.
	mustExecIdx(t, db, "CREATE INDEX idx_it_c2 ON it(c)")
	for i := 10; i < 15; i++ {
		mustExecIdx(t, db, fmt.Sprintf("INSERT INTO it VALUES(%d, 'w%d')", i, i))
	}
	r := db.Query("SELECT k FROM it WHERE c = 'v7'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(7) {
		t.Fatalf("recreated-index lookup after mid-stream DROP INDEX: %v, want [[7]]", r.Rows)
	}
	r = db.Query("SELECT COUNT(*) FROM it WHERE c >= ''")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Rows[0][0] != int64(15) {
		t.Fatalf("row count through recreated index: %v, want 15", r.Rows[0][0])
	}
}

// TestIndexDefsCacheInvalidationOnUniqueIndex adds a UNIQUE index mid-stream
// (after the def cache is primed by plain inserts): the new index must be
// enforced immediately, and a violating insert must fail atomically.
func TestIndexDefsCacheInvalidationOnUniqueIndex(t *testing.T) {
	db := idxCacheDB(t, 5)
	mustExecIdx(t, db, "CREATE UNIQUE INDEX uq_it_c ON it(c)")
	if r := db.Exec("INSERT INTO it VALUES(6, 'v0')"); r.Error == nil {
		t.Fatalf("duplicate insert after mid-stream CREATE UNIQUE INDEX succeeded")
	} else if want := "UNIQUE constraint failed: it.c"; r.Error.Error() != want {
		t.Fatalf("duplicate insert error: got %q, want %q", r.Error.Error(), want)
	}
	// The failing statement must not have left a partial write.
	r := db.Query("SELECT COUNT(*) FROM it")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Rows[0][0] != int64(5) {
		t.Fatalf("row count after failed duplicate insert: %v, want 5", r.Rows[0][0])
	}
}

// TestIndexDefsCacheInvalidationOnUpdateDelete drives UPDATE and DELETE
// through a mid-stream CREATE INDEX: index maintenance must apply to the new
// index immediately (stale entries would break indexed lookups both ways).
func TestIndexDefsCacheInvalidationOnUpdateDelete(t *testing.T) {
	db := idxCacheDB(t, 10)
	mustExecIdx(t, db, "CREATE INDEX idx_it_c ON it(c)")
	mustExecIdx(t, db, "UPDATE it SET c = 'u3' WHERE k = 3")
	mustExecIdx(t, db, "DELETE FROM it WHERE k = 4")
	r := db.Query("SELECT k FROM it WHERE c = 'u3'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(3) {
		t.Fatalf("indexed lookup after UPDATE: %v, want [[3]]", r.Rows)
	}
	r = db.Query("SELECT COUNT(*) FROM it WHERE c = 'v4'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Rows[0][0] != int64(0) {
		t.Fatalf("deleted row still in index: %v", r.Rows[0][0])
	}
}

// TestIndexDefsCacheInvalidationOnRename pins ALTER TABLE RENAME moving the
// fingerprint: inserts continue to maintain the (renamed) table's indexes.
func TestIndexDefsCacheInvalidationOnRename(t *testing.T) {
	db := idxCacheDB(t, 5)
	mustExecIdx(t, db, "CREATE INDEX idx_it_c ON it(c)")
	mustExecIdx(t, db, "ALTER TABLE it RENAME TO it2")
	for i := 5; i < 10; i++ {
		mustExecIdx(t, db, fmt.Sprintf("INSERT INTO it2 VALUES(%d, 'v%d')", i, i))
	}
	r := db.Query("SELECT k FROM it2 WHERE c = 'v8'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(8) {
		t.Fatalf("indexed lookup after RENAME: %v, want [[8]]", r.Rows)
	}
}
