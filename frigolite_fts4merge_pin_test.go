package frigolite

import (
	"fmt"
	"os"
	"strconv"
	"testing"
)

// Native pin for fts4merge 5.9-5.11 (T33r-fts, 2026-09-26). The generated
// 5.9 blocks bind `set L [expr 16*16*7 + 16*3 + 12]` = 1852 into the
// statement `... LIMIT $L`; the transpiler never registered L, so $L was
// NULL and LIMIT NULL failed with "datatype mismatch" (the LIMIT itself is
// oracle-correct: SQLite 3.54.0 rejects `LIMIT NULL` with "datatype
// mismatch"). The generated 5.9 is repaired to the literal LIMIT 1852; this
// pin holds the same contract end to end — 1000 single-statement docs,
// merge=1,5 twice, a full duplicate pass, merge=1,6 twice, 1852 further
// duplicate inserts drawn from `SELECT docid FROM t1 UNION ALL SELECT
// docid FROM t1 LIMIT 1852`, then the C segment layouts:
//
//	5.10: 0 {0 1 2 3 4 5 6 7 8 9 10 11} 1 0 2 0 3 0 X'010E'
//	5.11: 1 {0 1} 2 0 3 0 X'010E'
//
// verified identical for fts3 and fts4 (corpus contract; oracle 3.54.0).
func TestFTS4MergeDupInsertMergeLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("dup-insert workload")
	}
	for _, mod := range []string{"fts4", "fts3"} {
		mod := mod
		t.Run(mod, func(t *testing.T) {
			os.Remove("/tmp/fts4merge_pin.db")
			db, err := Open("/tmp/fts4merge_pin.db")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				db.Close()
				os.Remove("/tmp/fts4merge_pin.db")
			}()
			must := func(sql string) {
				if r := db.Exec(sql); r.Error != nil {
					t.Fatalf("%s: %v", sql, r.Error)
				}
			}
			q := func(sql string) string {
				rows := db.Query(sql).Rows
				parts := make([]string, 0, len(rows))
				for _, row := range rows {
					for _, c := range row {
						parts = append(parts, fmt.Sprint(c))
					}
				}
				return fmt.Sprint(parts)
			}
			must("CREATE VIRTUAL TABLE t1 USING " + mod + " (x, y)")
			xw := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
			yw := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
			for i := 0; i < 1000; i++ {
				x := xw[(i/1000)%10] + " " + xw[(i/100)%10] + " " + xw[(i/10)%10] + " " + xw[i%10]
				y := yw[(i/1000)%10] + " " + yw[(i/100)%10] + " " + yw[(i/10)%10] + " " + yw[i%10]
				must("INSERT INTO t1(docid, x, y) VALUES(" + strconv.Itoa(i) + ", '" + x + "', '" + y + "')")
			}
			must("INSERT INTO t1(t1) VALUES('merge=1,5')")
			must("INSERT INTO t1(t1) VALUES('merge=1,5')")
			for _, row := range db.Query("SELECT docid FROM t1").Rows {
				must("INSERT INTO t1 SELECT * FROM t1 WHERE docid=" + fmt.Sprint(row[0]))
			}
			must("INSERT INTO t1(t1) VALUES('merge=1,6')")
			must("INSERT INTO t1(t1) VALUES('merge=1,6')")
			for _, row := range db.Query("SELECT docid FROM t1 UNION ALL SELECT docid FROM t1 LIMIT 1852").Rows {
				must("INSERT INTO t1 SELECT * FROM t1 WHERE docid=" + fmt.Sprint(row[0]))
			}
			got510 := q("SELECT level, group_concat(idx, ' ') FROM t1_segdir GROUP BY level") + " " +
				q("SELECT quote(value) from t1_stat WHERE rowid=1")
			want510 := "[0 0 1 2 3 4 5 6 7 8 9 10 11 1 0 2 0 3 0] [X'010E']"
			if got510 != want510 {
				t.Errorf("5.10: got %s, want %s", got510, want510)
			}
			must("INSERT INTO t1(t1) VALUES('merge=1,6')")
			got511 := q("SELECT level, string_agg(idx, ' ') FROM t1_segdir GROUP BY level") + " " +
				q("SELECT quote(value) from t1_stat WHERE rowid=1")
			want511 := "[1 0 1 2 0 3 0] [X'010E']"
			if got511 != want511 {
				t.Errorf("5.11: got %s, want %s", got511, want511)
			}
		})
	}
}
