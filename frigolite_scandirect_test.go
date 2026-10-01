package frigolite

import (
	"fmt"
	"testing"
)

// Native pins for the PERF.STRUCT-scan direct column read (internal/execquery/
// select_scan_direct.go + internal/storage/record_column.go): scans whose live
// consumers touch only a few declared columns decode exactly those columns
// straight from the cell payload (vdbe.c OP_Column parity) instead of boxing
// the whole record. Every pin asserts the full engine-visible contract
// (columns, Go-typed cells, errors) so a decode that skips, mis-slots, or
// mistypes a value fails: the direct read must be byte-identical to the
// historical record-wide decode on every shape it fires on — and on every
// shape it must NOT fire on (star, ORDER BY maps, WITHOUT ROWID remap,
// dropped-column shift, short records).

// scanDirectOpen builds the pin fixtures:
//   - wide: 12 declared columns over every storage family (ints of each
//     width, NULLs, 0/1 constants, text, blob, REAL) with an INTEGER PRIMARY
//     KEY alias — the direct read's primary shape.
//   - gencol: VIRTUAL + STORED generated columns (mid-table storage slots).
//   - nocase: a bare output column with a non-BINARY declared collation.
//   - alt: short records (ALTER TABLE ADD COLUMN defaults).
//   - drop: a table whose column was removed (storage-position shift).
//   - wr: WITHOUT ROWID (PK-first permutation must keep the full decode).
func scanDirectOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	exec := func(sqlText string) {
		t.Helper()
		if r := db.Exec(sqlText); r.Error != nil {
			t.Fatalf("%s: %v", sqlText, r.Error)
		}
	}
	exec(`CREATE TABLE wide(
		id INTEGER PRIMARY KEY,
		i1 INTEGER, i2 INTEGER, i3 INTEGER, i4 INTEGER, i5 INTEGER, i6 INTEGER,
		s1 TEXT, s2 TEXT, r1 REAL, b1 BLOB, n1 INTEGER)`)
	for i := 1; i <= 40; i++ {
		exec(fmt.Sprintf(`INSERT INTO wide(i1,i2,i3,i4,i5,i6,s1,s2,r1,b1,n1) VALUES
			(%d,%d,%d,%d,%d,%d,'s%02d',%s,%d.25,x'%08x',%d)`,
			i, -i, i*100000,
			i, // i4 placeholder — real NULLs patched below
			i%2, i<<40,
			i,
			map[bool]string{true: "NULL", false: fmt.Sprintf("'t%02d'", i)}[i%3 == 0],
			i,
			i*0x01020304&0xffffffff,
			i,
		))
	}
	// NULL i4 cells on every fourth row (i%4 == 0 kept the placeholder).
	if r := db.Exec("UPDATE wide SET i4 = NULL WHERE i1 % 4 = 0"); r.Error != nil {
		t.Fatalf("null i4: %v", r.Error)
	}
	exec(`CREATE TABLE gencol(a INTEGER PRIMARY KEY, b INTEGER, v INTEGER AS (b*2) VIRTUAL, st INTEGER AS (b+7) STORED)`)
	for i := 1; i <= 9; i++ {
		exec(fmt.Sprintf("INSERT INTO gencol(a,b) VALUES (%d,%d)", i, i*3))
	}
	exec("CREATE TABLE nocase(id INTEGER PRIMARY KEY, name TEXT COLLATE NOCASE, k INTEGER)")
	exec("INSERT INTO nocase(name,k) VALUES ('Alpha',1),('beta',2),('GAMMA',3)")
	exec("CREATE TABLE alt(id INTEGER PRIMARY KEY, a INTEGER)")
	exec("INSERT INTO alt(id,a) VALUES (1,10),(2,20),(3,30)")
	exec("ALTER TABLE alt ADD COLUMN d INTEGER DEFAULT 42")
	exec("ALTER TABLE alt ADD COLUMN e TEXT DEFAULT 'zz'")
	exec("CREATE TABLE dr(id INTEGER PRIMARY KEY, b TEXT, c INTEGER)")
	exec("INSERT INTO dr(id,b,c) VALUES (1,'x',100),(2,'y',200),(3,'z',300)")
	exec("ALTER TABLE dr DROP COLUMN b")
	exec("CREATE TABLE wrt(a TEXT, b INTEGER, c INTEGER, PRIMARY KEY(a,b)) WITHOUT ROWID")
	exec("INSERT INTO wrt VALUES ('p',1,10),('q',2,20),('r',3,30)")
	return db
}

