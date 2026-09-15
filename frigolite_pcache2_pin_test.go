package frigolite_test

// Native pin for the engine-visible contract of pcache2.test (FULL-SUITE-
// DRIFT pairs-pager cluster).
//
// C contract (pcache2-1.1..1.3): sqlite3_config_pagecache(6000,100) installs
// a fixed-slot pagecache allocator; sqlite3_status(SQLITE_STATUS_PAGECACHE_USED)
// then reports how many SLOTS the allocator has handed out (2 after db's
// `PRAGMA cache_size=10; SELECT 1 FROM sqlite_master`, 4 after a second
// connection does the same). That counter instruments the C allocator, not
// SQL-visible behavior — the pure-Go engine has no slot allocator, so the
// exact counts stay N-A (skip map pcache2-1.2/pcache2-1.3, same class as
// memsubsys1). The assertions below pin what IS engine-visible:
//
//   - the pcache2-1.2/1.3 statements succeed on fresh databases,
//   - PRAGMA cache_size setter is connection-local (recorded per schema),
//   - two connections on two files hold independent caches,
//   - db.Status("SQLITE_STATUS_PAGECACHE_USED") reports a self-consistent
//     occupancy in pages (current == highwater for this counter model).

import (
	"testing"

	"github.com/pijalu/frigolite"
)

func TestPcache2Contract(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("PRAGMA cache_size=10; SELECT 1 FROM sqlite_master;"); r.Error != nil {
		t.Fatalf("pcache2-1.2 statements: %v", r.Error)
	}
	cur, high := db.Status("SQLITE_STATUS_PAGECACHE_USED")
	if cur < 0 || high < cur {
		t.Fatalf("PAGECACHE_USED not self-consistent: current=%d highwater=%d", cur, high)
	}

	db2, err := frigolite.Open("test2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if r := db2.Exec("PRAGMA cache_size=50; SELECT 1 FROM sqlite_master;"); r.Error != nil {
		t.Fatalf("pcache2-1.3 statements: %v", r.Error)
	}
	// The cache_size settings are per-connection (connection-local pragma).
	q := func(d *frigolite.DB, want int64) {
		r := d.Query("PRAGMA cache_size")
		if r.Error != nil {
			t.Fatalf("PRAGMA cache_size getter: %v", r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != want {
			t.Fatalf("cache_size: got %v, want [[%d]] (connection-local)", r.Rows, want)
		}
	}
	q(db2, 50)
	q(db, 10)
}
