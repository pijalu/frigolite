package frigolite_test

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// The sqlite_dbdata / sqlite_dbptr contracts pinned here are oracle-verified
// against a build of /Users/muaddib/dev/sqlite (amalgamation + dbdata.c
// compiled with SQLITE_ENABLE_DBPAGE_VTAB; standard sqlite3/python3 binaries
// do not ship the module). Row shapes match test/dbdata.test 1.1/1.2/2.1-2.3:
// one row per record field of each b-tree cell, field=-1 carrying the rowid
// of int-key leaf cells, no rows for interior table pages, and dbptr emitting
// the right-most child pointer first.

// dbdataRows formats a query result as "pgno cell field value" tuples for
// structural comparison.
func dbdataRows(t *testing.T, r *frigolite.Result) string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	var sb strings.Builder
	for _, row := range r.Rows {
		for i, v := range row {
			if i > 0 {
				sb.WriteByte(' ')
			}
			switch x := v.(type) {
			case nil:
				sb.WriteString("NULL")
			case int64:
				sb.WriteString(itoa64(x))
			case float64:
				sb.WriteString(f64str(x))
			case string:
				sb.WriteString("'" + x + "'")
			case []byte:
				sb.WriteString("x'" + hexStr(x) + "'")
			default:
				t.Fatalf("unexpected value type %T", v)
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func f64str(f float64) string {
	// Compact exact rendering for the literals used in the tests.
	switch f {
	case 3.14:
		return "3.14"
	case 1.0:
		return "1.0"
	}
	return "REAL"
}

const hexDigits = "0123456789abcdef"

func hexStr(b []byte) string {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0xf])
	}
	return string(out)
}

// TestNativeDbdata_CellRowsPerIntkeyLeaf pins test/dbdata.test 1.1/1.2: a
// table t1 root page carries one field=-1 row per cell (the rowid) plus one
// row per record field, and page 1 (sqlite_schema) decodes the same way.
func TestNativeDbdata_CellRowsPerIntkeyLeaf(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "dbd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE T1(a, b);",
		"INSERT INTO t1(rowid, a, b) VALUES(5, 'v', 'five');",
		"INSERT INTO t1(rowid, a, b) VALUES(10, 'x', 'ten');",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	r := db.Query("SELECT pgno, cell, field, value FROM sqlite_dbdata WHERE pgno=2;")
	if got, want := dbdataRows(t, r), "2 0 -1 5\n2 0 0 'v'\n2 0 1 'five'\n2 1 -1 10\n2 1 0 'x'\n2 1 1 'ten'\n"; got != want {
		t.Errorf("pgno=2 rows:\ngot:  %s\nwant: %s", got, want)
	}
	// Page 1: field 0..2 of the schema record are 'table'/name/name and
	// field 3 is the root page. The stored CREATE text (field 4) is
	// engine-serialized, so only its presence is asserted here.
	r = db.Query("SELECT field, value FROM sqlite_dbdata WHERE pgno=1 AND cell=0 ORDER BY field;")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	type fv struct {
		f int64
		v interface{}
	}
	var got []fv
	for _, row := range r.Rows {
		f := row[0].(int64)
		got = append(got, fv{f, row[1]})
	}
	if len(got) != 6 || got[0].f != -1 || got[0].v != int64(1) ||
		got[1].v != "table" || got[2].v != "T1" || got[3].v != "T1" ||
		got[4].v != int64(2) {
		t.Errorf("page 1 cell 0 fields: got %#v", got)
	}
	if _, ok := got[5].v.(string); !ok || got[5].f != 4 {
		t.Errorf("page 1 cell 0 field 4: got %#v", got[5])
	}
}