// scanDirectQuery renders one query's full result (columns, Go-typed cells,
// or the error text) so every pin asserts the whole engine-visible contract.
func scanDirectQuery(t *testing.T, db *DB, sqlText string) string {
	t.Helper()
	res := db.Query(sqlText)
	if res.Error != nil {
		return "ERR: " + res.Error.Error()
	}
	out := ""
	for i, col := range res.Columns {
		if i > 0 {
			out += "|"
		}
		out += col
	}
	out += " ;; "
	for _, row := range res.Rows {
		for i, v := range row {
			if i > 0 {
				out += ","
			}
			out += fmt.Sprintf("%T:%v", v, v)
		}
		out += ";"
	}
	return out
}

// wideCell mirrors one wide-table row's stored values (the INSERT loop's
// parameters) so want strings are computed, not hand-derived.
type wideCell struct {
	id                     int64
	i1, i2, i3, i4, i5, i6 int64
	i4ok                   bool
	s1, s2                 interface{}
	r1                     float64
	b1                     []byte
	n1                     int64
}

// wideCells builds the 40 fixture rows' expected cell images.
func wideCells() []wideCell {
	cells := make([]wideCell, 0, 40)
	for i := 1; i <= 40; i++ {
		c := wideCell{
			id: int64(i),
			i1: int64(i), i2: -int64(i), i3: int64(i) * 100000,
			i5: int64(i % 2), i6: int64(i) << 40,
			s1: fmt.Sprintf("s%02d", i),
			r1: float64(i) + 0.25,
			b1: []byte{byte(i * 0x01020304 >> 24), byte(i * 0x01020304 >> 16), byte(i * 0x01020304 >> 8), byte(i * 0x01020304)},
			n1: int64(i),
		}
		if i%4 == 0 {
			c.i4ok = false
		} else {
			c.i4, c.i4ok = int64(i), true
		}
		if i%3 == 0 {
			c.s2 = nil
		} else {
			c.s2 = fmt.Sprintf("t%02d", i)
		}
		cells = append(cells, c)
	}
	return cells
}

// TestScanDirectBareProjection pins single-column bare scans over the wide
// table (the direct read fires: 1 referenced column of 12): every storage
// family, NULLs, and the exact Go types must match the record content.
func TestScanDirectBareProjection(t *testing.T) {
	db := scanDirectOpen(t)
	defer db.Close()
	cells := wideCells()
	col := func(name string, render func(c wideCell) string) string {
		out := name + " ;; "
		for _, c := range cells {
			out += render(c)
		}
		return out
	}
	f := func(v interface{}) string { return fmt.Sprintf("%T:%v;", v, v) }
	g := func(v interface{}) string { return fmt.Sprintf("%T:%v,", v, v) }
	for _, tc := range []struct{ sql, want string }{
		{"SELECT i1 FROM wide", col("i1", func(c wideCell) string { return f(c.i1) })},
		{"SELECT i4 FROM wide", col("i4", func(c wideCell) string {
			if !c.i4ok {
				return "<nil>:<nil>;"
			}
			return f(c.i4)
		})},
		{"SELECT s2 FROM wide", col("s2", func(c wideCell) string { return f(c.s2) })},
		{"SELECT i5 FROM wide", col("i5", func(c wideCell) string { return f(c.i5) })},
		{"SELECT i6, s1 FROM wide", col("i6|s1", func(c wideCell) string { return g(c.i6) + f(c.s1) })},
		{"SELECT r1 FROM wide", col("r1", func(c wideCell) string { return f(c.r1) })},
		{"SELECT b1 FROM wide", col("b1", func(c wideCell) string { return f(c.b1) })},
		// duplicate output slots: one read serves both peel positions
		{"SELECT i1, i1 FROM wide", col("i1|i1", func(c wideCell) string { return g(c.i1) + f(c.i1) })},
		// three referenced columns (at the cap), case-variant spellings
		{"SELECT n1, I1 FROM wide", col("n1|i1", func(c wideCell) string { return g(c.n1) + f(c.i1) })},
		// empty result set through the direct path (columns still emitted)
		{"SELECT i1 FROM wide WHERE i1 > 1000000", "i1 ;; "},
	} {
		if got := scanDirectQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.sql, got, tc.want)
		}
	}
}

