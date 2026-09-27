package frigolite

import "testing"

// Native pin for fts5optimize 2.tn.4's merge loop (T33r-fts, 2026-09-26).
// The TCL idiom
//
//	while 1 {
//	  set c [db total_changes]
//	  execsql { INSERT INTO t1(t1, rank) VALUES('merge', 1) }
//	  set c [expr [db total_changes]-$c]
//	  if {$c<2} break
//	}
//
// terminates because a 'merge=1' special insert moves sqlite3_total_changes
// by less than 2 — oracle 3.54.0 measures delta = 1 (the vtab row itself;
// the merge's %_data structure/blob writes are direct sqlite3_blob I/O and
// never touch the change counters). The mirror must honor the same contract:
// module blob-tier shadow writes (%_data, %_idx) are untracked
// (internal/exec engineVtabDB.ExecSQLUntracked), so the loop's break
// condition is reachable. A plain fts5 INSERT moves total_changes by its
// SQL-tier shadow writes (oracle +7: %_content, %_docsize, %_stat, ...); the
// mirror issues fewer shadow SQL statements (+3), a documented divergence of
// the blob-per-segment model.
func TestFTS5OptimizeMergeLoopTermination(t *testing.T) {
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
	totalChanges := func() int64 {
		rows := db.Query("SELECT total_changes();").Rows
		if len(rows) == 0 {
			t.Fatal("total_changes query returned no rows")
		}
		return rows[0][0].(int64)
	}

	must("CREATE VIRTUAL TABLE t1 USING fts5(x, y)")
	must("INSERT INTO t1 VALUES('a b c', 'd e f')")
	must("INSERT INTO t1 VALUES('b c d', 'e f g')")
	must("INSERT INTO t1 VALUES('c d e', 'f g h')")

	// A plain INSERT's shadow SQL counts (bounded, positive — oracle +7,
	// mirror +3; both well past the loop threshold while merging).
	before := totalChanges()
	must("INSERT INTO t1 VALUES('x y z', 'w v u')")
	if delta := totalChanges() - before; delta < 1 {
		t.Fatalf("plain fts5 INSERT: total_changes delta = %d, want >= 1 (oracle 7, mirror 3)", delta)
	}

	// The merge loop's break condition: each 'merge=1' adds less than 2, so
	// the idiom terminates after a bounded number of iterations. Verify the
	// delta AND that the loop-shaped repetition terminates.
	for i := 0; i < 8; i++ {
		before := totalChanges()
		must("INSERT INTO t1(t1, rank) VALUES('merge', 1)")
		delta := totalChanges() - before
		if delta >= 2 {
			t.Fatalf("merge=1 iteration %d: total_changes delta = %d, want < 2 (oracle 1) — the 2.tn.4 loop would never terminate", i, delta)
		}
	}

	// The table survived the merge work with its content queryable.
	rows := db.Query("SELECT count(*) FROM t1 WHERE t1 MATCH 'c';").Rows
	if rows[0][0].(int64) == 0 {
		t.Fatalf("MATCH 'c' after merges returned no rows")
	}
}
