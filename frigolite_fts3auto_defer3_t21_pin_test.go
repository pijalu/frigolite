package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestFTS3AutoColumnNameFromStmt pins fts3auto.test's fts3_make_deferrable:
// sqlite3_column_name on SELECT * FROM an fts4(x) table supplies x, so the
// generated INSERT has a real column list.
func TestFTS3AutoColumnNameFromStmt(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec(`CREATE VIRTUAL TABLE t1 USING fts4(x)`); r.Error != nil {
		t.Fatal(r.Error)
	}
	stmt, err := db.Prepare(`SELECT * FROM t1`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Finalize()
	cols := stmt.Columns()
	if len(cols) != 1 || cols[0] != "x" {
		t.Fatalf("columns = %q, want [x]", cols)
	}
	if r := db.Exec(`INSERT INTO t1 (x) VALUES('aaaaaaa caaaaa')`); r.Error != nil {
		t.Fatalf("insert with prepared column name: %v", r.Error)
	}
	r := db.Query(`SELECT x FROM t1`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || fmt.Sprint(r.Rows[0][0]) != "aaaaaaa caaaaa" {
		t.Fatalf("rows = %#v, want one deferred-token row", r.Rows)
	}
}

// TestFTS3Defer3CorruptDoclist pins fts3defer3.test 1.4-1.7: zeroing the
// large "b" doclist keeps deferred "a b" queries working but makes a direct
// "b" query fail with SQLITE_CORRUPT.
func TestFTS3Defer3CorruptDoclist(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	exec(`CREATE VIRTUAL TABLE t1 USING fts4`)
	exec(`BEGIN`)
	for i := 1; i <= 800; i++ {
		doc := "b b"
		if i == 200 {
			doc = "a b"
		}
		exec(fmt.Sprintf("INSERT INTO t1 (docid, content) VALUES(%d, '%s')", i, doc))
	}
	exec(`COMMIT`)

	// fts3defer3 1.3: the corpus is queried once before the segment is
	// corrupted, exactly as in the SQLite source.
	before := db.Query(`SELECT docid, content FROM t1 WHERE t1 MATCH 'a b'`)
	if before.Error != nil {
		t.Fatalf("pre-corrupt a b query: %v", before.Error)
	}
	if len(before.Rows) != 1 || fmt.Sprint(before.Rows[0][1]) != "a b" {
		t.Fatalf("pre-corrupt rows = %#v, want 200 {a b}", before.Rows)
	}

	segments := db.Query(`SELECT blockid, length(block) FROM t1_segments`)
	if segments.Error != nil {
		t.Fatal(segments.Error)
	}
	if len(segments.Rows) != 2 {
		t.Fatalf("segments = %#v, want 2 blocks", segments.Rows)
	}
	exec(`UPDATE t1_segments SET block = zeroblob(length(block)) WHERE blockid = 2`)

	ab := db.Query(`SELECT count(*) FROM t1 WHERE t1 MATCH 'a b'`)
	if ab.Error != nil {
		t.Fatalf("deferred a b query: %v", ab.Error)
	}
	if len(ab.Rows) != 1 || fmt.Sprint(ab.Rows[0][0]) != "1" {
		t.Fatalf("a b count = %#v, want 1", ab.Rows)
	}
	bad := db.Query(`SELECT count(*) FROM t1 WHERE t1 MATCH 'b'`)
	if bad.Error == nil || !strings.Contains(bad.Error.Error(), "database disk image is malformed") {
		t.Fatalf("MATCH 'b' error = %v, want malformed-image error", bad.Error)
	}
}