// TestScanDirectFallbackShapes pins the shapes the direct read must NOT
// serve, whose outputs must stay identical to the historical decode: four
// referenced columns (over the cap), SELECT * (star output), ORDER BY (row
// maps), WITHOUT ROWID (PK-first permutation), dropped-column tables
// (storage-position shift), and generated columns.
func TestScanDirectFallbackShapes(t *testing.T) {
	db := scanDirectOpen(t)
	defer db.Close()
	cells := wideCells()
	row4 := "i1|s1|r1|i2 ;; "
	rowStar := "id|i1|i2|i3|i4|i5|i6|s1|s2|r1|b1|n1 ;; "
	for _, c := range cells {
		i4 := interface{}(c.i4)
		if !c.i4ok {
			i4 = nil
		}
		row4 += fmt.Sprintf("%T:%v,%T:%v,%T:%v,%T:%v;", c.i1, c.i1, c.s1, c.s1, c.r1, c.r1, c.i2, c.i2)
		rowStar += fmt.Sprintf("%T:%v,%T:%v,%T:%v,%T:%v,%T:%v,%T:%v,%T:%v,%T:%v,%T:%v,%T:%v,%T:%v,%T:%v;",
			c.id, c.id, c.i1, c.i1, c.i2, c.i2, c.i3, c.i3, i4, i4, c.i5, c.i5, c.i6, c.i6,
			c.s1, c.s1, c.s2, c.s2, c.r1, c.r1, c.b1, c.b1, c.n1, c.n1)
	}
	for _, tc := range []struct{ sql, want string }{
		{"SELECT i1, s1, r1, i2 FROM wide", row4},
		{"SELECT c FROM wrt", "c ;; int64:10;int64:20;int64:30;"},
		{"SELECT c FROM dr", "c ;; int64:100;int64:200;int64:300;"},
		{"SELECT st, b FROM gencol",
			"st|b ;; int64:10,int64:3;int64:13,int64:6;int64:16,int64:9;int64:19,int64:12;int64:22,int64:15;int64:25,int64:18;int64:28,int64:21;int64:31,int64:24;int64:34,int64:27;"},
		{"SELECT v FROM gencol", "v ;; int64:6;int64:12;int64:18;int64:24;int64:30;int64:36;int64:42;int64:48;int64:54;"},
		{"SELECT r1 FROM wide ORDER BY r1 LIMIT 3", "r1 ;; float64:1.25;float64:2.25;float64:3.25;"},
		{"SELECT i1 FROM wide ORDER BY i1", "i1 ;; " + func() string {
			out := ""
			for _, c := range cells {
				out += fmt.Sprintf("%T:%v;", c.i1, c.i1)
			}
			return out
		}()},
	} {
		if got := scanDirectQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.sql, got, tc.want)
		}
	}
}

