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

// merge4Doc10k builds the fts4merge4 grid document: 1000 distinct 3-character
// words (c1c2c3 over a..j) joined by spaces, repeated 10 times.
func merge4Doc10k() string {
	words := make([]string, 0, 1000)
	for _, a := range "abcdefghij" {
		for _, b := range "abcdefghij" {
			for _, c := range "abcdefghij" {
				words = append(words, string([]rune{a, b, c}))
			}
		}
	}
	doc := strings.Repeat(strings.Join(words, " ")+" ", 10)
	return strings.TrimSuffix(doc, " ")
}

// merge4Grind runs n transactions of BEGIN + five 10KB documents + COMMIT.
func merge4Grind(t *testing.T, db *DB, doc string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		sql := "BEGIN;"
		for j := 0; j < 5; j++ {
			sql += "INSERT INTO t2 VALUES('" + doc + "');"
		}
		sql += "COMMIT;"
		checkExecOK(t, db.Exec(sql))
	}
}

// TestFTS4Merge4AutomergePersistsAcrossReopen pins the fts4merge4 2.2
// openclose contract (T27-ftsflush): the automerge setting is PERSISTED in
// the %_stat id=2 row (fts3_write.c fts3DoAutoincrmerge's SQL_REPLACE_STAT)
// and a REOPENED connection restores it at flush time when its in-memory
// setting is still unknown (sqlite3Fts3PendingTermsFlush's
// p->nAutoincrmerge==0xff restore). Before the fix the reopened flow lost
// automerge and converged by crisis-merge alone ("0:4 1:6" — no level-2
// output, which only the automerge produces at this scale).
//
// Oracle checkpoints (/Users/muaddib/dev/sqlite sqlite3 CLI, page_size 1024,
// identical two-flow script, per-tx diff): flow tx20 "1:3 2:1" and flow2
// tx20 "1:3 2:1" — byte-identical across the reopen.
func TestFTS4Merge4AutomergePersistsAcrossReopen(t *testing.T) {
	if testing.Short() {
		t.Skip("slow automerge grind")
	}
	path := t.TempDir() + "/merge4reopen.db"
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE t2 USING fts4"))
	checkExecOK(t, db.Exec("INSERT INTO t2(t2) VALUES('automerge=2')"))
	// The setting persists as the %_stat id=2 INTEGER row (normalized value).
	res := db.Query("SELECT value FROM t2_stat WHERE id=2")
	if res.Error != nil {
		t.Fatalf("query t2_stat: %v", res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("%%_stat id=2 automerge row: got %v, want [2]", res.Rows)
	}

	doc := merge4Doc10k()
	merge4Grind(t, db, doc, 20)
	if got := merge4SegdirLevels(t, db, "t2"); got != "1:3 2:1" {
		t.Fatalf("flow1 tx20 level distribution: got %q, want %q", got, "1:3 2:1")
	}
	// DELETE-all wipes the index (SQLite's fts3DeleteAll clears the shadow
	// tables including %_stat) and the next grid iteration re-arms automerge.
	checkExecOK(t, db.Exec("DELETE FROM t2"))
	checkExecOK(t, db.Exec("INSERT INTO t2(t2) VALUES('automerge=2')"))
	// The REOPEN: the fresh connection's in-memory automerge state is gone;
	// the flush must restore it from the persisted %_stat id=2 row.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	merge4Grind(t, db, doc, 20)
	if got := merge4SegdirLevels(t, db, "t2"); got != "1:3 2:1" {
		t.Fatalf("reopened flow tx20 level distribution: got %q, want %q (automerge lost across reopen?)", got, "1:3 2:1")
	}
}

// TestFTS4OnePassInTxUpdateRestartFlush pins the fts4onepass-4.0 contract
// (T27-ftsflush): an UPDATE of a row whose docid equals the previous
// operation's docid restarts the pending sequence, so the pending batch
// flushes BEFORE the new terms pend (fts3_write.c fts3PendingTermsDocid:
// iDocid==iPrevDocid && bPrevDelete==0). Oracle per-statement segdir counts
// (testfixture, page_size 1024): insert1=1, insert2=1 (all-NULL document —
// empty pending flushes nothing), update1=1, update2=2, commit=3.
func TestFTS4OnePassInTxUpdateRestartFlush(t *testing.T) {
	db, err := Open(t.TempDir() + "/onepass.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	segdir := func() int {
		t.Helper()
		res := db.Query("SELECT count(*) FROM zt_segdir")
		if res.Error != nil {
			t.Fatalf("query segdir: %v", res.Error)
		}
		return int(res.Rows[0][0].(int64))
	}
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE zt USING fts4(a, b)"))
	checkExecOK(t, db.Exec("INSERT INTO zt(rowid, a, b) VALUES(1, 'unus duo', NULL)"))
	if got := segdir(); got != 1 {
		t.Fatalf("after insert1: segdir=%d, want 1", got)
	}
	checkExecOK(t, db.Exec("INSERT INTO zt(rowid, a, b) VALUES(2, NULL, NULL)"))
	if got := segdir(); got != 1 {
		t.Fatalf("after insert2 (all-NULL doc flushes no segment): segdir=%d, want 1", got)
	}
	checkExecOK(t, db.Exec("BEGIN"))
	checkExecOK(t, db.Exec("UPDATE zt SET b='septum' WHERE rowid = 1"))
	if got := segdir(); got != 1 {
		t.Fatalf("after update1: segdir=%d, want 1", got)
	}
	// The second UPDATE's xUpdate DELETE phase sees iDocid==iPrevDocid with
	// bPrevDelete==0: the restart flush lands update1's terms as their own
	// level-0 segment before update2's terms pend.
	checkExecOK(t, db.Exec("UPDATE zt SET b='octo' WHERE rowid = 1"))
	if got := segdir(); got != 2 {
		t.Fatalf("after update2 (docid-restart flush): segdir=%d, want 2", got)
	}
	checkExecOK(t, db.Exec("COMMIT"))
	if got := segdir(); got != 3 {
		t.Fatalf("after commit: segdir=%d, want 3", got)
	}
}
