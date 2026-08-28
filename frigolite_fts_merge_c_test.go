package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// ftsCTmpDB opens a throwaway file-backed database (the FTS flush and merge
// run at COMMIT).
func ftsCTmpDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir() + "/c.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ftsCSeedBatch(t *testing.T, db *DB, tag string, docs int) {
	t.Helper()
	doc := strings.Repeat(tag+" term ", 400)
	var sb strings.Builder
	sb.WriteString("BEGIN;")
	for i := 0; i < docs; i++ {
		sb.WriteString(fmt.Sprintf("INSERT INTO ft VALUES('%s%d');", doc, i))
	}
	sb.WriteString("COMMIT;")
	checkExecOK(t, db.Exec(sb.String()))
}

func ftsCQueryInt(t *testing.T, db *DB, q string) int64 {
	t.Helper()
	res := db.Query(q)
	if res.Error != nil {
		t.Fatalf("query %q: %v", q, res.Error)
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		t.Fatalf("query %q: no rows", q)
	}
	switch v := res.Rows[0][0].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return -1
}

// TestFTSMergeDeletesConsumedBlocks covers SQLite's fts3DeleteSegment: when
// an incremental merge consumes a segment, its %_segments blocks are deleted
// (DELETE FROM %_segments WHERE blockid BETWEEN start AND end) — not leaked
// forever (fts4merge4: the leaked blocks inflated the %_segments btree and
// every later nLeafEst estimate).
func TestFTSMergeDeletesConsumedBlocks(t *testing.T) {
	db := ftsCTmpDB(t)
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE ft USING fts4"))

	// Two multi-block level-0 segments.
	ftsCSeedBatch(t, db, "alpha", 5)
	ftsCSeedBatch(t, db, "beta", 5)
	blocksBefore := ftsCQueryInt(t, db, "SELECT count(*) FROM ft_segments")
	maxBefore := ftsCQueryInt(t, db, "SELECT max(blockid) FROM ft_segments")
	if blocksBefore <= 2 || maxBefore < 3 {
		t.Fatalf("expected multi-block segments, got %d blocks max %d", blocksBefore, maxBefore)
	}

	// Merge the whole level (quota larger than the level).
	checkExecOK(t, db.Exec("INSERT INTO ft(ft) VALUES('merge=1000,2')"))

	// The output segment's blocks only: every source block must be gone.
	count := ftsCQueryInt(t, db, "SELECT count(*) FROM ft_segments")
	oldLeft := ftsCQueryInt(t, db,
		"SELECT count(*) FROM ft_segments WHERE blockid <= "+fmt.Sprint(maxBefore))
	if oldLeft != 0 {
		t.Errorf("merge leaked %d source blocks (blockid <= %d)", oldLeft, maxBefore)
	}
	if count >= blocksBefore {
		t.Errorf("segment block count after merge = %d, before = %d (expected drop)", count, blocksBefore)
	}
}

// TestFTSMergeRepackKeepsRowids covers SQLite's fts3RepackSegdirLevel: the
// surviving rows of a partially-consumed level are renumbered via in-place
// UPDATE idx — their ROWIDS never change (the engine's delete-all +
// re-insert churned rowids and collided with the merge's captured
// segdirNextRowID, fts4merge4 2.2.x).
func TestFTSMergeRepackKeepsRowids(t *testing.T) {
	db := ftsCTmpDB(t)
	checkExecOK(t, db.Exec("CREATE VIRTUAL TABLE ft USING fts4"))

	// Three level-0 segments; a small merge quota consumes the oldest ones
	// partially so survivors remain at level 0.
	ftsCSeedBatch(t, db, "gamma", 3)
	ftsCSeedBatch(t, db, "delta", 3)
	ftsCSeedBatch(t, db, "eps", 3)

	rows := db.Query("SELECT rowid, idx FROM ft_segdir WHERE level=0 ORDER BY idx")
	if rows.Error != nil {
		t.Fatalf("segdir: %v", rows.Error)
	}
	if len(rows.Rows) < 3 {
		t.Fatalf("want 3 level-0 segments, got %d", len(rows.Rows))
	}
	rowids := map[string]bool{}
	for _, r := range rows.Rows {
		rowids[fmt.Sprint(r[0])] = true
	}

	// merge=2,2 with a tiny quota: consumes the two oldest segments (or part
	// of them) and leaves at least one survivor.
	checkExecOK(t, db.Exec("INSERT INTO ft(ft) VALUES('merge=2,2')"))

	after := db.Query("SELECT rowid, idx FROM ft_segdir WHERE level=0 ORDER BY idx")
	if after.Error != nil {
		t.Fatalf("segdir after: %v", after.Error)
	}
	if len(after.Rows) == 0 {
		t.Fatal("no level-0 survivors")
	}
	var idxs []string
	for _, r := range after.Rows {
		if !rowids[fmt.Sprint(r[0])] {
			t.Errorf("level-0 rowid %v is NEW (repack re-inserted rows); rowids must be preserved", r[0])
		}
		idxs = append(idxs, fmt.Sprint(r[1]))
	}
	for i, ix := range idxs {
		if ix != fmt.Sprint(i) {
			t.Errorf("survivor idx sequence %v, want 0..n-1", idxs)
			break
		}
	}
}
