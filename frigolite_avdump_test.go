package frigolite

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
)

// TestAVWalk2 dumps every i1 leaf entry's (b, rowid) and reports the
// duplicated keys + the missing rowids (the integrity_check's disagreement
// localized to concrete entries). TEMPORARY.
func TestAVWalk2(t *testing.T) {
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
	val := int64(7)
	for i := 0; i < 2000; i++ {
		val = (1103515245*val + 12345) % 2147483648
		b := fmt.Sprintf("%012x%012x", val, i)
		if res := d.Exec("INSERT INTO t1 VALUES(" + fmt.Sprint(i+1) + ", '" + b + "')"); res.Error != nil {
			t.Fatalf("insert %d: %v", i, res.Error)
		}
	}
	res := d.Query("SELECT rootpage FROM sqlite_schema WHERE name='i1'")
	root := res.Rows[0][0].(int64)
	pgr := d.engine.Pager()
	q := func(sql string) string {
		r := d.Query(sql)
		if r.Error != nil {
			return "ERR " + r.Error.Error()
		}
		out := ""
		for _, row := range r.Rows {
			for _, c := range row {
				out += " " + fmt.Sprint(c)
			}
		}
		return out
	}

	reasm := func(c *storage.Cell) []byte {
		if c.Overflow == 0 {
			return c.Payload
		}
		out := append([]byte{}, c.Payload...)
		next := c.Overflow
		for next != 0 {
			opg, oerr := pgr.ReadPage(next)
			if oerr != nil {
				break
			}
			nxt := uint32(opg.Data[0])<<24 | uint32(opg.Data[1])<<16 | uint32(opg.Data[2])<<8 | uint32(opg.Data[3])
			out = append(out, opg.Data[4:1016]...)
			next = nxt
		}
		if len(out) > c.PayloadLen {
			out = out[:c.PayloadLen]
		}
		return out
	}
	seen := map[string]int{}
	count := 0
	dumpSkips := 0
	leafCounts := map[uint32]int{}
	var walk func(pgno uint32)
	var chain []string
	walk = func(pgno uint32) {
		chain = append(chain, fmt.Sprint(pgno))
		pg, rerr := pgr.ReadPage(pgno)
		if rerr != nil {
			t.Logf("read %d: %v", pgno, rerr)
			chain = chain[:len(chain)-1]
			return
		}
		co := 0
		if pg.PageNum == 1 {
			co = 100
		}
		page, perr := storage.ParsePage(pg.Data, 1024, co)
		if perr != nil {
			t.Logf("parse %d: %v", pgno, perr)
			chain = chain[:len(chain)-1]
			return
		}
		switch page.PageType {
		case storage.PageTypeInteriorIndex:
			ptrBase := co + 12
			for i := 0; i < int(page.CellCount); i++ {
				cp := int(uint16(pg.Data[ptrBase+i*2])<<8 | uint16(pg.Data[ptrBase+i*2+1]))
				child := uint32(pg.Data[cp])<<24 | uint32(pg.Data[cp+1])<<16 | uint32(pg.Data[cp+2])<<8 | uint32(pg.Data[cp+3])
				// The interior divider IS an entry (C's index convention):
				// count it like a leaf cell.
				if cell, derr := storage.DecodeCell(pg.Data, cp, storage.CellIndexInterior, 1016); derr == nil {
					full := reasm(cell)
					if rec, rerr2 := storage.DecodeRecord(full); rerr2 == nil && len(rec.Values) >= 2 {
						b := fmt.Sprint(rec.Values[0])
						rid := rec.Values[1].(int64)
						key := fmt.Sprintf("%s|%d", b, rid)
						seen[key]++
						count++
						if seen[key] > 1 {
							t.Logf("  DUPLICATE (divider): rowid=%d b=%s on interior page %d (x%d)", rid, b[:12]+"...", pgno, seen[key])
						}
					}
				}
				walk(child)
			}
			rmp := uint32(pg.Data[co+8])<<24 | uint32(pg.Data[co+9])<<16 | uint32(pg.Data[co+10])<<8 | uint32(pg.Data[co+11])
			walk(rmp)
		case storage.PageTypeLeafIndex:
			leafCounts[pgno] = int(page.CellCount)
			for i := 0; i < int(page.CellCount); i++ {
				cp := int(uint16(pg.Data[co+8+i*2])<<8 | uint16(pg.Data[co+8+i*2+1]))
				cell, derr := storage.DecodeCell(pg.Data, cp, storage.CellIndexLeaf, 1016)
				if derr != nil {
					t.Logf("  p%d cell %d decode: %v", pgno, i, derr)
					continue
				}
				reasm := func(c *storage.Cell) []byte {
					if c.Overflow == 0 {
						return c.Payload
					}
					out := append([]byte{}, c.Payload...)
					next := c.Overflow
					for next != 0 {
						opg, oerr := pgr.ReadPage(next)
						if oerr != nil {
							break
						}
						nxt := uint32(opg.Data[0])<<24 | uint32(opg.Data[1])<<16 | uint32(opg.Data[2])<<8 | uint32(opg.Data[3])
						out = append(out, opg.Data[4:1016]...)
						next = nxt
					}
					if len(out) > c.PayloadLen {
						out = out[:c.PayloadLen]
					}
					return out
				}
				full := &storage.Cell{Payload: reasm(cell), PayloadLen: cell.PayloadLen}
				rec, rerr2 := storage.DecodeRecord(full.Payload)
				if rerr2 != nil {
					t.Logf("  p%d cell %d record: %v", pgno, i, rerr2)
					continue
				}
				if len(rec.Values) < 2 {
					if dumpSkips < 5 {
						t.Logf("  SKIP p%d cell %d: values=%d payloadLen=%d payloadHex=%x", pgno, i, len(rec.Values), len(full.Payload), full.Payload[:48])
						dumpSkips++
					}
					continue
				}
				b := fmt.Sprint(rec.Values[0])
				rid := rec.Values[1].(int64)
				key := fmt.Sprintf("%s|%d", b, rid)
				seen[key]++
				count++
				if seen[key] > 1 && count < 1e9 {
					t.Logf("  DUPLICATE entry: rowid=%d b=%s (x%d) on page %d", rid, b[:12]+"...", seen[key], pgno)
				}
			}
		}
		chain = chain[:len(chain)-1]
	}
	walk(uint32(root))
	t.Logf("dumped %d entries; %d duplicated; %d leaves", count, len(seen)-count, len(leafCounts))
	// The leaf cell-count distribution: the histogram.
	hist := map[int]int{}
	for _, n := range leafCounts {
		hist[n]++
	}
	for n := 1; n <= 40; n++ {
		if hist[n] > 0 {
			t.Logf("  leaves with %d cells: %d", n, hist[n])
		}
	}
	t.Logf("dbstat leaf cells: %s | dbstat interior cells: %s | dbstat leaves: %s",
		q("SELECT sum(ncell) FROM dbstat WHERE name='i1' AND pagetype='leaf'"),
		q("SELECT sum(ncell) FROM dbstat WHERE name='i1' AND pagetype='internal'"),
		q("SELECT count(*) FROM dbstat WHERE name='i1' AND pagetype='leaf'"))
	// The table's rows vs the index's rowid set.
	res2 := d.Query("SELECT rowid FROM t1 ORDER BY rowid DESC LIMIT 3")
	_ = res2
	if len(seen) > 0 {
		missing := 0
		for rid := int64(1); rid <= 2000 && missing < 6; rid++ {
			found := false
			// the row's b from the table
			br := d.Query(fmt.Sprintf("SELECT b FROM t1 WHERE rowid=%d", rid))
			if br.Error == nil && len(br.Rows) > 0 {
				key := fmt.Sprintf("%s|%d", fmt.Sprint(br.Rows[0][0]), rid)
				if seen[key] > 0 {
					found = true
				}
			}
			if !found {
				missing++
				t.Logf("  MISSING from index: rowid=%d", rid)
			}
		}
	}
}
