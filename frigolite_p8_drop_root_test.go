// Pure-Go native tests for the DROP TABLE / root-page placement bug
// (P8.INCRVACUUM BUG D, autovacuum-2.4.x / 9.x shape).
//
// SQLite's auto-vacuum contract (btree.c):
//   - btreeCreateTable allocates each new root at meta[3]+1 (largest root
//     page + 1), relocating the live page that occupies that position to a
//     fresh page first. All root pages therefore live in [3..meta[3]] and
//     auto-vacuum never relocates or truncates a root.
//   - incrVacuumStep refuses to relocate a PTRMAP_ROOTPAGE page
//     (SQLITE_CORRUPT) and stops when the header freelist count is 0.
//
// The engine allocated roots wherever AllocatePage() handed one out, so
// roots ended up interleaved with data pages; the commit-time drain then
// truncated straight through LIVE tables (DROP TABLE av3 destroyed av4's
// tree: root page 18 gone, pages 3..15 "never used").
package frigolite_test

import (
	"os"
	"testing"

	"github.com/pijalu/frigolite"
)

func p8BugDSetup(t *testing.T, db *frigolite.DB) {
	t.Helper()
	steps := []string{
		"CREATE TABLE av1(x)",
		"INSERT INTO av1 VALUES('" + nChars("abc", 3000) + "')",
		"INSERT INTO av1 VALUES('" + nChars("def", 3000) + "')",
		"INSERT INTO av1 VALUES('" + nChars("ghi", 3000) + "')",
		"INSERT INTO av1 VALUES('" + nChars("jkl", 3000) + "')",
		"CREATE TABLE av2(x)",
		"CREATE TABLE av3(x)",
		"CREATE TABLE av4(x)",
		"INSERT INTO av2 SELECT 'av1' || x FROM av1",
		"INSERT INTO av3 SELECT 'av2' || x FROM av1",
		"INSERT INTO av4 SELECT 'av3' || x FROM av1",
	}
	for _, s := range steps {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
}

func nChars(tag string, n int) string {
	v := tag
	for len(v) < n {
		v += v
	}
	return v[:n]
}

func p8BugDIntegrity(t *testing.T, db *frigolite.DB, tag string) {
	t.Helper()
	ic := db.Query("PRAGMA integrity_check")
	if ic.Error != nil {
		t.Fatalf("%s: integrity_check: %v", tag, ic.Error)
	}
	if len(ic.Rows) == 0 || ic.Rows[0][0] != "ok" {
		t.Fatalf("%s: integrity_check: %v", tag, ic.Rows)
	}
}

// TestP8DropTablesSurviveSiblingDrops pins autovacuum-2.4.1: dropping a
// subset of tables (av2, av1, av3 — inside and outside transactions) must
// never damage a surviving table (av4), and the drain must keep
// integrity_check clean.
func TestP8DropTablesSurviveSiblingDrops(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Exec("PRAGMA auto_vacuum = 1").Error; err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	p8BugDSetup(t, db)

	want := []string{"av3" + nChars("abc", 3000), "av3" + nChars("def", 3000),
		"av3" + nChars("ghi", 3000), "av3" + nChars("jkl", 3000)}
	checkAV4 := func(tag string) {
		t.Helper()
		r := db.Query("SELECT x FROM av4 ORDER BY rowid")
		if r.Error != nil {
			t.Fatalf("%s: select av4: %v", tag, r.Error)
		}
		if len(r.Rows) != len(want) {
			t.Fatalf("%s: av4 rows: got %d rows (%v), want %d", tag, len(r.Rows), r.Rows, len(want))
		}
		for i, w := range want {
			got, _ := r.Rows[i][0].(string)
			if got != w {
				t.Fatalf("%s: av4 row %d: got %q want %q", tag, i, got, w)
			}
		}
	}
	checkAV4("after setup")

	for _, drop := range []string{"DROP TABLE av2", "DROP TABLE av1", "DROP TABLE av3"} {
		if err := db.Exec(drop).Error; err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
		checkAV4("after " + drop)
		p8BugDIntegrity(t, db, "after "+drop)
	}
	// Drop av4 inside a transaction (autovacuum-2.4.1's trailing BEGIN).
	if err := db.Exec("BEGIN").Error; err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := db.Exec("DROP TABLE av4").Error; err != nil {
		t.Fatalf("drop av4 in txn: %v", err)
	}
	if err := db.Exec("COMMIT").Error; err != nil {
		t.Fatalf("commit: %v", err)
	}
	p8BugDIntegrity(t, db, "after drop av4")
}

// TestP8RootPagesInRootBlock pins the btreeCreateTable placement rule: in
// an auto-vacuum database every CREATE TABLE's root page is the previous
// largest root + 1, so all roots form the contiguous block [3..meta[3]]
// and sit BELOW the tables' data pages. With roots interleaved among data
// pages (the old AllocatePage behavior) the commit drain truncates them.
func TestP8RootPagesInRootBlock(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Exec("PRAGMA auto_vacuum = 1").Error; err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	p8BugDSetup(t, db)
	r := db.Query("SELECT name, rootpage FROM sqlite_schema WHERE type='table' AND name LIKE 'av%' ORDER BY name")
	if r.Error != nil {
		t.Fatalf("schema query: %v", r.Error)
	}
	prev := uint32(2)
	for _, row := range r.Rows {
		name, _ := row[0].(string)
		root, _ := row[1].(int64)
		if root != int64(prev+1) {
			t.Fatalf("root page of %s = %d, want %d (roots must be meta[3]+1 sequential)", name, root, prev+1)
		}
		prev = uint32(root)
	}
}