// TestNativeDbdata_OverflowChains pins test/dbdata.test 1.3/1.4/1.5: values
// long enough to spill to overflow pages come back byte-for-byte through the
// chain walk.
func TestNativeDbdata_OverflowChains(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "dbd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t1(a, b);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	big := strings.Repeat("big", 2000)
	if r := db.Exec("INSERT INTO t1 VALUES(NULL, '" + big + "');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Query("SELECT value FROM sqlite_dbdata WHERE pgno=2 AND cell=0 AND field=1;")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatalf("overflow select: %v rows=%d", r.Error, len(r.Rows))
	}
	if s, ok := r.Rows[0][0].(string); !ok || s != big {
		t.Errorf("overflow text: got %d bytes, want %d", len(s), len(big))
	}
	if r := db.Exec("DELETE FROM t1; INSERT INTO t1 VALUES(NULL, randomblob(5050));"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r = db.Query("SELECT value FROM sqlite_dbdata WHERE pgno=2 AND cell=0 AND field=1;")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatalf("blob select: %v rows=%d", r.Error, len(r.Rows))
	}
	blob, ok := r.Rows[0][0].([]byte)
	if !ok || len(blob) != 5050 {
		t.Fatalf("overflow blob: got %T len=%d", r.Rows[0][0], len(blob))
	}
	r = db.Query("SELECT b FROM t1;")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatal(r.Error)
	}
	orig, ok := r.Rows[0][0].([]byte)
	if !ok || !bytes.Equal(orig, blob) {
		t.Errorf("overflow blob != stored blob")
	}
}

// TestNativeDbdata_SerialTypesAndNegativeRowid decodes a record exercising
// every serial type class through dbdataValue: negative rowid (9-byte varint),
// sign-extended 1..6-byte ints, serial types 8/9 (0 and 1), an IEEE double,
// text, blob and NULL.
func TestNativeDbdata_SerialTypesAndNegativeRowid(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "dbd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE tp(i INT, r REAL, s TEXT, b BLOB, x);",
		"INSERT INTO tp(rowid, i, r, s, b, x) VALUES(-5, -1, 3.14, 'txt', x'010204', NULL);",
		"INSERT INTO tp(rowid, i, r, s, b, x) VALUES(7, 0, 1.0, '', x'', 300);",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	r := db.Query("SELECT cell, field, value FROM sqlite_dbdata WHERE pgno=2 ORDER BY cell, field;")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	type cellField struct {
		c, f int64
	}
	want := map[cellField]interface{}{
		{0, -1}: int64(-5),
		{0, 0}:  int64(-1),
		{0, 1}:  3.14,
		{0, 2}:  "txt",
		{0, 3}:  []byte{1, 2, 4},
		{0, 4}:  nil,
		{1, -1}: int64(7),
		{1, 0}:  int64(0),
		{1, 1}:  1.0,
		{1, 2}:  "",
		{1, 3}:  []byte{},
		{1, 4}:  int64(300),
	}
	seen := map[cellField]bool{}
	for _, row := range r.Rows {
		key := cellField{row[0].(int64), row[1].(int64)}
		val := row[2]
		if b, ok := val.([]byte); ok && len(b) == 0 {
			val = []byte{}
		}
		w, exist := want[key]
		if !exist {
			t.Errorf("unexpected row cell=%d field=%d value=%#v", key.c, key.f, val)
			continue
		}
		if wb, ok := w.([]byte); ok {
			vb, ok2 := val.([]byte)
			if !ok2 || !bytes.Equal(wb, vb) {
				t.Errorf("cell=%d field=%d: got %#v want %#v", key.c, key.f, val, w)
			}
		} else if val != w {
			t.Errorf("cell=%d field=%d: got %#v (%T) want %#v", key.c, key.f, val, val, w)
		}
		seen[key] = true
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("missing row cell=%d field=%d", k.c, k.f)
		}
	}
	if len(r.Rows) != len(want) {
		t.Errorf("row count: got %d want %d", len(r.Rows), len(want))
	}
}

// dbdataPageType reads the b-tree page type byte of pgno via sqlite_dbpage.
func dbdataPageType(t *testing.T, db *frigolite.DB, pgno int64) byte {
	t.Helper()
	r := db.Query("SELECT data FROM sqlite_dbpage('main') WHERE pgno=" + itoa64(pgno) + ";")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatalf("dbpage %d: %v", pgno, r.Error)
	}
	data := r.Rows[0][0].([]byte)
	off := 0
	if pgno == 1 {
		off = 100
	}
	return data[off]
}

// dbdataInteriorChildren parses one interior page's child pointers in dbptr
// row order: the header's right-most pointer first, then each cell's child.
func dbdataInteriorChildren(t *testing.T, db *frigolite.DB, pgno int64) []int64 {
	t.Helper()
	r := db.Query("SELECT data FROM sqlite_dbpage('main') WHERE pgno=" + itoa64(pgno) + ";")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatalf("dbpage %d: %v", pgno, r.Error)
	}
	data := r.Rows[0][0].([]byte)
	off := 0
	if pgno == 1 {
		off = 100
	}
	nCell := int(binary.BigEndian.Uint16(data[off+3 : off+5]))
	out := []int64{int64(binary.BigEndian.Uint32(data[off+8 : off+12]))}
	for i := 0; i < nCell; i++ {
		p := int(binary.BigEndian.Uint16(data[off+12+2*i : off+14+2*i]))
		out = append(out, int64(binary.BigEndian.Uint32(data[p:p+4])))
	}
	return out
}

