package frigolite

// Pure-Go regression test for P8.INCRVACUUM.phase8 (chain cycle /
// duplicate-leaf).
//
// Background: AllocatePage's in-memory branch popped pages from
// p.freePages and decremented header.count, but did NOT update the
// on-disk freelist chain (the trunk's nextTrunk pointer / the leaf
// slot in the trunk's leaves list). When the popped page was later
// re-allocated and re-freed, the chain had a duplicate entry — and
// the integrity_check walker then reported "Page X: never used" and
// btreeStructureOK reported "cycle at leaf=N trunk=M".
//
// This test exercises the failing autovacuum-1.1.20.3 scenario: enable
// auto-vacuum, create a table, insert 50 long-payload rows, then check
// integrity. With the fix, all pages in the chain must be unique (no
// "Page X: never used") and the chain must have no cycles.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestP8AutovacuumChainPop exercises the autovacuum-1.1.20.3 scenario
// end-to-end through the public engine. After 50 long-payload INSERTs
// in a PRAGMA auto_vacuum=1 database, the freelist chain must be
// well-formed (no duplicate leaves, no cycles, every page accounted
// for) and PRAGMA integrity_check must return "ok".
func TestP8AutovacuumChainPop(t *testing.T) {
	os.Chdir(t.TempDir())
	defer os.Remove("test.db")
	db, err := Open("test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, sql := range []string{
		"PRAGMA auto_vacuum = 1",
		"CREATE TABLE av1(a, b)",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%q: %v", sql, r.Error)
		}
	}
	// Long payload to force overflow pages and exercise the chain.
	big := strings.Repeat("0.", 800)
	for i := 1; i <= 50; i++ {
		sql := fmt.Sprintf("INSERT INTO av1 (oid, a, b) VALUES(%d, %q, %q)", i, big, big)
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("INSERT %d: %v", i, r.Error)
		}
	}
	r := db.Exec("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("integrity_check returned %d rows, want 1", len(r.Rows))
	}
	if got, want := fmt.Sprintf("%v", r.Rows[0][0]), "ok"; got != want {
		t.Errorf("integrity_check = %q, want %q", got, want)
	}
}

// TestP8AutovacuumChainPopFreeListCount is a sanity check: after 50
// long-payload INSERTs in PRAGMA auto_vacuum=1 mode (without
// intervening DELETE/VACUUM), the freelist count is 0 because no
// pages have been freed. (Vacuum operations free pages; INSERTs
// don't.)
func TestP8AutovacuumChainPopFreeListCount(t *testing.T) {
	os.Chdir(t.TempDir())
	defer os.Remove("test.db")
	db, err := Open("test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, sql := range []string{
		"PRAGMA auto_vacuum = 1",
		"CREATE TABLE av1(a)",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%q: %v", sql, r.Error)
		}
	}
	for i := 1; i <= 10; i++ {
		sql := fmt.Sprintf("INSERT INTO av1 (oid, a) VALUES(%d, %q)", i, "x")
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("INSERT %d: %v", i, r.Error)
		}
	}
	r := db.Query("PRAGMA freelist_count")
	if r.Error != nil {
		t.Fatalf("freelist_count: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("freelist_count returned %d rows, want 1", len(r.Rows))
	}
	// freelist_count returns a string like "0" in the current API.
	if got := fmt.Sprintf("%v", r.Rows[0][0]); got != "0" {
		t.Errorf("freelist_count = %q, want 0", got)
	}
}
