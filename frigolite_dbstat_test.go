package frigolite

import (
	"strings"
	"testing"
)

// Native anchors for the dbstat virtual table (src/dbstat.c parity). The
// page-level STRUCTURE (row set, path formats, pagetypes, pgoffset/pgsize
// formulas, overflow accounting) is pinned against the /usr/bin/sqlite3
// 3.51.0 oracle's behavior; the literal payload/unused NUMBERS depend on the
// btree's own allocation (frigolite's writer packs cells by its own split
// decisions), so aggregates are pinned by self-consistency with the page rows
// rather than by C's byte counts.

// dbstatRows runs a dbstat query and renders rows as pipe-joined strings.
func dbstatRows(t *testing.T, db *DB, query string) []string {
	t.Helper()
	r := db.Query(query)
	if r.Error != nil {
		t.Fatalf("%s: %v", query, r.Error)
	}
	out := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		parts := make([]string, 0, len(row))
		for _, v := range row {
			if v == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, formatSQLiteValue(v))
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

func TestNativeDBStatPageRows(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	exec("CREATE TABLE t1(a, b)")
	exec("CREATE INDEX i1 ON t1(a, b)")
	exec("INSERT INTO t1 VALUES(1, printf('%.100c','x'))")
	exec("INSERT INTO t1 VALUES(2, printf('%.5000c','y'))")

	// Every btree of the schema appears; sqlite_schema is always seeded
	// (statFilter's UNION ALL) even for an empty database.
	rows := dbstatRows(t, db, `SELECT DISTINCT name FROM dbstat ORDER BY name`)
	if got := strings.Join(rows, ","); got != "i1,sqlite_schema,t1" {
		t.Errorf("btree names: got [%s]", got)
	}

	// Overflow pages: pagetype=overflow, ncell=0, mx_payload=0, and each
	// non-final overflow page carries exactly usable-4 payload bytes
	// (getLocalPayload + statNext's overflow accounting).
	rows = dbstatRows(t, db, `SELECT DISTINCT ncell, mx_payload, payload FROM dbstat WHERE pagetype='overflow'`)
	for _, row := range rows {
		f := strings.Split(row, "|")
		if f[0] != "0" || f[1] != "0" {
			t.Errorf("overflow ncell/mx_payload: got [%s]", row)
		}
		// Non-final pages carry usable-4; the final page carries the
		// remainder (both are >0 and <= usable-4 for this 1024-page db).
		n, _ := atoi64v(f[2])
		if n <= 0 || n >= 1024 {
			t.Errorf("overflow payload out of range: [%s]", row)
		}
	}
	if len(rows) == 0 {
		t.Fatal("expected overflow rows for the 5000-byte row")
	}

	// pgoffset = pageSize*(pageno-1), pgsize = pageSize (statSizeAndOffset).
	rows = dbstatRows(t, db, `SELECT pageno, pgoffset, pgsize FROM dbstat WHERE name='t1' AND pagetype='leaf' LIMIT 1`)
	f := strings.Split(rows[0], "|")
	if f[1] != f[0] && false { // placeholder; exact check below
	}
	if f[2] != "1024" {
		t.Errorf("pgsize: got [%s] want 1024", rows[0])
	}
	var pgno, pgoffset int
	if _, err := fmtSscan(f[0], &pgno); err != nil {
		t.Fatal(err)
	}
	if _, err := fmtSscan(f[1], &pgoffset); err != nil {
		t.Fatal(err)
	}
	if pgoffset != 1024*(pgno-1) {
		t.Errorf("pgoffset formula: pageno=%d offset=%d", pgno, pgoffset)
	}

	// rowid = pageno (statRowid).
	rows = dbstatRows(t, db, `SELECT rowid, pageno FROM dbstat WHERE name='t1' LIMIT 1`)
	if f := strings.Split(rows[0], "|"); f[0] != f[1] {
		t.Errorf("rowid != pageno: [%s]", rows[0])
	}
}

// fmtSscan and atoi64v are tiny helpers keeping the assertions readable.
func fmtSscan(s string, out *int) (int, error) {
	var v int
	_, err := fmtSscanf(s, &v)
	if err == nil {
		*out = v
	}
	return 1, err
}

func fmtSscanf(s string, v *int) (int, error) {
	n := 0
	neg := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' && n == 0 {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	*v = n
	return 1, nil
}

func atoi64v(s string) (int64, error) {
	var v int
	_, err := fmtSscan(s, &v)
	return int64(v), err
}

func TestNativeDBStatAggregate(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	exec("CREATE TABLE t1(a, b)")
	exec("CREATE INDEX i1 ON t1(a, b)")
	exec("INSERT INTO t1 VALUES(1, printf('%.100c','x'))")
	exec("INSERT INTO t1 VALUES(2, printf('%.5000c','y'))")

	// aggregate=1: one row per btree with path/pagetype/pgoffset NULL and
	// pageno = the page count; the counters equal the aggregation of that
	// btree's page rows (statColumn's isAgg branches).
	rows := dbstatRows(t, db, `SELECT name, pageno, path, pagetype, pgoffset FROM dbstat WHERE aggregate=1 ORDER BY name`)
	if len(rows) != 3 {
		t.Fatalf("aggregate rows: got %d [%v], want 3", len(rows), rows)
	}
	for _, row := range rows {
		f := strings.Split(row, "|")
		if f[2] != "NULL" || f[3] != "NULL" || f[4] != "NULL" {
			t.Errorf("aggregate NULL columns: got [%s]", row)
		}
	}
	// Self-consistency: the aggregate row equals the aggregation of that
	// btree's page rows (pageno = count, payload = sum).
	rows = dbstatRows(t, db, `SELECT (SELECT count(*) FROM dbstat WHERE name='t1') = (SELECT pageno FROM dbstat WHERE aggregate=1 AND name='t1'), (SELECT sum(payload) FROM dbstat WHERE name='t1') = (SELECT payload FROM dbstat WHERE aggregate=1 AND name='t1'), (SELECT sum(pgsize) FROM dbstat WHERE name='t1') = (SELECT pgsize FROM dbstat WHERE aggregate=1 AND name='t1')`)
	if got := strings.Join(rows, ","); got != "1|1|1" && got != "1 1 1" {
		t.Errorf("aggregate self-consistency: got [%s] want all-true", got)
	}
}

func TestNativeDBStatHiddenSchemaBinding(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	exec("CREATE TABLE t1(a)")
	exec("INSERT INTO t1 VALUES('x')")

	// schema= selects the analyzed database; the schema column echoes it.
	rows := dbstatRows(t, db, `SELECT DISTINCT schema FROM dbstat WHERE schema='main'`)
	if len(rows) != 1 || rows[0] != "main" {
		t.Errorf("schema=main: got %v", rows)
	}
	// Unknown database: zero rows (C's statFilter sets isEof).
	rows = dbstatRows(t, db, `SELECT count(*) FROM dbstat WHERE schema='nosuch'`)
	if len(rows) != 1 || rows[0] != "0" {
		t.Errorf("schema=nosuch: got %v want [0]", rows)
	}
}

func TestNativeDBStatCorruptPage(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	exec("CREATE TABLE t1(a, b)")
	exec("INSERT INTO t1 VALUES(1, 'x')")
	exec("INSERT INTO t1 VALUES(2, 'y')")

	// Smash the root page's flag byte through sqlite_dbpage (defensive mode
	// off): dbstat reports pagetype "corrupted" with zeroed cells instead of
	// failing the scan (statDecodePage's statPageIsCorrupt path).
	db.SetDefensive(false)
	r := db.Query(`SELECT pageno FROM dbstat WHERE name='t1' AND pagetype='leaf' LIMIT 1`)
	if r.Error != nil || len(r.Rows) == 0 {
		t.Fatalf("pre-corrupt scan: %v", r.Error)
	}
	root := r.Rows[0][0]
	exec(fmtPageZero(root.(int64)))

	rows := dbstatRows(t, db, `SELECT pagetype, ncell, payload, unused, mx_payload FROM dbstat WHERE name='t1' AND pageno=`+formatSQLiteValue(root))
	if len(rows) == 0 {
		t.Fatal("corrupt scan returned no rows")
	}
	if !strings.HasPrefix(rows[0], "corrupted|0|0|0|0") {
		t.Errorf("corrupt page row: got [%s]", rows[0])
	}
}

// fmtPageZero builds the sqlite_dbpage UPDATE that zeroes page pgno's first
// byte (the b-tree flags byte).
func fmtPageZero(pgno int64) string {
	return `UPDATE sqlite_dbpage SET data = x'00' || substr(data, 2) WHERE pgno = ` +
		formatSQLiteValue(pgno)
}

func TestNativeDBStatOverflowChainPath(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	exec("CREATE TABLE t1(a, b)")
	exec("INSERT INTO t1 VALUES(1, printf('%.10000c','z'))")

	// Overflow path shape: '<cellpath>%.3x+%.6x' (statNext's zPath format),
	// ordering BEFORE the owning page's child paths under BINARY.
	rows := dbstatRows(t, db, `SELECT path FROM dbstat WHERE name='t1' AND pagetype='overflow' ORDER BY path LIMIT 1`)
	if len(rows) == 0 {
		t.Fatal("no overflow rows")
	}
	if !strings.Contains(rows[0], "000+") || !strings.HasSuffix(rows[0], "+000000") {
		t.Errorf("overflow path format: got [%s]", rows[0])
	}
	// Multiple overflow pages of one cell enumerate +0, +1, ... in order.
	rows = dbstatRows(t, db, `SELECT path FROM dbstat WHERE name='t1' AND pagetype='overflow' ORDER BY path`)
	if len(rows) < 2 || !strings.HasSuffix(rows[1], "+000001") {
		t.Errorf("overflow chain enumeration: got %v", rows)
	}
}
