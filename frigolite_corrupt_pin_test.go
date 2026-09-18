// Native pin tests for the FULL-SUITE-DRIFT.T26-corrupt hexio family
// (P8.CORRUPT residue). Each test pins one engine-visible corruption
// detection/deferral contract that the corresponding testgen package
// (corrupt, corruptB, corruptC, corruptF, corruptL, corruptN) exercises
// through byte-poked database images:
//
//   - TestCorruptCPageSizeDeferral: openPager defers an invalid header
//     page size to the first statement ("file is not a database",
//     btree.c lockBtree) instead of sizing buffers with it (corruptC).
//   - TestCorruptLSchemaRootpageValidation: sqlite3InitCallback row
//     checks — rootpage beyond the page count and duplicate index
//     rootpages report "malformed database schema (NAME) - invalid
//     rootpage", or the generic WriteSchema corrupt when writable_schema
//     is ON (corruptL 6.1/7.1/16.1/19.2, corruptN 3.1/4.2).
//   - TestCorruptLIndexKeyShapeIntegrityCheck: integrity_check fails
//     when an index b-tree's key width no longer matches its rewritten
//     CREATE INDEX definition (corruptL 19.4).
//   - TestCorruptFFreelistRootBeyondEOF: allocating a root from a
//     freelist leaf beyond the current page count grows the database
//     (corruptF 1.5/1.6).
//   - TestCorruptNSequenceSchemaParse: a sqlite_schema row whose CREATE
//     text no longer parses fails schema load with the parser's message
//     (or the WriteSchema generic) at first use (corruptN 3.1).
package frigolite_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// pokePageOverwrites the file at offset with the given 4-byte big-endian
// value (hexio_render_int32 style).
func pokePageInt32(t *testing.T, path string, offset int64, v uint32) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[offset] = byte(v >> 24)
	b[offset+1] = byte(v >> 16)
	b[offset+2] = byte(v >> 8)
	b[offset+3] = byte(v)
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestCorruptCPageSizeDeferral pins corruptC's single-byte header pokes:
// zeroing or garbling the page-size field (offsets 16-17) must not panic
// Open; like the /usr/bin/sqlite3 oracle, the corruption surfaces at the
// first statement as "file is not a database" (SQLITE_NOTADB).
func TestCorruptCPageSizeDeferral(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.db")
	db, err := frigolite.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("PRAGMA page_size=1024; CREATE TABLE t1(x,y)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	for i := 0; i < 32; i++ {
		if res := db.Exec("INSERT INTO t1 VALUES(1,1)"); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	db.Close()

	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int{16, 17} {
		poked := append([]byte(nil), raw...)
		poked[off] = 0xAB
		p := filepath.Join(dir, "poked.db")
		if err := os.WriteFile(p, poked, 0644); err != nil {
			t.Fatal(err)
		}
		// Open must not fail and must not panic (the pre-fix engine built
		// make([]byte, pageSize) with the bogus size and panicked).
		dbr, err := frigolite.Open(p)
		if err != nil {
			t.Fatalf("offset %d: open: %v", off, err)
		}
		res := dbr.Exec("SELECT count(*) FROM sqlite_master")
		if res.Error == nil {
			t.Errorf("offset %d: expected deferred NOTADB error, got nil", off)
		} else if !strings.Contains(res.Error.Error(), "file is not a database") {
			t.Errorf("offset %d: expected \"file is not a database\", got %q", off, res.Error)
		}
		dbr.Close()
	}
}

// TestCorruptLSchemaRootpageValidation pins the sqlite3InitCallback row
// checks: a schema row whose rootpage is beyond the database page count,
// and index rows that share one rootpage, report
// "malformed database schema (NAME) - invalid rootpage" — the generic
// "database disk image is malformed" when writable_schema is ON
// (prepare.c corruptSchema's SQLITE_WriteSchema branch).
func TestCorruptLSchemaRootpageValidation(t *testing.T) {
	dir := t.TempDir()
	build := func(name string) *frigolite.DB {
		p := filepath.Join(dir, name)
		db, err := frigolite.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}

	// (a) rootpage beyond the page count (corruptL-7.1 shape).
	db := build("range.db")
	if res := db.Exec("CREATE TABLE t1(a); CREATE INDEX t1x1 ON t1(a)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	db.Close()
	db = build("range.db")
	if res := db.Exec("PRAGMA writable_schema=1; UPDATE sqlite_schema SET rootpage=9999 WHERE name='t1x1'"); res.Error != nil {
		t.Fatal(res.Error)
	}
	db.Close()
	db = build("range.db")
	res := db.Exec("SELECT count(*) FROM t1")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "malformed database schema (t1x1) - invalid rootpage") {
		t.Errorf("out-of-range rootpage: got %v", res.Error)
	}
	db.Close()

	// (b) duplicate index rootpages (corruptN-4.2 shape): the swap makes
	// both autoindexes share one rootpage; with writable_schema OFF the
	// named error fires, with it ON the generic WriteSchema corrupt.
	db = build("dup.db")
	if res := db.Exec("CREATE TABLE x1(a INTEGER PRIMARY KEY, b UNIQUE, c UNIQUE); INSERT INTO x1 VALUES(1,1,2),(2,2,3),(3,3,4),(4,5,6)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("PRAGMA writable_schema=1; UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM sqlite_schema WHERE name='sqlite_autoindex_x1_2') WHERE name='sqlite_autoindex_x1_1'"); res.Error != nil {
		t.Fatal(res.Error)
	}
	db.Close()
	db = build("dup.db")
	res = db.Exec("SELECT count(*) FROM x1")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "invalid rootpage") {
		t.Errorf("duplicate autoindex rootpage: got %v", res.Error)
	}
	db.Close()
	db = build("dup.db")
	res = db.Exec("PRAGMA writable_schema=1; SELECT count(*) FROM x1")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "database disk image is malformed") {
		t.Errorf("duplicate autoindex rootpage under writable_schema: got %v", res.Error)
	}
	db.Close()
}

