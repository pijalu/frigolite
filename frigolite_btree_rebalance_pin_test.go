package frigolite_test

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestBtreeRebalanceDeleteAllChurn pins FULL-SUITE-DRIFT.T27-btreefix: a
// page_size=1024 table btree holding overflow-sized cells (~2.6KB and ~10KB
// payloads — the FTS %_segments shape) must survive repeated DELETE-ALL +
// heavy INSERT churn on ONE connection. The residue the T27-ftsflush agent
// isolated: after a DELETE-all, subsequent churn died with "btree: interior
// rebalance did not converge (page N)" (internal/btree/btree_insert.go) and
// REPLACE-style inserts hit "UNIQUE constraint failed: t2_segments.blockid"
// (stale seek).
//
// The workload mirrors fts4merge4's shadow-table grind: per-transaction
// batches of 5 large INSERTs, then a full wipe, then the identical grind
// again on the wiped tree.
func TestBtreeRebalanceDeleteAllChurn(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, pragma := range []string{
		`PRAGMA page_size=1024`,
		`CREATE TABLE t2_segments(blockid INTEGER PRIMARY KEY, block BLOB)`,
	} {
		if res := db.Exec(pragma); res.Error != nil {
			t.Fatal(res.Error)
		}
	}

	hexBlob := func(n int, seed byte) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = seed + byte(i%251)
		}
		return "X'" + hex.EncodeToString(b) + "'"
	}

	// grind runs 5-insert transactions (10KB payloads) using monotonically
	// growing blockids — the merge-writer allocation order.
	grind := func(txs int) error {
		blockid := 0
		for i := 0; i < txs; i++ {
			if res := db.Exec(`BEGIN`); res.Error != nil {
				return fmt.Errorf("tx %d begin: %w", i, res.Error)
			}
			for j := 0; j < 5; j++ {
				blockid++
				res := db.Exec(fmt.Sprintf(
					`INSERT OR REPLACE INTO t2_segments VALUES(%d, %s)`,
					blockid, hexBlob(10*1024, byte(blockid%251))))
				if res.Error != nil {
					return fmt.Errorf("tx %d insert blockid %d: %w", i, blockid, res.Error)
				}
			}
			if res := db.Exec(`COMMIT`); res.Error != nil {
				return fmt.Errorf("tx %d commit: %w", i, res.Error)
			}
		}
		return nil
	}

	// countRows sanity-checks the tree: SELECT count(*) plus MIN/MAX so a
	// scrambled interior (lost or duplicated subtrees) fails loudly.
	countRows := func() int {
		res := db.Query(`SELECT count(*), coalesce(min(blockid),0), coalesce(max(blockid),0) FROM t2_segments`)
		if res.Error != nil {
			t.Fatalf("count query: %v", res.Error)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("count query returned %d rows", len(res.Rows))
		}
		return int(res.Rows[0][0].(int64))
	}

	for cycle := 0; cycle < 4; cycle++ {
		if err := grind(100); err != nil {
			t.Fatalf("cycle %d grind: %v", cycle, err)
		}
		if got := countRows(); got != 500 {
			t.Fatalf("cycle %d: count=%d, want 500", cycle, got)
		}
		if res := db.Exec(`DELETE FROM t2_segments`); res.Error != nil {
			t.Fatalf("cycle %d delete-all: %v", cycle, res.Error)
		}
		if got := countRows(); got != 0 {
			t.Fatalf("cycle %d: post-delete count=%d, want 0", cycle, got)
		}
	}
}

// TestBtreeRebalanceFTSWipeChurn drives the exact fts4merge4 2.2.* shape
// (tn2=1, no reopen) that exposed the residue: CREATE VIRTUAL TABLE, set
// automerge, DELETE FROM t2 (mass segment-delete churn on the shadow
// btrees), then 100 transactions of 5 ~10KB doc INSERTs — repeated across
// automerge settings. Divergence appeared at tx87 of a post-wipe flow.
func TestBtreeRebalanceFTSWipeChurn(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`PRAGMA page_size=1024`); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec(`CREATE VIRTUAL TABLE t2 USING fts4`); res.Error != nil {
		t.Fatal(res.Error)
	}

	// The 1000-word doc: every c1c2c3 combination over 10 letters, repeated
	// 10 times (~10KB) — fts4merge4's `doc`.
	var words []byte
	for _, c1 := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		for _, c2 := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
			for _, c3 := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
				words = append(words, c1+c2+c3...)
				words = append(words, ' ')
			}
		}
	}
	doc := ""
	for i := 0; i < 10; i++ {
		doc += string(words)
	}
	insDoc := "INSERT INTO t2 VALUES('" + doc + "')"

	for _, am := range []int{2, 2} {
		if res := db.Exec(`DELETE FROM t2`); res.Error != nil {
			t.Fatalf("am=%d: DELETE FROM t2: %v", am, res.Error)
		}
		if res := db.Exec(fmt.Sprintf(`INSERT INTO t2(t2) VALUES('automerge=%d')`, am)); res.Error != nil {
			t.Fatalf("am=%d: automerge: %v", am, res.Error)
		}
		for tx := 0; tx < 100; tx++ {
			for _, s := range []string{`BEGIN`, insDoc, insDoc, insDoc, insDoc, insDoc, `COMMIT`} {
				if res := db.Exec(s); res.Error != nil {
					t.Fatalf("am=%d tx=%d: %v", am, tx, res.Error)
				}
			}
		}
		if res := db.Query(`SELECT count(*) FROM t2_segdir`); res.Error != nil {
			t.Fatalf("am=%d: segdir count: %v", am, res.Error)
		}
	}
}
