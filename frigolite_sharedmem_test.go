package frigolite

import "testing"

// SQLite's shared in-memory databases (file:NAME?mode=memory&cache=shared)
// live exactly as long as at least one connection references them: closing
// the last connection destroys the store, so a later open of the same URI
// starts empty (sqlite3Close drops the memdb at nRef 0). Pin the release
// path: a stale store surviving Close would leak rows into the next open.
func TestSharedMemoryDBReleasedOnLastClose(t *testing.T) {
	uri := "file:frigolite_sharedmem_pin?mode=memory&cache=shared"

	db1, err := Open(uri)
	if err != nil {
		t.Fatalf("open first connection: %v", err)
	}
	if res := db1.Exec("CREATE TABLE t(x)"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db1.Exec("INSERT INTO t VALUES (42)"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}

	db2, err := Open(uri)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	res := db2.Query("SELECT x FROM t")
	if res.Error != nil {
		t.Fatalf("second connection must share the store: %v", res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(42) {
		t.Fatalf("shared rows = %v, want [42]", res.Rows)
	}

	// Closing the first connection keeps the store alive for db2.
	if err = db1.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	res = db2.Query("SELECT x FROM t")
	if res.Error != nil {
		t.Fatalf("store must survive while a connection is open: %v", res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(42) {
		t.Fatalf("rows after first close = %v, want [42]", res.Rows)
	}

	// Closing the last connection destroys the store.
	if err = db2.Close(); err != nil {
		t.Fatalf("close last: %v", err)
	}
	db3, err := Open(uri)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db3.Close()
	if res := db3.Query("SELECT x FROM t"); res.Error == nil {
		t.Fatalf("reopen after last close must start empty, but saw table t")
	}
}
