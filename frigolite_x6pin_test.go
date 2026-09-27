package frigolite

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestT34X6_FTS4GrowthMergeContinuationPin pins the fts4growth 7.x
// continuation contract natively (the testgen cases 7.4-7.7 are skipped for
// the pre-fix MergeFTS-continuation divergence; this test carries the same
// engine-visible assertions without the TCL scaffolding).
//
// Scenario (fts4growth 7.1-7.6 shape): six bulk level-0 flushes, merge=25,4
// (quota stop — the merge writes ~25 leaf blocks and chomps the sources),
// the output's end_block stripped to its bare block id (the fts4growth 7.3
// UPDATE — fts3ReadEndBlockField then parses (iEnd, nLeafData=0)), a second
// merge=25,4, then full drains. Oracle ground truth (/usr/bin/sqlite3
// 3.54.0, darwin; re-derived 2026-09-27 with the corpus below — every value
// in this file comes from that run, never from frigolite output):
//
//	7.2  level-1 idx0 = (133,158,"4356 -38245"), L0 starts 7/29/51/73/95/117,
//	     segments count=123 sum=118913
//	7.4  the second merge APPENDS to the SAME level-1 segment — exactly one
//	     level-1 row (no idx=1), leaves_end_block extended 158->183, L0 starts
//	     13/35/57/79/101/123, count=112 sum=117951
//	7.5  merge=2500,4 drains level 0 into that one row: single segdir row
//	     (1,0,133,220,"4356"), count=89 sum=116533
//	7.6  merge=2500,2 is a no-op (level 1 holds one segment < nMin=2)
//
// The 7.4 append is the load-bearing behavior (fts3_write.c sqlite3Fts3Incrmerge:
// the hint engages the continuation, fts3IncrmergeLoad re-derives the writer
// from %_segdir + fts3IsAppendable's "WHERE blockid=? AND block IS NULL"
// marker test; a bare integer end_block does NOT block the append). The
// engine's continuation must also work when the in-memory merge context is
// gone — the direct %_segdir UPDATE between the merges forces a state
// reload, so the append is decided from the persisted geometry alone.
func TestT34X6_FTS4GrowthMergeContinuationPin(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	db, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %q: %v", truncateSQL(sql), r.Error)
		}
	}
	// segdirState renders %_segdir as comparable text rows ("level idx start
	// leafend end"), end_block normalized from blob/text/integer.
	segdirState := func(label, want string) {
		t.Helper()
		r := db.Query("SELECT level, idx, start_block, leaves_end_block, end_block FROM x6_segdir ORDER BY level DESC, idx")
		if r.Error != nil {
			t.Fatalf("%s: %v", label, r.Error)
		}
		rows := make([]string, 0, len(r.Rows))
		for _, row := range r.Rows {
			end := ""
			switch v := row[4].(type) {
			case string:
				end = v
			case []byte:
				end = string(v)
			default:
				end = fmt.Sprint(v)
			}
			rows = append(rows, fmt.Sprintf("%v %v %v %v %s", row[0], row[1], row[2], row[3], strings.TrimSpace(end)))
		}
		if got := strings.Join(rows, " | "); got != want {
			t.Errorf("%s segdir:\n  got:  %s\n  want: %s", label, got, want)
		}
	}
	segStats := func(label string, wantCount, wantSum int64) {
		t.Helper()
		r := db.Query("SELECT count(*), coalesce(sum(length(block)),0) FROM x6_segments")
		if r.Error != nil {
			t.Fatalf("%s: %v", label, r.Error)
		}
		gotCount, _ := r.Rows[0][0].(int64)
		gotSum, _ := r.Rows[0][1].(int64)
		if gotCount != wantCount || gotSum != wantSum {
			t.Errorf("%s segments: got count=%d sum=%d, want count=%d sum=%d", label, gotCount, gotSum, wantCount, wantSum)
		}
	}

	exec("PRAGMA page_size=1024")
	exec("CREATE TABLE t1(docid, words)")
	verses := pinCorpusVerses()
	exec("BEGIN TRANSACTION")
	for round := 0; round < 6; round++ {
		for i, v := range verses {
			for k := 0; k < 6; k++ {
				docid := 1000000 + round*100000 + i*10 + k + 1
				exec(fmt.Sprintf("INSERT INTO t1(docid,words) VALUES(%d,'%s')", docid, v))
			}
		}
	}
	exec("COMMIT")
	exec("CREATE VIRTUAL TABLE x6 USING fts4")
	for i := 0; i < 6; i++ {
		exec("INSERT INTO x6 SELECT words FROM t1")
	}
	segdirState("7.1", "0 0 1 22 22 19917 | 0 1 23 44 44 20005 | 0 2 45 66 66 20005 | "+
		"0 3 67 88 88 20005 | 0 4 89 110 110 20005 | 0 5 111 132 132 20005")
	segStats("7.1", 132, 119942)

	exec("INSERT INTO x6(x6) VALUES('merge=25,4')")
	segdirState("7.2", "1 0 133 158 4356 -38245 | 0 0 7 22 22 19917 | 0 1 29 44 44 20005 | "+
		"0 2 51 66 66 20005 | 0 3 73 88 88 20005 | 0 4 95 110 110 20005 | 0 5 117 132 132 20005")
	segStats("7.2", 123, 118913)
	// The pre-allocation marker (fts3IncrmergeWriter's (iEnd, NULL) row) is
	// present and its block column IS NULL (fts3IsAppendable's test).
	r := db.Query("SELECT block IS NULL FROM x6_segments WHERE blockid=4356")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("7.2: marker row at blockid 4356 must exist with NULL block: %v %v", r.Rows, r.Error)
	}

	// fts4growth 7.3: strip the size suffix — end_block becomes a bare
	// integer and the persisted leaf-data accounting is gone.
	exec("UPDATE x6_segdir SET end_block = (SELECT substr(end_block,1,instr(end_block||' ',' ')-1)) WHERE level=1")

	// fts4growth 7.4: the second merge MUST append to the existing output.
	exec("INSERT INTO x6(x6) VALUES('merge=25,4')")
	segdirState("7.4", "1 0 133 183 4356 | 0 0 13 22 22 19917 | 0 1 35 44 44 20005 | "+
		"0 2 57 66 66 20005 | 0 3 79 88 88 20005 | 0 4 101 110 110 20005 | 0 5 123 132 132 20005")
	segStats("7.4", 112, 117951)

	exec("INSERT INTO x6(x6) VALUES('merge=2500,4')")
	segdirState("7.5", "1 0 133 220 4356")
	segStats("7.5", 89, 116533)

	exec("INSERT INTO x6(x6) VALUES('merge=2500,2')")
	segdirState("7.6", "1 0 133 220 4356")
	segStats("7.6", 89, 116533)

	// The drained segment must remain readable and consistent.
	if r := db.Exec("INSERT INTO x6(x6) VALUES('integrity-check')"); r.Error != nil {
		t.Fatalf("integrity-check: %v", r.Error)
	}
}

