package frigolite

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// T33-misc probes: pure-Go engine checks for the five failing testgen/misc*
// packages. Each probe drives frigolite.Open/Exec/Query directly (AGENTS.md
// engine-first diagnosis). Oracle: sqlite3 3.51 built from /Users/muaddib/dev/sqlite.

func probeQueryRows(t *testing.T, db *DB, sql string) ([]string, error) {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		return nil, r.Error
	}
	var out []string
	for _, row := range r.Rows {
			cells := make([]string, len(row))
			for i, v := range row {
				if v == nil {
					cells[i] = "N"
				} else {
					cells[i] = sprintfValue(v)
				}
			}
		out = append(out, strings.Join(cells, "|"))
	}
	return out, nil
}

func sprintfValue(v interface{}) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

func TestT33ProbeLeadingDotLiterals(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct{ sql, want string }{
		{"SELECT .1", "0.1"},
		{"SELECT 2.", "2.0"},
		{"SELECT 3.e0", "3.0"},
		{"SELECT .4e+1", "4.0"},
	}
	for _, tc := range cases {
		rows, err := probeQueryRows(t, db, tc.sql)
		if err != nil {
			t.Errorf("%s: error %v", tc.sql, err)
			continue
		}
		if len(rows) != 1 || rows[0] != tc.want {
			t.Errorf("%s: got %v want [%s]", tc.sql, rows, tc.want)
		}
	}
}

func TestT33ProbeLimitSubqueryError(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec("CREATE TABLE t(a)").Error; err != nil {
		t.Fatal(err)
	}
	r := db.Exec("SELECT * FROM sqlite_master UNION ALL SELECT * FROM sqlite_master LIMIT (SELECT count(*) FROM blah)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such table: blah") {
		t.Errorf("want error 'no such table: blah', got %v", r.Error)
	}
}

func TestT33ProbeEvalDeleteDuringScan(t *testing.T) {
	// misc8-1.6: eval('DELETE FROM t1; SELECT ''bam''') runs while the outer
	// SELECT scans t1. Oracle 3.51: statement succeeds, rows
	// (1,2,3),(4,5,6),(7,bam,NULL).
	path := "t33probe.db"
	os.Remove(path)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close(); os.Remove(path) }()
	if err := db.Exec("CREATE TABLE t1(a,b,c); INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,null,9)").Error; err != nil {
		t.Fatal(err)
	}
	rows, err := probeQueryRows(t, db, "SELECT a, coalesce(b, eval('DELETE FROM t1; SELECT ''bam''')), c FROM t1 ORDER BY rowid")
	if err != nil {
		t.Fatalf("misc8-1.6: statement failed: %v", err)
	}
	t.Logf("misc8-1.6 rows: %v", rows)
	wantHead := []string{"1|2|3", "4|5|6"}
	if len(rows) < 2 {
		t.Fatalf("misc8-1.6: got %d rows, want >= 2 (%v)", len(rows), rows)
	}
	for i, w := range wantHead {
		if rows[i] != w {
			t.Errorf("misc8-1.6 row %d: got %q want %q", i, rows[i], w)
		}
	}
	// Row 3: a=7; b NULL -> eval fires (DELETE + bam); c is NULL in SQLite
	// (the column read happens after the delete invalidated the cursor).
	if !strings.HasPrefix(rows[2], "7|bam|") {
		t.Errorf("misc8-1.6 row 3: got %q want prefix 7|bam|", rows[2])
	}
	if len(rows) != 3 {
		t.Errorf("misc8-1.6: got %d rows, want 3 (SQLite stops after the scan cursor restore hits EOF)", len(rows))
	}
}