// TestCorruptNSequenceSchemaParse pins corruptN-3.1: a schema row whose
// CREATE text no longer parses ('name-seq' column) fails at first use —
// with writable_schema ON the engine reports the generic
// "database disk image is malformed" (WriteSchema branch), matching the
// /usr/bin/sqlite3 oracle.
func TestCorruptNSequenceSchemaParse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "seq.db")
	db, err := frigolite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("CREATE TABLE t1(x INTEGER PRIMARY KEY AUTOINCREMENT, y); PRAGMA writable_schema=1; UPDATE sqlite_schema SET sql='CREATE TABLE sqlite_sequence(name-seq)' WHERE name='sqlite_sequence'"); res.Error != nil {
		t.Fatal(res.Error)
	}
	db.Close()

	db, err = frigolite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res := db.Exec("PRAGMA writable_schema=1; INSERT INTO t1(y) VALUES('abc')")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "database disk image is malformed") {
		t.Errorf("corrupt sqlite_sequence schema SQL: got %v", res.Error)
	}
}

// TestCorruptLIndexKeyShapeIntegrityCheck pins corruptL-19.4: an index
// whose stored CREATE text was narrowed (writable_schema) while its b-tree
// still holds the old-width keys makes integrity_check fail with
// "database disk image is malformed" (oracle-verified).
func TestCorruptLIndexKeyShapeIntegrityCheck(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shape.db")
	db, err := frigolite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT, c INTEGER, d TEXT); CREATE INDEX i1 ON t1((NULL)); INSERT INTO t1 VALUES(1, NULL, 1, 'text value'); PRAGMA writable_schema=on; UPDATE sqlite_schema SET sql='CREATE INDEX i1 ON t1(b, c, d)', tbl_name='t1', type='index' WHERE name='i1'"); res.Error != nil {
		t.Fatal(res.Error)
	}
	db.Close()

	db, err = frigolite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res := db.Exec("PRAGMA integrity_check")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "database disk image is malformed") {
		t.Errorf("narrowed index integrity_check: got %v", res.Error)
	}
}

// TestCorruptFFreelistRootBeyondEOF pins corruptF 1.5/1.6: after freeing
// the t2/t3 roots (freelist trunk page 3 holding leaf page 4), rewriting
// the trunk's leaf entry to page 6 and creating a new table allocates
// root page 6 — the database grows to cover it and the new rootpage
// passes the schema rootpage validation.
func TestCorruptFFreelistRootBeyondEOF(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "freelist.db")
	db, err := frigolite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("PRAGMA auto_vacuum=0; PRAGMA page_size=1024; CREATE TABLE t1(x); CREATE TABLE t2(x); CREATE TABLE t3(x); DROP TABLE t2; DROP TABLE t3"); res.Error != nil {
		t.Fatal(res.Error)
	}
	db.Close()

	// Rewrite the freelist trunk's first leaf from page 4 to page 6 (the
	// trunk is page 3: offset (3-1)*1024 + 8).
	pokePageInt32(t, p, int64(2*1024+8), 6)

	db, err = frigolite.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE t4(x)"); res.Error != nil {
		t.Fatalf("CREATE TABLE t4 from freelist leaf 6: %v", res.Error)
	}
	r := db.Query("SELECT rootpage FROM sqlite_schema WHERE name='t4'")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		t.Fatalf("t4 rootpage query: %v", r.Rows)
	}
	root := 0
	switch v := r.Rows[0][0].(type) {
	case int64:
		root = int(v)
	case int:
		root = v
	}
	if root != 6 {
		t.Errorf("t4 rootpage: got %d, want 6", root)
	}
	// The next schema read must accept rootpage 6 (no invalid-rootpage
	// false positive now that the allocation grew the page count).
	if res := db.Exec("SELECT * FROM sqlite_schema"); res.Error != nil {
		t.Errorf("schema read after beyond-EOF root allocation: %v", res.Error)
	}
}