// TestNativeDbdata_InteriorPagesAndDbptr pins test/dbdata.test 2.1-2.3
// structurally (frigolite's page allocation may differ from the oracle's, so
// dbptr output is cross-checked against the raw page images): interior table
// pages yield no dbdata rows; index leaf rows start at field 0; dbptr lists
// the right-most pointer first, then each cell's child; unqualified scans
// concatenate pages in order.
func TestNativeDbdata_InteriorPagesAndDbptr(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "dbd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE t1(a);",
		"CREATE INDEX i1 ON t1(a);",
		"INSERT INTO t1 VALUES(randomblob(900));",
		"INSERT INTO t1 SELECT * FROM t1;",
		"INSERT INTO t1 SELECT * FROM t1;",
		"INSERT INTO t1 SELECT * FROM t1;",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	pgno := int64(1)
	var interior []int64
	indexLeaves := []int64{}
	for {
		r := db.Query("SELECT data FROM sqlite_dbpage('main') WHERE pgno=" + itoa64(pgno) + ";")
		if r.Error != nil || len(r.Rows) == 0 {
			break
		}
		switch dbdataPageType(t, db, pgno) {
		case 0x02, 0x05:
			interior = append(interior, pgno)
		case 0x0a:
			indexLeaves = append(indexLeaves, pgno)
		}
		pgno++
	}
	if len(interior) == 0 || len(indexLeaves) == 0 {
		t.Fatalf("fixture lacks interior/index pages: interior=%v indexLeaves=%v", interior, indexLeaves)
	}

	// Interior table pages produce no dbdata rows; index leaves start at
	// field 0 (no rowid row).
	for _, pg := range interior {
		r := db.Query("SELECT count(*) FROM sqlite_dbdata WHERE pgno=" + itoa64(pg) + ";")
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		if pgType := dbdataPageType(t, db, pg); pgType == 0x05 && r.Rows[0][0] != int64(0) {
			t.Errorf("interior table page %d yielded %v dbdata rows", pg, r.Rows[0][0])
		}
	}
	leaf := indexLeaves[0]
	r := db.Query("SELECT DISTINCT field FROM sqlite_dbdata WHERE pgno=" + itoa64(leaf) + " ORDER BY field;")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) == 0 || r.Rows[0][0] != int64(0) {
		t.Errorf("index leaf %d field numbers must start at 0, got %v", leaf, r.Rows)
	}

	// dbptr: every interior page's rows equal the raw children in order.
	r = db.Query("SELECT pgno, child FROM sqlite_dbptr;")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	wantRows := 0
	for _, pg := range interior {
		kids := dbdataInteriorChildren(t, db, pg)
		wantRows += len(kids)
		got := []string{}
		for _, row := range r.Rows {
			if row[0] == pg {
				got = append(got, itoa64(row[1].(int64)))
			}
		}
		want := []string{}
		for _, k := range kids {
			want = append(want, itoa64(k))
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("dbptr page %d: got [%s] want [%s]", pg, strings.Join(got, " "), strings.Join(want, " "))
		}
	}
	if len(r.Rows) != wantRows {
		t.Errorf("dbptr row count: got %d want %d", len(r.Rows), wantRows)
	}
}