// pinCorpusVerses returns the pin's fixed document corpus (public-domain KJV
// Genesis verses, as in the SQLite genesis.tcl corpus). The oracle values in
// TestT34X6_FTS4GrowthMergeContinuationPin were derived with exactly these
// strings and the test's docid arithmetic.
func pinCorpusVerses() []string {
	return []string{
		"In the beginning God created the heaven and the earth.",
		"And the earth was without form, and void; and darkness was upon the face of the deep. And the Spirit of God moved upon the face of the waters.",
		"And God said, Let there be light: and there was light.",
		"And God saw the light, that it was good: and God divided the light from the darkness.",
		"And God called the light Day, and the darkness he called Night. And the evening and the morning were the first day.",
		"And the evening and the morning were the third day.",
		"And God blessed them, saying, Be fruitful, and multiply, and fill the waters in the seas, and let fowl multiply in the earth.",
		"And God saw every thing that he had made, and, behold, it was very good. And the evening and the morning were the sixth day.",
		"Thus the heavens and the earth were finished, and all the host of them.",
		"These are the generations of the heavens and of the earth when they were created, in the day that the LORD God made the earth and the heavens,",
		"And Adam knew Eve his wife; and she conceived, and bare Cain, and said, I have gotten a man from the LORD.",
		"And Seth lived an hundred and five years, and begat Enos:",
	}
}

// truncateSQL shortens a statement for error messages.
func truncateSQL(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 100 {
		return s
	}
	return s[:100] + "..."
}
