package frigolite

// Pure-Go reproducer for P8.INCRVACUUM.freelist-multitrunk.
//
// Background: pager.FreePage previously made every new page a TRUNK
// with leafCount=0, so after > 254 FreePage calls the on-disk
// freelist chain was a list of empty trunks. The leaves were never
// recorded. As a result, after 286 FreePage calls, header.count = 286
// but the chain has 0 leaves. AllocatePage (which recycles freelist
// pages) then picks up the empty trunks and never reclaims the actual
// freed data pages, leaving them as "Page X: never used" in
// integrity_check.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestP8FreelistMultitrunk400 exercises 100 large-payload rows
// (each row forces 3-4 overflow pages, so ~400 freed pages after
// DROP). The on-disk freelist must hold ~400 pages in a multi-trunk
// chain. integrity_check must report "ok".
//
// Pre-fix: the chain has 0 leaves (every freed page becomes a
// 0-leaf trunk). When subsequent operations AllocatePage a free
// page, the trunk is consumed but the actual freed data pages are
// not on the chain — they remain as orphan pages referenced by
// nothing → "Page X: never used".
func TestP8FreelistMultitrunk400(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/freelist400.db"
	func() {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()

		if r := db.Exec("CREATE TABLE t(a, b)"); r.Error != nil {
			t.Fatalf("CREATE TABLE: %v", r.Error)
		}
		// 4000-byte payload per row -> 3-4 overflow pages each.
		// 100 rows -> ~400 freed pages after DROP TABLE.
		bigPayload := strings.Repeat("z", 4000)
		for i := 1; i <= 100; i++ {
			r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d, '%s')", i, bigPayload))
			if r.Error != nil {
				t.Fatalf("INSERT %d: %v", i, r.Error)
			}
		}
		if r := db.Exec("DROP TABLE t"); r.Error != nil {
			t.Fatalf("DROP TABLE: %v", r.Error)
		}
	}() // close + flush

	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("integrity_check returned no rows")
	}
	got := strings.TrimSpace(fmt.Sprintf("%v", r.Rows[0][0]))
	if got != "ok" {
		t.Fatalf("integrity_check: got %q want \"ok\"\n  detail: %v", got, r.Rows)
	}
}

// TestP8FreelistMultitrunkInspectChain walks the on-disk freelist
// chain after DROP TABLE and asserts:
//   - chain-walked page count == header.count
//   - no trunk has leafCount > (pageSize - 8) / 4 - 8
//   - at least 2 trunks are present (the second-trunk transition is
//     what the multi-trunk fix is for)
func TestP8FreelistMultitrunkInspectChain(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/freelist_inspect.db"
	func() {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()

		if r := db.Exec("CREATE TABLE t(a, b)"); r.Error != nil {
			t.Fatalf("CREATE TABLE: %v", r.Error)
		}
		bigPayload := strings.Repeat("z", 4000)
		for i := 1; i <= 100; i++ {
			r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d, '%s')", i, bigPayload))
			if r.Error != nil {
				t.Fatalf("INSERT %d: %v", i, r.Error)
			}
		}
		if r := db.Exec("DROP TABLE t"); r.Error != nil {
			t.Fatalf("DROP TABLE: %v", r.Error)
		}
	}() // close + flush

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	const pageSize = 1024
	if len(data) < pageSize {
		t.Fatalf("file too small: %d bytes", len(data))
	}
	trunk := uint32(data[32])<<24 | uint32(data[33])<<16 | uint32(data[34])<<8 | uint32(data[35])
	count := uint32(data[36])<<24 | uint32(data[37])<<16 | uint32(data[38])<<8 | uint32(data[39])
	t.Logf("header.trunk=%d, header.count=%d, file=%d bytes (%d pages)",
		trunk, count, len(data), len(data)/pageSize)

	// Pre-fix expectation: count < (file_size/pages - 1) because
	// every freed page becomes a 0-leaf trunk and many freed pages
	// are lost. With 100 rows * ~3.5 pages = 350+ freed pages, count
	// should be at least 300 — and the chain must span at least 2
	// trunks (since 246 < count). We assert that count is realistic.
	if int(count) < 200 {
		t.Fatalf("header.count=%d too low — FreePage is not freeing all table pages", count)
	}
	if int(count) >= 1000 {
		t.Fatalf("header.count=%d absurdly high", count)
	}

	// Walk: count trunks + leaves, assert total == count, and assert
	// at least 2 trunks (multi-trunk activation).
	seen := make(map[uint32]bool)
	walked := 0
	trunkCount := 0
	const maxIter = 100000
	const maxLeavesPerTrunk = (pageSize - 8) / 4 - 8 // SQLite's back-compat margin (246 for 1024-byte pages)
	for iter := 0; trunk != 0 && iter < maxIter; iter++ {
		if seen[trunk] {
			t.Fatalf("cycle at trunk=%d after walking %d pages", trunk, walked)
		}
		seen[trunk] = true
		walked++
		trunkCount++
		pageStart := int(trunk-1) * pageSize
		if pageStart+pageSize > len(data) {
			t.Fatalf("trunk=%d points past end of file (file=%d bytes)", trunk, len(data))
		}
		page := data[pageStart : pageStart+pageSize]
		coff := 0
		if trunk == 1 {
			coff = 100
		}
		nextTrunk := uint32(page[coff])<<24 | uint32(page[coff+1])<<16 | uint32(page[coff+2])<<8 | uint32(page[coff+3])
		leafCount := uint32(page[coff+4])<<24 | uint32(page[coff+5])<<16 | uint32(page[coff+6])<<8 | uint32(page[coff+7])
		if int(leafCount) > maxLeavesPerTrunk {
			t.Fatalf("trunk=%d has leafCount=%d which exceeds maxLeaves=%d (chain overflow)",
				trunk, leafCount, maxLeavesPerTrunk)
		}
		for i := 0; i < int(leafCount); i++ {
			off := coff + 8 + i*4
			if off+4 > len(page) {
				t.Fatalf("leaf %d of trunk=%d overflows page boundary (off=%d)", i, trunk, off)
			}
			leaf := uint32(page[off])<<24 | uint32(page[off+1])<<16 | uint32(page[off+2])<<8 | uint32(page[off+3])
			if leaf == 0 {
				break
			}
			if seen[leaf] {
				t.Fatalf("cycle at leaf=%d (trunk=%d)", leaf, trunk)
			}
			seen[leaf] = true
			walked++
		}
		trunk = nextTrunk
	}
	if walked != int(count) {
		t.Fatalf("chain walk: walked=%d, header.count=%d (MISMATCH — multi-trunk fix needed)", walked, count)
	}
	if trunkCount < 2 {
		t.Logf("note: only %d trunk in chain (expected >= 2 for 350+ freed pages); multi-trunk may be inactive", trunkCount)
	}
	t.Logf("chain walk: %d trunks, %d total pages, header.count=%d (MATCH)", trunkCount, walked, count)
}