// TestScanDirectAggFeed pins the simple-aggregate feed over the wide table
// stepping from direct column reads: COUNT(*), single-column SUM/AVG/TOTAL,
// the INTEGER PRIMARY KEY alias sum (stored-NULL → rowid substitution must
// ride the direct path's affinity plan), multi-aggregate, and WHERE-consumed
// aggregates.
func TestScanDirectAggFeed(t *testing.T) {
	db := scanDirectOpen(t)
	defer db.Close()
	sumI1, sumR1, sumSt := 0, 0.0, 0
	countI1 := 0
	for i := 1; i <= 40; i++ {
		sumI1 += i
		sumR1 += float64(i) + 0.25
		if i%4 != 0 {
			countI1++
		}
	}
	for i := 1; i <= 9; i++ {
		sumSt += i*3 + 7
	}
	for _, tc := range []struct{ sql, want string }{
		{"SELECT COUNT(*) FROM wide", "COUNT(*) ;; int64:40;"},
		{"SELECT SUM(i1) FROM wide", fmt.Sprintf("SUM(i1) ;; int64:%d;", sumI1)},
		{"SELECT SUM(id) FROM wide", fmt.Sprintf("SUM(id) ;; int64:%d;", sumI1)},
		{"SELECT COUNT(i4) FROM wide", fmt.Sprintf("COUNT(i4) ;; int64:%d;", countI1)},
		{"SELECT AVG(r1) FROM wide", fmt.Sprintf("AVG(r1) ;; float64:%v;", sumR1/40)},
		{"SELECT TOTAL(r1) FROM wide", fmt.Sprintf("TOTAL(r1) ;; float64:%v;", sumR1)},
		{"SELECT COUNT(*), SUM(i1), AVG(r1) FROM wide",
			fmt.Sprintf("COUNT(*)|SUM(i1)|AVG(r1) ;; int64:40,int64:%d,float64:%v;", sumI1, sumR1/40)},
		{"SELECT SUM(i1) FROM wide WHERE i1 % 4 = 0",
			"SUM(i1) ;; int64:220;"},
		{"SELECT COUNT(*) FROM wide WHERE i1 % 4 = 0",
			"COUNT(*) ;; int64:10;"},
		// empty input keeps the generic path's empty-aggregate contract
		{"SELECT SUM(i1) FROM wide WHERE i1 > 1000000", "SUM(i1) ;; <nil>:<nil>;"},
		{"SELECT COUNT(*) FROM wide WHERE i1 > 1000000", "COUNT(*) ;; int64:0;"},
		// gencol aggregates (STORED generated column as feed argument)
		{"SELECT SUM(st) FROM gencol", fmt.Sprintf("SUM(st) ;; int64:%d;", sumSt)},
	} {
		if got := scanDirectQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.sql, got, tc.want)
		}
	}
}

// TestScanDirectIPKAndCollation pins the INTEGER PRIMARY KEY alias bare
// output (stored NULL must surface as the rowid through the direct path's
// affinity plan) and the non-BINARY collation exemption (a NOCASE column's
// wrapper must still be peeled from the bare output).
func TestScanDirectIPKAndCollation(t *testing.T) {
	db := scanDirectOpen(t)
	defer db.Close()
	cells := wideCells()
	idRows, idI1 := "id ;; ", "id|i1 ;; "
	for _, c := range cells {
		idRows += fmt.Sprintf("%T:%v;", c.id, c.id)
		idI1 += fmt.Sprintf("%T:%v,%T:%v;", c.id, c.id, c.i1, c.i1)
	}
	for _, tc := range []struct{ sql, want string }{
		{"SELECT id FROM wide", idRows},
		{"SELECT id, i1 FROM wide", idI1},
		{"SELECT name FROM nocase", "name ;; string:Alpha;string:beta;string:GAMMA;"},
		{"SELECT name, k FROM nocase", "name|k ;; string:Alpha,int64:1;string:beta,int64:2;string:GAMMA,int64:3;"},
		{"SELECT d, e FROM alt", "d|e ;; int64:42,string:zz;int64:42,string:zz;int64:42,string:zz;"},
		{"SELECT a, d FROM alt", "a|d ;; int64:10,int64:42;int64:20,int64:42;int64:30,int64:42;"},
		{"SELECT SUM(d) FROM alt", "SUM(d) ;; int64:126;"},
	} {
		if got := scanDirectQuery(t, db, tc.sql); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.sql, got, tc.want)
		}
	}
}
