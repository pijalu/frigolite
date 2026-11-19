package frigolite

import (
	"fmt"
	"testing"
)

// TestAVSpillProbe reproduces the e_vacuum corruption: an auto_vacuum
// database with an index, inserts that trigger the leaf spill, then the
// integrity check. TEMPORARY.
func TestAVSpillProbe(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if res := d.Exec(`PRAGMA page_size=1024; PRAGMA auto_vacuum=FULL;
  CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT);
  CREATE INDEX i1 ON t1(b);
  PRAGMA journal_mode=wal;`); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Random-ish 6-byte keys: 2000 rows.
	val := int64(7)
	for i := 0; i < 2000; i++ {
		val = (1103515245*val + 12345) % 2147483648
		b := fmt.Sprintf("%012x%012x", val, i)
		if res := d.Exec("INSERT INTO t1 VALUES(" + fmt.Sprint(i+1) + ", '" + b + "')"); res.Error != nil {
			t.Fatalf("insert %d: %v", i, res.Error)
		}
	}
	q := func(sql string) string {
		res := d.Query(sql)
		if res.Error != nil {
			return "ERR: " + res.Error.Error()
		}
		out := ""
		for _, row := range res.Rows {
			for _, c := range row {
				out += " " + fmt.Sprint(c)
			}
		}
		return out
	}
	ic := d.Query("PRAGMA integrity_check")
	t.Logf("integrity_check rows:")
	for i, row := range ic.Rows {
		if i < 8 {
			t.Logf("  IC: %v", row[0])
		}
	}
	_ = ic
	t.Logf("i1 total cells: %s (leaf+interior, want 2000)", q("SELECT sum(ncell) FROM dbstat WHERE name='i1'"))
	t.Logf("i1 index-scan count: %s (want 2000)", q("SELECT count(*) FROM t1 WHERE b > '0'"))
	t.Logf("i1 interior cells: %s", q("SELECT sum(ncell) FROM dbstat WHERE name='i1' AND pagetype='internal'"))
	t.Logf("i1 leaf cells: %s", q("SELECT sum(ncell) FROM dbstat WHERE name='i1' AND pagetype='leaf'"))
	t.Logf("t1 rows: %s", q("SELECT count(*) FROM t1"))
	t.Logf("dup check: %s", q("SELECT b, count(*) FROM t1 GROUP BY b HAVING count(*)>1 LIMIT 4"))
	t.Logf("missing rows: %s", q("SELECT group_concat(rowid) FROM (SELECT rowid FROM t1 EXCEPT SELECT rowid FROM t1 WHERE rowid IN (SELECT rowid FROM t1 INDEXED BY i1))"))
	// Trace rows 23 and 1508: their b values and the index seeks.
	// ALSO: the per-row presence via the index for ALL rows: which rows'
	// full-key seeks fail? (the integrity's mechanism)
	if icRes := d.Query("PRAGMA integrity_check"); icRes.Error == nil {
		_ = icRes
	}
	for _, rid := range []int{23, 1508} {
		bres := d.Query(fmt.Sprintf("SELECT b FROM t1 WHERE rowid=%d", rid))
		if bres.Error != nil || len(bres.Rows) == 0 {
			t.Logf("  row %d: table lookup failed: %v", rid, bres.Error)
			continue
		}
		b := fmt.Sprint(bres.Rows[0][0])
		seek := d.Query(fmt.Sprintf("SELECT rowid FROM t1 WHERE b='%s'", b))
		seekFull := d.Query(fmt.Sprintf("SELECT rowid FROM t1 WHERE b='%s' AND rowid=%d", b, rid))
		t.Logf("  row %d: b=%s prefix-seek -> %v rows=%v; full-seek rows=%v err=%v", rid, b[:12]+"...", seek.Error, len(seek.Rows), len(seekFull.Rows), seekFull.Error)
	}
	// The range scan at the promoted boundary: the entries >= b23's prefix.
	// The promoted entry (the interior divider) must be the FIRST hit.
	b23 := ""
	if br := d.Query("SELECT b FROM t1 WHERE rowid=23"); br.Error == nil && len(br.Rows) > 0 {
		b23 = fmt.Sprint(br.Rows[0][0])
	}
	if b23 != "" {
		r1 := d.Query(fmt.Sprintf("SELECT b, rowid FROM t1 WHERE b >= '%s' ORDER BY b LIMIT 3", b23))
		if r1.Error == nil {
			for _, row := range r1.Rows {
				t.Logf("  RANGE[>=b23]: b=%s rowid=%v", fmt.Sprint(row[0])[:12]+"...", row[1])
			}
		} else {
			t.Logf("  RANGE err: %v", r1.Error)
		}
	}
	// Dump i1's leaf cells: find duplicated (b,rowid) keys and out-of-order pages.
	dup := map[string]int{}
	res2 := d.Query("SELECT pageno, cellbyid FROM dbstat('i1') WHERE pagetype='leaf' LIMIT 0")
	_ = res2
	t.Logf("dump via dbstat skipped; using index scan order check")
	if res := d.Query("SELECT b FROM t1 ORDER BY b"); res.Error == nil {
		prev := ""
		bad := 0
		for _, row := range res.Rows {
			b := fmt.Sprint(row[0])
			if prev != "" && b < prev && bad < 3 {
				t.Logf("  OUT-OF-ORDER at %s after %s", b, prev)
				bad++
			}
			if b == prev {
				dup[b]++
			}
			prev = b
		}
		t.Logf("  scan-order inversions: %d, dup adjacent: %d", bad, len(dup))
	}
	// Scan the index.
	if res := d.Query("SELECT count(*) FROM t1 WHERE b > '0'"); res.Error != nil {
		t.Logf("scan: ERR %v", res.Error)
	} else {
		t.Logf("scan count ok")
	}
}