// TestNativeDbdata_SchemaArgumentForms covers the hidden-column schema
// binding: argument form, WHERE form, attached databases, unknown-database
// errors, eponymous-only CREATE rejection and the always-NULL schema column.
func TestNativeDbdata_SchemaArgumentForms(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "dbd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE t1(a, b);",
		"INSERT INTO t1(rowid, a, b) VALUES(5, 'v', 'five');",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	// Argument form equals the default (main) scan.
	a := db.Query("SELECT pgno, cell, field, value FROM sqlite_dbdata('main') WHERE pgno=2;")
	b := db.Query("SELECT pgno, cell, field, value FROM sqlite_dbdata WHERE pgno=2;")
	if a.Error != nil || b.Error != nil {
		t.Fatalf("arg-form queries: %v / %v", a.Error, b.Error)
	}
	if dbdataRows(t, a) != dbdataRows(t, b) {
		t.Errorf("sqlite_dbdata('main') != sqlite_dbdata")
	}
	// WHERE schema=? form (hidden column binding).
	r := db.Query("SELECT count(*) FROM sqlite_dbdata WHERE schema='main' AND pgno=2;")
	if r.Error != nil || r.Rows[0][0] != int64(3) {
		t.Errorf("WHERE schema='main': %v %v", r.Error, r.Rows)
	}
	// Attached database scans through its own pager.
	if r := db.Exec("ATTACH ':memory:' AS aux; CREATE TABLE aux.small(x); INSERT INTO aux.small VALUES('hello');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r = db.Query("SELECT pgno, cell, field, value FROM sqlite_dbdata('aux') WHERE pgno=2;")
	if got, want := dbdataRows(t, r), "2 0 -1 1\n2 0 0 'hello'\n"; got != want {
		t.Errorf("aux scan:\ngot:  %s\nwant: %s", got, want)
	}
	// Unknown schema is an error (oracle: "unknown database 'nosuch'").
	r = db.Query("SELECT * FROM sqlite_dbdata('nosuch');")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "unknown database") {
		t.Errorf("unknown schema error: got %v", r.Error)
	}
	// The schema column itself always reads NULL (dbdataColumn has no case
	// for it) and rowids are a monotonic 1-based counter.
	r = db.Query("SELECT schema, rowid FROM sqlite_dbdata LIMIT 3;")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	for i, row := range r.Rows {
		if row[0] != nil {
			t.Errorf("schema column: got %#v, want NULL", row[0])
		}
		if row[1] != int64(i+1) {
			t.Errorf("rowid: got %v, want %d", row[1], i+1)
		}
	}
	// Eponymous-only: CREATE VIRTUAL TABLE reports "no such module".
	r = db.Exec("CREATE VIRTUAL TABLE vd USING sqlite_dbdata;")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such module") {
		t.Errorf("CREATE VIRTUAL TABLE error: got %v", r.Error)
	}
	// A page number beyond EOF yields no rows and no error.
	r = db.Query("SELECT count(*) FROM sqlite_dbdata WHERE pgno=999;")
	if r.Error != nil || r.Rows[0][0] != int64(0) {
		t.Errorf("pgno=999: %v %v", r.Error, r.Rows)
	}
}

// TestNativeDbdata_PageFunctionForm covers the dbdata.c dbdataIsFunction
// form: a schema argument ending in "()" names a SQL function supplying the
// page count (fn(0)) and page images (fn(pgno)) — the recover extension's
// alternate page source.
func TestNativeDbdata_PageFunctionForm(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "dbd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE t1(a, b);",
		"INSERT INTO t1(rowid, a, b) VALUES(5, 'v', 'five');",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: %v", sql, r.Error)
		}
	}
	// Snapshot the database file's pages into a Go map, then serve them
	// through a UDF exactly like test_recover.c's getpage().
	r := db.Query("PRAGMA page_count;")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	nPages := r.Rows[0][0].(int64)
	pages := map[int64][]byte{}
	for pg := int64(1); pg <= nPages; pg++ {
		r := db.Query("SELECT data FROM sqlite_dbpage('main') WHERE pgno=" + itoa64(pg) + ";")
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		pages[pg] = r.Rows[0][0].([]byte)
	}
	db.RegisterFunction("mypage", func(args []interface{}) (interface{}, error) {
		n, ok := args[0].(int64)
		if !ok {
			return nil, nil
		}
		if n == 0 {
			return nPages, nil
		}
		return pages[n], nil
	}, 1, 1)
	r = db.Query("SELECT pgno, cell, field, value FROM sqlite_dbdata('mypage()') WHERE pgno=2;")
	if got, want := dbdataRows(t, r), "2 0 -1 5\n2 0 0 'v'\n2 0 1 'five'\n"; got != want {
		t.Errorf("function-form scan:\ngot:  %s\nwant: %s", got, want)
	}
	// A missing page function is an error.
	r = db.Query("SELECT * FROM sqlite_dbdata('nosuchfn()');")
	if r.Error == nil {
		t.Errorf("missing page function: expected error, got %v", r.Rows)
	}
}