func TestT33ProbeScanOwnTableDeleteLoop(t *testing.T) {
	// misc2-7.2/7.3 semantics: materialize SELECT rowid, then DELETE each row.
	// Engine must allow modifying a table while iterating a materialized
	// result (SQLite allows this since 2006).
	path := "t33probe2.db"
	os.Remove(path)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close(); os.Remove(path) }()
	if err := db.Exec("CREATE TABLE t1(x); INSERT INTO t1 VALUES(1),(2),(3)").Error; err != nil {
		t.Fatal(err)
	}
	r := db.Query("SELECT rowid FROM t1")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, row := range r.Rows {
		if err := db.Exec("DELETE FROM t1 WHERE rowid=" + strconv.FormatInt(row[0].(int64), 10)).Error; err != nil {
			t.Fatalf("delete during iteration: %v", err)
		}
	}
	rows, err := probeQueryRows(t, db, "SELECT * FROM t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("t1 not empty after delete loop: %v", rows)
	}
}


func TestT33ProbeEvalDeleteDuringScanExec(t *testing.T) {
	// misc8-1.6 exact corpus path: db.Exec of the SELECT.
	path := "t33probe3.db"
	os.Remove(path)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close(); os.Remove(path) }()
	if err := db.Exec("CREATE TABLE t1(a,b,c); INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,null,9)").Error; err != nil {
		t.Fatal(err)
	}
	r := db.Exec("SELECT a, coalesce(b, eval('DELETE FROM t1; SELECT ''bam''')), c FROM t1\n   ORDER BY rowid")
	if r.Error != nil {
		t.Fatalf("misc8-1.6 exec: %v", r.Error)
	}
	t.Logf("rows: %v", r.Rows)
}

func TestT33ProbeEvalDeleteFullSequence(t *testing.T) {
	// Exact misc8-1.0..1.6 corpus sequence.
	path := "t33probe4.db"
	os.Remove(path)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close(); os.Remove(path) }()
	if r := db.Query("\n  CREATE TABLE t1(a,b,c);\n  INSERT INTO t1 VALUES(1,2,3),(4,5,6);\n  SELECT quote(eval('SELECT * FROM t1 ORDER BY a','-abc-'));\n"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Query("\n  SELECT quote(eval('SELECT * FROM t1 ORDER BY a'));\n"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("\n  SELECT quote(eval('SELECT d FROM t1 ORDER BY a'));\n"); r.Error == nil {
		t.Fatal("1.2 want error")
	}
	if r := db.Query("\n  INSERT INTO t1 VALUES(7,null,9);\n  SELECT eval('SELECT * FROM t1 ORDER BY a',',');\n"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("\n  BEGIN;\n  INSERT INTO t1 VALUES(10,11,12);\n  SELECT a, coalesce(b, eval('ROLLBACK; SELECT ''bam'';')), c\n   FROM t1 ORDER BY a;\n"); r.Error != nil {
		t.Fatalf("1.4: %v", r.Error)
	}
	if r := db.Exec("\n  INSERT INTO t1 VALUES(10,11,12);\n  SELECT a, coalesce(b, eval('SELECT ''bam''')), c\n    FROM t1\n   ORDER BY rowid;\n"); r.Error != nil {
		t.Fatalf("1.5: %v", r.Error)
	}
	r := db.Exec("\n  SELECT a, coalesce(b, eval('DELETE FROM t1; SELECT ''bam''')), c\n    FROM t1\n   ORDER BY rowid;\n")
	if r.Error != nil {
		t.Fatalf("1.6: %v", r.Error)
	}
	t.Logf("1.6 rows: %v", r.Rows)
}

func TestT33ProbeEvalDeleteBisect(t *testing.T) {
	for _, n := range []int{3, 4, 5, 10} {
		path := "t33probe5.db"
		os.Remove(path)
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sql := "CREATE TABLE t1(a,b,c);"
		for i := 1; i <= n; i++ {
			sql += " INSERT INTO t1 VALUES(" + strconv.Itoa(i) + ",null,9);"
		}
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
		r := db.Exec("\n  SELECT a, coalesce(b, eval('DELETE FROM t1; SELECT ''bam''')), c\n    FROM t1\n   ORDER BY rowid;\n")
		if r.Error != nil {
			t.Errorf("n=%d: %v", n, r.Error)
		} else {
			t.Logf("n=%d ok rows=%v", n, r.Rows)
		}
		db.Close()
		os.Remove(path)
	}
}
