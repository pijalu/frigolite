package fts4opt

import (
	"fmt"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

func dumpState(t *testing.T, db *frigolite.DB, label string) {
	rows := db.Query("SELECT level, idx, start_block, leaves_end_block, end_block FROM t2_segdir ORDER BY level, idx")
	if rows.Error != nil {
		t.Fatalf("%s segdir: %v", label, rows.Error)
	}
	for _, r := range rows.Rows {
		start := 0
		fmt.Sscan(fmt.Sprint(r[2]), &start)
		le := 0
		fmt.Sscan(fmt.Sprint(r[3]), &le)
		if start >= 1030 && start <= 1042 {
			t.Logf("%s segdir level=%v idx=%v start=%v leavesEnd=%v", label, r[0], r[1], r[2], r[3])
		}
	}
	blk := db.Query("SELECT blockid FROM t2_segments WHERE blockid BETWEEN 1030 AND 1042 ORDER BY blockid")
	if blk.Error != nil {
		t.Fatalf("%s seg query: %v", label, blk.Error)
	}
	t.Logf("%s blocks 1030-1042: %v", label, blk.Rows)
}

func TestZZOptProbe(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(" CREATE TABLE t1(docid, words) "); r.Error != nil {
		t.Fatal(r.Error)
	}
	ftsKJVGenesis(t, db)
	if r := db.Exec(" CREATE VIRTUAL TABLE t2 USING fts4(words, prefix=\"1,2,3\") "); r.Error != nil {
		t.Fatal(r.Error)
	}
	rows := db.Query("SELECT docid, words FROM t1")
	if rows.Error != nil {
		t.Fatal(rows.Error)
	}
	for _, row := range rows.Rows {
		if r := db.Exec("INSERT INTO t2(docid, words) VALUES(" + sqlLiteral(row[0]) + ", " + sqlLiteral(row[1]) + ")"); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	dumpState(t, db, "BEFORE")
	if r := db.Exec(tclPrepareForOptimizeSQL("t2")); r.Error != nil {
		t.Fatalf("prepare: %v", r.Error)
	}
	dumpState(t, db, "AFTER")
	if r := db.Exec(" INSERT INTO t2(t2) VALUES('merge=5,2') "); r.Error != nil {
		t.Fatalf("merge: %v", r.Error)
	}
	dumpState(t, db, "AFTER-MERGE")
}
