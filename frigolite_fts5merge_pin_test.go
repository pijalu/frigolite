package frigolite

import (
	"strconv"
	"testing"
)

// Native pin for the superseded testgen/fts5merge package (T33r-fts,
// 2026-09-26). fts5merge's substance: usermerge-configured incremental
// merges ('merge', N work units at 'usermerge' segments per merge) drive a
// level structure to convergence — every level ends with at most one
// segment — an 'integrity-check' passes afterwards, and 'merge' is a no-op
// on an empty table (fts5merge 6.1/6.2). Convergence is the not_merged
// contract at usermerge=2 (C's fts5IndexMerge only starts a level merge
// when the biggest level holds nMin segments, so usermerge=2 is the
// setting under which the corpus's merge-to-convergence loops terminate).
// The generated corpus could not express the convergence conditions
// (non-transpiled not_merged proc, [db total_changes] break expressions).
func TestFTS5UsermergeIncrementalConvergence(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	must := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must(`CREATE VIRTUAL TABLE x8 USING fts5(i);
INSERT INTO x8(x8, rank) VALUES('pgsz', 32);
INSERT INTO x8(x8, rank) VALUES('usermerge', 2);`)

	const nDoc = 200
	for i := 0; i < nDoc; i++ {
		must("INSERT INTO x8(rowid, i) VALUES(" + strconv.Itoa(i+1) + ", 'word" + strconv.Itoa(i%29) + " tail" + strconv.Itoa(i%13) + " more" + strconv.Itoa(i%7) + "');")
		// Keep merging as the corpus grows (the 3.x shape: usermerge
		// configured once, repeated merge=1 work quanta).
		if (i+1)%25 == 0 {
			must("INSERT INTO x8(x8, rank) VALUES('merge', 1);")
		}
	}

	// Drive to convergence with bounded work: each 'merge', 1 performs one
	// work unit; when nothing is left to merge the structure must be stable
	// and every level must hold at most one segment.
	prevTotal := -1
	for iter := 0; iter < 200; iter++ {
		must("INSERT INTO x8(x8, rank) VALUES('merge', 1);")
		rows := db.Query(`SELECT level, count(*) FROM fts5_structure((SELECT block FROM x8_data WHERE id=10)) GROUP BY level`)
		if rows.Error != nil {
			t.Fatalf("fts5_structure query: %v", rows.Error)
		}
		total, over := 0, 0
		for _, row := range rows.Rows {
			nSeg := row[1].(int64)
			total += int(nSeg)
			if nSeg > 1 {
				over++
			}
		}
		if over == 0 {
			break // converged
		}
		if iter == 199 || (total == prevTotal && iter > 50) {
			t.Fatalf("merge did not converge: %d levels hold >1 segment (total %d) after %d work units", over, total, iter+1)
		}
		prevTotal = total
	}

	must("INSERT INTO x8(x8) VALUES('integrity-check');")

	rows := db.Query("SELECT count(*) FROM x8 WHERE x8 MATCH 'word3'").Rows
	if rows[0][0].(int64) == 0 {
		t.Error("'word3' missing after convergence merges")
	}

	// 6.1/6.2 — 'merge' on an empty table (and negative work) is a no-op.
	must(`CREATE VIRTUAL TABLE g1 USING fts5(a, b);`)
	must(`INSERT INTO g1(g1, rank) VALUES('merge', 10);`)
	must(`INSERT INTO g1(g1, rank) VALUES('merge', -10);`)
	must(`INSERT INTO g1(g1) VALUES('integrity-check');`)
}
