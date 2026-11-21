package frigolite

import (
	"fmt"
	"os"
	"testing"
)

// TestDensityProbe compares the btree shape of the C oracle's walckptnoop
// workload against our own run (the density unit's measurement harness).
// TEMPORARY — delete when the unit closes.
func TestDensityProbe(t *testing.T) {
	openWithFunc := func(path string) *DB {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		val := int64(123)
		d.RegisterFunction("myrandomblob", func(args []interface{}) (interface{}, error) {
			n, ok := args[0].(int64)
			if !ok {
				return nil, fmt.Errorf("bad arg")
			}
			out := make([]byte, n)
			for i := range out {
				val = (1103515245*val + 12345) % 2147483648
				out[i] = byte(val % 256)
			}
			return out, nil
		}, 1, 1)
		return d
	}

	if _, err := os.Stat("/tmp/densprobe/testdir/test.db"); err == nil {
		reportShape(t, "C-oracle", "/tmp/densprobe/testdir/test.db", openWithFunc)
	}

	dir := t.TempDir()
	db := dir + "/test.db"
	d := openWithFunc(db)
	if res := d.Exec(`PRAGMA page_size=1024;
  PRAGMA auto_vacuum=NONE;
  PRAGMA secure_delete=OFF;
  VACUUM;
  CREATE TABLE t1(x INTEGER PRIMARY KEY, y TEXT);
  CREATE INDEX i1 ON t1(y);
  PRAGMA journal_mode = wal;`); res.Error != nil {
		t.Fatalf("setup: %v", res.Error)
	}
	if res := d.Query("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<1000) INSERT INTO t1 SELECT NULL, hex(myrandomblob(64)) FROM s"); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	reportShape(t, "ours", db, openWithFunc)
}

func reportShape(t *testing.T, tag, path string, open func(string) *DB) {
	t.Helper()
	d := open(path)
	defer d.Close()
	q := func(sql string) string {
		res := d.Query(sql)
		if res.Error != nil {
			t.Logf("%s: %v", sql, res.Error)
			return "?"
		}
		out := ""
		for _, row := range res.Rows {
			for _, c := range row {
				out += " " + formatHarnessValue("probe", c)
			}
		}
		return out
	}
	t.Logf("%s page_count:%s", tag, q("PRAGMA page_count"))
	t.Logf("%s entries:%s IC:%s", tag,
		q("SELECT count(*) FROM t1"),
		q("PRAGMA integrity_check"))
	t.Logf("%s i1 scan-tail:%s", tag, q(
		"SELECT count(*) FROM (SELECT y FROM t1 ORDER BY y)"))
	t.Logf("%s i1 pages:%s", tag, q(
		"SELECT pageno||':'||pagetype||':'||ncell||':'||path||' ' FROM dbstat WHERE name='i1' ORDER BY pageno"))
	t.Logf("%s t1 leaves:%s interiors:%s", tag,
		q("SELECT count(*) FROM dbstat WHERE name='t1' AND pagetype='leaf'"),
		q("SELECT count(*) FROM dbstat WHERE name='t1' AND pagetype='internal'"))
	t.Logf("%s i1 leaves:%s interiors:%s", tag,
		q("SELECT count(*) FROM dbstat WHERE name='i1' AND pagetype='leaf'"),
		q("SELECT count(*) FROM dbstat WHERE name='i1' AND pagetype='internal'"))
	t.Logf("%s i1 leaf fill pct:%s", tag, q(
		"SELECT min(100*(pgsize-unused)/pgsize), avg(100*(pgsize-unused)/pgsize), max(100*(pgsize-unused)/pgsize) FROM dbstat WHERE name='i1' AND pagetype='leaf'"))
	t.Logf("%s i1 cells/leaf min-avg-max:%s", tag, q(
		"SELECT min(ncell), avg(ncell), max(ncell) FROM dbstat WHERE name='i1' AND pagetype='leaf'"))
	t.Logf("%s freelist:%s", tag, q("PRAGMA freelist_count"))
	t.Logf("%s ckpt noop:%s", tag, q("PRAGMA wal_checkpoint = noop"))
}
