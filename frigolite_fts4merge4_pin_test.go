package frigolite

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fts4merge4 pin tests: the automerge level-distribution contract of SQLite
// test/fts4merge4.test 2.2 (page_size 1024 — the engine's DefaultPageSize and
// the SQLite test build's default), driving frigolite.Open/Exec/Query
// directly. The transpiled suite lives in testgen/fts4merge4.
//
// These tests encode the oracle's per-checkpoint segdir level counts, which
// the engine previously abandoned after the first level-1 grind: the merge
// scan reader walked a continuation output's leaves from the ROOT blob's
// first-block varint (a mid-chain interior node id after an interior-node
// split), so reading the level-1 output for the next merge failed with a
// missing block and MergeFTS silently returned — the L1->L2 automerge never
// ran and the level structure diverged (0:12 1:11 instead of 0:4 1:3 2:1).
// SQLite's fts3SegReaderNew walks leaves from %_segdir start_block through
// leaves_end_block and never consults the root for scans.

// merge4SegdirLevels returns "level:count" pairs of the FTS table's segdir
// (e.g. "0:4 1:3 2:1"), sorted by level.
func merge4SegdirLevels(t *testing.T, db *DB, table string) string {
	t.Helper()
	res := db.Query(fmt.Sprintf("SELECT level, count(*) FROM %s_segdir GROUP BY level ORDER BY level", table))
	if res.Error != nil {
		t.Fatalf("query segdir: %v", res.Error)
	}
	parts := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		parts = append(parts, fmt.Sprintf("%v:%v", row[0], row[1]))
	}
	return strings.Join(parts, " ")
}

// TestFTS4Merge4Automerge8Grind runs the fts4merge4 2.2 automerge=8 workload
// at full document scale (1000 distinct words x 10 repeats per document, five
// documents per transaction) and checks the oracle's level distribution at
// the grind checkpoints. Checkpoint values below were produced by
// /usr/bin/sqlite3 (3.54) at page_size 1024 — identical to the expectations
// the TCL test asserts for the same scenario.
func TestFTS4Merge4Automerge8Grind(t *testing.T) {
	if testing.Short() {
		t.Skip("slow automerge grind")
	}
	db, err := Open(t.TempDir() + "/merge4.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t2 USING fts4"))
	checkExecOK(t, db.Exec("INSERT INTO t2(t2) VALUES('automerge=8')"))

	words := make([]string, 0, 1000)
	for _, a := range "abcdefghij" {
		for _, b := range "abcdefghij" {
			for _, c := range "abcdefghij" {
				words = append(words, string([]rune{a, b, c}))
			}
		}
	}
	doc := strings.Repeat(strings.Join(words, " ")+" ", 10)
	doc = strings.TrimSuffix(doc, " ")

	want := map[int]string{
		10: "0:10",
		20: "0:4 1:1",
		40: "0:8 1:4",
		// Oracle checkpoints (/Users/muaddib/dev/sqlite 3.51 instrumented
		// build, AMQ/AMIT/AMCHOMP trace, page_size 1024): the engine's grind
		// now reproduces the oracle's per-transaction level structure exactly
		// (all 100 checkpoints byte-identical), closing the T24 known gap.
		80: "0:8 1:9 2:1",
		// The convergence the TCL suite asserts (fts4merge4 2.2.3: the L1
		// drain completes and one ~7.9MB level-2 output holds the index).
		100: "0:4 1:3 2:1",
	}
	deadline := time.Now().Add(120 * time.Second)
	for i := 1; i <= 100; i++ {
		sql := "BEGIN;"
		for j := 0; j < 5; j++ {
			sql += "INSERT INTO t2 VALUES('" + doc + "');"
		}
		sql += "COMMIT;"
		checkExecOK(t, db.Exec(sql))
		if wantLevels, ok := want[i]; ok {
			if got := merge4SegdirLevels(t, db, "t2"); got != wantLevels {
				t.Fatalf("tx %d level distribution: got %q, want %q", i, got, wantLevels)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("deadline exceeded at tx %d", i)
		}
	}
}

// TestFTS4Merge4SmallScaleCheckpoints pins the small-scale checkpoints of the
// same workload (1000-word documents), where the merge stays level-0-only
// but the per-transaction flush/merge interleaving is already exercised.
func TestFTS4Merge4SmallScaleCheckpoints(t *testing.T) {
	db, err := Open(t.TempDir() + "/merge4s.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t2 USING fts4"))
	checkExecOK(t, db.Exec("INSERT INTO t2(t2) VALUES('automerge=8')"))

	words := make([]string, 0, 1000)
	for _, a := range "abcdefghij" {
		for _, b := range "abcdefghij" {
			for _, c := range "abcdefghij" {
				words = append(words, string([]rune{a, b, c}))
			}
		}
	}
	doc := strings.Join(words, " ")

	want := map[int]string{
		10: "0:10",
		20: "0:4 1:1",
		30: "0:14 1:1",
	}
	for i := 1; i <= 30; i++ {
		sql := "BEGIN;"
		for j := 0; j < 5; j++ {
			sql += "INSERT INTO t2 VALUES('" + doc + "');"
		}
		sql += "COMMIT;"
		checkExecOK(t, db.Exec(sql))
		if wantLevels, ok := want[i]; ok {
			if got := merge4SegdirLevels(t, db, "t2"); got != wantLevels {
				t.Fatalf("tx %d level distribution: got %q, want %q", i, got, wantLevels)
			}
		}
	}
}
